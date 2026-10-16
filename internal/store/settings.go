package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
)

// SettingKeyMCPToken 是 MCP 访问令牌在 settings 表里的键。
const SettingKeyMCPToken = "mcp_token"

// Setting 读取一项配置。不存在时返回 ("", false, nil)。
func (s *Store) Setting(key string) (string, bool, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("读取配置 %s 失败: %w", key, err)
	}
	return v, true, nil
}

// SetSetting 写入一项配置。
func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(
		`INSERT INTO settings (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	if err != nil {
		return fmt.Errorf("写入配置 %s 失败: %w", key, err)
	}
	return nil
}

// EnsureMCPToken 返回 MCP 访问令牌,没有就生成一个并存下来。
//
// 令牌要稳定:它会被写进 Claude Code 的配置,每次启动都换新的会让
// 已经配好的连接全部失效。
func (s *Store) EnsureMCPToken() (string, error) {
	if v, ok, err := s.Setting(SettingKeyMCPToken); err != nil {
		return "", err
	} else if ok && v != "" {
		return v, nil
	}

	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("生成令牌失败: %w", err)
	}
	token := hex.EncodeToString(b[:])
	if err := s.SetSetting(SettingKeyMCPToken, token); err != nil {
		return "", err
	}
	return token, nil
}
