package domain

import "strings"

// 默认端口与名称。
const (
	DefaultPortalPort = 5900
	DefaultPortalName = "QLink2Desktop"
)

// Settings 是应用自身的全局配置。
//
// 相比早期版本有两处安全性改进：
//  1. 口令不再明文落盘，改用 salt + sha256 摘要（AuthHash / AuthSalt）。
//  2. 图标收敛为 Icon 值对象，与 Link.Icon 共用同一套语义。
type Settings struct {
	PortalPort     int    `json:"portal_port"`
	PortalName     string `json:"portal_name"`
	PortalUI       UIType `json:"portal_ui"`
	PortalAllUsers bool   `json:"portal_all_users"`
	PortalIcon     Icon   `json:"portal_icon,omitempty"`
	AuthHash       string `json:"auth_hash,omitempty"`
	AuthSalt       string `json:"auth_salt,omitempty"`

	// AutoReconcile 控制启动时是否自动补齐 / 恢复桌面图标。
	AutoReconcile bool `json:"auto_reconcile"`

	// IconsDir 是用户指定的图标存放目录。
	//
	// 留空表示"用应用自己的默认值"（数据目录下的 icons/）——刻意不在这里
	// 兜底填一个绝对路径：数据目录的实际位置取决于安装时的卷与 TRIM_PKGVAR，
	// 只有装配层才知道，settings 层不该猜。
	IconsDir string `json:"icons_dir,omitempty"`
}

// DefaultSettings 返回开箱默认配置。
func DefaultSettings() Settings {
	return Settings{
		PortalPort:     DefaultPortalPort,
		PortalName:     DefaultPortalName,
		PortalUI:       UIWindow,
		PortalAllUsers: false,
		PortalIcon: Icon{
			Source:    IconText,
			Text:      "QL",
			TextColor: "#ffffff",
			BgColor:   "#2563eb",
		},
		AutoReconcile: true,
	}
}

// Normalize 补齐非法 / 缺失字段。
func (s *Settings) Normalize() {
	if s.PortalPort <= 0 || s.PortalPort > 65535 {
		s.PortalPort = DefaultPortalPort
	}
	if strings.TrimSpace(s.PortalName) == "" {
		s.PortalName = DefaultPortalName
	}
	s.PortalName = strings.TrimSpace(s.PortalName)
	s.PortalUI = s.PortalUI.Normalize()
	if s.PortalIcon.Source == IconText && strings.TrimSpace(s.PortalIcon.Text) == "" {
		s.PortalIcon.Text = "QL"
	}
}

// HasPassword 报告是否已设置访问口令。
func (s Settings) HasPassword() bool { return s.AuthHash != "" }
