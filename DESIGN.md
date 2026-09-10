# goagent 设计手册

面向 goagent 包的开发者和外部集成者。阐述设计哲学、核心循环骨架、所有可扩展点、以及 goharness 等上层产品如何接入。

## 一、设计哲学

goagent 是 MindX 架构的**核心层组件**，定位是一个"可嵌入的、可扩展的 ReAct 循环引擎"。上层产品（goharness、mindx）在这个引擎之上装配会话、沙箱、RAG、多 Agent 协作等产品能力。

遵循两条准则：

1. **单一职责** — Agent 只做循环编排（LLM 调用 → 工具执行 → 再思考 这个循环），不做任何产品级决策
2. **只向内依赖** — 核心层依赖抽象接口，不依赖具体实现。goagent 定义 `ToolExecutor` / `LoopHook` / `EventBus` 接口，外部实现它们；goagent 本身不 import ollama/openai/anthropic 等具体客户端包（这些在 `llm.go` 的默认工厂里集中导入，Agent 循环从不走到那里）

这意味着 goagent 的循环骨架可以在不同 Provider、不同工具集、不同产品场景下**原样复用**。

## 二、核心循环骨架

`Agent.run()` 的逻辑是经典 ReAct（Reason + Act）循环：

```
for i in [0, MaxIterations):
    ← BeforeLLM hooks
    ← round(): 调 LLM，拿到 finishReason + usage + 消息
    ← AfterLLM hooks （带精确 finishReason）
    ← emit EvTokenUsage
    if len(ToolCalls) == 0:
        ← emit EvFinalAnswer
        ← emitStop(StopFinished)
        return 答案
    
    ← 对每个 tool_call:
        ← emit EvToolExecStart
        ← executor.Execute()
        if errors.Is(execErr, ErrNeedExternalInput):
            ← emit EvSuspend
            ← emitStop(StopSuspended)
            return "", nil      ← 挂起
        ← emit EvToolExecEnd
        ← 把 tool result 作为 role=tool 消息 append
    
    ← emit EvLoopEnd

// 循环耗尽或被中止：
← emitComplete(messages)
← 按原因分类 emitStop + 触发所有 hook.Abort(LIFO)
```

循环的每一步都是固定的，可被钩子和事件总线扩展，但永远不会从循环骨架里分支出产品特有的路径（如自动重试、自动 RAG 注入等）——那些都是 hook 的事。

## 三、可扩展点总览

外部开发人员可以替换或扩展 goagent 的 6 个接口/组件：

```
┌─────────────────────────────────────────────────────────────────┐
│                        Agent.run()                              │
│                                                                 │
│  ┌─────────────┐   ┌───────────────┐   ┌─────────────┐         │
│  │  core.Client │   │ ToolExecutor  │   │  LoopHook   │         │
│  │  (LLM 调用)  │   │ (工具执行)     │   │ (循环扩展)   │         │
│  └─────────────┘   └───────────────┘   └─────────────┘         │
│         ▲                 ▲                 ▲                   │
│         │ 实现            │ 实现            │ 实现               │
│    gochat 包         goharness        goharness                │
│                      (沙箱封装)        (RAG注入/收敛)           │
│                                                                 │
│  ┌─────────────┐   ┌───────────────┐                          │
│  │  EventBus    │   │ ToolEnumerator│ (可选接口)                │
│  │ (事件输出)    │   │ (工具列表)     │                          │
│  └─────────────┘   └───────────────┘                          │
│         ▲                                                       │
│         │ 实现                                                   │
│    goharness (WebSocket 广播)                                    │
│                                                                 │
│  ┌─────────────┐                                               │
│  │ToolCall     │  (工具作者实现)                                │
│  │Execute      │ → 返回 ErrNeedExternalInput 即可挂起循环       │
│  └─────────────┘                                               │
└─────────────────────────────────────────────────────────────────┘
```

以下逐一讲解每个扩展点的用途、接口契约、实现要点。

### 3.1 core.Client（LLM 客户端）

**用途**：与大模型通信。goagent 通过它发起 Chat / ChatStream 调用，拿到消息、finishReason、token 消耗。

**接口来源**：`github.com/DotNetAge/gochat/core.Client`（goagent 不定义，直接复用）。

**注入方式**：
```go
goagent.WithLLMClient(myClient)
```

**实现要点**：
- Agent 会调用 `Chat(ctx, messages, WithTools(...))` 或 `ChatStream(...)`
- 阻塞模式：返回 `core.Response`，包含 `Message`（模型回复）、`FinishReason`（"stop"/"tool_calls"/"length"/"content_filter"）、`Usage`（token 消耗）
- 流式模式：返回 `*core.Stream`，迭代 `EventContent`/`EventThinking`/`EventToolCall`/`EventDone`；`stream.EventDone().FinishReason` 是精确结束原因，`stream.Usage()` 是完整 token 消耗
- Provider 返回的 `FinishReason` 直接决定循环行为——goagent 不再靠 `len(ToolCalls)` 间接推断
- **错误恢复策略**（如 402 欠费重试、pairing error 修复）应该包装在自定义 Client 里，goagent 不处理这些产品级恢复逻辑

**默认实现**：`NewDefaultLLMClient(cfg)` 按 `Config.ClientType` 创建 gochat 具体客户端。生产环境通常直接注入自定义 Client。

### 3.2 ToolExecutor（工具执行器）

**用途**：按名称查找工具并执行。Agent 的工具调用不会直接 `tool.Execute()`，而是走 `executor.Execute(name, args)`。

**接口**：
```go
type ToolExecutor interface {
    Execute(ctx context.Context, name string, args json.RawMessage) (string, error)
}
```

**可选扩展**：
```go
type ToolEnumerator interface {
    Tools() []ToolCall
}
```
实现了 `ToolEnumerator` 的 executor，Agent 会用它拿到完整工具列表发给模型；否则 fallback 到 `Tools()` 注册到 Agent 的工具集合。

**注入方式**：
```go
// 默认执行器（自动合入 Tools() 注册的工具）
exec := goagent.NewDefaultToolExecutor()
exec.Register(myTool)
goagent.WithToolExecutor(exec)

// 或者：用 goharness 自己实现的带沙箱检查的执行器
goagent.WithToolExecutor(sandbox.Executor)
```

**实现要点**：
- `Execute` 返回 `(string, error)`。正常执行返回结果文本；返回 `ErrNeedExternalInput` 哨兵错误表示"需要外部输入"——Agent 会挂起循环并发 `EvSuspend`
- 未注册的工具应返回明确错误（如 `"goagent: 未注册工具 \"bash\""`），让模型能看到错误并自行修正
- 如果需要工具列表暴露给模型，同时实现 `ToolEnumerator`
- DefaultToolExecutor 使用 `sync.RWMutex` 保护内部 map；自定义 executor 应自行考虑并发安全

### 3.3 ToolCall（工具接口）

**用途**：声明元信息 + 执行逻辑。与 OpenAI NativeTool 协议对齐。

**接口**：
```go
type ToolCall interface {
    Name() string
    Description() string
    Parameters() json.RawMessage  // JSON Schema
    Execute(ctx context.Context, args json.RawMessage) (string, error)
}
```

**关键机制 — 挂起续跑**：

工具可以返回 `ErrNeedExternalInput` 让循环挂起。这是 goagent 支持对话式交互的核心机制。

```go
// 场景 1：安全授权
func (t *Bash) Execute(ctx context.Context, args json.RawMessage) (string, error) {
    var p struct{ Command string `json:"command"` }
    json.Unmarshal(args, &p)
    if t.sandbox.IsBlocked(p.Command) {
        return "", NewPermissionRequest(ctx, t.Name(), "", args, map[string]any{
            "scope":   "sandbox",
            "command": p.Command,
        })
    }
    return execShell(ctx, p.Command)
}

// 场景 2：对话式交互（需要用户补充参数）
func (t *CreatePullReq) Execute(ctx context.Context, args json.RawMessage) (string, error) {
    var p struct { Repo string `json:"repo"`; IssueNum int `json:"issue_num"` }
    json.Unmarshal(args, &p)
    if p.IssueNum == 0 {
        return "", NewAskUserRequest(ctx, t.Name(), "", args, AskUserRequest{
            Questions: []Question{{
                Type:   "text",
                Label:  "issue_num",
                Prompt: "请输入 issue 编号",
            }},
        })
    }
    return createPR(p.Repo, p.IssueNum)
}
```

**循环挂起后的恢复**：
1. `Chat()` 返回 `("", nil)`，`LastStopReason()` 返回 `StopSuspended`
2. `EvSuspend` 事件携带 `*ExternalInputRequest`，`Kind` 区分 `"permission"` / `"ask_user"`
3. 外部 UI 渲染授权对话框或输入表单
4. 用户同意/输入后，外部把结果作为 `role=tool` 消息追加到 Agent 的 `History`，再次 `Chat()` 即可恢复循环

### 3.4 LoopHook（循环钩子）

**用途**：在循环关键节点插入自定义逻辑。goagent 提供 3 个钩子点：

| 钩子 | 时机 | 入参 | 典型用途 |
|------|------|------|----------|
| `BeforeLLM` | LLM 调用前 | 当前迭代号、消息序列、工具定义 | RAG 注入、上下文压缩、日志记录、中止检查 |
| `AfterLLM` | LLM 返回后 | 当前迭代号、内容、思考、finishReason、ToolCalls | 错误收敛、token 消耗统计、中止检查 |
| `Abort` | 循环终止时（LIFO） | 终止原因 | 清理资源、回写状态 |

**接口**：
```go
type LoopHook interface {
    Priority() int
    BeforeLLM(ctx context.Context, input BeforeLLMInput) HookResult
    AfterLLM(ctx context.Context, input AfterLLMInput) HookResult
    Abort(ctx context.Context, reason string)
}
```

**实现要点**：
- **Priority 排序**：数值越小越先执行，相同 Priority 保持注册顺序。使用 `sort.Slice` 实现
- **HookResult 终止循环**：`IsTerminal()=true` 时循环立即中止。`Error!=nil` 视为错误终止；`Abort=true` 视为正常终止
- **Abort LIFO 顺序**：钩子终止或循环自然结束时，所有已注册钩子的 `Abort` 按与 Priority 相反的顺序执行（先注册的后清理）
- **钩子不修改消息序列**：`BeforeLLMInput.Messages` 是当前即将发送给 LLM 的消息序列。钩子如果需要"注入额外消息"（如 RAG 结果），应返回新的 `[]core.Message`，通过扩展实现——**不建议**让钩子直接修改 input 引用，避免破坏循环内部状态

**示例**：

```go
// MemoryThoughtHook — RAG 检索钩子（goharness 实际场景）
type MemoryThoughtHook struct {
    rag *rag.Retriever
}

func (h *MemoryThoughtHook) Priority() int { return 20 }
func (h *MemoryThoughtHook) BeforeLLM(ctx context.Context, in BeforeLLMInput) HookResult {
    // 用最近一条 user 消息做 query，检索相关记忆
    var lastUser string
    for i := len(in.Messages) - 1; i >= 0; i-- {
        if in.Messages[i].Role == core.RoleUser {
            lastUser = in.Messages[i].TextContent()
            break
        }
    }
    memories, err := h.rag.Retrieve(ctx, lastUser, 5)
    if err != nil || len(memories) == 0 {
        return HookResult{}
    }
    // 将检索结果作为 system 消息注入——通过修改 Messages slice 的 system 消息内容
    // 实际实现中应返回新的消息序列或注入到已有 system 消息的文本里
    return HookResult{}
}
func (h *MemoryThoughtHook) AfterLLM(ctx context.Context, in AfterLLMInput) HookResult {
    return HookResult{}
}
func (h *MemoryThoughtHook) Abort(ctx context.Context, reason string) {}
```

**注册**：
```go
goagent.Ask("...").
    Config(
        goagent.WithLoopHooks(
            &MemoryThoughtHook{rag: retriever},
            &ConvergenceHook{retryLimit: 3},
        ),
    ).
    Chat()
```

### 3.5 EventBus（事件总线）

**用途**：循环对外广播所有可观测事件。goagent 不直接调用 WebSocket / HTTP 回调——它只往 EventBus 发事件。上层产品订阅事件后自行决定如何消费。

**接口**：
```go
type EventBus interface {
    Emit(event Event)
    Subscribe() (<-chan Event, func())
    Close()
}
```

**默认实现**：`InProcessEventBus`（无缓冲通道，同步阻塞直到所有订阅者接收）。`NopEventBus`（空操作，Agent 未注入时兜底）。

**自定义实现场景**：

```go
// WebSocket 广播事件总线（goharness 实际场景）
type WebSocketEventBus struct { /* websocket 连接、订阅映射 */ }

func (b *WebSocketEventBus) Emit(e Event) {
    data, _ := json.Marshal(e)
    b.ws.Broadcast(data)
}
func (b *WebSocketEventBus) Subscribe() (<-chan Event, func()) {
    // 返回本地 channel，由 goharness 内部消费
}
func (b *WebSocketEventBus) Close() { /* 关闭 websocket */ }
```

### 3.6 Callbacks（旧式函数指针回调）

`Callbacks` 是 `ChatStream` 专用的、面向旧式回调风格的 API。通过 EventBus 桥接实现——Agent 内部统一发事件，同时回调对应的函数指针。

**优先级**：EventBus 是主要扩展机制，Callbacks 是向后兼容层。新功能优先用 EventBus。

## 四、事件类型参考

完整事件类型定义见 `events.go`。以下按生命周期阶段分组：

### 流式增量阶段（一个 LLM 调用内）

| 事件 | 触发条件 | Data 类型 |
|------|----------|----------|
| `EvThinkingDelta` | 每次接收思考增量 | `string` |
| `EvContentDelta` | 每次接收回答文本增量 | `string` |
| `EvToolUseDelta` | 每次接收工具调用参数增量 | `core.ToolCallDelta` |
| `EvThinkingDone` | 思考阶段完成（LLM 开始生成回答或工具调用） | `nil` |

### 每轮循环阶段（一个完整的 Think → Act → Think）

| 事件 | 触发条件 | Data 类型 |
|------|----------|----------|
| `EvTokenUsage` | LLM 调用完成后立即 | `*TokenUsageEvent{Iteration, Usage, Duration}` |
| `EvToolExecStart` | 工具开始执行 | `*toolExecData{Name, Args}` |
| `EvToolExecEnd` | 工具执行完成（含挂起情况不发此事件） | `*toolExecEndData{Name, Duration, Success, Result, Error}` |
| `EvLoopEnd` | 一轮 Think-Act 循环结束（工具结果全部回传） | `*LoopEndData{Iteration}` |

### 循环终止阶段

| 事件 | 触发条件 | Data 类型 |
|------|----------|----------|
| `EvSuspend` | 工具返回 `ErrNeedExternalInput` | `*ExternalInputRequest{ToolName, ToolCallID, Kind, Details}` |
| `EvFinalAnswer` | 模型输出最终答案（无 ToolCalls） | `string` |
| `EvStop` | 循环停止（每次循环都会发：StopToolLoop→StopFinished/MaxIterations/Error/Suspended） | `StopReason` |
| `EvComplete` | 对话结束（无论成功/出错/达到轮数） | `[]core.Message` 完整上下文 |

**注意**：`EvStop` 和 `EvComplete` 语义不同。`EvStop` 在循环过程中会多次触发（每次从 LLM 返回后都会 emit StopToolLoop 或 StopFinished），`EvComplete` 只在最终返回时触发一次。

## 五、goharness 接入指南

goharness 是 goagent 的上层产品外壳。接入的核心思路：**不继承、不修改 goagent，只实现接口注入**。

### 接入点清单

| goagent 扩展点 | goharness 实现 | 注入方式 |
|---------------|---------------|----------|
| `core.Client` | 带 LLMRetry + isToolPairingError 修复的包装 | `WithLLMClient` |
| `ToolExecutor` | 带 sandbox 检查的执行器 | `WithToolExecutor` |
| `LoopHook[0]` | MemoryThoughtHook（RAG 注入） | `WithLoopHooks` |
| `LoopHook[1]` | ConvergenceHook（重复错误收敛） | `WithLoopHooks` |
| `LoopHook[2]` | TokenBudgetHook（预算检查） | `WithLoopHooks` |
| `EventBus` | WebSocket 广播实现 | `WithEventBus` |

### suspend 完整流程示例

```
┌─ goagent 工具 ──────────────┐     ┌─ goharness 产品层 ─────────────┐
│                              │     │                                │
│  Bash.Execute(ctx, args)     │     │                                │
│    ↓                         │     │                                │
│  sandbox.Check(name, args)   │     │                                │
│    ↓ 返回 ErrNeedExternalInput    │                                │
│  return "", NewPermission... │─────│ EvSuspend 事件                 │
│                              │     │   ↓                            │
│                              │     │ WebSocket → 前端               │
│                              │     │   ↓                            │
│                              │     │ 弹出授权对话框                  │
│                              │     │   ↓ 用户点击"允许"              │
│                              │     │ 补 role=tool 消息到 History     │
│                              │     │ 再次 Chat()                     │
│                              │←────│                                │
│                              │     │                                │
│  (同一 Agent 实例，History    │     │                                │
│   已追加 tool result)         │     │                                │
│                              │     │                                │
│  循环继续执行剩余工具调用      │     │                                │
│  → 进入下一轮 LLM 思考        │     │                                │
└──────────────────────────────┘     └────────────────────────────────┘
```

### 会话 + 压缩 + RAG 接入

goagent 不做会话管理——它只消费 `History(messages)` 并在 `Complete/OnComplete` 里吐出完整消息序列。goharness 的会话层负责：

1. 持久化 `Complete` 回调返回的 `[]core.Message`
2. 上下文压缩（压缩后的摘要作为 System 消息或独立消息注入）
3. RAG 检索作为 `MemoryThoughtHook.BeforeLLM` 的实现
4. 滑动窗口和 token 预算

## 六、测试策略

goagent 的测试分三层：

1. **纯单元测试**：用 mock `core.Client` 验证循环骨架行为（`agent_test.go`）
2. **集成测试**：连真实 Ollama，验证工具执行 + 多轮循环
3. **Hook/Event 测试**：验证钩子优先级排序、挂起机制、事件发射顺序

扩展自己组件时（自定义 Client / Executor / Hook）应仿照 `agent_test.go` 的 mock 策略：

```go
type mockLLMClient struct{ /* 按轮次返回预设响应 */ }
func (m *mockLLMClient) Chat(...) (core.Response, error) { ... }
func (m *mockLLMClient) ChatStream(...) (*core.Stream, error) { ... }
```

## 七、边界情况速查

| 场景 | 行为 | 外部该做什么 |
|------|------|-------------|
| 工具未注册 | 返回 `"goagent: 未注册工具 \"xxx\""` 作为 tool result | 模型自行修正调用 |
| LLM 返回 finishReason="length" | 正常终止但 answer 可能为空 | 检查 `FinishReason` 或 `answer == ""` |
| LLM 返回 finishReason="content_filter" | 正常终止，answer 携带 refusal 文本 | 同上 |
| 工具挂起 | 循环挂起 | 渲染授权/输入 UI，补 tool result 再 Chat() |
| 钩子中止（HookResult.Abort=true） | 循环正常终止，`LastStopReason()=StopFinished` | 检查 `AbortReason` 字段 |
| ctx 取消 | 循环以错误终止，`LastStopReason()=StopError` | 检查 `err` |
| MaxIterations 耗尽 | 循环以错误终止，`LastStopReason()=StopMaxIterations` | 增加轮数或让模型给出最终答案 |

## 八、与 goharness 的职责边界

| 职责 | goagent | goharness |
|------|---------|-----------|
| ReAct 循环骨架 | ✅ | |
| LLM 调用 + finishReason 处理 | ✅ | |
| Token Usage 感知 | ✅ | |
| 挂起续跑机制 | ✅ | 消费 EvSuspend + 渲染 UI + 恢复 |
| Hook 系统 | ✅（定义接口 + 排序 + 执行） | 实现具体 Hook |
| EventBus | ✅（定义接口 + 默认实现） | 实现 WebSocket 广播 |
| ToolCall 接口 | ✅ | 实现具体工具 |
| 会话管理 | | ✅（持久化、滑动窗口） |
| 上下文压缩 | | ✅（压缩后注入为 System） |
| RAG | | ✅（MemoryThoughtHook 实现） |
| 沙箱安全门控 | | ✅（SandboxedExecutor 实现） |
| LLM 错误恢复策略 | | ✅（RetryingClient 实现） |
| 多 Agent 协作 | | ✅（SubAgent/Team 在 goharness 编排） |

## 九、演进规划

goagent 当前已实现的核心机制：

- ✅ 循环骨架（ReAct）
- ✅ 阻塞 / 流式双模式
- ✅ 可注入的 4 个核心接口（Client / Executor / Hook / EventBus）
- ✅ 挂起续跑
- ✅ LoopHook 系统（BeforeLLM / AfterLLM / Abort）
- ✅ 12 种事件类型
- ✅ Token Usage 感知
- ✅ 精确 finishReason（从 Provider 直接取）

已识别的后续演进：

- ⏭️ **并发工具执行**：ToolExecutor.Execute 可扩展为按工具声明并发/串行，goagent 循环按批执行
- ⏭️ **Plan-and-Solve 模式**：可选"规划 → 执行"双阶段，让 Agent 先出计划再执行
- ⏭️ **SubAgent 原生支持**：Agent 循环内可派生子会话，子会话消息自动冒泡回主会话
- ⏭️ **结构化输出强制**：支持要求 LLM 返回特定 JSON schema（用 `response_format` 约束）

这些都是 goagent 循环骨架自然扩展的方向——**不修改骨架本身，只在骨架上装配新的行为模式**。
