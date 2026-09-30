package fnos

import (
	"fmt"
	"runtime"
	"strings"
)

// 生成包的署名信息。
//
// 这些子应用包由 QLink2Desktop 在用户的飞牛设备上现场生成，
// 因此开发者署名与来源地址都指向本项目自身。
const (
	// Maintainer 是开发者 / 团队名称。
	Maintainer = "MisiteQ"
	// MaintainerURL 是项目主页。
	MaintainerURL = "https://github.com/MisiteQ/QLink2Desktop"
	// Distributor 是发布者名称。
	Distributor = "MisiteQ"
)

// manifestField 是 manifest 里的一行 key = value。
type manifestField struct {
	Key   string
	Value string
}

// Manifest 描述一个飞牛应用包的元数据。
//
// 用「有序字段切片 + 构造函数」取代早期版本的巨型 fmt.Sprintf 字符串拼接：
// 早期版本里字段顺序、缩进、条件分支全部搅在一起，
// 想加一个字段要在两百行字符串里找位置，且极易漏掉换行。
//
// 字段集严格对齐 developer.fnnas.com 的 Manifest 文档：
// 只写官方定义过的键，不夹带私货（例如旧实现的 arch、ctl_stop 猜测值）。
type Manifest struct {
	fields []manifestField
}

// NewManifest 创建 manifest 并写入所有必需字段。
//
// 字段顺序刻意与官方文档的段落顺序一致（基础信息 → 平台 → 开发者 →
// 兼容范围 → 安装 → 桌面集成），方便和文档逐条对照。
func NewManifest(appName, displayName, desc string) *Manifest {
	m := &Manifest{}
	m.Set("appname", appName)
	m.Set("version", "1.0.0")
	m.Set("display_name", displayName)
	m.Set("desc", desc)
	m.Set("platform", ManifestPlatform())
	m.Set("source", "thirdparty")
	m.Set("maintainer", Maintainer)
	m.Set("maintainer_url", MaintainerURL)
	m.Set("distributor", Distributor)
	m.Set("distributor_url", MaintainerURL)
	m.Set("os_min_version", "0.9.0")
	m.Set("install_type", "root")
	m.Set("desktop_uidir", "ui")
	return m
}

// ManifestPlatform 把 Go 的目标架构翻译为飞牛规范里的 platform 取值。
//
// 官方只认 x86 / arm / all。这里永远不返回 all：
// 本应用包含原生的 Go 二进制，声明 all 会让 ARM 设备装上 x86 包后开不了机。
func ManifestPlatform() string {
	switch runtime.GOARCH {
	case "arm64", "arm":
		return "arm"
	default:
		return "x86"
	}
}

// Set 新增或覆盖一个字段（保持首次插入的位置，便于输出稳定可比对）。
func (m *Manifest) Set(key, value string) *Manifest {
	value = sanitizeManifestValue(value)
	for i := range m.fields {
		if m.fields[i].Key == key {
			m.fields[i].Value = value
			return m
		}
	}
	m.fields = append(m.fields, manifestField{Key: key, Value: value})
	return m
}

// Unset 删除一个字段（不存在时无副作用）。
func (m *Manifest) Unset(key string) *Manifest {
	out := m.fields[:0]
	for _, f := range m.fields {
		if f.Key != key {
			out = append(out, f)
		}
	}
	m.fields = out
	return m
}

// WithLaunchName 声明桌面启动入口。
//
// 取值必须与 {desktop_uidir}/config 里 .url 下的入口 ID 完全一致，
// 否则应用中心卡片点击后找不到入口。
func (m *Manifest) WithLaunchName(name string) *Manifest {
	return m.Set("desktop_applaunchname", name)
}

// WithServicePort 声明服务端口，并按飞牛规范关闭端口探测（checkport=false）。
//
// 这里声明的端口是「桌面图标要打开的端口」，不是本包自己监听的端口
// ——子应用是个纯粹的快捷方式，没有任何常驻进程。
// 若保持 checkport 默认值 true，应用中心会去探测这个端口并因探测失败
// 把图标标为异常，用户看到的就是一个带感叹号的灰图标。
func (m *Manifest) WithServicePort(port int) *Manifest {
	if port <= 0 {
		return m
	}
	return m.Set("service_port", fmt.Sprint(port)).Set("checkport", "false")
}

// Bytes 按稳定顺序渲染 manifest 文本。
func (m *Manifest) Bytes() []byte {
	var b strings.Builder
	b.Grow(len(m.fields) * 48)
	// 与官方模板一致：键左对齐到固定宽度，人读和 diff 都更舒服。
	width := 0
	for _, f := range m.fields {
		if len(f.Key) > width {
			width = len(f.Key)
		}
	}
	for _, f := range m.fields {
		b.WriteString(f.Key)
		b.WriteString(strings.Repeat(" ", width-len(f.Key)))
		b.WriteString(" = ")
		b.WriteString(f.Value)
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

// sanitizeManifestValue 把换行折叠掉。
// manifest 是逐行的 key = value 格式，值里一旦出现换行就会把文件结构破坏掉，
// 而 desc / display_name 都来自用户输入，因此必须在这里兜一道。
func sanitizeManifestValue(v string) string {
	v = strings.ReplaceAll(v, "\r\n", " ")
	v = strings.ReplaceAll(v, "\n", " ")
	v = strings.ReplaceAll(v, "\r", " ")
	return strings.TrimSpace(v)
}
