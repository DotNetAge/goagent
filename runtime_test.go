package goagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/DotNetAge/goagent/subagent"

	"github.com/DotNetAge/gochat/core"
)

// ── 控制平面测试辅助 ──

// cpClient 控制平面测试用的 mock LLM 客户端：每轮调用 chatFn 决定响应，
// 按调用次序编号（0-based）驱动多轮场景。
type cpClient struct {
	chatFn func(ctx context.Context, call int) (core.Response, error)

	mu    sync.Mutex
	calls int
}

func (c *cpClient) Chat(ctx context.Context, messages []core.Message, opts ...core.Option) (*core.Response, error) {
	c.mu.Lock()
	n := c.calls
	c.calls++
	c.mu.Unlock()
	resp, err := c.chatFn(ctx, n)
	if err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *cpClient) ChatStream(ctx context.Context, messages []core.Message, opts ...core.Option) (*core.Stream, error) {
	return nil, errors.New("控制平面测试不支持流式")
}

// toolCallResp 构造一轮"发起工具调用"的模型响应。
func toolCallResp(id, name, args string) core.Response {
	return core.Response{
		Message: core.Message{
			Role: core.RoleAssistant,
			ToolCalls: []core.ToolCall{{
				ID:        id,
				Name:      name,
				Arguments: args,
			}},
		},
		FinishReason: "tool_calls",
	}
}

// finalResp 构造一轮"输出最终答案"的模型响应。
func finalResp(text string) core.Response {
	return core.Response{
		Message: core.Message{
			Role:    core.RoleAssistant,
			Content: []core.ContentBlock{{Type: core.ContentTypeText, Text: text}},
		},
		FinishReason: "stop",
	}
}

// finalOnlyClient 每轮都直接返回最终答案的客户端。
func finalOnlyClient() *cpClient {
	return &cpClient{chatFn: func(ctx context.Context, call int) (core.Response, error) {
		return finalResp("done"), nil
	}}
}

// blockingClient 阻塞到 ctx 取消的客户端（模拟长时间运行的 LLM 调用）。
func blockingClient() *cpClient {
	return &cpClient{chatFn: func(ctx context.Context, call int) (core.Response, error) {
		<-ctx.Done()
		return core.Response{}, ctx.Err()
	}}
}

// newCPAgent 构造接入控制平面的测试 Agent（注入独立登记表以隔离测试）。
func newCPAgent(id string, m *RuntimeManager, client core.Client, tools ...ToolCall) *Agent {
	return Ask("hi").
		Config(WithLLMClient(client), WithRuntimeID(id), withRuntimeManager(m)).
		Tools(tools...)
}

// waitStatus 轮询等待实例进入指定状态，超时则测试失败。
func waitStatus(t *testing.T, rt Runtime, want RuntimeStatus, d time.Duration) RuntimeStatus {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if st := rt.Status(); st == want {
			return st
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("等待状态 %v 超时，当前 %v", want, rt.Status())
	return rt.Status()
}

// ── 验收 1 + 2：状态跃迁完整性与挂起恢复 ──

// TestControlPlaneFullTransition 验收 1/2：
// mock Client 驱动 Running → Completed（挂起返回）；宿主感知等待输入后以新 runtimeID
// 对同一会话再次发起运行 → Running → Completed，各点 Status() 观测正确。
// Think Loop 没有中间态：挂起返回即本轮正常结束，"恢复"是宿主编排而非内核状态。
func TestControlPlaneFullTransition(t *testing.T) {
	m := newRuntimeManager()

	// 恢复运行期间（第二轮 LLM 调用中）观测到的状态
	var observedDuringResume RuntimeStatus
	client := &cpClient{chatFn: func(ctx context.Context, call int) (core.Response, error) {
		switch call {
		case 0:
			return toolCallResp("call-1", "ask_permission", `{"action":"run"}`), nil
		case 1:
			if rt, ok := m.Get("rt-full-2"); ok {
				observedDuringResume = rt.Status()
			}
			return finalResp("done"), nil
		default:
			return finalResp("done"), nil
		}
	}}

	a := newCPAgent("rt-full-1", m, client, &suspendedTool{})

	// 第一次运行：工具需要授权 → 挂起返回 → 本轮正常结束
	if _, err := a.ChatCtx(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a.LastStopReason() != StopSuspended {
		t.Fatalf("StopReason 应为 StopSuspended，得到 %v", a.LastStopReason())
	}
	rt1, ok := m.Get("rt-full-1")
	if !ok {
		t.Fatal("条目应已在登记表中")
	}
	if st := rt1.Status(); st != StatusCompleted {
		t.Fatalf("挂起返回是本轮正常结束，状态应为 Completed，得到 %v", st)
	}
	if rt1.EndedAt().IsZero() {
		t.Fatal("本轮结束后 EndedAt 应非零")
	}
	select {
	case <-rt1.Done():
	case <-time.After(time.Second):
		t.Fatal("本轮结束后 Done 应关闭")
	}

	// 宿主编排恢复：感知挂起（EvSuspend / LastStopReason）→ 补 tool 消息 → 新 runtimeID 再 Chat
	a.Config(WithRuntimeID("rt-full-2"))
	a.History([]core.Message{{
		Role:       core.RoleTool,
		ToolCallID: "call-1",
		Content:    []core.ContentBlock{{Type: core.ContentTypeText, Text: "已授权"}},
	}})
	if _, err := a.ChatCtx(context.Background()); err != nil {
		t.Fatal(err)
	}
	if observedDuringResume != StatusRunning {
		t.Fatalf("恢复运行期间新条目应为 Running，观测到 %v", observedDuringResume)
	}
	rt2, ok := m.Get("rt-full-2")
	if !ok {
		t.Fatal("恢复运行应以新 runtimeID 生成新实例")
	}
	if st := rt2.Status(); st != StatusCompleted {
		t.Fatalf("恢复运行应正常 Completed，得到 %v", st)
	}
	select {
	case <-rt2.Done():
	case <-time.After(time.Second):
		t.Fatal("终态后 Done 应关闭")
	}
}

// ── 验收 3：Cancel mid-run ──

// TestControlPlaneCancelMidRun 验收 3：
// 运行中 Cancel → Cancelled 终态、Done 关闭、循环收尾返回、重复 Cancel 幂等。
func TestControlPlaneCancelMidRun(t *testing.T) {
	m := newRuntimeManager()
	rt, err := m.Register("rt-cancel")
	if err != nil {
		t.Fatal(err)
	}

	a := newCPAgent("rt-cancel", m, blockingClient())

	errCh := make(chan error, 1)
	go func() {
		_, err := a.ChatCtx(context.Background())
		errCh <- err
	}()

	waitStatus(t, rt, StatusRunning, time.Second)
	rt.Cancel()

	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("循环应以 ctx 取消路径收尾，得到 %v", err)
	}
	waitStatus(t, rt, StatusCancelled, time.Second)
	if rt.EndedAt().IsZero() {
		t.Fatal("终态后 EndedAt 应非零")
	}
	select {
	case <-rt.Done():
	case <-time.After(time.Second):
		t.Fatal("终态后 Done 应关闭")
	}

	// 幂等：重复 Cancel 不改变终态、不 panic
	rt.Cancel()
	rt.Cancel()
	if rt.Status() != StatusCancelled {
		t.Fatalf("重复 Cancel 后状态应保持 Cancelled，得到 %v", rt.Status())
	}
}

// ── 验收 4：预登记 Pending → 内核接管 → Running；Fail → Failed ──

// TestControlPlanePendingTakeoverAndFail 验收 4：
// Register 后为 Pending；内核接管后进入 Running；Fail 仅对 Pending 生效。
func TestControlPlanePendingTakeoverAndFail(t *testing.T) {
	m := newRuntimeManager()

	// 启动前失败：Register → Fail → Failed
	rt, err := m.Register("rt-pending")
	if err != nil {
		t.Fatal(err)
	}
	if rt.Status() != StatusPending {
		t.Fatalf("新登记实例应为 Pending，得到 %v", rt.Status())
	}
	m.Fail("rt-pending", "构造子 Agent 失败")
	if rt.Status() != StatusFailed {
		t.Fatalf("Fail 后应为 Failed，得到 %v", rt.Status())
	}
	if rt.EndedAt().IsZero() {
		t.Fatal("Fail 后 EndedAt 应非零")
	}
	select {
	case <-rt.Done():
	case <-time.After(time.Second):
		t.Fatal("Fail 后 Done 应关闭（等待者经 Done() 唤醒）")
	}

	// Fail 只对 Pending 生效：已完成实例不受影响
	rt2, err := m.Register("rt-done")
	if err != nil {
		t.Fatal(err)
	}
	a2 := newCPAgent("rt-done", m, finalOnlyClient())
	if _, err := a2.ChatCtx(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rt2.Status() != StatusCompleted {
		t.Fatalf("实例应已 Completed，得到 %v", rt2.Status())
	}
	m.Fail("rt-done", "迟到的失败")
	if rt2.Status() != StatusCompleted {
		t.Fatalf("Fail 不应影响非 Pending 实例，得到 %v", rt2.Status())
	}
}

// ── 验收 5：未预登记自动新建；同 ID 终态再登记报错 ──

// TestControlPlaneAutoCreateAndReRegister 验收 5：
// 未预登记时内核自动新建并正常运转；同 ID 终态条目再登记报错（宿主 ID 复用 fail fast）。
func TestControlPlaneAutoCreateAndReRegister(t *testing.T) {
	m := newRuntimeManager()

	a := newCPAgent("rt-auto", m, finalOnlyClient())
	if _, err := a.ChatCtx(context.Background()); err != nil {
		t.Fatal(err)
	}

	rt, ok := m.Get("rt-auto")
	if !ok {
		t.Fatal("未预登记的实例应被内核自动新建进表")
	}
	if rt.Status() != StatusCompleted {
		t.Fatalf("自动新建实例应正常 Completed，得到 %v", rt.Status())
	}

	if _, err := m.Register("rt-auto"); err == nil {
		t.Fatal("同 ID 终态条目再登记应报错")
	}

	// 同 ID 运行中并发接管应报错（单实例单活跃运行）
	rt3, err := m.Register("rt-busy")
	if err != nil {
		t.Fatal(err)
	}
	a3 := newCPAgent("rt-busy", m, blockingClient())
	errCh := make(chan error, 1)
	go func() { _, err := a3.ChatCtx(context.Background()); errCh <- err }()
	waitStatus(t, rt3, StatusRunning, time.Second)
	if _, err := a3.ChatCtx(context.Background()); err == nil {
		t.Fatal("同一 Agent 并发 Chat 应返回错误（单实例单活跃运行）")
	}
	rt3.Cancel()
	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("被取消的运行应以 ctx 取消路径收尾，得到 %v", err)
	}
}

// ── 验收 6：并发 Get/List 与跃迁无竞态（-race）──

// TestControlPlaneConcurrentAccess 验收 6：
// 并发 Get/List/Status 与内核状态跃迁并行执行，-race 下无竞态。
func TestControlPlaneConcurrentAccess(t *testing.T) {
	m := newRuntimeManager()

	// 后台读 goroutine：持续 Get/List/Status
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = m.List()
				if rt, ok := m.Get("rt-race-0"); ok {
					_ = rt.Status()
					_ = rt.StartedAt()
					_ = rt.EndedAt()
				}
			}
		}()
	}

	// 主线程串行运行多个实例（每次运行触发完整的跃迁序列）
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("rt-race-%d", i)
		a := newCPAgent(id, m, finalOnlyClient())
		if _, err := a.ChatCtx(context.Background()); err != nil {
			t.Fatal(err)
		}
	}

	close(stop)
	wg.Wait()
}

// ── 验收 7：终态保留与 Unregister 语义 ──

// TestControlPlaneTerminalRetention 验收 7：
// 内核不自动移除条目，终态保留供查询；Unregister 后不可再查到。
func TestControlPlaneTerminalRetention(t *testing.T) {
	m := newRuntimeManager()

	a := newCPAgent("rt-retain", m, finalOnlyClient())
	if _, err := a.ChatCtx(context.Background()); err != nil {
		t.Fatal(err)
	}

	rt, ok := m.Get("rt-retain")
	if !ok {
		t.Fatal("终态条目应保留在表中（提供\"刚刚结束\"的查询窗口）")
	}
	if rt.Status() != StatusCompleted {
		t.Fatalf("保留条目应为 Completed，得到 %v", rt.Status())
	}

	m.Unregister("rt-retain")
	if _, ok := m.Get("rt-retain"); ok {
		t.Fatal("Unregister 后条目不应再能查到")
	}
	if n := len(m.List()); n != 0 {
		t.Fatalf("Unregister 后 List 应为空，得到 %d", n)
	}
}

// ── 验收 8：ChatCtx 外部取消 → Cancelled ──

// TestControlPlaneExternalCtxCancel 验收 8：
// 外部 ctx 取消传导进循环，实例落为 Cancelled。
func TestControlPlaneExternalCtxCancel(t *testing.T) {
	m := newRuntimeManager()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rt, err := m.Register("rt-external")
	if err != nil {
		t.Fatal(err)
	}

	a := newCPAgent("rt-external", m, blockingClient())
	errCh := make(chan error, 1)
	go func() {
		_, err := a.ChatCtx(ctx)
		errCh <- err
	}()

	waitStatus(t, rt, StatusRunning, time.Second)
	cancel()

	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("外部取消应以 ctx.Err() 收尾，得到 %v", err)
	}
	waitStatus(t, rt, StatusCancelled, time.Second)
}

// ── 安全回归：挂起哨兵被包装后的协议完整性 ──

// wrappedSuspendTool 返回包装过的 ErrNeedExternalInput——只携带哨兵、
// 不携带 *ExternalInputRequest（模拟工具层错误包装）。
type wrappedSuspendTool struct{}

func (wrappedSuspendTool) Name() string        { return "wrapped_suspend" }
func (wrappedSuspendTool) Description() string { return "返回包装的挂起哨兵错误" }
func (wrappedSuspendTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}

func (wrappedSuspendTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	return "", fmt.Errorf("业务包装: %w", ErrNeedExternalInput)
}

// TestWrappedSuspendErrorKeepsProtocol 包装的挂起哨兵不得触发挂起路径：
// 应按普通工具失败回传 tool 结果消息，保证 tool_calls 协议完整，
// 循环继续并正常完成（否则下一轮 LLM 调用会因缺少配对消息而报错）。
func TestWrappedSuspendErrorKeepsProtocol(t *testing.T) {
	m := newRuntimeManager()
	client := &cpClient{chatFn: func(ctx context.Context, call int) (core.Response, error) {
		if call == 0 {
			return toolCallResp("call-1", "wrapped_suspend", `{}`), nil
		}
		return finalResp("done"), nil
	}}
	a := newCPAgent("rt-wrapped", m, client, wrappedSuspendTool{})

	ans, err := a.ChatCtx(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ans != "done" {
		t.Fatalf("应继续循环得到最终答案，得到 %q", ans)
	}
	rt, ok := m.Get("rt-wrapped")
	if !ok || rt.Status() != StatusCompleted {
		t.Fatalf("应正常 Completed，得到 %v", rt)
	}
}

// ── 开发者体验：RuntimeID 是可选参数 ──

// TestAgentRuntimeIDAccessor 运行后可经 RuntimeID() 获取本轮实例 ID：
// 指定时返回指定值；未指定时返回内核自动生成的非空 ID，且能在登记表中定位。
func TestAgentRuntimeIDAccessor(t *testing.T) {
	m := newRuntimeManager()

	// 指定 ID：原样返回并可在表中定位
	a := newCPAgent("rt-accessor-1", m, finalOnlyClient())
	if _, err := a.ChatCtx(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a.RuntimeID() != "rt-accessor-1" {
		t.Fatalf("RuntimeID 应返回指定值，得到 %q", a.RuntimeID())
	}
	if _, ok := m.Get(a.RuntimeID()); !ok {
		t.Fatal("经 RuntimeID() 应能在登记表中定位实例")
	}

	// 未指定 ID：自动生成、非空、可定位（绝大多数调用的默认路径）
	b := Ask("hi").Config(WithLLMClient(finalOnlyClient()), withRuntimeManager(m))
	if _, err := b.ChatCtx(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b.RuntimeID() == "" {
		t.Fatal("自动生成的 RuntimeID 不应为空")
	}
	if _, ok := m.Get(b.RuntimeID()); !ok {
		t.Fatal("自动生成的 RuntimeID 应能在登记表中定位实例")
	}
}

// TestFailReasonVisible 验证 Fail 的 reason 落地可查：Reason() 返回宿主注明的失败原因，
// 且 Cancel（Pending 态）等价 Fail("cancelled") 也有原因可查。
func TestFailReasonVisible(t *testing.T) {
	m := newRuntimeManager()

	if _, err := m.Register("rt-fail-r"); err != nil {
		t.Fatal(err)
	}
	m.Fail("rt-fail-r", "构造子 Agent 失败")
	rt, ok := m.Get("rt-fail-r")
	if !ok || rt.Status() != StatusFailed {
		t.Fatal("Fail 后应为 Failed 终态")
	}
	if rt.Reason() != "构造子 Agent 失败" {
		t.Fatalf("Reason 应为宿主注明的失败原因，得到 %q", rt.Reason())
	}

	// Cancel 的 Pending 分支等价 Fail("cancelled")
	if _, err := m.Register("rt-cancel-r"); err != nil {
		t.Fatal(err)
	}
	rtCancel, _ := m.Get("rt-cancel-r")
	rtCancel.Cancel()
	rt2, _ := m.Get("rt-cancel-r")
	if rt2.Status() != StatusFailed || rt2.Reason() != "cancelled" {
		t.Fatalf("Cancel(Pending) 应落 Failed 且 Reason=cancelled，得到 %v/%q", rt2.Status(), rt2.Reason())
	}

	// 非 Fail 路径（内核结算）Reason 为空
	a := newCPAgent("rt-no-reason", m, finalOnlyClient())
	if _, err := a.ChatCtx(context.Background()); err != nil {
		t.Fatal(err)
	}
	rt3, _ := m.Get("rt-no-reason")
	if rt3.Reason() != "" {
		t.Fatalf("内核结算路径 Reason 应为空，得到 %q", rt3.Reason())
	}
}

// ── 概念分层：TaskID（任务，稳定）→ Runtime（每轮运行实例）──

// TestTaskIDDefaultDegenerates 默认退化：未指定 TaskID 时一次任务即一轮运行，
// TaskID 等于本轮 runtimeID，GetByTask 与 Get 定位到同一实例。
func TestTaskIDDefaultDegenerates(t *testing.T) {
	m := newRuntimeManager()

	a := newCPAgent("rt-task-default", m, finalOnlyClient())
	if _, err := a.ChatCtx(context.Background()); err != nil {
		t.Fatal(err)
	}
	rt, ok := m.Get("rt-task-default")
	if !ok {
		t.Fatal("实例应存在于登记表")
	}
	if rt.TaskID() != "rt-task-default" {
		t.Fatalf("默认 TaskID 应退化为 RuntimeID，得到 %q", rt.TaskID())
	}
	got, ok := m.GetByTask("rt-task-default")
	if !ok || got.ID() != "rt-task-default" {
		t.Fatal("GetByTask 应按退化 TaskID 定位到同一实例")
	}
}

// TestGetByTaskPrefersActive 验证任务定位规则：同一任务多轮实例共享 TaskID，
// GetByTask 优先返回进行中的实例；全部终态时返回最近结束的一轮。
func TestGetByTaskPrefersActive(t *testing.T) {
	m := newRuntimeManager()

	// 第一轮：跑完（终态）
	r1 := Ask("hi").Config(
		WithLLMClient(finalOnlyClient()), withRuntimeManager(m),
		WithRuntimeID("rt-task-a-1"), WithTaskID("task-a"))
	if _, err := r1.ChatCtx(context.Background()); err != nil {
		t.Fatal(err)
	}

	// 第二轮（模拟挂起恢复）：新运行实例，同 TaskID，阻塞在 LLM 调用上（进行中）
	gate := make(chan struct{})
	blocking := &cpClient{chatFn: func(ctx context.Context, call int) (core.Response, error) {
		<-gate
		return finalResp("resumed"), nil
	}}
	r2 := Ask("继续").Config(
		WithLLMClient(blocking), withRuntimeManager(m),
		WithRuntimeID("rt-task-a-2"), WithTaskID("task-a"))
	doneCh := make(chan error, 1)
	go func() { _, err := r2.ChatCtx(context.Background()); doneCh <- err }()

	// 等第二轮进入 Running，GetByTask 应返回进行中的第二轮（而非终态的第一轮）
	deadline := time.Now().Add(2 * time.Second)
	for {
		if e, ok := m.Get("rt-task-a-2"); ok && e.Status() == StatusRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("等待第二轮进入 Running 超时")
		}
		time.Sleep(2 * time.Millisecond)
	}
	rt, ok := m.GetByTask("task-a")
	if !ok || rt.ID() != "rt-task-a-2" {
		t.Fatalf("进行中的第二轮应优先，得到 %v", rt)
	}

	// 第二轮完成后：全部终态，返回最近结束的第二轮
	close(gate)
	if err := <-doneCh; err != nil {
		t.Fatal(err)
	}
	rt2, ok := m.GetByTask("task-a")
	if !ok || rt2.ID() != "rt-task-a-2" {
		t.Fatal("全部终态时应返回最近结束的第二轮实例")
	}
	if rt2.TaskID() != "task-a" {
		t.Fatalf("TaskID 应保持 %q，得到 %q", "task-a", rt2.TaskID())
	}
}

// ── 验收 8：嵌套派发（SubAgent 工具 × 控制平面端到端）──
//
// SubAgent 协议与工具已移至 goagent/subagent 包（Harness 通用概念），
// 本验收保留在内核：验证该工具与引擎、控制平面的端到端协作语义，
// 是 goharness 第 2 步 Dispatcher 实现的规格基准。
// 独立登记表（withRuntimeManager）隔离测试，避免共享全局表引入竞态。

// nestedStub 模拟控制平面的宿主受理流程：
// Register（Pending）→ 构造子 Agent（WithRuntimeID + 独立登记表 + Dispatcher）→
// go 子 Agent.ChatCtx（goroutine 归宿主，生命周期不继承父工具调用的 ctx）→ 回执。
// 派发深度两层：首个子实例继续派发（孙任务 leaf-task），其余直接完成。
type nestedStub struct {
	m *RuntimeManager

	mu          sync.Mutex
	seq         int
	finalStates map[string]RuntimeStatus
}

func newNestedStub(m *RuntimeManager) *nestedStub {
	return &nestedStub{m: m, finalStates: make(map[string]RuntimeStatus)}
}

func (d *nestedStub) Submit(ctx context.Context, req subagent.SubAgentRequest) (subagent.SubAgentReceipt, error) {
	d.mu.Lock()
	d.seq++
	id := fmt.Sprintf("child-%d", d.seq)
	d.mu.Unlock()

	// 受理即登记（Pending，UI 立即可见）
	if _, err := d.m.Register(id); err != nil {
		return subagent.SubAgentReceipt{}, err
	}

	// 子 Agent 的 LLM 行为：leaf-task 直接完成；否则仅首轮派发一次（嵌套两层），后续轮给最终答案
	childClient := &cpClient{chatFn: func(ctx context.Context, call int) (core.Response, error) {
		if req.Task == "leaf-task" {
			return finalResp("leaf done"), nil
		}
		if call == 0 {
			return toolCallResp("call-"+id, subagent.SubAgentToolName, `{"agent_name":"leaf","task":"leaf-task"}`), nil
		}
		return finalResp("child done"), nil
	}}

	child := Ask(req.Task).
		Config(WithLLMClient(childClient), WithRuntimeID(id), withRuntimeManager(d.m)).
		Tools(subagent.NewSubAgentTool(d))

	go func() {
		// 宿主编排生命周期：不继承父工具调用的 ctx
		child.ChatCtx(context.Background())
		// 收尾：记录终态后 Unregister
		d.mu.Lock()
		if rt, ok := d.m.Get(id); ok {
			d.finalStates[id] = rt.Status()
		}
		d.mu.Unlock()
		d.m.Unregister(id)
	}()

	return subagent.SubAgentReceipt{Accepted: true, TaskID: id}, nil
}

// waitNestedReceipts 轮询等待回执数量达标。
func waitNestedReceipts(t *testing.T, d *nestedStub, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		d.mu.Lock()
		got := len(d.finalStates)
		d.mu.Unlock()
		if got >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待 %d 个子任务收尾超时，当前 %d", n, len(d.finalStates))
}

// TestSubAgentNestedDispatch：父 → 子 → 孙嵌套派发全部经宿主受理，
// 每层都是独立 Runtime 实例，收尾后从登记表移除，父会话正常完成。
func TestSubAgentNestedDispatch(t *testing.T) {
	m := newRuntimeManager()
	stub := newNestedStub(m)

	// 父 Agent：先派发，再给最终答案
	parentClient := &cpClient{chatFn: func(ctx context.Context, call int) (core.Response, error) {
		if call == 0 {
			return toolCallResp("call-parent", subagent.SubAgentToolName, `{"agent_name":"worker","task":"level2"}`), nil
		}
		return finalResp("parent done"), nil
	}}
	parent := newCPAgent("parent", m, parentClient, subagent.NewSubAgentTool(stub))

	ans, err := parent.ChatCtx(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ans != "parent done" {
		t.Fatalf("父会话应正常完成，得到 %q", ans)
	}

	// 等待子、孙两层全部收尾
	waitNestedReceipts(t, stub, 2)

	stub.mu.Lock()
	states := make(map[string]RuntimeStatus, len(stub.finalStates))
	for k, v := range stub.finalStates {
		states[k] = v
	}
	stub.mu.Unlock()

	if len(states) != 2 {
		t.Fatalf("应有子、孙两个实例收尾，得到 %v", states)
	}
	for id, st := range states {
		if st != StatusCompleted {
			t.Fatalf("实例 %s 应正常 Completed，得到 %v", id, st)
		}
	}

	// 收尾后子实例已从登记表移除，父条目保留为终态
	if _, ok := m.Get("child-1"); ok {
		t.Fatal("child-1 收尾后应已 Unregister")
	}
	parentRT, ok := m.Get("parent")
	if !ok {
		t.Fatal("父条目应保留在登记表中")
	}
	if parentRT.Status() != StatusCompleted {
		t.Fatalf("父实例应为 Completed，得到 %v", parentRT.Status())
	}
}
