package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/MisiteQ/qlink2desktop/internal/discovery"
	"github.com/MisiteQ/qlink2desktop/internal/domain"
	"github.com/MisiteQ/qlink2desktop/internal/proxy"
)

// ---------------------------------------------------------------------------
// 连通性探测
// ---------------------------------------------------------------------------

// probeTimeout 是单次连通性探测的上限。
const probeTimeout = 8 * time.Second

// probe 探测一个地址是否可达。
func (s *Server) probe(ctx context.Context, target string) proxy.ProbeResult {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	return proxy.Probe(ctx, target, probeTimeout)
}

type probeRequest struct {
	URL string `json:"url"`
}

func (s *Server) handleProbeTarget(w http.ResponseWriter, r *http.Request) {
	var req probeRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	ok(w, s.probe(r.Context(), req.URL))
}

func (s *Server) handlePickPort(w http.ResponseWriter, r *http.Request) {
	ok(w, map[string]int{"port": s.findFreePort()})
}

// ---------------------------------------------------------------------------
// 本机端口发现
// ---------------------------------------------------------------------------

// portListResponse 是端口 / 扫描结果的统一响应。
type portListResponse struct {
	Items []discovery.PortInfo `json:"items"`
	Total int                  `json:"total"`
	// Docker 表示容器运行时是否可用（不可用时前端应隐藏"容器"筛选）。
	Docker bool `json:"docker"`
	// ElapsedMs 是本次采集 / 扫描耗时，便于用户判断是否需要缩小范围。
	ElapsedMs int64 `json:"elapsed_ms,omitempty"`
}

func (s *Server) handleLocalPorts(w http.ResponseWriter, r *http.Request) {
	if s.deps.Scanner == nil {
		ok(w, portListResponse{Items: []discovery.PortInfo{}, Docker: false})
		return
	}

	start := time.Now()
	ports := s.deps.Scanner.Listening(queryBool(r, "force"))
	s.markManaged(ports)

	ok(w, portListResponse{
		Items:     ports,
		Total:     len(ports),
		Docker:    s.dockerAvailable(),
		ElapsedMs: time.Since(start).Milliseconds(),
	})
}

// markManaged 给已被本项目放到桌面的端口打标记。
//
// 前端据此把这类端口显示为"已在桌面"，避免用户重复添加同一条链接。
func (s *Server) markManaged(ports []discovery.PortInfo) {
	if len(ports) == 0 {
		return
	}
	managed := make(map[int]bool)
	for _, l := range s.deps.Store.ListLinks() {
		if l.Enabled && l.Kind == domain.KindLocalPort && l.Port > 0 {
			managed[l.Port] = true
		}
	}
	for i := range ports {
		if managed[ports[i].Port] {
			ports[i].Managed = true
		}
	}
}

func (s *Server) dockerAvailable() bool {
	return s.deps.Docker != nil && s.deps.Docker.Available()
}

// ---------------------------------------------------------------------------
// 容器标签（兼容 Watchcow 生态）
// ---------------------------------------------------------------------------

// dockLabelItem 是「一键启用」列表里的一项。
type dockLabelItem struct {
	Key           string `json:"key"`
	Title         string `json:"title"`
	Icon          string `json:"icon,omitempty"`
	Port          int    `json:"port,omitempty"`
	Scheme        string `json:"scheme,omitempty"`
	Path          string `json:"path,omitempty"`
	AllUsers      bool   `json:"all_users,omitempty"`
	ContainerName string `json:"container_name,omitempty"`

	// Enabled 表示该容器当前已经有一条对应的桌面链接。
	Enabled bool `json:"enabled"`
	// LinkID 是已存在的链接 ID，便于前端做"编辑已有项"。
	LinkID string `json:"link_id,omitempty"`
}

func (s *Server) handleDockLabels(w http.ResponseWriter, r *http.Request) {
	if s.deps.Docker == nil || !s.deps.Docker.Available() {
		ok(w, map[string]any{"items": []dockLabelItem{}, "total": 0, "docker": false})
		return
	}

	hints := s.deps.Docker.LabelHints()
	items := make([]dockLabelItem, 0, len(hints))
	for _, h := range hints {
		item := dockLabelItem{
			Key:           h.Key(),
			Title:         h.Title,
			Icon:          h.Icon,
			Port:          h.Port,
			Scheme:        h.Scheme,
			Path:          h.Path,
			AllUsers:      h.AllUsers,
			ContainerName: h.ContainerName,
		}
		if l, found := s.linkByContainer(h.ContainerName); found {
			item.Enabled = true
			item.LinkID = l.ID
			if !l.Enabled {
				item.Enabled = false
			}
		}
		items = append(items, item)
	}
	ok(w, map[string]any{"items": items, "total": len(items), "docker": true})
}

// dockLabelToggleRequest 是「一键启用 / 停用」请求体。
type dockLabelToggleRequest struct {
	// Key 是 LabelHint.Key()，形如 docklabel-<容器名>。
	Key     string `json:"key"`
	Enabled bool   `json:"enabled"`
}

func (s *Server) handleEnableDockLabel(w http.ResponseWriter, r *http.Request) {
	if s.deps.Docker == nil || !s.deps.Docker.Available() {
		writeError(w, fmt.Errorf("%w: 未检测到可用的容器运行时", domain.ErrValidation))
		return
	}

	var req dockLabelToggleRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Key) == "" {
		writeError(w, fmt.Errorf("%w: 缺少容器标识", domain.ErrValidation))
		return
	}

	hint, found := s.findHint(req.Key)
	if !found {
		writeError(w, fmt.Errorf("%w: 未找到容器 %q 的桌面标签", domain.ErrNotFound, req.Key))
		return
	}

	existing, hasExisting := s.linkByContainer(hint.ContainerName)

	// 停用：删除已有的那条链接（并回收桌面图标）。
	if !req.Enabled {
		if !hasExisting {
			ok(w, map[string]any{"ok": true, "changed": false})
			return
		}
		if s.deps.Coordinator != nil {
			// 同样只受理：注销图标要调 appcenter-cli，不能堵在请求里。
			if _, err := s.deps.Coordinator.QueueRemove(existing.ID); err != nil {
				writeError(w, err)
				return
			}
		} else if _, err := s.deps.Store.DeleteLink(existing.ID); err != nil {
			writeError(w, err)
			return
		}
		// 记录开关状态，避免下次启动又被自动补齐。
		_ = s.deps.Store.SetToggle(hint.Key(), false)
		s.broadcast(EventLinksChanged, nil)
		ok(w, map[string]any{"ok": true, "changed": true})
		return
	}

	// 启用：已有链接则只恢复启用位，否则按标签内容新建一条。
	if hasExisting {
		l, err := s.deps.Store.SetLinkEnabled(existing.ID, true)
		if err != nil {
			writeError(w, err)
			return
		}
		s.queueSync(l.ID)
		_ = s.deps.Store.SetToggle(hint.Key(), true)
		s.broadcast(EventLinksChanged, nil)
		ok(w, map[string]any{"ok": true, "changed": true, "link_id": l.ID})
		return
	}

	link := linkFromHint(hint)
	saved, err := s.deps.Store.UpsertLink(link)
	if err != nil {
		writeError(w, err)
		return
	}
	s.queueSync(saved.ID)
	_ = s.deps.Store.SetToggle(hint.Key(), true)

	s.deps.Logger.Info("已从容器标签创建桌面链接", "container", hint.ContainerName, "name", hint.Title)
	s.broadcast(EventLinksChanged, nil)
	ok(w, map[string]any{"ok": true, "changed": true, "link_id": saved.ID})
}

// linkFromHint 把容器标签配置翻译成一条链接定义。
//
// 一律建模为 KindLocalPort：容器端口已经映射到宿主机上，
// 桌面图标直连 127.0.0.1 即可，多绕一层内置代理没有收益。
func linkFromHint(hint discovery.LabelHint) domain.Link {
	port := hint.Port
	icon := domain.Icon{}
	if strings.TrimSpace(hint.Icon) != "" {
		icon = domain.Icon{Source: domain.IconURL, Ref: hint.Icon}
	}
	return domain.Link{
		Name:     hint.Title,
		Kind:     domain.KindLocalPort,
		Scheme:   hint.Scheme,
		Port:     port,
		Path:     hint.Path,
		UI:       domain.UIWindow,
		AllUsers: hint.AllUsers,
		Icon:     icon,
		Enabled:  true,
		Container: domain.Container{
			Name: hint.ContainerName,
		},
	}
}

// findHint 按 key 在容器标签里查找。
func (s *Server) findHint(key string) (discovery.LabelHint, bool) {
	for _, h := range s.deps.Docker.LabelHints() {
		if h.Key() == key {
			return h, true
		}
	}
	return discovery.LabelHint{}, false
}

// linkByContainer 按容器名反查已有链接。
func (s *Server) linkByContainer(name string) (domain.Link, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return domain.Link{}, false
	}
	for _, l := range s.deps.Store.ListLinks() {
		if strings.EqualFold(l.Container.Name, name) {
			return l, true
		}
	}
	return domain.Link{}, false
}

// ---------------------------------------------------------------------------
// 局域网 / 远端扫描
//
// 这三个端点合起来构成「后台扫描」：POST 只负责开始，GET 拿进度与结果，
// DELETE 取消。之所以不能是一个同步接口 —— 一个 /24 网段要发上万个 TCP 连接，
// 真机上是十几秒起步，而前端请求预算是 15 秒：用户看到的会是一句
// 「请求超时：15 秒内没有响应（api/discovery/scan）」，而扫描其实还在跑。
// ---------------------------------------------------------------------------

// handleScanNetwork 开始（或接管）一轮网络扫描。
func (s *Server) handleScanNetwork(w http.ResponseWriter, r *http.Request) {
	if s.deps.ScanRunner == nil {
		writeError(w, fmt.Errorf("当前运行模式下不支持网络扫描"))
		return
	}

	var req discovery.ScanRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	state, err := s.deps.ScanRunner.Start(req)
	if err != nil {
		// 这里的错误都是"给用户看的"：范围写法不对、主机数超限、
		// 没有检测到网段……一律以 400 返回，让用户直接改输入。
		writeError(w, fmt.Errorf("%w: %v", domain.ErrValidation, err))
		return
	}

	s.deps.Logger.Info("已受理网络扫描", "range", state.Range, "hosts", state.HostsTotal, "ports", len(state.Ports))
	// 202：只保证"开始扫了"。结果要么靠 GET 轮询，要么等扫描结束时的那条广播。
	acceptedWith(w, s.withManaged(state))
}

// handleScanStatus 返回当前（或最近一次）扫描的进度与结果。
func (s *Server) handleScanStatus(w http.ResponseWriter, r *http.Request) {
	if s.deps.ScanRunner == nil {
		writeError(w, fmt.Errorf("当前运行模式下不支持网络扫描"))
		return
	}
	ok(w, s.withManaged(s.deps.ScanRunner.Snapshot()))
}

// handleCancelScan 请求停止当前扫描。
//
// 返回的是**请求取消之后立刻**的状态（此时通常还是 running），
// 前端会继续轮询直到 running 变 false。
func (s *Server) handleCancelScan(w http.ResponseWriter, r *http.Request) {
	if s.deps.ScanRunner == nil {
		writeError(w, fmt.Errorf("当前运行模式下不支持网络扫描"))
		return
	}
	canceled := s.deps.ScanRunner.Cancel()
	state := s.withManaged(s.deps.ScanRunner.Snapshot())
	if canceled {
		s.deps.Logger.Info("已请求取消网络扫描", "id", state.ID)
	}
	ok(w, state)
}

// withManaged 给扫描结果里的端口补上「已在桌面」标记。
//
// 标记必须**每次读取时重算**：扫描结果是快照，而用户完全可能在结果出来之后
// 才把某个端口放上桌面（甚至一边扫一边放）。把标记固化进快照，
// 列表就会一直显示成未管理。
func (s *Server) withManaged(state discovery.ScanState) discovery.ScanState {
	s.markManaged(state.Items)
	return state
}

// handleSubnets 返回本机网络环境，供前端的「本机网段 / 上级网段」选项使用。
func (s *Server) handleSubnets(w http.ResponseWriter, r *http.Request) {
	info := discovery.DetectNetwork()
	scanning := s.deps.ScanRunner != nil && s.deps.ScanRunner.Busy()

	ok(w, map[string]any{
		"local_ips":   info.LocalIPs,
		"subnets":     info.Subnets,
		"primary":     info.Primary,
		"parent":      info.Parent,
		"gateway":     info.Gateway,
		"cidrs":       info.Cidrs,
		"ifaces":      info.Ifaces,
		"warning":     info.Warning,
		"ports":       discovery.CommonPorts(),
		"probe_ports": discovery.HostProbePorts(),
		"limit":       discovery.MaxScanHosts,
		"ping_above":  32,
		"scanning":    scanning,
	})
}
