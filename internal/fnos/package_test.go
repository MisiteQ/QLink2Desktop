package fnos

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/MisiteQ/qlink2desktop/internal/domain"
)

func baseSpec(appName string) PackageSpec {
	return PackageSpec{
		AppName: appName,
		Title:   "测试应用",
		Desc:    "由测试生成的入口",
		Scheme:  domain.SchemeHTTP,
		Path:    "/",
		UI:      domain.UIWindow,
		Icons:   DefaultIconSet(),
	}
}

func buildInto(t *testing.T, spec PackageSpec) string {
	t.Helper()
	dir := t.TempDir()
	if err := BuildPackage(dir, spec); err != nil {
		t.Fatalf("BuildPackage 失败: %v", err)
	}
	return dir
}

// readEntry 读取包内桌面入口配置。
//
// 路径取 app/ui/config：desktop_uidir=ui 时官方规定入口就在 app/{desktop_uidir}/ 下。
func readEntry(t *testing.T, dir, appName string) uiEntry {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "app", UIDir, "config"))
	if err != nil {
		t.Fatalf("读取 app/ui/config 失败: %v", err)
	}
	var cfg uiConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("解析 app/ui/config 失败: %v\n内容: %s", err, data)
	}
	entry, ok := cfg.Entries[UIEntryKey(appName)]
	if !ok {
		t.Fatalf("app/ui/config 中缺少 %s 入口: %s", UIEntryKey(appName), data)
	}
	return entry
}

// parseManifest 把包内的 manifest 解析成 key → value。
//
// 不直接对整段文本做子串匹配：manifest 的键按官方模板左对齐补空格，
// 子串匹配会被空格数量绑死，改一次对齐格式就得改一遍断言。
func parseManifest(t *testing.T, dir string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "manifest"))
	if err != nil {
		t.Fatalf("读取 manifest 失败: %v", err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("manifest 里出现非 key = value 行: %q", line)
		}
		out[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return out
}

func TestBuildPackageRejectsInvalidAppName(t *testing.T) {
	t.Parallel()

	spec := baseSpec("!!bad!!")
	if err := BuildPackage(t.TempDir(), spec); err == nil {
		t.Fatal("非法包名应被拒绝")
	}
}

func TestBuildPackageRejectsMissingIcons(t *testing.T) {
	t.Parallel()

	spec := baseSpec("qlink2d.ok-1")
	spec.Icons = IconSet{}
	if err := BuildPackage(t.TempDir(), spec); err == nil {
		t.Fatal("缺少图标应被拒绝")
	}
}

func TestBuildPackagePortMode(t *testing.T) {
	t.Parallel()

	spec := baseSpec("qlink2d.port-1")
	spec.Port = 5244
	spec.Title = "Alist"
	if err := domain.ValidateAppName(spec.AppName); err != nil {
		t.Fatal(err)
	}

	dir := buildInto(t, spec)

	// 1) manifest —— 字段集必须与官方 Manifest 文档一致。
	mf := parseManifest(t, dir)
	want := map[string]string{
		"appname":               "qlink2d.port-1",
		"display_name":          "Alist",
		"platform":              ManifestPlatform(),
		"source":                "thirdparty",
		"maintainer":            Maintainer,
		"maintainer_url":        MaintainerURL,
		"distributor":           Distributor,
		"service_port":          "5244",
		"checkport":             "false",
		"desktop_uidir":         "ui",
		"desktop_applaunchname": "qlink2d.port-1.main",
	}
	for key, value := range want {
		if mf[key] != value {
			t.Errorf("manifest[%s] = %q，期望 %q", key, mf[key], value)
		}
	}
	// 官方 manifest 文档里没有 arch 字段，不应夹带。
	if _, ok := mf["arch"]; ok {
		t.Error("manifest 不应包含非官方的 arch 字段")
	}
	// 入口 ID 必须与 desktop_applaunchname 完全一致，否则点卡片打不开。
	if mf["desktop_applaunchname"] != UIEntryKey(mf["appname"]) {
		t.Errorf("desktop_applaunchname(%q) 与入口 ID(%q) 不一致",
			mf["desktop_applaunchname"], UIEntryKey(mf["appname"]))
	}

	// 2) app/ui/config
	entry := readEntry(t, dir, spec.AppName)
	if entry.Protocol != "http" || entry.Port != "5244" || entry.URL != "/" {
		t.Errorf("端口模式入口不正确: %+v", entry)
	}
	if !entry.AllUsers {
		t.Error("端口模式必须放开为全员可见（否则 FN Connect 外网访问会被拒）")
	}
	if entry.Type != string(domain.UIWindow) {
		t.Errorf("窗口形态不正确: %q", entry.Type)
	}
	if entry.Icon != IconPlaceholder {
		t.Errorf("图标路径应为官方的 %q，实际 %q", IconPlaceholder, entry.Icon)
	}

	// 3) 端口模式不应生成 CGI
	if _, err := os.Stat(filepath.Join(dir, "app", UIDir, "index.cgi")); err == nil {
		t.Error("端口模式不应生成 index.cgi")
	}

	// 4) 目录完整性
	assertTree(t, dir)
}

// TestBuildPackageShortcutUsesCGI302 锁定「网址快捷方式 = CGI 302」这条产品约束。
//
// 上一版实现把入口 url 写成**绝对外链**（routeExternal），理由是"书签不该占用
// 飞牛的网关"。真机复测证明这条路走不通 —— 飞牛桌面对入口地址的拼接规则是
//
//	{protocol}://{当前浏览器 hostname}:{port}{url}
//
// host 永远是飞牛自己（内网是 NAS 的 IP，走 FN Connect 是飞牛的中继域名），
// 入口配置里没有地方能指定别的主机。于是 "https://www.baidu.com" 被拼成了
// "http://<NAS>:<端口>https://www.baidu.com" —— 用户截图里的"飞牛地址 + 目标网址"
// 就是这个形态。
//
// 所以这里锁定三件事：
//  1. 入口 url 指向 /cgi/ThirdParty/<app>/index.cgi/...，不是外链；
//  2. 生成的 index.cgi **自带真正的 302 响应头**，不依赖主程序是否在跑；
//  3. 兜底页面里仍然带着目标地址，万一 302 头被中间层吞掉还有二次机会。
func TestBuildPackageShortcutUsesCGI302(t *testing.T) {
	t.Parallel()

	spec := baseSpec("qlink2d.short-1")
	spec.Port = 0
	spec.ShortcutURL = "https://example.com/dashboard"
	spec.UI = domain.UITab

	dir := buildInto(t, spec)

	entry := readEntry(t, dir, spec.AppName)
	if !strings.HasPrefix(entry.URL, "/cgi/ThirdParty/") {
		t.Errorf("快捷方式应走 CGI 302，实际 url=%q", entry.URL)
	}
	if entry.Port != "" || entry.Protocol != "" {
		t.Errorf("CGI 入口不该声明端口/协议，实际 port=%q protocol=%q", entry.Port, entry.Protocol)
	}

	cgiPath := filepath.Join(dir, "app", UIDir, "index.cgi")
	cgi, err := os.ReadFile(cgiPath)
	if err != nil {
		t.Fatalf("快捷方式必须生成 index.cgi: %v", err)
	}
	body := string(cgi)
	if !strings.Contains(body, "Status: 302") {
		t.Errorf("CGI 必须自己返回 302 响应头:\n%s", body)
	}
	if !strings.Contains(body, "https://example.com/dashboard") {
		t.Errorf("CGI 应包含目标地址:\n%s", body)
	}

	// 可执行位必须设置，否则飞牛网关无法执行它（Windows 上该位不生效，跳过）。
	if hasExecBit {
		fi, err := os.Stat(cgiPath)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm()&0o111 == 0 {
			t.Errorf("index.cgi 缺少可执行位: %v", fi.Mode())
		}
	}
}

// TestCGIScriptRunsWithoutTrimEnv 是本次真机事故的正牌回归守卫。
//
// 之前那版 index.cgi 在开头写了 set -u，却又直接在引号里展开 ${TRIM_APPDEST}。
// 而 CGI 环境下那批 TRIM_* 变量**根本不会被设置**：bash 以
// "unbound variable" 立刻终止，一行输出都没有。飞牛网关拿不到任何响应头，
// 只能回 500 —— 用户看到的就是「网页出错了」。
//
// 这类 bug 的性质是：**静态检查一条都拦不住**。
//
//	· `bash -n` 语法检查通过（语法完全合法）；
//	· go vet / gofmt 通过（Go 侧毫无问题）；
//	· 包内容断言也通过 —— 断言只看了"文件里有没有那几行字符串"。
//
// 唯一能拦住它的办法是**真的把脚本跑一遍**，而且必须跑得像网关那样：
// 用 env -i 清空环境（TRIM_* 一个都不给），然后看它是不是还能交出一份
// 合法的 CGI 响应。
//
// 这条注释同时也是给以后改这个文件的人的警告：
// 想往模板里加环境变量引用，先问一句「网关真的会给我这个变量吗」，
// 写了就要写成 ${VAR:-}。
func TestCGIScriptRunsWithoutTrimEnv(t *testing.T) {
	t.Parallel()
	if !canRunBash(t) {
		t.Skip("当前平台没有 bash，跳过脚本执行验证")
	}

	spec := baseSpec("qlink2d.cgi-run")
	spec.Port = 0
	spec.ShortcutURL = "https://example.com/target?a=1&b=2"

	dir := buildInto(t, spec)
	cgiPath := filepath.Join(dir, "app", UIDir, "index.cgi")

	// env -i：连 PATH 都没有，正是"最坏情况"的 CGI 环境。
	cmd := exec.Command("env", "-i", filepath.ToSlash(cgiPath))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("index.cgi 在空环境下执行失败（真机上这就是 HTTP 500）: %v\n输出:\n%s", err, out)
	}
	text := string(out)

	if strings.Contains(text, "unbound variable") {
		t.Errorf("脚本引用了未定义变量（典型的 set -u 事故）:\n%s", text)
	}
	if !strings.Contains(text, "Status: 302") {
		t.Errorf("必须输出 302 状态行，实际输出:\n%s", text)
	}
	if !strings.Contains(text, "Location: https://example.com/target?a=1&b=2") {
		t.Errorf("Location 头不对，实际输出:\n%s", text)
	}
	// 响应头与响应体之间必须有空行，否则网关会把整个输出当成头解析。
	if !strings.Contains(text, "\r\n\r\n") && !strings.Contains(text, "\n\n") {
		t.Errorf("缺少「头结束」的空行:\n%s", text)
	}
}

// TestCGIScriptEmptyTargetReplies400 确认「没有目标地址」也是一个**合法响应**。
//
// 这条守护的是一个容易被忽略的点：失败形态必须是"有输出的失败"。
// 空目标时不输出任何东西，和上面的 set -u 事故在网关看来是一模一样的 500。
func TestCGIScriptEmptyTargetReplies400(t *testing.T) {
	t.Parallel()
	if !canRunBash(t) {
		t.Skip("当前平台没有 bash，跳过脚本执行验证")
	}

	spec := baseSpec("qlink2d.cgi-empty")
	spec.Port = 0
	spec.ShortcutURL = ""
	spec.Path = ""

	dir := buildInto(t, spec)
	cgiPath := filepath.Join(dir, "app", UIDir, "index.cgi")

	out, err := exec.Command("env", "-i", filepath.ToSlash(cgiPath)).CombinedOutput()
	if err != nil {
		t.Fatalf("空目标时脚本仍然失败: %v\n输出:\n%s", err, out)
	}
	text := string(out)
	if !strings.Contains(text, "Status: 400") {
		t.Errorf("空目标应返回 400 说明页，而不是 302 或空输出:\n%s", text)
	}
	if strings.Contains(text, "正在跳转") {
		t.Errorf("没有目标还在说「正在跳转」，用户会一直等:\n%s", text)
	}
}

// canRunBash 报告本机能否执行 bash 脚本。
//
// Windows 上没有 bash（Git-Bash 不一定在 PATH 里），跳过而不是失败 ——
// 真正的部署目标是 Linux，CI 上这条会实打实地跑起来。
func canRunBash(t *testing.T) bool {
	t.Helper()
	_, err := exec.LookPath("env")
	if err != nil {
		return false
	}
	_, err = exec.LookPath("bash")
	return err == nil
}

// TestBuildPackagePortlessWithoutTargetIsExplicit 覆盖 CGI 的极端分支。
//
// 没有目标地址时不能渲染"正在跳转…"——那会让用户一直等一个不存在的东西。
func TestBuildPackagePortlessWithoutTargetIsExplicit(t *testing.T) {
	t.Parallel()

	spec := baseSpec("qlink2d.notarget-1")
	spec.Port = 0
	spec.ShortcutURL = ""
	spec.Path = ""

	dir := buildInto(t, spec)
	cgi, err := os.ReadFile(filepath.Join(dir, "app", UIDir, "index.cgi"))
	if err != nil {
		t.Fatalf("必须生成 index.cgi: %v", err)
	}
	if strings.Contains(string(cgi), `content="0; url=`) {
		t.Error("没有目标地址时不应渲染带跳转的页面（否则用户会一直等）")
	}
	if !strings.Contains(string(cgi), "未配置目标地址") {
		t.Errorf("应给出明确说明:\n%s", cgi)
	}
}

// TestBuildPackageEscapesRedirectTarget 保证目标地址不会破坏 CGI 脚本或页面。
//
// 目标地址来自用户输入，三处注入面都要堵住：
//   - 拼进 HTML 的副本必须做 HTML 转义（否则是 XSS）；
//   - 拼进 shell 的副本必须按单引号规则加引号（否则能改掉脚本结构）；
//   - 会进响应头的那一份必须去掉控制字符（否则是响应拆分）。
func TestBuildPackageEscapesRedirectTarget(t *testing.T) {
	t.Parallel()

	spec := baseSpec("qlink2d.escape-1")
	spec.Port = 0
	spec.ShortcutURL = `https://example.com/?q=<script>alert(1)</script>`

	dir := buildInto(t, spec)
	cgi, err := os.ReadFile(filepath.Join(dir, "app", UIDir, "index.cgi"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(cgi)

	// HTML 副本：尖括号必须被实体化。
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Errorf("兜底页里的目标地址必须做 HTML 转义:\n%s", body)
	}
	// shell 副本：紧跟在 = 后面的必须是单引号，值整体被包住。
	if !strings.Contains(body, "TARGET='https://example.com/?q=") {
		t.Errorf("目标地址必须被单引号包住:\n%s", body)
	}

	// 含单引号的地址不能被截断，而要按 '\'' 规则转义。
	spec2 := baseSpec("qlink2d.escape-2")
	spec2.Port = 0
	spec2.ShortcutURL = "https://example.com/a'b"
	cgi2, err := os.ReadFile(filepath.Join(buildInto(t, spec2), "app", UIDir, "index.cgi"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cgi2), `'\''`) {
		t.Errorf("单引号必须按 shell 规则转义:\n%s", cgi2)
	}

	// 控制字符（这里是换行）必须被清掉，否则 Location 头可以被拆开。
	spec3 := baseSpec("qlink2d.escape-3")
	spec3.Port = 0
	spec3.ShortcutURL = "https://example.com/ok\r\nX-Injected: 1"
	cgi3, err := os.ReadFile(filepath.Join(buildInto(t, spec3), "app", UIDir, "index.cgi"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cgi3), "X-Injected: 1\n") && strings.Contains(string(cgi3), "FALLBACK_TARGET='https://example.com/ok\r") {
		t.Errorf("控制字符必须被清除:\n%s", cgi3)
	}
}

// TestBuildUIEntryMatchesWrittenConfig 是本文件最重要的一个用例。
//
// 漂移检测（isStale）用的是 BuildUIEntry 计算的期望值，
// 而真正安装时写盘的是 writeUIConfig。两者一旦不一致，
// 就会出现「每次启动都判定配置过期 → 无限重装」的死循环。
// 因此必须锁定：期望值 == 实际写入值。
func TestBuildUIEntryMatchesWrittenConfig(t *testing.T) {
	t.Parallel()

	specs := []PackageSpec{
		func() PackageSpec { s := baseSpec("qlink2d.eq-1"); s.Port = 8080; return s }(),
		func() PackageSpec {
			s := baseSpec("qlink2d.eq-2")
			s.Port = 8443
			s.Scheme = "https"
			s.Path = "/admin"
			return s
		}(),
		func() PackageSpec { s := baseSpec("qlink2d.eq-3"); s.ShortcutURL = "https://a.b/c"; return s }(),
		func() PackageSpec {
			// 无端口形态（网址快捷方式）：走 CGI 302。
			s := baseSpec("qlink2d.eq-3b")
			s.ShortcutURL = "https://a.b/c"
			s.UI = domain.UITab
			return s
		}(),
		func() PackageSpec {
			s := baseSpec("qlink2d.eq-5")
			s.Port = 9000
			s.NoDisplay = true
			s.FileTypes = []string{".torrent"}
			return s
		}(),
	}

	for _, spec := range specs {
		dir := t.TempDir()
		if err := BuildPackage(dir, spec); err != nil {
			t.Fatalf("BuildPackage(%s) 失败: %v", spec.AppName, err)
		}
		written := readEntry(t, dir, spec.AppName)
		expected := BuildUIEntry(spec)
		if !written.Equal(expected) {
			t.Errorf("包 %s 的期望入口与实际写入不一致\n写入: %+v\n期望: %+v", spec.AppName, written, expected)
		}
	}
}

func TestManifestValuesAreSingleLine(t *testing.T) {
	t.Parallel()

	spec := baseSpec("qlink2d.mf-1")
	spec.Port = 80
	spec.Title = "第一行\n第二行"
	spec.Desc = "描述\r\n带换行"

	dir := buildInto(t, spec)
	mf := parseManifest(t, dir)

	// parseManifest 会在遇到不成对的行时直接失败，因此能走到这里
	// 就说明每一行都是规整的 key = value。
	if mf["display_name"] != "第一行 第二行" {
		t.Errorf("display_name 的换行未被折叠: %q", mf["display_name"])
	}
	if mf["desc"] != "描述 带换行" {
		t.Errorf("desc 的换行未被折叠: %q", mf["desc"])
	}
	for key, value := range mf {
		if strings.ContainsAny(value, "\r\n") {
			t.Errorf("字段 %s 的值里残留换行: %q", key, value)
		}
		if strings.TrimSpace(value) != value {
			t.Errorf("字段 %s 的值首尾有空白: %q", key, value)
		}
	}
}

func TestNoDisplayOmitsLaunchNameAndPort(t *testing.T) {
	t.Parallel()

	spec := baseSpec("qlink2d.nodisp-1")
	spec.Port = 7000
	spec.NoDisplay = true

	dir := buildInto(t, spec)
	mf := parseManifest(t, dir)
	if _, ok := mf["desktop_applaunchname"]; ok {
		t.Error("noDisplay 时不应声明桌面启动入口")
	}
	if _, ok := mf["service_port"]; ok {
		t.Error("noDisplay 时不应声明服务端口")
	}
}

// TestPackageLayoutMatchesFnpackRules 锁定官方 fnpack 的打包校验清单。
//
// 名单取自 developer.fnnas.com/docs/cli/fnpack 的「打包检查」表格：
// manifest、config/privilege、config/resource、ICON.PNG、ICON_256.PNG、
// app/、cmd/、wizard/、app/{desktop_uidir}/。
// 其中 wizard/ 曾被早期实现完全遗漏，会让本地打包校验失败。
func TestPackageLayoutMatchesFnpackRules(t *testing.T) {
	t.Parallel()

	dir := buildInto(t, baseSpec("qlink2d.icon-1"))

	requiredFiles := []string{
		"manifest",
		"config/privilege",
		"config/resource",
		"ICON.PNG",
		"ICON_256.PNG",
		"app/ui/config",
		"app/ui/images/icon_64.png",
		"app/ui/images/icon_256.png",
	}
	for _, rel := range requiredFiles {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel))); err != nil {
			t.Errorf("缺少官方必填文件 %s: %v", rel, err)
		}
	}

	requiredDirs := []string{"app", "cmd", "config", "wizard", "app/ui"}
	for _, rel := range requiredDirs {
		fi, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil || !fi.IsDir() {
			t.Errorf("缺少官方必填目录 %s/", rel)
		}
	}

	// wizard/ 目录必须存在，且里面的向导文件是合法的 JSON 数组。
	wiz, err := os.ReadFile(filepath.Join(dir, "wizard", "config"))
	if err != nil {
		t.Fatalf("wizard/config 应存在: %v", err)
	}
	var steps []map[string]any
	if err := json.Unmarshal(wiz, &steps); err != nil {
		t.Errorf("wizard/config 必须是合法的 JSON 数组: %v\n内容: %s", err, wiz)
	}

	// 入口 ID 必须与 manifest 的 desktop_applaunchname 一致。
	var cfg uiConfig
	data, err := os.ReadFile(filepath.Join(dir, "app", "ui", "config"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Entries[UIEntryKey("qlink2d.icon-1")]; !ok {
		t.Errorf(".url 下缺少入口 %q: %s", UIEntryKey("qlink2d.icon-1"), data)
	}

	// 桌面入口的 icon 必须能被 {0} 占位符命中。
	entry := cfg.Entries[UIEntryKey("qlink2d.icon-1")]
	for _, size := range []string{"64", "256"} {
		rel := strings.ReplaceAll(entry.Icon, "{0}", size)
		if _, err := os.Stat(filepath.Join(dir, "app", "ui", filepath.FromSlash(rel))); err != nil {
			t.Errorf("图标 %s 不存在于 app/ui/ 下: %v", rel, err)
		}
	}
}

// TestPrivilegeAndResourceAreValidJSON 保证两个必填 JSON 文件真的可解析。
func TestPrivilegeAndResourceAreValidJSON(t *testing.T) {
	t.Parallel()

	dir := buildInto(t, baseSpec("qlink2d.json-1"))
	for _, rel := range []string{"config/privilege", "config/resource"} {
		data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", rel, err)
		}
		var v any
		if err := json.Unmarshal(data, &v); err != nil {
			t.Errorf("%s 不是合法 JSON: %v\n内容: %s", rel, err, data)
		}
	}
}

func TestCGIEntryPath(t *testing.T) {
	t.Parallel()

	got := cgiEntryPath("qlink2d.foo-1")
	want := "/cgi/ThirdParty/qlink2d.foo-1/index.cgi/redirect/qlink2d.foo-1/_"
	if got != want {
		t.Fatalf("cgiEntryPath() = %q, 期望 %q", got, want)
	}
}

// assertTree 校验包目录树的必备文件全部存在。
func assertTree(t *testing.T, dir string) {
	t.Helper()
	required := []string{
		"manifest", "ICON.PNG", "ICON_256.PNG",
		"config/privilege", "config/resource",
		"wizard/config",
		"cmd/main", "cmd/install_init", "cmd/install_callback",
		"cmd/uninstall_init", "cmd/uninstall_callback",
		"cmd/upgrade_init", "cmd/upgrade_callback",
		"cmd/config_init", "cmd/config_callback",
	}
	for _, rel := range required {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		fi, err := os.Stat(p)
		if err != nil {
			t.Errorf("缺少文件 %s", rel)
			continue
		}
		if hasExecBit && strings.HasPrefix(rel, "cmd/") && fi.Mode().Perm()&0o111 == 0 {
			t.Errorf("%s 缺少可执行位 (%v)", rel, fi.Mode())
		}
	}
}

// hasExecBit 报告当前文件系统是否真的支持 Unix 可执行位。
//
// Windows / NTFS 上 os.Chmod 不会生效（实测一律得到 0666），
// 因此这类断言只在类 Unix 平台上执行——真实的部署目标是 Linux 的飞牛 OS，
// 在那里这个位是必须的（否则飞牛网关无法执行 cmd/main 与 index.cgi）。
var hasExecBit = runtime.GOOS != "windows"
