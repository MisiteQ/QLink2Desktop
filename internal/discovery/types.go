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

// PortInfo 描述一个被监听的服务。
//
// 注意这里的粒度和内核表不同：内核按**套接字**记账，一个服务往往占好几个
// （IPv4/IPv6 通配各一份、tcp/udp 各一份、每个网卡地址各一份）。
// 出口给前端之前会先过一遍 GroupPorts，把它们收成一行 ——
// 所以 Port / Proto / Address 是"代表值"，完整信息在 Protos / Addresses。
type PortInfo struct {
	Port    int    `json:"port"`
	Proto   Proto  `json:"proto"`
	Address string `json:"address,omitempty"` // 绑定地址，如 0.0.0.0 / 127.0.0.1
	Process string `json:"process,omitempty"` // 进程名
	PID     int    `json:"pid,omitempty"`
	User    string `json:"user,omitempty"`

	// Addresses 是同一个服务绑定的全部地址，已去重并排序。
	//
	// 合并后仍要把完整名单带出去：用户据此才能看出"这个服务到底对谁可见"。
	Addresses []string `json:"addresses,omitempty"`
	// Protos 是同一个服务监听的协议集合，顺序固定 tcp → udp。
	//
	// 它必须单独存在、并且必须被界面显示出来：早先协议只在第二行为空时
	// 才当兜底显示，于是同地址同端口的一对 tcp / udp 套接字渲染成了两条
	// 一模一样的行，用户看到的就是"扫描出来的端口是重复的"。
	Protos []Proto `json:"protos,omitempty"`
	// Scope 说明这些地址的可见范围（所有网卡 / 仅本机 / 指定网卡）。
	// 判断在 GroupPorts 里做一次，前端只负责翻译成人话。
	Scope AddressScope `json:"scope,omitempty"`

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
