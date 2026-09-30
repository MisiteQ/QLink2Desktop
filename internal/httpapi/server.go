// Package httpapi 组装 QLink2Desktop 的 HTTP 接口。
//
// 分层原则：这一层只做「解析请求 → 调用领域服务 → 序列化响应」，
// 不含任何业务规则。所有判断都下沉到 store / fnos / proxy / discovery，
// 因此接口层可以用很少的测试覆盖，真正的逻辑由各子系统的单测保证。
package httpapi

import (
	"bytes"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/MisiteQ/qlink2desktop/internal/auth"
	"github.com/MisiteQ/qlink2desktop/internal/discovery"
	"github.com/MisiteQ/qlink2desktop/internal/domain"
	"github.com/MisiteQ/qlink2desktop/internal/fnos"
	"github.com/MisiteQ/qlink2desktop/internal/logx"
	"github.com/MisiteQ/qlink2desktop/internal/proxy"
	"github.com/MisiteQ/qlink2desktop/internal/store"
)

// Coordinator 抽象「把链接定义同步到飞牛桌面与内置代理」的编排动作。
//
// 之所以在这里定义接口而不是直接依赖 app 包：
// 方向必须是 app → httpapi，接口层不应该反向依赖装配层。有了这个接口，
// 接口层可以完全用假实现测试，不受真实 appcenter-cli / 端口占用影响。
//
// 这个接口**只暴露受理型动作**，是有意为之的约束：
//
//	凡是要调用 appcenter-cli 的动作（注册 / 注销 / 对账 / 清理）单条最长 3 分钟，
//	把它放进请求路径，前端 15 秒的网络预算必然超时，于是用户看到一条
//	「请求超时」——而服务端其实还在正常干活，那条报错是假的（真机真实发生过）。
//
// 因此接口层一律「受理 + 立刻回报当前阶段」，真正的执行结果通过状态机
// （PhasePending → PhaseInstalled / PhaseFailed）与 SSE 事件回传。
// 想在这里调用同步版本，先问一句：这个动作的耗时上限是多少？
type Coordinator interface {
	// Views 返回全部链接的定义 + 实时运行态。
	Views() []domain.View
	// View 返回单条链接的视图。
	View(id string) (domain.View, bool)
	// ProxyPort 返回某条链接当前实际使用的本机代理端口（0 表示未启用）。
	ProxyPort(id string) int

	// QueueSync 受理一次「把链接同步到飞牛桌面」。
	QueueSync(id string)
	// QueueSetEnabled 立刻落盘启用状态，并把随之而来的图标动作交给后台。
	QueueSetEnabled(id string, enabled bool) error
	// QueueRemove 立刻摘除链接定义与代理监听，注销桌面图标交给后台。
	QueueRemove(id string) (domain.Link, error)
	// QueueReconcile 受理一次全量对账。
	QueueReconcile()
	// QueueCleanupOrphans 受理一次「清理遗留图标」。
	QueueCleanupOrphans()
	// QueueRestartSelf 受理一次「重启本应用服务」。
	QueueRestartSelf()
	// Pending 返回后台队列里还没做完的工作数量。
	Pending() int
}

// Deps 是构造 Server 所需的全部依赖。
type Deps struct {
	Store    *store.Store
	Service  *fnos.Service
	Proxy    *proxy.Manager
	Scanner  *discovery.Scanner
	Docker   *discovery.DockerClient
	Sessions *auth.Manager
	Logs     *logx.Ring
	Logger   *slog.Logger

	// Coordinator 为 nil 时，接口层会退化为「只读写定义、不落地」的只读模式，
	// 便于在没有 appcenter-cli 的开发机上调试前端。
	Coordinator Coordinator

	// ScanRunner 是后台网络扫描任务管理器。
	//
	// 扫描不能同步跑在请求里：一个 /24 网段要发上万个 TCP 连接，真机上
	// 十几秒起步，必然撞上前端 15 秒的请求预算（真机上就是这么失败的）。
	// 为 nil 时扫描接口返回明确错误，而不是假装受理。
	ScanRunner *discovery.ScanRunner

	// IconsDir 是图标目录的**兜底**默认值。
	//
	// 实际生效的值以 deps.Service.IconsDir() 为准——它是可以在设置页
	// 运行时修改的。这里保留一份是为了在没有 Service 的只读调试模式下
	// 仍能读图标。见 Server.iconsDir()。
	IconsDir string
	// LogDir 是日志目录，用于列出并下载历史日志。
	LogDir string
	// WebFS 内嵌的前端资源；为 nil 时只提供 API，不提供页面。
	WebFS fs.FS
	// Version 是当前构建版本号，展示在「关于」里。
	Version string
	// PortBase 是分配代理端口的起点。
	PortBase int
	// ProxyHostLimit 限制远端端口扫描的主机数量上限，避免误点导致长时间扫描。
	ProxyHostLimit int
}

// Server 持有路由表与运行期依赖。
type Server struct {
	deps      Deps
	hub       *hub
	mux       *http.ServeMux
	startedAt time.Time
	limiter   *loginLimiter
}

// New 构造 Server 并注册全部路由。
func New(deps Deps) *Server {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.PortBase <= 0 {
		deps.PortBase = proxy.DefaultProxyPortBase
	}
	if deps.ProxyHostLimit <= 0 {
		deps.ProxyHostLimit = 254
	}

	s := &Server{
		deps:      deps,
		hub:       newHub(),
		mux:       http.NewServeMux(),
		startedAt: time.Now(),
		limiter:   newLoginLimiter(),
	}
	s.routes()
	return s
}

// Handler 返回带全部中间件的处理器。
func (s *Server) Handler() http.Handler {
	return withSecurityHeaders(
		withRecover(s.deps.Logger)(
			withRequestLog(s.deps.Logger)(s.mux)))
}

// Close 释放后台资源。
func (s *Server) Close() {
	s.hub.close()
}

// Subscribers 返回当前 SSE 订阅者数量（诊断用）。
func (s *Server) Subscribers() int { return s.hub.count() }

// BroadcastStatus 把最新状态推给所有前端，由装配层注入到编排器的 onChange 上。
//
// 它是异步响应模型的「最后一公里」：后台队列把一条链接从「排队中」推到
// 「已就绪」时，如果没人广播，前端就永远停在排队中 —— 用户看到的是一个
// 不会自愈的假进度，而服务端其实早就干完了。
//
// 推的是**完整快照**而不是一条空通知：前端收到后直接替换内存状态并刷新，
// 省掉一次 GET /api/links 的往返。这是异步模型下最高频的一次更新。
func (s *Server) BroadcastStatus() { s.broadcast(EventStatusChanged, s.snapshot()) }

// BroadcastDiscovery 通知前端「端口 / 扫描结果有更新」。
//
// 只在一轮扫描结束时调用一次，不跟进度 —— 进度是前端自己轮询的，
// 每 300 毫秒推一条 SSE 只会把事件流变成噪声。
func (s *Server) BroadcastDiscovery() { s.broadcast(EventDiscovery, nil) }

// ---------------------------------------------------------------------------
// 路由
// ---------------------------------------------------------------------------

func (s *Server) routes() {
	// ---- 公开端点 -------------------------------------------------------
	// 健康检查：必须无鉴权，否则网关与外部探活无法判断服务是否活着。
	s.mux.HandleFunc("GET /api/health", s.handleHealth)
	// 登录相关：本身就必须在鉴权之外。
	s.mux.HandleFunc("GET /api/auth/status", s.handleAuthStatus)
	s.mux.HandleFunc("POST /api/auth/login", s.handleAuthLogin)
	s.mux.HandleFunc("POST /api/auth/logout", s.handleAuthLogout)
	// 图标与前端资源：桌面应用中心的 <img> 会直接引用，带不了令牌。
	s.mux.HandleFunc("GET /icons/{name}", s.handleServeIcon)
	if s.deps.WebFS != nil {
		s.mux.Handle("GET /assets/", http.StripPrefix("/assets/", s.assetHandler()))
		// 注意这里必须用 /{$}（仅精确匹配根路径），不能写成 GET /。
		//
		// Go 1.22+ 的 ServeMux 会在「模式无法排出明确优先级」时直接 panic：
		// `GET /` 与无方法限定的 `/api/` 谁也不覆盖谁，注册第二个就会崩，
		// 表现为服务一启动就 panic 退出、网关 502。
		// /{$} 只匹配根路径，与 /api/ 完全不相交，路由表才能稳定建立。
		s.mux.HandleFunc("GET /{$}", s.handleSPA)
	}

	// ---- 受保护端点 -----------------------------------------------------
	api := http.NewServeMux()

	api.HandleFunc("GET /api/links", s.handleListLinks)
	api.HandleFunc("POST /api/links", s.handleCreateLink)
	api.HandleFunc("GET /api/links/export", s.handleExportLinks)
	api.HandleFunc("GET /api/links/{id}", s.handleGetLink)
	api.HandleFunc("PUT /api/links/{id}", s.handleUpdateLink)
	api.HandleFunc("DELETE /api/links/{id}", s.handleDeleteLink)
	api.HandleFunc("POST /api/links/{id}/enabled", s.handleToggleLink)
	api.HandleFunc("POST /api/links/{id}/sync", s.handleSyncLink)
	api.HandleFunc("POST /api/links/{id}/probe", s.handleProbeLink)

	api.HandleFunc("GET /api/settings", s.handleGetSettings)
	api.HandleFunc("POST /api/settings", s.handleUpdateSettings)

	api.HandleFunc("GET /api/discovery/ports", s.handleLocalPorts)
	api.HandleFunc("GET /api/discovery/subnets", s.handleSubnets)
	api.HandleFunc("GET /api/discovery/docklabels", s.handleDockLabels)
	api.HandleFunc("POST /api/discovery/docklabels/enable", s.handleEnableDockLabel)
	// 扫描是一个「开始 → 轮询 → 可取消」的后台任务，见 handleScanNetwork 的注释。
	api.HandleFunc("POST /api/discovery/scan", s.handleScanNetwork)
	api.HandleFunc("GET /api/discovery/scan", s.handleScanStatus)
	api.HandleFunc("DELETE /api/discovery/scan", s.handleCancelScan)

	api.HandleFunc("GET /api/hosts", s.handleListHosts)
	api.HandleFunc("POST /api/hosts", s.handleSaveHost)
	api.HandleFunc("DELETE /api/hosts/{id}", s.handleDeleteHost)
	api.HandleFunc("POST /api/hosts/{id}/probe", s.handleProbeHost)
	api.HandleFunc("GET /api/hosts/{id}/ports", s.handleHostPorts)

	api.HandleFunc("GET /api/icons", s.handleListIcons)
	api.HandleFunc("POST /api/icons/upload", s.handleUploadIcon)
	api.HandleFunc("DELETE /api/icons/{name}", s.handleDeleteIcon)

	api.HandleFunc("GET /api/tools/port", s.handlePickPort)
	api.HandleFunc("POST /api/tools/probe", s.handleProbeTarget)

	api.HandleFunc("GET /api/system", s.handleSystem)
	api.HandleFunc("GET /api/bootstrap", s.handleBootstrap)
	api.HandleFunc("POST /api/system/reconcile", s.handleReconcile)
	api.HandleFunc("POST /api/system/cleanup-orphans", s.handleCleanupOrphans)
	api.HandleFunc("POST /api/system/restart", s.handleRestart)
	api.HandleFunc("GET /api/logs", s.handleGetLogs)
	api.HandleFunc("GET /api/logs/download", s.handleDownloadLog)
	api.HandleFunc("POST /api/logs/client", s.handleClientLog)
	api.HandleFunc("GET /api/events", s.handleEvents)

	// 子路由表也要有兜底：否则未知 /api/ 路径会落到 net/http 默认的
	// 纯文本 404，前端拿到非 JSON 响应会解析失败。
	api.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, errorBody{Error: "接口不存在", Kind: "not_found"})
	})

	s.mux.Handle("/api/", s.requireAuth(api))

	// 未匹配的路径：浏览器导航请求回落到 SPA 入口（方便深链接），其余返回
	// JSON 404，避免前端拿到一坨 HTML 解析失败。
	s.mux.HandleFunc("/", s.handleFallback)
}

// ---------------------------------------------------------------------------
// 快照
// ---------------------------------------------------------------------------

// snapshot 是 SSE 建连时推送的初始全量数据。
//
// 带上 links / settings / system 三块，前端拿到后即可完成首屏渲染，
// 省掉「先连 SSE 再补三个 GET」的往返。
type snapshot struct {
	Links    []domain.View   `json:"links"`
	Settings domain.Settings `json:"settings"`
	System   systemInfo      `json:"system"`
}

func (s *Server) snapshot() snapshot {
	return snapshot{
		Links:    s.views(),
		Settings: s.deps.Store.Settings(),
		// SSE 建连是高频路径，同样不跑外部命令探测。
		System: s.systemInfo(false),
	}
}

// views 返回全部链接视图；无 Coordinator 时降级为「仅定义、状态未知」。
func (s *Server) views() []domain.View {
	if s.deps.Coordinator != nil {
		return s.deps.Coordinator.Views()
	}
	links := s.deps.Store.ListLinks()
	out := make([]domain.View, 0, len(links))
	for _, l := range links {
		out = append(out, domain.NewView(l, domain.Status{
			Phase: domain.PhaseUnknown,
		}))
	}
	return out
}

// ---------------------------------------------------------------------------
// 前端资源
// ---------------------------------------------------------------------------

// assetHandler 提供前端静态资源。
func (s *Server) assetHandler() http.Handler {
	sub, err := fs.Sub(s.deps.WebFS, "assets")
	if err != nil {
		return http.NotFoundHandler()
	}
	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 静态资源一律 no-store。
		//
		// 曾经用的是 no-cache（协商缓存）。那个策略有个致命前提：
		// 浏览器手上那份缓存**必须是本版本的**。可升级路径里并不成立 ——
		// 真机实测：新包装上后，浏览器仍在跑上一版 app.js（它按旧布局
		// import ./dom.js），于是所有模块 404，界面停在启动页，
		// 而服务端一切正常，排查方向被彻底带偏。
		//
		// 前端资源总共十来个小文件，局域网下全量重取的代价可以忽略，
		// 换来的是「装了什么版本就一定跑什么版本」这个绝对性质。
		// 叠加 index.html 里的 ?v=<版本号>，新旧 URL 天然不同，双保险。
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
		w.Header().Set("Pragma", "no-cache")
		fileServer.ServeHTTP(w, r)
	})
}

// assetVersionToken 是 index.html 里等待被替换成真实版本号的占位符。
const assetVersionToken = "__V__"

// handleSPA 返回前端入口页面。
//
// 刻意不复用 http.FileServer：SPA 需要「任何未命中的路径都回落到 index.html」，
// 而 FileServer 会直接给 404。同时 index.html 必须完全不可缓存 ——
// 它是整个应用的根引用，它一旧，引用的资源就全是旧的。
//
// 返回前把资源 URL 上的 __V__ 换成真实版本号。为什么必须这么做：
// style.css / app.js 的文件名不含哈希，一旦浏览器在某个旧版本用强缓存
// （max-age）存下过它们，升级后会继续用旧 JS 而不回源 —— 表现就是
// "新版本装了却没生效，界面还是老样子"。换了 URL 就等于绕开旧缓存。
func (s *Server) handleSPA(w http.ResponseWriter, r *http.Request) {
	data, err := fs.ReadFile(s.deps.WebFS, "index.html")
	if err != nil {
		writeJSON(w, http.StatusNotFound, errorBody{Error: "前端资源缺失", Kind: "not_found"})
		return
	}

	version := strings.TrimSpace(s.deps.Version)
	if version == "" {
		// 没有版本号时也不能把占位符原样吐出去，否则资源 URL 会变成
		// ?v=__V__ 这种诡异取值，排查时反而更困惑。
		version = "dev"
	}
	data = bytes.ReplaceAll(data, []byte(assetVersionToken), []byte(url.QueryEscape(version)))

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// no-store 而不是 no-cache：no-cache 仍会保留副本、由 revalidate 决定新鲜度，
	// 一旦任何中间层（网关 / 代理）对 HTML 做了缓存，用户就会被钉在旧页面上。
	// 入口页面只有几 KB，完全不该有副本存在。
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// handleFallback 兜底所有未被路由表命中的请求。
//
// 两种归宿：
//   - 浏览器地址栏 / 刷新（Accept 带 text/html）→ 回落 SPA，前端自行处理未知路径；
//   - 其余（脚本、fetch、探测器）→ JSON 404。
//
// 判据用 Accept 而不是路径：深链接的路径形状无法穷举，
// 而浏览器导航请求一定带 text/html，这是 HTTP 语义里最稳的信号。
func (s *Server) handleFallback(w http.ResponseWriter, r *http.Request) {
	if s.deps.WebFS != nil && strings.Contains(r.Header.Get("Accept"), "text/html") {
		s.handleSPA(w, r)
		return
	}
	writeJSON(w, http.StatusNotFound, errorBody{Error: "接口不存在", Kind: "not_found"})
}

// healthResponse 是健康检查的响应体。
type healthResponse struct {
	OK        bool      `json:"ok"`
	Version   string    `json:"version"`
	Time      time.Time `json:"time"`
	CLI       bool      `json:"appcenter_cli"`
	Protected bool      `json:"password_protected"`
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ok(w, healthResponse{
		OK:        true,
		Version:   s.deps.Version,
		Time:      time.Now(),
		CLI:       s.deps.Service != nil && s.deps.Service.Available(),
		Protected: s.authEnabled(),
	})
}
