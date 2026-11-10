// Package cachemgr 管理构建缓存目录（依赖/编译缓存），提供占用统计与一键清理。
package cachemgr

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// 子目录名，按技术栈划分（见设计文档 §6.1）。
const (
	DirMaven  = "m2"       // Maven 本地仓库
	DirGradle = "gradle"   // Gradle 依赖缓存
	DirNpm    = "npm"      // npm/pnpm/yarn 缓存
	DirGoMod  = "go-mod"   // Go 模块缓存
	DirGoBuild = "go-build" // Go 编译缓存
	DirBun    = "bun"      // Bun 包缓存
)

// Names 全部缓存子目录。
var Names = []string{DirMaven, DirGradle, DirNpm, DirGoMod, DirGoBuild, DirBun}

var walkMu sync.Mutex // 并发构建只会有一个（单 worker），防御性串行化

// Manager 管理缓存根目录 <data>/cache。
type Manager struct {
	root string
}

func New(root string) *Manager {
	return &Manager{root: root}
}

// Root 返回缓存根目录。
func (m *Manager) Root() string { return m.root }

// Dir 返回某个缓存子目录的绝对路径。
func (m *Manager) Dir(name string) string { return filepath.Join(m.root, name) }

// EnsureDirs 确保所有缓存目录存在。
func (m *Manager) EnsureDirs() error {
	for _, n := range Names {
		if err := os.MkdirAll(m.Dir(n), 0o755); err != nil {
			return fmt.Errorf("创建缓存目录 %s 失败: %w", n, err)
		}
	}
	return nil
}

// Size 统计一个目录的磁盘占用（字节），目录不存在返回 0。
func Size(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // 忽略不可读条目
		}
		if d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

// Usage 返回各缓存子目录占用（字节）及总占用。
func (m *Manager) Usage() (map[string]int64, int64, error) {
	walkMu.Lock()
	defer walkMu.Unlock()
	usage := map[string]int64{}
	var total int64
	for _, n := range Names {
		s := Size(m.Dir(n))
		usage[n] = s
		total += s
	}
	return usage, total, nil
}

// Clear 一键清理：清空所有缓存子目录内容（保留目录本身）。
func (m *Manager) Clear() error {
	walkMu.Lock()
	defer walkMu.Unlock()
	for _, n := range Names {
		dir := m.Dir(n)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
				return fmt.Errorf("清理 %s 失败: %w", e.Name(), err)
			}
		}
	}
	return nil
}
