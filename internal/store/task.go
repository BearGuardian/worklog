package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"worklog/internal/model"
)

// ErrTaskNotFound 表示事项不存在。
var ErrTaskNotFound = errors.New("事项不存在")

// TaskFilter 是 ListTasks 的过滤条件。零值表示不限。
type TaskFilter struct {
	From     *time.Time // start_at >= From
	To       *time.Time // start_at <  To
	Status   model.Status
	Category string // 分类 ID;"__none__" 表示未分类
	Query    string // 标题模糊匹配
	Limit    int
}

// NoCategory 是 TaskFilter.Category 的特殊值,用来筛"未分类"的事项。
const NoCategory = "__none__"

const taskCols = `t.id, t.title, COALESCE(t.category_id,''), t.status,
                  t.start_at, t.end_at, t.confirmed, t.source, t.created_at, t.updated_at`

func scanTask(sc interface {
	Scan(dest ...any) error
}) (model.Task, error) {
	var t model.Task
	var start, end, created, updated int64
	var confirmed int
	if err := sc.Scan(&t.ID, &t.Title, &t.CategoryID, &t.Status,
		&start, &end, &confirmed, &t.Source, &created, &updated); err != nil {
		return t, err
	}
	t.StartAt = fromUnix(start)
	t.EndAt = fromUnix(end)
	t.Confirmed = confirmed != 0
	t.CreatedAt = fromUnix(created)
	t.UpdatedAt = fromUnix(updated)
	return t, nil
}

// CreateTask 新建事项。ID 为空时自动生成。
//
// 时段按状态决定:
//   - todo  不建时段 —— 还没开始,时长是 0。跨度就是计划时段。
//   - doing 建一段**开着的**时段,从 StartAt 起。既然声明了在做,时长就该开始走。
//   - done  建一段闭区间 [StartAt, EndAt],时长即这段的长度。
func (s *Store) CreateTask(t model.Task) (*model.Task, error) {
	if strings.TrimSpace(t.Title) == "" {
		return nil, fmt.Errorf("标题不能为空")
	}
	if !t.Status.Valid() {
		return nil, fmt.Errorf("状态非法: %q", t.Status)
	}
	// 允许相等:**零长区间表示"结束时间还没定"**,不是错误。
	// 跨度是半开区间 [start, end),end == start 就是"只有起点、终点未知"。
	// 只有"没有时段"的事项会出现这种跨度 —— syncEnvelope 给有时段的事项
	// 兜了 +60,不会塌成零长。见 DESIGN §7.3。
	if t.EndAt.Before(t.StartAt) {
		return nil, fmt.Errorf("结束时间不能早于开始时间")
	}
	if t.Source == "" {
		t.Source = model.SourceUI
	}

	now := nowFunc()
	if t.ID == "" {
		t.ID = newID()
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = now
	}
	t.UpdatedAt = now

	var catID any
	if t.CategoryID != "" {
		catID = t.CategoryID
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(
		`INSERT INTO tasks (id, title, category_id, status, start_at, end_at,
		                    confirmed, source, created_at, updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?)`,
		t.ID, t.Title, catID, t.Status, unix(t.StartAt), unix(t.EndAt),
		boolInt(t.Confirmed), t.Source, unix(t.CreatedAt), unix(t.UpdatedAt),
	); err != nil {
		return nil, fmt.Errorf("新建事项失败: %w", err)
	}

	switch t.Status {
	case model.StatusDoing:
		if _, err := tx.Exec(
			`INSERT INTO task_spans (id, task_id, start_at, end_at, created_at) VALUES (?,?,?,NULL,?)`,
			newID(), t.ID, unix(t.StartAt), unix(now)); err != nil {
			return nil, fmt.Errorf("写入时段失败: %w", err)
		}
	case model.StatusDone:
		if _, err := tx.Exec(
			`INSERT INTO task_spans (id, task_id, start_at, end_at, created_at) VALUES (?,?,?,?,?)`,
			newID(), t.ID, unix(t.StartAt), unix(t.EndAt), unix(now)); err != nil {
			return nil, fmt.Errorf("写入时段失败: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("提交失败: %w", err)
	}
	return s.GetTask(t.ID)
}

// GetTask 取单个事项,连带分类、备注与时段。
func (s *Store) GetTask(id string) (*model.Task, error) {
	row := s.db.QueryRow(`SELECT `+taskCols+` FROM tasks t WHERE t.id = ?`, id)
	t, err := scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrTaskNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("查询事项失败: %w", err)
	}
	if err := s.attachCategory(&t); err != nil {
		return nil, err
	}
	notes, err := s.NotesOf(t.ID)
	if err != nil {
		return nil, err
	}
	t.Notes = notes

	spans, err := s.SpansOf(t.ID)
	if err != nil {
		return nil, err
	}
	t.Spans = spans
	return &t, nil
}

// ListTasks 按条件查询事项,按开始时间升序。附带分类与时段,不含备注。
func (s *Store) ListTasks(f TaskFilter) ([]model.Task, error) {
	where := []string{"1=1"}
	args := []any{}

	if f.From != nil {
		where = append(where, "t.start_at >= ?")
		args = append(args, unix(*f.From))
	}
	if f.To != nil {
		where = append(where, "t.start_at < ?")
		args = append(args, unix(*f.To))
	}
	if f.Status != "" {
		where = append(where, "t.status = ?")
		args = append(args, string(f.Status))
	}
	switch f.Category {
	case "":
		// 不限
	case NoCategory:
		where = append(where, "t.category_id IS NULL")
	default:
		where = append(where, "t.category_id = ?")
		args = append(args, f.Category)
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		where = append(where, `t.title LIKE ? ESCAPE '\'`)
		args = append(args, "%"+escapeLike(q)+"%")
	}

	// 同一开始时刻的事项靠 rowid 决胜:没有它,两条并列的事项在两次
	// 刷新之间会互换位置,界面上看着像在抖。
	q := `SELECT ` + taskCols + ` FROM tasks t WHERE ` + strings.Join(where, " AND ") +
		` ORDER BY t.start_at, t.rowid`
	if f.Limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", f.Limit)
	}

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("查询事项失败: %w", err)
	}
	defer rows.Close()

	out := []model.Task{}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, fmt.Errorf("读取事项失败: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i := range out {
		if err := s.attachCategory(&out[i]); err != nil {
			return nil, err
		}
	}
	if err := s.attachSpans(out); err != nil {
		return nil, err
	}
	return out, nil
}

// attachSpans 给一批事项批量挂上时段。
//
// 界面一次取一整周,按条查会变成 N+1。这里一次捞回再按 task_id 分发,
// 和 attachCategory 是同一个套路。
func (s *Store) attachSpans(tasks []model.Task) error {
	if len(tasks) == 0 {
		return nil
	}
	ids := make([]any, 0, len(tasks))
	for _, t := range tasks {
		ids = append(ids, t.ID)
	}
	ph := make([]string, len(ids))
	for i := range ph {
		ph[i] = "?"
	}

	rows, err := s.db.Query(
		`SELECT `+spanCols+` FROM task_spans WHERE task_id IN (`+strings.Join(ph, ",")+`)
		 ORDER BY start_at, rowid`, ids...)
	if err != nil {
		return fmt.Errorf("批量查询时段失败: %w", err)
	}
	defer rows.Close()

	spans, err := collectSpans(rows)
	if err != nil {
		return err
	}
	byTask := map[string][]model.Span{}
	for _, sp := range spans {
		byTask[sp.TaskID] = append(byTask[sp.TaskID], sp)
	}
	for i := range tasks {
		tasks[i].Spans = byTask[tasks[i].ID]
	}
	return nil
}

// TaskPatch 描述一次局部更新。nil 表示该字段不动。
type TaskPatch struct {
	Title      *string
	CategoryID *string // 指向空串表示改为未分类
	Status     *model.Status
	StartAt    *time.Time
	EndAt      *time.Time
	Confirmed  *bool
}

// UpdateTask 局部更新事项。
//
// 这里改的 StartAt / EndAt 是**跨度**,而且刻意**不**重算成时段包络 ——
// 时段包络只在时段增删改时重算。否则人手动把跨度拉宽之后立刻被覆盖,
// 跨度就永远等于实际投入,没法表达"计划做两小时、实际只做了一小时"。
func (s *Store) UpdateTask(id string, p TaskPatch) (*model.Task, error) {
	// 先把改动套到当前值上算出生效区间,校验通过再落库。
	// 先写后校验会把非法区间留在库里,要等下一次读出来才发现 ——
	// 那时已经无从判断是谁写坏的。
	cur, err := s.GetTask(id)
	if err != nil {
		return nil, err
	}
	start, end := cur.StartAt, cur.EndAt
	if p.StartAt != nil {
		start = *p.StartAt
	}
	if p.EndAt != nil {
		end = *p.EndAt
	}
	// 同上:相等 = 未定结束,只有反向才是错的
	if end.Before(start) {
		return nil, fmt.Errorf("结束时间不能早于开始时间")
	}

	sets := []string{"updated_at = ?"}
	args := []any{unix(nowFunc())}

	if p.Title != nil {
		if strings.TrimSpace(*p.Title) == "" {
			return nil, fmt.Errorf("标题不能为空")
		}
		sets = append(sets, "title = ?")
		args = append(args, *p.Title)
	}
	if p.CategoryID != nil {
		if *p.CategoryID == "" {
			sets = append(sets, "category_id = NULL")
		} else {
			sets = append(sets, "category_id = ?")
			args = append(args, *p.CategoryID)
		}
	}
	if p.Status != nil {
		if !p.Status.Valid() {
			return nil, fmt.Errorf("状态非法: %q", *p.Status)
		}
		sets = append(sets, "status = ?")
		args = append(args, string(*p.Status))
	}
	if p.StartAt != nil {
		sets = append(sets, "start_at = ?")
		args = append(args, unix(*p.StartAt))
	}
	if p.EndAt != nil {
		sets = append(sets, "end_at = ?")
		args = append(args, unix(*p.EndAt))
	}
	if p.Confirmed != nil {
		sets = append(sets, "confirmed = ?")
		args = append(args, boolInt(*p.Confirmed))
	}

	args = append(args, id)
	res, err := s.db.Exec(`UPDATE tasks SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("更新事项失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, fmt.Errorf("%w: %s", ErrTaskNotFound, id)
	}
	return s.GetTask(id)
}

// DeleteTask 删除事项,备注跟着级联删。
func (s *Store) DeleteTask(id string) error {
	res, err := s.db.Exec(`DELETE FROM tasks WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("删除事项失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: %s", ErrTaskNotFound, id)
	}
	return nil
}

// CurrentTasks 返回所有"进行中"的事项。
//
// 返回切片而不是单个:允许并行计时,同时推进两件事是常态。
func (s *Store) CurrentTasks() ([]model.Task, error) {
	return s.ListTasks(TaskFilter{Status: model.StatusDoing})
}

// ---------------------------------------------------------------- 备注

// AddNote 往事项上追加一条备注。
func (s *Store) AddNote(taskID, body string, source model.Source) (*model.Note, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return nil, fmt.Errorf("备注内容不能为空")
	}
	if source == "" {
		source = model.SourceUI
	}

	// 先确认事项存在,否则外键报错的信息对调用方没意义
	if _, err := s.GetTask(taskID); err != nil {
		return nil, err
	}

	n := model.Note{ID: newID(), TaskID: taskID, Body: body, Source: source, CreatedAt: nowFunc()}
	if _, err := s.db.Exec(
		`INSERT INTO notes (id, task_id, body, source, created_at) VALUES (?,?,?,?,?)`,
		n.ID, n.TaskID, n.Body, n.Source, unix(n.CreatedAt),
	); err != nil {
		return nil, fmt.Errorf("写入备注失败: %w", err)
	}
	return &n, nil
}

// NotesOf 返回某事项的全部备注,按时间升序。
//
// 同秒内的多条靠 rowid 决胜,不能用 id —— id 是随机十六进制,
// 拿它排序等于把同一秒里的备注顺序交给运气。Claude 连着补几条进展
// 正好落在这个窗口里,顺序错乱看着就是"备注乱了"。
func (s *Store) NotesOf(taskID string) ([]model.Note, error) {
	rows, err := s.db.Query(
		`SELECT id, task_id, body, source, created_at, updated_at FROM notes
		 WHERE task_id = ? ORDER BY created_at, rowid`, taskID)
	if err != nil {
		return nil, fmt.Errorf("查询备注失败: %w", err)
	}
	defer rows.Close()

	out := []model.Note{}
	for rows.Next() {
		var n model.Note
		var created int64
		var updated *int64
		if err := rows.Scan(&n.ID, &n.TaskID, &n.Body, &n.Source, &created, &updated); err != nil {
			return nil, fmt.Errorf("读取备注失败: %w", err)
		}
		n.CreatedAt = fromUnix(created)
		if updated != nil {
			t := fromUnix(*updated)
			n.UpdatedAt = &t
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// GetNote 取单条备注。
func (s *Store) GetNote(id string) (*model.Note, error) {
	var n model.Note
	var created int64
	var updated *int64
	err := s.db.QueryRow(
		`SELECT id, task_id, body, source, created_at, updated_at FROM notes WHERE id = ?`, id).
		Scan(&n.ID, &n.TaskID, &n.Body, &n.Source, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("备注不存在: %s", id)
	}
	if err != nil {
		return nil, fmt.Errorf("查询备注失败: %w", err)
	}
	n.CreatedAt = fromUnix(created)
	if updated != nil {
		t := fromUnix(*updated)
		n.UpdatedAt = &t
	}
	return &n, nil
}

// UpdateNote 改一条备注的正文。
//
// **只动 body 和 updated_at,CreatedAt 一概不动** —— 备注在时间线上的位置
// 由创建时刻决定,改内容不该让它跳位置。updated_at 是留痕:没有它的话,
// 一条被改过的备注和原始记录长得一模一样。
//
// 拒绝空内容,和 AddNote 一致。**不能**把"改成空"当成删除:删除有独立入口
// (要点那个 ×),手滑清空输入框不该丢掉一条记录。
func (s *Store) UpdateNote(id, body string) (*model.Note, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return nil, fmt.Errorf("备注内容不能为空")
	}

	res, err := s.db.Exec(
		`UPDATE notes SET body = ?, updated_at = ? WHERE id = ?`, body, unix(nowFunc()), id)
	if err != nil {
		return nil, fmt.Errorf("改写备注失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, fmt.Errorf("备注不存在: %s", id)
	}
	return s.GetNote(id)
}

// DeleteNote 删掉一条备注。
func (s *Store) DeleteNote(id string) error {
	res, err := s.db.Exec(`DELETE FROM notes WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("删除备注失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("备注不存在: %s", id)
	}
	return nil
}

// ---------------------------------------------------------------- 内部

// attachCategory 按 CategoryID 填充 Category 字段。
func (s *Store) attachCategory(t *model.Task) error {
	if t.CategoryID == "" {
		return nil
	}
	c, err := s.GetCategory(t.CategoryID)
	if err != nil {
		return err
	}
	t.Category = c
	return nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// escapeLike 转义 LIKE 模式里的通配符,让用户输入的 % 和 _ 按字面匹配。
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}
