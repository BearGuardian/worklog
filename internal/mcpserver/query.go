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

// ---------------------------------------------------------------- 出参

// CategoryOut 是分类对外的表示。
type CategoryOut struct {
	ID    string `json:"id" jsonschema:"分类 ID"`
	Name  string `json:"name" jsonschema:"分类名"`
	Color string `json:"color" jsonschema:"颜色 #RRGGBB"`
}

// GroupOut 是报告里按分类聚合的一组。
type GroupOut struct {
	Category  string    `json:"category" jsonschema:"分类名"`
	Color     string    `json:"color" jsonschema:"颜色"`
	Minutes   int       `json:"minutes" jsonschema:"该分类耗时(分钟)"`
	Percent   float64   `json:"percent" jsonschema:"占总时长的百分比"`
	TaskCount int       `json:"task_count" jsonschema:"事项数"`
	Tasks     []TaskOut `json:"tasks" jsonschema:"事项明细"`
}

// ReportOut 是一段时间的结构化汇总。
type ReportOut struct {
	From           string     `json:"from" jsonschema:"起始日期"`
	To             string     `json:"to" jsonschema:"结束日期(不含)"`
	TotalMinutes   int        `json:"total_minutes" jsonschema:"总时长(分钟),各分类之和"`
	UnionMinutes   int        `json:"union_minutes" jsonschema:"墙钟时长(分钟),并行的事项只算一次"`
	OverlapMinutes int        `json:"overlap_minutes" jsonschema:"被重复计算的分钟数,大于 0 说明有并行推进"`
	TaskCount      int        `json:"task_count" jsonschema:"事项总数"`
	Groups         []GroupOut `json:"groups" jsonschema:"按分类分组,按耗时降序"`
}

func toReportOut(r *store.Report) ReportOut {
	o := ReportOut{
		From:           r.From.Format("2006-01-02"),
		To:             r.To.Format("2006-01-02"),
		TotalMinutes:   r.TotalMinutes,
		UnionMinutes:   r.UnionMinutes,
		OverlapMinutes: r.OverlapMinutes,
		TaskCount:      r.TaskCount,
		Groups:         make([]GroupOut, 0, len(r.Groups)),
	}
	for _, g := range r.Groups {
		o.Groups = append(o.Groups, GroupOut{
			Category:  g.Name,
			Color:     g.Color,
			Minutes:   g.Minutes,
			Percent:   g.Percent,
			TaskCount: g.TaskCount,
			Tasks:     toTaskOuts(g.Tasks),
		})
	}
	return o
}

// ---------------------------------------------------------------- 读操作

type ListTasksIn struct {
	From     string `json:"from,omitempty" jsonschema:"起始日期 YYYY-MM-DD,默认不限"`
	To       string `json:"to,omitempty" jsonschema:"结束日期 YYYY-MM-DD(不含),默认不限"`
	Status   string `json:"status,omitempty" jsonschema:"todo/doing/done,默认不限"`
	Category string `json:"category,omitempty" jsonschema:"分类名,默认不限。填 \"-\" 只看未分类"`
	Query    string `json:"query,omitempty" jsonschema:"标题关键词,默认不限"`
	Limit    int    `json:"limit,omitempty" jsonschema:"最多返回几条,默认 100"`
}

func (s *Server) listTasks(ctx context.Context, _ *mcp.CallToolRequest, in ListTasksIn) (*mcp.CallToolResult, []TaskOut, error) {
	f := store.TaskFilter{Limit: in.Limit}
	if f.Limit <= 0 {
		f.Limit = 100
	}

	if strings.TrimSpace(in.From) != "" {
		d, err := parseDay(in.From)
		if err != nil {
			return nil, nil, err
		}
		f.From = &d
	}
	if strings.TrimSpace(in.To) != "" {
		d, err := parseDay(in.To)
		if err != nil {
			return nil, nil, err
		}
		// To 是"不含",所以取次日零点,让用户写的日期包含当天
		next := d.AddDate(0, 0, 1)
		f.To = &next
	}
	if strings.TrimSpace(in.Status) != "" {
		st := model.Status(strings.TrimSpace(in.Status))
		if !st.Valid() {
			return nil, nil, fmt.Errorf("status 只能是 todo / doing / done,收到 %q", in.Status)
		}
		f.Status = st
	}
	switch strings.TrimSpace(in.Category) {
	case "":
	case "-":
		f.Category = store.NoCategory
	default:
		c, err := s.store.CategoryByName(in.Category)
		if err != nil {
			return nil, nil, err
		}
		if c == nil {
			// 分类不存在就是没匹配,不是错误
			return &mcp.CallToolResult{Content: say("没有叫「%s」的分类", in.Category)}, []TaskOut{}, nil
		}
		f.Category = c.ID
	}
	f.Query = in.Query

	tasks, err := s.store.ListTasks(f)
	if err != nil {
		return nil, nil, err
	}
	out := toTaskOuts(tasks)

	if len(out) == 0 {
		return &mcp.CallToolResult{Content: say("没有匹配的事项")}, out, nil
	}
	return &mcp.CallToolResult{
		Content: say("共 %d 条,合计 %s", len(out), durText(sumMinutes(tasks))),
	}, out, nil
}

type GetTaskIn struct {
	ID string `json:"id" jsonschema:"事项 ID"`
}

func (s *Server) getTask(ctx context.Context, _ *mcp.CallToolRequest, in GetTaskIn) (*mcp.CallToolResult, TaskOut, error) {
	var zero TaskOut
	t, err := s.store.GetTask(in.ID)
	if err != nil {
		return nil, zero, err
	}
	out := toTaskOut(*t)
	return &mcp.CallToolResult{
		Content: say("%s · %s %s–%s · %s · 备注 %d 条",
			out.Title, out.Date, out.Start, out.End, out.Status, len(out.Notes)),
	}, out, nil
}

// currentTask 返回正在推进的事项。允许并行,所以是个列表。
func (s *Server) currentTask(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, []TaskOut, error) {
	tasks, err := s.store.CurrentTasks()
	if err != nil {
		return nil, nil, err
	}
	out := toTaskOuts(tasks)

	if len(out) == 0 {
		return &mcp.CallToolResult{Content: say("当前没有进行中的事项")}, out, nil
	}
	names := make([]string, 0, len(out))
	for _, t := range out {
		names = append(names, t.Title)
	}
	return &mcp.CallToolResult{
		Content: say("正在进行:%s", strings.Join(names, "、")),
	}, out, nil
}

type ReportIn struct {
	From string `json:"from,omitempty" jsonschema:"起始日期 YYYY-MM-DD。不填则默认本周一"`
	To   string `json:"to,omitempty" jsonschema:"结束日期 YYYY-MM-DD(含)。不填则默认本周日"`
}

// report 返回结构化汇总。文字(比如周报正文)由模型自己组织,
// 这样换口径、换语言都不用改代码。
func (s *Server) report(ctx context.Context, _ *mcp.CallToolRequest, in ReportIn) (*mcp.CallToolResult, ReportOut, error) {
	var zero ReportOut

	from, to := store.WeekRange(time.Now())
	if strings.TrimSpace(in.From) != "" {
		d, err := parseDay(in.From)
		if err != nil {
			return nil, zero, err
		}
		from = d
	}
	if strings.TrimSpace(in.To) != "" {
		d, err := parseDay(in.To)
		if err != nil {
			return nil, zero, err
		}
		to = d.AddDate(0, 0, 1) // 用户写的是"含",内部用"不含"
	}
	if !to.After(from) {
		return nil, zero, fmt.Errorf("结束日期必须晚于开始日期")
	}

	rep, err := s.store.ReportBetween(from, to)
	if err != nil {
		return nil, zero, err
	}
	out := toReportOut(rep)

	if rep.TaskCount == 0 {
		return &mcp.CallToolResult{
			Content: say("%s 到 %s 之间没有记录", out.From, out.To),
		}, out, nil
	}

	summary := fmt.Sprintf("%s 到 %s:共 %d 项,%s",
		out.From, out.To, out.TaskCount, durText(out.TotalMinutes))
	if out.OverlapMinutes > 0 {
		summary += fmt.Sprintf("(其中有 %s 是并行推进、被重复计算的)", durText(out.OverlapMinutes))
	}
	return &mcp.CallToolResult{Content: say("%s", summary)}, out, nil
}

func (s *Server) listCategories(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, []CategoryOut, error) {
	cats, err := s.store.ListCategories()
	if err != nil {
		return nil, nil, err
	}
	out := make([]CategoryOut, 0, len(cats))
	for _, c := range cats {
		out = append(out, CategoryOut{ID: c.ID, Name: c.Name, Color: c.Color})
	}
	if len(out) == 0 {
		return &mcp.CallToolResult{Content: say("还没有分类")}, out, nil
	}
	return &mcp.CallToolResult{Content: say("共 %d 个分类", len(out))}, out, nil
}

// ---------------------------------------------------------------- 资源

// registerResources 挂上只读资源。
//
// 工具负责"做事",资源负责"看" —— Claude 想先了解今天/本周的情况时,
// 读资源比调工具再解析更直接。
func (s *Server) registerResources() {
	s.mcp.AddResource(&mcp.Resource{
		URI:         "worklog://today",
		Name:        "今天",
		Description: "今天的所有事项与合计时长",
		MIMEType:    "text/markdown",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		from, to := store.DayRange(time.Now())
		return s.renderRange(from, to, req.Params.URI, "今天")
	})

	s.mcp.AddResource(&mcp.Resource{
		URI:         "worklog://week",
		Name:        "本周",
		Description: "本周(周一到周日)的事项与按分类的耗时占比",
		MIMEType:    "text/markdown",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		from, to := store.WeekRange(time.Now())
		return s.renderRange(from, to, req.Params.URI, "本周")
	})

	s.mcp.AddResource(&mcp.Resource{
		URI:         "worklog://current",
		Name:        "进行中",
		Description: "当前正在推进的事项",
		MIMEType:    "text/markdown",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		tasks, err := s.store.CurrentTasks()
		if err != nil {
			return nil, err
		}
		var b strings.Builder
		if len(tasks) == 0 {
			b.WriteString("当前没有进行中的事项。\n")
		} else {
			for _, t := range tasks {
				cat := "未分类"
				if t.Category != nil {
					cat = t.Category.Name
				}
				fmt.Fprintf(&b, "- **%s** · %s · %s–%s\n",
					t.Title, cat, t.StartAt.Format("15:04"), t.EndAt.Format("15:04"))
			}
		}
		return &mcp.ReadResourceResult{
			Contents: []*mcp.ResourceContents{{
				URI: req.Params.URI, MIMEType: "text/markdown", Text: b.String(),
			}},
		}, nil
	})
}

// renderRange 把一段时间的事项渲染成 Markdown。
func (s *Server) renderRange(from, to time.Time, uri, title string) (*mcp.ReadResourceResult, error) {
	rep, err := s.store.ReportBetween(from, to)
	if err != nil {
		return nil, err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# %s(%s)\n\n", title, from.Format("2006-01-02"))

	if rep.TaskCount == 0 {
		b.WriteString("没有记录。\n")
	} else {
		fmt.Fprintf(&b, "合计 %s,共 %d 项。\n\n", durText(rep.TotalMinutes), rep.TaskCount)
		for _, g := range rep.Groups {
			fmt.Fprintf(&b, "## %s · %s(%.0f%%)\n\n", g.Name, durText(g.Minutes), g.Percent)
			for _, t := range g.Tasks {
				// 一行一件事:时长、标题、当天做过的那些时段。
				// 时段逐个列出,因为一件事可能分几段做,只印首尾会把中间的空档说成在做。
				fmt.Fprintf(&b, "- **%s** · %s · %s",
					t.Title, durText(t.Minutes(time.Now())),
					t.StartAt.Format("01-02"))
				for _, sp := range t.Spans {
					end := "进行中"
					if sp.EndAt != nil {
						end = sp.EndAt.Format("15:04")
					}
					fmt.Fprintf(&b, " %s–%s", sp.StartAt.Format("15:04"), end)
				}
				b.WriteString("\n")
			}
			b.WriteString("\n")
		}
	}

	return &mcp.ReadResourceResult{
		Contents: []*mcp.ResourceContents{{
			URI: uri, MIMEType: "text/markdown", Text: b.String(),
		}},
	}, nil
}

// ---------------------------------------------------------------- 小工具

func sumMinutes(ts []model.Task) int {
	now := time.Now()
	total := 0
	for _, t := range ts {
		total += t.Minutes(now)
	}
	return total
}
