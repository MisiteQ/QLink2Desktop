package proxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// recorder 是一个可记录收到的请求、并允许自定义响应的测试后端。
type recorder struct {
	mu       sync.Mutex
	last     *http.Request
	lastBody string
	handler  http.HandlerFunc
}

func (rc *recorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	rc.mu.Lock()
	rc.last = r.Clone(r.Context())
	rc.lastBody = string(body)
	h := rc.handler
	rc.mu.Unlock()

	if h != nil {
		h(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte("backend-ok"))
}

func (rc *recorder) request() *http.Request {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.last
}

func newBackend(t *testing.T) (*recorder, *httptest.Server) {
	t.Helper()
	rc := &recorder{}
	srv := httptest.NewServer(rc)
	t.Cleanup(srv.Close)
	return rc, srv
}

func startRoute(t *testing.T, m *Manager, r Route) int {
	t.Helper()
	if err := m.Start(r); err != nil {
		t.Fatalf("启动代理失败: %v", err)
	}
	waitListening(t, r.Port)
	return r.Port
}

// waitListening 等待代理端口真正可连接，消除测试里的时序抖动。
func waitListening(t *testing.T, port int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addrOf(port), 100*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("代理端口 %d 未在预期时间内开始监听", port)
}

func addrOf(port int) string { return net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) }

func urlOf(port int, path string) string { return fmt.Sprintf("http://%s%s", addrOf(port), path) }

// 进程内的端口预留表。
//
// 这些用例都是 t.Parallel() 的，而「探测端口空闲 → 真正去监听」之间必然存在窗口：
// 两个并行用例可能探测到同一个端口，随后其中一个启动失败。
// 因此除了探测系统可用性，还要在本进程内做一次互斥预留。
var (
	portMu    sync.Mutex
	portTaken = map[int]bool{}
)

func freePort(t *testing.T) int {
	t.Helper()
	portMu.Lock()
	defer portMu.Unlock()

	for p := DefaultProxyPortBase; p < maxPort; p++ {
		if portTaken[p] || !PortAvailable(p) {
			continue
		}
		portTaken[p] = true
		t.Cleanup(func() {
			portMu.Lock()
			delete(portTaken, p)
			portMu.Unlock()
		})
		return p
	}
	t.Fatal("无法分配空闲端口")
	return 0
}

// -------------------------------------------------------------------------- 基础转发

func TestStartForwardsToBackend(t *testing.T) {
	t.Parallel()

	rc, backend := newBackend(t)
	m := NewManager()
	defer m.StopAll()

	port := startRoute(t, m, Route{ID: "a", Port: freePort(t), Target: backend.URL})

	resp, err := http.Get(urlOf(port, "/hello"))
	if err != nil {
		t.Fatalf("请求代理失败: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if string(body) != "backend-ok" {
		t.Errorf("响应体不正确: %q", body)
	}
	if got := rc.request(); got == nil || got.URL.Path != "/hello" {
		t.Errorf("后端收到的路径不正确: %v", got)
	}
}

func TestStartIsIdempotentForSameConfig(t *testing.T) {
	t.Parallel()

	_, backend := newBackend(t)
	m := NewManager()
	defer m.StopAll()

	port := freePort(t)
	route := Route{ID: "a", Port: port, Target: backend.URL}

	if err := m.Start(route); err != nil {
		t.Fatal(err)
	}
	waitListening(t, port)

	// 配置完全一致时重复启动不应报错，也不应换端口。
	if err := m.Start(route); err != nil {
		t.Fatalf("重复启动相同配置应无副作用: %v", err)
	}
	if got := m.Port("a"); got != port {
		t.Errorf("端口不应变化: %d -> %d", port, got)
	}
}

func TestStartRebuildsOnConfigChange(t *testing.T) {
	t.Parallel()

	_, backendA := newBackend(t)
	rcB, backendB := newBackend(t)

	m := NewManager()
	defer m.StopAll()

	port := freePort(t)
	startRoute(t, m, Route{ID: "a", Port: port, Target: backendA.URL})

	// 换目标地址后应重建，请求要落到新后端。
	if err := m.Start(Route{ID: "a", Port: port, Target: backendB.URL}); err != nil {
		t.Fatalf("重建失败: %v", err)
	}
	waitListening(t, port)

	resp, err := http.Get(urlOf(port, "/"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)
	if rcB.request() == nil {
		t.Error("请求应落到新的后端")
	}
}

func TestStartRejectsPortConflict(t *testing.T) {
	t.Parallel()

	_, backend := newBackend(t)
	m := NewManager()
	defer m.StopAll()

	// 刻意让两条路由争抢同一个端口。
	port := freePort(t)
	startRoute(t, m, Route{ID: "a", Port: port, Target: backend.URL})

	err := m.Start(Route{ID: "b", Port: port, Target: backend.URL})
	if err == nil {
		t.Fatal("端口冲突应报错")
	}
	if !strings.Contains(err.Error(), "已被路由") {
		t.Errorf("错误信息应说明冲突: %v", err)
	}
}

func TestStartValidatesRoute(t *testing.T) {
	t.Parallel()

	_, backend := newBackend(t)
	m := NewManager()
	defer m.StopAll()

	cases := []struct {
		name  string
		route Route
	}{
		{"缺 ID", Route{Port: 18001, Target: backend.URL}},
		{"端口越界", Route{ID: "a", Port: 70000, Target: backend.URL}},
		{"缺目标", Route{ID: "a", Port: 18001}},
		{"目标非法", Route{ID: "a", Port: 18001, Target: "http://"}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := m.Start(tc.route); err == nil {
				t.Fatal("非法路由应被拒绝")
			}
		})
	}
}

func TestStartNormalizesTargetWithoutScheme(t *testing.T) {
	t.Parallel()

	_, backend := newBackend(t)
	m := NewManager()
	defer m.StopAll()

	port := freePort(t)
	startRoute(t, m, Route{ID: "a", Port: port, Target: strings.TrimPrefix(backend.URL, "http://")})

	resp, err := http.Get(urlOf(port, "/"))
	if err != nil {
		t.Fatalf("应自动补全 http:// 协议: %v", err)
	}
	resp.Body.Close()
}

// -------------------------------------------------------------------------- 请求头改写

func TestDirectorRewritesHeaders(t *testing.T) {
	t.Parallel()

	rc, backend := newBackend(t)
	m := NewManager()
	defer m.StopAll()

	port := startRoute(t, m, Route{ID: "a", Port: freePort(t), Target: backend.URL})

	req, _ := http.NewRequest(http.MethodGet, "http://"+addrOf(port)+"/api?x=1", nil)
	req.Header.Set("Origin", "http://nas.local:18000")
	req.Header.Set("Referer", "http://nas.local:18000/page")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	got := rc.request()
	if got == nil {
		t.Fatal("后端未收到请求")
	}
	if got.Host != strings.TrimPrefix(backend.URL, "http://") {
		t.Errorf("Host 未改写为后端主机: %q", got.Host)
	}
	if o := got.Header.Get("Origin"); o != backend.URL {
		t.Errorf("Origin 应改写为后端 origin，实际 %q", o)
	}
	if orig := got.Header.Get("X-Original-Origin"); orig != "http://nas.local:18000" {
		t.Errorf("应保留原始 Origin，实际 %q", orig)
	}
	if ref := got.Header.Get("Referer"); !strings.HasPrefix(ref, backend.URL) {
		t.Errorf("Referer 未改写: %q", ref)
	}
	// X-Forwarded-Host 记录的是客户端实际访问的 Host（即代理自身的地址），
	// 而不是请求里手写的头 —— 否则转发链上的下游会拿到伪造的值。
	if got.Header.Get("X-Forwarded-Host") != addrOf(port) {
		t.Errorf("X-Forwarded-Host 应为代理自身地址 %q，实际 %q", addrOf(port), got.Header.Get("X-Forwarded-Host"))
	}
	if got.Header.Get("X-Forwarded-For") == "" {
		t.Error("应设置 X-Forwarded-For")
	}
}

func TestDirectorDeduplicatesTargetPathPrefix(t *testing.T) {
	t.Parallel()

	rc, backend := newBackend(t)
	m := NewManager()
	defer m.StopAll()

	port := freePort(t)
	// 目标自带 /gitea 前缀。
	startRoute(t, m, Route{ID: "a", Port: port, Target: backend.URL + "/gitea"})

	// 请求路径也已带前缀 → 不应拼成 /gitea/gitea/...
	resp, err := http.Get(urlOf(port, "/gitea/repo"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := rc.request(); got.URL.Path != "/gitea/repo" {
		t.Errorf("路径前缀被重复拼接: %q", got.URL.Path)
	}

	// 请求路径未带前缀 → 应补上。
	resp2, err := http.Get(urlOf(port, "/repo"))
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if got := rc.request(); got.URL.Path != "/gitea/repo" {
		t.Errorf("应补上目标前缀，实际 %q", got.URL.Path)
	}
}

// -------------------------------------------------------------------------- 响应头改写

func TestModifyResponseRewritesLocation(t *testing.T) {
	t.Parallel()

	rc, backend := newBackend(t)
	backendHost := strings.TrimPrefix(backend.URL, "http://")
	rc.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "http://"+backendHost+"/login?next=1")
		w.WriteHeader(http.StatusFound)
	}

	m := NewManager()
	defer m.StopAll()
	port := startRoute(t, m, Route{ID: "a", Port: freePort(t), Target: backend.URL})

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(urlOf(port, "/"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	loc := resp.Header.Get("Location")
	if loc != "/login?next=1" {
		t.Errorf("Location 应改写为相对路径，实际 %q", loc)
	}
}

func TestModifyResponseRemovesFrameBlockers(t *testing.T) {
	t.Parallel()

	rc, backend := newBackend(t)
	rc.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'; img-src *")
		w.Write([]byte("ok"))
	}

	m := NewManager()
	defer m.StopAll()
	port := startRoute(t, m, Route{ID: "a", Port: freePort(t), Target: backend.URL})

	resp, err := http.Get(urlOf(port, "/"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if v := resp.Header.Get("X-Frame-Options"); v != "" {
		t.Errorf("X-Frame-Options 应被删除，实际 %q", v)
	}
	csp := resp.Header.Get("Content-Security-Policy")
	if strings.Contains(csp, "frame-ancestors") {
		t.Errorf("frame-ancestors 应被移除，实际 %q", csp)
	}
	if !strings.Contains(csp, "default-src") {
		t.Errorf("其它 CSP 指令应被保留，实际 %q", csp)
	}
}

func TestModifyResponseStripsCookieDomain(t *testing.T) {
	t.Parallel()

	rc, backend := newBackend(t)
	rc.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Set-Cookie", "sid=xyz; Domain=192.168.1.10; Path=/; HttpOnly")
		w.Header().Add("Set-Cookie", "plain=1; Path=/")
		w.Write([]byte("ok"))
	}

	m := NewManager()
	defer m.StopAll()
	port := startRoute(t, m, Route{ID: "a", Port: freePort(t), Target: backend.URL})

	resp, err := http.Get(urlOf(port, "/"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	cookies := resp.Header["Set-Cookie"]
	if len(cookies) != 2 {
		t.Fatalf("应保留两个 Set-Cookie，实际 %d: %v", len(cookies), cookies)
	}
	for _, c := range cookies {
		if strings.Contains(strings.ToLower(c), "domain=") {
			t.Errorf("Domain 属性应被移除: %q", c)
		}
	}
	if !strings.Contains(cookies[0], "sid=xyz") || !strings.Contains(cookies[0], "HttpOnly") {
		t.Errorf("其它属性应保留: %q", cookies[0])
	}
}

func TestCORSHeadersEchoedForCrossOrigin(t *testing.T) {
	t.Parallel()

	_, backend := newBackend(t)
	m := NewManager()
	defer m.StopAll()
	port := startRoute(t, m, Route{ID: "a", Port: freePort(t), Target: backend.URL})

	req, _ := http.NewRequest(http.MethodGet, "http://"+addrOf(port)+"/", nil)
	req.Header.Set("Origin", "http://nas.local:18000")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "http://nas.local:18000" {
		t.Errorf("应回显请求来源，实际 %q", got)
	}
	if resp.Header.Get("Access-Control-Allow-Credentials") != "true" {
		t.Error("跨域带凭据场景需要 Allow-Credentials")
	}
}

func TestPreflightShortCircuits(t *testing.T) {
	t.Parallel()

	rc, backend := newBackend(t)
	m := NewManager()
	defer m.StopAll()
	port := startRoute(t, m, Route{ID: "a", Port: freePort(t), Target: backend.URL})

	req, _ := http.NewRequest(http.MethodOptions, "http://"+addrOf(port)+"/api", nil)
	req.Header.Set("Origin", "http://nas.local:18000")
	req.Header.Set("Access-Control-Request-Method", "POST")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("预检应返回 204，实际 %d", resp.StatusCode)
	}
	if rc.request() != nil {
		t.Error("预检请求不应转发到后端")
	}
}

// -------------------------------------------------------------------------- 生命周期

func TestRunningAndUsedPorts(t *testing.T) {
	t.Parallel()

	_, backend := newBackend(t)
	m := NewManager()
	defer m.StopAll()

	p1, p2 := freePort(t), 0
	for {
		p2 = freePort(t)
		if p2 != p1 {
			break
		}
	}
	startRoute(t, m, Route{ID: "a", Port: p1, Target: backend.URL})
	startRoute(t, m, Route{ID: "b", Port: p2, Target: backend.URL})

	running := m.Running()
	if len(running) != 2 || running["a"] != p1 || running["b"] != p2 {
		t.Fatalf("Running() 不正确: %v", running)
	}
	if !m.IsRunning("a") || m.Port("b") != p2 {
		t.Error("IsRunning / Port 不正确")
	}
	if used := m.UsedPorts(); !used[p1] || !used[p2] {
		t.Errorf("UsedPorts() 应包含两个端口: %v", used)
	}

	m.Stop("a")
	if m.IsRunning("a") {
		t.Error("停止后不应仍在运行")
	}
	if m.Port("a") != 0 {
		t.Error("停止后端口应返回 0")
	}
	if used := m.UsedPorts(); used[p1] {
		t.Error("停止后端口应被释放")
	}
}

func TestStopUnknownIDIsNoop(t *testing.T) {
	t.Parallel()

	m := NewManager()
	m.Stop("never-existed") // 不应 panic
}

func TestStopAll(t *testing.T) {
	t.Parallel()

	_, backend := newBackend(t)
	m := NewManager()

	ports := map[int]bool{}
	for i := 0; i < 3; i++ {
		p := freePort(t)
		for ports[p] {
			p = freePort(t)
		}
		ports[p] = true
		startRoute(t, m, Route{ID: "r" + strconv.Itoa(i), Port: p, Target: backend.URL})
	}

	m.StopAll()
	if len(m.Running()) != 0 {
		t.Errorf("StopAll 后不应残留实例: %v", m.Running())
	}
	for p := range ports {
		if !PortAvailable(p) {
			t.Errorf("端口 %d 未被释放", p)
		}
	}
}

func TestErrorPageOnUnreachableBackend(t *testing.T) {
	t.Parallel()

	m := NewManager()
	defer m.StopAll()

	port := freePort(t)
	// 指向一个确定连不上的地址。
	startRoute(t, m, Route{ID: "a", Port: port, Target: "http://127.0.0.1:1"})

	resp, err := http.Get(urlOf(port, "/"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("应返回 502，实际 %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), "无法连接到目标服务") {
		t.Errorf("应输出可读的错误页，实际: %.200s", body)
	}
}

func TestWithDialerOption(t *testing.T) {
	t.Parallel()

	_, backend := newBackend(t)
	var called bool
	var mu sync.Mutex

	m := NewManager(WithDialer(func(ctx context.Context, network, addr string) (net.Conn, error) {
		mu.Lock()
		called = true
		mu.Unlock()
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}))
	defer m.StopAll()

	port := startRoute(t, m, Route{ID: "a", Port: freePort(t), Target: backend.URL})
	resp, err := http.Get(urlOf(port, "/"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	mu.Lock()
	defer mu.Unlock()
	if !called {
		t.Error("自定义拨号器应被调用")
	}
}

func TestRouteEqual(t *testing.T) {
	t.Parallel()

	base := Route{ID: "a", Port: 1, Target: "http://x"}
	if !base.equal(base) {
		t.Error("相同路由应判定相等")
	}
	changed := base
	changed.Port = 2
	if base.equal(changed) {
		t.Error("端口不同应判定不等")
	}
}
