// Package mcpserver 把 worklog 暴露成 MCP 服务,供 Claude Code 调用。
//
// 传输用 streamable HTTP 而不是 stdio:stdio 的服务端是客户端按需拉起的
// 子进程,生命周期跟着 Claude Code;而我们要的是"worklog 一启动,MCP 就
// 在",所以必须是个常驻的 HTTP 端点。
//
// 只监听回环地址,并且强制 Bearer token —— 同机其他程序不该能随便读写
// 你的工作记录。
package mcpserver

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"worklog/internal/store"
)

// DefaultPort 是 HTTP MCP 的默认端口。
//
// 端口写死在配置里(claude mcp add 时填的就是完整 URL),所以被占用时
// 必须报错让用户改,不能静默换一个 —— 换了配置就失效了。
const DefaultPort = 7717

// Path 是 MCP 端点路径。
const Path = "/mcp"

// PingResult 是 ping 工具的返回。
type PingResult struct {
	OK      bool   `json:"ok" jsonschema:"是否正常"`
	Service string `json:"service" jsonschema:"服务名"`
	Version string `json:"version" jsonschema:"版本"`
	Time    string `json:"time" jsonschema:"服务端当前时间(RFC3339)"`
}

// Server 是常驻的 MCP HTTP 服务。
type Server struct {
	mcp   *mcp.Server
	http  *http.Server
	addr  string
	store *store.Store

	mu      sync.RWMutex
	changed func()
}

// Options 是构造参数。
type Options struct {
	// Store 是数据层。必填。
	Store *store.Store
	// Addr 是监听地址,形如 "127.0.0.1:7717"。空则用默认端口。
	Addr string
	// Token 是访问令牌。为空表示不校验 —— 仅供本机调试,正式运行必须给。
	Token string
	// Version 会出现在 ping 的返回里,便于确认连的是哪个版本。
	Version string
}

// URL 返回应当填进 claude mcp add 的完整地址。
func URL(addr string) string {
	if addr == "" {
		addr = fmt.Sprintf("127.0.0.1:%d", DefaultPort)
	}
	return "http://" + addr + Path
}

// New 构造服务。此时还没开始监听。
func New(opts Options) *Server {
	if opts.Addr == "" {
		opts.Addr = fmt.Sprintf("127.0.0.1:%d", DefaultPort)
	}
	if opts.Version == "" {
		opts.Version = "dev"
	}

	s := mcp.NewServer(&mcp.Implementation{
		Name:    "worklog",
		Version: opts.Version,
	}, nil)

	srv := &Server{mcp: s, addr: opts.Addr, store: opts.Store}
	srv.registerTools(opts.Version)
	srv.registerResources()

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil)

	mux := http.NewServeMux()
	mux.Handle(Path, requireToken(opts.Token, handler))

	srv.http = &http.Server{
		Addr:              opts.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return srv
}

// registerTools 挂上全部工具。
func (s *Server) registerTools(version string) {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "ping",
		Description: "连通性自检。确认 Claude Code 已经连上本机的 worklog 服务。",
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, PingResult, error) {
		out := PingResult{OK: true, Service: "worklog", Version: version, Time: time.Now().Format(time.RFC3339)}
		return &mcp.CallToolResult{
			Content: say("worklog 已就绪,版本 %s", version),
		}, out, nil
	})

	// ---- 写
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "log_task",
		Description: "补记一条已完成的工作事项。这是最常用的入口 —— " +
			"当你需要把「今天做了什么、花了多久」记下来时用它。" +
			"时长必须显式给出,因为对话经过的时间不等于实际投入的时间。",
	}, s.logTask)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "create_task",
		Description: "创建一条事项,默认是待办。用于安排还没做的事,而不是补记已完成的。",
	}, s.createTask)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "start_task",
		Description: "开始计时:开一段时段。已经在跑就原样返回,重复调用是安全的。" +
			"一件事分几次做完时,每次接着做都调它。",
	}, s.startTask)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "pause_task",
		Description: "暂停计时:关掉正在跑的那一段,状态仍是进行中。" +
			"中间去开会、被打断时用它,暂停期间不计入时长。",
	}, s.pauseTask)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "finish_task",
		Description: "结束事项并记录耗时。优先传 duration_min —— " +
			"不传则按当前时间减去开始时间算,那样会把中途的会议、等待也算进去。" +
			"给的值作用在正在跑的那一段上。",
	}, s.finishTask)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "add_span",
		Description: "给事项补一段实际投入(开始 + 结束两个时刻)。" +
			"事后回忆「上午 10-11 做了一段,下午 15-16 又做了一段」时用它," +
			"时长是两段之和,中间停掉的时间不算。",
	}, s.addSpan)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "delete_span",
		Description: "删除一段时段。补记错了要能改。",
	}, s.deleteSpan)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "update_task",
		Description: "修改事项的标题、分类、状态或时间跨度。注意改的是跨度,不会动已有的时段。",
	}, s.updateTask)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "add_note",
		Description: "往事项上追加一条进展备注。边做边记的时候用它 —— " +
			"这些备注会和人手写的备注一起显示在时间线上。",
	}, s.addNote)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "delete_task",
		Description: "删除事项及其全部备注。",
	}, s.deleteTask)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "create_category",
		Description: "新建分类。一般不需要主动调 —— log_task 遇到不存在的分类名会自动创建。",
	}, s.createCategory)

	// ---- 读
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "list_tasks",
		Description: "按日期范围、状态、分类或关键词查询事项。",
	}, s.listTasks)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "get_task",
		Description: "取单个事项的详情,含完整备注时间线。",
	}, s.getTask)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "current_task",
		Description: "查看当前正在推进的事项。可能有多个 —— 允许并行计时。",
	}, s.currentTask)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "report",
		Description: "取一段时间(默认本周)的结构化汇总:按分类的耗时与占比、事项明细。" +
			"写周报时先调它拿数据,文字由你自己组织。",
	}, s.report)

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "list_categories",
		Description: "列出全部分类及其颜色。",
	}, s.listCategories)
}

// requireToken 校验 Authorization 头。
//
// 用 constant-time 比较:令牌是本地生成的短串,理论上可以被同机进程逐字节
// 试探,虽然风险很低,但这里几乎没有成本。
func requireToken(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token == "" {
			next.ServeHTTP(w, r)
			return
		}
		got := strings.TrimSpace(r.Header.Get("Authorization"))
		got = strings.TrimPrefix(got, "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="worklog"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Start 开始监听。它在自己的 goroutine 里跑,立即返回;
// 真正的监听错误通过 errCh 报出来。
func (s *Server) Start() (<-chan error, error) {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return nil, fmt.Errorf("MCP 端口 %s 无法监听(可能被占用): %w", s.addr, err)
	}

	errCh := make(chan error, 1)
	go func() {
		log.Printf("MCP 服务已启动: %s", URL(s.addr))
		if err := s.http.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()
	return errCh, nil
}

// Stop 优雅关闭。
func (s *Server) Stop(ctx context.Context) error {
	return s.http.Shutdown(ctx)
}

// SetOnChange 注册"数据被改动了"的回调。
//
// 界面的刷新原来只由用户自己的操作触发,于是 Claude 通过 MCP 记完一条事项,
// 窗口要等用户点一下才显示出来 —— 而这恰恰是这个工具最主要的用法。
func (s *Server) SetOnChange(fn func()) {
	s.mu.Lock()
	s.changed = fn
	s.mu.Unlock()
}

// notify 通知界面数据变了。
//
// 回调跑在 HTTP 的 goroutine 上,不能让它把请求带崩 —— 通知失败只是界面
// 晚一点刷新,不该变成一次工具调用失败。
func (s *Server) notify() {
	s.mu.RLock()
	fn := s.changed
	s.mu.RUnlock()
	if fn == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			log.Printf("数据变更回调出错: %v", r)
		}
	}()
	fn()
}
