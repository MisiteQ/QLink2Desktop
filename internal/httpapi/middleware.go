package httpapi

import (
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"
)

// errUnauthorized 表示请求未通过身份校验。
var errUnauthorized = errors.New("未登录或会话已过期，请重新登录")

// tokenCookie 是会话令牌的 Cookie 名。
const tokenCookie = "qlink_token"

// ---------------------------------------------------------------------------
// 安全响应头
// ---------------------------------------------------------------------------

// contentSecurityPolicy 是门户自身的 CSP。
//
// 关键点是 frame-ancestors：
// 门户页面会被飞牛桌面以 iframe 的方式嵌入，而桌面域常常与本服务不同源
// （尤其是经过飞牛 Connect 外网访问时）。这里必须放开 frame-ancestors，
// 否则用户点开桌面图标只会看到一片空白。
//
// frame-src 同样放开：面板里允许用户把任意后端服务嵌进预览框。
const contentSecurityPolicy = "default-src 'self'; " +
	"img-src 'self' data: blob: http: https:; " +
	"style-src 'self' 'unsafe-inline'; " +
	"script-src 'self' 'unsafe-inline'; " +
	"connect-src 'self'; " +
	"frame-src *; " +
	"frame-ancestors *; " +
	"object-src 'none'; " +
	"base-uri 'self'"

func withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		// 关闭老浏览器上会误伤正常页面的 XSS 过滤器：现代浏览器已忽略该头。
		h.Set("X-Xss-Protection", "0")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------------
// 崩溃兜底
// ---------------------------------------------------------------------------

// withRecover 兜住处理器里的 panic。
//
// 一个请求的 bug 不该让整个面板进程退出——那样用户会连"看日志"的入口都没有了。
func withRecover(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					logger.Error("接口发生崩溃，已恢复",
						"method", r.Method,
						"path", r.URL.Path,
						"panic", rec,
						"stack", string(debug.Stack()))
					writeJSON(w, http.StatusInternalServerError, errorBody{
						Error: "服务内部错误，请查看日志",
						Kind:  "panic",
					})
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// ---------------------------------------------------------------------------
// 访问日志
// ---------------------------------------------------------------------------

// statusRecorder 记录实际写出的状态码与字节数。
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(p []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(p)
	s.bytes += n
	return n, err
}

// Flush 透传，保证 SSE 能真正把数据推出去。
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap 让 http.ResponseController 能拿到原始 ResponseWriter。
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func withRequestLog(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// 静态资源与健康检查不值得写入日志，否则会把真正有用的记录淹掉。
			if isQuietPath(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}

			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w}
			next.ServeHTTP(rec, r)

			level := slog.LevelInfo
			switch {
			case rec.status >= 500:
				level = slog.LevelError
			case rec.status >= 400:
				level = slog.LevelWarn
			}
			logger.Log(r.Context(), level, "请求完成",
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status,
				"bytes", rec.bytes,
				"duration", time.Since(start).Round(time.Millisecond).String(),
				"ip", clientIP(r),
			)
		})
	}
}

// isQuietPath 报告该路径是否跳过访问日志。
func isQuietPath(p string) bool {
	switch {
	case p == "/api/health":
		return true
	case strings.HasPrefix(p, "/icons/"):
		return true
	case strings.HasPrefix(p, "/assets/"):
		return true
	default:
		return false
	}
}

// ---------------------------------------------------------------------------
// 身份校验
// ---------------------------------------------------------------------------

// requireAuth 在「已设置口令」时校验会话。
//
// 未设置口令时直接放行：这是家用 NAS 的常见使用方式，
// 强制登录反而会把用户拦在自己家里。面板会显著提示"当前无口令保护"。
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.authEnabled() {
			next.ServeHTTP(w, r)
			return
		}
		if s.sessionValid(r) {
			next.ServeHTTP(w, r)
			return
		}
		writeError(w, errUnauthorized)
	})
}

// authEnabled 报告当前是否启用了口令保护。
func (s *Server) authEnabled() bool {
	return s.deps.Store.Settings().HasPassword()
}

// sessionValid 从 Cookie / Authorization 头 / 查询参数中取出令牌并校验。
//
// 之所以三种都支持：普通请求用 Cookie；脚本化调用（curl）用 Bearer；
// SSE 因为浏览器的 EventSource 不能自定义请求头，只能用查询参数兜底。
func (s *Server) sessionValid(r *http.Request) bool {
	if s.deps.Sessions == nil {
		return false
	}
	token := extractToken(r)
	if token == "" {
		return false
	}
	return s.deps.Sessions.Valid(token)
}

func extractToken(r *http.Request) string {
	if c, err := r.Cookie(tokenCookie); err == nil && c.Value != "" {
		return c.Value
	}
	if h := r.Header.Get("Authorization"); h != "" {
		if after, found := strings.CutPrefix(h, "Bearer "); found {
			return strings.TrimSpace(after)
		}
	}
	if s := r.Header.Get("X-Auth-Token"); s != "" {
		return strings.TrimSpace(s)
	}
	return strings.TrimSpace(r.URL.Query().Get("token"))
}
