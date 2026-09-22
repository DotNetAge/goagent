package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// 安全默认参数
const (
	// DefaultBashTimeout 单条命令的默认超时。Context 已带 deadline 时取更早者。
	DefaultBashTimeout = 2 * time.Minute
	// bashOutputLimit 命令输出上限（字节），防止超量输出撑爆内存。
	bashOutputLimit = 256 * 1024
)

// Bash 在 shell 中执行命令。
//
// 安全边界：本工具不做命令白名单校验——需要授权门控时，
// 叠加自定义 ToolExecutor 或让命令返回 ErrNeedExternalInput（见包 goagent 的挂起机制）。
type Bash struct {
	// WorkDir 命令工作目录；空表示继承当前进程。
	WorkDir string
	// Timeout 单条命令超时；零值取 DefaultBashTimeout。
	// 与 Context deadline 并存时生效更早者。
	Timeout time.Duration
}

// Name 工具名称
func (Bash) Name() string { return "bash" }

// Description 工具用途说明
func (Bash) Description() string {
	return "在 shell 中执行命令，返回标准输出与标准错误"
}

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

// Execute 执行命令。
// 带默认超时与输出上限：超时整树终止（unix 下按进程组击杀），
// 超限输出截断并标注，避免不可信命令造成资源耗尽。
func (b Bash) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("解析参数失败: %w", err)
	}
	if strings.TrimSpace(p.Command) == "" {
		return "", fmt.Errorf("缺少 command 参数")
	}

	timeout := b.Timeout
	if timeout <= 0 {
		timeout = DefaultBashTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, "bash", "-c", p.Command)
	applyProcessGroup(cmd) // unix：独立进程组，超时按组整树击杀
	if b.WorkDir != "" {
		cmd.Dir = b.WorkDir
	}

	out, err := cmd.CombinedOutput()
	// 超时/取消优先反馈超时语义，附带已产生的部分输出帮助模型判断现场
	if runCtx.Err() != nil {
		return truncateBashOutput(out), fmt.Errorf("命令超时（上限 %v）或被取消: %w", timeout, runCtx.Err())
	}
	return truncateBashOutput(out), err
}

// truncateBashOutput 截断超限输出并标注。
// 截断点可能落在多字节字符中间（外部命令输出不做字符边界对齐，可接受）。
func truncateBashOutput(out []byte) string {
	if len(out) <= bashOutputLimit {
		return string(out)
	}
	return fmt.Sprintf("%s\n[输出超过 %d 字节上限，已截断，丢弃 %d 字节]",
		out[:bashOutputLimit], bashOutputLimit, len(out)-bashOutputLimit)
}
