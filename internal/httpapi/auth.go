package httpapi

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/MisiteQ/qlink2desktop/internal/auth"
)

// ---------------------------------------------------------------------------
// 登录限流
// ---------------------------------------------------------------------------

// 限流参数。
//
// 取「连续 5 次失败后锁定 5 分钟」：正常用户手滑打错两三次完全不受影响，
// 而自动化爆破会被迅速摁住。锁定是按来源 IP 计数的，
// 家用场景下不会出现「锁了别人」的问题。
const (
	loginMaxFailures = 5
	loginLockout     = 5 * time.Minute
)

type loginLimiter struct {
	mu       sync.Mutex
	failures map[string]int
	lockedAt map[string]time.Time
	now      func() time.Time
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{
		failures: make(map[string]int),
		lockedAt: make(map[string]time.Time),
		now:      time.Now,
	}
}

// allow 报告该来源当前是否允许尝试登录；被锁定时返回剩余等待时长。
func (l *loginLimiter) allow(ip string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	until, locked := l.lockedAt[ip]
	if !locked {
		return true, 0
	}
	remaining := until.Sub(l.now())
	if remaining <= 0 {
		delete(l.lockedAt, ip)
		delete(l.failures, ip)
		return true, 0
	}
	return false, remaining.Truncate(time.Second)
}

// fail 记录一次失败，达到阈值则锁定。
func (l *loginLimiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.failures[ip]++
	if l.failures[ip] >= loginMaxFailures {
		l.lockedAt[ip] = l.now().Add(loginLockout)
		l.failures[ip] = 0
	}
}

// reset 在登录成功后清空计数。
func (l *loginLimiter) reset(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.failures, ip)
	delete(l.lockedAt, ip)
}

// ---------------------------------------------------------------------------
// 处理器
// ---------------------------------------------------------------------------

// authStatus 是登录状态的响应。
type authStatus struct {
	// Protected 表示当前是否启用了口令保护。
	Protected bool `json:"protected"`
	// LoggedIn 表示本次请求是否已被授权（未启用口令时恒为 true）。
	LoggedIn bool `json:"logged_in"`
	// ExpiresAt 是当前会话的过期时间。
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	// TTLSeconds 是会话有效期，前端可据此提示用户。
	TTLSeconds int `json:"ttl_seconds,omitempty"`
}

func (s *Server) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	settings := s.deps.Store.Settings()
	protected := settings.HasPassword()

	resp := authStatus{Protected: protected, LoggedIn: !protected}
	if protected && s.sessionValid(r) {
		resp.LoggedIn = true
	}
	if s.deps.Sessions != nil {
		ttl := s.deps.Sessions.TTL()
		resp.TTLSeconds = int(ttl.Seconds())
		exp := time.Now().Add(ttl)
		resp.ExpiresAt = &exp
	}
	ok(w, resp)
}

// loginRequest 是登录请求体。
type loginRequest struct {
	Password string `json:"password"`
}

func (s *Server) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	settings := s.deps.Store.Settings()

	// 未设置口令：直接视为已登录，前端据此跳过登录页。
	if !settings.HasPassword() {
		ok(w, authStatus{Protected: false, LoggedIn: true})
		return
	}
	if s.deps.Sessions == nil {
		writeError(w, fmt.Errorf("会话子系统未初始化"))
		return
	}

	ip := clientIP(r)
	if allowed, wait := s.limiter.allow(ip); !allowed {
		writeJSON(w, http.StatusTooManyRequests, errorBody{
			Error: fmt.Sprintf("尝试次数过多，请 %s 后重试", wait),
			Kind:  "rate_limited",
		})
		return
	}

	var req loginRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	if !auth.Verify(req.Password, settings.AuthSalt, settings.AuthHash) {
		s.limiter.fail(ip)
		s.deps.Logger.Warn("登录失败", "ip", ip)
		// 刻意不区分「口令为空」与「口令错误」，避免泄露信息。
		writeError(w, fmt.Errorf("%w: 口令不正确", errUnauthorized))
		return
	}

	s.limiter.reset(ip)
	sess := s.deps.Sessions.Issue()
	if sess.Token == "" {
		writeError(w, fmt.Errorf("生成会话令牌失败"))
		return
	}
	s.setSessionCookie(w, r, sess)

	s.deps.Logger.Info("登录成功", "ip", ip, "expires_at", sess.ExpiresAt.Format(time.RFC3339))
	ok(w, authStatus{Protected: true, LoggedIn: true, ExpiresAt: &sess.ExpiresAt})
}

func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	if s.deps.Sessions != nil {
		if token := extractToken(r); token != "" {
			s.deps.Sessions.Revoke(token)
		}
	}
	s.clearSessionCookie(w, r)
	ok(w, map[string]bool{"ok": true})
}

// setSessionCookie 依据当前连接是否为 HTTPS 决定 Secure 标志。
//
// 不能无条件加 Secure：局域网内用户直接用 http 访问时，
// 带 Secure 的 Cookie 会被浏览器直接丢弃，表现为「登录后立刻又跳回登录页」。
func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, sess auth.Session) {
	http.SetCookie(w, &http.Cookie{
		Name:     tokenCookie,
		Value:    sess.Token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isSecureRequest(r),
		Expires:  sess.ExpiresAt,
		MaxAge:   int(time.Until(sess.ExpiresAt).Seconds()),
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     tokenCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isSecureRequest(r),
		MaxAge:   -1,
	})
}

// isSecureRequest 判断浏览器到本服务的这一段是否为 HTTPS。
//
// 飞牛 Connect 外网访问时，网关以 https 面向浏览器、以 http 回源到本进程，
// 因此必须同时看 X-Forwarded-Proto。
func isSecureRequest(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	proto := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")))
	return proto == "https"
}
