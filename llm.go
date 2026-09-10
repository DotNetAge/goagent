package goagent

import (
	"fmt"

	"github.com/DotNetAge/gochat"
	"github.com/DotNetAge/gochat/client/anthropic"
	"github.com/DotNetAge/gochat/client/deepseek"
	"github.com/DotNetAge/gochat/client/ollama"
	"github.com/DotNetAge/gochat/client/openai"
	"github.com/DotNetAge/gochat/core"
)

// NewDefaultLLMClient 根据 Config 中的 ClientType 创建对应的 gochat 客户端。
// 当 Agent 未显式注入 core.Client 时，通过此工厂按需创建。
//
// 支持的 ClientType：
//   - gochat.OllamaClient       → ollama 原生 API
//   - gochat.OpenAIClient       → OpenAI 兼容端点
//   - gochat.QwenClient         → 阿里百炼（同 OpenAI 兼容协议）
//   - gochat.DeepSeekClient     → DeepSeek
//   - gochat.AnthropicClient    → Anthropic Messages API
func NewDefaultLLMClient(cfg Config) (core.Client, error) {
	config := core.Config{
		BaseURL:     cfg.BaseURL,
		APIKey:      cfg.APIKey,
		Model:       cfg.Model,
		Timeout:     cfg.Timeout,
		Temperature: cfg.Temperature,
	}
	switch cfg.ClientType {
	case gochat.OllamaClient:
		return ollama.NewOllamaClient(config)
	case gochat.OpenAIClient, gochat.QwenClient:
		return openai.NewOpenAI(config)
	case gochat.DeepSeekClient:
		return deepseek.NewDeepSeek(config)
	case gochat.AnthropicClient:
		return anthropic.NewAnthropic(config)
	default:
		return nil, fmt.Errorf("goagent: 不支持的客户端类型 %v", cfg.ClientType)
	}
}
