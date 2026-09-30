package domain

import (
	"fmt"
	"net"
	"strings"
	"time"
)

// HostAuth 描述连接远端主机的方式。
type HostAuth struct {
	// Password 用于需要交互式密码的 SSH 登录（会原样交给 ssh 客户端）。
	Password string `json:"password,omitempty"`
	// KeyPath 指定私钥路径，留空则使用 ssh 的默认密钥与 ssh-agent。
	KeyPath string `json:"key_path,omitempty"`
	// InsecureSkipHostKey 跳过主机密钥校验。
	// 默认 false；只有用户明确知道风险时才应开启。
	InsecureSkipHostKey bool `json:"insecure_skip_host_key,omitempty"`
}

// Host 是一台可以从本机访问的远端机器。
//
// 用途：当某台机器（另一台 NAS、软路由、树莓派）上跑着服务时，
// 可以通过 SSH 隧道把它的端口映射过来，再放到飞牛桌面。
type Host struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Address string `json:"address"` // IP 或域名
	Port    int    `json:"port"`    // SSH 端口，默认 22

	User string   `json:"user,omitempty"`
	Auth HostAuth `json:"auth,omitempty"`

	// LastOK / LastError 记录最近一次连通性探测结果（只读展示用）。
	LastOK    bool      `json:"last_ok,omitempty"`
	LastError string    `json:"last_error,omitempty"`
	CheckedAt time.Time `json:"checked_at,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Normalize 补齐默认值并清理输入。
func (h *Host) Normalize() {
	h.Name = strings.TrimSpace(h.Name)
	h.Address = strings.TrimSpace(h.Address)
	h.User = strings.TrimSpace(h.User)
	if h.Port <= 0 || h.Port > 65535 {
		h.Port = 22
	}
	if h.Name == "" {
		h.Name = h.Address
	}
}

// Validate 校验主机配置。
func (h Host) Validate() error {
	if h.Address == "" {
		return fmt.Errorf("%w: 主机地址不能为空", ErrValidation)
	}
	if net.ParseIP(h.Address) == nil {
		// 不是 IP 就当作域名，简单校验合法性。
		if !strings.Contains(h.Address, ".") && h.Address != "localhost" {
			return fmt.Errorf("%w: 主机地址 %q 看起来既不是 IP 也不是域名", ErrValidation, h.Address)
		}
	}
	if h.Port <= 0 || h.Port > 65535 {
		return fmt.Errorf("%w: 端口 %d 越界", ErrValidation, h.Port)
	}
	return nil
}

// SSHTarget 返回 ssh 命令使用的目标字符串，如 user@host。
func (h Host) SSHTarget() string {
	if h.User == "" {
		return h.Address
	}
	return h.User + "@" + h.Address
}

// AddressWithPort 返回 host:port 形式。
func (h Host) AddressWithPort() string {
	return net.JoinHostPort(h.Address, fmt.Sprint(h.Port))
}
