//go:build windows

package tools

import "os/exec"

// applyProcessGroup 在 Windows 上无进程组语义，保持系统默认终止行为
// （CommandContext 超时杀直接子进程）。
func applyProcessGroup(cmd *exec.Cmd) {}
