package forward

import (
	"sync"
	"sync/atomic"
	"time"
)

// Counters 是一条转发规则的累计计数。
//
// 全部用原子量：它们被每个连接的收发 goroutine 并发更新，
// 而查询（接口层读状态、前端看统计）来自另一个 goroutine。
// 常见的竞态写法是「读写各一把锁」，但这里是典型的读少写多，
// 原子量既免锁又不会把热路径串行化。
//
// 字段类型是 atomic.Int64 而不是 int64，这样意图就写在类型上：
// 任何想直接读写它的代码都会立刻被编译器或 go vet 拦住，
// 不会有人"忘了加锁"。
type Counters struct {
	// Active 是当前活跃的连接 / 会话数。
	Active atomic.Int64
	// Total 是历史累计的连接 / 会话数。
	Total atomic.Int64
	// BytesIn / BytesOut 按**转发方向**计数：
	// In 是从外部收到、要送给目标的字节；Out 是从目标回来、要送回外部的字节。
	BytesIn  atomic.Int64
	BytesOut atomic.Int64
	// Denied 是被访问控制拒绝的次数。
	Denied atomic.Int64
	// Failed 是连接目标失败的次数（目标不可达、超时）。
	Failed atomic.Int64
	// Limited 是因并发上限被拒的次数。它是配置问题的信号：
	// 一旦这个数在涨，说明规则填的上限偏小，或者正在被扫描。
	Limited atomic.Int64
}

// snapshot 取一份计数快照。
func (c *Counters) snapshot() CounterSnapshot {
	return CounterSnapshot{
		Active:   c.Active.Load(),
		Total:    c.Total.Load(),
		BytesIn:  c.BytesIn.Load(),
		BytesOut: c.BytesOut.Load(),
		Denied:   c.Denied.Load(),
		Failed:   c.Failed.Load(),
		Limited:  c.Limited.Load(),
	}
}

// CounterSnapshot 是计数的只读快照，供接口层序列化。
type CounterSnapshot struct {
	Active   int64 `json:"active"`
	Total    int64 `json:"total"`
	BytesIn  int64 `json:"bytes_in"`
	BytesOut int64 `json:"bytes_out"`
	Denied   int64 `json:"denied"`
	Failed   int64 `json:"failed"`
	Limited  int64 `json:"limited"`
}

// Outcome 是一次连接尝试的结果。
type Outcome string

const (
	// OutcomeOK：正常转发完成。
	OutcomeOK Outcome = "ok"
	// OutcomeDenied：被访问控制拒绝。
	OutcomeDenied Outcome = "denied"
	// OutcomeDialFailed：连不上目标。
	OutcomeDialFailed Outcome = "dial_failed"
	// OutcomeLimited：并发已达上限。
	OutcomeLimited Outcome = "limited"
)

// Chinese 返回面向用户的结果描述。
func (o Outcome) Chinese() string {
	switch o {
	case OutcomeOK:
		return "已完成"
	case OutcomeDenied:
		return "已拒绝"
	case OutcomeDialFailed:
		return "目标不可达"
	case OutcomeLimited:
		return "已达上限"
	default:
		return string(o)
	}
}

// ConnLog 是一条连接记录。
type ConnLog struct {
	At         time.Time `json:"at"`
	Protocol   string    `json:"protocol"`
	Source     string    `json:"source"`
	Target     string    `json:"target"`
	Outcome    Outcome   `json:"outcome"`
	BytesIn    int64     `json:"bytes_in"`
	BytesOut   int64     `json:"bytes_out"`
	DurationMs int64     `json:"duration_ms"`
	Err        string    `json:"error,omitempty"`
}

// connRing 是最近连接记录的环形缓冲。
//
// 用固定容量的环形缓冲而不是无界切片：一条配置错误的规则每秒可能产生
// 成百上千条记录，无界增长会在几分钟内吃光内存。
// "最近 N 条"也正是排查时需要的信息——没有人会去看一小时前的连接记录。
type connRing struct {
	mu      sync.Mutex
	cap     int
	entries []ConnLog
	next    int
}

const defaultLogCap = 100

func newConnRing(capacity int) *connRing {
	if capacity <= 0 {
		capacity = defaultLogCap
	}
	return &connRing{cap: capacity, entries: make([]ConnLog, 0, capacity)}
}

// add 追加一条记录，超出容量时覆盖最旧的一条。
func (r *connRing) add(e ConnLog) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.entries) < r.cap {
		r.entries = append(r.entries, e)
		return
	}
	r.entries[r.next] = e
	r.next = (r.next + 1) % r.cap
}

// tail 按时间从旧到新返回最近 n 条。
func (r *connRing) tail(n int) []ConnLog {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n <= 0 || n > len(r.entries) {
		n = len(r.entries)
	}
	// 环形缓冲里第 next 个位置是最旧的，从它开始顺时针取。
	out := make([]ConnLog, 0, n)
	start := 0
	if len(r.entries) == r.cap {
		start = r.next
	}
	for i := 0; i < n; i++ {
		out = append(out, r.entries[(start+i)%len(r.entries)])
	}
	return out
}

// clear 清空记录（规则被删除或重启时调用）。
func (r *connRing) clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = r.entries[:0]
	r.next = 0
}
