package auth

import (
	"path/filepath"
	"testing"
	"time"

	"samll-ci/internal/store"
)

func newTestAuth(t *testing.T) (*store.Store, *Manager) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st, New(st)
}

func TestDefaultPasswordAndLogin(t *testing.T) {
	st, m := newTestAuth(t)
	created, err := m.EnsureDefaultPassword()
	if err != nil || !created {
		t.Fatalf("首次应生成默认密码: %v %v", created, err)
	}
	// 二次调用不再生成
	if created, err := m.EnsureDefaultPassword(); created || err != nil {
		t.Fatalf("二次调用不应重复生成: %v %v", created, err)
	}
	// 错误密码
	if _, err := m.Login("wrong"); err == nil {
		t.Fatal("错误密码应被拒绝")
	}
	// 正确密码
	tok, err := m.Login(DefaultPassword)
	if err != nil || tok == "" {
		t.Fatalf("默认密码登录失败: %v", err)
	}
	if !m.Validate(tok) {
		t.Fatal("token 应有效")
	}
	_ = st
}

func TestSessionExpiryAndLogout(t *testing.T) {
	st, m := newTestAuth(t)
	_, _ = m.EnsureDefaultPassword()
	tok, err := m.Login("admin")
	if err != nil {
		t.Fatal(err)
	}
	// 手动把 session 过期时间改到过去
	if err := st.SetSetting("auth.session."+tok, "1"); err != nil {
		t.Fatal(err)
	}
	if m.Validate(tok) {
		t.Fatal("过期 session 应无效")
	}
	// 正常 token 注销后失效
	tok2, _ := m.Login("admin")
	m.Logout(tok2)
	if m.Validate(tok2) {
		t.Fatal("注销后应无效")
	}
}

func TestChangePassword(t *testing.T) {
	_, m := newTestAuth(t)
	_, _ = m.EnsureDefaultPassword()
	// 旧密码错误
	if err := m.ChangePassword("bad", "newpass"); err == nil {
		t.Fatal("旧密码错误应拒绝")
	}
	if err := m.ChangePassword("admin", "newpass"); err != nil {
		t.Fatalf("改密码失败: %v", err)
	}
	if _, err := m.Login("admin"); err == nil {
		t.Fatal("旧密码应已失效")
	}
	if _, err := m.Login("newpass"); err != nil {
		t.Fatalf("新密码登录失败: %v", err)
	}
}

func TestTTL(t *testing.T) {
	st, m := newTestAuth(t)
	if m.TTLSeconds() != 86400 {
		t.Fatalf("默认 TTL 应 86400, got %d", m.TTLSeconds())
	}
	_ = st.SetSetting("auth.session_ttl", "3600")
	if m.TTLSeconds() != 3600 {
		t.Fatalf("TTL 应 3600, got %d", m.TTLSeconds())
	}
	_ = time.Now
}
