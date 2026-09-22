package goagent

import (
	"os"
	"time"

	"github.com/DotNetAge/gochat"
	"github.com/DotNetAge/gochat/core"
)

// 默认配置：本机 Ollama minicpm 视觉模型
const (
	DefaultBaseURL = "http://localhost:11434" // Ollama 原生 API 端点
	DefaultModel   = "minicpm-v4.6:latest"
	DefaultAPIKey  = "ollama"
)

// Config 大模型客户端配置，可通过环境变量或 With* 选项覆盖
type Config struct {
	BaseURL       string            // Ollama / OpenAI 兼容端点基础地址
	APIKey        string            // API 密钥（Ollama 可填任意占位）
	Model         string            // 模型名称
	Temperature   float64           // 采样温度
	Timeout       time.Duration     // HTTP 超时
	MaxIterations int               // 多轮思考-工具循环的最大轮数
	ClientType    gochat.ClientType // gochat 客户端类型（默认 OllamaClient）
}

// defaultConfig 返回默认配置：优先读取环境变量，其次使用内置默认值
func defaultConfig() Config {
	return Config{
		BaseURL:       envOr("GOAGENT_BASE_URL", DefaultBaseURL),
		APIKey:        envOr("GOAGENT_API_KEY", DefaultAPIKey),
		Model:         envOr("GOAGENT_MODEL", DefaultModel),
		Temperature:   0.7,
		Timeout:       10 * time.Minute,
		MaxIterations: 30,
		ClientType:    gochat.OllamaClient,
	}
}

// envOr 读取环境变量，为空时返回默认值
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Option 修改 Agent 配置（编程方式覆盖环境变量或注入依赖）
type Option func(*Agent)

// WithBaseURL 设置模型端点基础地址（Ollama 原生 API 为 http://localhost:11434）
func WithBaseURL(u string) Option {
	return func(a *Agent) { a.cfg.BaseURL = u }
}

// WithAPIKey 设置 API 密钥
func WithAPIKey(k string) Option {
	return func(a *Agent) { a.cfg.APIKey = k }
}

// WithModel 设置模型名称
func WithModel(m string) Option {
	return func(a *Agent) { a.cfg.Model = m }
}

// WithTemperature 设置采样温度
func WithTemperature(t float64) Option {
	return func(a *Agent) { a.cfg.Temperature = t }
}

// WithTimeout 设置 HTTP 超时
func WithTimeout(d time.Duration) Option {
	return func(a *Agent) { a.cfg.Timeout = d }
}

// WithMaxIterations 设置多轮思考-工具循环的最大轮数
func WithMaxIterations(n int) Option {
	return func(a *Agent) { a.cfg.MaxIterations = n }
}

// WithClientType 设置 gochat 客户端类型（默认 OllamaClient；其他 OpenAI 兼容服务可换 OpenAIClient）
func WithClientType(t gochat.ClientType) Option {
	return func(a *Agent) { a.cfg.ClientType = t }
}

// WithLLMClient 注入 gochat/core.Client 实例，跳过内部按 ClientType 创建的默认逻辑。
// 外部可传入 mock、带重试包装或任何自定义实现。
func WithLLMClient(c core.Client) Option {
	return func(a *Agent) { a.client = c }
}

// WithToolExecutor 注入自定义工具执行器。
// 外部可实现带权限门控、超时、并发调度等增强能力的 ToolExecutor。
func WithToolExecutor(exec ToolExecutor) Option {
	return func(a *Agent) { a.executor = exec }
}

// WithLoopHooks 注册一个或多个循环钩子。钩子会按 Priority 自动排序。
func WithLoopHooks(hooks ...LoopHook) Option {
	return func(a *Agent) { a.hooks = append(a.hooks, hooks...) }
}

// WithEventBus 注入事件总线。默认使用进程内通道实现。
func WithEventBus(bus EventBus) Option {
	return func(a *Agent) { a.bus = bus }
}

// WithRuntimeID 指定本次运行的实例 ID，便于经 DefaultRuntimeManager 定位与控制。
// 未指定时内核自动生成全局唯一 ID（运行后经 Agent.RuntimeID() 取回）。
func WithRuntimeID(id string) Option {
	return func(a *Agent) { a.runtimeID = id }
}

// WithTaskID 指定任务归属：一次任务跨多轮运行（挂起恢复、宿主编排续跑）时，
// 各轮运行共享同一 TaskID，客户端经 DefaultRuntimeManager().GetByTask(taskID)
// 统一定位"我的任务现在怎么样了"。
// 未指定时退化为"一次任务 = 一轮运行"，TaskID 等于本轮 runtimeID——普通调用方无感知。
func WithTaskID(taskID string) Option {
	return func(a *Agent) { a.taskID = taskID }
}

// WithRuntimeValue 向 Agent 运行时注入生命周期上下文值（构造期写入，运行期只读）。
// 工具与循环钩子经 RuntimeValue[T].From(ctx) 取回——详见 RuntimeValue 类型文档。
// 同名槽重复注入时后注入覆盖先注入。
func WithRuntimeValue[T any](v RuntimeValue[T], value T) Option {
	return func(a *Agent) {
		if a.runtimeValues == nil {
			a.runtimeValues = make(map[string]any)
		}
		a.runtimeValues[v.name] = value
	}
}

// withRuntimeManager 注入独立登记表——仅供包内测试隔离使用（生产代码的
// 所有运行统一进入包内唯一的 defaultRuntimeManager，外部禁止注入或创建）。
func withRuntimeManager(m *RuntimeManager) Option {
	return func(a *Agent) { a.manager = m }
}
