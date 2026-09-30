// Package discovery 负责「发现有哪些服务可以放到桌面」。
//
// 三条发现路径：
//
//	ports_*.go   本机正在监听的端口（Linux 走 procfs 内核套接字表）
//	docker.go    Docker 容器及其端口映射（走 docker.sock，零 SDK 依赖）
//	scan.go      远端 / 局域网主机的端口扫描
//	ssh.go       通过宿主机的 ssh 客户端做隧道拨号
//
// 所有发现结果都归一为 PortInfo，由上层决定是否值得放到桌面。
package discovery

import "strings"

// Proto 是传输层协议。
type Proto string

const (
	ProtoTCP Proto = "tcp"
	ProtoUDP Proto = "udp"
)

// PortInfo 描述一个被监听的端口。
type PortInfo struct {
	Port    int    `json:"port"`
	Proto   Proto  `json:"proto"`
	Address string `json:"address,omitempty"` // 绑定地址，如 0.0.0.0 / 127.0.0.1
	Process string `json:"process,omitempty"` // 进程名
	PID     int    `json:"pid,omitempty"`
	User    string `json:"user,omitempty"`

	// Container 非空时表示该端口由 Docker 容器映射出来。
	Container *ContainerInfo `json:"container,omitempty"`
	// Managed 表示该端口已经被本项目放到桌面。
	Managed bool `json:"managed,omitempty"`
	// LabelHint 非空表示容器带有兼容 Watchcow 的标签配置，可直接一键启用。
	LabelHint *LabelHint `json:"label_hint,omitempty"`
}

// ContainerInfo 是容器侧的信息快照。
type ContainerInfo struct {
	ID          string `json:"id,omitempty"`
	Name        string `json:"name,omitempty"`
	Image       string `json:"image,omitempty"`
	Service     string `json:"service,omitempty"`
	Status      string `json:"status,omitempty"`
	PrivatePort int    `json:"private_port,omitempty"`
}

// LabelHint 承载从容器标签里读到的桌面图标配置。
//
// 兼容 Watchcow 生态的标签约定，使用户从 Watchcow 迁移过来时无需重新配置。
type LabelHint struct {
	Title    string `json:"title,omitempty"`
	Icon     string `json:"icon,omitempty"`
	Port     int    `json:"port,omitempty"`
	Scheme   string `json:"scheme,omitempty"`
	Path     string `json:"path,omitempty"`
	AllUsers bool   `json:"all_users,omitempty"`
	// ContainerName 是该标签所属容器名，用于生成稳定的去重键。
	ContainerName string `json:"container_name,omitempty"`
}

// Key 返回用于去重与状态记录的稳定标识。
func (h LabelHint) Key() string {
	name := h.ContainerName
	if name == "" {
		name = h.Title
	}
	return "docklabel-" + strings.ToLower(name)
}

// Display 返回用于列表展示的一行摘要。
func (p PortInfo) Display() string {
	var b strings.Builder
	if p.Process != "" {
		b.WriteString(p.Process)
	}
	if p.Container != nil && p.Container.Name != "" {
		if b.Len() > 0 {
			b.WriteString(" · ")
		}
		b.WriteString(p.Container.Name)
	}
	if p.Address != "" {
		if b.Len() > 0 {
			b.WriteString(" · ")
		}
		b.WriteString(p.Address)
	}
	return b.String()
}

// SortKey 让端口列表按「端口号升序、TCP 优先」稳定输出。
func (p PortInfo) SortKey() (int, int) {
	protoRank := 1
	if p.Proto == ProtoTCP {
		protoRank = 0
	}
	return p.Port, protoRank
}
