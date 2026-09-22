package goagent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/DotNetAge/gochat/core"
)

// RuntimeValue 注入/取回机制验收：
// 宿主构造期注入 → 内核 execLoop 注入 ctx → 工具与钩子经 From(ctx) 取回，
// 全程不改 ToolCall / LoopHook 的接口签名。

// 测试用值槽：strVal 与 mismatchVal 同名不同型，用于验证类型不匹配安全降级。
var (
	strVal      = NewRuntimeValue[string]("session-user")
	mismatchVal = NewRuntimeValue[int]("session-user")
	hookVal     = NewRuntimeValue[string]("hook-scope")
)

// valueProbe 是探测工具：执行时从 ctx 取回运行时值并记录。
type valueProbe struct{}

func (valueProbe) Name() string                { return "value_probe" }
func (valueProbe) Description() string         { return "探测工具：记录运行时值取回结果" }
func (valueProbe) Parameters() json.RawMessage { return json.RawMessage(`{}`) }

func (valueProbe) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	v, ok := strVal.From(ctx)
	probeToolValue, probeToolOK = v, ok
	_, mismatchOK := mismatchVal.From(ctx)
	probeMismatchOK = mismatchOK
	return "探测完成", nil
}

var (
	probeToolValue  string
	probeToolOK     bool
	probeMismatchOK bool
	probeHookValue  string
	probeHookOK     bool
)

// valueHook 是探测钩子：BeforeLLM 时从 ctx 取回运行时值并记录。
type valueHook struct{}

func (valueHook) Priority() int { return 0 }

func (valueHook) BeforeLLM(ctx context.Context, input BeforeLLMInput) HookResult {
	v, ok := hookVal.From(ctx)
	probeHookValue, probeHookOK = v, ok
	return HookResult{}
}

func (valueHook) AfterLLM(ctx context.Context, input AfterLLMInput) HookResult { return HookResult{} }

func (valueHook) Abort(ctx context.Context, reason string) {}

// TestRuntimeValueAccessibleFromToolAndHook：工具与钩子都能取回宿主注入的值。
func TestRuntimeValueAccessibleFromToolAndHook(t *testing.T) {
	m := newRuntimeManager()
	client := &cpClient{chatFn: func(ctx context.Context, call int) (core.Response, error) {
		if call == 0 {
			return toolCallResp("call-1", "value_probe", `{}`), nil
		}
		return finalResp("done"), nil
	}}

	agent := newCPAgent("rv-test", m, client, valueProbe{}).
		Config(
			WithRuntimeValue(strVal, "alice"),
			WithRuntimeValue(hookVal, "tenant-a"),
			WithLoopHooks(valueHook{}),
		)

	if _, err := agent.ChatCtx(context.Background()); err != nil {
		t.Fatal(err)
	}

	if !probeToolOK || probeToolValue != "alice" {
		t.Fatalf("工具应取回注入值 alice，得到 ok=%v value=%q", probeToolOK, probeToolValue)
	}
	if !probeHookOK || probeHookValue != "tenant-a" {
		t.Fatalf("钩子应取回注入值 tenant-a，得到 ok=%v value=%q", probeHookOK, probeHookValue)
	}
}

// TestRuntimeValueTypeMismatchSafe：同名不同型的槽取回应安全降级（false，不 panic）。
func TestRuntimeValueTypeMismatchSafe(t *testing.T) {
	m := newRuntimeManager()
	client := &cpClient{chatFn: func(ctx context.Context, call int) (core.Response, error) {
		if call == 0 {
			return toolCallResp("call-1", "value_probe", `{}`), nil
		}
		return finalResp("done"), nil
	}}

	agent := newCPAgent("rv-mismatch", m, client, valueProbe{}).
		Config(WithRuntimeValue(strVal, "alice")) // 只注入 string 版槽

	if _, err := agent.ChatCtx(context.Background()); err != nil {
		t.Fatal(err)
	}

	if probeMismatchOK {
		t.Fatal("同名不同型的槽应取回失败（false），实际成功")
	}
}

// TestRuntimeValueNotInjected：未注入槽时取回安全降级（false）。
func TestRuntimeValueNotInjected(t *testing.T) {
	m := newRuntimeManager()
	client := &cpClient{chatFn: func(ctx context.Context, call int) (core.Response, error) {
		if call == 0 {
			return toolCallResp("call-1", "value_probe", `{}`), nil
		}
		return finalResp("done"), nil
	}}

	agent := newCPAgent("rv-absent", m, client, valueProbe{})

	if _, err := agent.ChatCtx(context.Background()); err != nil {
		t.Fatal(err)
	}

	if probeToolOK {
		t.Fatal("未注入槽应取回失败（false），实际成功")
	}
}

// TestRuntimeValueOverride：同名槽重复注入时后注入覆盖先注入。
func TestRuntimeValueOverride(t *testing.T) {
	m := newRuntimeManager()
	client := &cpClient{chatFn: func(ctx context.Context, call int) (core.Response, error) {
		if call == 0 {
			return toolCallResp("call-1", "value_probe", `{}`), nil
		}
		return finalResp("done"), nil
	}}

	agent := newCPAgent("rv-override", m, client, valueProbe{}).
		Config(
			WithRuntimeValue(strVal, "first"),
			WithRuntimeValue(strVal, "second"),
		)

	if _, err := agent.ChatCtx(context.Background()); err != nil {
		t.Fatal(err)
	}

	if probeToolValue != "second" {
		t.Fatalf("同名槽后注入应覆盖先注入，得到 %q", probeToolValue)
	}
}
