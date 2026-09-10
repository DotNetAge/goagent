package goagent

import (
	"context"
	"encoding/json"

	"github.com/DotNetAge/gochat/core"
)

// ToolCall 工具接口：声明元信息（与 OpenAI NativeTool 协议对齐），并带 Execute 执行方法。
// Parameters 返回 JSON Schema 形式的参数定义（与 gochat core.Tool.Parameters 同构，可直接转换）。
type ToolCall interface {
	// Name 工具名称（模型调用时的标识）
	Name() string
	// Description 工具用途说明，帮助模型决定何时调用
	Description() string
	// Parameters JSON Schema 形式的参数定义（json.RawMessage）
	Parameters() json.RawMessage
	// Execute 执行工具调用，args 为模型生成的参数（json.RawMessage）
	Execute(ctx context.Context, args json.RawMessage) (string, error)
}

// StopReason 一次思考循环的停止原因
type StopReason int

const (
	StopFinished      StopReason = iota // 思考完成，输出最终答案
	StopToolLoop                        // 思考暂停，转入工具执行
	StopMaxIterations                   // 达到最大思考轮数
	StopError                           // 出错终止
	StopSuspended                       // 工具需要外部输入，循环挂起等待
)

// String 返回 StopReason 的可读名称。
func (r StopReason) String() string {
	switch r {
	case StopFinished:
		return "finished"
	case StopToolLoop:
		return "tool_loop"
	case StopMaxIterations:
		return "max_iterations"
	case StopError:
		return "error"
	case StopSuspended:
		return "suspended"
	default:
		return "unknown"
	}
}

// Callbacks 流式事件回调（ChatStream 使用）
type Callbacks struct {
	// ThinkCallback 模型思考增量（思考过程与回答文本，逐段触发）
	ThinkCallback func(delta string)
	// BeforeToolExec 工具执行前触发，tool 为对应的工具实现，args 为模型生成的参数
	BeforeToolExec func(tool ToolCall, args json.RawMessage)
	// AfterToolExec 工具执行后触发，result 为工具输出，err 为执行错误
	AfterToolExec func(tool ToolCall, result string, err error)
	// Finished 整个对话完成，answer 为最终答案
	Finished func(answer string)
	// OnComplete 整个对话完成（含出错、达到轮数）时触发，messages 为完整的对话上下文（原始 Message 列表）
	OnComplete func(messages []core.Message)
	// Stop 一次思考循环结束时触发
	Stop func(reason StopReason)
}
