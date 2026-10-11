package forward

import (
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MisiteQ/qlink2desktop/internal/domain"
)

// UDP 转发与 TCP 最大的差别：UDP **没有连接**。
//
// 监听侧收到的是一个一个的数据报，每个都可能来自不同的对端；
// 传输层不会告诉我们"这个对端说完话了"。所以必须自己维护
// 「源地址 → 到目标的 socket」这张会话表，并自己决定什么时候回收。
// 这是整个转发功能里唯一一处需要显式生命周期管理的地方。

const (
	// udpBufferSize 是单个数据报的读取缓冲。
	//
	// 8192 是一个权衡：它完整容纳 DNS（含 EDNS0 的 4096）、NTP、
	// 绝大多数游戏与实时音视频的包；再大的 UDP 报文本身就会在 IP 层
	// 被分片，而分片重组失败的责任在端点而不是转发层。
	// 每个会话有收发两个这样的缓冲，定成 65535 会让上千个会话吃掉上百 MB。
	udpBufferSize = 8192

	// udpIdleTimeout 是一个会话多久没有双向流量就被回收。
	udpIdleTimeout = 60 * time.Second

	// udpReapInterval 是回收扫描的间隔。
	udpReapInterval = 15 * time.Second
)

// udpSession 是一个「源地址 → 目标」的 UDP 会话。
type udpSession struct {
	key string
	src *net.UDPAddr
	// up 是到目标的已连接 socket。用 connected socket 而不是每次 WriteTo，
	// 这样内核只在这一条五元组上做路由与校验，开销更低。
	up *net.UDPConn

	// last 是最近一次有流量的时间（Unix 纳秒），供回收扫描判断空闲。
	last atomic.Int64
	// closed 关闭后通知回程 goroutine 退出。
	closed chan struct{}
	// once 保证 closed 只被关一次（回收与整条规则停止可能同时发生）。
	once sync.Once
}

func newUDPSession(key string, src *net.UDPAddr, up *net.UDPConn) *udpSession {
	s := &udpSession{key: key, src: src, up: up, closed: make(chan struct{})}
	s.touch()
	return s
}

func (s *udpSession) touch() { s.last.Store(time.Now().UnixNano()) }

// idleFor 返回该会话已空闲多久。
func (s *udpSession) idleFor() time.Duration {
	return time.Since(time.Unix(0, s.last.Load()))
}

// close 关闭会话。重复调用安全。
func (s *udpSession) close() {
	s.once.Do(func() {
		close(s.closed)
		_ = s.up.Close()
	})
}

// udpFwd 是一个正在运行的 UDP 转发监听。
type udpFwd struct {
	spec     domain.Forward
	acl      ACL
	counters *Counters
	logs     *connRing
	logger   *slog.Logger

	conn *net.UDPConn
	// mode 记录实际落到的协议栈（双栈 / 仅 IPv4 / 仅 IPv6）。
	mode bindMode
	// sem 限制并发会话数；MaxConns 为 0 时保持 nil。
	sem chan struct{}

	mu       sync.Mutex
	sessions map[string]*udpSession

	wg        sync.WaitGroup
	closeOnce sync.Once
	closed    chan struct{}
}

func newUDPFwd(spec domain.Forward, acl ACL, counters *Counters, logs *connRing, logger *slog.Logger) *udpFwd {
	var sem chan struct{}
	if spec.MaxConns > 0 {
		sem = make(chan struct{}, spec.MaxConns)
	}
	return &udpFwd{
		spec:     spec,
		acl:      acl,
		counters: counters,
		logs:     logs,
		logger:   logger,
		sem:      sem,
		sessions: make(map[string]*udpSession),
		closed:   make(chan struct{}),
	}
}

// start 建立监听并启动读取与回收两个后台循环。
//
// 与 TCP 一样会退让绑定形态：UDP 的 `:port` 同样是双栈绑定，
// 被某一侧占用时退到单栈能救回整条规则。
func (u *udpFwd) start() error {
	conn, mode, err := listenUDP(u.spec.ListenHost, u.spec.ListenPort)
	if err != nil {
		return err
	}
	u.conn = conn
	u.mode = mode
	if mode.degraded() {
		u.logger.Warn("UDP 端口被某一侧占用，已退化为单栈监听",
			"rule", u.spec.Key(), "listen", u.spec.ListenAddr(), "mode", mode)
	}
	u.wg.Add(2)
	go u.readLoop()
	go u.reapLoop()
	return nil
}

// readLoop 读取外部发来的数据报，转发给目标。
func (u *udpFwd) readLoop() {
	defer u.wg.Done()
	buf := make([]byte, udpBufferSize)

	for {
		n, src, err := u.conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-u.closed:
				return
			default:
			}
			u.logger.Warn("UDP 转发读取失败", "rule", u.spec.Key(), "error", err)
			if ne, ok := err.(net.Error); ok && ne.Temporary() {
				time.Sleep(50 * time.Millisecond)
				continue
			}
			return
		}
		if n == 0 || src == nil {
			continue
		}

		if !u.acl.Allows(src.String()) {
			u.counters.Denied.Add(1)
			u.record(ConnLog{
				At: time.Now(), Protocol: "udp", Source: src.String(),
				Target: u.spec.TargetAddr(), Outcome: OutcomeDenied,
			})
			continue
		}

		sess, ok := u.sessionOf(src)
		if !ok {
			continue
		}
		// 写入是同步的（就在这个 goroutine 里），所以复用 buf 是安全的：
		// 下一次 ReadFromUDP 开始之前，这次的 Write 已经完成。
		if _, err := sess.up.Write(buf[:n]); err != nil {
			u.logger.Debug("UDP 转发写入目标失败", "rule", u.spec.Key(), "error", err)
			continue
		}
		u.counters.BytesIn.Add(int64(n))
		sess.touch()
	}
}

// sessionOf 取出（必要时新建）某个源地址的会话。
//
// 返回 false 表示没能建立会话（并发已达上限或目标不可达），
// 调用方应当直接丢弃这个数据报——UDP 本来就不保证送达，
// 与其缓存起来不如让对端重传。
func (u *udpFwd) sessionOf(src *net.UDPAddr) (*udpSession, bool) {
	key := src.String()

	u.mu.Lock()
	if sess, ok := u.sessions[key]; ok {
		sess.touch()
		u.mu.Unlock()
		return sess, true
	}
	u.mu.Unlock()

	if u.sem != nil {
		select {
		case u.sem <- struct{}{}:
			// 占位成功；会话回收时必须归还，否则上限会只减不增。
		default:
			u.counters.Limited.Add(1)
			u.record(ConnLog{
				At: time.Now(), Protocol: "udp", Source: key,
				Target: u.spec.TargetAddr(), Outcome: OutcomeLimited,
			})
			return nil, false
		}
	}

	up, err := net.DialTimeout("udp", u.spec.TargetAddr(), dialTimeout)
	if err != nil {
		if u.sem != nil {
			<-u.sem
		}
		u.counters.Failed.Add(1)
		u.record(ConnLog{
			At: time.Now(), Protocol: "udp", Source: key,
			Target: u.spec.TargetAddr(), Outcome: OutcomeDialFailed, Err: shortErr(err),
		})
		return nil, false
	}

	sess := newUDPSession(key, src, up.(*net.UDPConn))
	u.mu.Lock()
	// 双重检查：等待 Dial 期间可能已经有另一个数据报建好了会话。
	if existing, ok := u.sessions[key]; ok {
		u.mu.Unlock()
		existing.touch()
		sess.close()
		if u.sem != nil {
			<-u.sem
		}
		return existing, true
	}
	u.sessions[key] = sess
	u.mu.Unlock()

	u.counters.Total.Add(1)
	u.counters.Active.Add(1)
	u.wg.Add(1)
	go u.pump(sess)
	return sess, true
}

// pump 把目标回来的数据报送回源地址。
//
// 必须用**监听的那个 socket** 写回，不能用另开的 socket：
// 对端（以及途中的 NAT）是靠 "响应来自我发往的那个端口" 来认领回包的，
// 换一个源端口发出去，对端会把它当成来历不明的包直接丢弃。
func (u *udpFwd) pump(sess *udpSession) {
	defer u.wg.Done()
	buf := make([]byte, udpBufferSize)

	for {
		n, err := sess.up.Read(buf)
		if err != nil {
			select {
			case <-sess.closed:
				// 正常回收。
			default:
				u.logger.Debug("UDP 回程读取结束", "rule", u.spec.Key(), "error", err)
			}
			return
		}
		if n == 0 {
			continue
		}
		if _, err := u.conn.WriteToUDP(buf[:n], sess.src); err != nil {
			u.logger.Debug("UDP 回程写入失败", "rule", u.spec.Key(), "error", err)
			continue
		}
		u.counters.BytesOut.Add(int64(n))
		sess.touch()
	}
}

// reapLoop 定期回收空闲会话。
//
// 没有它，会话表只会增长：UDP 没有 FIN，一个连过一次就消失的客户端
// 会永久占住一个 socket 和一个 goroutine。这是 UDP 转发最常见的内存泄漏点。
func (u *udpFwd) reapLoop() {
	defer u.wg.Done()
	ticker := time.NewTicker(udpReapInterval)
	defer ticker.Stop()
	for {
		select {
		case <-u.closed:
			return
		case <-ticker.C:
			u.reap()
		}
	}
}

func (u *udpFwd) reap() {
	u.mu.Lock()
	stale := make([]*udpSession, 0)
	for key, sess := range u.sessions {
		if sess.idleFor() < udpIdleTimeout {
			continue
		}
		delete(u.sessions, key)
		stale = append(stale, sess)
	}
	u.mu.Unlock()

	for _, sess := range stale {
		sess.close()
		u.counters.Active.Add(-1)
		if u.sem != nil {
			<-u.sem
		}
		u.record(ConnLog{
			At: time.Now(), Protocol: "udp", Source: sess.src.String(),
			Target: u.spec.TargetAddr(), Outcome: OutcomeOK,
		})
	}
}

func (u *udpFwd) record(e ConnLog) { u.logs.add(e) }

// close 停止监听、关闭全部会话并等待后台 goroutine 收尾。
func (u *udpFwd) close() {
	u.closeOnce.Do(func() {
		close(u.closed)
		if u.conn != nil {
			_ = u.conn.Close()
		}
		// 逐个关掉会话，让 pump 的 Read 立刻返回。
		u.mu.Lock()
		for _, sess := range u.sessions {
			sess.close()
		}
		u.sessions = make(map[string]*udpSession)
		u.mu.Unlock()
	})
	u.wg.Wait()
}
