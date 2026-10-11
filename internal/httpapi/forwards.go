package httpapi

import (
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/MisiteQ/qlink2desktop/internal/domain"
	"github.com/MisiteQ/qlink2desktop/internal/forward"
)

// ---------------------------------------------------------------------------
// 视图与请求体
// ---------------------------------------------------------------------------

// ForwardView 是一条转发规则的「定义 + 运行态」。
//
// 与 domain.View 同构：定义来自 store，运行态由 forward.Manager 实时合成。
// 两者分开返回，前端不必去猜"哪个字段是实时的"。
type ForwardView struct {
	Forward domain.Forward `json:"forward"`
	Status  forward.Status `json:"status"`
}

// forwardRequest 是转发规则的新增 / 修改请求体。
//
// 与 linkRequest 一样刻意声明 id / created_at / updated_at 三个只读字段：
// 前端的编辑表单会直接把整行数据回传，这里不声明它们，严格解码就会
// 报"未知字段"，用户看到的是一句与他填的内容毫无关系的错误。
type forwardRequest struct {
	ID        string    `json:"id,omitempty"`
	CreatedAt time.Time `json:"created_at,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`

	Name       string   `json:"name"`
	Desc       string   `json:"desc,omitempty"`
	Protocol   string   `json:"protocol,omitempty"`
	ListenHost string   `json:"listen_host,omitempty"`
	ListenPort int      `json:"listen_port"`
	TargetHost string   `json:"target_host"`
	TargetPort int      `json:"target_port"`
	AccessMode string   `json:"access_mode,omitempty"`
	AccessList []string `json:"access_list,omitempty"`
	MaxConns   int      `json:"max_conns,omitempty"`

	// Enabled 用指针以区分「未提供」（新建时默认启用）与「显式关闭」。
	Enabled *bool `json:"enabled,omitempty"`
}

// toDomain 把请求体合并进一个基准规则。
//
// base 为空表示新建；传现有规则表示修改（保留 CreatedAt 等只读字段）。
func (req forwardRequest) toDomain(base domain.Forward) (domain.Forward, error) {
	proto, err := domain.ParseProtocol(req.Protocol)
	if err != nil {
		return domain.Forward{}, err
	}
	mode, err := domain.ParseAccessMode(req.AccessMode)
	if err != nil {
		return domain.Forward{}, err
	}

	f := base
	f.Name = req.Name
	f.Desc = req.Desc
	f.Protocol = proto
	f.ListenHost = req.ListenHost
	f.ListenPort = req.ListenPort
	f.TargetHost = req.TargetHost
	f.TargetPort = req.TargetPort
	f.AccessMode = mode
	f.AccessList = req.AccessList
	f.MaxConns = req.MaxConns

	if req.Enabled != nil {
		f.Enabled = *req.Enabled
	}
	f.Normalize()
	if err := f.Validate(); err != nil {
		return domain.Forward{}, err
	}
	return f, nil
}

// forwardMutationResponse 是保存类接口的响应。
//
// 多出一个 Warning 字段是刻意的：**配置保存成功不代表监听已经起来**。
// 目标服务还没启动、端口被别的程序占着，都是很常见的状态，
// 这时候如果直接返回 4xx，用户连配置都存不下来，只能先去别处解决问题
// 再回来重新填一遍表单——毫无必要。
// 正确做法是照常落盘，把"没起来"作为一个可观测的事实告诉用户。
type forwardMutationResponse struct {
	Forward domain.Forward `json:"forward"`
	Status  forward.Status `json:"status"`
	Warning string         `json:"warning,omitempty"`
}

// ---------------------------------------------------------------------------
// 处理器
// ---------------------------------------------------------------------------

func (s *Server) handleListForwards(w http.ResponseWriter, r *http.Request) {
	items := s.forwardViews()
	ok(w, map[string]any{
		"items": items,
		"total": len(items),
		// 被占用的端口一并返回，供前端在编辑时提前提示冲突，
		// 不必等保存失败才知道。
		"used_ports": s.usedForwardPorts(),
	})
}

func (s *Server) handleGetForward(w http.ResponseWriter, r *http.Request) {
	id := pathValue(r, "id")
	f, found := s.deps.Store.GetForward(id)
	if !found {
		writeError(w, domain.NotFound("转发规则"))
		return
	}
	ok(w, s.forwardView(f))
}

func (s *Server) handleCreateForward(w http.ResponseWriter, r *http.Request) {
	var req forwardRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	f, err := req.toDomain(domain.Forward{})
	if err != nil {
		writeError(w, err)
		return
	}
	// 新建默认启用：用户填完表单的意图就是"让它开始工作"。
	f.Enabled = true
	if req.Enabled != nil {
		f.Enabled = *req.Enabled
	}

	// 落盘之前先挡掉确定性冲突。见 forward.Manager.Check 的注释：
	// 端口被自家另一条规则占用时如果照常保存，规则会长期停在
	// "存下来了但永远起不来"的状态。
	if err := s.checkForward(f); err != nil {
		writeError(w, err)
		return
	}

	saved, err := s.deps.Store.UpsertForward(f)
	if err != nil {
		writeError(w, err)
		return
	}
	s.deps.Logger.Info("新建端口转发规则", "id", saved.ID, "name", saved.Name,
		"listen", saved.ListenAddr(), "target", saved.TargetAddr(), "protocol", saved.Protocol)

	s.broadcast(EventForwardChanged, nil)
	ok(w, s.applyForward(saved, ""))
}

func (s *Server) handleUpdateForward(w http.ResponseWriter, r *http.Request) {
	id := pathValue(r, "id")
	existing, found := s.deps.Store.GetForward(id)
	if !found {
		writeError(w, domain.NotFound("转发规则"))
		return
	}

	var req forwardRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	f, err := req.toDomain(existing)
	if err != nil {
		writeError(w, err)
		return
	}

	if err := s.checkForward(f); err != nil {
		writeError(w, err)
		return
	}

	saved, err := s.deps.Store.UpsertForward(f)
	if err != nil {
		writeError(w, err)
		return
	}
	s.deps.Logger.Info("更新端口转发规则", "id", saved.ID, "name", saved.Name,
		"listen", saved.ListenAddr(), "target", saved.TargetAddr())

	s.broadcast(EventForwardChanged, nil)
	ok(w, s.applyForward(saved, ""))
}

// handleDeleteForward 删除规则：先停监听，再删定义。
//
// 顺序不能反：先删定义的话，一旦停止监听失败，那条"看不见但还在占端口"
// 的规则就成了幽灵——用户无法在界面上操作它，却也用不了那个端口。
func (s *Server) handleDeleteForward(w http.ResponseWriter, r *http.Request) {
	id := pathValue(r, "id")
	f, found := s.deps.Store.GetForward(id)
	if !found {
		writeError(w, domain.NotFound("转发规则"))
		return
	}
	if s.deps.Forwards != nil {
		s.deps.Forwards.Stop(id)
	}
	if _, err := s.deps.Store.DeleteForward(id); err != nil {
		writeError(w, err)
		return
	}
	s.deps.Logger.Info("删除端口转发规则", "id", id, "name", f.Name, "listen", f.ListenAddr())

	s.broadcast(EventForwardChanged, nil)
	ok(w, map[string]bool{"ok": true})
}

func (s *Server) handleToggleForward(w http.ResponseWriter, r *http.Request) {
	id := pathValue(r, "id")
	var req toggleRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	// 与新建 / 修改同款的前置检查，且必须在改存储**之前**：
	// 否则冲突时会留下一条"启用着但起不来"的规则。
	if existing, found := s.deps.Store.GetForward(id); found {
		next := existing
		next.Enabled = req.Enabled
		if err := s.checkForward(next); err != nil {
			writeError(w, err)
			return
		}
	}

	f, err := s.deps.Store.SetForwardEnabled(id, req.Enabled)
	if err != nil {
		writeError(w, err)
		return
	}
	s.deps.Logger.Info("切换端口转发规则状态", "id", id, "name", f.Name, "enabled", req.Enabled)

	s.broadcast(EventForwardChanged, nil)
	ok(w, s.applyForward(f, ""))
}

// probeForwardResponse 是目标连通性探测的结果。
type probeForwardResponse struct {
	OK        bool   `json:"ok"`
	LatencyMs int64  `json:"latency_ms"`
	Error     string `json:"error,omitempty"`
	Note      string `json:"note,omitempty"`
}

// forwardPortCheck 是"这个端口能不能用"的回答。
type forwardPortCheck struct {
	Protocol string `json:"protocol"`
	Port     int    `json:"port"`
	// Available 为 true 表示本应用可以监听这个端口。
	Available bool `json:"available"`
	// Mode / Degraded 描述"能监听"是哪种形态：双栈，还是只能单栈。
	Mode     string `json:"mode,omitempty"`
	Degraded bool   `json:"degraded,omitempty"`
	// InUseByRule 表示端口被本应用的另一条规则占着（配置冲突，不是环境问题）。
	InUseByRule bool `json:"in_use_by_rule,omitempty"`
	// Self 表示占用者是本应用自己（例如内置反向代理占的 18000 段）。
	Self bool `json:"self,omitempty"`
	// Holders 是查到的占用者进程。
	Holders []forward.Holder `json:"holders,omitempty"`
	// Error 是绑定失败的原始说明（已翻译成用户可读文案）。
	Error string `json:"error,omitempty"`
	// Message 是可以直接展示给用户的一句话结论。
	Message string `json:"message"`
}

// handleCheckForwardPort 报告某个监听配置在本机能不能用，并尽量指出占用者。
//
// 与 /api/tools/port 的区别：那个是"帮我随便挑一个空闲端口"，
// 这个是"我就要用这个端口，先告诉我行不行、是谁占着"。
// 后者才是用户真正会问的问题 —— 他手里已经有一个想用的端口了。
func (s *Server) handleCheckForwardPort(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	proto := strings.ToLower(strings.TrimSpace(q.Get("protocol")))
	if proto == "" {
		proto = "tcp"
	}
	if proto != "tcp" && proto != "udp" {
		writeError(w, fmt.Errorf("%w: 协议只能是 tcp 或 udp", domain.ErrValidation))
		return
	}

	port, err := strconv.Atoi(strings.TrimSpace(q.Get("port")))
	if err != nil || port < 1 || port > 65535 {
		writeError(w, fmt.Errorf("%w: 端口号必须在 1-65535 之间", domain.ErrValidation))
		return
	}
	host := strings.TrimSpace(q.Get("host"))

	// 先问自家规则。这类冲突是**配置**问题，提示必须指向"哪条规则"——
	// 对一个本进程内的端口报"PID 12345"，用户拿着这个号也不知道该关什么。
	//
	// 这里刻意用 CheckListen 而不是 checkForward：后者会跑完整规则校验
	// （名称必填等），探测一个端口号时拿到"规则名称不能为空"是荒谬的。
	if s.deps.Forwards != nil {
		if err := s.deps.Forwards.CheckListen(domain.Protocol(proto), host, port); err != nil {
			ok(w, forwardPortCheck{
				Protocol: proto, Port: port, Available: false,
				InUseByRule: true, Message: err.Error(),
			})
			return
		}
	}

	res := forward.ProbePort(host, port, proto)
	out := forwardPortCheck{
		Protocol: proto, Port: port, Available: res.Available,
		Mode: res.Mode, Degraded: res.Degraded,
		Self: res.Self, Holders: res.Holders, Error: res.Error,
	}
	switch {
	case res.Available && res.Degraded:
		side := "IPv4"
		if res.Mode == "ipv6" {
			side = "IPv6"
		}
		out.Message = fmt.Sprintf(
			"端口 %d（%s）可以用，但只能退化为仅 %s 监听：另一侧被别的程序占着。"+
				"如果只用到 %s，这样就能跑；想两侧都能连，就得先腾出那个端口。",
			port, strings.ToUpper(proto), side, side)
	case res.Available:
		out.Message = fmt.Sprintf("端口 %d（%s）在本机可以监听（IPv4 + IPv6）", port, strings.ToUpper(proto))
	default:
		out.Message = res.Error
	}
	ok(w, out)
}

// handleProbeForward 探测转发目标是否可达。
//
// 用途：保存之前先确认目标填对了。这是转发功能里最高频的一类错误——
// 目标主机 IP 写错一位，规则会"正常启动、连接全部失败"，
// 用户看到的现象是"转发没生效"，却没有任何线索指向目标地址。
func (s *Server) handleProbeForward(w http.ResponseWriter, r *http.Request) {
	id := pathValue(r, "id")
	f, found := s.deps.Store.GetForward(id)
	if !found {
		writeError(w, domain.NotFound("转发规则"))
		return
	}
	ok(w, s.probeForwardTarget(f))
}

func (s *Server) probeForwardTarget(f domain.Forward) probeForwardResponse {
	addr := f.TargetAddr()
	network := "tcp"
	if !f.Protocol.HasTCP() {
		network = "udp"
	}

	start := time.Now()
	conn, err := net.DialTimeout(network, addr, forwardProbeTimeout)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return probeForwardResponse{OK: false, LatencyMs: latency, Error: shortDialErr(err)}
	}
	defer conn.Close()

	res := probeForwardResponse{OK: true, LatencyMs: latency}
	// UDP 的"连上了"几乎不提供信息：UDP 是无连接的，向任意地址 dial
	// 都会立刻成功。这里如实说明，免得用户把一个绿灯当成"服务在跑"。
	if network == "udp" {
		res.Note = "UDP 无连接，此处只能确认地址可解析、路由可达；无法确认目标服务是否在运行"
	}
	return res
}

// handleForwardConns 返回某条规则最近的连接记录。
func (s *Server) handleForwardConns(w http.ResponseWriter, r *http.Request) {
	id := pathValue(r, "id")
	if _, found := s.deps.Store.GetForward(id); !found {
		writeError(w, domain.NotFound("转发规则"))
		return
	}
	if s.deps.Forwards == nil {
		ok(w, map[string]any{"items": []forward.ConnLog{}})
		return
	}
	ok(w, map[string]any{"items": s.deps.Forwards.Recent(id, recentConnLimit)})
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

// forwardProbeTimeout 是目标探测的超时。
const forwardProbeTimeout = 5 * time.Second

// recentConnLimit 是单次返回的最近连接记录条数。
const recentConnLimit = 100

// applyForward 让运行态跟上定义，并把失败整理成 Warning。
//
// 它是「保存」与「启用」两条路径的公共收尾，因此"保存成功但没起来"
// 这个语义只有一份实现，不会在两条路径上长得不一样。
func (s *Server) applyForward(f domain.Forward, _ string) forwardMutationResponse {
	res := forwardMutationResponse{Forward: f}
	if s.deps.Forwards == nil {
		return res
	}
	if err := s.deps.Forwards.Apply(f); err != nil {
		res.Warning = err.Error()
		s.deps.Logger.Warn("转发规则未能启动", "id", f.ID, "name", f.Name, "error", err)
	}
	res.Status = s.deps.Forwards.Status(f.ID)
	return res
}

// checkForward 在落盘前检查规则是否存在确定性冲突。
func (s *Server) checkForward(f domain.Forward) error {
	if s.deps.Forwards == nil {
		return nil
	}
	return s.deps.Forwards.Check(f)
}

// forwardView 组装单条规则的视图。
func (s *Server) forwardView(f domain.Forward) ForwardView {
	return ForwardView{Forward: f, Status: s.forwardStatus(f.ID)}
}

func (s *Server) forwardStatus(id string) forward.Status {
	if s.deps.Forwards == nil {
		return forward.Status{}
	}
	return s.deps.Forwards.Status(id)
}

func (s *Server) forwardViews() []ForwardView {
	list := s.deps.Store.ListForwards()
	out := make([]ForwardView, 0, len(list))
	for _, f := range list {
		out = append(out, s.forwardView(f))
	}
	return out
}

func (s *Server) usedForwardPorts() map[string]bool {
	if s.deps.Forwards == nil {
		return map[string]bool{}
	}
	return s.deps.Forwards.UsedPorts()
}

// shortDialErr 把拨号错误压成一句能显示给用户的话。
//
// 原始的 "dial tcp 192.168.1.50:3389: connect: connection refused" 太长，
// 而且地址在界面上已经单独显示，重复一遍只会让提示更嘈杂。
func shortDialErr(err error) string {
	if err == nil {
		return ""
	}
	s := strings.TrimSpace(err.Error())
	if i := strings.LastIndex(s, ": "); i > 0 && i < len(s)-2 {
		s = s[i+2:]
	}
	switch {
	case strings.Contains(strings.ToLower(s), "connection refused"):
		return "目标端口没有服务在监听（连接被拒绝）"
	case strings.Contains(strings.ToLower(s), "no such host"):
		return "目标主机名无法解析"
	case strings.Contains(strings.ToLower(s), "i/o timeout"),
		strings.Contains(strings.ToLower(s), "deadline exceeded"):
		return "连接超时（目标不可达或被防火墙拦截）"
	case strings.Contains(strings.ToLower(s), "network is unreachable"):
		return "目标网络不可达"
	default:
		return s
	}
}
