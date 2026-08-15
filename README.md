# goagent

迷你 agent 引擎：实现对大模型的多轮思考-工具执行循环。基于 [gochat](https://github.com/DotNetAge/gochat) 构建，默认对接本机 Ollama 的 `minicpm-v4.6:latest` 模型。

## 特性

- 多轮思考循环：模型可发起多次工具调用，工具结果回传后继续思考，直到给出最终答案
- 流式与阻塞双模式：`Chat` 阻塞返回、`ChatStream` 流式输出思考增量与各阶段事件
- 工具协议对齐 OpenAI NativeTool：`ToolCall` 接口声明 `Name / Description / Parameters`，带 `Execute` 方法
- 配置灵活：环境变量 + 编程式 `With*` 选项，支持 Ollama / OpenAI / DeepSeek / Anthropic 客户端
- 上下文连续：`History` 注入历史对话、`Images` 附加图片输入

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
| `GO_AGENT_MODEL`   | `minicpm-v4.6:latest`    | 模型名称                    |

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

可用选项：`WithBaseURL`、`WithAPIKey`、`WithModel`、`WithTemperature`、`WithTimeout`、`WithMaxIterations`、`WithClientType`。

## 流式对话

`ChatStream` 通过回调接收思考增量与工具执行事件：

```go
err := goagent.Ask("查看当前目录，统计文件数量").
	Tools(tools.Bash{}).
	ChatStream(goagent.Callbacks{
		// 思考增量（思考过程 + 回答文本），收到即调用
		ThinkCallback: func(delta string) { fmt.Print(delta) },
		// 工具执行前：args 为模型生成的参数 JSON
		BeforeToolExec: func(tool goagent.ToolCall, args json.RawMessage) {
			fmt.Printf("\n正在执行 %s, %s\n", tool.Name(), args)
		},
		// 工具执行后
		AfterToolExec: func(tool goagent.ToolCall, result string, err error) {
			fmt.Printf("执行结束: %s, 结果: %s\n", tool.Name(), result)
		},
		// 对话完成
		Finished: func(answer string) {
			fmt.Println("\n", answer)
		},
		// 每次思考循环停止（StopFinished / StopToolLoop / StopMaxIterations / StopError）
		Stop: func(reason goagent.StopReason) {},
	})
```

流式模式默认开启模型思考（OpenAI 兼容端点发送 `enable_thinking`）。

## 系统提示词

`System` 设置系统提示词，作为模型的顶层指令（角色设定、行为约束等）。工具定义仍通过 tools 协议传给模型，无需在系统提示词中重复描述：

```go
answer, _ := goagent.Ask("帮我写一份周报").
	System("你是一名资深的项目管理者，擅长简洁清晰的汇报，回复控制在 200 字以内。").
	Chat()
```

## 持续多轮回话

`History` 注入上一轮对话的原始 `core.Message` 数据（含思考内容与工具调用），即可接着讨论。配合 `Complete`（阻塞式）或 `Callbacks.OnComplete`（流式）获取完整上下文，就能形成"多轮 → 存上下文 → 再续聊"的闭环：

```go
// 第一轮：结束后用 Complete 拿到完整对话上下文
var context []core.Message
answer1, _ := goagent.Ask("帮我查看当前目录下的文件").
	Tools(tools.Bash{}).
	Complete(func(messages []core.Message) {
		context = messages // 完整上下文：系统提示 + 历史 + 每轮 assistant/tool 消息
	}).
	Chat()

// 第二轮：把上一轮上下文作为 History 注入，模型"记得"之前的思考与工具执行
answer2, _ := goagent.Ask("刚才那个文件内容是什么？").
	History(context).
	Tools(tools.Bash{}).
	Chat()

// 第三轮可继续：History(新一轮的 context)
```

流式版本使用 `OnComplete` 收集上下文：

```go
var context []core.Message
err := goagent.Ask("第一轮问题").
	Tools(tools.Bash{}).
	ChatStream(goagent.Callbacks{
		OnComplete: func(messages []core.Message) {
			context = messages
		},
	})

// 下一轮
answer, _ := goagent.Ask("继续上一轮讨论").
	History(context).
	Tools(tools.Bash{}).
	Chat()
```

要点：

- `Complete` / `OnComplete` 在对话**无论成功、出错还是达到最大轮数**时都会触发，回调里拿到的是完整的原始 Message 序列
- 该序列包含 `assistant` 消息的 `tool_calls` 与 `role=tool` 结果消息，原样交给下一轮 `History`，模型能准确接续之前的工具执行过程
- `History` 传入的消息排列在当前问题之前，可连续拼接多轮形成长对话

## 图片输入

`Images` 附加本地图片路径，随当前问题一起发给模型（多模态模型）：

```go
answer, _ := goagent.Ask("这张图里有什么？").
	Images("photo.png").
	Chat()
```

## 默认 CLI

项目内置流式 CLI，将思考流与工具执行过程打印到屏幕：

```bash
go run ./cmd/goagent "当前目录下有哪些文件？"
echo "你好" | go run ./cmd/goagent
```

## 扩展工具

实现 `ToolCall` 接口即可：声明 `Name / Description / Parameters`（JSON Schema），实现 `Execute`。可参考内置的 [tools/bash.go](tools/bash.go)。

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

注册使用：

```go
answer, _ := goagent.Ask("帮我回显 hello").
	Tools(tools.Bash{}, Echo{}).
	Chat()
```

工具未注册时，模型发起的调用会以错误提示回传，模型可自行修正。

## 目录结构

```
goagent/
├── agent.go          # 核心引擎：Ask/Config/Tools/System/Images/History/Complete/Chat/ChatStream
├── config.go         # 配置与环境变量
├── tool.go           # ToolCall 接口、回调、停止原因
├── tools/            # 内置工具
│   └── bash.go       # Shell 命令执行
├── cmd/goagent/      # 默认 CLI
└── examples/basic/   # 基础示例
```

## 停止原因

- `StopFinished`：思考完成，输出最终答案
- `StopToolLoop`：思考暂停，转入工具执行
- `StopMaxIterations`：达到最大思考轮数
- `StopError`：出错终止
