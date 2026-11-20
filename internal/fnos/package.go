package fnos

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/MisiteQ/qlink2desktop/internal/domain"
)

// IconSet 是一组已解析完成的图标（两档尺寸，PNG 字节）。
type IconSet struct {
	Small []byte // 64x64
	Large []byte // 256x256
}

// Valid 报告图标数据是否齐备。
func (s IconSet) Valid() bool { return len(s.Small) > 0 && len(s.Large) > 0 }

// PackageSpec 描述一个待生成的飞牛子应用包。
type PackageSpec struct {
	AppName string
	Title   string
	Desc    string

	// Port > 0 走「直连端口」模式（入口声明 port）；Port == 0 走 CGI 或 fnOS 反代。
	Port int
	// ManifestPort 是 manifest 里要声明的 service_port。
	// routeDirect 时通常等于 Port；routeFnosProxy / routeRedirect 时
	// 也可能有值（让 fnOS Nginx 知道反代到哪）。
	// 为 0 表示不声明 service_port（例如纯快捷方式、没有真实后端的场景）。
	ManifestPort int
	// Scheme/Path 用于端口 / fnOS 反代模式；ShortcutURL 用于 CGI 模式。
	Scheme      string
	Path        string
	ShortcutURL string

	// WANMode / PublicURL 启用「外网优先走公网反代」。
	// 此时入口不声明端口（仍走 CGI），由 CGI 按访问来源分流：
	// 内网直连 LocalPort，外网 302 到 PublicURL。
	WANMode   domain.WANMode
	PublicURL string
	LocalPort int

	UI        domain.UIType
	AllUsers  bool
	NoDisplay bool
	FileTypes []string

	Icons IconSet
}

// routeMode 决定桌面图标点击后如何到达目标。
type routeMode int

const (
	routeDirect    routeMode = iota // 直接声明 protocol + port + url（保留既有形态）
	routeRedirect                   // CGI 302 跳转
	routeFnosProxy                  // fnOS 内置 Nginx 反代（入口不声明 port，让 fnOS 自己反代到 service_port）
)

// decideRoute 是三种模式唯一的判定入口。
//
// 三种形态的触发条件（按优先级从高到低）：
//
//  1. WANModePublic + 有反代地址  → routeRedirect
//     入口没有地方能直接换外网域名，只能让 CGI 脚本按访问来源分流
//     （外网 302 到用户反代域名，内网回到 fnOS 反代路径或本机端口）。
//
//  2. WANModeFnosProxy           → routeFnosProxy
//     入口不声明 port、protocol 留空（自适应），只写相对路径。
//     manifest 里永远声明 service_port 让 fnOS Nginx 反代接手。
//     浏览器拼出来的是 hostname + 子路径（没有端口），
//     用户再到 fnOS 应用设置里填反代域名 / 子域名反代即可。
//
//  3. Port > 0（WANModeRelay / WANModeIntranet） → routeDirect
//     保留既有形态：入口声明 protocol + port + url。
//     浏览器拼 protocol://hostname:port/url，
//     内网直连本机 IP，外网经 FN Connect 时由中继给三级子域名。
//     两者路由形态相同，区别仅在「是否遵守 all_users」：中继模式强制全员可见
//     （否则外网被网关拒绝），仅内网模式遵守用户设置（放弃外网中继）。
//
//  4. 无端口（快捷方式等）        → routeRedirect
//     没什么可走，只能 CGI 302 兜底。
func decideRoute(spec PackageSpec) routeMode {
	if spec.WANMode == domain.WANModePublic && strings.TrimSpace(spec.PublicURL) != "" {
		return routeRedirect
	}
	if spec.WANMode == domain.WANModeFnosProxy {
		return routeFnosProxy
	}
	if spec.Port > 0 {
		return routeDirect
	}
	return routeRedirect
}

// needsCGI 报告该形态是否需要在包内生成 app/ui/index.cgi。
func needsCGI(spec PackageSpec) bool {
	return decideRoute(spec) == routeRedirect
}

// ---------------------------------------------------------------------------
// 官方包结构约定
// ---------------------------------------------------------------------------

// UIDir 是桌面入口目录名，对应 manifest 的 desktop_uidir。
//
// 官方规范：入口配置位于 app/{desktop_uidir}/config。
// 因此 desktop_uidir = "ui" 时，入口文件是 app/ui/config ——
// 注意**不是**包根目录下的 ui/。fnpack 打包前会校验
// app/{desktop_uidir}/ 目录存在，放错位置会直接打包失败。
const UIDir = "ui"

// IconPlaceholder 是桌面入口 icon 字段的取值。
//
// {0} 会被飞牛桌面替换为尺寸（64 / 256），因此磁盘上必须同时存在
// app/ui/images/icon_64.png 与 icon_256.png。
const IconPlaceholder = "images/icon_{0}.png"

// packageDirs 是官方 fpk 规范要求的目录骨架。
//
// 依据 developer.fnnas.com 的「应用框架」与「fnpack 打包检查」：
//
//	app/{desktop_uidir}/  声明 desktop_uidir 时必须存在
//	app/                  必须存在
//	cmd/                  必须存在
//	wizard/               必须存在
//
// wizard/ 是最容易被忽略的一个：早期实现完全没有创建它，
// 导致 fnOS 侧「本地打包」这一步的完整性校验直接失败，
// 表现为安装没有任何报错但图标永远不出现。这里显式建出来。
var packageDirs = []string{
	"",
	"app",
	filepath.FromSlash("app/" + UIDir),
	filepath.FromSlash("app/" + UIDir + "/images"),
	"cmd",
	"config",
	"wizard",
}

// 需要生成的生命周期脚本（除 main 之外）。
//
// 全部是无副作用的空脚本：子应用没有真实服务，
// 安装、升级、配置、卸载各阶段都不需要做额外的事情。
// 但文件必须存在 —— 官方规范把 cmd/ 列为必填目录。
var lifecycleScripts = []string{
	"install_init", "install_callback",
	"uninstall_init", "uninstall_callback",
	"upgrade_init", "upgrade_callback",
	"config_init", "config_callback",
}

// UIEntryKey 返回桌面入口在 .url 下的键名。
//
// 官方约定：入口 ID 以 appname 为前缀并带一个稳定后缀，
// 且必须与 manifest 的 desktop_applaunchname 完全一致。
func UIEntryKey(appName string) string { return appName + ".main" }

// BuildPackage 在 dir 下生成一个完整的飞牛子应用包目录树。
//
// 目录结构（与 developer.fnnas.com 的 fpk 规范对齐）：
//
//	manifest                 应用元数据
//	ICON.PNG                 64x64 包图标（全局统一）
//	ICON_256.PNG             256x256 包图标（全局统一）
//	cmd/                     生命周期脚本（main + 8 个钩子）
//	config/privilege         运行身份声明
//	config/resource          资源声明
//	wizard/config            空向导（目录为官方必填项）
//	app/ui/config            桌面入口（desktop_uidir = ui）
//	app/ui/images/icon_{0}.png  桌面图标两档
func BuildPackage(dir string, spec PackageSpec) error {
	if err := domain.ValidateAppName(spec.AppName); err != nil {
		return err
	}
	if !spec.Icons.Valid() {
		return fmt.Errorf("%w: 图标数据缺失", domain.ErrValidation)
	}

	for _, sub := range packageDirs {
		p := filepath.Join(dir, filepath.FromSlash(sub))
		if err := os.MkdirAll(p, 0o755); err != nil {
			return fmt.Errorf("创建目录 %s 失败: %w", p, err)
		}
	}

	for _, step := range []func() error{
		func() error { return writeManifest(dir, spec) },
		func() error { return writeUIConfig(dir, spec) },
		func() error { return writeIcons(dir, spec.Icons) },
		func() error { return writeLifecycleScripts(dir, spec) },
		func() error { return writeConfigFiles(dir) },
		func() error { return writeWizard(dir) },
	} {
		if err := step(); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// manifest
// ---------------------------------------------------------------------------

func writeManifest(dir string, spec PackageSpec) error {
	title := strings.TrimSpace(spec.Title)
	if title == "" {
		title = "桌面应用"
	}
	desc := strings.TrimSpace(spec.Desc)
	if desc == "" {
		desc = fmt.Sprintf("由 QLink2Desktop 生成的桌面入口：%s", title)
	}

	m := NewManifest(spec.AppName, title, desc)
	if !spec.NoDisplay {
		// 入口 ID 与 ui/config 中的键必须一字不差。
		m.WithLaunchName(UIEntryKey(spec.AppName))
		// service_port 让 fnOS Nginx 知道反代目标端口。
		// routeDirect / routeFnosProxy / WANModePublic-CGI 都需要它，
		// 只有纯快捷方式（没真实后端）才不声明。
		port := spec.ManifestPort
		if port <= 0 {
			port = spec.Port
		}
		m.WithServicePort(port)
	}
	return os.WriteFile(filepath.Join(dir, "manifest"), m.Bytes(), 0o644)
}

// ---------------------------------------------------------------------------
// 桌面入口
// ---------------------------------------------------------------------------

// uiEntry 是 ui/config 里单个入口的定义。
//
// 用强类型结构体取代早期的 map[string]interface{}：字段名由编译器保证，
// 也天然避免了「有时写 allUsers、有时写 allusers」这类低级错误。
//
// 字段名与官方「应用入口」文档逐条对应；未使用的字段靠 omitempty 省略，
// 避免写入空串被飞牛误判为「显式声明了一个空协议」。
type uiEntry struct {
	Title     string   `json:"title"`
	Icon      string   `json:"icon"`
	Type      string   `json:"type"`
	Protocol  string   `json:"protocol,omitempty"`
	Port      string   `json:"port,omitempty"`
	URL       string   `json:"url"`
	AllUsers  bool     `json:"allUsers"`
	NoDisplay bool     `json:"noDisplay"`
	FileTypes []string `json:"fileTypes,omitempty"`
}

// Equal 逐字段比较两个入口定义。
//
// 存在的原因很实在：uiEntry 含 FileTypes 切片，结构体本身不可比较，
// 而「已安装配置是否与期望一致」这个判断直接决定了会不会触发重装循环，
// 必须有一个明确、可测的相等语义。
func (e uiEntry) Equal(other uiEntry) bool {
	if e.Title != other.Title ||
		e.Icon != other.Icon ||
		e.Type != other.Type ||
		e.Protocol != other.Protocol ||
		e.Port != other.Port ||
		e.URL != other.URL ||
		e.AllUsers != other.AllUsers ||
		e.NoDisplay != other.NoDisplay {
		return false
	}
	if len(e.FileTypes) != len(other.FileTypes) {
		return false
	}
	for i := range e.FileTypes {
		if e.FileTypes[i] != other.FileTypes[i] {
			return false
		}
	}
	return true
}

type uiConfig struct {
	Entries map[string]uiEntry `json:".url"`
}

// BuildUIEntry 构造该包**应当**写入的桌面入口。
//
// 这是全项目唯一一份「期望入口」的构造逻辑：安装时写盘、启动对账时比对，
// 走的都是这个函数。早期实现里写盘与比对各写了一遍，两者一旦出现细微差异
// （比如一边补了 allUsers=true、另一边忘了），就会退化成
// 「每次启动都判定配置过期 → 卸载重装 → 再判定过期」的死循环。
func BuildUIEntry(spec PackageSpec) uiEntry {
	entry := uiEntry{
		Title:     spec.Title,
		Icon:      IconPlaceholder,
		Type:      string(spec.UI.Normalize()),
		AllUsers:  spec.AllUsers,
		NoDisplay: spec.NoDisplay,
		FileTypes: spec.FileTypes,
	}

	switch decideRoute(spec) {
	case routeDirect:
		scheme := spec.Scheme
		if scheme == "" {
			scheme = domain.SchemeHTTP
		}
		path := spec.Path
		if path == "" {
			path = "/"
		}
		entry.Protocol = scheme
		entry.Port = strconv.Itoa(spec.Port)
		entry.URL = path

		// 飞牛 Connect 的外网子域名网关按 Cookie 域隔离来鉴权；
		// 若桌面入口声明为「仅管理员可见」，外网访问会被网关直接拒绝并报
		// "FN Connect 暂无权限访问该服务"。因此中继模式（默认）一律放开为全员可见。
		// 仅内网模式（WANModeIntranet）下用户显式放弃了外网中继，此时遵守用户的
		// all_users 设置，让「仅管理员可见」在内网真正生效。
		if spec.WANMode != domain.WANModeIntranet {
			entry.AllUsers = true
		}

	case routeFnosProxy:
		// fnOS 内置反代形态：入口不声明 port、protocol 留空让 fnOS 自适应，
		// 只写相对路径。manifest 里声明 service_port 让 fnOS Nginx 把
		// 这个入口反代到目标端口。浏览器拼出来的就是 hostname + 子路径，
		// 没有单独端口。
		entry.URL = proxyEntryPath(spec.AppName, spec.Path)

	default:
		// CGI 跳转经由 CGI 入口，此时不声明端口与协议。
		//
		// protocol 刻意留空（官方文档：空字符串表示交给系统按当前访问方式自适应）。
		// 早期这里写死 "http"，而桌面通常是 https 访问的 —— 那等于在 https
		// 页面里声明一个明文入口，属于协议降级，也是个纯粹的坑。
		entry.URL = cgiEntryPath(spec.AppName)
	}
	return entry
}

// proxyEntryPath 为 fnOS 反代形态生成入口 url。
//
// 我们这里约定子应用入口的反代路径前缀就是 appname（小写，去掉 qlink2d. 前缀），
// 这样 fnOS Nginx 可以按路径前缀反代：
//
//	https://NAS-域名/<appname-path-prefix>/  →  127.0.0.1:service_port/
//
// 用户可以在 fnOS 应用设置里给应用单独配子域名反代，也可以直接走
// 路径反代。两种反代形态共存，浏览器看到的都是"没有单独端口"的干净 URL。
func proxyEntryPath(appName, userPath string) string {
	prefix := strings.TrimPrefix(appName, "qlink2d.")
	prefix = strings.ToLower(strings.TrimSpace(prefix))
	if prefix == "" {
		prefix = "app"
	}
	userPath = strings.TrimSpace(userPath)
	if userPath == "" {
		userPath = "/"
	}
	if !strings.HasPrefix(userPath, "/") {
		userPath = "/" + userPath
	}
	if prefix == "/" {
		return userPath
	}
	return "/" + prefix + userPath
}

func writeUIConfig(dir string, spec PackageSpec) error {
	cfg := uiConfig{Entries: map[string]uiEntry{
		UIEntryKey(spec.AppName): BuildUIEntry(spec),
	}}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化桌面入口配置失败: %w", err)
	}
	path := filepath.Join(dir, filepath.FromSlash("app/"+UIDir+"/config"))
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func cgiEntryPath(appName string) string {
	return fmt.Sprintf("/cgi/ThirdParty/%s/index.cgi/redirect/%s/_", appName, appName)
}

// ---------------------------------------------------------------------------
// 图标
// ---------------------------------------------------------------------------

func writeIcons(dir string, icons IconSet) error {
	// ICON.PNG / ICON_256.PNG 供应用中心展示；
	// app/ui/images/ 下的两档供桌面按 {0} 占位符取用。
	targets := []struct {
		rel  string
		data []byte
	}{
		{"ICON.PNG", icons.Small},
		{"ICON_256.PNG", icons.Large},
		{"app/" + UIDir + "/images/icon_64.png", icons.Small},
		{"app/" + UIDir + "/images/icon_256.png", icons.Large},
	}
	for _, t := range targets {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(t.rel)), t.data, 0o644); err != nil {
			return fmt.Errorf("写入图标 %s 失败: %w", t.rel, err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 生命周期脚本
// ---------------------------------------------------------------------------

// shortcutMainScript 是子应用的 cmd/main。
//
// 关键设计：子应用**没有任何常驻进程**，它只是一个桌面快捷方式。
// 但飞牛应用中心是通过「应用是否处于运行态」来决定桌面图标是否显示、
// 以及是否显示启动/停止按钮的。因此这里的 start 立即返回 0
// （等价于「服务已就绪」），stop 同样返回 0，status 恒为「运行中」。
//
// 之所以不按官方对静态应用的建议设置 ctl_stop=false：
// 隐藏运行控制后，应用中心的运行态判定路径会随之改变，
// 而「图标必须出现在桌面上」是本应用的核心承诺 —— 在真机上
// 验证过当前写法之前，不拿这条主链路冒险。原因同样记录在 ARCHITECTURE.md。
const shortcutMainScript = `#!/bin/bash
# 由 QLink2Desktop 自动生成 —— 请勿手工修改，重新安装会覆盖。
#
# 本应用是一个桌面快捷方式，没有常驻进程：
# start / stop 均为「已达成目标状态」的空操作，status 恒为运行中，
# 以便飞牛应用中心把桌面图标维持在可见状态。
set -u

log_err() {
  # 官方约定：用户可见的错误信息写入 TRIM_TEMP_LOGFILE。
  if [ -n "${TRIM_TEMP_LOGFILE:-}" ]; then
    echo "$1" > "${TRIM_TEMP_LOGFILE}"
  fi
  echo "$1" >&2
}

case "${1:-}" in
  start)
    exit 0
    ;;
  stop)
    exit 0
    ;;
  status)
    # 0 = 运行中 / 3 = 未运行
    exit 0
    ;;
  *)
    log_err "未知的运行控制指令: ${1:-<空>}"
    exit 1
    ;;
esac
`

// installCallbackScript 在安装完成后补一次可执行位。
//
// CGI 脚本是从包目录直接复制过来的，部分文件系统（挂载的 NTFS、
// 某些 tar 实现）会丢掉可执行位，导致飞牛网关无法执行它 ——
// 表现为点开图标是一片空白而不是跳转页面。
const installCallbackScript = `#!/bin/bash
# 由 QLink2Desktop 自动生成 —— 请勿手工修改，重新安装会覆盖。
set -u

# 注意路径：源码包里的 app/ui/index.cgi 安装后位于 $TRIM_APPDEST/ui/index.cgi
# （app/ 这一层会被剥掉），所以这里只看这一处。
for f in "${TRIM_APPDEST:-}/ui/index.cgi"; do
  [ -n "${f}" ] && [ -f "${f}" ] || continue
  chmod +x "${f}" 2>/dev/null || true
done
exit 0
`

func writeLifecycleScripts(dir string, spec PackageSpec) error {
	if err := writeExecutable(filepath.Join(dir, "cmd", "main"), shortcutMainScript); err != nil {
		return err
	}

	noop := "#!/bin/bash\n" +
		"# 由 QLink2Desktop 自动生成 —— 本阶段无需额外处理。\n" +
		"exit 0\n"
	script, cgi := renderCGIScript(spec)

	for _, name := range lifecycleScripts {
		content := noop
		if name == "install_callback" && cgi {
			content = installCallbackScript
		}
		if err := writeExecutable(filepath.Join(dir, "cmd", name), content); err != nil {
			return err
		}
	}

	if !cgi {
		return nil
	}
	return writeExecutable(filepath.Join(dir, "app", UIDir, "index.cgi"), script)
}

// renderCGIScript 是 CGI 脚本的**唯一生成入口**，返回（脚本内容, 是否需要 CGI）。
//
// 为什么抽成纯函数：判断"磁盘上的脚本是否过期"（Service.isStale）必须用与写盘
// 完全相同的逻辑再生成一份来比对。比对用一份逻辑、写盘用另一份，这个检查迟早会
// 因为两边改动不同步而形同虚设 —— 和 BuildUIEntry 同时服务于"写 config"与
// "判断 config 是否过期"是同一个道理。
//
// 目标地址：网址快捷方式取完整外链；其余无端口形态（例如端口映射但代理端口
// 缺失）退回配置里的访问路径。它会同时进 302 头与兜底页面。
func renderCGIScript(spec PackageSpec) (string, bool) {
	if !needsCGI(spec) {
		return "", false
	}
	if spec.WANMode == domain.WANModePublic && strings.TrimSpace(spec.PublicURL) != "" {
		return buildWANRedirectScript(spec), true
	}
	target := strings.TrimSpace(spec.ShortcutURL)
	if target == "" {
		target = strings.TrimSpace(spec.Path)
	}
	target = sanitizeTarget(target)
	return buildCGIScript(target, redirectPage(target)), true
}

// ---------------------------------------------------------------------------
// config / wizard
// ---------------------------------------------------------------------------

// writeConfigFiles 生成 config/privilege 与 config/resource。
//
// 关于 run-as：官方规范默认推荐 package（专用应用用户），
// 但本项目的子应用包名形如 qlink2d.alist-3f2a1c —— 含点号。
// 飞牛在未知 username 时会依据 appname 自动生成专用用户，
// 而 Linux 的 useradd 默认拒绝含点号的用户名，生成失败会让整个安装中断。
// 同时 install_callback 需要给 index.cgi 补可执行位。
// 因此这里显式声明 root，并把原因写在文件里以便日后审计。
func writeConfigFiles(dir string) error {
	privilege := []byte(`{
  "defaults": {
    "run-as": "root"
  }
}
`)
	if err := os.WriteFile(filepath.Join(dir, "config", "privilege"), privilege, 0o644); err != nil {
		return err
	}
	// 子应用不申请共享目录、端口转发之外的任何系统资源。
	resource := []byte("{}\n")
	return os.WriteFile(filepath.Join(dir, "config", "resource"), resource, 0o644)
}

// writeWizard 写入一个空向导。
//
// wizard/ 在官方规范里是必填目录，而空目录在压缩 / 复制过程中容易丢失，
// 因此写入一个合法的空向导文件（[] 表示没有任何步骤需要用户填写），
// 既保证目录非空，也保证内容符合向导文件的 JSON 结构。
func writeWizard(dir string) error {
	return os.WriteFile(filepath.Join(dir, "wizard", "config"), []byte("[]\n"), 0o644)
}

func writeExecutable(path, content string) error {
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		return err
	}
	// 某些文件系统（如挂载的 NTFS / tmpfs）会忽略 WriteFile 的权限位，这里再兜一次。
	if err := os.Chmod(path, 0o755); err != nil && !errors.Is(err, os.ErrPermission) {
		return err
	}
	return nil
}
