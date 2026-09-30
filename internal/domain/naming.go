package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// appNamePattern 对应飞牛包标识规范：字母数字开头，仅允许字母、数字、点、下划线、短横线。
var appNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ValidateAppName 校验一个飞牛包标识是否合法。
func ValidateAppName(appName string) error {
	n := len(appName)
	if n < MinAppNameLength {
		return fmt.Errorf("%w: 应用标识 %q 过短（至少 %d 个字符）", ErrValidation, appName, MinAppNameLength)
	}
	if n > MaxAppNameLength {
		return fmt.Errorf("%w: 应用标识 %q 过长（最多 %d 个字符，当前 %d）", ErrValidation, appName, MaxAppNameLength, n)
	}
	if !appNamePattern.MatchString(appName) {
		return fmt.Errorf("%w: 应用标识 %q 含非法字符（需字母数字开头，且仅含字母、数字、. _ -）", ErrValidation, appName)
	}
	return nil
}

// slug 把任意字符串压缩成 [a-z0-9-] 组成的小写片段。
// 中文、空格、下划线、斜杠等一律丢弃或归一为短横线。
func slug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-', r == '_', r == '.', r == ' ', r == '/':
			b.WriteRune('-')
		}
	}
	return strings.Trim(strings.Join(strings.FieldsFunc(b.String(), func(r rune) bool { return r == '-' }), "-"), "-")
}

// Slug 导出 slug，供前端 / 图标文件名等复用。
func Slug(s string) string { return slug(s) }

// isASCIIIdent 报告 s 是否可直接用作标识片段（无需 transliterate）。
func isASCIIIdent(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

// shortDiscriminator 从链接 ID 派生 6 位以内、可用于包名的唯一后缀。
// 这是保证「同名容器 / 同端口也能各自生成独立桌面图标」的关键。
func shortDiscriminator(l Link) string {
	id := slug(strings.TrimPrefix(l.ID, "item-"))
	if len(id) > 6 {
		id = id[:6]
	}
	if id != "" {
		return id
	}
	sum := sha256.Sum256([]byte(l.ID + "\x00" + l.Name + "\x00" + strconv.Itoa(l.Port)))
	return hex.EncodeToString(sum[:])[:6]
}

// baseFragment 选择包名中最具辨识度的主体片段。
func baseFragment(l Link) string {
	switch {
	case l.Container.Name != "":
		return l.Container.Name
	case l.Name != "" && isASCIIIdent(l.Name):
		return l.Name
	case l.Port > 0:
		return portFragment(l.Port)
	default:
		return "app"
	}
}

// DeriveAppName 为由 Link 派生一个符合飞牛规范的包标识：
//
//	qlink2d.<主体片段>-<6位判别码>
//
// 长度严格控制在 MaxAppNameLength 以内，主体片段会被裁剪但判别码永不丢弃。
func DeriveAppName(l Link) string {
	disc := shortDiscriminator(l)
	base := slug(baseFragment(l))
	if base == "" {
		base = "app"
	}

	// 预算是：前缀 + 主体 + "-" + 判别码 ≤ 32
	budget := MaxAppNameLength - len(AppPrefix) - len(disc) - 1
	if budget < 3 {
		budget = 3
	}
	if len(base) > budget {
		base = strings.Trim(base[:budget], "-")
	}
	if base == "" {
		base = "app"
	}

	name := AppPrefix + base + "-" + disc
	if len(name) > MaxAppNameLength { // 双保险
		name = name[:MaxAppNameLength]
	}
	return name
}

// ResolveAppName 返回一个在 taken 中尚未被占用的包标识。
// 已显式设置 AppName 且未被占用时原样返回；否则按 -2、-3… 递增后缀。
func ResolveAppName(l Link, taken map[string]bool) string {
	if l.AppName != "" && !taken[l.AppName] {
		return l.AppName
	}
	base := DeriveAppName(l)
	if !taken[base] {
		return base
	}
	for i := 2; i < 1000; i++ {
		suffix := "-" + strconv.Itoa(i)
		candidate := base
		if len(candidate)+len(suffix) > MaxAppNameLength {
			candidate = candidate[:MaxAppNameLength-len(suffix)]
		}
		candidate += suffix
		if !taken[candidate] {
			return candidate
		}
	}
	return base
}

// SanitizeFilePart 把字符串清洗为安全的文件名单段（用于图标落盘）。
func SanitizeFilePart(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := strings.Trim(b.String(), "._-")
	if out == "" {
		return "icon.png"
	}
	return out
}
