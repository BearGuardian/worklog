// Package model 定义 worklog 的核心数据结构。
package model

import (
	"fmt"
	"time"
)

// Status 是事项的状态。
type Status string

const (
	StatusTodo  Status = "todo"  // 待办
	StatusDoing Status = "doing" // 进行中
	StatusDone  Status = "done"  // 已完成
)

// Valid 报告状态是否是三个合法值之一。
func (s Status) Valid() bool {
	switch s {
	case StatusTodo, StatusDoing, StatusDone:
		return true
	}
	return false
}

// Source 标记一条记录是谁写的 —— 界面、热键,还是 Claude Code。
//
// 备注时间线上两者要用不同颜色区分,所以这个信息必须落库。
type Source string

const (
	SourceUI  Source = "ui"
	SourceMCP Source = "mcp"
)

// Category 是事项的分类。受控集合,有颜色。
type Category struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Color     string    `json:"color"`
	SortOrder int       `json:"sortOrder"`
	CreatedAt time.Time `json:"createdAt"`
}

// Task 是一条工作事项。
//
// StartAt / EndAt 是这条事项的**跨度**:一段连续的时间范围,用来在时间轴上
// 定位和筛选。真正投入的时间在 Spans 里 —— 一条事项可以中途暂停、下午接着做,
// 跨度覆盖整个过程,而时长只算实际做的那几段。
//
// 没有时段的事项(待办)跨度就是计划时段,时长是 0。
type Task struct {
	ID         string    `json:"id"`
	Title      string    `json:"title"`
	CategoryID string    `json:"categoryId,omitempty"` // 空表示未分类
	Status     Status    `json:"status"`
	StartAt    time.Time `json:"startAt"`
	EndAt      time.Time `json:"endAt"`
	Confirmed  bool      `json:"confirmed"` // 时长是否经人确认过
	Source     Source    `json:"source"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`

	// 以下是查询时按需填充的关联数据,不是 tasks 表的列。
	Category *Category `json:"category,omitempty"`
	Notes    []Note    `json:"notes,omitempty"`
	Spans    []Span    `json:"spans,omitempty"`
}

// Span 是一段实际投入的时间。
//
// EndAt 为 nil 表示这一段还在跑 —— 用可空列而不是额外的布尔字段来表达
// "正在进行",是因为两者本来就是同一件事,分成两处迟早会不一致。
type Span struct {
	ID        string     `json:"id"`
	TaskID    string     `json:"taskId"`
	StartAt   time.Time  `json:"startAt"`
	EndAt     *time.Time `json:"endAt"`
	CreatedAt time.Time  `json:"createdAt"`
}

// Duration 返回这一段的时间长度。还在跑的段算到 now。
func (s Span) Duration(now time.Time) time.Duration {
	if s.EndAt == nil {
		if d := now.Sub(s.StartAt); d > 0 {
			return d
		}
		return 0
	}
	return s.EndAt.Sub(s.StartAt)
}

// Minutes 返回这一段四舍五入到分钟的时长。
func (s Span) Minutes(now time.Time) int { return RoundMinutes(s.Duration(now)) }

// Open 报告这一段是否还在跑。
func (s Span) Open() bool { return s.EndAt == nil }

// RoundMinutes 把时长四舍五入到分钟。
//
// 用四舍五入而不是截断:截断会让 59 秒的段显示成 0 分钟,而那是真实做过的
// 时间,不该凭空消失。
//
// 这个函数是**全局唯一**的取整口径。时长必须逐段取整再求和,不能先求总和
// 再取整 —— 两条路径混用会让分组显示 16 分钟、组里唯一那条事项却显示 17 分钟。
func RoundMinutes(d time.Duration) int {
	if d <= 0 {
		return 0
	}
	return int((d + 30*time.Second) / time.Minute)
}

// Duration 返回事项的实际投入时长,即各时段之和。
//
// 同一事项的时段不该互相重叠(暂停后才会开新的一段),所以求和即可。
// 跨任务的墙钟并集是另一回事,由 store 的报表口径负责。
//
// now 显式传入而不是内部取 time.Now():还在跑的那段要算到"此刻",
// 而这个"此刻"必须是调用方的那一个 —— 否则测试没法冻结时间。
func (t Task) Duration(now time.Time) time.Duration {
	var d time.Duration
	for _, s := range t.Spans {
		d += s.Duration(now)
	}
	return d
}

// Minutes 返回事项的时长,按**逐段取整再求和**算。
//
// 和 Duration 的区别只在取整口径上,但这个区别是可见的:
// 直接用 Duration 取整会和分组数字对不上(见 RoundMinutes)。
func (t Task) Minutes(now time.Time) int {
	total := 0
	for _, s := range t.Spans {
		total += s.Minutes(now)
	}
	return total
}

// Running 报告事项此刻是否有时段在跑。
//
// "已暂停"不单独做状态:status 仍是 doing,只是没有开着的时段。
// 状态管的是生命周期,时段管的是计时,两者分开就不会互相打架。
func (t Task) Running() bool {
	for _, s := range t.Spans {
		if s.Open() {
			return true
		}
	}
	return false
}

// Note 是挂在事项上的一条备注。
//
// UpdatedAt 为 nil 表示从没改过。改动**不动 CreatedAt** —— 备注在时间线上的
// 位置由它决定,改个错别字不该让"下午三点记的那条"跳到"晚上七点"。
type Note struct {
	ID        string     `json:"id"`
	TaskID    string     `json:"taskId"`
	Body      string     `json:"body"`
	Source    Source     `json:"source"`
	CreatedAt time.Time  `json:"createdAt"`
	UpdatedAt *time.Time `json:"updatedAt,omitempty"`
}

// ---------------------------------------------------------------- 需求

// Stage 是需求的执行阶段。
//
// 存英文、显示中文,和 Status 一个路子 —— 库里是稳定的标识符,界面上是给人看的字。
type Stage string

const (
	StageSubmit  Stage = "submit"  // 需求提交
	StageClarify Stage = "clarify" // 需求澄清
	StageDev     Stage = "dev"     // 需求开发
	StageAccept  Stage = "accept"  // 需求验收
	StageOnline  Stage = "online"  // 需求上线
)

// stageOrder 是阶段从早到晚的排列,进度条和"到哪一步了"都按它来。
//
// 顺序只在这里写一遍:再散落到界面和统计里,加一个阶段就得改好几处。
var stageOrder = []Stage{StageSubmit, StageClarify, StageDev, StageAccept, StageOnline}

var stageLabels = map[Stage]string{
	StageSubmit:  "需求提交",
	StageClarify: "需求澄清",
	StageDev:     "需求开发",
	StageAccept:  "需求验收",
	StageOnline:  "需求上线",
}

// Valid 报告阶段是否是五个合法值之一。
func (s Stage) Valid() bool {
	_, ok := stageLabels[s]
	return ok
}

// Label 返回阶段的中文名。非法值原样返回,便于排查。
func (s Stage) Label() string {
	if l, ok := stageLabels[s]; ok {
		return l
	}
	return string(s)
}

// Index 返回阶段在顺序里的下标(0 起)。非法值返回 -1。
func (s Stage) Index() int {
	for i, v := range stageOrder {
		if v == s {
			return i
		}
	}
	return -1
}

// Stages 返回全部阶段的顺序副本,供界面画进度条。
func Stages() []Stage {
	out := make([]Stage, len(stageOrder))
	copy(out, stageOrder)
	return out
}

// Component 是需求的归属组件。
//
// 和 Category 是**两套维度**:分类说的是"这是什么性质的工作"(开发/会议/运维),
// 组件说的是"属于哪个东西"(axeops / xdr)。硬合成一个维度会让事项的分类下拉里
// 混进一堆组件名,所以宁可多一张表。
type Component struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Color     string    `json:"color"`
	SortOrder int       `json:"sortOrder"`
	CreatedAt time.Time `json:"createdAt"`
}

// Requirement 是一条需求。
//
// 和 Task 是两种东西:事项记的是"今天几点做了什么",需求记的是"这个季度在推什么"。
// 需求从提交到上线跨周跨月,没有时段、没有时长。
//
// StartAt 就是**添加时间** —— 建的时候是什么时候,这条需求就是什么时候开始的。
// OnlineAt 为 nil 表示还没上线。
type Requirement struct {
	ID          string     `json:"id"`
	Title       string     `json:"title"`
	ComponentID string     `json:"componentId,omitempty"` // 空表示未指定组件
	Link        string     `json:"link,omitempty"`
	Detail      string     `json:"detail,omitempty"` // 需求内容
	Stage       Stage      `json:"stage"`
	StartAt     time.Time  `json:"startAt"`
	OnlineAt    *time.Time `json:"onlineAt,omitempty"`
	Source      Source     `json:"source"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`

	// 查询时按需填充,不是 requirements 表的列。
	Component *Component `json:"component,omitempty"`
}

// Online 报告这条需求是否已上线。
//
// 它就是 stage == StageOnline —— online_at 有没有值必须和它一致,
// 这个不变量由 store 统一维护(见 store.applyStage)。
func (r Requirement) Online() bool { return r.Stage == StageOnline }

// Quarter 返回 t 所属的季度,1–4。
//
// 季度是**算出来的**,不落库:它和 online_at 之间是确定性的换算,
// 存一份就多一处能对不上的地方。
func Quarter(t time.Time) int { return (int(t.Month())-1)/3 + 1 }

// QuarterLabel 把时刻写成 "2026 Q4" 这样的季度标签。
func QuarterLabel(t time.Time) string {
	return fmt.Sprintf("%d Q%d", t.Year(), Quarter(t))
}
