package domain

import "fmt"

// Kind 描述一个链接（Link）以何种方式被放到飞牛桌面上。
//
// 三种形态对应三种完全不同的落地策略，因此把它显式建模为类型，
// 而不是像早期版本那样用 "mode" 字符串在十多个 if 分支里反复判断。
type Kind string

const (
	// KindLocalPort：服务已经在本机某个端口上跑着（例如 Docker 映射出来的端口）。
	// 桌面图标直接指向 127.0.0.1:<port>，不经过内置代理。
	KindLocalPort Kind = "local"

	// KindProxy：服务在别的机器 / 公网。由本机反向代理成一个本地端口后再上桌面，
	// 从而可以借用飞牛 Connect 的外网穿透能力。
	KindProxy Kind = "proxy"

	// KindShortcut：纯粹的网址快捷方式，不占用任何本地端口。
	KindShortcut Kind = "shortcut"
)

// AllKinds 按 UI 展示顺序枚举全部形态。
var AllKinds = []Kind{KindLocalPort, KindProxy, KindShortcut}

// Valid 报告 k 是否为已定义的形态。
func (k Kind) Valid() bool {
	switch k {
	case KindLocalPort, KindProxy, KindShortcut:
		return true
	default:
		return false
	}
}

// NeedsProxy 报告该形态是否需要由本机拉起一个反向代理监听。
func (k Kind) NeedsProxy() bool {
	return k == KindProxy
}

// NeedsHostPort 报告该形态是否必须声明一个可达的主机端口。
func (k Kind) NeedsHostPort() bool {
	return k == KindLocalPort || k == KindProxy
}

// ParseKind 解析来自 API / 配置文件的形态字符串。
func ParseKind(s string) (Kind, error) {
	k := Kind(s)
	if !k.Valid() {
		return "", fmt.Errorf("%w: 未知的链接形态 %q (可选: local / proxy / shortcut)", ErrValidation, s)
	}
	return k, nil
}

// UIType 决定飞牛桌面打开图标时的窗口形态。
type UIType string

const (
	// UIWindow：在飞牛桌面内部以 iframe 弹窗打开，体验最接近原生应用。
	UIWindow UIType = "iframe"
	// UITab：新开浏览器标签页打开。
	UITab UIType = "url"
)

// Normalize 把空值或非法值收敛为默认的弹窗形态。
func (u UIType) Normalize() UIType {
	if u == UITab {
		return UITab
	}
	return UIWindow
}

// String 实现 fmt.Stringer。
func (u UIType) String() string { return string(u.Normalize()) }
