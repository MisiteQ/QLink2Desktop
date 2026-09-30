package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MisiteQ/qlink2desktop/internal/domain"
	"github.com/MisiteQ/qlink2desktop/internal/fnos"
	"github.com/MisiteQ/qlink2desktop/internal/proxy"
	"github.com/MisiteQ/qlink2desktop/internal/store"
)

/* ------------------------------------------------------------------ 测试脚手架 */

type testEnv struct {
	store       *store.Store
	cli         *fnos.FakeCLI
	svc         *fnos.Service
	proxy       *proxy.Manager
	coord       *Coordinator
	installRoot string
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// newTestEnv 搭一套完全内存 / 临时目录的运行环境。
//
// base 用不同的端口起点区分各个用例，避免并行运行时互相抢端口。
func newTestEnv(t *testing.T, base int) *testEnv {
	t.Helper()

	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "data"))
	if err != nil {
		t.Fatalf("打开存储失败: %v", err)
	}

	installRoot := filepath.Join(root, "apps")
	if err := os.MkdirAll(installRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	cli := fnos.NewFakeCLI()
	cli.InstallRoot = installRoot

	svc := fnos.NewService(fnos.Options{
		CLI:             cli,
		IconsDir:        filepath.Join(root, "icons"),
		AllowRemoteIcon: false,
	})
	// 让「配置是否过期」的判定能真正读到安装结果，
	// 否则 isStale 恒为真，测试就测不出重复安装问题。
	svc.SetInstallRoots([]string{installRoot})

	pm := proxy.NewManager(proxy.WithLogger(discardLogger()))
	coord := NewCoordinator(st, svc, pm, base, discardLogger())
	t.Cleanup(coord.Stop)

	return &testEnv{
		store:       st,
		cli:         cli,
		svc:         svc,
		proxy:       pm,
		coord:       coord,
		installRoot: installRoot,
	}
}

// save 写入一条链接并返回落盘后的定义。
func (e *testEnv) save(t *testing.T, l domain.Link) domain.Link {
	t.Helper()
	if l.CreatedAt.IsZero() {
		l.CreatedAt = time.Now()
	}
	saved, err := e.store.UpsertLink(l)
	if err != nil {
		t.Fatalf("写入链接失败: %v", err)
	}
	return saved
}

// desktopEntry 读出已安装应用的桌面入口配置。
//
// 路径为 <安装根>/<appname>/ui/config：app/ 的内容安装后成为 target 目录，
// 因此 desktop_uidir=ui 对应的入口文件就在安装目录的 ui/ 下。
// 入口键为 <appname>.main，需与 manifest 的 desktop_applaunchname 一致。
func (e *testEnv) desktopEntry(t *testing.T, appName string) map[string]any {
	t.Helper()

	path := filepath.Join(e.installRoot, appName, "ui", "config")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取桌面入口 %s 失败: %v", path, err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("解析桌面入口失败: %v", err)
	}
	var entries map[string]map[string]any
	if err := json.Unmarshal(raw[".url"], &entries); err != nil {
		t.Fatalf("解析 .url 段失败: %v", err)
	}
	entry, ok := entries[appName+".main"]
	if !ok {
		t.Fatalf("桌面入口里没有 %s.main，实际有 %v", appName, keysOf(entries))
	}
	return entry
}

func keysOf(m map[string]map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func countCalls(calls []string, prefix string) int {
	n := 0
	for _, c := range calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

/* ------------------------------------------------------------------ 纯函数 */

func TestNeedsProxy(t *testing.T) {
	cases := []struct {
		name string
		link domain.Link
		want bool
	}{
		{"停用的端口映射不需要代理", domain.Link{Kind: domain.KindProxy, Enabled: false}, false},
		{"启用的端口映射需要代理", domain.Link{Kind: domain.KindProxy, Enabled: true}, true},
		{"本机端口永不走代理", domain.Link{Kind: domain.KindLocalPort, Enabled: true}, false},
		{"本机端口带路径也不走代理", domain.Link{Kind: domain.KindLocalPort, Enabled: true, Path: "/x"}, false},
		{"快捷方式永不占端口", domain.Link{Kind: domain.KindShortcut, Enabled: true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := needsProxy(tc.link); got != tc.want {
				t.Fatalf("needsProxy = %v，期望 %v", got, tc.want)
			}
		})
	}
}

func TestStablePortStartIsDeterministicAndInRange(t *testing.T) {
	const base = 18000

	first := stablePortStart(base, "link-abc")
	for i := 0; i < 50; i++ {
		if got := stablePortStart(base, "link-abc"); got != first {
			t.Fatalf("同一 ID 的起始端口应当稳定，得到 %d 与 %d", first, got)
		}
	}
	if first < base || first >= base+portSpan {
		t.Fatalf("起始端口 %d 越出 [%d, %d)", first, base, base+portSpan)
	}
	if stablePortStart(base, "link-xyz") == first {
		t.Log("两个不同 ID 恰好落在同一槽位，属于可接受的哈希碰撞")
	}
}

func TestSortLinksForAllocationIsStable(t *testing.T) {
	now := time.Now()
	links := []domain.Link{
		{ID: "c", CreatedAt: now.Add(2 * time.Second)},
		{ID: "a", CreatedAt: now},
		{ID: "b", CreatedAt: now.Add(time.Second)},
	}
	sorted := sortLinksForAllocation(links)
	want := []string{"a", "b", "c"}
	for i, id := range want {
		if sorted[i].ID != id {
			t.Fatalf("排序结果 %v，期望 %v", idsOf(sorted), want)
		}
	}
}

func idsOf(links []domain.Link) []string {
	out := make([]string, 0, len(links))
	for _, l := range links {
		out = append(out, l.ID)
	}
	return out
}

/* ------------------------------------------------------------------ 编排 */

func TestSyncProxyLinkCreatesRouteAndIcon(t *testing.T) {
	env := newTestEnv(t, 21900)
	ctx := context.Background()

	saved := env.save(t, domain.Link{
		Name: "远端影音", Kind: domain.KindProxy,
		Scheme: domain.SchemeHTTPS, Host: "10.0.0.9", Port: 8096, Path: "/web",
		UI: domain.UIWindow, AllUsers: true, Enabled: true,
	})

	if err := env.coord.Sync(ctx, saved.ID); err != nil {
		t.Fatalf("同步失败: %v", err)
	}

	port := env.coord.ProxyPort(saved.ID)
	if port <= 0 {
		t.Fatal("端口映射形态应当建立本机代理监听")
	}
	if !env.proxy.IsRunning(saved.ID) {
		t.Fatal("代理管理器报告该路由未运行")
	}

	appName := saved.EffectiveAppName()
	if _, ok := env.cli.States()[appName]; !ok {
		t.Fatalf("桌面图标未注册，已注册项：%v", env.cli.States())
	}

	entry := env.desktopEntry(t, appName)
	if got := entry["port"]; got != strconv.Itoa(port) {
		t.Fatalf("桌面入口端口 = %v，期望代理端口 %d", got, port)
	}
	// 内置代理不做 TLS 终止，桌面入口必须是明文 http，
	// 否则浏览器会拿 https 去连一个明文端口，直接握手失败。
	if got := entry["protocol"]; got != domain.SchemeHTTP {
		t.Fatalf("桌面入口协议 = %v，期望 http", got)
	}
	if got := entry["url"]; got != "/web" {
		t.Fatalf("桌面入口路径 = %v，期望 /web", got)
	}
}

func TestSyncHealthyLinkDoesNotReinstall(t *testing.T) {
	env := newTestEnv(t, 22000)
	ctx := context.Background()

	saved := env.save(t, domain.Link{
		Name: "本机服务", Kind: domain.KindLocalPort,
		Scheme: domain.SchemeHTTP, Port: 34567, Path: "/",
		UI: domain.UIWindow, AllUsers: true, Enabled: true,
	})

	if err := env.coord.Sync(ctx, saved.ID); err != nil {
		t.Fatalf("首次同步失败: %v", err)
	}
	afterFirst := countCalls(env.cli.CallLog(), "install-local")
	if afterFirst != 1 {
		t.Fatalf("首次同步应安装一次，实际 %d 次", afterFirst)
	}

	if err := env.coord.Sync(ctx, saved.ID); err != nil {
		t.Fatalf("二次同步失败: %v", err)
	}
	if after := countCalls(env.cli.CallLog(), "install-local"); after != afterFirst {
		t.Fatalf("配置未变化时不应重复安装，安装次数从 %d 变成 %d", afterFirst, after)
	}
}

// TestSyncLocalPortNeverOccupiesProxyPort 锁定「本机端口形态不套代理」。
func TestSyncLocalPortNeverOccupiesProxyPort(t *testing.T) {
	env := newTestEnv(t, 22200)

	saved := env.save(t, domain.Link{
		Name: "本机服务", Kind: domain.KindLocalPort,
		Scheme: domain.SchemeHTTPS, Port: 34668, Path: "/app",
		UI:      domain.UIWindow,
		Enabled: true, AllUsers: true,
	})

	if err := env.coord.Sync(context.Background(), saved.ID); err != nil {
		t.Fatalf("同步失败: %v", err)
	}

	// 本机端口形态永远直连，不拉起内置代理 —— 少一次转发、少一个故障点。
	if port := env.coord.ProxyPort(saved.ID); port != 0 {
		t.Fatalf("本机端口不应占用代理端口，实际 %d", port)
	}
	entry := env.desktopEntry(t, saved.EffectiveAppName())
	if got := entry["port"]; got != "34668" {
		t.Fatalf("桌面入口应直连真实端口 34668，实际 %v", got)
	}
	// 协议保持用户填写的 https：不再为了代理而降级成明文。
	if got := entry["protocol"]; got != domain.SchemeHTTPS {
		t.Fatalf("桌面入口协议 = %v，期望 https", got)
	}
}

// TestSyncShortcutUsesCGI302 锁定「网址快捷方式点开后能到达目标站点」。
//
// 这个用例改过三次，三次都对应一次真机事故，值得记住：
//  1. 最早断言入口必须在 /cgi/ThirdParty/... 上 —— 那是 CGI 中继地址，
//     外网经飞牛 Connect 打开时会整页报错；
//  2. 于是改成断言入口 url 必须是目标网址本身（外链直达）；
//     真机复测证明这条路也不通：飞牛的拼接规则是
//     {protocol}://{当前浏览器 hostname}:{port}{url}，host 永远是飞牛自己，
//     目标网址被拼成了"飞牛地址 + 目标网址"；
//  3. 现在回到 CGI —— 入口配置表达不了飞牛以外的主机，只能靠 CGI 返回 302。
//
// 所以这里锁定的不是"入口地址是什么"，而是"有没有一条能真正跳出飞牛的通路"。
func TestSyncShortcutUsesCGI302(t *testing.T) {
	env := newTestEnv(t, 22300)

	saved := env.save(t, domain.Link{
		Name: "公司 Wiki", Kind: domain.KindShortcut,
		Path: "https://wiki.example.com/", Enabled: true, UI: domain.UITab,
	})

	if err := env.coord.Sync(context.Background(), saved.ID); err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	if port := env.coord.ProxyPort(saved.ID); port != 0 {
		t.Fatalf("快捷方式不应占用代理端口，实际 %d", port)
	}

	entry := env.desktopEntry(t, saved.EffectiveAppName())
	if got, _ := entry["url"].(string); !strings.HasPrefix(got, "/cgi/ThirdParty/") {
		t.Fatalf("快捷方式应走 CGI 302，实际 url=%v", got)
	}
	if _, has := entry["port"]; has {
		t.Fatalf("CGI 入口不应声明端口，实际 %v", entry["port"])
	}
	if _, has := entry["protocol"]; has {
		t.Fatalf("CGI 入口不应声明协议（交给系统自适应），实际 %v", entry["protocol"])
	}
}

func TestSetEnabledFalseUninstallsIcon(t *testing.T) {
	env := newTestEnv(t, 22400)
	ctx := context.Background()

	saved := env.save(t, domain.Link{
		Name: "待停用", Kind: domain.KindLocalPort, Scheme: domain.SchemeHTTP,
		Port: 34669, Path: "/", Enabled: true, UI: domain.UIWindow,
	})
	if err := env.coord.Sync(ctx, saved.ID); err != nil {
		t.Fatalf("同步失败: %v", err)
	}

	if err := env.coord.SetEnabled(ctx, saved.ID, false); err != nil {
		t.Fatalf("停用失败: %v", err)
	}

	if _, ok := env.cli.States()[saved.EffectiveAppName()]; ok {
		t.Fatal("停用后桌面图标应当被注销")
	}
	if !env.cli.HasCall("uninstall ") {
		t.Fatalf("应当调用过卸载，调用序列：%v", env.cli.CallLog())
	}

	view, ok := env.coord.View(saved.ID)
	if !ok {
		t.Fatal("停用后链接定义仍然应当存在")
	}
	if view.Status.Phase != domain.PhaseStopped {
		t.Fatalf("停用后的状态应为 stopped，实际 %s", view.Status.Phase)
	}
}

func TestRemoveStopsProxyAndUninstalls(t *testing.T) {
	env := newTestEnv(t, 22500)
	ctx := context.Background()

	saved := env.save(t, domain.Link{
		Name: "将被删除", Kind: domain.KindProxy, Scheme: domain.SchemeHTTP,
		Host: "192.168.1.50", Port: 5000, Path: "/", Enabled: true, UI: domain.UIWindow,
	})
	if err := env.coord.Sync(ctx, saved.ID); err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	port := env.coord.ProxyPort(saved.ID)
	if port <= 0 {
		t.Fatal("前置条件不成立：代理未建立")
	}

	if err := env.coord.Remove(ctx, saved.ID); err != nil {
		t.Fatalf("删除失败: %v", err)
	}

	if _, ok := env.store.GetLink(saved.ID); ok {
		t.Fatal("删除后链接定义不应残留")
	}
	if env.proxy.IsRunning(saved.ID) {
		t.Fatal("删除后代理监听应当停止")
	}
	if _, ok := env.cli.States()[saved.EffectiveAppName()]; ok {
		t.Fatal("删除后桌面图标应当被注销")
	}
}

func TestPortAllocationIsStableAcrossRuns(t *testing.T) {
	ctx := context.Background()
	now := time.Now()

	links := []domain.Link{
		{ID: "stable-a", Name: "A", Kind: domain.KindProxy, Scheme: domain.SchemeHTTP,
			Host: "10.0.0.1", Port: 8080, Path: "/", Enabled: true, UI: domain.UIWindow,
			CreatedAt: now},
		{ID: "stable-b", Name: "B", Kind: domain.KindProxy, Scheme: domain.SchemeHTTP,
			Host: "10.0.0.2", Port: 8081, Path: "/", Enabled: true, UI: domain.UIWindow,
			CreatedAt: now.Add(time.Second)},
		{ID: "stable-c", Name: "C", Kind: domain.KindProxy, Scheme: domain.SchemeHTTP,
			Host: "10.0.0.3", Port: 8082, Path: "/", Enabled: true, UI: domain.UIWindow,
			CreatedAt: now.Add(2 * time.Second)},
	}

	run := func() map[string]int {
		env := newTestEnv(t, 23000)
		for _, l := range links {
			env.save(t, l)
		}
		got := map[string]int{}
		// 按稳定顺序同步，模拟两次独立启动时的分配过程。
		for _, l := range links {
			if err := env.coord.Sync(ctx, l.ID); err != nil {
				t.Fatalf("同步 %s 失败: %v", l.ID, err)
			}
			got[l.ID] = env.coord.ProxyPort(l.ID)
		}
		for id, port := range got {
			if port <= 0 {
				t.Fatalf("%s 未分配到端口", id)
			}
		}
		env.coord.Stop()
		return got
	}

	first := run()
	second := run()

	for id, port := range first {
		if second[id] != port {
			t.Fatalf("两次运行的端口分配不一致：%s %d vs %d。"+
				"端口漂移会导致每次开机都触发一轮图标重装", id, port, second[id])
		}
	}
}

func TestProtectedNamesExcludesDisabledAndSelf(t *testing.T) {
	env := newTestEnv(t, 24000)

	keep := env.save(t, domain.Link{
		Name: "保持启用", Kind: domain.KindLocalPort, Scheme: domain.SchemeHTTP,
		Port: 34670, Path: "/", Enabled: true, UI: domain.UIWindow,
	})
	drop := env.save(t, domain.Link{
		Name: "已停用", Kind: domain.KindLocalPort, Scheme: domain.SchemeHTTP,
		Port: 34671, Path: "/", Enabled: false, UI: domain.UIWindow,
	})

	protected := env.coord.protectedNames()
	if !protected[keep.EffectiveAppName()] {
		t.Fatal("启用中的链接应当出现在保护集合里")
	}
	if protected[drop.EffectiveAppName()] {
		t.Fatal("停用的链接不应出现在保护集合里")
	}
	if protected[keep.EffectiveAppName()] && protected[drop.EffectiveAppName()] {
		t.Fatal("保护集合语义错误")
	}
}

func TestReconcileLeavesHealthyItemsUntouched(t *testing.T) {
	env := newTestEnv(t, 25000)
	ctx := context.Background()

	saved := env.save(t, domain.Link{
		Name: "健康项", Kind: domain.KindLocalPort, Scheme: domain.SchemeHTTP,
		Port: 34672, Path: "/", Enabled: true, UI: domain.UIWindow,
	})
	if err := env.coord.Sync(ctx, saved.ID); err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	before := countCalls(env.cli.CallLog(), "install-local")

	result := env.coord.Reconcile(ctx)

	if after := countCalls(env.cli.CallLog(), "install-local"); after != before {
		t.Fatalf("对账不应重装已就绪的项：安装次数 %d → %d", before, after)
	}
	if len(result.Installed)+len(result.Upgraded)+len(result.Started)+len(result.Failed) != 0 {
		t.Fatalf("已就绪时不应产生任何对账动作，实际 %+v", result)
	}
}

func TestReconcileNeverPrunesAnything(t *testing.T) {
	env := newTestEnv(t, 26000)

	// 对账是启动时的自动行为，绝不允许夹带任何卸载动作——
	// 即便是 qlink2d. 命名空间下的孤儿也不行（用户可能只是重装了本应用，
	// 数据还没导回来）。清理只能由 CleanupOrphans 显式触发。
	env.cli.Seed("qlink2d.legacy-abcdef", fnos.StateRunning)
	env.cli.Seed("trim.photos", fnos.StateRunning)
	env.cli.Seed("watchcow.dashboard", fnos.StateRunning)
	before := countCalls(env.cli.CallLog(), "uninstall")

	env.coord.Reconcile(context.Background())

	if after := countCalls(env.cli.CallLog(), "uninstall"); after != before {
		t.Fatalf("对账不应产生任何卸载调用：%d → %d", before, after)
	}
	states := env.cli.States()
	for _, name := range []string{"qlink2d.legacy-abcdef", "trim.photos", "watchcow.dashboard"} {
		if _, ok := states[name]; !ok {
			t.Fatalf("对账绝不能卸载 %s", name)
		}
	}
}

func TestCleanupOrphansOnlyTouchesOwnNamespace(t *testing.T) {
	env := newTestEnv(t, 26100)

	env.cli.Seed("qlink2d.legacy-abcdef", fnos.StateRunning)
	env.cli.Seed("othertool.from-old-project", fnos.StateRunning) // 其它工具的命名空间，绝不能动
	env.cli.Seed("trim.photos", fnos.StateRunning)
	env.cli.Seed("watchcow.dashboard", fnos.StateRunning)

	removed, err := env.coord.CleanupOrphans(context.Background())
	if err != nil {
		t.Fatalf("CleanupOrphans 失败: %v", err)
	}

	if len(removed) != 1 || removed[0] != "qlink2d.legacy-abcdef" {
		t.Fatalf("应只清理 qlink2d.legacy-abcdef，实际: %v", removed)
	}
	states := env.cli.States()
	for _, name := range []string{"othertool.from-old-project", "trim.photos", "watchcow.dashboard"} {
		if _, ok := states[name]; !ok {
			t.Fatalf("绝不能清理其它项目的应用 %s", name)
		}
	}
}

func TestViewsReportsStoppedPhaseForDisabledLink(t *testing.T) {
	env := newTestEnv(t, 27000)

	saved := env.save(t, domain.Link{
		Name: "停用项", Kind: domain.KindLocalPort, Scheme: domain.SchemeHTTP,
		Port: 34673, Path: "/", Enabled: false, UI: domain.UIWindow,
	})

	view, ok := env.coord.View(saved.ID)
	if !ok {
		t.Fatal("视图应当存在")
	}
	if view.Status.Phase != domain.PhaseStopped {
		t.Fatalf("停用链接的状态应为 stopped，实际 %s", view.Status.Phase)
	}
	if view.Status.AppName != saved.EffectiveAppName() {
		t.Fatalf("视图里的包名应为 %s，实际 %s", saved.EffectiveAppName(), view.Status.AppName)
	}
}
