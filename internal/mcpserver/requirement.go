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

// 需求是和时间轴上的事项并列的一类东西:事项记"今天几点做了什么",
// 需求记"这个季度在推什么"。这里给它一套独立的工具面。

// RequirementOut 是一条需求给模型看的形态。
//
// stage 给英文取值、stage_label 给中文名 —— 模型要能直接读,也要能原样回填给
// update_requirement,只给一种都得让它猜。
type RequirementOut struct {
	ID         string `json:"id" jsonschema:"需求 ID"`
	Title      string `json:"title" jsonschema:"需求标题"`
	Component  string `json:"component,omitempty" jsonschema:"归属组件名"`
	Link       string `json:"link,omitempty" jsonschema:"需求链接"`
	Detail     string `json:"detail,omitempty" jsonschema:"需求内容"`
	Stage      string `json:"stage" jsonschema:"submit/clarify/dev/accept/online"`
	StageLabel string `json:"stage_label" jsonschema:"阶段的中文名"`
	Start      string `json:"start" jsonschema:"需求开始时间 YYYY-MM-DD,即创建它的那天"`
	OnlineAt   string `json:"online_at,omitempty" jsonschema:"上线时间 YYYY-MM-DD;空表示还没上线"`
	Quarter    string `json:"quarter,omitempty" jsonschema:"所属季度,如 2026 Q4;空表示还没上线"`
}

func toRequirementOut(r model.Requirement) RequirementOut {
	o := RequirementOut{
		ID:         r.ID,
		Title:      r.Title,
		Link:       r.Link,
		Detail:     r.Detail,
		Stage:      string(r.Stage),
		StageLabel: r.Stage.Label(),
		Start:      r.StartAt.Format("2006-01-02"),
	}
	if r.Component != nil {
		o.Component = r.Component.Name
	}
	if r.OnlineAt != nil {
		o.OnlineAt = r.OnlineAt.Format("2006-01-02")
		o.Quarter = model.QuarterLabel(*r.OnlineAt)
	}
	return o
}

func toRequirementOuts(rs []model.Requirement) []RequirementOut {
	out := make([]RequirementOut, 0, len(rs))
	for _, r := range rs {
		out = append(out, toRequirementOut(r))
	}
	return out
}

// parseStage 解析阶段。英文取值和中文名都收 ——
// 模型可能照着界面上的字写,为这个让它重试一次不划算。
func parseStage(s string) (model.Stage, error) {
	v := strings.TrimSpace(s)
	for _, st := range model.Stages() {
		if string(st) == v || st.Label() == v {
			return st, nil
		}
	}
	return "", fmt.Errorf("阶段只能是 submit/clarify/dev/accept/online(或需求提交/需求澄清/需求开发/需求验收/需求上线),收到 %q", s)
}

// ---------------------------------------------------------------- 写

type CreateRequirementIn struct {
	Title     string `json:"title" jsonschema:"需求标题。必填"`
	Component string `json:"component,omitempty" jsonschema:"归属组件名,不存在会自动创建"`
	Link      string `json:"link,omitempty" jsonschema:"需求链接"`
	Detail    string `json:"detail,omitempty" jsonschema:"需求内容"`
	Stage     string `json:"stage,omitempty" jsonschema:"初始阶段,默认 submit(需求提交)"`
	OnlineAt  string `json:"online_at,omitempty" jsonschema:"上线时间 YYYY-MM-DD。填了就等于它已经上线,阶段会一并变成 online"`
}

func (s *Server) createRequirement(ctx context.Context, _ *mcp.CallToolRequest, in CreateRequirementIn) (*mcp.CallToolResult, RequirementOut, error) {
	var zero RequirementOut

	if strings.TrimSpace(in.Title) == "" {
		return nil, zero, fmt.Errorf("title 不能为空")
	}

	stage := model.StageSubmit
	if strings.TrimSpace(in.Stage) != "" {
		var err error
		if stage, err = parseStage(in.Stage); err != nil {
			return nil, zero, err
		}
	}

	var online *time.Time
	if strings.TrimSpace(in.OnlineAt) != "" {
		d, err := parseDay(in.OnlineAt)
		if err != nil {
			return nil, zero, err
		}
		online = &d
	}

	comp, err := s.store.EnsureComponent(in.Component)
	if err != nil {
		return nil, zero, err
	}
	compID := ""
	if comp != nil {
		compID = comp.ID
	}

	r, err := s.store.CreateRequirement(model.Requirement{
		Title: strings.TrimSpace(in.Title), ComponentID: compID,
		Link: strings.TrimSpace(in.Link), Detail: strings.TrimSpace(in.Detail),
		Stage: stage, OnlineAt: online, Source: model.SourceMCP,
	})
	if err != nil {
		return nil, zero, err
	}
	s.notify()

	out := toRequirementOut(*r)
	return &mcp.CallToolResult{Content: say("已记下需求「%s」,当前阶段 %s", out.Title, out.StageLabel)}, out, nil
}

type UpdateRequirementIn struct {
	ID        string `json:"id" jsonschema:"需求 ID"`
	Title     string `json:"title,omitempty" jsonschema:"新标题"`
	Component string `json:"component,omitempty" jsonschema:"新组件名,不存在会自动创建。填 \"-\" 表示改为未指定组件"`
	Link      string `json:"link,omitempty" jsonschema:"新链接"`
	Detail    string `json:"detail,omitempty" jsonschema:"新需求内容"`
	Stage     string `json:"stage,omitempty" jsonschema:"新阶段 submit/clarify/dev/accept/online。切到 online 且没给 online_at 时,上线时间记成今天"`
	OnlineAt  string `json:"online_at,omitempty" jsonschema:"上线时间 YYYY-MM-DD。填了阶段会一并变成 online"`
}

func (s *Server) updateRequirement(ctx context.Context, _ *mcp.CallToolRequest, in UpdateRequirementIn) (*mcp.CallToolResult, RequirementOut, error) {
	var zero RequirementOut

	if strings.TrimSpace(in.ID) == "" {
		return nil, zero, fmt.Errorf("id 不能为空")
	}

	p := store.RequirementPatch{}
	if in.Title != "" {
		t := strings.TrimSpace(in.Title)
		p.Title = &t
	}
	if in.Link != "" {
		p.Link = &in.Link
	}
	if in.Detail != "" {
		p.Detail = &in.Detail
	}
	if strings.TrimSpace(in.Stage) != "" {
		st, err := parseStage(in.Stage)
		if err != nil {
			return nil, zero, err
		}
		p.Stage = &st
	}
	if strings.TrimSpace(in.OnlineAt) != "" {
		d, err := parseDay(in.OnlineAt)
		if err != nil {
			return nil, zero, err
		}
		p.OnlineAt = &d
	}
	if in.Component != "" {
		c := in.Component
		if strings.TrimSpace(c) == "-" {
			c = "" // 和事项那边的约定一致:"-" 表示改成未分类 / 未指定组件
		} else {
			comp, err := s.store.EnsureComponent(c)
			if err != nil {
				return nil, zero, err
			}
			if comp != nil {
				c = comp.ID
			} else {
				c = ""
			}
		}
		p.ComponentID = &c
	}

	r, err := s.store.UpdateRequirement(in.ID, p)
	if err != nil {
		return nil, zero, err
	}
	s.notify()

	out := toRequirementOut(*r)
	return &mcp.CallToolResult{Content: say("已更新需求「%s」,当前阶段 %s", out.Title, out.StageLabel)}, out, nil
}

type DeleteRequirementIn struct {
	ID string `json:"id" jsonschema:"需求 ID"`
}

func (s *Server) deleteRequirement(ctx context.Context, _ *mcp.CallToolRequest, in DeleteRequirementIn) (*mcp.CallToolResult, struct{}, error) {
	if err := s.store.DeleteRequirement(in.ID); err != nil {
		return nil, struct{}{}, err
	}
	s.notify()
	return &mcp.CallToolResult{Content: say("已删除需求 %s", in.ID)}, struct{}{}, nil
}

// ---------------------------------------------------------------- 读

type ListRequirementsIn struct {
	Component string `json:"component,omitempty" jsonschema:"组件名,默认不限。填 \"-\" 只看未指定组件的"`
	Stage     string `json:"stage,omitempty" jsonschema:"只看某个阶段,默认不限"`
	Query     string `json:"query,omitempty" jsonschema:"标题 / 内容关键词,默认不限"`
	Limit     int    `json:"limit,omitempty" jsonschema:"最多返回几条,默认 100"`
}

func (s *Server) listRequirements(ctx context.Context, _ *mcp.CallToolRequest, in ListRequirementsIn) (*mcp.CallToolResult, []RequirementOut, error) {
	f := store.RequirementFilter{Limit: in.Limit}
	if f.Limit <= 0 {
		f.Limit = 100
	}

	if c := strings.TrimSpace(in.Component); c != "" {
		if c == "-" {
			f.Component = store.NoComponent
		} else {
			comp, err := s.store.ComponentByName(c)
			if err != nil {
				return nil, nil, err
			}
			if comp == nil {
				// 按一个不存在的组件查,结果就是空 —— 不报错,不然模型会以为调用方式错了
				return &mcp.CallToolResult{Content: say("没有叫「%s」的组件", c)}, []RequirementOut{}, nil
			}
			f.Component = comp.ID
		}
	}
	if strings.TrimSpace(in.Stage) != "" {
		st, err := parseStage(in.Stage)
		if err != nil {
			return nil, nil, err
		}
		f.Stage = st
	}
	f.Query = in.Query

	reqs, err := s.store.ListRequirements(f)
	if err != nil {
		return nil, nil, err
	}

	out := toRequirementOuts(reqs)
	if len(out) == 0 {
		return &mcp.CallToolResult{Content: say("没有符合条件的需求")}, out, nil
	}
	return &mcp.CallToolResult{
		Content: say("共 %d 条需求,其中 %d 条已上线", len(out), countOnline(reqs)),
	}, out, nil
}

func countOnline(rs []model.Requirement) int {
	n := 0
	for _, r := range rs {
		if r.Online() {
			n++
		}
	}
	return n
}

// ReqComponentOut 是一个组件下的需求完成情况。
type ReqComponentOut struct {
	Component string  `json:"component" jsonschema:"组件名"`
	Total     int     `json:"total" jsonschema:"需求总数"`
	Online    int     `json:"online" jsonschema:"已上线数"`
	Percent   float64 `json:"percent" jsonschema:"完成度百分比 0-100"`
}

// ReqQuarterOut 是一个季度里上线的需求数。
type ReqQuarterOut struct {
	Quarter string `json:"quarter" jsonschema:"季度标签,如 2026 Q4"`
	Online  int    `json:"online" jsonschema:"该季度上线的需求数"`
}

// RequirementReportOut 是需求完成情况的汇总。
type RequirementReportOut struct {
	Total      int               `json:"total" jsonschema:"需求总数"`
	Online     int               `json:"online" jsonschema:"已上线数"`
	Percent    float64           `json:"percent" jsonschema:"整体完成度百分比"`
	Components []ReqComponentOut `json:"components" jsonschema:"按组件拆分"`
	Quarters   []ReqQuarterOut   `json:"quarters" jsonschema:"按季度拆分(只含已上线的),最近的在前"`
}

type RequirementReportIn struct{}

func (s *Server) requirementReport(ctx context.Context, _ *mcp.CallToolRequest, _ RequirementReportIn) (*mcp.CallToolResult, RequirementReportOut, error) {
	st, err := s.store.RequirementStatsOf()
	if err != nil {
		return nil, RequirementReportOut{}, err
	}

	out := RequirementReportOut{
		Total:      st.Total,
		Online:     st.Online,
		Components: make([]ReqComponentOut, 0, len(st.Components)),
		Quarters:   make([]ReqQuarterOut, 0, len(st.Quarters)),
	}
	if st.Total > 0 {
		out.Percent = float64(st.Online) * 100 / float64(st.Total)
	}
	for _, c := range st.Components {
		out.Components = append(out.Components, ReqComponentOut{
			Component: c.Name, Total: c.Total, Online: c.Online, Percent: c.Percent(),
		})
	}
	for _, q := range st.Quarters {
		out.Quarters = append(out.Quarters, ReqQuarterOut{
			Quarter: fmt.Sprintf("%d Q%d", q.Year, q.Quarter), Online: q.Online,
		})
	}

	return &mcp.CallToolResult{
		Content: say("共 %d 条需求,已上线 %d 条(%.0f%%)", out.Total, out.Online, out.Percent),
	}, out, nil
}
