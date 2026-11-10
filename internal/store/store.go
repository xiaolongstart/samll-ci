// Package store 提供基于 modernc.org/sqlite（纯 Go，无 CGO）的持久化层。
package store

import (
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Store 封装 SQLite 数据库访问。
type Store struct {
	db *sql.DB
	mu sync.Mutex // 串行化写操作，避免 SQLITE_BUSY
}

// Open 打开（必要时创建）数据库并初始化表结构。
func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS project (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			name          TEXT NOT NULL,
			git_url       TEXT NOT NULL,
			branch        TEXT DEFAULT 'main',
			token         TEXT DEFAULT '',
			build_mode    TEXT DEFAULT 'shell',
			toolchain_id  INTEGER DEFAULT 0,
			docker_image  TEXT DEFAULT '',
			dockerfile    TEXT DEFAULT '',
			image_tag     TEXT DEFAULT '',
			build_cmds    TEXT DEFAULT '',
			output_dir    TEXT DEFAULT '',
			deploy_cmds   TEXT DEFAULT '',
			timeout       INTEGER DEFAULT 3600,
			cache_enabled INTEGER DEFAULT 1,
			mem_limit_mib INTEGER DEFAULT 0,
			skip_built_commit INTEGER DEFAULT 0,
			created_at    TEXT DEFAULT (datetime('now','localtime')),
			updated_at    TEXT DEFAULT (datetime('now','localtime'))
		)`,
		`CREATE TABLE IF NOT EXISTS build_record (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			project_id INTEGER NOT NULL,
			status     TEXT DEFAULT 'pending',
			git_commit TEXT DEFAULT '',
			start_at   TEXT,
			end_at     TEXT,
			log_path   TEXT DEFAULT '',
			created_at TEXT DEFAULT (datetime('now','localtime'))
		)`,
		`CREATE INDEX IF NOT EXISTS idx_build_project ON build_record(project_id, created_at DESC)`,
		`CREATE TABLE IF NOT EXISTS toolchain (
			id             INTEGER PRIMARY KEY AUTOINCREMENT,
			type           TEXT NOT NULL,
			name           TEXT NOT NULL,
			version        TEXT DEFAULT '',
			download_url   TEXT DEFAULT '',
			install_path   TEXT DEFAULT '',
			maven_settings TEXT DEFAULT '',
			status         TEXT DEFAULT 'not_installed',
			created_at     TEXT DEFAULT (datetime('now','localtime'))
		)`,
		`CREATE TABLE IF NOT EXISTS setting (
			key   TEXT PRIMARY KEY,
			value TEXT
		)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("初始化表结构失败: %w", err)
		}
	}
	// 旧库升级（列已存在时忽略）
	alters := []string{
		`ALTER TABLE project ADD COLUMN mem_limit_mib INTEGER DEFAULT 0`,
		`ALTER TABLE project ADD COLUMN skip_built_commit INTEGER DEFAULT 0`,
	}
	for _, stmt := range alters {
		_, _ = s.db.Exec(stmt) // duplicate column 错误忽略
	}
	return nil
}

// ---------------------------------------------------------------- setting

func (s *Store) GetSetting(key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM setting WHERE key=?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

func (s *Store) SetSetting(key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT INTO setting(key,value) VALUES(?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// DeleteSetting 删除单个 key。
func (s *Store) DeleteSetting(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM setting WHERE key=?`, key)
	return err
}

// SettingsByPrefix 返回所有以 prefix 开头的 key（不含前缀）到 value 的映射。
func (s *Store) SettingsByPrefix(prefix string) (map[string]string, error) {
	rows, err := s.db.Query(`SELECT key,value FROM setting WHERE key LIKE ?`, prefix+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		m[strings.TrimPrefix(k, prefix)] = v
	}
	return m, rows.Err()
}

// ---------------------------------------------------------------- project

// Project 一条构建任务配置。
type Project struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	GitURL       string `json:"git_url"`
	Branch       string `json:"branch"`
	Token        string `json:"token"`
	BuildMode    string `json:"build_mode"` // shell | docker
	ToolchainID  int64  `json:"toolchain_id"`
	DockerImage  string `json:"docker_image"` // docker 模式①：基础镜像
	Dockerfile   string `json:"dockerfile"`   // docker 模式②：Dockerfile 路径（相对工作区）
	ImageTag     string `json:"image_tag"`    // docker 模式②：镜像 tag
	BuildCmds    string `json:"build_cmds"`   // 换行分隔
	OutputDir    string `json:"output_dir"`
	DeployCmds   string `json:"deploy_cmds"` // 换行分隔
	Timeout      int    `json:"timeout"`     // 秒
	CacheEnabled bool   `json:"cache_enabled"`
	// MemLimitMB 项目级内存护栏覆盖（0 = 继承全局设置 build.mem_limit_mib）
	MemLimitMB int `json:"mem_limit_mib"`
	// SkipBuiltCommit 同 commit 已成功构建（产物存在）时跳过构建直接复用（0/1）
	SkipBuiltCommit int    `json:"skip_built_commit"`
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
}

const projectCols = `id,name,git_url,branch,token,build_mode,toolchain_id,docker_image,dockerfile,image_tag,
build_cmds,output_dir,deploy_cmds,timeout,cache_enabled,mem_limit_mib,skip_built_commit,created_at,updated_at`

func scanProject(row interface{ Scan(...any) error }) (*Project, error) {
	p := &Project{}
	var cacheEnabled int
	err := row.Scan(&p.ID, &p.Name, &p.GitURL, &p.Branch, &p.Token, &p.BuildMode, &p.ToolchainID,
		&p.DockerImage, &p.Dockerfile, &p.ImageTag,
		&p.BuildCmds, &p.OutputDir, &p.DeployCmds, &p.Timeout, &cacheEnabled, &p.MemLimitMB,
		&p.SkipBuiltCommit, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, err
	}
	p.CacheEnabled = cacheEnabled != 0
	return p, nil
}

func (s *Store) ListProjects() ([]*Project, error) {
	rows, err := s.db.Query(`SELECT ` + projectCols + ` FROM project ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) GetProject(id int64) (*Project, error) {
	row := s.db.QueryRow(`SELECT `+projectCols+` FROM project WHERE id=?`, id)
	p, err := scanProject(row)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("项目 %d 不存在", id)
	}
	return p, err
}

func (s *Store) CreateProject(p *Project) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.Branch == "" {
		p.Branch = "main"
	}
	if p.BuildMode == "" {
		p.BuildMode = "shell"
	}
	if p.Timeout <= 0 {
		p.Timeout = 3600
	}
	res, err := s.db.Exec(`INSERT INTO project
		(name,git_url,branch,token,build_mode,toolchain_id,docker_image,dockerfile,image_tag,
		 build_cmds,output_dir,deploy_cmds,timeout,cache_enabled,mem_limit_mib,skip_built_commit)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		p.Name, p.GitURL, p.Branch, p.Token, p.BuildMode, p.ToolchainID, p.DockerImage, p.Dockerfile, p.ImageTag,
		p.BuildCmds, p.OutputDir, p.DeployCmds, p.Timeout, boolInt(p.CacheEnabled), p.MemLimitMB, p.SkipBuiltCommit)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) UpdateProject(p *Project) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.Branch == "" {
		p.Branch = "main"
	}
	if p.Timeout <= 0 {
		p.Timeout = 3600
	}
	_, err := s.db.Exec(`UPDATE project SET name=?,git_url=?,branch=?,token=?,build_mode=?,toolchain_id=?,
		docker_image=?,dockerfile=?,image_tag=?,build_cmds=?,output_dir=?,deploy_cmds=?,timeout=?,cache_enabled=?,
		mem_limit_mib=?,skip_built_commit=?,updated_at=datetime('now','localtime') WHERE id=?`,
		p.Name, p.GitURL, p.Branch, p.Token, p.BuildMode, p.ToolchainID, p.DockerImage, p.Dockerfile, p.ImageTag,
		p.BuildCmds, p.OutputDir, p.DeployCmds, p.Timeout, boolInt(p.CacheEnabled), p.MemLimitMB, p.SkipBuiltCommit, p.ID)
	return err
}

func (s *Store) DeleteProject(id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM project WHERE id=?`, id)
	return err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ---------------------------------------------------------------- build_record

// BuildRecord 一次流水线执行实例。
type BuildRecord struct {
	ID        int64  `json:"id"`
	ProjectID int64  `json:"project_id"`
	Status    string `json:"status"` // pending|running|success|failed|cancelled
	GitCommit string `json:"git_commit"`
	StartAt   string `json:"start_at"`
	EndAt     string `json:"end_at"`
	LogPath   string `json:"-"`
	CreatedAt string `json:"created_at"`
}

const buildCols = `id,project_id,status,git_commit,start_at,end_at,log_path,created_at`

func scanBuild(row interface{ Scan(...any) error }) (*BuildRecord, error) {
	b := &BuildRecord{}
	var startAt, endAt sql.NullString
	err := row.Scan(&b.ID, &b.ProjectID, &b.Status, &b.GitCommit, &startAt, &endAt, &b.LogPath, &b.CreatedAt)
	b.StartAt = startAt.String
	b.EndAt = endAt.String
	return b, err
}

func (s *Store) CreateBuild(projectID int64, logPath string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`INSERT INTO build_record(project_id,status,log_path) VALUES(?, 'pending', ?)`,
		projectID, logPath)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) GetBuild(id int64) (*BuildRecord, error) {
	row := s.db.QueryRow(`SELECT `+buildCols+` FROM build_record WHERE id=?`, id)
	b, err := scanBuild(row)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("构建 %d 不存在", id)
	}
	return b, err
}

// ListBuilds 构建历史列表，可按项目/状态筛选。
func (s *Store) ListBuilds(projectID int64, status string, limit, offset int) ([]*BuildRecord, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	q := `SELECT ` + buildCols + ` FROM build_record WHERE 1=1`
	var args []any
	if projectID > 0 {
		q += ` AND project_id=?`
		args = append(args, projectID)
	}
	if status != "" {
		q += ` AND status=?`
		args = append(args, status)
	}
	q += ` ORDER BY id DESC LIMIT ? OFFSET ?`
	args = append(args, limit, offset)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*BuildRecord
	for rows.Next() {
		b, err := scanBuild(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// SetBuildLogPath 更新构建日志路径。
func (s *Store) SetBuildLogPath(id int64, path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`UPDATE build_record SET log_path=? WHERE id=?`, path, id)
	return err
}

// SetBuildStatus 更新构建状态及时间戳。
func (s *Store) SetBuildStatus(id int64, status, gitCommit string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var startAt, endAt any
	switch status {
	case "running":
		startAt = time.Now().Format("2006-01-02 15:04:05")
	case "success", "failed", "cancelled":
		endAt = time.Now().Format("2006-01-02 15:04:05")
	}
	_, err := s.db.Exec(`UPDATE build_record SET status=?, git_commit=CASE WHEN ?<>'' THEN ? ELSE git_commit END,
		start_at=COALESCE(?, start_at), end_at=COALESCE(?, end_at) WHERE id=?`,
		status, gitCommit, gitCommit, startAt, endAt, id)
	return err
}

// RecoverBuilds 进程重启恢复：running 标记为 failed 并返回其 id 与日志路径（用于补记中断备注），
// pending 返回待重新入队的构建 id（按 id 升序）。
func (s *Store) RecoverBuilds() (interrupted []InterruptedBuild, pending []int64, err error) {
	s.mu.Lock()
	rows, err := s.db.Query(`SELECT id, log_path FROM build_record WHERE status='running'`)
	if err != nil {
		s.mu.Unlock()
		return nil, nil, err
	}
	for rows.Next() {
		var id int64
		var logPath string
		if err := rows.Scan(&id, &logPath); err != nil {
			rows.Close()
			s.mu.Unlock()
			return nil, nil, err
		}
		interrupted = append(interrupted, InterruptedBuild{ID: id, LogPath: logPath})
	}
	rows.Close()
	_, err = s.db.Exec(`UPDATE build_record SET status='failed', end_at=datetime('now','localtime')
		WHERE status='running'`)
	s.mu.Unlock()
	if err != nil {
		return nil, nil, err
	}

	rows2, err := s.db.Query(`SELECT id FROM build_record WHERE status='pending' ORDER BY id`)
	if err != nil {
		return nil, nil, err
	}
	defer rows2.Close()
	for rows2.Next() {
		var id int64
		if err := rows2.Scan(&id); err != nil {
			return nil, nil, err
		}
		pending = append(pending, id)
	}
	return interrupted, pending, rows2.Err()
}

// InterruptedBuild 重启时被标记 failed 的构建。
type InterruptedBuild struct {
	ID      int64
	LogPath string
}

// LastBuildOfProject 返回项目最近一次构建（无则 nil）。
func (s *Store) LastBuildOfProject(projectID int64) (*BuildRecord, error) {
	row := s.db.QueryRow(`SELECT `+buildCols+` FROM build_record WHERE project_id=? ORDER BY id DESC LIMIT 1`, projectID)
	b, err := scanBuild(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return b, err
}

// ---------------------------------------------------------------- toolchain

// Toolchain 工具链配置（JDK/Maven/Node/Go/Bun）。
type Toolchain struct {
	ID            int64  `json:"id"`
	Type          string `json:"type"` // jdk|maven|node|go|bun
	Name          string `json:"name"`
	Version       string `json:"version"`
	DownloadURL   string `json:"download_url"`
	InstallPath   string `json:"install_path"`
	MavenSettings string `json:"maven_settings"`
	Status        string `json:"status"` // not_installed|installing|ready|failed
	CreatedAt     string `json:"created_at"`
}

const tcCols = `id,type,name,version,download_url,install_path,maven_settings,status,created_at`

func scanToolchain(row interface{ Scan(...any) error }) (*Toolchain, error) {
	t := &Toolchain{}
	err := row.Scan(&t.ID, &t.Type, &t.Name, &t.Version, &t.DownloadURL, &t.InstallPath, &t.MavenSettings, &t.Status, &t.CreatedAt)
	return t, err
}

func (s *Store) ListToolchains() ([]*Toolchain, error) {
	rows, err := s.db.Query(`SELECT ` + tcCols + ` FROM toolchain ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Toolchain
	for rows.Next() {
		t, err := scanToolchain(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) GetToolchain(id int64) (*Toolchain, error) {
	row := s.db.QueryRow(`SELECT `+tcCols+` FROM toolchain WHERE id=?`, id)
	t, err := scanToolchain(row)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("工具链 %d 不存在", id)
	}
	return t, err
}

func (s *Store) CreateToolchain(t *Toolchain) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t.Status == "" {
		t.Status = "not_installed"
	}
	res, err := s.db.Exec(`INSERT INTO toolchain(type,name,version,download_url,install_path,maven_settings,status)
		VALUES(?,?,?,?,?,?,?)`,
		t.Type, t.Name, t.Version, t.DownloadURL, t.InstallPath, t.MavenSettings, t.Status)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) UpdateToolchain(t *Toolchain) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`UPDATE toolchain SET type=?,name=?,version=?,download_url=?,install_path=?,
		maven_settings=? WHERE id=?`,
		t.Type, t.Name, t.Version, t.DownloadURL, t.InstallPath, t.MavenSettings, t.ID)
	return err
}

func (s *Store) SetToolchainStatus(id int64, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`UPDATE toolchain SET status=? WHERE id=?`, status, id)
	return err
}

func (s *Store) DeleteToolchain(id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM toolchain WHERE id=?`, id)
	return err
}
