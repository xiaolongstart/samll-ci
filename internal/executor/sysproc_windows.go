//go:build windows

package executor

import (
	"context"
	"os/exec"
)

func newCmd(ctx context.Context, name string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, name, args...)
}

// killProcessGroup Windows 无进程组语义，退化为杀主进程。
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
}
