package forward

import (
	"fmt"
	"strings"
)

// 本文件只做**文本解析**，不做任何 IO，因此在不带 /proc 的平台上也能被测到。
//
// 这一点是刻意的：占用者诊断的 Linux 实现（who_linux.go）在开发机上
// 根本跑不起来，如果把解析和读文件揉在一起，那段最容易写错（列号错、
// 十六进制端口比较错）的逻辑就永远没有测试覆盖。

// parseListenInodes 从 /proc/net/{tcp,tcp6,udp,udp6} 的**内容**里
// 找出正在监听 port 的 socket inode。
//
// 表格式（空格分隔，首行是表头）：
//
//		sl  local_address rem_address  st tx_queue:rx_queue ... uid timeout inode
//		 0: 00000000:1F90 00000000:0000 0A 00000000:00000000 ...     0       0 31453 ...
//
//	  - local_address 是「十六进制 IP : 十六进制端口」，端口一律大写十六进制；
//	  - st 为 `0A` 表示 TCP LISTEN，`07` 表示 UDP 未连接（也就是在监听）；
//	  - inode 落在第 10 列（下标 9），它是后续反查进程的唯一钥匙。
func parseListenInodes(content string, port int, proto string) []string {
	// 端口在表里是**四位补零**的大写十六进制：53 写成 "0035"、8080 写成 "1F90"。
	// 按 "%X" 拼出来会得到 "35"，与内核的写法永远不相等 —— 这个坑在写
	// Linux 实现时靠肉眼是看不出来的（8080 恰好是四位，看着就是对的），
	// 是解析函数的单测抓出来的。
	wantPort := fmt.Sprintf("%04X", port)
	wantState := "0A"
	if proto == "udp" {
		wantState = "07"
	}

	var out []string
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		// 表头只有 9 列左右且第 1 列不是 "N:"，用它挡掉；
		// 同时挡掉可能的截断行。
		if len(fields) < 10 {
			continue
		}
		if fields[3] != wantState {
			continue
		}
		colon := strings.LastIndex(fields[1], ":")
		if colon < 0 {
			continue
		}
		if !strings.EqualFold(fields[1][colon+1:], wantPort) {
			continue
		}
		inode := fields[9]
		if inode == "" || inode == "0" {
			continue
		}
		out = append(out, inode)
	}
	return out
}

// socketInodeOfLink 从 /proc/<pid>/fd 的软链目标里取出 socket inode。
//
// 目标形如 `socket:[31453]`；普通文件、管道、匿名 inode（`anon_inode:...`）
// 一律返回 false —— 它们不可能是监听端口的那一个。
func socketInodeOfLink(link string) (string, bool) {
	const prefix, suffix = "socket:[", "]"
	if !strings.HasPrefix(link, prefix) || !strings.HasSuffix(link, suffix) {
		return "", false
	}
	inode := link[len(prefix) : len(link)-len(suffix)]
	if inode == "" {
		return "", false
	}
	return inode, true
}
