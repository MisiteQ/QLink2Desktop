package discovery

import (
	"errors"
	"fmt"
	"net"
	"strings"
)

// ErrRangeTooLarge 表示用户给的范围超出了单次扫描的上限。
var ErrRangeTooLarge = errors.New("扫描范围过大")

// MaxScanHosts 是单次扫描展开后允许的最大主机数。
//
// 1024 台的量级：家用 / 小型办公网络里，/24 网段（254 台）是最常见的诉求，
// 连扫四个网段也在预算内；而再往上（/22、/16）就该让用户明确知道
// "这次要扫几万台"了 —— 与其让请求挂住，不如直接拒绝并说明原因。
const MaxScanHosts = 1024

// ExpandRangeSpec 把用户填写的范围描述展开成待扫描主机列表。
//
// 支持的写法（多种用逗号、分号或换行分隔）：
//
//	192.168.1            整段（.1 ~ .254）
//	192.168.1.10         单台
//	192.168.1.10-60      段内区间
//	192.168.1.10-192.168.1.60   跨段区间
//	192.168.1.0/24       按掩码展开（自动跳过网络号与广播地址）
//	nas.local            主机名 / 域名
//	https://a.b:8080/x   粘贴进来的网址（自动剥离协议、端口与路径）
//
// 设计取舍：**不做静默纠错**。写错的地方一律报错并指出是哪一个 token，
// 因为"扫描范围悄悄少了一段"比"明确报错"难发现得多。
func ExpandRangeSpec(spec string) ([]string, error) {
	tokens := splitSpec(spec)
	if len(tokens) == 0 {
		return nil, errors.New("请填写要扫描的范围")
	}

	var hosts []string
	seen := make(map[string]bool, 254)
	add := func(ip string) error {
		if ip == "" || seen[ip] {
			return nil
		}
		if len(hosts) >= MaxScanHosts {
			return fmt.Errorf("%w：单次最多扫描 %d 台主机，请缩小范围或用逗号分次扫描",
				ErrRangeTooLarge, MaxScanHosts)
		}
		seen[ip] = true
		hosts = append(hosts, ip)
		return nil
	}

	for _, tok := range tokens {
		// 用户很可能直接从浏览器里复制一段网址贴进来。先剥掉协议、端口与路径，
		// 否则 URL 里的斜杠会被当成 CIDR 的掩码分隔符，报出一个莫名其妙的错误。
		if strings.Contains(tok, "://") {
			tok = NormalizeHostInput(tok)
		}

		switch {
		case strings.Contains(tok, "/"):
			list, err := expandCIDR(tok)
			if err != nil {
				return nil, err
			}
			for _, ip := range list {
				if err := add(ip); err != nil {
					return nil, err
				}
			}

		case isDashRange(tok):
			list, err := expandDashRange(tok)
			if err != nil {
				return nil, err
			}
			for _, ip := range list {
				if err := add(ip); err != nil {
					return nil, err
				}
			}

		default:
			norm := NormalizeHostInput(tok)

			// 完整的四段地址 = 单台。
			if ip := net.ParseIP(norm); ip != nil && ip.To4() != nil {
				if err := add(ip.To4().String()); err != nil {
					return nil, err
				}
				continue
			}

			// 三段前缀的写法（192.168.1）不是合法 IP，但很常用，单独识别。
			if prefix, ok := parseShortPrefix(norm); ok {
				for _, h := range ExpandSubnet(prefix) {
					if err := add(h); err != nil {
						return nil, err
					}
				}
				continue
			}

			if !looksLikeHostname(norm) {
				return nil, fmt.Errorf("无法识别的范围写法 %q（可填 192.168.1、192.168.1.10-60、192.168.1.0/24 或主机名）", tok)
			}
			if err := add(norm); err != nil {
				return nil, err
			}
		}
	}

	if len(hosts) == 0 {
		return nil, errors.New("没有解析出任何可扫描的地址")
	}
	return hosts, nil
}

// splitSpec 按常见分隔符切分范围描述。
func splitSpec(spec string) []string {
	fields := strings.FieldsFunc(spec, func(r rune) bool {
		switch r {
		case ',', ';', '\n', '\r', '\t', ' ', '，', '；', '、':
			return true
		}
		return false
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

// parseShortPrefix 识别 "192.168.1" 这种省略了主机位的写法。
func parseShortPrefix(tok string) (string, bool) {
	parts := strings.Split(strings.TrimSpace(tok), ".")
	if len(parts) != 3 {
		return "", false
	}
	for _, p := range parts {
		if !isDecimal(p) {
			return "", false
		}
		if n := atoiSafe(p); n < 0 || n > 255 {
			return "", false
		}
	}
	return strings.Join(parts, "."), true
}

// expandCIDR 展开 a.b.c.d/len。
func expandCIDR(tok string) ([]string, error) {
	ip, ipNet, err := net.ParseCIDR(strings.TrimSpace(tok))
	if err != nil {
		return nil, fmt.Errorf("无法识别的网段写法 %q", tok)
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return nil, fmt.Errorf("只支持 IPv4 网段（%q）", tok)
	}
	ones, bits := ipNet.Mask.Size()
	if bits != 32 {
		return nil, fmt.Errorf("只支持 IPv4 网段（%q）", tok)
	}

	// 计算可用地址数，超上限直接拒绝（不要先展开再截断 —— 那会让用户
	// 以为"扫完了"，实际只扫了前面一小部分）。
	total := 1 << uint(32-ones)
	usable := total
	if ones <= 30 {
		usable = total - 2 // 去掉网络号与广播地址
	}
	if usable > MaxScanHosts {
		return nil, fmt.Errorf("%w：%s 包含 %d 个地址，超过单次上限 %d 台",
			ErrRangeTooLarge, tok, usable, MaxScanHosts)
	}

	base := ipToUint32(ipNet.IP)
	start, end := base, base+uint32(total)-1
	if ones <= 30 {
		start, end = base+1, base+uint32(total)-2
	}

	out := make([]string, 0, usable)
	for v := start; v <= end; v++ {
		out = append(out, uint32ToIP(v).String())
		if len(out) >= MaxScanHosts {
			break
		}
	}
	return out, nil
}

// isDashRange 判断 token 是不是 "起始地址-结束地址" 这种区间写法。
//
// 必须先把带连字符的**主机名**（my-nas.local）排除掉 —— 否则一个再正常
// 不过的域名会被当成区间，报出一个莫名其妙的错误。
func isDashRange(tok string) bool {
	left, right, ok := strings.Cut(tok, "-")
	if !ok || strings.TrimSpace(left) == "" || strings.TrimSpace(right) == "" {
		return false
	}
	ip := net.ParseIP(NormalizeHostInput(left))
	return ip != nil && ip.To4() != nil
}

// expandDashRange 展开 "a.b.c.d-e" 与 "a.b.c.d-a.b.c.e" 两种区间写法。
func expandDashRange(tok string) ([]string, error) {
	left, right, ok := strings.Cut(strings.TrimSpace(tok), "-")
	if !ok {
		return nil, fmt.Errorf("无法识别的范围写法 %q", tok)
	}
	left = strings.TrimSpace(left)
	right = strings.TrimSpace(right)
	if left == "" || right == "" {
		return nil, fmt.Errorf("范围 %q 缺少起始或结束地址", tok)
	}

	startIP := net.ParseIP(NormalizeHostInput(left))
	if startIP == nil || startIP.To4() == nil {
		return nil, fmt.Errorf("范围 %q 的起始地址不是合法 IPv4", tok)
	}
	start := ipToUint32(startIP.To4())

	// 右端可以是完整地址，也可以只是最后一段（10-60）。
	var end uint32
	if endIP := net.ParseIP(NormalizeHostInput(right)); endIP != nil && endIP.To4() != nil {
		end = ipToUint32(endIP.To4())
	} else {
		if !isDecimal(right) {
			return nil, fmt.Errorf("范围 %q 的结束地址不合法", tok)
		}
		last := atoiSafe(right)
		if last < 0 || last > 255 {
			return nil, fmt.Errorf("范围 %q 的结束地址应在 0-255 之间", tok)
		}
		end = (start & 0xFFFFFF00) | uint32(last)
	}

	if end < start {
		return nil, fmt.Errorf("范围 %q 的结束地址小于起始地址", tok)
	}
	if int64(end-start)+1 > MaxScanHosts {
		return nil, fmt.Errorf("%w：%s 包含 %d 个地址，超过单次上限 %d 台",
			ErrRangeTooLarge, tok, int64(end-start)+1, MaxScanHosts)
	}

	out := make([]string, 0, end-start+1)
	for v := start; v <= end; v++ {
		out = append(out, uint32ToIP(v).String())
	}
	return out, nil
}

func ipToUint32(ip net.IP) uint32 {
	ip4 := ip.To4()
	if ip4 == nil {
		return 0
	}
	return uint32(ip4[0])<<24 | uint32(ip4[1])<<16 | uint32(ip4[2])<<8 | uint32(ip4[3])
}

func uint32ToIP(v uint32) net.IP {
	return net.IPv4(byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}

func isDecimal(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// looksLikeHostname 判断一个字符串是否像主机名 / 域名。
//
// 只做保守校验：字母数字加 . _ -，且至少含一个字母，长度不超过 253。
// 目的是拦住明显的手误（例如把 "192.168.1..5" 这种输入当域名扫出去）。
func looksLikeHostname(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	hasLetter := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
			hasLetter = true
		case r >= '0' && r <= '9', r == '.', r == '-', r == '_':
		default:
			return false
		}
	}
	return hasLetter
}
