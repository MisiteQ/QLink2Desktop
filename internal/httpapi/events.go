package httpapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// Event 是推送给前端的实时事件。
//
// 统一成「类型 + 任意负载」的信封，前端用一个 switch 就能分发；
// 每加一种通知都不需要改传输层。
type Event struct {
	Type string    `json:"type"`
	At   time.Time `json:"at"`
	Data any       `json:"data,omitempty"`
}

// 事件类型常量。前端的 EventSource 监听同名事件。
const (
	// EventSnapshot：连接建立时推送的初始全量快照。
	EventSnapshot = "snapshot"
	// EventLinksChanged：链接定义有增删改，前端应重新拉取列表。
	EventLinksChanged = "links"
	// EventStatusChanged：运行态发生变化（安装进度、失败原因等）。
	EventStatusChanged = "status"
	// EventLog：一条运行日志。
	EventLog = "log"
	// EventDiscovery：端口 / 容器发现结果更新。
	EventDiscovery = "discovery"
)

// hub 是最小可用的发布订阅中心。
//
// 刻意不做「按主题过滤」：订阅者数量等于当前打开的浏览器标签数（个位数），
// 全量广播的成本可以忽略，而过滤逻辑会带来状态同步问题。
type hub struct {
	mu     sync.Mutex
	subs   map[int]chan Event
	nextID int
	closed bool
}

func newHub() *hub {
	return &hub{subs: make(map[int]chan Event)}
}

// publish 向所有订阅者广播事件。
//
// 订阅者通道满时丢弃该事件而不是阻塞：一个卡住的浏览器标签
// 绝不能拖慢后端的安装流程。
func (h *hub) publish(typ string, data any) {
	ev := Event{Type: typ, At: time.Now(), Data: data}

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	chans := make([]chan Event, 0, len(h.subs))
	for _, ch := range h.subs {
		chans = append(chans, ch)
	}
	h.mu.Unlock()

	for _, ch := range chans {
		select {
		case ch <- ev:
		default:
		}
	}
}

// subscribe 注册订阅者。
func (h *hub) subscribe(buffer int) (int, <-chan Event) {
	if buffer <= 0 {
		buffer = 64
	}
	ch := make(chan Event, buffer)

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		close(ch)
		return -1, ch
	}
	id := h.nextID
	h.nextID++
	h.subs[id] = ch
	return id, ch
}

// unsubscribe 注销订阅者并关闭通道。
func (h *hub) unsubscribe(id int) {
	h.mu.Lock()
	ch, found := h.subs[id]
	delete(h.subs, id)
	h.mu.Unlock()
	if found {
		close(ch)
	}
}

// count 返回当前订阅者数量（诊断用）。
func (h *hub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// close 关闭全部订阅。
func (h *hub) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	for id, ch := range h.subs {
		close(ch)
		delete(h.subs, id)
	}
}

// ---------------------------------------------------------------------------
// SSE 处理器
// ---------------------------------------------------------------------------

// heartbeatInterval 是 SSE 心跳间隔。
//
// 中间层（飞牛网关、各类反代、家用路由）普遍会掐掉长时间无数据的连接，
// 20 秒一条注释行足以让连接长活。
const heartbeatInterval = 20 * time.Second

// handleEvents 是 Server-Sent Events 端点。
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, canFlush := w.(http.Flusher)
	if !canFlush {
		writeError(w, fmt.Errorf("当前环境不支持流式响应"))
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	// 关掉可能存在的反代缓冲，否则事件会被攒到连接关闭才吐出来。
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	id, events := s.hub.subscribe(64)
	if id < 0 {
		return
	}
	defer s.hub.unsubscribe(id)

	// 连接建立后第一件事是推送全量快照，前端因此不需要额外发一次 GET。
	writeSSE(w, Event{Type: EventSnapshot, At: time.Now(), Data: s.snapshot()})
	flusher.Flush()

	// 日志订阅：把运行日志实时镜像到浏览器控制台。
	var logID int = -1
	var logCh <-chan string
	if s.deps.Logs != nil {
		logID, logCh = s.deps.Logs.Subscribe(128)
		defer s.deps.Logs.Unsubscribe(logID)
	}

	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return

		case ev, open := <-events:
			if !open {
				return
			}
			writeSSE(w, ev)
			flusher.Flush()

		case line, open := <-logCh:
			if !open {
				logCh = nil
				continue
			}
			writeSSE(w, Event{Type: EventLog, At: time.Now(), Data: line})
			flusher.Flush()

		case <-ticker.C:
			// 注释行是 SSE 规范里的合法心跳，客户端会忽略其内容但会重置超时。
			_, _ = io.WriteString(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

// writeSSE 按 SSE 线格式写出一个事件。
func writeSSE(w io.Writer, ev Event) {
	payload, err := json.Marshal(ev)
	if err != nil {
		return
	}
	// json.Marshal 会把换行转义成 \n，因此 data 永远是单行，不会破坏 SSE 帧结构。
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, payload)
}

// broadcast 是给内部各处理器用的广播快捷方式（nil 安全）。
func (s *Server) broadcast(typ string, data any) {
	if s.hub != nil {
		s.hub.publish(typ, data)
	}
}
