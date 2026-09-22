package goagent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// RuntimeStatus 由内核循环派生的运行实例状态，唯一写者是 goagent 内核。
// 与内核既有的 StopReason / EvSuspend 一一对应（见 PR-SubAgent-Control-Plane §3.1）。
type RuntimeStatus int

const (
	// StatusPending 已登记，循环未启动（宿主 Register 后、内核接管前的窗口期）。
	StatusPending RuntimeStatus = iota
	// StatusRunning 循环执行中。
	StatusRunning
	// StatusCompleted 正常结束（StopFinished；含钩子正常中止与挂起返回 StopSuspended）。
	// Think Loop 没有中间态：挂起返回即本轮循环正常结束，
	// "等待外部输入、稍后恢复"是宿主编排（经 EvSuspend / LastStopReason 感知）。
	StatusCompleted
	// StatusFailed 失败（StopError / 达到最大轮数 / 启动前失败）。
	StatusFailed
	// StatusCancelled 被取消（ctx 取消或 Cancel()）。
	StatusCancelled
)

// String 返回状态的可读名称。
func (s RuntimeStatus) String() string {
	switch s {
	case StatusPending:
		return "pending"
	case StatusRunning:
		return "running"
	case StatusCompleted:
		return "completed"
	case StatusFailed:
		return "failed"
	case StatusCancelled:
		return "cancelled"
	default:
		return "unknown"
	}
}

// isTerminal 判断是否终态。终态条目保留在登记表中供查询，直到宿主 Unregister。
func (s RuntimeStatus) isTerminal() bool {
	return s == StatusCompleted || s == StatusFailed || s == StatusCancelled
}

// Runtime 是一个运行中的 Agent 实例的控制视图：任何人可读，Cancel 是唯一写操作。
// 与构建期的 goagent.Agent 相对：Agent 是执行器（可复用），Runtime 是一次运行的实例。
//
// 概念分层：TaskID（任务，稳定）→ 多轮 Runtime（运行实例，每轮）。
// 用户发起的一个问题是一个任务；任务挂起恢复后以新运行实例续跑，
// 多轮实例共享同一 TaskID。客户端定位与控制统一用 TaskID（GetByTask），
// RuntimeID 是内核状态机的精确载体（终态后不可复用）。
type Runtime interface {
	// ID 运行实例 ID（内核自动生成或 WithRuntimeID 指定，全局唯一）。
	ID() string
	// TaskID 任务归属：一次任务跨多轮运行时共享同一 TaskID。
	// 未显式指定（WithTaskID）时退化为"一次任务 = 一轮运行"，等于 ID()。
	TaskID() string
	// Status 当前状态（由内核循环派生）。
	Status() RuntimeStatus
	// StartedAt 首次进入 Running 的时间；未启动时为零值。
	StartedAt() time.Time
	// EndedAt 进入终态的时间；零值表示未到终态。
	EndedAt() time.Time
	// Done 返回完成信号通道：进入终态时关闭；Pending 被 Fail/Cancel 时也关闭。
	Done() <-chan struct{}
	// Reason 返回终态原因：经 Fail 置为 Failed 时为宿主注明的失败原因；
	// 其余终态路径（内核结算 / Cancel）返回空串。
	Reason() string
	// Result 返回任务结果：经 Complete 结算（宿主直结的 Pending 实例）时为宿主写入的结果；
	// 内核接管的实例结果由会话承载，此处返回空串。
	Result() string
	// Cancel 取消实例：
	//   - Pending 态等价 Fail(id, "cancelled")（内核尚未接管，直接进入启动前失败终态）；
	//   - Running 态取消执行循环（循环以既有 ctx 取消路径收尾，最终落为 Cancelled）；
	//   - 终态 no-op。
	// 幂等，可安全重复调用。
	Cancel()
}

// runtimeEntry 是 Runtime 接口的唯一实现：登记表条目，内核直接驱动其内部状态。
// 状态读取走 RLock，写入只发生在内核跃迁（transit/settle）与宿主 Cancel/Fail 两类路径。
type runtimeEntry struct {
	id string

	mu           sync.RWMutex
	status       RuntimeStatus
	taskID       string // 任务归属；acquire 接管时写入，此后只读
	startedAt    time.Time
	endedAt      time.Time
	cancelFn     context.CancelFunc // 内核 run() 接管时注入；Cancel 据此取消执行循环
	failedReason string             // Fail 置入的失败原因；其余路径为空
	result       string             // Complete 置入的任务结果；仅宿主直结的 Pending 实例有值
	done         chan struct{}      // 进入终态时 close（close-once）
}

// newRuntimeEntry 创建 Pending 态条目。
func newRuntimeEntry(id string) *runtimeEntry {
	return &runtimeEntry{
		id:   id,
		done: make(chan struct{}),
	}
}

func (e *runtimeEntry) ID() string            { return e.id }
func (e *runtimeEntry) Done() <-chan struct{} { return e.done }

// TaskID 返回任务归属；未显式指定时等于 ID（一次任务 = 一轮运行的退化语义）。
func (e *runtimeEntry) TaskID() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.taskID == "" {
		return e.id
	}
	return e.taskID
}

// Status 原子读取当前状态。
func (e *runtimeEntry) Status() RuntimeStatus {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.status
}

// StartedAt 返回首次进入 Running 的时间。
func (e *runtimeEntry) StartedAt() time.Time {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.startedAt
}

// EndedAt 返回进入终态的时间；零值表示未到终态。
func (e *runtimeEntry) EndedAt() time.Time {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.endedAt
}

// Reason 返回 Fail 置入的失败原因；其余终态路径为空串。
func (e *runtimeEntry) Reason() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.failedReason
}

// Result 返回 Complete 置入的任务结果；仅宿主直结的 Pending 实例有值，其余为空串。
func (e *runtimeEntry) Result() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.result
}

// Cancel 取消实例（语义见 Runtime.Cancel）。幂等。
func (e *runtimeEntry) Cancel() {
	e.mu.Lock()
	defer e.mu.Unlock()
	switch e.status {
	case StatusPending:
		// 内核尚未接管：等价 Fail("cancelled")，直接进入启动前失败终态
		e.failedReason = "cancelled"
		e.finishLocked(StatusFailed)
	case StatusRunning:
		// 运行中：只取消循环，不直接改状态——终态由内核收尾时结算
		if e.cancelFn != nil {
			e.cancelFn()
		}
	default:
		// 终态：no-op
	}
}

// acquireLocked 内核接管条目：Pending → Running（首次启动）。
// 调用方必须持有 e.mu。终态或已在运行时返回错误（fail fast：宿主 ID 复用 bug 立即可见）。
func (e *runtimeEntry) acquireLocked(cancelFn context.CancelFunc) error {
	switch e.status {
	case StatusPending:
		e.status = StatusRunning
		e.startedAt = time.Now()
		e.cancelFn = cancelFn
		return nil
	case StatusRunning:
		return fmt.Errorf("goagent: runtime %q 已在运行中，不支持并发运行", e.id)
	default:
		return fmt.Errorf("goagent: runtime %q 生命周期已结束（%s），不能复用，宿主应生成新 ID", e.id, e.status)
	}
}

// finishLocked 进入终态并关闭 Done（close-once：只在非终态时执行）。
// 调用方必须持有 e.mu。
func (e *runtimeEntry) finishLocked(status RuntimeStatus) {
	if e.status.isTerminal() {
		return
	}
	e.status = status
	e.endedAt = time.Now()
	close(e.done)
}

// RuntimeManager 是运行实例的登记表（对应初始想法中的"线程池"）。
//
// 内部化原则：控制平面是内核设施而非可插拔组件——登记表只有包内唯一的
// defaultRuntimeManager 实例（所有 Ask/Chat 运行自动登记于此），外部禁止创建，
// 统一经 DefaultRuntimeManager() 获取访问句柄：查询状态（Get/List）、
// 控制执行（Runtime.Cancel）、清理终态条目（Unregister）。
//
// 职责是登记 + 查询快照 + 并发安全，不承载任何策略（注册时机、级联取消、
// 并发上限、崩溃恢复全部是宿主策略，见 PR-SubAgent-New-Mechanism §3.4）。
// 以具体类型定稿：内核需要驱动条目内部状态，唯一实现即参考实现；
// 未来如需自定义存储再抽接口。
type RuntimeManager struct {
	mu      sync.RWMutex
	entries map[string]*runtimeEntry
}

// defaultRuntimeManager 是包内唯一的运行实例登记表。
// Agent 构造时默认绑定它；包内测试经 withRuntimeManager 注入独立实例以隔离。
var defaultRuntimeManager = newRuntimeManager()

// DefaultRuntimeManager 返回包内唯一的运行实例登记表。
// 所有 Agent 运行自动登记于此；客户端用它统一查询与控制多个运行实例：
//
//	m := goagent.DefaultRuntimeManager()
//	for _, rt := range m.List() {
//		if rt.Status() == goagent.StatusRunning && rt.ID() == id {
//			rt.Cancel()
//		}
//	}
func DefaultRuntimeManager() *RuntimeManager { return defaultRuntimeManager }

// newRuntimeManager 创建空的登记表（包内构造：外部禁止创建登记表实例）。
func newRuntimeManager() *RuntimeManager {
	return &RuntimeManager{entries: make(map[string]*runtimeEntry)}
}

// Register 预登记实例（Pending 态），UI 在"已受理未运行"的窗口期即可见。
// 同 ID 非终态条目已存在时幂等返回既有视图；
// 同 ID 终态条目存在时返回错误（该 runtimeID 生命周期已结束，应生成新 ID）。
// 这是宿主编排的协作入口（如 SubAgent 受理流程），经 DefaultRuntimeManager() 统一访问。
func (m *RuntimeManager) Register(id string) (Runtime, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.entries[id]; ok {
		if e.Status().isTerminal() {
			return nil, fmt.Errorf("goagent: runtime %q 生命周期已结束（%s），不能重复登记", id, e.Status())
		}
		return e, nil
	}
	e := newRuntimeEntry(id)
	m.entries[id] = e
	return e, nil
}

// Get 查询实例；不存在时 ok=false。
func (m *RuntimeManager) Get(id string) (Runtime, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.entries[id]
	return e, ok
}

// List 返回全部实例快照（daemon API 与 UI 的数据源）。
// 内核永不自动移除条目；终态条目保留至宿主 Unregister。
func (m *RuntimeManager) List() []Runtime {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Runtime, 0, len(m.entries))
	for _, e := range m.entries {
		out = append(out, e)
	}
	return out
}

// Fail 将 Pending 实例置为 Failed 并关闭 Done，等待者经 Done() 唤醒；
// reason 经 Runtime.Reason() 可查（失败审计信息，不再丢弃）。
// 仅对 Pending 生效（内核已接管的实例由内核负责终态结算）；
// 用于 goroutine 启动前的失败（构造子 Agent 失败等场景）。
// 经 DefaultRuntimeManager() 统一访问。
func (m *RuntimeManager) Fail(id, reason string) {
	m.mu.Lock()
	e, ok := m.entries[id]
	m.mu.Unlock()
	if !ok {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.status != StatusPending {
		return
	}
	e.failedReason = reason
	e.finishLocked(StatusFailed)
}

// Complete 将 Pending 实例结算为 Completed 并写入结果、关闭 Done，等待者经 Done() 唤醒。
// 与 Fail 对称：仅对尚未被内核接管的实例生效（内核接管的实例由内核负责终态结算）；
// 用于宿主自管任务循环的直结场景（如 SubAgent 任务在宿主 goroutine 中跑完后写入结果）。
// 经 DefaultRuntimeManager() 统一访问。
func (m *RuntimeManager) Complete(id, result string) {
	m.mu.Lock()
	e, ok := m.entries[id]
	m.mu.Unlock()
	if !ok {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.status != StatusPending {
		return
	}
	e.result = result
	e.finishLocked(StatusCompleted)
}

// Unregister 移除终态实例（客户端/宿主的清理职责）；内核永不自动移除
// （终态保留供查询，提供"刚刚结束"的窗口）。
func (m *RuntimeManager) Unregister(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.entries, id)
}

// acquire 内核接管入口：按 ID 定位条目并驱动 Pending → Running。
// 未预登记时自动新建并直接进入 Running；命中终态或已运行时返回错误。
// taskID 是任务归属（空表示未显式指定，读取时退化为 ID）；cancelFn 是本轮
// 执行循环的取消函数，Cancel() 据此取消运行中的循环。
func (m *RuntimeManager) acquire(id, taskID string, cancelFn context.CancelFunc) (*runtimeEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[id]
	if !ok {
		// 未预登记：自动新建并直接进入 Running（宿主无法在表中预见的场景，见控制平面 §4.1）
		e = newRuntimeEntry(id)
		e.mu.Lock()
		e.status = StatusRunning
		e.taskID = taskID
		e.startedAt = time.Now()
		e.cancelFn = cancelFn
		e.mu.Unlock()
		m.entries[id] = e
		return e, nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.acquireLocked(cancelFn); err != nil {
		return nil, err
	}
	e.taskID = taskID // 预登记条目在接管时补写任务归属
	return e, nil
}

// GetByTask 按任务 ID 定位运行实例：优先返回该任务进行中的实例（Pending/Running），
// 多条时取最新启动的；全部终态时返回最近结束的。不存在时 ok=false。
// 挂起恢复产生多轮运行实例共享同一 TaskID，本方法是"我的任务现在怎么样了"的定位入口。
func (m *RuntimeManager) GetByTask(taskID string) (Runtime, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var best *runtimeEntry
	for _, e := range m.entries {
		if e.TaskID() != taskID {
			continue
		}
		switch {
		case best == nil:
			best = e
		case !e.Status().isTerminal() && best.Status().isTerminal():
			// 进行中的实例优先于终态实例
			best = e
		case e.Status().isTerminal() == best.Status().isTerminal():
			// 同类之间取最新：进行中比 StartedAt，终态比 EndedAt
			if e.Status().isTerminal() {
				if e.EndedAt().After(best.EndedAt()) {
					best = e
				}
			} else if e.StartedAt().After(best.StartedAt()) {
				best = e
			}
		}
	}
	return best, best != nil
}

// newRuntimeID 生成随机运行实例 ID（内核自动生成场景）。
func newRuntimeID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 失败极罕见；退化为时间戳保证非空唯一性
		return fmt.Sprintf("rt-%d", time.Now().UnixNano())
	}
	return "rt-" + hex.EncodeToString(b[:])
}
