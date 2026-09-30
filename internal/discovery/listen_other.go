//go:build !linux

package discovery

// collectListening 在非 Linux 平台无法读取内核套接字表。
//
// 这里刻意返回空而不是「尝试连接常用端口」：
// 后者既慢又不可靠（防火墙、只绑定特定网卡的端口都会被漏掉或误报），
// 与其给出一份似是而非的列表，不如明确地什么都不返回，由前端提示
// 「当前平台不支持本机端口发现」。
//
// 这个分支的主要用途是让开发者能在 Windows / macOS 上编译并调试
// 前端与 API 层。
func collectListening(string) []rawSocket { return nil }

type socketOwner struct {
	PID  int
	Name string
	User string
}

func collectSocketOwners(string) map[string]socketOwner { return map[string]socketOwner{} }
