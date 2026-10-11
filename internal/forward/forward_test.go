package forward

import (
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/MisiteQ/qlink2desktop/internal/domain"
)

// freePort 取一个当前空闲的端口。
//
// 先监听再关闭拿到内核分配的端口，比随机猜一个可靠得多：
// 随机端口在 CI 上会和其它并行测试撞车，表现为偶发失败 ——
// 那种失败最耗时间，因为它看起来像是被测代码有问题。
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("无法申请空闲端口: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

// tcpEcho 起一个回显服务，返回它的地址与停止函数。
func tcpEcho(t *testing.T) (int, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("无法启动回显后端: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	return port, func() { _ = ln.Close(); <-done }
}

// udpEcho 起一个 UDP 回显服务（收到什么原样发回）。
func udpEcho(t *testing.T) (int, func()) {
	t.Helper()
	addr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("解析 UDP 地址失败: %v", err)
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		t.Fatalf("无法启动 UDP 回显后端: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 2048)
		for {
			n, src, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			_, _ = conn.WriteToUDP(buf[:n], src)
		}
	}()
	port := conn.LocalAddr().(*net.UDPAddr).Port
	return port, func() { _ = conn.Close(); <-done }
}

// waitFor 轮询等待某个条件成立，超时则失败。
//
// 转发的多数可观测结果都是**异步落地**的：客户端收到回包的那一刻，
// 转发层还在等另一个方向的 goroutine 收尾、然后才更新计数。
// 直接断言等于赌调度顺序——单次跑几乎必过，压测或并行跑时随机失败，
// 而那种"偶尔红一次"的测试比一直红更耗时间，因为它会让人怀疑被测代码。
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待超时：%s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func testRule(id string, listen, target int, proto domain.Protocol) domain.Forward {
	f := domain.Forward{
		ID:         id,
		Name:       "规则-" + id,
		Protocol:   proto,
		ListenPort: listen,
		TargetHost: "127.0.0.1",
		TargetPort: target,
		Enabled:    true,
	}
	f.Normalize()
	return f
}

// --------------------------------------------------------------------------
// TCP
// --------------------------------------------------------------------------

// TestTCPForwardRoundTrip 是整个转发功能的地基：数据必须真的走一趟来回。
//
// 只断言"监听起来了"是不够的——监听成功但双向拷贝写反、或者半关闭漏了，
// 一样会起得来、一样连得上，只是数据永远回不来。
func TestTCPForwardRoundTrip(t *testing.T) {
	backend, stopBackend := tcpEcho(t)
	defer stopBackend()

	listen := freePort(t)
	m := NewManager()
	defer m.StopAll()

	if err := m.Apply(testRule("r1", listen, backend, domain.ProtoTCP)); err != nil {
		t.Fatalf("启动转发失败: %v", err)
	}

	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", listen), 3*time.Second)
	if err != nil {
		t.Fatalf("连接转发端口失败: %v", err)
	}
	defer conn.Close()

	// 半关闭写方向后仍要能读完回包 —— 这正是 TCP 转发最容易写错的地方。
	// 若转发层在收到 FIN 后立刻 Close 整条连接，这里就会读到 EOF 而不是数据。
	payload := "hello-forward"
	if _, err := conn.Write([]byte(payload)); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if cw, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}

	got, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("读取回包失败: %v", err)
	}
	if string(got) != payload {
		t.Fatalf("回包内容 = %q，期望 %q", got, payload)
	}

	// 记账是异步落地的，而且**分两步**：先更新计数，再写连接记录。
	// 只等计数会踩到"计数已到、记录还没写"的中间态 —— 单跑往往看不出来，
	// -count=N 或并行时就随机 FAIL（这个坑本项目已经踩过一次）。
	// 所以这里等的是最终状态：计数与连接记录都到位。
	waitFor(t, "转发计数与连接记录更新", func() bool {
		c := m.Status("r1").Counters
		ok := c.Total == 1 && c.BytesIn == int64(len(payload)) && c.BytesOut == int64(len(payload))
		return ok && len(m.Recent("r1", 10)) == 1
	})

	st := m.Status("r1")
	if !st.Running {
		t.Errorf("规则状态应为运行中")
	}
	if st.Counters.Total != 1 {
		t.Errorf("累计连接数 = %d，期望 1", st.Counters.Total)
	}
	if st.Counters.BytesIn != int64(len(payload)) {
		t.Errorf("入向字节 = %d，期望 %d", st.Counters.BytesIn, len(payload))
	}
	if st.Counters.BytesOut != int64(len(payload)) {
		t.Errorf("出向字节 = %d，期望 %d", st.Counters.BytesOut, len(payload))
	}

	logs := m.Recent("r1", 10)
	if len(logs) != 1 {
		t.Fatalf("连接记录数 = %d，期望 1", len(logs))
	}
	if logs[0].Outcome != OutcomeOK {
		t.Errorf("连接结果 = %q，期望 %q", logs[0].Outcome, OutcomeOK)
	}
}

// TestTCPForwardToUnreachableTarget 验证目标不通时的表现。
//
// 期望不是"转发规则起不来"，而是"规则照常监听、连接被记为失败"：
// 目标服务可能只是暂时没起来，规则不应该因此消失。
func TestTCPForwardToUnreachableTarget(t *testing.T) {
	listen := freePort(t)
	dead := freePort(t) // 没有任何服务在听

	m := NewManager()
	defer m.StopAll()
	if err := m.Apply(testRule("r1", listen, dead, domain.ProtoTCP)); err != nil {
		t.Fatalf("启动转发失败: %v", err)
	}

	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", listen), 3*time.Second)
	if err != nil {
		t.Fatalf("连接转发端口失败: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	_, _ = conn.Write([]byte("x"))
	_, _ = io.ReadAll(conn) // 对端会立刻关闭

	waitFor(t, "目标失败被计数并记录", func() bool {
		return m.Status("r1").Counters.Failed > 0 && len(m.Recent("r1", 10)) == 1
	})

	st := m.Status("r1")
	if st.Counters.Total != 1 {
		t.Errorf("累计连接数 = %d，期望 1", st.Counters.Total)
	}
	logs := m.Recent("r1", 10)
	if len(logs) != 1 {
		t.Fatalf("连接记录数 = %d，期望 1", len(logs))
	}
	if logs[0].Outcome != OutcomeDialFailed {
		t.Errorf("连接结果 = %q，期望 %q", logs[0].Outcome, OutcomeDialFailed)
	}
}

// --------------------------------------------------------------------------
// UDP
// --------------------------------------------------------------------------

// TestUDPForwardRoundTrip 验证 UDP 会话表确实建立了并把回包送回原路。
func TestUDPForwardRoundTrip(t *testing.T) {
	backend, stopBackend := udpEcho(t)
	defer stopBackend()

	listen := freePort(t)
	m := NewManager()
	defer m.StopAll()
	if err := m.Apply(testRule("r1", listen, backend, domain.ProtoUDP)); err != nil {
		t.Fatalf("启动转发失败: %v", err)
	}

	// 客户端用**同一个** socket 收发，模拟真实的 NAT 场景：
	// 回包必须从监听端口原路返回，否则对端收不到。
	cli, err := net.DialTimeout("udp", fmt.Sprintf("127.0.0.1:%d", listen), 3*time.Second)
	if err != nil {
		t.Fatalf("连接转发端口失败: %v", err)
	}
	defer cli.Close()

	payload := "udp-ping"
	if _, err := cli.Write([]byte(payload)); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	_ = cli.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 256)
	n, err := cli.Read(buf)
	if err != nil {
		t.Fatalf("读取回包失败（UDP 会话可能没有建立）: %v", err)
	}
	if string(buf[:n]) != payload {
		t.Fatalf("回包内容 = %q，期望 %q", buf[:n], payload)
	}

	waitFor(t, "UDP 计数更新", func() bool {
		c := m.Status("r1").Counters
		return c.BytesIn == int64(len(payload)) && c.BytesOut == int64(len(payload))
	})

	if got := m.Status("r1").Counters.Active; got != 1 {
		t.Errorf("活跃会话数 = %d，期望 1", got)
	}
}

// TestUDPSessionReclaimed 验证空闲会话会被回收。
//
// 没有回收的话，会话表只会增长 —— 这是 UDP 转发最隐蔽的资源泄漏：
// 功能一切正常，内存与 fd 却在慢慢涨，几周后才被发现。
func TestUDPSessionReclaimed(t *testing.T) {
	backend, stopBackend := udpEcho(t)
	defer stopBackend()

	listen := freePort(t)
	m := NewManager()
	defer m.StopAll()
	if err := m.Apply(testRule("r1", listen, backend, domain.ProtoUDP)); err != nil {
		t.Fatalf("启动转发失败: %v", err)
	}

	cli, err := net.DialTimeout("udp", fmt.Sprintf("127.0.0.1:%d", listen), 3*time.Second)
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	_, _ = cli.Write([]byte("x"))
	_ = cli.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 64)
	_, _ = cli.Read(buf)
	_ = cli.Close()

	m.mu.RLock()
	st := m.rules["r1"]
	m.mu.RUnlock()
	if st == nil {
		t.Fatal("规则未登记")
	}
	waitFor(t, "UDP 会话建立", func() bool {
		st.udp.mu.Lock()
		defer st.udp.mu.Unlock()
		return len(st.udp.sessions) == 1
	})
	// 不去等 60 秒的真实超时：直接把会话的空闲时间推到超时之前，
	// 再手动触发一次回收，验证的仍然是同一段清理逻辑。
	st.udp.mu.Lock()
	for _, sess := range st.udp.sessions {
		sess.last.Store(time.Now().Add(-2 * udpIdleTimeout).UnixNano())
	}
	st.udp.mu.Unlock()
	st.udp.reap()

	st.udp.mu.Lock()
	left := len(st.udp.sessions)
	st.udp.mu.Unlock()
	if left != 0 {
		t.Fatalf("回收后仍剩 %d 个会话，期望 0", left)
	}
	if got := st.counters.Active.Load(); got != 0 {
		t.Errorf("活跃会话计数 = %d，期望 0（回收时必须减回去）", got)
	}
}

// --------------------------------------------------------------------------
// 访问控制
// --------------------------------------------------------------------------

func TestAccessControl(t *testing.T) {
	cases := []struct {
		name  string
		mode  domain.AccessMode
		list  []string
		addr  string
		allow bool
	}{
		{"不限制时谁都能连", domain.AccessOff, nil, "8.8.8.8:1", true},
		{"白名单内放行", domain.AccessAllow, []string{"192.168.1.5"}, "192.168.1.5:1234", true},
		{"白名单外拒绝", domain.AccessAllow, []string{"192.168.1.5"}, "192.168.1.6:1234", false},
		{"白名单 CIDR 命中", domain.AccessAllow, []string{"192.168.1.0/24"}, "192.168.1.77:1", true},
		{"白名单 CIDR 未命中", domain.AccessAllow, []string{"10.0.0.0/8"}, "192.168.1.77:1", false},
		{"黑名单内拒绝", domain.AccessDeny, []string{"10.0.0.9"}, "10.0.0.9:1", false},
		{"黑名单外放行", domain.AccessDeny, []string{"10.0.0.9"}, "10.0.0.10:1", true},
		// IPv4-mapped IPv6 必须被当成同一个地址，否则双栈下白名单会时灵时不灵。
		{"IPv4-mapped 等价", domain.AccessAllow, []string{"192.168.1.5"}, "[::ffff:192.168.1.5]:1", true},
		{"无法解析时白名单拒绝", domain.AccessAllow, []string{"192.168.1.5"}, "not-an-ip", false},
		{"无法解析时黑名单放行", domain.AccessDeny, []string{"192.168.1.5"}, "not-an-ip", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			acl := compileACL(tc.mode, tc.list)
			if got := acl.Allows(tc.addr); got != tc.allow {
				t.Errorf("Allows(%q) = %v，期望 %v", tc.addr, got, tc.allow)
			}
		})
	}
}

// TestAccessControlDeniedIsCounted 验证被拒绝的连接会被计数与记录。
func TestAccessControlDeniedIsCounted(t *testing.T) {
	backend, stopBackend := tcpEcho(t)
	defer stopBackend()

	listen := freePort(t)
	m := NewManager()
	defer m.StopAll()

	rule := testRule("r1", listen, backend, domain.ProtoTCP)
	rule.AccessMode = domain.AccessDeny
	rule.AccessList = []string{"127.0.0.1"}
	if err := m.Apply(rule); err != nil {
		t.Fatalf("启动转发失败: %v", err)
	}

	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", listen), 3*time.Second)
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	_, _ = conn.Write([]byte("x"))
	// 被拒绝时连接会被立刻关闭，读到这里就是 EOF。
	_, _ = io.ReadAll(conn)
	_ = conn.Close()

	waitFor(t, "拒绝被计数并记录", func() bool {
		return m.Status("r1").Counters.Denied > 0 && len(m.Recent("r1", 10)) == 1
	})

	logs := m.Recent("r1", 10)
	if len(logs) != 1 {
		t.Fatalf("连接记录数 = %d，期望 1", len(logs))
	}
	if logs[0].Outcome != OutcomeDenied {
		t.Errorf("连接结果 = %q，期望 %q", logs[0].Outcome, OutcomeDenied)
	}
}

// --------------------------------------------------------------------------
// 校验：自转发、端口冲突、转发环
// --------------------------------------------------------------------------

func TestSelfForwardRejected(t *testing.T) {
	f := domain.Forward{
		Name: "自转发", Protocol: domain.ProtoTCP,
		ListenPort: 8080, TargetHost: "127.0.0.1", TargetPort: 8080, Enabled: true,
	}
	f.Normalize()
	if err := f.Validate(); err == nil {
		t.Fatal("把流量转发给本机的同一个端口居然通过了校验 —— 它会形成死循环")
	}
	// localhost / ::1 / 0.0.0.0 都算本机，不能因为写法不同就漏网。
	for _, host := range []string{"localhost", "::1", "0.0.0.0", "127.0.0.9"} {
		f.TargetHost = host
		f.Normalize()
		if err := f.Validate(); err == nil {
			t.Errorf("目标 %q 未被识别为自转发", host)
		}
	}
}

func TestPortConflictDetected(t *testing.T) {
	backend, stopBackend := tcpEcho(t)
	defer stopBackend()

	listen := freePort(t)
	m := NewManager()
	defer m.StopAll()
	if err := m.Apply(testRule("r1", listen, backend, domain.ProtoTCP)); err != nil {
		t.Fatalf("启动第一条失败: %v", err)
	}
	err := m.Apply(testRule("r2", listen, backend, domain.ProtoTCP))
	if err == nil {
		t.Fatal("两条规则监听同一个 TCP 端口却没有冲突 —— 后者会静默抢走前者的流量")
	}

	// 同号的 UDP 端口不该被 TCP 规则挡住：两者是不同的协议、不同的端口空间。
	if err := m.Apply(testRule("r3", listen, backend, domain.ProtoUDP)); err != nil {
		t.Errorf("TCP 占用 %d 后仍应能监听同号 UDP 端口: %v", listen, err)
	}
}

func TestForwardLoopDetected(t *testing.T) {
	backend, stopBackend := tcpEcho(t)
	defer stopBackend()

	a, b := freePort(t), freePort(t)
	m := NewManager()
	defer m.StopAll()

	// A: a → b（目标是本机的 b 端口）
	if err := m.Apply(testRule("A", a, b, domain.ProtoTCP)); err != nil {
		t.Fatalf("启动 A 失败: %v", err)
	}
	// B: b → a，与 A 互为转发目标 → 一个包会在两端口间无限弹射。
	err := m.Apply(testRule("B", b, a, domain.ProtoTCP))
	if err == nil {
		t.Fatal("互为转发目标却没有被拦住 —— 这会让数据包在两端口间无限弹射")
	}
	if !strings.Contains(err.Error(), "死循环") {
		t.Errorf("错误信息没有说清原因: %v", err)
	}
	_ = backend
}

func TestDisabledRuleDoesNotListen(t *testing.T) {
	backend, stopBackend := tcpEcho(t)
	defer stopBackend()
	listen := freePort(t)

	m := NewManager()
	defer m.StopAll()
	rule := testRule("r1", listen, backend, domain.ProtoTCP)
	rule.Enabled = false
	if err := m.Apply(rule); err != nil {
		t.Fatalf("停用规则不应报错: %v", err)
	}
	if m.Status("r1").Running {
		t.Error("已停用的规则仍在监听")
	}
	// 端口必须真的被释放：停用的意义就是不再占用端口。
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", listen))
	if err != nil {
		t.Fatalf("停用后端口仍被占用: %v", err)
	}
	_ = ln.Close()
}

func TestStopReleasesPort(t *testing.T) {
	backend, stopBackend := tcpEcho(t)
	defer stopBackend()
	listen := freePort(t)

	m := NewManager()
	defer m.StopAll()
	if err := m.Apply(testRule("r1", listen, backend, domain.ProtoTCP)); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	m.Stop("r1")
	if m.Status("r1").Running {
		t.Error("停止后仍在运行")
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", listen))
	if err != nil {
		t.Fatalf("停止后端口仍被占用: %v", err)
	}
	_ = ln.Close()
}
