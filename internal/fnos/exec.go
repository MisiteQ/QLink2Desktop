package fnos

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/MisiteQ/qlink2desktop/internal/domain"
)

// 飞牛应用中心命令行工具的常见安装位置。
// 按「越靠近真实运行环境越优先」排列。
var cliCandidates = []string{
	"/var/apps/appcenter/target/bin/appcenter-cli",
	"/usr/bin/appcenter-cli",
	"/usr/local/bin/appcenter-cli",
	"/host/root/usr/bin/appcenter-cli",
	"/host/root/var/apps/appcenter/target/bin/appcenter-cli",
}

// commandTimeout 是单条 appcenter 命令的超时上限。
// 安装类命令涉及解包与校验，给得宽裕一些；查询类命令短超时以免拖垮接口响应。
const (
	queryTimeout  = 10 * time.Second
	mutateTimeout = 3 * time.Minute
)

// ExecCLI 通过 os/exec 调用真实的 appcenter-cli。
type ExecCLI struct {
	path string
}

// NewExecCLI 探测并返回一个真实的 CLI 实现。
// 若环境中不存在该命令行工具，返回的实例 Available() 为 false，
// 上层据此降级为「模拟模式」（仅构建包、不真正注册）。
func NewExecCLI() *ExecCLI {
	if p := os.Getenv("QLINK_APPCENTER_CLI"); p != "" {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			slog.Info("使用环境变量指定的 appcenter-cli", "path", p)
			return &ExecCLI{path: p}
		}
		slog.Warn("环境变量 QLINK_APPCENTER_CLI 指向的文件不可用，回退到自动探测", "path", p)
	}

	for _, p := range cliCandidates {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			slog.Info("检测到飞牛应用中心命令行工具", "path", p)
			return &ExecCLI{path: p}
		}
	}
	if p, err := exec.LookPath("appcenter-cli"); err == nil {
		slog.Info("从 PATH 中检测到 appcenter-cli", "path", p)
		return &ExecCLI{path: p}
	}

	slog.Info("未检测到 appcenter-cli，将以模拟模式运行（仅构建安装包，不注册桌面图标）")
	return &ExecCLI{}
}

// Available 报告命令行工具是否可用。
func (c *ExecCLI) Available() bool { return c != nil && c.path != "" }

// Path 返回探测到的可执行文件路径（测试与诊断用）。
func (c *ExecCLI) Path() string {
	if c == nil {
		return ""
	}
	return c.path
}

// run 执行一条命令并返回其合并输出。
func (c *ExecCLI) run(timeout time.Duration, dir string, args ...string) ([]byte, error) {
	if !c.Available() {
		return nil, ErrCLIUnavailable
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, c.path, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return out, fmt.Errorf("appcenter-cli %s 超时（%s）", args[0], timeout)
	}
	return out, err
}

// DefaultVolume 查询默认安装卷，失败时由调用方回落到 1。
func (c *ExecCLI) DefaultVolume() (int, error) {
	out, err := c.run(queryTimeout, "", "default-volume")
	if err != nil {
		return 0, err
	}
	return ParseDefaultVolume(string(out))
}

// List 返回应用中心已注册的应用名列表。
func (c *ExecCLI) List() ([]string, error) {
	out, err := c.run(queryTimeout, "", "list")
	if err != nil {
		return nil, fmt.Errorf("appcenter-cli list 失败: %w (%s)", err, CleanCLIOutput(string(out)))
	}
	return ParseAppList(string(out)), nil
}

// Status 返回单个应用的运行状态。
func (c *ExecCLI) Status(appName string) (AppState, error) {
	if err := domain.ValidateAppName(appName); err != nil {
		return StateUnknown, err
	}
	out, err := c.run(queryTimeout, "", "status", appName)
	if err != nil {
		return StateUnknown, fmt.Errorf("appcenter-cli status %s 失败: %w (%s)", appName, err, CleanCLIOutput(string(out)))
	}
	return ParseAppState(string(out)), nil
}

// Start 启动应用；若已处于运行态则视为成功（幂等）。
func (c *ExecCLI) Start(appName string) error {
	if err := domain.ValidateAppName(appName); err != nil {
		return err
	}
	out, err := c.run(mutateTimeout, "", "start", appName)
	if err != nil {
		if isAlreadyRunning(err, out) {
			return nil
		}
		return fmt.Errorf("appcenter-cli start %s 失败: %w (%s)", appName, err, CleanCLIOutput(string(out)))
	}
	return nil
}

// Stop 停止应用；若已处于停止态则视为成功（幂等）。
func (c *ExecCLI) Stop(appName string) error {
	if err := domain.ValidateAppName(appName); err != nil {
		return err
	}
	out, err := c.run(mutateTimeout, "", "stop", appName)
	if err != nil {
		if isAlreadyStopped(err, out) {
			return nil
		}
		// 停止失败不应阻断后续卸载流程，交由调用方决定是否告警。
		return fmt.Errorf("appcenter-cli stop %s 失败: %w (%s)", appName, err, CleanCLIOutput(string(out)))
	}
	return nil
}

// InstallLocal 在 dir 目录下执行本地安装。
func (c *ExecCLI) InstallLocal(dir string, volume int) error {
	if volume <= 0 {
		volume = 1
	}
	start := time.Now()
	out, err := c.run(mutateTimeout, dir, "install-local", "--volume", fmt.Sprint(volume))
	dur := time.Since(start)
	if err != nil {
		return fmt.Errorf("appcenter-cli install-local 失败: %w (%s)", err, CleanCLIOutput(string(out)))
	}
	slog.Debug("appcenter-cli install-local 完成", "dir", dir, "volume", volume, "duration", dur)
	return nil
}

// Uninstall 卸载应用。
//
// 这里有一道硬性安全闸：只有本项目管理命名空间（qlink2d. / 历史遗留前缀）的包才允许卸载，
// 避免任何误传参数导致误删用户的其它原生应用。
func (c *ExecCLI) Uninstall(appName string) error {
	if !domain.IsManagedApp(appName) {
		return fmt.Errorf("%w: %q", ErrRefused, appName)
	}
	out, err := c.run(mutateTimeout, "", "uninstall", appName)
	if err != nil {
		return fmt.Errorf("appcenter-cli uninstall %s 失败: %w (%s)", appName, err, CleanCLIOutput(string(out)))
	}
	return nil
}

func isAlreadyRunning(err error, out []byte) bool {
	combined := strings.ToLower(string(out) + " " + err.Error())
	return strings.Contains(combined, "already running") || strings.Contains(combined, "已运行")
}

func isAlreadyStopped(err error, out []byte) bool {
	combined := strings.ToLower(string(out) + " " + err.Error())
	return strings.Contains(combined, "not running") || strings.Contains(combined, "already stopped") ||
		strings.Contains(combined, "已停止") || strings.Contains(combined, "未运行")
}
