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
	"sync/atomic"
	"time"

	"github.com/DotNetAge/gochat/core"
)

// Agent 迷你 agent 引擎：实现对大模型的多轮思考-工具执行循环。
//
// 核心设计：Agent 本身只负责循环编排，所有可变依赖（LLM 客户端、工具执行器、
// 循环钩子、事件总线）都通过 With* Option 注入——未注入时使用默认实现。
// 这让单元测试可以 mock 任意层，也让上层产品（如 goharness）可以替换任一
// 组件而不影响循环骨架。
type Agent struct {
	cfg      Config
	client   core.Client  // 注入的 LLM 客户端；nil 时由 execLoop 的 resolveClient 按需创建
	executor ToolExecutor // 注入的工具执行器；nil 时用 DefaultToolExecutor
	hooks    []LoopHook   // 循环钩子（无序；run 前按 Priority 排序）
	bus      EventBus     // 事件总线；nil 时用 NopEventBus

	system  string
	images  []string
	history []core.Message

	// rawTools 是 Tools() 注册但还未进入 executor 的原始工具列表。
	// run() 开始时一次性合入 DefaultToolExecutor（仅未注入自定义 ToolExecutor 时）。
	rawTools []ToolCall

	completeFn func([]core.Message) // 阻塞式对话完成回调（Complete 设置）

	// legacyCallbacks 是流式对话的旧式函数指针回调（ChatStream 使用），
	// 通过 EventBus 桥接实现——所有事件发射后同步回调对应函数。
	legacyCallbacks Callbacks

	// lastStopReason 记录最近一次 run() 循环的停止原因，Chat()/ChatStream() 返回后可查询。
	lastStopReason StopReason

	// runtimeID 运行实例 ID：经 WithRuntimeID 指定（便于在 DefaultRuntimeManager
	// 中定位与控制），或由内核自动生成。
	runtimeID string

	// taskID 任务归属：经 WithTaskID 显式指定（挂起恢复的多轮运行共享同一任务），
	// 未指定时退化为"一次任务 = 一轮运行"，TaskID 等于本轮 runtimeID。
	taskID string

	// lastRuntimeID 最近一次运行实际使用的实例 ID（含自动生成场景），
	// 供客户端运行后经 RuntimeID() 读取，再到 DefaultRuntimeManager 中定位。
	// 单实例运行保护保证写入无并发竞争。
	lastRuntimeID string

	// manager 运行实例登记表：Ask 构造时默认绑定包内唯一的 defaultRuntimeManager，
	// 所有运行统一进入控制平面（扁平架构：不存在"不登记的裸循环"旁路）。
	// 仅包内测试经 withRuntimeManager 注入独立实例以隔离。
	manager *RuntimeManager

	// running 单实例单活跃运行保护：同一 Agent 实例并发 Chat 返回错误。
	running atomic.Bool

	// runtimeValues 宿主注入的生命周期上下文值（WithRuntimeValue，构造期写入）。
	// execLoop 开始时整体注入 ctx，工具/钩子经 RuntimeValue[T].From(ctx) 取回；
	// 运行期只读，无锁。
	runtimeValues map[string]any
}

// LastStopReason 返回最近一次 run() 循环的停止原因。
// 用于在 Chat()（无 Callbacks）模式下判断循环语义：正常完成、挂起、出错或达到轮数。
// 值为 0 表示尚未执行过 run()。
func (a *Agent) LastStopReason() StopReason {
	return a.lastStopReason
}

// RuntimeID 返回最近一次运行实际使用的实例 ID。
// 未指定 WithRuntimeID 时由内核自动生成——运行结束后经此获取 ID，
// 再到 DefaultRuntimeManager 中定位与控制本次运行；从未运行过时返回空串。
func (a *Agent) RuntimeID() string {
	return a.lastRuntimeID
}

// Ask 开始一次新的对话，question 为首轮用户问题。
// 构造时默认绑定包内唯一的运行实例登记表：每次运行自动进入控制平面，
// 客户端经 DefaultRuntimeManager() 统一查询与控制。
func Ask(question string) *Agent {
	return &Agent{
		cfg:     defaultConfig(),
		history: []core.Message{core.NewUserMessage(question)},
		manager: defaultRuntimeManager,
	}
}

// Config 通过选项修改大模型配置或注入依赖（可链式调用）
func (a *Agent) Config(opts ...Option) *Agent {
	for _, o := range opts {
		o(a)
	}
	return a
}

// Tools 注册可供模型调用的工具（可链式调用）。
// 当未注入自定义 ToolExecutor 时，run() 会把这些工具合入 DefaultToolExecutor。
func (a *Agent) Tools(tools ...ToolCall) *Agent {
	a.rawTools = append(a.rawTools, tools...)
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

// Complete 设置对话完成回调（仅阻塞式 Chat 使用）：整个对话结束（成功、出错或达到轮数）时，
// 通过回调获取完整的对话上下文（原始 Message 列表）。流式对话请使用 Callbacks.OnComplete。
func (a *Agent) Complete(fn func([]core.Message)) *Agent {
	a.completeFn = fn
	return a
}

// Chat 阻塞式执行完整的多轮思考-工具循环，返回最终答案。
// 循环可能因正常终止、挂起等待外部输入、错误或达到最大轮数而返回。
// 调用方需结合 LastStopReason() 判断返回语义。
func (a *Agent) Chat() (string, error) {
	return a.run(context.Background(), nil, false)
}

// ChatStream 流式执行，通过回调接收思考增量与各阶段事件
func (a *Agent) ChatStream(cb Callbacks) error {
	a.legacyCallbacks = cb
	_, err := a.run(context.Background(), &cb, true)
	return err
}

// ChatCtx 阻塞式执行完整的多轮思考-工具循环，返回最终答案。
// 与 Chat() 的唯一差异：接受外部 ctx（超时/请求作用域取消）；
// 运行中实例的外部控制统一走 DefaultRuntimeManager（Cancel）。
// 调用方需结合 LastStopReason() 判断返回语义。
func (a *Agent) ChatCtx(ctx context.Context) (string, error) {
	return a.run(ctx, nil, false)
}

// ChatStreamCtx 流式执行，通过回调接收思考增量与各阶段事件。
// 与 ChatStream() 的唯一差异：接受外部 ctx（超时/请求作用域取消）；
// 运行中实例的外部控制统一走 DefaultRuntimeManager（Cancel）。
func (a *Agent) ChatStreamCtx(ctx context.Context, cb Callbacks) error {
	a.legacyCallbacks = cb
	_, err := a.run(ctx, &cb, true)
	return err
}

// run 一次运行的编排入口：单实例运行保护 + 控制平面状态机驱动 + 执行循环。
// 所有运行统一进入控制平面（manager 由 Ask 默认绑定；零值构造兜底绑定为
// 包内唯一 defaultRuntimeManager）——不存在不登记的裸循环旁路。
func (a *Agent) run(ctx context.Context, cb *Callbacks, stream bool) (string, error) {
	// 单实例单活跃运行：同一 Agent 实例并发 Chat 返回错误（控制平面 §4.5）
	if !a.running.CompareAndSwap(false, true) {
		return "", errors.New("goagent: 同一 Agent 实例不支持并发运行")
	}
	defer a.running.Store(false)

	// 零值防御：绕过 Ask 构造的实例兜底绑定包内唯一登记表
	if a.manager == nil {
		a.manager = defaultRuntimeManager
	}

	// runCtx 是本轮执行循环的实际 ctx，
	// 其 cancel 函数交给登记表条目持有，Cancel() 据此取消运行中的循环
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// 记录本轮实际使用的实例 ID（自动生成场景供客户端事后定位）；
	// 任务归属未显式指定时退化为本轮 runtimeID（一次任务 = 一轮运行）
	id := a.resolveRuntimeID()
	a.lastRuntimeID = id
	taskID := a.taskID
	if taskID == "" {
		taskID = id
	}

	entry, err := a.manager.acquire(id, taskID, cancel)
	if err != nil {
		a.lastStopReason = StopError
		return "", err
	}

	ans, loopErr := a.execLoop(runCtx, cb, stream)

	// 终态结算：内核是状态唯一写者，跃迁与既有 StopReason / ctx 取消一一对应
	a.settleRuntime(entry, runCtx, loopErr)
	return ans, loopErr
}

// resolveRuntimeID 返回本轮运行的实例 ID：优先用 WithRuntimeID 指定的；未指定则自动生成。
func (a *Agent) resolveRuntimeID() string {
	if a.runtimeID != "" {
		return a.runtimeID
	}
	return newRuntimeID()
}

// settleRuntime 结算本轮运行的终态（内核唯一写者）。分类规则与既有
// StopReason / ctx 取消一一对应（控制平面 §4.2）：
//   - runCtx 已取消（外部 ctx 取消或 Cancel()）→ Cancelled
//   - 错误 / 轮数耗尽 → Failed
//   - 正常结束（StopFinished / StopSuspended / 钩子中止）→ Completed
//
// Think Loop 没有中间态：挂起返回（StopSuspended）即本轮循环正常结束，
// "等待外部输入、稍后恢复"是宿主编排——宿主经 EvSuspend / LastStopReason 感知，
// 补齐输入后以新 runtimeID 对同一会话再次发起运行。
func (a *Agent) settleRuntime(entry *runtimeEntry, runCtx context.Context, loopErr error) {
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.status.isTerminal() {
		// 宿主已抢先结算（如 Pending 态被 Cancel），保留该结果
		return
	}
	switch {
	case runCtx.Err() != nil:
		entry.finishLocked(StatusCancelled)
	case loopErr != nil || a.lastStopReason == StopError || a.lastStopReason == StopMaxIterations:
		entry.finishLocked(StatusFailed)
	default:
		entry.finishLocked(StatusCompleted)
	}
}

// execLoop 多轮思考-工具执行主循环。
// 内部通过闭包捕获 bus，保证所有事件发射走同一个 EventBus 实例。
func (a *Agent) execLoop(ctx context.Context, cb *Callbacks, stream bool) (string, error) {
	// 0. 注入宿主的运行时上下文值：工具、钩子、LLM 客户端全链路可经
	// RuntimeValue[T].From(ctx) 取回（构造期写入，此处之后只读）
	ctx = withRuntimeValues(ctx, a.runtimeValues)

	// 1. 合成依赖（懒创建 + 一次性合入工具）
	client, err := a.resolveClient()
	if err != nil {
		a.lastStopReason = StopError
		return "", err
	}
	executor := a.resolveExecutor()
	bus := a.resolveBus()
	defer bus.Close() // 统一由发送方收尾关闭：所有返回路径都会触发，关闭时已无在途投递

	hooks := sortHooks(a.hooks)

	// 2. 组装消息序列
	messages, err := a.buildMessages()
	if err != nil {
		a.lastStopReason = StopError
		return "", err
	}

	// availableTools 是本轮可发给 LLM 的工具定义集合。
	// 优先从 executor 自身拿（如果实现了 ToolEnumerator），否则 fallback 到 Agent._tools 注册的。
	availableTools := a.collectTools(executor)

	// 3. 内部闭包：统一发射事件 + 回写 legacyCallbacks + 记录 lastStopReason
	emitStop := func(reason StopReason) {
		a.lastStopReason = reason
		bus.Emit(Event{Type: EvStop, Data: reason})
		if a.legacyCallbacks.Stop != nil {
			a.legacyCallbacks.Stop(reason)
		}
	}
	emitComplete := func(msgs []core.Message) {
		bus.Emit(Event{Type: EvComplete, Data: msgs})
		if a.completeFn != nil {
			a.completeFn(msgs)
		}
		if a.legacyCallbacks.OnComplete != nil {
			a.legacyCallbacks.OnComplete(msgs)
		}
	}

	// 4. 主循环
	var lastAbortReason string
	var loopErr error
loop:
	for i := 0; i < a.cfg.MaxIterations; i++ {
		// ctx 取消 → 终止
		if err := ctx.Err(); err != nil {
			loopErr = err
			break loop
		}

		// ── BeforeLLM hooks ──
		{
			input := BeforeLLMInput{
				Iteration: i,
				Messages:  messages,
				Tools:     availableTools,
			}
			for _, h := range hooks {
				result := h.BeforeLLM(ctx, input)
				if result.IsTerminal() {
					if result.Error != nil {
						loopErr = result.Error
					} else {
						lastAbortReason = result.AbortReason
					}
					break loop
				}
			}
		}

		// ── 与模型交互一轮 ──
		roundStart := time.Now()
		msg, finishReason, usage, err := a.round(ctx, client, messages, availableTools, stream, cb)
		if err != nil {
			loopErr = err
			break loop
		}
		// 每轮 LLM 调用后发射 TokenUsage 事件
		bus.Emit(Event{Type: EvTokenUsage, Data: &TokenUsageEvent{
			Iteration: i,
			Usage:     usage,
			Duration:  time.Since(roundStart),
		}})

		messages = append(messages, msg)

		// ── AfterLLM hooks（带精确 finishReason）──
		{
			input := AfterLLMInput{
				Iteration:       i,
				ResponseContent: msg.TextContent(),
				Reasoning:       msg.ReasoningContent,
				FinishReason:    finishReason,
				ToolCalls:       msg.ToolCalls,
			}
			for _, h := range hooks {
				result := h.AfterLLM(ctx, input)
				if result.IsTerminal() {
					if result.Error != nil {
						loopErr = result.Error
					} else {
						lastAbortReason = result.AbortReason
					}
					break loop
				}
			}
		}

		// ── 模型没有发起工具调用：思考完成 ──
		if len(msg.ToolCalls) == 0 {
			answer := msg.TextContent()
			bus.Emit(Event{Type: EvFinalAnswer, Data: answer})
			if cb != nil && cb.Finished != nil {
				cb.Finished(answer)
			}
			emitComplete(messages)
			emitStop(StopFinished)
			return answer, nil
		}

		// ── 发起工具调用：执行并回传结果 ──
		emitStop(StopToolLoop)
		for _, call := range msg.ToolCalls {
			tool := a.resolveTool(call.Name, executor)
			var args json.RawMessage
			if call.Arguments != "" {
				args = json.RawMessage(call.Arguments)
			}

			bus.Emit(Event{Type: EvToolExecStart, Data: &ToolExecStartData{Name: call.Name, ToolCallID: call.ID, Args: args}})
			if cb != nil && cb.BeforeToolExec != nil && tool != nil {
				cb.BeforeToolExec(tool, args)
			}

			start := time.Now()
			result, execErr := executor.Execute(ctx, call.Name, args)

			// ── 挂起检测：工具需要外部输入（权限/用户回答）──
			if errors.Is(execErr, ErrNeedExternalInput) {
				var req *ExternalInputRequest
				if errors.As(execErr, &req) {
					req.ToolCallID = call.ID // 若工具未设置 ToolCallID，从 call 回填
					bus.Emit(Event{Type: EvSuspend, Data: req})
					emitComplete(messages)
					emitStop(StopSuspended)
					return "", nil
				}
				// 包装错误只携带哨兵不携带请求体：按普通失败处理，
				// 保证 tool_calls 消息有配对的 tool 结果，协议完整
			}

			success := execErr == nil
			duration := time.Since(start)

			bus.Emit(Event{Type: EvToolExecEnd, Data: &ToolExecEndData{
				Name:       call.Name,
				ToolCallID: call.ID,
				Duration:   duration,
				Success:    success,
				Result:     result,
				Error:      execErr,
			}})
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

		// ── 一轮 Think-Act 循环结束 ──
		bus.Emit(Event{Type: EvLoopEnd, Data: &LoopEndData{Iteration: i}})
	}

	// 循环结束收尾：触发所有钩子的 Abort（LIFO），按实际结束原因分类返回
	emitComplete(messages)

	switch {
	case loopErr != nil:
		for i := len(hooks) - 1; i >= 0; i-- {
			hooks[i].Abort(ctx, "error:"+loopErr.Error())
		}
		emitStop(StopError)
		return "", loopErr
	case lastAbortReason != "":
		for i := len(hooks) - 1; i >= 0; i-- {
			hooks[i].Abort(ctx, "hook_abort:"+lastAbortReason)
		}
		emitStop(StopFinished)
		return "", nil
	default:
		for i := len(hooks) - 1; i >= 0; i-- {
			hooks[i].Abort(ctx, "max_iterations")
		}
		emitStop(StopMaxIterations)
		return "", errors.New("达到最大思考轮数，仍未得到最终答案")
	}
}

// round 与模型交互一轮。
// 返回模型消息、精确的 finishReason、本轮 token 消耗（可能为 nil）、错误。
func (a *Agent) round(ctx context.Context, client core.Client, messages []core.Message, availableTools []core.Tool, stream bool, cb *Callbacks) (core.Message, string, *core.Usage, error) {
	opts := a.chatOptions(availableTools, stream)
	if !stream {
		resp, err := client.Chat(ctx, messages, opts...)
		if err != nil {
			return core.Message{}, "", nil, err
		}
		return resp.Message, resp.FinishReason, resp.Usage, nil
	}
	msg, finishReason, usage, err := a.chatStream(ctx, client, messages, opts, cb)
	return msg, finishReason, usage, err
}

// chatStream 流式对话：思考增量经 ThinkCallback 输出，返回累积完整的模型消息 + finishReason + usage。
func (a *Agent) chatStream(ctx context.Context, client core.Client, messages []core.Message, opts []core.Option, cb *Callbacks) (core.Message, string, *core.Usage, error) {
	stream, err := client.ChatStream(ctx, messages, opts...)
	if err != nil {
		return core.Message{}, "", nil, err
	}
	defer stream.Close()

	var content, thinking strings.Builder
	think := a.thinkFn(cb)
	bus := a.resolveBus()

	for stream.Next() {
		ev := stream.Event()
		if ev.Err != nil {
			return core.Message{}, "", nil, ev.Err
		}
		switch ev.Type {
		case core.EventContent:
			content.WriteString(ev.Content)
			bus.Emit(Event{Type: EvContentDelta, Data: ev.Content})
			if think != nil {
				think(ev.Content)
			}
		case core.EventThinking:
			thinking.WriteString(ev.Content)
			bus.Emit(Event{Type: EvThinkingDelta, Data: ev.Content})
			if think != nil {
				think(ev.Content)
			}
		case core.EventToolCall:
			for _, td := range ev.ToolCallDeltas {
				bus.Emit(Event{Type: EvToolUseDelta, Data: td})
			}
		}
	}
	bus.Emit(Event{Type: EvThinkingDone})

	// 流结束后从 stream 获取 finishReason 和 usage
	finishReason := ""
	if ev := stream.Event(); ev.Type == core.EventDone {
		finishReason = ev.FinishReason
	}
	usage := stream.Usage()

	return core.Message{
		Role:             core.RoleAssistant,
		Content:          []core.ContentBlock{{Type: core.ContentTypeText, Text: content.String()}},
		ReasoningContent: thinking.String(),
		ToolCalls:        stream.ToolCalls(),
	}, finishReason, usage, nil
}

// ── 依赖解析 ──

// resolveClient 返回可直接使用的 core.Client。优先用注入的；未注入则通过 Config 创建。
func (a *Agent) resolveClient() (core.Client, error) {
	if a.client != nil {
		return a.client, nil
	}
	return NewDefaultLLMClient(a.cfg)
}

// resolveExecutor 返回可直接使用的 ToolExecutor。优先用注入的；
// 未注入则用 DefaultToolExecutor 合入 Tools() 注册的工具。
func (a *Agent) resolveExecutor() ToolExecutor {
	if a.executor != nil {
		return a.executor
	}
	exec := NewDefaultToolExecutor(a.rawTools...)
	a.rawTools = nil // 已合入，清空
	return exec
}

// resolveBus 返回可直接使用的 EventBus。nil 时返回 NopEventBus。
func (a *Agent) resolveBus() EventBus {
	if a.bus != nil {
		return a.bus
	}
	return NopEventBus{}
}

// collectTools 收集本轮可用工具定义。
// 优先从 executor 拿（如果它实现了 ToolEnumerator 可选接口），
// 否则 fallback 到 Agent._tools 注册的。
func (a *Agent) collectTools(exec ToolExecutor) []core.Tool {
	var toolCalls []ToolCall
	if enumerator, ok := exec.(ToolEnumerator); ok {
		toolCalls = enumerator.Tools()
	} else {
		toolCalls = a.rawTools
	}
	out := make([]core.Tool, 0, len(toolCalls))
	for _, t := range toolCalls {
		out = append(out, core.Tool{
			Name:        t.Name(),
			Description: t.Description(),
			Parameters:  t.Parameters(),
		})
	}
	return out
}

// resolveTool 按名称查找工具。优先从 executor 查找（executor 未导出 Get 时返回 nil）。
func (a *Agent) resolveTool(name string, exec ToolExecutor) ToolCall {
	if de, ok := exec.(*DefaultToolExecutor); ok {
		return de.Get(name)
	}
	return nil
}

// ── LLM 调用参数 ──

// chatOptions 组装每轮请求参数。流式默认开启模型思考。
func (a *Agent) chatOptions(availableTools []core.Tool, stream bool) []core.Option {
	opts := make([]core.Option, 0, 2)
	if len(availableTools) > 0 {
		opts = append(opts, core.WithTools(availableTools...))
	}
	if stream {
		opts = append(opts, core.WithThinking(0))
	}
	return opts
}

// ── 消息组装 ──

// buildMessages 组装完整消息序列：系统提示词 + 历史（图片附加到当前问题）
func (a *Agent) buildMessages() ([]core.Message, error) {
	msgs := make([]core.Message, 0, len(a.history)+2)
	if a.system != "" {
		msgs = append(msgs, core.NewSystemMessage(a.system))
	}
	for i, m := range a.history {
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

// thinkFn 从 Callbacks 提取思考回调（nil-safe）
func (a *Agent) thinkFn(cb *Callbacks) func(string) {
	if cb == nil || cb.ThinkCallback == nil {
		return nil
	}
	return cb.ThinkCallback
}
