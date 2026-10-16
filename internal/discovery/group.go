package discovery

import (
	"net"
	"sort"
	"strconv"
	"strings"
)

// AddressScope 描述一组绑定地址的可见范围。
//
// 这是**判断**而不是**展示**：前端据此决定这一行写「全网卡」还是列出具体
// 地址。判据必须只写一处（IsWildcardAddress / IsLoopbackOnly），
// 前后端不要各自去比字符串 "0.0.0.0" —— 两处判据迟早会不一致。
type AddressScope string

const (
	// ScopeAll 表示至少绑了一个通配地址（0.0.0.0 / ::），服务对所有网卡可见。
	ScopeAll AddressScope = "all"
	// ScopeLocal 表示只绑了回环地址，只有这台机器自己能访问。
	//
	// 注意它**不是**"没用"：桌面链接是经这台机器上的本应用转发的，
	// 只监听 127.0.0.1 的服务照样能放到桌面上。这里只是如实告知范围。
	ScopeLocal AddressScope = "local"
	// ScopeSpecific 表示绑在明确的网卡地址上。
	ScopeSpecific AddressScope = "specific"
)

// IsWildcardAddress 报告地址是不是"所有网卡"的通配地址。
//
// /proc/net/tcp 里的 0.0.0.0 与 tcp6 里的 :: 是同一个意思的两份副本；
// 并排显示出来，用户只会当成扫描出了一堆重复的脏数据（用户原话：
// 「为什么扫描出来的端口是重复的，有 0.0.0.0 的有不带的」）。
func IsWildcardAddress(addr string) bool {
	ip := net.ParseIP(strings.TrimSpace(addr))
	return ip != nil && ip.IsUnspecified()
}

// ClassifyScope 用同一份判据给一组地址定范围。
//
// 有通配地址就直接是 all —— 通配的含义就是"所有网卡都能到"，
// 再混进几个具体网卡地址不改变这个事实。
func ClassifyScope(addrs []string) AddressScope {
	if len(addrs) == 0 {
		return ScopeSpecific
	}
	loopbackOnly := true
	for _, a := range addrs {
		if IsWildcardAddress(a) {
			return ScopeAll
		}
		if !IsLoopbackOnly(a) {
			loopbackOnly = false
		}
	}
	if loopbackOnly {
		return ScopeLocal
	}
	return ScopeSpecific
}

// GroupPorts 把「同一服务在内核表里的多个套接字」合并成一行。
//
// 为什么必须合并：/proc/net 是按**套接字**记账的，用户看的却是**服务**。
//
//	sshd     → net/tcp 的 0.0.0.0:22 与 net/tcp6 的 :::22        两份
//	rpcbind  → tcp/udp × IPv4/IPv6                             四份
//	nmbd     → 在每一个网卡地址上各绑一份                        十几份
//
// 合并前飞牛上显示"监听端口 101 个"，其中真正不同的服务不到二十个，
// 其余全是同一个东西的副本 —— 而界面上还看不出它们是副本（见 portItem：
// 协议只在第二行为空时才显示，于是同地址同端口的 tcp / udp 两行长得一模一样）。
//
// 合并键是「进程 + 端口」：协议**不进键**（同一服务的 tcp 与 udp 是同一件事），
// 地址不进键（地址是要收集的信息，不是身份），容器也不进键 ——
// 容器信息是被 Enrich 按端口贴上去的**信息**，把它算进身份只会让
// "某个套接字恰好没被贴上容器"变成一条幻影重复。
//
// 不同进程占同一个端口（SO_REUSEPORT、容器映射与宿主机服务各一份）仍然分开，
// 靠的就是键里的进程这一项。
func GroupPorts(in []PortInfo) []PortInfo {
	if len(in) == 0 {
		return []PortInfo{}
	}

	keys := make([]string, 0, len(in))
	groups := make(map[string]*portGroup, len(in))

	for _, raw := range in {
		key := groupKey(raw)
		g, ok := groups[key]
		if !ok {
			info := raw
			info.Addresses = nil
			info.Protos = nil
			info.Scope = ""
			g = &portGroup{
				info:      info,
				protos:    make(map[Proto]bool, 2),
				addresses: make(map[string]bool, 2),
			}
			groups[key] = g
			keys = append(keys, key)
		}

		if raw.Address != "" {
			g.addresses[raw.Address] = true
		}
		if raw.Proto != "" {
			g.protos[raw.Proto] = true
		}

		// 这里**故意**不写"逐字段补全"。
		//
		// 写过一版"先到的套接字读不到进程名时，让后到的补上"，测试直接证明了
		// 那段永远不执行：进程名 / PID / 用户名来自同一次 /proc/<pid>/fd 查找，
		// 要么一起读到要么一起读不到，而 groupKey 又要求同一行的进程相同。
		// 留着的死代码比没有更危险 —— 它让"这里做过容错"看起来是真的。
		//
		// 只对两类**由外部注入**的信息做"不留空缺"的合并：容器信息与桌面标记。
		if g.info.Container == nil {
			g.info.Container = raw.Container
		}
		if g.info.LabelHint == nil {
			g.info.LabelHint = raw.LabelHint
		}
		if raw.Managed {
			g.info.Managed = true
		}
	}

	out := make([]PortInfo, 0, len(keys))
	for _, key := range keys {
		g := groups[key]
		info := g.info
		info.Addresses = sortedStrings(g.addresses)
		info.Protos = orderedProtos(g.protos)
		if primary := primaryProto(info.Protos); primary != "" {
			// Proto 保留成"主协议"，给仍然只认单协议的老调用方（排序、容器匹配）。
			info.Proto = primary
		}
		info.Address = representativeAddress(info.Addresses)
		info.Scope = ClassifyScope(info.Addresses)
		out = append(out, info)
	}
	return out
}

// portGroup 是合并过程中的中间态。
type portGroup struct {
	info      PortInfo
	protos    map[Proto]bool
	addresses map[string]bool
}

// groupKey 是「同一个服务」的身份：进程 + 端口。
func groupKey(p PortInfo) string {
	var b strings.Builder
	switch {
	case p.Process != "":
		b.WriteString(p.Process)
	case p.PID > 0:
		// 进程名读不到时退回 PID：同一个进程的多个套接字仍然能合并，
		// 而两个恰好都读不到名字的不同进程不会被误并。
		b.WriteString("pid:")
		b.WriteString(strconv.Itoa(p.PID))
	}
	b.WriteByte('|')
	b.WriteString(strconv.Itoa(p.Port))
	return b.String()
}

// orderedProtos 固定输出顺序 tcp → udp。
//
// 顺序稳定是为了前端能安心把它直接拼成「TCP+UDP」这样的标签：
// 顺序游移会让同一行在每次刷新时来回跳，看起来像数据在闪。
func orderedProtos(set map[Proto]bool) []Proto {
	out := make([]Proto, 0, len(set))
	for _, p := range []Proto{ProtoTCP, ProtoUDP} {
		if set[p] {
			out = append(out, p)
		}
	}
	// 将来若多出别的协议，排在后面而不是被静静丢掉 —— 丢掉会让"少了一个
	// 监听协议"这件事完全不可见。排序保证顺序仍然是确定的。
	rest := make([]string, 0, 2)
	for p := range set {
		if p != ProtoTCP && p != ProtoUDP {
			rest = append(rest, string(p))
		}
	}
	sort.Strings(rest)
	for _, p := range rest {
		out = append(out, Proto(p))
	}
	return out
}

// primaryProto 取主协议。orderedProtos 已经保证 tcp 排在前面。
func primaryProto(protos []Proto) Proto {
	if len(protos) == 0 {
		return ""
	}
	return protos[0]
}

// representativeAddress 挑一个代表地址，供仍然只认单地址的调用方使用
// （PortInfo.Display 等）。
//
// 界面**不用**它：nmbd 那种绑了十一个网卡地址的服务，挑哪一个都是误导
// —— 按字典序第一个往往落在 docker 的网桥地址上。界面走 Scope + Addresses。
func representativeAddress(addrs []string) string {
	if len(addrs) == 0 {
		return ""
	}
	for _, a := range addrs {
		if IsWildcardAddress(a) {
			return a
		}
	}
	return addrs[0]
}

func sortedStrings(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
