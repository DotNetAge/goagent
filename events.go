package goagent

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"

	"github.com/DotNetAge/gochat/core"
)

// EventType 标识事件类型，工作在思考循环的业务级别（而非 LLM token 级别）。
type EventType string

const (
	// EvThinkingDelta 模型思考增量（流式）。Data：string。
	EvThinkingDelta EventType = "thinking_delta"
	// EvContentDelta 模型回答文本增量（流式）。Data：string。
	EvContentDelta EventType = "content_delta"
	// EvToolUseDelta 模型发起的工具调用参数增量（流式）。Data：gochat core.ToolCallDelta。
	EvToolUseDelta EventType = "tool_use_delta"
	// EvThinkingDone 思考阶段完成（流式思考流结束）。Data：nil。
	EvThinkingDone EventType = "thinking_done"
	// EvLoopEnd 一轮完整的 Think-Act 循环结束。Data：*LoopEndData。
	EvLoopEnd EventType = "loop_end"
	// EvToolExecStart 工具开始执行。Data：*ToolExecStartData。
	EvToolExecStart EventType = "tool_exec_start"
	// EvToolExecEnd 工具执行完成。Data：*ToolExecEndData。
	EvToolExecEnd EventType = "tool_exec_end"
	// EvFinalAnswer 思考循环产生最终答案。Data：string（最终答案文本）。
	EvFinalAnswer EventType = "final_answer"
	// EvStop 思考循环终止。Data：StopReason（错误终止即 StopError，错误本身经 Chat 返回值传递）。
	EvStop EventType = "stop"
	// EvComplete 对话完成（无论成功、出错还是达到最大轮数），
	// 携带完整的对话上下文。Data：[]gochat core.Message。
	EvComplete EventType = "complete"
	// EvSuspend 工具执行需要外部输入，循环挂起等待。Data：*ExternalInputRequest（见 suspend.go）。
	EvSuspend EventType = "suspend"
	// EvTokenUsage 每次 LLM 调用完成后发射，携带该轮的 token 消耗。Data：*TokenUsageEvent。
	EvTokenUsage EventType = "token_usage"
)

// TokenUsageEvent 是 EvTokenUsage 事件的数据载体。
type TokenUsageEvent struct {
	// Iteration 本轮对应的循环迭代号（0-based）。
	Iteration int
	// Usage 本轮调用的 token 消耗。Provider 未返回 usage 时为 nil。
	Usage *core.Usage
	// Duration 本轮 LLM 调用耗时。
	Duration time.Duration
}

// LoopEndData 是 EvLoopEnd 事件的数据载体。
type LoopEndData struct {
	// Iteration 结束的循环迭代号（0-based）。
	Iteration int
}

// ToolExecStartData 是 EvToolExecStart 事件的数据载体。
type ToolExecStartData struct {
	// Name 被执行的工具名称。
	Name string
	// Args 模型生成的工具参数（json.RawMessage）。
	Args json.RawMessage
}

// ToolExecEndData 是 EvToolExecEnd 事件的数据载体。
type ToolExecEndData struct {
	// Name 被执行的工具名称。
	Name string
	// Duration 工具执行耗时。
	Duration time.Duration
	// Success 是否执行成功。
	Success bool
	// Result 工具输出文本（成功时）。
	Result string
	// Error 执行错误（失败时非 nil）。
	Error error
}

// Event 是事件总线中的一个事件。
type Event struct {
	// Type 事件类型。
	Type EventType
	// Data 事件数据，类型由 Type 决定（通常是指向特定结构体的指针）。
	Data any
}

// EventBus 是事件发布-订阅的抽象接口。
// 默认实现是进程内通道；外部（如 goharness）可替换为带过滤订阅、缓冲和关闭语义的实现。
type EventBus interface {
	// Emit 发布一个事件给所有订阅者。
	Emit(event Event)
	// Subscribe 返回一个事件通道和一个取消函数。
	Subscribe() (<-chan Event, func())
	// Close 关闭事件总线，关闭所有订阅者通道。后续 Emit 变为空操作。
	Close()
}

// InProcessEventBus 是 EventBus 的默认实现：基于通道的进程内事件总线。
//
// 并发模型（安全审计后定稿）：
//   - 订阅者通道无缓冲，Emit 阻塞直到订阅者接收——事件"同步可靠"，不丢事件；
//   - Emit 先快照订阅者列表并释放总线锁，再逐个投递：投递阻塞期间
//     不持有任何总线锁，Subscribe/Cancel/Close 不被饿死（修复历史上的死锁）；
//   - Cancel 只置位（墓碑），不关闭通道、不阻塞：与在途投递并发绝对安全；
//   - 通道统一由 Close() 关闭——Close 必须由发送方生命周期收尾调用
//     （如 execLoop 的 defer），此刻已无在途投递，不存在向已关闭通道发送。
//   - 已取消的订阅者在投递时被跳过（事件静默丢弃）。
type InProcessEventBus struct {
	mu          sync.RWMutex
	subscribers []*busSubscriber
}

// busSubscriber 是单个订阅者：数据通道与"已取消"标记分离，
// 取消操作（置位）与投递操作（发送）因此无需互斥，永不阻塞、永不 panic。
type busSubscriber struct {
	ch     chan Event
	closed atomic.Bool
}

// NewInProcessEventBus 创建默认进程内事件总线。
func NewInProcessEventBus() *InProcessEventBus {
	return &InProcessEventBus{}
}

// Emit 向所有订阅者发布事件。
// 已取消的订阅者被跳过；通道无缓冲，投递会阻塞直到订阅者接收。
func (b *InProcessEventBus) Emit(event Event) {
	// 快照后立即释放锁：投递可能长时间阻塞（等待慢消费者），
	// 期间必须允许其他 goroutine 订阅/取消/查询。
	b.mu.RLock()
	subs := make([]*busSubscriber, len(b.subscribers))
	copy(subs, b.subscribers)
	b.mu.RUnlock()

	for _, s := range subs {
		if s.closed.Load() {
			continue // 已取消的订阅者不再投递
		}
		s.ch <- event
	}
}

// Subscribe 订阅所有事件，返回只读通道和取消函数。取消函数幂等，可安全重复调用。
//
// 取消语义：置位后该订阅者不再收到事件（在途投递除外）。通道不会被取消
// 操作关闭——所有订阅者通道（含已取消的）统一在总线 Close() 时关闭，
// 消费侧以通道关闭为终止信号。
func (b *InProcessEventBus) Subscribe() (<-chan Event, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := &busSubscriber{ch: make(chan Event)}
	b.subscribers = append(b.subscribers, s)
	var once sync.Once
	cancel := func() {
		// 只置位、不从列表移除：移除会让 Close 的快照丢失该订阅者，
		// 导致其通道永远不被关闭、消费侧 range 永久挂起。
		once.Do(func() { s.closed.Store(true) })
	}
	return s.ch, cancel
}

// Close 关闭事件总线：关闭所有订阅者通道，后续 Emit 变为空操作。
//
// 安全前提：Close 必须在发送方（execLoop）结束投递之后调用
// （defer bus.Close() 天然满足），否则可能与在途投递竞态。
func (b *InProcessEventBus) Close() {
	b.mu.Lock()
	subs := b.subscribers
	b.subscribers = nil
	b.mu.Unlock()
	for _, s := range subs {
		s.closed.Store(true)
		close(s.ch)
	}
}

// NopEventBus 是空操作事件总线：所有 Emit 不做任何事，Subscribe 返回已关闭通道。
// 当 Agent 未配置事件总线时，用它兜底避免 nil 调用。
type NopEventBus struct{}

func (NopEventBus) Emit(Event) {}
func (NopEventBus) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event)
	close(ch)
	return ch, func() {}
}
func (NopEventBus) Close() {}
