package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// Bash 在 shell 中执行命令
type Bash struct{}

// Name 工具名称
func (Bash) Name() string { return "bash" }

// Description 工具用途说明
func (Bash) Description() string { return "在 shell 中执行命令，返回标准输出与标准错误" }

// Parameters 参数定义（JSON Schema）
func (Bash) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"command": {"type": "string", "description": "要执行的 shell 命令"}
		},
		"required": ["command"]
	}`)
}

// Execute 执行命令
func (Bash) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("解析参数失败: %w", err)
	}
	if strings.TrimSpace(p.Command) == "" {
		return "", fmt.Errorf("缺少 command 参数")
	}
	cmd := exec.CommandContext(ctx, "bash", "-c", p.Command)
	out, err := cmd.CombinedOutput()
	return string(out), err
}
