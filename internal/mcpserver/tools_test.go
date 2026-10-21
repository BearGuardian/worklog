package mcpserver

import (
	"context"
	"testing"
	"time"

	"worklog/internal/model"
	"worklog/internal/store"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return New(Options{Store: st, Token: "t"})
}

func TestParseDay(t *testing.T) {
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)

	cases := []struct {
		in   string
		want time.Time
	}{
		{"", today},
		{"today", today},
		{"今天", today},
		{"TODAY", today},
		{"yesterday", today.AddDate(0, 0, -1)},
		{"昨天", today.AddDate(0, 0, -1)},
		{"tomorrow", today.AddDate(0, 0, 1)},
		{"明天", today.AddDate(0, 0, 1)},
	}
	for _, c := range cases {
		got, err := parseDay(c.in)
		if err != nil {
			t.Errorf("parseDay(%q) 报错: %v", c.in, err)
			continue
		}
		if !got.Equal(c.want) {
			t.Errorf("parseDay(%q) = %v,期望 %v", c.in, got, c.want)
		}
	}

	// 显式日期
	got, err := parseDay("2026-03-05")
	if err != nil {
		t.Fatalf("显式日期应当解析成功: %v", err)
	}
	if got.Year() != 2026 || got.Month() != 3 || got.Day() != 5 {
		t.Errorf("显式日期解析错误: %v", got)
	}

	if _, err := parseDay("三月五号"); err == nil {
		t.Error("无法识别的日期应当报错,而不是静默用今天")
	}
}

func TestParseClock(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"00:00", 0},
		{"09:30", 9*60 + 30},
		{"23:59", 23*60 + 59},
		{"9:05", 9*60 + 5},
		{"14:00:00", 14 * 60},
	}
	for _, c := range cases {
		got, err := parseClock(c.in)
		if err != nil {
			t.Errorf("parseClock(%q) 报错: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseClock(%q) = %d,期望 %d", c.in, got, c.want)
		}
	}
	if _, err := parseClock("下午三点"); err == nil {
		t.Error("无法识别的时间应当报错")
	}
}

// 补记时通常不记得几点开始,应当自动接在当天最后一条之后
func TestLogTaskAutoPlacesAfterLast(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	if _, _, err := s.logTask(ctx, nil, LogTaskIn{
		Title: "第一件", DurationMin: 60, Date: "today", Start: "09:00",
	}); err != nil {
		t.Fatal(err)
	}

	// 不给 start —— 应当从 10:00 开始
	_, out, err := s.logTask(ctx, nil, LogTaskIn{Title: "第二件", DurationMin: 30, Date: "today"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Start != "10:00" || out.End != "10:30" {
		t.Errorf("应当接在 10:00–10:30,实际 %s–%s", out.Start, out.End)
	}
}

// 当天没有任何事项时,自动排位落在 09:00
func TestLogTaskAutoStartDefaultsTo9(t *testing.T) {
	s := newTestServer(t)
	_, out, err := s.logTask(context.Background(), nil, LogTaskIn{
		Title: "第一件", DurationMin: 45, Date: "today",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Start != "09:00" {
		t.Errorf("空的一天应当从 09:00 开始,实际 %s", out.Start)
	}
}

func TestLogTaskValidates(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	if _, _, err := s.logTask(ctx, nil, LogTaskIn{Title: "  ", DurationMin: 30}); err == nil {
		t.Error("空标题应当报错")
	}
	if _, _, err := s.logTask(ctx, nil, LogTaskIn{Title: "x", DurationMin: 0}); err == nil {
		t.Error("零耗时应当报错")
	}
	if _, _, err := s.logTask(ctx, nil, LogTaskIn{Title: "x", DurationMin: -5}); err == nil {
		t.Error("负耗时应当报错")
	}
}

// 补记的时长是显式给的,不该再被要求确认
func TestLogTaskMarksConfirmed(t *testing.T) {
	s := newTestServer(t)
	_, out, err := s.logTask(context.Background(), nil, LogTaskIn{
		Title: "一件事", DurationMin: 90, Date: "today",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Confirmed {
		t.Error("显式给出时长的事项应当标记为已确认")
	}
	if out.DurationMin != 90 {
		t.Errorf("时长应当是 90,实际 %d", out.DurationMin)
	}
}

// 分类不存在时自动创建 —— 记录的动作不该被打断
func TestLogTaskCreatesCategory(t *testing.T) {
	s := newTestServer(t)

	if _, _, err := s.logTask(context.Background(), nil, LogTaskIn{
		Title: "一件事", DurationMin: 30, Date: "today", Category: "新分类",
	}); err != nil {
		t.Fatal(err)
	}

	c, err := s.store.CategoryByName("新分类")
	if err != nil {
		t.Fatal(err)
	}
	if c == nil {
		t.Fatal("分类应当被自动创建")
	}
	if c.Color == "" {
		t.Error("自动创建的分类应当有颜色")
	}
}

// finish_task 必须优先用显式给的时长,而不是按时间差算 ——
// Claude 干活的时间和对话经过的时间不是一回事
func TestFinishTaskUsesExplicitDuration(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	// 建一条今天 09:00–10:00 的待办
	_, created, err := s.createTask(ctx, nil, CreateTaskIn{
		Title: "写文档", Date: "today", Start: "09:00", End: "10:00",
	})
	if err != nil {
		t.Fatal(err)
	}

	// 显式说只花了 25 分钟
	_, out, err := s.finishTask(ctx, nil, FinishTaskIn{ID: created.ID, DurationMin: 25})
	if err != nil {
		t.Fatal(err)
	}
	if out.DurationMin != 25 {
		t.Errorf("应当用给定的 25 分钟,实际 %d", out.DurationMin)
	}
	if out.Start != "09:00" || out.End != "09:25" {
		t.Errorf("结束时间应当被回写成 09:25,实际 %s", out.End)
	}
	if out.Status != string(model.StatusDone) {
		t.Errorf("状态应当是 done,实际 %s", out.Status)
	}
}

// 不给时长时按当前时间算,但区间不能反
func TestFinishTaskClampsFutureTask(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	// 排到很远的将来,此时"现在"早于开始时间
	future := time.Now().AddDate(0, 0, 30)
	_, created, err := s.createTask(ctx, nil, CreateTaskIn{
		Title: "未来的事", Date: future.Format("2006-01-02"), Start: "09:00", End: "10:00",
	})
	if err != nil {
		t.Fatal(err)
	}

	_, out, err := s.finishTask(ctx, nil, FinishTaskIn{ID: created.ID})
	if err != nil {
		t.Fatalf("结束未来的事项不该报错: %v", err)
	}
	if out.DurationMin <= 0 {
		t.Errorf("时长必须为正,实际 %d", out.DurationMin)
	}
}

func TestCreateTaskDefaults(t *testing.T) {
	s := newTestServer(t)

	_, out, err := s.createTask(context.Background(), nil, CreateTaskIn{Title: "只有标题"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != string(model.StatusTodo) {
		t.Errorf("默认状态应当是 todo,实际 %s", out.Status)
	}
	// 开始时间默认 09:00;结束不传就是"未定结束",不再是默认 +1 小时
	if out.Start != "09:00" {
		t.Errorf("默认开始应当是 09:00,实际 %s", out.Start)
	}
	if out.End != "" {
		t.Errorf("不传 end 应当是未定结束(空串),实际 %q", out.End)
	}
}

// 不传 end 建出来的事项是"未定结束":跨度零长,但照样能被列出来。
func TestCreateTaskWithoutEndIsOpenEnded(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	_, out, err := s.createTask(ctx, nil, CreateTaskIn{Title: "还没想好做到几点"})
	if err != nil {
		t.Fatal(err)
	}
	if out.End != "" {
		t.Errorf("TaskOut.End 应当是空串,实际 %q", out.End)
	}

	_, got, err := s.getTask(ctx, nil, GetTaskIn{ID: out.ID})
	if err != nil {
		t.Fatal(err)
	}
	if got.Start != out.Start || got.End != "" {
		t.Errorf("读回来应当还是未定结束,实际 %s–%s", got.Start, got.End)
	}

	// 零长跨度不该被当成错误而查不到
	_, list, err := s.listTasks(ctx, nil, ListTasksIn{})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, t2 := range list {
		if t2.ID == out.ID {
			found = true
		}
	}
	if !found {
		t.Error("未定结束的事项应当照样能列出来")
	}
}

// 未定结束的事项只改开始时刻,结束要跟着挪 —— 不挪会算出反向区间直接报错。
func TestUpdateTaskMovesOpenEndWithStart(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	_, created, err := s.createTask(ctx, nil, CreateTaskIn{Title: "x", Start: "09:00"})
	if err != nil {
		t.Fatal(err)
	}

	_, out, err := s.updateTask(ctx, nil, UpdateTaskIn{ID: created.ID, Start: "14:00"})
	if err != nil {
		t.Fatalf("未定结束的事项改开始时刻不该报错: %v", err)
	}
	if out.Start != "14:00" || out.End != "" {
		t.Errorf("结束应当跟着起点走、仍是未定,实际 %s–%s", out.Start, out.End)
	}

	// 有真实结束时间的事项,改出反向区间仍然要报错
	_, fixed, err := s.createTask(ctx, nil, CreateTaskIn{Title: "y", Start: "09:00", End: "10:00"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.updateTask(ctx, nil, UpdateTaskIn{ID: fixed.ID, Start: "18:00"}); err == nil {
		t.Error("把开始改到结束之后应当报错")
	}
}

func TestCreateTaskRejectsBadStatus(t *testing.T) {
	s := newTestServer(t)
	_, _, err := s.createTask(context.Background(), nil, CreateTaskIn{Title: "x", Status: "进行中"})
	if err == nil {
		t.Error("非法状态应当报错(必须是英文枚举)")
	}
}

func TestUpdateTaskMovesToAnotherDay(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	_, created, err := s.createTask(ctx, nil, CreateTaskIn{
		Title: "搬家", Date: "2026-03-05", Start: "09:00", End: "10:00",
	})
	if err != nil {
		t.Fatal(err)
	}

	_, out, err := s.updateTask(ctx, nil, UpdateTaskIn{ID: created.ID, Date: "2026-03-06"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Date != "2026-03-06" {
		t.Errorf("日期没搬过去: %s", out.Date)
	}
	// 只改日期时,时刻应当保持不变
	if out.Start != "09:00" || out.End != "10:00" {
		t.Errorf("只改日期不该动时刻,实际 %s–%s", out.Start, out.End)
	}
}

// 填 "-" 表示退回未分类
func TestUpdateTaskClearsCategory(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	_, created, err := s.createTask(ctx, nil, CreateTaskIn{Title: "x", Category: "开发"})
	if err != nil {
		t.Fatal(err)
	}
	if created.Category != "开发" {
		t.Fatalf("分类没设上: %q", created.Category)
	}

	_, out, err := s.updateTask(ctx, nil, UpdateTaskIn{ID: created.ID, Category: "-"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Category != "" {
		t.Errorf("应当退回未分类,实际 %q", out.Category)
	}
}

func TestAddNoteKeepsSource(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	_, created, err := s.createTask(ctx, nil, CreateTaskIn{Title: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.addNote(ctx, nil, AddNoteIn{TaskID: created.ID, Text: "进展"}); err != nil {
		t.Fatal(err)
	}

	_, out, err := s.getTask(ctx, nil, GetTaskIn{ID: created.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Notes) != 1 {
		t.Fatalf("应当有 1 条备注,实际 %d", len(out.Notes))
	}
	if out.Notes[0].Who != "claude" {
		t.Errorf("MCP 写的备注来源应当是 claude,实际 %q", out.Notes[0].Who)
	}
}

func TestListTasksByCategory(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	for _, c := range []struct{ title, cat string }{
		{"开发一", "开发"}, {"开发二", "开发"}, {"运维一", "运维"},
	} {
		if _, _, err := s.logTask(ctx, nil, LogTaskIn{
			Title: c.title, DurationMin: 30, Date: "today", Category: c.cat,
		}); err != nil {
			t.Fatal(err)
		}
	}

	_, out, err := s.listTasks(ctx, nil, ListTasksIn{Category: "开发"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Errorf("开发分类应当有 2 条,实际 %d", len(out))
	}

	// 查一个不存在的分类不是错误,只是没有匹配
	_, out, err = s.listTasks(ctx, nil, ListTasksIn{Category: "不存在的"})
	if err != nil {
		t.Fatalf("查不存在的分类不该报错: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("应当没有匹配,实际 %d 条", len(out))
	}
}

// report 的默认区间是本周,占比之和必须是 100
func TestReportDefaultsToThisWeek(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	for _, c := range []struct {
		title string
		mins  int
		cat   string
	}{
		{"甲", 60, "开发"}, {"乙", 120, "开发"}, {"丙", 60, "运维"},
	} {
		if _, _, err := s.logTask(ctx, nil, LogTaskIn{
			Title: c.title, DurationMin: c.mins, Date: "today", Category: c.cat,
		}); err != nil {
			t.Fatal(err)
		}
	}

	_, rep, err := s.report(ctx, nil, ReportIn{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.TotalMinutes != 240 {
		t.Errorf("总时长应当 240 分钟,实际 %d", rep.TotalMinutes)
	}

	var sum float64
	for _, g := range rep.Groups {
		sum += g.Percent
	}
	if sum < 99.9 || sum > 100.1 {
		t.Errorf("占比之和应当约等于 100,实际 %.2f", sum)
	}
}

func TestReportRejectsBadRange(t *testing.T) {
	s := newTestServer(t)
	_, _, err := s.report(context.Background(), nil, ReportIn{From: "2026-03-10", To: "2026-03-05"})
	if err == nil {
		t.Error("结束早于开始应当报错")
	}
}

func TestDurText(t *testing.T) {
	cases := []struct {
		in   int
		want string
	}{
		{0, "0m"}, {45, "45m"}, {60, "1h"}, {75, "1h15m"}, {150, "2h30m"}, {600, "10h"},
	}
	for _, c := range cases {
		if got := durText(c.in); got != c.want {
			t.Errorf("durText(%d) = %q,期望 %q", c.in, got, c.want)
		}
	}
}

// 界面靠这个回调知道该重新取数。写操作漏了 notify,用户就会看到
// "Claude 说记好了,但窗口里没有" —— 这类问题很难靠肉眼发现,所以逐个覆盖。
func TestWriteToolsNotify(t *testing.T) {
	srv := newTestServer(t)
	ctx := context.Background()

	var fired int
	srv.SetOnChange(func() { fired++ })

	// log_task
	if _, out, err := srv.logTask(ctx, nil, LogTaskIn{Title: "写代码", DurationMin: 30}); err != nil {
		t.Fatalf("log_task: %v", err)
	} else {
		first := out
		if fired != 1 {
			t.Errorf("log_task 后回调次数 = %d, 期望 1", fired)
		}

		// add_note
		_, note, err := srv.addNote(ctx, nil, AddNoteIn{TaskID: first.ID, Text: "进展"})
		if err != nil {
			t.Fatalf("add_note: %v", err)
		}
		if fired != 2 {
			t.Errorf("add_note 后回调次数 = %d, 期望 2", fired)
		}

		// update_note
		if _, _, err := srv.updateNote(ctx, nil, UpdateNoteIn{NoteID: note.ID, Text: "进展(改)"}); err != nil {
			t.Fatalf("update_note: %v", err)
		}
		if fired != 3 {
			t.Errorf("update_note 后回调次数 = %d, 期望 3", fired)
		}

		// delete_note
		if _, _, err := srv.deleteNote(ctx, nil, DeleteNoteIn{NoteID: note.ID}); err != nil {
			t.Fatalf("delete_note: %v", err)
		}
		if fired != 4 {
			t.Errorf("delete_note 后回调次数 = %d, 期望 4", fired)
		}

		// start_task
		if _, _, err := srv.startTask(ctx, nil, StartTaskIn{ID: first.ID}); err != nil {
			t.Fatalf("start_task: %v", err)
		}
		if fired != 5 {
			t.Errorf("start_task 后回调次数 = %d, 期望 5", fired)
		}

		// update_task
		if _, _, err := srv.updateTask(ctx, nil, UpdateTaskIn{ID: first.ID, Title: "写代码(改)"}); err != nil {
			t.Fatalf("update_task: %v", err)
		}
		if fired != 6 {
			t.Errorf("update_task 后回调次数 = %d, 期望 6", fired)
		}

		// finish_task
		if _, _, err := srv.finishTask(ctx, nil, FinishTaskIn{ID: first.ID, DurationMin: 45}); err != nil {
			t.Fatalf("finish_task: %v", err)
		}
		if fired != 7 {
			t.Errorf("finish_task 后回调次数 = %d, 期望 7", fired)
		}

		// delete_task
		if _, _, err := srv.deleteTask(ctx, nil, DeleteTaskIn{ID: first.ID}); err != nil {
			t.Fatalf("delete_task: %v", err)
		}
		if fired != 8 {
			t.Errorf("delete_task 后回调次数 = %d, 期望 8", fired)
		}
	}

	// create_task
	if _, _, err := srv.createTask(ctx, nil, CreateTaskIn{Title: "明天的事"}); err != nil {
		t.Fatalf("create_task: %v", err)
	}
	if fired != 9 {
		t.Errorf("create_task 后回调次数 = %d, 期望 9", fired)
	}

	// create_category
	if _, _, err := srv.createCategory(ctx, nil, CreateCategoryIn{Name: "杂项"}); err != nil {
		t.Fatalf("create_category: %v", err)
	}
	if fired != 10 {
		t.Errorf("create_category 后回调次数 = %d, 期望 10", fired)
	}
}

// 改备注要能读到新内容,并且标记成已编辑。
func TestUpdateNoteTool(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	_, created, err := s.createTask(ctx, nil, CreateTaskIn{Title: "x"})
	if err != nil {
		t.Fatal(err)
	}
	_, note, err := s.addNote(ctx, nil, AddNoteIn{TaskID: created.ID, Text: "写错了"})
	if err != nil {
		t.Fatal(err)
	}
	if note.ID == "" {
		t.Fatal("add_note 必须回传 note id —— 没有它就没法指名改哪一条")
	}

	if _, _, err := s.updateNote(ctx, nil, UpdateNoteIn{NoteID: note.ID, Text: "改好了"}); err != nil {
		t.Fatalf("update_note: %v", err)
	}

	_, out, err := s.getTask(ctx, nil, GetTaskIn{ID: created.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Notes) != 1 {
		t.Fatalf("应当有 1 条备注,实际 %d", len(out.Notes))
	}
	got := out.Notes[0]
	if got.Text != "改好了" {
		t.Errorf("应当读到改后的内容,实际 %q", got.Text)
	}
	if !got.Edited {
		t.Error("改过之后 edited 应当为 true")
	}
	if got.ID != note.ID {
		t.Errorf("ID 应当原样保留,实际 %q", got.ID)
	}
	if got.Who != "claude" {
		t.Errorf("来源不该被改掉,实际 %q", got.Who)
	}

	// 空内容要被拒绝,而且原内容还在
	if _, _, err := s.updateNote(ctx, nil, UpdateNoteIn{NoteID: note.ID, Text: "  "}); err == nil {
		t.Error("改成空应当报错")
	}
	if _, out, _ := s.getTask(ctx, nil, GetTaskIn{ID: created.ID}); out.Notes[0].Text != "改好了" {
		t.Errorf("被拒绝的改动不该生效,实际 %q", out.Notes[0].Text)
	}
}

func TestDeleteNoteTool(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	_, created, err := s.createTask(ctx, nil, CreateTaskIn{Title: "x"})
	if err != nil {
		t.Fatal(err)
	}
	_, note, err := s.addNote(ctx, nil, AddNoteIn{TaskID: created.ID, Text: "删掉"})
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := s.deleteNote(ctx, nil, DeleteNoteIn{NoteID: note.ID}); err != nil {
		t.Fatalf("delete_note: %v", err)
	}

	_, out, err := s.getTask(ctx, nil, GetTaskIn{ID: created.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Notes) != 0 {
		t.Fatalf("删完应当没有备注,实际 %+v", out.Notes)
	}

	if _, _, err := s.deleteNote(ctx, nil, DeleteNoteIn{NoteID: note.ID}); err == nil {
		t.Error("删已经删掉的应当报错")
	}
}

// 读操作不该触发刷新 —— 否则每查一次数据界面就重绘一次。
func TestReadToolsDoNotNotify(t *testing.T) {
	srv := newTestServer(t)
	ctx := context.Background()

	var fired int
	srv.SetOnChange(func() { fired++ })

	if _, _, err := srv.listTasks(ctx, nil, ListTasksIn{}); err != nil {
		t.Fatalf("list_tasks: %v", err)
	}
	if _, _, err := srv.listCategories(ctx, nil, struct{}{}); err != nil {
		t.Fatalf("list_categories: %v", err)
	}
	if _, _, err := srv.report(ctx, nil, ReportIn{}); err != nil {
		t.Fatalf("report: %v", err)
	}
	if fired != 0 {
		t.Errorf("读操作触发了 %d 次刷新,期望 0", fired)
	}
}

// 回调 panic 不能把工具调用带崩 —— 界面刷不刷新是次要的,
// 数据已经写进去了,调用方必须拿到成功返回。
func TestNotifyPanicIsContained(t *testing.T) {
	srv := newTestServer(t)
	srv.SetOnChange(func() { panic("界面那边炸了") })

	if _, _, err := srv.logTask(context.Background(), nil, LogTaskIn{Title: "照常记录", DurationMin: 10}); err != nil {
		t.Fatalf("回调 panic 影响到了工具调用: %v", err)
	}
}

// 没注册回调时也要能正常工作(比如 -mcp-only 模式)。
func TestNotifyWithoutCallback(t *testing.T) {
	srv := newTestServer(t)
	if _, _, err := srv.logTask(context.Background(), nil, LogTaskIn{Title: "无界面", DurationMin: 5}); err != nil {
		t.Fatalf("未注册回调时失败: %v", err)
	}
}

// ---------------------------------------------------------------- 多时段

// 用户要的完整流程:开始 → 暂停 → 继续 → 结束。
// 暂停期间不计时,所以累计时长是两段之和,不是首尾之差。
func TestStartPauseResumeFinish(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	_, created, err := s.createTask(ctx, nil, CreateTaskIn{
		Title: "分几次做完", Date: "today", Start: "09:00", End: "18:00",
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.DurationMin != 0 {
		t.Errorf("待办时长应当 0,实际 %d", created.DurationMin)
	}

	// 开始
	_, started, err := s.startTask(ctx, nil, StartTaskIn{ID: created.ID})
	if err != nil {
		t.Fatal(err)
	}
	if !started.Running {
		t.Fatal("开始后应当处于正在跑的状态")
	}
	if len(started.Spans) != 1 {
		t.Fatalf("应当有 1 段,实际 %d", len(started.Spans))
	}

	// 重复开始不该凭空多出一段
	_, again, err := s.startTask(ctx, nil, StartTaskIn{ID: created.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Spans) != 1 {
		t.Errorf("重复开始不该多出时段,实际 %d 段", len(again.Spans))
	}

	// 暂停:关掉那一段,状态仍是进行中
	_, paused, err := s.pauseTask(ctx, nil, PauseTaskIn{ID: created.ID})
	if err != nil {
		t.Fatal(err)
	}
	if paused.Running {
		t.Error("暂停后不该还在跑")
	}
	if paused.Status != string(model.StatusDoing) {
		t.Errorf("暂停不该改状态,实际 %s", paused.Status)
	}

	// 重复暂停是安全的
	if _, _, err := s.pauseTask(ctx, nil, PauseTaskIn{ID: created.ID}); err != nil {
		t.Fatalf("重复暂停不该报错: %v", err)
	}

	// 继续:再开一段
	_, resumed, err := s.startTask(ctx, nil, StartTaskIn{ID: created.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(resumed.Spans) != 2 {
		t.Fatalf("继续后应当有 2 段,实际 %d", len(resumed.Spans))
	}
	if !resumed.Running {
		t.Error("继续后应当在跑")
	}

	// 结束
	_, done, err := s.finishTask(ctx, nil, FinishTaskIn{ID: created.ID})
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != string(model.StatusDone) {
		t.Errorf("状态应当是 done,实际 %s", done.Status)
	}
	if done.Running {
		t.Error("结束后不该还在跑")
	}
	if len(done.Spans) != 2 {
		t.Errorf("结束后仍应当是 2 段,实际 %d", len(done.Spans))
	}
	for _, sp := range done.Spans {
		if sp.End == "" {
			t.Errorf("结束后每段都该有结束时间,实际 %+v", sp)
		}
	}
}

// 补记"上午 10–11 做了一段,下午 15–16 又做了一段":时长是两段之和,
// 中间停掉的四小时不算。
func TestAddSpanRecordsSeparateStretches(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	_, created, err := s.createTask(ctx, nil, CreateTaskIn{
		Title: "写方案", Date: "today", Start: "10:00", End: "11:00", Category: "设计",
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, out, err := s.addSpan(ctx, nil, AddSpanIn{
		TaskID: created.ID, Date: "today", Start: "10:00", End: "11:00",
	}); err != nil {
		t.Fatal(err)
	} else if out.DurationMin != 60 {
		t.Errorf("补第一段后应当 60 分钟,实际 %d", out.DurationMin)
	}

	_, out, err := s.addSpan(ctx, nil, AddSpanIn{
		TaskID: created.ID, Date: "today", Start: "15:00", End: "16:00",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.DurationMin != 120 {
		t.Errorf("两段合计应当 120 分钟(不是 6 小时),实际 %d", out.DurationMin)
	}
	if len(out.Spans) != 2 {
		t.Fatalf("应当有 2 段,实际 %d", len(out.Spans))
	}
	// 跨度覆盖中间的空档,但时长不含它
	if out.Start != "10:00" || out.End != "16:00" {
		t.Errorf("跨度应当是 10:00–16:00,实际 %s–%s", out.Start, out.End)
	}
}

func TestAddSpanValidation(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	_, created, err := s.createTask(ctx, nil, CreateTaskIn{Title: "校验", Date: "today"})
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		in   AddSpanIn
	}{
		{"结束早于开始", AddSpanIn{TaskID: created.ID, Date: "today", Start: "11:00", End: "10:00"}},
		{"缺结束", AddSpanIn{TaskID: created.ID, Date: "today", Start: "10:00"}},
		{"缺开始", AddSpanIn{TaskID: created.ID, Date: "today", End: "11:00"}},
	}
	for _, c := range cases {
		if _, _, err := s.addSpan(ctx, nil, c.in); err == nil {
			t.Errorf("%s:应当报错", c.name)
		}
	}
}

// 结束一条还没计过时的待办:按跨度补一段,而不是以 0 时长收场
func TestFinishTaskOnTaskWithoutSpans(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	_, created, err := s.createTask(ctx, nil, CreateTaskIn{
		Title: "直接结束", Date: "today", Start: "09:00", End: "10:00",
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.DurationMin != 0 {
		t.Fatalf("待办时长应当 0,实际 %d", created.DurationMin)
	}

	_, out, err := s.finishTask(ctx, nil, FinishTaskIn{ID: created.ID, DurationMin: 40})
	if err != nil {
		t.Fatal(err)
	}
	if out.DurationMin != 40 {
		t.Errorf("应当按给定的 40 分钟记一段,实际 %d", out.DurationMin)
	}
	if len(out.Spans) != 1 {
		t.Errorf("应当补出 1 段,实际 %d", len(out.Spans))
	}
}

// 给时长时,作用在"正在跑的那一段"上,之前暂停过的段不动
func TestFinishTaskDurationAppliesToOpenSpanOnly(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	_, created, err := s.createTask(ctx, nil, CreateTaskIn{Title: "两段", Date: "today", Start: "09:00", End: "18:00"})
	if err != nil {
		t.Fatal(err)
	}
	// 先补一段固定的 60 分钟
	if _, _, err := s.addSpan(ctx, nil, AddSpanIn{
		TaskID: created.ID, Date: "today", Start: "10:00", End: "11:00",
	}); err != nil {
		t.Fatal(err)
	}
	// 再开一段
	if _, _, err := s.startTask(ctx, nil, StartTaskIn{ID: created.ID}); err != nil {
		t.Fatal(err)
	}
	// 给 30 分钟:加在开着的那段上
	_, out, err := s.finishTask(ctx, nil, FinishTaskIn{ID: created.ID, DurationMin: 30})
	if err != nil {
		t.Fatal(err)
	}
	if out.DurationMin != 90 {
		t.Errorf("应当 60 + 30 = 90 分钟,实际 %d", out.DurationMin)
	}
	if len(out.Spans) != 2 {
		t.Fatalf("应当 2 段,实际 %d", len(out.Spans))
	}
}

// 删除时段也要推事件给界面
func TestDeleteSpanNotifies(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	var fired int
	s.SetOnChange(func() { fired++ })

	_, created, err := s.createTask(ctx, nil, CreateTaskIn{Title: "删段", Date: "today"})
	if err != nil {
		t.Fatal(err)
	}
	_, withSpan, err := s.addSpan(ctx, nil, AddSpanIn{
		TaskID: created.ID, Date: "today", Start: "10:00", End: "11:00",
	})
	if err != nil {
		t.Fatal(err)
	}
	before := fired

	if _, _, err := s.deleteSpan(ctx, nil, DeleteSpanIn{SpanID: withSpan.Spans[0].ID}); err != nil {
		t.Fatal(err)
	}
	if fired != before+1 {
		t.Errorf("删除时段应当推一次事件,%d → %d", before, fired)
	}
}

// 补了实际投入就不该还算待办 —— 否则会出现"待办,但已投入 2 小时"
func TestAddSpanPromotesTodoToDoing(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	_, created, err := s.createTask(ctx, nil, CreateTaskIn{Title: "待办", Date: "today"})
	if err != nil {
		t.Fatal(err)
	}
	if created.Status != string(model.StatusTodo) {
		t.Fatalf("初始应当是待办,实际 %s", created.Status)
	}

	_, out, err := s.addSpan(ctx, nil, AddSpanIn{
		TaskID: created.ID, Date: "today", Start: "10:00", End: "11:00",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != string(model.StatusDoing) {
		t.Errorf("补了时段后应当变成进行中,实际 %s", out.Status)
	}

	// 已完成的补记一段,不该被退回进行中
	_, done, err := s.createTask(ctx, nil, CreateTaskIn{
		Title: "已完成的", Date: "today", Status: "done",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, after, err := s.addSpan(ctx, nil, AddSpanIn{
		TaskID: done.ID, Date: "today", Start: "14:00", End: "15:00",
	})
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != string(model.StatusDone) {
		t.Errorf("已完成的事项补记后应当仍是已完成,实际 %s", after.Status)
	}
}

// ---------------------------------------------------------------- 需求

func TestCreateRequirementDefaultsToSubmit(t *testing.T) {
	srv := newTestServer(t)

	_, out, err := srv.createRequirement(context.Background(), nil, CreateRequirementIn{
		Title: "告警分析 Agent 联调", Component: "axeops",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Stage != "submit" {
		t.Errorf("默认阶段该是 submit,实际 %s", out.Stage)
	}
	if out.StageLabel != "需求提交" {
		t.Errorf("阶段中文名该是 需求提交,实际 %s", out.StageLabel)
	}
	if out.OnlineAt != "" || out.Quarter != "" {
		t.Errorf("新需求不该有上线时间和季度: %q / %q", out.OnlineAt, out.Quarter)
	}
	if out.Component != "axeops" {
		t.Errorf("组件该自动建出来,实际 %q", out.Component)
	}
}

// 填了上线时间就等于说它上线了 —— 阶段不用再单独传一遍。
func TestCreateRequirementWithOnlineDate(t *testing.T) {
	srv := newTestServer(t)

	_, out, err := srv.createRequirement(context.Background(), nil, CreateRequirementIn{
		Title: "巡检报告导出", OnlineAt: "2026-10-15",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Stage != "online" {
		t.Errorf("给了上线时间,阶段该是 online,实际 %s", out.Stage)
	}
	if out.OnlineAt != "2026-10-15" {
		t.Errorf("上线时间该是 2026-10-15,实际 %q", out.OnlineAt)
	}
	if out.Quarter != "2026 Q4" {
		t.Errorf("季度该是 2026 Q4,实际 %q", out.Quarter)
	}
}

func TestUpdateRequirementStageToOnline(t *testing.T) {
	srv := newTestServer(t)
	ctx := context.Background()

	_, created, err := srv.createRequirement(ctx, nil, CreateRequirementIn{Title: "推进中的需求"})
	if err != nil {
		t.Fatal(err)
	}

	_, out, err := srv.updateRequirement(ctx, nil, UpdateRequirementIn{ID: created.ID, Stage: "online"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Stage != "online" || out.OnlineAt == "" {
		t.Errorf("切到 online 该自动记上线时间: stage=%s online_at=%q", out.Stage, out.OnlineAt)
	}
	if out.Quarter == "" {
		t.Error("上线后该有季度")
	}
}

// 中文阶段名也收 —— 模型很可能照着界面上的字写。
func TestUpdateRequirementAcceptsChineseStage(t *testing.T) {
	srv := newTestServer(t)
	ctx := context.Background()

	_, created, err := srv.createRequirement(ctx, nil, CreateRequirementIn{Title: "中文阶段"})
	if err != nil {
		t.Fatal(err)
	}
	_, out, err := srv.updateRequirement(ctx, nil, UpdateRequirementIn{ID: created.ID, Stage: "需求开发"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Stage != "dev" {
		t.Errorf("「需求开发」该解析成 dev,实际 %s", out.Stage)
	}
}

func TestUpdateRequirementRejectsBadStage(t *testing.T) {
	srv := newTestServer(t)
	ctx := context.Background()

	_, created, err := srv.createRequirement(ctx, nil, CreateRequirementIn{Title: "阶段写错"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := srv.updateRequirement(ctx, nil, UpdateRequirementIn{ID: created.ID, Stage: "已上线"}); err == nil {
		t.Error("认不出的阶段该报错,而不是静默忽略")
	}
}

func TestListRequirementsByComponent(t *testing.T) {
	srv := newTestServer(t)
	ctx := context.Background()

	if _, _, err := srv.createRequirement(ctx, nil, CreateRequirementIn{Title: "甲", Component: "组件甲"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := srv.createRequirement(ctx, nil, CreateRequirementIn{Title: "乙", Component: "组件乙"}); err != nil {
		t.Fatal(err)
	}

	_, out, err := srv.listRequirements(ctx, nil, ListRequirementsIn{Component: "组件甲"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].Title != "甲" {
		t.Errorf("按组件过滤结果不对: %+v", out)
	}

	// 查一个不存在的组件返回空,不报错
	_, none, err := srv.listRequirements(ctx, nil, ListRequirementsIn{Component: "没这个"})
	if err != nil {
		t.Fatalf("查不存在的组件不该报错: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("该是空,实际 %+v", none)
	}
}

func TestRequirementReportByQuarter(t *testing.T) {
	srv := newTestServer(t)
	ctx := context.Background()

	for _, in := range []CreateRequirementIn{
		{Title: "一", Component: "axeops", OnlineAt: "2026-10-05"},
		{Title: "二", Component: "axeops", OnlineAt: "2026-12-20"},
		{Title: "三", Component: "xdr"},
	} {
		if _, _, err := srv.createRequirement(ctx, nil, in); err != nil {
			t.Fatal(err)
		}
	}

	_, out, err := srv.requirementReport(ctx, nil, RequirementReportIn{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Total != 3 || out.Online != 2 {
		t.Fatalf("总数/上线数不对: %d / %d", out.Total, out.Online)
	}
	if len(out.Quarters) != 1 || out.Quarters[0].Quarter != "2026 Q4" || out.Quarters[0].Online != 2 {
		t.Errorf("季度拆分不对: %+v", out.Quarters)
	}

	byName := map[string]ReqComponentOut{}
	for _, c := range out.Components {
		byName[c.Component] = c
	}
	if got := byName["axeops"]; got.Total != 2 || got.Online != 2 || got.Percent != 100 {
		t.Errorf("axeops 该是 2/2 100%%,实际 %+v", got)
	}
	if got := byName["xdr"]; got.Total != 1 || got.Online != 0 {
		t.Errorf("xdr 该是 0/1,实际 %+v", got)
	}
}

func TestDeleteRequirementTool(t *testing.T) {
	srv := newTestServer(t)
	ctx := context.Background()

	_, created, err := srv.createRequirement(ctx, nil, CreateRequirementIn{Title: "要删的"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := srv.deleteRequirement(ctx, nil, DeleteRequirementIn{ID: created.ID}); err != nil {
		t.Fatal(err)
	}

	_, left, err := srv.listRequirements(ctx, nil, ListRequirementsIn{})
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Errorf("删完该是空的,实际 %+v", left)
	}
}

// 需求的写操作也要推刷新,读操作不能推 —— 和事项那套规则一致。
func TestRequirementToolsNotify(t *testing.T) {
	srv := newTestServer(t)
	ctx := context.Background()

	var fired int
	srv.SetOnChange(func() { fired++ })

	_, created, err := srv.createRequirement(ctx, nil, CreateRequirementIn{Title: "会通知的需求"})
	if err != nil {
		t.Fatal(err)
	}
	if fired != 1 {
		t.Errorf("create_requirement 后回调次数 = %d, 期望 1", fired)
	}

	if _, _, err := srv.updateRequirement(ctx, nil, UpdateRequirementIn{ID: created.ID, Title: "改个名"}); err != nil {
		t.Fatal(err)
	}
	if fired != 2 {
		t.Errorf("update_requirement 后回调次数 = %d, 期望 2", fired)
	}

	if _, _, err := srv.deleteRequirement(ctx, nil, DeleteRequirementIn{ID: created.ID}); err != nil {
		t.Fatal(err)
	}
	if fired != 3 {
		t.Errorf("delete_requirement 后回调次数 = %d, 期望 3", fired)
	}

	if _, _, err := srv.listRequirements(ctx, nil, ListRequirementsIn{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := srv.requirementReport(ctx, nil, RequirementReportIn{}); err != nil {
		t.Fatal(err)
	}
	if fired != 3 {
		t.Errorf("读操作不该触发刷新,回调次数 = %d, 期望仍是 3", fired)
	}
}
