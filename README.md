# goagent

迷你 agent 引擎：实现对大模型的多轮思考-工具执行循环。基于 [gochat](https://github.com/DotNetAge/gochat) 构建，默认对接本机 Ollama 的 `minicpm-v4.6:latest` 模型。

## 特性

- **多轮思考循环**：模型可发起多次工具调用，工具结果回传后继续思考，直到给出最终答案
- **三种运行入口**：`Chat` 阻塞返回、`ChatStream` 流式输出、`ChatCtx` / `ChatStreamCtx` 带外部取消
- **运行实例控制平面（内部化）**：每次运行自动登记进包内唯一的 `DefaultRuntimeManager()`，状态机由内核循环驱动（唯一真相源）；客户端统一经它查询快照、取消、等待落定——不存在"不登记的裸循环"旁路
- **长任务后台处理**：运行 goroutine 归宿主、客户端不持有运行资源——控制界面关闭、客户端断连，已发起的运行持续执行至自然终态，全程可观测、可中断、可挂起续跑、可派生子任务（跨进程持久化恢复是宿主策略）
- **任务委托（SubAgent）**：`goagent/subagent` 独立包承载 Harness 通用协议与工具（内核零感知），创建权上收宿主，支持嵌套派发
- **运行时上下文注入**：`RuntimeValue` 类型安全槽位，宿主构造期注入生命周期上下文（会话/沙箱/身份），工具与钩子经 `From(ctx)` 取回，接口签名零变更
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

| Option              | 说明                                                                                             |
| ------------------- | ------------------------------------------------------------------------------------------------ |
| `WithBaseURL`       | 模型端点基础地址                                                                                 |
| `WithAPIKey`        | API 密钥                                                                                         |
| `WithModel`         | 模型名称                                                                                         |
| `WithTemperature`   | 采样温度                                                                                         |
| `WithTimeout`       | HTTP 超时                                                                                        |
| `WithMaxIterations` | 多轮思考最大轮数                                                                                 |
| `WithClientType`    | gochat 客户端类型                                                                                |
| `WithLLMClient`     | 注入自定义 `core.Client`（跳过默认工厂）                                                         |
| `WithToolExecutor`  | 注入自定义 `ToolExecutor`                                                                        |
| `WithLoopHooks`     | 注册循环钩子（可多个，按 Priority 排序）                                                         |
| `WithEventBus`      | 注入事件总线实例                                                                                 |
| `WithRuntimeID`     | （可选）指定运行实例 ID，便于预定位；不指定时自动生成，经 `Agent.RuntimeID()` 取回               |
| `WithTaskID`        | （可选）指定任务归属：多轮运行共享同一 TaskID，经 `GetByTask` 定位；不指定时退化为本轮 RuntimeID |
| `WithRuntimeValue`  | 注入 Agent 生命周期上下文值（构造期写入，工具/钩子经 `RuntimeValue[T].From(ctx)` 取回）          |

## 事件总线

`EventBus` 是 Agent 唯一的事件输出通道——**所有事件（流式增量、工具执行、挂起、token 消耗、循环结束）都走 EventBus**。默认实现是进程内无缓冲通道，外部可替换为带过滤、缓冲或跨进程传播的实现（如 WebSocket 广播）。

> Callbacks 是 EventBus 的**简化包装/向后兼容层**（函数指针回调），Agent 内部统一走 `bus.Emit(Event{Type, Data})`，同时同步触发 Callbacks。新功能和生产级场景优先用 EventBus。

### 事件类型一览

| 事件类型          | Data 类型               | 说明                                   |
| ----------------- | ----------------------- | -------------------------------------- |
| `EvThinkingDelta` | `string`                | 模型思考增量（流式）                   |
| `EvContentDelta`  | `string`                | 模型回答文本增量（流式）               |
| `EvToolUseDelta`  | `core.ToolCallDelta`    | 工具调用参数增量（流式）               |
| `EvThinkingDone`  | `nil`                   | 思考阶段完成                           |
| `EvTokenUsage`    | `*TokenUsageEvent`      | 每轮 LLM 调用后的 token 消耗           |
| `EvLoopEnd`       | `*LoopEndData`          | 一轮完整的 Think-Act 循环结束          |
| `EvToolExecStart` | `*ToolExecStartData`    | 工具开始执行                           |
| `EvToolExecEnd`   | `*ToolExecEndData`      | 工具执行完成                           |
| `EvSuspend`       | `*ExternalInputRequest` | 工具需要外部输入，循环挂起             |
| `EvFinalAnswer`   | `string`                | 最终答案                               |
| `EvStop`          | `StopReason`            | 循环停止                               |
| `EvComplete`      | `[]core.Message`        | 对话完成（无论成功、出错还是达到轮数） |

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

`bus.Close()` 在 Agent 循环退出时会自动被调用（`execLoop` 中 `defer bus.Close()`），所以外部显式 `defer bus.Close()` 是安全的双重保险。

取消语义：`cancel()` 只让订阅者停止接收（幂等且永不阻塞），**不关闭通道**——所有订阅者通道（含已取消的）统一在总线 `Close()` 时关闭，消费侧的 `for range ch` 以通道关闭自然退出。

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

接入控制平面时，挂起在事实层面就是本轮循环的正常结束——实例落 `Completed`、Done 关闭，"恢复"是宿主的编排动作（以新 runtimeID 对同一会话再次发起运行），详见[运行实例控制平面](#运行实例控制平面)。

`NewPermissionRequest` / `NewAskUserRequest` 是两个便捷构造函数。`Kind` 字段区分挂起类别：

- `"permission"` — 安全授权场景（沙箱检查、白名单外文件访问等）
- `"ask_user"` — 对话式交互场景（补充参数、确认选项等）

## 运行实例控制平面

控制平面回答一个问题：**此刻有哪些 Agent 在运行、各自什么状态、如何干预**。

**内部化原则**：控制平面是内核设施而非可插拔组件——登记表只有包内唯一的实例，由 `DefaultRuntimeManager()` 获取，外部禁止创建。每次运行（`Chat` / `ChatStream` / `ChatCtx` / `ChatStreamCtx`）自动登记进它，**只有一种使用方式**：`Ask` 照常用，控制平面自动生效。客户端（或宿主）统一经它访问所有运行实例——查询状态、控制执行、清理终态，不存在第二套路径。

状态的唯一真相源是内核循环（`lastStopReason` / `EvSuspend` / ctx 取消都产生于循环内部），因此状态机由内核驱动并结算，访问方只读——`Cancel` 是唯一的写操作。

### 状态机

Think Loop 没有中间态——要么在运行，要么已落终态。"挂起 / 恢复"是宿主编排（见下文），不是内核状态：

```text
              Register()                Chat()/ChatCtx()
  [无] ─────────────→ Pending ──────────────→ Running ──┬─ StopFinished / StopSuspended ──→ Completed
                        │                              ├─ StopError / 达到轮数 ──────────→ Failed
                        │ Fail()/Cancel()              └─ ctx 取消 / Cancel() ──────────→ Cancelled
                        ▼
                      Failed（启动前失败）
```

| 状态              | 说明                                                       |
| ----------------- | ---------------------------------------------------------- |
| `StatusPending`   | 已登记，循环未启动（"已受理未运行"窗口期，UI 立即可见）    |
| `StatusRunning`   | 循环执行中                                                 |
| `StatusCompleted` | 正常结束（含挂起返回 `StopSuspended`，等待输入由宿主编排） |
| `StatusFailed`    | 失败（出错 / 达到最大轮数 / 启动前失败）                   |
| `StatusCancelled` | 被取消（外部 ctx 取消或 `Cancel()`）                       |

终态条目保留在登记表中供查询（"刚刚结束"窗口），直到 `Unregister`。内核永不自动移除条目。

### RuntimeManager API

| 方法                      | 说明                                                                        |
| ------------------------- | --------------------------------------------------------------------------- |
| `DefaultRuntimeManager()` | 返回包内唯一的登记表（**唯一获取途径**，外部禁止创建实例）                  |
| `Register(id)`            | 预登记为 Pending；同 ID 非终态条目已存在时幂等返回，终态条目存在时报错      |
| `Get(id)`                 | 按 RuntimeID 查询实例；不存在时 ok=false                                    |
| `GetByTask(taskID)`       | 按任务定位：优先进行中的实例，全部终态时返回最近结束的一轮                  |
| `List()`                  | 全部实例快照（daemon API 与 UI 的数据源）                                   |
| `Fail(id, reason)`        | 将 Pending 实例置为 Failed 并关闭 Done（仅对 Pending 生效，用于启动前失败） |
| `Unregister(id)`          | 移除终态实例（客户端/宿主的清理职责）                                       |

**概念分层**：用户发起的一个问题是一个**任务**（TaskID，稳定）；任务挂起恢复后以**新运行实例**续跑，多轮实例共享同一 TaskID。客户端定位与控制统一用任务词汇（`GetByTask`），内核状态机载体是 RuntimeID（终态后不可复用）——两个粒度各司其职，不混用词汇。

每个条目实现 `Runtime` 接口：`ID / TaskID / Status / StartedAt / EndedAt / Done / Reason / Cancel`。`TaskID()` 未显式指定（`WithTaskID`）时退化为"一次任务 = 一轮运行"，等于 `ID()`——普通调用方无感知。`Done()` 进入终态时关闭，等待者多路 select 即可实现 Promise.all 语义；`Reason()` 返回 `Fail` 注入的失败原因（其余终态路径为空）；`Cancel()` 幂等——Pending 态等价 Fail("cancelled")，Running 态取消循环（循环以既有 ctx 取消路径收尾），终态 no-op。

### 用法

**`WithRuntimeID` / `WithTaskID` 都是可选参数**——绝大多数调用不需要生成或传入任何 ID：

```go
// 场景一（默认路径）：完全不关心 ID，Ask 照常用
ans, _ := goagent.Ask("查看当前目录，统计文件数量").Tools(tools.Bash{}).Chat()

// 需要查询/取消这次运行时，经 RuntimeID() 取回内核自动生成的 ID 再定位
a := goagent.Ask("长任务...").Tools(tools.Bash{})
go a.Chat()
rt, ok := goagent.DefaultRuntimeManager().Get(a.RuntimeID()) // ok=true，控制视图
rt.Cancel() // 随时取消
```

任务跨多轮运行（挂起恢复、宿主编排续跑）时指定 TaskID，客户端始终问"我的任务怎么样了"：

```go
m := goagent.DefaultRuntimeManager()

// 第一轮：发起任务
go goagent.Ask("...").
	Config(goagent.WithTaskID("task-1")). // 任务归属：恢复后的新运行实例也用它
	ChatCtx(ctx)

rt, _ := m.GetByTask("task-1") // "我的任务现在怎么样了"——优先进行中的实例
<-rt.Done()
rt.Status() // 第一轮 completed（可能是挂起返回）

// 第二轮：宿主补齐输入后恢复，新运行实例共享同一 TaskID
go goagent.Ask("...").Config(goagent.WithTaskID("task-1")).History(msgs).ChatCtx(ctx)

rt2, _ := m.GetByTask("task-1") // 自动定位到最新一轮
```

只有宿主编排需要**预先**知道 RuntimeID 时才指定（如 SubAgent 受理流程先 `Register(id)` 再以同 ID 运行）：

```go
m := goagent.DefaultRuntimeManager()
rt, _ := m.Register("run-1") // 预登记：Pending，"已受理未运行"窗口期即可见

a := goagent.Ask("...").Config(goagent.WithRuntimeID("run-1")) // 与预登记同 ID
go a.Chat()

<-rt.Done()            // 等待本轮落定
rt.Status()            // completed / failed / cancelled
m.Unregister("run-1")  // 终态条目清理

for _, rt := range m.List() { // 枚举全部实例（UI/daemon 数据源）
	fmt.Println(rt.ID(), rt.TaskID(), rt.Status())
}
```

要点：

- **`WithRuntimeID` / `WithTaskID` 都是可选参数**：不指定时内核自动生成 RuntimeID（运行后经 `Agent.RuntimeID()` 取回）；TaskID 退化为本轮 RuntimeID——普通调用方无感知
- **没有"不登记的裸循环"**：所有运行自动进入唯一登记表，是否用控制平面只取决于客户端是否去查询
- 同一 RuntimeID 终态后不可复用（fail fast，应生成新 ID）；同一任务跨轮共享 TaskID（`GetByTask` 定位）；同一 Agent 实例不支持并发运行（并发 `Chat` 返回错误）
- 控制平面是拉模式：`List()` 快照是 daemon API 与 UI 的数据源；事件推送仍走既有 EventBus，内核不新增事件类型

### 生命周期归属：客户端断开不影响运行

运行 goroutine 归**宿主**（发起 `Chat` 的进程），控制平面是被动登记表——UI 等客户端不持有任何运行资源。因此：

- 控制界面关闭、客户端断连，所有已发起的运行**继续执行直至自然终态**（Completed / Failed / Cancelled）
- 中断运行只有三条路径：显式 `Cancel`、`ChatCtx` 外部 ctx 取消/超时、宿主进程退出（v1 无跨进程恢复，恢复属宿主策略——从 session store 重建）
- **事件桥接是宿主职责**：默认 EventBus 同步无缓冲，宿主若把客户端消费通道直接挂上总线，客户端断开后停止消费会让运行**阻塞在事件投递上**（不是终止，是卡死）。宿主应桥接：订阅后扇出到带缓冲的客户端队列，断开时 `cancel()` 该订阅并丢弃积压——订阅取消只影响投递目标，不影响运行
- SubAgent 链同理：子运行 goroutine 归宿主 Dispatcher，父运行结束**不级联取消**（是否级联由宿主策略决定，如按 Sponsor 过滤 `List()` 后逐个 `Cancel`）

### 挂起与恢复（宿主编排）

工具挂起（`StopSuspended`）在事实层面就是本轮循环的正常结束——实例落 `Completed`、Done 关闭。恢复是宿主的编排动作：

1. 宿主经 `EvSuspend` 事件 / `LastStopReason()` 感知等待输入，路由授权/提问 UI，在自己的元数据里标记"等待输入"
2. 用户答复后，宿主把 `role=tool` 结果补入 `History`，以**新 runtimeID** 对同一会话再次 `ChatCtx`（跟踪句柄是会话 ID，不受 runtimeID 更换影响）
3. 新一轮实例 Running → Completed

## 运行时上下文注入（RuntimeValue）

工具与钩子收到的 `ctx` 只携带**请求作用域**信息（取消/超时）。宿主还需要把**Agent 生命周期上下文**（会话对象、用户身份、沙箱引用等）传递到工具与钩子内部——`RuntimeValue` 补上这条通道，语义等同于"Agent 级、构造期、类型安全的 `context.WithValue`"，且**不改变任何工具/钩子的接口签名**：

```go
// 1. 声明值槽（包级，按用途各声明一个）
var sandboxVal = goagent.NewRuntimeValue[*sandbox.Sandbox]("sandbox")
var sessionVal = goagent.NewRuntimeValue[*Session]("session")

// 2. 宿主构造期注入（可多个；同名槽后注入覆盖先注入）
goagent.Ask("...").
    Config(
        goagent.WithRuntimeValue(sandboxVal, sb),
        goagent.WithRuntimeValue(sessionVal, sess),
    ).
    Tools(tools.Bash{}).
    ChatCtx(ctx)

// 3. 工具 / 钩子内取回（ctx 已由内核自动注入）
func (b Bash) Execute(ctx context.Context, args json.RawMessage) (string, error) {
    sb, ok := sandboxVal.From(ctx) // 未注入或类型不匹配 → 零值 + false，不 panic
    ...
}
```

**设计要点**：

- **构造期写入、运行期只读**：值只能经 `WithRuntimeValue` 在 Config 链注入，内核不提供运行期写入 API——无锁、无数据竞争
- **类型安全**：注入方与消费方共用同一 `RuntimeValue[T]` 声明即天然类型匹配；`From` 带断言，错配返回 `false` 不 panic
- **全链路可见**：`execLoop` 开始时一次性派生注入，工具、循环钩子、LLM 客户端均可取回
- 多来源共存：goharness 挂沙箱、业务插件挂用户信息，各用各的槽互不冲突

## SubAgent 派发（多 Agent 协作）

SubAgent 协议与工具住在**独立子包 `goagent/subagent`**（仅依赖标准库，可被任意宿主侧代码复用）——内核根包对它**零感知**：该工具与 Bash 等普通工具走完全相同的执行路径，内核不存在任何"父子等级"概念。工具名 `"subagent"` 及参数 schema 是 Harness 通用概念，对外保持稳定。

机制原语与策略分工：`SubAgentTool` 只发请求、同步等回执；**创建权上收宿主**——宿主实现 `SubAgentDispatcher` 受理请求并创建新的运行实例，Runtime 不自我派生，父子之间只剩宿主解释的元数据（Sponsor）。

### 原语（goagent/subagent 包）

| 类型                 | 说明                                                                 |
| -------------------- | -------------------------------------------------------------------- |
| `SubAgentRequest`    | 派发请求：`AgentName` / `Task` / `SessionID`（可选，显式复用子会话） |
| `SubAgentReceipt`    | 受理回执：`Accepted` / `SessionID`（跟踪句柄）/ `Reason`             |
| `SubAgentDispatcher` | 宿主实现并注入：`Submit`（同步受理）/ `Wait`（Promise.all 语义等待） |
| `SubAgentTool`       | 工具形态：解析参数 → Submit → 回执转文本，模型依赖同步回执自纠       |

### 宿主 Dispatcher 实现

```go
import "goagent/subagent"

func (d *HostDispatcher) Submit(ctx context.Context, req subagent.SubAgentRequest) (subagent.SubAgentReceipt, error) {
	id := d.newRuntimeID() // 宿主生成全局唯一 runtimeID
	mgr := goagent.DefaultRuntimeManager()
	if _, err := mgr.Register(id); err != nil {
		return subagent.SubAgentReceipt{}, err
	}
	child := goagent.Ask(req.Task).
		Config(goagent.WithRuntimeID(id)).      // 登记自动生效（唯一登记表），只需指定 ID
		Tools(subagent.NewSubAgentTool(d))      // 子 Agent 也能继续派发（嵌套）
	go child.ChatCtx(d.orchestrationCtx())     // goroutine 归宿主：生命周期不继承父工具调用的 ctx
	return subagent.SubAgentReceipt{Accepted: true, SessionID: d.bindSession(id)}, nil
}

func (d *HostDispatcher) Wait(ctx context.Context, sessionIDs []string) map[string]error {
	// 经 DefaultRuntimeManager().Get(id) 多路等待各实例 Done()，返回早期失败的句柄 → 原因
}
```

要点：

- **request/reply 语义**：`Submit` 同步返回回执，模型立即得到跟踪句柄或失败原因——同步失败反馈是模型自纠的依据
- **零产品知识**：工具不知道 Session 的存在，也不携带 Sponsor 身份——Dispatcher 实例与发起方绑定（宿主构造父 Agent 时注入），发起者由绑定关系携带
- **生命周期归属**：子任务的 goroutine 归宿主 Dispatcher，不继承父回合工具调用的 ctx——不存在 `context.WithoutCancel` 类的生命周期 hack
- **扁平架构**：父、子、孙实例都在唯一登记表里，无等级差别；级联取消（按 Sponsor 过滤 `List()` 后逐个 `Cancel`）、并发上限、会话复用、崩溃恢复全部是宿主策略，内核不做级联

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

| 字段         | 说明                                         |
| ------------ | -------------------------------------------- |
| `Abort=true` | 循环立即中止，视为正常终止（`StopFinished`） |
| `Error!=nil` | 循环以错误终止（`StopError`）                |
| 两者都设     | `Error` 优先（视为错误终止）                 |
| 都不设       | 钩子正常返回，继续循环                       |

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

| 常量                | 值  | 说明                           |
| ------------------- | --- | ------------------------------ |
| `StopFinished`      | 0   | 思考完成，输出最终答案         |
| `StopToolLoop`      | 1   | 思考暂停，转入工具执行         |
| `StopMaxIterations` | 2   | 达到最大思考轮数               |
| `StopError`         | 3   | 出错终止                       |
| `StopSuspended`     | 4   | 工具需要外部输入，循环挂起等待 |

## 目录结构

```
goagent/
├── agent.go          # 核心引擎：Ask / Config / Tools / Chat / ChatStream / ChatCtx / ChatStreamCtx / 状态结算
├── runtime.go        # 控制平面（内部化）：RuntimeStatus / Runtime / RuntimeManager（唯一实例）
├── subagent/         # SubAgent 协议与工具（独立子包，Harness 通用概念；内核零感知）
│   ├── subagent.go       # SubAgentRequest / Receipt / Dispatcher / SubAgent 工具
│   └── subagent_test.go  # fake Dispatcher 并发一致性测试（goharness 受理规格）
├── config.go         # Config 结构体 + 环境变量 + With* Option
├── tool.go           # ToolCall 接口、Callbacks、StopReason
├── llm.go            # NewDefaultLLMClient 工厂
├── executor.go       # ToolExecutor / ToolEnumerator / DefaultToolExecutor
├── hooks.go          # LoopHook / LoopResult / sortHooks
├── events.go         # EventBus + 12 种事件类型 + 默认实现
├── suspend.go        # ErrNeedExternalInput / ExternalInputRequest / 构造函数
├── runtime_test.go   # 控制平面验收测试（状态跃迁 / 取消 / 并发）
├── tools/            # 内置工具
│   └── bash.go       # Shell 命令执行
├── cmd/goagent/      # 默认 CLI
└── examples/
    ├── basic/            # 基础示例：多轮思考 + 工具执行
    └── subagent/         # 多 Agent 协作示例：宿主 Dispatcher 受理 + 嵌套派发
```

## 架构设计

goagent 按整洁架构分层，核心层不向外依赖：

```
┌──────────────────────────────────────────────────────────┐
│ goagent (核心层)                                          │
│   Agent.run() → 依赖 5 个抽象：                           │
│     ├─ core.Client        (来自 gochat/core)             │
│     ├─ ToolExecutor       (goagent 定义)                  │
│     ├─ LoopHook           (goagent 定义)                  │
│     ├─ EventBus           (goagent 定义)                  │
│     └─ SubAgentDispatcher (subagent 包定义接口，宿主实现受理) │
│   RuntimeManager：运行实例控制平面（内部化——包内唯一实例， │
│   所有运行自动登记，客户端经 DefaultRuntimeManager() 访问） │
└──────────────────────────────────────────────────────────┘
         ▲              ▲              ▲
         │ 实现          │ 实现          │ 实现
    ┌────┴────┐    ┌────┴────┐    ┌────┴────┐
    │ gochat  │    │goharness│    │goharness│
    │具体客户端│    │沙箱工具  │    │Dispatcher│
    └─────────┘    └─────────┘    └─────────┘
```

外部产品可替换上述任一抽象组件而不影响循环骨架（RuntimeManager 是内核设施，不在此列）。机制（状态机、派发原语）在内核，策略（注册时机、并发上限、级联取消、崩溃恢复）在外层——详见 [DESIGN.md](DESIGN.md)。
