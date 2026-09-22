// SubAgent 多 Agent 协作示例：宿主实现 Dispatcher 受理派发，父 Agent 委托子 Agent 完成
// 需要工具执行的任务。运行前提：本地 LLM 端点可用（默认 Ollama，环境变量同基础示例）。
//
// 本示例演示"创建权上收宿主"的完整闭环：
//
//	父 Agent 调用 subagent 工具 → HostDispatcher.Submit 受理（登记 Pending）
//	→ 宿主构造子 Agent（goroutine 归宿主，生命周期不继承父工具调用的 ctx）
//	→ 子 Agent 独立运行直至终态 → 宿主收尾 Unregister。
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"goagent"
	"goagent/subagent"
	"goagent/tools"
)

// hostDispatcher 是宿主受理方的最小实现：
// Submit 登记 + 构造子 Agent 并在后台运行；Wait 按跟踪句柄等待全部落定。
type hostDispatcher struct {
	// agents 是本示例注册的 Agent 配置（模拟宿主的 Agent 配置库）。
	agents map[string]func(task string) *goagent.Agent
}

func newHostDispatcher() *hostDispatcher {
	return &hostDispatcher{agents: make(map[string]func(task string) *goagent.Agent)}
}

// registerAgent 注册一个可被派发的 Agent 配置（子 Agent 同样携带 subagent 工具，支持嵌套委托）。
func (h *hostDispatcher) registerAgent(name string, build func(task string) *goagent.Agent) {
	h.agents[name] = build
}

func (h *hostDispatcher) Submit(ctx context.Context, req subagent.SubAgentRequest) (subagent.SubAgentReceipt, error) {
	build, ok := h.agents[req.AgentName]
	if !ok {
		return subagent.SubAgentReceipt{}, fmt.Errorf("未知的 Agent 配置 %q", req.AgentName)
	}

	// 受理即登记（Pending，控制平面立即可见），实例 ID 作为跟踪句柄
	manager := goagent.DefaultRuntimeManager()
	rt, err := manager.Register("")
	if err != nil {
		return subagent.SubAgentReceipt{}, err
	}
	id := rt.ID()

	// 宿主编排生命周期：goroutine 归宿主，ctx 不继承父工具调用
	child := build(req.Task).Config(goagent.WithRuntimeID(id))
	go func() {
		child.ChatCtx(context.Background())
		// 收尾：终态读取后从登记表移除
		manager.Unregister(id)
	}()

	return subagent.SubAgentReceipt{Accepted: true, SessionID: id}, nil
}

func (h *hostDispatcher) Wait(ctx context.Context, sessionIDs []string) map[string]error {
	manager := goagent.DefaultRuntimeManager()
	failures := make(map[string]error)
	for _, id := range sessionIDs {
		rt, ok := manager.Get(id)
		if !ok {
			failures[id] = fmt.Errorf("实例不存在")
			continue
		}
		select {
		case <-rt.Done():
			if st := rt.Status(); st != goagent.StatusCompleted {
				failures[id] = fmt.Errorf("子任务异常终态: %v", st)
			}
		case <-ctx.Done():
			return failures
		}
	}
	return failures
}

func main() {
	question := "请派发一个子 Agent 查看当前目录的文件列表并汇报，然后你自己总结汇报内容"
	if len(os.Args) > 1 && os.Args[1] != "" {
		question = os.Args[1]
	}
	fmt.Println("问题:", question)
	fmt.Print("思考: ")

	host := newHostDispatcher()
	// 注册子 Agent 配置：真实 LLM + 基础工具 + subagent 工具（嵌套委托能力）
	host.registerAgent("file-inspector", func(task string) *goagent.Agent {
		return goagent.Ask(task).
			Config(goagent.WithMaxIterations(10)).
			Tools(tools.Bash{}, subagent.NewSubAgentTool(host))
	})

	err := goagent.Ask(question).
		Config(goagent.WithMaxIterations(10)).
		Tools(tools.Bash{}, subagent.NewSubAgentTool(host)).
		ChatStream(goagent.Callbacks{
			ThinkCallback: func(delta string) { fmt.Print(delta) },
			AfterToolExec: func(tool goagent.ToolCall, result string, err error) {
				status := "成功"
				if err != nil {
					status = "失败: " + err.Error()
				}
				fmt.Printf("\n[工具] %s (%s)\n", tool.Name(), status)
				if r := strings.TrimSpace(result); r != "" {
					r = strings.ReplaceAll(r, "\n", " ")
					if runes := []rune(r); len(runes) > 120 {
						r = string(runes[:120]) + "..."
					}
					fmt.Printf("      结果: %s\n", r)
				}
			},
			Finished: func(answer string) {
				fmt.Println("\n\n回答:", answer)
			},
		})
	if err != nil {
		fmt.Fprintln(os.Stderr, "\n错误:", err)
		os.Exit(1)
	}
}
