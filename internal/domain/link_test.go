package domain

import (
	"errors"
	"testing"
)

func TestLinkValidate(t *testing.T) {
	t.Parallel()

	valid := func(l Link) Link { l.Normalize(); return l }

	cases := []struct {
		name    string
		link    Link
		wantErr bool
	}{
		{
			name: "本机端口-最小合法",
			link: valid(Link{Name: "A", Kind: KindLocalPort, Port: 8080}),
		},
		{
			name:    "缺少名称",
			link:    valid(Link{Kind: KindLocalPort, Port: 8080}),
			wantErr: true,
		},
		{
			name:    "端口越界",
			link:    valid(Link{Name: "A", Kind: KindLocalPort, Port: 70000}),
			wantErr: true,
		},
		{
			name:    "端口映射缺主机",
			link:    valid(Link{Name: "A", Kind: KindProxy, Port: 80}),
			wantErr: true,
		},
		{
			name: "端口映射合法",
			link: valid(Link{Name: "A", Kind: KindProxy, Port: 80, Host: "10.0.0.9"}),
		},
		{
			name: "快捷方式合法",
			link: valid(Link{Name: "A", Kind: KindShortcut, Path: "https://example.com/x"}),
		},
		{
			name:    "快捷方式空网址",
			link:    valid(Link{Name: "A", Kind: KindShortcut}),
			wantErr: true,
		},
		{
			name:    "文字图标缺文字",
			link:    valid(Link{Name: "A", Kind: KindLocalPort, Port: 1, Icon: Icon{Source: IconText}}),
			wantErr: true,
		},
		{
			name:    "未知形态",
			link:    Link{Name: "A", Kind: Kind("weird")},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.link.Validate()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("期望校验失败，实际通过")
				}
				if !errors.Is(err, ErrValidation) {
					t.Fatalf("期望 ErrValidation，实际: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("期望校验通过，实际失败: %v", err)
			}
		})
	}
}

func TestLinkNormalizeForcesLoopbackForLocalPort(t *testing.T) {
	t.Parallel()

	l := Link{Name: "A", Kind: KindLocalPort, Port: 8080, Host: "10.0.0.5"}
	l.Normalize()
	if l.Host != "localhost" {
		t.Fatalf("本机端口形态的主机应被强制为 localhost，实际 %q", l.Host)
	}
}

func TestLinkNormalizeDefaults(t *testing.T) {
	t.Parallel()

	l := Link{Name: "  A  ", Kind: KindProxy, Host: " 1.2.3.4 ", Port: 80, Path: "admin"}
	l.Normalize()

	if l.Name != "A" {
		t.Errorf("名称未 trim: %q", l.Name)
	}
	if l.Host != "1.2.3.4" {
		t.Errorf("主机未 trim: %q", l.Host)
	}
	if l.Scheme != SchemeHTTP {
		t.Errorf("协议默认值应为 http，实际 %q", l.Scheme)
	}
	if l.UI != UIWindow {
		t.Errorf("窗口形态默认值应为 iframe，实际 %q", l.UI)
	}
	if l.Path != "/admin" {
		t.Errorf("路径应补前导斜杠，实际 %q", l.Path)
	}
}

func TestBackendURL(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		link Link
		want string
	}{
		{
			name: "本机端口总是走回环",
			link: Link{Kind: KindLocalPort, Scheme: "http", Port: 5244, Path: "/", Host: "随意"},
			want: "http://127.0.0.1:5244/",
		},
		{
			name: "端口映射走远端主机",
			link: Link{Kind: KindProxy, Scheme: "https", Host: "nas.local", Port: 443, Path: "/ui"},
			want: "https://nas.local:443/ui",
		},
		{
			name: "无端口时省略冒号",
			link: Link{Kind: KindProxy, Scheme: "http", Host: "example.com", Path: "/"},
			want: "http://example.com/",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.link.BackendURL(); got != tc.want {
				t.Fatalf("BackendURL() = %q, 期望 %q", got, tc.want)
			}
		})
	}
}

func TestDesktopURLPointsAtLoopback(t *testing.T) {
	t.Parallel()

	// 端口映射形态：Port 已被代理端口覆盖，桌面图标必须指向本机代理而不是远端。
	l := Link{Kind: KindProxy, Scheme: "http", Host: "10.0.0.9", Port: 18001, Path: "/"}
	if got := l.DesktopURL(); got != "http://localhost:18001/" {
		t.Fatalf("DesktopURL() = %q, 期望 http://localhost:18001/", got)
	}
}

func TestShortcutURL(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"example.com":           "https://example.com",
		"https://example.com/a": "https://example.com/a",
		"http://example.com/a":  "http://example.com/a",
		"":                      "",
	}
	for input, want := range cases {
		l := Link{Kind: KindShortcut, Path: input}
		if got := l.ShortcutURL(); got != want {
			t.Errorf("ShortcutURL(%q) = %q, 期望 %q", input, got, want)
		}
	}
}

func TestKindHelpers(t *testing.T) {
	t.Parallel()

	if !KindProxy.NeedsProxy() || KindLocalPort.NeedsProxy() {
		t.Error("NeedsProxy 判定错误")
	}
	if !KindLocalPort.NeedsHostPort() || KindShortcut.NeedsHostPort() {
		t.Error("NeedsHostPort 判定错误")
	}
	if _, err := ParseKind("bogus"); err == nil {
		t.Error("非法形态应报错")
	}
	if k, err := ParseKind("proxy"); err != nil || k != KindProxy {
		t.Errorf("ParseKind(proxy) 失败: %v %v", k, err)
	}
}
