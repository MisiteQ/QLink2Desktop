// Package auth 提供访问口令的哈希存储与短期会话管理。
//
// 设计取舍：
//   - 口令**绝不**明文落盘，只保存 salt 与派生摘要；
//   - 派生用「多轮 sha256」而非单轮，抬高离线爆破成本，同时不引入任何外部依赖
//     （golang.org/x/crypto/pbkdf2 会把依赖树带进单二进制分发形态里，不值得）；
//   - 比较一律走 subtle.ConstantTimeCompare，避免用耗时差异反推口令；
//   - 会话只存在于内存：进程重启即全部失效，不需要考虑会话文件的权限与清理问题。
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"sync"
	"time"
)

// saltBytes 是随机盐的长度（字节）。
const saltBytes = 16

// tokenBytes 是会话令牌的随机字节数。
const tokenBytes = 32

// deriveIterations 是口令派生的迭代轮数。
//
// 取 5 万轮：在 NAS 这种低功耗 CPU 上约几十毫秒，登录感知不到，
// 但把离线爆破的成本抬高了五个数量级。
const deriveIterations = 50000

// NewSalt 生成一段随机的十六进制盐。
func NewSalt() string {
	buf := make([]byte, saltBytes)
	if _, err := rand.Read(buf); err != nil {
		// rand.Read 在现代 Go 上不会失败；万一失败，宁可返回空串让上层拒绝设置口令，
		// 也好过用可预测的盐悄悄降低安全性。
		return ""
	}
	return hex.EncodeToString(buf)
}

// Hash 用 salt 派生出口令摘要（十六进制）。
//
// 注意：salt 为空时返回空串，调用方据此判定「未设置口令」。
func Hash(plain, salt string) string {
	if salt == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(salt + "\x00" + plain))
	cur := sum[:]

	// 多轮拉伸。每轮把「上一轮摘要 + 盐 + 口令」再哈希一次，
	// 使计算无法被预计算表加速。
	buf := make([]byte, 0, len(cur)+len(salt)+len(plain))
	for i := 0; i < deriveIterations; i++ {
		buf = buf[:0]
		buf = append(buf, cur...)
		buf = append(buf, salt...)
		buf = append(buf, plain...)
		next := sha256.Sum256(buf)
		cur = next[:]
	}
	return hex.EncodeToString(cur)
}

// Verify 判断明文口令是否与摘要匹配。
//
// 使用常量时间比较：普通的 == 会在首个不同字节处提前返回，
// 足够多的采样就能把摘要逐字节猜出来。
func Verify(plain, salt, want string) bool {
	if salt == "" || want == "" {
		return false
	}
	got := Hash(plain, salt)
	if len(got) != len(want) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// NewToken 生成一个随机的会话令牌。
func NewToken() string {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

// Session 是一次登录会话。
type Session struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Manager 管理内存中的会话表。
//
// 之所以不用 JWT：本应用是单实例部署，会话表放在内存里既简单又能做到
// 「改口令 / 退出登录立刻生效」——无状态令牌做不到即时吊销。
type Manager struct {
	mu       sync.Mutex
	sessions map[string]time.Time
	ttl      time.Duration
	now      func() time.Time
}

// DefaultTTL 是会话的默认有效期。
//
// 取 7 天：这是个人 NAS 面板，过短会让用户天天重新登录，
// 过长则失去「离开后自动失效」的意义。
const DefaultTTL = 7 * 24 * time.Hour

// NewManager 创建会话管理器；ttl <= 0 时使用 DefaultTTL。
func NewManager(ttl time.Duration) *Manager {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	return &Manager{
		sessions: make(map[string]time.Time),
		ttl:      ttl,
		now:      time.Now,
	}
}

// TTL 返回会话有效期。
func (m *Manager) TTL() time.Duration { return m.ttl }

// Issue 签发一个新会话。
func (m *Manager) Issue() Session {
	token := NewToken()
	if token == "" {
		return Session{}
	}
	expires := m.now().Add(m.ttl)

	m.mu.Lock()
	m.sessions[token] = expires
	m.mu.Unlock()

	return Session{Token: token, ExpiresAt: expires}
}

// Valid 报告令牌是否是仍然有效的会话。
func (m *Manager) Valid(token string) bool {
	token = strings.TrimSpace(token)
	if token == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	expires, ok := m.sessions[token]
	if !ok {
		return false
	}
	if m.now().After(expires) {
		delete(m.sessions, token)
		return false
	}
	return true
}

// Revoke 注销单个会话。
func (m *Manager) Revoke(token string) {
	m.mu.Lock()
	delete(m.sessions, token)
	m.mu.Unlock()
}

// RevokeAll 注销全部会话。
//
// 改口令时必须调用：否则旧口令签发的令牌会继续有效，
// 用户以为「换了密码就安全了」，实际上并没有。
func (m *Manager) RevokeAll() {
	m.mu.Lock()
	m.sessions = make(map[string]time.Time)
	m.mu.Unlock()
}

// GC 清理已过期的会话，返回清理数量。
func (m *Manager) GC() int {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()

	removed := 0
	for token, expires := range m.sessions {
		if now.After(expires) {
			delete(m.sessions, token)
			removed++
		}
	}
	return removed
}

// Count 返回当前会话数量（诊断用）。
func (m *Manager) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}
