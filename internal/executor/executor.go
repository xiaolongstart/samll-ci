// Package executor 定义可插拔的命令执行器接口，提供 Shell 与 Docker 两种实现。
package executor

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// Executor 实际跑命令的后端。
type Executor interface {
	// Run 在给定工作目录执行脚本（多条命令，换行分隔，set -e 语义），
	// stdout/stderr 写入 w。
	Run(ctx context.Context, workDir string, script string, env []string, w io.Writer) error
	// Name 返回执行器标识。
	Name() string
}

// Command 返回带进程组设置的子进程命令（供引擎运行 git 等外部命令并捕获输出）。
func Command(ctx context.Context, name string, args ...string) *exec.Cmd {
	return newCmd(ctx, name, args...)
}

// JoinLines 将多条命令合并为 fail-fast 脚本：任一行非零退出即终止。
// set -x 使每条命令回显到 stderr，方便日志定位。
func JoinLines(cmds string) string {
	cmds = strings.TrimRight(cmds, "\n")
	if cmds == "" {
		return ""
	}
	return "set -ex\n" + cmds + "\n"
}

// ---------- Shell ----------

// Shell 本机执行器：宿主直接运行。
type Shell struct{}

func NewShell() *Shell { return &Shell{} }

func (s *Shell) Name() string { return "shell" }

func (s *Shell) Run(ctx context.Context, workDir string, script string, env []string, w io.Writer) error {
	return runCommand(ctx, workDir, env, w, "sh", "-c", script)
}

// runCommand 统一的子进程启动逻辑：进程组 kill、上下文取消、退出码包装。
func runCommand(ctx context.Context, workDir string, env []string, w io.Writer, name string, args ...string) error {
	cmd := newCmd(ctx, name, args...)
	cmd.Dir = workDir
	if env != nil {
		cmd.Env = env
	}
	if workDir == "" {
		cmd.Dir = "."
	}
	cmd.Stdout = w
	cmd.Stderr = w

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动命令失败: %w", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case <-ctx.Done():
		killProcessGroup(cmd) // 超时/取消：杀掉整个进程组，避免遗留子进程
		<-done                // 等待 Wait 返回，避免僵尸进程
		return ctx.Err()
	case err := <-done:
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("命令退出码非零: %w", err)
		}
		return nil
	}
}
