// Package app 负责把各个子系统装配成一个可运行实例。
//
// 它是整个项目里唯一"知道全部零件"的地方：store、fnos、proxy、discovery、
// auth、httpapi 在这里被连起来，并在退出时按相反顺序收尾。
// 其它包之间只通过窄接口协作，因此可以各自独立测试。
package app

import (
	"context"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/MisiteQ/qlink2desktop/internal/auth"
	"github.com/MisiteQ/qlink2desktop/internal/discovery"
	"github.com/MisiteQ/qlink2desktop/internal/fnos"
	"github.com/MisiteQ/qlink2desktop/internal/httpapi"
	"github.com/MisiteQ/qlink2desktop/internal/logx"
	"github.com/MisiteQ/qlink2desktop/internal/proxy"
	"github.com/MisiteQ/qlink2desktop/internal/store"
)

// 后台任务的时间参数。
const (
	// startupDelay 是「启动后多久开始对账」。
	//
	// 留出这段时间是为了让网关先把进程当成"已就绪"，避免用户点开图标时
	// 正好撞上一轮全量安装导致首屏长时间空白。
	startupDelay = 900 * time.Millisecond

	// guardInterval 是周期守护的间隔。
	//
	// 只做「确保代理还活着」这种极轻量的动作，不碰桌面图标 ——
	// 每分钟重装一遍图标是原实现留下来的最明显的体验问题之一。
	guardInterval = 60 * time.Second
)

// Options 是装配参数。
type Options struct {
	DataDir  string
	IconsDir string
	LogDir   string
	Version  string
	// PortBase 是分配代理端口的起点。
	PortBase int
	// AllowRemoteIcon 控制是否允许为图标发起外网请求（离线部署应关闭）。
	AllowRemoteIcon bool
	// WebFS 是内嵌的前端资源；为 nil 时只提供接口。
	WebFS fs.FS
	// Logger 与 Logs 由调用方（main）先建好，因为装配过程本身也要记日志。
	Logger *slog.Logger
	Logs   *logx.Ring
	// CLI 允许注入假的应用中心命令行（测试用）。
	CLI fnos.CLI
}

// App 是装配完成的运行实例。
type App struct {
	Store       *store.Store
	Service     *fnos.Service
	Proxy       *proxy.Manager
	Coordinator *Coordinator
	Scanner     *discovery.Scanner
	Docker      *discovery.DockerClient
	Sessions    *auth.Manager
	Server      *httpapi.Server

	Version string
	logger  *slog.Logger
}

// New 按依赖顺序装配全部子系统。
func New(opts Options) (*App, error) {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	dataDir := opts.DataDir
	if dataDir == "" {
		dataDir = "data"
	}
	iconsDir := opts.IconsDir
	if iconsDir == "" {
		iconsDir = filepath.Join(dataDir, "icons")
	}
	logDir := opts.LogDir
	if logDir == "" {
		logDir = filepath.Join(dataDir, "logs")
	}
	if err := os.MkdirAll(iconsDir, 0o755); err != nil {
		return nil, err
	}

	// 1) 持久化。
	st, err := store.Open(dataDir)
	if err != nil {
		return nil, err
	}

	// 2) 服务发现（只读，无副作用）。
	docker := discovery.NewDockerClient("")
	scanner := discovery.NewScanner(discovery.ScannerOptions{
		ProcPath: DetectProcPath(),
		Docker:   docker,
	})

	// 3) 反向代理。
	pm := proxy.NewManager(proxy.WithLogger(logger))

	// 4) 飞牛桌面注册。
	svc := fnos.NewService(fnos.Options{
		CLI:      opts.CLI,
		IconsDir: iconsDir,
		// 重启脚本会活过本进程，它的日志只能落在磁盘上 —— 见 restartLogPath。
		LogDir:          logDir,
		AllowRemoteIcon: opts.AllowRemoteIcon,
	})

	// 图标目录可以在设置页运行时修改，持久化在 settings 里，启动时应用回去。
	//
	// 失败**不阻断启动**：最常见的原因是目标卷（外接盘 / 其它存储池）还没挂上，
	// 这时候退回默认目录、把话说清楚，比整个应用起不来好得多。
	if dir := strings.TrimSpace(st.Settings().IconsDir); dir != "" {
		if err := svc.SetIconsDir(dir); err != nil {
			logger.Warn("自定义图标目录不可用，回退到默认目录",
				"configured", dir, "fallback", iconsDir, "error", err)
		} else if dir != iconsDir {
			logger.Info("图标目录取自设置", "dir", dir)
		}
	}

	// 5) 编排器：把前四者串起来。
	coord := NewCoordinator(st, svc, pm, opts.PortBase, logger)

	// 6) 会话。
	sessions := auth.NewManager(0)

	// 7) HTTP 接口。
	//
	// 扫描任务管理器需要能在结束时通知接口层广播，而接口层又需要它才能注册路由 ——
	// 于是先把 server 声明出来，回调里做一次空判断。回调只可能在用户点了
	// 「开始扫描」之后才触发，那时 server 早已装配完毕。
	var server *httpapi.Server
	scanRunner := discovery.NewScanRunner(func(state discovery.ScanState) {
		logger.Info("网络扫描结束",
			"range", state.Range, "hosts", state.HostsTotal, "alive", state.HostsAlive,
			"found", state.Found, "canceled", state.Canceled,
			"elapsed", time.Duration(state.ElapsedMs)*time.Millisecond)
		if server != nil {
			server.BroadcastDiscovery()
		}
	})

	server = httpapi.New(httpapi.Deps{
		Store:       st,
		Service:     svc,
		Proxy:       pm,
		Scanner:     scanner,
		Docker:      docker,
		Sessions:    sessions,
		Logs:        opts.Logs,
		Logger:      logger,
		Coordinator: coord,
		ScanRunner:  scanRunner,
		IconsDir:    iconsDir,
		LogDir:      logDir,
		WebFS:       opts.WebFS,
		Version:     opts.Version,
		PortBase:    opts.PortBase,
	})

	// 8) 把「后台任务引起的状态变化」接到 SSE 上。
	//
	// 这一步是异步响应模型的最后一公里：接口层受理后立即返回，
	// 真正的安装 / 注销跑在后台队列里，如果状态变化没人广播，
	// 前端就永远停在「排队中」——一个不会自愈的假进度。
	// 广播的是完整快照，前端收到后直接替换内存状态，不必再回查一次。
	coord.SetOnChange(server.BroadcastStatus)

	return &App{
		Store:       st,
		Service:     svc,
		Proxy:       pm,
		Coordinator: coord,
		Scanner:     scanner,
		Docker:      docker,
		Sessions:    sessions,
		Server:      server,
		Version:     opts.Version,
		logger:      logger,
	}, nil
}

// Handler 返回 HTTP 处理器。
func (a *App) Handler() http.Handler { return a.Server.Handler() }

// Supervise 启动后台任务：一次性对账 + 周期性轻量守护。
//
// 它是一个阻塞函数，应当在独立 goroutine 中运行，并在 ctx 取消时返回。
func (a *App) Supervise(ctx context.Context) {
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(startupDelay):
		}

		settings := a.Store.Settings()
		if !settings.AutoReconcile {
			a.logger.Info("已关闭启动时自动对账，仅恢复反向代理")
			a.Coordinator.EnsureRoutes()
			return
		}

		a.logger.Info("开始启动对账")
		start := time.Now()
		result := a.Coordinator.Reconcile(ctx)
		a.logger.Info("启动对账结束",
			"elapsed", time.Since(start).Round(time.Millisecond).String(),
			"installed", len(result.Installed),
			"started", len(result.Started),
			"upgraded", len(result.Upgraded),
			"failed", len(result.Failed))
	}()

	ticker := time.NewTicker(guardInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.Coordinator.EnsureRoutes()
			if n := a.Sessions.GC(); n > 0 {
				a.logger.Debug("已清理过期会话", "count", n)
			}
		}
	}
}

// Close 收尾：先停后台队列（连带代理）再关接口层。
//
// 顺序上先停队列是为了让"服务不可用"与"端口已释放"同时发生，
// 不会出现监听还在、处理器已死的中间态。
func (a *App) Close() {
	a.Coordinator.Stop()
	a.CloseEvents()
}

// CloseEvents 断开所有 SSE 长连接。
//
// 必须在 http.Server.Shutdown **之前**调用，否则退出会被硬生生拖慢：
// Shutdown 会等所有连接变为空闲，而 SSE 是长连接、永远不会自己变空闲 ——
// 关闭它的代码如果排在 Shutdown 后面，就永远轮不到执行。
//
// 真机上的表现是每次停止都要耗满 Shutdown 的 8 秒预算，然后被应用中心的
// 10 秒宽限兜底 SIGKILL，连最后那句"已退出"都来不及打印。
// 升级安装时同样走这条路径，所以它影响的不只是"重启"这一个功能。
func (a *App) CloseEvents() {
	if a.Server != nil {
		a.Server.Close()
	}
}

// DetectProcPath 选出能看到宿主机进程的 procfs 挂载点。
//
// 飞牛应用运行在受控环境里，容器内的 /proc 往往只反映自身沙箱，
// 直接读它会得到一份"几乎什么服务都没在跑"的假清单。
// 网关通常会额外挂载宿主机根文件系统，优先用那里的 proc。
func DetectProcPath() string {
	candidates := []string{
		"/host/root/proc",
		"/host/proc",
		"/proc",
	}
	for _, p := range candidates {
		// 用 net/tcp 作为探针：它一定存在，且正是我们最需要读的文件。
		if fi, err := os.Stat(filepath.Join(p, "net", "tcp")); err == nil && !fi.IsDir() {
			return p
		}
	}
	return "/proc"
}
