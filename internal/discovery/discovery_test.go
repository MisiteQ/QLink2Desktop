package discovery

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// unusedPort 返回一个此刻确实未被监听的端口。
//
// 不能用 port+1 之类的相邻端口当"关闭端口"样本：并行测试与系统服务
// 完全可能正好占用相邻端口，那样断言就会随机失败。
func unusedPort(t *testing.T) int {
	t.Helper()
	for i := 0; i < 20; i++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		p := ln.Addr().(*net.TCPAddr).Port
		ln.Close()

		// 回连确认：能连上说明已被别人占用，换一个再试。
		c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p)), 100*time.Millisecond)
		if err != nil {
			return p
		}
		c.Close()
	}
	t.Skip("找不到空闲端口")
	return 0
}

// -------------------------------------------------------------------------- procfs 解析

// 下面这份样例直接取自真实机器上的 /proc/net/tcp，保留了原始的列宽与空格。
const sampleProcNetTCP = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 24601 1 0000000000000000 100 0 0 10 0
   1: 0100007F:0035 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 24602 1 0000000000000000 100 0 0 10 0
   2: 0100007F:8AE4 0100007F:1F90 01 00000000:00000000 00:00000000 00000000  1000        0 24603 1 0000000000000000 20 4 30 10 -1
   3: 00000000:0050 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 24604 1 0000000000000000 100 0 0 10 0
`

func TestParseProcNetTCP(t *testing.T) {
	t.Parallel()

	got := ParseProcNet(sampleProcNetTCP, ProtoTCP, false)

	// 第 3 行状态是 01（ESTABLISHED），必须被过滤掉。
	if len(got) != 3 {
		t.Fatalf("应解析出 3 个监听套接字，实际 %d: %+v", len(got), got)
	}

	want := []struct {
		addr  string
		port  int
		inode string
	}{
		{"0.0.0.0", 8080, "24601"},
		{"127.0.0.1", 53, "24602"},
		{"0.0.0.0", 80, "24604"},
	}
	for i, w := range want {
		if got[i].Address != w.addr || got[i].Port != w.port || got[i].Inode != w.inode {
			t.Errorf("第 %d 项 = %+v, 期望 addr=%s port=%d inode=%s", i, got[i], w.addr, w.port, w.inode)
		}
	}
}

func TestParseProcNetFiltersByState(t *testing.T) {
	t.Parallel()

	// UDP 的「监听」状态码是 07，而不是 0A。
	const udp = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000:0044 00000000:0000 07 00000000:00000000 00:00000000 00000000     0        0 31001 1
   1: 00000000:0045 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 31002 1
`
	got := ParseProcNet(udp, ProtoUDP, false)
	if len(got) != 1 || got[0].Port != 68 {
		t.Fatalf("UDP 监听态解析不正确: %+v", got)
	}
	if got[0].Proto != ProtoUDP {
		t.Errorf("协议应为 udp，实际 %q", got[0].Proto)
	}
}

func TestParseProcNetIgnoresMalformed(t *testing.T) {
	t.Parallel()

	cases := []string{
		"",
		"  sl  local_address\n", // 只有表头
		"   0: garbage\n",       // 字段不足
		"   0: 00000000:ZZZZ 00000000:0000 0A 0 0 0 0 0 0 1 1\n", // 端口非十六进制
		"   0: 00000000:0000 00000000:0000 0A 0 0 0 0 0 0 1 1\n", // 端口为 0
	}
	for _, c := range cases {
		if got := ParseProcNet(c, ProtoTCP, false); len(got) != 0 {
			t.Errorf("输入 %q 应解析为空，实际 %+v", c, got)
		}
	}
}

func TestDecodeProcAddressIPv4(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"0100007F": "127.0.0.1",
		"00000000": "0.0.0.0",
		"C0A80101": "1.1.168.192", // 小端序：01 01 A8 C0 → 1.1.168.192
		"bad":      "bad",
	}
	for in, want := range cases {
		if got := decodeProcAddress(in, false); got != want {
			t.Errorf("decodeProcAddress(%q) = %q, 期望 %q", in, got, want)
		}
	}
}

func TestDecodeProcAddressIPv6(t *testing.T) {
	t.Parallel()

	// 全零即未指定地址。
	if got := decodeProcAddress(strings.Repeat("0", 32), true); got != "::" {
		t.Errorf("全零 IPv6 应为 ::，实际 %q", got)
	}
	// 长度不对时原样返回，便于排查而不是静默给错值。
	if got := decodeProcAddress("abcd", true); got != "abcd" {
		t.Errorf("非法长度应原样返回，实际 %q", got)
	}
}

func TestParseSocketInodeOwner(t *testing.T) {
	t.Parallel()

	cases := []struct {
		link   string
		want   string
		wantOK bool
	}{
		{"socket:[24601]", "24601", true},
		{"socket:[]", "", false},
		{"anon_inode:[eventpoll]", "", false},
		{"/dev/sda", "", false},
		{"socket:[123", "", false},
	}
	for _, c := range cases {
		got, ok := ParseSocketInodeOwner(c.link)
		if ok != c.wantOK || got != c.want {
			t.Errorf("ParseSocketInodeOwner(%q) = (%q,%v), 期望 (%q,%v)", c.link, got, ok, c.want, c.wantOK)
		}
	}
}

func TestIsEphemeralPort(t *testing.T) {
	t.Parallel()

	if !IsEphemeralPort(45000) {
		t.Error("45000 应属于动态端口")
	}
	if IsEphemeralPort(8080) {
		t.Error("8080 不应属于动态端口")
	}
}

// -------------------------------------------------------------------------- 标签解析

func TestExtractLabelHint(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		labels    map[string]string
		container string
		wantOK    bool
		check     func(LabelHint) bool
	}{
		{
			name:      "标准 watchcow 标签",
			labels:    map[string]string{"watchcow.enable": "true", "watchcow.title": "我的网盘", "watchcow.icon": "alist"},
			container: "alist",
			wantOK:    true,
			check:     func(h LabelHint) bool { return h.Title == "我的网盘" && h.Icon == "alist" && h.Path == "/" },
		},
		{
			name:      "无前缀写法",
			labels:    map[string]string{"watchcow": "1", "watchcow.port": "5244", "watchcow.scheme": "https"},
			container: "svc",
			wantOK:    true,
			check:     func(h LabelHint) bool { return h.Port == 5244 && h.Scheme == "https" },
		},
		{
			name:      "大小写与下划线变体",
			labels:    map[string]string{"WatchCow.ENABLED": "yes", "WatchCow.Name": "X", "WatchCow.All_Users": "true"},
			container: "x",
			wantOK:    true,
			check:     func(h LabelHint) bool { return h.Title == "X" && h.AllUsers },
		},
		{
			name:      "本项目前缀",
			labels:    map[string]string{"qlink2d.enable": "on", "qlink2d.path": "/admin"},
			container: "y",
			wantOK:    true,
			check:     func(h LabelHint) bool { return h.Path == "/admin" },
		},
		{
			name:      "未声明启用",
			labels:    map[string]string{"watchcow.title": "X"},
			container: "x",
			wantOK:    false,
		},
		{
			name:      "显式声明隐藏",
			labels:    map[string]string{"watchcow.enable": "true", "watchcow.hide": "true"},
			container: "x",
			wantOK:    false,
		},
		{
			name:      "无任何相关标签",
			labels:    map[string]string{"com.docker.compose.service": "web"},
			container: "x",
			wantOK:    false,
		},
		{
			name:      "空标签集",
			labels:    nil,
			container: "x",
			wantOK:    false,
		},
		{
			name:      "非法端口被忽略",
			labels:    map[string]string{"watchcow.enable": "true", "watchcow.port": "abc"},
			container: "x",
			wantOK:    true,
			check:     func(h LabelHint) bool { return h.Port == 0 },
		},
		{
			name:      "超范围端口被忽略",
			labels:    map[string]string{"watchcow.enable": "true", "watchcow.port": "99999"},
			container: "x",
			wantOK:    true,
			check:     func(h LabelHint) bool { return h.Port == 0 },
		},
		{
			name:      "标题缺省回落到容器名",
			labels:    map[string]string{"watchcow.enable": "true"},
			container: "my-app",
			wantOK:    true,
			check:     func(h LabelHint) bool { return h.Title == "my-app" },
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			hint, ok := ExtractLabelHint(tc.labels, tc.container)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, 期望 %v (%+v)", ok, tc.wantOK, hint)
			}
			if ok && tc.check != nil && !tc.check(hint) {
				t.Errorf("字段不符合预期: %+v", hint)
			}
		})
	}
}

func TestLabelHintKeyIsStable(t *testing.T) {
	t.Parallel()

	a := LabelHint{Title: "A", ContainerName: "MyContainer"}
	b := LabelHint{Title: "B", ContainerName: "MyContainer"}
	if a.Key() != b.Key() {
		t.Errorf("同一容器应得到相同键: %q vs %q", a.Key(), b.Key())
	}
	c := LabelHint{Title: "A"}
	if a.Key() == c.Key() {
		t.Error("不同容器不应共享键")
	}
	if !strings.HasPrefix(a.Key(), "docklabel-") {
		t.Errorf("键应带前缀: %q", a.Key())
	}
}

// -------------------------------------------------------------------------- 主机串清理

func TestNormalizeHostInput(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"192.168.1.10":                  "192.168.1.10",
		"  192.168.1.10  ":              "192.168.1.10",
		"http://192.168.1.10:8080/x":    "192.168.1.10",
		"https://nas.example.com/admin": "nas.example.com",
		"nas.local:5900":                "nas.local",
		"nas.local/?q=1":                "nas.local",
		"":                              "",
	}
	for in, want := range cases {
		if got := NormalizeHostInput(in); got != want {
			t.Errorf("NormalizeHostInput(%q) = %q, 期望 %q", in, got, want)
		}
	}
}

func TestExpandSubnet(t *testing.T) {
	t.Parallel()

	got := ExpandSubnet("192.168.1")
	if len(got) != 254 {
		t.Fatalf("应展开 254 个地址，实际 %d", len(got))
	}
	if got[0] != "192.168.1.1" || got[253] != "192.168.1.254" {
		t.Errorf("边界地址不正确: %s .. %s", got[0], got[253])
	}
	for _, addr := range got {
		if strings.HasSuffix(addr, ".0") || strings.HasSuffix(addr, ".255") {
			t.Errorf("不应包含网络地址或广播地址: %s", addr)
		}
	}
}

func TestCommonPortsReturnsCopy(t *testing.T) {
	t.Parallel()

	a := CommonPorts()
	if len(a) == 0 {
		t.Fatal("默认端口集不应为空")
	}
	a[0] = -1
	b := CommonPorts()
	if b[0] == -1 {
		t.Error("CommonPorts 必须返回副本，否则调用方会污染全局配置")
	}
}

// -------------------------------------------------------------------------- 扫描

func TestScanTCPFindsOpenPort(t *testing.T) {
	t.Parallel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	got := ScanTCP(context.Background(), ScanOptions{
		Hosts: []string{"127.0.0.1"},
		Ports: []int{port},
	})
	if len(got) != 1 || got[0].Port != port {
		t.Fatalf("应发现开放端口 %d，实际 %+v", port, got)
	}
}

func TestScanTCPRespectsContextCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	got := ScanTCP(ctx, ScanOptions{Hosts: ExpandSubnet("10.255.255"), Ports: CommonPorts()})
	if time.Since(start) > 3*time.Second {
		t.Error("取消后应立即返回")
	}
	if len(got) != 0 {
		t.Errorf("取消后不应有结果: %+v", got)
	}
}

func TestProbeHostPorts(t *testing.T) {
	t.Parallel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	// 一个开放的 + 一个确认关闭的。
	closed := unusedPort(t)
	got := ProbeHostPorts(context.Background(), "127.0.0.1", []int{port, closed}, 300*time.Millisecond)

	if len(got) != 1 || got[0] != port {
		t.Fatalf("应只发现 %d，实际 %v", port, got)
	}
}

func TestLocalSubnetsDoesNotPanic(t *testing.T) {
	t.Parallel()

	// 只要求不 panic、返回的每项都是三段式前缀。
	for _, s := range LocalSubnets() {
		if strings.Count(s, ".") != 2 {
			t.Errorf("网段前缀格式不正确: %q", s)
		}
	}
}

// -------------------------------------------------------------------------- Docker 客户端

func TestDockerUnavailableWhenSocketMissing(t *testing.T) {
	t.Parallel()

	c := NewDockerClient("/nonexistent/docker.sock")
	if c.Available() {
		t.Error("套接字不存在时应报告不可用")
	}
	if got := c.Containers(false); got != nil {
		t.Errorf("不可用时应返回空: %+v", got)
	}
	if got := c.LabelHints(); len(got) != 0 {
		t.Errorf("不可用时应返回空标签: %+v", got)
	}
}

func TestDockerDefaultSocketPath(t *testing.T) {
	t.Parallel()

	if got := NewDockerClient("").SocketPath(); got != DefaultDockerSocket {
		t.Errorf("空路径应回落到默认值，实际 %q", got)
	}
}

// -------------------------------------------------------------------------- SSH

func TestSSHTunnelUnavailableWithoutBinary(t *testing.T) {
	t.Parallel()

	// 直接构造并清空 sshBin，模拟没有 ssh 客户端的环境。
	tun := &SSHTunnel{}
	if tun.Available() {
		t.Error("未探测到 ssh 时应报告不可用")
	}
	_, err := tun.DialContext(context.Background(), "tcp", "127.0.0.1:80")
	if err == nil {
		t.Error("不可用时应返回错误")
	}
}

func TestSSHTunnelArgs(t *testing.T) {
	t.Parallel()

	tun := &SSHTunnel{sshBin: "/usr/bin/ssh"}
	tun.host.Address = "10.0.0.5"
	tun.host.User = "root"
	tun.host.Port = 2222
	tun.host.Auth.KeyPath = "/root/.ssh/id_ed25519"
	tun.host.Auth.InsecureSkipHostKey = true

	args := strings.Join(tun.args("127.0.0.1:8080"), " ")
	for _, want := range []string{"-W 127.0.0.1:8080", "root@10.0.0.5", "-p 2222", "-i /root/.ssh/id_ed25519", "StrictHostKeyChecking=no"} {
		if !strings.Contains(args, want) {
			t.Errorf("ssh 参数缺少 %q\n实际: %s", want, args)
		}
	}
}

func TestTCPProbe(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()

	if err := TCPProbe(context.Background(), strings.TrimPrefix(srv.URL, "http://"), 2*time.Second); err != nil {
		t.Errorf("应探测成功: %v", err)
	}
	if err := TCPProbe(context.Background(), "127.0.0.1:1", 500*time.Millisecond); err == nil {
		t.Error("关闭的端口应探测失败")
	}
}
