// Package toolchain 管理工具链的下载、安装与环境变量注入。
// 下载地址可自定义（内网镜像源），安装后保留复用；安装日志落盘可查。
package toolchain

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"samll-ci/internal/store"
)

// Manager 工具链管理器。
type Manager struct {
	store   *store.Store
	dataDir string // <data>/toolchains、<data>/logs

	mu         sync.Mutex
	installing map[int64]bool
}

func New(st *store.Store, dataDir string) *Manager {
	return &Manager{store: st, dataDir: dataDir, installing: map[int64]bool{}}
}

// Types 支持的工具链类型。
var Types = []string{"jdk", "maven", "gradle", "node", "go", "bun"}

// ValidType 校验工具链类型。
func ValidType(t string) bool {
	for _, v := range Types {
		if v == t {
			return true
		}
	}
	return false
}

// DefaultInstallPath 返回默认安装目录 <data>/toolchains/<type>/<version>。
func (m *Manager) DefaultInstallPath(tcType, version string) string {
	return filepath.Join(m.dataDir, "toolchains", tcType, version)
}

// InstallLogPath 工具链安装日志路径。
func (m *Manager) InstallLogPath(id int64) string {
	return filepath.Join(m.dataDir, "logs", fmt.Sprintf("toolchain_%d.log", id))
}

// Install 异步安装：status → installing → ready/failed，日志写入 install log。
func (m *Manager) Install(id int64) error {
	m.mu.Lock()
	if m.installing[id] {
		m.mu.Unlock()
		return fmt.Errorf("工具链 %d 正在安装中", id)
	}
	m.installing[id] = true
	m.mu.Unlock()

	tc, err := m.store.GetToolchain(id)
	if err != nil {
		m.mu.Lock()
		delete(m.installing, id)
		m.mu.Unlock()
		return err
	}
	if tc.Status == "installing" {
		m.mu.Lock()
		delete(m.installing, id)
		m.mu.Unlock()
		return fmt.Errorf("工具链 %d 正在安装中", id)
	}

	_ = m.store.SetToolchainStatus(id, "installing")
	go func() {
		defer func() {
			m.mu.Lock()
			delete(m.installing, id)
			m.mu.Unlock()
		}()
		if err := m.InstallSync(context.Background(), tc); err != nil {
			appendLog(m.InstallLogPath(id), "安装失败: "+err.Error()+"\n")
			_ = m.store.SetToolchainStatus(id, "failed")
			return
		}
		_ = m.store.SetToolchainStatus(id, "ready")
	}()
	return nil
}

// EnsureInstalled 构建阶段②：工具链未就绪则同步安装（幂等，ready 直接跳过）。
func (m *Manager) EnsureInstalled(ctx context.Context, tc *store.Toolchain, w io.Writer) error {
	if tc.Status == "ready" && dirReady(tc.InstallPath) {
		fmt.Fprintf(w, "工具链 [%s] 已就绪：%s\n", tc.Name, tc.InstallPath)
		return nil
	}
	fmt.Fprintf(w, "工具链 [%s] 未就绪（status=%s），开始下载安装...\n", tc.Name, tc.Status)
	if err := m.InstallSync(ctx, tc); err != nil {
		_ = m.store.SetToolchainStatus(tc.ID, "failed")
		return fmt.Errorf("工具链安装失败: %w", err)
	}
	return m.store.SetToolchainStatus(tc.ID, "ready")
}

// InstallSync 同步下载并安装工具链。
func (m *Manager) InstallSync(ctx context.Context, tc *store.Toolchain) error {
	if tc.DownloadURL == "" {
		return fmt.Errorf("未配置下载地址")
	}
	installPath := tc.InstallPath
	if installPath == "" {
		installPath = m.DefaultInstallPath(tc.Type, tc.Version)
	}
	if dirReady(installPath) {
		return nil // 已存在，保留复用
	}
	logPath := m.InstallLogPath(tc.ID)
	_ = os.MkdirAll(filepath.Dir(logPath), 0o755)

	tmpRoot, err := os.MkdirTemp(filepath.Join(m.dataDir, "toolchains"), "dl-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpRoot)

	archiveFile, err := m.download(ctx, tc.DownloadURL, tmpRoot, logPath)
	if err != nil {
		return err
	}
	extractDir := filepath.Join(tmpRoot, "x")
	if err := extract(archiveFile, extractDir, logPath); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(installPath), 0o755); err != nil {
		return err
	}
	if err := os.Rename(extractDir, installPath); err != nil {
		// 跨设备时退化为复制
		if err := copyTree(extractDir, installPath); err != nil {
			return fmt.Errorf("安装目录移动失败: %w", err)
		}
	}
	appendLog(logPath, "安装完成: "+installPath+"\n")
	return nil
}

// download 下载压缩包到 destDir，进度写入日志。
func (m *Manager) download(ctx context.Context, url, destDir, logPath string) (string, error) {
	appendLog(logPath, "下载: "+url+"\n")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("下载失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("下载失败: HTTP %d", resp.StatusCode)
	}
	name := sanitizeName(url)
	if name == "" {
		name = "toolchain.bin"
	}
	dst := filepath.Join(destDir, name)
	f, err := os.Create(dst)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var written int64
	buf := make([]byte, 32*1024)
	lastMB := int64(-1)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				return "", werr
			}
			written += int64(n)
			if mb := written / (1024 * 1024); mb != lastMB {
				lastMB = mb
				appendLog(logPath, fmt.Sprintf("已下载 %d MB\n", mb))
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
	}
	appendLog(logPath, fmt.Sprintf("下载完成，共 %d 字节\n", written))
	return dst, nil
}

// extract 解压 tar.gz / zip（按文件头嗅探）到 destDir：
// 自动剥离单一顶层目录（如 jdk-17.0.9/），并补齐可执行位。
func extract(archive, destDir, logPath string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	head := make([]byte, 2)
	if _, err := io.ReadFull(f, head); err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if head[0] == 0x1f && head[1] == 0x8b {
		appendLog(logPath, "识别为 tar.gz，开始解压\n")
		if err := extractTarGz(f, destDir); err != nil {
			return err
		}
	} else {
		appendLog(logPath, "识别为 zip，开始解压\n")
		if err := extractZipFile(f, destDir); err != nil {
			return err
		}
		// zip 可能丢失执行位（部分打包器），统一补齐常见可执行目录
		chmodBins(destDir)
	}
	return stripTopDir(destDir)
}

// stripTopDir 若解压结果为单一顶层目录，则将其内容上移一层。
func stripTopDir(destDir string) error {
	entries, err := os.ReadDir(destDir)
	if err != nil {
		return err
	}
	if len(entries) != 1 || !entries[0].IsDir() {
		return nil
	}
	inner := filepath.Join(destDir, entries[0].Name())
	children, err := os.ReadDir(inner)
	if err != nil {
		return err
	}
	for _, c := range children {
		if err := os.Rename(filepath.Join(inner, c.Name()), filepath.Join(destDir, c.Name())); err != nil {
			return err
		}
	}
	return os.RemoveAll(inner)
}

func extractTarGz(r io.Reader, destDir string) error {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		// 防路径穿越
		target := filepath.Join(destDir, filepath.Clean("/"+hdr.Name))
		if !strings.HasPrefix(target, filepath.Clean(destDir)+string(os.PathSeparator)) &&
			target != filepath.Clean(destDir) {
			continue
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)|0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)|0o600)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			out.Close()
		case tar.TypeSymlink:
			_ = os.MkdirAll(filepath.Dir(target), 0o755)
			_ = os.Symlink(hdr.Linkname, target)
		}
	}
}

func extractZipFile(f *os.File, destDir string) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	return extractZip(f, info.Size(), destDir)
}

func extractZip(r io.ReaderAt, size int64, destDir string) error {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}
	for _, zf := range zr.File {
		target := filepath.Join(destDir, filepath.Clean("/"+zf.Name))
		if !strings.HasPrefix(target, filepath.Clean(destDir)+string(os.PathSeparator)) &&
			target != filepath.Clean(destDir) {
			continue
		}
		if zf.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := zf.Open()
		if err != nil {
			return err
		}
		mode := zf.Mode()
		if mode == 0 {
			mode = 0o644
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode|0o600)
		if err != nil {
			rc.Close()
			return err
		}
		_, err = io.Copy(out, rc)
		out.Close()
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// chmodBins 保证可执行权限（zip 解压可能丢失执行位）。
func chmodBins(installPath string) {
	binDir := filepath.Join(installPath, "bin")
	if entries, err := os.ReadDir(binDir); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				_ = os.Chmod(filepath.Join(binDir, e.Name()), 0o755)
			}
		}
	}
	// 顶层单二进制（如 bun）
	if entries, err := os.ReadDir(installPath); err == nil && len(entries) <= 4 {
		for _, e := range entries {
			if !e.IsDir() {
				_ = os.Chmod(filepath.Join(installPath, e.Name()), 0o755)
			}
		}
	}
}

// SettingsFile 将 Maven settings.xml 内容写入托管路径并返回文件路径（无内容返回空串）。
func (m *Manager) SettingsFile(tc *store.Toolchain) (string, error) {
	if strings.TrimSpace(tc.MavenSettings) == "" {
		return "", nil
	}
	dir := filepath.Join(m.dataDir, "toolchains", "settings")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	p := filepath.Join(dir, fmt.Sprintf("tc_%d.xml", tc.ID))
	if err := os.WriteFile(p, []byte(tc.MavenSettings), 0o644); err != nil {
		return "", err
	}
	return p, nil
}

// ---------- 工具函数 ----------

func dirReady(p string) bool {
	if p == "" {
		return false
	}
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

func sanitizeName(url string) string {
	name := url
	if i := strings.LastIndexByte(url, '/'); i >= 0 {
		name = url[i+1:]
	}
	name = strings.SplitN(name, "?", 2)[0]
	if name == "" || name == "/" {
		return ""
	}
	if len(name) > 80 {
		name = name[len(name)-80:]
	}
	return name
}

func appendLog(path, s string) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(time.Now().Format("15:04:05 ") + s)
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		info, err := d.Info()
		mode := os.FileMode(0o644)
		if err == nil {
			mode = info.Mode().Perm()
		}
		return os.WriteFile(target, data, mode)
	})
}
