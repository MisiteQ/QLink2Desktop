package forward

import (
	"context"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/MisiteQ/qlink2desktop/internal/domain"
)

// dialTimeout 是连接目标服务的超时。
//
// 选 10 秒的理由：目标在局域网内通常毫秒级可达；跨公网或目标已关机时，
// 不设超时会让 goroutine 与 fd 一直挂到 TCP 层的默认重试结束（分钟级），
// 一个被扫描的端口就能把设备拖垮。10 秒对"正常但慢"的服务足够宽容，
// 对"根本不通"的目标足够果断。
const dialTimeout = 10 * time.Second

// closeWriter 是支持「半关闭」的连接。
//
// 半关闭（只关自己这一侧的写方向）对转发是必需的：很多协议靠"我发完了"
// 这个信号来结束一次交互（HTTP/1.0 的短连接、不少自定义 TCP 协议）。
// 如果转发层等到双方都收完才关，或者直接 Close 整条连接，
// 表现就是"请求发出去了但响应永远不回来"。
type closeWriter interface{ CloseWrite() error }

// tcpFwd 是一个正在运行的 TCP 转发监听。
type tcpFwd struct {
	spec     domain.Forward
	acl      ACL
	counters *Counters
	logs     *connRing
	logger   *slog.Logger
	dialer   *net.Dialer

	ln net.Listener
	// mode 记录实际落到的协议栈（双栈 / 仅 IPv4 / 仅 IPv6）。
	mode bindMode
	// sem 是并发连接的信号量；MaxConns 为 0 时保持 nil（不限制）。
	sem chan struct{}

	wg        sync.WaitGroup
	closeOnce sync.Once
	closed    chan struct{}
}

func newTCPFwd(spec domain.Forward, acl ACL, counters *Counters, logs *connRing, logger *slog.Logger) *tcpFwd {
	var sem chan struct{}
	if spec.MaxConns > 0 {
		sem = make(chan struct{}, spec.MaxConns)
	}
	return &tcpFwd{
		spec:     spec,
		acl:      acl,
		counters: counters,
		logs:     logs,
		logger:   logger,
		dialer:   &net.Dialer{Timeout: dialTimeout},
		sem:      sem,
		closed:   make(chan struct{}),
	}
}

// start 建立监听并开始接受连接。
//
// 监听失败不重试换端口：端口被占用是**用户需要知道的事实**，
// 静默换一个端口只会让规则指向一个没人预期的地方。
//
// 但绑定**形态**会退让（双栈 → 仅 IPv4 → 仅 IPv6）——
// 换形态不改变用户填的端口，只是把"被某一侧占用"这种情况也救回来。
func (t *tcpFwd) start() error {
	ln, mode, err := listenTCP(t.spec.ListenHost, t.spec.ListenPort)
	if err != nil {
		return err
	}
	t.ln = ln
	t.mode = mode
	if mode.degraded() {
		t.logger.Warn("端口被某一侧占用，已退化为单栈监听",
			"rule", t.spec.Key(), "listen", t.spec.ListenAddr(), "mode", mode)
	}
	t.wg.Add(1)
	go t.acceptLoop()
	return nil
}

func (t *tcpFwd) acceptLoop() {
	defer t.wg.Done()
	for {
		conn, err := t.ln.Accept()
		if err != nil {
			select {
			case <-t.closed:
				return // 是我们自己关的，正常退出
			default:
			}
			// 临时的 accept 错误（fd 耗尽等）不该终止整个监听，
			// 但连续失败会把日志刷爆，因此退避一下再试。
			t.logger.Warn("转发接受连接失败", "rule", t.spec.Key(), "error", err)
			if ne, ok := err.(net.Error); ok && ne.Temporary() {
				time.Sleep(50 * time.Millisecond)
				continue
			}
			return
		}
		t.wg.Add(1)
		go func() {
			defer t.wg.Done()
			t.serve(conn)
		}()
	}
}

// serve 处理一条连接：访问控制 → 并发上限 → 拨号目标 → 双向转发。
func (t *tcpFwd) serve(src net.Conn) {
	defer src.Close()

	srcAddr := ""
	if ra := src.RemoteAddr(); ra != nil {
		srcAddr = ra.String()
	}

	if !t.acl.Allows(srcAddr) {
		t.counters.Denied.Add(1)
		t.record(ConnLog{
			At: time.Now(), Protocol: "tcp", Source: srcAddr,
			Target: t.spec.TargetAddr(), Outcome: OutcomeDenied,
		})
		t.logger.Warn("转发连接被访问控制拒绝", "rule", t.spec.Key(), "source", srcAddr)
		return
	}

	if t.sem != nil {
		select {
		case t.sem <- struct{}{}:
			defer func() { <-t.sem }()
		default:
			t.counters.Limited.Add(1)
			t.record(ConnLog{
				At: time.Now(), Protocol: "tcp", Source: srcAddr,
				Target: t.spec.TargetAddr(), Outcome: OutcomeLimited,
			})
			t.logger.Warn("转发连接超出并发上限", "rule", t.spec.Key(),
				"source", srcAddr, "max", t.spec.MaxConns)
			return
		}
	}

	t.counters.Total.Add(1)
	t.counters.Active.Add(1)
	defer t.counters.Active.Add(-1)

	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	defer cancel()

	up, err := t.dialer.DialContext(ctx, "tcp", t.spec.TargetAddr())
	if err != nil {
		t.counters.Failed.Add(1)
		t.record(ConnLog{
			At: time.Now(), Protocol: "tcp", Source: srcAddr,
			Target: t.spec.TargetAddr(), Outcome: OutcomeDialFailed,
			DurationMs: time.Since(started).Milliseconds(), Err: shortErr(err),
		})
		t.logger.Warn("转发连接目标失败", "rule", t.spec.Key(),
			"target", t.spec.TargetAddr(), "error", err)
		return
	}
	defer up.Close()

	// 两个方向各一个 goroutine。
	//
	// 用 channel 回传字节数而不是直接写共享变量：两个方向确实是并行的，
	// 靠 <-copied 拿到结果就自然完成同步，不需要额外的锁或原子量。
	copied := make(chan int64, 1)
	go func() {
		n, _ := io.Copy(up, src)
		// 我这边发完了：半关闭到目标的方向，但继续收它的回包。
		if cw, ok := up.(closeWriter); ok {
			_ = cw.CloseWrite()
		}
		copied <- n
	}()

	outN, _ := io.Copy(src, up)
	inN := <-copied
	if cw, ok := src.(closeWriter); ok {
		_ = cw.CloseWrite()
	}

	t.counters.BytesIn.Add(inN)
	t.counters.BytesOut.Add(outN)
	t.record(ConnLog{
		At: time.Now(), Protocol: "tcp", Source: srcAddr,
		Target: t.spec.TargetAddr(), Outcome: OutcomeOK,
		BytesIn: inN, BytesOut: outN, DurationMs: time.Since(started).Milliseconds(),
	})
}

func (t *tcpFwd) record(e ConnLog) { t.logs.add(e) }

// close 停止监听并等待在途连接收尾。
//
// 等待是有上限意义的：转发的是长连接（SSH、SMB 都可能连几小时），
// 强关会让用户看到一个"刚才还连着的会话突然断了"。
// 但也不能无限等——进程退出路径上必须有终点，所以由调用方决定是否限时。
func (t *tcpFwd) close() {
	t.closeOnce.Do(func() {
		close(t.closed)
		if t.ln != nil {
			_ = t.ln.Close()
		}
	})
	t.wg.Wait()
}
