package app

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/MisiteQ/qlink2desktop/internal/discovery"
	"github.com/MisiteQ/qlink2desktop/internal/domain"
	"github.com/MisiteQ/qlink2desktop/internal/fnos"
	"github.com/MisiteQ/qlink2desktop/internal/proxy"
	"github.com/MisiteQ/qlink2desktop/internal/store"
)

// 后台队列的参数。
const (
	// jobQueueSize 是待处理工作的上限。取一个明显大于"人多手快"的量级即可，
	// 真正的意义是给它一个上界：队列满时宁可明确报错，也不要无限堆积内存。
	jobQueueSize = 64

	// jobTimeout 是单件工作的兜底超时。
	//
	// 它**不是**主要的时限来源——真正的界限在 fnos.exec 里（单条 appcenter 命令
	// 90 秒 / 3 分钟）。这里只是防止某天出现一条卡死的路径把整个队列焊死。
	// 取值要覆盖最坏的一件工作：查询列表 + 打包 + 安装 + 唤醒。
	jobTimeout = 10 * time.Minute
)

// job 是交给后台队列的一件工作。
type job struct {
	// linkID / appName / phase 用于「入队即反映状态」：
	// 用户点完按钮立刻看到「排队中」，而不是面对一个没有任何反馈的界面。
	linkID  string
	appName string
	phase   domain.Phase
	detail  string

	run func(ctx context.Context) error
	// onError 决定失败怎么记录。留空则只记一条 Warn 日志。
	// 有的工作必须落到失败态让用户看见（安装），有的只该记日志（注销图标，
	// 它本来就有「清理遗留图标」作为兜底）。
	onError func(err error)
}

// Coordinator 是「链接定义」与「飞牛桌面 + 内置代理」之间的编排者。
//
// 它是本项目唯一同时持有 store / fnos / proxy 三者的组件，
// 因此也是唯一需要理解「三者先后顺序」的地方：
//
//	先调代理（端口就绪） → 再装桌面图标（入口指向该端口） → 最后才可能清理
//
// 把这段顺序知识集中在一个类型里，而不是散落在 HTTP 处理器中，
// 是这次重构在可维护性上最直接的收益。
//
// 它同时是这个项目里**唯一**知道「哪些动作慢」的地方：凡是会调用
// appcenter-cli 的动作（注册 / 注销 / 对账 / 清理）都走在自己的后台队列上，
// 请求路径只负责受理。详见 enqueue 的注释。
type Coordinator struct {
	store *store.Store
	svc   *fnos.Service
	proxy *proxy.Manager
	log   *slog.Logger

	base int

	mu    sync.Mutex
	ports map[string]int                   // linkID → 已分配的本机代理端口
	dials map[string]proxy.DialContextFunc // linkID → 复用的拨号器（SSH 隧道）

	// 后台队列。root 的生命周期跟随进程，而不是任何一次请求——
	// 这是整件事的关键：请求返回后 r.Context() 立刻被取消，
	// 继承它的任务会在动手的那一刻就被掐断。
	root   context.Context
	cancel context.CancelFunc
	jobs   chan job
	wg     sync.WaitGroup

	// onChange 在状态发生变化时被调用，由装配层注入（用来推 SSE）。
	// 只在装配期写入，服务开始受理请求之后不再改动。
	onChange func()
}

// NewCoordinator 创建编排器，并把「桌面端口」的解析权接管过来。
//
// 关键点：Service 通过回调向编排器询问端口，而不是自己持有端口。
// 端口是运行态（进程重启就变），把它写进 Link 会造成「持久化了一份过期事实」，
// 这正是原实现里一些难查问题的根源。
func NewCoordinator(st *store.Store, svc *fnos.Service, pm *proxy.Manager, base int, logger *slog.Logger) *Coordinator {
	if logger == nil {
		logger = slog.Default()
	}
	if base <= 1024 {
		base = proxy.DefaultProxyPortBase
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := &Coordinator{
		store:  st,
		svc:    svc,
		proxy:  pm,
		log:    logger,
		base:   base,
		ports:  make(map[string]int),
		dials:  make(map[string]proxy.DialContextFunc),
		root:   ctx,
		cancel: cancel,
		jobs:   make(chan job, jobQueueSize),
	}
	svc.SetPortResolver(c.desktopPortFor)

	c.wg.Add(1)
	go c.worker()
	return c
}

// SetOnChange 注册状态变化回调。
//
// 必须在服务开始受理请求之前调用（装配期一次性设置），
// 之后只读——因此不需要加锁。
func (c *Coordinator) SetOnChange(fn func()) { c.onChange = fn }

// Pending 返回后台队列里还没做完的工作数量（诊断用）。
func (c *Coordinator) Pending() int { return len(c.jobs) }

/* ------------------------------------------------------------------ 后台队列 */

// enqueue 把一件「慢工作」交给后台串行队列。
//
// 为什么需要这个队列——这是真机反馈出来的一个真实故障：
//
//	注册与注销都要调用 appcenter-cli，而单条命令的超时上限是 3 分钟
//	（见 fnos.exec.mutateTimeout）。请求路径里同步执行它，就意味着后端
//	最坏要 6 分钟才写得出响应，而前端的网络预算是 15 秒。用户看到的
//	是一条「请求超时」的报错，可服务端其实还在正常干活——报错是假的，
//	真正的问题在响应模型：把分钟级的外部命令放进了秒级的请求路径。
//
// 所以请求路径只负责「受理 + 立刻回报当前阶段」，真正的执行结果通过
// 状态机（PhasePending → PhaseInstalled / PhaseFailed）与 SSE 事件回传。
//
// 为什么串行执行：appcenter-cli 内部要抢应用中心的锁，并发调用只会互相
// 拖慢，还让"谁先谁后"变得不可预测。串行之后每件工作都以最快速度跑完，
// 排在后面的项在界面上显示为「排队中」——这正是 PhasePending 的意义。
func (c *Coordinator) enqueue(j job) {
	if j.phase != "" {
		c.svc.MarkPhase(j.linkID, j.appName, j.phase, j.detail)
	}
	select {
	case c.jobs <- j:
	default:
		// 队列满说明前面堆了大量长任务。此时宁可明确报失败，
		// 也不要静默丢弃——用户点了按钮却什么都不发生是最糟的反馈。
		err := fmt.Errorf("后台任务队列已满（%d 件待处理），请等前面的任务跑完再试", cap(c.jobs))
		c.log.Warn("任务入队失败", "link", j.linkID, "error", err)
		if j.phase != "" {
			c.svc.SetFailure(j.linkID, j.appName, err)
		}
	}
	c.notify()
}

// worker 串行消费队列，直到进程退出。
func (c *Coordinator) worker() {
	defer c.wg.Done()
	for {
		select {
		case <-c.root.Done():
			return
		case j := <-c.jobs:
			c.runJob(j)
		}
	}
}

// runJob 执行一件工作，并把结果落到日志与状态上。
func (c *Coordinator) runJob(j job) {
	// 每件工作用自己的超时，**不继承任何请求的 context**：
	// 请求一返回，它的 context 立刻被取消，继承它等于让任务刚动手就被掐断。
	ctx, cancel := context.WithTimeout(c.root, jobTimeout)
	defer cancel()

	start := time.Now()
	err := j.run(ctx)
	elapsed := time.Since(start).Round(time.Millisecond)

	if err != nil {
		if j.onError != nil {
			j.onError(err)
		} else {
			c.log.Warn("后台任务失败", "link", j.linkID, "elapsed", elapsed.String(), "error", err)
		}
	} else if elapsed > time.Second {
		// 只给耗时明显的任务留一条记录：这既是排查依据，
		// 也是"appcenter-cli 到底有多慢"这个问题的长期观测数据。
		c.log.Info("后台任务完成", "link", j.linkID, "elapsed", elapsed.String())
	}

	c.notify()
}

// notify 通知装配层把最新状态推给前端；未注入回调时静默。
func (c *Coordinator) notify() {
	if c.onChange != nil {
		c.onChange()
	}
}

// ---------------------------------------------------------------------------
// 供 httpapi.Coordinator 接口实现
// ---------------------------------------------------------------------------

// Views 返回全部链接的定义 + 实时运行态。
func (c *Coordinator) Views() []domain.View {
	links := c.store.ListLinks()
	out := make([]domain.View, 0, len(links))
	for _, l := range links {
		out = append(out, c.view(l))
	}
	return out
}

// View 返回单条链接的视图。
func (c *Coordinator) View(id string) (domain.View, bool) {
	l, found := c.store.GetLink(id)
	if !found {
		return domain.View{}, false
	}
	return c.view(l), true
}

// ProxyPort 返回某条链接当前实际监听的本机端口；0 表示未启用代理。
func (c *Coordinator) ProxyPort(id string) int {
	if c.proxy == nil {
		return 0
	}
	return c.proxy.Port(id)
}

// Sync 把一条链接的最新定义同步到代理与飞牛桌面。
func (c *Coordinator) Sync(ctx context.Context, id string) error {
	l, found := c.store.GetLink(id)
	if !found {
		return domain.ErrNotFound
	}

	// 顺序不能反：桌面入口要写的是代理端口，必须先把代理拉起来。
	if err := c.ensureRoute(l); err != nil {
		return err
	}

	if !l.Enabled {
		return c.deactivate(ctx, l)
	}

	// 配置没变化就什么都不做。少了这一步，用户每点一次「保存」
	// 都会走一遍卸载 + 安装，桌面图标会肉眼可见地闪一下。
	if !c.svc.NeedsInstall(l) {
		c.log.Debug("桌面图标已是最新，跳过安装", "appName", l.EffectiveAppName())
		// 必须显式落一个终态。异步路径在入队时已经把这一行写成「排队中」了，
		// 这里若直接返回，那一行会永远停在「排队中」——一个不会自愈的假进度。
		c.svc.MarkPhase(l.ID, l.EffectiveAppName(), domain.PhaseInstalled, "")
		return nil
	}
	return c.svc.Install(ctx, fnos.InstallRequest{Link: l})
}

// Remove 删除一条链接，并回收它的代理监听与桌面图标。
//
// 这是**同步**语义：函数返回时桌面图标已经注销完毕。只适合启动流程与测试；
// HTTP 请求路径请用 QueueRemove，否则用户会为 3 分钟级的 appcenter 命令买单。
func (c *Coordinator) Remove(ctx context.Context, id string) error {
	deleted, err := c.detach(id)
	if err != nil {
		return err
	}

	if err := c.teardown(ctx, deleted); err != nil {
		c.log.Warn("删除链接成功，但注销桌面图标失败（可在系统页手动清理）",
			"id", id, "appName", deleted.EffectiveAppName(), "error", err)
	}
	return nil
}

// detach 摘掉定义与代理监听，返回被摘除的链接供后续清理。
//
// 顺序是有意为之：若先卸载图标而删除失败，用户会看到"链接还在、图标没了"
// 这种悬空状态；反过来最多留下一个无主图标，用户可在「系统与日志」页
// 用"清理遗留图标"显式回收。
func (c *Coordinator) detach(id string) (domain.Link, error) {
	deleted, err := c.store.DeleteLink(id)
	if err != nil {
		return domain.Link{}, err
	}
	c.dropRoute(id)
	return deleted, nil
}

// teardown 注销一条链接对应的桌面图标。
func (c *Coordinator) teardown(ctx context.Context, l domain.Link) error {
	return c.svc.Uninstall(ctx, l, c.protectedNames())
}

// SetEnabled 切换启用状态，并同步落地效果。
//
// 同样是**同步**语义，理由与 Remove 相同；请求路径用 QueueSetEnabled。
func (c *Coordinator) SetEnabled(ctx context.Context, id string, enabled bool) error {
	l, err := c.store.SetLinkEnabled(id, enabled)
	if err != nil {
		return err
	}
	return c.Sync(ctx, l.ID)
}

/* ---------------------------------------------------------------------------
 * 受理型接口（请求路径专用）
 *
 * 下面这组方法与上面的同步方法一一对应，区别只有一点：**不等待**。
 * 落盘与代理这些毫秒级的部分立刻做完并返回，凡是要碰 appcenter-cli 的
 * 部分一律交给后台队列，结果通过状态 + SSE 回传。
 * ------------------------------------------------------------------------- */

// QueueSync 受理一次「把链接同步到飞牛桌面」。
func (c *Coordinator) QueueSync(id string) {
	l, found := c.store.GetLink(id)
	if !found {
		return
	}
	c.enqueue(job{
		linkID:  l.ID,
		appName: l.EffectiveAppName(),
		phase:   domain.PhasePending,
		run: func(ctx context.Context) error {
			return c.Sync(ctx, id)
		},
	})
}

// QueueSetEnabled 立刻落盘启用状态，并把随之而来的图标动作交给后台。
func (c *Coordinator) QueueSetEnabled(id string, enabled bool) error {
	l, err := c.store.SetLinkEnabled(id, enabled)
	if err != nil {
		return err
	}
	c.QueueSync(l.ID)
	return nil
}

// QueueRemove 从面板上摘除一条链接（立即生效），并把注销图标交给后台。
//
// 返回被摘除的定义，供接口层记日志。
func (c *Coordinator) QueueRemove(id string) (domain.Link, error) {
	deleted, err := c.detach(id)
	if err != nil {
		return domain.Link{}, err
	}
	// 这里刻意不写「排队中」：链接已经从列表里消失了，没有可展示的行。
	c.enqueue(job{
		linkID:  deleted.ID,
		appName: deleted.EffectiveAppName(),
		run: func(ctx context.Context) error {
			return c.teardown(ctx, deleted)
		},
		onError: func(err error) {
			// 注销失败只记日志，不落失败态：链接已经删了，无处安放这个错误。
			// 兜底入口是系统页的「清理遗留图标」。
			c.log.Warn("注销桌面图标失败（可在系统页手动清理）",
				"id", deleted.ID, "appName", deleted.EffectiveAppName(), "error", err)
		},
	})
	return deleted, nil
}

// QueueReconcile 受理一次全量对账。
func (c *Coordinator) QueueReconcile() {
	c.enqueue(job{
		run: func(ctx context.Context) error {
			c.Reconcile(ctx)
			return nil
		},
	})
}

// QueueCleanupOrphans 受理一次「清理遗留图标」。
func (c *Coordinator) QueueCleanupOrphans() {
	c.enqueue(job{
		run: func(ctx context.Context) error {
			_, err := c.CleanupOrphans(ctx)
			return err
		},
	})
}

// QueueRestartSelf 受理一次「重启本应用服务」。
//
// 这是**必须**异步的一个动作，理由比其它几个更硬：命令本身会终止
// 执行它的那个进程。放在请求路径上等结果，用户拿到的必然是超时，
// 而服务其实正在重启 —— 那个报错和事实完全相反，还会诱导用户再点一次。
//
// 另一点要注意：把它排进队列时，队列里可能还有别的任务（正在装图标之类）。
// 这里不加特殊处理，让它们按顺序跑完再重启 —— 这比"立刻掐掉所有进行中的工作"
// 更符合用户预期（点了重启，结果发现有几个图标装到一半）。
func (c *Coordinator) QueueRestartSelf() {
	c.enqueue(job{
		run: func(ctx context.Context) error {
			return c.svc.RestartSelf(ctx)
		},
		onError: func(err error) {
			// 走到这里通常是"根本没停成"（CLI 缺失、权限不足）。
			// 真的停成了的话，进程已经没了，这条日志也写不出去 ——
			// 所以能看到的失败，基本都是"什么都没发生"，如实记下来即可。
			c.log.Error("重启应用服务失败", "error", err)
		},
	})
}

// Reconcile 全量对账，把代理与桌面图标都收敛到期望状态。
func (c *Coordinator) Reconcile(ctx context.Context) fnos.ReconcileResult {
	links := c.store.ListLinks()

	// 先把所有启用的项预置为「排队中」，消除前端从"已就绪"闪回"恢复中"的抖动。
	c.svc.MarkPending(links)

	// 1) 代理先行。
	for _, l := range links {
		if ctx.Err() != nil {
			break
		}
		if err := c.ensureRoute(l); err != nil {
			c.log.Warn("建立反向代理失败，该链接将退化为跳转入口",
				"link", l.Name, "kind", string(l.Kind), "error", err)
		}
	}

	// 2) 桌面图标对账。
	result := c.svc.Reconcile(ctx, links)

	// 注意：这里刻意不做孤儿清理。
	//
	// 早期版本会在每次对账时顺手 PruneOrphans，结果用户一安装本应用，
	// 其它工具生成的桌面图标就被当成"遗留"卸载了——
	// 那是别人的命名空间，本应用根本无权判定它是不是孤儿。
	// 清理只应发生在用户显式操作时（CleanupOrphans，前端带确认对话框），
	// 或用户删除单条链接时（Remove → Uninstall，针对自己创建的图标）。
	c.notify()
	return result
}

// CleanupOrphans 显式清理应用中心里「qlink2d.* 命名空间下、已无对应链接」的图标。
//
// 只能由用户在界面上主动触发（带二次确认），绝不在启动对账等
// 任何自动流程里调用——清理是不可逆的破坏性操作，必须由人拍板。
func (c *Coordinator) CleanupOrphans(ctx context.Context) ([]string, error) {
	links := c.store.ListLinks()
	keep := make(map[string]bool, len(links))
	for _, l := range links {
		if l.Enabled {
			keep[l.EffectiveAppName()] = true
		}
	}
	orphans, err := c.svc.PruneOrphans(ctx, keep)
	if err != nil {
		return nil, err
	}
	if len(orphans) > 0 {
		c.log.Info("已按用户指令清理遗留桌面图标", "count", len(orphans), "apps", orphans)
	}
	c.notify()
	return orphans, nil
}

// EnsureRoutes 确保所有应当存在的代理监听都活着（幂等）。
//
// 周期调用它来兜住"代理意外退出"（例如后端地址被改坏、端口被别的程序抢走）。
// 相比每轮都重装图标，它的代价极低：配置没变时 proxy.Start 直接返回。
func (c *Coordinator) EnsureRoutes() {
	for _, l := range c.store.ListLinks() {
		if !needsProxy(l) {
			// 已停用或不需要代理的项，顺手回收可能残留的监听。
			if c.ProxyPort(l.ID) > 0 {
				c.dropRoute(l.ID)
			}
			continue
		}
		if err := c.ensureRoute(l); err != nil {
			c.log.Warn("代理守护失败", "link", l.Name, "error", err)
		}
	}
}

// Stop 停止后台队列与全部代理监听（进程退出时调用）。
//
// 先取消后台任务再停代理：被取消的那件工作可能正在跑 appcenter-cli，
// 它会在自己的 context 上立刻退出，而不是拖到命令超时。
func (c *Coordinator) Stop() {
	c.cancel()
	c.wg.Wait()
	if c.proxy != nil {
		c.proxy.StopAll()
	}
}

// ---------------------------------------------------------------------------
// 内部
// ---------------------------------------------------------------------------

// view 组装一条链接的视图，把运行态按当前事实修正。
func (c *Coordinator) view(l domain.Link) domain.View {
	st := c.svc.StatusOf(l.ID, l.EffectiveAppName())
	if p := c.ProxyPort(l.ID); p > 0 {
		st.ProxyPort = p
		st.ProxyActive = true
	}

	if !l.Enabled {
		// 停用态优先：即便上一次留下了失败原因或安装进度，
		// 用户此刻看到的事实是"这条链接没在用"。进行中的任务不覆盖。
		if !st.Phase.Busy() {
			st = domain.Status{
				LinkID:  l.ID,
				AppName: l.EffectiveAppName(),
				Phase:   domain.PhaseStopped,
			}
		}
	}
	return domain.NewView(l, st)
}

// desktopPortFor 回答「这条链接的桌面入口应当指向哪个本机端口」。
//
// 返回 0 表示"不需要经过代理"，此时 fnos 包会自动回退到直连真实端口
// （本机端口形态）或 CGI 跳转（缺少代理的端口映射形态）。
func (c *Coordinator) desktopPortFor(l domain.Link) int {
	if !needsProxy(l) {
		return 0
	}
	return c.ProxyPort(l.ID)
}

// deactivate 注销一条被停用链接的桌面图标。
func (c *Coordinator) deactivate(ctx context.Context, l domain.Link) error {
	return c.svc.Uninstall(ctx, l, c.protectedNames())
}

// protectedNames 返回仍被其它启用链接占用的包名集合。
//
// 传给 Uninstall 做白名单保护：并发编辑时即使两条链接意外指向同一包名，
// 也不会出现"删掉一条把另一条的图标也带走了"。
func (c *Coordinator) protectedNames(exclude ...string) map[string]bool {
	skip := make(map[string]bool, len(exclude))
	for _, s := range exclude {
		skip[s] = true
	}
	out := map[string]bool{}
	for _, l := range c.store.ListLinks() {
		if !l.Enabled {
			continue
		}
		name := l.EffectiveAppName()
		if skip[name] {
			continue
		}
		out[name] = true
	}
	return out
}

// ensureRoute 按最新定义建立或拆除代理路由。
func (c *Coordinator) ensureRoute(l domain.Link) error {
	if !needsProxy(l) {
		c.dropRoute(l.ID)
		return nil
	}

	target := l.BackendURL()
	if strings.TrimSpace(target) == "" {
		return fmt.Errorf("%w: 无法推导 %q 的后端地址", domain.ErrValidation, l.Name)
	}

	port, err := c.assignPort(l)
	if err != nil {
		return err
	}

	return c.proxy.Start(proxy.Route{
		ID:            l.ID,
		Port:          port,
		Target:        target,
		SkipTLSVerify: l.SkipTLSVerify,
		Dial:          c.dialerFor(l),
	})
}

// dropRoute 停止并遗忘一条路由，让它的端口可以被重新分配。
func (c *Coordinator) dropRoute(id string) {
	if c.proxy != nil {
		c.proxy.Stop(id)
	}
	c.mu.Lock()
	delete(c.ports, id)
	delete(c.dials, id)
	c.mu.Unlock()
}

// assignPort 为链接分配本机端口（幂等：已分配过就直接复用）。
func (c *Coordinator) assignPort(l domain.Link) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if p, ok := c.ports[l.ID]; ok && p > 0 {
		return p, nil
	}

	used := make(map[int]bool, len(c.ports))
	for _, p := range c.ports {
		used[p] = true
	}

	p := pickStablePort(stablePortStart(c.base, l.ID), used)
	if p <= 0 {
		return 0, fmt.Errorf("无法为 %q 分配本机端口（%d 起已被占用）", l.Name, c.base)
	}
	c.ports[l.ID] = p
	return p, nil
}

// dialerFor 在目标主机只可经由 SSH 到达时，返回一个走隧道的拨号器。
//
// 判定依据很直接：链接里的 Host 能匹配到用户保存过的某台「远端主机」，
// 且当前环境有 ssh 客户端。否则返回 nil，使用默认的直连拨号。
func (c *Coordinator) dialerFor(l domain.Link) proxy.DialContextFunc {
	if l.Kind != domain.KindProxy || strings.TrimSpace(l.Host) == "" {
		return nil
	}

	c.mu.Lock()
	if fn, ok := c.dials[l.ID]; ok {
		c.mu.Unlock()
		return fn
	}
	c.mu.Unlock()

	host, found := c.matchHost(l.Host)
	if !found {
		return nil
	}
	tunnel := discovery.NewSSHTunnel(host)
	if !tunnel.Available() {
		return nil
	}

	c.log.Info("目标主机将经由 SSH 隧道访问", "link", l.Name, "gateway", host.Address)
	fn := tunnel.DialContext

	c.mu.Lock()
	c.dials[l.ID] = fn
	c.mu.Unlock()
	return fn
}

// matchHost 在已保存的远端主机里查找与给定地址（或名称）匹配的一台。
func (c *Coordinator) matchHost(addr string) (domain.Host, bool) {
	addr = strings.TrimSpace(addr)
	for _, h := range c.store.ListHosts() {
		if strings.EqualFold(h.Address, addr) || strings.EqualFold(h.Name, addr) {
			return h, true
		}
	}
	return domain.Host{}, false
}

// LinkByAppName 供诊断接口反查链接。
func (c *Coordinator) LinkByAppName(appName string) (domain.Link, bool) {
	return c.store.LinkByAppName(appName)
}
