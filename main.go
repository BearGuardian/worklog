// worklog —— 本地工作事项与计时工具,带 MCP。
//
// 单进程两件事:一个常驻窗口(看和改),一个常驻 HTTP MCP 服务
// (让 Claude Code 读写)。两者共享同一个 SQLite。
package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	wopts "github.com/wailsapp/wails/v2/pkg/options/windows"

	"worklog/internal/mcpserver"
	"worklog/internal/store"
)

//go:embed all:frontend/dist
var assets embed.FS

// 版本号,构建时可用 -ldflags 覆盖。
var version = "0.1.0-dev"

const (
	logMaxBytes    = 2 << 20 // 单份日志上限 2MB,超了就轮转
	windowTitle    = "worklog"
	shutdownWindow = 3 * time.Second
)

func main() {
	var (
		addr     = flag.String("mcp-addr", "", "MCP 监听地址,默认 127.0.0.1:7717")
		dataDir  = flag.String("data", "", "数据目录,默认 %LOCALAPPDATA%\\worklog")
		noMCP    = flag.Bool("no-mcp", false, "不启动 MCP 服务(仅排查界面用)")
		headless = flag.Bool("mcp-only", false, "只跑 MCP 服务,不开界面")
	)
	flag.Parse()

	dir := *dataDir
	if dir == "" {
		var err error
		if dir, err = store.DefaultDir(); err != nil {
			log.Fatal(err)
		}
	}

	// 目录必须先建出来:setupLogging 要往里写日志文件,
	// 首次运行时目录还不存在,日志会静默退化成只写 stderr。
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Fatalf("创建数据目录失败: %v", err)
	}

	closeLog := setupLogging(dir)
	defer closeLog()

	st, err := store.Open(dir)
	if err != nil {
		log.Fatalf("打开数据目录失败: %v", err)
	}
	defer st.Close()
	log.Printf("---- worklog %s 启动,数据目录 %s ----", version, dir)

	// MCP 的令牌要稳定:它会被写进 Claude Code 的配置,
	// 每次启动换新的会让已经配好的连接全部失效。
	token, err := st.EnsureMCPToken()
	if err != nil {
		log.Fatalf("准备 MCP 令牌失败: %v", err)
	}

	var srv *mcpserver.Server
	if !*noMCP {
		srv = mcpserver.New(mcpserver.Options{Store: st, Addr: *addr, Token: token, Version: version})
		if _, err := srv.Start(); err != nil {
			// 端口被占用是常见情况(比如已经开了一个实例),
			// 不该因此让界面打不开 —— 提示一下继续跑。
			log.Printf("MCP 服务启动失败(界面仍可用): %v", err)
			srv = nil
		}
	}

	if *headless {
		fmt.Printf("MCP 地址: %s\n", mcpserver.URL(*addr))
		fmt.Printf("接入命令: claude mcp add --transport http worklog %s -H \"Authorization: Bearer %s\" -s user\n",
			mcpserver.URL(*addr), token)
		select {}
	}

	app := NewApp(st, srv, *addr, token)
	if srv != nil {
		// Claude 通过 MCP 写完,推事件让界面立刻跟上
		srv.SetOnChange(app.DataChanged)
	}

	err = wails.Run(&options.App{
		Title:     windowTitle,
		Width:     1440,
		Height:    920,
		MinWidth:  1040,
		MinHeight: 620,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		// 和前端深色底一致,避免启动瞬间白闪
		BackgroundColour: &options.RGBA{R: 10, G: 11, B: 15, A: 1},
		OnStartup:        app.startup,
		Bind:             []interface{}{app},
		Windows: &wopts.Options{
			WebviewIsTransparent: false,
			WindowIsTranslucent:  false,
		},
	})
	if err != nil {
		log.Fatalf("界面启动失败: %v", err)
	}

	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), shutdownWindow)
		defer cancel()
		_ = srv.Stop(ctx)
	}
	log.Printf("---- 退出 ----")
}

// logf 写一条带前缀的日志,供 app.go 复用。
func logf(format string, args ...any) { log.Printf(format, args...) }

// parseClock 把 "HH:MM" 解析成当天的分钟偏移。
func parseClock(s string) (int, error) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, fmt.Errorf("时间格式不对: %q,应当形如 09:30", s)
	}
	return t.Hour()*60 + t.Minute(), nil
}

// tolerantWriter 吞掉底层写入错误。
//
// io.MultiWriter 在第一个 writer 出错时就会短路,后面的全都不写 ——
// 用 stderr 兜底会让重定向到文件的那一路整个失效。这里两边都不报错。
type tolerantWriter struct{ w io.Writer }

func (t tolerantWriter) Write(p []byte) (int, error) {
	_, _ = t.w.Write(p)
	return len(p), nil
}

// setupLogging 把日志同时写到文件和 stderr,返回关闭函数。
func setupLogging(dir string) func() {
	path := filepath.Join(dir, "worklog.log")
	if info, err := os.Stat(path); err == nil && info.Size() > logMaxBytes {
		_ = os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		log.Printf("无法写入日志文件,仅输出到标准错误: %v", err)
		return func() {}
	}
	log.SetOutput(io.MultiWriter(tolerantWriter{f}, tolerantWriter{os.Stderr}))
	return func() { f.Close() }
}
