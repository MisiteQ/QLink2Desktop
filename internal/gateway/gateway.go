// Package gateway 负责接入飞牛 OS 的统一网关。
//
// 为什么需要这个包：
// 飞牛桌面的应用入口有两种到达方式 ——
//  1. 直接在 ui/config 里声明 protocol + port（占用一个本机端口）；
//  2. 走「统一网关」：应用把 HTTP 服务监听在一个 Unix 域套接字上，
//     网关按路径转发，**零端口占用**，也就不会和其它应用抢端口。
//
// 本包提供第二种方式的两端：
//
//	Listen——把 HTTP 服务挂到 Unix 套接字上（服务端）；
//	RunCGI——以 CGI 进程身份把请求转发回套接字（客户端，供 index.cgi 调用）。
package gateway

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
)

// SocketDirPerm 是套接字所在目录的权限要求。
//
// 网关以另一个用户身份访问套接字，因此目录必须可穿透。
const SocketDirPerm = 0o755

// DefaultSocketName 是套接字文件名。
const DefaultSocketName = "app.sock"

// SocketCandidates 是套接字的候选位置，按「越接近真实运行环境越优先」排列。
//
// 之所以要列多个：不同飞牛版本、不同安装卷布局下，TRIM_APPDEST 可能不可见，
// 而套接字路径又是桌面入口写死的。多列几个能显著提高"装完就能用"的概率。
func SocketCandidates(appName string) []string {
	paths := make([]string, 0, 8)

	if dest := strings.TrimSpace(os.Getenv("TRIM_APPDEST")); dest != "" {
		paths = append(paths, filepath.Join(dest, DefaultSocketName))
	}
	paths = append(paths,
		filepath.Join("/var/apps", appName, "target", DefaultSocketName),
		filepath.Join("/usr/local/apps/@appcenter", appName, DefaultSocketName),
	)
	if dest := strings.TrimSpace(os.Getenv("TRIM_PKGVAR")); dest != "" {
		paths = append(paths, filepath.Join(dest, DefaultSocketName))
	}
	paths = append(paths, filepath.Join(os.TempDir(), appName+".sock"))
	return paths
}

// ResolveSocket 选出应当使用的套接字路径：优先显式指定，其次已存在可用的候选，
// 最后回退到候选列表的第一项。
func ResolveSocket(explicit, appName string) string {
	if p := strings.TrimSpace(explicit); p != "" {
		return p
	}
	for _, p := range SocketCandidates(appName) {
		if IsSocket(p) {
			return p
		}
	}
	candidates := SocketCandidates(appName)
	if len(candidates) == 0 {
		return filepath.Join(os.TempDir(), appName+".sock")
	}
	// 优先选一个所在目录已经存在的候选，避免"父目录都没建好"的路径。
	for _, p := range candidates {
		if fi, err := os.Stat(filepath.Dir(p)); err == nil && fi.IsDir() {
			return p
		}
	}
	return candidates[0]
}

// IsSocket 报告路径是否是一个可用的套接字（或普通文件，兼容部分环境用文件占位）。
func IsSocket(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeSocket != 0 || fi.Mode().IsRegular()
}

// Listen 在 Unix 套接字上建立监听。
//
// 会先删除遗留的套接字文件：进程被 kill -9 后文件会残留，
// 而 net.Listen 遇到已存在的路径会直接报 address already in use，
// 表现为"重启后网关一直 502"。
func Listen(path string) (net.Listener, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, fmt.Errorf("套接字路径为空")
	}
	if err := os.MkdirAll(filepath.Dir(path), SocketDirPerm); err != nil {
		return nil, fmt.Errorf("创建套接字目录失败: %w", err)
	}
	_ = os.Remove(path)

	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("监听 Unix 套接字 %s 失败: %w", path, err)
	}
	// 网关可能以其它用户 / 组身份运行，套接字必须放开读写。
	if err := os.Chmod(path, 0o666); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("设置套接字权限失败: %w", err)
	}
	return ln, nil
}

// Remove 清理套接字文件（进程退出时调用）。
func Remove(path string) {
	if strings.TrimSpace(path) == "" {
		return
	}
	_ = os.Remove(path)
}

// StripAppPrefix 剥掉网关可能加上的应用路径前缀。
//
// 网关在部分版本上会把 /app/<appname> 拼在原始路径前面再转发，
// 如果服务端不剥掉，所有路由都会 404，表现为"页面能打开但接口全挂"。
func StripAppPrefix(path, appName string, aliases []string) string {
	names := append([]string{appName}, aliases...)
	for _, name := range names {
		if strings.TrimSpace(name) == "" {
			continue
		}
		prefix := "/app/" + name
		if path == prefix {
			return "/"
		}
		if strings.HasPrefix(path, prefix+"/") {
			return strings.TrimPrefix(path, prefix)
		}
	}
	return path
}
