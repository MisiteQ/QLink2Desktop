package discovery

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

/* ---------------------------------------------------------------- 范围解析 */

func TestExpandRangeSpec(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		spec  string
		count int
		first string
		last  string
	}{
		{"三段前缀=整段", "192.168.1", 254, "192.168.1.1", "192.168.1.254"},
		{"单台", "192.168.1.10", 1, "192.168.1.10", "192.168.1.10"},
		{"段内区间", "192.168.1.10-12", 3, "192.168.1.10", "192.168.1.12"},
		{"跨段区间", "192.168.1.254-192.168.2.2", 5, "192.168.1.254", "192.168.2.2"},
		{"CIDR /24", "192.168.1.0/24", 254, "192.168.1.1", "192.168.1.254"},
		{"CIDR /30 去掉网络号与广播", "192.168.1.0/30", 2, "192.168.1.1", "192.168.1.2"},
		{"CIDR /32 就是单台", "192.168.1.10/32", 1, "192.168.1.10", "192.168.1.10"},
		{"主机名", "nas.local", 1, "nas.local", "nas.local"},
		{"带连字符的主机名不能被当成区间", "my-nas.local", 1, "my-nas.local", "my-nas.local"},
		{"粘贴进来的网址会被剥成主机", "https://192.168.1.5:8080/admin", 1, "192.168.1.5", "192.168.1.5"},
		{"中文逗号分隔", "192.168.1.10，192.168.1.11", 2, "192.168.1.10", "192.168.1.11"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ExpandRangeSpec(tc.spec)
			if err != nil {
				t.Fatalf("ExpandRangeSpec(%q) 报错: %v", tc.spec, err)
			}
			if len(got) != tc.count {
				t.Fatalf("ExpandRangeSpec(%q) 得到 %d 个地址，期望 %d（前几个：%v）",
					tc.spec, len(got), tc.count, head(got, 4))
			}
			if got[0] != tc.first || got[len(got)-1] != tc.last {
				t.Fatalf("ExpandRangeSpec(%q) 首尾 = %s..%s，期望 %s..%s",
					tc.spec, got[0], got[len(got)-1], tc.first, tc.last)
			}
		})
	}
}

func TestExpandRangeSpecDedupes(t *testing.T) {
	t.Parallel()

	got, err := ExpandRangeSpec("192.168.1.10, 192.168.1.10, 192.168.1.10-11")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"192.168.1.10", "192.168.1.11"}
	if len(got) != len(want) {
		t.Fatalf("重复地址应被去重，实际 %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("去重后顺序不稳定：%v", got)
		}
	}
}

// TestExpandRangeSpecRejectsBadInput 断言"写错就报错"。
//
// 静默纠错比报错危险得多：用户以为扫了 3 个网段，实际只扫了 1 个，
// 而界面上看不出任何异常。
func TestExpandRangeSpecRejectsBadInput(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"空输入":      "",
		"纯空白":      "   ",
		"乱码":       "asdf***",
		"越界八位组":    "192.168.1.300",
		"区间反了":     "192.168.1.50-10",
		"缺结束地址":    "192.168.1.10-",
		"掩码写错":     "192.168.1.0/99",
		"IPv6 不支持": "2001:db8::/64",
	}

	for name, spec := range cases {
		name, spec := name, spec
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got, err := ExpandRangeSpec(spec); err == nil {
				t.Fatalf("期望报错，实际得到 %d 个地址（%v）", len(got), head(got, 3))
			}
		})
	}
}

// TestExpandRangeSpecRejectsOversizeRange 断言超限是**明确拒绝**而不是悄悄截断。
func TestExpandRangeSpecRejectsOversizeRange(t *testing.T) {
	t.Parallel()

	_, err := ExpandRangeSpec("10.0.0.0/16")
	if err == nil {
		t.Fatal("一个 /16 应该被拒绝")
	}
	if !errors.Is(err, ErrRangeTooLarge) {
		t.Fatalf("错误应可被识别为范围过大，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "65534") {
		t.Errorf("错误信息里应当带上实际地址数，方便用户判断，实际 %q", err.Error())
	}
}

/* ------------------------------------------------------------ 路由表与网段 */

// sampleProcRoute 是真实设备 /proc/net/route 的形状（含默认路由与本地路由）。
const sampleProcRoute = `Iface	Destination	Gateway 	Flags	RefCnt	Use	Metric	Mask		MTU	Window	IRTT
eth0	00000000	0101A8C0	0003	0	0	0	00000000	0	0	0
eth0	0000A8C0	00000000	0001	0	0	0	00FFFFFF	0	0	0
docker0	000011AC	00000000	0001	0	0	0	0000FFFF	0	0	0
`

func TestParseProcRouteAndDefaultRoute(t *testing.T) {
	t.Parallel()

	routes := ParseProcRoute(sampleProcRoute)
	if len(routes) != 3 {
		t.Fatalf("应解析出 3 条路由，实际 %d", len(routes))
	}
	if routes[0].Dest != "0.0.0.0" {
		t.Errorf("默认路由的 Destination 应为 0.0.0.0，实际 %q", routes[0].Dest)
	}
	// 0101A8C0 是小端序的 192.168.1.1 —— 解析错了不会报错，只会得到一个看着像 IP 的错地址。
	if routes[0].Gateway != "192.168.1.1" {
		t.Errorf("网关应为 192.168.1.1，实际 %q", routes[0].Gateway)
	}

	iface, gw := DefaultRouteFrom(routes)
	if iface != "eth0" || gw != "192.168.1.1" {
		t.Fatalf("默认路由 = %q/%q，期望 eth0/192.168.1.1", iface, gw)
	}
}

func TestDefaultRoutePrefersLowestMetric(t *testing.T) {
	t.Parallel()

	routes := []ProcRoute{
		{Iface: "wlan0", Dest: "0.0.0.0", Gateway: "192.168.1.1", Metric: 600, Up: true},
		{Iface: "eth0", Dest: "0.0.0.0", Gateway: "192.168.1.254", Metric: 100, Up: true},
		{Iface: "down0", Dest: "0.0.0.0", Gateway: "192.168.1.9", Metric: 1, Up: false},
	}
	iface, gw := DefaultRouteFrom(routes)
	if iface != "eth0" || gw != "192.168.1.254" {
		t.Fatalf("应取 metric 最小且 UP 的那条，实际 %q/%q", iface, gw)
	}
}

// TestDetectFromPrefersDefaultRouteIface 是这次修复的核心断言。
//
// 真机上的故障形态：列表里排在前面的是 docker0（172.17.0.0/16），
// 于是"扫描局域网"扫的是容器内部网络，一台设备都扫不到。
func TestDetectFromPrefersDefaultRouteIface(t *testing.T) {
	t.Parallel()

	addrs := []ifaceAddrs{
		{Name: "docker0", Virtual: true, IPv4: []net.IP{net.ParseIP("172.17.0.1")}, Mask: []int{16}},
		{Name: "br-1a2b", Virtual: true, IPv4: []net.IP{net.ParseIP("172.18.0.1")}, Mask: []int{16}},
		{Name: "enp1s0", IPv4: []net.IP{net.ParseIP("192.168.31.205")}, Mask: []int{24}},
	}

	info := detectFrom(addrs, "enp1s0", "192.168.31.1")
	if info.Primary != "192.168.31" {
		t.Fatalf("本机网段应取默认路由出口的网段，实际 %q", info.Primary)
	}
	for _, s := range info.Subnets {
		if strings.HasPrefix(s, "172.17") || strings.HasPrefix(s, "172.18") {
			t.Fatalf("容器网段不应出现在候选里：%v", info.Subnets)
		}
	}
	if info.Parent != "" {
		t.Errorf("网关与本机同段时不应推导出上级网段，实际 %q", info.Parent)
	}
}

// TestDetectFromFallsBackToPhysicalWhenNoDefaultRoute 覆盖读不到路由表的环境。
//
// 没有 /proc/net/route（非 Linux、容器里没挂 procfs）时，退化为
// "第一个非虚拟网卡"，而不是把容器网段当成局域网。
func TestDetectFromFallsBackToPhysicalWhenNoDefaultRoute(t *testing.T) {
	t.Parallel()

	addrs := []ifaceAddrs{
		{Name: "docker0", Virtual: true, IPv4: []net.IP{net.ParseIP("172.17.0.1")}, Mask: []int{16}},
		{Name: "eth0", IPv4: []net.IP{net.ParseIP("10.0.0.5")}, Mask: []int{24}},
	}

	info := detectFrom(addrs, "", "")
	if info.Primary != "10.0.0" {
		t.Fatalf("没有默认路由时应取非虚拟网卡，实际 %q（候选 %v）", info.Primary, info.Subnets)
	}
	if len(info.Subnets) != 1 {
		t.Fatalf("容器网段应被排除，实际候选 %v", info.Subnets)
	}
}

// TestDetectFromInfersParentSubnet 覆盖"NAS 在子网里、网关在上一段"的拓扑。
func TestDetectFromInfersParentSubnet(t *testing.T) {
	t.Parallel()

	addrs := []ifaceAddrs{
		{Name: "eth0", IPv4: []net.IP{net.ParseIP("192.168.31.5")}, Mask: []int{24}},
	}
	info := detectFrom(addrs, "eth0", "192.168.1.1")
	if info.Primary != "192.168.31" {
		t.Fatalf("本机网段 = %q", info.Primary)
	}
	if info.Parent != "192.168.1" {
		t.Fatalf("网关在别的网段时应推导出上级网段，实际 %q", info.Parent)
	}
}

// TestDetectFromOnlyVirtualInterfaces 覆盖容器环境：至少要给出提醒，
// 而不是让用户对着一份空结果猜原因。
func TestDetectFromOnlyVirtualInterfaces(t *testing.T) {
	t.Parallel()

	addrs := []ifaceAddrs{
		{Name: "docker0", Virtual: true, IPv4: []net.IP{net.ParseIP("172.17.0.1")}, Mask: []int{16}},
	}
	info := detectFrom(addrs, "", "")
	if info.Primary != "172.17.0" {
		t.Fatalf("没有任何真实网卡时应退回虚拟网卡（/24 前缀），实际 %q", info.Primary)
	}
	if info.Warning == "" {
		t.Error("只探测到虚拟网卡时应当给出提醒")
	}
}

func TestSubnetPrefix(t *testing.T) {
	t.Parallel()

	if got := SubnetPrefix(net.ParseIP("192.168.31.205")); got != "192.168.31" {
		t.Errorf("SubnetPrefix = %q", got)
	}
	if got := SubnetPrefix(net.ParseIP("::1")); got != "" {
		t.Errorf("IPv6 应返回空串，实际 %q", got)
	}
}

/* -------------------------------------------------------------- 请求解析 */

func TestScanRequestResolve(t *testing.T) {
	t.Parallel()

	info := NetworkInfo{Primary: "192.168.31", Parent: "192.168.1", Gateway: "192.168.1.1"}

	t.Run("auto 用本机网段", func(t *testing.T) {
		hosts, label, err := ScanRequest{Mode: "auto"}.Resolve(info)
		if err != nil {
			t.Fatal(err)
		}
		if len(hosts) != 254 || hosts[0] != "192.168.31.1" {
			t.Fatalf("auto 应展开本机网段，实际 %d 台（首个 %s）", len(hosts), hosts[0])
		}
		if !strings.Contains(label, "本机网段") {
			t.Errorf("范围描述应说明用的是哪个网段，实际 %q", label)
		}
	})

	t.Run("parent 用上级网段", func(t *testing.T) {
		hosts, label, err := ScanRequest{Mode: "parent"}.Resolve(info)
		if err != nil {
			t.Fatal(err)
		}
		if len(hosts) != 254 || hosts[0] != "192.168.1.1" {
			t.Fatalf("parent 应展开上级网段，实际 %d 台（首个 %s）", len(hosts), hosts[0])
		}
		if !strings.Contains(label, "上级网段") {
			t.Errorf("范围描述应标明上级网段，实际 %q", label)
		}
	})

	t.Run("没有上级网段时给出可执行的提示", func(t *testing.T) {
		_, _, err := ScanRequest{Mode: "parent"}.Resolve(NetworkInfo{Primary: "192.168.31"})
		if err == nil {
			t.Fatal("没有上级网段时应当报错")
		}
		if !strings.Contains(err.Error(), "自定义") {
			t.Errorf("错误信息应告诉用户可以改用自定义范围，实际 %q", err.Error())
		}
	})

	t.Run("custom 解析范围文本", func(t *testing.T) {
		hosts, label, err := ScanRequest{Mode: "custom", Spec: "192.168.1.10-12"}.Resolve(info)
		if err != nil {
			t.Fatal(err)
		}
		if len(hosts) != 3 {
			t.Fatalf("应有 3 台，实际 %d", len(hosts))
		}
		if !strings.Contains(label, "192.168.1.10-12") {
			t.Errorf("范围描述应回显用户输入，实际 %q", label)
		}
	})

	t.Run("custom 缺范围文本时报错", func(t *testing.T) {
		if _, _, err := (ScanRequest{Mode: "custom"}).Resolve(info); err == nil {
			t.Fatal("custom 模式没填范围应当报错")
		}
	})

	t.Run("没有网段时 auto 报错而不是静默扫空", func(t *testing.T) {
		if _, _, err := (ScanRequest{Mode: "auto"}).Resolve(NetworkInfo{}); err == nil {
			t.Fatal("探测不到网段时应当报错")
		}
	})

	t.Run("兼容显式目标", func(t *testing.T) {
		hosts, _, err := ScanRequest{Hosts: []string{"10.0.0.1", "10.0.0.2"}}.Resolve(info)
		if err != nil {
			t.Fatal(err)
		}
		if len(hosts) != 2 {
			t.Fatalf("显式目标应原样使用，实际 %v", hosts)
		}
	})
}

/* -------------------------------------------------------------- 任务生命周期 */

func TestScanRunnerIdleSnapshot(t *testing.T) {
	t.Parallel()

	runner := NewScanRunner(nil)
	state := runner.Snapshot()
	if state.Running || state.Done {
		t.Fatalf("空闲状态不应是运行中或已完成：%+v", state)
	}
	if state.Items == nil {
		t.Error("Items 应为空切片而不是 null，前端不必为此写分支")
	}
	if runner.Cancel() {
		t.Error("空闲时取消应当返回 false")
	}
}

// TestScanRunnerRunsAndReports 覆盖「开始 → 完成 → 快照可读」这条主链路。
//
// 目标刻意取 127.0.0.1 上的一个几乎不可能开放的低端口：结果稳定、耗时可控，
// 而且这条用例检验的是任务生命周期，不是网络。
func TestScanRunnerRunsAndReports(t *testing.T) {
	t.Parallel()

	var done chan ScanState = make(chan ScanState, 1)
	runner := NewScanRunner(func(st ScanState) { done <- st })

	state, err := runner.Start(ScanRequest{Hosts: []string{"127.0.0.1"}, Ports: []int{1}, TimeoutMs: 200})
	if err != nil {
		t.Fatalf("启动扫描失败: %v", err)
	}
	if !state.Running {
		t.Fatalf("Start 应立刻返回一个「运行中」的快照，实际 %+v", state)
	}
	if state.HostsTotal != 1 {
		t.Errorf("目标主机数应为 1，实际 %d", state.HostsTotal)
	}
	if state.ID == "" {
		t.Error("任务应当带一个 ID，前端据此判断是否换了一轮")
	}

	select {
	case final := <-done:
		if !final.Done || final.Running {
			t.Fatalf("结束回调应给出终态，实际 %+v", final)
		}
		if final.Found != 0 {
			t.Errorf("127.0.0.1:1 不应被判定为开放，实际 found=%d", final.Found)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("扫描未在 10 秒内结束")
	}

	// 结束后快照仍是可读的（前端最后那一次轮询要拿结果）。
	snap := runner.Snapshot()
	if !snap.Done || snap.ID != state.ID {
		t.Fatalf("结束后快照应保留最后一轮结果，实际 %+v", snap)
	}
}

// TestScanRunnerCancelStopsLongScan 断言取消真的能停下正在跑的扫描。
//
// 用 TEST-NET（192.0.2.0/22）做目标：这段地址由 RFC 5737 保留，
// 不会指向任何真实主机，因此"取消前它一定还在跑"是稳定的。
func TestScanRunnerCancelStopsLongScan(t *testing.T) {
	t.Parallel()

	runner := NewScanRunner(nil)
	if _, err := runner.Start(ScanRequest{
		Spec:        "192.0.2.0/22",
		Concurrency: 1,
		TimeoutMs:   2000,
	}); err != nil {
		t.Fatalf("启动扫描失败: %v", err)
	}

	if !runner.Cancel() {
		t.Fatal("扫描应当还在运行，Cancel 却返回 false")
	}
	if !runner.Wait(5 * time.Second) {
		t.Fatal("取消后 5 秒内未结束")
	}
	final := runner.Snapshot()
	if final.Running {
		t.Fatalf("取消后不应仍是运行中：%+v", final)
	}
	if !final.Canceled {
		t.Fatalf("终态应标记为已取消：%+v", final)
	}
	if !strings.Contains(final.Stage, "取消") {
		t.Errorf("阶段文案应反映取消，实际 %q", final.Stage)
	}
}

// TestScanRunnerRejectsInvalidRange 断言非法范围在"开始"这一步就被拒绝，
// 而不是受理之后在后台失败 —— 后者用户看到的会是一个永远转的进度条。
func TestScanRunnerRejectsInvalidRange(t *testing.T) {
	t.Parallel()

	runner := NewScanRunner(nil)
	if _, err := runner.Start(ScanRequest{Mode: "custom", Spec: "10.0.0.0/16"}); err == nil {
		t.Fatal("超限范围应当被拒绝")
	}
	if runner.Busy() {
		t.Error("被拒绝的请求不应留下运行中的任务")
	}
}

/* ------------------------------------------------------------------ 小工具 */

func head(list []string, n int) []string {
	if len(list) <= n {
		return list
	}
	return list[:n]
}

// TestScanNetworkRespectsContextCancellation 保证 ctx 一取消就不再发新探测。
func TestScanNetworkRespectsContextCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	items := ScanNetwork(ctx, NetworkScanOptions{
		Hosts:   ExpandSubnet("192.0.2"),
		Timeout: 500 * time.Millisecond,
	}, nil)
	if len(items) != 0 {
		t.Fatalf("已取消的扫描不应返回结果，实际 %d 条", len(items))
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("已取消的扫描应立刻返回，实际耗时 %s", elapsed)
	}
}
