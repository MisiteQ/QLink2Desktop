package forward

import (
	"net"
	"strings"

	"github.com/MisiteQ/qlink2desktop/internal/domain"
)

// ACL 是一条规则编译后的访问控制表。
//
// 为什么要先编译：转发的热路径上每个连接都要问一次"这个 IP 能不能连"。
// 如果每次都去 strings.Split + net.ParseCIDR，一个被扫描的端口会白白
// 烧掉大量 CPU——而且那正是最需要省 CPU 的时刻。
type ACL struct {
	mode domain.AccessMode
	// nets 存放列表里的 CIDR 段。
	nets []*net.IPNet
	// ips 存放列表里的单个 IP，键是归一化后的字符串。
	ips map[string]bool
}

// compileACL 把规则的访问控制配置编译成查询表。
func compileACL(mode domain.AccessMode, list []string) ACL {
	a := ACL{mode: mode, ips: make(map[string]bool)}
	for _, item := range list {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if strings.Contains(item, "/") {
			if _, n, err := net.ParseCIDR(item); err == nil {
				a.nets = append(a.nets, n)
			}
			continue
		}
		if ip := net.ParseIP(item); ip != nil {
			a.ips[normalizeIP(ip)] = true
		}
	}
	return a
}

// Allows 判断来自 addr 的连接是否被允许。
//
// addr 可以是纯 IP，也可以是 net.Conn 那种 host:port 形式——
// 转发热路径上拿到的是后者，这里顺手剥掉端口，调用方不必先切一次字符串。
//
// 解析不出 IP 时的取舍值得说明：
//   - 白名单模式返回 false（fail-closed）：白名单的语义是"只有这些能进"，
//     认不出来就不属于"这些"，放行等于把安全策略开一个口子；
//   - 黑名单模式返回 true（fail-open）：黑名单的语义是"只挡这些"，
//     认不出来就不该被株连，否则一条写错的规则会挡住所有人。
func (a ACL) Allows(addr string) bool {
	if a.mode == domain.AccessOff {
		return true
	}
	ip := parseAddrIP(addr)
	if ip == nil {
		return a.mode == domain.AccessDeny
	}
	hit := a.matches(ip)
	if a.mode == domain.AccessAllow {
		return hit
	}
	return !hit
}

// Enabled 报告这条规则是否启用了访问控制。
func (a ACL) Enabled() bool { return a.mode != domain.AccessOff }

func (a ACL) matches(ip net.IP) bool {
	if a.ips[normalizeIP(ip)] {
		return true
	}
	for _, n := range a.nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// parseAddrIP 从 "1.2.3.4:5678"、"[::1]:80" 或 "1.2.3.4" 里取出 IP。
func parseAddrIP(addr string) net.IP {
	s := strings.TrimSpace(addr)
	if s == "" {
		return nil
	}
	// 先按 host:port 切开。IPv6 带方括号，用 LastIndex(':') 之外的办法不可靠，
	// 因此统一走 SplitHostPort，失败再当纯 IP 解析。
	if host, _, err := net.SplitHostPort(s); err == nil {
		s = host
	}
	s = strings.TrimSuffix(strings.TrimPrefix(s, "["), "]")
	return net.ParseIP(s)
}

// normalizeIP 归一化 IP，让 IPv4 与它的 IPv4-mapped IPv6 写法等价。
//
// 双栈监听下，同一个对端可能一会儿报 ::ffff:192.168.1.5、一会儿报
// 192.168.1.5。不归一化的话，一条写 192.168.1.5 的白名单会在 IPv6
// 路径上神秘失效——这类"有时能用有时不行"的问题最难查。
func normalizeIP(ip net.IP) string {
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return ip.String()
}
