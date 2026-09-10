package goagent

import (
	"sync"
	"time"

	"github.com/DotNetAge/gochat/core"
)

// EventType 标识事件类型，工作在思考循环的业务级别（而非 LLM token 级别）。
type EventType string

const (
	// EvThinkingDelta 模型思考增量（流式）。
	EvThinkingDelta EventType = "thinking_delta"
	// EvContentDelta 模型回答文本增量（流式）。
	EvContentDelta EventType = "content_delta"
	// EvToolUseDelta 模型发起的工具调用参数增量（流式）。
	EvToolUseDelta EventType = "tool_use_delta"
	// EvThinkingDone 思考阶段完成。
	EvThinkingDone EventType = "thinking_done"
	// EvLoopEnd 一轮完整的 Think-Act 循环结束（含迭代号和耗时）。
	EvLoopEnd EventType = "loop_end"
	// EvToolExecStart 工具开始执行。
	EvToolExecStart EventType = "tool_exec_start"
	// EvToolExecEnd 工具执行完成。
	EvToolExecEnd EventType = "tool_exec_end"
	// EvFinalAnswer 思考循环产生最终答案。
	EvFinalAnswer EventType = "final_answer"
	// EvStop 思考循环终止，携带 StopReason。
	EvStop EventType = "stop"
	// EvError 思考循环发生错误。
	EvError EventType = "error"
	// EvComplete 对话完成（无论成功、出错还是达到最大轮数），
	// 携带完整的对话上下文（原始 Message 列表）。
	EvComplete EventType = "complete"
	// EvSuspend 工具执行需要外部输入，循环挂起等待。
	// Data 类型为 *ExternalInputRequest（见 suspend.go）。
	EvSuspend EventType = "suspend"
	// EvTokenUsage 每次 LLM 调用完成后发射，携带该轮的 token 消耗。
	// Data 类型为 *TokenUsageEvent。
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
// 订阅者通道是无缓冲的——发布操作会阻塞直到所有订阅者接收完毕。
// 这种设计让事件"同步可靠"，避免因慢消费者丢事件。
// 如果需要更高性能，调用方应在订阅侧自行缓冲。
type InProcessEventBus struct {
	mu          sync.RWMutex
	subscribers []chan Event
}

// NewInProcessEventBus 创建默认进程内事件总线。
func NewInProcessEventBus() *InProcessEventBus {
	return &InProcessEventBus{}
}

// Emit 向所有订阅者发布事件。
func (b *InProcessEventBus) Emit(event Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, ch := range b.subscribers {
		ch <- event
	}
}

// Subscribe 订阅所有事件，返回只读通道和取消函数。
func (b *InProcessEventBus) Subscribe() (<-chan Event, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	ch := make(chan Event)
	b.subscribers = append(b.subscribers, ch)
	cancel := func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		for i, c := range b.subscribers {
			if c == ch {
				b.subscribers = append(b.subscribers[:i], b.subscribers[i+1:]...)
				break
			}
		}
		close(ch)
	}
	return ch, cancel
}

// Close 关闭事件总线，关闭所有订阅者通道。后续 Emit 变为空操作。
func (b *InProcessEventBus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range b.subscribers {
		close(ch)
	}
	b.subscribers = nil
}

// NopEventBus 是空操作事件总线：所有 Emit 不做任何事，Subscribe 返回已关闭通道。
// 当 Agent 未配置事件总线时，用它兜底避免 nil 调用。
type NopEventBus struct{}

func (NopEventBus) Emit(Event)                              {}
func (NopEventBus) Subscribe() (<-chan Event, func())       { ch := make(chan Event); close(ch); return ch, func() {} }
func (NopEventBus) Close()                                  {}
