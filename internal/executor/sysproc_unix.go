//go:build unix

package executor

import (
	"context"
	"os/exec"
	"syscall"
)

// newCmd 设置进程组，使超时/取消时可连带杀掉全部子进程（mvn/npm 等会派生进程）。
func newCmd(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
}

// killProcessGroup 杀掉命令的整个进程组（pgid = 子进程 pid）。
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
