// Package store 负责 worklog 的持久化。
//
// 用 modernc.org/sqlite —— 纯 Go 实现,因为本机 CGO_ENABLED=0,
// mattn/go-sqlite3 这类 cgo 驱动无法编译。
package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// schemaVersion 记录表结构版本,用于增量迁移。
//
//	1 —— 初始表结构
//	2 —— task_spans:事项支持多时段(暂停/继续)
const schemaVersion = 2

// Store 是数据库句柄。并发安全由 database/sql 连接池保证。
type Store struct {
	db  *sql.DB
	dir string
}

// DefaultDir 返回数据目录 %LOCALAPPDATA%\worklog。
func DefaultDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("无法定位 LOCALAPPDATA: %w", err)
	}
	return filepath.Join(base, "worklog"), nil
}

// Open 打开(必要时创建)数据库。
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("创建数据目录失败: %w", err)
	}

	dbPath := filepath.Join(dir, "worklog.db")
	// DSN 里的路径必须用正斜杠,否则 Windows 反斜杠会被当作转义符
	dsn := "file:" + filepath.ToSlash(dbPath) + "?" + url.Values{
		"_pragma": {
			"journal_mode(WAL)",   // WAL 下读写不互斥,MCP 查询不会阻塞界面写入
			"foreign_keys(1)",     // 删分类把事项的 category_id 置空、删事项级联删备注
			"busy_timeout(5000)",  // 遇到锁时等待而非立刻报错(MCP 与界面是两个连接)
			"synchronous(NORMAL)", // WAL 下的常规取舍:断电可能丢最后几条,不损坏库
		},
	}.Encode()

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}
	// SQLite 写入是串行的,限制连接数避免 "database is locked"
	db.SetMaxOpenConns(1)

	s := &Store{db: db, dir: dir}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close 关闭数据库。
func (s *Store) Close() error { return s.db.Close() }

// Dir 返回数据目录。
func (s *Store) Dir() string { return s.dir }

const schemaV1 = `
CREATE TABLE IF NOT EXISTS categories (
  id         TEXT PRIMARY KEY,
  name       TEXT NOT NULL UNIQUE,
  color      TEXT NOT NULL,
  sort_order INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS tasks (
  id          TEXT PRIMARY KEY,
  title       TEXT NOT NULL,
  category_id TEXT REFERENCES categories(id) ON DELETE SET NULL,
  status      TEXT NOT NULL CHECK (status IN ('todo','doing','done')),
  start_at    INTEGER NOT NULL,
  end_at      INTEGER NOT NULL,
  confirmed   INTEGER NOT NULL DEFAULT 0,
  source      TEXT NOT NULL,
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS notes (
  id         TEXT PRIMARY KEY,
  task_id    TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  body       TEXT NOT NULL,
  source     TEXT NOT NULL,
  created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);

CREATE INDEX IF NOT EXISTS idx_tasks_start  ON tasks(start_at DESC);
CREATE INDEX IF NOT EXISTS idx_tasks_cat    ON tasks(category_id, start_at DESC);
CREATE INDEX IF NOT EXISTS idx_tasks_status ON tasks(status, start_at DESC);
CREATE INDEX IF NOT EXISTS idx_notes_task   ON notes(task_id, created_at);
`

// schemaV2 加入时段表。end_at 可空 —— 空表示这一段还在跑。
const schemaV2 = `
CREATE TABLE IF NOT EXISTS task_spans (
  id         TEXT PRIMARY KEY,
  task_id    TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  start_at   INTEGER NOT NULL,
  end_at     INTEGER,
  created_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_spans_task ON task_spans(task_id, start_at);
CREATE INDEX IF NOT EXISTS idx_spans_time ON task_spans(start_at);
`

// migrate 把库升到当前版本。
//
// 必须是分步的:早先的写法是"版本对不上就整批 CREATE TABLE IF NOT EXISTS",
// 那在加新表新列时根本不会生效 —— IF NOT EXISTS 对已存在的表是空操作,
// 而老库里的 tasks 表并不会因此多出新列。所以每个版本单独一段。
func (s *Store) migrate() error {
	var ver int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&ver); err != nil {
		return fmt.Errorf("读取表结构版本失败: %w", err)
	}
	if ver == schemaVersion {
		return nil
	}
	if ver > schemaVersion {
		return fmt.Errorf("数据库版本 %d 高于本程序支持的 %d,请升级程序", ver, schemaVersion)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("开启迁移事务失败: %w", err)
	}
	defer tx.Rollback()

	if ver < 1 {
		if _, err := tx.Exec(schemaV1); err != nil {
			return fmt.Errorf("建表失败: %w", err)
		}
		if err := seedCategories(tx); err != nil {
			return err
		}
	}
	if ver < 2 {
		if _, err := tx.Exec(schemaV2); err != nil {
			return fmt.Errorf("建时段表失败: %w", err)
		}
		if err := backfillSpans(tx); err != nil {
			return err
		}
	}

	if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion)); err != nil {
		return fmt.Errorf("写入表结构版本失败: %w", err)
	}
	return tx.Commit()
}

// backfillSpans 给升级前就存在的事项补一条时段,让时长口径不变。
//
// 只补 doing / done:待办还没有实际投入,给它建时段会让计划区间被算进时长 ——
// 那正是这次改动要修掉的口径。跨度列(start_at/end_at)原样保留,不丢信息。
func backfillSpans(tx *sql.Tx) error {
	_, err := tx.Exec(`
		INSERT INTO task_spans (id, task_id, start_at, end_at, created_at)
		SELECT lower(hex(randomblob(16))), id, start_at, end_at, created_at
		FROM tasks
		WHERE status IN ('doing','done') AND end_at > start_at`)
	if err != nil {
		return fmt.Errorf("回填时段失败: %w", err)
	}
	return nil
}

// seedCategories 给全新库放几个默认分类。
//
// 只在 categories 为空时插入 —— 用户删光了所有分类是他的选择,
// 不该下次启动又被塞回来。
func seedCategories(tx *sql.Tx) error {
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM categories`).Scan(&n); err != nil {
		return fmt.Errorf("统计分类失败: %w", err)
	}
	if n > 0 {
		return nil
	}
	defaults := []struct{ name, color string }{
		{"开发", "#7c6bff"},
		{"会议", "#3b8fe8"},
		{"运维", "#f0913a"},
		{"日常", "#2fb98a"},
	}
	now := time.Now().Unix()
	for i, d := range defaults {
		if _, err := tx.Exec(
			`INSERT INTO categories (id, name, color, sort_order, created_at) VALUES (?,?,?,?,?)`,
			newID(), d.name, d.color, i, now,
		); err != nil {
			return fmt.Errorf("写入默认分类失败: %w", err)
		}
	}
	return nil
}

// ---------------------------------------------------------------- 工具

// nowFunc 取当前时间。抽成变量是为了测试能冻结时间。
var nowFunc = time.Now

// newID 生成一个 16 字节的随机十六进制 ID。
//
// 不引 uuid 依赖:这个 ID 只在本机用,不参与任何需要全局唯一性的场景。
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 在现代系统上不会失败;真失败了也没法继续
		panic("无法生成随机 ID: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// unix 把时间转成库里的整数表示。统一用 UTC 秒,避免夏令时歧义。
func unix(t time.Time) int64 { return t.UTC().Unix() }

// fromUnix 把库里的整数还原成本地时区的时间。
func fromUnix(v int64) time.Time { return time.Unix(v, 0).Local() }
