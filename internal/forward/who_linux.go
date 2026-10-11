//go:build linux

package forward

import (
	"os"
	"strconv"
	"strings"
)

// Holder 描述一个正在占用某端口的进程。
type Holder struct {
	// PID 是占用者的进程号。
	PID int `json:"pid"`
	// Name 是进程名（/proc/<pid>/comm），取不到时为空。
	Name string `json:"name,omitempty"`
	// Side 是占用的协议栈侧："IPv4" 或 "IPv6"。
	//
	// 这一条对用户能不能自己解决问题关系很大：知道"对方只占着 IPv6 侧"，
	// 用户就可以把监听地址填成 0.0.0.0 绕过去，而不必换端口。
	Side string `json:"side,omitempty"`
	// Self 表示占用者就是本进程 —— 也就是"本应用自己"。
	//
	// 这是最容易让用户困惑的一种冲突：他配一条转发规则用 18080，
	// 报"已被其它程序占用"，就去满机器找那个"其它程序"——
	// 而那个程序就是本应用自己（18000 段是内置反向代理的端口）。
	Self bool `json:"self,omitempty"`
}

// whoHoldsPort 反查正在监听该端口的进程。查不到时返回 nil。
//
// 为什么直接读 /proc 而不是调 ss / lsof：真机（fnOS）不一定装了这些命令，
// 而本项目一直避免依赖外部可执行文件 —— 一台正闹端口冲突的机器上，
// 最不该假设的就是"诊断工具一定在"。读 /proc 只需要内核在，
// 这也正是 ss 自己做的事。
func whoHoldsPort(port int, proto string) []Holder {
	files := []struct{ path, side string }{
		{"/proc/net/tcp", "IPv4"},
		{"/proc/net/tcp6", "IPv6"},
	}
	if proto == "udp" {
		files[0].path, files[1].path = "/proc/net/udp", "/proc/net/udp6"
	}

	// inode → 协议栈侧。同一个 inode 只会出现在一侧。
	sideOf := make(map[string]string, 2)
	for _, f := range files {
		data, err := os.ReadFile(f.path)
		if err != nil {
			continue
		}
		for _, ino := range parseListenInodes(string(data), port, proto) {
			sideOf[ino] = f.side
		}
	}
	if len(sideOf) == 0 {
		return nil
	}

	self := os.Getpid()
	var out []Holder
	for _, pid := range procPIDs() {
		for ino, side := range sideOf {
			if !processHoldsSocket(pid, ino) {
				continue
			}
			out = append(out, Holder{
				PID:  pid,
				Name: processName(pid),
				Side: side,
				Self: pid == self,
			})
			break
		}
	}
	return out
}

// procPIDs 列出当前所有数字命名的 /proc 子目录。
func procPIDs() []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	out := make([]int, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		out = append(out, pid)
	}
	return out
}

// processHoldsSocket 判断该进程是否持有指定 inode 的 socket。
func processHoldsSocket(pid int, inode string) bool {
	fdDir := "/proc/" + strconv.Itoa(pid) + "/fd"
	entries, err := os.ReadDir(fdDir)
	if err != nil {
		// 进程刚好退出，或没有权限（本应用以 root 运行，后者基本不会发生）。
		return false
	}
	for _, e := range entries {
		link, err := os.Readlink(fdDir + "/" + e.Name())
		if err != nil {
			continue
		}
		if got, ok := socketInodeOfLink(link); ok && got == inode {
			return true
		}
	}
	return false
}

// processName 读进程名；失败返回空串（调用方会显示"未知进程"）。
func processName(pid int) string {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/comm")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
