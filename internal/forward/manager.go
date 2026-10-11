// Package forward 实现四层（TCP / UDP）端口转发。
//
// 它与 internal/proxy 的关系是**互补而不是替代**：
//
//	proxy 工作在应用层，能按域名路由、能跳过自签证书、能改写响应，
//	但它只认 HTTP；forward 工作在传输层，看不见也不解析内容，
//	因此 SSH、RDP、SMB、数据库、DNS、游戏这些都能原样穿过去。
//
// 典型用途是把内网某个服务的一个端口暴露给能访问到这台设备的人：
// 例如把 192.168.31.50:3389 的远程桌面映射成本机的 13389，
// 或者把公网的 IPv6 入口转发到只支持 IPv4 的内网设备上。
package forward

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/MisiteQ/qlink2desktop/internal/domain"
)

// Status 是一条转发规则的实时运行态。
//
// 与定义分离（和 domain.Status 同样的道理）：它每次查询实时合成、
// 不写盘，因此永远不会把"上一次的临时故障"当成事实持久化下来。
type Status struct {
	// Running 为 true 表示至少有一个协议正在监听。
	Running bool `json:"running"`
	// Since 是本次启动的时间。
	Since time.Time `json:"since,omitempty"`
	// ListenAddr 是实际监听的地址（含端口）。
	ListenAddr string `json:"listen_addr,omitempty"`
	// BindMode 是实际落到的协议栈（dual / ipv4 / ipv6）。
	//
	// 留空监听地址时绑定会自动退让，所以"监听成功"不等于"两种协议都能连"：
	// 退到仅 IPv6 时，IPv4 的访客是连不上的。把形态显示出来，
	// 用户看到的才是事实，而不是一个"正常运行"的假象。
	BindMode string `json:"bind_mode,omitempty"`
	// Protocols 列出真正在监听的协议，便于排查"只起了一半"的情况。
	//
	// both 的规则如果 TCP 端口被占用、UDP 空闲，就会是这种半死不活的状态：
	// 只报一个 running=true 会把问题彻底藏起来。
	Protocols []string `json:"protocols,omitempty"`
	// Error 是最近一次启动 / 运行失败的原因。
	Error string `json:"error,omitempty"`
	// Counters 是累计计数。
	Counters CounterSnapshot `json:"counters"`
}

// 默认最大连接日志条数（每条规则各自一份缓冲）。
const defaultRecentCap = 100

// ruleState 是 Manager 内部持有的单条规则状态。
type ruleState struct {
	spec     domain.Forward
	acl      ACL
	counters *Counters
	logs     *connRing
	tcp      *tcpFwd
	udp      *udpFwd
	since    time.Time
	err      string
}

// Manager 持有全部转发规则的监听实例。
//
// 并发模型与 proxy.Manager 一致：一把 RWMutex 保护索引表，
// 启停都在锁内完成，避免出现「端口已释放但索引还指向它」的中间态。
// 真正的转发 goroutine 完全不碰这把锁——它们只在规则停止时被等待收尾。
type Manager struct {
	mu       sync.RWMutex
	rules    map[string]*ruleState
	logger   *slog.Logger
	logCap   int
	onChange func()
}

// Option 是 Manager 的可选配置。
type Option func(*Manager)

// WithLogger 注入日志器。
func WithLogger(l *slog.Logger) Option {
	return func(m *Manager) {
		if l != nil {
			m.logger = l
		}
	}
}

// WithLogCapacity 设置每条规则保留的最近连接记录条数。
func WithLogCapacity(n int) Option {
	return func(m *Manager) {
		if n > 0 {
			m.logCap = n
		}
	}
}

// NewManager 创建转发管理器。
func NewManager(opts ...Option) *Manager {
	m := &Manager{
		rules:  make(map[string]*ruleState),
		logger: slog.Default(),
		logCap: defaultRecentCap,
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// SetOnChange 注册状态变化回调（用于向前端广播）。
//
// 转发规则的启停会发生在本进程内部的任何地方（接口层改配置、
// 装配层启动恢复），回调让"状态变了"这件事有一个统一的出口，
// 前端不必靠轮询才能看到变化。
func (m *Manager) SetOnChange(fn func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onChange = fn
}

func (m *Manager) notify() {
	if m.onChange != nil {
		m.onChange()
	}
}

// Apply 让一条规则的运行状态与它的定义一致。
//
// 幂等：配置完全相同时直接返回，这样「启动时恢复全部规则」可以无条件
// 调用而不会打断正在转发的连接——那条连接可能是一个 SSH 会话。
//
// Enabled 为 false 时等价于 Stop：规则只保留定义，不占用端口。
// 把它放在这里判断而不是让每个调用方写 if，是因为"停用"这个语义
// 只有一种正确实现（释放端口），分散写迟早会漏掉一处。
func (m *Manager) Apply(spec domain.Forward) error {
	spec.Normalize()
	if err := spec.Validate(); err != nil {
		return err
	}
	if !spec.Enabled {
		m.Stop(spec.ID)
		return nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if existing, ok := m.rules[spec.ID]; ok {
		if sameSpec(existing.spec, spec) && existing.err == "" {
			return nil
		}
		m.stopLocked(spec.ID)
	}

	if err := m.checkConflictsLocked(spec); err != nil {
		return err
	}
	if err := m.checkLoopLocked(spec); err != nil {
		return err
	}

	st := &ruleState{
		spec:     spec,
		acl:      compileACL(spec.AccessMode, spec.AccessList),
		counters: &Counters{},
		logs:     newConnRing(m.logCap),
		since:    time.Now(),
	}

	// 先建 UDP 再建 TCP：UDP 端口通常是"顺带"要的，
	// TCP 失败更常见（被别的服务占用），让它的错误信息排在前面更有用。
	var errs []string
	if spec.Protocol.HasUDP() {
		f := newUDPFwd(spec, st.acl, st.counters, st.logs, m.logger)
		if err := f.start(); err != nil {
			errs = append(errs, fmt.Sprintf("UDP %s", describeListenErr(err, spec.ListenPort, "udp")))
		} else {
			st.udp = f
		}
	}
	if spec.Protocol.HasTCP() {
		f := newTCPFwd(spec, st.acl, st.counters, st.logs, m.logger)
		if err := f.start(); err != nil {
			errs = append(errs, fmt.Sprintf("TCP %s", describeListenErr(err, spec.ListenPort, "tcp")))
		} else {
			st.tcp = f
		}
	}

	m.rules[spec.ID] = st
	if len(errs) > 0 {
		st.err = strings.Join(errs, "；")
		// 部分失败也要登记：both 的规则可能只起了 UDP，
		// 让调用方知道"没全起来"，而不是拿到一个 200 就以为万事大吉。
		m.logger.Error("端口转发监听部分失败", "rule", spec.Key(),
			"listen", spec.ListenAddr(), "error", st.err)
		go m.notify()
		return fmt.Errorf("%w: %s", domain.ErrValidation, st.err)
	}

	m.logger.Info("端口转发已启动", "rule", spec.Key(), "listen", spec.ListenAddr(),
		"target", spec.TargetAddr(), "protocol", spec.Protocol)
	go m.notify()
	return nil
}

// Check 只判断「这条规则能不能启动」，不真的建立监听。
//
// 存在的意义是让调用方能在**落盘之前**把确定性的冲突挡掉。
//
// 为什么必须区分：Apply 的失败有两类，性质完全不同。
//   - 端口被本应用的另一条规则占用、与另一条规则互为转发目标：
//     这是配置本身的错误，只要不改就永远不会好。如果让它落盘，
//     规则会长期停在"保存了但不工作"的状态，用户每次打开都要重新判断一次。
//   - 端口被本应用之外的程序占用：这是环境的暂时状态，随时可能变好
//     （那个程序被关掉）。这种必须允许保存，否则用户连配置都存不下来。
//
// Check 只覆盖第一类——它只知道本应用内部的规则，不知道外部占用了什么。
// 第二类由 Apply 返回、调用方作为告警呈现。
func (m *Manager) Check(spec domain.Forward) error {
	spec.Normalize()
	if err := spec.Validate(); err != nil {
		return err
	}
	if !spec.Enabled {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if err := m.checkConflictsLocked(spec); err != nil {
		return err
	}
	return m.checkLoopLocked(spec)
}

// CheckListen 检查某个监听配置是否与本应用已有的规则冲突。
//
// 与 Check 的差别：Check 面向"保存一条完整规则"，会先过字段校验
// （名称、端口范围…）。而这里的调用方可能手里只有"协议 + 地址 + 端口"——
// 比如前端的端口占用检测、或者用户还在填表单。
// 用 Check 会导致探测一个空闲端口时收到"规则名称不能为空"这种
// 与端口毫无关系的报错（这个 bug 是探测接口的真实验证抓出来的）。
func (m *Manager) CheckListen(proto domain.Protocol, host string, port int) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.checkConflictsLocked(domain.Forward{
		Protocol:   proto,
		ListenHost: host,
		ListenPort: port,
	})
}

// Stop 停止一条规则。
func (m *Manager) Stop(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopLocked(id) {
		m.logger.Info("端口转发已停止", "id", id)
		go m.notify()
	}
}

// stopLocked 停止并移除规则，返回是否真的停止了某个实例。
func (m *Manager) stopLocked(id string) bool {
	st, ok := m.rules[id]
	if !ok {
		return false
	}
	delete(m.rules, id)
	if st.tcp != nil {
		st.tcp.close()
	}
	if st.udp != nil {
		st.udp.close()
	}
	return true
}

// StopAll 停止全部规则（进程退出时调用）。
func (m *Manager) StopAll() {
	m.mu.Lock()
	ids := make([]string, 0, len(m.rules))
	for id := range m.rules {
		ids = append(ids, id)
	}
	for _, id := range ids {
		m.stopLocked(id)
	}
	m.mu.Unlock()
	m.logger.Info("端口转发已全部停止", "count", len(ids))
}

// Status 查询单条规则的运行态。
func (m *Manager) Status(id string) Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	st, ok := m.rules[id]
	if !ok {
		return Status{}
	}
	return st.status()
}

// Statuses 返回全部运行中规则的运行态。
func (m *Manager) Statuses() map[string]Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]Status, len(m.rules))
	for id, st := range m.rules {
		out[id] = st.status()
	}
	return out
}

// Recent 返回某条规则最近的连接记录（按时间从旧到新）。
func (m *Manager) Recent(id string, n int) []ConnLog {
	m.mu.RLock()
	st, ok := m.rules[id]
	m.mu.RUnlock()
	if !ok {
		return []ConnLog{}
	}
	return st.logs.tail(n)
}

// RunningCount 返回正在运行的规则数量。
func (m *Manager) RunningCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.rules)
}

// UsedPorts 返回已被转发占用的端口（按协议分开），供前端提示冲突。
//
// 键形如 "tcp:8080"。TCP 与 UDP 的同号端口互不冲突，
// 因此不能只返回端口号——否则填 UDP 8080 会被一个 TCP 8080 误伤。
func (m *Manager) UsedPorts() map[string]bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]bool, len(m.rules)*2)
	for _, st := range m.rules {
		for _, p := range st.status().Protocols {
			out[fmt.Sprintf("%s:%d", p, st.spec.ListenPort)] = true
		}
	}
	return out
}

// --------------------------------------------------------------------------
// 冲突与环检测
// --------------------------------------------------------------------------

// checkConflictsLocked 检查监听端口是否被另一条规则占用。
//
// 监听地址也要一起比：两条规则分别监听 127.0.0.1:8080 与 0.0.0.0:8080
// 在 Linux 上是会冲突的（后者覆盖了前者），但在 Go 的 net.Listen 上
// 前者能成功。这里宁可保守一点把冲突报出来，也不要让用户遇到
// "明明提示没占用的端口，保存却失败"。
func (m *Manager) checkConflictsLocked(spec domain.Forward) error {
	for id, other := range m.rules {
		if id == spec.ID {
			continue
		}
		if !protocolsOverlap(spec.Protocol, other.spec.Protocol) {
			continue
		}
		if other.spec.ListenPort != spec.ListenPort {
			continue
		}
		if !listenHostsOverlap(spec.ListenHost, other.spec.ListenHost) {
			continue
		}
		return fmt.Errorf("%w: 端口 %d 已被规则「%s」占用，请换一个端口或先停用那条规则",
			domain.ErrPortInUse, spec.ListenPort, other.spec.Name)
	}
	return nil
}

// checkLoopLocked 检测两跳转发环：A→B→A。
//
// 为什么只查两跳而不是做全图遍历：真实的转发配置几乎不会超过两跳，
// 而两跳环（两条规则互相转发对方）恰恰是手工配置最容易配出来的 ——
// 想把 8080 暴露出去、又想把 9090 暴露出去，手一滑就对调了目标端口。
// 它一旦发生，一个数据包会在两个端口之间无限弹射，几秒内吃满 CPU。
//
// 直接的自转发（A→A）由 domain.Forward.Validate 拦住，不走这里。
func (m *Manager) checkLoopLocked(spec domain.Forward) error {
	if !domain.IsLocalHost(spec.TargetHost) {
		return nil
	}
	for id, other := range m.rules {
		if id == spec.ID {
			continue
		}
		if !protocolsOverlap(spec.Protocol, other.spec.Protocol) {
			continue
		}
		if !domain.IsLocalHost(other.spec.TargetHost) {
			continue
		}
		if other.spec.ListenPort != spec.TargetPort || other.spec.TargetPort != spec.ListenPort {
			continue
		}
		return fmt.Errorf("%w: 与规则「%s」互为转发目标（%d ⇄ %d），会形成死循环",
			domain.ErrValidation, other.spec.Name, spec.ListenPort, other.spec.TargetPort)
	}
	return nil
}

func protocolsOverlap(a, b domain.Protocol) bool {
	return (a.HasTCP() && b.HasTCP()) || (a.HasUDP() && b.HasUDP())
}

// listenHostsOverlap 判断两个监听地址是否会互相干扰。
//
// 空串代表"监听全部地址"，与任何具体地址都重叠。
func listenHostsOverlap(a, b string) bool {
	if a == "" || b == "" {
		return true
	}
	return strings.EqualFold(a, b)
}

// --------------------------------------------------------------------------
// 辅助
// --------------------------------------------------------------------------

func (st *ruleState) status() Status {
	s := Status{
		Running:   st.tcp != nil || st.udp != nil,
		Since:     st.since,
		Protocols: []string{},
		Error:     st.err,
		Counters:  st.counters.snapshot(),
	}
	if st.tcp != nil {
		s.Protocols = append(s.Protocols, "tcp")
	}
	if st.udp != nil {
		s.Protocols = append(s.Protocols, "udp")
	}
	if s.Running && st.tcp != nil {
		s.ListenAddr = st.tcp.ln.Addr().String()
		s.BindMode = string(st.tcp.mode)
	} else if s.Running && st.udp != nil {
		s.ListenAddr = st.udp.conn.LocalAddr().String()
		s.BindMode = string(st.udp.mode)
	} else {
		s.ListenAddr = st.spec.ListenAddr()
	}
	return s
}

// sameSpec 判断两条规则的关键配置是否一致（决定是否需要重建监听）。
func sameSpec(a, b domain.Forward) bool {
	if a.Protocol != b.Protocol || a.ListenHost != b.ListenHost ||
		a.ListenPort != b.ListenPort || a.TargetHost != b.TargetHost ||
		a.TargetPort != b.TargetPort || a.MaxConns != b.MaxConns ||
		a.AccessMode != b.AccessMode {
		return false
	}
	if len(a.AccessList) != len(b.AccessList) {
		return false
	}
	for i := range a.AccessList {
		if a.AccessList[i] != b.AccessList[i] {
			return false
		}
	}
	return true
}

// describeListenErr 把监听失败翻译成用户能看懂的话，并尽量指出是谁占着。
//
// 原始的 "listen tcp :8080: bind: address already in use" 对排查有用，
// 但对用户没有行动指引——他不知道是该换端口，还是该先停掉别的服务。
//
// 更关键的是"是谁占着"：只报"已被其它程序占用"，用户在遇到
// **本应用自己占用**的情况时会去满机器找一个根本不存在的"其它程序"。
// 占用者是别的进程还是本进程，给出的行动建议完全不同。
func describeListenErr(err error, port int, proto string) string {
	if err == nil {
		return ""
	}
	s := strings.ToLower(err.Error())
	switch {
	case isAddrInUse(err):
		return fmt.Sprintf("端口 %d 已被占用%s", port, occupancyHint(port, proto))
	case strings.Contains(s, "permission denied"):
		// 1024 以下的特权端口在非 root 下会走到这里。本应用以 root 运行，
		// 真机一般不会遇到；但开发机上跑二进制时相当常见，值得一说。
		return fmt.Sprintf("端口 %d 没有监听权限（1024 以下的端口需要管理员权限）", port)
	default:
		return err.Error()
	}
}

// occupancyHint 尝试查出占用者，拼成一句补充说明。
//
// 查不到时回落成中性文案：宁可不说，也不要给一个可能误导人的猜测。
func occupancyHint(port int, proto string) string {
	holders := whoHoldsPort(port, proto)
	if len(holders) == 0 {
		return "，请换一个端口或先停掉占用它的服务"
	}

	parts := make([]string, 0, len(holders))
	self := false
	for _, h := range holders {
		if h.Self {
			self = true
		}
		name := h.Name
		if name == "" {
			name = "未知进程"
		}
		parts = append(parts, fmt.Sprintf("%s（PID %d，占用 %s 侧）", name, h.PID, h.Side))
	}
	detail := strings.Join(parts, "、")

	if self {
		// 只报 PID 对本应用没有意义——用户在自己进程里看不出是哪个功能。
		// 18000 段是内置反向代理的分配区间，这是最常见的一种。
		return fmt.Sprintf("，占用者是本应用自己（%s）。"+
			"最常见的原因是：该端口落在内置反向代理的分配区间（18000 段），"+
			"被「桌面应用」里的某条链接占用了 —— 去那里停用那条链接，"+
			"或给本条规则换一个端口。", detail)
	}
	return fmt.Sprintf("，占用者是 %s。请换一个端口，或先停掉占用它的服务", detail)
}

// isAddrInUse 判断错误是否为「地址已被占用」。
//
// 用字符串匹配而不是直接比 syscall.EADDRINUSE：后者在各平台上的
// 常量值与可达性并不一致，而这里只需要区分"端口被占"与"其它原因"，
// 文本判断足够可靠且可测。
func isAddrInUse(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "address already in use") ||
		strings.Contains(s, "only one usage of each socket address")
}

// shortErr 把错误压成一行短文本，供连接日志展示。
func shortErr(err error) string {
	if err == nil {
		return ""
	}
	s := strings.TrimSpace(err.Error())
	if i := strings.Index(s, "\n"); i > 0 {
		s = s[:i]
	}
	// 去掉 "dial tcp 1.2.3.4:80: " 这类重复的前缀，日志里目标地址已单列。
	if i := strings.LastIndex(s, ": "); i > 0 && i < len(s)-2 {
		s = s[i+2:]
	}
	if len([]rune(s)) > 120 {
		s = string([]rune(s)[:120]) + "…"
	}
	return s
}
