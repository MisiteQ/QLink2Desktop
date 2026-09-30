//go:build linux

package discovery

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// procNetSources 描述要读取的内核套接字表。
var procNetSources = []struct {
	file  string
	proto Proto
	ipv6  bool
}{
	{"net/tcp", ProtoTCP, false},
	{"net/tcp6", ProtoTCP, true},
	{"net/udp", ProtoUDP, false},
	{"net/udp6", ProtoUDP, true},
}

// collectListening 读取 procfs 中的内核套接字表，返回监听中的套接字。
//
// 这是 Linux 上唯一「准确」的做法：相比逐个端口尝试连接，
// 它能拿到真实绑定地址、协议、以及所属进程的 inode，
// 也不会因为防火墙或只绑定内网地址而漏报。
func collectListening(procPath string) []rawSocket {
	if procPath == "" {
		procPath = "/proc"
	}
	var all []rawSocket
	for _, src := range procNetSources {
		data, err := os.ReadFile(filepath.Join(procPath, filepath.FromSlash(src.file)))
		if err != nil {
			continue // 未启用 IPv6 等情况下文件不存在，属正常
		}
		all = append(all, ParseProcNet(string(data), src.proto, src.ipv6)...)
	}
	return all
}

// socketOwner 是某个 socket inode 的占用者。
type socketOwner struct {
	PID  int
	Name string
	User string
}

// collectSocketOwners 遍历 /proc/<pid>/fd 建立「socket inode → 进程」索引。
//
// 这是 /proc 扫描里最贵的一步（每个进程都要读 fd 目录），
// 因此只在需要时调用，结果由 Scanner 缓存。
func collectSocketOwners(procPath string) map[string]socketOwner {
	if procPath == "" {
		procPath = "/proc"
	}
	owners := make(map[string]socketOwner, 256)

	entries, err := os.ReadDir(procPath)
	if err != nil {
		return owners
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}

		procDir := filepath.Join(procPath, e.Name())
		fdDir := filepath.Join(procDir, "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue // 权限不足（非 root 时常见），跳过即可
		}

		// 进程名与用户名只在确实有 socket 时才去解析，省掉大量无效读。
		var name, user string
		resolved := false

		for _, fd := range fds {
			link, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
			if err != nil {
				continue
			}
			inode, ok := ParseSocketInodeOwner(link)
			if !ok {
				continue
			}
			if !resolved {
				name = readProcessName(procDir)
				user = readProcessUser(procDir)
				resolved = true
			}
			owners[inode] = socketOwner{PID: pid, Name: name, User: user}
		}
	}
	return owners
}

func readProcessName(procDir string) string {
	// 优先用 comm：它是内核维护的短名，读取成本最低。
	if data, err := os.ReadFile(filepath.Join(procDir, "comm")); err == nil {
		if s := strings.TrimSpace(string(data)); s != "" {
			return s
		}
	}
	// 回退到 cmdline 的 basename。
	if data, err := os.ReadFile(filepath.Join(procDir, "cmdline")); err == nil {
		parts := strings.Split(strings.TrimRight(string(data), "\x00"), "\x00")
		if len(parts) > 0 && parts[0] != "" {
			return filepath.Base(parts[0])
		}
	}
	return ""
}

func readProcessUser(procDir string) string {
	data, err := os.ReadFile(filepath.Join(procDir, "status"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "Uid:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				return fields[1] // 真实 UID
			}
		}
	}
	return ""
}
