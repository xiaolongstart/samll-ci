package executor

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestJoinLines(t *testing.T) {
	if got := JoinLines(""); got != "" {
		t.Fatalf("空命令应返回空脚本, got %q", got)
	}
	got := JoinLines("echo a\necho b")
	if !strings.HasPrefix(got, "set -ex\n") || !strings.HasSuffix(got, "echo b\n") {
		t.Fatalf("JoinLines 结果不符: %q", got)
	}
}

func TestShellRunSuccess(t *testing.T) {
	var buf bytes.Buffer
	s := NewShell()
	script := JoinLines("echo hello\necho world")
	err := s.Run(context.Background(), "", script, nil, &buf)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "hello") || !strings.Contains(out, "world") {
		t.Fatalf("输出缺失: %q", out)
	}
	// set -x 回显
	if !strings.Contains(out, "+ echo hello") {
		t.Fatalf("set -x 回显缺失: %q", out)
	}
}

func TestShellRunFailFast(t *testing.T) {
	var buf bytes.Buffer
	s := NewShell()
	err := s.Run(context.Background(), "", "exit 3\necho should-not-run", nil, &buf)
	if err == nil {
		t.Fatal("非零退出应报错")
	}
	if strings.Contains(buf.String(), "should-not-run") {
		t.Fatal("set -e 未生效，后续命令不应执行")
	}
}

func TestShellRunEnvAndDir(t *testing.T) {
	var buf bytes.Buffer
	dir := t.TempDir()
	s := NewShell()
	env := []string{"MYVAR=fromenv", "PATH=/usr/bin:/bin"}
	err := s.Run(context.Background(), dir, "pwd > wd.txt; echo $MYVAR", env, &buf)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	wd, err := readFile(dir + "/wd.txt")
	if err != nil {
		t.Fatalf("工作目录文件未生成: %v", err)
	}
	// macOS 上 /var 是 /private/var 的符号链接，先归一化再比较
	dirReal, _ := filepath.EvalSymlinks(dir)
	wdReal, _ := filepath.EvalSymlinks(strings.TrimSpace(wd))
	if wdReal != dirReal {
		t.Fatalf("工作目录不符: %q != %q", wdReal, dirReal)
	}
	if !strings.Contains(buf.String(), "fromenv") {
		t.Fatalf("环境变量未注入: %q", buf.String())
	}
}

func TestShellRunTimeoutKill(t *testing.T) {
	var buf bytes.Buffer
	s := NewShell()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	// 子 shell 里再起一个长驻子进程，验证进程组级联 kill
	err := s.Run(ctx, "", "sleep 30 & sleep 30", nil, &buf)
	if err == nil {
		t.Fatal("超时应返回错误")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("超时后未及时终止: %v", elapsed)
	}
}

func readFile(p string) (string, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
