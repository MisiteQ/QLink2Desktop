package domain

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// IconSource 说明图标从哪里来。
type IconSource string

const (
	IconAuto   IconSource = ""       // 交给系统按镜像 / 容器自动匹配官方图标
	IconText   IconSource = "text"   // 由文字 + 配色现场生成
	IconURL    IconSource = "url"    // 远端 URL
	IconUpload IconSource = "upload" // 用户上传，落盘为 icons/<file>
)

// Icon 描述一个桌面图标的完整来源信息。
// 集中成一个结构体，替代早期版本里散落的 Icon / IconType / IconText / IconTextColor / IconBgColor 五个平行字段。
type Icon struct {
	Source    IconSource `json:"source,omitempty"`
	Ref       string     `json:"ref,omitempty"`        // URL、文件名或 dataURI
	Text      string     `json:"text,omitempty"`       // Source == IconText 时使用
	TextColor string     `json:"text_color,omitempty"` // #rrggbb
	BgColor   string     `json:"bg_color,omitempty"`   // #rrggbb
}

// IsAuto 报告该图标是否交回系统自动匹配。
func (i Icon) IsAuto() bool { return i.Source == IconAuto || strings.TrimSpace(i.Ref) == "" }

// Container 记录链接背后的 Docker 容器信息（可选）。
type Container struct {
	Name    string `json:"name,omitempty"`
	ID      string `json:"id,omitempty"`
	Image   string `json:"image,omitempty"`
	Service string `json:"service,omitempty"`
}

// Link 是一条「放到桌面」的链接定义——即用户意图本身。
//
// 重要的架构决策：Link 只承载**定义**，不承载**运行态**。
// 应用是否已注册、是否在运行、代理是否活着，全部放在 Status 里由注册器与代理管理器回报。
// 早期版本把两者混在同一个结构体里，导致每次刷新状态都要写一遍 JSON，也容易把过期状态持久化下来。
type Link struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Desc    string `json:"desc,omitempty"`
	Kind    Kind   `json:"kind"`
	AppName string `json:"app_name,omitempty"` // 飞牛包标识，空则由命名规则派生

	// 目标地址。KindShortcut 时 Host/Port 留空，URL 直接放在 Path。
	Scheme string `json:"scheme,omitempty"` // http | https
	Host   string `json:"host,omitempty"`   // 127.0.0.1 / 192.168.1.10 / example.com
	Port   int    `json:"port,omitempty"`
	Path   string `json:"path,omitempty"` // 访问路径，或 KindShortcut 的完整 URL

	Container Container `json:"container,omitempty"`

	// 展示属性
	UI        UIType   `json:"ui"`
	AllUsers  bool     `json:"all_users"`
	NoDisplay bool     `json:"no_display,omitempty"`
	Icon      Icon     `json:"icon,omitempty"`
	FileTypes []string `json:"file_types,omitempty"`

	// 行为属性
	SkipTLSVerify bool `json:"skip_tls_verify,omitempty"`

	// 生命周期
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Scheme 的规范化默认值。
const (
	SchemeHTTP  = "http"
	SchemeHTTPS = "https"
)

// Normalize 就地补齐默认值，保证进入存储层之前 Link 已是自洽状态。
func (l *Link) Normalize() {
	l.Name = strings.TrimSpace(l.Name)
	l.Desc = strings.TrimSpace(l.Desc)
	l.Host = strings.TrimSpace(l.Host)
	l.Path = strings.TrimSpace(l.Path)

	l.UI = l.UI.Normalize()
	if l.Scheme == "" {
		l.Scheme = SchemeHTTP
	}
	l.Scheme = strings.ToLower(l.Scheme)

	if l.Kind == KindLocalPort {
		// 本机端口只可能指向本机回环地址，前端也不允许改。
		l.Host = "localhost"
	}
	if l.Path == "" && l.Kind != KindShortcut {
		l.Path = "/"
	}
	if !strings.HasPrefix(l.Path, "/") && l.Kind != KindShortcut {
		l.Path = "/" + l.Path
	}
}

// Validate 校验 Link 是否自洽；返回的错误均可用 errors.Is(err, ErrValidation) 识别。
func (l Link) Validate() error {
	if l.Name == "" {
		return fmt.Errorf("%w: 名称不能为空", ErrValidation)
	}
	if len([]rune(l.Name)) > 64 {
		return fmt.Errorf("%w: 名称过长（最多 64 字符）", ErrValidation)
	}
	if !l.Kind.Valid() {
		return fmt.Errorf("%w: 未知的链接形态 %q", ErrValidation, l.Kind)
	}

	switch l.Kind {
	case KindShortcut:
		if _, err := url.Parse(l.ShortcutURL()); err != nil {
			return fmt.Errorf("%w: 无效的网址 %q", ErrValidation, l.Path)
		}
		if l.ShortcutURL() == "" {
			return fmt.Errorf("%w: 快捷方式必须填写网址", ErrValidation)
		}
	case KindLocalPort, KindProxy:
		if l.Port <= 0 || l.Port > 65535 {
			return fmt.Errorf("%w: 端口 %d 越界（1-65535）", ErrValidation, l.Port)
		}
		if l.Kind == KindProxy && l.Host == "" {
			return fmt.Errorf("%w: 端口映射必须填写目标主机", ErrValidation)
		}
		if l.Scheme != SchemeHTTP && l.Scheme != SchemeHTTPS {
			return fmt.Errorf("%w: 协议只能是 http 或 https", ErrValidation)
		}
	}

	if l.Icon.Source == IconText && strings.TrimSpace(l.Icon.Text) == "" {
		return fmt.Errorf("%w: 文字图标必须填写文字内容", ErrValidation)
	}
	if l.AppName != "" {
		if err := ValidateAppName(l.AppName); err != nil {
			return err
		}
	}
	return nil
}

// ShortcutURL 返回快捷方式的完整目标地址（自动补协议）。
func (l Link) ShortcutURL() string {
	raw := strings.TrimSpace(l.Path)
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		return raw
	}
	return SchemeHTTPS + "://" + raw
}

// BackendURL 返回反向代理应当转发到的后端地址。
// 仅对 KindProxy 有意义；本机端口形态直接走 127.0.0.1。
func (l Link) BackendURL() string {
	host := l.Host
	if l.Kind == KindLocalPort {
		host = "127.0.0.1"
	}
	if host == "" {
		return ""
	}
	if l.Port <= 0 {
		return fmt.Sprintf("%s://%s%s", l.Scheme, host, l.Path)
	}
	return fmt.Sprintf("%s://%s:%d%s", l.Scheme, host, l.Port, l.Path)
}

// DesktopURL 返回写进飞牛桌面 ui/config 里的 url 字段。
//
// 注意：桌面图标指向的永远是**本机**地址。对 KindProxy 来说这里应当传入代理端口
// （由调用方通过 Rebind 覆盖 Port 后再调用），而不是远端真实地址——否则桌面图标
// 会绕过内置代理，自签证书跳过就会失效。
func (l Link) DesktopURL() string {
	return fmt.Sprintf("%s://localhost:%d%s", l.Scheme, l.Port, l.Path)
}

// WithPort 返回把端口改写为 port 的副本，常用于「代理端口覆盖真实端口」的场景。
func (l Link) WithPort(port int) Link {
	l.Port = port
	return l
}

// Key 返回用于日志与去重的可读标识。
func (l Link) Key() string {
	if l.AppName != "" {
		return l.AppName
	}
	return l.ID
}

// EffectiveAppName 返回该链接最终使用的飞牛包标识。
func (l Link) EffectiveAppName() string {
	if l.AppName != "" {
		return l.AppName
	}
	return DeriveAppName(l)
}

// 规格上限，对应飞牛包名规范。
const (
	MinAppNameLength = 3
	MaxAppNameLength = 32
	// AppPrefix 是本项目创建的桌面子应用的专属命名空间。
	// 所有卸载 / 清理逻辑都必须严格限定在该前缀内，绝不触碰第三方原生应用。
	AppPrefix = "qlink2d."
)

// IsManagedApp 报告 appName 是否由本项目创建（用于安全的批量清理）。
//
// 只认本项目的 qlink2d. 前缀。早期的实现把另外两个前缀
// 也列为"历史遗留前缀"，但那是其它工具的命名空间——
// 用户完全可能同时装着它们并使用其生成的图标，把它们当孤儿清理
// 属于越权删除（真实发生过）。本应用与其它工具没有任何数据血缘，
// 对它们创建的应用既不识别、更不清理。
func IsManagedApp(appName string) bool {
	return strings.HasPrefix(appName, AppPrefix)
}

// DerivedPortName 是 KindLocalPort 派生包名时的兜底片段。
func portFragment(port int) string {
	if port <= 0 {
		return "app"
	}
	return "port-" + strconv.Itoa(port)
}
