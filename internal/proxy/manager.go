// Package proxy 管理内置的反向代理监听。
//
// 存在意义：把「局域网 / 公网的某个服务」搬到本机的一个端口上，
// 再由飞牛桌面图标指向本机端口 —— 这样飞牛 Connect 的外网穿透、
// 自签证书跳过这些能力才能统一生效。
package proxy

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/MisiteQ/qlink2desktop/internal/domain"
)

// DialContextFunc 允许注入自定义拨号器（例如带 SSH 兜底的智能拨号）。
type DialContextFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// Route 描述一条待建立的反向代理路由。
type Route struct {
	// ID 是路由标识，与本项目的一条 Link 一一对应。
	ID string
	// Port 是本机监听端口。
	Port int
	// Target 是后端地址（含协议、主机、端口、可选路径前缀）。
	Target string
	// SkipTLSVerify 用于后端使用自签证书的场景。
	SkipTLSVerify bool
	// Dial 可覆盖该条路由的拨号方式。
	//
	// 典型用途：目标在另一台机器的内网里，只能通过 SSH 隧道到达
	// （`ssh -W target host`）。此时传一个走隧道的拨号器，
	// 代理依然按普通 HTTP 转发，对上层完全透明。
	// 为 nil 时使用 Manager 上的默认拨号器。
	Dial DialContextFunc
}

// instance 是一个正在运行的代理监听。
type instance struct {
	route Route
	// parsed 是解析后的后端地址，保存下来供状态查询与日志使用。
	parsed *url.URL
	srv    *http.Server
	ln     net.Listener
}

// Manager 持有全部代理实例。
//
// 并发模型：一把 RWMutex 保护两张索引表（按 ID、按端口），
// 实例自身的启停都在锁内完成，避免出现「端口已释放但索引还指向它」的中间态。
type Manager struct {
	mu      sync.RWMutex
	byID    map[string]*instance
	byPort  map[int]string
	dial    DialContextFunc
	logger  *slog.Logger
	buffers httputil.BufferPool
}

// Option 是 Manager 的可选配置。
//
// 相比早期版本把一堆可选行为塞进一个越来越长的结构体参数，
// 函数式选项让「默认行为」与「定制行为」在调用点一目了然。
type Option func(*Manager)

// WithDialer 注入自定义拨号器。
func WithDialer(fn DialContextFunc) Option {
	return func(m *Manager) { m.dial = fn }
}

// WithLogger 注入日志器。
func WithLogger(l *slog.Logger) Option {
	return func(m *Manager) {
		if l != nil {
			m.logger = l
		}
	}
}

// NewManager 创建代理管理器。
func NewManager(opts ...Option) *Manager {
	m := &Manager{
		byID:    make(map[string]*instance),
		byPort:  make(map[int]string),
		logger:  slog.Default(),
		buffers: newBufferPool(),
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Start 建立（或按需重建）一条代理路由。
//
// 幂等性：若同 ID 的路由配置完全一致，直接返回成功，
// 这样启动时的「恢复代理」流程可以无条件调用而不会打断正在服务的连接。
func (m *Manager) Start(r Route) error {
	if err := validateRoute(r); err != nil {
		return err
	}

	target, err := normalizeTarget(r.Target)
	if err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if existing, ok := m.byID[r.ID]; ok {
		if existing.route.equal(r) {
			return nil
		}
		m.stopLocked(r.ID)
	}

	if owner, taken := m.byPort[r.Port]; taken && owner != r.ID {
		return fmt.Errorf("端口 %d 已被路由 %q 占用", r.Port, owner)
	}

	ln, err := listenWithRetry(r.Port)
	if err != nil {
		return fmt.Errorf("无法监听本机端口 %d: %w", r.Port, err)
	}

	handler := m.buildHandler(r, target)
	inst := &instance{
		route:  r,
		parsed: target,
		srv:    &http.Server{Handler: handler},
		ln:     ln,
	}

	m.byID[r.ID] = inst
	m.byPort[r.Port] = r.ID

	go func() {
		if serveErr := inst.srv.Serve(ln); serveErr != nil && serveErr != http.ErrServerClosed {
			m.logger.Error("反向代理异常退出", "id", r.ID, "port", r.Port, "error", serveErr)
		}
	}()

	m.logger.Info("反向代理已启动", "id", r.ID, "port", r.Port, "target", r.Target,
		"skipTLS", r.SkipTLSVerify)
	return nil
}

// buildHandler 组装请求处理链：预检 → 真实转发。
func (m *Manager) buildHandler(r Route, target *url.URL) http.Handler {
	rp := &httputil.ReverseProxy{
		BufferPool:     m.buffers,
		Transport:      m.newTransport(r),
		Director:       newDirector(target),
		ModifyResponse: modifyResponse,
		ErrorHandler: func(w http.ResponseWriter, req *http.Request, err error) {
			m.logger.Error("反向代理转发失败", "id", r.ID, "target", r.Target, "error", err)
			writeProxyError(w, r.Target, err)
		},
	}

	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// 浏览器预检请求直接应答，避免后端被 OPTIONS 打扰。
		if isPreflight(req) {
			writePreflight(w, req)
			return
		}
		rp.ServeHTTP(w, req)
	})
}

func (m *Manager) newTransport(r Route) *http.Transport {
	// 拨号器优先级：路由自带 > Manager 默认 > 系统默认。
	dialer := r.Dial
	if dialer == nil {
		dialer = m.dial
	}
	if dialer == nil {
		d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
		dialer = d.DialContext
	}
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		// 后端常见自签证书，由路由显式声明是否放行。
		TLSClientConfig: &tls.Config{InsecureSkipVerify: r.SkipTLSVerify},
	}
}

// Stop 停止指定路由。
func (m *Manager) Stop(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopLocked(id)
}

func (m *Manager) stopLocked(id string) {
	inst, ok := m.byID[id]
	if !ok {
		return
	}
	// 给在途请求 2 秒收尾时间，超时则强制断开。
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = inst.srv.Shutdown(ctx)
	_ = inst.ln.Close()

	delete(m.byPort, inst.route.Port)
	delete(m.byID, id)
	m.logger.Info("反向代理已停止", "id", id, "port", inst.route.Port)
}

// StopAll 停止全部路由（进程退出时调用）。
func (m *Manager) StopAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id := range m.byID {
		m.stopLocked(id)
	}
}

// Running 返回「路由 ID → 监听端口」的快照。
func (m *Manager) Running() map[string]int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]int, len(m.byID))
	for id, inst := range m.byID {
		out[id] = inst.route.Port
	}
	return out
}

// Port 返回指定路由的监听端口；未运行时返回 0。
func (m *Manager) Port(id string) int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if inst, ok := m.byID[id]; ok {
		return inst.route.Port
	}
	return 0
}

// IsRunning 报告指定路由是否正在监听。
func (m *Manager) IsRunning(id string) bool { return m.Port(id) > 0 }

// UsedPorts 返回已被代理占用的端口集合，供端口分配时避让。
func (m *Manager) UsedPorts() map[int]bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[int]bool, len(m.byPort))
	for p := range m.byPort {
		out[p] = true
	}
	return out
}

// --------------------------------------------------------------------------
// 辅助
// --------------------------------------------------------------------------

func validateRoute(r Route) error {
	if strings.TrimSpace(r.ID) == "" {
		return fmt.Errorf("%w: 路由缺少 ID", domain.ErrValidation)
	}
	if r.Port <= 0 || r.Port > 65535 {
		return fmt.Errorf("%w: 监听端口 %d 越界", domain.ErrValidation, r.Port)
	}
	if strings.TrimSpace(r.Target) == "" {
		return fmt.Errorf("%w: 路由缺少目标地址", domain.ErrValidation)
	}
	return nil
}

// normalizeTarget 补全协议并解析目标地址。
func normalizeTarget(raw string) (*url.URL, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, fmt.Errorf("%w: 目标地址为空", domain.ErrValidation)
	}
	if !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return nil, fmt.Errorf("%w: 目标地址无法解析: %v", domain.ErrValidation, err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("%w: 目标地址缺少主机名", domain.ErrValidation)
	}
	return u, nil
}

// listenWithRetry 监听端口，失败后短暂重试一次。
// 用途：刚重启时上一个进程的监听可能还在 TIME_WAIT，稍等即可成功。
func listenWithRetry(port int) (net.Listener, error) {
	addr := fmt.Sprintf(":%d", port)
	ln, err := net.Listen("tcp", addr)
	if err == nil {
		return ln, nil
	}
	time.Sleep(150 * time.Millisecond)
	return net.Listen("tcp", addr)
}

// equal 判断两条路由的关键配置是否一致。
//
// Dial 刻意不参与比较：函数值之间无法比较，而拨号方式完全由 Target 推导
// （同一台目标主机对应的隧道也一样），因此 Target 相同即可认为无需重建。
func (r Route) equal(other Route) bool {
	return r.Port == other.Port &&
		r.Target == other.Target &&
		r.SkipTLSVerify == other.SkipTLSVerify
}

// newBufferPool 是 httputil.BufferPool 的简单实现，避免每次转发都新分配 32KB。
func newBufferPool() httputil.BufferPool { return &bufferPool{} }

type bufferPool struct{ pool sync.Pool }

func (p *bufferPool) Get() []byte {
	if v := p.pool.Get(); v != nil {
		return v.([]byte)
	}
	return make([]byte, 32*1024)
}

func (p *bufferPool) Put(b []byte) { p.pool.Put(b) }
