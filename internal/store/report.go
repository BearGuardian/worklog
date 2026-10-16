package store

import (
	"sort"
	"time"

	"worklog/internal/model"
)

// ReportGroup 是报告里按分类聚合出的一组。
type ReportGroup struct {
	CategoryID string       `json:"categoryId,omitempty"`
	Name       string       `json:"name"`
	Color      string       `json:"color"`
	Minutes    int          `json:"minutes"`
	Percent    float64      `json:"percent"`
	TaskCount  int          `json:"taskCount"`
	Tasks      []model.Task `json:"tasks"`
}

// Report 是某段时间的结构化汇总。
//
// 只给数据,不生成文字 —— 周报的措辞交给模型去组织,换口径不用改代码。
type Report struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`

	// TotalMinutes 是各分类时长之和,占比以它为分母,所以占比之和恒为 100%。
	TotalMinutes int `json:"totalMinutes"`

	// UnionMinutes 是墙钟并集:同一时刻并行的多件事只算一次。
	// 允许并行计时后,求和会重复计算重叠部分,这个值才是"实际占用了多少时间"。
	UnionMinutes int `json:"unionMinutes"`

	// OverlapMinutes = TotalMinutes - UnionMinutes,即被重复计算的分钟数。
	// 大于 0 说明这周有并行推进的事项。
	OverlapMinutes int `json:"overlapMinutes"`

	TaskCount int           `json:"taskCount"`
	Groups    []ReportGroup `json:"groups"`
}

// ReportBetween 汇总 [from, to) 之间的实际投入,按分类分组。
//
// 口径是**按时段裁剪**:一段从周日 23:00 做到周一 01:00,两周各算各的那一半。
// 所以先查与区间相交的时段,再逐段裁到区间内。
//
// 返回的每条事项,它的 Spans 被替换成**裁剪后的那些段**。这样
// `task.Duration(now)` 恰好等于这条事项在本区间内的贡献,和分组合计对得上 ——
// 否则跨周的事项会显示全量时长,和它所在分组的数字自相矛盾。
//
// 没有时段的事项(待办)不进报表 —— 时长是实际投入,还没开始就没有投入。
func (s *Store) ReportBetween(from, to time.Time) (*Report, error) {
	hits, err := s.SpansBetween(from, to)
	if err != nil {
		return nil, err
	}

	rep := &Report{From: from, To: to}

	// 第一遍:把时段裁到区间内,按事项归拢
	type acc struct {
		task  model.Task
		spans []model.Span
		mins  int
	}
	byTask := map[string]*acc{}
	order := make([]string, 0, len(hits))
	spans := make([]timeSpan, 0, len(hits))

	for _, h := range hits {
		clipped := ClipSpan(h.Span, from, to, nowFunc())
		if clipped == nil {
			continue // 理论上不会发生(查询已经过滤过),留个兜底
		}
		// 逐段取整,和 model.RoundMinutes 同一个口径 ——
		// 先求总和再取整会让分组数字和组内事项的数字对不上
		mins := model.RoundMinutes(clipped.Duration())
		if mins <= 0 {
			continue
		}

		a, ok := byTask[h.Task.ID]
		if !ok {
			a = &acc{task: h.Task}
			byTask[h.Task.ID] = a
			order = append(order, h.Task.ID)
		}
		end := clipped.end
		a.spans = append(a.spans, model.Span{
			ID: h.Span.ID, TaskID: h.Task.ID,
			StartAt: clipped.start, EndAt: &end, CreatedAt: h.Span.CreatedAt,
		})
		a.mins += mins

		spans = append(spans, *clipped)
		rep.TotalMinutes += mins
	}

	// 第二遍:按分类归组
	byCat := map[string]*ReportGroup{}
	for _, id := range order {
		a := byTask[id]
		t := a.task
		t.Spans = a.spans

		key := t.CategoryID
		g, ok := byCat[key]
		if !ok {
			g = &ReportGroup{CategoryID: t.CategoryID, Name: "未分类", Color: "#7b8496"}
			if t.Category != nil {
				g.Name = t.Category.Name
				g.Color = t.Category.Color
			}
			byCat[key] = g
		}
		g.Minutes += a.mins
		g.Tasks = append(g.Tasks, t)
	}

	rep.UnionMinutes = unionMinutes(spans)
	rep.OverlapMinutes = rep.TotalMinutes - rep.UnionMinutes
	rep.TaskCount = len(order)

	groups := make([]ReportGroup, 0, len(byCat))
	for _, g := range byCat {
		g.TaskCount = len(g.Tasks)
		if rep.TotalMinutes > 0 {
			g.Percent = float64(g.Minutes) / float64(rep.TotalMinutes) * 100
		}
		sort.Slice(g.Tasks, func(i, j int) bool { return g.Tasks[i].StartAt.Before(g.Tasks[j].StartAt) })
		groups = append(groups, *g)
	}
	// 按耗时降序;同耗时按名字,保证结果稳定
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].Minutes != groups[j].Minutes {
			return groups[i].Minutes > groups[j].Minutes
		}
		return groups[i].Name < groups[j].Name
	})
	rep.Groups = groups

	return rep, nil
}

// ClipSpan 把一段时段裁剪到 [from, to),返回裁剪后的区间;完全落在外面则返回 nil。
//
// 还在跑的段(EndAt 为 nil)按 now 当作结束时间。周报和界面的跨天渲染共用它,
// 所以裁剪规则只有一处。
func ClipSpan(sp model.Span, from, to, now time.Time) *timeSpan {
	start, end := sp.StartAt, now
	if sp.EndAt != nil {
		end = *sp.EndAt
	}
	if start.Before(from) {
		start = from
	}
	if end.After(to) {
		end = to
	}
	if !end.After(start) {
		return nil
	}
	return &timeSpan{start: start, end: end}
}

type timeSpan struct {
	start, end time.Time
}

// Duration 返回区间的长度。
func (t timeSpan) Duration() time.Duration { return t.end.Sub(t.start) }

// unionMinutes 计算一组区间并集覆盖的分钟数。
//
// 并行计时会让区间重叠,直接求和会把重叠部分算两次 —— 这个函数算的是
// "这些事一共占用了多少墙钟时间"。
func unionMinutes(spans []timeSpan) int {
	if len(spans) == 0 {
		return 0
	}
	sorted := make([]timeSpan, len(spans))
	copy(sorted, spans)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].start.Before(sorted[j].start) })

	var total time.Duration
	cur := sorted[0]
	for _, sp := range sorted[1:] {
		if sp.start.After(cur.end) {
			total += cur.end.Sub(cur.start)
			cur = sp
			continue
		}
		if sp.end.After(cur.end) {
			cur.end = sp.end
		}
	}
	total += cur.end.Sub(cur.start)

	// 四舍五入到分钟:截断会让 59 秒的差显示成 0 分钟
	return model.RoundMinutes(total)
}

// WeekRange 返回 t 所在自然周的周一到下周一(本地时区)。
//
// 用 [周一 00:00, 下周一 00:00) 这样的左闭右开区间,跨周的事项不会被算两次。
func WeekRange(t time.Time) (from, to time.Time) {
	d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
	offset := (int(d.Weekday()) + 6) % 7 // 周一 = 0
	from = d.AddDate(0, 0, -offset)
	return from, from.AddDate(0, 0, 7)
}

// DayRange 返回 t 所在自然日的 [00:00, 次日 00:00)。
func DayRange(t time.Time) (from, to time.Time) {
	from = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
	return from, from.AddDate(0, 0, 1)
}
