package goagent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
)

// ToolExecutor 管理已注册工具并按名称执行的抽象接口。
// 默认实现为简单的顺序执行，外部（如 goharness）可替换为带权限门控、超时、
// 并发调度等增强能力的实现。
type ToolExecutor interface {
	// Execute 按名称查找工具并执行，返回结果文本和错误。
	// 未注册的工具返回错误提示，让调用方可自行决定如何向模型反馈。
	Execute(ctx context.Context, name string, args json.RawMessage) (string, error)
}

// ToolEnumerator 是 ToolExecutor 的可选扩展接口。
// 实现了此接口的执行器可以把自己持有的工具列表暴露给 agent，
// 否则 agent 只能用 Tools() 注册到 Agent 的工具集合作为 fallback。
//
// DefaultToolExecutor 实现了它（直接返回内部注册表）。
// 自定义 executor 可选实现——如果 executor 自己通过 Register 管理工具，
// 就应该同时实现 ToolEnumerator 让 agent 知道哪些工具可用。
type ToolEnumerator interface {
	// Tools 返回执行器当前持有的所有工具。
	Tools() []ToolCall
}

// DefaultToolExecutor 是 ToolExecutor 的默认实现：内部维护一个工具注册表，
// 按名称直接调用 ToolCall.Execute。
//
// 使用 Register 添加工具，或通过 NewDefaultToolExecutor(tools...) 一次性注入。
type DefaultToolExecutor struct {
	mu    sync.RWMutex
	tools map[string]ToolCall
}

// NewDefaultToolExecutor 创建默认工具执行器并预注册给定工具。
func NewDefaultToolExecutor(tools ...ToolCall) *DefaultToolExecutor {
	e := &DefaultToolExecutor{
		tools: make(map[string]ToolCall, len(tools)),
	}
	for _, t := range tools {
		e.tools[t.Name()] = t
	}
	return e
}

// Register 向执行器添加一个工具。重名工具会被覆盖。
func (e *DefaultToolExecutor) Register(tool ToolCall) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.tools[tool.Name()] = tool
}

// Get 按名称查找已注册工具，未找到返回 nil。
func (e *DefaultToolExecutor) Get(name string) ToolCall {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.tools[name]
}

// Tools 返回所有已注册工具的切片。按工具名排序保证稳定性。
func (e *DefaultToolExecutor) Tools() []ToolCall {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]ToolCall, 0, len(e.tools))
	for _, t := range e.tools {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Name() < out[j].Name()
	})
	return out
}

// Execute 按名称查找并执行工具。未注册工具返回明确错误。
func (e *DefaultToolExecutor) Execute(ctx context.Context, name string, args json.RawMessage) (string, error) {
	e.mu.RLock()
	tool, ok := e.tools[name]
	e.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("goagent: 未注册工具 %q", name)
	}
	return tool.Execute(ctx, args)
}
