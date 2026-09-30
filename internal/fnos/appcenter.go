// Package fnos 负责与飞牛 OS（fnOS）的桌面 / 应用中心交互。
//
// 组织方式：
//
//	appcenter.go  应用中心命令行的抽象接口与输出解析（纯逻辑，可测）
//	exec.go       基于 os/exec 的真实实现
//	manifest.go   manifest 文件构造
//	package.go    子应用包目录树构造
//	cgi.go        生成 CGI 跳转脚本（无端口形态的唯一出口）
//	icon.go       图标解析、缩放、落盘
//	service.go    安装、卸载、对账、孤立清理的编排
//
// 最关键的架构改进是把「命令行」抽象成 CLI 接口：
// 早期版本的安装逻辑与 exec.Command 强耦合，导致在开发机上完全无法测试，
// 只有在真机上才敢跑。现在安装编排可以用 FakeCLI 在毫秒级跑完全部用例。
package fnos

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/MisiteQ/qlink2desktop/internal/domain"
)

// 哨兵错误。
var (
	// ErrCLIUnavailable：当前环境没有 appcenter-cli（开发机 / 未安装飞牛）。
	ErrCLIUnavailable = errors.New("未检测到 appcenter-cli")
	// ErrRefused：出于安全策略拒绝操作某个包（例如不属于本项目的包名）。
	ErrRefused = errors.New("拒绝操作非本项目管理的应用")
)

// AppState 是飞牛应用中心报告的应用运行状态。
type AppState string

const (
	StateRunning  AppState = "running"
	StateStarting AppState = "starting"
	StateStopping AppState = "stopping"
	StateStopped  AppState = "stopped"
	StateUnknown  AppState = "unknown"
)

// Active 报告应用是否处于「图标应当可见」的状态。
func (s AppState) Active() bool { return s == StateRunning || s == StateStarting }

// String 实现 fmt.Stringer。
func (s AppState) String() string { return string(s) }

// CLI 抽象飞牛应用中心命令行工具。
//
// 所有方法与 __真实命令__ 一一对应，接口刻意保持窄小，
// 以便 FakeCLI 能用最少代码覆盖全部编排分支。
type CLI interface {
	// Available 报告命令行工具是否可用。
	Available() bool
	// DefaultVolume 返回默认安装卷号，失败时调用方应回落到 1。
	DefaultVolume() (int, error)
	// List 返回应用中心已注册的应用名列表。
	List() ([]string, error)
	// Status 返回单个应用的运行状态。
	Status(appName string) (AppState, error)
	// Start 启动应用（使桌面图标可见）。
	Start(appName string) error
	// Stop 停止应用（使桌面图标立即隐藏）。
	Stop(appName string) error
	// InstallLocal 在 dir 目录下执行本地安装。
	InstallLocal(dir string, volume int) error
	// Uninstall 卸载应用。
	Uninstall(appName string) error
}

// ---------------------------------------------------------------------------
// 输出解析
// ---------------------------------------------------------------------------

// nameTokenPattern 用于从命令行输出里捞出形如 a.b 的应用标识。
// 比早期版本「按 │ 切分取第二列」稳得多：既容忍列宽变化，
// 也容忍不同版本的表格边框字符（│ / | / ┃）。
var nameTokenPattern = regexp.MustCompile(`[A-Za-z0-9][A-Za-z0-9._-]*\.[A-Za-z0-9._-]+`)

// borderRunes 是各版本表格可能使用的竖线字符。
const borderRunes = "│|┃║"

// boxDrawingRunes 是表格里的全部制表符（含边框、分隔线、转角）。
// 只用来判断「这一行是分隔线」——纯制表符的行没有任何业务信息。
const boxDrawingRunes = borderRunes + "+-=─┌┬┐├┼┤└┴┘╭╮╰╯"

// ParseAppList 从 `appcenter-cli list` 的输出里解析出应用名列表。
//
// 同时兼容三种形态：
//  1. 带边框的表格（任意竖线字符）；
//  2. 制表符 / 多空格分隔的纯文本列；
//  3. 每行一个名字的裸列表。
func ParseAppList(output string) []string {
	seen := make(map[string]bool)
	var names []string

	appendName := func(n string) {
		n = strings.TrimSpace(n)
		if n == "" || seen[n] {
			return
		}
		seen[n] = true
		names = append(names, n)
	}

	for _, rawLine := range strings.Split(output, "\n") {
		line := strings.TrimSpace(strings.Trim(rawLine, "\r"))
		if line == "" {
			continue
		}

		// 跳过表头分隔线与表头文字。
		if isTableDivider(line) {
			continue
		}

		// 优先按竖线切列。
		if strings.ContainsAny(line, borderRunes) {
			fields := strings.FieldsFunc(line, func(r rune) bool {
				return strings.ContainsRune(borderRunes, r)
			})
			for _, f := range fields {
				f = strings.TrimSpace(f)
				if f != "" {
					appendName(f)
					break // 第一列即应用名
				}
			}
			continue
		}

		// 退化为「首个字段」。
		fields := strings.Fields(line)
		if len(fields) > 0 {
			appendName(fields[0])
		}
	}
	return names
}

func isTableDivider(line string) bool {
	trimmed := strings.Trim(line, "+-= ")
	if trimmed == "" {
		return true
	}
	// 形如 "├──────┼──────┤"
	letters := strings.Map(func(r rune) rune {
		if strings.ContainsRune(boxDrawingRunes, r) {
			return -1
		}
		return r
	}, line)
	return strings.TrimSpace(letters) == ""
}

// ExtractManagedApps 从列表解析结果中筛出本项目管理（或历史遗留）的包名。
func ExtractManagedApps(names []string) []string {
	var out []string
	seen := make(map[string]bool)
	for _, n := range names {
		if !domain.IsManagedApp(n) || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

// ParseAppState 把 `appcenter-cli status` 的输出归一为 AppState。
//
// 采用「关键词命中」而非「全等比较」：真实输出里常带额外说明文字
// （例如 "app is running (pid 1234)"），全等比较会误判为 unknown。
func ParseAppState(output string) AppState {
	lower := strings.ToLower(output)
	switch {
	// 注意顺序：必须先判 stopped。"not running" 同时包含 "running"，
	// 若先判 running，停止态会被误判成运行态，桌面图标就会一直显示但点开是空的。
	case strings.Contains(lower, "stopped"), strings.Contains(lower, "not running"),
		strings.Contains(lower, "已停止"), strings.Contains(lower, "未运行"):
		return StateStopped
	case strings.Contains(lower, "stopping"), strings.Contains(lower, "停止中"):
		return StateStopping
	case strings.Contains(lower, "starting"), strings.Contains(lower, "启动中"):
		return StateStarting
	case strings.Contains(lower, "running"), strings.Contains(lower, "运行中"):
		return StateRunning
	default:
		return StateUnknown
	}
}

// cleaner 剥离命令行里的进度动画噪声，让日志可读。
//
// 早期版本为此写了近百行的启发式规则；这里收敛为「去掉纯符号行」一条规则，
// 因为真正需要保留的信息（错误正文）一定含有字母或数字。
func CleanCLIOutput(output string) string {
	var kept []string
	for _, line := range strings.Split(strings.ReplaceAll(output, "\r", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.ContainsFunc(line, func(r rune) bool {
			return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
				r >= 0x4E00 // 汉字
		}) {
			continue
		}
		// 过滤 "Installing..." 这类纯进度行。
		lower := strings.ToLower(line)
		if strings.HasSuffix(lower, "...") && !strings.Contains(lower, "error") && !strings.Contains(lower, "fail") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "; ")
}

// volumePattern 匹配（可带符号的）整数，用于识别 "-1" 这类非法输出。
var volumePattern = regexp.MustCompile(`-?\d+`)

// ParseDefaultVolume 从 `appcenter-cli default-volume` 的输出里解析卷号。
// 卷号必须是正数：0 和负数都视为解析失败，由调用方回落到 1。
func ParseDefaultVolume(output string) (int, error) {
	trimmed := strings.TrimSpace(output)
	digits := volumePattern.FindString(trimmed)
	if digits == "" {
		return 0, fmt.Errorf("无法从 %q 解析默认存储卷", trimmed)
	}
	v, err := strconv.Atoi(digits)
	if err != nil || v <= 0 {
		return 0, fmt.Errorf("解析出的存储卷号非法: %q", digits)
	}
	return v, nil
}
