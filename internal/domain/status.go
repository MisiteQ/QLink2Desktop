package domain

// Phase 是桌面图标在飞牛应用中心里的注册阶段。
type Phase string

const (
	PhaseUnknown    Phase = "unknown"    // 尚未探测
	PhasePending    Phase = "pending"    // 排队等待安装
	PhaseInstalling Phase = "installing" // 正在安装 / 恢复
	PhaseUpgrading  Phase = "upgrading"  // 检测到配置过期，正在重装
	PhaseInstalled  Phase = "installed"  // 已注册且运行中
	PhaseStopped    Phase = "stopped"    // 已注册但被停用
	PhaseFailed     Phase = "failed"     // 最后一次操作失败
)

// Chinese 返回面向用户的中文描述。
func (p Phase) Chinese() string {
	switch p {
	case PhasePending:
		return "排队中"
	case PhaseInstalling:
		return "恢复中"
	case PhaseUpgrading:
		return "升级中"
	case PhaseInstalled:
		return "已就绪"
	case PhaseStopped:
		return "已停用"
	case PhaseFailed:
		return "失败"
	default:
		return "未知"
	}
}

// Busy 报告该阶段是否处于「进行中」，前端据此显示骨架 / 转圈。
func (p Phase) Busy() bool {
	return p == PhasePending || p == PhaseInstalling || p == PhaseUpgrading
}

// Status 是链接的**运行态快照**，由各子系统（注册器 / 代理管理器）回报。
//
// 与 Link 分离是这个重构最重要的可维护性改进之一：
// 运行态不写盘、每次查询实时合成，因此永远不会把「上次的临时状态」当成事实持久化下来。
type Status struct {
	LinkID  string `json:"link_id"`
	AppName string `json:"app_name,omitempty"`

	Phase     Phase  `json:"phase"`
	Detail    string `json:"detail,omitempty"`     // 阶段补充说明（如 "3/8"）
	LastError string `json:"last_error,omitempty"` // 最近一次失败原因

	ProxyPort   int  `json:"proxy_port,omitempty"` // 内置反向代理实际监听端口，0 表示未启用
	ProxyActive bool `json:"proxy_active"`         // 代理是否正在监听
	Healthy     bool `json:"healthy"`              // 后端连通性（由探测填充，可为零值）
}

// View 是返回给前端的只读聚合视图：定义 + 运行态。
type View struct {
	Link   Link   `json:"link"`
	Status Status `json:"status"`
}

// NewView 组装一个视图，自动补齐 LinkID / AppName 字段。
func NewView(l Link, s Status) View {
	s.LinkID = l.ID
	if s.AppName == "" {
		s.AppName = l.EffectiveAppName()
	}
	return View{Link: l, Status: s}
}
