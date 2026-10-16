package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"worklog/internal/mcpserver"
	"worklog/internal/model"
	"worklog/internal/store"
)

// App 是暴露给前端的 API 面。方法名会被 Wails 注入成 window.go.main.App.Xxx。
type App struct {
	ctx context.Context

	store *store.Store
	mcp   *mcpserver.Server
	addr  string
	token string

	mu sync.Mutex
}

func NewApp(st *store.Store, srv *mcpserver.Server, addr, token string) *App {
	return &App{store: st, mcp: srv, addr: addr, token: token}
}

func (a *App) startup(ctx context.Context) { a.ctx = ctx }

// ---------------------------------------------------------------- 视图类型

// TaskView 是事项给前端的形态。
//
// 时间同时给出字符串(HH:MM,直接显示)和分钟数(当天第几分钟,用来算纵向位置),
// 前端两边都要用,让它自己再解析一遍只会多一处出错的地方。
//
// Start/End 是**跨度**;实际投入在 Spans 里。一条事项可以中途暂停、下午接着做,
// 跨度覆盖整个过程,时长只算做过的那些段。
type TaskView struct {
	ID          string     `json:"id"`
	Title       string     `json:"title"`
	CategoryID  string     `json:"categoryId"`
	Status      string     `json:"status"`
	Running     bool       `json:"running"` // status 是 doing 但它是 false,就是已暂停
	Date        string     `json:"date"`    // YYYY-MM-DD
	Start       string     `json:"start"`
	End         string     `json:"end"`
	StartMin    int        `json:"startMin"`
	EndMin      int        `json:"endMin"`
	DurationMin int        `json:"durationMin"` // 实际投入,即各段之和
	Confirmed   bool       `json:"confirmed"`
	Spans       []SpanView `json:"spans"`
	Notes       []NoteView `json:"notes"`
}

// SpanView 是一段实际投入。
//
// EndMin 用 StartMin + 时长算,不直接取结束时刻的分钟数 —— 这样跨午夜的时段
// 也能得到一个单调区间(23:00–01:00 就是 1380→1500),前端不必再判跨天,
// 只要看 EndMin 有没有超过 1440 就知道要不要在次日也画一截。
type SpanView struct {
	ID       string `json:"id"`
	Date     string `json:"date"` // 这一段的开始日期
	Start    string `json:"start"`
	End      string `json:"end"` // 空 = 还在跑
	StartMin int    `json:"startMin"`
	EndMin   int    `json:"endMin"`
	Minutes  int    `json:"minutes"`
	Open     bool   `json:"open"`
}

type NoteView struct {
	ID   string `json:"id"`
	Who  string `json:"who"` // me | claude
	At   string `json:"at"`
	Text string `json:"text"`
}

type CategoryView struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color"`
	Count int    `json:"count"` // 被多少条事项使用
}

func toTaskView(t model.Task) TaskView {
	now := time.Now()
	v := TaskView{
		ID:          t.ID,
		Title:       t.Title,
		CategoryID:  t.CategoryID,
		Status:      string(t.Status),
		Running:     t.Running(),
		Date:        t.StartAt.Format("2006-01-02"),
		Start:       t.StartAt.Format("15:04"),
		End:         t.EndAt.Format("15:04"),
		StartMin:    t.StartAt.Hour()*60 + t.StartAt.Minute(),
		EndMin:      t.EndAt.Hour()*60 + t.EndAt.Minute(),
		DurationMin: t.Minutes(now),
		Confirmed:   t.Confirmed,
		Spans:       make([]SpanView, 0, len(t.Spans)),
	}

	// 跨度按时段重算,并让还在跑的段延伸到此刻 —— 库里存的是写入时的值,
	// 界面要的是"现在进行到哪了"。没有时段(待办)时才用计划跨度。
	if len(t.Spans) > 0 {
		lo, hi := math.MaxInt, math.MinInt
		for _, sp := range t.Spans {
			sm := sp.StartAt.Hour()*60 + sp.StartAt.Minute()
			mins := sp.Minutes(now)

			sv := SpanView{
				ID: sp.ID, Date: sp.StartAt.Format("2006-01-02"),
				Start: sp.StartAt.Format("15:04"), StartMin: sm, Minutes: mins,
			}
			if sp.EndAt != nil {
				sv.End = sp.EndAt.Format("15:04")
			} else {
				sv.Open = true
			}
			sv.EndMin = sm + mins
			v.Spans = append(v.Spans, sv)

			lo = min(lo, sm)
			hi = max(hi, sv.EndMin)
		}
		v.StartMin, v.EndMin = lo, hi
		v.Start = toHM(lo)
		v.End = toHM(hi)
	}

	for _, n := range t.Notes {
		who := "me"
		if n.Source == model.SourceMCP {
			who = "claude"
		}
		v.Notes = append(v.Notes, NoteView{
			ID: n.ID, Who: who, At: n.CreatedAt.Format("15:04"), Text: n.Body,
		})
	}
	return v
}

// toHM 把分钟数写成 HH:MM。超过一天的部分会绕回来(1500 → 01:00),
// 这是显示用的兜底,纵向位置另有 EndMin 负责。
func toHM(min int) string {
	return fmt.Sprintf("%02d:%02d", (min/60)%24, min%60)
}

// ---------------------------------------------------------------- 事项

// ListTasks 返回 [from, to] 闭区间内的事项。日期是 YYYY-MM-DD。
func (a *App) ListTasks(from, to string) ([]TaskView, error) {
	f, err := time.ParseInLocation("2006-01-02", from, time.Local)
	if err != nil {
		return nil, fmt.Errorf("起始日期格式不对: %q", from)
	}
	t, err := time.ParseInLocation("2006-01-02", to, time.Local)
	if err != nil {
		return nil, fmt.Errorf("结束日期格式不对: %q", to)
	}
	to2 := t.AddDate(0, 0, 1) // 闭区间 → 左闭右开

	tasks, err := a.store.ListTasks(store.TaskFilter{From: &f, To: &to2})
	if err != nil {
		return nil, err
	}

	out := make([]TaskView, 0, len(tasks))
	for _, task := range tasks {
		// 备注只在详情栏用,列表里带上会让每次刷新都多一批查询
		out = append(out, toTaskView(task))
	}
	return out, nil
}

// GetTask 取单个事项,含完整备注。
func (a *App) GetTask(id string) (*TaskView, error) {
	t, err := a.store.GetTask(id)
	if err != nil {
		return nil, err
	}
	v := toTaskView(*t)
	return &v, nil
}

// TaskInput 是新建/编辑事项的入参。ID 为空表示新建。
type TaskInput struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	CategoryID string `json:"categoryId"`
	Status     string `json:"status"`
	Date       string `json:"date"`
	Start      string `json:"start"` // HH:MM
	End        string `json:"end"`
}

// SaveTask 新建或更新事项。
//
// 弹窗管的是**跨度**(日期 + 开始 + 结束)。时段怎么处理分三种情况:
//
//	没有时段 → 只改跨度(待办:那本来就是计划时段)
//	只有 1 段 → 顺带把那段设成同区间。保住单区间的旧手感:改时间就是改那一段
//	≥ 2 段   → 只改跨度,段一律不动。否则会把用户一段段补出来的记录悄悄抹掉
//
// 时段本身的增删在详情栏里做(AddSpan / DeleteSpan)。
func (a *App) SaveTask(in TaskInput) (*TaskView, error) {
	title := strings.TrimSpace(in.Title)
	if title == "" {
		return nil, fmt.Errorf("标题不能为空")
	}

	day, err := time.ParseInLocation("2006-01-02", in.Date, time.Local)
	if err != nil {
		return nil, fmt.Errorf("日期格式不对: %q", in.Date)
	}
	sm, err := parseClock(in.Start)
	if err != nil {
		return nil, err
	}
	em, err := parseClock(in.End)
	if err != nil {
		return nil, err
	}
	if em <= sm {
		return nil, fmt.Errorf("结束时间必须晚于开始时间")
	}

	status := model.Status(strings.TrimSpace(in.Status))
	if status == "" {
		status = model.StatusTodo
	}
	if !status.Valid() {
		return nil, fmt.Errorf("状态非法: %q", in.Status)
	}

	start := day.Add(time.Duration(sm) * time.Minute)
	end := day.Add(time.Duration(em) * time.Minute)

	if in.ID == "" {
		t, err := a.store.CreateTask(model.Task{
			Title: title, CategoryID: in.CategoryID, Status: status,
			StartAt: start, EndAt: end, Source: model.SourceUI,
		})
		if err != nil {
			return nil, err
		}
		v := toTaskView(*t)
		return &v, nil
	}

	before, err := a.store.GetTask(in.ID)
	if err != nil {
		return nil, err
	}

	catID := in.CategoryID
	t, err := a.store.UpdateTask(in.ID, store.TaskPatch{
		Title: &title, CategoryID: &catID, Status: &status,
		StartAt: &start, EndAt: &end,
	})
	if err != nil {
		return nil, err
	}

	if len(before.Spans) == 1 {
		if err := a.store.ReplaceSpans(in.ID, start, end); err != nil {
			return nil, err
		}
		if t, err = a.store.GetTask(in.ID); err != nil {
			return nil, err
		}
	}

	v := toTaskView(*t)
	return &v, nil
}

// DeleteTask 删除事项。
func (a *App) DeleteTask(id string) error { return a.store.DeleteTask(id) }

// SetTaskStatus 改状态。切到 doing 时开一段时段 —— 声明了在做,时长就该开始走。
//
// 已经在跑就不重复开:重复点"继续"不该凭空多出一段。
func (a *App) SetTaskStatus(id, status string) (*TaskView, error) {
	st := model.Status(strings.TrimSpace(status))
	if !st.Valid() {
		return nil, fmt.Errorf("状态非法: %q", status)
	}

	cur, err := a.store.GetTask(id)
	if err != nil {
		return nil, err
	}
	if st == model.StatusDoing && !cur.Running() {
		if _, err := a.store.AddSpan(id, time.Now(), nil); err != nil {
			return nil, err
		}
	}

	t, err := a.store.UpdateTask(id, store.TaskPatch{Status: &st})
	if err != nil {
		return nil, err
	}
	v := toTaskView(*t)
	return &v, nil
}

// PauseTask 暂停计时:关掉正在跑的那一段,状态仍是进行中。
//
// 没有在跑的时候调用是安全的(比如连点两下),不会报错也不会多出时段。
func (a *App) PauseTask(id string) (*TaskView, error) {
	if _, err := a.store.CloseOpenSpan(id, time.Now()); err != nil {
		return nil, err
	}
	return a.GetTask(id)
}

// AddSpan 给事项补一段实际投入。日期 + 起止时刻。
func (a *App) AddSpan(taskID, date, start, end string) (*TaskView, error) {
	day, err := time.ParseInLocation("2006-01-02", date, time.Local)
	if err != nil {
		return nil, fmt.Errorf("日期格式不对: %q", date)
	}
	sm, err := parseClock(start)
	if err != nil {
		return nil, err
	}
	em, err := parseClock(end)
	if err != nil {
		return nil, err
	}
	if em <= sm {
		return nil, fmt.Errorf("结束时间必须晚于开始时间")
	}

	s := day.Add(time.Duration(sm) * time.Minute)
	e := day.Add(time.Duration(em) * time.Minute)
	if _, err := a.store.AddSpan(taskID, s, &e); err != nil {
		return nil, err
	}

	// 补了实际投入就不再是待办了(已完成的不动,那是明确标记过的结果)
	if cur, err := a.store.GetTask(taskID); err == nil && cur.Status == model.StatusTodo {
		doing := model.StatusDoing
		if _, err := a.store.UpdateTask(taskID, store.TaskPatch{Status: &doing}); err != nil {
			return nil, err
		}
	}

	return a.GetTask(taskID)
}

// DeleteSpan 删掉一段时段。界面删完自己重新拉数据,所以不返回事项。
func (a *App) DeleteSpan(spanID string) error { return a.store.DeleteSpan(spanID) }

// FinishTask 结束事项并确认耗时。
//
// durationMin 是人在确认框里改过的值;<= 0 表示沿用自动算出的时长。
// 给的值作用在**正在跑的那一段**上 —— 它就是这次投入,之前暂停过的段不动。
// 确认后写一条备注留痕:用户改过的时长要能看出来。
func (a *App) FinishTask(id string, durationMin int) (*TaskView, error) {
	cur, err := a.store.GetTask(id)
	if err != nil {
		return nil, err
	}

	var openStart time.Time
	hasOpen := false
	for _, sp := range cur.Spans {
		if sp.Open() {
			openStart, hasOpen = sp.StartAt, true
			break
		}
	}

	// 自动算出的时长要在改动之前取,否则拿到的是改完之后的值
	auto := 0
	if hasOpen {
		auto = int(time.Since(openStart).Minutes())
	}

	switch {
	case hasOpen:
		end := time.Now()
		if durationMin > 0 {
			end = openStart.Add(time.Duration(durationMin) * time.Minute)
		}
		if !end.After(openStart) {
			end = openStart.Add(time.Minute)
		}
		if _, err := a.store.CloseOpenSpan(id, end); err != nil {
			return nil, err
		}

	case len(cur.Spans) == 0:
		// 从没计过时就直接结束(待办或排在未来的事项):按跨度补一段,
		// 否则这条事项会以 0 时长结束,白记了
		start := cur.StartAt
		end := time.Now()
		if durationMin > 0 {
			end = start.Add(time.Duration(durationMin) * time.Minute)
		}
		if !end.After(start) {
			end = start.Add(time.Minute)
		}
		if _, err := a.store.AddSpan(id, start, &end); err != nil {
			return nil, err
		}

	default:
		// 有段但都不在跑(暂停中结束):累计时长就是既有的
	}

	done := model.StatusDone
	yes := true
	if _, err := a.store.UpdateTask(id, store.TaskPatch{Status: &done, Confirmed: &yes}); err != nil {
		return nil, err
	}

	if durationMin > 0 && durationMin != auto {
		if _, err := a.store.AddNote(id,
			fmt.Sprintf("确认耗时 %s(自动算出 %s)", durText(durationMin), durText(auto)),
			model.SourceUI); err != nil {
			return nil, err
		}
	}

	return a.GetTask(id)
}

// AddNote 追加一条备注。
func (a *App) AddNote(taskID, text string) (*TaskView, error) {
	if _, err := a.store.AddNote(taskID, text, model.SourceUI); err != nil {
		return nil, err
	}
	return a.GetTask(taskID)
}

// ---------------------------------------------------------------- 分类

// ListCategories 返回全部分类,附带使用计数。
func (a *App) ListCategories() ([]CategoryView, error) {
	cats, err := a.store.ListCategories()
	if err != nil {
		return nil, err
	}

	// 计数走一次全表扫描就够了 —— 分类不多,事项也就几千条
	usage := map[string]int{}
	all, err := a.store.ListTasks(store.TaskFilter{Limit: 100000})
	if err != nil {
		return nil, err
	}
	for _, t := range all {
		if t.CategoryID != "" {
			usage[t.CategoryID]++
		}
	}

	out := make([]CategoryView, 0, len(cats))
	for _, c := range cats {
		out = append(out, CategoryView{ID: c.ID, Name: c.Name, Color: c.Color, Count: usage[c.ID]})
	}
	return out, nil
}

// SaveCategory 新建(ID 为空)或更新分类。
func (a *App) SaveCategory(id, name, color string) (*CategoryView, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("分类名不能为空")
	}

	if id == "" {
		c, err := a.store.CreateCategory(name, color)
		if err != nil {
			return nil, err
		}
		return &CategoryView{ID: c.ID, Name: c.Name, Color: c.Color}, nil
	}

	if err := a.store.UpdateCategory(id, name, color); err != nil {
		return nil, err
	}
	c, err := a.store.GetCategory(id)
	if err != nil || c == nil {
		return nil, fmt.Errorf("分类不存在: %s", id)
	}
	return &CategoryView{ID: c.ID, Name: c.Name, Color: c.Color}, nil
}

// DeleteCategory 删除分类。事项会退回未分类,不会被一起删掉。
func (a *App) DeleteCategory(id string) error { return a.store.DeleteCategory(id) }

// ---------------------------------------------------------------- 汇总

type ReportView struct {
	From           string          `json:"from"`
	To             string          `json:"to"`
	TotalMinutes   int             `json:"totalMinutes"`
	UnionMinutes   int             `json:"unionMinutes"`
	OverlapMinutes int             `json:"overlapMinutes"`
	TaskCount      int             `json:"taskCount"`
	Groups         []ReportGroupIn `json:"groups"`
}

type ReportGroupIn struct {
	CategoryID string     `json:"categoryId"`
	Name       string     `json:"name"`
	Color      string     `json:"color"`
	Minutes    int        `json:"minutes"`
	Percent    float64    `json:"percent"`
	Tasks      []TaskView `json:"tasks"`
}

// Report 返回 [from, to] 闭区间的汇总。
func (a *App) Report(from, to string) (*ReportView, error) {
	f, err := time.ParseInLocation("2006-01-02", from, time.Local)
	if err != nil {
		return nil, fmt.Errorf("起始日期格式不对: %q", from)
	}
	t, err := time.ParseInLocation("2006-01-02", to, time.Local)
	if err != nil {
		return nil, fmt.Errorf("结束日期格式不对: %q", to)
	}

	rep, err := a.store.ReportBetween(f, t.AddDate(0, 0, 1))
	if err != nil {
		return nil, err
	}

	v := &ReportView{
		From:           rep.From.Format("2006-01-02"),
		To:             rep.To.AddDate(0, 0, -1).Format("2006-01-02"),
		TotalMinutes:   rep.TotalMinutes,
		UnionMinutes:   rep.UnionMinutes,
		OverlapMinutes: rep.OverlapMinutes,
		TaskCount:      rep.TaskCount,
		Groups:         make([]ReportGroupIn, 0, len(rep.Groups)),
	}
	for _, g := range rep.Groups {
		grp := ReportGroupIn{
			CategoryID: g.CategoryID, Name: g.Name, Color: g.Color,
			Minutes: g.Minutes, Percent: g.Percent,
			Tasks: make([]TaskView, 0, len(g.Tasks)),
		}
		for _, task := range g.Tasks {
			grp.Tasks = append(grp.Tasks, toTaskView(task))
		}
		v.Groups = append(v.Groups, grp)
	}
	return v, nil
}

// ---------------------------------------------------------------- MCP 接入

// MCPStatus 描述 Claude Code 那边的接入情况。
type MCPStatus struct {
	URL       string `json:"url"`       // 本机服务地址
	Token     string `json:"token"`     // 访问令牌,展示给用户复制
	Installed bool   `json:"installed"` // 用户级配置里已经有 worklog 了吗
	Matches   bool   `json:"matches"`   // 已配置的地址和令牌与当前一致吗
	CLI       bool   `json:"cli"`       // 本机有 claude 命令吗
	ConfigAt  string `json:"configAt"`  // 配置文件路径,方便用户自己看
}

func (a *App) MCPStatus() MCPStatus {
	st := MCPStatus{URL: mcpserver.URL(a.addr), Token: a.token}
	st.ConfigAt = claudeConfigPath()

	_, err := exec.LookPath("claude")
	st.CLI = err == nil

	servers, err := readUserMCPServers()
	if err != nil {
		return st
	}
	cfg, ok := servers["worklog"]
	if !ok {
		return st
	}
	st.Installed = true
	// 令牌也要比:数据目录被清过之后会重新生成令牌,这时地址没变但
	// Claude Code 那边拿的是旧的,连上去只会得到 401。只看地址会把
	// 这种情况误判成"已经接好了"。
	st.Matches = cfg.URL == st.URL && bearerOf(cfg.Headers) == st.Token
	return st
}

// bearerOf 从请求头里取出 Bearer 令牌。
func bearerOf(h map[string]string) string {
	for k, v := range h {
		if strings.EqualFold(k, "Authorization") {
			return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(v), "Bearer "))
		}
	}
	return ""
}

// InstallMCP 把 worklog 注册进 Claude Code。
//
// 走官方 CLI 而不是手改 ~/.claude.json —— 那是 Claude Code 的核心配置,
// 手改格式出错的代价太高,交给它自己写。
func (a *App) InstallMCP() (string, error) {
	if _, err := exec.LookPath("claude"); err != nil {
		return "", fmt.Errorf("没找到 claude 命令,请先安装 Claude Code,或手动执行下面的命令")
	}

	// 已经注册过但对不上(令牌变了)时先摘掉:claude mcp add 遇到重名只会
	// 回一句 already exists,不会覆盖。不摘的话用户点了"一键安装"看起来成功,
	// 实际还是连不上 —— 这比直接报错更难查。
	if st := a.MCPStatus(); st.Installed && !st.Matches {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		out, err := exec.CommandContext(ctx, "claude", "mcp", "remove", "worklog", "-s", "user").CombinedOutput()
		cancel()
		if err != nil {
			return strings.TrimSpace(string(out)),
				fmt.Errorf("旧的配置摘不掉: %v\n%s", err, strings.TrimSpace(string(out)))
		}
	}

	url := mcpserver.URL(a.addr)
	args := []string{
		"mcp", "add", "--transport", "http", "worklog", url,
		"-H", "Authorization: Bearer " + a.token,
		"-s", "user",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "claude", args...)
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		// 已经存在时 claude 会报错,这不算失败 —— 用户要的结果已经达成了
		if strings.Contains(text, "already exists") {
			return text, nil
		}
		return text, fmt.Errorf("注册失败: %v\n%s", err, text)
	}
	return text, nil
}

// MCPCommand 返回给用户手动执行的那条命令。
func (a *App) MCPCommand() string {
	return fmt.Sprintf("claude mcp add --transport http worklog %s -H \"Authorization: Bearer %s\" -s user",
		mcpserver.URL(a.addr), a.token)
}

// claudeConfigPath 返回 Claude Code 的用户级配置文件路径。
func claudeConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude.json")
}

type mcpServerConfig struct {
	Type    string            `json:"type"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
}

// readUserMCPServers 读取 ~/.claude.json 里的 mcpServers。
func readUserMCPServers() (map[string]mcpServerConfig, error) {
	path := claudeConfigPath()
	if path == "" {
		return nil, fmt.Errorf("无法定位用户目录")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var doc struct {
		MCPServers map[string]mcpServerConfig `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", path, err)
	}
	return doc.MCPServers, nil
}

// ---------------------------------------------------------------- 杂项

// DataChanged 由 MCP 侧在写入后回调,推一个事件让界面立刻重新取数。
//
// 界面自己也有 30 秒轮询和聚焦刷新兜底,所以这里丢了也不会一直不更新 ——
// 它的价值在于"Claude 刚记完就能看到",而不是唯一的刷新途径。
func (a *App) DataChanged() {
	if a.ctx == nil {
		return // 界面还没起来(-mcp-only 模式),没人在听
	}
	runtime.EventsEmit(a.ctx, "worklog:changed")
}

// OpenDataDir 在资源管理器里打开数据目录。
func (a *App) OpenDataDir() error {
	return exec.Command("explorer", a.store.Dir()).Start()
}

// ClientLog 接收前端的诊断日志。界面没有可见控制台,这是唯一的出口。
func (a *App) ClientLog(msg string) {
	logf("[前端] %s", msg)
}

// Quit 退出程序。
func (a *App) Quit() {
	if a.ctx != nil {
		runtime.Quit(a.ctx)
	}
}

func durText(min int) string {
	if min < 60 {
		return fmt.Sprintf("%dm", min)
	}
	if min%60 == 0 {
		return fmt.Sprintf("%dh", min/60)
	}
	return fmt.Sprintf("%dh%02dm", min/60, min%60)
}
