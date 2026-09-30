package fnos

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestParseAppList(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		output string
		want   []string
	}{
		{
			name: "标准表格（Unicode 边框）",
			output: "┌─────────┬────────┐\n" +
				"│ NAME    │ STATUS │\n" +
				"├─────────┼────────┤\n" +
				"│ qlink2d.a-1 │ running │\n" +
				"│ trim.photos │ running │\n" +
				"└─────────┴────────┘\n",
			want: []string{"NAME", "qlink2d.a-1", "trim.photos"},
		},
		{
			name: "ASCII 边框",
			output: "+---------+--------+\n" +
				"| qlink2d.b-2 | stopped |\n" +
				"+---------+--------+\n",
			want: []string{"qlink2d.b-2"},
		},
		{
			name:   "制表符分隔",
			output: "qlink2d.c-3\trunning\nqlink2d.d-4\tstopped\n",
			want:   []string{"qlink2d.c-3", "qlink2d.d-4"},
		},
		{
			name:   "多空格分隔",
			output: "qlink2d.e-5    running\n",
			want:   []string{"qlink2d.e-5"},
		},
		{
			name:   "裸列表",
			output: "alpha\nbeta\n",
			want:   []string{"alpha", "beta"},
		},
		{
			name:   "空输出",
			output: "",
			want:   nil,
		},
		{
			name:   "重复项去重",
			output: "qlink2d.x\na\nqlink2d.x\n",
			want:   []string{"qlink2d.x", "a"},
		},
		{
			name:   "含 Windows 换行",
			output: "qlink2d.a\r\nqlink2d.b\r\n",
			want:   []string{"qlink2d.a", "qlink2d.b"},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ParseAppList(tc.output)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ParseAppList() = %#v, 期望 %#v", got, tc.want)
			}
		})
	}
}

func TestParseAppState(t *testing.T) {
	t.Parallel()

	cases := map[string]AppState{
		"running":                   StateRunning,
		"app is running (pid 1234)": StateRunning,
		"当前状态：运行中":                  StateRunning,
		"starting":                  StateStarting,
		"正在启动中…":                    StateStarting,
		"stopped":                   StateStopped,
		"not running":               StateStopped,
		"已停止":                       StateStopped,
		"stopping":                  StateStopping,
		"某段无法识别的文字":                 StateUnknown,
		"":                          StateUnknown,
	}
	for input, want := range cases {
		if got := ParseAppState(input); got != want {
			t.Errorf("ParseAppState(%q) = %v, 期望 %v", input, got, want)
		}
	}
}

func TestParseDefaultVolume(t *testing.T) {
	t.Parallel()

	ok := map[string]int{
		"1":                 1,
		"2\n":               2,
		"default volume: 3": 3,
		"卷 4":               4,
	}
	for input, want := range ok {
		got, err := ParseDefaultVolume(input)
		if err != nil {
			t.Errorf("ParseDefaultVolume(%q) 意外报错: %v", input, err)
			continue
		}
		if got != want {
			t.Errorf("ParseDefaultVolume(%q) = %d, 期望 %d", input, got, want)
		}
	}

	for _, bad := range []string{"", "abc", "volume: zero", "-1"} {
		if _, err := ParseDefaultVolume(bad); err == nil {
			t.Errorf("ParseDefaultVolume(%q) 应当报错", bad)
		}
	}
}

func TestExtractManagedApps(t *testing.T) {
	t.Parallel()

	input := []string{
		"qlink2d.alist-a1",
		"trim.photos",
		"othertool.legacy",
		"anothertool.old",
		"qlink2d.alist-a1", // 重复
		"system.app",
	}
	got := ExtractManagedApps(input)
	// 其它工具的前缀（othertool. / anothertool.）不属于本项目，绝不能被识别。
	want := []string{"qlink2d.alist-a1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ExtractManagedApps() = %#v, 期望 %#v", got, want)
	}
}

func TestCleanCLIOutput(t *testing.T) {
	t.Parallel()

	raw := "Installing...\r\n- \r\n| \r\nVerifying files...\n安装失败: 磁盘空间不足\n/home\n"
	got := CleanCLIOutput(raw)
	if !strings.Contains(got, "磁盘空间不足") {
		t.Fatalf("应保留错误正文，实际: %q", got)
	}
	if strings.Contains(got, "Installing") || strings.Contains(got, "Verifying") {
		t.Fatalf("应过滤进度行，实际: %q", got)
	}
}

func TestExecCLIUnavailable(t *testing.T) {
	t.Parallel()

	var c ExecCLI
	if c.Available() {
		t.Error("空 ExecCLI 应报告不可用")
	}
	if _, err := c.List(); !errors.Is(err, ErrCLIUnavailable) {
		t.Fatalf("不可用时应返回 ErrCLIUnavailable，实际: %v", err)
	}
	if _, err := c.DefaultVolume(); !errors.Is(err, ErrCLIUnavailable) {
		t.Fatalf("不可用时应返回 ErrCLIUnavailable，实际: %v", err)
	}
}

func TestExecCLIUninstallRefusesForeignApp(t *testing.T) {
	t.Parallel()

	c := &ExecCLI{path: "/bin/true"}
	err := c.Uninstall("trim.photos")
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("必须拒绝卸载非本项目应用，实际: %v", err)
	}
}

func TestExecCLIRejectsInvalidAppName(t *testing.T) {
	t.Parallel()

	c := &ExecCLI{path: "/bin/true"}
	if err := c.Start("-bad"); err == nil {
		t.Error("非法包名应被拒绝")
	}
	if _, err := c.Status("a b"); err == nil {
		t.Error("非法包名应被拒绝")
	}
}
