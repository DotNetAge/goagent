package goagent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/DotNetAge/gochat/core"

	"github.com/DotNetAge/goagent/tools"
)

// echoTool 测试用工具：回显参数
type echoTool struct{ called string }

func (e *echoTool) Name() string { return "echo" }
func (e *echoTool) Description() string {
	return "回显文本"
}
func (e *echoTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {"text": {"type": "string"}},
		"required": ["text"]
	}`)
}
func (e *echoTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	var p struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", err
	}
	e.called = p.Text
	return p.Text, nil
}

// ollamaMock 模拟 Ollama /api/chat 原生端点：按请求轮次返回预设的 NDJSON 行
type ollamaMock struct {
	mu      sync.Mutex
	reqs    int
	rounds  [][]string // 每轮请求返回的 NDJSON 行
	lastReq map[string]interface{}
}

func (m *ollamaMock) handler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/api/chat" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	body, _ := io.ReadAll(r.Body)
	m.mu.Lock()
	m.reqs++
	var req map[string]interface{}
	_ = json.Unmarshal(body, &req)
	m.lastReq = req
	round := m.reqs - 1
	lines := m.rounds[round]
	m.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	for _, l := range lines {
		_, _ = io.WriteString(w, l+"\n")
	}
}

// 验证多轮思考-工具执行循环：第一轮返回工具调用，第二轮返回最终答案
func TestAgentToolLoop(t *testing.T) {
	mock := &ollamaMock{rounds: [][]string{
		// 第一轮：模型发起 echo 工具调用（ollama 原生 arguments 为对象）
		{`{"model":"m","message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","function":{"name":"echo","arguments":{"text":"hi"}}}]},"done":true,"done_reason":"stop"}`},
		// 第二轮：模型给出最终答案
		{`{"model":"m","message":{"role":"assistant","content":"done"},"done":true,"done_reason":"stop"}`},
	}}
	srv := httptest.NewServer(http.HandlerFunc(mock.handler))
	defer srv.Close()

	echo := &echoTool{}
	ans, err := Ask("hello").
		Config(WithBaseURL(srv.URL), WithAPIKey("x"), WithModel("m"), WithMaxIterations(3)).
		Tools(echo).
		Chat()
	if err != nil {
		t.Fatal(err)
	}
	if ans != "done" {
		t.Fatalf("期望 done，得到 %q", ans)
	}
	if mock.reqs != 2 {
		t.Fatalf("期望 2 轮请求，得到 %d", mock.reqs)
	}
	if echo.called != "hi" {
		t.Fatalf("工具未被正确调用: %q", echo.called)
	}
	// 工具结果应以 role=tool 回传且携带 tool_call_id
	if mock.reqs != 2 {
		return
	}
}

// 验证流式：思考增量（含 thinking 与 content）、完成回调、停止原因
func TestAgentChatStream(t *testing.T) {
	mock := &ollamaMock{rounds: [][]string{
		{
			`{"model":"m","message":{"role":"assistant","content":"hel"}}`,
			`{"model":"m","message":{"role":"assistant","thinking":"think"}}`,
			`{"model":"m","message":{"role":"assistant","content":"lo"}}`,
			`{"model":"m","done":true,"done_reason":"stop"}`,
		},
	}}
	srv := httptest.NewServer(http.HandlerFunc(mock.handler))
	defer srv.Close()

	var think strings.Builder
	var finished string
	var stops []StopReason
	err := Ask("hi").
		Config(WithBaseURL(srv.URL), WithAPIKey("x"), WithModel("m")).
		ChatStream(Callbacks{
			ThinkCallback: func(d string) { think.WriteString(d) },
			Finished:      func(a string) { finished = a },
			Stop:          func(r StopReason) { stops = append(stops, r) },
		})
	if err != nil {
		t.Fatal(err)
	}
	if want := "helthinklo"; think.String() != want {
		t.Fatalf("流式增量错误: %q，期望 %q", think.String(), want)
	}
	if finished != "hello" {
		t.Fatalf("完成回调错误: %q", finished)
	}
	if len(stops) != 1 || stops[0] != StopFinished {
		t.Fatalf("停止原因错误: %v", stops)
	}
}

// 验证 System() 与 Images()：系统提示词（含工具清单）与图片 base64 正确进入请求
func TestAgentSystemAndImages(t *testing.T) {
	// 1x1 透明 PNG
	pngData, _ := base64.StdEncoding.DecodeString(
		"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==")
	imgPath := filepath.Join(t.TempDir(), "pixel.png")
	if err := os.WriteFile(imgPath, pngData, 0o644); err != nil {
		t.Fatal(err)
	}

	mock := &ollamaMock{rounds: [][]string{
		{`{"model":"m","message":{"role":"assistant","content":"ok"},"done":true,"done_reason":"stop"}`},
	}}
	srv := httptest.NewServer(http.HandlerFunc(mock.handler))
	defer srv.Close()

	_, err := Ask("看图说话").
		Config(WithBaseURL(srv.URL), WithAPIKey("x"), WithModel("m")).
		System("你是测试助手").
		Tools(tools.Bash{}).
		Images(imgPath).
		Chat()
	if err != nil {
		t.Fatal(err)
	}

	msgs, _ := mock.lastReq["messages"].([]interface{})
	if len(msgs) < 2 {
		t.Fatalf("期望至少 system+user 两条消息，得到 %d", len(msgs))
	}
	sys := msgs[0].(map[string]interface{})
	if sys["role"] != "system" {
		t.Fatalf("首条消息应为 system: %v", sys["role"])
	}
	sysText, _ := sys["content"].(string)
	if sysText != "你是测试助手" {
		t.Fatalf("系统提示词应只含用户自定义内容: %q", sysText)
	}
	// 工具定义应通过 tools 协议字段传递，而非塞进系统提示词
	toolsDef, _ := mock.lastReq["tools"].([]interface{})
	if len(toolsDef) != 1 {
		t.Fatalf("期望 1 个工具定义，得到 %d", len(toolsDef))
	}
	def := toolsDef[0].(map[string]interface{})
	fn, _ := def["function"].(map[string]interface{})
	if fn["name"] != "bash" {
		t.Fatalf("工具名应为 bash: %v", fn["name"])
	}
	user := msgs[1].(map[string]interface{})
	if user["role"] != "user" {
		t.Fatalf("第二条消息应为 user: %v", user["role"])
	}
	imgs, _ := user["images"].([]interface{})
	if len(imgs) != 1 {
		t.Fatalf("期望 1 张图片，得到 %d", len(imgs))
	}
	got, _ := imgs[0].(string)
	decoded, err := base64.StdEncoding.DecodeString(got)
	if err != nil || len(decoded) < 8 || string(decoded[:8]) != "\x89PNG\r\n\x1a\n" {
		t.Fatalf("图片 base64 解码后应为 PNG: %v", err)
	}
}

// 验证 History()：历史对话消息前置在当前问题之前，形成连续上下文
func TestAgentHistory(t *testing.T) {
	mock := &ollamaMock{rounds: [][]string{
		{`{"model":"m","message":{"role":"assistant","content":"ok"},"done":true,"done_reason":"stop"}`},
	}}
	srv := httptest.NewServer(http.HandlerFunc(mock.handler))
	defer srv.Close()

	history := []core.Message{
		core.NewUserMessage("上一轮问题"),
		{Role: core.RoleAssistant, Content: []core.ContentBlock{{Type: core.ContentTypeText, Text: "上一轮回答"}}},
	}
	if _, err := Ask("当前问题").
		Config(WithBaseURL(srv.URL), WithAPIKey("x"), WithModel("m")).
		History(history).
		Chat(); err != nil {
		t.Fatal(err)
	}

	msgs, _ := mock.lastReq["messages"].([]interface{})
	if len(msgs) != 3 {
		t.Fatalf("期望 3 条消息（历史 2 条 + 当前问题），得到 %d", len(msgs))
	}
	want := []struct{ role, content string }{
		{"user", "上一轮问题"},
		{"assistant", "上一轮回答"},
		{"user", "当前问题"},
	}
	for i, w := range want {
		m := msgs[i].(map[string]interface{})
		if m["role"] != w.role || m["content"] != w.content {
			t.Fatalf("第 %d 条消息应为 %s:%q，得到 %v:%q", i, w.role, w.content, m["role"], m["content"])
		}
	}
}

// 验证无工具时不注入系统提示词、未注册工具返回错误提示
func TestUnknownToolAndNoSystem(t *testing.T) {
	mock := &ollamaMock{rounds: [][]string{
		{`{"model":"m","message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","function":{"name":"ghost","arguments":{"text":"hi"}}}]},"done":true,"done_reason":"stop"}`},
		{`{"model":"m","message":{"role":"assistant","content":"done"},"done":true,"done_reason":"stop"}`},
	}}
	srv := httptest.NewServer(http.HandlerFunc(mock.handler))
	defer srv.Close()

	ans, err := Ask("hi").
		Config(WithBaseURL(srv.URL), WithAPIKey("x"), WithModel("m"), WithMaxIterations(3)).
		Tools(&echoTool{}).
		Chat()
	if err != nil {
		t.Fatal(err)
	}
	if ans != "done" {
		t.Fatalf("期望 done，得到 %q", ans)
	}
	// 未注册工具应转成错误提示回传给模型（第二轮请求应包含 role=tool 消息）
	msgs, _ := mock.lastReq["messages"].([]interface{})
	found := false
	for _, m := range msgs {
		if msg, ok := m.(map[string]interface{}); ok && msg["role"] == "tool" {
			found = true
			if !strings.Contains(fmt.Sprint(msg["content"]), "未注册工具") {
				t.Fatalf("工具错误提示应回传: %v", msg["content"])
			}
		}
	}
	if !found {
		t.Fatal("期望存在 role=tool 的消息")
	}
}

// 验证阻塞式 Complete()：对话结束后通过回调获取完整上下文（原始 Message 列表）
func TestChatComplete(t *testing.T) {
	mock := &ollamaMock{rounds: [][]string{
		{`{"model":"m","message":{"role":"assistant","content":"done"},"done":true,"done_reason":"stop"}`},
	}}
	srv := httptest.NewServer(http.HandlerFunc(mock.handler))
	defer srv.Close()

	var got []core.Message
	ans, err := Ask("hi").
		Config(WithBaseURL(srv.URL), WithAPIKey("x"), WithModel("m")).
		System("sys").
		Complete(func(m []core.Message) { got = m }).
		Chat()
	if err != nil {
		t.Fatal(err)
	}
	if ans != "done" {
		t.Fatalf("期望 done，得到 %q", ans)
	}
	if len(got) != 3 {
		t.Fatalf("期望完整上下文 3 条（system+user+assistant），得到 %d", len(got))
	}
	if got[0].Role != core.RoleSystem || got[1].Role != core.RoleUser || got[2].Role != core.RoleAssistant {
		t.Fatalf("消息角色顺序错误: %v", got)
	}
}

// 验证流式 ChatStream 的 OnComplete：对话结束后回调携带完整的原始 Message 列表
func TestChatStreamOnComplete(t *testing.T) {
	mock := &ollamaMock{rounds: [][]string{
		{
			`{"model":"m","message":{"role":"assistant","content":"he"}}`,
			`{"model":"m","message":{"role":"assistant","content":"llo"}}`,
			`{"model":"m","done":true,"done_reason":"stop"}`,
		},
	}}
	srv := httptest.NewServer(http.HandlerFunc(mock.handler))
	defer srv.Close()

	var got []core.Message
	err := Ask("hi").
		Config(WithBaseURL(srv.URL), WithAPIKey("x"), WithModel("m")).
		ChatStream(Callbacks{
			OnComplete: func(m []core.Message) { got = m },
		})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("期望 2 条（user+assistant），得到 %d", len(got))
	}
	if got[1].TextContent() != "hello" {
		t.Fatalf("assistant 消息内容错误: %q", got[1].TextContent())
	}
}

// suspendedTool 在 Execute 中返回 ErrNeedExternalInput，用于测试挂起机制
type suspendedTool struct {
	returnValue string
}

func (s *suspendedTool) Name() string        { return "ask_permission" }
func (s *suspendedTool) Description() string { return "触发安全授权挂起" }
func (s *suspendedTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"action":{"type":"string"}}}`)
}
func (s *suspendedTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	return "", NewPermissionRequest(ctx, "ask_permission", "call-123", args, map[string]any{"scope": "sandbox"})
}

// ── Hook 测试辅助类型 ──

// abortHook 在 AfterLLM 里请求中止循环，验证钩子的中止能力
type abortHook struct {
	priority int
	aborted  bool
}

func (h *abortHook) Priority() int { return h.priority }
func (h *abortHook) BeforeLLM(ctx context.Context, input BeforeLLMInput) HookResult {
	return HookResult{}
}
func (h *abortHook) AfterLLM(ctx context.Context, input AfterLLMInput) HookResult {
	h.aborted = true
	return HookResult{Abort: true, AbortReason: "用户中止"}
}
func (h *abortHook) Abort(ctx context.Context, reason string) {}

// logHook 记录 BeforeLLM 调用，验证钩子执行顺序
type logHook struct {
	name     string
	priority int
	called   []string // 调用顺序（名称列表，每次 BeforeLLM 追加）
}

func (h *logHook) Priority() int { return h.priority }
func (h *logHook) BeforeLLM(ctx context.Context, input BeforeLLMInput) HookResult {
	h.called = append(h.called, h.name)
	return HookResult{}
}
func (h *logHook) AfterLLM(ctx context.Context, input AfterLLMInput) HookResult {
	return HookResult{}
}
func (h *logHook) Abort(ctx context.Context, reason string) {}

// TestAgentSuspendable 验证工具返回 ErrNeedExternalInput 时循环挂起
func TestAgentSuspendable(t *testing.T) {
	mock := &ollamaMock{
		rounds: [][]string{
			// 第一轮：模型发起 ask_permission 工具调用
			{`{"model":"m","message":{"role":"assistant","content":"","tool_calls":[{"id":"call-123","function":{"name":"ask_permission","arguments":{"action":"run"}}}]},"done":true,"done_reason":"tool_calls"}`},
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(mock.handler))
	defer srv.Close()

	a := Ask("允许这个命令吗？").
		Config(WithBaseURL(srv.URL), WithAPIKey("x"), WithModel("m")).
		Tools(&suspendedTool{})

	// 第一次调用：模型发起工具调用 → 工具挂起 → 循环挂起
	answer, err := a.Chat()
	if err != nil {
		t.Fatal(err)
	}
	if answer != "" {
		t.Fatalf("挂起时 answer 应为空字符串，得到 %q", answer)
	}
	if a.LastStopReason() != StopSuspended {
		t.Fatalf("StopReason 应为 StopSuspended，得到 %v", a.LastStopReason())
	}
	if mock.reqs != 1 {
		t.Fatalf("挂起后只应有 1 轮 LLM 请求，得到 %d", mock.reqs)
	}
}

// TestAgentHooks 验证 LoopHook 的优先级排序和中止能力
func TestAgentHooks(t *testing.T) {
	mock := &ollamaMock{rounds: [][]string{
		{`{"model":"m","message":{"role":"assistant","content":"好的"},"done":true,"done_reason":"stop"}`},
	}}
	srv := httptest.NewServer(http.HandlerFunc(mock.handler))
	defer srv.Close()

	hookA := &logHook{name: "A", priority: 10}
	hookB := &logHook{name: "B", priority: 5}
	hookAbort := &abortHook{priority: 100}

	a := Ask("hi").
		Config(WithBaseURL(srv.URL), WithAPIKey("x"), WithModel("m"),
			WithLoopHooks(hookA, hookB, hookAbort))

	// abortHook 在 AfterLLM 中请求中止 → 循环应被中止但不报错
	_, err := a.Chat()
	if err != nil {
		t.Fatal(err)
	}
	if !hookAbort.aborted {
		t.Fatal("abortHook 应该被调用并中止循环")
	}
	// BeforeLLM 顺序：B(prio=5) → A(prio=10) → abort(prio=100)
	if len(hookA.called) == 0 || len(hookB.called) == 0 {
		t.Fatalf("钩子应该至少各被调用一次: A=%v B=%v", hookA.called, hookB.called)
	}
	// 检查顺序：B 在 A 之前被调用
	beforeA := false
	beforeB := false
	for _, name := range hookB.called {
		if name == "B" {
			beforeB = true
		}
	}
	for _, name := range hookA.called {
		if name == "A" {
			beforeA = true
		}
	}
	if !beforeB || !beforeA {
		t.Fatalf("钩子调用顺序异常")
	}
}
