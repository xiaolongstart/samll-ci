// Package auth 实现单账号密码登录：bcrypt 哈希 + 随机 session token（落库，带过期）。
package auth

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"strconv"
	"time"

	"golang.org/x/crypto/bcrypt"

	"samll-ci/internal/store"
)

const (
	settingPasswordHash = "auth.password_hash"
	settingSessionTTL   = "auth.session_ttl"
	sessionKeyPrefix    = "auth.session."
	defaultTTLSeconds   = 86400
	DefaultPassword     = "admin"
)

// Manager 管理密码与 session。
type Manager struct {
	store *store.Store
}

func New(st *store.Store) *Manager {
	return &Manager{store: st}
}

// EnsureDefaultPassword 首次启动时生成默认密码 admin 并提示。
func (m *Manager) EnsureDefaultPassword() (created bool, err error) {
	hash, err := m.store.GetSetting(settingPasswordHash)
	if err != nil {
		return false, err
	}
	if hash != "" {
		return false, nil
	}
	h, err := bcrypt.GenerateFromPassword([]byte(DefaultPassword), bcrypt.DefaultCost)
	if err != nil {
		return false, err
	}
	if err := m.store.SetSetting(settingPasswordHash, string(h)); err != nil {
		return false, err
	}
	log.Printf("[鉴权] 首次启动，默认密码为 %q，请尽快登录后在「设置」页修改", DefaultPassword)
	return true, nil
}

// TTLSeconds 读取 session 有效期（秒）。
func (m *Manager) TTLSeconds() int {
	v, err := m.store.GetSetting(settingSessionTTL)
	if err != nil || v == "" {
		return defaultTTLSeconds
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return defaultTTLSeconds
	}
	return n
}

// Login 校验密码；成功则签发随机 token 并落库（带过期时间）。
func (m *Manager) Login(password string) (string, error) {
	hash, err := m.store.GetSetting(settingPasswordHash)
	if err != nil {
		return "", err
	}
	if hash == "" {
		return "", fmt.Errorf("尚未初始化密码")
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		return "", fmt.Errorf("密码错误")
	}
	if err := m.cleanExpiredSessions(); err != nil {
		return "", err
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := hex.EncodeToString(buf)
	expiry := time.Now().Add(time.Duration(m.TTLSeconds()) * time.Second).Unix()
	if err := m.store.SetSetting(sessionKeyPrefix+token, strconv.FormatInt(expiry, 10)); err != nil {
		return "", err
	}
	return token, nil
}

// Validate 校验 token 是否有效且未过期。
func (m *Manager) Validate(token string) bool {
	if token == "" {
		return false
	}
	v, err := m.store.GetSetting(sessionKeyPrefix + token)
	if err != nil || v == "" {
		return false
	}
	expiry, err := strconv.ParseInt(v, 10, 64)
	if err != nil || time.Now().Unix() > expiry {
		return false
	}
	return true
}

// Logout 注销 session。
func (m *Manager) Logout(token string) {
	if token != "" {
		_ = m.store.DeleteSetting(sessionKeyPrefix + token)
	}
}

// ChangePassword 校验旧密码后更新为新密码，并清空全部 session 强制重新登录。
func (m *Manager) ChangePassword(oldPwd, newPwd string) error {
	if newPwd == "" {
		return fmt.Errorf("新密码不能为空")
	}
	hash, err := m.store.GetSetting(settingPasswordHash)
	if err != nil {
		return err
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(oldPwd)) != nil {
		return fmt.Errorf("旧密码错误")
	}
	h, err := bcrypt.GenerateFromPassword([]byte(newPwd), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	if err := m.store.SetSetting(settingPasswordHash, string(h)); err != nil {
		return err
	}
	// 清空所有 session，强制重新登录
	sessions, err := m.store.SettingsByPrefix(sessionKeyPrefix)
	if err == nil {
		for k := range sessions {
			_ = m.store.DeleteSetting(sessionKeyPrefix + k)
		}
	}
	return nil
}

func (m *Manager) cleanExpiredSessions() error {
	sessions, err := m.store.SettingsByPrefix(sessionKeyPrefix)
	if err != nil {
		return err
	}
	now := time.Now().Unix()
	for k, v := range sessions {
		expiry, err := strconv.ParseInt(v, 10, 64)
		if err != nil || now > expiry {
			_ = m.store.DeleteSetting(sessionKeyPrefix + k)
		}
	}
	return nil
}
