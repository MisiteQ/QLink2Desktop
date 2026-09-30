package fnos

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/MisiteQ/qlink2desktop/internal/domain"
)

// Service 把 Link 编排成飞牛桌面图标（以及反向的注销），并对外提供实时的注册状态。
//
// 这一层是纯粹的业务编排：它不直接碰 exec、不直接碰文件系统细节
// （那些都在 package.go / exec.go 里），因此可以用 FakeCLI 完整测试。
type Service struct {
	cli CLI
	// iconsDir 用独立的锁保护：它可以在运行时被用户改（见 SetIconsDir），
	// 而读写它的路径（上传、读取、内联）都不在 s.mu 的临界区里，
	// 混用一把锁容易埋下重入死锁。
	iconsMu  sync.RWMutex
	iconsDir string
	// logDir 是应用日志目录；重启脚本的旁路日志写在这里。
	logDir string
	// allowRemoteIcon 控制是否允许为图标发起外网请求（离线 / 内网部署可关闭）。
	allowRemoteIcon bool

	// installRoots 覆盖「已安装应用在哪」的探测范围，测试时指向临时目录。
	installRoots []string

	// portResolver 由代理管理器注入，用于回答「这条链接的桌面入口应该指向哪个本机端口」。
	// 之所以用回调而不是把端口存进 Link：端口是运行态，进程重启后可能变化，
	// 写进定义会造成「持久化了一份过期事实」这类难查的问题。
	portResolver PortResolver

	mu     sync.RWMutex
	status map[string]domain.Status // key: linkID
}

// PortResolver 返回某条链接的桌面入口应当指向的本机端口，0 表示当前不可用。
type PortResolver func(domain.Link) int

// SetPortResolver 注入端口解析回调。
func (s *Service) SetPortResolver(fn PortResolver) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.portResolver = fn
}

// desktopPort 计算桌面入口应当使用的本机端口。
func (s *Service) desktopPort(l domain.Link) int {
	s.mu.RLock()
	fn := s.portResolver
	s.mu.RUnlock()
	if fn != nil {
		if p := fn(l); p > 0 {
			return p
		}
	}
	// 没有代理端口时：本机端口形态仍可直连真实端口；其它形态只能退化为跳转。
	if l.Kind == domain.KindLocalPort {
		return l.Port
	}
	return 0
}

// Options 是构造 Service 的参数。
type Options struct {
	CLI      CLI
	IconsDir string
	// LogDir 是重启脚本写旁路日志的目录（见 restartLogPath）。
	// 留空则不写日志 —— 那一段脚本比写它的进程活得久，能留就留。
	LogDir          string
	AllowRemoteIcon bool
}

// NewService 创建编排服务。
func NewService(opts Options) *Service {
	cli := opts.CLI
	if cli == nil {
		cli = NewExecCLI()
	}
	if opts.IconsDir != "" {
		_ = os.MkdirAll(opts.IconsDir, 0o755)
	}
	return &Service{
		cli:             cli,
		iconsDir:        opts.IconsDir,
		logDir:          opts.LogDir,
		allowRemoteIcon: opts.AllowRemoteIcon,
		status:          make(map[string]domain.Status),
	}
}

// Available 报告真实的应用中心命令行是否可用。
// false 时安装流程只构建包而不注册，用于开发机调试。
func (s *Service) Available() bool { return s.cli.Available() }

// IconsDir 返回当前生效的图标目录。
func (s *Service) IconsDir() string {
	s.iconsMu.RLock()
	defer s.iconsMu.RUnlock()
	return s.iconsDir
}

// SetIconsDir 切换图标目录（用户可在设置页指定）。
//
// 校验分三步，缺一不可：必须是绝对路径（相对路径在不同工作目录下含义不同，
// 而安装后的进程工作目录由飞牛决定，不可依赖），必须能创建，必须真的可写
// （写一个探针文件再删掉——只有真正落一次盘才算数，权限位看着对但没有写权限
// 的目录在 NAS 上很常见）。任何一步不过就整单拒绝，不留半成品。
//
// 刻意**不**搬移已有图标：目录是用户指定的，替他猜"要不要搬、搬去哪"
// 只会制造更难解释的状态。界面上会明确提示已有图标不会自动迁移。
func (s *Service) SetIconsDir(dir string) error {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return fmt.Errorf("%w: 图标目录不能为空", domain.ErrValidation)
	}
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("%w: 图标目录必须是绝对路径（例如 /vol1/%s/icons）",
			domain.ErrValidation, "qlink2d")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建图标目录失败: %w", err)
	}
	probe := filepath.Join(dir, ".qlink2d-write-probe")
	if err := os.WriteFile(probe, []byte("ok"), 0o644); err != nil {
		return fmt.Errorf("图标目录不可写: %w", err)
	}
	_ = os.Remove(probe)

	s.iconsMu.Lock()
	s.iconsDir = dir
	s.iconsMu.Unlock()
	return nil
}

// maxInlineIconBytes 是 data URI 形式的图标体积上限。
//
// 图标一旦被内联成 data URI，每份用到它的响应都会原样带上这份 base64；
// 上传的图标本身已经压过一次，正常都在几十 KB。超过这个体积就干脆不内联，
// 而不是把一个几百 KB 的字符串塞进响应里。
const maxInlineIconBytes = 512 << 10

// IconDataURI 把上传的图标读成 data URI，用于需要把图标直接嵌进 HTML 的场合。
//
// 为什么不返回 URL：
//   - 应用挂在统一网关的 /app/qlink2desktop/ 前缀之后，外部地址取决于网关与
//     FN Connect 域名，事先算不出来；
//   - 而 data URI 本身就是一整段字节，放到任何 origin 下都成立，
//     从根上消除了"地址算错 → 裂图"这一整类问题。
//
// 代价自然是体积，所以由 maxInlineIconBytes 兜住上限。
func (s *Service) IconDataURI(ref string) string {
	name := strings.TrimSpace(ref)
	if name == "" || name != filepath.Base(name) {
		// 带路径分隔符一律拒绝，不做静默纠正。
		return ""
	}
	data, err := os.ReadFile(filepath.Join(s.IconsDir(), name))
	if err != nil || len(data) == 0 || len(data) > maxInlineIconBytes {
		return ""
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(data)
}

// CLI 暴露底层命令行（诊断用）。
func (s *Service) CLI() CLI { return s.cli }

// ---------------------------------------------------------------- 状态

func (s *Service) setStatus(linkID, appName string, phase domain.Phase, detail string) {
	if linkID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status[linkID] = domain.Status{
		LinkID:  linkID,
		AppName: appName,
		Phase:   phase,
		Detail:  detail,
	}
}

// setFailure 记录失败阶段与原因。
//
// 单独抽一个方法而不是复用 setStatus：失败原因是前端要直接展示给用户的
// 关键信息，必须落在 LastError 而不是泛泛的 Detail 里。
func (s *Service) setFailure(linkID, appName string, cause error) {
	if linkID == "" {
		return
	}
	msg := "未知错误"
	if cause != nil {
		msg = strings.TrimSpace(cause.Error())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status[linkID] = domain.Status{
		LinkID:    linkID,
		AppName:   appName,
		Phase:     domain.PhaseFailed,
		LastError: msg,
	}
}

func (s *Service) clearStatus(linkID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.status, linkID)
}

// StatusOf 返回某条链接当前的注册状态；未记录时返回 unknown 阶段。
func (s *Service) StatusOf(linkID, appName string) domain.Status {
	s.mu.RLock()
	st, ok := s.status[linkID]
	s.mu.RUnlock()
	if ok {
		return st
	}
	return domain.Status{LinkID: linkID, AppName: appName, Phase: domain.PhaseUnknown}
}

// MarkPending 在启动对账开始前，把所有已启用链接预置为「排队中」。
//
// 目的是消除前端闪烁：早期版本在真正开始安装之前会先显示一次「已就绪」，
// 用户看到图标亮了一下又变回恢复中，体验很糟。
func (s *Service) MarkPending(links []domain.Link) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range links {
		if !l.Enabled {
			continue
		}
		s.status[l.ID] = domain.Status{
			LinkID:  l.ID,
			AppName: l.EffectiveAppName(),
			Phase:   domain.PhasePending,
		}
	}
}

// MarkPhase 直接记录一条链接的阶段。
//
// 供异步任务在**入队时**立刻反映状态：用户点完按钮马上看到「排队中」，
// 而不是面对一个毫无反馈的界面等上几分钟。真实阶段随后由 Install /
// Uninstall 自己覆盖。
func (s *Service) MarkPhase(linkID, appName string, phase domain.Phase, detail string) {
	s.setStatus(linkID, appName, phase, detail)
}

// SetFailure 从外部记录一次失败（例如任务没能入队）。
func (s *Service) SetFailure(linkID, appName string, cause error) {
	s.setFailure(linkID, appName, cause)
}

// ---------------------------------------------------------------- 安装

// InstallRequest 是一次安装的完整输入。
type InstallRequest struct {
	Link domain.Link
	// PortOverride 用于「端口映射」场景：桌面入口必须指向本机代理端口，
	// 而不是后端真实端口，否则图标绕过代理、自签证书跳过失效。
	// 0 表示不覆盖。
	PortOverride int
}

// Install 生成并注册一个桌面图标。
func (s *Service) Install(ctx context.Context, req InstallRequest) error {
	l := req.Link
	if err := l.Validate(); err != nil {
		return err
	}
	appName := l.EffectiveAppName()
	s.setStatus(l.ID, appName, domain.PhaseInstalling, "")

	port := req.PortOverride
	if port <= 0 {
		// 调用方未显式指定时，自动解析出应当使用的本机端口。
		port = s.desktopPort(l)
	}

	spec := s.buildSpec(l, port)
	pkgDir, err := os.MkdirTemp("", "qlink2d-"+appName+"-")
	if err != nil {
		s.fail(l.ID, appName, err)
		return fmt.Errorf("创建打包目录失败: %w", err)
	}
	defer os.RemoveAll(pkgDir)

	if err := BuildPackage(pkgDir, spec); err != nil {
		s.fail(l.ID, appName, err)
		return err
	}

	if !s.cli.Available() {
		// 模拟模式：包已生成，仅记录日志，便于开发机上检查产物。
		slog.Info("模拟模式：安装包已构建但未注册", "appName", appName, "dir", pkgDir, "title", l.Name)
		s.clearStatus(l.ID)
		return nil
	}

	if err := ctx.Err(); err != nil {
		s.clearStatus(l.ID)
		return err
	}

	volume := s.resolveVolume()
	slog.Info("正在注册桌面图标", "appName", appName, "title", l.Name, "port", spec.Port, "volume", volume, "mode", routeName(decideRoute(spec)))

	if err := s.cli.InstallLocal(pkgDir, volume); err != nil {
		s.fail(l.ID, appName, err)
		return err
	}

	// 装完必须确保处于运行态，否则图标不会出现在桌面上。
	if err := s.ensureRunning(appName); err != nil {
		slog.Warn("桌面图标已注册但未能自动启动，请稍后在应用中心手动启用", "appName", appName, "error", err)
	}

	slog.Info("桌面图标注册完成", "appName", appName, "title", l.Name)
	// 落一个明确的终态而不是清空状态。
	//
	// 清空之后 StatusOf 会返回 unknown，界面只能显示「未知」——在同步调用
	// 的年代这问题不明显（用户没在盯着），但异步化之后，"排队中 → 处理中 →
	// ?" 这个结尾会显得像是没做完。PhaseInstalled 这个阶段本来就是这个用途。
	s.setStatus(l.ID, appName, domain.PhaseInstalled, "")
	return nil
}

// ensureRunning 确保应用处于运行态；已运行则空操作。
func (s *Service) ensureRunning(appName string) error {
	state, err := s.cli.Status(appName)
	if err == nil && state.Active() {
		return nil
	}
	if err := s.cli.Start(appName); err != nil {
		return err
	}
	// 给应用中心一点时间落状态，避免紧接着的查询读到旧值。
	time.Sleep(300 * time.Millisecond)
	return nil
}

func (s *Service) resolveVolume() int {
	if v, err := s.cli.DefaultVolume(); err == nil && v > 0 {
		return v
	}
	return 1
}

// buildSpec 把领域对象翻译为包描述，是「业务 → 打包」的唯一出口。
func (s *Service) buildSpec(l domain.Link, portOverride int) PackageSpec {
	port := l.Port
	scheme := l.Scheme

	switch l.Kind {
	case domain.KindLocalPort:
		// 本机端口形态一律直连真实端口。
		//
		// 早期这里会在"开了开屏提示"时改用内置代理端口 —— 提示页需要一次
		// HTTP 响应才能渲染。该功能已移除，而 needsProxy 对本机端口恒为 false，
		// 也就不会再有人给这个形态分配代理端口。保留分支只会留下一条
		// 没人走、却写着过期原因的路径，故一并删掉。
		_ = portOverride

	case domain.KindProxy:
		// 端口映射形态的桌面入口**永远**指向本机代理端口，而不是远端真实端口。
		// 同理，代理是明文的，这里的 scheme 描述的是"桌面 → 代理"这一段，
		// 而不是"代理 → 后端"那一段（后者的协议由 Route.Target 决定）。
		if portOverride > 0 {
			port = portOverride
			scheme = domain.SchemeHTTP
		} else {
			// 缺少代理端口时不生成直连入口，退化为 CGI 跳转，
			// 避免图标指向一个并不存在的端口。
			port = 0
		}

	default:
		port = 0
	}

	icons := ResolveIcon(IconRequest{
		Icon:        l.Icon,
		UploadDir:   s.IconsDir(),
		Candidates:  iconCandidates(l),
		AllowRemote: s.allowRemoteIcon,
	})

	spec := PackageSpec{
		AppName:   l.EffectiveAppName(),
		Title:     l.Name,
		Desc:      l.Desc,
		Port:      port,
		Scheme:    scheme,
		Path:      l.Path,
		UI:        l.UI,
		AllUsers:  l.AllUsers,
		NoDisplay: l.NoDisplay,
		FileTypes: l.FileTypes,
		Icons:     icons,
	}
	if l.Kind == domain.KindShortcut {
		// 网址快捷方式没有本地端口，只能走 CGI 302（原因见 decideRoute 的注释：
		// 入口配置无法指定飞牛以外的主机）。这里把目标地址传给 CGI 兜底页。
		spec.ShortcutURL = l.ShortcutURL()
	}
	return spec
}

// iconCandidates 汇总可用于自动匹配官方图标的关键词。
func iconCandidates(l domain.Link) []string {
	return []string{l.Container.Image, l.Container.Name, l.Container.Service, l.Name}
}

// ---------------------------------------------------------------- 卸载

// Uninstall 注销一条链接对应的桌面图标。
// protected 是「仍被其它启用链接占用」的包名集合，命中的一律跳过，避免误删别人的图标。
func (s *Service) Uninstall(ctx context.Context, l domain.Link, protected map[string]bool) error {
	appName := l.EffectiveAppName()
	if protected[appName] {
		slog.Info("该包名仍被其它启用项使用，跳过卸载", "appName", appName)
		return nil
	}
	if !s.cli.Available() {
		s.clearStatus(l.ID)
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	// 先停再卸：停止能让桌面图标立刻消失，用户感知更快。
	if err := s.cli.Stop(appName); err != nil {
		slog.Debug("停止应用未成功（可能本就未运行）", "appName", appName, "error", err)
	}
	if err := s.cli.Uninstall(appName); err != nil {
		return err
	}
	s.clearStatus(l.ID)
	return nil
}

// PruneOrphans 清理应用中心里存在、但已不在期望集合中的本项目图标。
//
// 安全约束：只处理本项目管理命名空间下的包（domain.IsManagedApp），
// 绝不触碰用户的其它原生应用。
func (s *Service) PruneOrphans(ctx context.Context, keep map[string]bool) ([]string, error) {
	if !s.cli.Available() {
		return nil, nil
	}
	installed, err := s.cli.List()
	if err != nil {
		return nil, err
	}

	orphans := make([]string, 0)
	for _, name := range ExtractManagedApps(installed) {
		if keep[name] {
			continue
		}
		orphans = append(orphans, name)
	}
	if len(orphans) == 0 {
		return nil, nil
	}

	slog.Info("发现历史遗留桌面图标，开始清理", "count", len(orphans), "apps", orphans)

	// 并发停止（快），限流注销（避免争抢应用中心的锁）。
	var wg sync.WaitGroup
	sem := make(chan struct{}, 3)
	for _, name := range orphans {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(app string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			_ = s.cli.Stop(app)
			if err := s.cli.Uninstall(app); err != nil {
				slog.Warn("清理遗留图标失败，将在下次启动重试", "appName", app, "error", err)
				return
			}
			slog.Info("已清理遗留桌面图标", "appName", app)
		}(name)
	}
	wg.Wait()
	return orphans, nil
}

// ---------------------------------------------------------------- 对账

// ReconcileResult 汇总一次对账的结果，便于日志与前端展示。
type ReconcileResult struct {
	Installed []string `json:"installed"`
	Started   []string `json:"started"`
	Upgraded  []string `json:"upgraded"`
	Failed    []string `json:"failed"`
}

// Reconcile 让飞牛应用中心的实际状态向期望状态收敛。
//
// 三种情况分别处理：
//   - 缺失   → 补齐安装
//   - 配置过期 → 卸载后重装（用于让老版本留下的错误配置自愈）
//   - 已停用 → 仅启动，不做任何多余动作
//
// 「已就绪」的项一律不碰：这是最关键的克制，
// 早期版本因为每次启动都对所有项做无差别重装，导致开机后桌面图标长时间抖动。
func (s *Service) Reconcile(ctx context.Context, links []domain.Link) ReconcileResult {
	var result ReconcileResult
	if !s.cli.Available() {
		return result
	}

	installed := map[string]bool{}
	if names, err := s.cli.List(); err == nil {
		for _, n := range names {
			installed[n] = true
		}
	} else {
		slog.Warn("无法读取已注册应用列表，跳过本次对账", "error", err)
		return result
	}

	type task struct {
		link    domain.Link
		appName string
		action  plan
	}
	var todo []task

	for _, l := range links {
		if !l.Enabled {
			continue
		}
		appName := l.EffectiveAppName()

		switch s.classify(l, installed) {
		case planNone:
			// 已就绪：不产生任何写操作，这是整个对账逻辑最重要的克制。
			s.setStatus(l.ID, appName, domain.PhaseInstalled, "")
		case planInstall:
			s.setStatus(l.ID, appName, domain.PhasePending, "")
			todo = append(todo, task{link: l, appName: appName, action: planInstall})
		case planUpgrade:
			s.setStatus(l.ID, appName, domain.PhaseUpgrading, "")
			todo = append(todo, task{link: l, appName: appName, action: planUpgrade})
		case planStart:
			s.setStatus(l.ID, appName, domain.PhaseInstalling, "唤醒中")
			todo = append(todo, task{link: l, appName: appName, action: planStart})
		}
	}

	for i, t := range todo {
		if ctx.Err() != nil {
			slog.Info("对账被取消，剩余任务将在下次启动继续", "remaining", len(todo)-i)
			break
		}
		detail := fmt.Sprintf("%d/%d", i+1, len(todo))

		switch t.action {
		case planStart:
			s.setStatus(t.link.ID, t.appName, domain.PhaseInstalling, detail)
			if err := s.cli.Start(t.appName); err != nil {
				slog.Warn("唤醒已停用的桌面图标失败", "appName", t.appName, "error", err)
				s.setFailure(t.link.ID, t.appName, err)
				result.Failed = append(result.Failed, t.appName)
				continue
			}
			slog.Info("已唤醒桌面图标", "appName", t.appName, "title", t.link.Name)
			result.Started = append(result.Started, t.appName)

		case planUpgrade:
			s.setStatus(t.link.ID, t.appName, domain.PhaseUpgrading, detail)
			_ = s.cli.Stop(t.appName)
			if err := s.cli.Uninstall(t.appName); err != nil {
				slog.Warn("卸载过期配置失败", "appName", t.appName, "error", err)
			}
			fallthrough

		default:
			s.setStatus(t.link.ID, t.appName, domain.PhaseInstalling, detail)
			if err := s.Install(ctx, InstallRequest{Link: t.link}); err != nil {
				slog.Warn("补齐安装桌面图标失败", "appName", t.appName, "error", err)
				s.setFailure(t.link.ID, t.appName, err)
				result.Failed = append(result.Failed, t.appName)
				continue
			}
			if t.action == planUpgrade {
				result.Upgraded = append(result.Upgraded, t.appName)
			} else {
				result.Installed = append(result.Installed, t.appName)
			}
		}
		// 走到这里说明这一项收敛成功（失败的分支都 continue 了）。
		s.setStatus(t.link.ID, t.appName, domain.PhaseInstalled, "")
	}

	if len(todo) > 0 {
		slog.Info("桌面图标对账完成", "installed", len(result.Installed),
			"started", len(result.Started), "upgraded", len(result.Upgraded), "failed", len(result.Failed))
	}
	return result
}

// ---------------------------------------------------------------- 配置漂移检测

// plan 是一条链接在"向期望状态收敛"时需要执行的动作。
//
// 把判定结果显式建模，而不是散落成若干个布尔量：
// 布尔量之间可以组合出无意义的状态（比如既"要安装"又"只启动"），
// 而枚举天然互斥，新增一种动作时编译器会提醒所有分支都要处理。
type plan int

const (
	// planNone：已注册、配置正确、正在运行——什么都不要做。
	planNone plan = iota
	// planStart：已注册、配置正确，但被停用了，只需唤醒。
	planStart
	// planUpgrade：已注册，但磁盘上的配置与当前期望不一致，需要卸载后重装。
	planUpgrade
	// planInstall：应用中心里根本没有这个包，需要安装。
	planInstall
)

// classify 判断一条链接需要什么动作。installed 是应用中心已注册应用的集合。
//
// 这是「该不该动用户桌面」的唯一判定入口：对账与单条同步都走它，
// 因此不可能出现「批量对账认为没问题、手动点同步却重装一遍」这种不一致。
func (s *Service) classify(l domain.Link, installed map[string]bool) plan {
	appName := l.EffectiveAppName()

	if !installed[appName] {
		return planInstall
	}
	// 已注册：先看配置是不是过期了。这一步同时覆盖了"列表里有、磁盘上没有"的不一致状态。
	if s.isStale(l) {
		return planUpgrade
	}
	// 配置正常：再看是否需要唤醒。
	state, err := s.cli.Status(appName)
	if err != nil {
		slog.Warn("查询应用状态失败，按需唤醒", "appName", appName, "error", err)
		return planStart
	}
	if !state.Active() {
		return planStart
	}
	return planNone
}

// NeedsInstall 报告该链接的桌面图标是否需要重新注册或唤醒。
//
// 供「用户点击保存后立即同步」这类增量路径使用：
// 配置没变化时直接返回 false，避免每次保存都卸载重装一遍同一个图标。
func (s *Service) NeedsInstall(l domain.Link) bool {
	if !s.cli.Available() {
		// 模拟模式：没有真实应用中心可查，交给调用方按"需要"处理。
		return true
	}
	names, err := s.cli.List()
	if err != nil {
		// 读不到列表时宁可重装一次，也不要留下一个"看着有、点开是空的"图标。
		return true
	}
	installed := make(map[string]bool, len(names))
	for _, n := range names {
		installed[n] = true
	}
	return s.classify(l, installed) != planNone
}

// defaultInstallRoots 枚举飞牛可能把应用装到的位置。
//
// 不同版本 / 不同存储卷布局下位置不同：/var/apps、@appcenter、@appstore，
// 以及宿主机根目录挂载视图。一次性全部覆盖，读不到就按「无配置」处理。
func defaultInstallRoots() []string {
	roots := []string{
		"/var/apps",
		"/usr/local/apps/@appcenter",
		"/host/root/var/apps",
		"/host/root/usr/local/apps/@appcenter",
	}
	for v := 1; v <= 12; v++ {
		roots = append(roots,
			fmt.Sprintf("/vol%d/@appcenter", v),
			fmt.Sprintf("/vol%d/@appstore", v),
			fmt.Sprintf("/host/root/vol%d/@appcenter", v),
			fmt.Sprintf("/host/root/vol%d/@appstore", v),
		)
	}
	return roots
}

// installDirsFor 返回某个应用可能的安装目录列表。
func (s *Service) installDirsFor(appName string) []string {
	roots := s.installRoots
	if len(roots) == 0 {
		roots = defaultInstallRoots()
	}
	dirs := make([]string, 0, len(roots)*2)
	for _, root := range roots {
		dirs = append(dirs, filepath.Join(root, appName), filepath.Join(root, appName, "target"))
	}
	return dirs
}

// SetInstallRoots 覆盖安装根目录集合（测试与自定义部署布局时使用）。
func (s *Service) SetInstallRoots(roots []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.installRoots = roots
}

// readInstalledEntry 读取已安装应用的桌面入口定义。
//
// 路径推导（这点非常容易搞错）：
//
//	源码包里  app/ui/config      —— desktop_uidir=ui，fnpack 校验 app/{desktop_uidir}/
//	安装后    $TRIM_APPDEST/ui/config
//
// 因为「app/ 目录里的内容」会成为应用的 target 目录，app/ 这一层本身被剥掉。
// 所以磁盘上真实可读的相对路径是 <安装目录>/ui/config，
// 而 <安装目录> 可能是 <root>/<appname>，也可能是 <root>/<appname>/target，
// 两者都试一遍，适配不同版本的存储卷布局。
func (s *Service) readInstalledEntry(appName string) (uiEntry, bool) {
	rel := filepath.FromSlash(UIDir + "/config")
	for _, dir := range s.installDirsFor(appName) {
		data, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil || len(data) == 0 {
			continue
		}
		var cfg uiConfig
		if err := json.Unmarshal(data, &cfg); err != nil {
			continue
		}
		if e, ok := cfg.Entries[UIEntryKey(appName)]; ok {
			return e, true
		}
	}
	return uiEntry{}, false
}

// isStale 判断已安装的桌面入口是否与当前期望不一致。
//
// 早期版本用「字符串包含 `"port"` / `"allUsers": true`」来猜，容易误判也难维护；
// 这里改为结构化比较：把期望的入口生成出来，和磁盘上的逐字段比对。
//
// 注意它比的是两样东西，缺一不可：
//
//  1. ui/config —— 桌面图标本身的定义；
//  2. ui/index.cgi —— 无端口形态真正的执行体。
//
// 第 2 条是补上去的，起因是一次真机事故：主程序升级修好了 CGI 脚本，
// 但已存在的快捷方式**一点变化都没有**，点开照旧是 500。原因是 isStale
// 只比对 ui/config，而这次修的东西全在 index.cgi 里 —— 配置一字未变，
// 于是被判定为"健康"，永远不会重建。用户看到的就是「你说修好了，可我还是坏的」。
//
// 教训：**isStale 必须覆盖"这个图标会执行的全部内容"，而不只是它的元数据。**
func (s *Service) isStale(l domain.Link) bool {
	actual, ok := s.readInstalledEntry(l.EffectiveAppName())
	if !ok {
		// 列表里说有、磁盘上找不到配置文件，说明状态不一致，重装最稳。
		return true
	}
	spec := s.buildSpec(l, s.desktopPort(l))
	// 期望值直接取自打包层唯一的构造入口，与写盘内容天然一致。
	if !actual.Equal(BuildUIEntry(spec)) {
		return true
	}
	return s.cgiScriptOutdated(l, spec)
}

// cgiScriptOutdated 报告磁盘上的 ui/index.cgi 是否与当前版本应当生成的不一致。
//
// 只对需要 CGI 的形态生效：有端口的入口根本不生成这个文件，
// 目录里万一残留一份旧脚本也不该触发重装。
//
// 比对采用"重新生成一份逐字对照"而不是存版本号或哈希：
// 生成逻辑本来就有唯一入口（renderCGIScript），再生成一次是最不容易出错的做法 ——
// 只要模板改了，这里立刻就能看出来，不需要任何人记得去同步一个版本常量。
func (s *Service) cgiScriptOutdated(l domain.Link, spec PackageSpec) bool {
	expected, needed := renderCGIScript(spec)
	if !needed {
		return false
	}
	for _, dir := range s.installDirsFor(l.EffectiveAppName()) {
		data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(UIDir+"/index.cgi")))
		if err != nil {
			continue
		}
		return string(data) != expected
	}
	// 应用装在列表里，却处处找不到这个脚本 —— 图标大概率真的是坏的，重装兜底。
	return true
}

// PortalAppName 是本应用自身的应用中心包名。
//
// 刻意不带 `qlink2d.` 前缀：那个前缀属于「桌面快捷式子应用」的命名空间，
// 本应用是一个正常的第三方应用，混在一起会被孤立清理逻辑误伤。
const PortalAppName = "qlink2desktop"

// RestartSelf 重启本应用自身的服务。
//
// # 为什么不能在本进程里「Stop 再 Start」
//
// 第一版就是这么写的，真机上**必然把应用停死**。原因不是代码写错，而是
// 这条路在机理上走不通：`appcenter-cli stop` 是**同步**的 —— 它调用应用
// 自己的 cmd/main stop，后者向本进程发 TERM，再每秒轮询等它退出（10 秒
// 宽限，超时 KILL）。也就是说，Stop 返回之前本进程已经不存在了，
// "Stop 之后再 Start" 那几行代码永远执行不到。
// 真机日志（早期实现把应用停死的那一次）：
//
//	22:53:13 POST /api/system/restart → 202
//	22:53:14 [main] Stopping qlink2desktop...
//	22:53:24 [main] send KILL signal to PID:516508...
//	22:53:25 [main] qlink2desktop stopped.   ← 应用此后再也没起来
//
// 更糟的是它还会顺带把优雅退出卡死：`Service.Stop` 阻塞在等 appcenter-cli
// 返回，而 appcenter-cli 在等我们退出 —— 于是 `Coordinator.Stop()` 里的
// `wg.Wait()` 永远等不到，退出流程也一起僵住。
//
// # 现在的做法
//
// 把"等一会儿 → stop → 等一会儿 → start"整条链交给一个**另立会话、
// 不属于本进程**的 shell（见 detachSysProcAttr 的说明），本函数只负责
// 把它点着就返回。进程随后被那个 shell 停掉时，链子的后半段照跑不误。
//
// 调用方仍然必须把它放进后台队列：本函数几秒后就会把自己杀掉，
// 放进请求路径只会让用户收到一个与事实相反的超时。
func (s *Service) RestartSelf(ctx context.Context) error {
	if !s.cli.Available() {
		return ErrCLIUnavailable
	}
	cliPath := s.cliPath()
	if cliPath == "" {
		// 只存在于模拟模式，但说清楚比让用户对着一个转圈的界面强。
		return fmt.Errorf("无法定位飞牛应用中心命令行，请改用应用中心重启")
	}
	if err := spawnDetachedShell(buildRestartScript(cliPath, s.restartLogPath())); err != nil {
		return fmt.Errorf("提交重启任务失败: %w", err)
	}
	return nil
}

// cliPath 返回应用中心命令行的绝对路径。
//
// 重启脚本要脱离本进程运行，因此不能依赖 PATH 与当前工作目录 ——
// 必须写死绝对路径。取不到时返回空串，由调用方给出明确错误。
func (s *Service) cliPath() string {
	if p, ok := s.cli.(*ExecCLI); ok {
		return p.Path()
	}
	return ""
}

// restartLogPath 是重启脚本的旁路日志。
//
// 为什么单独一份：真正干活的 shell 活过了本进程，而本进程的日志到此为止。
// 重启失败时（start 报错、应用中心忙）那段记录是**唯一**线索，
// 没有它就只能看到一个"停掉之后再也没起来"的应用。
func (s *Service) restartLogPath() string {
	if s.logDir == "" {
		return ""
	}
	return filepath.Join(s.logDir, "restart.log")
}

// buildRestartScript 生成交给脱离会话的 shell 执行的脚本。
//
// 抽成纯函数是为了能测：脚本内容是这个功能里唯一"看得见"的契约，
// 而它在真机上出错的代价是应用起不来，不能只靠肉眼检查。
func buildRestartScript(cliPath, logPath string) string {
	const stamp = `"[$(date '+%Y-%m-%d %H:%M:%S')]`
	cli := shellQuote(cliPath)
	app := shellQuote(PortalAppName)

	lines := []string{
		"#!/bin/sh",
		"# QLink2Desktop 自身重启。由主进程在退出前 spawn，运行在独立会话里。",
		"# 每一步都留痕迹：这段脚本比写它的进程活得久，是失败时唯一的线索。",
	}
	if logPath != "" {
		// 整段落到旁路日志里；没有它就只剩一个"停掉之后再也没起来"的应用。
		lines = append(lines, "exec >> "+shellQuote(logPath)+" 2>&1")
	}
	lines = append(lines,
		"echo "+stamp+" 重启开始（本行由独立会话写入）\"",
		fmt.Sprintf("sleep %d", int(restartHandoffDelay/time.Second)),
		// stop 会杀掉发起方；脚本本身已脱离会话，所以能继续往下走。
		"echo "+stamp+" 停止 "+PortalAppName+"\"",
		fmt.Sprintf("if %s stop %s; then echo %s stop 成功\"; else echo %s stop 失败（仍继续尝试启动）\"; fi",
			cli, app, stamp, stamp),
		// 等应用中心把套接字收干净：太快 start 可能读到上一个进程残留的
		// socket 文件，表现为"重启完点开还是 502"。
		fmt.Sprintf("sleep %d", int(restartSettleDelay/time.Second)),
		"echo "+stamp+" 启动 "+PortalAppName+"\"",
		fmt.Sprintf("if %s start %s; then echo %s start 成功\"; else echo %s start 失败 —— 请到飞牛应用中心手动启动\"; fi",
			cli, app, stamp, stamp),
		"echo "+stamp+" 重启流程结束\"",
	)
	return strings.Join(lines, "\n") + "\n"
}

// restartShell 是执行重启脚本的 POSIX shell，真机上恒为 /bin/sh。
//
// 做成变量而不是常量，是为了让单测能在开发机上把它指向 Git Bash 的 sh，
// 从而**真的跑一遍**脚本，而不是只对着脚本文本做字符串断言 ——
// 这段脚本出错的代价是应用起不来，光靠肉眼检查不够。
var restartShell = "/bin/sh"

// spawnDetachedShell 在一个独立会话里执行脚本，并且**不等它结束**。
//
// 三个细节缺一不可：
//   - detachSysProcAttr()（setsid）：让它脱离本进程的会话，成为"别人家的孩子"，
//     cmd/main 的 stop 只 kill PID 文件里那一个 PID，不会波及它；
//   - 标准流全部丢弃：我们马上要被 KILL，任何继承的管道都会变成悬空的写端；
//   - Release 而不是 Wait：它注定比我们活得久，不可能被回收；本进程死后
//     它会由 init 接管。这里如果 Wait，重启功能会连"返回"都做不到。
func spawnDetachedShell(script string) error {
	cmd := exec.Command(restartShell, "-c", script)
	cmd.Env = os.Environ()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	cmd.SysProcAttr = detachSysProcAttr()
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// restartHandoffDelay 是「受理」到「真的开始停」之间的等待。
//
// 它有两个用途：
//  1. 让 HTTP 的 202 先回到浏览器 —— 用户至少能看到"已受理"，
//     而不是一个连响应都没拿到的断连；
//  2. 给前端留出弹出「服务正在重启」等待层的时间。
//
// 不能太短，否则界面还没反应过来服务就没了，用户会以为点崩了。
const restartHandoffDelay = 4 * time.Second

// restartSettleDelay 是 stop 与 start 之间的等待。
//
// 不是一个可以随便调的"感觉值"：它要覆盖的是套接字文件清理。
// 宁可多等两秒，也不要留下一个起不来的服务 —— 用户点重启的预期是
// "重启完还能用"，不是"重启完就没了"。
const restartSettleDelay = 3 * time.Second

// ---------------------------------------------------------------- 自身应用同步

// SyncPortal 把门户自身的名称 / 可见性就地写回已安装的桌面配置，
// 使用户改设置后无需重装整个应用即可生效。
func (s *Service) SyncPortal(settings domain.Settings) {
	appName := PortalAppName
	// 安装后入口配置位于 $TRIM_APPDEST/ui/config，原因见 readInstalledEntry。
	rel := filepath.FromSlash(UIDir + "/config")
	updated := 0
	for _, dir := range s.installDirsFor(appName) {
		path := filepath.Join(dir, rel)
		if !fileExists(path) {
			continue
		}
		if err := patchPortalConfig(path, settings); err != nil {
			slog.Warn("更新门户桌面配置失败", "path", path, "error", err)
			continue
		}
		updated++
	}
	if updated > 0 {
		slog.Info("门户桌面配置已同步", "files", updated, "name", settings.PortalName)
	}
}

func patchPortalConfig(path string, settings domain.Settings) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return err
	}
	raw, ok := root[".url"]
	if !ok {
		return fmt.Errorf("%s 缺少 .url 字段", path)
	}
	var entries map[string]map[string]any
	if err := json.Unmarshal(raw, &entries); err != nil {
		return err
	}
	for _, e := range entries {
		e["title"] = settings.PortalName
		e["allUsers"] = settings.PortalAllUsers
		e["type"] = string(settings.PortalUI.Normalize())
	}
	encoded, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	root[".url"] = encoded
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomicLocal(path, out, 0o644)
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

// writeFileAtomicLocal 与 store 包同款的原子写，这里为避免跨包依赖做了本地副本。
func writeFileAtomicLocal(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".qlink2d-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, perm); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// routeName 返回路由模式的中文名，仅用于日志可读性。
func routeName(m routeMode) string {
	switch m {
	case routeDirect:
		return "直连端口"
	default:
		return "CGI 跳转"
	}
}

// fail 记录失败状态。
func (s *Service) fail(linkID, appName string, err error) {
	slog.Error("桌面图标注册失败", "appName", appName, "error", err)
	s.setFailure(linkID, appName, err)
}

// ErrCLINotFound 便于上层判断「当前环境没有飞牛应用中心」。
var ErrCLINotFound = ErrCLIUnavailable
