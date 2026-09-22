//go:build unix

package tools

import (
	"os/exec"
	"syscall"
	"time"
)

// applyProcessGroup 让命令运行在独立进程组，并在超时/取消时整树击杀。
// exec.CommandContext 的默认行为只杀直接子进程（bash），不杀孙子进程
// （如命令里后台启动的进程）；按负 PID 发信号给整个进程组解决孤儿驻留。
func applyProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			// 负 PID 表示向进程组发信号
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	// 终止后管道仍可能被孤儿进程占用导致 Wait 阻塞，给兜底等待窗口
	cmd.WaitDelay = 5 * time.Second
}
