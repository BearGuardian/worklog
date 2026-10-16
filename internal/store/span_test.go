package store

import (
	"errors"
	"testing"
	"time"

	"worklog/internal/model"
)

// mustTodo 建一条待办。待办没有时段 —— 这正是"时长 = 实际投入"的前提。
func mustTodo(t *testing.T, s *Store, title string, start, end time.Time) model.Task {
	t.Helper()
	task, err := s.CreateTask(model.Task{
		Title: title, Status: model.StatusTodo, StartAt: start, EndAt: end,
	})
	if err != nil {
		t.Fatalf("建待办失败: %v", err)
	}
	return *task
}

// 用户要的场景:上午做一段、下午接着做,中间停掉的时间不算。
func TestSpansAcrossGap(t *testing.T) {
	s := newTestStore(t)
	task := mustTodo(t, s, "跨时段的事", at(12, 9, 0), at(12, 17, 0))

	if len(task.Spans) != 0 {
		t.Fatalf("待办不该有时段,实际 %d 段", len(task.Spans))
	}
	if task.Duration(nowFunc()) != 0 {
		t.Errorf("待办时长为 0,实际 %v", task.Duration(nowFunc()))
	}

	// 上午 10–11
	if _, err := s.AddSpan(task.ID, at(12, 10, 0), ptrTime(at(12, 11, 0))); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetTask(task.ID)
	if d := got.Duration(nowFunc()); d != time.Hour {
		t.Errorf("第一段后时长应当 1h,实际 %v", d)
	}
	// 跨度收成这一段
	if !got.StartAt.Equal(at(12, 10, 0)) || !got.EndAt.Equal(at(12, 11, 0)) {
		t.Errorf("跨度应当收成 10:00–11:00,实际 %s–%s",
			got.StartAt.Format("15:04"), got.EndAt.Format("15:04"))
	}

	// 下午 15–16
	if _, err := s.AddSpan(task.ID, at(12, 15, 0), ptrTime(at(12, 16, 0))); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetTask(task.ID)
	if d := got.Duration(nowFunc()); d != 2*time.Hour {
		t.Errorf("两段合计应当 2h,实际 %v", d)
	}
	if len(got.Spans) != 2 {
		t.Fatalf("应当有 2 段,实际 %d", len(got.Spans))
	}
	// 跨度是两段的包络,覆盖中间的空档
	if !got.StartAt.Equal(at(12, 10, 0)) || !got.EndAt.Equal(at(12, 16, 0)) {
		t.Errorf("跨度应当是 10:00–16:00,实际 %s–%s",
			got.StartAt.Format("15:04"), got.EndAt.Format("15:04"))
	}

	// 删掉下午那段,时长和跨度都要跟着回退
	if err := s.DeleteSpan(got.Spans[1].ID); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetTask(task.ID)
	if d := got.Duration(nowFunc()); d != time.Hour {
		t.Errorf("删掉一段后应当 1h,实际 %v", d)
	}
	if !got.EndAt.Equal(at(12, 11, 0)) {
		t.Errorf("跨度应当回退到 11:00,实际 %s", got.EndAt.Format("15:04"))
	}
}

// 正在跑的段:没有结束时间,时长算到当下
func TestOpenSpanCountsToNow(t *testing.T) {
	s := newTestStore(t)
	task := mustTodo(t, s, "正在做", at(12, 9, 0), at(12, 10, 0))

	base := at(12, 10, 0)
	nowFunc = func() time.Time { return base }
	defer func() { nowFunc = time.Now }()

	if _, err := s.AddSpan(task.ID, base, nil); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetTask(task.ID)
	if !got.Running() {
		t.Fatal("应当处于正在跑的状态")
	}

	nowFunc = func() time.Time { return base.Add(45 * time.Minute) }
	got, _ = s.GetTask(task.ID)
	if d := got.Duration(nowFunc()); d != 45*time.Minute {
		t.Errorf("跑着的段应当算到当下(45m),实际 %v", d)
	}

	// 关掉之后时长就定死了,不再随 now 变
	closed, err := s.CloseOpenSpan(task.ID, base.Add(50*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if !closed {
		t.Fatal("应当关掉了一段")
	}
	got, _ = s.GetTask(task.ID)
	if got.Running() {
		t.Error("关掉之后不该还在跑")
	}
	if d := got.Duration(nowFunc()); d != 50*time.Minute {
		t.Errorf("关掉后应当固定 50m,实际 %v", d)
	}
}

// 没有开着的段时再关一次是安全的 —— 暂停按钮连点两下不该报错
func TestCloseOpenSpanIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	task := mustTodo(t, s, "连点两下", at(12, 9, 0), at(12, 10, 0))

	closed, err := s.CloseOpenSpan(task.ID, at(12, 10, 0))
	if err != nil {
		t.Fatalf("没有在跑的段时不该报错: %v", err)
	}
	if closed {
		t.Error("没有在跑的段,不该报告关掉了一段")
	}
}

// 时段的区间校验
func TestAddSpanValidation(t *testing.T) {
	s := newTestStore(t)
	task := mustTodo(t, s, "校验", at(12, 9, 0), at(12, 10, 0))

	cases := []struct {
		name       string
		start, end time.Time
	}{
		{"结束早于开始", at(12, 11, 0), at(12, 10, 0)},
		{"首尾相同", at(12, 10, 0), at(12, 10, 0)},
	}
	for _, c := range cases {
		if _, err := s.AddSpan(task.ID, c.start, ptrTime(c.end)); err == nil {
			t.Errorf("%s:应当报错", c.name)
		}
	}

	// 不存在的事项要给出清楚的错误,而不是外键报错
	_, err := s.AddSpan("不存在", at(12, 10, 0), ptrTime(at(12, 11, 0)))
	if !errors.Is(err, ErrTaskNotFound) {
		t.Errorf("事项不存在时应当返回 ErrTaskNotFound,实际 %v", err)
	}
}

// 建事项时按状态决定时段:待办没有,进行中开一段,已完成闭一段
func TestCreateTaskSeedsSpanByStatus(t *testing.T) {
	s := newTestStore(t)

	todo, err := s.CreateTask(model.Task{
		Title: "待办", Status: model.StatusTodo, StartAt: at(12, 9, 0), EndAt: at(12, 10, 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(todo.Spans) != 0 {
		t.Errorf("待办不该有时段,实际 %d", len(todo.Spans))
	}

	doing, err := s.CreateTask(model.Task{
		Title: "在做", Status: model.StatusDoing, StartAt: at(12, 9, 0), EndAt: at(12, 10, 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(doing.Spans) != 1 || !doing.Running() {
		t.Errorf("进行中应当有一段开着的时段,实际 %+v", doing.Spans)
	}

	done, err := s.CreateTask(model.Task{
		Title: "做完", Status: model.StatusDone, StartAt: at(12, 9, 0), EndAt: at(12, 10, 30),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(done.Spans) != 1 || done.Running() {
		t.Errorf("已完成应当有一段闭区间,实际 %+v", done.Spans)
	}
	if d := done.Duration(nowFunc()); d != 90*time.Minute {
		t.Errorf("已完成时长应当 90m,实际 %v", d)
	}
}

// 删掉全部时段后:时长归零,但跨度留在最后一次算出的位置
func TestEnvelopeKeptWhenAllSpansRemoved(t *testing.T) {
	s := newTestStore(t)
	task := mustTodo(t, s, "计划 14–15", at(12, 14, 0), at(12, 15, 0))

	sp, err := s.AddSpan(task.ID, at(12, 10, 0), ptrTime(at(12, 11, 0)))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSpan(sp.ID); err != nil {
		t.Fatal(err)
	}

	got, _ := s.GetTask(task.ID)
	if len(got.Spans) != 0 {
		t.Fatalf("时段应当被删光,实际 %d 段", len(got.Spans))
	}
	if d := got.Duration(time.Now()); d != 0 {
		t.Errorf("没有时段时时长应当为 0,实际 %v", d)
	}
	// 跨度不会回到最初的 14:00–15:00 —— 那个值在第一次算包络时就被覆盖了。
	// 留在这里更符合直觉:那条事项确实是在 10–11 做的,只是记录被删了。
	if !got.StartAt.Equal(at(12, 10, 0)) || !got.EndAt.Equal(at(12, 11, 0)) {
		t.Errorf("跨度应当停在最后一次算出的位置(10:00–11:00),实际 %s–%s",
			got.StartAt.Format("15:04"), got.EndAt.Format("15:04"))
	}
}

func ptrTime(t time.Time) *time.Time { return &t }
