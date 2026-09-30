package fnos

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// FakeCLI 是 CLI 的内存实现，服务于两个场景：
//
//  1. 单元测试：让安装 / 卸载 / 对账的每一条分支都能在毫秒级被覆盖，
//     而不需要一台真实的飞牛设备。这是本项目相对早期版本最重要的可测性改进。
//  2. 演练模式：把 QLINK_APPCENTER_CLI 指向空值时，可以完整走一遍安装编排
//     并观察日志与生成的包目录，而不真正改动系统。
type FakeCLI struct {
	mu sync.Mutex

	// Installed 记录当前已注册的应用 → 运行状态。
	Installed map[string]AppState
	// Calls 按顺序记录收到的命令，便于断言调用序列。
	Calls []string

	// 可注入的故障点。
	FailInstall   map[string]error
	FailStart     map[string]error
	FailUninstall map[string]error
	ListErr       error
	Volume        int

	// InstallRoot 非空时，InstallLocal 会把包里的桌面入口配置
	// 落到 <InstallRoot>/<appName>/ 下，模拟真实应用中心的安装结果。
	//
	// 有了它，测试才能覆盖「已安装且配置正确 → 不重复安装」这条关键路径；
	// 否则 isStale 永远为真，测试会误以为系统在正确地反复重装。
	InstallRoot string
}

// NewFakeCLI 创建一个空的假 CLI。
func NewFakeCLI() *FakeCLI {
	return &FakeCLI{
		Installed:     map[string]AppState{},
		FailInstall:   map[string]error{},
		FailStart:     map[string]error{},
		FailUninstall: map[string]error{},
		Volume:        1,
	}
}

func (f *FakeCLI) record(format string, args ...any) {
	f.Calls = append(f.Calls, fmt.Sprintf(format, args...))
}

// Seed 预置一个已注册的应用。
func (f *FakeCLI) Seed(appName string, state AppState) *FakeCLI {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Installed[appName] = state
	return f
}

// Available 总是返回 true。
func (f *FakeCLI) Available() bool { return true }

// DefaultVolume 返回预设的卷号。
func (f *FakeCLI) DefaultVolume() (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Volume <= 0 {
		return 1, nil
	}
	return f.Volume, nil
}

// List 返回全部已注册应用名。
func (f *FakeCLI) List() ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("list")
	if f.ListErr != nil {
		return nil, f.ListErr
	}
	out := make([]string, 0, len(f.Installed))
	for name := range f.Installed {
		out = append(out, name)
	}
	return out, nil
}

// Status 返回应用状态。
func (f *FakeCLI) Status(appName string) (AppState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("status %s", appName)
	if s, ok := f.Installed[appName]; ok {
		return s, nil
	}
	return StateStopped, nil
}

// Start 把应用置为运行态。
func (f *FakeCLI) Start(appName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("start %s", appName)
	if err, ok := f.FailStart[appName]; ok {
		return err
	}
	if _, ok := f.Installed[appName]; !ok {
		return fmt.Errorf("应用 %s 未安装", appName)
	}
	f.Installed[appName] = StateRunning
	return nil
}

// Stop 把应用置为停止态。
func (f *FakeCLI) Stop(appName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("stop %s", appName)
	if _, ok := f.Installed[appName]; !ok {
		return nil // 与真实 CLI 的幂等语义保持一致
	}
	f.Installed[appName] = StateStopped
	return nil
}

// InstallLocal 从包目录里的 manifest 读出包名并登记为运行中。
//
// 刻意去读真实的 manifest（而不是让调用方预设包名）：
// 这样测试顺带验证了 BuildPackage 确实写出了 appname 正确的 manifest，
// 也避免了「测试里预设的包名和真实产物对不上」这类假绿灯。
func (f *FakeCLI) InstallLocal(dir string, volume int) error {
	name, err := readManifestAppName(dir)
	if err != nil {
		return fmt.Errorf("FakeCLI: %w", err)
	}

	f.mu.Lock()
	root := f.InstallRoot
	f.mu.Unlock()

	if root != "" {
		if err := materializePackage(dir, root, name); err != nil {
			return fmt.Errorf("FakeCLI: %w", err)
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("install-local %s volume=%d", name, volume)

	if err, ok := f.FailInstall[name]; ok {
		return err
	}
	f.Installed[name] = StateRunning
	return nil
}

// materializePackage 把包里的桌面入口配置复制到安装根目录。
//
// 落盘为 <InstallRoot>/<appName>/ui/config：模拟「app/ 目录内容被解压成
// target 目录」之后的真实布局。测试关心的只有
// 「已安装应用的桌面入口配置长什么样」这一件事。
func materializePackage(pkgDir, root, appName string) error {
	rel := filepath.FromSlash(UIDir + "/config")
	data, err := os.ReadFile(filepath.Join(pkgDir, "app", rel))
	if err != nil {
		return nil
	}
	dst := filepath.Join(root, appName, rel)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o644)
}

// readManifestAppName 解析包目录下 manifest 的 appname 字段。
func readManifestAppName(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, "manifest"))
	if err != nil {
		return "", fmt.Errorf("读取 manifest 失败: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, found := strings.Cut(line, "=")
		if !found || strings.TrimSpace(key) != "appname" {
			continue
		}
		name := strings.TrimSpace(value)
		if name == "" {
			return "", fmt.Errorf("manifest 中 appname 为空")
		}
		return name, nil
	}
	return "", fmt.Errorf("manifest 中缺少 appname")
}

// Uninstall 移除应用。
func (f *FakeCLI) Uninstall(appName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("uninstall %s", appName)
	if err, ok := f.FailUninstall[appName]; ok {
		return err
	}
	delete(f.Installed, appName)
	return nil
}

// CallLog 返回调用序列的快照。
func (f *FakeCLI) CallLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.Calls))
	copy(out, f.Calls)
	return out
}

// HasCall 报告是否出现过某次调用（前缀匹配）。
func (f *FakeCLI) HasCall(prefix string) bool {
	for _, c := range f.CallLog() {
		if len(c) >= len(prefix) && c[:len(prefix)] == prefix {
			return true
		}
	}
	return false
}

// States 返回已注册应用的快照。
func (f *FakeCLI) States() map[string]AppState {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]AppState, len(f.Installed))
	for k, v := range f.Installed {
		out[k] = v
	}
	return out
}
