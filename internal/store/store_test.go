package store

import (
	"path/filepath"
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestSettingCRUD(t *testing.T) {
	st := openTestStore(t)
	v, _ := st.GetSetting("k1")
	if v != "" {
		t.Fatalf("期望空值, got %q", v)
	}
	if err := st.SetSetting("k1", "v1"); err != nil {
		t.Fatal(err)
	}
	if v, _ := st.GetSetting("k1"); v != "v1" {
		t.Fatalf("期望 v1, got %q", v)
	}
	if err := st.SetSetting("k1", "v2"); err != nil {
		t.Fatal(err)
	}
	if v, _ := st.GetSetting("k1"); v != "v2" {
		t.Fatalf("期望覆盖为 v2, got %q", v)
	}
	m, _ := st.SettingsByPrefix("k")
	if len(m) != 1 || m["1"] != "v2" {
		t.Fatalf("SettingsByPrefix 结果异常: %v", m)
	}
	_ = st.DeleteSetting("k1")
	if v, _ := st.GetSetting("k1"); v != "" {
		t.Fatalf("删除后应取不到")
	}
}

func TestProjectCRUD(t *testing.T) {
	st := openTestStore(t)
	id, err := st.CreateProject(&Project{
		Name: "demo", GitURL: "https://example.com/x.git", Branch: "dev",
		BuildMode: "docker", Dockerfile: "Dockerfile", ImageTag: "demo:1",
		BuildCmds: "go build", OutputDir: "dist", DeployCmds: "echo hi",
		Timeout: 60, CacheEnabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	p, err := st.GetProject(id)
	if err != nil {
		t.Fatal(err)
	}
	if p.Branch != "dev" || p.BuildMode != "docker" || !p.CacheEnabled || p.Timeout != 60 {
		t.Fatalf("字段回读不符: %+v", p)
	}
	// 默认值
	id2, _ := st.CreateProject(&Project{Name: "d2", GitURL: "u"})
	p2, _ := st.GetProject(id2)
	if p2.Branch != "main" || p2.BuildMode != "shell" || p2.Timeout != 3600 {
		t.Fatalf("默认值不符: %+v", p2)
	}
	p2.Branch = "master"
	p2.CacheEnabled = false
	if err := st.UpdateProject(p2); err != nil {
		t.Fatal(err)
	}
	p2b, _ := st.GetProject(id2)
	if p2b.Branch != "master" || p2b.CacheEnabled {
		t.Fatalf("更新不符: %+v", p2b)
	}
	list, _ := st.ListProjects()
	if len(list) != 2 {
		t.Fatalf("期望 2 个项目, got %d", len(list))
	}
	if err := st.DeleteProject(id2); err != nil {
		t.Fatal(err)
	}
	list, _ = st.ListProjects()
	if len(list) != 1 {
		t.Fatalf("删除后期望 1 个, got %d", len(list))
	}
}

func TestBuildRecordAndRecovery(t *testing.T) {
	st := openTestStore(t)
	pid, _ := st.CreateProject(&Project{Name: "p", GitURL: "u"})
	bid, err := st.CreateBuild(pid, "logs/1.log")
	if err != nil {
		t.Fatal(err)
	}
	b, err := st.GetBuild(bid)
	if err != nil || b.Status != "pending" {
		t.Fatalf("初始状态应 pending: %+v %v", b, err)
	}
	if err := st.SetBuildLogPath(bid, "logs/x.log"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetBuildStatus(bid, "running", "abc123"); err != nil {
		t.Fatal(err)
	}
	b, _ = st.GetBuild(bid)
	if b.Status != "running" || b.GitCommit != "abc123" || b.StartAt == "" {
		t.Fatalf("running 状态不符: %+v", b)
	}
	if err := st.SetBuildStatus(bid, "success", ""); err != nil {
		t.Fatal(err)
	}
	b, _ = st.GetBuild(bid)
	if b.Status != "success" || b.EndAt == "" {
		t.Fatalf("终态不符: %+v", b)
	}
	last, err := st.LastBuildOfProject(pid)
	if err != nil || last.ID != bid {
		t.Fatalf("LastBuildOfProject 异常: %v", err)
	}
	// 重启恢复：running→failed；pending 重新入队
	r1, _ := st.CreateBuild(pid, "")
	_ = st.SetBuildStatus(r1, "running", "")
	r2, _ := st.CreateBuild(pid, "")
	interrupted, ids, err := st.RecoverBuilds()
	if err != nil {
		t.Fatal(err)
	}
	if len(interrupted) != 1 || interrupted[0].ID != r1 {
		t.Fatalf("中断构建应仅含 running %d, got %v", r1, interrupted)
	}
	if len(ids) != 1 || ids[0] != r2 {
		t.Fatalf("恢复入队应仅含 pending %d, got %v", r2, ids)
	}
	b1, _ := st.GetBuild(r1)
	if b1.Status != "failed" {
		t.Fatalf("重启后 running 应标记 failed, got %s", b1.Status)
	}
	// 列表筛选
	lst, _ := st.ListBuilds(pid, "failed", 10, 0)
	if len(lst) != 1 {
		t.Fatalf("按状态筛选异常: %d", len(lst))
	}
}

func TestToolchainCRUD(t *testing.T) {
	st := openTestStore(t)
	id, err := st.CreateToolchain(&Toolchain{
		Type: "maven", Name: "Maven 3.9", Version: "3.9.6",
		DownloadURL: "https://mirror/maven.tar.gz", MavenSettings: "<settings/>",
	})
	if err != nil {
		t.Fatal(err)
	}
	tc, err := st.GetToolchain(id)
	if err != nil || tc.Status != "not_installed" || tc.MavenSettings != "<settings/>" {
		t.Fatalf("回读不符: %+v %v", tc, err)
	}
	if err := st.SetToolchainStatus(id, "ready"); err != nil {
		t.Fatal(err)
	}
	tc, _ = st.GetToolchain(id)
	if tc.Status != "ready" {
		t.Fatal("状态更新失败")
	}
	tc.InstallPath = "/opt/maven"
	if err := st.UpdateToolchain(tc); err != nil {
		t.Fatal(err)
	}
	tc, _ = st.GetToolchain(id)
	if tc.InstallPath != "/opt/maven" {
		t.Fatal("install_path 更新失败")
	}
	if err := st.DeleteToolchain(id); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetToolchain(id); err == nil {
		t.Fatal("删除后应报错")
	}
}
