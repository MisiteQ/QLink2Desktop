package httpapi

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MisiteQ/qlink2desktop/internal/auth"
	"github.com/MisiteQ/qlink2desktop/internal/domain"
	"github.com/MisiteQ/qlink2desktop/internal/store"
	"github.com/MisiteQ/qlink2desktop/internal/webui"
)

// newRoutingServer 构造一个只用于路由层测试的 Server。
//
// 这组测试的直接动机是曾出现过的一个真实事故：路由表里同时注册了
// `GET /`（SPA）和无方法限定的 `/api/`，Go 1.22+ 的 ServeMux 判定两者
// 无法排出优先级，直接 panic —— 服务一启动就退出，网关 502。
// 此前 httpapi 没有任何测试，这个问题只能在真机上被发现。
func newRoutingServer(t *testing.T) *Server {
	t.Helper()

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("打开测试存储失败: %v", err)
	}

	return New(Deps{
		Store:    st,
		Sessions: auth.NewManager(0),
		WebFS:    webui.FS(),
		Version:  "test",
		// 让上传 / 日志相关的处理器有可用目录，避免误触时空指针。
		IconsDir: t.TempDir(),
		LogDir:   filepath.Join(t.TempDir(), "logs"),
	})
}

func do(t *testing.T, h http.Handler, method, target string, accept string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestRoutesRegisterWithoutPanic 是本文件最基本的一条保障：
// 只要路由表能建立起来，其余行为才谈得上正确。
func TestRoutesRegisterWithoutPanic(t *testing.T) {
	t.Parallel()

	_ = newRoutingServer(t)
}

func TestRoutingBehaviour(t *testing.T) {
	t.Parallel()

	s := newRoutingServer(t)
	h := s.Handler()

	// 1) 根路径返回 SPA 入口页。
	rec := do(t, h, http.MethodGet, "/", "text/html,application/xhtml+xml")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d，期望 200\n%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("GET / 的 Content-Type = %q，期望 text/html", ct)
	}
	if !strings.Contains(rec.Body.String(), "QLink2Desktop") {
		t.Error("SPA 入口页应包含产品名")
	}

	// 2) 静态资源可访问。
	rec = do(t, h, http.MethodGet, "/assets/app.js", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /assets/app.js = %d，期望 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("JS 资源 Content-Type = %q", ct)
	}

	// 3) 健康检查无鉴权可用。
	rec = do(t, h, http.MethodGet, "/api/health", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/health = %d，期望 200\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Errorf("健康检查应返回 ok:true，实际 %s", rec.Body.String())
	}

	// 4) 未设置口令时为开放模式：受保护接口可直接访问。
	rec = do(t, h, http.MethodGet, "/api/links", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("开放模式 GET /api/links = %d，期望 200\n%s", rec.Code, rec.Body.String())
	}

	// 5) 未匹配路径：浏览器导航回落 SPA。
	rec = do(t, h, http.MethodGet, "/some/deep/link", "text/html,application/xhtml+xml")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "QLink2Desktop") {
		t.Errorf("深链接应回落到 SPA，实际 %d / %s", rec.Code, rec.Body.String())
	}

	// 6) 未匹配路径：非浏览器请求得到 JSON 404，方便前端按结构化错误处理。
	rec = do(t, h, http.MethodGet, "/api/definitely/not/a/route", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知接口 = %d，期望 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"kind":"not_found"`) {
		t.Errorf("未知接口应返回 JSON 404，实际 %s", rec.Body.String())
	}

	// 7) 方法不匹配也走 JSON 404，而不是 GET / 返回的页面。
	rec = do(t, h, http.MethodPost, "/", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("POST / = %d，期望 404", rec.Code)
	}
}

// TestProtectedEndpointsRequireSession 验证设置口令后，受保护接口会真正拦截。
//
// 鉴权是「有口令才启用」的：全新安装（还没设口令）时必须开放访问，
// 否则用户连设置口令的页面都进不去。
func TestProtectedEndpointsRequireSession(t *testing.T) {
	t.Parallel()

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("打开测试存储失败: %v", err)
	}
	if err := st.UpdateSettings(func(s *domain.Settings) error {
		s.AuthHash = "not-a-real-hash"
		s.AuthSalt = "salt"
		return nil
	}); err != nil {
		t.Fatalf("写入口令失败: %v", err)
	}

	s := New(Deps{
		Store:    st,
		Sessions: auth.NewManager(0),
		WebFS:    webui.FS(),
		Version:  "test",
		IconsDir: t.TempDir(),
	})
	h := s.Handler()

	// 登录状态查询本身必须无鉴权可用，否则前端无法判断该不该弹登录框。
	if rec := do(t, h, http.MethodGet, "/api/auth/status", ""); rec.Code != http.StatusOK {
		t.Fatalf("GET /api/auth/status = %d，期望 200", rec.Code)
	}

	// 受保护接口：无令牌 401。
	if rec := do(t, h, http.MethodGet, "/api/links", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("已设口令时 GET /api/links = %d，期望 401\n%s", rec.Code, rec.Body.String())
	}

	// 静态资源与图标仍然公开，桌面 <img> 带不了令牌。
	if rec := do(t, h, http.MethodGet, "/icons/default-64.png", ""); rec.Code != http.StatusOK {
		t.Errorf("已设口令时图标仍应公开，实际 %d", rec.Code)
	}
}

// fakeCoordinator 是 Coordinator 的最小实现，只统计受理次数。
//
// 它存在的意义是验证「受理型接口」的契约：响应必须是 202 + 一句人话，
// 而不是等事情做完再返回。少了这一类测试，接口层可以悄悄退化回同步执行，
// 而所有其它测试照样通过 —— 真机上则表现为前端超时报错、后端其实在干活。
type fakeCoordinator struct{ restarts int }

func (f *fakeCoordinator) Views() []domain.View                    { return nil }
func (f *fakeCoordinator) View(string) (domain.View, bool)         { return domain.View{}, false }
func (f *fakeCoordinator) ProxyPort(string) int                    { return 0 }
func (f *fakeCoordinator) QueueSync(string)                        {}
func (f *fakeCoordinator) QueueSetEnabled(string, bool) error      { return nil }
func (f *fakeCoordinator) QueueRemove(string) (domain.Link, error) { return domain.Link{}, nil }
func (f *fakeCoordinator) QueueReconcile()                         {}
func (f *fakeCoordinator) QueueCleanupOrphans()                    {}
func (f *fakeCoordinator) QueueRestartSelf()                       { f.restarts++ }
func (f *fakeCoordinator) Pending() int                            { return 0 }

// TestRestartEndpointIsAccepted 验证重启是「受理即返回」而不是同步等待。
//
// 为什么必须异步：停止命令会终止执行它的那个进程，请求路径**根本等不到**
// 「重启完成」这一刻 —— 等到的只会是自己的连接断开。若改回同步，
// 表现是用户点一次、前端超时报错、然后服务其实重启成功了，
// 用户再点一次。这个测试锁住的就是这条契约。
func TestRestartEndpointIsAccepted(t *testing.T) {
	t.Parallel()

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("打开测试存储失败: %v", err)
	}
	coord := &fakeCoordinator{}
	s := New(Deps{
		Store:       st,
		Sessions:    auth.NewManager(0),
		WebFS:       webui.FS(),
		Version:     "test",
		IconsDir:    t.TempDir(),
		Coordinator: coord,
	})

	rec := do(t, s.Handler(), http.MethodPost, "/api/system/restart", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST /api/system/restart = %d，期望 202\n%s", rec.Code, rec.Body.String())
	}
	if coord.restarts != 1 {
		t.Errorf("重启任务入队 %d 次，期望 1 次", coord.restarts)
	}
	if !strings.Contains(rec.Body.String(), `"accepted":true`) {
		t.Errorf("受理回执里缺少 accepted:true，实际 %s", rec.Body.String())
	}
}

// TestRestartEndpointWithoutCoordinator 验证只读模式下给出明确错误。
//
// 此时**不能**假装受理：返回 202 会让前端进入「等待服务回来」的轮询，
// 而服务其实什么都没做，用户会一直等到两分钟超时。
func TestRestartEndpointWithoutCoordinator(t *testing.T) {
	t.Parallel()

	rec := do(t, newRoutingServer(t).Handler(), http.MethodPost, "/api/system/restart", "")
	if rec.Code == http.StatusAccepted {
		t.Fatalf("没有 Coordinator 时不该受理，实际 %d / %s", rec.Code, rec.Body.String())
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("期望 500（当前运行模式不支持重启），实际 %d", rec.Code)
	}
}

// TestGeneratedIconEndpointIsPublic 验证动态生成的默认图标不需要登录。
// 飞牛桌面的 <img> 标签带不了会话令牌，这条路径若加上鉴权，
// 桌面上所有图标都会变成裂图。
func TestGeneratedIconEndpointIsPublic(t *testing.T) {
	t.Parallel()

	s := newRoutingServer(t)
	rec := do(t, s.Handler(), http.MethodGet, "/icons/default-64.png", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /icons/default-64.png = %d，期望 200\n%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "image/png") {
		t.Errorf("图标 Content-Type = %q，期望 image/png", ct)
	}
	if rec.Body.Len() == 0 {
		t.Error("图标内容不应为空")
	}
}

// TestSPAInjectsAssetVersion 锁定「index.html 返回前必须把资源 URL 上的
// __V__ 占位符换成真实版本号」这条行为。
//
// 动机：styles.css / app.js 的文件名里不含哈希，旧版本若被浏览器强缓存过，
// 升级后会继续用旧 JS 而不回源 —— 表现是"新版本装了却没生效，界面还是老样子"。
// 换了 URL 就等于绕开旧缓存。
func TestSPAInjectsAssetVersion(t *testing.T) {
	t.Parallel()

	rec := do(t, newRoutingServer(t).Handler(), http.MethodGet, "/", "text/html")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d，期望 200", rec.Code)
	}

	body := rec.Body.String()
	if strings.Contains(body, assetVersionToken) {
		t.Errorf("响应里仍残留占位符 %q，资源缓存无法失效", assetVersionToken)
	}
	// newRoutingServer 注入的 Version 是 "test"。
	for _, want := range []string{"assets/style.css?v=test", "assets/app.js?v=test"} {
		if !strings.Contains(body, want) {
			t.Errorf("文档里找不到 %q，升级后可能命中旧缓存", want)
		}
	}
}

// TestSPAContainsTrailingSlashGuard 锁定尾斜杠守卫脚本的存在与位置。
//
// 这是真机上排查最久的一类白屏：网关吃掉 ui/config 里 url 字段的尾斜杠后，
// iframe 的文档 URL 变成 /app/<name>（无斜杠），浏览器把相对路径基准算成
// /app/，于是 assets/app.js 被解析成 /app/assets/app.js 而全部 404。
// 守卫脚本必须出现在任何相对资源之前 —— 晚了资源请求已经发出去了。
func TestSPAContainsTrailingSlashGuard(t *testing.T) {
	t.Parallel()

	body := do(t, newRoutingServer(t).Handler(), http.MethodGet, "/", "text/html").Body.String()

	guard := strings.Index(body, "location.replace")
	if guard < 0 {
		t.Fatal("index.html 里找不到尾斜杠守卫脚本，真机会白屏")
	}
	// 注意必须匹配「带属性的真实引用」，不能只搜文件名：
	// 守卫脚本自己的注释里就提到了 assets/app.js，朴素查找会命中注释而误判。
	for _, asset := range []string{`href="assets/style.css`, `src="assets/app.js`} {
		idx := strings.Index(body, asset)
		if idx < 0 {
			t.Errorf("文档里找不到资源引用 %s", asset)
			continue
		}
		if idx < guard {
			t.Errorf("守卫脚本必须位于 %s 之前：guard=%d asset=%d", asset, guard, idx)
		}
	}
}
