// Package logx 提供本项目的日志装配：结构化输出 + 大小轮转 + 内存环形缓冲。
//
// 为什么不用第三方日志库：
//   - 本项目坚持单二进制零外部依赖，便于分发到任意 NAS；
//   - 真实需求只有三点——同时写文件与控制台、按大小轮转、能被 HTTP 接口读到尾部。
//     这三件事用标准库 log/slog 加两个小类型就够，引入一个日志框架反而是负担。
package logx

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// 环形缓冲
// ---------------------------------------------------------------------------

// Ring 是一段固定容量的内存日志缓冲。
//
// 它同时解决两个需求：
//  1. `/api/logs` 需要在不读磁盘的前提下回显最近的日志（NAS 上日志文件可能已经轮转走）；
//  2. `/api/events` 的 SSE 需要把新日志实时推给已连接的浏览器。
type Ring struct {
	mu     sync.Mutex
	lines  []string
	start  int // 环形起点
	filled int // 已写入的条目数（可能大于容量）
	size   int

	subs   map[int]chan string
	nextID int
}

// NewRing 创建容量为 size 条目的环形缓冲。
func NewRing(size int) *Ring {
	if size <= 0 {
		size = 500
	}
	return &Ring{
		lines: make([]string, size),
		size:  size,
		subs:  make(map[int]chan string),
	}
}

// Append 写入一条日志；写入失败（订阅者阻塞）时会丢弃该订阅者的一条消息，
// 而不是阻塞整个日志链路——日志永远不该成为业务路径上的阻塞点。
func (r *Ring) Append(line string) {
	r.mu.Lock()
	r.lines[(r.start+r.filled)%r.size] = line
	if r.filled < r.size {
		r.filled++
	} else {
		r.start = (r.start + 1) % r.size
	}
	subs := make([]chan string, 0, len(r.subs))
	for _, ch := range r.subs {
		subs = append(subs, ch)
	}
	r.mu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- line:
		default:
			// 订阅者落后太多：丢弃本条，保证日志写入永不阻塞。
		}
	}
}

// Tail 返回最近 n 条日志（时间正序）。
func (r *Ring) Tail(n int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	if n <= 0 || n > r.filled {
		n = r.filled
	}
	out := make([]string, 0, n)
	for i := r.filled - n; i < r.filled; i++ {
		line := r.lines[(r.start+i)%r.size]
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// Subscribe 注册一个实时订阅，返回订阅 ID 与只读通道。
func (r *Ring) Subscribe(buffer int) (int, <-chan string) {
	if buffer <= 0 {
		buffer = 128
	}
	ch := make(chan string, buffer)

	r.mu.Lock()
	id := r.nextID
	r.nextID++
	r.subs[id] = ch
	r.mu.Unlock()

	return id, ch
}

// Unsubscribe 注销订阅并关闭通道。
func (r *Ring) Unsubscribe(id int) {
	r.mu.Lock()
	ch, ok := r.subs[id]
	delete(r.subs, id)
	r.mu.Unlock()
	if ok {
		close(ch)
	}
}

// Len 返回当前缓冲里的条目数。
func (r *Ring) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.filled
}

// ---------------------------------------------------------------------------
// 轮转写入器
// ---------------------------------------------------------------------------

// rotatingWriter 是一个按大小轮转的 io.Writer。
//
// 轮转策略沿用经典的 logrotate 命名：当前文件 + .1/.2/... 归档，
// 序号越大越旧，超出保留数量直接删除。这样运维同学不需要额外学习成本。
type rotatingWriter struct {
	mu         sync.Mutex
	dir        string
	name       string
	maxSize    int64
	maxBackups int

	file *os.File
	size int64
}

func newRotatingWriter(dir, name string, maxSize int64, maxBackups int) (*rotatingWriter, error) {
	if maxSize <= 0 {
		maxSize = 4 << 20 // 4MB
	}
	if maxBackups < 1 {
		maxBackups = 3
	}
	w := &rotatingWriter{
		dir:        dir,
		name:       name,
		maxSize:    maxSize,
		maxBackups: maxBackups,
	}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *rotatingWriter) path() string { return filepath.Join(w.dir, w.name) }

func (w *rotatingWriter) open() error {
	if err := os.MkdirAll(w.dir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(w.path(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	w.file = f
	if fi, err := f.Stat(); err == nil {
		w.size = fi.Size()
	}
	return nil
}

func (w *rotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.file == nil {
		if err := w.open(); err != nil {
			return 0, err
		}
	}
	if w.size+int64(len(p)) > w.maxSize {
		if err := w.rotateLocked(); err != nil {
			// 轮转失败不应丢日志：继续写当前文件即可。
			_ = err
		}
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *rotatingWriter) rotateLocked() error {
	if w.file != nil {
		_ = w.file.Sync()
		_ = w.file.Close()
		w.file = nil
	}

	// 归档向后平移：.2 → .3，.1 → .2。
	for i := w.maxBackups - 1; i >= 1; i-- {
		old := fmt.Sprintf("%s.%d", w.path(), i)
		next := fmt.Sprintf("%s.%d", w.path(), i+1)
		if _, err := os.Stat(old); err != nil {
			continue
		}
		_ = os.Remove(next)
		_ = os.Rename(old, next)
	}
	_ = os.Remove(fmt.Sprintf("%s.%d", w.path(), w.maxBackups))
	_ = os.Rename(w.path(), w.path()+".1")

	w.size = 0
	return w.open()
}

// Close 关闭底层文件。
func (w *rotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

// Files 返回该轮转器涉及的全部文件路径（当前文件 + 归档），供接口列举下载。
func (w *rotatingWriter) Files() []string {
	w.mu.Lock()
	defer w.mu.Unlock()

	out := []string{w.path()}
	for i := 1; i <= w.maxBackups; i++ {
		p := fmt.Sprintf("%s.%d", w.path(), i)
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// slog 处理器
// ---------------------------------------------------------------------------

// textHandler 把日志渲染成「时间 级别 消息 key=value…」的单行文本，
// 同时分发给文件与控制台，并镜像一份到内存环形缓冲。
//
// 刻意不直接用 slog.TextHandler：它按 key=value 输出但不会注入到 Ring，
// 而 Ring 正是 /api/logs 与 SSE 的数据源。
type textHandler struct {
	level   slog.Leveler
	writers []io.Writer
	ring    *Ring

	mu     *sync.Mutex
	attrs  []slog.Attr
	groups []string
}

func newTextHandler(level slog.Leveler, ring *Ring, writers ...io.Writer) *textHandler {
	return &textHandler{
		level:   level,
		writers: writers,
		ring:    ring,
		mu:      &sync.Mutex{},
	}
}

func (h *textHandler) Enabled(_ context.Context, l slog.Level) bool {
	return l >= h.level.Level()
}

func (h *textHandler) Handle(_ context.Context, rec slog.Record) error {
	var b strings.Builder
	b.WriteString(rec.Time.Format("2006-01-02 15:04:05.000"))
	b.WriteByte(' ')
	b.WriteString(levelTag(rec.Level))
	b.WriteByte(' ')
	b.WriteString(rec.Message)

	writeAttr := func(a slog.Attr) {
		if a.Key == "" {
			return
		}
		b.WriteByte(' ')
		b.WriteString(strings.Join(h.groups, "."))
		if len(h.groups) > 0 {
			b.WriteByte('.')
		}
		b.WriteString(a.Key)
		b.WriteByte('=')
		b.WriteString(formatValue(a.Value))
	}

	for _, a := range h.attrs {
		writeAttr(a)
	}
	rec.Attrs(func(a slog.Attr) bool {
		writeAttr(a)
		return true
	})

	line := b.String() + "\n"

	h.mu.Lock()
	for _, w := range h.writers {
		if w == nil {
			continue
		}
		_, _ = io.WriteString(w, line)
	}
	h.mu.Unlock()

	if h.ring != nil {
		h.ring.Append(strings.TrimRight(line, "\n"))
	}
	return nil
}

func (h *textHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	return &textHandler{
		level:   h.level,
		writers: h.writers,
		ring:    h.ring,
		mu:      h.mu,
		attrs:   append(append([]slog.Attr{}, h.attrs...), attrs...),
		groups:  h.groups,
	}
}

func (h *textHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return &textHandler{
		level:   h.level,
		writers: h.writers,
		ring:    h.ring,
		mu:      h.mu,
		attrs:   h.attrs,
		groups:  append(append([]string{}, h.groups...), name),
	}
}

func levelTag(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "ERROR"
	case l >= slog.LevelWarn:
		return "WARN "
	case l >= slog.LevelInfo:
		return "INFO "
	default:
		return "DEBUG"
	}
}

func formatValue(v slog.Value) string {
	v = v.Resolve()
	switch v.Kind() {
	case slog.KindString:
		s := v.String()
		if strings.ContainsAny(s, " \t\"") {
			return fmt.Sprintf("%q", s)
		}
		return s
	case slog.KindTime:
		return v.Time().Format(time.RFC3339)
	case slog.KindDuration:
		return v.Duration().String()
	default:
		return fmt.Sprint(v.Any())
	}
}

// ---------------------------------------------------------------------------
// 对外装配
// ---------------------------------------------------------------------------

// Options 是日志子系统的装配参数。
type Options struct {
	// Dir 为日志目录；留空则只写控制台不落盘。
	Dir string
	// FileName 是日志文件名，默认 qlink2desktop.log。
	FileName string
	// MaxSizeMB 是单文件大小上限（MB），默认 4。
	MaxSizeMB int
	// MaxBackups 是保留的归档数量，默认 3。
	MaxBackups int
	// Level 是日志级别，默认 Info。
	Level slog.Level
	// Ring 为 nil 时内部自动创建一个 500 条容量的环形缓冲。
	Ring *Ring
	// Console 控制是否输出到标准错误，默认 true。
	Console bool
	// ConsoleWriter 可覆盖控制台输出目标（测试用）。
	ConsoleWriter io.Writer
}

// Results 是装配结果。
type Results struct {
	Logger  *slog.Logger
	Ring    *Ring
	Dir     string
	Files   func() []string
	closers []io.Closer
}

// Setup 装配日志子系统。
//
// 即使日志目录不可写也不会返回错误：日志写不进去不该让整个服务起不来，
// 调用方拿到的仍是一个可用的 Logger（只是不落盘）。
func Setup(opts Options) *Results {
	ring := opts.Ring
	if ring == nil {
		ring = NewRing(500)
	}

	level := opts.Level
	var writers []io.Writer

	console := opts.Console || opts.ConsoleWriter != nil
	if console {
		w := opts.ConsoleWriter
		if w == nil {
			w = os.Stderr
		}
		writers = append(writers, w)
	}
	// 没指定任何输出时至少保证有一个，否则日志会凭空消失。
	if len(writers) == 0 {
		writers = append(writers, os.Stderr)
	}

	res := &Results{Ring: ring, Dir: opts.Dir}
	res.Files = func() []string { return nil }

	if strings.TrimSpace(opts.Dir) != "" {
		name := opts.FileName
		if name == "" {
			name = "qlink2desktop.log"
		}
		rw, err := newRotatingWriter(opts.Dir, name, int64(opts.MaxSizeMB)<<20, opts.MaxBackups)
		if err != nil {
			// 降级为仅控制台，但要把原因说出来，否则用户会以为「日志功能坏了」。
			_, _ = fmt.Fprintf(writers[0], "警告：日志文件不可用（%v），已降级为仅控制台输出\n", err)
		} else {
			writers = append(writers, rw)
			res.closers = append(res.closers, rw)
			res.Files = rw.Files
			res.Dir = opts.Dir
		}
	}

	handler := newTextHandler(level, ring, writers...)
	res.Logger = slog.New(handler)
	return res
}

// Close 关闭全部文件句柄。
func (r *Results) Close() error {
	var first error
	for _, c := range r.closers {
		if err := c.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// ListLogFiles 列出目录下的日志文件（按修改时间倒序）。
func ListLogFiles(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	type item struct {
		path string
		mod  time.Time
	}
	var items []item
	for _, e := range entries {
		if e.IsDir() || !strings.Contains(e.Name(), ".log") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		items = append(items, item{filepath.Join(dir, e.Name()), info.ModTime()})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].mod.After(items[j].mod) })
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.path)
	}
	return out
}
