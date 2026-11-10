package toolchain

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"samll-ci/internal/store"
)

func TestExtractTarGz(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "tool.tar.gz")
	// 构造带单一顶层目录 jdk-17/ 的 tar.gz
	f, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	files := map[string]string{
		"jdk-17/bin/java":     "#!/bin/sh\necho java\n",
		"jdk-17/release":      "JAVA_VERSION=17",
		"jdk-17/conf/xx.conf": "conf",
	}
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	gz.Close()
	f.Close()

	dest := filepath.Join(dir, "out")
	if err := extract(archive, dest, filepath.Join(dir, "extract.log")); err != nil {
		t.Fatalf("extract: %v", err)
	}
	// 顶层目录应被剥离
	for _, p := range []string{"bin/java", "release", "conf/xx.conf"} {
		full := filepath.Join(dest, p)
		if _, err := os.Stat(full); err != nil {
			t.Fatalf("缺少解压产物 %s: %v", p, err)
		}
	}
	info, _ := os.Stat(filepath.Join(dest, "bin", "java"))
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("bin/java 权限应 755, got %v", info.Mode().Perm())
	}
}

func TestExtractZip(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "bun.zip")
	zf, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(zf)
	for _, name := range []string{"bun", "bun-license"} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte("#!/bin/sh\necho bun\n")); err != nil {
			t.Fatal(err)
		}
	}
	zw.Close()
	zf.Close()

	dest := filepath.Join(dir, "out")
	if err := extract(archive, dest, filepath.Join(dir, "extract.log")); err != nil {
		t.Fatalf("extract: %v", err)
	}
	info, err := os.Stat(filepath.Join(dest, "bun"))
	if err != nil {
		t.Fatalf("bun 未解压: %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("bun 权限应 755（zip 丢失执行位需补齐）, got %v", info.Mode().Perm())
	}
}

func TestExtractRejectsPathTraversal(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "evil.tar.gz")
	f, _ := os.Create(archive)
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	evil := "../../evil.txt"
	_ = tw.WriteHeader(&tar.Header{Name: evil, Mode: 0o644, Size: 5})
	_, _ = tw.Write([]byte("pwned"))
	tw.Close()
	gz.Close()
	f.Close()
	dest := filepath.Join(dir, "out")
	if err := extract(archive, dest, filepath.Join(dir, "extract.log")); err != nil {
		t.Fatalf("恶意路径应跳过而非报错: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "evil.txt")); err == nil {
		t.Fatal("路径穿越未被阻止")
	}
}

func TestSettingsFile(t *testing.T) {
	dataDir := t.TempDir()
	m := New(nil, dataDir)
	tc := &store.Toolchain{ID: 7, MavenSettings: "<settings><mirrors/></settings>"}
	p, err := m.SettingsFile(tc)
	if err != nil || p == "" {
		t.Fatalf("settings 文件生成失败: %v", err)
	}
	data, _ := os.ReadFile(p)
	if string(data) != tc.MavenSettings {
		t.Fatalf("内容不符: %q", string(data))
	}
	if !strings.Contains(p, filepath.Join(dataDir, "toolchains", "settings", "tc_7.xml")) {
		t.Fatalf("路径不符: %q", p)
	}
	// 空 settings 返回空串
	tc2 := &store.Toolchain{ID: 8, MavenSettings: ""}
	if p2, _ := m.SettingsFile(tc2); p2 != "" {
		t.Fatalf("空 settings 应返回空路径, got %q", p2)
	}
}
