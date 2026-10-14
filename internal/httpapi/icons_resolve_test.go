package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MisiteQ/qlink2desktop/internal/auth"
	"github.com/MisiteQ/qlink2desktop/internal/domain"
	"github.com/MisiteQ/qlink2desktop/internal/fnos"
	"github.com/MisiteQ/qlink2desktop/internal/store"
	"github.com/MisiteQ/qlink2desktop/internal/webui"
)

// 图标解析预览接口。
//
// 这一档存在的理由是把图标解析的**过程**讲给用户听（用户原话：
// 「这个过程有个什么提示吗或者能让人知道搜索中或者搜索状态」），
// 所以测试盯的两件事是：它收得下前端实际会发的请求体，以及
// 它一定会给出一句说得清的结论。

func newResolveServer(t *testing.T, allowRemote bool, iconsDir string) *Server {
	t.Helper()

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("打开测试存储失败: %v", err)
	}
	return New(Deps{
		Store:    st,
		Service:  fnos.NewService(fnos.Options{CLI: &fnos.FakeCLI{}, IconsDir: iconsDir, AllowRemoteIcon: allowRemote}),
		Sessions: auth.NewManager(0),
		WebFS:    webui.FS(),
		Version:  "test",
		IconsDir: iconsDir,
		LogDir:   t.TempDir(),
	})
}

func postResolve(t *testing.T, h http.Handler, payload string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/icons/resolve", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// 前端发的就是整条链接的表单数据（toPayload 的结果），字段远多于本接口
// 真正用得上的那几个。这条测试守的是一个已经真实发生过的回归：
// 接口一开始用了严格解码，真机验证立刻撞上
// `json: unknown field "desc"` —— 图标那一格静默退回默认图，
// 而报错内容与图标毫无关系，几乎不可能从现象反推回来。
func TestResolveIconAcceptsWholeLinkPayload(t *testing.T) {
	t.Parallel()

	s := newResolveServer(t, false, t.TempDir())
	rec := postResolve(t, s.Handler(), `{
		"name": "百度", "desc": "搜索引擎", "kind": "shortcut", "app_name": "",
		"scheme": "https", "host": "", "port": 0, "path": "https://www.baidu.com",
		"ui": "iframe", "all_users": true, "no_display": false,
		"file_types": ["pdf"], "skip_tls_verify": false,
		"icon": {"source": "", "ref": "", "text": "", "text_color": "", "bg_color": ""},
		"container": {"image": "", "name": "", "service": ""},
		"enabled": true
	}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/icons/resolve = %d，期望 200\n%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON：%v\n%s", err, rec.Body.String())
	}
	for _, key := range []string{"source", "message", "detail", "site", "elapsed_ms", "preview"} {
		if _, ok := body[key]; !ok {
			t.Errorf("响应缺少 %q —— 界面就少一个能说的话（响应=%s）", key, rec.Body.String())
		}
	}
}

// 用户关掉了「允许远端图标」时，必须说明是**开关**关掉了抓取，
// 而不是含糊地说"没找到" —— 后者会让人以为功能坏了。
func TestResolveIconExplainsWhenRemoteDisabled(t *testing.T) {
	t.Parallel()

	s := newResolveServer(t, false, t.TempDir())
	rec := postResolve(t, s.Handler(), `{"kind":"shortcut","path":"https://www.baidu.com","icon":{"source":""}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("= %d，期望 200\n%s", rec.Code, rec.Body.String())
	}

	var body struct {
		Source  string `json:"source"`
		Detail  string `json:"detail"`
		Message string `json:"message"`
		Preview string `json:"preview"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应解析失败：%v", err)
	}
	if body.Source != string(fnos.IconSourceDefault) {
		t.Errorf("source = %q，期望 %q", body.Source, fnos.IconSourceDefault)
	}
	if body.Message == "" {
		t.Error("每个结果都要有一句短标题，前端不该自己维护映射表")
	}
	if !strings.Contains(body.Detail, "允许远端图标") {
		t.Errorf("detail 应当说明是开关关掉了抓取，实际 %q", body.Detail)
	}
	// 即便没找到图标，也要给出一张预览图（就是内置默认那张）——
	// 否则编辑器那一格只能继续画一个写死的图片，用户还是分不清状态。
	if !strings.HasPrefix(body.Preview, "data:image/png;base64,") {
		t.Errorf("preview 应当是可直接显示的数据 URI，实际 %.40q", body.Preview)
	}
}

// 地址不全时也不能崩，要给出说明并回落到默认图标。
func TestResolveIconHandlesIncompleteDraft(t *testing.T) {
	t.Parallel()

	s := newResolveServer(t, true, t.TempDir())
	rec := postResolve(t, s.Handler(), `{"kind":"local","port":0,"path":"/","icon":{"source":""}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("= %d，期望 200\n%s", rec.Code, rec.Body.String())
	}

	var body struct {
		Source string `json:"source"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应解析失败：%v", err)
	}
	if body.Source != string(fnos.IconSourceDefault) || body.Detail == "" {
		t.Errorf("未填完整的草案也要给出结论与说明，实际 source=%q detail=%q", body.Source, body.Detail)
	}
}

// 自动解析出来的图标是程序产物，不该出现在「我的图标」里。
func TestIconLibraryHidesAutoIcons(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s := newResolveServer(t, false, dir)

	// 造两个文件：一个用户上传的、一个程序生成的。
	if err := os.WriteFile(filepath.Join(dir, "uploaded.png"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	autoName := fnos.AutoIconName(domain.Link{ID: "item-abc123"})
	if autoName == "" {
		t.Fatal("自动图标应当有文件名，否则下面的断言无从成立")
	}
	if err := os.WriteFile(filepath.Join(dir, autoName), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	rec := do(t, s.Handler(), http.MethodGet, "/api/icons", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/icons = %d\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "auto-") {
		t.Errorf("自动图标不该混进图标库：%s", body)
	}
}
