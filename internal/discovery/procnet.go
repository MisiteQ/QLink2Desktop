package discovery

import (
	"encoding/hex"
	"net"
	"strconv"
	"strings"
)

// rawSocket 是从内核套接字表里解析出来的一行。
type rawSocket struct {
	Proto    Proto
	Address  string
	Port     int
	Inode    string
	UID      int
	Listened bool
}

// tcpListenState / udpListenState 是 /proc/net 里表示「正在监听」的套接字状态码。
//
//   - TCP：0A = TCP_LISTEN
//   - UDP：07 = TCP_CLOSE（UDP 是无连接协议，未连接即代表在收包）
const (
	tcpListenState = "0A"
	udpListenState = "07"
)

// ParseProcNet 解析 /proc/net/{tcp,tcp6,udp,udp6} 的内容。
//
// 抽成纯函数是刻意的：这样解析逻辑可以在任何平台上被完整单测覆盖，
// 而不必真的跑在一台 Linux 机器上。原项目把解析与文件读取揉在一起，
// 导致这段最容易出错的代码长期无法测试。
func ParseProcNet(content string, proto Proto, ipv6 bool) []rawSocket {
	var out []rawSocket
	for i, line := range strings.Split(content, "\n") {
		if i == 0 { // 表头
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 10 {
			continue
		}

		// 字段布局（内核稳定）：sl local_addr rem_addr st tx:rx tr:tm retrnsmt uid timeout inode
		local := fields[1]
		state := strings.ToUpper(fields[3])
		uid := atoiSafe(fields[7])
		inode := fields[9]

		listenState := tcpListenState
		if proto == ProtoUDP {
			listenState = udpListenState
		}
		if state != listenState {
			continue
		}

		hostHex, portHex, ok := strings.Cut(local, ":")
		if !ok {
			continue
		}
		port64, err := strconv.ParseUint(portHex, 16, 32)
		if err != nil || port64 == 0 || port64 > 65535 {
			continue
		}

		out = append(out, rawSocket{
			Proto:    proto,
			Address:  decodeProcAddress(hostHex, ipv6),
			Port:     int(port64),
			Inode:    inode,
			UID:      uid,
			Listened: true,
		})
	}
	return out
}

// decodeProcAddress 还原 /proc/net 里的小端序地址表示。
func decodeProcAddress(hexAddr string, ipv6 bool) string {
	if ipv6 {
		// 32 个十六进制字符 = 16 字节，但每 4 字节内部是小端。
		if len(hexAddr) != 32 {
			return hexAddr
		}
		b, err := hex.DecodeString(hexAddr)
		if err != nil {
			return hexAddr
		}
		for i := 0; i+3 < len(b); i += 4 {
			b[i], b[i+3] = b[i+3], b[i]
			b[i+1], b[i+2] = b[i+2], b[i+1]
		}
		return net.IP(b).String()
	}

	// 8 个十六进制字符 = 4 字节，整体小端。
	if len(hexAddr) != 8 {
		return hexAddr
	}
	v, err := strconv.ParseUint(hexAddr, 16, 32)
	if err != nil {
		return hexAddr
	}
	ip := net.IPv4(
		byte(v&0xff),
		byte(v>>8&0xff),
		byte(v>>16&0xff),
		byte(v>>24&0xff),
	)
	return ip.String()
}

// ParseSocketInodeOwner 从 /proc/<pid>/fd 下的符号链接目标里解析 socket inode。
// 链接形如 "socket:[24601]"。
func ParseSocketInodeOwner(link string) (string, bool) {
	const prefix = "socket:["
	if !strings.HasPrefix(link, prefix) || !strings.HasSuffix(link, "]") {
		return "", false
	}
	inode := link[len(prefix) : len(link)-1]
	if inode == "" {
		return "", false
	}
	return inode, true
}

func atoiSafe(s string) int {
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return v
}

// IsEphemeralPort 报告端口是否落在动态端口区间（通常由客户端临时占用）。
// 这类端口不应出现在「可放到桌面」的候选里。
func IsEphemeralPort(port int) bool {
	return port >= 32768
}

// IsLoopbackOnly 报告地址是否只对本机可见。
func IsLoopbackOnly(addr string) bool {
	if addr == "" {
		return false
	}
	ip := net.ParseIP(addr)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}
