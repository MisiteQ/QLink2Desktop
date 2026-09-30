package discovery

import (
	"sort"
	"sync"
	"time"
)

// Scanner 采集本机正在监听的端口，并尽力关联 Docker 容器信息。
//
// 只读、无副作用：它不会修改任何东西，结果的用途由上层（HTTP 层）决定。
type Scanner struct {
	procPath string
	docker   *DockerClient

	ttl time.Duration
	now func() time.Time

	mu       sync.Mutex
	cache    []PortInfo
	cachedAt time.Time
}

// ScannerOptions 是构造 Scanner 的参数。
type ScannerOptions struct {
	// ProcPath 是 procfs 挂载点。容器里通常需要指向 /host/root/proc。
	ProcPath string
	// Docker 可为空，为空时跳过容器关联。
	Docker *DockerClient
	// TTL 是结果缓存时长，避免前端频繁刷新时反复遍历 /proc。
	TTL time.Duration
}

// NewScanner 创建扫描器。
func NewScanner(opts ScannerOptions) *Scanner {
	if opts.TTL <= 0 {
		opts.TTL = 3 * time.Second
	}
	return &Scanner{
		procPath: opts.ProcPath,
		docker:   opts.Docker,
		ttl:      opts.TTL,
		now:      time.Now,
	}
}

// Listening 返回本机监听中的端口列表。
//
// force 为 true 时忽略缓存。缓存的存在有两层意义：
// 一是 /proc 遍历在高负载 NAS 上并不便宜；二是前端 1~2 秒刷新一次，
// 没有缓存会让监控本身变成可观测的负载。
func (s *Scanner) Listening(force bool) []PortInfo {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !force && s.cache != nil && s.now().Sub(s.cachedAt) < s.ttl {
		return clonePorts(s.cache)
	}

	raw := collectListening(s.procPath)
	owners := collectSocketOwners(s.procPath)

	out := make([]PortInfo, 0, len(raw))
	for _, rs := range raw {
		info := PortInfo{
			Port:    rs.Port,
			Proto:   rs.Proto,
			Address: rs.Address,
		}
		if own, ok := owners[rs.Inode]; ok {
			info.Process = own.Name
			info.PID = own.PID
			info.User = own.User
		}
		out = append(out, info)
	}

	// 关联容器信息（端口 → 容器）。
	if s.docker != nil {
		s.docker.Enrich(out)
	}

	sort.Slice(out, func(i, j int) bool {
		pi, ti := out[i].SortKey()
		pj, tj := out[j].SortKey()
		if pi != pj {
			return pi < pj
		}
		if ti != tj {
			return ti < tj
		}
		return out[i].Address < out[j].Address
	})

	s.cache = out
	s.cachedAt = s.now()
	return clonePorts(out)
}

// Invalidate 让下一次查询强制重新采集（例如刚安装完一个桌面图标后）。
func (s *Scanner) Invalidate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cache = nil
}

func clonePorts(in []PortInfo) []PortInfo {
	if in == nil {
		return []PortInfo{}
	}
	out := make([]PortInfo, len(in))
	copy(out, in)
	return out
}
