package app

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/MisiteQ/qlink2desktop/internal/domain"
)

/* ---------------------------------------------------------------------------
 * 后台队列的行为契约
 *
 * 这些用例守的是一个真机事故换来的结论：
 * 请求路径里**不能**同步等待 appcenter-cli（单条命令最长 3 分钟），
 * 否则前端 15 秒的网络预算必然超时，用户看到一条假的「请求超时」。
 *
 * 因此队列必须满足四条性质：
 *   ① 受理立即返回（不阻塞调用方）；
 *   ② 串行执行（并发调用 appcenter-cli 只会互相拖慢）；
 *   ③ 状态能走到终态（否则界面永远停在「排队中」）；
 *   ④ 退出时能取消在跑的任务（不能拖到命令自己超时）。
 * ------------------------------------------------------------------------- */

// waitFor 轮询直到条件成立；超时即失败。
//
// 异步行为没有同步的完成信号，只能轮询。轮询本身不难写，
// 难在**给它一个明确的失败出口**——少了超时，测试会以「跑不完」的形式挂住。
func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待 %s 超时（%s）", what, timeout)
}

// TestQueueSyncReturnsImmediately 验证「受理」不阻塞调用方。
func TestQueueSyncReturnsImmediately(t *testing.T) {
	env := newTestEnv(t, 28000)

	// 让队列里的第一件工作阻塞住，这样"立即返回"才有说服力：
	// 如果 QueueSync 是同步的，它会被这件工作一起堵住。
	release := make(chan struct{})
	started := make(chan struct{})
	env.coord.enqueue(job{run: func(ctx context.Context) error {
		close(started)
		<-release
		return nil
	}})
	<-started

	saved := env.save(t, domain.Link{
		Name: "受理探针", Kind: domain.KindShortcut, Path: "https://example.com",
		UI: domain.UIWindow, Enabled: true,
	})

	done := make(chan struct{})
	go func() {
		env.coord.QueueSync(saved.ID)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("QueueSync 阻塞了调用方——请求路径会被分钟级的 appcenter-cli 拖死")
	}

	// 入队即反映状态：用户点完按钮立刻看到「排队中」，而不是一片空白。
	v, _ := env.coord.View(saved.ID)
	if v.Status.Phase != domain.PhasePending {
		t.Fatalf("受理后应立即进入 pending，实际 %s", v.Status.Phase)
	}

	close(release)

	// ③ 终态必须能走到：否则界面永远停在「排队中」。
	waitFor(t, "同步完成", 5*time.Second, func() bool {
		v, _ := env.coord.View(saved.ID)
		return v.Status.Phase == domain.PhaseInstalled
	})
}

// TestQueueRemoveDetachesImmediately 验证删除「定义立刻消失、注销后台进行」。
func TestQueueRemoveDetachesImmediately(t *testing.T) {
	env := newTestEnv(t, 28050)

	saved := env.save(t, domain.Link{
		Name: "待删除", Kind: domain.KindShortcut, Path: "https://example.com",
		UI: domain.UIWindow, Enabled: true,
	})
	appName := saved.EffectiveAppName()

	// 先让它真的装上，这样后面的注销才有东西可验。
	if err := env.coord.Sync(context.Background(), saved.ID); err != nil {
		t.Fatalf("预置安装失败: %v", err)
	}
	if _, ok := env.cli.States()[appName]; !ok {
		t.Fatalf("预置安装后应有 %s", appName)
	}

	deleted, err := env.coord.QueueRemove(saved.ID)
	if err != nil {
		t.Fatalf("QueueRemove 失败: %v", err)
	}
	if deleted.ID != saved.ID {
		t.Fatalf("应返回被摘除的定义，实际 %s", deleted.ID)
	}
	if _, found := env.store.GetLink(saved.ID); found {
		t.Fatal("链接应当立刻从库中消失（落盘与注销必须解耦）")
	}

	// 注销在后台跑：给它一点时间，最终应用中心里不该再有这个图标。
	waitFor(t, "后台注销完成", 5*time.Second, func() bool {
		_, ok := env.cli.States()[appName]
		return !ok
	})
}

// TestQueueRunsSerially 验证串行执行——并发调用 appcenter-cli 只会互相拖慢。
func TestQueueRunsSerially(t *testing.T) {
	env := newTestEnv(t, 28100)

	const n = 8
	var mu sync.Mutex
	active := 0
	maxActive := 0
	done := make(chan struct{})

	for i := 0; i < n; i++ {
		env.coord.enqueue(job{run: func(ctx context.Context) error {
			mu.Lock()
			active++
			if active > maxActive {
				maxActive = active
			}
			mu.Unlock()

			// 每件工作都确实占用一段时间，重叠才会被观测到。
			time.Sleep(20 * time.Millisecond)

			mu.Lock()
			active--
			if active == 0 && maxActive > 0 {
				// 只在最后一件完成时关一次；close 两次会 panic。
				select {
				case <-done:
				default:
					close(done)
				}
			}
			mu.Unlock()
			return nil
		}})
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("队列没有在预期时间内跑完")
	}

	mu.Lock()
	got := maxActive
	mu.Unlock()
	if got != 1 {
		t.Fatalf("队列应当串行执行，实测最大并发 %d", got)
	}
}

// TestQueueNotifiesChange 验证 onChange 回调会被调用。
//
// 没有它，异步模型下前端永远收不到「排队中 → 已就绪」的变化，
// 用户看到的是一个不会自愈的假进度。
func TestQueueNotifiesChange(t *testing.T) {
	env := newTestEnv(t, 28150)

	var mu sync.Mutex
	calls := 0
	env.coord.SetOnChange(func() {
		mu.Lock()
		calls++
		mu.Unlock()
	})

	saved := env.save(t, domain.Link{
		Name: "通知探针", Kind: domain.KindShortcut, Path: "https://example.com",
		UI: domain.UIWindow, Enabled: true,
	})

	env.coord.QueueSync(saved.ID)
	waitFor(t, "通知到达", 5*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		// 入队一次 + 完成一次，至少两次。
		return calls >= 2
	})
}

// TestQueueFullReportsFailure 验证队列满时明确报错，而不是静默丢弃。
//
// 静默丢弃是最糟的反馈：用户点了按钮，界面毫无反应，
// 既没有成功也没有失败，无从判断该重试还是等待。
func TestQueueFullReportsFailure(t *testing.T) {
	env := newTestEnv(t, 28200)

	saved := env.save(t, domain.Link{
		Name: "溢出探针", Kind: domain.KindShortcut, Path: "https://example.com",
		UI: domain.UIWindow, Enabled: true,
	})

	// 先占住 worker，让通道真的能被填满。
	release := make(chan struct{})
	started := make(chan struct{})
	env.coord.enqueue(job{run: func(ctx context.Context) error {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil
	}})
	<-started

	// 填满通道：容量是 jobQueueSize，而 worker 正被第一件工作占着。
	for i := 0; i < jobQueueSize; i++ {
		env.coord.enqueue(job{run: func(ctx context.Context) error { return nil }})
	}
	if got := env.coord.Pending(); got != jobQueueSize {
		close(release)
		t.Fatalf("队列深度应为 %d，实际 %d", jobQueueSize, got)
	}

	// 第 jobQueueSize+1 件必然入不了队。
	env.coord.enqueue(job{
		linkID:  saved.ID,
		appName: saved.EffectiveAppName(),
		phase:   domain.PhasePending,
		run:     func(ctx context.Context) error { return nil },
	})

	v, _ := env.coord.View(saved.ID)
	if v.Status.Phase != domain.PhaseFailed {
		close(release)
		t.Fatalf("入队失败应当落到 failed，实际 %s（%s）", v.Status.Phase, v.Status.LastError)
	}
	if v.Status.LastError == "" {
		close(release)
		t.Fatal("失败态必须带上原因，否则用户无从判断")
	}

	close(release)
}

// TestStopCancelsRunningJob 验证退出时能掐断在跑的任务。
//
// 否则进程要等 appcenter-cli 自己超时（最长 3 分钟）才能真正退出，
// 表现为「点了停止但服务一直不消失」。
func TestStopCancelsRunningJob(t *testing.T) {
	env := newTestEnv(t, 28250)
	// 本用例自己控制退出时机，注销掉脚手架注册的默认 Stop，避免重复调用。
	// （Stop 幂等：cancel 可重复调用，wg.Wait 也能安全再等一次。）

	started := make(chan struct{})
	finished := make(chan struct{})
	env.coord.enqueue(job{run: func(ctx context.Context) error {
		close(started)
		defer close(finished)
		// 只等 ctx：被取消就必须立刻返回。
		<-ctx.Done()
		return ctx.Err()
	}})
	<-started

	stopped := make(chan struct{})
	go func() {
		env.coord.Stop()
		close(stopped)
	}()

	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop 没能掐断正在执行的任务")
	}
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop 没有返回（worker 未退出）")
	}
}
