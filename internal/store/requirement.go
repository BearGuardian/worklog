package store

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"worklog/internal/model"
)

// ErrComponentExists 表示同名组件已存在。
var ErrComponentExists = errors.New("组件已存在")

// ErrRequirementNotFound 表示需求不存在。
var ErrRequirementNotFound = errors.New("需求不存在")

// NoComponent 是 RequirementFilter.Component 的哨兵值,表示"只看未指定组件的"。
//
// 用哨兵而不是空串,是因为空串在过滤条件里天然表示"不限",两个含义得分开。
const NoComponent = "__none__"

// RequirementFilter 是需求列表的查询条件。零值表示不限。
type RequirementFilter struct {
	Component string      // 组件 ID;NoComponent 表示未指定组件的
	Stage     model.Stage // 只看某个阶段
	Query     string      // 标题 / 内容关键词
	Limit     int
}

// RequirementPatch 是需求的局部更新。nil 字段表示不改。
//
// Stage 和 OnlineAt 不是独立的两个字段 —— 它们必须自洽,收敛规则见 resolveStage。
// 调用方只管把"想变成什么"传进来,不用记得同时改另一处。
type RequirementPatch struct {
	Title       *string
	ComponentID *string // 指向 "" 表示改为未指定组件
	Link        *string
	Detail      *string
	Stage       *model.Stage
	OnlineAt    *time.Time
}

// ---------------------------------------------------------------- 组件

// ListComponents 按 sort_order 返回全部组件。
func (s *Store) ListComponents() ([]model.Component, error) {
	rows, err := s.db.Query(
		`SELECT id, name, color, sort_order, created_at FROM components
		 ORDER BY sort_order, name`)
	if err != nil {
		return nil, fmt.Errorf("查询组件失败: %w", err)
	}
	defer rows.Close()

	out := []model.Component{}
	for rows.Next() {
		var c model.Component
		var created int64
		if err := rows.Scan(&c.ID, &c.Name, &c.Color, &c.SortOrder, &created); err != nil {
			return nil, fmt.Errorf("读取组件失败: %w", err)
		}
		c.CreatedAt = fromUnix(created)
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetComponent 按 ID 取组件。不存在时返回 (nil, nil)。
func (s *Store) GetComponent(id string) (*model.Component, error) {
	var c model.Component
	var created int64
	err := s.db.QueryRow(
		`SELECT id, name, color, sort_order, created_at FROM components WHERE id = ?`, id,
	).Scan(&c.ID, &c.Name, &c.Color, &c.SortOrder, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("查询组件失败: %w", err)
	}
	c.CreatedAt = fromUnix(created)
	return &c, nil
}

// ComponentByName 按名字取组件(忽略首尾空白)。不存在时返回 (nil, nil)。
func (s *Store) ComponentByName(name string) (*model.Component, error) {
	var c model.Component
	var created int64
	err := s.db.QueryRow(
		`SELECT id, name, color, sort_order, created_at FROM components WHERE name = ?`,
		strings.TrimSpace(name),
	).Scan(&c.ID, &c.Name, &c.Color, &c.SortOrder, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("查询组件失败: %w", err)
	}
	c.CreatedAt = fromUnix(created)
	return &c, nil
}

// CreateComponent 新建组件。color 为空时自动挑一个还没被用过的颜色。
func (s *Store) CreateComponent(name, color string) (*model.Component, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("组件名不能为空")
	}

	if exist, err := s.ComponentByName(name); err != nil {
		return nil, err
	} else if exist != nil {
		return nil, fmt.Errorf("%w: %s", ErrComponentExists, name)
	}

	if color == "" {
		c, err := s.nextColorFor("components")
		if err != nil {
			return nil, err
		}
		color = c
	}

	var maxOrder int
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(sort_order), -1) FROM components`).Scan(&maxOrder); err != nil {
		return nil, fmt.Errorf("读取组件顺序失败: %w", err)
	}

	c := model.Component{ID: newID(), Name: name, Color: color, SortOrder: maxOrder + 1}
	if _, err := s.db.Exec(
		`INSERT INTO components (id, name, color, sort_order, created_at) VALUES (?,?,?,?,?)`,
		c.ID, c.Name, c.Color, c.SortOrder, unix(nowFunc()),
	); err != nil {
		return nil, fmt.Errorf("新建组件失败: %w", err)
	}
	c.CreatedAt = nowFunc()
	return &c, nil
}

// EnsureComponent 按名字找组件,没有就建一个。
//
// 和 EnsureCategory 一样,是"输入不存在的名字就自动创建"的落点。
func (s *Store) EnsureComponent(name string) (*model.Component, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, nil // 未指定组件是合法状态,不是错误
	}
	if c, err := s.ComponentByName(name); err != nil {
		return nil, err
	} else if c != nil {
		return c, nil
	}
	c, err := s.CreateComponent(name, "")
	if err != nil {
		// 并发下可能刚好被别人建了,再查一次
		if errors.Is(err, ErrComponentExists) {
			return s.ComponentByName(name)
		}
		return nil, err
	}
	return c, nil
}

// UpdateComponent 改名字或颜色。空字符串表示不改。
func (s *Store) UpdateComponent(id, name, color string) error {
	name = strings.TrimSpace(name)

	if name != "" {
		if other, err := s.ComponentByName(name); err != nil {
			return err
		} else if other != nil && other.ID != id {
			return fmt.Errorf("%w: %s", ErrComponentExists, name)
		}
	}

	sets := []string{}
	args := []any{}
	if name != "" {
		sets = append(sets, "name = ?")
		args = append(args, name)
	}
	if color != "" {
		sets = append(sets, "color = ?")
		args = append(args, color)
	}
	if len(sets) == 0 {
		return nil
	}

	args = append(args, id)
	res, err := s.db.Exec(`UPDATE components SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...)
	if err != nil {
		return fmt.Errorf("更新组件失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("组件不存在: %s", id)
	}
	return nil
}

// DeleteComponent 删除组件。
//
// 需求**不会**跟着删,只是退回"未指定组件"(component_id 置空,靠外键的
// ON DELETE SET NULL)。整理组件不该弄丢需求。
func (s *Store) DeleteComponent(id string) error {
	res, err := s.db.Exec(`DELETE FROM components WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("删除组件失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("组件不存在: %s", id)
	}
	return nil
}

// ---------------------------------------------------------------- 需求

// requirementCols 是需求的列清单,查询和扫描共用一份,顺序必须一致。
const requirementCols = `id, title, component_id, link, detail, stage, start_at, online_at, source, created_at, updated_at`

// scanRequirement 按 requirementCols 的顺序读一行。
func scanRequirement(sc interface{ Scan(...any) error }) (model.Requirement, error) {
	var r model.Requirement
	var compID sql.NullString
	var stage, source string
	var start, created, updated int64
	var online sql.NullInt64

	if err := sc.Scan(&r.ID, &r.Title, &compID, &r.Link, &r.Detail, &stage,
		&start, &online, &source, &created, &updated); err != nil {
		return r, err
	}

	r.ComponentID = compID.String
	r.Stage = model.Stage(stage)
	r.Source = model.Source(source)
	r.StartAt = fromUnix(start)
	r.CreatedAt = fromUnix(created)
	r.UpdatedAt = fromUnix(updated)
	if online.Valid {
		t := fromUnix(online.Int64)
		r.OnlineAt = &t
	}
	return r, nil
}

// CreateRequirement 新建需求。
//
// start_at 就是**添加时间** —— 什么时候建的就是什么时候开始的,不接受调用方指定。
// 这是"添加需求的时间记录为需求开始时间"那条要求的落点:与其让每个调用方
// 都记得传对,不如根本不给这个参数。
func (s *Store) CreateRequirement(r model.Requirement) (*model.Requirement, error) {
	r.Title = strings.TrimSpace(r.Title)
	if r.Title == "" {
		return nil, fmt.Errorf("需求标题不能为空")
	}

	if r.Stage == "" {
		r.Stage = model.StageSubmit
	}
	if !r.Stage.Valid() {
		return nil, fmt.Errorf("需求阶段非法: %q", r.Stage)
	}
	if r.Source == "" {
		r.Source = model.SourceUI
	}

	now := nowFunc()
	r.ID = newID()
	r.StartAt = now
	r.CreatedAt = now
	r.UpdatedAt = now

	// 建的时候就要把 stage / online_at 收敛好 —— 直接建一条"已上线"的需求
	// 也得带上上线时间,不能等到第一次 update 才补
	r.Stage, r.OnlineAt = resolveStage(r.Stage, r.OnlineAt, nil, r.OnlineAt, now)

	var comp any
	if r.ComponentID != "" {
		comp = r.ComponentID
	}

	if _, err := s.db.Exec(
		`INSERT INTO requirements (id, title, component_id, link, detail, stage, start_at, online_at, source, created_at, updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.Title, comp, r.Link, r.Detail, string(r.Stage), unix(r.StartAt),
		nullUnix(r.OnlineAt), string(r.Source), unix(r.CreatedAt), unix(r.UpdatedAt),
	); err != nil {
		return nil, fmt.Errorf("新建需求失败: %w", err)
	}
	return s.GetRequirement(r.ID)
}

// GetRequirement 按 ID 取需求,带上组件。
func (s *Store) GetRequirement(id string) (*model.Requirement, error) {
	row := s.db.QueryRow(`SELECT `+requirementCols+` FROM requirements WHERE id = ?`, id)
	r, err := scanRequirement(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrRequirementNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("查询需求失败: %w", err)
	}
	if err := s.attachComponent(&r); err != nil {
		return nil, err
	}
	return &r, nil
}

// ListRequirements 按过滤条件返回需求。
//
// 排序:未上线的在前(按开始时间倒序),已上线的在后(按上线时间倒序)。
// 还在推的东西顶到眼前,已上线的是账、不是待办。
func (s *Store) ListRequirements(f RequirementFilter) ([]model.Requirement, error) {
	where := []string{}
	args := []any{}

	if f.Component == NoComponent {
		where = append(where, `component_id IS NULL`)
	} else if f.Component != "" {
		where = append(where, `component_id = ?`)
		args = append(args, f.Component)
	}
	if f.Stage != "" {
		where = append(where, `stage = ?`)
		args = append(args, string(f.Stage))
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		where = append(where, `(title LIKE ? ESCAPE '\' OR detail LIKE ? ESCAPE '\')`)
		pat := "%" + escapeLike(q) + "%"
		args = append(args, pat, pat)
	}

	sqlText := `SELECT ` + requirementCols + ` FROM requirements`
	if len(where) > 0 {
		sqlText += ` WHERE ` + strings.Join(where, " AND ")
	}
	sqlText += ` ORDER BY (online_at IS NOT NULL), COALESCE(online_at, start_at) DESC, rowid`

	if f.Limit > 0 {
		sqlText += ` LIMIT ?`
		args = append(args, f.Limit)
	}

	rows, err := s.db.Query(sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("查询需求失败: %w", err)
	}
	// 单连接:必须先把 rows 读完关掉,后面 attachComponent 才能再发查询
	out := []model.Requirement{}
	for rows.Next() {
		r, err := scanRequirement(rows)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("读取需求失败: %w", err)
		}
		out = append(out, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}

	if err := s.attachComponents(out); err != nil {
		return nil, err
	}
	return out, nil
}

// UpdateRequirement 局部更新需求,并维持 stage / online_at 的自洽。
func (s *Store) UpdateRequirement(id string, p RequirementPatch) (*model.Requirement, error) {
	cur, err := s.GetRequirement(id)
	if err != nil {
		return nil, err
	}

	title := cur.Title
	if p.Title != nil {
		title = strings.TrimSpace(*p.Title)
		if title == "" {
			return nil, fmt.Errorf("需求标题不能为空")
		}
	}
	compID := cur.ComponentID
	if p.ComponentID != nil {
		compID = *p.ComponentID
	}
	link, detail := cur.Link, cur.Detail
	if p.Link != nil {
		link = *p.Link
	}
	if p.Detail != nil {
		detail = *p.Detail
	}

	// 唯一的不变量在这里收敛,别在调用方各写一遍
	stage, online := resolveStage(cur.Stage, cur.OnlineAt, p.Stage, p.OnlineAt, nowFunc())

	var comp any
	if compID != "" {
		comp = compID
	}

	res, err := s.db.Exec(
		`UPDATE requirements
		 SET title = ?, component_id = ?, link = ?, detail = ?, stage = ?, online_at = ?, updated_at = ?
		 WHERE id = ?`,
		title, comp, link, detail, string(stage), nullUnix(online), unix(nowFunc()), id,
	)
	if err != nil {
		return nil, fmt.Errorf("更新需求失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrRequirementNotFound
	}
	return s.GetRequirement(id)
}

// DeleteRequirement 删除需求。
func (s *Store) DeleteRequirement(id string) error {
	res, err := s.db.Exec(`DELETE FROM requirements WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("删除需求失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrRequirementNotFound
	}
	return nil
}

// resolveStage 把 (阶段, 上线时间) 收敛到唯一自洽的组合。
//
// 这是整个需求模块唯一的不变量:stage == online ⟺ online_at 有值。
// 两处能各自改就会漂,所以只在 UpdateRequirement / CreateRequirement 里各调一次。
//
//	显式给了上线时间  → 阶段一并置成 online(给了日期就等于说它上线了)
//	阶段是 online     → 没日期就记成现在;已有日期则保留,重复标记不该改历史
//	其余              → 清空上线时间
//
// 最后一条是有意的:从 online 退回到验收,它确实不再算上线了,留着旧日期
// 会让季度统计撒谎。想再上线就再标一次,重新记日期。
func resolveStage(curStage model.Stage, curOnline *time.Time,
	pStage *model.Stage, pOnline *time.Time, now time.Time) (model.Stage, *time.Time) {

	stage := curStage
	if pStage != nil {
		stage = *pStage
	}
	online := curOnline
	if pOnline != nil {
		online = pOnline
	}

	switch {
	case pOnline != nil:
		return model.StageOnline, online
	case stage == model.StageOnline:
		if online == nil {
			t := now
			online = &t
		}
		return model.StageOnline, online
	default:
		return stage, nil
	}
}

// ---------------------------------------------------------------- 统计

// ComponentCount 是一个组件下的需求完成情况。
type ComponentCount struct {
	ComponentID string
	Name        string
	Color       string
	Total       int
	Online      int
}

// Percent 返回完成度百分比(0–100)。没有需求时是 0,不是 NaN。
func (c ComponentCount) Percent() float64 {
	if c.Total == 0 {
		return 0
	}
	return float64(c.Online) * 100 / float64(c.Total)
}

// QuarterCount 是一个季度里上线的需求数。
type QuarterCount struct {
	Year    int
	Quarter int
	Online  int
}

// RequirementStats 是需求的完成情况汇总。
//
// 一次全表扫描后在 Go 里折,和 ReportBetween 一个路子 —— 需求是几十条的规模,
// 而下推到 SQL 里拼月份区间反而比用 time.Time 算季度更容易出错。
type RequirementStats struct {
	Total      int
	Online     int
	Components []ComponentCount
	Quarters   []QuarterCount
}

// RequirementStatsOf 统计需求的完成情况。
func (s *Store) RequirementStatsOf() (*RequirementStats, error) {
	comps, err := s.ListComponents()
	if err != nil {
		return nil, err
	}
	reqs, err := s.ListRequirements(RequirementFilter{})
	if err != nil {
		return nil, err
	}

	byComp := map[string]*ComponentCount{}
	for _, c := range comps {
		byComp[c.ID] = &ComponentCount{ComponentID: c.ID, Name: c.Name, Color: c.Color}
	}

	st := &RequirementStats{Total: len(reqs)}
	// 未指定组件的那一组排在最后,单独占一格 —— 否则它们会在看板上凭空消失
	none := &ComponentCount{Name: "未指定组件"}

	quarters := map[[2]int]int{}
	for _, r := range reqs {
		g := byComp[r.ComponentID]
		if g == nil {
			g = none
		}
		g.Total++
		if r.Online() && r.OnlineAt != nil {
			g.Online++
			st.Online++
			q := [2]int{r.OnlineAt.Year(), model.Quarter(*r.OnlineAt)}
			quarters[q]++
		}
	}

	// 按组件的创建顺序排,和界面上维护列表的顺序一致
	for _, c := range comps {
		st.Components = append(st.Components, *byComp[c.ID])
	}
	if none.Total > 0 {
		st.Components = append(st.Components, *none)
	}

	for q, n := range quarters {
		st.Quarters = append(st.Quarters, QuarterCount{Year: q[0], Quarter: q[1], Online: n})
	}
	// 最近的季度在前
	sort.Slice(st.Quarters, func(i, j int) bool {
		if st.Quarters[i].Year != st.Quarters[j].Year {
			return st.Quarters[i].Year > st.Quarters[j].Year
		}
		return st.Quarters[i].Quarter > st.Quarters[j].Quarter
	})

	return st, nil
}

// ---------------------------------------------------------------- 内部

// attachComponent 给一条需求补上组件。
func (s *Store) attachComponent(r *model.Requirement) error {
	if r.ComponentID == "" {
		return nil
	}
	c, err := s.GetComponent(r.ComponentID)
	if err != nil {
		return err
	}
	r.Component = c
	return nil
}

// attachComponents 给一批需求补上组件。
//
// 一次查全部组件再按 ID 分发,不是每条需求查一次 —— 需求再多也就几十条,
// 但每条一次查询是几十次往返,没必要。
func (s *Store) attachComponents(reqs []model.Requirement) error {
	if len(reqs) == 0 {
		return nil
	}
	comps, err := s.ListComponents()
	if err != nil {
		return err
	}
	byID := make(map[string]*model.Component, len(comps))
	for i := range comps {
		byID[comps[i].ID] = &comps[i]
	}
	for i := range reqs {
		reqs[i].Component = byID[reqs[i].ComponentID] // 未指定组件时是 nil
	}
	return nil
}

// nullUnix 把可空时刻写成可空整数。
func nullUnix(t *time.Time) any {
	if t == nil {
		return nil
	}
	return unix(*t)
}
