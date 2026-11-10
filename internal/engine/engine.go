// Package engine 实现构建任务引擎：FIFO 队列、单 worker 串行调度、
// 五阶段流水线（拉代码→准备环境→构建→打包→部署）、超时与取消、日志落盘。
package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"samll-ci/internal/cachemgr"
	"samll-ci/internal/executor"
	"samll-ci/internal/store"
	"samll-ci/internal/toolchain"
)

const queueSize = 1024

// Engine 任务引擎。任意时刻只跑 1 个构建。
type Engine struct {
	store   *store.Store
	dataDir string
	cache   *cachemgr.Manager
	tcMgr   *toolchain.Manager
	shell   *executor.Shell
	docker  *executor.Docker

	mu       sync.Mutex
	cancels  map[int64]context.CancelFunc
	running  int64 // 当前 running 的 build id，0 表示空闲
	queue    chan int64
}

func New(st *store.Store, dataDir string, cache *cachemgr.Manager, tcMgr *toolchain.Manager) *Engine {
	return &Engine{
		store:   st,
		dataDir: dataDir,
		cache:   cache,
		tcMgr:   tcMgr,
		shell:   executor.NewShell(),
		docker:  executor.NewDocker(),
		cancels: map[int64]context.CancelFunc{},
		queue:   make(chan int64, queueSize),
	}
}

// Start 启动 worker（含进程重启恢复：running→failed，pending 重新入队）。
func (e *Engine) Start(ctx context.Context) {
	interrupted, pending, err := e.store.RecoverBuilds()
	if err != nil {
		log.Printf("[引擎] 重启恢复失败: %v", err)
	} else {
		for _, it := range interrupted {
			appendFileLine(it.LogPath, "\n===== 进程重启，构建中断，已标记 failed =====\n")
		}
		if len(interrupted) > 0 {
			log.Printf("[引擎] 重启恢复：%d 个 running 构建标记为 failed（进程重启中断）", len(interrupted))
		}
	}
	if len(pending) > 0 {
		log.Printf("[引擎] 重启恢复：%d 个 pending 构建重新入队", len(pending))
		for _, id := range pending {
			select {
			case e.queue <- id:
			default:
				log.Printf("[引擎] 队列已满，构建 %d 丢弃恢复入队", id)
			}
		}
	}
	go e.worker(ctx)
}

// appendFileLine 向文件追加一行（用于补记中断备注）。
func appendFileLine(path, s string) {
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(s)
}

func (e *Engine) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-e.queue:
			e.run(id)
		}
	}
}

// Trigger 触发构建：创建 pending 记录并入队。
func (e *Engine) Trigger(projectID int64) (int64, error) {
	if _, err := e.store.GetProject(projectID); err != nil {
		return 0, err
	}
	id, err := e.store.CreateBuild(projectID, "")
	if err != nil {
		return 0, err
	}
	logPath := filepath.Join(e.dataDir, "logs", fmt.Sprintf("%d.log", id))
	if err := e.store.SetBuildLogPath(id, logPath); err != nil {
		return id, err
	}
	select {
	case e.queue <- id:
		return id, nil
	default:
		_ = e.store.SetBuildStatus(id, "failed", "")
		return 0, fmt.Errorf("构建队列已满，请稍后重试")
	}
}

// Cancel 取消进行中的构建。
func (e *Engine) Cancel(buildID int64) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	rec, err := e.store.GetBuild(buildID)
	if err != nil {
		return err
	}
	switch rec.Status {
	case "pending":
		return fmt.Errorf("构建仍在排队中，暂不支持取消排队任务")
	case "running":
		if cancel, ok := e.cancels[buildID]; ok {
			cancel()
			return nil
		}
		return fmt.Errorf("构建 %d 未找到可取消的执行上下文", buildID)
	default:
		return fmt.Errorf("构建 %d 已结束（%s），无需取消", buildID, rec.Status)
	}
}

// RunningID 当前正在执行的构建 id（0 = 空闲）。
func (e *Engine) RunningID() int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.running
}

// ---------------------------------------------------------------- 流水线

type stage struct {
	no   int
	name string
	fn   func(ctx context.Context, lw *buildLogger) error
}

func (e *Engine) run(id int64) {
	st := e.store
	rec, err := st.GetBuild(id)
	if err != nil {
		log.Printf("[引擎] 构建 %d 不存在: %v", id, err)
		return
	}
	proj, err := st.GetProject(rec.ProjectID)
	if err != nil {
		_ = st.SetBuildStatus(id, "failed", "")
		return
	}

	e.mu.Lock()
	e.running = id
	e.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	e.mu.Lock()
	e.cancels[id] = cancel
	e.mu.Unlock()
	defer func() {
		cancel()
		e.mu.Lock()
		delete(e.cancels, id)
		e.running = 0
		e.mu.Unlock()
	}()

	_ = st.SetBuildStatus(id, "running", "")
	lw := newBuildLogger(rec.LogPath)
	defer lw.Close()
	start := time.Now()
	lw.printf("===== 构建 #%d 项目[%s] 分支[%s] 模式[%s] 开始 %s =====\n",
		id, proj.Name, proj.Branch, proj.BuildMode, start.Format("2006-01-02 15:04:05"))

	workDir := filepath.Join(e.dataDir, "workspaces", fmt.Sprintf("%d", id))
	status := "success"
	for _, s := range e.stages(proj, id, workDir) {
		sctx, scancel := context.WithTimeout(ctx, time.Duration(proj.Timeout)*time.Second)
		lw.printf("\n==> [%d/5] %s\n", s.no, s.name)
		if err := s.fn(sctx, lw); err != nil {
			scancel()
			if errors.Is(err, context.Canceled) && ctx.Err() != nil {
				status = "cancelled"
				lw.printf("阶段 [%s] 被取消\n", s.name)
			} else {
				status = "failed"
				lw.printf("阶段 [%s] 失败: %v\n", s.name, err)
			}
			break
		}
		scancel()
	}

	_ = st.SetBuildStatus(id, status, lw.commit)
	lw.printf("\n===== 构建结束: %s 耗时 %s =====\n", status, time.Since(start).Round(time.Second))
	// 清理工作区（保留日志与产物）
	if status == "success" {
		_ = os.RemoveAll(workDir)
	}
}

// stages 返回五阶段定义（见文档 §5.2）。
func (e *Engine) stages(proj *store.Project, buildID int64, workDir string) []stage {
	return []stage{
		{1, "拉代码", func(ctx context.Context, lw *buildLogger) error {
			return e.stageClone(ctx, proj, workDir, lw)
		}},
		{2, "准备环境", func(ctx context.Context, lw *buildLogger) error {
			return e.stagePrepare(ctx, proj, lw)
		}},
		{3, "构建", func(ctx context.Context, lw *buildLogger) error {
			return e.stageBuild(ctx, proj, workDir, lw)
		}},
		{4, "打包", func(ctx context.Context, lw *buildLogger) error {
			return e.stagePackage(proj, workDir, lw)
		}},
		{5, "部署", func(ctx context.Context, lw *buildLogger) error {
			return e.stageDeploy(ctx, proj, workDir, lw)
		}},
	}
}

// stageClone 阶段①：git clone --depth 1 --single-branch -b <branch> <url>（私有库注入 token）。
func (e *Engine) stageClone(ctx context.Context, proj *store.Project, workDir string, lw *buildLogger) error {
	if err := os.MkdirAll(filepath.Dir(workDir), 0o755); err != nil {
		return err
	}
	url := rewriteGitURL(proj.GitURL, proj.Token)
	args := []string{"clone", "--depth", "1", "--single-branch", "-b", proj.Branch, url, workDir}
	lw.printf("$ git clone --depth 1 --single-branch -b %s %s\n", proj.Branch, redactGitURL(url))

	tw := &teeWriter{w: lw}
	cmd := executor.Command(ctx, "git", args...)
	cmd.Stdout = tw
	cmd.Stderr = tw
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git clone 失败: %v", err)
	}
	// 记录 commit id
	out, err := exec.CommandContext(ctx, "git", "-C", workDir, "rev-parse", "HEAD").Output()
	if err != nil {
		lw.printf("警告：获取 commit id 失败: %v\n", err)
		return nil
	}
	lw.setCommit(strings.TrimSpace(string(out)))
	lw.printf("commit: %s\n", lw.commit)
	return nil
}

// stagePrepare 阶段②：确认工具链 ready（缺则下载安装）、缓存目录挂载。
func (e *Engine) stagePrepare(ctx context.Context, proj *store.Project, lw *buildLogger) error {
	if proj.BuildMode == "docker" {
		lw.printf("Docker 模式：工具链由镜像提供，检查 Docker 引擎...\n")
		if err := e.docker.CheckAvailable(); err != nil {
			return err
		}
		lw.printf("Docker 引擎可用。\n")
		return nil
	}
	if err := e.cache.EnsureDirs(); err != nil {
		return err
	}
	if proj.ToolchainID <= 0 {
		lw.printf("未选择工具链，将使用宿主 PATH 中的工具。\n")
		return nil
	}
	tc, err := e.store.GetToolchain(proj.ToolchainID)
	if err != nil {
		return fmt.Errorf("获取工具链失败: %w", err)
	}
	return e.tcMgr.EnsureInstalled(ctx, tc, lw)
}

// stageBuild 阶段③：按模式执行构建命令。
func (e *Engine) stageBuild(ctx context.Context, proj *store.Project, workDir string, lw *buildLogger) error {
	cmds := strings.TrimSpace(proj.BuildCmds)
	if cmds == "" {
		lw.printf("未配置构建命令，跳过。\n")
		return nil
	}
	script := executor.JoinLines(cmds)
	switch proj.BuildMode {
	case "docker":
		return e.runDocker(ctx, proj, workDir, script, lw)
	default:
		env := e.buildShellEnv(proj, lw)
		return e.shell.Run(ctx, workDir, e.wrapCPU(script), env, lw)
	}
}

// stagePackage 阶段④：把产物复制到 outputs/<project_id>/（直接覆盖，不做版本号）。
func (e *Engine) stagePackage(proj *store.Project, workDir string, lw *buildLogger) error {
	if strings.TrimSpace(proj.OutputDir) == "" {
		lw.printf("未配置产物输出目录，跳过打包。\n")
		return nil
	}
	src := filepath.Join(workDir, filepath.Clean("/"+strings.TrimSpace(proj.OutputDir)))
	// 防止越出工作区
	if rel, err := filepath.Rel(workDir, src); err != nil || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("产物目录 %q 越出工作区", proj.OutputDir)
	}
	info, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("产物目录不存在: %s", proj.OutputDir)
	}
	dst := filepath.Join(e.dataDir, "outputs", fmt.Sprintf("%d", proj.ID))
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	var n int
	if info.IsDir() {
		n, err = copyTree(src, dst)
	} else {
		// 单文件产物：直接复制并保留文件名
		n = 1
		err = copyFile(src, filepath.Join(dst, filepath.Base(src)))
	}
	if err != nil {
		return fmt.Errorf("复制产物失败: %w", err)
	}
	lw.printf("产物已输出到 %s（%d 个文件）\n", dst, n)
	return nil
}

// stageDeploy 阶段⑤：执行部署命令（宿主 Shell）。
func (e *Engine) stageDeploy(ctx context.Context, proj *store.Project, workDir string, lw *buildLogger) error {
	cmds := strings.TrimSpace(proj.DeployCmds)
	if cmds == "" {
		lw.printf("未配置部署命令，跳过。\n")
		return nil
	}
	env := e.buildShellEnv(proj, lw)
	return e.shell.Run(ctx, workDir, e.wrapCPU(executor.JoinLines(cmds)), env, lw)
}

// runDocker 在容器内构建（模式①）；工具链 settings.xml 以只读挂载注入。
func (e *Engine) runDocker(ctx context.Context, proj *store.Project, workDir string, script string, lw *buildLogger) error {
	image := strings.TrimSpace(proj.DockerImage)
	if image == "" {
		image = e.defaultDockerImage(proj)
		lw.printf("未配置镜像，使用默认：%s\n", image)
	}
	opts := executor.RunOptions{
		Image:     image,
		Workspace: workDir,
	}
	if proj.CacheEnabled {
		opts.CacheVolumes = map[string]string{}
		for dir, cpath := range executor.ContainerCacheVolumes() {
			opts.CacheVolumes[dir] = cpath
			opts.Env = append(opts.Env, containerCacheEnv(dir, cpath)...)
		}
	}
	// 内存护栏（与 Shell 模式一致；项目级覆盖优先）
	if mem := e.memLimit(proj); mem > 0 {
		opts.MemoryLimitMB = mem
		if !optsEnvKey(opts, "NODE_OPTIONS") {
			opts.Env = append(opts.Env, fmt.Sprintf("NODE_OPTIONS=--max-old-space-size=%d", mem))
		}
		if !optsEnvKey(opts, "JAVA_TOOL_OPTIONS") {
			opts.Env = append(opts.Env, fmt.Sprintf("JAVA_TOOL_OPTIONS=-Xmx%dm", mem))
		}
		if !optsEnvKey(opts, "GOMEMLIMIT") {
			opts.Env = append(opts.Env, fmt.Sprintf("GOMEMLIMIT=%dMiB", mem))
		}
	}
	// CPU 护栏（设置页「最大 CPU 核数」，0 = 不限）
	if n := e.settingInt("build.max_cpu_cores", 0); n > 0 {
		opts.CPULimit = n
	}
	// Gradle daemon（与 Shell 模式一致）
	if !optsEnvKey(opts, "GRADLE_OPTS") && e.settingOrDefault("gradle.no_daemon", "true") != "false" {
		opts.Env = append(opts.Env, "GRADLE_OPTS=-Dorg.gradle.daemon=false")
	}
	if proj.ToolchainID > 0 {
		if tc, err := e.store.GetToolchain(proj.ToolchainID); err == nil {
			if p, err := e.tcMgr.SettingsFile(tc); err == nil && p != "" {
				opts.ExtraBinds = append(opts.ExtraBinds, p+":/cicd-settings.xml:ro")
				opts.Env = append(opts.Env, `MAVEN_ARGS=-s /cicd-settings.xml`)
			}
		}
	}
	return e.docker.Run(ctx, opts, script, lw)
}

// memLimit 构建内存护栏（MB）：项目级覆盖优先，0 = 用全局设置。
func (e *Engine) memLimit(proj *store.Project) int {
	if proj.MemLimitMB > 0 {
		return proj.MemLimitMB
	}
	return e.settingInt("build.mem_limit_mib", 384)
}

// buildShellEnv 组装 Shell 模式环境变量（缓存路径 + 工具链注入 + 国内镜像代理）。
func (e *Engine) buildShellEnv(proj *store.Project, lw *buildLogger) []string {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	// 缓存目录（持久化复用）
	env["GOMODCACHE"] = e.cache.Dir(cachemgr.DirGoMod)
	env["GOCACHE"] = e.cache.Dir(cachemgr.DirGoBuild)
	env["npm_config_cache"] = e.cache.Dir(cachemgr.DirNpm)
	env["BUN_INSTALL_CACHE_DIR"] = e.cache.Dir(cachemgr.DirBun)
	env["GRADLE_USER_HOME"] = e.cache.Dir(cachemgr.DirGradle)
	env["MAVEN_REPO_LOCAL"] = e.cache.Dir(cachemgr.DirMaven)
	// 国内镜像代理（开箱即用；宿主环境已显式配置时尊重现有值）
	if _, ok := env["GOPROXY"]; !ok {
		env["GOPROXY"] = e.settingOrDefault("mirror.goproxy", "https://goproxy.cn,direct")
	}
	if _, ok := env["npm_config_registry"]; !ok {
		env["npm_config_registry"] = e.settingOrDefault("mirror.npm_registry", "https://registry.npmmirror.com")
	}
	// 内存护栏（设置页「最大内存使用 MB」或项目级覆盖，0 = 不限）：
	// 按技术栈映射为对应护栏；宿主已显式配置时尊重现有值
	if mem := e.memLimit(proj); mem > 0 {
		nodeOpts := []string{}
		if _, ok := env["NODE_OPTIONS"]; !ok {
			nodeOpts = append(nodeOpts, fmt.Sprintf("--max-old-space-size=%d", mem))
			// 512M 兼容：小新生代降低 RSS 峰值（GC 更频繁，时间换内存）
			if semi := e.settingInt("node.semi_space_mib", 0); semi > 0 {
				nodeOpts = append(nodeOpts, fmt.Sprintf("--max-semi-space-size=%d", semi))
			}
			env["NODE_OPTIONS"] = strings.Join(nodeOpts, " ")
		}
		if _, ok := env["JAVA_TOOL_OPTIONS"]; !ok {
			env["JAVA_TOOL_OPTIONS"] = fmt.Sprintf("-Xmx%dm", mem)
		}
		if _, ok := env["GOMEMLIMIT"]; !ok {
			env["GOMEMLIMIT"] = fmt.Sprintf("%dMiB", mem)
		}
	}
	// Gradle 默认禁用 daemon：小内存机器上驻留的 daemon（双 JVM）会持续吃内存
	if _, ok := env["GRADLE_OPTS"]; !ok && e.settingOrDefault("gradle.no_daemon", "true") != "false" {
		env["GRADLE_OPTS"] = "-Dorg.gradle.daemon=false"
	}

	var binPrepend []string
	if proj.ToolchainID > 0 {
		if tc, err := e.store.GetToolchain(proj.ToolchainID); err == nil && tc.Status == "ready" {
			ip := tc.InstallPath
			switch tc.Type {
			case "jdk":
				env["JAVA_HOME"] = ip
				binPrepend = append(binPrepend, filepath.Join(ip, "bin"))
			case "maven":
				env["MAVEN_HOME"] = ip
				binPrepend = append(binPrepend, filepath.Join(ip, "bin"))
				// Maven 依赖 JDK：自动关联第一条 ready 的 JDK 工具链注入 JAVA_HOME
				if jdk := e.firstReadyToolchain("jdk"); jdk != nil {
					env["JAVA_HOME"] = jdk.InstallPath
					binPrepend = append(binPrepend, filepath.Join(jdk.InstallPath, "bin"))
				} else {
					lw.printf("警告：未找到 ready 状态的 JDK 工具链，mvn 将使用 PATH 中的 java\n")
				}
				// 本地仓库始终指向缓存目录（磁盘换时间）；有自定义 settings.xml 时一并注入
				mavenArgs := "-Dmaven.repo.local=" + e.cache.Dir(cachemgr.DirMaven)
				if p, err := e.tcMgr.SettingsFile(tc); err == nil && p != "" {
					mavenArgs = "-s " + p + " " + mavenArgs
					lw.printf("Maven settings: %s\n", p)
				}
				env["MAVEN_ARGS"] = mavenArgs
			case "node":
				binPrepend = append(binPrepend, filepath.Join(ip, "bin"))
			case "gradle":
				binPrepend = append(binPrepend, filepath.Join(ip, "bin"))
				// Gradle 编译依赖 JDK：自动关联第一条 ready 的 JDK 工具链
				if jdk := e.firstReadyToolchain("jdk"); jdk != nil {
					env["JAVA_HOME"] = jdk.InstallPath
					binPrepend = append(binPrepend, filepath.Join(jdk.InstallPath, "bin"))
				}
			case "go":
				env["GOROOT"] = ip
				binPrepend = append(binPrepend, filepath.Join(ip, "bin"))
			case "bun":
				binPrepend = append(binPrepend, ip)
			}
		}
	}
	if len(binPrepend) > 0 {
		env["PATH"] = strings.Join(binPrepend, ":") + ":" + env["PATH"]
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// firstReadyToolchain 返回第一条指定类型且 ready 的工具链（无则 nil）。
func (e *Engine) firstReadyToolchain(tcType string) *store.Toolchain {
	list, err := e.store.ListToolchains()
	if err != nil {
		return nil
	}
	for _, t := range list {
		if t.Type == tcType && t.Status == "ready" && t.InstallPath != "" {
			return t
		}
	}
	return nil
}

// settingOrDefault 读取全局设置，缺省用默认值。
func (e *Engine) settingOrDefault(key, def string) string {
	v, err := e.store.GetSetting(key)
	if err != nil || v == "" {
		return def
	}
	return v
}

// settingInt 读取整型全局设置（非法/负值回退默认）。
func (e *Engine) settingInt(key string, def int) int {
	v := e.settingOrDefault(key, "")
	if v == "" {
		return def
	}
	if n, err := strconv.Atoi(v); err == nil && n >= 0 {
		return n
	}
	return def
}

// optsEnvKey 检查 RunOptions.Env 中是否已包含某个变量。
func optsEnvKey(o executor.RunOptions, key string) bool {
	prefix := key + "="
	for _, kv := range o.Env {
		if strings.HasPrefix(kv, prefix) {
			return true
		}
	}
	return false
}

// defaultDockerImage 未配置镜像时按工具链类型给出推荐基础镜像（见文档 §7.2）。
func (e *Engine) defaultDockerImage(proj *store.Project) string {
	if proj.ToolchainID > 0 {
		if tc, err := e.store.GetToolchain(proj.ToolchainID); err == nil {
			switch tc.Type {
			case "jdk", "maven":
				return "maven:3.9-eclipse-temurin-8"
			case "gradle":
				return "gradle:8.14-jdk8"
			case "node":
				return "node:20-alpine"
			case "go":
				return "golang:1.22-alpine"
			case "bun":
				return "oven/bun:1"
			}
		}
	}
	return "alpine:3.19"
}

// wrapCPU 根据设置页「最大 CPU 核数」限制构建命令的 CPU 使用：
// Linux 且 taskset 可用时以绑核方式执行；其他平台或 0 = 不限。
func (e *Engine) wrapCPU(script string) string {
	n := e.settingInt("build.max_cpu_cores", 0)
	if n <= 0 || runtime.GOOS != "linux" {
		return script
	}
	if _, err := exec.LookPath("taskset"); err != nil {
		return script
	}
	return fmt.Sprintf("taskset -c 0-%d sh -c %s", n-1, shellQuoteSingle(script))
}

// shellQuoteSingle POSIX 单引号安全转义。
func shellQuoteSingle(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// containerCacheEnv 容器内缓存环境变量。
func containerCacheEnv(dir, containerPath string) []string {
	switch dir {
	case "m2":
		return []string{"MAVEN_REPO_LOCAL=" + containerPath}
	case "gradle":
		return []string{"GRADLE_USER_HOME=" + containerPath}
	case "npm":
		return []string{"npm_config_cache=" + containerPath}
	case "go-mod":
		return []string{"GOMODCACHE=" + containerPath}
	case "go-build":
		return []string{"GOCACHE=" + containerPath}
	case "bun":
		return []string{"BUN_INSTALL_CACHE_DIR=" + containerPath}
	}
	return nil
}

// ---------------------------------------------------------------- git 辅助

// rewriteGitURL 私有库 token 注入：https://<user>:<token>@host/...。
func rewriteGitURL(rawURL, token string) string {
	if token == "" {
		return rawURL
	}
	i := strings.Index(rawURL, "://")
	if i < 0 || strings.HasPrefix(rawURL, "git@") {
		return rawURL // ssh 形式不处理，交给宿主 ssh-key
	}
	scheme := rawURL[:i]
	rest := rawURL[i+3:]
	user := "git"
	if strings.Contains(rest, "github.com") {
		user = "x-access-token"
	} else if strings.Contains(rest, "gitlab") {
		user = "oauth2"
	}
	return scheme + "://" + user + ":" + token + "@" + rest
}

// redactGitURL 日志中隐藏 token。
func redactGitURL(rawURL string) string {
	if i := strings.Index(rawURL, "@"); i >= 0 {
		if j := strings.Index(rawURL, "://"); j >= 0 && i > j {
			return rawURL[:j+3] + "***" + rawURL[i:]
		}
	}
	return rawURL
}

// ---------------------------------------------------------------- 日志

// teeWriter 同时写日志并捕获 clone 输出。
type teeWriter struct {
	w   io.Writer
	buf bytes.Buffer
}

func (t *teeWriter) Write(p []byte) (int, error) {
	t.buf.Write(p)
	return t.w.Write(p)
}

// buildLogger 构建日志写入器：并发安全（exec 的 Stdout/Stderr 两个 goroutine 同时写），
// 并保存 commit id 供落库。
type buildLogger struct {
	mu     sync.Mutex
	f      *os.File
	commit string
}

func newBuildLogger(path string) *buildLogger {
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		log.Printf("[引擎] 打开日志文件失败 %s: %v", path, err)
	}
	return &buildLogger{f: f}
}

func (l *buildLogger) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		if _, err := l.f.Write(p); err != nil {
			return 0, err
		}
		_ = l.f.Sync()
	}
	return len(p), nil
}

func (l *buildLogger) printf(format string, a ...any) {
	_, _ = fmt.Fprintf(l, format, a...)
}

func (l *buildLogger) setCommit(c string) {
	l.mu.Lock()
	l.commit = c
	l.mu.Unlock()
}

// Close 关闭底层日志文件。
func (l *buildLogger) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		_ = l.f.Close()
		l.f = nil
	}
}

// ---------------------------------------------------------------- 文件复制

func copyTree(src, dst string) (int, error) {
	var count int
	err := filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if err := copyFile(p, target); err != nil {
			return err
		}
		count++
		return nil
	})
	return count, err
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
