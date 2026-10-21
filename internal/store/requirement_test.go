package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"worklog/internal/model"
)

func mustComp(t *testing.T, s *Store, name string) *model.Component {
	t.Helper()
	c, err := s.EnsureComponent(name)
	if err != nil {
		t.Fatalf("建组件 %s 失败: %v", name, err)
	}
	if c == nil {
		t.Fatalf("组件 %s 应当被创建", name)
	}
	return c
}

// mustReq 建一条需求,并冻结"现在"让 start_at 可预期。
func mustReq(t *testing.T, s *Store, title, compID string, now time.Time) *model.Requirement {
	t.Helper()
	prev := nowFunc
	nowFunc = func() time.Time { return now }
	defer func() { nowFunc = prev }()

	r, err := s.CreateRequirement(model.Requirement{
		Title: title, ComponentID: compID, Source: model.SourceUI,
	})
	if err != nil {
		t.Fatalf("建需求 %s 失败: %v", title, err)
	}
	return r
}

// ---------------------------------------------------------------- 季度

func TestQuarter(t *testing.T) {
	cases := []struct {
		month int
		want  int
	}{
		{1, 1}, {3, 1}, // 一季度边界
		{4, 2}, {6, 2},
		{7, 3}, {9, 3},
		{10, 4}, {12, 4}, // 四季度边界
	}
	for _, c := range cases {
		got := model.Quarter(time.Date(2026, time.Month(c.month), 15, 0, 0, 0, 0, time.Local))
		if got != c.want {
			t.Errorf("%d 月应当是 Q%d,实际 Q%d", c.month, c.want, got)
		}
	}
}

func TestQuarterLabel(t *testing.T) {
	got := model.QuarterLabel(time.Date(2026, 10, 20, 0, 0, 0, 0, time.Local))
	if got != "2026 Q4" {
		t.Errorf("期望 2026 Q4,实际 %s", got)
	}
}

// ---------------------------------------------------------------- 阶段不变量

func TestCreateRequirementDefaultsToSubmit(t *testing.T) {
	s := newTestStore(t)
	r := mustReq(t, s, "没指定阶段", "", at(20, 10, 0))

	if r.Stage != model.StageSubmit {
		t.Errorf("默认阶段该是 submit,实际 %s", r.Stage)
	}
	if r.OnlineAt != nil {
		t.Errorf("刚建的需求不该有上线时间,实际 %v", r.OnlineAt)
	}
	if !r.StartAt.Equal(at(20, 10, 0)) {
		t.Errorf("开始时间该是添加时间 %v,实际 %v", at(20, 10, 0), r.StartAt)
	}
}

func TestStageOnlineSetsOnlineAt(t *testing.T) {
	s := newTestStore(t)
	r := mustReq(t, s, "标上线", "", at(20, 10, 0))

	stage := model.StageOnline
	got, err := s.UpdateRequirement(r.ID, RequirementPatch{Stage: &stage})
	if err != nil {
		t.Fatal(err)
	}
	if got.Stage != model.StageOnline {
		t.Fatalf("阶段该是 online,实际 %s", got.Stage)
	}
	if got.OnlineAt == nil {
		t.Fatal("只标了上线阶段,上线时间该自动补上")
	}
}

func TestOnlineAtImpliesStage(t *testing.T) {
	s := newTestStore(t)
	r := mustReq(t, s, "直接给上线时间", "", at(20, 10, 0))

	when := at(22, 16, 0)
	got, err := s.UpdateRequirement(r.ID, RequirementPatch{OnlineAt: &when})
	if err != nil {
		t.Fatal(err)
	}
	if got.Stage != model.StageOnline {
		t.Errorf("给了上线时间,阶段该一并变成 online,实际 %s", got.Stage)
	}
	if got.OnlineAt == nil || !got.OnlineAt.Equal(when) {
		t.Errorf("上线时间该是 %v,实际 %v", when, got.OnlineAt)
	}
}

func TestStageRegressionClearsOnlineAt(t *testing.T) {
	s := newTestStore(t)
	r := mustReq(t, s, "上线又退回", "", at(20, 10, 0))

	online := model.StageOnline
	if _, err := s.UpdateRequirement(r.ID, RequirementPatch{Stage: &online}); err != nil {
		t.Fatal(err)
	}

	// 退回到验收 —— 它确实不再算上线了,留着旧日期会让季度统计撒谎
	accept := model.StageAccept
	got, err := s.UpdateRequirement(r.ID, RequirementPatch{Stage: &accept})
	if err != nil {
		t.Fatal(err)
	}
	if got.Stage != model.StageAccept {
		t.Fatalf("阶段该是 accept,实际 %s", got.Stage)
	}
	if got.OnlineAt != nil {
		t.Errorf("退回上线之前,上线时间该清空,实际 %v", got.OnlineAt)
	}
}

// 重复标记上线不该把日期改成"现在" —— 那会让季度统计悄悄漂走。
func TestRemarkingOnlineKeepsDate(t *testing.T) {
	s := newTestStore(t)
	r := mustReq(t, s, "重复标上线", "", at(20, 10, 0))

	online := model.StageOnline
	first, err := s.UpdateRequirement(r.ID, RequirementPatch{Stage: &online})
	if err != nil {
		t.Fatal(err)
	}

	// 把"现在"往后拨一天再标一次
	prev := nowFunc
	nowFunc = func() time.Time { return at(21, 9, 0) }
	defer func() { nowFunc = prev }()

	again, err := s.UpdateRequirement(r.ID, RequirementPatch{Stage: &online})
	if err != nil {
		t.Fatal(err)
	}
	if again.OnlineAt == nil || !again.OnlineAt.Equal(*first.OnlineAt) {
		t.Errorf("重复标上线不该改日期:第一次 %v,第二次 %v", first.OnlineAt, again.OnlineAt)
	}
}

// 只改标题不该碰 stage / online_at —— resolveStage 收不到 patch 时
// 必须原样保留,而不是把 online_at 清掉。
func TestTitleOnlyPatchKeepsStage(t *testing.T) {
	s := newTestStore(t)
	r := mustReq(t, s, "原标题", "", at(20, 10, 0))

	online := model.StageOnline
	if _, err := s.UpdateRequirement(r.ID, RequirementPatch{Stage: &online}); err != nil {
		t.Fatal(err)
	}

	title := "新标题"
	got, err := s.UpdateRequirement(r.ID, RequirementPatch{Title: &title})
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "新标题" {
		t.Errorf("标题该改掉,实际 %q", got.Title)
	}
	if got.Stage != model.StageOnline || got.OnlineAt == nil {
		t.Errorf("只改标题不该动阶段:stage=%s onlineAt=%v", got.Stage, got.OnlineAt)
	}
}

// 直接建一条"已上线"的需求,上线时间也得当场补上。
func TestCreateOnlineRequirementGetsDate(t *testing.T) {
	s := newTestStore(t)
	prev := nowFunc
	nowFunc = func() time.Time { return at(20, 10, 0) }
	defer func() { nowFunc = prev }()

	r, err := s.CreateRequirement(model.Requirement{
		Title: "建就是已上线", Stage: model.StageOnline, Source: model.SourceUI,
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.OnlineAt == nil {
		t.Fatal("建的时候就是 online,上线时间该自动补上")
	}
}

func TestUpdateRequirementRejectsBadStage(t *testing.T) {
	s := newTestStore(t)
	r := mustReq(t, s, "阶段非法", "", at(20, 10, 0))

	bogus := model.Stage("shipped")
	// patch 里的阶段不校验 —— 是 CREATE 时校验。这里确认 CHECK 约束会拦住它。
	if _, err := s.UpdateRequirement(r.ID, RequirementPatch{Stage: &bogus}); err == nil {
		t.Fatal("非法阶段该被 CHECK 约束拦住")
	}
}

// ---------------------------------------------------------------- 组件

func TestDeleteComponentKeepsRequirements(t *testing.T) {
	s := newTestStore(t)
	c := mustComp(t, s, "axeops")
	r := mustReq(t, s, "挂在组件上的需求", c.ID, at(20, 10, 0))

	if err := s.DeleteComponent(c.ID); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetRequirement(r.ID)
	if err != nil {
		t.Fatalf("删组件不该删掉需求: %v", err)
	}
	if got.ComponentID != "" {
		t.Errorf("组件的引用该被置空,实际 %q", got.ComponentID)
	}
	if got.Component != nil {
		t.Errorf("组件已经删了,不该还挂着,实际 %v", got.Component)
	}
}

func TestEnsureComponentIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	a := mustComp(t, s, "xdr")
	b := mustComp(t, s, "xdr")
	if a.ID != b.ID {
		t.Errorf("同名组件该复用,拿到两个 ID: %s / %s", a.ID, b.ID)
	}
}

// 两个组件不该拿到同一个颜色 —— 组件表自己一套去重。
func TestComponentColorsDistinct(t *testing.T) {
	s := newTestStore(t)
	a := mustComp(t, s, "第一个")
	b := mustComp(t, s, "第二个")
	if a.Color == b.Color {
		t.Errorf("两个组件撞色了: %s", a.Color)
	}
}

// 组件和分类各查各的表,互不干扰。
func TestComponentColorIgnoresCategories(t *testing.T) {
	s := newTestStore(t)
	// 清空分类,让分类表一个颜色都不占
	if _, err := s.db.Exec(`DELETE FROM categories`); err != nil {
		t.Fatal(err)
	}
	c := mustComp(t, s, "唯一组件")
	if c.Color == "" {
		t.Error("组件该拿到一个颜色")
	}
}

// ---------------------------------------------------------------- 过滤与排序

func TestListRequirementsOrder(t *testing.T) {
	s := newTestStore(t)
	prev := nowFunc
	nowFunc = func() time.Time { return at(20, 10, 0) }
	defer func() { nowFunc = prev }()

	// 三条:两条未上线(新旧不同),一条已上线
	old := mustReq(t, s, "旧的未上线", "", at(1, 9, 0))
	recent := mustReq(t, s, "新的未上线", "", at(18, 9, 0))
	done := mustReq(t, s, "已上线", "", at(5, 9, 0))
	if _, err := s.UpdateRequirement(done.ID, RequirementPatch{OnlineAt: ptrTo(at(10, 9, 0))}); err != nil {
		t.Fatal(err)
	}

	list, err := s.ListRequirements(RequirementFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("期望 3 条,实际 %d", len(list))
	}
	want := []string{recent.ID, old.ID, done.ID}
	for i, id := range want {
		if list[i].ID != id {
			t.Errorf("第 %d 条该是 %s(%s),实际 %s(%s)",
				i, id, map[string]string{recent.ID: "新的未上线", old.ID: "旧的未上线", done.ID: "已上线"}[id],
				list[i].ID, list[i].Title)
		}
	}
}

func TestListRequirementsFilterByComponent(t *testing.T) {
	s := newTestStore(t)
	a := mustComp(t, s, "组件甲")
	b := mustComp(t, s, "组件乙")

	mustReq(t, s, "甲的需求", a.ID, at(20, 10, 0))
	mustReq(t, s, "乙的需求", b.ID, at(20, 11, 0))
	mustReq(t, s, "没组件的需求", "", at(20, 12, 0))

	onlyA, err := s.ListRequirements(RequirementFilter{Component: a.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(onlyA) != 1 || onlyA[0].Title != "甲的需求" {
		t.Errorf("按组件过滤结果不对: %v", titles(onlyA))
	}

	// 哨兵值:只看未指定组件的
	none, err := s.ListRequirements(RequirementFilter{Component: NoComponent})
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 1 || none[0].Title != "没组件的需求" {
		t.Errorf("NoComponent 过滤结果不对: %v", titles(none))
	}
}

func TestListRequirementsFilterByQuery(t *testing.T) {
	s := newTestStore(t)
	mustReq(t, s, "告警分析 Agent 联调", "", at(20, 10, 0))
	mustReq(t, s, "巡检报告导出", "", at(20, 11, 0))

	got, err := s.ListRequirements(RequirementFilter{Query: "告警"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("关键词过滤该只中一条,实际 %v", titles(got))
	}
}

// ---------------------------------------------------------------- 统计

func TestRequirementStatsByComponent(t *testing.T) {
	s := newTestStore(t)
	a := mustComp(t, s, "axeops")
	b := mustComp(t, s, "xdr")

	// axeops:3 条,2 条上线
	for _, tt := range []struct {
		title string
		on    bool
	}{
		{"甲一", true}, {"甲二", true}, {"甲三", false},
	} {
		r := mustReq(t, s, tt.title, a.ID, at(20, 10, 0))
		if tt.on {
			if _, err := s.UpdateRequirement(r.ID, RequirementPatch{OnlineAt: ptrTo(at(20, 15, 0))}); err != nil {
				t.Fatal(err)
			}
		}
	}
	// xdr:1 条,0 条上线
	mustReq(t, s, "乙一", b.ID, at(20, 10, 0))

	st, err := s.RequirementStatsOf()
	if err != nil {
		t.Fatal(err)
	}
	if st.Total != 4 || st.Online != 2 {
		t.Fatalf("总数/上线数不对: %d / %d", st.Total, st.Online)
	}

	byName := map[string]ComponentCount{}
	for _, c := range st.Components {
		byName[c.Name] = c
	}
	if got := byName["axeops"]; got.Total != 3 || got.Online != 2 {
		t.Errorf("axeops 该是 2/3,实际 %d/%d", got.Online, got.Total)
	}
	if p := byName["axeops"].Percent(); p < 66.6 || p > 66.7 {
		t.Errorf("axeops 完成度该约 66.7%%,实际 %.1f", p)
	}
	if got := byName["xdr"]; got.Total != 1 || got.Online != 0 {
		t.Errorf("xdr 该是 0/1,实际 %d/%d", got.Online, got.Total)
	}
}

func TestRequirementStatsCountsUnassigned(t *testing.T) {
	s := newTestStore(t)
	mustReq(t, s, "没组件的", "", at(20, 10, 0))

	st, err := s.RequirementStatsOf()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Components) != 1 {
		t.Fatalf("未指定组件的需求也得占一格,实际 %d 格", len(st.Components))
	}
	if st.Components[0].Name != "未指定组件" || st.Components[0].Total != 1 {
		t.Errorf("未指定组件那一格不对: %+v", st.Components[0])
	}
}

// 跨年:12 月的上线时间属于当年 Q4,不能串到次年 Q1。
func TestRequirementStatsByQuarter(t *testing.T) {
	s := newTestStore(t)

	// 2026 Q4 / 2026 Q4 / 2027 Q1 / 未上线(不计入季度)
	for _, when := range []time.Time{
		time.Date(2026, 10, 5, 9, 0, 0, 0, time.Local),
		time.Date(2026, 12, 31, 23, 0, 0, 0, time.Local),
		time.Date(2027, 1, 1, 0, 30, 0, 0, time.Local),
	} {
		r := mustReq(t, s, "需求 "+when.Format("2006-01-02"), "", at(20, 10, 0))
		w := when
		if _, err := s.UpdateRequirement(r.ID, RequirementPatch{OnlineAt: &w}); err != nil {
			t.Fatal(err)
		}
	}
	mustReq(t, s, "还没上线", "", at(20, 11, 0))

	st, err := s.RequirementStatsOf()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Quarters) != 2 {
		t.Fatalf("该有 2 个季度,实际 %d: %+v", len(st.Quarters), st.Quarters)
	}
	// 最近的在前
	if st.Quarters[0].Year != 2027 || st.Quarters[0].Quarter != 1 || st.Quarters[0].Online != 1 {
		t.Errorf("第一格该是 2027 Q1 共 1 条,实际 %+v", st.Quarters[0])
	}
	if st.Quarters[1].Year != 2026 || st.Quarters[1].Quarter != 4 || st.Quarters[1].Online != 2 {
		t.Errorf("第二格该是 2026 Q4 共 2 条,实际 %+v", st.Quarters[1])
	}
	if st.Total != 4 || st.Online != 3 {
		t.Errorf("总数/上线数不对: %d / %d", st.Total, st.Online)
	}
}

// ---------------------------------------------------------------- 删除

func TestDeleteRequirement(t *testing.T) {
	s := newTestStore(t)
	r := mustReq(t, s, "要删的", "", at(20, 10, 0))

	if err := s.DeleteRequirement(r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetRequirement(r.ID); !errors.Is(err, ErrRequirementNotFound) {
		t.Errorf("删完该查不到,实际 %v", err)
	}
	// 再删一次要报"不存在",而不是静默成功
	if err := s.DeleteRequirement(r.ID); !errors.Is(err, ErrRequirementNotFound) {
		t.Errorf("重复删除该报不存在,实际 %v", err)
	}
}

// ---------------------------------------------------------------- 迁移

func writeV4DB(t *testing.T, dir string) {
	t.Helper()
	writeV3DB(t, dir)

	dsn := "file:" + filepath.ToSlash(filepath.Join(dir, "worklog.db"))
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("打开裸库失败: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec(`ALTER TABLE notes ADD COLUMN updated_at INTEGER`); err != nil {
		t.Fatalf("加 updated_at 失败: %v", err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 4`); err != nil {
		t.Fatalf("写版本号失败: %v", err)
	}
}

func TestMigrateV4ToV5AddsRequirementTables(t *testing.T) {
	dir := t.TempDir()
	writeV4DB(t, dir)

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("打开 v4 库失败: %v", err)
	}
	defer s.Close()

	var ver int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&ver); err != nil {
		t.Fatal(err)
	}
	if ver != schemaVersion {
		t.Fatalf("版本应当升到 %d,实际 %d", schemaVersion, ver)
	}

	// 老数据一条不少
	tasks, err := s.ListTasks(TaskFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) == 0 {
		t.Fatal("迁移后事项不该丢")
	}
	notes, err := s.NotesOf("t-done")
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 2 {
		t.Fatalf("迁移后备注不该丢,期望 2,实际 %d", len(notes))
	}

	// 新表建好且是空的 —— 组件不 seed
	comps, err := s.ListComponents()
	if err != nil {
		t.Fatal(err)
	}
	if len(comps) != 0 {
		t.Errorf("组件不该有种子数据,实际 %d 个", len(comps))
	}

	// 新表真的能用
	r := mustReq(t, s, "迁移后建的需求", "", at(20, 10, 0))
	if r.ID == "" {
		t.Fatal("迁移后该能建需求")
	}
}

// ptrTo 是 time.Time 的取址简写。
func ptrTo(t time.Time) *time.Time { return &t }

func titles(rs []model.Requirement) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.Title)
	}
	return out
}
