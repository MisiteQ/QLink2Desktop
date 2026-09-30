package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/MisiteQ/qlink2desktop/internal/discovery"
	"github.com/MisiteQ/qlink2desktop/internal/domain"
)

// ---------------------------------------------------------------------------
// 请求 / 响应
// ---------------------------------------------------------------------------

// hostRequest 是远端主机的新增 / 修改请求体。
type hostRequest struct {
	// 只读回显字段：允许前端整体回传对象。
	ID        string    `json:"id,omitempty"`
	CreatedAt time.Time `json:"created_at,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`

	Name    string `json:"name,omitempty"`
	Address string `json:"address"`
	Port    int    `json:"port,omitempty"`
	User    string `json:"user,omitempty"`

	Auth *hostAuthRequest `json:"auth,omitempty"`
}

// hostAuthRequest 是认证信息。
//
// Password 留空表示「保持原值不变」——这样前端回显掩码后不修改也能正常保存。
type hostAuthRequest struct {
	Password            string `json:"password,omitempty"`
	ClearPassword       bool   `json:"clear_password,omitempty"`
	KeyPath             string `json:"key_path,omitempty"`
	InsecureSkipHostKey bool   `json:"insecure_skip_host_key,omitempty"`
}

// hostView 是返回给前端的主机信息（口令已掩码）。
type hostView struct {
	domain.Host
	// HasPassword 表示已保存口令（真实的密码永不出网）。
	HasPassword bool `json:"has_password"`
	// SSHAvailable 表示当前环境有可用的 ssh 客户端。
	SSHAvailable bool `json:"ssh_available"`
}

func (s *Server) hostViewOf(h domain.Host) hostView {
	v := hostView{Host: h}
	v.Auth.Password = ""
	v.HasPassword = h.Auth.Password != ""
	v.SSHAvailable = discovery.NewSSHTunnel(h).Available()
	return v
}

// ---------------------------------------------------------------------------
// 处理器
// ---------------------------------------------------------------------------

func (s *Server) handleListHosts(w http.ResponseWriter, r *http.Request) {
	hosts := s.deps.Store.ListHosts()
	items := make([]hostView, 0, len(hosts))
	for _, h := range hosts {
		items = append(items, s.hostViewOf(h))
	}
	ok(w, map[string]any{"items": items, "total": len(items)})
}

func (s *Server) handleSaveHost(w http.ResponseWriter, r *http.Request) {
	var req hostRequest
	if !decodeLenient(w, r, &req) {
		return
	}

	var base domain.Host
	if id := strings.TrimSpace(req.ID); id != "" {
		existing, found := s.deps.Store.GetHost(id)
		if !found {
			writeError(w, domain.ErrNotFound)
			return
		}
		base = existing
	}

	h := base
	h.Name = req.Name
	h.Address = req.Address
	h.Port = req.Port
	h.User = req.User

	if req.Auth != nil {
		h.Auth.KeyPath = req.Auth.KeyPath
		h.Auth.InsecureSkipHostKey = req.Auth.InsecureSkipHostKey
		switch {
		case req.Auth.ClearPassword:
			h.Auth.Password = ""
		case strings.TrimSpace(req.Auth.Password) != "":
			h.Auth.Password = req.Auth.Password
		}
		// Password 留空且未要求清除：保留 base 里的原值，即"不改动"。
	}

	saved, err := s.deps.Store.SaveHost(h)
	if err != nil {
		writeError(w, err)
		return
	}

	s.deps.Logger.Info("已保存远端主机", "id", saved.ID, "name", saved.Name, "address", saved.Address)
	ok(w, s.hostViewOf(saved))
}

func (s *Server) handleDeleteHost(w http.ResponseWriter, r *http.Request) {
	id := pathValue(r, "id")
	if err := s.deps.Store.DeleteHost(id); err != nil {
		writeError(w, err)
		return
	}
	s.deps.Logger.Info("已删除远端主机", "id", id)
	ok(w, map[string]bool{"ok": true})
}

// hostProbeResult 是一次主机探测的完整结果。
type hostProbeResult struct {
	OK bool `json:"ok"`
	// Mode 说明本次是通过哪种方式连通的：direct / ssh。
	Mode      string `json:"mode,omitempty"`
	LatencyMs int64  `json:"latency_ms"`
	Message   string `json:"message"`
	// SSHAvailable 表示环境里是否有 ssh 客户端（没有则密钥 / 密码方式都不可用）。
	SSHAvailable bool `json:"ssh_available"`
}

// hostProbeTimeout 是主机探测的整体超时。
const hostProbeTimeout = 12 * time.Second

func (s *Server) handleProbeHost(w http.ResponseWriter, r *http.Request) {
	id := pathValue(r, "id")
	h, found := s.deps.Store.GetHost(id)
	if !found {
		writeError(w, domain.NotFound("主机"))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), hostProbeTimeout)
	defer cancel()

	result := s.probeHost(ctx, h)

	// 把探测结果记回主机定义，用户下次打开面板就能看到上次的连通情况。
	s.deps.Store.RecordHostProbe(h.ID, result.OK, pickErrMsg(result))

	s.deps.Logger.Info("远端主机探测完成",
		"id", h.ID, "address", h.Address, "ok", result.OK, "mode", result.Mode, "latency", result.LatencyMs)

	ok(w, result)
}

// probeHost 先试直连，失败再试 SSH 隧道。
//
// 顺序很重要：直连更快也更常见，SSH 只作为"端口被防火墙挡住但 ssh 通"时的兜底。
func (s *Server) probeHost(ctx context.Context, h domain.Host) hostProbeResult {
	tunnel := discovery.NewSSHTunnel(h)
	res := hostProbeResult{SSHAvailable: tunnel.Available()}

	// 1) 直连 SSH 端口（这既是"主机活着"也是"ssh 可用"的证据）。
	start := time.Now()
	directErr := discovery.TCPProbe(ctx, h.AddressWithPort(), 5*time.Second)
	res.LatencyMs = time.Since(start).Milliseconds()
	if directErr == nil {
		res.OK = true
		res.Mode = "direct"
		res.Message = fmt.Sprintf("SSH 端口可达（%s，%dms）", h.AddressWithPort(), res.LatencyMs)
		return res
	}

	// 2) 直连失败，尝试通过 ssh 客户端做一次真正的隧道握手。
	if !tunnel.Available() {
		res.Message = fmt.Sprintf("直连 %s 失败：%v；且环境中没有 ssh 客户端可供兜底", h.AddressWithPort(), directErr)
		return res
	}

	start = time.Now()
	sshErr := tunnel.Probe(ctx, h.AddressWithPort())
	res.LatencyMs = time.Since(start).Milliseconds()
	if sshErr == nil {
		res.OK = true
		res.Mode = "ssh"
		res.Message = fmt.Sprintf("已通过 SSH 隧道连通（%dms）", res.LatencyMs)
		return res
	}

	res.Message = fmt.Sprintf("直连失败：%v；SSH 隧道也失败：%v", directErr, sshErr)
	return res
}

func pickErrMsg(res hostProbeResult) string {
	if res.OK {
		return ""
	}
	return res.Message
}

// handleHostPorts 探测远端主机上开放的端口。
//
// 支持 ?ports=80,443,8080 指定端口；缺省用内置的常见端口集合。
func (s *Server) handleHostPorts(w http.ResponseWriter, r *http.Request) {
	id := pathValue(r, "id")
	h, found := s.deps.Store.GetHost(id)
	if !found {
		writeError(w, domain.NotFound("主机"))
		return
	}

	ports := discovery.CommonPorts()
	if raw := strings.TrimSpace(r.URL.Query().Get("ports")); raw != "" {
		parsed, err := parsePortList(raw)
		if err != nil {
			writeError(w, err)
			return
		}
		ports = parsed
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	start := time.Now()
	open := discovery.ProbeHostPorts(ctx, h.Address, ports, 800*time.Millisecond)

	items := make([]discovery.PortInfo, 0, len(open))
	for _, p := range open {
		items = append(items, discovery.PortInfo{
			Port:    p,
			Proto:   discovery.ProtoTCP,
			Address: h.Address,
			Process: "remote",
		})
	}

	s.deps.Logger.Info("远端端口探测完成",
		"host", h.Address, "scanned", len(ports), "open", len(items),
		"elapsed", time.Since(start).Round(time.Millisecond).String())

	ok(w, portListResponse{
		Items:     items,
		Total:     len(items),
		ElapsedMs: time.Since(start).Milliseconds(),
	})
}

// parsePortList 解析 "80,443,8080" 或 "8000-8100" 形式的端口列表。
func parsePortList(raw string) ([]int, error) {
	var out []int
	seen := map[int]bool{}

	add := func(p int) error {
		if p <= 0 || p > 65535 {
			return fmt.Errorf("%w: 端口 %d 越界", domain.ErrValidation, p)
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
		return nil
	}

	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if lo, hi, ok := strings.Cut(part, "-"); ok {
			start, err1 := strconv.Atoi(strings.TrimSpace(lo))
			end, err2 := strconv.Atoi(strings.TrimSpace(hi))
			if err1 != nil || err2 != nil {
				return nil, fmt.Errorf("%w: 无法解析端口区间 %q", domain.ErrValidation, part)
			}
			if end < start {
				start, end = end, start
			}
			// 区间上限 1000 个端口：再大就不是"探测"而是"扫描"了，
			// 应该走 /api/discovery/scan。
			if end-start > 1000 {
				return nil, fmt.Errorf("%w: 端口区间过大（最多 1000 个）", domain.ErrValidation)
			}
			for p := start; p <= end; p++ {
				if err := add(p); err != nil {
					return nil, err
				}
			}
			continue
		}
		p, err := strconv.Atoi(part)
		if err != nil {
			return nil, fmt.Errorf("%w: 无法解析端口 %q", domain.ErrValidation, part)
		}
		if err := add(p); err != nil {
			return nil, err
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: 端口列表为空", domain.ErrValidation)
	}
	return out, nil
}

// decodeLenient 解析请求体但忽略未知字段（用于允许整体回传对象的接口）。
func decodeLenient(w http.ResponseWriter, r *http.Request, dst any) bool {
	if r.Body == nil {
		writeError(w, fmt.Errorf("%w: 请求体为空", domain.ErrValidation))
		return false
	}
	body := http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(body).Decode(dst); err != nil {
		writeError(w, fmt.Errorf("%w: 请求体解析失败: %v", domain.ErrValidation, err))
		return false
	}
	return true
}
