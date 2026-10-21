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
//	3 —— 分类配色去重(无 DDL,只改数据)
//	4 —— notes.updated_at:备注可编辑
//	5 —— components / requirements:需求进度管理
const schemaVersion = 5

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

// schemaV4 给备注加 updated_at,记下它被改过。
//
// 可空、不给默认值:NULL 就是"从没改过"。这正好也是 ALTER TABLE ADD COLUMN
// 唯一能接受的形态 —— 不能加一个既 NOT NULL 又没有默认值的列。
// 老库升上来时所有备注都是 NULL,它们确实没被改过,不用回填。
//
// 上面 schemaV1 里那份 CREATE TABLE notes **不要动** —— 它是历史版本,
// 改了会让全新库和从 v1 一步步升上来的库表结构对不上。
const schemaV4 = `ALTER TABLE notes ADD COLUMN updated_at INTEGER`

// schemaV5 加需求进度管理要的两张表。
//
// **没有 seed 步骤** —— 和分类不同。分类有默认值是因为"开发/会议/运维"是通用起点;
// 组件是用户自己的项目名,凭空塞几个假的比空着更烦人。第一个组件在新增需求时
// 输入名字自动建(见 EnsureComponent)。
//
// requirements.online_at 可空:NULL 就是"还没上线"。它和 stage 必须一致
// (stage=online ⟺ online_at 有值),这个不变量由 store.applyStage 统一维护,
// 不交给调用方。
const schemaV5 = `
CREATE TABLE IF NOT EXISTS components (
  id         TEXT PRIMARY KEY,
  name       TEXT NOT NULL UNIQUE,
  color      TEXT NOT NULL,
  sort_order INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS requirements (
  id           TEXT PRIMARY KEY,
  title        TEXT NOT NULL,
  component_id TEXT REFERENCES components(id) ON DELETE SET NULL,
  link         TEXT NOT NULL DEFAULT '',
  detail       TEXT NOT NULL DEFAULT '',
  stage        TEXT NOT NULL CHECK (stage IN ('submit','clarify','dev','accept','online')),
  start_at     INTEGER NOT NULL,
  online_at    INTEGER,
  source       TEXT NOT NULL,
  created_at   INTEGER NOT NULL,
  updated_at   INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_req_component ON requirements(component_id, start_at DESC);
CREATE INDEX IF NOT EXISTS idx_req_stage     ON requirements(stage, start_at DESC);
CREATE INDEX IF NOT EXISTS idx_req_online    ON requirements(online_at);
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
	if ver < 3 {
		if err := dedupeCategoryColors(tx); err != nil {
			return err
		}
	}
	if ver < 4 {
		if _, err := tx.Exec(schemaV4); err != nil {
			return fmt.Errorf("给备注加 updated_at 失败: %w", err)
		}
	}
	if ver < 5 {
		if _, err := tx.Exec(schemaV5); err != nil {
			return fmt.Errorf("建需求表失败: %w", err)
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

// dedupeCategoryColors 把"看着一样"的分类颜色拆开。
//
// 为什么不按字符串相等去重:老调色板里的 #7c6bff 和 #8b5cf6 是两个不同的
// 字符串,但肉眼几乎分不出(ΔE = 9.1)—— 而那恰恰是用户会抱怨的东西,
// 字符串去重根本不会碰它。所以按 ΔE 判断,见 color.go。
//
// 按 sort_order 走,**先出现的保留原色,只有后出现的撞色才会被换掉** ——
// 这样用户已有的列表大体不动,不会醒来发现整个配色被重排了。
//
// 分两遍做。一遍做不了:换色的时候要挑一个"离已保留色最远"的,
// 但如果把被替换的那些的原色也算成"在用",挑选范围会被无谓地缩掉;
// 不算的话又可能挑中一条**没被替换**的分类正在用的颜色(甚至只是同色系的),
// 轮到它时就得再换一次 —— 最后多改好几个,还可能是它本来好好的。
func dedupeCategoryColors(tx *sql.Tx) error {
	rows, err := tx.Query(`SELECT id, color FROM categories ORDER BY sort_order, name`)
	if err != nil {
		return fmt.Errorf("读取分类配色失败: %w", err)
	}

	type cat struct {
		id, color string
		replace   bool
	}
	var cats []cat
	for rows.Next() {
		var c cat
		if err := rows.Scan(&c.id, &c.color); err != nil {
			rows.Close()
			return fmt.Errorf("读取分类配色失败: %w", err)
		}
		cats = append(cats, c)
	}
	err = rows.Err()
	// 必须显式关:连接池只有一个连接,rows 没关时下面写不进去
	rows.Close()
	if err != nil {
		return err
	}

	// 第一遍:定下哪些要换。保留色的集合在这里就固定了,后面只增不减。
	var kept []string
	for i := range cats {
		col := normHex(cats[i].color)
		if tooClose(col, kept) {
			cats[i].replace = true
			continue // 被替换的颜色不占位 —— 它马上就要走了
		}
		kept = append(kept, col)
	}

	// 第二遍:换色。可选的色要排除掉所有保留色的当前值。
	used := map[string]int{}
	for _, c := range cats {
		if !c.replace {
			used[normHex(c.color)]++
		}
	}
	for _, c := range cats {
		if !c.replace {
			continue
		}
		col := farthestFrom(kept, used)
		if _, err := tx.Exec(`UPDATE categories SET color = ? WHERE id = ?`, col, c.id); err != nil {
			return fmt.Errorf("改写分类配色失败: %w", err)
		}
		used[col]++
		kept = append(kept, col) // 下一个替换色也要离它远点
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
		{"会议", "#3566d8"},
		{"运维", "#cf6415"},
		{"日常", "#178a55"},
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
