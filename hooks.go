package goagent

import (
	"context"

	"github.com/DotNetAge/gochat/core"
)

// HookResult 是 LoopHook 的返回值，指示是否需要中止循环。
type HookResult struct {
	// Abort 为 true 时，循环立即终止，AbortReason 会作为终止原因写入事件。
	Abort bool
	// AbortReason 是中止循环的可读原因（Abort=true 时必填）。
	AbortReason string
	// Error 非 nil 时，循环以错误终止。
	Error error
}

// IsTerminal 判断钩子是否请求终止循环。
func (r HookResult) IsTerminal() bool {
	return r.Abort || r.Error != nil
}

// BeforeLLMInput 是 BeforeLLM 钩子的入参，携带本轮 LLM 调用的完整上下文。
type BeforeLLMInput struct {
	// Iteration 当前是第几次迭代（0-based）。
	Iteration int
	// Messages 即将发送给 LLM 的完整消息序列（含 system + 历史 + 当前问题）。
	Messages []core.Message
	// Tools 本轮可用的工具定义。
	Tools []core.Tool
}

// AfterLLMInput 是 AfterLLM 钩子的入参，携带本轮 LLM 调用的响应摘要。
type AfterLLMInput struct {
	// Iteration 当前是第几次迭代（0-based）。
	Iteration int
	// ResponseContent LLM 返回的文本内容（不含思考和工具调用）。
	ResponseContent string
	// Reasoning LLM 的思考过程文本。
	Reasoning string
	// FinishReason LLM 返回的结束原因（stop / tool_calls / length 等）。
	FinishReason string
	// ToolCalls LLM 发起的工具调用列表。
	ToolCalls []core.ToolCall
}

// LoopHook 是思考循环的扩展契约，允许在 LLM 调用前后插入自定义逻辑。
//
// 优先级（Priority）决定钩子执行顺序，数值越小越先执行。
// 任一回调返回 IsTerminal()=true 的 HookResult 会立即终止循环。
type LoopHook interface {
	// Priority 返回钩子的执行优先级，数值越小越先执行。
	Priority() int
	// BeforeLLM 在 LLM 调用前运行，可检查/修改输入或中止循环。
	BeforeLLM(ctx context.Context, input BeforeLLMInput) HookResult
	// AfterLLM 在 LLM 返回后运行，可检查响应或中止循环。
	AfterLLM(ctx context.Context, input AfterLLMInput) HookResult
	// Abort 在循环被终止（正常结束 / 错误 / 钩子中止）时调用，
	// 按与注册顺序相反的 LIFO 执行，便于清理资源。
	Abort(ctx context.Context, reason string)
}

// sortHooks 按 Priority 升序排序钩子切片。优先级相同的保持原有顺序。
func sortHooks(hooks []LoopHook) []LoopHook {
	if len(hooks) <= 1 {
		return hooks
	}
	// 插入排序（钩子数量通常很少，性能无关）
	out := make([]LoopHook, len(hooks))
	copy(out, hooks)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1].Priority() > out[j].Priority(); j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}
