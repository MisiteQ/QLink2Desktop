//go:build !linux

package forward

// Holder 描述一个正在占用某端口的进程。
//
// 非 Linux 平台上是空结构：本项目的运行环境（飞牛 fnOS）只有 Linux，
// 开发机上不需要再维护一套 GetExtendedTcpTable / sysctl 的实现。
// 与其写一个没人验证过的近似版本，不如明确地"查不到"。
type Holder struct {
	PID  int    `json:"pid"`
	Name string `json:"name,omitempty"`
	Side string `json:"side,omitempty"`
	Self bool   `json:"self,omitempty"`
}

// whoHoldsPort 在非 Linux 平台上不做反查，直接返回 nil。
//
// 调用方据此回落成中性文案（"请换一个端口或先停掉占用它的服务"），
// 而不是显示一条半真半假的诊断结果。
func whoHoldsPort(port int, proto string) []Holder { return nil }
