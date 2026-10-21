package store

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"worklog/internal/model"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// 时间戳辅助:把某天的 HH:MM 转成本地时间
func at(day int, hh, mm int) time.Time {
	return time.Date(2026, 10, day, hh, mm, 0, 0, time.Local)
}

func mustCat(t *testing.T, s *Store, name string) *model.Category {
	t.Helper()
	c, err := s.EnsureCategory(name)
	if err != nil {
		t.Fatalf("建分类 %s 失败: %v", name, err)
	}
	if c == nil {
		t.Fatalf("分类 %s 应当被创建", name)
	}
	return c
}

func mustTask(t *testing.T, s *Store, title string, start, end time.Time, catID string) *model.Task {
	t.Helper()
	task, err := s.CreateTask(model.Task{
		Title: title, StartAt: start, EndAt: end,
		CategoryID: catID, Status: model.StatusDone, Source: model.SourceUI,
	})
	if err != nil {
		t.Fatalf("建事项 %s 失败: %v", title, err)
	}
	return task
}

// ---------------------------------------------------------------- 基础

func TestOpenSeedsCategories(t *testing.T) {
	s := newTestStore(t)

	cats, err := s.ListCategories()
	if err != nil {
		t.Fatalf("列分类失败: %v", err)
	}
	if len(cats) == 0 {
		t.Fatal("全新库应当有几个默认分类")
	}
	// 默认分类的颜色必须两两不同,否则调色板取色就失效了
	seen := map[string]bool{}
	for _, c := range cats {
		if seen[c.Color] {
			t.Errorf("默认分类出现重复颜色: %s", c.Color)
		}
		seen[c.Color] = true
	}
}

// 删光分类后重启,不该又被塞回默认分类 —— 那是用户的选择
func TestSeedDoesNotComeBack(t *testing.T) {
	dir := t.TempDir()

	s1, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	cats, _ := s1.ListCategories()
	for _, c := range cats {
		if err := s1.DeleteCategory(c.ID); err != nil {
			t.Fatalf("删分类失败: %v", err)
		}
	}
	s1.Close()

	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	after, _ := s2.ListCategories()
	if len(after) != 0 {
		t.Errorf("分类被删光后重启不该重新塞默认值,实际有 %d 个", len(after))
	}
}

func TestEnsureCategoryIsIdempotent(t *testing.T) {
	s := newTestStore(t)

	a, err := s.EnsureCategory("  告警  ")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.EnsureCategory("告警")
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != b.ID {
		t.Errorf("同名分类应当复用,得到两个 ID: %s / %s", a.ID, b.ID)
	}
	if a.Name != "告警" {
		t.Errorf("名字应当去掉首尾空白,实际 %q", a.Name)
	}
}

func TestEnsureCategoryEmptyIsNotAnError(t *testing.T) {
	s := newTestStore(t)
	c, err := s.EnsureCategory("   ")
	if err != nil {
		t.Fatalf("空名字不该报错: %v", err)
	}
	if c != nil {
		t.Error("空名字应当返回 nil(未分类是合法状态)")
	}
}

// 删分类不能连带删掉事项 —— 整理分类不该弄丢记录
func TestDeleteCategoryKeepsTasks(t *testing.T) {
	s := newTestStore(t)
	c := mustCat(t, s, "临时")
	task := mustTask(t, s, "一件事", at(15, 9, 0), at(15, 10, 0), c.ID)

	if err := s.DeleteCategory(c.ID); err != nil {
		t.Fatalf("删分类失败: %v", err)
	}

	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("事项应当还在: %v", err)
	}
	if got.CategoryID != "" {
		t.Errorf("事项应当退回未分类,实际还挂着 %s", got.CategoryID)
	}
	if got.Category != nil {
		t.Error("分类对象应当被清空")
	}
}

func TestCreateCategoryRejectsDuplicate(t *testing.T) {
	s := newTestStore(t)
	mustCat(t, s, "重复")

	_, err := s.CreateCategory("重复", "")
	if !errors.Is(err, ErrCategoryExists) {
		t.Errorf("重名应当返回 ErrCategoryExists,实际 %v", err)
	}
}

func TestUpdateCategory(t *testing.T) {
	s := newTestStore(t)
	c := mustCat(t, s, "旧名")

	if err := s.UpdateCategory(c.ID, "新名", "#123456"); err != nil {
		t.Fatalf("改名失败: %v", err)
	}
	got, _ := s.GetCategory(c.ID)
	if got.Name != "新名" || got.Color != "#123456" {
		t.Errorf("更新没生效: %+v", got)
	}

	// 改成另一个已存在的名字应当被拒
	other := mustCat(t, s, "别的")
	if err := s.UpdateCategory(c.ID, other.Name, ""); !errors.Is(err, ErrCategoryExists) {
		t.Errorf("重名应当被拒,实际 %v", err)
	}
}

// ---------------------------------------------------------------- 事项

func TestTaskRoundTrip(t *testing.T) {
	s := newTestStore(t)
	c := mustCat(t, s, "开发")

	created := mustTask(t, s, "写设计文档", at(15, 9, 30), at(15, 11, 0), c.ID)

	got, err := s.GetTask(created.ID)
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if got.Title != "写设计文档" {
		t.Errorf("标题不对: %q", got.Title)
	}
	if d := got.Duration(nowFunc()); d != 90*time.Minute {
		t.Errorf("时长应当 90 分钟,实际 %v", d)
	}
	if got.Category == nil || got.Category.Name != "开发" {
		t.Errorf("分类没带出来: %+v", got.Category)
	}
	if !got.StartAt.Equal(at(15, 9, 30)) {
		t.Errorf("开始时间对不上: %v", got.StartAt)
	}
}

func TestCreateTaskValidation(t *testing.T) {
	s := newTestStore(t)

	cases := []struct {
		name string
		task model.Task
	}{
		{"空标题", model.Task{StartAt: at(15, 9, 0), EndAt: at(15, 10, 0), Status: model.StatusTodo}},
		{"区间反了", model.Task{Title: "x", StartAt: at(15, 10, 0), EndAt: at(15, 9, 0), Status: model.StatusTodo}},
		{"状态非法", model.Task{Title: "x", StartAt: at(15, 9, 0), EndAt: at(15, 10, 0), Status: "nope"}},
	}
	for _, c := range cases {
		if _, err := s.CreateTask(c.task); err == nil {
			t.Errorf("%s:应当报错", c.name)
		}
	}
}

func TestUpdateTaskAndRangeValidation(t *testing.T) {
	s := newTestStore(t)
	task := mustTask(t, s, "一件事", at(15, 9, 0), at(15, 10, 0), "")

	// 只改开始时间,把它推到结束之后 —— 必须在写完之后校验区间
	bad := at(15, 11, 0)
	if _, err := s.UpdateTask(task.ID, TaskPatch{StartAt: &bad}); err == nil {
		t.Error("把开始时间推到结束之后应当报错")
	}

	title := "改过的标题"
	got, err := s.UpdateTask(task.ID, TaskPatch{Title: &title})
	if err != nil {
		t.Fatalf("改标题失败: %v", err)
	}
	if got.Title != title {
		t.Errorf("标题没改上: %q", got.Title)
	}
}

func TestDeleteTaskCascadesNotes(t *testing.T) {
	s := newTestStore(t)
	task := mustTask(t, s, "带备注的事", at(15, 9, 0), at(15, 10, 0), "")
	if _, err := s.AddNote(task.ID, "一条备注", model.SourceMCP); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteTask(task.ID); err != nil {
		t.Fatalf("删事项失败: %v", err)
	}

	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM notes WHERE task_id = ?`, task.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("备注应当随事项级联删除,还剩 %d 条", n)
	}
}

func TestListTasksFilters(t *testing.T) {
	s := newTestStore(t)
	dev := mustCat(t, s, "开发")
	ops := mustCat(t, s, "运维")

	mustTask(t, s, "周一开发", at(12, 9, 0), at(12, 10, 0), dev.ID)
	mustTask(t, s, "周二运维", at(13, 9, 0), at(13, 10, 0), ops.ID)
	mustTask(t, s, "周三未分类", at(14, 9, 0), at(14, 10, 0), "")

	from, to := at(13, 0, 0), at(14, 0, 0)

	got, err := s.ListTasks(TaskFilter{From: &from, To: &to})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Title != "周二运维" {
		t.Errorf("按日期筛应当只剩周二那条,实际 %d 条", len(got))
	}

	got, _ = s.ListTasks(TaskFilter{Category: dev.ID})
	if len(got) != 1 || got[0].Title != "周一开发" {
		t.Errorf("按分类筛不对: %d 条", len(got))
	}

	got, _ = s.ListTasks(TaskFilter{Category: NoCategory})
	if len(got) != 1 || got[0].Title != "周三未分类" {
		t.Errorf("筛未分类不对: %d 条", len(got))
	}

	got, _ = s.ListTasks(TaskFilter{Query: "运维"})
	if len(got) != 1 {
		t.Errorf("按标题搜应当命中 1 条,实际 %d 条", len(got))
	}
}

// LIKE 的通配符必须被转义,否则搜 "%" 会命中所有记录
func TestSearchEscapesWildcards(t *testing.T) {
	s := newTestStore(t)
	mustTask(t, s, "进度 100%", at(15, 9, 0), at(15, 10, 0), "")
	mustTask(t, s, "别的事", at(15, 11, 0), at(15, 12, 0), "")

	got, err := s.ListTasks(TaskFilter{Query: "%"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Title != "进度 100%" {
		t.Errorf("百分号应当按字面匹配,实际命中 %d 条", len(got))
	}
}

func TestCurrentTasks(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.CreateTask(model.Task{
		Title: "在做的事", StartAt: at(15, 9, 0), EndAt: at(15, 10, 0), Status: model.StatusDoing,
	}); err != nil {
		t.Fatal(err)
	}
	mustTask(t, s, "做完的事", at(15, 11, 0), at(15, 12, 0), "")

	cur, err := s.CurrentTasks()
	if err != nil {
		t.Fatal(err)
	}
	if len(cur) != 1 || cur[0].Title != "在做的事" {
		t.Errorf("进行中的应当只有 1 条,实际 %d 条", len(cur))
	}
}

func TestNotesOrdered(t *testing.T) {
	s := newTestStore(t)
	task := mustTask(t, s, "带备注的事", at(15, 9, 0), at(15, 10, 0), "")

	base := nowFunc()
	nowFunc = func() time.Time { return base }
	defer func() { nowFunc = time.Now }()

	for _, b := range []string{"第一条", "第二条", "第三条"} {
		if _, err := s.AddNote(task.ID, b, model.SourceMCP); err != nil {
			t.Fatal(err)
		}
	}

	notes, err := s.NotesOf(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 3 {
		t.Fatalf("应当有 3 条备注,实际 %d", len(notes))
	}
	for i, want := range []string{"第一条", "第二条", "第三条"} {
		if notes[i].Body != want {
			t.Errorf("第 %d 条应当是 %q,实际 %q", i, want, notes[i].Body)
		}
		if notes[i].Source != model.SourceMCP {
			t.Errorf("来源应当保留 mcp,实际 %q", notes[i].Source)
		}
	}
}

// 改备注只动正文和 updated_at,**创建时刻一概不动**。
//
// 时间线上那条备注的位置由创建时刻决定 —— 改个错别字不该让"下午三点记的那条"
// 跳到"晚上七点",那时间线就没意义了。
func TestUpdateNoteKeepsCreatedAt(t *testing.T) {
	s := newTestStore(t)
	task := mustTask(t, s, "带备注的事", at(15, 9, 0), at(15, 10, 0), "")

	// 用整秒的时间:库里的时间戳精度就是秒,带纳秒的 now 往返之后会被截掉
	created := time.Date(2026, 10, 15, 9, 0, 0, 0, time.Local)
	nowFunc = func() time.Time { return created }
	defer func() { nowFunc = time.Now }()

	n, err := s.AddNote(task.ID, "写错了", model.SourceUI)
	if err != nil {
		t.Fatal(err)
	}
	if n.UpdatedAt != nil {
		t.Errorf("刚写的备注不该有 updated_at")
	}

	edited := created.Add(2 * time.Hour)
	nowFunc = func() time.Time { return edited }

	got, err := s.UpdateNote(n.ID, "  改好了  ")
	if err != nil {
		t.Fatal(err)
	}
	if got.Body != "改好了" {
		t.Errorf("正文应当 trim 后写入,实际 %q", got.Body)
	}
	if !got.CreatedAt.Equal(created) {
		t.Errorf("创建时刻不该变:%v → %v", created, got.CreatedAt)
	}
	if got.UpdatedAt == nil {
		t.Fatal("改过之后应当有 updated_at")
	}
	if !got.UpdatedAt.Equal(edited) {
		t.Errorf("updated_at 应当是改动时刻 %v,实际 %v", edited, got.UpdatedAt)
	}

	// 读回来的也要带上
	notes, err := s.NotesOf(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 || notes[0].UpdatedAt == nil {
		t.Errorf("NotesOf 应当带回 updated_at,实际 %+v", notes)
	}
}

// 改成空不算删除 —— 手滑清空输入框不该丢掉一条记录。
func TestUpdateNoteRejectsEmpty(t *testing.T) {
	s := newTestStore(t)
	task := mustTask(t, s, "带备注的事", at(15, 9, 0), at(15, 10, 0), "")
	n, err := s.AddNote(task.ID, "原来的内容", model.SourceUI)
	if err != nil {
		t.Fatal(err)
	}

	for _, bad := range []string{"", "   ", "\n\t "} {
		if _, err := s.UpdateNote(n.ID, bad); err == nil {
			t.Errorf("空内容 %q 应当被拒绝", bad)
		}
	}

	after, err := s.GetNote(n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Body != "原来的内容" {
		t.Errorf("被拒绝的改动不该生效,实际 %q", after.Body)
	}
	if after.UpdatedAt != nil {
		t.Errorf("被拒绝的改动不该留下 updated_at,实际 %v", after.UpdatedAt)
	}

	if _, err := s.UpdateNote("没这个id", "x"); err == nil {
		t.Error("改不存在的备注应当报错")
	}
}

func TestDeleteNote(t *testing.T) {
	s := newTestStore(t)
	task := mustTask(t, s, "带备注的事", at(15, 9, 0), at(15, 10, 0), "")
	keep, err := s.AddNote(task.ID, "留着", model.SourceUI)
	if err != nil {
		t.Fatal(err)
	}
	drop, err := s.AddNote(task.ID, "删掉", model.SourceUI)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteNote(drop.ID); err != nil {
		t.Fatal(err)
	}
	notes, err := s.NotesOf(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 || notes[0].ID != keep.ID {
		t.Fatalf("应当只剩第一条,实际 %+v", notes)
	}
	if err := s.DeleteNote(drop.ID); err == nil {
		t.Error("删已经删掉的应当报错")
	}
}

// ---------------------------------------------------------------- 汇总
// 并行计时的核心口径:求和会重复计算重叠部分,并集不会。
func TestUnionMinutes(t *testing.T) {
	cases := []struct {
		name  string
		spans []timeSpan
		want  int
	}{
		{"空", nil, 0},
		{"单个", []timeSpan{{at(15, 9, 0), at(15, 10, 0)}}, 60},
		{
			"完全不重叠",
			[]timeSpan{{at(15, 9, 0), at(15, 10, 0)}, {at(15, 11, 0), at(15, 12, 0)}},
			120,
		},
		{
			"部分重叠:60 + 60,重叠 30 → 并集 90",
			[]timeSpan{{at(15, 9, 0), at(15, 10, 0)}, {at(15, 9, 30), at(15, 10, 30)}},
			90,
		},
		{
			"完全包含:60 + 20(在内)→ 60",
			[]timeSpan{{at(15, 9, 0), at(15, 10, 0)}, {at(15, 9, 10), at(15, 9, 30)}},
			60,
		},
		{
			"完全重合 → 60",
			[]timeSpan{{at(15, 9, 0), at(15, 10, 0)}, {at(15, 9, 0), at(15, 10, 0)}},
			60,
		},
		{
			"首尾相接不算重叠",
			[]timeSpan{{at(15, 9, 0), at(15, 10, 0)}, {at(15, 10, 0), at(15, 11, 0)}},
			120,
		},
		{
			"乱序输入也要正确",
			[]timeSpan{{at(15, 11, 0), at(15, 12, 0)}, {at(15, 9, 0), at(15, 10, 0)}},
			120,
		},
	}
	for _, c := range cases {
		if got := unionMinutes(c.spans); got != c.want {
			t.Errorf("%s: unionMinutes = %d,期望 %d", c.name, got, c.want)
		}
	}
}

func TestReportPercentSumsTo100(t *testing.T) {
	s := newTestStore(t)
	dev := mustCat(t, s, "开发")
	ops := mustCat(t, s, "运维")

	mustTask(t, s, "开发一", at(12, 9, 0), at(12, 10, 30), dev.ID)
	mustTask(t, s, "开发二", at(13, 9, 0), at(13, 10, 0), dev.ID)
	mustTask(t, s, "运维一", at(14, 9, 0), at(14, 9, 30), ops.ID)

	from, to := at(12, 0, 0), at(15, 0, 0)
	rep, err := s.ReportBetween(from, to)
	if err != nil {
		t.Fatal(err)
	}

	// 90 + 60 + 30 = 180 分钟
	if rep.TotalMinutes != 180 {
		t.Errorf("总时长应当 180 分钟,实际 %d", rep.TotalMinutes)
	}
	if rep.TaskCount != 3 {
		t.Errorf("事项数应当 3,实际 %d", rep.TaskCount)
	}

	var sum float64
	for _, g := range rep.Groups {
		sum += g.Percent
	}
	if sum < 99.9 || sum > 100.1 {
		t.Errorf("占比之和应当约等于 100,实际 %.2f", sum)
	}

	// 开发 150 分钟,应当排第一
	if len(rep.Groups) != 2 || rep.Groups[0].Name != "开发" {
		t.Fatalf("分组不对: %+v", rep.Groups)
	}
	if rep.Groups[0].Minutes != 150 {
		t.Errorf("开发应当 150 分钟,实际 %d", rep.Groups[0].Minutes)
	}
}

// 并行推进两件事时,总和会大于墙钟时间,重叠量必须如实报出来
func TestReportReportsOverlap(t *testing.T) {
	s := newTestStore(t)
	mustTask(t, s, "A", at(15, 9, 0), at(15, 10, 0), "")
	mustTask(t, s, "B", at(15, 9, 30), at(15, 10, 30), "")

	from, to := at(15, 0, 0), at(15, 23, 0)
	rep, err := s.ReportBetween(from, to)
	if err != nil {
		t.Fatal(err)
	}

	if rep.TotalMinutes != 120 {
		t.Errorf("求和应当 120 分钟,实际 %d", rep.TotalMinutes)
	}
	if rep.UnionMinutes != 90 {
		t.Errorf("墙钟并集应当 90 分钟,实际 %d", rep.UnionMinutes)
	}
	if rep.OverlapMinutes != 30 {
		t.Errorf("重叠应当 30 分钟,实际 %d", rep.OverlapMinutes)
	}
}

func TestReportEmptyRange(t *testing.T) {
	s := newTestStore(t)
	from, to := at(1, 0, 0), at(2, 0, 0)

	rep, err := s.ReportBetween(from, to)
	if err != nil {
		t.Fatalf("空区间不该报错: %v", err)
	}
	if rep.TotalMinutes != 0 || len(rep.Groups) != 0 {
		t.Errorf("空区间应当没有任何数据: %+v", rep)
	}
}

func TestWeekRange(t *testing.T) {
	// 2026-10-15 是周四,所在周应当是 10-12(周一)到 10-19(下周一)
	from, to := WeekRange(at(15, 14, 30))
	if from.Day() != 12 || from.Month() != 10 {
		t.Errorf("周一应当是 10-12,实际 %v", from)
	}
	if to.Day() != 19 {
		t.Errorf("下周一应当是 10-19,实际 %v", to)
	}
	if from.Weekday() != time.Monday {
		t.Errorf("起点应当是周一,实际 %v", from.Weekday())
	}

	// 周日属于同一周,不能滑到下一周去
	from2, _ := WeekRange(at(18, 23, 0))
	if from2.Day() != 12 {
		t.Errorf("周日的周起点应当是 10-12,实际 %v", from2)
	}

	// 周一当天就是起点
	from3, _ := WeekRange(at(12, 0, 1))
	if from3.Day() != 12 {
		t.Errorf("周一的周起点应当是当天,实际 %v", from3)
	}
}

func TestDayRange(t *testing.T) {
	from, to := DayRange(at(15, 23, 59))
	if from.Day() != 15 || to.Day() != 16 {
		t.Errorf("日区间不对: %v → %v", from, to)
	}
}

// ---------------------------------------------------------------- 多时段

// 一段横跨周日/周一:两周各算各的那一半。
// 2026-10-11 是周日,10-12 是周一。
func TestReportClipsSpansAcrossWeeks(t *testing.T) {
	s := newTestStore(t)
	task := mustTodo(t, s, "通宵的那件事", at(11, 23, 0), at(12, 1, 0))

	if _, err := s.AddSpan(task.ID, at(11, 23, 0), ptrTime(at(12, 1, 0))); err != nil {
		t.Fatal(err)
	}

	// 上一周:10-05 ~ 10-12(左闭右开,所以 10-12 00:00 属于下一周)
	prev, err := s.ReportBetween(at(5, 0, 0), at(12, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if prev.TotalMinutes != 60 {
		t.Errorf("上周应当只算 23:00–24:00 这一小时,实际 %d 分钟", prev.TotalMinutes)
	}

	// 本周:10-12 ~ 10-19
	cur, err := s.ReportBetween(at(12, 0, 0), at(19, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if cur.TotalMinutes != 60 {
		t.Errorf("本周应当只算 00:00–01:00 这一小时,实际 %d 分钟", cur.TotalMinutes)
	}

	// 两周加起来正好是那两小时,一分钟不多一分钟不少
	if prev.TotalMinutes+cur.TotalMinutes != 120 {
		t.Errorf("两周合计应当 120 分钟,实际 %d", prev.TotalMinutes+cur.TotalMinutes)
	}
}

// 待办没有实际投入,不该进周报 —— 这是这次改动有意改掉的口径
func TestReportExcludesTasksWithoutSpans(t *testing.T) {
	s := newTestStore(t)
	mustTodo(t, s, "还没开始的事", at(12, 9, 0), at(12, 12, 0))

	rep, err := s.ReportBetween(at(12, 0, 0), at(13, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if rep.TotalMinutes != 0 {
		t.Errorf("待办不该计入总时长,实际 %d 分钟", rep.TotalMinutes)
	}
	if rep.TaskCount != 0 {
		t.Errorf("待办不该出现在周报里,实际 %d 条", rep.TaskCount)
	}
	if len(rep.Groups) != 0 {
		t.Errorf("不该有分组,实际 %d 组", len(rep.Groups))
	}
}

// 一条事项的两段分别落在不同的天,周报要按段求和
func TestReportSumsMultipleSpans(t *testing.T) {
	s := newTestStore(t)
	dev := mustCat(t, s, "开发")
	task := mustTodo(t, s, "分两天做", at(12, 10, 0), at(13, 16, 0))

	if _, err := s.UpdateTask(task.ID, TaskPatch{CategoryID: &dev.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddSpan(task.ID, at(12, 10, 0), ptrTime(at(12, 11, 0))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddSpan(task.ID, at(13, 15, 0), ptrTime(at(13, 16, 0))); err != nil {
		t.Fatal(err)
	}

	rep, err := s.ReportBetween(at(12, 0, 0), at(14, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if rep.TotalMinutes != 120 {
		t.Errorf("两段合计应当 120 分钟,实际 %d", rep.TotalMinutes)
	}
	// 一条事项有多段,只算一条
	if rep.TaskCount != 1 {
		t.Errorf("事项数应当 1,实际 %d", rep.TaskCount)
	}
	if len(rep.Groups) != 1 || rep.Groups[0].Name != "开发" {
		t.Fatalf("分组不对: %+v", rep.Groups)
	}
	if rep.Groups[0].Minutes != 120 {
		t.Errorf("开发组应当 120 分钟,实际 %d", rep.Groups[0].Minutes)
	}
	if len(rep.Groups[0].Tasks) != 1 {
		t.Errorf("组里的事项应当只列一次,实际 %d", len(rep.Groups[0].Tasks))
	}
}

// ---------------------------------------------------------------- 迁移

// writeV1DB 手工造一个 v1 版本的库,用来验证升级路径。
func writeV1DB(t *testing.T, dir string) {
	t.Helper()
	dsn := "file:" + filepath.ToSlash(filepath.Join(dir, "worklog.db"))
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("打开裸库失败: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec(schemaV1); err != nil {
		t.Fatalf("建 v1 表失败: %v", err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 1`); err != nil {
		t.Fatalf("写版本号失败: %v", err)
	}

	base := at(12, 9, 0)
	const ins = `INSERT INTO tasks
	  (id,title,category_id,status,start_at,end_at,confirmed,source,created_at,updated_at)
	  VALUES (?,?,NULL,?,?,?,0,'ui',?,?)`
	rows := []struct {
		id, title, status string
		end               time.Time
	}{
		{"t-done", "做完的", "done", base.Add(time.Hour)},
		{"t-doing", "在做", "doing", base.Add(30 * time.Minute)},
		{"t-todo", "待办", "todo", base.Add(2 * time.Hour)},
		// 未定结束(零长跨度)的进行中事项:不该被回填出假时段
		{"t-open", "还没定几点结束", "doing", base},
	}
	for _, r := range rows {
		if _, err := db.Exec(ins, r.id, r.title, r.status,
			unix(base), unix(r.end), unix(base), unix(base)); err != nil {
			t.Fatalf("插入旧数据失败: %v", err)
		}
	}
}

func TestMigrateV1ToV2BackfillsSpans(t *testing.T) {
	dir := t.TempDir()
	writeV1DB(t, dir)

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("打开 v1 库失败: %v", err)
	}
	defer s.Close()

	var ver int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&ver); err != nil {
		t.Fatal(err)
	}
	if ver != schemaVersion {
		t.Fatalf("版本应当升到 %d,实际 %d", schemaVersion, ver)
	}

	now := time.Now()

	// 已完成:回填成一段闭区间,时长不变
	done, err := s.GetTask("t-done")
	if err != nil {
		t.Fatal(err)
	}
	if len(done.Spans) != 1 || done.Running() {
		t.Fatalf("已完成的事项应当有一段闭区间,实际 %+v", done.Spans)
	}
	if d := done.Duration(now); d != time.Hour {
		t.Errorf("回填后时长应当仍是 1h,实际 %v", d)
	}

	// 进行中:同样回填,时长不变
	doing, err := s.GetTask("t-doing")
	if err != nil {
		t.Fatal(err)
	}
	if len(doing.Spans) != 1 {
		t.Fatalf("进行中的事项应当有一段,实际 %d", len(doing.Spans))
	}
	if d := doing.Duration(now); d != 30*time.Minute {
		t.Errorf("回填后时长应当仍是 30m,实际 %v", d)
	}

	// 待办:不回填。计划区间不是实际投入,这正是这次改动要修正的口径。
	todo, err := s.GetTask("t-todo")
	if err != nil {
		t.Fatal(err)
	}
	if len(todo.Spans) != 0 {
		t.Errorf("待办不该被回填时段,实际 %d 段", len(todo.Spans))
	}
	if d := todo.Duration(now); d != 0 {
		t.Errorf("待办时长应当为 0,实际 %v", d)
	}

	// 未定结束(零长跨度)的进行中事项:也不回填。
	// 回填的话会造出一段零长的"实际投入",而它根本没计过时。
	open, err := s.GetTask("t-open")
	if err != nil {
		t.Fatal(err)
	}
	if len(open.Spans) != 0 {
		t.Errorf("零长跨度的事项不该被回填时段,实际 %d 段", len(open.Spans))
	}
}

// 零长跨度 = 未定结束,是合法的,不是"区间填反了"。
func TestCreateTaskAllowsOpenEnd(t *testing.T) {
	s := newTestStore(t)
	day := at(15, 9, 0)

	task, err := s.CreateTask(model.Task{
		Title: "还没想好做到几点", StartAt: day, EndAt: day, Status: model.StatusTodo,
	})
	if err != nil {
		t.Fatalf("未定结束应当能建: %v", err)
	}
	if !task.EndAt.Equal(task.StartAt) {
		t.Errorf("跨度应当是零长,实际 %v → %v", task.StartAt, task.EndAt)
	}

	// 照样能按日期列出来 —— 过滤只看 start_at,零长不影响
	from, to := at(15, 0, 0), at(16, 0, 0)
	list, err := s.ListTasks(TaskFilter{From: &from, To: &to})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != task.ID {
		t.Fatalf("未定结束的事项应当能被列出,实际 %d 条", len(list))
	}

	// 反向才是错的
	if _, err := s.CreateTask(model.Task{
		Title: "反了", StartAt: at(15, 10, 0), EndAt: at(15, 9, 0), Status: model.StatusTodo,
	}); err == nil {
		t.Error("结束早于开始应当报错")
	}
}

func TestUpdateTaskAllowsOpenEnd(t *testing.T) {
	s := newTestStore(t)
	task := mustTask(t, s, "x", at(15, 9, 0), at(15, 10, 0), "")

	day := at(15, 9, 0)
	if _, err := s.UpdateTask(task.ID, TaskPatch{EndAt: &day}); err != nil {
		t.Fatalf("把结束改成和开始一样(未定结束)应当允许: %v", err)
	}
	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.EndAt.Equal(got.StartAt) {
		t.Errorf("跨度应当塌成零长,实际 %v → %v", got.StartAt, got.EndAt)
	}

	// 反向仍然拒绝
	earlier := at(15, 8, 0)
	if _, err := s.UpdateTask(task.ID, TaskPatch{EndAt: &earlier}); err == nil {
		t.Error("结束早于开始应当报错")
	}
}

// 已经是 v2 的库再打开一次不该重复回填
func TestMigrateV2IsIdempotent(t *testing.T) {
	dir := t.TempDir()
	writeV1DB(t, dir)

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := s.GetTask("t-done")
	n := len(before.Spans)
	s.Close()

	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("二次打开失败: %v", err)
	}
	defer s2.Close()

	after, _ := s2.GetTask("t-done")
	if len(after.Spans) != n {
		t.Errorf("二次打开不该重复回填:%d → %d", n, len(after.Spans))
	}
}

// writeV2DB 造一个 v2 版本的库,里面塞进三类分类配色:
// 正常不撞的、精确重复的、以及"看着一样但字符串不同"的。
func writeV2DB(t *testing.T, dir string) {
	t.Helper()
	writeV1DB(t, dir)

	dsn := "file:" + filepath.ToSlash(filepath.Join(dir, "worklog.db"))
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("打开裸库失败: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec(schemaV2); err != nil {
		t.Fatalf("建 v2 表失败: %v", err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 2`); err != nil {
		t.Fatalf("写版本号失败: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM categories`); err != nil {
		t.Fatalf("清空分类失败: %v", err)
	}

	const ins = `INSERT INTO categories (id, name, color, sort_order, created_at)
	             VALUES (?,?,?,?,0)`
	cats := []struct{ id, name, color string }{
		{"c1", "需求编写", "#7c6bff"}, // 先出现的紫,保留
		{"c2", "会议", "#3b8fe8"},   // 和谁都不撞,保留
		{"c3", "问题排查", "#8b5cf6"}, // 和老紫 ΔE 只有 9.1,看着一样 —— 要换
		{"c4", "部署日常", "#3b8fe8"}, // 精确重复 —— 要换
		{"c5", "运营优化", "#c9a227"}, // 不撞,保留
	}
	for i, c := range cats {
		if _, err := db.Exec(ins, c.id, c.name, c.color, i); err != nil {
			t.Fatalf("插入分类失败: %v", err)
		}
	}
}

func TestMigrateV2ToV3DedupesCategoryColors(t *testing.T) {
	dir := t.TempDir()
	writeV2DB(t, dir)

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("打开 v2 库失败: %v", err)
	}
	defer s.Close()

	var ver int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&ver); err != nil {
		t.Fatal(err)
	}
	if ver != schemaVersion {
		t.Fatalf("版本应当升到 %d,实际 %d", schemaVersion, ver)
	}

	cats, err := s.ListCategories()
	if err != nil {
		t.Fatal(err)
	}
	if len(cats) != 5 {
		t.Fatalf("分类数不该变,期望 5,实际 %d", len(cats))
	}

	byID := map[string]string{}
	for _, c := range cats {
		byID[c.ID] = c.Color
	}

	// 不撞的原样保留 —— 迁移不该顺手重排用户的整个配色
	for _, id := range []string{"c1", "c2", "c5"} {
		if byID[id] != map[string]string{"c1": "#7c6bff", "c2": "#3b8fe8", "c5": "#c9a227"}[id] {
			t.Errorf("%s 的颜色不该被改,实际 %s", id, byID[id])
		}
	}
	// 撞的两个必须换掉
	if byID["c3"] == "#8b5cf6" {
		t.Errorf("c3 和老紫看着一样,应当被换掉")
	}
	if byID["c4"] == "#3b8fe8" {
		t.Errorf("c4 是精确重复,应当被换掉")
	}

	// 换完两两都得拉开
	for i := 0; i < len(cats); i++ {
		for j := i + 1; j < len(cats); j++ {
			if d := colorDist(cats[i].Color, cats[j].Color); d < colorMinDist {
				t.Errorf("%s(%s) 和 %s(%s) 仍然太接近:ΔE = %.1f",
					cats[i].Name, cats[i].Color, cats[j].Name, cats[j].Color, d)
			}
		}
	}
}

// 已经是 v3 的库再打开一次不该重复改写配色
func TestMigrateV3IsIdempotent(t *testing.T) {
	dir := t.TempDir()
	writeV2DB(t, dir)

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := s.ListCategories()
	s.Close()

	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("二次打开失败: %v", err)
	}
	defer s2.Close()

	after, _ := s2.ListCategories()
	if len(before) != len(after) {
		t.Fatalf("分类数变了:%d → %d", len(before), len(after))
	}
	for i := range before {
		if before[i].Color != after[i].Color {
			t.Errorf("%s 的颜色在二次打开时被改了:%s → %s",
				before[i].Name, before[i].Color, after[i].Color)
		}
	}
}

// writeV3DB 造一个 v3 版本的库,带几条备注。
//
// v3 没有 DDL(它只改分类配色数据),所以表结构和 v2 一样,
// 差别只在 user_version。
func writeV3DB(t *testing.T, dir string) {
	t.Helper()
	writeV2DB(t, dir)

	dsn := "file:" + filepath.ToSlash(filepath.Join(dir, "worklog.db"))
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("打开裸库失败: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec(`PRAGMA user_version = 3`); err != nil {
		t.Fatalf("写版本号失败: %v", err)
	}
	for i, b := range []string{"第一条备注", "第二条备注"} {
		if _, err := db.Exec(
			`INSERT INTO notes (id, task_id, body, source, created_at) VALUES (?,?,?,'ui',?)`,
			fmt.Sprintf("n%d", i), "t-done", b, 1000+i); err != nil {
			t.Fatalf("插入备注失败: %v", err)
		}
	}
}

func TestMigrateV3ToV4AddsUpdatedAt(t *testing.T) {
	dir := t.TempDir()
	writeV3DB(t, dir)

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("打开 v3 库失败: %v", err)
	}
	defer s.Close()

	var ver int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&ver); err != nil {
		t.Fatal(err)
	}
	if ver != schemaVersion {
		t.Fatalf("版本应当升到 %d,实际 %d", schemaVersion, ver)
	}

	notes, err := s.NotesOf("t-done")
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 2 {
		t.Fatalf("备注一条都不该丢,期望 2,实际 %d", len(notes))
	}
	for _, n := range notes {
		if n.UpdatedAt != nil {
			t.Errorf("老备注没被改过,updated_at 该是空,实际 %v", n.UpdatedAt)
		}
	}
}

// 新建分类时,用户手动挑的 #7C6BFF 不能被当成"还没用过"的色 ——
// 大小写归一化没做的话,这里会挑出一个看着重复的颜色。
func TestNextColorIgnoresHexCase(t *testing.T) {
	s := newTestStore(t)
	// 把种子分类全删掉,自己造两个:一个手动大写、一个走自动
	if _, err := s.db.Exec(`DELETE FROM categories`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO categories (id,name,color,sort_order,created_at) VALUES (?,?,?,0,0)`,
		"cx", "手动挑的", "#7C6BFF"); err != nil {
		t.Fatal(err)
	}

	got, err := s.nextColor()
	if err != nil {
		t.Fatal(err)
	}
	if normHex(got) == "#7c6bff" {
		t.Errorf("大写形式的 #7C6BFF 已被占用,不该再挑出 %s", got)
	}
}

// 周报里每条事项的时长必须等于它在本区间内的贡献。
//
// 跨周的事项如果显示全量时长,和它所在分组的数字就自相矛盾了 ——
// 分组说 1 小时,点开里面那条却写着 2 小时。
func TestReportTaskDurationMatchesContribution(t *testing.T) {
	s := newTestStore(t)
	task := mustTodo(t, s, "通宵的那件事", at(11, 23, 0), at(12, 1, 0))
	if _, err := s.AddSpan(task.ID, at(11, 23, 0), ptrTime(at(12, 1, 0))); err != nil {
		t.Fatal(err)
	}

	// 只看后一半(周一 00:00 起)
	rep, err := s.ReportBetween(at(12, 0, 0), at(19, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Groups) != 1 || len(rep.Groups[0].Tasks) != 1 {
		t.Fatalf("应当 1 组 1 条,实际 %+v", rep.Groups)
	}

	got := int(rep.Groups[0].Tasks[0].Duration(time.Now()).Minutes())
	if got != 60 {
		t.Errorf("事项在本区间内的时长应当 60 分钟,实际 %d", got)
	}
	if got != rep.Groups[0].Minutes {
		t.Errorf("事项时长 %d 与分组合计 %d 对不上", got, rep.Groups[0].Minutes)
	}
	// 挂上的应当是裁剪后的段,不是原始那一段
	if n := len(rep.Groups[0].Tasks[0].Spans); n != 1 {
		t.Errorf("应当挂 1 段裁剪后的时段,实际 %d", n)
	}
}

// 取整口径必须只有一条:分组数字 == 组内各事项之和 == 各段之和。
//
// 先求总和再取整、和逐段取整再求和,这两个算法在带秒的时段上会差出整整
// 一分钟 —— 表现就是分组显示 16 分钟、组里唯一那条事项却显示 17 分钟。
func TestReportMinutesAddUp(t *testing.T) {
	s := newTestStore(t)
	dev := mustCat(t, s, "开发")
	task := mustTodo(t, s, "带秒的碎段", at(12, 10, 0), at(12, 11, 0))
	if _, err := s.UpdateTask(task.ID, TaskPatch{CategoryID: &dev.ID}); err != nil {
		t.Fatal(err)
	}

	// 12m59s 和 4m30s:逐段四舍五入是 13 + 5 = 18;
	// 先求和(17m29s)再四舍五入是 17。两条路径必须只留一条。
	a := at(12, 10, 0)
	b := a.Add(12*time.Minute + 59*time.Second)
	c := at(12, 14, 0)
	d := c.Add(4*time.Minute + 30*time.Second)
	if _, err := s.AddSpan(task.ID, a, &b); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddSpan(task.ID, c, &d); err != nil {
		t.Fatal(err)
	}

	rep, err := s.ReportBetween(at(12, 0, 0), at(13, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Groups) != 1 {
		t.Fatalf("应当 1 组,实际 %d", len(rep.Groups))
	}

	now := time.Now()
	g := rep.Groups[0]
	sum := 0
	for _, tk := range g.Tasks {
		sum += tk.Minutes(now)
	}
	if g.Minutes != sum {
		t.Errorf("分组合计 %d 与组内事项之和 %d 对不上", g.Minutes, sum)
	}
	if rep.TotalMinutes != g.Minutes {
		t.Errorf("总时长 %d 与唯一分组的 %d 对不上", rep.TotalMinutes, g.Minutes)
	}
	if g.Minutes != 18 {
		t.Errorf("逐段四舍五入应当是 13+5=18 分钟,实际 %d", g.Minutes)
	}

	// 事项自己的时长也要等于它各段之和
	tk := g.Tasks[0]
	segSum := 0
	for _, sp := range tk.Spans {
		segSum += sp.Minutes(now)
	}
	if tk.Minutes(now) != segSum {
		t.Errorf("事项时长 %d 与它各段之和 %d 对不上", tk.Minutes(now), segSum)
	}
}
