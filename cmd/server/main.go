// Command server 是 QLink2Desktop 的主程序。
//
// 两种运行模式：
//
//	server（默认）—— 常驻服务，监听飞牛统一网关的 Unix 套接字，可选再监听 TCP 端口；
//	cgi           —— 短命进程，把网关交给它的 CGI 请求转发回常驻服务。
//	                 当 index.cgi 被当作可执行文件拉起时触发。
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/MisiteQ/qlink2desktop/internal/app"
	"github.com/MisiteQ/qlink2desktop/internal/gateway"
	"github.com/MisiteQ/qlink2desktop/internal/logx"
	"github.com/MisiteQ/qlink2desktop/internal/webui"
)

// version 由构建脚本通过 -ldflags "-X main.version=..." 注入。
var version = "1.0.0"

// appName 是本应用在飞牛应用中心的标识。
//
// 注意它**不带** qlink2d. 前缀：那个前缀是桌面快捷式子应用的命名空间，
// 本应用自身是一个正常的第三方应用，混在一起会被清理逻辑误伤。
const appName = "qlink2desktop"

func main() {
	modeFlag := flag.String("mode", "server", "运行模式：server 或 cgi")
	portFlag := flag.Int("port", 0, "监听端口（0 表示按环境自动决定）")
	hostFlag := flag.String("host", "", "监听地址，默认 0.0.0.0")
	dataDirFlag := flag.String("data", "", "数据目录")
	socketFlag := flag.String("socket", "", "飞牛统一网关的 Unix 套接字路径")
	logDirFlag := flag.String("log-dir", "", "日志目录")
	portBaseFlag := flag.Int("proxy-port-base", 0, "内置反向代理的端口起点")
	noRemoteIcon := flag.Bool("no-remote-icon", false, "禁止拉取远端图标（离线部署）")
	printVersion := flag.Bool("version", false, "打印版本号并退出")
	flag.Parse()

	if *printVersion {
		fmt.Println(version)
		return
	}

	// ---------------------------------------------------------------- CGI 模式
	// 作为 CGI 进程运行时不做任何初始化：它会立刻转发请求然后退出，
	// 初始化数据目录、日志文件既浪费时间又可能产生并发写冲突。
	if *modeFlag == "cgi" || os.Getenv("GATEWAY_INTERFACE") != "" {
		gateway.RunCGI(gateway.ResolveSocket(*socketFlag, appName))
		return
	}

	// ---------------------------------------------------------------- 目录准备
	dataDir := resolveDataDir(*dataDirFlag)
	logDir := *logDirFlag
	if logDir == "" {
		logDir = filepath.Join(dataDir, "logs")
	}

	logs := logx.Setup(logx.Options{
		Dir:        logDir,
		FileName:   appName + ".log",
		MaxSizeMB:  4,
		MaxBackups: 3,
		Level:      logLevel(),
		Console:    true,
	})
	defer func() { _ = logs.Close() }()
	logger := logs.Logger
	slog.SetDefault(logger)

	portBase := *portBaseFlag
	if portBase <= 0 {
		portBase = 18000
	}

	logger.Info("QLink2Desktop 正在启动",
		"version", version, "mode", "server", "data", dataDir, "log", logDir)

	// ---------------------------------------------------------------- 装配
	application, err := app.New(app.Options{
		DataDir:         dataDir,
		LogDir:          logDir,
		Version:         version,
		PortBase:        portBase,
		AllowRemoteIcon: !*noRemoteIcon,
		WebFS:           webui.FS(),
		Logger:          logger,
		Logs:            logs.Ring,
	})
	if err != nil {
		logger.Error("初始化失败，进程退出", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 后台任务：启动对账 + 周期守护代理。
	// 放在独立 goroutine 里，避免安装流程拖慢"服务已就绪"的判断。
	go application.Supervise(ctx)

	// ------------------------------------------------------------ 请求分发
	var ready atomic.Bool
	dispatcher := buildDispatcher(application, &ready)

	srv := &http.Server{
		Handler: dispatcher,
		// 读写超时故意留空：SSE 是长连接，任何全局超时都会把它掐断，
		// 表现为浏览器控制台里的 ERR_INCOMPLETE_CHUNKED_ENCODING。
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// ------------------------------------------------------------ Unix 套接字
	socketPath := gateway.ResolveSocket(*socketFlag, appName)
	var sockLn net.Listener
	if socketPath != "" {
		ln, err := gateway.Listen(socketPath)
		if err != nil {
			logger.Warn("统一网关套接字创建失败，将仅使用 TCP 端口", "socket", socketPath, "error", err)
		} else {
			sockLn = ln
			logger.Info("飞牛统一网关套接字已就绪", "socket", socketPath)
			// 部分版本的网关只会去 /tmp 下找套接字，做一份软链兼容。
			tmpLink := filepath.Join(os.TempDir(), appName+".sock")
			if tmpLink != socketPath {
				_ = os.Remove(tmpLink)
				if err := os.Symlink(socketPath, tmpLink); err != nil {
					logger.Debug("创建套接字软链失败（不影响网关直连）", "link", tmpLink, "error", err)
				}
			}
		}
	}

	// ------------------------------------------------------------ TCP 监听
	// 有套接字时默认不再监听 TCP（零端口占用原则）；
	// 显式指定端口或设置 PORT 环境变量才额外开一个，方便局域网直连调试。
	port := resolvePort(*portFlag, sockLn != nil)
	var tcpLn net.Listener
	if port > 0 {
		addr := fmt.Sprintf("%s:%d", resolveHost(*hostFlag), port)
		ln, err := listenWithRetry(addr, 3)
		if err != nil {
			logger.Warn("端口被占用，改用一个空闲端口", "requested", addr, "error", err)
			ln, port, err = listenOnFreePort(resolveHost(*hostFlag), port)
		}
		if err != nil {
			logger.Error("无法建立 TCP 监听", "error", err)
		} else {
			tcpLn = ln
			logger.Info("TCP 监听已建立", "address", fmt.Sprintf("%s:%d", resolveHost(*hostFlag), port))
		}
	} else {
		logger.Info("未启用 TCP 监听（由飞牛统一网关经 Unix 套接字接入，零端口占用）")
	}

	if sockLn == nil && tcpLn == nil {
		logger.Error("既没有可用的 Unix 套接字也没有 TCP 监听，无法对外提供服务")
		os.Exit(1)
	}

	// 一切就绪后才打开闸门。此前到达的请求会收到一张"正在启动"的页面，
	// 而不是 502 —— 后者在安装完成后立刻点开图标时非常常见。
	ready.Store(true)
	logger.Info("服务已就绪", "socket", socketPath, "tcp_port", port)

	serverErr := make(chan error, 2)
	if sockLn != nil {
		go func() {
			if err := srv.Serve(sockLn); err != nil && err != http.ErrServerClosed {
				serverErr <- err
			}
		}()
	}
	if tcpLn != nil {
		go func() {
			if err := srv.Serve(tcpLn); err != nil && err != http.ErrServerClosed {
				serverErr <- err
			}
		}()
	}

	select {
	case <-ctx.Done():
		logger.Info("收到退出信号，正在停止服务")
	case err := <-serverErr:
		logger.Error("HTTP 服务异常终止", "error", err)
	}

	// 顺序很关键：**先断开 SSE，再 Shutdown**。
	//
	// Shutdown 会等所有连接变为空闲后才返回，而 SSE 是长连接，永远不会
	// 自己变空闲。早期版本把关闭 SSE 的调用排在 Shutdown 后面，于是它永远
	// 等不到 —— 每次停止都耗满这 8 秒，再被应用中心的 10 秒宽限兜底 SIGKILL，
	// 连下面那句"已退出"都打印不出来（真机日志里确实没有）。
	application.CloseEvents()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("优雅关闭超时，强制退出", "error", err)
	}
	application.Close()
	gateway.Remove(socketPath)
	logger.Info("QLink2Desktop 已退出")
}

/* ------------------------------------------------------------------ 分发 */

// buildDispatcher 组装请求分发链：启动闸门 → 路径前缀剥离 → 真实处理器。
func buildDispatcher(application *app.App, ready *atomic.Bool) http.Handler {
	handler := application.Handler()

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !ready.Load() {
			serveStarting(w, r)
			return
		}
		// 网关在某些版本上会把 /app/<appname> 前缀一起转发过来。
		// 不剥掉的话所有路由都会 404，表现为"页面能打开但接口全挂"。
		if trimmed := gateway.StripAppPrefix(r.URL.Path, appName, []string{"qlink2desktop"}); trimmed != r.URL.Path {
			// 飞牛网关会把 ui/config 里 url 字段的尾斜杠吃掉，
			// iframe 的文档 URL 变成 /app/<appname>（无斜杠）。
			// 此时前端所有相对路径都会以 /app/ 为基准解析成
			// /app/assets/... → 网关 404，表现为「加载页卡住」。
			// 在这里把无斜杠的文档请求 302 到目录形态，一劳永逸。
			if trimmed == "/" && !strings.HasSuffix(r.URL.Path, "/") &&
				(r.Method == http.MethodGet || r.Method == http.MethodHead) {
				target := r.URL.Path + "/"
				if r.URL.RawQuery != "" {
					target += "?" + r.URL.RawQuery
				}
				http.Redirect(w, r, target, http.StatusFound)
				return
			}
			r2 := r.Clone(r.Context())
			r2.URL.Path = trimmed
			r2.RequestURI = trimmed
			if r.URL.RawQuery != "" {
				r2.RequestURI = trimmed + "?" + r.URL.RawQuery
			}
			handler.ServeHTTP(w, r2)
			return
		}
		handler.ServeHTTP(w, r)
	})
}

// serveStarting 在服务尚未就绪时返回一张自刷新的启动页。
//
// 对 API 请求返回 JSON，这样前端的 fetch 能读到明确状态而不是一坨 HTML。
func serveStarting(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"服务正在启动，请稍候","kind":"starting"}`))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(startingHTML))
}

/* ------------------------------------------------------------------ 辅助 */

// resolveDataDir 决定数据目录，优先级：命令行 > 环境变量 > 飞牛包变量 > ./data。
func resolveDataDir(flagValue string) string {
	if v := strings.TrimSpace(flagValue); v != "" {
		return v
	}
	for _, key := range []string{"QLINK_DATA_DIR", "DATA_DIR"} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
	}
	// 飞牛把每个应用的可写目录放在 TRIM_PKGVAR 下，这是最合适的位置。
	if v := strings.TrimSpace(os.Getenv("TRIM_PKGVAR")); v != "" {
		return filepath.Join(v, "data")
	}
	return "data"
}

func resolveHost(flagValue string) string {
	if v := strings.TrimSpace(flagValue); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("HOST")); v != "" {
		return v
	}
	return "0.0.0.0"
}

// resolvePort 决定是否监听 TCP。
//
// 关键决策：只要统一网关的套接字可用，就默认**不开** TCP 端口。
// 这正是"零端口占用"的价值所在 —— 用户不需要为这个面板腾出一个端口，
// 也不会和别的应用冲突。
func resolvePort(flagValue int, hasSocket bool) int {
	if flagValue > 0 {
		return flagValue
	}
	if v := strings.TrimSpace(os.Getenv("PORT")); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 && p <= 65535 {
			return p
		}
	}
	if hasSocket {
		return 0
	}
	// 没有套接字就必须自己监听，否则服务无法访问。
	return 5900
}

func listenWithRetry(addr string, attempts int) (net.Listener, error) {
	var lastErr error
	for i := 0; i < attempts; i++ {
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			return ln, nil
		}
		lastErr = err
		// 刚重启时上一个进程的监听可能还在 TIME_WAIT，稍等即可成功。
		time.Sleep(400 * time.Millisecond)
	}
	return nil, lastErr
}

// listenOnFreePort 依次尝试 requested 之后的端口，全部失败则让系统随机分配。
func listenOnFreePort(host string, requested int) (net.Listener, int, error) {
	for p := requested + 1; p <= requested+20 && p <= 65535; p++ {
		ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", host, p))
		if err == nil {
			return ln, p, nil
		}
	}
	ln, err := net.Listen("tcp", host+":0")
	if err != nil {
		return nil, 0, err
	}
	if tcp, ok := ln.Addr().(*net.TCPAddr); ok {
		return ln, tcp.Port, nil
	}
	return ln, 0, nil
}

// logLevel 读取日志级别，默认 info。
func logLevel() slog.Level {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("QLINK_LOG_LEVEL"))) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

const startingHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>正在启动 · QLink2Desktop</title>
<style>
  :root { color-scheme: light dark; }
  body { margin:0; min-height:100vh; display:flex; align-items:center; justify-content:center;
         background:#f5f7fa; color:#0f172a;
         font-family:-apple-system,BlinkMacSystemFont,"Segoe UI","PingFang SC","Microsoft YaHei",sans-serif; }
  @media (prefers-color-scheme: dark) { body { background:#0b1120; color:#e8edf6; } .card { background:#131c31 !important; border-color:#243149 !important; } p { color:#8b98ab !important; } }
  .card { text-align:center; padding:2.4rem 2rem; max-width:340px; width:88%;
          background:#fff; border:1px solid #e2e8f0; border-radius:16px;
          box-shadow:0 10px 30px -12px rgba(15,23,42,.18); }
  .spinner { width:42px; height:42px; margin:0 auto 1.4rem;
             border:3px solid #e2e8f0; border-top-color:#2563eb; border-radius:50%;
             animation:spin .8s linear infinite; }
  @keyframes spin { to { transform:rotate(360deg); } }
  h1 { font-size:1.1rem; margin:0 0 .4rem; letter-spacing:-.01em; }
  p { font-size:.85rem; color:#64748b; margin:0; line-height:1.6; }
</style>
</head>
<body>
  <div class="card">
    <div class="spinner"></div>
    <h1>QLink2Desktop</h1>
    <p>服务正在启动，页面将自动刷新…</p>
  </div>
  <script>setTimeout(function () { location.reload(); }, 1200);</script>
</body>
</html>`
