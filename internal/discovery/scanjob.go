package discovery

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------------------
// 网络扫描：两阶段 + 可观测进度
//
// 为什么必须是"后台任务"而不是一个同步接口：
// 一个 /24 网段 × 46 个常见端口 = 上万个 TCP 连接。真机上十几秒是常态，
// 而前端的请求预算是 15 秒 —— 于是用户看到的是「请求超时：15 秒内没有响应」，
// 而扫描其实还在跑，结果却永远回不来（截图里就是这个形态）。
//
// 所以这里的形态是：请求只负责"开始"，进度和结果靠轮询/快照拿。
// 并且分两阶段，让大部分时间花在真正有响应的主机上：
//   阶段一 探活：对每台主机只探一小组端口，找出在线设备；
//   阶段二 细扫：只对在线设备跑完整端口表。
// ---------------------------------------------------------------------------

// hostProbePorts 是「探活」阶段使用的端口集合。
//
// 选取原则：覆盖面要宽（不同系统的设备至少会开其中一个），但必须够小 ——
// 这一阶段的代价是「主机数 × 端口数」，每多一个端口就线性拖慢整轮扫描。
var hostProbePorts = []int{
	22, 80, 81, 139, 443, 445, 554, 3389,
	5000, 5244, 5666, 5900, 8006, 8080, 8443, 9000,
}

// HostProbePorts 返回探活端口集合的副本。
func HostProbePorts() []int {
	out := make([]int, len(hostProbePorts))
	copy(out, hostProbePorts)
	return out
}

// hostPingThreshold 决定"多小的范围不做探活、直接全端口扫"。
//
// 对小范围（用户明确列了几台机器）跳過探活：那种场景下用户要的就是
// "把这几个地址的所有端口都看一遍"，用探活把它们筛掉反而是错的。
const hostPingThreshold = 32

// ScanProgress 是扫描过程中的实时进度快照。
type ScanProgress struct {
	// Stage 是当前阶段的中文描述，直接展示给用户。
	Stage string `json:"stage"`
	// ProbesDone / ProbesTotal 是当前阶段的探测进度（用于进度条）。
	ProbesDone  int `json:"probes_done"`
	ProbesTotal int `json:"probes_total"`
	// HostsTotal / HostsAlive 是主机维度的进度。
	HostsTotal int `json:"hosts_total"`
	HostsAlive int `json:"hosts_alive"`
	// Found 是已确认开放的端口总数，Items 是明细。
	Found int        `json:"found"`
	Items []PortInfo `json:"items,omitempty"`
}

// NetworkScanOptions 是一次网络扫描的参数。
type NetworkScanOptions struct {
	Hosts       []string
	Ports       []int
	Timeout     time.Duration
	Concurrency int
	// SkipHostPing 强制跳过探活阶段（范围很小时由调用方决定，或用户显式要求）。
	SkipHostPing bool
}

// ScanNetwork 执行一次两阶段网络扫描。
//
// onProgress 会被周期性调用（约 300ms 一次）与最后一次完整调用，
// 传入的是快照，调用方可以安全地长期持有。
func ScanNetwork(ctx context.Context, opts NetworkScanOptions, onProgress func(ScanProgress)) []PortInfo {
	ports := opts.Ports
	if len(ports) == 0 {
		ports = CommonPorts()
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 400 * time.Millisecond
	}
	workers := opts.Concurrency
	if workers <= 0 {
		workers = 256
	}
	if workers > 512 {
		workers = 512
	}

	hosts := make([]string, 0, len(opts.Hosts))
	for _, h := range opts.Hosts {
		if v := NormalizeHostInput(h); v != "" {
			hosts = append(hosts, v)
		}
	}
	if len(hosts) == 0 {
		return nil
	}

	board := newScanBoard(len(hosts))

	// 进度上报协程：扫描过程中周期性把快照推出去。
	// 用独立的 goroutine 而不是在每个探测里回调，是为了让热路径保持无锁与廉价。
	reporterDone := make(chan struct{})
	if onProgress != nil {
		go func() {
			ticker := time.NewTicker(300 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-reporterDone:
					return
				case <-ticker.C:
					onProgress(board.snapshot())
				}
			}
		}()
	}
	finish := func() {
		if onProgress == nil {
			return
		}
		close(reporterDone)
		onProgress(board.snapshot())
	}

	// 小范围不做探活：用户明确列了几台机器时，要的就是"这些都扫一遍"，
	// 用探活把它们筛掉反而是错的。
	usePing := !opts.SkipHostPing && len(hosts) > hostPingThreshold

	alive := hosts
	if usePing {
		board.setStage("正在探测在线设备")
		board.setTotal(len(hosts) * len(hostProbePorts))
		runProbes(ctx, hosts, hostProbePorts, timeout, workers, board.record)
		alive = board.aliveHosts(hosts)
		board.resetProbes()
	}

	if ctx.Err() == nil && len(alive) > 0 {
		board.setStage(fmt.Sprintf("正在扫描 %d 台在线设备的端口", len(alive)))
		board.setTotal(len(alive) * len(ports))
		runProbes(ctx, alive, ports, timeout, workers, board.record)
	}

	board.setStage("已完成")
	finish()
	return board.items()
}

// runProbes 对 host × port 的笛卡尔积做有界并发探测，每命中一个就回调一次。
//
// 用固定数量的 worker 轮转取任务，而不是"每个目标一个 goroutine + 信号量"：
// 后者的 goroutine 数量随目标数线性增长，一个 /16 网段就能把内存吃光。
func runProbes(ctx context.Context, hosts []string, ports []int, timeout time.Duration,
	workers int, onOpen func(host string, port int)) {

	if len(hosts) == 0 || len(ports) == 0 {
		return
	}
	total := int64(len(hosts)) * int64(len(ports))
	if workers > int(total) {
		workers = int(total)
	}

	var next int64
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				if ctx.Err() != nil {
					return
				}
				i := atomic.AddInt64(&next, 1) - 1
				if i >= total {
					return
				}
				host := hosts[i/int64(len(ports))]
				port := ports[i%int64(len(ports))]
				if dialOnce(ctx, host, port, timeout) {
					onOpen(host, port)
				}
			}
		}()
	}
	wg.Wait()
}

// ---------------------------------------------------------------------------
// 进度与结果的累加器
// ---------------------------------------------------------------------------

// scanBoard 汇总扫描过程中的进度与结果。
//
// 两个计数器用 atomic（热路径上每秒被写上千次），结果集合与阶段文案用互斥锁
// （写频率低得多）。刻意分开，避免让每一次连接探测都去竞争同一把锁。
type scanBoard struct {
	probesDone  int64
	probesTotal int64

	hostsTotal int

	mu         sync.Mutex
	stage      string
	hostsAlive map[string]bool
	found      map[string]PortInfo
}

func newScanBoard(hostsTotal int) *scanBoard {
	return &scanBoard{
		hostsTotal: hostsTotal,
		stage:      "正在准备",
		hostsAlive: map[string]bool{},
		found:      map[string]PortInfo{},
	}
}

func (b *scanBoard) setStage(s string) {
	b.mu.Lock()
	b.stage = s
	b.mu.Unlock()
}

func (b *scanBoard) setTotal(n int) {
	atomic.StoreInt64(&b.probesTotal, int64(n))
	atomic.StoreInt64(&b.probesDone, 0)
}

// resetProbes 让阶段二从零开始计数（两个阶段的探测总量不同）。
func (b *scanBoard) resetProbes() {
	atomic.StoreInt64(&b.probesDone, 0)
}

// record 记下一个**探测成功**的结果。
//
// 只在连接真的建立起来时被调用，所以它同时证明了两件事：
// 这台主机在线，以及这个端口是开放的。
func (b *scanBoard) record(host string, port int) {
	atomic.AddInt64(&b.probesDone, 1)

	key := host + ":" + strconv.Itoa(port)
	b.mu.Lock()
	b.hostsAlive[host] = true
	if _, exists := b.found[key]; !exists {
		b.found[key] = PortInfo{
			Port:    port,
			Proto:   ProtoTCP,
			Address: host,
			Process: "tcp-open",
		}
	}
	b.mu.Unlock()
}

func (b *scanBoard) aliveHosts(all []string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, 0, len(all))
	for _, h := range all {
		if b.hostsAlive[h] {
			out = append(out, h)
		}
	}
	return out
}

func (b *scanBoard) snapshot() ScanProgress {
	b.mu.Lock()
	items := make([]PortInfo, 0, len(b.found))
	for _, it := range b.found {
		items = append(items, it)
	}
	sortPorts(items)
	alive := len(b.hostsAlive)
	stage := b.stage
	b.mu.Unlock()

	return ScanProgress{
		Stage:       stage,
		ProbesDone:  int(atomic.LoadInt64(&b.probesDone)),
		ProbesTotal: int(atomic.LoadInt64(&b.probesTotal)),
		HostsTotal:  b.hostsTotal,
		HostsAlive:  alive,
		Found:       len(items),
		Items:       items,
	}
}

func (b *scanBoard) items() []PortInfo {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]PortInfo, 0, len(b.found))
	for _, it := range b.found {
		out = append(out, it)
	}
	sortPorts(out)
	return out
}

// sortPorts 按「主机 → 端口」稳定排序。
func sortPorts(items []PortInfo) {
	sort.Slice(items, func(i, j int) bool {
		if items[i].Address != items[j].Address {
			return lessIP(items[i].Address, items[j].Address)
		}
		return items[i].Port < items[j].Port
	})
}

// lessIP 按数值比较点分十进制地址，否则 192.168.1.10 会排在 192.168.1.9 前面。
func lessIP(a, b string) bool {
	ai, bi := ipToUint32(net.ParseIP(a)), ipToUint32(net.ParseIP(b))
	if ai == 0 || bi == 0 {
		return a < b
	}
	return ai < bi
}

// ---------------------------------------------------------------------------
// 扫描任务
// ---------------------------------------------------------------------------

// ScanRequest 是一次扫描的请求参数，同时也是 HTTP 请求体。
type ScanRequest struct {
	// Mode 指定目标来源：auto（本机网段）/ parent（上级网段）/ custom（自定义范围）。
	Mode string `json:"mode,omitempty"`
	// Spec 是自定义范围文本，Mode=custom 时使用。
	Spec string `json:"spec,omitempty"`
	// Hosts / Subnets 是兼容用的显式目标（历史接口形态）。
	Hosts   []string `json:"hosts,omitempty"`
	Subnets []string `json:"subnets,omitempty"`
	// Ports 为空时使用内置常见端口表。
	Ports []int `json:"ports,omitempty"`
	// TimeoutMs 是单端口连接超时，默认 400ms。
	TimeoutMs int `json:"timeout_ms,omitempty"`
	// Concurrency 是并发上限，默认 256。
	Concurrency int `json:"concurrency,omitempty"`
}

// Resolve 把请求解析成待扫描主机列表与一句可展示的范围描述。
func (r ScanRequest) Resolve(info NetworkInfo) (hosts []string, label string, err error) {
	spec := strings.TrimSpace(r.Spec)
	mode := strings.ToLower(strings.TrimSpace(r.Mode))

	switch {
	case mode == "custom" || spec != "":
		if spec == "" {
			return nil, "", errors.New("请填写要扫描的范围")
		}
		list, err := ExpandRangeSpec(spec)
		if err != nil {
			return nil, "", err
		}
		return list, "自定义范围：" + spec, nil

	case mode == "parent":
		if info.Parent == "" {
			return nil, "", errors.New("没有检测到上级网段（默认网关与本机在同一网段）；请用「自定义范围」直接填写要扫描的地址")
		}
		return ExpandSubnet(info.Parent), info.Parent + ".0/24（上级网段）", nil

	case mode == "auto" || (len(r.Hosts) == 0 && len(r.Subnets) == 0):
		if info.Primary == "" {
			return nil, "", errors.New("没有检测到本机网段；请用「自定义范围」直接填写要扫描的地址")
		}
		return ExpandSubnet(info.Primary), info.Primary + ".0/24（本机网段）", nil
	}

	// 显式目标（历史形态）：hosts 优先，subnets 展开。
	var out []string
	for _, h := range r.Hosts {
		if v := NormalizeHostInput(h); v != "" {
			out = append(out, v)
		}
	}
	for _, prefix := range r.Subnets {
		p := strings.TrimSuffix(strings.TrimSpace(prefix), ".")
		if p == "" {
			continue
		}
		out = append(out, ExpandSubnet(p)...)
	}
	if len(out) == 0 {
		return nil, "", errors.New("没有可扫描的目标主机")
	}
	if len(out) > MaxScanHosts {
		out = out[:MaxScanHosts]
	}
	return out, fmt.Sprintf("指定目标（%d 台）", len(out)), nil
}

// ScanState 是一轮扫描的完整快照（含结果），也是接口的响应体。
type ScanState struct {
	ID       string `json:"id"`
	Running  bool   `json:"running"`
	Done     bool   `json:"done"`
	Canceled bool   `json:"canceled"`
	Error    string `json:"error,omitempty"`

	Stage string `json:"stage,omitempty"`
	Range string `json:"range,omitempty"`
	Mode  string `json:"mode,omitempty"`

	HostsTotal  int `json:"hosts_total"`
	HostsAlive  int `json:"hosts_alive"`
	ProbesDone  int `json:"probes_done"`
	ProbesTotal int `json:"probes_total"`

	Ports []int `json:"ports,omitempty"`

	Found     int        `json:"found"`
	Items     []PortInfo `json:"items"`
	ElapsedMs int64      `json:"elapsed_ms"`

	StartedAt  time.Time `json:"started_at,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
}

// ScanRunner 管理「最多一轮」网络扫描。
//
// 同一时刻只允许一轮：扫描是重 I/O 动作，并发跑两轮只会让两边都变慢，
// 而且结果会互相覆盖，用户看到的是"点了两次，结果乱了"。
type ScanRunner struct {
	mu    sync.Mutex
	cur   *scanRun
	last  ScanState
	now   func() time.Time
	onEnd func(ScanState)
}

type scanRun struct {
	id        string
	cancel    context.CancelFunc
	state     ScanState
	mu        sync.Mutex
	canceled  bool
	startedAt time.Time
}

// NewScanRunner 创建扫描任务管理器。onEnd 在每轮扫描结束后被调用一次
// （成功、失败、取消都算），用于向前端广播"结果已就绪"。
func NewScanRunner(onEnd func(ScanState)) *ScanRunner {
	return &ScanRunner{now: time.Now, onEnd: onEnd}
}

// Busy 报告当前是否有扫描在进行。
func (r *ScanRunner) Busy() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cur != nil
}

// Start 启动一轮扫描。
//
// 若已有扫描在跑，不报错也不排队，而是把正在跑的那一轮原样交回给调用方 ——
// 用户连点两次"开始扫描"时，想要的是看到进度，不是一个"已在扫描"的报错。
func (r *ScanRunner) Start(req ScanRequest) (ScanState, error) {
	r.mu.Lock()
	if r.cur != nil {
		snap := r.cur.snapshot()
		r.mu.Unlock()
		return snap, nil
	}

	// 每次启动都重新探测网络环境：用户很可能刚插上网线、刚切了网段。
	info := DetectNetwork()
	hosts, label, err := req.Resolve(info)
	if err != nil {
		r.mu.Unlock()
		return ScanState{}, err
	}

	ports := req.Ports
	if len(ports) == 0 {
		ports = CommonPorts()
	}
	timeout := time.Duration(req.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 400 * time.Millisecond
	}

	startedAt := r.now()
	ctx, cancel := context.WithCancel(context.Background())
	run := &scanRun{
		id:        scanID(startedAt),
		cancel:    cancel,
		startedAt: startedAt,
		state: ScanState{
			ID:         scanID(startedAt),
			Running:    true,
			Stage:      "正在准备",
			Range:      label,
			Mode:       strings.ToLower(strings.TrimSpace(req.Mode)),
			HostsTotal: len(hosts),
			Ports:      ports,
			Items:      []PortInfo{},
			StartedAt:  startedAt,
		},
	}
	r.cur = run
	snap := run.snapshot()
	r.mu.Unlock()

	go r.execute(ctx, run, hosts, ports, timeout, req.Concurrency)
	return snap, nil
}

// execute 是扫描任务的后台主体。
func (r *ScanRunner) execute(ctx context.Context, run *scanRun, hosts []string, ports []int, timeout time.Duration, concurrency int) {
	items := ScanNetwork(ctx, NetworkScanOptions{
		Hosts:       hosts,
		Ports:       ports,
		Timeout:     timeout,
		Concurrency: concurrency,
	}, func(p ScanProgress) {
		run.applyProgress(p)
	})

	finishedAt := r.now()
	final := run.finish(items, finishedAt, ctx.Err() != nil)

	r.mu.Lock()
	if r.cur == run {
		r.cur = nil
	}
	r.last = final
	cb := r.onEnd
	r.mu.Unlock()

	if cb != nil {
		cb(final)
	}
}

// Snapshot 返回当前（或最近一次）扫描的状态。
func (r *ScanRunner) Snapshot() ScanState {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cur != nil {
		return r.cur.snapshot()
	}
	if r.last.ID == "" {
		return ScanState{Stage: "空闲", Items: []PortInfo{}}
	}
	return cloneState(r.last)
}

// Cancel 请求停止当前扫描。
func (r *ScanRunner) Cancel() bool {
	r.mu.Lock()
	run := r.cur
	r.mu.Unlock()
	if run == nil {
		return false
	}
	run.markCanceled()
	run.cancel()
	return true
}

// Wait 阻塞等待当前扫描结束（测试与关闭流程使用）。
func (r *ScanRunner) Wait(d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !r.Busy() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return !r.Busy()
}

/* ---------------------------------------------------------------- 内部状态 */

func (run *scanRun) snapshot() ScanState {
	run.mu.Lock()
	defer run.mu.Unlock()
	return cloneState(run.state)
}

// applyProgress 把进度回调写进任务状态。
func (run *scanRun) applyProgress(p ScanProgress) {
	run.mu.Lock()
	defer run.mu.Unlock()
	if p.Stage != "" {
		run.state.Stage = p.Stage
	}
	run.state.ProbesDone = p.ProbesDone
	run.state.ProbesTotal = p.ProbesTotal
	run.state.HostsAlive = p.HostsAlive
	if p.Items != nil {
		run.state.Items = p.Items
	}
	run.state.Found = p.Found
}

// finish 落终态。
func (run *scanRun) finish(items []PortInfo, at time.Time, canceled bool) ScanState {
	run.mu.Lock()
	defer run.mu.Unlock()

	run.state.Running = false
	run.state.Done = true
	run.state.FinishedAt = at
	run.state.ElapsedMs = at.Sub(run.startedAt).Milliseconds()
	run.state.Items = items
	if items == nil {
		run.state.Items = []PortInfo{}
	}
	run.state.Found = len(run.state.Items)
	if canceled || run.canceled {
		run.state.Canceled = true
		run.state.Stage = "已取消"
	} else {
		run.state.Stage = "已完成"
	}
	return cloneState(run.state)
}

func (run *scanRun) markCanceled() {
	run.mu.Lock()
	run.canceled = true
	run.mu.Unlock()
}

// cloneState 复制状态，避免调用方拿到内部切片。
func cloneState(s ScanState) ScanState {
	out := s
	out.Items = append([]PortInfo(nil), s.Items...)
	if out.Items == nil {
		out.Items = []PortInfo{}
	}
	out.Ports = append([]int(nil), s.Ports...)
	return out
}

func scanID(t time.Time) string {
	return fmt.Sprintf("scan-%d", t.UnixNano()/int64(time.Millisecond))
}
