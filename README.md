# goagent

迷你 agent 引擎：实现对大模型的多轮思考-工具执行循环。基于 [gochat](https://github.com/DotNetAge/gochat) 构建，默认对接本机 Ollama 的 `minicpm-v4.6:latest` 模型。

## 特性

- **多轮思考循环**：模型可发起多次工具调用，工具结果回传后继续思考，直到给出最终答案
- **流式与阻塞双模式**：`Chat` 阻塞返回、`ChatStream` 流式输出思考增量与各阶段事件
- **工具协议对齐 OpenAI NativeTool**：`ToolCall` 接口声明 `Name / Description / Parameters`，带 `Execute` 方法
- **可注入的核心依赖**：LLM 客户端、工具执行器、循环钩子、事件总线，全部通过 `With*` 选项注入
- **工具挂起续跑**：工具返回 `ErrNeedExternalInput` 即可挂起循环等待外部输入（用户授权、用户回答），支持对话式交互 agent
- **循环钩子系统**：`LoopHook` 在 `BeforeLLM` / `AfterLLM` 插入自定义逻辑，按优先级排序，支持中止
- **事件总线**：`EventBus` + 12 种事件类型，覆盖从 token 级增量到对话完整生命周期
- **Token Usage 感知**：每轮 LLM 调用后发射 `EvTokenUsage` 事件，携带精确 token 消耗和耗时
- **精确 finishReason**：从 Provider 响应直接取 `stop / tool_calls / length / content_filter`，不再靠 ToolCalls 有无间接推断
- **上下文连续**：`History` 注入历史对话、`Images` 附加图片输入
- **配置灵活**：环境变量 + 编程式 `With*` 选项，支持 Ollama / OpenAI / DeepSeek / Anthropic

## 引入

```bash
go get github.com/DotNetAge/gochat@v0.2.9
```

## 快速开始

```go
package main

import (
	"fmt"

	"goagent"
	"goagent/tools"
)

func main() {
	answer, err := goagent.Ask("当前目录下有哪些文件？").
		Tools(tools.Bash{}).
		Chat()
	if err != nil {
		panic(err)
	}
	fmt.Println(answer)
}
```

## 配置

默认通过环境变量读取，也可用 `Config(With*...)` 编程覆盖（优先级更高）：

| 环境变量           | 默认值                   | 说明                        |
| ------------------ | ------------------------ | --------------------------- |
| `GOAGENT_BASE_URL` | `http://localhost:11434` | 模型端点基础地址            |
| `GOAGENT_API_KEY`  | `ollama`                 | API 密钥（Ollama 任意占位） |
| `GOAGENT_MODEL`    | `minicpm-v4.6:latest`    | 模型名称                    |

```go
a := goagent.Ask("你好").
	Config(
		goagent.WithModel("qwen3:8b"),
		goagent.WithTemperature(0.2),
		goagent.WithMaxIterations(10),
		goagent.WithTimeout(5*time.Minute),
		goagent.WithClientType(gochat.OpenAIClient), // 其他 OpenAI 兼容服务
	)
```

### 可用 Option 一览

| Option | 说明 |
|--------|------|
| `WithBaseURL` | 模型端点基础地址 |
| `WithAPIKey` | API 密钥 |
| `WithModel` | 模型名称 |
| `WithTemperature` | 采样温度 |
| `WithTimeout` | HTTP 超时 |
| `WithMaxIterations` | 多轮思考最大轮数 |
| `WithClientType` | gochat 客户端类型 |
| `WithLLMClient` | 注入自定义 `core.Client`（跳过默认工厂） |
| `WithToolExecutor` | 注入自定义 `ToolExecutor` |
| `WithLoopHooks` | 注册循环钩子（可多个，按 Priority 排序） |
| `WithEventBus` | 注入事件总线实例 |

## 事件总线

`EventBus` 是 Agent 唯一的事件输出通道——**所有事件（流式增量、工具执行、挂起、token 消耗、循环结束）都走 EventBus**。默认实现是进程内无缓冲通道，外部可替换为带过滤、缓冲或跨进程传播的实现（如 WebSocket 广播）。

> Callbacks 是 EventBus 的**简化包装/向后兼容层**（函数指针回调），Agent 内部统一走 `bus.Emit(Event{Type, Data})`，同时同步触发 Callbacks。新功能和生产级场景优先用 EventBus。

### 事件类型一览

| 事件类型 | Data 类型 | 说明 |
|----------|----------|------|
| `EvThinkingDelta` | `string` | 模型思考增量（流式） |
| `EvContentDelta` | `string` | 模型回答文本增量（流式） |
| `EvToolUseDelta` | `core.ToolCallDelta` | 工具调用参数增量（流式） |
| `EvThinkingDone` | `nil` | 思考阶段完成 |
| `EvTokenUsage` | `*TokenUsageEvent` | 每轮 LLM 调用后的 token 消耗 |
| `EvLoopEnd` | `*LoopEndData` | 一轮完整的 Think-Act 循环结束 |
| `EvToolExecStart` | `*toolExecData` | 工具开始执行 |
| `EvToolExecEnd` | `*toolExecEndData` | 工具执行完成 |
| `EvSuspend` | `*ExternalInputRequest` | 工具需要外部输入，循环挂起 |
| `EvFinalAnswer` | `string` | 最终答案 |
| `EvStop` | `StopReason` | 循环停止 |
| `EvComplete` | `[]core.Message` | 对话完成（无论成功、出错还是达到轮数） |

### 消费事件（生产推荐）

```go
bus := goagent.NewInProcessEventBus()
defer bus.Close()

ch, cancel := bus.Subscribe()
defer cancel()

go func() {
	for ev := range ch {
		switch ev.Type {
		case goagent.EvContentDelta:
			fmt.Print(ev.Data.(string))           // 回答文本增量
		case goagent.EvThinkingDelta:
			fmt.Fprint(os.Stderr, ev.Data.(string)) // 思考增量
		case goagent.EvSuspend:
			req := ev.Data.(*goagent.ExternalInputRequest)
			fmt.Printf("工具 %s 需要外部输入 (kind=%s)\n", req.ToolName, req.Kind)
		case goagent.EvTokenUsage:
			te := ev.Data.(*goagent.TokenUsageEvent)
			fmt.Printf("第 %d 轮: token=%v, 耗时=%v\n", te.Iteration, te.Usage, te.Duration)
		case goagent.EvStop:
			fmt.Printf("\n循环停止: %v\n", ev.Data.(goagent.StopReason))
		}
	}
}()

answer, err := goagent.Ask("查看当前目录，统计文件数量").
	Config(goagent.WithEventBus(bus)).
	Tools(tools.Bash{}).
	Chat()
```

`bus.Close()` 在 Agent 循环退出时会自动被调用（`run()` 开头有 `defer bus.Close()`），所以外部显式 `defer bus.Close()` 是安全的双重保险。

### 自定义 EventBus（WebSocket 广播示例）

上层产品（如 goharness）可替换 EventBus 实现，把事件广播到 WebSocket：

```go
type WebSocketEventBus struct { /* ws 连接管理 */ }

func (b *WebSocketEventBus) Emit(e goagent.Event) {
	data, _ := json.Marshal(e)
	b.ws.Broadcast(data)
}
func (b *WebSocketEventBus) Subscribe() (<-chan goagent.Event, func()) { ... }
func (b *WebSocketEventBus) Close() { /* 关闭连接 */ }

goagent.Ask("...").
	Config(goagent.WithEventBus(&WebSocketEventBus{})).
	Chat()
```

## 流式对话

流式对话同样走 EventBus 消费增量事件（`EvThinkingDelta` / `EvContentDelta` / `EvToolUseDelta`），上文示例已覆盖。

如果只需要最简单的函数指针回调，可以用 `Callbacks`——它是 EventBus 的桥接层，每条事件都会同步触发对应的函数：

```go
err := goagent.Ask("查看当前目录，统计文件数量").
	Tools(tools.Bash{}).
	ChatStream(goagent.Callbacks{
		ThinkCallback:  func(delta string) { fmt.Print(delta) },
		BeforeToolExec: func(tool goagent.ToolCall, args json.RawMessage) {
			fmt.Printf("\n正在执行 %s, %s\n", tool.Name(), args)
		},
		AfterToolExec: func(tool goagent.ToolCall, result string, err error) {
			fmt.Printf("执行结束: %s, 结果: %s\n", tool.Name(), result)
		},
		Finished: func(answer string) { fmt.Println("\n", answer) },
		Stop:     func(reason goagent.StopReason) {},
	})
```

> `Callbacks` 与 EventBus 可以同时使用——两者不互斥。Agent 内部先 `bus.Emit(Event)` 再同步调用函数指针。`OnComplete` 回调和 `EvComplete` 事件等价。

流式模式默认开启模型思考（OpenAI 兼容端点发送 `enable_thinking`）。

## 系统提示词

`System` 设置系统提示词，作为模型的顶层指令（角色设定、行为约束等）。工具定义仍通过 tools 协议传给模型，无需在系统提示词中重复描述：

```go
answer, _ := goagent.Ask("帮我写一份周报").
	System("你是一名资深的项目管理者，擅长简洁清晰的汇报，回复控制在 200 字以内。").
	Chat()
```

## 持续多轮回话

`History` 注入上一轮对话的原始 `core.Message` 数据（含思考内容与工具调用），即可接着讨论。配合 `Complete`（阻塞式）或监听 `EvComplete` 事件 / `Callbacks.OnComplete`（流式，向后兼容）获取完整上下文，就能形成"多轮 → 存上下文 → 再续聊"的闭环：

```go
var context []core.Message
answer1, _ := goagent.Ask("帮我查看当前目录下的文件").
	Tools(tools.Bash{}).
	Complete(func(messages []core.Message) {
		context = messages
	}).
	Chat()

answer2, _ := goagent.Ask("刚才那个文件内容是什么？").
	History(context).
	Tools(tools.Bash{}).
	Chat()
```

要点：

- `Complete` / `OnComplete` 在对话**无论成功、出错还是达到最大轮数**时都会触发
- 序列包含 `assistant` 消息的 `tool_calls` 与 `role=tool` 结果消息，原样交给下一轮 `History`，模型能准确接续之前的工具执行过程
- `History` 传入的消息排列在当前问题之前，可连续拼接多轮形成长对话

## 图片输入

`Images` 附加本地图片路径，随当前问题一起发给模型（多模态模型）：

```go
answer, _ := goagent.Ask("这张图里有什么？").
	Images("photo.png").
	Chat()
```

## 工具挂起续跑（对话式 Agent）

工具执行时如果需要外部输入（用户授权、用户回答等），可让 `Execute` 返回 `ErrNeedExternalInput` 哨兵错误，循环会自动挂起并发射 `EvSuspend` 事件：

```go
func (b *Bash) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct{ Command string `json:"command"` }
	if json.Unmarshal(args, &p) != nil {
		return "", nil
	}
	// 模拟安全检查：包含危险命令时需要用户授权
	if isDangerous(p.Command) {
		return "", NewPermissionRequest(ctx, b.Name(), "call-001", args, map[string]any{
			"scope":   "sandbox",
			"command": p.Command,
			"reason":  "命令不在安全白名单内",
		})
	}
	// 正常执行...
}
```

循环挂起后：

- `Chat()` 返回 `("", nil)`，`LastStopReason()` 返回 `StopSuspended`
- `EvSuspend` 事件携带 `*ExternalInputRequest`（ToolName / ToolCallID / Kind / Details）
- 调用方渲染授权对话框，用户同意后把 `role=tool` 结果消息补入 `History`，再次 `Chat()` 即可恢复
- 恢复后的循环会继续执行剩余工具调用或进入下一轮 LLM 思考

`NewPermissionRequest` / `NewAskUserRequest` 是两个便捷构造函数。`Kind` 字段区分挂起类别：

- `"permission"` — 安全授权场景（沙箱检查、白名单外文件访问等）
- `"ask_user"` — 对话式交互场景（补充参数、确认选项等）

## 循环钩子

实现 `LoopHook` 接口即可在循环关键节点插入逻辑。钩子按 `Priority` 升序执行，`IsTerminal()` 的返回可中止循环。

```go
type DebugHook struct{ log []string }

func (h *DebugHook) Priority() int                          { return 10 }
func (h *DebugHook) BeforeLLM(ctx context.Context, in BeforeLLMInput) HookResult {
	h.log = append(h.log, fmt.Sprintf("LLM 调用前，消息数=%d", len(in.Messages)))
	return HookResult{}
}
func (h *DebugHook) AfterLLM(ctx context.Context, in AfterLLMInput) HookResult {
	h.log = append(h.log, fmt.Sprintf("LLM 返回，finishReason=%s", in.FinishReason))
	return HookResult{}
}
func (h *DebugHook) Abort(ctx context.Context, reason string) {
	h.log = append(h.log, "循环结束: "+reason)
}
```

注册：

```go
hook := &DebugHook{}
answer, _ := goagent.Ask("你好").
	Config(goagent.WithLoopHooks(hook)).
	Chat()
```

### HookResult 语义

| 字段 | 说明 |
|------|------|
| `Abort=true` | 循环立即中止，视为正常终止（`StopFinished`） |
| `Error!=nil` | 循环以错误终止（`StopError`） |
| 两者都设 | `Error` 优先（视为错误终止） |
| 都不设 | 钩子正常返回，继续循环 |

### Abort 执行顺序

所有钩子的 `Abort` 按 LIFO（与 Priority 相反）执行，便于反向清理。`reason` 参数携带终止原因：

- `"error:..."` — 因错误终止
- `"hook_abort:..."` — 被钩子主动中止
- `"max_iterations"` — 达到最大轮数

## 扩展工具

实现 `ToolCall` 接口即可。可参考内置的 [tools/bash.go](tools/bash.go)。

```go
type Echo struct{}

func (Echo) Name() string        { return "echo" }
func (Echo) Description() string { return "回显输入的文本" }
func (Echo) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {"text": {"type": "string"}},
		"required": ["text"]
	}`)
}
func (Echo) Execute(_ context.Context, args json.RawMessage) (string, error) {
	var p struct{ Text string `json:"text"` }
	if err := json.Unmarshal(args, &p); err != nil {
		return "", err
	}
	return p.Text, nil
}
```

## 自定义 LLM 客户端 / 工具执行器

goagent 依赖接口不依赖具体实现。外部可通过 `WithLLMClient` 和 `WithToolExecutor` 注入自定义实现：

```go
// 带重试的 LLM 客户端
type RetryingClient struct{ inner core.Client }

func (c *RetryingClient) Chat(ctx context.Context, msgs []core.Message, opts ...core.Option) (core.Response, error) {
	for attempt := 0; attempt < 3; attempt++ {
		resp, err := c.inner.Chat(ctx, msgs, opts...)
		if err == nil || !isRetryable(err) {
			return resp, err
		}
		time.Sleep(time.Second << attempt)
	}
	return core.Response{}, err
}

// 带权限门控的工具执行器
type SandboxedExecutor struct {
	*DefaultToolExecutor
	sandbox *sandbox.Sandbox
}

func (e *SandboxedExecutor) Execute(ctx context.Context, name string, args json.RawMessage) (string, error) {
	if err := e.sandbox.Check(name, args); err != nil {
		return "", NewPermissionRequest(ctx, name, "", args, err.Error())
	}
	return e.DefaultToolExecutor.Execute(ctx, name, args)
}
```

## 默认 CLI

```bash
go run ./cmd/goagent "当前目录下有哪些文件？"
echo "你好" | go run ./cmd/goagent
```

## 停止原因

| 常量 | 值 | 说明 |
|------|---|------|
| `StopFinished` | 0 | 思考完成，输出最终答案 |
| `StopToolLoop` | 1 | 思考暂停，转入工具执行 |
| `StopMaxIterations` | 2 | 达到最大思考轮数 |
| `StopError` | 3 | 出错终止 |
| `StopSuspended` | 4 | 工具需要外部输入，循环挂起等待 |

## 目录结构

```
goagent/
├── agent.go          # 核心引擎：Ask / Config / Tools / Chat / ChatStream / LastStopReason
├── config.go         # Config 结构体 + 环境变量 + With* Option
├── tool.go           # ToolCall 接口、Callbacks、StopReason
├── llm.go            # NewDefaultLLMClient 工厂
├── executor.go       # ToolExecutor / ToolEnumerator / DefaultToolExecutor
├── hooks.go          # LoopHook / LoopResult / sortHooks
├── events.go         # EventBus + 12 种事件类型 + 默认实现
├── suspend.go        # ErrNeedExternalInput / ExternalInputRequest / 构造函数
├── tools/            # 内置工具
│   └── bash.go       # Shell 命令执行
├── cmd/goagent/      # 默认 CLI
└── examples/basic/   # 基础示例
```

## 架构设计

goagent 按整洁架构分层，核心层不向外依赖：

```
┌─────────────────────────────────────────────────┐
│ goagent (核心层)                                  │
│   Agent.run() → 依赖 4 个抽象接口：              │
│     ├─ core.Client       (来自 gochat/core)     │
│     ├─ ToolExecutor      (goagent 定义)          │
│     ├─ LoopHook          (goagent 定义)          │
│     └─ EventBus          (goagent 定义)          │
└─────────────────────────────────────────────────┘
         ▲              ▲              ▲
         │ 实现          │ 实现          │ 实现
    ┌────┴────┐    ┌────┴────┐    ┌────┴────┐
    │ gochat  │    │goharness│    │goharness│
    │具体客户端│    │沙箱工具  │    │RAG钩子   │
    └─────────┘    └─────────┘    └─────────┘
```

外部产品可替换任一组件而不影响循环骨架。详见 [DESIGN.md](DESIGN.md)。
