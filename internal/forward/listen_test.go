package forward

import (
	"fmt"
	"net"
	"runtime"
	"strings"
	"testing"
)

// 这组测试盯的是一个具体的用户场景：
//
//	"同样的设置，别的工具能跑，这个程序报端口被占用。"
//
// 根因是绑定的**形态**写死了：`:port` 在 Linux 上是双栈绑定，
// 要求 IPv4 与 IPv6 两侧都空闲；只绑单栈的工具在"被占了一侧"时仍能成功。
// 因此这里断言的不是"能绑上"，而是"被占一侧时退到另一侧后仍能绑上"。
//
// freePort 复用 forward_test.go 里的同名辅助函数。

// requireLinuxBindSemantics 跳过依赖 Linux 绑定语义的断言。
//
// 为什么必须跳过而不是"将就着跑"：Windows 的端口绑定规则与 Linux 差别很大
// （SO_REUSEADDR 在 Windows 上允许重复绑定、两套协议栈的占用关系也不同），
// 本机实测**即使 IPv4 与 IPv6 两侧都被占，仍然能绑上**。
// 留着这些断言的结果是它们永远"通过"，而通过的原因不是代码对，是平台宽松 ——
// 一条永远绿的测试比没有测试更糟，因为它会让人以为这块被守住了。
//
// 这几条断言的真实验证发生在真机（fnOS / Linux）上：`go test ./internal/forward/`。
// 纯函数部分（候选顺序、/proc 解析）不依赖平台，仍然在本机实跑。
func requireLinuxBindSemantics(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skipf("绑定语义断言只在 Linux 上有意义（本机 %s 允许重复绑定，无法证伪）", runtime.GOOS)
	}
}

func TestBindCandidatesAutoPrefersDualThenFallsBack(t *testing.T) {
	got := bindCandidates("", 8080, "tcp")
	want := []bindCandidate{
		{network: "tcp", addr: ":8080", mode: bindDual},
		{network: "tcp4", addr: "0.0.0.0:8080", mode: bindV4},
		{network: "tcp6", addr: "[::]:8080", mode: bindV6},
	}
	if len(got) != len(want) {
		t.Fatalf("候选数量 = %d，期望 %d：%+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 个候选 = %+v，期望 %+v（顺序即优先级）", i+1, got[i], want[i])
		}
	}
}

func TestBindCandidatesHonoursExplicitHostWithoutFallback(t *testing.T) {
	// 用户明确指定了地址：只能试这一个，替他换地址是更严重的错误。
	got := bindCandidates("192.168.31.5", 8080, "tcp")
	if len(got) != 1 || got[0].addr != "192.168.31.5:8080" || got[0].mode != bindFixed {
		t.Fatalf("显式地址不该产生退让候选: %+v", got)
	}

	// IPv6 字面量必须加方括号，否则会被当成主机名。
	got = bindCandidates("::1", 8080, "udp")
	if len(got) != 1 || got[0].addr != "[::1]:8080" || got[0].network != "udp" {
		t.Fatalf("IPv6 字面量未加方括号或协议错了: %+v", got)
	}
}

func TestBindModeDegraded(t *testing.T) {
	// 只有真的退让才算降级：用户自己填地址的固定绑定不是降级，
	// 把它也报成降级会稀释告警。
	cases := map[bindMode]bool{
		bindDual:  false,
		bindFixed: false,
		bindV4:    true,
		bindV6:    true,
	}
	for mode, want := range cases {
		if got := mode.degraded(); got != want {
			t.Errorf("%q.degraded() = %v，期望 %v", mode, got, want)
		}
	}
}

// TestListenTCPFallsBackWhenDualCandidateTaken 是本机（Windows）唯一能
// 真实跑到的"降级发生了"的验证：占用者用 `:port`，第一个候选（双栈）必然被拒，
// 随后必须有某个单栈候选顶上。
//
// 它只在非 Linux 上跑，与 TestListenTCPReportsInUseWhenFullyOccupied 互补 ——
// 两个平台各覆盖一半，合起来才是完整的语义：
//
//	Windows：`:p` 与 `0.0.0.0:p` / `[::]:p` 可共存 → 首选被占时能退，测试这条；
//	Linux  ：`:p` 会吃住两侧              → 全被占时应当报错，测试那条。
func TestListenTCPFallsBackWhenDualCandidateTaken(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("Linux 上 `:port` 会同时占住两侧，见 TestListenTCPReportsInUseWhenFullyOccupied")
	}
	port := freePort(t)

	squatter, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		t.Skipf("本机无法占住该端口，跳过: %v", err)
	}
	defer squatter.Close()

	ln, mode, err := listenTCP("", port)
	if err != nil {
		t.Fatalf("首选形态被占后应当退到单栈继续服务，却整体失败: %v", err)
	}
	defer ln.Close()
	if mode == bindDual {
		t.Errorf("首选的双栈候选都没绑上，却被报告成双栈形态（%s）", mode)
	}
	t.Logf("%s：双栈候选被占 → 实际退化形态 %s，监听在 %s", runtime.GOOS, mode, ln.Addr())
}

func TestListenTCPStillStartsWhenIPv4SideTaken(t *testing.T) {
	requireLinuxBindSemantics(t)
	port := freePort(t)

	v4, err := net.Listen("tcp4", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		t.Skipf("本机无法占用 IPv4 侧，跳过: %v", err)
	}
	defer v4.Close()

	ln, mode, err := listenTCP("", port)
	if err != nil {
		t.Fatalf("IPv4 侧被别人占着时，应当退到另一侧继续服务，却整体失败: %v", err)
	}
	defer ln.Close()

	t.Logf("IPv4 侧被占 → 实际形态 %s，监听在 %s", mode, ln.Addr())
	if mode == bindV4 {
		t.Errorf("退化成仅 IPv4，可 IPv4 侧正是别人占着的 —— 退让选错了方向")
	}
}

func TestListenTCPStillStartsWhenIPv6SideTaken(t *testing.T) {
	requireLinuxBindSemantics(t)
	port := freePort(t)

	v6, err := net.Listen("tcp6", fmt.Sprintf("[::]:%d", port))
	if err != nil {
		t.Skipf("本机无法占用 IPv6 侧，跳过: %v", err)
	}
	defer v6.Close()

	ln, mode, err := listenTCP("", port)
	if err != nil {
		t.Fatalf("IPv6 侧被别人占着时，应当退到 IPv4 继续服务，却整体失败: %v", err)
	}
	defer ln.Close()

	t.Logf("IPv6 侧被占 → 实际形态 %s，监听在 %s", mode, ln.Addr())
	if mode == bindV6 {
		t.Errorf("退化成仅 IPv6，可 IPv6 侧正是别人占着的 —— 退让选错了方向")
	}
}

// TestListenTCPReportsInUseWhenFullyOccupied 验证"确实无路可走"时如实报错。
//
// 用 `:port` 作为占用者是最真实的构造：在 Linux 上它会同时占住 IPv4 与
// IPv6 两侧（v6only=0），与"别的程序把这个端口吃干净了"等价。
//
// 这条也是 Linux-only，原因值得记一笔：Windows 上 `net.Listen("tcp", ":p")`
// 与 `0.0.0.0:p`、`[::]:p` **可以共存**（实测），所以在 Windows 上这个占用者
// 并不会真的堵死两侧，我们反而会降级到 IPv4 绑上 —— 那是一个正确的结果，
// 只是它证明不了"全占用时应当报错"。
func TestListenTCPReportsInUseWhenFullyOccupied(t *testing.T) {
	requireLinuxBindSemantics(t)
	port := freePort(t)

	squatter, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		t.Skipf("本机无法占住该端口，跳过: %v", err)
	}
	defer squatter.Close()

	ln, _, err := listenTCP("", port)
	if err == nil {
		ln.Close()
		t.Fatal("端口两侧都被占着却还能绑上 —— 那说明我们绑的根本不是这个端口")
	}
	if !isAddrInUse(err) {
		t.Fatalf("失败原因应当是「地址已被占用」，实际: %v", err)
	}
}

func TestListenTCPExplicitHostDoesNotFallBack(t *testing.T) {
	requireLinuxBindSemantics(t)
	port := freePort(t)

	v4, err := net.Listen("tcp4", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		t.Skipf("本机无法占用 IPv4 侧，跳过: %v", err)
	}
	defer v4.Close()

	// 用户自己写了 0.0.0.0：被占就必须如实报错，不能悄悄换到别的形态，
	// 否则他会以为自己配的地址生效了。
	if ln, _, err := listenTCP("0.0.0.0", port); err == nil {
		ln.Close()
		t.Fatal("显式指定的监听地址被占时不该退让")
	}
}

func TestListenUDPFallsBackAsWell(t *testing.T) {
	requireLinuxBindSemantics(t)
	port := freePort(t)

	// UDP 与 TCP 的端口空间独立，这里占用的是 UDP 的 IPv4 侧。
	pc, err := net.ListenPacket("udp4", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		t.Skipf("本机无法占用 UDP IPv4 侧，跳过: %v", err)
	}
	defer pc.Close()

	conn, mode, err := listenUDP("", port)
	if err != nil {
		t.Fatalf("UDP IPv4 侧被占时应当退到另一侧，却整体失败: %v", err)
	}
	defer conn.Close()
	t.Logf("UDP IPv4 侧被占 → 实际形态 %s，监听在 %s", mode, conn.LocalAddr())
	if mode == bindV4 {
		t.Errorf("UDP 退化成仅 IPv4，方向错了")
	}
}

func TestDescribeListenErrNamesTheOccupierWhenKnown(t *testing.T) {
	// 查不到占用者（本机非 Linux 或确实查不到）时必须是中性文案，
	// 宁可不说，也不要编一个可能误导人的结论。
	msg := describeListenErr(fmt.Errorf("listen tcp :8080: bind: address already in use"), 8080, "tcp")
	if msg == "" {
		t.Fatal("占用类错误必须有可读文案")
	}
	if !strings.Contains(msg, "8080") {
		t.Errorf("文案里应当带上端口号，实际: %s", msg)
	}
	if !strings.Contains(msg, "占用") {
		t.Errorf("文案应当说明是「被占用」，实际: %s", msg)
	}
	// 权限类错误不能被说成"被占用"——那会把用户引向完全错误的排查方向。
	perm := describeListenErr(fmt.Errorf("listen tcp :80: bind: permission denied"), 80, "tcp")
	if strings.Contains(perm, "已被占用") {
		t.Errorf("权限错误被误报成端口占用: %s", perm)
	}
}
