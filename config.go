package goagent

import (
	"os"
	"time"

	"github.com/DotNetAge/gochat"
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
		Model:         envOr("GO_AGENT_MODEL", DefaultModel),
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

// Option 修改 Agent 配置（编程方式覆盖环境变量）
type Option func(*Config)

// WithBaseURL 设置模型端点基础地址（Ollama 原生 API 为 http://localhost:11434）
func WithBaseURL(u string) Option { return func(c *Config) { c.BaseURL = u } }

// WithAPIKey 设置 API 密钥
func WithAPIKey(k string) Option { return func(c *Config) { c.APIKey = k } }

// WithModel 设置模型名称
func WithModel(m string) Option { return func(c *Config) { c.Model = m } }

// WithTemperature 设置采样温度
func WithTemperature(t float64) Option { return func(c *Config) { c.Temperature = t } }

// WithTimeout 设置 HTTP 超时
func WithTimeout(d time.Duration) Option { return func(c *Config) { c.Timeout = d } }

// WithMaxIterations 设置多轮思考-工具循环的最大轮数
func WithMaxIterations(n int) Option { return func(c *Config) { c.MaxIterations = n } }

// WithClientType 设置 gochat 客户端类型（默认 OllamaClient；其他 OpenAI 兼容服务可换 OpenAIClient）
func WithClientType(t gochat.ClientType) Option { return func(c *Config) { c.ClientType = t } }
