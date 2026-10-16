package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"worklog/internal/model"
)

// ErrSpanNotFound 表示时段不存在。
var ErrSpanNotFound = errors.New("时段不存在")

const spanCols = `id, task_id, start_at, end_at, created_at`

func scanSpan(sc interface {
	Scan(dest ...any) error
}) (model.Span, error) {
	var s model.Span
	var start, created int64
	var end sql.NullInt64
	if err := sc.Scan(&s.ID, &s.TaskID, &start, &end, &created); err != nil {
		return s, err
	}
	s.StartAt = fromUnix(start)
	if end.Valid {
		e := fromUnix(end.Int64)
		s.EndAt = &e
	}
	s.CreatedAt = fromUnix(created)
	return s, nil
}

// SpansOf 返回某事项的全部时段,按开始时间升序。
//
// 同秒靠 rowid 决胜 —— id 是随机十六进制,拿它排序等于把顺序交给运气。
func (s *Store) SpansOf(taskID string) ([]model.Span, error) {
	rows, err := s.db.Query(
		`SELECT `+spanCols+` FROM task_spans WHERE task_id = ? ORDER BY start_at, rowid`, taskID)
	if err != nil {
		return nil, fmt.Errorf("查询时段失败: %w", err)
	}
	defer rows.Close()
	return collectSpans(rows)
}

// collectSpans 读完全部行,并且**在返回前关掉 rows**。
//
// 关掉是必须的:连接池只有一条连接,rows 没关就再发查询会死锁。
// 调用方拿到返回值之后就可以安全地发下一次查询了。
func collectSpans(rows *sql.Rows) ([]model.Span, error) {
	out := []model.Span{}
	for rows.Next() {
		sp, err := scanSpan(rows)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("读取时段失败: %w", err)
		}
		out = append(out, sp)
	}
	err := rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	return out, nil
}

// AddSpan 给事项加一段实际投入。
//
// end 传 nil 表示这一段从 start 开始、此刻还在跑。
func (s *Store) AddSpan(taskID string, start time.Time, end *time.Time) (*model.Span, error) {
	if end != nil && !end.After(start) {
		return nil, fmt.Errorf("时段结束必须晚于开始")
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	// 先确认事项存在,否则外键报错的信息远不如这句清楚
	var exists int
	if err := tx.QueryRow(`SELECT 1 FROM tasks WHERE id = ?`, taskID).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrTaskNotFound
		}
		return nil, fmt.Errorf("查询事项失败: %w", err)
	}

	sp := model.Span{ID: newID(), TaskID: taskID, StartAt: start, EndAt: end, CreatedAt: nowFunc()}
	var endArg any
	if end != nil {
		endArg = unix(*end)
	}
	if _, err := tx.Exec(
		`INSERT INTO task_spans (id, task_id, start_at, end_at, created_at) VALUES (?,?,?,?,?)`,
		sp.ID, sp.TaskID, unix(sp.StartAt), endArg, unix(sp.CreatedAt)); err != nil {
		return nil, fmt.Errorf("写入时段失败: %w", err)
	}

	if err := syncEnvelope(tx, taskID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("提交失败: %w", err)
	}
	return &sp, nil
}

// CloseOpenSpan 关掉事项上正在跑的那一段。没有开着的段时返回 false,不报错 ——
// 调用方(暂停/结束)要的是"确保现在没有在跑",重复调用是安全的。
func (s *Store) CloseOpenSpan(taskID string, end time.Time) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	// 开着的段按开始时间取最后一个:理论上只可能有一个,取最后一个是为了
	// 万一有脏数据也能收敛,而不是随机关掉其中一段
	var id string
	var start int64
	err = tx.QueryRow(
		`SELECT id, start_at FROM task_spans WHERE task_id = ? AND end_at IS NULL
		 ORDER BY start_at DESC, rowid DESC LIMIT 1`, taskID).Scan(&id, &start)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("查询进行中的时段失败: %w", err)
	}

	// 时钟回拨或调用方给了更早的时间时,至少给这一段留一分钟,
	// 否则会写出一条负时长的记录
	if !end.After(fromUnix(start)) {
		end = fromUnix(start).Add(time.Minute)
	}
	if _, err := tx.Exec(`UPDATE task_spans SET end_at = ? WHERE id = ?`, unix(end), id); err != nil {
		return false, fmt.Errorf("关闭时段失败: %w", err)
	}

	if err := syncEnvelope(tx, taskID); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("提交失败: %w", err)
	}
	return true, nil
}

// DeleteSpan 删除一段时段。
func (s *Store) DeleteSpan(spanID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	var taskID string
	if err := tx.QueryRow(`SELECT task_id FROM task_spans WHERE id = ?`, spanID).Scan(&taskID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrSpanNotFound
		}
		return fmt.Errorf("查询时段失败: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM task_spans WHERE id = ?`, spanID); err != nil {
		return fmt.Errorf("删除时段失败: %w", err)
	}
	if err := syncEnvelope(tx, taskID); err != nil {
		return err
	}
	return tx.Commit()
}

// ReplaceSpans 用给定区间整体替换某事项的时段,只保留第一段。
//
// 用在"编辑弹窗改了时间"这条路径上:只有一段的事项,改时间就等于改那一段。
// 多段的事项不走这里 —— 那样会把用户辛苦补的时段悄悄抹掉。
func (s *Store) ReplaceSpans(taskID string, start, end time.Time) error {
	if !end.After(start) {
		return fmt.Errorf("结束时间必须晚于开始时间")
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM task_spans WHERE task_id = ?`, taskID); err != nil {
		return fmt.Errorf("清理旧时段失败: %w", err)
	}
	if _, err := tx.Exec(
		`INSERT INTO task_spans (id, task_id, start_at, end_at, created_at) VALUES (?,?,?,?,?)`,
		newID(), taskID, unix(start), unix(end), unix(nowFunc())); err != nil {
		return fmt.Errorf("写入时段失败: %w", err)
	}
	if err := syncEnvelope(tx, taskID); err != nil {
		return err
	}
	return tx.Commit()
}

// SpanHit 是一段时段连同它所属的事项。
type SpanHit struct {
	Span model.Span
	Task model.Task
}

// SpansBetween 返回与 [from, to) 有交集的时段,连同所属事项。
//
// 判据是**重叠**而不是"开始于区间内":一段从昨晚 23:00 做到今早 01:00,
// 它有一半在今天,应当算进今天。
func (s *Store) SpansBetween(from, to time.Time) ([]SpanHit, error) {
	rows, err := s.db.Query(
		`SELECT `+spanCols+` FROM task_spans
		 WHERE start_at < ? AND COALESCE(end_at, start_at) > ?
		 ORDER BY start_at, rowid`, unix(to), unix(from))
	if err != nil {
		return nil, fmt.Errorf("查询区间时段失败: %w", err)
	}
	defer rows.Close()

	spans, err := collectSpans(rows)
	if err != nil {
		return nil, err
	}
	if len(spans) == 0 {
		return nil, nil
	}

	// 一次把涉及的事项全捞回来,避免按条查
	ids := make([]any, 0, len(spans))
	seen := map[string]bool{}
	for _, sp := range spans {
		if !seen[sp.TaskID] {
			seen[sp.TaskID] = true
			ids = append(ids, sp.TaskID)
		}
	}
	tasks, err := s.tasksByIDs(ids)
	if err != nil {
		return nil, err
	}

	out := make([]SpanHit, 0, len(spans))
	for _, sp := range spans {
		t, ok := tasks[sp.TaskID]
		if !ok {
			continue // 事项被删了但时段还在,跳过而不是报错
		}
		out = append(out, SpanHit{Span: sp, Task: t})
	}
	return out, nil
}

// syncEnvelope 把事项的跨度重算成时段的包络。
//
// 只在时段变动时调用。人通过 update_task 手动改跨度时不走这里 ——
// 否则改完立刻被覆盖,跨度就永远等于实际投入,没法把范围拉宽。
//
// 还在跑的段按"开始时刻"参与计算,不用 now:拿 now 当结束会让存进去的值
// 随写入时刻变化,同一个库两次读出来不一样。真实的"进行到此刻"由读路径
// 补上(见 app.go 的 toTaskView)。末尾那个 +1 分钟是兜底,避免只有一段
// 开着的时段时跨度塌成零长 —— tasks.end_at 必须严格大于 start_at。
func syncEnvelope(tx *sql.Tx, taskID string) error {
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM task_spans WHERE task_id = ?`, taskID).Scan(&n); err != nil {
		return fmt.Errorf("统计时段失败: %w", err)
	}
	if n == 0 {
		// 时段被删光了,跨度保持当前值不动。
		//
		// 注意这里回不到"最初的计划时段" —— 那个值在第一次算出包络时就已经
		// 被覆盖了。要恢复只能由人重新编辑跨度,界面上就是这么做的。
		// 之所以不重置,是因为清空计时记录后跨度还留在原处更符合直觉:
		// 那条事项确实是在那个时间点做的,只是记录被删了。
		return nil
	}

	_, err := tx.Exec(`
		UPDATE tasks SET
		  start_at = (SELECT MIN(start_at) FROM task_spans WHERE task_id = ?),
		  end_at   = MAX(
		               (SELECT MAX(COALESCE(end_at, start_at)) FROM task_spans WHERE task_id = ?),
		               (SELECT MIN(start_at) + 60 FROM task_spans WHERE task_id = ?)
		             ),
		  updated_at = ?
		WHERE id = ?`, taskID, taskID, taskID, unix(nowFunc()), taskID)
	if err != nil {
		return fmt.Errorf("重算跨度失败: %w", err)
	}
	return nil
}

// tasksByIDs 按 ID 批量取事项。
//
// 分类必须在**读完并关掉 rows 之后**再挂 —— db.SetMaxOpenConns(1) 下
// 只有一条连接,行没读完就再发一次查询会直接死锁。
func (s *Store) tasksByIDs(ids []any) (map[string]model.Task, error) {
	ph := make([]string, len(ids))
	for i := range ids {
		ph[i] = "?"
	}
	rows, err := s.db.Query(
		`SELECT `+taskCols+` FROM tasks t WHERE t.id IN (`+strings.Join(ph, ",")+`)`, ids...)
	if err != nil {
		return nil, fmt.Errorf("批量查询事项失败: %w", err)
	}

	var list []model.Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("读取事项失败: %w", err)
		}
		list = append(list, t)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}

	out := make(map[string]model.Task, len(list))
	for _, t := range list {
		if err := s.attachCategory(&t); err != nil {
			return nil, err
		}
		out[t.ID] = t
	}
	return out, nil
}
