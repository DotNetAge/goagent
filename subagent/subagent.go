// Package subagent 提供 SubAgent 协议与工具（Harness 通用概念）。
//
// 工具名 "subagent" 及参数 schema 对外保持稳定——即使各宿主内部受理实现不同，
// 对模型与跨实现协作而言这是统一契约。
//
// 分层定位：本包不是 goagent 内核组件——内核对 SubAgent 零感知，本工具与
// Bash 等普通工具走完全相同的执行路径。创建权上收宿主：宿主实现
// SubAgentDispatcher 受理请求并创建新的运行实例（经控制平面登记）。
// 本包仅依赖标准库（工具契约按结构化类型匹配，无需导入 goagent），
// 可被 goharness / 示例等任意宿主侧代码复用。
package subagent

import (
	"context"
	"encoding/json"
	"fmt"
)

// SubAgentRequest 是父 Agent 派发子任务时发出的请求。
// 子 Agent 由宿主受理创建（创建权上收宿主：Runtime 不自我派生）。
type SubAgentRequest struct {
	// AgentName 目标 Agent 配置名。
	AgentName string `json:"agent_name"`
	// Task 任务描述。
	Task string `json:"task"`
	// SessionID 可选：显式复用子会话（延续上下文）；空 = 由宿主按复用策略决定。
	SessionID string `json:"session_id,omitempty"`
}

// SubAgentReceipt 是受理回执：Submit 同步返回，LLM 依赖它获得跟踪句柄或立即纠错。
type SubAgentReceipt struct {
	// Accepted 是否受理。
	Accepted bool
	// TaskID 受理后的任务跟踪句柄（CollectResults 据此收集）。
	TaskID string
	// Reason 拒绝原因（Accepted=false 时）。
	Reason string
}

// SubAgentDispatcher 由宿主实现并注入 SubAgent 工具。
// Dispatcher 实例与发起方 Runtime 绑定（宿主构造父 Agent 时注入），
// Sponsor 身份由绑定关系携带——工具与请求都不传递发起者信息。
type SubAgentDispatcher interface {
	// Submit 受理派发请求并同步返回回执。
	// 受理过程失败（非策略性拒绝，如内部错误）时返回 error。
	Submit(ctx context.Context, req SubAgentRequest) (SubAgentReceipt, error)
}

// SubAgentToolName 是 SubAgent 工具的注册名（Harness 通用概念，对外保持稳定）。
const SubAgentToolName = "subagent"

// SubAgentTool 是 SubAgent 原语的工具形态：解析参数 → Submit → 同步回执 → 返回 running 存根。
// 工具只做"发请求、等回执"，零产品知识：不知道 Session 的存在，
// 也不携带 Sponsor 身份（由宿主与 Dispatcher 的绑定关系决定）。
type SubAgentTool struct {
	dispatcher SubAgentDispatcher
}

// NewSubAgentTool 创建绑定到指定 Dispatcher 的 SubAgent 工具。
func NewSubAgentTool(dispatcher SubAgentDispatcher) *SubAgentTool {
	return &SubAgentTool{dispatcher: dispatcher}
}

func (t *SubAgentTool) Name() string { return SubAgentToolName }

func (t *SubAgentTool) Description() string {
	return "派发子任务给另一个 Agent 异步执行。调用后立即同步返回受理回执（含跟踪句柄），子任务在后台独立运行；之后用收集工具按跟踪句柄等待并汇总结果。"
}

func (t *SubAgentTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"agent_name": {"type": "string", "description": "目标 Agent 配置名"},
			"task": {"type": "string", "description": "要委托给子 Agent 的任务描述，需自包含（子 Agent 看不到本对话的上下文）"},
			"session_id": {"type": "string", "description": "可选：显式复用某个已存在的子会话以延续其上下文；留空则由系统按复用策略决定"}
		},
		"required": ["agent_name", "task"]
	}`)
}

// Execute 同步派发：校验参数 → Submit → 把回执转为给模型的文本反馈。
// 同步失败反馈（拒绝/错误）直接作为工具结果返回（而非 error），让模型依赖
// 回执文本自纠；受理成功时返回 running 存根（跟踪句柄）。
func (t *SubAgentTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var req SubAgentRequest
	if err := json.Unmarshal(args, &req); err != nil {
		return fmt.Sprintf("派发被拒绝：参数解析失败（%v）", err), nil
	}
	// 参数校验前移到同步阶段：模型立即得到纠错反馈，而不是等子任务失败
	if req.AgentName == "" {
		return "派发被拒绝：agent_name 不能为空", nil
	}
	if req.Task == "" {
		return "派发被拒绝：task 不能为空", nil
	}

	receipt, err := t.dispatcher.Submit(ctx, req)
	if err != nil {
		return fmt.Sprintf("派发失败：%v", err), nil
	}
	if !receipt.Accepted {
		return fmt.Sprintf("派发被拒绝：%s", receipt.Reason), nil
	}
	return fmt.Sprintf("子任务已受理并开始运行。跟踪句柄：%s。请用收集工具等待该句柄的结果，不要重复派发。", receipt.TaskID), nil
}
