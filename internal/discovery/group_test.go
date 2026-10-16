package discovery

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// 这一组测试盯的是用户报的那个现象：
// 「为什么扫描出来的端口是重复的，有 0.0.0.0 的有不带的」。
//
// 全部是纯函数：/proc/net 的解析与合并都不依赖真的跑在 Linux 上。
// 原项目把"读文件"和"解析"揉在一起，导致这段最容易出错的代码长期无法测试。

// --- 通配地址的判据 ---------------------------------------------------------

func TestIsWildcardAddress(t *testing.T) {
	cases := map[string]bool{
		"0.0.0.0":         true, // net/tcp 里的 IPv4 通配
		"::":              true, // net/tcp6 里的 IPv6 通配（Go 打印成 ::）
		"0:0:0:0:0:0:0:0": true,
		"127.0.0.1":       false,
		"::1":             false,
		"192.168.31.80":   false,
		"172.17.0.1":      false,
		"":                false, // 解析不出来的空串不能当成"所有网卡"
		"垃圾":              false,
	}
	for addr, want := range cases {
		if got := IsWildcardAddress(addr); got != want {
			t.Errorf("IsWildcardAddress(%q) = %v，期望 %v", addr, got, want)
		}
	}
}

func TestClassifyScope(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want AddressScope
	}{
		{"v4 通配 → 所有网卡", []string{"0.0.0.0"}, ScopeAll},
		{"v6 通配 → 所有网卡", []string{"::"}, ScopeAll},
		{"v4+v6 通配 → 所有网卡", []string{"0.0.0.0", "::"}, ScopeAll},
		{"通配与具体地址混在一起仍是所有网卡", []string{"0.0.0.0", "192.168.31.80"}, ScopeAll},
		{"只绑回环 → 仅本机", []string{"127.0.0.1"}, ScopeLocal},
		{"v4+v6 回环 → 仅本机", []string{"127.0.0.1", "::1"}, ScopeLocal},
		{"绑在网卡地址上", []string{"192.168.31.80"}, ScopeSpecific},
		{"回环 + 网卡地址 = 明确指定", []string{"127.0.0.1", "192.168.31.80"}, ScopeSpecific},
		{"空列表不冒充所有网卡", nil, ScopeSpecific},
	}
	for _, c := range cases {
		if got := ClassifyScope(c.in); got != c.want {
			t.Errorf("%s：ClassifyScope(%v) = %q，期望 %q", c.name, c.in, got, c.want)
		}
	}
}

// --- 合并 -------------------------------------------------------------------

// TestGroupPortsMergesAddressFamilies 是用户截图里最直观的一对：
// 同一台机器上 sshd 在 net/tcp 与 net/tcp6 各有一份，界面上却是两行。
func TestGroupPortsMergesAddressFamilies(t *testing.T) {
	out := GroupPorts([]PortInfo{
		{Port: 22, Proto: ProtoTCP, Address: "0.0.0.0", Process: "sshd", PID: 100, User: "0"},
		{Port: 22, Proto: ProtoTCP, Address: "::", Process: "sshd", PID: 100, User: "0"},
	})

	if len(out) != 1 {
		t.Fatalf("0.0.0.0 与 :: 是同一个监听的两个副本，应合并成 1 行，实际 %d 行：%+v", len(out), out)
	}
	got := out[0]
	if !reflect.DeepEqual(got.Addresses, []string{"0.0.0.0", "::"}) {
		t.Errorf("完整绑定地址必须原样带出去，实际 %v", got.Addresses)
	}
	if got.Scope != ScopeAll {
		t.Errorf("Scope = %q，期望 %q", got.Scope, ScopeAll)
	}
	if got.Address != "0.0.0.0" {
		t.Errorf("代表地址 = %q，期望通配的 0.0.0.0 优先", got.Address)
	}
}

// TestGroupPortsRepeatedAddressCountsOnce：同一个地址重复出现（例如解析出两行）
// 不能变成两个条目，否则"合并了但还是在重复"。
func TestGroupPortsRepeatedAddressCountsOnce(t *testing.T) {
	out := GroupPorts([]PortInfo{
		{Port: 22, Proto: ProtoTCP, Address: "::", Process: "sshd"},
		{Port: 22, Proto: ProtoTCP, Address: "::", Process: "sshd"},
	})
	if len(out) != 1 || !reflect.DeepEqual(out[0].Addresses, []string{"::"}) {
		t.Fatalf("重复地址应去重，实际 %+v", out)
	}
}

// TestGroupPortsMergesTCPAndUDP 是另一半现象：rpcbind 同时占 tcp 与 udp，
// 在界面上是两条**一模一样**的行 —— 因为协议当时只在第二行为空时才显示。
func TestGroupPortsMergesTCPAndUDP(t *testing.T) {
	out := GroupPorts([]PortInfo{
		{Port: 111, Proto: ProtoTCP, Address: "0.0.0.0", Process: "rpcbind"},
		{Port: 111, Proto: ProtoUDP, Address: "0.0.0.0", Process: "rpcbind"},
		{Port: 111, Proto: ProtoTCP, Address: "::", Process: "rpcbind"},
		{Port: 111, Proto: ProtoUDP, Address: "::", Process: "rpcbind"},
	})

	if len(out) != 1 {
		t.Fatalf("tcp/udp × IPv4/IPv6 四份是同一个服务，应合并成 1 行，实际 %d 行", len(out))
	}
	got := out[0]
	if !reflect.DeepEqual(got.Protos, []Proto{ProtoTCP, ProtoUDP}) {
		t.Errorf("协议集合顺序必须稳定为 tcp → udp，实际 %v", got.Protos)
	}
	if got.Proto != ProtoTCP {
		t.Errorf("主协议应为 tcp，实际 %q", got.Proto)
	}
	if len(got.Addresses) != 2 {
		t.Errorf("应带上两个绑定地址，实际 %v", got.Addresses)
	}
}

// TestGroupPortsProtosOrderIsStable：输入顺序颠倒，输出顺序不能跟着变。
// 不稳定的顺序会让前端拼出来的标签（TCP+UDP）在每次刷新时来回跳。
func TestGroupPortsProtosOrderIsStable(t *testing.T) {
	a := GroupPorts([]PortInfo{
		{Port: 111, Proto: ProtoUDP, Address: "0.0.0.0", Process: "rpcbind"},
		{Port: 111, Proto: ProtoTCP, Address: "0.0.0.0", Process: "rpcbind"},
	})
	b := GroupPorts([]PortInfo{
		{Port: 111, Proto: ProtoTCP, Address: "0.0.0.0", Process: "rpcbind"},
		{Port: 111, Proto: ProtoUDP, Address: "0.0.0.0", Process: "rpcbind"},
	})
	if !reflect.DeepEqual(a[0].Protos, b[0].Protos) {
		t.Fatalf("协议顺序随输入变化：%v vs %v", a[0].Protos, b[0].Protos)
	}
}

// TestGroupPortsKeepsDistinctServicesApart：不能为了"少几行"把不同服务并掉。
func TestGroupPortsKeepsDistinctServicesApart(t *testing.T) {
	out := GroupPorts([]PortInfo{
		{Port: 80, Proto: ProtoTCP, Address: "0.0.0.0", Process: "nginx"},
		{Port: 80, Proto: ProtoTCP, Address: "::", Process: "caddy"},
	})
	if len(out) != 2 {
		t.Fatalf("不同进程占同一个端口必须分开，实际 %d 行", len(out))
	}

	// 只靠端口区分不了进程时，靠 PID。
	out = GroupPorts([]PortInfo{
		{Port: 53, Proto: ProtoUDP, Address: "0.0.0.0", PID: 11},
		{Port: 53, Proto: ProtoUDP, Address: "::", PID: 22},
	})
	if len(out) != 2 {
		t.Fatalf("进程名都读不到时应退回 PID 区分，实际 %d 行", len(out))
	}

	// 容器映射与宿主机自己的服务占了同一个端口号，也不能并：
	// 它们的进程名不同（容器一侧是 docker-proxy），键因此不同。
	out = GroupPorts([]PortInfo{
		{Port: 8080, Proto: ProtoTCP, Address: "0.0.0.0", Process: "docker-proxy",
			Container: &ContainerInfo{ID: "abc", Name: "app"}},
		{Port: 8080, Proto: ProtoTCP, Address: "0.0.0.0", Process: "nginx"},
	})
	if len(out) != 2 {
		t.Fatalf("容器映射与宿主机服务同号时必须分开，实际 %d 行", len(out))
	}
}

// TestGroupPortsDoesNotGuessUnknownOwner：进程名读不到时**不猜**。
//
// 写这一条是因为上一版的实现里有一段"让后到的套接字补上前一个的空缺"，
// 而这在本实现里永远不执行 —— 进程名不同就进不了同一个键。
// 与其留一段看着像在容错、实际一次都跑不到的分支，不如把真实行为钉住：
// 读不到归属的套接字自己成一行，不会被塞进别人的行里冒充。
//
// 猜错的代价不对称：把两个不同的服务并成一行，用户以为只有一个；
// 分开显示顶多多一行。宁可多一行。
func TestGroupPortsDoesNotGuessUnknownOwner(t *testing.T) {
	out := GroupPorts([]PortInfo{
		{Port: 445, Proto: ProtoTCP, Address: "0.0.0.0", Process: "smbd", PID: 7},
		{Port: 445, Proto: ProtoTCP, Address: "::"}, // 归属读不到
	})
	if len(out) != 2 {
		t.Fatalf("来源不明的套接字不能被并进别人的行，实际 %d 行：%+v", len(out), out)
	}
	for _, p := range out {
		if p.Process == "smbd" && len(p.Addresses) != 1 {
			t.Errorf("两个套接字被并成一行，说明做了猜测：%+v", p)
		}
	}
}

// TestGroupPortsKeepsInformationFromAnyMember：容器信息与桌面标记是**外部注入**
// 到某个套接字上的，不能因为"第一个恰好没有"就丢掉。
func TestGroupPortsKeepsInformationFromAnyMember(t *testing.T) {
	out := GroupPorts([]PortInfo{
		{Port: 445, Proto: ProtoTCP, Address: "0.0.0.0", Process: "docker-proxy", PID: 7},
		{Port: 445, Proto: ProtoUDP, Address: "::", Process: "docker-proxy", PID: 7,
			Container: &ContainerInfo{ID: "abc", Name: "files", Image: "img/files"},
			Managed:   true},
	})
	if len(out) != 1 {
		t.Fatalf("应合并成 1 行，实际 %d 行", len(out))
	}
	got := out[0]
	if got.Container == nil || got.Container.Name != "files" {
		t.Errorf("容器信息只贴在其中一个套接字上，合并后不能丢：%+v", got.Container)
	}
	if got.Container != nil && got.Container.Image == "" {
		t.Error("镜像名丢了，界面第二行会变成空白")
	}
	if !got.Managed {
		t.Error("任意一个绑定已在桌面上就该是本行已在桌面")
	}
}

func TestGroupPortsCarriesLabelHint(t *testing.T) {
	out := GroupPorts([]PortInfo{
		{Port: 5003, Proto: ProtoTCP, Address: "0.0.0.0", Process: "docker-proxy", PID: 9},
		{Port: 5003, Proto: ProtoUDP, Address: "::", Process: "docker-proxy", PID: 9,
			Container: &ContainerInfo{ID: "c1", Name: "kspeeder", Image: "registry/kspeeder"},
			LabelHint: &LabelHint{Title: "加速器", ContainerName: "kspeeder"}},
	})
	if len(out) != 1 {
		t.Fatalf("应合并成 1 行，实际 %d 行", len(out))
	}
	if out[0].LabelHint == nil {
		t.Error("桌面标签提示丢了，容器一键启用会消失")
	}
	if out[0].Proto != ProtoTCP || len(out[0].Protos) != 2 {
		t.Errorf("主协议应为 tcp、且两个协议都要留着，实际 %q / %v", out[0].Proto, out[0].Protos)
	}
}

// TestGroupPortsMatchesScreenshot 复刻用户截图里的那一段（rpcbind 四条、
// sshd 两条、nmbd 两条），把"重复"收敛成几行。
func TestGroupPortsMatchesScreenshot(t *testing.T) {
	var in []PortInfo
	for _, p := range []struct {
		port int
		name string
	}{
		{22, "sshd"}, {80, "nginx"}, {111, "rpcbind"}, {137, "nmbd"},
	} {
		for _, addr := range []string{"0.0.0.0", "::"} {
			for _, proto := range []Proto{ProtoTCP, ProtoUDP} {
				in = append(in, PortInfo{Port: p.port, Proto: proto, Address: addr, Process: p.name})
			}
		}
	}
	// 16 个套接字，但只有 4 个服务。
	if len(in) != 16 {
		t.Fatalf("夹具构造错了：%d", len(in))
	}
	out := GroupPorts(in)
	if len(out) != 4 {
		t.Fatalf("16 个套接字应合并成 4 行，实际 %d 行：%+v", len(out), out)
	}
	for _, p := range out {
		if len(p.Addresses) != 2 || len(p.Protos) != 2 {
			t.Errorf("端口 %d 应带 2 个地址与 2 个协议，实际 %v / %v", p.Port, p.Addresses, p.Protos)
		}
	}
}

func TestGroupPortsEmptyInput(t *testing.T) {
	if got := GroupPorts(nil); got == nil || len(got) != 0 {
		t.Fatalf("空输入要返回空切片（不能是 nil：JSON 会变成 null 而不是 []），实际 %#v", got)
	}
}

// TestPortInfoJSONCarriesGroupedFields 守的是**上线那一刻**：
// 合并出来的地址、协议、可见范围必须真的到得了前端。
//
// 字段加在结构体上、但 json tag 写错（或 omitempty 把"恰好是零值"吃掉），
// 表现是界面上一片空白而所有单测照样全绿 —— 前端读的就是这几个名字。
func TestPortInfoJSONCarriesGroupedFields(t *testing.T) {
	b, err := json.Marshal(GroupPorts([]PortInfo{
		{Port: 22, Proto: ProtoTCP, Address: "0.0.0.0", Process: "sshd"},
		{Port: 22, Proto: ProtoUDP, Address: "::", Process: "sshd"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, want := range []string{
		`"protos":["tcp","udp"]`,
		`"scope":"all"`,
		`"addresses":["0.0.0.0","::"]`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("前端读的就是这几个字段名，缺了它界面只会显示空白：期望包含 %s\n实际 %s", want, got)
		}
	}

	// 回环只有一个：scope 必须如实说"仅本机"，不能一律 all。
	b, err = json.Marshal(GroupPorts([]PortInfo{
		{Port: 631, Proto: ProtoTCP, Address: "127.0.0.1", Process: "cupsd"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"scope":"local"`) {
		t.Errorf("只绑回环的服务应标为 local，实际 %s", b)
	}
}
