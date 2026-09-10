package goagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrNeedExternalInput 是 ToolCall.Execute 返回的哨兵错误：表示工具需要外部输入
// （用户授权、用户回答等）才能继续执行。
//
// 调用方用 errors.Is 判断：
//
//	if errors.Is(err, ErrNeedExternalInput) { ... }
//
// 同时可用 errors.As 取出具体的挂起请求：
//
//	var req *ExternalInputRequest
//	if errors.As(err, &req) { ... }
var ErrNeedExternalInput = errors.New("goagent: 工具需要外部输入")

// ExternalInputRequest 描述一次挂起请求的上下文。
// 循环捕获后会发射 EvSuspend 事件，让调用方决定如何响应用户。
// 外部（如 goharness）应将挂起原因、展示文本、等待方式等填在 Details 里。
type ExternalInputRequest struct {
	// ToolName 触发挂起的工具名称。
	ToolName string
	// ToolCallID 本次挂起对应的 tool_call.id（供恢复时匹配配对）。
	ToolCallID string
	// Arguments 本次工具调用的原始参数。
	Arguments json.RawMessage
	// Kind 挂起类别："permission"（安全授权）或 "ask_user"（对话式交互）。
	Kind string
	// Details 是外部自定义的附加信息（如 PermissionRequired 的 scope、
	// AskUser 的问题列表等），类型由调用方决定。
	Details any
}

// Error 实现 error 接口。
func (r *ExternalInputRequest) Error() string {
	if r.ToolName == "" {
		return ErrNeedExternalInput.Error()
	}
	return fmt.Sprintf("goagent: 工具 %q 需要外部输入", r.ToolName)
}

// Unwrap 让 errors.Is 能匹配 ErrNeedExternalInput。
func (r *ExternalInputRequest) Unwrap() error {
	return ErrNeedExternalInput
}

// NewPermissionRequest 创建一个 kind="permission" 的 ExternalInputRequest。
// 工具在执行前需要安全授权时调用：
//
//	return nil, NewPermissionRequest(ctx, "Bash", call.ID, args, "sandbox: /etc/passwd 不在白名单")
func NewPermissionRequest(ctx context.Context, toolName, toolCallID string, args json.RawMessage, details any) *ExternalInputRequest {
	return &ExternalInputRequest{
		ToolName:   toolName,
		ToolCallID: toolCallID,
		Arguments:  args,
		Kind:       "permission",
		Details:    details,
	}
}

// NewAskUserRequest 创建一个 kind="ask_user" 的 ExternalInputRequest。
// 工具需要用户补充信息时调用。
func NewAskUserRequest(ctx context.Context, toolName, toolCallID string, args json.RawMessage, details any) *ExternalInputRequest {
	return &ExternalInputRequest{
		ToolName:   toolName,
		ToolCallID: toolCallID,
		Arguments:  args,
		Kind:       "ask_user",
		Details:    details,
	}
}
