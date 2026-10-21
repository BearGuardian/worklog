package mcpserver

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"worklog/internal/model"
	"worklog/internal/store"
)

// ---------------------------------------------------------------- 出入参

// TaskOut 是事项对外的表示。时间拆成 date / start / end 三个字段,
// 比丢一个 RFC3339 时间戳更好读,也少一次时区换算的心智负担。
//
// Start / End 是这条事项的**跨度**;真正投入的时间在 Spans 里。
// 一条事项可以中途暂停、下午接着做,跨度覆盖整个过程,时长只算做过的那些段。
type TaskOut struct {
	ID          string    `json:"id" jsonschema:"事项 ID"`
	Title       string    `json:"title" jsonschema:"标题"`
	Category    string    `json:"category,omitempty" jsonschema:"分类名"`
	Status      string    `json:"status" jsonschema:"todo/doing/done"`
	Running     bool      `json:"running" jsonschema:"此刻是否有时段在跑。status 是 doing 但它是 false,就是已暂停"`
	Date        string    `json:"date" jsonschema:"日期 YYYY-MM-DD"`
	Start       string    `json:"start" jsonschema:"跨度开始 HH:MM"`
	End         string    `json:"end" jsonschema:"跨度结束 HH:MM"`
	DurationMin int       `json:"duration_min" jsonschema:"实际投入时长(分钟),即各时段之和"`
	Confirmed   bool      `json:"confirmed" jsonschema:"时长是否经人确认"`
	Spans       []SpanOut `json:"spans,omitempty" jsonschema:"实际投入的时段。end 为空表示这一段还在跑"`
	Notes       []NoteOut `json:"notes,omitempty" jsonschema:"备注时间线"`
}

// SpanOut 是一段实际投入的时间。
type SpanOut struct {
	ID      string `json:"id" jsonschema:"时段 ID"`
	Start   string `json:"start" jsonschema:"开始 HH:MM"`
	End     string `json:"end" jsonschema:"结束 HH:MM;空表示还在跑"`
	Minutes int    `json:"minutes" jsonschema:"这一段的时长(分钟)"`
}

// NoteOut 是一条备注。
//
// ID 是给 update_note / delete_note 指名用的 —— 没有它 Claude 没法说"改哪一条"。
type NoteOut struct {
	ID     string `json:"id" jsonschema:"备注 ID,改/删时用它指名"`
	Who    string `json:"who" jsonschema:"me 或 claude"`
	At     string `json:"at" jsonschema:"HH:MM"`
	Text   string `json:"text" jsonschema:"内容"`
	Edited bool   `json:"edited,omitempty" jsonschema:"改过没有"`
}

func toSpanOut(sp model.Span, now time.Time) SpanOut {
	o := SpanOut{ID: sp.ID, Start: sp.StartAt.Format("15:04")}
	if sp.EndAt != nil {
		o.End = sp.EndAt.Format("15:04")
	}
	o.Minutes = sp.Minutes(now)
	return o
}

func toTaskOut(t model.Task) TaskOut {
	now := time.Now()
	o := TaskOut{
		ID:          t.ID,
		Title:       t.Title,
		Status:      string(t.Status),
		Running:     t.Running(),
		Date:        t.StartAt.Format("2006-01-02"),
		Start:       t.StartAt.Format("15:04"),
		End:         spanEndText(t),
		DurationMin: t.Minutes(now),
		Confirmed:   t.Confirmed,
	}
	if t.Category != nil {
		o.Category = t.Category.Name
	}
	for _, sp := range t.Spans {
		o.Spans = append(o.Spans, toSpanOut(sp, now))
	}
	for _, n := range t.Notes {
		o.Notes = append(o.Notes, toNoteOut(n))
	}
	return o
}

// spanEndText 把跨度的结束时刻写成 HH:MM;**未定结束时返回空串**。
//
// 未定结束存的是零长区间(end == start),直接格式化会输出自相矛盾的
// "09:00–09:00",看着像 bug。空串才如实表达"没有结束时间"。
func spanEndText(t model.Task) string {
	if !t.EndAt.After(t.StartAt) {
		return ""
	}
	return t.EndAt.Format("15:04")
}

// toNoteOut 把一条备注转成给模型看的形态。Who 由 source 推出来 ——// Claude 自己写的那几条要能认出来,不然它会把自己的记录当成用户说的。
func toNoteOut(n model.Note) NoteOut {
	who := "me"
	if n.Source == model.SourceMCP {
		who = "claude"
	}
	return NoteOut{
		ID: n.ID, Who: who, At: n.CreatedAt.Format("15:04"), Text: n.Body,
		Edited: n.UpdatedAt != nil,
	}
}

func toTaskOuts(ts []model.Task) []TaskOut {
	out := make([]TaskOut, 0, len(ts))
	for _, t := range ts {
		out = append(out, toTaskOut(t))
	}
	return out
}

// say 构造一段人类可读的返回文本。结构化数据在 structuredContent 里,
// 这段文字是给模型当"摘要"用的,不承载唯一信息。
func say(format string, a ...any) []mcp.Content {
	return []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(format, a...)}}
}

// ---------------------------------------------------------------- 时间解析

// parseDay 把日期描述解析成当天零点(本地时区)。
//
// 接受空串(今天)、today/yesterday/tomorrow、今天/昨天/明天、以及 YYYY-MM-DD。
// 模型给的日期形式不固定,这里放宽一点比让它反复试错划算。
func parseDay(s string) (time.Time, error) {
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)

	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "today", "今天":
		return today, nil
	case "yesterday", "昨天":
		return today.AddDate(0, 0, -1), nil
	case "tomorrow", "明天":
		return today.AddDate(0, 0, 1), nil
	}

	d, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(s), time.Local)
	if err != nil {
		return time.Time{}, fmt.Errorf("日期格式无法识别: %q,请用 YYYY-MM-DD 或 today/yesterday", s)
	}
	return d, nil
}

// parseClock 把 "09:30" 解析成当天的分钟偏移。
func parseClock(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	for _, layout := range []string{"15:04", "15:04:05", "3:04pm", "3:04 pm"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.Hour()*60 + t.Minute(), nil
		}
	}
	return 0, fmt.Errorf("时间格式无法识别: %q,请用 HH:MM", s)
}

// atClock 把某天的 HH:MM 组合成时间。
func atClock(day time.Time, minutes int) time.Time {
	return day.Add(time.Duration(minutes) * time.Minute)
}

// ---------------------------------------------------------------- 写操作

type LogTaskIn struct {
	Title       string `json:"title" jsonschema:"事项标题,简洁描述做了什么。必填"`
	DurationMin int    `json:"duration_min" jsonschema:"耗时(分钟)。必填。这是实际花掉的时间,不是区间长度"`
	Category    string `json:"category,omitempty" jsonschema:"分类名。不存在会自动创建"`
	Date        string `json:"date,omitempty" jsonschema:"日期 YYYY-MM-DD,默认今天"`
	Start       string `json:"start,omitempty" jsonschema:"开始时间 HH:MM。不填则排在当天最后一条之后"`
	Note        string `json:"note,omitempty" jsonschema:"可选备注,记进备注时间线"`
}

// logTask 一步补记一条已完成事项。这是 Claude Code 最常用的入口。
func (s *Server) logTask(ctx context.Context, _ *mcp.CallToolRequest, in LogTaskIn) (*mcp.CallToolResult, TaskOut, error) {
	var zero TaskOut

	if strings.TrimSpace(in.Title) == "" {
		return nil, zero, fmt.Errorf("title 不能为空")
	}
	if in.DurationMin <= 0 {
		return nil, zero, fmt.Errorf("duration_min 必须大于 0")
	}

	day, err := parseDay(in.Date)
	if err != nil {
		return nil, zero, err
	}

	startMin, err := parseClock(in.Start)
	if err != nil {
		return nil, zero, err
	}
	// 没给开始时间就排在当天最后一条之后 —— 补记时人通常不记得具体几点开始
	if strings.TrimSpace(in.Start) == "" {
		startMin, err = s.autoStart(day)
		if err != nil {
			return nil, zero, err
		}
	}

	start := atClock(day, startMin)
	end := start.Add(time.Duration(in.DurationMin) * time.Minute)

	cat, err := s.store.EnsureCategory(in.Category)
	if err != nil {
		return nil, zero, err
	}

	t, err := s.store.CreateTask(model.Task{
		Title:      strings.TrimSpace(in.Title),
		StartAt:    start,
		EndAt:      end,
		Status:     model.StatusDone,
		Confirmed:  true, // 时长是显式给的,不需要再确认
		Source:     model.SourceMCP,
		CategoryID: catID(cat),
	})
	if err != nil {
		return nil, zero, err
	}

	if n := strings.TrimSpace(in.Note); n != "" {
		if _, err := s.store.AddNote(t.ID, n, model.SourceMCP); err != nil {
			return nil, zero, err
		}
		if t, err = s.store.GetTask(t.ID); err != nil {
			return nil, zero, err
		}
	}

	s.notify()
	out := toTaskOut(*t)
	return &mcp.CallToolResult{
		Content: say("已记录:%s · %s · %s %s–%s",
			out.Title, durText(out.DurationMin), out.Date, out.Start, out.End),
	}, out, nil
}

// autoStart 返回当天可以接着排的分钟偏移:当天最后一个时段的结束时间。
//
// 按时段算而不是按跨度 —— 跨度可能被拉得很宽(比如把一条事项的范围
// 手动改到晚上),补记时接在那后面会排到很晚。没有时段(全是待办)时
// 回退到跨度的最大结束,再没有就用 09:00。
func (s *Server) autoStart(day time.Time) (int, error) {
	from, to := store.DayRange(day)
	existing, err := s.store.ListTasks(store.TaskFilter{From: &from, To: &to})
	if err != nil {
		return 0, err
	}
	if len(existing) == 0 {
		return 9 * 60, nil
	}

	var last time.Time
	for _, t := range existing {
		for _, sp := range t.Spans {
			end := sp.StartAt
			if sp.EndAt != nil {
				end = *sp.EndAt
			}
			if end.After(last) {
				last = end
			}
		}
	}
	if last.IsZero() {
		// 当天还没有任何时段,用跨度的最大结束兜底
		for _, t := range existing {
			if t.EndAt.After(last) {
				last = t.EndAt
			}
		}
	}
	return last.Hour()*60 + last.Minute(), nil
}

type CreateTaskIn struct {
	Title    string `json:"title" jsonschema:"标题。必填"`
	Category string `json:"category,omitempty" jsonschema:"分类名,不存在会自动创建"`
	Status   string `json:"status,omitempty" jsonschema:"todo/doing/done,默认 todo"`
	Date     string `json:"date,omitempty" jsonschema:"日期 YYYY-MM-DD,默认今天"`
	Start    string `json:"start,omitempty" jsonschema:"开始 HH:MM,默认 09:00"`
	End      string `json:"end,omitempty" jsonschema:"结束 HH:MM。不传 = 未定结束(跨度只有起点)"`
}

func (s *Server) createTask(ctx context.Context, _ *mcp.CallToolRequest, in CreateTaskIn) (*mcp.CallToolResult, TaskOut, error) {
	var zero TaskOut

	if strings.TrimSpace(in.Title) == "" {
		return nil, zero, fmt.Errorf("title 不能为空")
	}
	day, err := parseDay(in.Date)
	if err != nil {
		return nil, zero, err
	}
	startMin, err := parseClock(in.Start)
	if err != nil {
		return nil, zero, err
	}
	if strings.TrimSpace(in.Start) == "" {
		startMin = 9 * 60
	}
	endMin := startMin
	if strings.TrimSpace(in.End) != "" {
		if endMin, err = parseClock(in.End); err != nil {
			return nil, zero, err
		}
		if endMin < startMin {
			return nil, zero, fmt.Errorf("结束时间不能早于开始时间")
		}
	}

	status := model.StatusTodo
	if in.Status != "" {
		status = model.Status(strings.TrimSpace(in.Status))
		if !status.Valid() {
			return nil, zero, fmt.Errorf("status 只能是 todo / doing / done,收到 %q", in.Status)
		}
	}

	cat, err := s.store.EnsureCategory(in.Category)
	if err != nil {
		return nil, zero, err
	}

	t, err := s.store.CreateTask(model.Task{
		Title:      strings.TrimSpace(in.Title),
		StartAt:    atClock(day, startMin),
		EndAt:      atClock(day, endMin),
		Status:     status,
		Source:     model.SourceMCP,
		CategoryID: catID(cat),
	})
	if err != nil {
		return nil, zero, err
	}

	s.notify()
	out := toTaskOut(*t)
	return &mcp.CallToolResult{
		Content: say("已创建:%s · %s %s–%s · %s", out.Title, out.Date, out.Start, out.End, out.Status),
	}, out, nil
}

type StartTaskIn struct {
	ID string `json:"id" jsonschema:"事项 ID"`
}

// startTask 开始计时:开一段时段,并把状态置为进行中。
//
// 已经在跑就原样返回 —— 重复调用是安全的,调用方不用先查再调。
func (s *Server) startTask(ctx context.Context, _ *mcp.CallToolRequest, in StartTaskIn) (*mcp.CallToolResult, TaskOut, error) {
	var zero TaskOut

	cur, err := s.store.GetTask(in.ID)
	if err != nil {
		return nil, zero, err
	}

	if !cur.Running() {
		if _, err := s.store.AddSpan(in.ID, time.Now(), nil); err != nil {
			return nil, zero, err
		}
	}

	doing := model.StatusDoing
	t, err := s.store.UpdateTask(in.ID, store.TaskPatch{Status: &doing})
	if err != nil {
		return nil, zero, err
	}

	s.notify()
	out := toTaskOut(*t)
	return &mcp.CallToolResult{
		Content: say("已开始:%s(%s 起)", out.Title, out.Start),
	}, out, nil
}

type PauseTaskIn struct {
	ID string `json:"id" jsonschema:"事项 ID"`
}

// pauseTask 暂停计时:关掉正在跑的那一段,状态仍是进行中。
//
// 状态不变是关键 —— 暂停是计时的事,不是生命周期的事。界面上
// "进行中且没有时段在跑"就显示成已暂停。
func (s *Server) pauseTask(ctx context.Context, _ *mcp.CallToolRequest, in PauseTaskIn) (*mcp.CallToolResult, TaskOut, error) {
	var zero TaskOut

	closed, err := s.store.CloseOpenSpan(in.ID, time.Now())
	if err != nil {
		return nil, zero, err
	}
	t, err := s.store.GetTask(in.ID)
	if err != nil {
		return nil, zero, err
	}

	s.notify()
	out := toTaskOut(*t)
	text := "已暂停:" + out.Title
	if !closed {
		text = "本来就没有在计时:" + out.Title
	}
	return &mcp.CallToolResult{
		Content: say("%s · 累计 %s", text, durText(out.DurationMin)),
	}, out, nil
}

type FinishTaskIn struct {
	ID          string `json:"id" jsonschema:"事项 ID"`
	DurationMin int    `json:"duration_min,omitempty" jsonschema:"实际耗时(分钟)。给了就用它定这一段的结束;不给则按当前时间算"`
	Note        string `json:"note,omitempty" jsonschema:"结束时的备注"`
}

// finishTask 结束事项:关掉正在跑的时段,状态置为已完成。
//
// 时长优先用调用方显式给的值 —— Claude 干活的时间不等于任务时间,
// 中间人可能去开会了,按时间差算会严重偏大。给的值作用在**正在跑的
// 那一段**上(它就是这次投入),之前暂停过的段不动。
func (s *Server) finishTask(ctx context.Context, _ *mcp.CallToolRequest, in FinishTaskIn) (*mcp.CallToolResult, TaskOut, error) {
	var zero TaskOut

	cur, err := s.store.GetTask(in.ID)
	if err != nil {
		return nil, zero, err
	}

	var openStart time.Time
	hasOpen := false
	for _, sp := range cur.Spans {
		if sp.Open() {
			openStart, hasOpen = sp.StartAt, true
			break
		}
	}

	switch {
	case hasOpen:
		end := time.Now()
		if in.DurationMin > 0 {
			end = openStart.Add(time.Duration(in.DurationMin) * time.Minute)
		}
		if !end.After(openStart) {
			end = openStart.Add(time.Minute)
		}
		if _, err := s.store.CloseOpenSpan(in.ID, end); err != nil {
			return nil, zero, err
		}

	case len(cur.Spans) == 0:
		// 从没计过时就直接结束(待办补记,或排在未来的事项):
		// 按跨度补一段,否则这条事项会以 0 时长结束,白记了
		start := cur.StartAt
		end := time.Now()
		if in.DurationMin > 0 {
			end = start.Add(time.Duration(in.DurationMin) * time.Minute)
		}
		if !end.After(start) {
			end = start.Add(time.Minute)
		}
		if _, err := s.store.AddSpan(in.ID, start, &end); err != nil {
			return nil, zero, err
		}

	default:
		// 有段但都不在跑(暂停中结束):累计时长就是既有的,不因"结束"而改变
	}

	done := model.StatusDone
	yes := true
	t, err := s.store.UpdateTask(in.ID, store.TaskPatch{Status: &done, Confirmed: &yes})
	if err != nil {
		return nil, zero, err
	}

	if n := strings.TrimSpace(in.Note); n != "" {
		if _, err := s.store.AddNote(t.ID, n, model.SourceMCP); err != nil {
			return nil, zero, err
		}
		t, err = s.store.GetTask(t.ID)
		if err != nil {
			return nil, zero, err
		}
	}

	s.notify()
	out := toTaskOut(*t)
	return &mcp.CallToolResult{
		Content: say("已结束:%s · 累计 %s", out.Title, durText(out.DurationMin)),
	}, out, nil
}

type AddSpanIn struct {
	TaskID string `json:"task_id" jsonschema:"事项 ID"`
	Start  string `json:"start" jsonschema:"这一段的开始 HH:MM"`
	End    string `json:"end" jsonschema:"这一段的结束 HH:MM"`
	Date   string `json:"date,omitempty" jsonschema:"日期 YYYY-MM-DD,默认今天"`
}

// addSpan 给事项补一段实际投入。
//
// 这是"一件事分几次做完"的入口:上午 10-11 做了一段,下午 15-16 接着做,
// 就补两段。时长是两段之和,中间停掉的四小时不计。
func (s *Server) addSpan(ctx context.Context, _ *mcp.CallToolRequest, in AddSpanIn) (*mcp.CallToolResult, TaskOut, error) {
	var zero TaskOut

	day, err := parseDay(in.Date)
	if err != nil {
		return nil, zero, err
	}
	sm, err := parseClock(in.Start)
	if err != nil {
		return nil, zero, err
	}
	em, err := parseClock(in.End)
	if err != nil {
		return nil, zero, err
	}
	if strings.TrimSpace(in.Start) == "" || strings.TrimSpace(in.End) == "" {
		return nil, zero, fmt.Errorf("start 和 end 都要给,补记时段得知道两头的时刻")
	}

	start, end := atClock(day, sm), atClock(day, em)
	if !end.After(start) {
		return nil, zero, fmt.Errorf("时段结束必须晚于开始: %s–%s", in.Start, in.End)
	}

	if _, err := s.store.AddSpan(in.TaskID, start, &end); err != nil {
		return nil, zero, err
	}

	// 补了实际投入就不再是待办了。不提升的话会出现"待办,但已经投入 2 小时"
	// 这种自相矛盾的状态 —— 界面上它既列在待办里,又计进本周总时长。
	// 已完成的不动:那是调用方明确标记过的结果,补记一段不该把它退回去。
	cur, err := s.store.GetTask(in.TaskID)
	if err != nil {
		return nil, zero, err
	}
	if cur.Status == model.StatusTodo {
		doing := model.StatusDoing
		if _, err := s.store.UpdateTask(in.TaskID, store.TaskPatch{Status: &doing}); err != nil {
			return nil, zero, err
		}
	}

	t, err := s.store.GetTask(in.TaskID)
	if err != nil {
		return nil, zero, err
	}

	s.notify()
	out := toTaskOut(*t)
	return &mcp.CallToolResult{
		Content: say("已补一段:%s · %s–%s · 累计 %s",
			out.Title, in.Start, in.End, durText(out.DurationMin)),
	}, out, nil
}

type DeleteSpanIn struct {
	SpanID string `json:"span_id" jsonschema:"时段 ID,从 get_task / list_tasks 的 spans 里取"`
}

// deleteSpan 删掉一段时段。记错了要能改。
func (s *Server) deleteSpan(ctx context.Context, _ *mcp.CallToolRequest, in DeleteSpanIn) (*mcp.CallToolResult, struct{}, error) {
	if err := s.store.DeleteSpan(in.SpanID); err != nil {
		return nil, struct{}{}, err
	}
	s.notify()
	return &mcp.CallToolResult{Content: say("已删除时段 %s", in.SpanID)}, struct{}{}, nil
}

type UpdateTaskIn struct {
	ID        string `json:"id" jsonschema:"事项 ID"`
	Title     string `json:"title,omitempty" jsonschema:"新标题"`
	Category  string `json:"category,omitempty" jsonschema:"新分类名,不存在会自动创建。填 \"-\" 表示改为未分类"`
	Status    string `json:"status,omitempty" jsonschema:"todo/doing/done"`
	Date      string `json:"date,omitempty" jsonschema:"新日期 YYYY-MM-DD"`
	Start     string `json:"start,omitempty" jsonschema:"新开始 HH:MM"`
	End       string `json:"end,omitempty" jsonschema:"新结束 HH:MM"`
	Confirmed *bool  `json:"confirmed,omitempty" jsonschema:"时长是否已确认"`
}

func (s *Server) updateTask(ctx context.Context, _ *mcp.CallToolRequest, in UpdateTaskIn) (*mcp.CallToolResult, TaskOut, error) {
	var zero TaskOut

	cur, err := s.store.GetTask(in.ID)
	if err != nil {
		return nil, zero, err
	}
	p := store.TaskPatch{Confirmed: in.Confirmed}

	if strings.TrimSpace(in.Title) != "" {
		p.Title = &in.Title
	}
	if strings.TrimSpace(in.Status) != "" {
		st := model.Status(strings.TrimSpace(in.Status))
		if !st.Valid() {
			return nil, zero, fmt.Errorf("status 只能是 todo / doing / done,收到 %q", in.Status)
		}
		p.Status = &st
	}
	if strings.TrimSpace(in.Category) != "" {
		if strings.TrimSpace(in.Category) == "-" {
			empty := ""
			p.CategoryID = &empty
		} else {
			cat, err := s.store.EnsureCategory(in.Category)
			if err != nil {
				return nil, zero, err
			}
			id := catID(cat)
			p.CategoryID = &id
		}
	}

	// 日期和时刻是分开给的,合起来算新区间时以"当前值"为基准补全另一端
	if strings.TrimSpace(in.Date) != "" || strings.TrimSpace(in.Start) != "" || strings.TrimSpace(in.End) != "" {
		day := time.Date(cur.StartAt.Year(), cur.StartAt.Month(), cur.StartAt.Day(), 0, 0, 0, 0, time.Local)
		if strings.TrimSpace(in.Date) != "" {
			if day, err = parseDay(in.Date); err != nil {
				return nil, zero, err
			}
		}
		// 现在就是"未定结束"(零长跨度)的事项,只改开始时刻时结束要**跟着挪**。
		// 不挪的话 endMin 还是旧起点的那分钟,会算出 14:00–09:00 这种反向区间
		// 直接报错 —— 而它的语义本来就是"起点在哪,没有终点"。
		openEnd := cur.EndAt.Equal(cur.StartAt)

		startMin := cur.StartAt.Hour()*60 + cur.StartAt.Minute()
		endMin := cur.EndAt.Hour()*60 + cur.EndAt.Minute()
		if strings.TrimSpace(in.Start) != "" {
			if startMin, err = parseClock(in.Start); err != nil {
				return nil, zero, err
			}
		}
		if strings.TrimSpace(in.End) != "" {
			if endMin, err = parseClock(in.End); err != nil {
				return nil, zero, err
			}
		} else if openEnd {
			endMin = startMin
		}
		ns, ne := atClock(day, startMin), atClock(day, endMin)
		p.StartAt, p.EndAt = &ns, &ne
	}

	t, err := s.store.UpdateTask(in.ID, p)
	if err != nil {
		return nil, zero, err
	}
	s.notify()
	out := toTaskOut(*t)
	return &mcp.CallToolResult{
		Content: say("已更新:%s · %s %s–%s · %s", out.Title, out.Date, out.Start, out.End, out.Status),
	}, out, nil
}

type AddNoteIn struct {
	TaskID string `json:"task_id" jsonschema:"事项 ID"`
	Text   string `json:"text" jsonschema:"备注内容"`
}

// addNote 往事项上追加一条进展。Claude 边做边记就靠这个。
func (s *Server) addNote(ctx context.Context, _ *mcp.CallToolRequest, in AddNoteIn) (*mcp.CallToolResult, NoteOut, error) {
	var zero NoteOut
	n, err := s.store.AddNote(in.TaskID, in.Text, model.SourceMCP)
	if err != nil {
		return nil, zero, err
	}
	s.notify()
	out := NoteOut{ID: n.ID, Who: "claude", At: n.CreatedAt.Format("15:04"), Text: n.Body}
	return &mcp.CallToolResult{Content: say("已记下:%s", out.Text)}, out, nil
}

type UpdateNoteIn struct {
	NoteID string `json:"note_id" jsonschema:"备注 ID,从 get_task 的 notes[].id 拿"`
	Text   string `json:"text" jsonschema:"新的备注内容,不能为空"`
}

// updateNote 改一条已经写下的备注。
//
// 创建时刻不动 —— 备注在时间线上的位置由它决定,改内容不该让它跳位置。
func (s *Server) updateNote(ctx context.Context, _ *mcp.CallToolRequest, in UpdateNoteIn) (*mcp.CallToolResult, NoteOut, error) {
	var zero NoteOut
	n, err := s.store.UpdateNote(in.NoteID, in.Text)
	if err != nil {
		return nil, zero, err
	}
	s.notify()
	out := toNoteOut(*n)
	return &mcp.CallToolResult{Content: say("已改成:%s", out.Text)}, out, nil
}

type DeleteNoteIn struct {
	NoteID string `json:"note_id" jsonschema:"备注 ID,从 get_task 的 notes[].id 拿"`
}

// deleteNote 删掉一条备注。
func (s *Server) deleteNote(ctx context.Context, _ *mcp.CallToolRequest, in DeleteNoteIn) (*mcp.CallToolResult, struct{}, error) {
	if err := s.store.DeleteNote(in.NoteID); err != nil {
		return nil, struct{}{}, err
	}
	s.notify()
	return &mcp.CallToolResult{Content: say("已删除备注 %s", in.NoteID)}, struct{}{}, nil
}

type DeleteTaskIn struct {
	ID string `json:"id" jsonschema:"事项 ID"`
}

func (s *Server) deleteTask(ctx context.Context, _ *mcp.CallToolRequest, in DeleteTaskIn) (*mcp.CallToolResult, struct{}, error) {
	if err := s.store.DeleteTask(in.ID); err != nil {
		return nil, struct{}{}, err
	}
	s.notify()
	return &mcp.CallToolResult{Content: say("已删除事项 %s", in.ID)}, struct{}{}, nil
}

type CreateCategoryIn struct {
	Name  string `json:"name" jsonschema:"分类名"`
	Color string `json:"color,omitempty" jsonschema:"颜色 #RRGGBB,不填自动分配"`
}

func (s *Server) createCategory(ctx context.Context, _ *mcp.CallToolRequest, in CreateCategoryIn) (*mcp.CallToolResult, CategoryOut, error) {
	var zero CategoryOut
	c, err := s.store.CreateCategory(in.Name, in.Color)
	if err != nil {
		return nil, zero, err
	}
	s.notify()
	out := CategoryOut{ID: c.ID, Name: c.Name, Color: c.Color}
	return &mcp.CallToolResult{Content: say("已创建分类:%s", out.Name)}, out, nil
}

// ---------------------------------------------------------------- 小工具

func catID(c *model.Category) string {
	if c == nil {
		return ""
	}
	return c.ID
}

// durText 把分钟数写成 "1h15m" 这样的短文本。
func durText(min int) string {
	if min < 60 {
		return fmt.Sprintf("%dm", min)
	}
	if min%60 == 0 {
		return fmt.Sprintf("%dh", min/60)
	}
	return fmt.Sprintf("%dh%02dm", min/60, min%60)
}
