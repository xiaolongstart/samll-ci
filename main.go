// samll-ci —— 轻量单二进制 CI/CD 流水线程序（1 核 512M 友好）。
//
// 用法: ./cicd -data ./data -port 8080 -host 127.0.0.1
package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"samll-ci/internal/auth"
	"samll-ci/internal/cachemgr"
	"samll-ci/internal/engine"
	"samll-ci/internal/server"
	"samll-ci/internal/store"
	"samll-ci/internal/toolchain"
)

//go:embed web
var webFS embed.FS

func main() {
	dataDir := flag.String("data", "./data", "数据目录（db/缓存/日志/工具链）")
	portFlag := flag.Int("port", 0, "监听端口（0 = 使用设置 server.port，默认 8080）")
	host := flag.String("host", "127.0.0.1", "绑定地址")
	flag.Parse()

	log.SetFlags(log.Ldate | log.Ltime)
	fmt.Println(banner)

	// 1. 初始化数据目录结构（见设计文档 §2.3）
	for _, d := range []string{"", "cache", "toolchains", "workspaces", "logs", "outputs"} {
		if err := os.MkdirAll(filepath.Join(*dataDir, d), 0o755); err != nil {
			log.Fatalf("创建数据目录失败: %v", err)
		}
	}

	// 2. 打开存储
	st, err := store.Open(filepath.Join(*dataDir, "app.db"))
	if err != nil {
		log.Fatalf("初始化存储失败: %v", err)
	}
	defer st.Close()

	// 3. 种子设置（无配置文件即可启动）
	seedSettings(st, *dataDir)

	// 4. 鉴权（首次启动生成默认密码 admin）
	authMgr := auth.New(st)
	created, err := authMgr.EnsureDefaultPassword()
	if err != nil {
		log.Fatalf("初始化鉴权失败: %v", err)
	}

	// 5. 缓存与工具链管理
	cacheMgr := cachemgr.New(filepath.Join(*dataDir, "cache"))
	if err := cacheMgr.EnsureDirs(); err != nil {
		log.Fatalf("初始化缓存目录失败: %v", err)
	}
	tcMgr := toolchain.New(st, *dataDir)

	// 6. 任务引擎（单 worker 串行）
	eng := engine.New(st, *dataDir, cacheMgr, tcMgr)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	eng.Start(ctx)

	// 7. HTTP 服务
	srv := server.New(st, authMgr, eng, cacheMgr, tcMgr, *dataDir, webFS)

	port := resolvePort(st, *portFlag)
	addr := fmt.Sprintf("%s:%d", *host, port)
	log.Printf("samll-ci 已启动: http://%s （数据目录: %s）", addr, *dataDir)
	if created {
		log.Printf("提示: 默认密码 admin，请尽快登录控制台修改")
	}
	if err := http.ListenAndServe(addr, srv.Handler()); err != nil {
		log.Fatalf("HTTP 服务退出: %v", err)
	}
}

func seedSettings(st *store.Store, dataDir string) {
	defaults := map[string]string{
		"auth.session_ttl": "86400",
		"server.port":      "8080",
		"docker.enabled":   "true",
		"cache.root":       filepath.Join(dataDir, "cache"),
		"default.timeout":  "3600",
	}
	for k, v := range defaults {
		cur, err := st.GetSetting(k)
		if err == nil && cur == "" {
			_ = st.SetSetting(k, v)
		}
	}
}

// resolvePort 端口优先级：命令行 -port > 设置 server.port > 8080。
func resolvePort(st *store.Store, flagPort int) int {
	if flagPort > 0 {
		return flagPort
	}
	if v, err := st.GetSetting("server.port"); err == nil && v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 8080
}

const banner = `
┌─────────────────────────────────────────────┐
│  samll-ci · 轻量单二进制 CI/CD 流水线        │
│  拉代码 → 构建 → 打包 → 部署                 │
└─────────────────────────────────────────────┘`
