package domain

import (
	"strings"
	"testing"
)

func TestValidateAppName(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"合法基础名", "qlink2d.alist-a1b2c3", false},
		{"允许短横线与下划线", "qlink2d.my_app-x1", false},
		{"过短", "ab", true},
		{"过长", strings.Repeat("a", MaxAppNameLength+1), true},
		{"数字开头合法", "1abc", false},
		{"非法首字符", "-abc", true},
		{"含中文", "qlink2d.中文", true},
		{"含空格", "qlink2d. my app", true},
		{"含斜杠", "qlink2d.a/b", true},
		{"边界长度刚好", strings.Repeat("a", MaxAppNameLength), false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateAppName(tc.input)
			if tc.wantErr && err == nil {
				t.Fatalf("期望校验失败，实际通过: %q", tc.input)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("期望校验通过，实际失败: %q -> %v", tc.input, err)
			}
		})
	}
}

func TestDeriveAppNameAlwaysValid(t *testing.T) {
	t.Parallel()

	links := []Link{
		{ID: "item-528153", Name: "Alist 网盘", Kind: KindLocalPort, Port: 5244, Container: Container{Name: "alist"}},
		{ID: "item-1", Name: "一个非常非常长的中文名称用来测试裁剪行为是否正确", Kind: KindProxy, Port: 8080, Host: "10.0.0.2"},
		{ID: "item-2", Name: "qBittorrent", Kind: KindLocalPort, Port: 8081, Container: Container{Name: "linuxserver-qbittorrent-latest"}},
		{ID: "item-3", Kind: KindLocalPort, Port: 9000},
		{ID: "", Name: "", Kind: KindLocalPort},
		{ID: "item-4", Name: "!!!", Kind: KindShortcut},
	}

	for _, l := range links {
		got := DeriveAppName(l)
		if err := ValidateAppName(got); err != nil {
			t.Fatalf("派生的包名不合法: Link=%+v -> %q: %v", l, got, err)
		}
		if !strings.HasPrefix(got, AppPrefix) {
			t.Fatalf("派生包名缺少专属前缀: %q", got)
		}
		if len(got) > MaxAppNameLength {
			t.Fatalf("派生包名超长(%d): %q", len(got), got)
		}
	}
}

func TestDeriveAppNameIsStable(t *testing.T) {
	t.Parallel()

	l := Link{ID: "item-abc123", Name: "Gitea", Kind: KindLocalPort, Port: 3000, Container: Container{Name: "gitea"}}
	first := DeriveAppName(l)
	for i := 0; i < 20; i++ {
		if got := DeriveAppName(l); got != first {
			t.Fatalf("派生结果不稳定: %q != %q", got, first)
		}
	}
}

func TestDeriveAppNameDistinguishesSameContainer(t *testing.T) {
	t.Parallel()

	// 同一个容器名、不同 ID，必须得到不同的包标识，否则后者会顶掉前者。
	a := Link{ID: "item-aaaaaa", Name: "Web", Kind: KindLocalPort, Port: 80, Container: Container{Name: "nginx"}}
	b := Link{ID: "item-bbbbbb", Name: "Web2", Kind: KindLocalPort, Port: 81, Container: Container{Name: "nginx"}}
	if DeriveAppName(a) == DeriveAppName(b) {
		t.Fatalf("不同链接派生出相同包名: %q", DeriveAppName(a))
	}
}

func TestDeriveAppNamePrefersContainerName(t *testing.T) {
	t.Parallel()

	l := Link{ID: "item-x1y2z3", Name: "我的网盘", Kind: KindLocalPort, Port: 5244, Container: Container{Name: "alist"}}
	if got := DeriveAppName(l); !strings.Contains(got, "alist") {
		t.Fatalf("应优先使用容器名作为主体片段，实际: %q", got)
	}
}

func TestDeriveAppNameFallsBackToPort(t *testing.T) {
	t.Parallel()

	l := Link{ID: "item-x1y2z3", Name: "中文名称", Kind: KindLocalPort, Port: 8123}
	if got := DeriveAppName(l); !strings.Contains(got, "port-8123") {
		t.Fatalf("中文名且无容器时应回退到端口片段，实际: %q", got)
	}
}

func TestResolveAppNameAvoidsCollision(t *testing.T) {
	t.Parallel()

	l := Link{ID: "item-aaa111", Name: "alist", Kind: KindLocalPort, Port: 5244, Container: Container{Name: "alist"}}
	base := DeriveAppName(l)

	taken := map[string]bool{base: true}
	got := ResolveAppName(l, taken)
	if got == base {
		t.Fatalf("发生冲突时应返回新名字，实际仍为 %q", got)
	}
	if err := ValidateAppName(got); err != nil {
		t.Fatalf("冲突消解后的包名不合法: %q: %v", got, err)
	}

	taken[got] = true
	third := ResolveAppName(l, taken)
	if third == got || third == base {
		t.Fatalf("第二次冲突消解未生效: %q", third)
	}
}

func TestResolveAppNameHonoursExplicitName(t *testing.T) {
	t.Parallel()

	l := Link{ID: "item-aaa111", Name: "x", Kind: KindLocalPort, Port: 1, AppName: "qlink2d.custom-zz"}
	if got := ResolveAppName(l, map[string]bool{}); got != "qlink2d.custom-zz" {
		t.Fatalf("应保留用户显式指定的包名，实际: %q", got)
	}
}

func TestIsManagedApp(t *testing.T) {
	t.Parallel()

	cases := map[string]bool{
		"qlink2d.alist-a1b2": true,
		// 其它工具的命名空间：本应用是独立应用，
		// 绝不能识别或清理它们创建的桌面图标。
		"othertool.legacy-1":   false,
		"anothertool.legacy-1": false,
		"watchcow.something":   false,
		"trim.photos":          false,
		"":                     false,
	}
	for input, want := range cases {
		if got := IsManagedApp(input); got != want {
			t.Errorf("IsManagedApp(%q) = %v, 期望 %v", input, got, want)
		}
	}
}

func TestSlugStripsNonIdentifierRunes(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"Hello World":    "hello-world",
		"a/b/c":          "a-b-c",
		"__leading__":    "leading",
		"中文":             "",
		"MiXeD-123":      "mixed-123",
		"多个   空格":        "",
		"port_8080":      "port-8080",
		"trailing---":    "trailing",
		"-leading-dash-": "leading-dash",
	}
	for input, want := range cases {
		if got := Slug(input); got != want {
			t.Errorf("Slug(%q) = %q, 期望 %q", input, got, want)
		}
	}
}
