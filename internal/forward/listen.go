package forward

import (
	"fmt"
	"net"
	"strings"
	"time"
)

// bindMode 描述一次监听实际落在哪个协议栈上。
//
// 它必须被记录下来并显示给用户：`[::]:18080` 在"双栈"与"仅 IPv6"两种
// 情况下字符串完全一样，但后者意味着**IPv4 的访客连不上** ——
// 一个只在 IPv6 上监听转发规则，用户会以为"转发坏了"，而真相是
// 占用者占着 IPv4 侧、我们退到了 IPv6 侧。
type bindMode string

const (
	// bindDual 是 IPv6 + IPv4 双栈，对应 Go 对 ":port" 的默认行为。
	bindDual bindMode = "dual"
	// bindV4 是仅 IPv4。
	bindV4 bindMode = "ipv4"
	// bindV6 是仅 IPv6。
	bindV6 bindMode = "ipv6"
	// bindFixed 表示用户明确指定了监听地址，本应用不做任何退让。
	bindFixed bindMode = "fixed"
)

// degraded 报告这次绑定是否"退让"过（与自动模式下的首选形态不同）。
//
// 只有真退让才值得告警：用户自己填了地址的固定绑定不是降级，
// 把它也报成降级会稀释告警的意义。
func (m bindMode) degraded() bool { return m == bindV4 || m == bindV6 }

// bindCandidate 是一种可尝试的绑定形态。
type bindCandidate struct {
	network string
	addr    string
	mode    bindMode
}

// bindCandidates 给出该监听配置下应当尝试的绑定形态，按优先级排列。
//
// host 为空 = 「全部地址」= 用户没有指定协议栈，此时**必须**留退路。
// 这是本项目踩过的一个真实差异：固定绑 `:port`（Go 的双栈）在 Linux 上
// 要求 IPv4 与 IPv6 两侧都空闲，只要占用者占了任一侧就失败；而只绑
// 单栈的工具此时反而能绑上，用户看到的就是"同样的设置，那个程序行、这个不行"。
//
//   - 先 `[::]:port`（双栈）：覆盖最广，绝大多数情况的正确答案；
//   - 再 `0.0.0.0:port`（仅 IPv4）：救"IPv6 侧被占"——只监听 IPv6 的服务
//     或 v6only=1 的程序占着端口时，双栈绑不上，仅 IPv4 可以；
//   - 最后 `[::]:port` + v6only（仅 IPv6）：救"IPv4 侧被占"，
//     此时 IPv4 是回不来了，但至少让 IPv6 访客能用上，而不是整条规则趴着。
//
// host 非空时只有一个候选：用户明确指定了地址，替他改地址是更严重的错误。
func bindCandidates(host string, port int, proto string) []bindCandidate {
	if host != "" {
		return []bindCandidate{{network: proto, addr: hostPort(host, port), mode: bindFixed}}
	}
	return []bindCandidate{
		{network: proto, addr: fmt.Sprintf(":%d", port), mode: bindDual},
		{network: proto + "4", addr: fmt.Sprintf("0.0.0.0:%d", port), mode: bindV4},
		{network: proto + "6", addr: fmt.Sprintf("[::]:%d", port), mode: bindV6},
	}
}

// hostPort 把主机与端口拼成 net 包认得的地址串。
//
// IPv6 字面量必须加方括号，否则 `::1:8080` 会被解析成主机名。
func hostPort(host string, port int) string {
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]"
	}
	return fmt.Sprintf("%s:%d", host, port)
}

// listenTCP 建立 TCP 监听，返回监听器与真实的绑定形态。
//
// 只有"地址已被占用"才继续试下一种形态；端口号非法、没有权限这类
// 与形态无关的错误立即返回 —— 换个栈再试一次不会有不同结果。
func listenTCP(host string, port int) (net.Listener, bindMode, error) {
	var firstErr error
	for _, c := range bindCandidates(host, port, "tcp") {
		ln, err := listenWithRetry(c.network, c.addr)
		if err == nil {
			return ln, c.mode, nil
		}
		if firstErr == nil {
			firstErr = err
		}
		if !isAddrInUse(err) {
			break
		}
	}
	return nil, "", firstErr
}

// listenUDP 建立 UDP 监听，语义与 listenTCP 完全一致。
func listenUDP(host string, port int) (*net.UDPConn, bindMode, error) {
	var firstErr error
	for _, c := range bindCandidates(host, port, "udp") {
		laddr, err := net.ResolveUDPAddr(c.network, c.addr)
		if err != nil {
			return nil, "", err
		}
		conn, err := net.ListenUDP(c.network, laddr)
		if err == nil {
			return conn, c.mode, nil
		}
		if firstErr == nil {
			firstErr = err
		}
		if !isAddrInUse(err) {
			break
		}
	}
	return nil, "", firstErr
}

// listenWithRetry 监听端口，失败后短暂重试一次。
//
// 用途：刚重启时上一个进程的监听可能还在 TIME_WAIT，稍等即可成功。
// 与 proxy 包的同名函数出于同样的原因存在——这是端口复用最常见的假故障。
func listenWithRetry(network, addr string) (net.Listener, error) {
	ln, err := net.Listen(network, addr)
	if err == nil {
		return ln, nil
	}
	if !isAddrInUse(err) {
		return nil, err
	}
	time.Sleep(150 * time.Millisecond)
	return net.Listen(network, addr)
}
