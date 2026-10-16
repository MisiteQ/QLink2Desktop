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
	sockets  int
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

// Listening 返回本机正在监听的服务列表（同一服务的多个套接字已合并），
// 以及**合并前**的套接字数量。
//
// force 为 true 时忽略缓存。缓存的存在有两层意义：
// 一是 /proc 遍历在高负载 NAS 上并不便宜；二是前端 1~2 秒刷新一次，
// 没有缓存会让监控本身变成可观测的负载。
//
// 为什么要一并给出套接字数：合并是"显示的行数少了一大截"的改动，用户第一反应
// 是「是不是漏了东西」。把「101 个套接字 → 二十来个服务」这个过程显式说出来，
// 比让他自己去猜要好。收敛必须暴露，不能悄悄发生。
//
// 不要把这两个返回值拆成一个只回列表的便捷方法：那样它就没人调用了 ——
// 而一个没人调用的壳看起来和"有这条能力"一模一样（本项目踩过）。
func (s *Scanner) Listening(force bool) ([]PortInfo, int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !force && s.cache != nil && s.now().Sub(s.cachedAt) < s.ttl {
		return clonePorts(s.cache), s.sockets
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

	// 关联容器信息（端口 → 容器）。必须在合并**之前**做：
	// 容器是按「公网端口 + 协议」索引的，合并后就只剩一个主协议了。
	if s.docker != nil {
		s.docker.Enrich(out)
	}

	// 合并放在最后：先按套接字补齐进程与容器信息，再把同一服务收成一行。
	out = GroupPorts(out)

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
	s.sockets = len(raw)
	s.cachedAt = s.now()
	return clonePorts(out), s.sockets
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
