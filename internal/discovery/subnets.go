package discovery

import (
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// 本机网段识别
//
// 这一段存在的理由：早期实现直接遍历 net.Interfaces()，把每个非回环网卡的
// 前三段都当成「本机网段」。在有 Docker 的机器上，列表里排在最前面的往往是
// docker0（172.17.0.0/16）或某个 br-xxxx 自定义网络（172.18.0.0/16），
// 于是「局域网扫描」扫的是容器内部网络 —— 一台设备都扫不到，
// 而用户完全看不出哪里不对（真机上就是这个现象）。
//
// 现在的判定顺序：
//  1. 默认路由所在的网卡及其网段最可信（那才是真正通向外部的口）；
//  2. 物理 / 桥接网卡次之；
//  3. 虚拟网卡（docker*、br-*、veth*、tailscale*……）只在没有任何别的
//     候选时才拿出来用，并且永远排在最后。
// ---------------------------------------------------------------------------

// virtualIfacePrefixes 是公认的虚拟 / 容器网卡名前缀。
//
// 用前缀匹配而不是白名单：不同发行版、不同容器运行时的网卡命名差异很大，
// 而"哪些名字是虚拟的"这件事是稳定的。
var virtualIfacePrefixes = []string{
	"docker", "br-", "veth", "virbr", "vmnet", "vboxnet",
	"tap", "tun", "wg", "tailscale", "zt", "cni", "flannel",
	"cali", "kube", "dummy", "lo",
}

// IsVirtualInterface 报告网卡名是否像一个虚拟 / 容器网卡。
func IsVirtualInterface(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "" {
		return false
	}
	for _, p := range virtualIfacePrefixes {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	return false
}

// SubnetPrefix 返回 IP 所在的 /24 前缀，形如 192.168.31。
// 非 IPv4 返回空串。
func SubnetPrefix(ip net.IP) string {
	ip4 := ip.To4()
	if ip4 == nil {
		return ""
	}
	return fmt.Sprintf("%d.%d.%d", ip4[0], ip4[1], ip4[2])
}

// IfaceBrief 是一个网卡的展示用摘要。
type IfaceBrief struct {
	Name    string   `json:"name"`
	IPv4    []string `json:"ipv4,omitempty"`
	CIDRs   []string `json:"cidrs,omitempty"`
	Virtual bool     `json:"virtual,omitempty"`
	Default bool     `json:"default,omitempty"`
}

// NetworkInfo 是本机网络环境的快照。
type NetworkInfo struct {
	// LocalIPs 是本机全部可用的 IPv4 地址。
	LocalIPs []string `json:"local_ips"`
	// Subnets 是候选 /24 网段前缀（已按可信度排序，虚拟网卡垫底）。
	Subnets []string `json:"subnets"`
	// Primary 是默认推荐扫描的网段 —— 默认路由所在网卡的 /24。
	Primary string `json:"primary,omitempty"`
	// Parent 是「上级网段」：默认网关不在本机 /24 里时，网关所在的那个 /24。
	// 拓扑简单（网关和 NAS 同段）时为空，这是事实，不是失败。
	Parent string `json:"parent,omitempty"`
	// Gateway 是默认网关地址。
	Gateway string `json:"gateway,omitempty"`
	// Cidrs 是各网卡真实的网段（含掩码），用于提示"本机网段其实更大"。
	Cidrs []string `json:"cidrs,omitempty"`
	// Ifaces 是网卡摘要，出问题时能直接看出探到了什么。
	Ifaces []IfaceBrief `json:"ifaces,omitempty"`
	// Warning 是给用户的一句话提醒（例如只探到虚拟网卡）。
	Warning string `json:"warning,omitempty"`
}

// ifaceAddrs 是检测过程的中间形态（纯数据，便于单测）。
type ifaceAddrs struct {
	Name    string
	Virtual bool
	IPv4    []net.IP
	Mask    []int // 与 IPv4 对齐的掩码位宽，0 表示未知
}

// DetectNetwork 采集本机网络环境。
func DetectNetwork() NetworkInfo {
	defIface, defGateway := readDefaultRoute()
	return detectFrom(collectIfaceAddrs(), defIface, defGateway)
}

// readDefaultRoute 读取 Linux 的默认路由（网关 + 出口网卡）。
//
// 读不到（非 Linux / 权限受限）时返回空串 —— 上层会退回"取第一个非虚拟网卡"，
// 而不是让整个功能失败。
func readDefaultRoute() (string, string) {
	const path = "/proc/net/route"
	content, err := os.ReadFile(path)
	if err != nil {
		return "", ""
	}
	return DefaultRouteFrom(ParseProcRoute(string(content)))
}

// ProcRoute 是 /proc/net/route 里的一行。
type ProcRoute struct {
	Iface   string
	Dest    string // 点分十进制
	Gateway string
	Mask    string
	Metric  int
	Up      bool
}

// ParseProcRoute 解析 /proc/net/route 的内容。
//
// 抽成纯函数是为了可测：这段格式（小端十六进制、制表符分隔）出错的方式
// 特别隐蔽 —— 解析错了不会报错，只会得到一个看着像 IP 的错地址。
func ParseProcRoute(content string) []ProcRoute {
	var out []ProcRoute
	for i, line := range strings.Split(content, "\n") {
		if i == 0 { // 表头
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 8 {
			continue
		}
		flags := 0
		_, _ = fmt.Sscanf(fields[3], "%x", &flags)
		out = append(out, ProcRoute{
			Iface:   fields[0],
			Dest:    decodeProcAddress(fields[1], false),
			Gateway: decodeProcAddress(fields[2], false),
			Mask:    decodeProcAddress(fields[7], false),
			Metric:  atoiSafe(fields[6]),
			Up:      flags&0x1 != 0,
		})
	}
	return out
}

// DefaultRouteFrom 从路由表里挑出默认路由的出口网卡与网关。
//
// 有多条默认路由时取 metric 最小的一条 —— 那是内核实际会用的那条。
func DefaultRouteFrom(routes []ProcRoute) (string, string) {
	best := ProcRoute{}
	found := false
	for _, r := range routes {
		if r.Dest != "0.0.0.0" || !r.Up {
			continue
		}
		if !found || r.Metric < best.Metric {
			best = r
			found = true
		}
	}
	if !found {
		return "", ""
	}
	gw := best.Gateway
	if gw == "0.0.0.0" {
		gw = ""
	}
	return best.Iface, gw
}

// collectIfaceAddrs 枚举网卡与其 IPv4 地址。
func collectIfaceAddrs() []ifaceAddrs {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []ifaceAddrs
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		item := ifaceAddrs{Name: iface.Name, Virtual: IsVirtualInterface(iface.Name)}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipNet.IP.To4()
			if ip4 == nil || ipNet.IP.IsLoopback() || ipNet.IP.IsLinkLocalUnicast() {
				continue
			}
			ones, _ := ipNet.Mask.Size()
			item.IPv4 = append(item.IPv4, ip4)
			item.Mask = append(item.Mask, ones)
		}
		if len(item.IPv4) > 0 {
			out = append(out, item)
		}
	}
	return out
}

// detectFrom 是网段判定的纯函数部分。
func detectFrom(addrs []ifaceAddrs, defIface, defGateway string) NetworkInfo {
	info := NetworkInfo{Gateway: defGateway}

	type candidate struct {
		prefix  string
		rank    int // 越小越可信
		iface   string
		virtual bool
	}
	var candidates []candidate

	for _, a := range addrs {
		// 网卡优先级：默认路由出口 > 物理网卡 > 虚拟网卡。
		rank := 1
		if !a.Virtual {
			rank = 0
		} else {
			rank = 2
		}
		if defIface != "" && a.Name == defIface {
			rank = -1
		}

		brief := IfaceBrief{
			Name:    a.Name,
			Virtual: a.Virtual,
			Default: defIface != "" && a.Name == defIface,
		}
		for i, ip := range a.IPv4 {
			ips := ip.String()
			info.LocalIPs = append(info.LocalIPs, ips)
			brief.IPv4 = append(brief.IPv4, ips)
			if i < len(a.Mask) && a.Mask[i] > 0 {
				cidr := fmt.Sprintf("%s/%d", ips, a.Mask[i])
				brief.CIDRs = append(brief.CIDRs, cidr)
				info.Cidrs = append(info.Cidrs, cidr)
			}
			prefix := SubnetPrefix(ip)
			if prefix == "" {
				continue
			}
			candidates = append(candidates, candidate{prefix: prefix, rank: rank, iface: a.Name, virtual: a.Virtual})
		}
		info.Ifaces = append(info.Ifaces, brief)
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].rank != candidates[j].rank {
			return candidates[i].rank < candidates[j].rank
		}
		return candidates[i].prefix < candidates[j].prefix
	})

	// 只在没有任何"非虚拟"候选时，才把虚拟网卡端上来。
	// 那种情况通常是容器里跑本程序（没有物理网卡），能让用户至少扫到点什么。
	hasReal := false
	for _, c := range candidates {
		if !c.virtual {
			hasReal = true
			break
		}
	}

	seen := map[string]bool{}
	for _, c := range candidates {
		if c.virtual && hasReal {
			continue
		}
		if seen[c.prefix] {
			continue
		}
		seen[c.prefix] = true
		info.Subnets = append(info.Subnets, c.prefix)
	}

	if len(info.Subnets) > 0 {
		info.Primary = info.Subnets[0]
	}

	// 上级网段：默认网关不在本机 /24 里时，网关所在网段就是"上一跳"那一段。
	// 网关和 NAS 同段时这里保持为空 —— 那说明本段就是最外层，没有可推导的上级。
	if gw := net.ParseIP(defGateway); gw != nil && SubnetPrefix(gw) != "" {
		p := SubnetPrefix(gw)
		if p != info.Primary {
			info.Parent = p
		}
	}

	switch {
	case len(info.Subnets) == 0:
		info.Warning = "没有探测到任何可用的 IPv4 网段，请用「自定义范围」直接填写要扫描的地址。"
	case !hasReal:
		info.Warning = "只探测到虚拟网卡（通常是容器环境），扫描结果可能不代表真实局域网。"
	}

	return info
}
