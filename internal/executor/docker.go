package executor

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Docker 容器执行器。
// 模式①（容器内构建）：docker run 挂载工作区 + 缓存 named volume。
// 模式②（产物打镜像）：docker build。
type Docker struct {
	// bin docker 命令行路径，默认 "docker"
	bin string
}

func NewDocker() *Docker { return &Docker{bin: "docker"} }

func (d *Docker) Name() string { return "docker" }

// CheckAvailable 构建前置检查：探测 docker 引擎是否可用（见文档 §14.1 风险预案）。
func (d *Docker) CheckAvailable() error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, d.bin, "info", "--format", "{{.ServerVersion}}").
		CombinedOutput()
	if err != nil {
		return fmt.Errorf("Docker 引擎不可用（docker info 失败）: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// 容器内缓存路径映射（named volume → 容器内标准路径），见文档 §6.2/§7.2。
var containerCacheVolumes = map[string]string{
	"m2":       "/root/.m2",
	"gradle":   "/root/.gradle",
	"npm":      "/root/.npm",
	"go-mod":   "/go/pkg/mod",
	"go-build": "/root/.cache/go-build",
	"bun":      "/root/.bun/install/cache",
}

// ContainerCacheVolumes 返回缓存目录名 → 容器内路径的映射（供引擎挂载与注入环境变量）。
func ContainerCacheVolumes() map[string]string {
	m := make(map[string]string, len(containerCacheVolumes))
	for k, v := range containerCacheVolumes {
		m[k] = v
	}
	return m
}

// VolumeName 缓存 named volume 名。
func VolumeName(cacheDir string) string {
	return "cicd-" + strings.ReplaceAll(cacheDir, "/", "-")
}

// RunOptions 描述一次容器内构建。
type RunOptions struct {
	Image        string            // 基础镜像（必填）
	Workspace    string            // 宿主工作区绝对路径，挂载到 /workspace
	Env          []string          // 以 K=V 形式透传的环境变量
	CacheVolumes map[string]string // 缓存目录名 → 容器内路径（空表示不挂载缓存）
	ExtraBinds   []string          // 额外挂载（如 settings.xml）
	MemoryLimitMB int              // 容器内存上限 MB（0 = 不限，对应 --memory）
	CPULimit      int              // 容器 CPU 核数上限（0 = 不限，对应 --cpus）
}

// Run 用 docker run 在容器内执行脚本。
func (d *Docker) Run(ctx context.Context, opts RunOptions, script string, w io.Writer) error {
	if opts.Image == "" {
		return fmt.Errorf("docker 模式未配置基础镜像")
	}
	args := []string{"run", "--rm",
		"-v", opts.Workspace + ":/workspace",
		"-w", "/workspace",
	}
	for _, kv := range opts.Env {
		if k, _, ok := strings.Cut(kv, "="); ok && k != "PATH" && k != "JAVA_HOME" {
			args = append(args, "-e", kv)
		}
	}
	for dir, containerPath := range opts.CacheVolumes {
		args = append(args, "-v", VolumeName(dir)+":"+containerPath)
	}
	for _, b := range opts.ExtraBinds {
		args = append(args, "-v", b)
	}
	if opts.MemoryLimitMB > 0 {
		args = append(args, "--memory", fmt.Sprintf("%dm", opts.MemoryLimitMB))
	}
	if opts.CPULimit > 0 {
		args = append(args, "--cpus", strconv.Itoa(opts.CPULimit))
	}
	args = append(args, opts.Image, "sh", "-c", script)
	return runCommand(ctx, "", nil, w, d.bin, args...)
}

// BuildImage 模式②：按 Dockerfile 构建镜像（contextDir 为构建上下文目录）。
func (d *Docker) BuildImage(ctx context.Context, contextDir, dockerfile, tag string, w io.Writer) error {
	args := []string{"build", "-t", tag}
	if dockerfile != "" {
		args = append(args, "-f", dockerfile)
	}
	args = append(args, contextDir)
	return runCommand(ctx, contextDir, nil, w, d.bin, args...)
}
