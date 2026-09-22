package goagent

import (
	"context"
)

// RuntimeValue 是宿主注入 Agent 运行时的生命周期上下文值槽（类型安全）。
//
// 解决的问题：工具与循环钩子只收到请求作用域的 ctx（取消/超时），宿主无法
// 把会话对象、用户身份、沙箱引用等 Agent 生命周期上下文传递到工具与钩子
// 内部。RuntimeValue 补上这条通道，语义等同于"Agent 级、构造期、类型安全
// 的 context.WithValue"：
//
//	// 1. 声明值槽（包级，按用途各声明一个）
//	var sandboxVal = goagent.NewRuntimeValue[*Sandbox]("sandbox")
//
//	// 2. 宿主构造期注入（可多个；同名后注入覆盖先注入）
//	goagent.Ask(q).Config(goagent.WithRuntimeValue(sandboxVal, sb))
//
//	// 3. 工具 / 钩子内取回（ctx 已由内核自动注入，接口签名零变更）
//	if sb, ok := sandboxVal.From(ctx); ok { ... }
//
// 并发模型：值仅在构造期（Config 链）写入，运行期只读——内核不加锁、
// 不提供运行期写入 API，从机制上排除数据竞争。
// 取回带类型断言：注入方与消费方共用同一 RuntimeValue[T] 声明即天然类型
// 匹配；类型不匹配时 From 返回零值 + false，不 panic。
type RuntimeValue[T any] struct {
	name string
}

// runtimeValueKey 是 ctx key 的载体类型（非导出结构体实例，符合 context 惯例：
// 用自定义类型做 key 避免与其他包的值冲突）。
type runtimeValueKey struct {
	name string
}

// NewRuntimeValue 声明一个运行时值槽。name 即槽名（同名即同槽）。
func NewRuntimeValue[T any](name string) RuntimeValue[T] {
	return RuntimeValue[T]{name: name}
}

// Name 返回槽名（调试用途）。
func (v RuntimeValue[T]) Name() string { return v.name }

// From 从 ctx 取出注入值。ctx 来自内核注入（工具 Execute / 钩子回调收到的
// ctx 均已携带）；未注入或类型不匹配时返回零值 + false。
func (v RuntimeValue[T]) From(ctx context.Context) (T, bool) {
	var zero T
	val, ok := ctx.Value(runtimeValueKey{name: v.name}).(T)
	if !ok {
		return zero, false
	}
	return val, true
}

// withRuntimeValues 把全部运行时值注入 ctx（execLoop 开始时调用一次，全链路
// 可见：工具、循环钩子、LLM 客户端均可经 RuntimeValue[T].From 取回）。
func withRuntimeValues(ctx context.Context, values map[string]any) context.Context {
	if len(values) == 0 {
		return ctx
	}
	for name, val := range values {
		ctx = context.WithValue(ctx, runtimeValueKey{name: name}, val)
	}
	return ctx
}
