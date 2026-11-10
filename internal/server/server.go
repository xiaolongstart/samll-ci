// Package server 提供 HTTP 服务：内嵌控制台静态页、REST API 与 SSE 日志流。
// 路由基于 Go 1.22+ net/http method+path 模式，响应约定 {code,message,data}。
package server

import (
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"samll-ci/internal/auth"
	"samll-ci/internal/cachemgr"
	"samll-ci/internal/engine"
	"samll-ci/internal/store"
	"samll-ci/internal/toolchain"
)

const sessionCookie = "cicd_session"

// Server HTTP 服务。
type Server struct {
	store   *store.Store
	auth    *auth.Manager
	eng     *engine.Engine
	cache   *cachemgr.Manager
	tcMgr   *toolchain.Manager
	dataDir string
	webFS   fs.FS
}

// New 构造 Server 并装配路由。
func New(st *store.Store, a *auth.Manager, eng *engine.Engine,
	cache *cachemgr.Manager, tcMgr *toolchain.Manager, dataDir string, web embed.FS) *Server {
	sub, err := fs.Sub(web, "web")
	if err != nil {
		panic("内嵌 web 资源缺失: " + err.Error())
	}
	s := &Server{
		store: st, auth: a, eng: eng, cache: cache, tcMgr: tcMgr,
		dataDir: dataDir, webFS: sub,
	}
	return s
}

// Handler 返回根处理器。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// 鉴权
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.authed(s.handleLogout))
	mux.HandleFunc("GET /api/me", s.authed(s.handleMe))

	// 项目
	mux.HandleFunc("GET /api/projects", s.authed(s.handleProjectList))
	mux.HandleFunc("POST /api/projects", s.authed(s.handleProjectCreate))
	mux.HandleFunc("GET /api/projects/{id}", s.authed(s.handleProjectGet))
	mux.HandleFunc("PUT /api/projects/{id}", s.authed(s.handleProjectUpdate))
	mux.HandleFunc("DELETE /api/projects/{id}", s.authed(s.handleProjectDelete))
	mux.HandleFunc("POST /api/projects/{id}/build", s.authed(s.handleProjectBuild))

	// 构建
	mux.HandleFunc("GET /api/builds", s.authed(s.handleBuildList))
	mux.HandleFunc("GET /api/builds/{id}", s.authed(s.handleBuildGet))
	mux.HandleFunc("GET /api/builds/{id}/log", s.authed(s.handleBuildLog)) // SSE
	mux.HandleFunc("POST /api/builds/{id}/cancel", s.authed(s.handleBuildCancel))

	// 工具链
	mux.HandleFunc("GET /api/toolchains", s.authed(s.handleTcList))
	mux.HandleFunc("GET /api/toolchain-versions", s.authed(s.handleTcVersions))
	mux.HandleFunc("POST /api/toolchains", s.authed(s.handleTcCreate))
	mux.HandleFunc("GET /api/toolchains/{id}", s.authed(s.handleTcGet))
	mux.HandleFunc("PUT /api/toolchains/{id}", s.authed(s.handleTcUpdate))
	mux.HandleFunc("DELETE /api/toolchains/{id}", s.authed(s.handleTcDelete))
	mux.HandleFunc("POST /api/toolchains/{id}/install", s.authed(s.handleTcInstall))

	// 缓存与设置
	mux.HandleFunc("GET /api/cache", s.authed(s.handleCacheUsage))
	mux.HandleFunc("POST /api/cache/clear", s.authed(s.handleCacheClear))
	mux.HandleFunc("GET /api/settings", s.authed(s.handleSettingsGet))
	mux.HandleFunc("PUT /api/settings", s.authed(s.handleSettingsPut))
	mux.HandleFunc("POST /api/password", s.authed(s.handlePassword))

	// 内嵌控制台静态页（SPA：未知路径回退 index.html，由前端处理未登录跳转）
	mux.HandleFunc("/", s.handleStatic)
	return s.logMW(mux)
}

// ---------------------------------------------------------------- 中间件与响应

type responseWriter struct {
	http.ResponseWriter
	status int
}

func (w *responseWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Flush 透传 Flusher，保证 SSE 日志流可用。
func (w *responseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) logMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rw := &responseWriter{ResponseWriter: w, status: 200}
		start := time.Now()
		next.ServeHTTP(rw, r)
		if strings.HasPrefix(r.URL.Path, "/api/") {
			fmt.Printf("%s %s -> %d (%dms)\n", r.Method, r.URL.Path, rw.status, time.Since(start).Milliseconds())
		}
	})
}

// authed 鉴权中间件：Authorization: Bearer <token> 或 session cookie。
func (s *Server) authed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		if !s.auth.Validate(token) {
			writeErr(w, http.StatusUnauthorized, 401, "未登录或会话已过期")
			return
		}
		next(w, r)
	}
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	if c, err := r.Cookie(sessionCookie); err == nil {
		return c.Value
	}
	return ""
}

type apiResp struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func writeOK(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(apiResp{Code: 0, Message: "ok", Data: data})
}

func writeErr(w http.ResponseWriter, httpStatus, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(httpStatus)
	_ = json.NewEncoder(w).Encode(apiResp{Code: code, Message: msg})
}

func readJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	return dec.Decode(v)
}

func pathID(r *http.Request) (int64, error) {
	return strconv.ParseInt(r.PathValue("id"), 10, 64)
}

// ---------------------------------------------------------------- 鉴权 API

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password string `json:"password"`
	}
	if err := readJSON(r, &body); err != nil || body.Password == "" {
		writeErr(w, http.StatusBadRequest, 400, "缺少 password")
		return
	}
	token, err := s.auth.Login(body.Password)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, 401, err.Error())
		return
	}
	// 同时种 cookie，供 EventSource(SSE) 使用
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/",
		HttpOnly: true, MaxAge: s.auth.TTLSeconds(),
	})
	writeOK(w, map[string]string{"token": token})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.auth.Logout(bearerToken(r))
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	writeOK(w, nil)
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	writeOK(w, map[string]any{"authenticated": true})
}

// ---------------------------------------------------------------- 项目 API

func (s *Server) handleProjectList(w http.ResponseWriter, r *http.Request) {
	projects, err := s.store.ListProjects()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	type row struct {
		*store.Project
		LastBuild *store.BuildRecord `json:"last_build"`
	}
	out := make([]row, 0, len(projects))
	for _, p := range projects {
		last, _ := s.store.LastBuildOfProject(p.ID)
		out = append(out, row{p, last})
	}
	writeOK(w, out)
}

func (s *Server) handleProjectCreate(w http.ResponseWriter, r *http.Request) {
	var p store.Project
	if err := readJSON(r, &p); err != nil {
		writeErr(w, http.StatusBadRequest, 400, "请求体不是合法 JSON")
		return
	}
	if p.Name == "" || p.GitURL == "" {
		writeErr(w, http.StatusBadRequest, 400, "项目名与 Git 地址必填")
		return
	}
	if p.BuildMode != "shell" && p.BuildMode != "docker" {
		writeErr(w, http.StatusBadRequest, 400, "build_mode 只能是 shell 或 docker")
		return
	}
	id, err := s.store.CreateProject(&p)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	created, _ := s.store.GetProject(id)
	writeOK(w, created)
}

func (s *Server) handleProjectGet(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, 400, "非法 id")
		return
	}
	p, err := s.store.GetProject(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, 404, err.Error())
		return
	}
	writeOK(w, p)
}

func (s *Server) handleProjectUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, 400, "非法 id")
		return
	}
	var p store.Project
	if err := readJSON(r, &p); err != nil {
		writeErr(w, http.StatusBadRequest, 400, "请求体不是合法 JSON")
		return
	}
	p.ID = id
	if err := s.store.UpdateProject(&p); err != nil {
		writeErr(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	updated, _ := s.store.GetProject(id)
	writeOK(w, updated)
}

func (s *Server) handleProjectDelete(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, 400, "非法 id")
		return
	}
	if err := s.store.DeleteProject(id); err != nil {
		writeErr(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	writeOK(w, nil)
}

func (s *Server) handleProjectBuild(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, 400, "非法 id")
		return
	}
	buildID, err := s.eng.Trigger(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	writeOK(w, map[string]int64{"build_id": buildID})
}

// ---------------------------------------------------------------- 构建 API

func (s *Server) handleBuildList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	projectID, _ := strconv.ParseInt(q.Get("project_id"), 10, 64)
	status := q.Get("status")
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	builds, err := s.store.ListBuilds(projectID, status, limit, offset)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	if builds == nil {
		builds = []*store.BuildRecord{}
	}
	writeOK(w, builds)
}

func (s *Server) handleBuildGet(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, 400, "非法 id")
		return
	}
	b, err := s.store.GetBuild(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, 404, err.Error())
		return
	}
	proj, _ := s.store.GetProject(b.ProjectID)
	writeOK(w, map[string]any{"build": b, "project_name": projName(proj)})
}

func projName(p *store.Project) string {
	if p == nil {
		return ""
	}
	return p.Name
}

// handleBuildLog SSE 日志流：运行中的构建增量推送，历史构建推送全文后关闭。
func (s *Server) handleBuildLog(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, 400, "非法 id")
		return
	}
	b, err := s.store.GetBuild(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, 404, err.Error())
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, 500, "当前连接不支持流式响应")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	sendEvent := func(event, payload string) bool {
		_, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, payload)
		if err != nil {
			return false
		}
		fl.Flush()
		return true
	}
	sendData := func(payload any) bool {
		b, _ := json.Marshal(payload)
		return sendEvent("log", string(b))
	}

	var offset int64
	lastStatus := ""
	for {
		select {
		case <-r.Context().Done():
			return
		default:
		}
		// 增量读取日志文件
		if b.LogPath != "" {
			if chunk, n, err := readFrom(b.LogPath, offset); err == nil && len(chunk) > 0 {
				if !sendData(map[string]string{"text": string(chunk)}) {
					return
				}
				offset = n
			}
		}
		// 状态变化推送
		nb, err := s.store.GetBuild(id)
		if err != nil {
			return
		}
		if nb.Status != lastStatus {
			lastStatus = nb.Status
			if !sendEvent("status", `{"status":"`+nb.Status+`"}`) {
				return
			}
		}
		if isTerminal(nb.Status) {
			// 终态且日志已读完：发送 done 后关闭
			if chunk, n, err := readFrom(b.LogPath, offset); err == nil && len(chunk) > 0 {
				if !sendData(map[string]string{"text": string(chunk)}) {
					return
				}
				offset = n
			}
			_ = sendEvent("done", `{"status":"`+nb.Status+`"}`)
			return
		}
		b = nb
		select {
		case <-r.Context().Done():
			return
		case <-time.After(400 * time.Millisecond):
		}
	}
}

func isTerminal(status string) bool {
	return status == "success" || status == "failed" || status == "cancelled"
}

// readFrom 从 offset 起增量读取文件内容，返回新内容与新 offset。
func readFrom(path string, offset int64) ([]byte, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, offset, err
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, offset, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, offset, err
	}
	return data, offset + int64(len(data)), nil
}

func (s *Server) handleBuildCancel(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, 400, "非法 id")
		return
	}
	if err := s.eng.Cancel(id); err != nil {
		writeErr(w, http.StatusBadRequest, 400, err.Error())
		return
	}
	writeOK(w, nil)
}

// ---------------------------------------------------------------- 工具链 API

func (s *Server) handleTcList(w http.ResponseWriter, r *http.Request) {
	tcs, err := s.store.ListToolchains()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	if tcs == nil {
		tcs = []*store.Toolchain{}
	}
	writeOK(w, tcs)
}

func (s *Server) handleTcCreate(w http.ResponseWriter, r *http.Request) {
	var t store.Toolchain
	if err := readJSON(r, &t); err != nil {
		writeErr(w, http.StatusBadRequest, 400, "请求体不是合法 JSON")
		return
	}
	if t.Name == "" || !toolchain.ValidType(t.Type) {
		writeErr(w, http.StatusBadRequest, 400, "名称必填且 type 必须是 jdk|maven|node|go|bun")
		return
	}
	if t.InstallPath == "" {
		t.InstallPath = s.tcMgr.DefaultInstallPath(t.Type, t.Version)
	}
	// 安装目录已存在则直接标记 ready（支持指向宿主已有工具链）
	if dirReady(t.InstallPath) {
		t.Status = "ready"
	}
	id, err := s.store.CreateToolchain(&t)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	created, _ := s.store.GetToolchain(id)
	writeOK(w, created)
}

func (s *Server) handleTcGet(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, 400, "非法 id")
		return
	}
	t, err := s.store.GetToolchain(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, 404, err.Error())
		return
	}
	writeOK(w, t)
}

func (s *Server) handleTcUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, 400, "非法 id")
		return
	}
	var t store.Toolchain
	if err := readJSON(r, &t); err != nil {
		writeErr(w, http.StatusBadRequest, 400, "请求体不是合法 JSON")
		return
	}
	t.ID = id
	if err := s.store.UpdateToolchain(&t); err != nil {
		writeErr(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	updated, _ := s.store.GetToolchain(id)
	writeOK(w, updated)
}

func (s *Server) handleTcDelete(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, 400, "非法 id")
		return
	}
	if err := s.store.DeleteToolchain(id); err != nil {
		writeErr(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	writeOK(w, nil)
}

func (s *Server) handleTcInstall(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, 400, "非法 id")
		return
	}
	if err := s.tcMgr.Install(id); err != nil {
		writeErr(w, http.StatusBadRequest, 400, err.Error())
		return
	}
	writeOK(w, nil)
}

func dirReady(p string) bool {
	if p == "" {
		return false
	}
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

// ---------------------------------------------------------------- 缓存与设置 API

func (s *Server) handleCacheUsage(w http.ResponseWriter, r *http.Request) {
	usage, total, err := s.cache.Usage()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	writeOK(w, map[string]any{"usage": usage, "total": total, "root": s.cache.Root()})
}

func (s *Server) handleCacheClear(w http.ResponseWriter, r *http.Request) {
	if err := s.cache.Clear(); err != nil {
		writeErr(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	writeOK(w, nil)
}

var publicSettings = []string{"auth.session_ttl", "server.port", "docker.enabled", "cache.root", "default.timeout",
	"mirror.goproxy", "mirror.npm_registry"}

func (s *Server) handleSettingsGet(w http.ResponseWriter, r *http.Request) {
	out := map[string]string{}
	for _, k := range publicSettings {
		v, err := s.store.GetSetting(k)
		if err == nil {
			out[k] = v
		}
	}
	writeOK(w, out)
}

func (s *Server) handleSettingsPut(w http.ResponseWriter, r *http.Request) {
	var body map[string]string
	if err := readJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, 400, "请求体不是合法 JSON")
		return
	}
	allowed := map[string]bool{}
	for _, k := range publicSettings {
		allowed[k] = true
	}
	for k := range body {
		if !allowed[k] {
			writeErr(w, http.StatusBadRequest, 400, "不允许修改设置项: "+k)
			return
		}
	}
	for k, v := range body {
		if err := s.store.SetSetting(k, v); err != nil {
			writeErr(w, http.StatusInternalServerError, 500, err.Error())
			return
		}
	}
	writeOK(w, nil)
}

func (s *Server) handlePassword(w http.ResponseWriter, r *http.Request) {
	var body struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, 400, "请求体不是合法 JSON")
		return
	}
	if err := s.auth.ChangePassword(body.OldPassword, body.NewPassword); err != nil {
		writeErr(w, http.StatusBadRequest, 400, err.Error())
		return
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	writeOK(w, nil)
}

// ---------------------------------------------------------------- 静态页

func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(r.URL.Path, "/")
	if p == "" {
		p = "index.html"
	}
	data, err := fs.ReadFile(s.webFS, p)
	if err != nil {
		// SPA 回退
		data, err = fs.ReadFile(s.webFS, "index.html")
		if err != nil {
			http.Error(w, "控制台资源缺失", http.StatusNotFound)
			return
		}
		p = "index.html"
	}
	switch filepath.Ext(p) {
	case ".html":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	case ".js":
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	case ".css":
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	case ".svg":
		w.Header().Set("Content-Type", "image/svg+xml")
	default:
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
}
