package domain

import (
	"fmt"
	"net"
	"strings"
	"time"
)

// Protocol 是端口转发规则支持的传输层协议。
//
// 它是「传输层」而不是「应用层」：这里转发的是原始字节流 / 数据报，
// 不解析 HTTP，因此一条规则能承载 SSH、RDP、SMB、数据库等任意 TCP/UDP 服务。
// 这与 internal/proxy 的七层反向代理是互补的两件事——后者能按域名路由、
// 能跳过自签证书，但只能服务 HTTP；前者什么都通，但看不见内容。
type Protocol string

const (
	// ProtoTCP 只转发 TCP。
	ProtoTCP Protocol = "tcp"
	// ProtoUDP 只转发 UDP。
	ProtoUDP Protocol = "udp"
	// ProtoBoth 同时转发 TCP 与 UDP（常见于 DNS、游戏、实时音视频）。
	ProtoBoth Protocol = "both"
)

// AllProtocols 按界面展示顺序列出全部协议。
var AllProtocols = []Protocol{ProtoTCP, ProtoUDP, ProtoBoth}

// Valid 报告 p 是否为已定义的协议。
func (p Protocol) Valid() bool {
	switch p {
	case ProtoTCP, ProtoUDP, ProtoBoth:
		return true
	default:
		return false
	}
}

// HasTCP / HasUDP 报告该协议是否需要建立对应的监听。
func (p Protocol) HasTCP() bool { return p == ProtoTCP || p == ProtoBoth }
func (p Protocol) HasUDP() bool { return p == ProtoUDP || p == ProtoBoth }

// Chinese 返回面向用户的协议名。
func (p Protocol) Chinese() string {
	switch p {
	case ProtoTCP:
		return "TCP"
	case ProtoUDP:
		return "UDP"
	case ProtoBoth:
		return "TCP + UDP"
	default:
		return string(p)
	}
}

// ParseProtocol 解析来自 API / 配置文件的协议字符串。
func ParseProtocol(s string) (Protocol, error) {
	p := Protocol(strings.ToLower(strings.TrimSpace(s)))
	if !p.Valid() {
		return "", fmt.Errorf("%w: 未知的协议 %q（可选: tcp / udp / both）", ErrValidation, s)
	}
	return p, nil
}

// AccessMode 是转发规则的访问控制模式。
//
// 之所以值得做：转发规则一旦开启，等于把内网服务的一个端口暴露给了
// 能访问到这台设备的人。对没有认证的服务（Redis、SMB、摄像头 RTSP 等）
// 来说，白名单往往是唯一能挡住的东西。
type AccessMode string

const (
	// AccessOff 不做限制。
	AccessOff AccessMode = ""
	// AccessAllow 是白名单：只有列表内的来源可以连。
	AccessAllow AccessMode = "allow"
	// AccessDeny 是黑名单：列表内的来源会被拒绝，其余放行。
	AccessDeny AccessMode = "deny"
)

// ParseAccessMode 解析访问控制模式字符串；空值收敛为「不限制」。
func ParseAccessMode(s string) (AccessMode, error) {
	m := AccessMode(strings.ToLower(strings.TrimSpace(s)))
	switch m {
	case AccessOff, AccessAllow, AccessDeny:
		return m, nil
	default:
		return "", fmt.Errorf("%w: 未知的访问控制模式 %q（可选: off / allow / deny）", ErrValidation, s)
	}
}

// Forward 是一条端口转发规则：把本机某个端口收到的流量转送到另一处。
//
// 与 Link 一样，这里只承载**定义**，不承载运行态。
// 是否真的在监听、累计转了多少字节、最近一次失败原因，全部由
// internal/forward 的 Manager 实时合成，不写进 JSON。
type Forward struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Desc string `json:"desc,omitempty"`

	// Protocol 决定建立哪种监听。both 会同时起 TCP 与 UDP 两个监听。
	Protocol Protocol `json:"protocol"`

	// ListenHost 是本机的监听地址。
	//
	// 空串表示监听全部地址（0.0.0.0 与 ::），这也是绝大多数场景需要的：
	// 转发规则的典型用途正是让**局域网或公网的别的机器**连进来。
	// 只有想把它限制成"仅本机可用"时才填 127.0.0.1。
	ListenHost string `json:"listen_host,omitempty"`
	// ListenPort 是本机监听端口。
	ListenPort int `json:"listen_port"`

	// TargetHost 是目标主机（IP 或域名）。
	TargetHost string `json:"target_host"`
	// TargetPort 是目标端口。
	TargetPort int `json:"target_port"`

	// Enabled 为 false 时规则只保存、不监听。
	Enabled bool `json:"enabled"`

	// AccessMode 与 AccessList 是一对：前者为 off 时后者无意义。
	AccessMode AccessMode `json:"access_mode,omitempty"`
	// AccessList 的每一项可以是单个 IP（192.168.1.5）或 CIDR（192.168.1.0/24）。
	AccessList []string `json:"access_list,omitempty"`

	// MaxConns 限制并发连接数（TCP）或并发会话数（UDP）。
	//
	// 0 表示不限制。它是一条保险绳而不是性能开关：一个配置错误的规则
	// 可能瞬间被扫出成千上万条连接，把设备的 fd 与内存吃干。
	MaxConns int `json:"max_conns,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Normalize 就地补齐默认值，保证进入存储层之前规则已是自洽状态。
func (f *Forward) Normalize() {
	f.Name = strings.TrimSpace(f.Name)
	f.Desc = strings.TrimSpace(f.Desc)
	f.ListenHost = strings.TrimSpace(f.ListenHost)
	f.TargetHost = strings.TrimSpace(f.TargetHost)

	f.Protocol = Protocol(strings.ToLower(strings.TrimSpace(string(f.Protocol))))
	if !f.Protocol.Valid() {
		f.Protocol = ProtoTCP
	}
	f.AccessMode = AccessMode(strings.ToLower(strings.TrimSpace(string(f.AccessMode))))

	// 空串与 0.0.0.0 是同一个意思（监听全部地址），统一存成空串，
	// 否则"看起来一样的两条规则"会被判成配置不同而反复重建监听。
	if f.ListenHost == "0.0.0.0" || f.ListenHost == "::" {
		f.ListenHost = ""
	}

	list := make([]string, 0, len(f.AccessList))
	seen := make(map[string]bool, len(f.AccessList))
	for _, item := range f.AccessList {
		s := strings.TrimSpace(item)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		list = append(list, s)
	}
	if len(list) > 0 {
		f.AccessList = list
	} else {
		f.AccessList = nil
	}

	if f.MaxConns < 0 {
		f.MaxConns = 0
	}
}

// Validate 校验规则是否自洽；返回的错误均可用 errors.Is(err, ErrValidation) 识别。
func (f Forward) Validate() error {
	if f.Name == "" {
		return fmt.Errorf("%w: 规则名称不能为空", ErrValidation)
	}
	if len([]rune(f.Name)) > 64 {
		return fmt.Errorf("%w: 规则名称过长（最多 64 字符）", ErrValidation)
	}
	if !f.Protocol.Valid() {
		return fmt.Errorf("%w: 未知的协议 %q", ErrValidation, f.Protocol)
	}
	if f.ListenPort <= 0 || f.ListenPort > 65535 {
		return fmt.Errorf("%w: 监听端口 %d 越界（1-65535）", ErrValidation, f.ListenPort)
	}
	if f.TargetHost == "" {
		return fmt.Errorf("%w: 目标地址不能为空", ErrValidation)
	}
	if f.TargetPort <= 0 || f.TargetPort > 65535 {
		return fmt.Errorf("%w: 目标端口 %d 越界（1-65535）", ErrValidation, f.TargetPort)
	}
	if _, err := ParseAccessMode(string(f.AccessMode)); err != nil {
		return err
	}
	for _, item := range f.AccessList {
		if !isIPOrCIDR(item) {
			return fmt.Errorf("%w: 访问控制列表里的 %q 不是合法的 IP 或 CIDR", ErrValidation, item)
		}
	}
	if f.MaxConns > maxForwardConns {
		return fmt.Errorf("%w: 并发上限 %d 超过最大值 %d", ErrValidation, f.MaxConns, maxForwardConns)
	}
	if f.IsSelfForward() {
		return fmt.Errorf("%w: 目标就是本机的同一个端口（%d），转发会形成死循环",
			ErrValidation, f.ListenPort)
	}
	return nil
}

// maxForwardConns 是单条规则允许填写的并发上限的天花板。
const maxForwardConns = 100000

// IsSelfForward 报告这条规则的目标是否就是它自己的监听端口。
//
// 这是最容易被用户顺手配出来的错误：想把 8080 暴露出去，就填了
// 8080 → 127.0.0.1:8080。它不会报错，只会让每个连进来的请求被
// 反复转发给本机，直到 fd 耗尽——表现是"转发开了一会儿设备就卡了"。
// 参考实现的「自转发预防」防的就是这一条。
//
// 这里只判**直接**自转发（无需外部知识）。间接环（A 的监听端口正好是
// B 的目标端口）由 internal/forward 的 Manager 在启动时检查，
// 因为那需要知道全部规则。
func (f Forward) IsSelfForward() bool {
	if f.ListenPort <= 0 || f.TargetPort <= 0 {
		return false
	}
	return f.ListenPort == f.TargetPort && IsLocalHost(f.TargetHost)
}

// IsLocalHost 报告 host 是否指向本机。
//
// 认三类写法：回环地址（127.0.0.0/8、::1）、localhost、以及未指定地址
// （0.0.0.0 / ::）。前两类是字面意义上的"本机"，第三类在 Linux 上
// 拨号时同样会被解释成本机，因此必须一起算进来，否则自转发检测会漏。
func IsLocalHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	h = strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
	switch h {
	case "", "localhost", "localhost.":
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		// IsUnspecified 覆盖 0.0.0.0 与 ::。
		//
		// 它们不是回环地址，但在 Linux 上对它们拨号同样会被解释成本机，
		// 因此必须一起算进来，否则"8080 → 0.0.0.0:8080"这种写法会绕过
		// 自转发检测。这里刻意**不**在 switch 里再列一遍这两个字面量：
		// 两处都能达成同一个结果时，测试就守不住其中任何一处。
		return ip.IsLoopback() || ip.IsUnspecified()
	}
	return false
}

// isIPOrCIDR 判断字符串是否为合法的 IP 或 CIDR。
func isIPOrCIDR(s string) bool {
	if strings.Contains(s, "/") {
		_, _, err := net.ParseCIDR(s)
		return err == nil
	}
	return net.ParseIP(s) != nil
}

// TargetAddr 返回目标地址的 host:port 形式，供拨号使用。
func (f Forward) TargetAddr() string {
	host := f.TargetHost
	// 含冒号说明是 IPv6 字面量，必须加方括号，否则端口会被当成地址的一部分。
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]"
	}
	return fmt.Sprintf("%s:%d", host, f.TargetPort)
}

// ListenAddr 返回监听地址的 host:port 形式。
//
// 空 host 时返回 ":port"，Go 的 net.Listen 会据此监听全部地址
// （双栈环境下同时覆盖 IPv4 与 IPv6）。
func (f Forward) ListenAddr() string {
	host := f.ListenHost
	if host == "" {
		return fmt.Sprintf(":%d", f.ListenPort)
	}
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]"
	}
	return fmt.Sprintf("%s:%d", host, f.ListenPort)
}

// Key 返回用于日志与去重的可读标识。
func (f Forward) Key() string {
	if f.Name != "" {
		return f.Name
	}
	return f.ID
}
