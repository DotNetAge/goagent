package goagent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/DotNetAge/gochat"
	"github.com/DotNetAge/gochat/client/anthropic"
	"github.com/DotNetAge/gochat/client/deepseek"
	"github.com/DotNetAge/gochat/client/ollama"
	"github.com/DotNetAge/gochat/client/openai"
	"github.com/DotNetAge/gochat/core"
)

// Agent 迷你 agent 引擎：实现对大模型的多轮思考-工具执行循环
type Agent struct {
	cfg        Config
	tools      []ToolCall
	system     string
	images     []string
	history    []core.Message
	completeFn func([]core.Message) // 阻塞式对话完成回调（Complete 设置）
}

// Ask 开始一次新的对话，question 为首轮用户问题
func Ask(question string) *Agent {
	return &Agent{
		cfg:     defaultConfig(),
		history: []core.Message{core.NewUserMessage(question)},
	}
}

// Config 通过选项修改大模型配置（可链式调用）
func (a *Agent) Config(opts ...Option) *Agent {
	for _, o := range opts {
		o(&a.cfg)
	}
	return a
}

// Tools 注册可供模型调用的工具（可链式调用）
func (a *Agent) Tools(tools ...ToolCall) *Agent {
	a.tools = append(a.tools, tools...)
	return a
}

// System 设置系统提示词，作为模型的顶层指令（有工具时自动追加工具清单说明）
func (a *Agent) System(system string) *Agent {
	a.system = system
	return a
}

// Images 附加图片输入（本地文件路径），随当前问题一起发送给模型
func (a *Agent) Images(file ...string) *Agent {
	a.images = append(a.images, file...)
	return a
}

// History 注入之前的历史对话消息（原 Message 数据，含思考内容），
// 使本次对话可以接着上一轮继续讨论。传入的消息排列在当前问题之前。
func (a *Agent) History(messages []core.Message) *Agent {
	history := make([]core.Message, 0, len(messages)+len(a.history))
	history = append(history, messages...)
	history = append(history, a.history...)
	a.history = history
	return a
}

// Chat 阻塞式执行完整的多轮思考-工具循环，返回最终答案
func (a *Agent) Chat() (string, error) {
	return a.run(context.Background(), nil, false)
}

// Complete 设置对话完成回调（仅阻塞式 Chat 使用）：整个对话结束（成功、出错或达到轮数）时，
// 通过回调获取完整的对话上下文（原始 Message 列表）。流式对话请使用 Callbacks.OnComplete。
func (a *Agent) Complete(fn func([]core.Message)) *Agent {
	a.completeFn = fn
	return a
}

// ChatStream 流式执行，通过回调接收思考增量与各阶段事件
func (a *Agent) ChatStream(cb Callbacks) error {
	_, err := a.run(context.Background(), &cb, true)
	return err
}

// run 多轮思考-工具执行主循环
func (a *Agent) run(ctx context.Context, cb *Callbacks, stream bool) (string, error) {
	messages, err := a.buildMessages()
	if err != nil {
		a.emitStop(cb, StopError)
		return "", err
	}
	coreTools := toCoreTools(a.tools)

	for i := 0; i < a.cfg.MaxIterations; i++ {
		msg, err := a.round(ctx, messages, coreTools, stream, cb)
		if err != nil {
			a.emitComplete(cb, stream, messages)
			a.emitStop(cb, StopError)
			return "", err
		}
		messages = append(messages, msg)

		// 模型没有发起工具调用：思考完成，输出最终答案
		if len(msg.ToolCalls) == 0 {
			answer := msg.TextContent()
			if cb != nil && cb.Finished != nil {
				cb.Finished(answer)
			}
			a.emitComplete(cb, stream, messages)
			a.emitStop(cb, StopFinished)
			return answer, nil
		}

		// 模型发起工具调用：暂停思考，转入工具执行
		a.emitStop(cb, StopToolLoop)
		for _, call := range msg.ToolCalls {
			tool := a.findTool(call.Name)
			var args json.RawMessage
			if call.Arguments != "" {
				args = json.RawMessage(call.Arguments)
			}
			if cb != nil && cb.BeforeToolExec != nil && tool != nil {
				cb.BeforeToolExec(tool, args)
			}
			result, execErr := a.execute(ctx, tool, call)
			if cb != nil && cb.AfterToolExec != nil && tool != nil {
				cb.AfterToolExec(tool, result, execErr)
			}
			content := result
			if execErr != nil {
				content = fmt.Sprintf("工具执行失败: %v", execErr)
			}
			messages = append(messages, core.Message{
				Role:       core.RoleTool,
				ToolCallID: call.ID,
				Content:    []core.ContentBlock{{Type: core.ContentTypeText, Text: content}},
			})
		}
	}

	a.emitComplete(cb, stream, messages)
	a.emitStop(cb, StopMaxIterations)
	return "", errors.New("达到最大思考轮数，仍未得到最终答案")
}

// round 与模型交互一轮：阻塞或流式，返回模型回复的消息
func (a *Agent) round(ctx context.Context, messages []core.Message, coreTools []core.Tool, stream bool, cb *Callbacks) (core.Message, error) {
	client, err := a.buildClient()
	if err != nil {
		return core.Message{}, err
	}
	opts := a.chatOptions(coreTools, stream)
	if !stream {
		resp, err := client.Chat(ctx, messages, opts...)
		if err != nil {
			return core.Message{}, err
		}
		return resp.Message, nil
	}
	return a.chatStream(ctx, client, messages, opts, cb)
}

// chatStream 流式对话：思考增量经 ThinkCallback 输出，返回累积完整的模型消息
func (a *Agent) chatStream(ctx context.Context, client core.Client, messages []core.Message, opts []core.Option, cb *Callbacks) (core.Message, error) {
	stream, err := client.ChatStream(ctx, messages, opts...)
	if err != nil {
		return core.Message{}, err
	}
	defer stream.Close()

	var content, thinking strings.Builder
	think := a.thinkFn(cb)
	for stream.Next() {
		ev := stream.Event()
		if ev.Err != nil {
			return core.Message{}, ev.Err
		}
		switch ev.Type {
		case core.EventContent:
			content.WriteString(ev.Content)
			if think != nil {
				think(ev.Content)
			}
		case core.EventThinking:
			thinking.WriteString(ev.Content)
			if think != nil {
				think(ev.Content)
			}
		}
	}
	return core.Message{
		Role:             core.RoleAssistant,
		Content:          []core.ContentBlock{{Type: core.ContentTypeText, Text: content.String()}},
		ReasoningContent: thinking.String(),
		ToolCalls:        stream.ToolCalls(),
	}, nil
}

// buildClient 按 ClientType 构建 gochat 客户端（config 层传入温度等参数）
func (a *Agent) buildClient() (core.Client, error) {
	config := core.Config{
		BaseURL:     a.cfg.BaseURL,
		APIKey:      a.cfg.APIKey,
		Model:       a.cfg.Model,
		Timeout:     a.cfg.Timeout,
		Temperature: a.cfg.Temperature,
	}
	switch a.cfg.ClientType {
	case gochat.OllamaClient:
		return ollama.NewOllamaClient(config)
	case gochat.OpenAIClient, gochat.QwenClient:
		return openai.NewOpenAI(config)
	case gochat.DeepSeekClient:
		return deepseek.NewDeepSeek(config)
	case gochat.AnthropicClient:
		return anthropic.NewAnthropic(config)
	default:
		return nil, fmt.Errorf("不支持的客户端类型: %v", a.cfg.ClientType)
	}
}

// chatOptions 组装每轮请求参数（温度/最大 token 已由 client config 承载，这里传工具定义）
// 流式对话默认开启模型思考（OpenAI 兼容端点发送 enable_thinking；ollama 端忽略该选项，响应侧已支持 thinking）
func (a *Agent) chatOptions(coreTools []core.Tool, stream bool) []core.Option {
	opts := make([]core.Option, 0, 2)
	if len(coreTools) > 0 {
		opts = append(opts, core.WithTools(coreTools...))
	}
	if stream {
		opts = append(opts, core.WithThinking(0))
	}
	return opts
}

// buildMessages 组装完整消息序列：系统提示词 + 历史（图片附加到当前问题）
func (a *Agent) buildMessages() ([]core.Message, error) {
	msgs := make([]core.Message, 0, len(a.history)+2)
	if a.system != "" {
		msgs = append(msgs, core.NewSystemMessage(a.system))
	}
	for i, m := range a.history {
		// 图片只附加到当前问题（Ask 的问题位于历史最后一条）
		if i == len(a.history)-1 && len(a.images) > 0 {
			blocks := make([]core.ContentBlock, 0, len(m.Content)+len(a.images))
			blocks = append(blocks, m.Content...)
			imgBlocks, err := readImageBlocks(a.images)
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, imgBlocks...)
			m.Content = blocks
		}
		msgs = append(msgs, m)
	}
	return msgs, nil
}

// toCoreTools 把 ToolCall 实现转换为 gochat core.Tool（OpenAI tools 协议）
func toCoreTools(tools []ToolCall) []core.Tool {
	out := make([]core.Tool, 0, len(tools))
	for _, t := range tools {
		out = append(out, core.Tool{
			Name:        t.Name(),
			Description: t.Description(),
			Parameters:  t.Parameters(),
		})
	}
	return out
}

// readImageBlocks 读取图片文件并转为 base64 内容块
func readImageBlocks(paths []string) ([]core.ContentBlock, error) {
	blocks := make([]core.ContentBlock, 0, len(paths))
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("读取图片 %s 失败: %w", p, err)
		}
		blocks = append(blocks, core.ContentBlock{
			Type:      core.ContentTypeImage,
			MediaType: mimeByExt(filepath.Ext(p)),
			Data:      base64.StdEncoding.EncodeToString(data),
		})
	}
	return blocks, nil
}

// mimeByExt 根据扩展名推断图片 MIME 类型，未知类型默认 image/jpeg
func mimeByExt(ext string) string {
	switch strings.ToLower(ext) {
	case ".png":
		return "image/png"
	case ".webp":
		return "image/webp"
	case ".gif":
		return "image/gif"
	case ".bmp":
		return "image/bmp"
	default:
		return "image/jpeg"
	}
}

// execute 执行工具调用；未注册的工具返回错误提示给模型
func (a *Agent) execute(ctx context.Context, tool ToolCall, call core.ToolCall) (string, error) {
	if tool == nil {
		return "", fmt.Errorf("未注册工具 %q", call.Name)
	}
	var args json.RawMessage
	if call.Arguments != "" {
		args = json.RawMessage(call.Arguments)
	}
	return tool.Execute(ctx, args)
}

// findTool 按名称查找已注册工具
func (a *Agent) findTool(name string) ToolCall {
	for _, t := range a.tools {
		if t.Name() == name {
			return t
		}
	}
	return nil
}

// thinkFn 返回思考回调，未注册时返回 nil
func (a *Agent) thinkFn(cb *Callbacks) func(string) {
	if cb == nil || cb.ThinkCallback == nil {
		return nil
	}
	return cb.ThinkCallback
}

// emitStop 触发停止事件
func (a *Agent) emitStop(cb *Callbacks, reason StopReason) {
	if cb != nil && cb.Stop != nil {
		cb.Stop(reason)
	}
}

// emitComplete 触发对话完成回调，向外部提供完整的对话上下文（原始 Message 列表）
// 流式对话使用 Callbacks.OnComplete，阻塞式使用 Complete 设置的回调
func (a *Agent) emitComplete(cb *Callbacks, stream bool, messages []core.Message) {
	if stream {
		if cb != nil && cb.OnComplete != nil {
			cb.OnComplete(messages)
		}
		return
	}
	if a.completeFn != nil {
		a.completeFn(messages)
	}
}
