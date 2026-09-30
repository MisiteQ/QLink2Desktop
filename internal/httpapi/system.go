package httpapi

import (
	"fmt"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/MisiteQ/qlink2desktop/internal/discovery"
	"github.com/MisiteQ/qlink2desktop/internal/domain"
	"github.com/MisiteQ/qlink2desktop/internal/fnos"
)

// ---------------------------------------------------------------------------
// 系统概览
// ---------------------------------------------------------------------------

// systemInfo 是「系统」面板展示的全部信息。
//
// 集中成一个结构体而不是让前端连打七八个接口：
// 用户点开系统页时最想知道的是"一切是否正常"，
// 分开拉取只会让页面慢慢分块出现，观感很差。
type systemInfo struct {
	Version   string    `json:"version"`
	GoVersion string    `json:"go_version"`
	OS        string    `json:"os"`
	Arch      string    `json:"arch"`
	StartedAt time.Time `json:"started_at"`
	Uptime    string    `json:"uptime"`

	DataDir  string `json:"data_dir"`
	LogDir   string `json:"log_dir"`
	IconsDir string `json:"icons_dir"`

	// AppcenterCLI 表示飞牛应用中心命令行是否可用。
	AppcenterCLI bool `json:"appcenter_cli"`
	// CLIPath 是探测到的可执行文件路径。
	CLIPath string `json:"cli_path,omitempty"`
	// Simulated 为 true 表示当前处于「只打包不注册」的模拟模式。
	Simulated bool `json:"simulated"`

	DefaultVolume int      `json:"default_volume"`
	InstalledApps []string `json:"installed_apps"`

	Links        int `json:"links"`
	EnabledLinks int `json:"enabled_links"`
	ProxyRoutes  int `json:"proxy_routes"`
	// PendingJobs 是后台队列里还没做完的工作数。
	//
	// 它存在的意义是让「排队中」这个状态可被观测：用户看到一行停留在
	// 排队中时，可以确认队列里确实还压着活，而不是界面卡死了。
	PendingJobs int `json:"pending_jobs"`

	Docker          bool `json:"docker"`
	SSH             bool `json:"ssh"`
	PasswordProtect bool `json:"password_protected"`
	Sessions        int  `json:"sessions"`
	Subscribers     int  `json:"sse_subscribers"`
	LogLines        int  `json:"log_lines"`
	PortBase        int  `json:"port_base"`
}

func (s *Server) handleSystem(w http.ResponseWriter, r *http.Request) {
	ok(w, s.systemInfo(true))
}

// bootstrapResponse 是前端启动所需数据的合集。
type bootstrapResponse struct {
	Links     []domain.View   `json:"links"`
	Settings  domain.Settings `json:"settings"`
	Protected bool            `json:"protected"`
	System    systemInfo      `json:"system"`
}

// handleBootstrap 一次返回前端首屏所需的全部数据。
//
// 为什么要合并成一个接口：飞牛统一网关（Unix socket 转发）在部分版本上
// 不能可靠地并发处理同一页面的多个并行请求——HTML、JS 这类串行资源都正常，
// 但前端启动时并发发出的三个 API 请求会一起悬住，表现为界面永远停在
// 「正在读取配置与桌面应用…」，控制台干净无报错。合并成单请求后，
// 启动路径只依赖一次成功转发，同时省掉两个往返。
func (s *Server) handleBootstrap(w http.ResponseWriter, r *http.Request) {
	settings := s.deps.Store.Settings()
	// 摘要与盐绝不能出网。
	settings.AuthHash = ""
	settings.AuthSalt = ""

	ok(w, bootstrapResponse{
		Links:     s.views(),
		Settings:  settings,
		Protected: s.deps.Store.Settings().HasPassword(),
		// 启动路径不跑 appcenter-cli 探测（可能耗时数秒），
		// 这些重信息留给「系统与日志」页按需拉取。
		System: s.systemInfo(false),
	})
}

// systemInfo 汇总系统概览。
//
// probeCLI 为 false 时跳过外部命令探测（appcenter-cli list / default-volume），
// 只回报内存态信息——启动路径必须快，不能被外部命令拖住。
func (s *Server) systemInfo(probeCLI bool) systemInfo {
	info := systemInfo{
		Version:     s.deps.Version,
		GoVersion:   runtime.Version(),
		OS:          runtime.GOOS,
		Arch:        runtime.GOARCH,
		StartedAt:   s.startedAt,
		Uptime:      time.Since(s.startedAt).Round(time.Second).String(),
		IconsDir:    s.iconsDir(),
		LogDir:      s.deps.LogDir,
		PortBase:    s.deps.PortBase,
		Subscribers: s.hub.count(),
		PendingJobs: s.pendingJobs(),
	}
	if s.deps.Store != nil {
		info.DataDir = s.deps.Store.Dir()
		links := s.deps.Store.ListLinks()
		info.Links = len(links)
		for _, l := range links {
			if l.Enabled {
				info.EnabledLinks++
			}
		}
		info.PasswordProtect = s.deps.Store.Settings().HasPassword()
	}
	if s.deps.Sessions != nil {
		info.Sessions = s.deps.Sessions.Count()
	}
	if s.deps.Logs != nil {
		info.LogLines = s.deps.Logs.Len()
	}
	if s.deps.Proxy != nil {
		info.ProxyRoutes = len(s.deps.Proxy.Running())
	}
	if s.deps.Service != nil {
		info.AppcenterCLI = s.deps.Service.Available()
		info.Simulated = !info.AppcenterCLI
		if probeCLI && info.AppcenterCLI {
			if cli := s.deps.Service.CLI(); cli != nil {
				if p, ok := cli.(*fnos.ExecCLI); ok {
					info.CLIPath = p.Path()
				}
				if v, err := cli.DefaultVolume(); err == nil {
					info.DefaultVolume = v
				}
			}
		}
	}
	info.Docker = s.dockerAvailable()
	info.SSH = sshAvailable()
	if probeCLI {
		info.InstalledApps = s.installedApps()
	}
	if info.InstalledApps == nil {
		info.InstalledApps = []string{}
	}
	return info
}

// handleReconcile 受理一次全量对账。
//
// 以前这里同步返回 ReconcileResult，但全量对账要给每一条链接跑一遍
// appcenter-cli，链接一多就是分钟级；前端因此会先超时报错，
// 而服务端还在老老实实地装。现在改成受理即返回，结果通过 SSE 状态推送。
func (s *Server) handleReconcile(w http.ResponseWriter, r *http.Request) {
	if s.deps.Coordinator == nil {
		writeError(w, fmt.Errorf("当前运行模式下不支持对账"))
		return
	}
	s.deps.Coordinator.QueueReconcile()
	s.broadcast(EventLinksChanged, nil)
	accepted(w, "对账已在后台开始，每条链接的状态会自动更新", s.deps.Coordinator.Pending())
}

// handleCleanupOrphans 清理 qlink2d.* 命名空间下已无对应链接的桌面图标。
//
// 这是破坏性操作（卸载后要恢复只能重新同步链接），因此前端必须先弹
// 确认对话框再调用；后端不再做二次校验，信任自家的带鉴权 API。
func (s *Server) handleCleanupOrphans(w http.ResponseWriter, r *http.Request) {
	if s.deps.Coordinator == nil {
		writeError(w, fmt.Errorf("当前运行模式下不支持清理"))
		return
	}
	s.deps.Coordinator.QueueCleanupOrphans()
	s.broadcast(EventLinksChanged, nil)
	accepted(w, "清理已在后台开始，完成后刷新本页即可看到结果", s.deps.Coordinator.Pending())
}

// handleRestart 受理一次「重启本应用服务」。
//
// 为什么是受理型而不是同步返回结果：停止命令会终止执行它的这个进程，
// 请求路径根本等不到「重启完成」那一刻 —— 等到的只会是自己的连接断开。
// 所以这里如实告诉用户「已受理，页面稍后会自动重连」，然后让连接自然断掉。
//
// 前端据此不该显示"失败"，而应进入"等待服务回来"的轮询状态 ——
// 把一次故意的断开报成错误，是这类功能最常见的体验事故。
func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	if s.deps.Coordinator == nil {
		writeError(w, fmt.Errorf("当前运行模式下不支持重启"))
		return
	}
	s.deps.Coordinator.QueueRestartSelf()
	accepted(w, "重启已受理，服务停止后页面会短暂断开，恢复后自动重连", s.deps.Coordinator.Pending())
}

// ---------------------------------------------------------------------------
// 日志
// ---------------------------------------------------------------------------

// logsResponse 是日志查询的响应。
type logsResponse struct {
	Lines []string `json:"lines"`
	Total int      `json:"total"`
	// RingCapacity 说明这是内存缓冲里的片段而非完整日志文件的全部内容。
	RingCapacity int `json:"ring_capacity"`
}

// defaultLogLines 是默认返回的日志行数。
const defaultLogLines = 200

// maxLogLines 是单次可请求的最大行数，避免把整个缓冲一次性吐给浏览器。
const maxLogLines = 2000

func (s *Server) handleGetLogs(w http.ResponseWriter, r *http.Request) {
	if s.deps.Logs == nil {
		ok(w, logsResponse{Lines: []string{}})
		return
	}

	n := defaultLogLines
	if raw := strings.TrimSpace(r.URL.Query().Get("lines")); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil {
			n = v
		}
	}
	if n <= 0 {
		n = defaultLogLines
	}
	if n > maxLogLines {
		n = maxLogLines
	}

	lines := s.deps.Logs.Tail(n)
	ok(w, logsResponse{
		Lines:        lines,
		Total:        len(lines),
		RingCapacity: s.deps.Logs.Len(),
	})
}

// handleDownloadLog 把最近日志作为文本文件下载。
//
// 直接导出内存缓冲而不是磁盘文件，原因是：日志文件已经轮转过多轮，
// 磁盘上最新的那一份反而不如缓冲里连续。用户想要的通常是"刚才发生了什么"。
func (s *Server) handleDownloadLog(w http.ResponseWriter, r *http.Request) {
	var content string
	if s.deps.Logs != nil {
		content = strings.Join(s.deps.Logs.Tail(maxLogLines), "\n")
	}
	if content == "" {
		content = "（暂无日志）"
	}

	filename := fmt.Sprintf("qlink2desktop-%s.log", time.Now().Format("20060102-150405"))
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(content))
}

// clientLogRequest 是前端回传的日志。
type clientLogRequest struct {
	Level   string         `json:"level,omitempty"`
	Message string         `json:"message"`
	Fields  map[string]any `json:"fields,omitempty"`
}

func (s *Server) handleClientLog(w http.ResponseWriter, r *http.Request) {
	var req clientLogRequest
	if !decodeLenient(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Message) == "" {
		ok(w, map[string]bool{"ok": true})
		return
	}

	// 把前端字段摊平进结构化日志，与后端日志混在同一条时间线上，
	// 排查"用户点了按钮但什么都没发生"这类问题时特别有用。
	args := make([]any, 0, len(req.Fields)*2)
	for k, v := range req.Fields {
		args = append(args, k, v)
	}

	switch strings.ToLower(req.Level) {
	case "error":
		s.deps.Logger.Error("[前端] "+req.Message, args...)
	case "warn", "warning":
		s.deps.Logger.Warn("[前端] "+req.Message, args...)
	case "debug":
		s.deps.Logger.Debug("[前端] "+req.Message, args...)
	default:
		s.deps.Logger.Info("[前端] "+req.Message, args...)
	}
	ok(w, map[string]bool{"ok": true})
}

// ---------------------------------------------------------------------------
// 平台能力探测
// ---------------------------------------------------------------------------

// sshAvailable 报告环境里是否存在 ssh 客户端。
func sshAvailable() bool {
	return discovery.NewSSHTunnel(domain.Host{}).Available()
}
