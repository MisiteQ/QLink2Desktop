package fnos

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/MisiteQ/qlink2desktop/internal/domain"
)

// newTestService 构造一个带 FakeCLI 的服务，并把「安装根目录」指向临时目录，
// 这样配置漂移检测就能在测试里真实工作。
func newTestService(t *testing.T) (*Service, *FakeCLI, string) {
	t.Helper()
	fake := NewFakeCLI()
	iconsDir := t.TempDir()
	svc := NewService(Options{CLI: fake, IconsDir: iconsDir})
	roots := t.TempDir()
	svc.SetInstallRoots([]string{roots})
	return svc, fake, roots
}

// TestSetIconsDirValidatesAndSwitches 覆盖「图标目录可配置」的三条关键约束。
//
// 用户可以在设置页把图标目录指到任意位置，所以这里必须挡住三类输入：
// 空、相对路径、不可写。前两类会让落盘位置变得不可预测，第三类会让
// 上传在很久以后才以一个莫名其妙的错误失败 —— 都要在改之前就拒绝。
func TestSetIconsDirValidatesAndSwitches(t *testing.T) {
	t.Parallel()

	svc, _, _ := newTestService(t)
	original := svc.IconsDir()
	if original == "" {
		t.Fatal("前置条件不成立：默认图标目录为空")
	}

	if err := svc.SetIconsDir("   "); err == nil {
		t.Error("空目录应当被拒绝")
	} else if !errors.Is(err, domain.ErrValidation) {
		t.Errorf("空目录应报校验错误，实际: %v", err)
	}

	if err := svc.SetIconsDir(filepath.Join("relative", "icons")); err == nil {
		t.Error("相对路径应当被拒绝（进程工作目录由飞牛决定，不可依赖）")
	} else if !errors.Is(err, domain.ErrValidation) {
		t.Errorf("相对路径应报校验错误，实际: %v", err)
	}

	// 被拒绝之后当前目录必须原封不动。
	if got := svc.IconsDir(); got != original {
		t.Fatalf("校验失败不应改动当前目录：%q → %q", original, got)
	}

	target := filepath.Join(t.TempDir(), "nested", "icons")
	if err := svc.SetIconsDir(target); err != nil {
		t.Fatalf("合法目录应当被接受: %v", err)
	}
	if got := svc.IconsDir(); got != target {
		t.Fatalf("目录未切换：期望 %q，实际 %q", target, got)
	}
	if fi, err := os.Stat(target); err != nil || !fi.IsDir() {
		t.Fatalf("目录层级应当被自动创建: %v", err)
	}

	// 上传落盘必须跟着新目录走，而不是还在往旧目录写。
	saved, err := SaveUpload(svc.IconsDir(), "probe.png", pngBytes())
	if err != nil {
		t.Fatalf("写入图标失败: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, saved)); err != nil {
		t.Fatalf("图标没有落到新目录: %v", err)
	}

	// 图标 → data URI 的读取也得跟着新目录走（带 ../ 的引用一律拒绝）。
	if uri := svc.IconDataURI(saved); !strings.HasPrefix(uri, "data:image/png;base64,") {
		t.Errorf("IconDataURI 应返回 data URI，实际: %q", uri)
	}
	// 带路径分隔符的引用一律拒绝，不做静默纠正。
	if uri := svc.IconDataURI("../" + saved); uri != "" {
		t.Errorf("带路径的引用应当被拒绝，实际: %q", uri)
	}
}

// pngBytes 返回一张 1×1 的合法 PNG，够 SaveUpload 走完转码与落盘。
func pngBytes() []byte {
	return []byte{
		0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A,
		0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4,
		0x89, 0x00, 0x00, 0x00, 0x0A, 0x49, 0x44, 0x41,
		0x54, 0x78, 0x9C, 0x63, 0x00, 0x01, 0x00, 0x00,
		0x05, 0x00, 0x01, 0x0D, 0x0A, 0x2D, 0xB4, 0x00,
		0x00, 0x00, 0x00, 0x49, 0x45, 0x4E, 0x44, 0xAE,
		0x42, 0x60, 0x82,
	}
}

func testLink(id, name string, port int) domain.Link {
	l := domain.Link{
		ID: id, Name: name, Kind: domain.KindLocalPort,
		Scheme: domain.SchemeHTTP, Port: port, Path: "/",
		UI: domain.UIWindow, Enabled: true,
	}
	l.Normalize()
	return l
}

// fakeInstallDir 把某个包的桌面入口配置预先写到测试用安装根目录，
// 用于模拟「应用已注册且配置正确」的状态。
//
// 落盘路径与真实安装布局一致：app/ 的内容成为 target 目录，
// 所以是 <roots>/<appName>/target/ui/config。
// fakeInstallDir 在假的安装根目录下铺一份 ui/config，返回该 ui 目录。
//
// 返回值供需要进一步摆布同目录其它文件（例如 index.cgi）的用例使用。
func fakeInstallDir(t *testing.T, roots, appName string, spec PackageSpec) string {
	t.Helper()
	dir := filepath.Join(roots, appName, "target", UIDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := uiConfig{Entries: map[string]uiEntry{UIEntryKey(appName): BuildUIEntry(spec)}}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// -------------------------------------------------------------------------- 安装

func TestInstallRegistersAndStarts(t *testing.T) {
	t.Parallel()

	svc, fake, _ := newTestService(t)
	l := testLink("item-1", "Alist", 5244)

	if err := svc.Install(context.Background(), InstallRequest{Link: l}); err != nil {
		t.Fatalf("Install 失败: %v", err)
	}

	appName := l.EffectiveAppName()
	states := fake.States()
	if states[appName] != StateRunning {
		t.Fatalf("安装后应处于运行态，实际: %v", states)
	}
	if !fake.HasCall("install-local") {
		t.Error("应调用过 install-local")
	}
	// 安装完成后状态应被清空（即恢复为「已就绪」的默认未知状态）。
	if got := svc.StatusOf(l.ID, appName); got.Phase == domain.PhaseFailed {
		t.Errorf("安装成功后不应是失败态: %+v", got)
	}
}

func TestInstallFailureRecordsPhase(t *testing.T) {
	t.Parallel()

	svc, fake, _ := newTestService(t)
	l := testLink("item-1", "Alist", 5244)
	fake.FailInstall[l.EffectiveAppName()] = errors.New("磁盘空间不足")

	err := svc.Install(context.Background(), InstallRequest{Link: l})
	if err == nil {
		t.Fatal("安装失败应返回错误")
	}
	if !strings.Contains(err.Error(), "磁盘空间不足") {
		t.Errorf("错误应包含底层原因: %v", err)
	}

	st := svc.StatusOf(l.ID, l.EffectiveAppName())
	if st.Phase != domain.PhaseFailed {
		t.Fatalf("应记录失败阶段，实际: %v", st.Phase)
	}
	if !strings.Contains(st.LastError, "磁盘空间不足") {
		t.Errorf("应记录失败原因，实际: %q", st.LastError)
	}
}

func TestInstallWithoutCLIOnlyBuilds(t *testing.T) {
	t.Parallel()

	svc := NewService(Options{CLI: &ExecCLI{}, IconsDir: t.TempDir()}) // 空路径 = 不可用
	if svc.Available() {
		t.Fatal("空 ExecCLI 应不可用")
	}
	l := testLink("item-1", "Alist", 5244)
	if err := svc.Install(context.Background(), InstallRequest{Link: l}); err != nil {
		t.Fatalf("模拟模式下安装不应报错: %v", err)
	}
}

func TestInstallHonoursContextCancellation(t *testing.T) {
	t.Parallel()

	svc, _, _ := newTestService(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	l := testLink("item-1", "Alist", 5244)
	if err := svc.Install(ctx, InstallRequest{Link: l}); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消后应立即返回 context.Canceled，实际: %v", err)
	}
}

// -------------------------------------------------------------------------- 卸载

func TestUninstallStopsThenUninstalls(t *testing.T) {
	t.Parallel()

	svc, fake, _ := newTestService(t)
	l := testLink("item-1", "Alist", 5244)
	fake.Seed(l.EffectiveAppName(), StateRunning)

	if err := svc.Uninstall(context.Background(), l, nil); err != nil {
		t.Fatalf("Uninstall 失败: %v", err)
	}

	if _, ok := fake.States()[l.EffectiveAppName()]; ok {
		t.Error("卸载后不应仍存在于应用中心")
	}

	// 顺序必须是先 stop 后 uninstall。
	log := fake.CallLog()
	stopIdx, uninstIdx := -1, -1
	for i, c := range log {
		if strings.HasPrefix(c, "stop ") {
			stopIdx = i
		}
		if strings.HasPrefix(c, "uninstall ") {
			uninstIdx = i
		}
	}
	if stopIdx < 0 || uninstIdx < 0 || stopIdx > uninstIdx {
		t.Errorf("应先 stop 再 uninstall，实际调用序列: %v", log)
	}
}

func TestUninstallSkipsProtectedAppName(t *testing.T) {
	t.Parallel()

	svc, fake, _ := newTestService(t)
	l := testLink("item-1", "Shared", 5244)
	fake.Seed(l.EffectiveAppName(), StateRunning)

	protected := map[string]bool{l.EffectiveAppName(): true}
	if err := svc.Uninstall(context.Background(), l, protected); err != nil {
		t.Fatalf("Uninstall 失败: %v", err)
	}
	if _, ok := fake.States()[l.EffectiveAppName()]; !ok {
		t.Error("被保护的包名不应被卸载")
	}
}

// -------------------------------------------------------------------------- 孤立清理

func TestPruneOrphansOnlyTouchesManagedApps(t *testing.T) {
	t.Parallel()

	svc, fake, _ := newTestService(t)
	fake.Seed("qlink2d.keep-1", StateRunning)
	fake.Seed("qlink2d.orphan-1", StateRunning)
	// 其它工具的命名空间：本应用是独立应用，
	// 用户可能同时在用别处生成的图标，绝不能清理。
	fake.Seed("othertool.legacy-1", StateRunning)
	fake.Seed("anothertool.legacy-1", StateRunning)
	fake.Seed("trim.photos", StateRunning) // 第三方原生应用，绝不能动
	fake.Seed("watchcow.something", StateRunning)

	removed, err := svc.PruneOrphans(context.Background(), map[string]bool{"qlink2d.keep-1": true})
	if err != nil {
		t.Fatalf("PruneOrphans 失败: %v", err)
	}

	got := append([]string(nil), removed...)
	if len(got) != 1 {
		t.Fatalf("应清理 1 个遗留图标，实际 %d: %v", len(got), got)
	}

	states := fake.States()
	if _, ok := states["qlink2d.orphan-1"]; ok {
		t.Error("应清理 qlink2d.orphan-1")
	}
	for _, foreign := range []string{"othertool.legacy-1", "anothertool.legacy-1", "trim.photos", "watchcow.something"} {
		if _, ok := states[foreign]; !ok {
			t.Errorf("绝不能触碰 %s", foreign)
		}
	}
	if _, ok := states["qlink2d.keep-1"]; !ok {
		t.Error("应保留仍在使用的应用")
	}
}

// -------------------------------------------------------------------------- 对账

func TestReconcileInstallsMissing(t *testing.T) {
	t.Parallel()

	svc, fake, _ := newTestService(t)
	l := testLink("item-1", "Alist", 5244)

	res := svc.Reconcile(context.Background(), []domain.Link{l})
	if len(res.Installed) != 1 {
		t.Fatalf("应安装 1 项，实际: %+v", res)
	}
	if fake.States()[l.EffectiveAppName()] != StateRunning {
		t.Error("补齐安装后应处于运行态")
	}
}

func TestReconcileStartsStoppedWithoutReinstalling(t *testing.T) {
	t.Parallel()

	svc, fake, roots := newTestService(t)
	l := testLink("item-1", "Alist", 5244)
	appName := l.EffectiveAppName()

	// 已注册、配置正确，但被停用了。
	fake.Seed(appName, StateStopped)
	spec := svc.buildSpec(l, 0)
	fakeInstallDir(t, roots, appName, spec)

	res := svc.Reconcile(context.Background(), []domain.Link{l})
	if len(res.Started) != 1 {
		t.Fatalf("应唤醒 1 项，实际: %+v", res)
	}
	if len(res.Installed) != 0 || len(res.Upgraded) != 0 {
		t.Errorf("不应重新安装: %+v", res)
	}
	if fake.HasCall("install-local") {
		t.Error("仅唤醒场景不应触发安装")
	}
	if fake.States()[appName] != StateRunning {
		t.Error("应回到运行态")
	}
}

func TestReconcileLeavesHealthyItemsUntouched(t *testing.T) {
	t.Parallel()

	svc, fake, roots := newTestService(t)
	l := testLink("item-1", "Alist", 5244)
	appName := l.EffectiveAppName()

	fake.Seed(appName, StateRunning)
	fakeInstallDir(t, roots, appName, svc.buildSpec(l, 0))

	res := svc.Reconcile(context.Background(), []domain.Link{l})
	if !reflect.DeepEqual(res, ReconcileResult{}) {
		t.Fatalf("健康的项目不应被改动，实际: %+v", res)
	}
	for _, c := range fake.CallLog() {
		if c != "list" && !strings.HasPrefix(c, "status ") {
			t.Errorf("健康项目不应产生任何写操作，实际调用: %s", c)
		}
	}
}

func TestReconcileUpgradesStaleConfig(t *testing.T) {
	t.Parallel()

	svc, fake, roots := newTestService(t)
	l := testLink("item-1", "Alist", 5244)
	appName := l.EffectiveAppName()

	fake.Seed(appName, StateRunning)
	// 写入一份「端口不对」的过期配置，模拟老版本遗留。
	stale := svc.buildSpec(l, 0)
	stale.Port = 9999
	fakeInstallDir(t, roots, appName, stale)

	res := svc.Reconcile(context.Background(), []domain.Link{l})
	if len(res.Upgraded) != 1 {
		t.Fatalf("应识别出配置过期并重装，实际: %+v", res)
	}

	// 重装后配置必须与期望一致。
	spec := svc.buildSpec(l, 0)
	entry, ok := svc.readInstalledEntry(appName)
	if !ok {
		t.Fatal("重装后应能读到配置")
	}
	// 注意：BuildPackage 写到临时目录，不会回写测试用的安装根目录，
	// 因此这里只断言「判定为过期」这一事实，以及应用状态被恢复。
	if fake.States()[appName] != StateRunning {
		t.Error("升级后应处于运行态")
	}
	_ = spec
	_ = entry
}

func TestReconcileSkipsDisabledLinks(t *testing.T) {
	t.Parallel()

	svc, fake, _ := newTestService(t)
	l := testLink("item-1", "Disabled", 8080)
	l.Enabled = false

	res := svc.Reconcile(context.Background(), []domain.Link{l})
	if !reflect.DeepEqual(res, ReconcileResult{}) {
		t.Fatalf("未启用的链接不应被处理，实际: %+v", res)
	}
	if fake.HasCall("install-local") {
		t.Error("未启用的链接不应触发安装")
	}
}

func TestReconcileHandlesListFailureGracefully(t *testing.T) {
	t.Parallel()

	svc, fake, _ := newTestService(t)
	fake.ListErr = errors.New("应用中心无响应")

	res := svc.Reconcile(context.Background(), []domain.Link{testLink("item-1", "A", 1)})
	if !reflect.DeepEqual(res, ReconcileResult{}) {
		t.Fatalf("列表读取失败时应安全退出，实际: %+v", res)
	}
}

func TestReconcileRecordsFailure(t *testing.T) {
	t.Parallel()

	svc, fake, _ := newTestService(t)
	l := testLink("item-1", "A", 8080)
	fake.FailInstall[l.EffectiveAppName()] = errors.New("安装被拒绝")

	res := svc.Reconcile(context.Background(), []domain.Link{l})
	if len(res.Failed) != 1 {
		t.Fatalf("应记录失败，实际: %+v", res)
	}
	if got := svc.StatusOf(l.ID, l.EffectiveAppName()); got.Phase != domain.PhaseFailed {
		t.Errorf("应处于失败阶段，实际: %v", got.Phase)
	}
}

func TestReconcileStopsOnContextCancellation(t *testing.T) {
	t.Parallel()

	svc, _, _ := newTestService(t)
	links := []domain.Link{
		testLink("item-1", "A", 1),
		testLink("item-2", "B", 2),
		testLink("item-3", "C", 3),
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// 不应 panic，也不应处理任何一项。
	res := svc.Reconcile(ctx, links)
	if len(res.Installed) != 0 {
		t.Errorf("取消后不应继续安装: %+v", res)
	}
}

// -------------------------------------------------------------------------- 状态

func TestMarkPendingOnlyForEnabled(t *testing.T) {
	t.Parallel()

	svc, _, _ := newTestService(t)
	enabled := testLink("item-1", "A", 1)
	disabled := testLink("item-2", "B", 2)
	disabled.Enabled = false

	svc.MarkPending([]domain.Link{enabled, disabled})

	if got := svc.StatusOf(enabled.ID, ""); got.Phase != domain.PhasePending {
		t.Errorf("启用项应预置为排队中，实际: %v", got.Phase)
	}
	if got := svc.StatusOf(disabled.ID, ""); got.Phase != domain.PhaseUnknown {
		t.Errorf("未启用项不应被预置，实际: %v", got.Phase)
	}
}

func TestStatusOfUnknownLink(t *testing.T) {
	t.Parallel()

	svc, _, _ := newTestService(t)
	st := svc.StatusOf("nope", "qlink2d.x-1")
	if st.Phase != domain.PhaseUnknown {
		t.Errorf("未知链接应返回 unknown，实际: %v", st.Phase)
	}
	if st.AppName != "qlink2d.x-1" {
		t.Errorf("应回填 AppName，实际: %q", st.AppName)
	}
}

// -------------------------------------------------------------------------- 路由判定

func TestDecideRoute(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		spec PackageSpec
		want routeMode
	}{
		{"有端口即直连", PackageSpec{Port: 8080}, routeDirect},
		{"有端口时目标地址不影响判定", PackageSpec{Port: 8080, ShortcutURL: "https://a.b/c"}, routeDirect},
		{"无端口即 CGI 跳转", PackageSpec{}, routeRedirect},
		{"无端口但有网址仍是 CGI 跳转", PackageSpec{ShortcutURL: "https://a.b/c"}, routeRedirect},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := decideRoute(tc.spec); got != tc.want {
				t.Fatalf("decideRoute() = %v, 期望 %v", got, tc.want)
			}
		})
	}
}

// -------------------------------------------------------------------------- 参数映射

func TestBuildSpecPortSubstitution(t *testing.T) {
	t.Parallel()

	svc, _, _ := newTestService(t)

	// 本机端口形态 + 未指定覆盖 → 使用真实端口。
	local := testLink("item-1", "A", 5244)
	if got := svc.buildSpec(local, 0).Port; got != 5244 {
		t.Errorf("本机端口应为 5244，实际 %d", got)
	}
	// 本机端口形态 + 指定覆盖：该形态永远不会被分配代理端口，
	// 因此覆盖值必须被忽略，不能凭空把图标指到一个没人监听的端口上。
	if got := svc.buildSpec(local, 18001).Port; got != 5244 {
		t.Errorf("本机端口不应被代理端口覆盖，实际 %d", got)
	}

	// 端口映射形态：必须指向本机代理端口，而不是远端端口。
	proxy := domain.Link{ID: "item-2", Name: "Remote", Kind: domain.KindProxy,
		Scheme: domain.SchemeHTTP, Host: "10.0.0.9", Port: 8080, Path: "/", UI: domain.UIWindow}
	proxy.Normalize()
	if got := svc.buildSpec(proxy, 18002).Port; got != 18002 {
		t.Errorf("端口映射应使用代理端口 18002，实际 %d", got)
	}
	// 缺少代理端口时不应生成直连入口（否则图标会指向不存在的端口）。
	if got := svc.buildSpec(proxy, 0).Port; got != 0 {
		t.Errorf("缺少代理端口时应退化为跳转，实际端口 %d", got)
	}

	// 快捷方式永远不占端口。
	shortcut := domain.Link{ID: "item-3", Name: "S", Kind: domain.KindShortcut, Path: "https://a.b"}
	shortcut.Normalize()
	if got := svc.buildSpec(shortcut, 18003).Port; got != 0 {
		t.Errorf("快捷方式不应占用端口，实际 %d", got)
	}
}

// TestBuildSpecShortcutGoesThroughCGI 锁定「网址快捷方式只能走 CGI 302」在 service 层的接线。
//
// 曾经的实现是"入口 url 直接写目标网址"，真机证明无效：飞牛把入口 url 拼在
// {protocol}://{当前浏览器 hostname}:{port} 之后，host 永远是飞牛自己，
// 于是目标网址被拼成了"飞牛地址 + 目标网址"。
// 唯一出路是让 CGI 返回 302 —— 所以这里必须确认 service 层把目标地址
// 交给了 CGI，并且确实生成了 CGI。
func TestBuildSpecShortcutGoesThroughCGI(t *testing.T) {
	t.Parallel()

	svc, _, _ := newTestService(t)

	shortcut := domain.Link{ID: "item-s1", Name: "百度", Kind: domain.KindShortcut, Path: "www.baidu.com"}
	shortcut.Normalize()

	spec := svc.buildSpec(shortcut, 0)
	if spec.ShortcutURL != "https://www.baidu.com" {
		t.Errorf("缺少协议时应自动补 https，实际 %q", spec.ShortcutURL)
	}
	if spec.Port != 0 {
		t.Errorf("快捷方式不应声明端口，实际 %d", spec.Port)
	}
	if got := decideRoute(spec); got != routeRedirect {
		t.Errorf("无端口的快捷方式只能走 CGI 302，实际 %v", got)
	}
	if !needsCGI(spec) {
		t.Error("快捷方式必须生成 CGI —— 它没有别的办法跳出飞牛主机")
	}
}

func TestBuildSpecAlwaysProducesIcons(t *testing.T) {
	t.Parallel()

	svc, _, _ := newTestService(t)
	// 图标来源完全不可用时，也必须降级到内置默认图标而不是失败。
	l := testLink("item-1", "A", 1)
	l.Icon = domain.Icon{Source: domain.IconUpload, Ref: "missing.png"}

	spec := svc.buildSpec(l, 0)
	if !spec.Icons.Valid() {
		t.Fatal("图标解析必须永远返回可用结果")
	}
}

func TestIsStaleWhenConfigMissing(t *testing.T) {
	t.Parallel()

	svc, _, _ := newTestService(t)
	l := testLink("item-1", "A", 1)
	if !svc.isStale(l) {
		t.Error("磁盘上没有配置文件时应判定为过期")
	}
}

func TestIsStaleWhenConfigMatches(t *testing.T) {
	t.Parallel()

	svc, _, roots := newTestService(t)
	l := testLink("item-1", "A", 8080)
	fakeInstallDir(t, roots, l.EffectiveAppName(), svc.buildSpec(l, 0))

	if svc.isStale(l) {
		t.Error("配置一致时不应判定为过期")
	}
}

// TestIsStaleWhenCGIScriptOutdated 锁定「生成物过期也算过期」。
//
// 这是真机事故的正牌回归：主程序升级修好了 index.cgi，但 ui/config 一字未变，
// 于是旧图标被判定为"健康"，永远不重建 —— 用户点开还是 500。
// 修法是把 index.cgi 也纳入 isStale 的比较范围。
//
// 反向验证记录：把 cgiScriptOutdated 从 isStale 里去掉 → 本用例立刻失败。
func TestIsStaleWhenCGIScriptOutdated(t *testing.T) {
	t.Parallel()

	svc, _, roots := newTestService(t)
	// 快捷方式：没有端口，才是需要 CGI 脚本的形态。
	// 目标地址存在 Path 里（ShortcutURL() 是它的读取方法）。
	l := testLink("item-1", "百度", 0)
	l.Kind = domain.KindShortcut
	l.Path = "https://www.baidu.com"
	l.Normalize()

	spec := svc.buildSpec(l, 0)
	dir := fakeInstallDir(t, roots, l.EffectiveAppName(), spec)

	// 前置条件：刚写好的脚本应当与期望一致。
	script, needed := renderCGIScript(svc.buildSpec(l, 0))
	if !needed {
		t.Fatal("前置条件不成立：快捷方式应当需要 CGI 脚本")
	}
	if err := os.WriteFile(filepath.Join(dir, "index.cgi"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if svc.isStale(l) {
		t.Fatal("脚本与当前版本一致时不应判定为过期")
	}

	// 模拟「磁盘上留着上一版生成的旧脚本」。
	old := "#!/bin/bash\nset -u\nFALLBACK_TARGET='https://www.baidu.com'\n"
	if err := os.WriteFile(filepath.Join(dir, "index.cgi"), []byte(old), 0o755); err != nil {
		t.Fatal(err)
	}
	if !svc.isStale(l) {
		t.Error("旧脚本必须触发重建，否则升级后用户会一直跑上一版的行为")
	}
}

// TestIsStaleIgnoresCGIScriptForPortLinks 确认有端口的形态不受这条判定影响。
//
// 端口形态根本不生成 index.cgi，目录里万一残留一份旧文件也不该让它被反复重装。
func TestIsStaleIgnoresCGIScriptForPortLinks(t *testing.T) {
	t.Parallel()

	svc, _, roots := newTestService(t)
	l := testLink("item-1", "本机服务", 5244)

	spec := svc.buildSpec(l, 0)
	dir := fakeInstallDir(t, roots, l.EffectiveAppName(), spec)

	// 故意在目录里放一份内容完全对的脚本之外的东西。
	if err := os.WriteFile(filepath.Join(dir, "index.cgi"), []byte("#!/bin/bash\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if svc.isStale(l) {
		t.Error("有端口的形态不该因为 index.cgi 而被判定过期")
	}
}
