package fnos

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 这一组测试守的是同一件事：**「Stop 之后再 Start」必须真的发生**。
//
// 早期实现在真机上把应用停死了 —— 原因不是脚本写错，而是那条链原本写在
// 本进程里，而 `appcenter-cli stop` 是同步的（它等本进程退出才返回），
// 于是后半段"再起来"永远执行不到。所以这里两类断言都要有：
//   - 脚本文本层面：stop 在前、start 在后，且 stop 失败也不能中断；
//   - 脚本执行层面：真的 spawn 一次，看文件里到底落了 "stop\nstart\n"。
//
// 只做第一类是不够的：文本写得再漂亮，spawn 失败 / 被引号吃掉 / 顺序反了
// 都不会体现出来，而这类 bug 的表现全是「应用起不来」。

// ---------------------------------------------------------------- 脚本文本

// TestBuildRestartScriptRunsWholeChain 断言脚本按顺序覆盖整条链。
func TestBuildRestartScriptRunsWholeChain(t *testing.T) {
	t.Parallel()

	script := buildRestartScript("/usr/bin/appcenter-cli", "/tmp/restart.log")

	// 断言具体的秒数而不是"有没有 sleep 这个词"：写成 sleep 0 也能骗过
	// 后者，但它等于没有等待 —— 那正是这里要挡的东西。
	handoff := fmt.Sprintf("sleep %d", int(restartHandoffDelay/time.Second))
	settle := fmt.Sprintf("sleep %d", int(restartSettleDelay/time.Second))
	if restartHandoffDelay <= 0 || restartSettleDelay <= 0 {
		t.Fatalf("两段等待都必须为正：交接 %s / 静置 %s", restartHandoffDelay, restartSettleDelay)
	}

	stopAt := strings.Index(script, "appcenter-cli' stop")
	startAt := strings.Index(script, "appcenter-cli' start")
	if stopAt < 0 || startAt < 0 {
		t.Fatalf("脚本没有完整地包含 stop / start 两步:\n%s", script)
	}
	if startAt < stopAt {
		t.Fatalf("start 出现在 stop 之前 —— 那等于先启动再停止，会把服务留在停止态:\n%s", script)
	}
	// 两步之间必须有静置等待：否则 start 可能读到上一个进程残留的套接字。
	if !strings.Contains(script[stopAt:startAt], settle) {
		t.Errorf("stop 与 start 之间缺少静置等待 %q:\n%s", settle, script[stopAt:startAt])
	}
	// 开头也必须有交接等待：HTTP 202 要先回到浏览器，前端才来得及弹等待层。
	if !strings.Contains(script[:stopAt], handoff) {
		t.Errorf("stop 之前缺少交接等待 %q，用户会看到一个连响应都没拿到的断连:\n%s", handoff, script[:stopAt])
	}
	// 时间戳由常量拼出来，很容易多拼一个右括号（真出过），
	// 那种日志读起来是 [2026-09-30 23:19:00]] 开始 —— 不影响功能但很难看。
	if strings.Contains(script, "]]") {
		t.Errorf("时间戳被拼出了两个右方括号:\n%s", script)
	}
}

// TestBuildRestartScriptStartsEvenIfStopFails 断言 stop 失败不会中断脚本。
//
// 这是「应用停死」的直接防线：只要 stop 返回非零码就把整条链断掉，
// 用户点一次重启就永久失去服务。所以 stop 必须包在 if 里，无论成败都继续
// 往下走 —— 而且失败时要写一句人能看懂的话（这段脚本比写它的进程活得久，
// 那句话经常是唯一的线索）。
func TestBuildRestartScriptStartsEvenIfStopFails(t *testing.T) {
	t.Parallel()

	script := buildRestartScript("/usr/bin/appcenter-cli", "")

	if !strings.Contains(script, "; then echo") || !strings.Contains(script, "; else echo") {
		t.Fatalf("stop 必须包在 if / then / else 里，否则失败会中断整条链:\n%s", script)
	}
	// 失败分支里得有"继续启动"的意思，而不是一句让人以为就此结束的话。
	elseIdx := strings.Index(script, "; else echo")
	fiIdx := strings.Index(script[elseIdx:], " fi")
	if fiIdx < 0 {
		t.Fatalf("if 块没有闭合:\n%s", script)
	}
	elseBranch := script[elseIdx : elseIdx+fiIdx]
	if !strings.Contains(elseBranch, "仍继续") {
		t.Errorf("stop 失败分支应当明确写出「仍继续尝试启动」:\n%s", elseBranch)
	}
}

// TestBuildRestartScriptQuotesEveryPath 断言所有路径都进了单引号。
//
// 路径里带空格 / 引号在 NAS 上并不罕见（卷名、用户目录都可能有），
// 一旦漏掉引号，脚本要么语法错、要么把参数劈成两半，
// 而这两种情况的共同表现都是"点了重启没反应"。
func TestBuildRestartScriptQuotesEveryPath(t *testing.T) {
	t.Parallel()

	const cli = "/vol1/My Apps/appcenter-cli"
	const log = "/vol1/My Apps/restart.log"
	script := buildRestartScript(cli, log)

	if !strings.Contains(script, "'"+cli+"'") {
		t.Errorf("命令行路径没有被单引号包裹:\n%s", script)
	}
	if !strings.Contains(script, "'"+log+"'") {
		t.Errorf("日志路径没有被单引号包裹:\n%s", script)
	}
	if !strings.Contains(script, "'"+PortalAppName+"'") {
		t.Errorf("应用名没有被单引号包裹:\n%s", script)
	}
}

// TestBuildRestartScriptSideLog 断言旁路日志只在给了目录时才出现。
func TestBuildRestartScriptSideLog(t *testing.T) {
	t.Parallel()

	if s := buildRestartScript("/usr/bin/appcenter-cli", "/var/log/q/restart.log"); !strings.Contains(s, "exec >>") {
		t.Errorf("给了日志目录就应当写旁路日志:\n%s", s)
	}
	if s := buildRestartScript("/usr/bin/appcenter-cli", ""); strings.Contains(s, "exec >>") {
		t.Errorf("没给日志目录却写了重定向，会往当前目录乱丢文件:\n%s", s)
	}
}

// ---------------------------------------------------------------- 受理路径

// TestRestartSelfRejectsUnresolvableCLI 断言拿不到命令行路径时明确报错。
//
// 这里最怕的是"静默假装受理"：接口返回 202、界面弹出等待层，
// 而实际上什么都没发生，用户会一直等到超时。宁可当场报错。
func TestRestartSelfRejectsUnresolvableCLI(t *testing.T) {
	t.Parallel()

	// 命令行本身不可用 → 直接报 CLI 不可用。
	offline := NewService(Options{CLI: &ExecCLI{}})
	if err := offline.RestartSelf(context.Background()); !errors.Is(err, ErrCLIUnavailable) {
		t.Fatalf("命令行不可用时应报 ErrCLIUnavailable，实际: %v", err)
	}

	// 命令行"可用"却拿不到路径（模拟模式）→ 也要报错，且说清该怎么办。
	svc, _, _ := newTestService(t)
	err := svc.RestartSelf(context.Background())
	if err == nil {
		t.Fatal("拿不到命令行路径时应当报错，而不是静默返回成功")
	}
	if !strings.Contains(err.Error(), "应用中心") {
		t.Errorf("错误里应当告诉用户改用应用中心手动重启，实际: %v", err)
	}
}

// ---------------------------------------------------------------- 真跑一遍

// TestRestartSelfRunsStopThenStartInDetachedShell 是整条修复的回归点：
// 脱离子进程的 shell 里必须先后出现 stop 与 start。
func TestRestartSelfRunsStopThenStartInDetachedShell(t *testing.T) {
	withTestShell(t)

	dir := restartTestDir(t)
	calls := filepath.Join(dir, "calls.log")
	cliPath := writeFakeCLI(t, dir, 0)

	svc := NewService(Options{CLI: &ExecCLI{path: cliPath}, LogDir: dir})
	if err := svc.RestartSelf(context.Background()); err != nil {
		t.Fatalf("受理重启失败: %v", err)
	}

	got := waitForContent(t, calls, "start")
	if got != "stop\nstart\n" {
		t.Fatalf("独立会话里的调用序列 = %q，期望 %q\n这正是「Stop 之后永远没有 Start」那个真机事故的回归点", got, "stop\nstart\n")
	}

	// 旁路日志必须真的落盘：重启失败时它是唯一的线索。
	data, err := os.ReadFile(filepath.Join(dir, "restart.log"))
	if err != nil {
		t.Fatalf("旁路日志没有落盘: %v", err)
	}
	if !strings.Contains(string(data), "重启开始") {
		t.Errorf("旁路日志内容不完整:\n%s", data)
	}
}

// TestRestartSelfStillStartsWhenStopFails 断言 stop 失败时仍会把应用拉起来。
//
// 真机上 stop 失败完全可能发生（应用中心正忙、状态不一致）。
// 那种情况下最坏的结果不是"没重启成功"，而是"停掉之后再也起不来"。
func TestRestartSelfStillStartsWhenStopFails(t *testing.T) {
	withTestShell(t)

	dir := restartTestDir(t)
	calls := filepath.Join(dir, "calls.log")
	cliPath := writeFakeCLI(t, dir, 1) // stop 退出码 1

	svc := NewService(Options{CLI: &ExecCLI{path: cliPath}, LogDir: dir})
	if err := svc.RestartSelf(context.Background()); err != nil {
		t.Fatalf("受理重启失败: %v", err)
	}

	got := waitForContent(t, calls, "start")
	if got != "stop\nstart\n" {
		t.Fatalf("stop 失败后仍必须执行 start，实际调用序列 = %q", got)
	}
}

// ---------------------------------------------------------------- 测试夹具

// withTestShell 把 restartShell 换成本机能用的 POSIX shell。
//
// 真机上恒为 /bin/sh；Windows 开发机上没有这个路径，但 Git Bash 提供了 sh，
// 指过去就能让"真跑一遍脚本"的断言在本机也生效 —— 否则这组测试只能靠
// 字符串断言，恰恰漏掉它最该抓住的那类问题。
//
// 改全局量所以不能 t.Parallel()。
func withTestShell(t *testing.T) {
	t.Helper()
	shell := ""
	if p, err := exec.LookPath("sh"); err == nil {
		shell = p
	} else if _, err := os.Stat("/bin/sh"); err == nil {
		shell = "/bin/sh"
	} else {
		t.Skip("本机没有 POSIX shell，跳过「真跑一遍脚本」的断言")
	}
	old := restartShell
	restartShell = shell
	t.Cleanup(func() { restartShell = old })
}

// restartTestDir 在包目录下建一个临时工作目录，返回它的**相对**路径。
//
// 为什么不用 t.TempDir() 的绝对路径（这是本机踩到的坑，值得写下来）：
// Go 是原生 Windows 程序，它 spawn 的 Git Bash sh 会对自己的 argv 做一遍
// 「Win32 ↔ POSIX」路径转换 —— 脚本里任何 /c/... 形式的绝对路径都会被改写
// 成 \c\...，于是脚本静默失败，表现为"等了 60 秒什么都没发生"。
// 相对路径不含盘符、不含挂载点，不会被转换。
//
// 代价是临时目录落在包目录下，所以必须 Cleanup 删掉；名字带前缀便于
// 万一没删干净时也一眼能认出是什么。
func restartTestDir(t *testing.T) string {
	t.Helper()
	name := "zz-restart-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if err := os.MkdirAll(name, 0o755); err != nil {
		t.Fatalf("创建测试工作目录失败: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(name) })
	return name
}

// writeFakeCLI 写一个假的 appcenter-cli：把收到的子命令追加到 <dir>/calls.log。
//
// 路径一律用正斜杠的相对形式：它会被拼进 shell 脚本，而 MSYS 的 sh 在
// 处理路径时对反斜杠并不宽容（见 restartTestDir 的注释）。
// stopExit 是 stop 子命令的退出码，用来模拟"stop 失败"。
func writeFakeCLI(t *testing.T, dir string, stopExit int) string {
	t.Helper()
	path := filepath.ToSlash(filepath.Join(dir, "appcenter-cli"))
	calls := filepath.ToSlash(filepath.Join(dir, "calls.log"))
	body := "#!/bin/sh\n" +
		"echo \"$1\" >> " + shellQuote(calls) + "\n" +
		"if [ \"$1\" = stop ]; then exit " + strconv.Itoa(stopExit) + "; fi\n" +
		"exit 0\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("写假命令行失败: %v", err)
	}
	return path
}

// waitForContent 轮询等待文件出现并包含某个子串。
//
// 必须轮询：脚本是脱离本进程跑的，而且开头还有一段刻意的交接等待，
// 调用返回时它才刚睡下。超时时间给得比脚本总时长宽裕得多，
// 免得在慢机器上变成随机失败。
func waitForContent(t *testing.T, path, want string) string {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	var last string
	for {
		data, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(data), want) {
			return string(data)
		}
		last = string(data)
		if time.Now().After(deadline) {
			t.Fatalf("等待 %q 超时（最后读到 %q，err=%v）", path, last, err)
			return last
		}
		time.Sleep(200 * time.Millisecond)
	}
}
