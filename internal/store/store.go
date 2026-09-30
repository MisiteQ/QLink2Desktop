// Package store 负责把领域对象持久化到磁盘。
//
// 设计要点：
//   - 单一并发出口：所有读写都经过一把 RWMutex，内存是唯一事实来源，磁盘只是投影。
//   - 原子落盘：见 atomic.go。
//   - 宽容读取：读到的文件损坏时不会清空数据，而是备份原文件并以默认值继续启动。
//   - 更新走「读-改-写」闭包，避免调用方拿到副本后回写造成丢更新。
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/MisiteQ/qlink2desktop/internal/domain"
)

// 磁盘文件名。
const (
	fileLinks    = "links.json"
	fileSettings = "settings.json"
	fileToggles  = "toggles.json"
)

// Store 是链接与设置的持久化仓储。
type Store struct {
	mu  sync.RWMutex
	dir string

	links    map[string]domain.Link
	hosts    map[string]domain.Host
	settings domain.Settings
	// toggles 保存「容器 label 自动发现项」的启用开关。
	// 这类条目没有完整的 Link 定义（它们由 Docker 标签动态产生），
	// 因此单独用一个 KV 表记录用户的取舍。
	toggles map[string]bool
}

// Open 打开（必要时初始化）数据目录。
func Open(dir string) (*Store, error) {
	if strings.TrimSpace(dir) == "" {
		dir = "data"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("无法创建数据目录 %s: %w", dir, err)
	}

	s := &Store{
		dir:      dir,
		links:    make(map[string]domain.Link),
		hosts:    make(map[string]domain.Host),
		settings: domain.DefaultSettings(),
		toggles:  make(map[string]bool),
	}

	for _, load := range []func() error{s.loadSettings, s.loadLinks, s.loadToggles, s.loadHosts} {
		if err := load(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// Dir 返回数据目录。
func (s *Store) Dir() string { return s.dir }

// Path 返回某个逻辑文件的绝对路径（供导出 / 备份使用）。
func (s *Store) Path(name string) string { return filepath.Join(s.dir, name) }

// ---------------------------------------------------------------- settings

func (s *Store) loadSettings() error {
	data, ok, err := readJSONFile(filepath.Join(s.dir, fileSettings))
	if err != nil {
		return fmt.Errorf("读取设置失败: %w", err)
	}
	if !ok {
		return s.persistSettings()
	}

	var loaded domain.Settings
	if err := json.Unmarshal(data, &loaded); err != nil {
		s.quarantine(fileSettings, err)
		return s.persistSettings()
	}
	loaded.Normalize()
	s.settings = loaded
	return nil
}

// Settings 返回当前设置的副本。
func (s *Store) Settings() domain.Settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.settings
}

// UpdateSettings 以闭包方式修改设置，读改写全程持锁，杜绝丢更新。
func (s *Store) UpdateSettings(mutate func(*domain.Settings) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	next := s.settings
	if mutate != nil {
		if err := mutate(&next); err != nil {
			return err
		}
	}
	next.Normalize()
	s.settings = next
	return s.persistSettings()
}

func (s *Store) persistSettings() error {
	data, err := json.MarshalIndent(s.settings, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化设置失败: %w", err)
	}
	return writeFileAtomic(filepath.Join(s.dir, fileSettings), data, 0o644)
}

// ---------------------------------------------------------------- links

func (s *Store) loadLinks() error {
	data, ok, err := readJSONFile(filepath.Join(s.dir, fileLinks))
	if err != nil {
		return fmt.Errorf("读取链接失败: %w", err)
	}
	if !ok {
		return nil
	}

	var loaded []domain.Link
	if err := json.Unmarshal(data, &loaded); err != nil {
		s.quarantine(fileLinks, err)
		return nil
	}

	now := time.Now()
	for idx, l := range loaded {
		if l.ID == "" {
			l.ID = fmt.Sprintf("item-%d-%d", now.UnixNano(), idx)
		}
		// 兼容早期版本：CreatedAt 缺失时按文件顺序倒推，保证排序稳定。
		if l.CreatedAt.IsZero() {
			l.CreatedAt = now.Add(-time.Duration(idx) * time.Second)
		}
		l.Normalize()
		s.links[l.ID] = l
	}
	return nil
}

// ListLinks 返回按创建时间倒序排列的全部链接。
func (s *Store) ListLinks() []domain.Link {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.listLinksLocked()
}

func (s *Store) listLinksLocked() []domain.Link {
	out := make([]domain.Link, 0, len(s.links))
	for _, l := range s.links {
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID > out[j].ID
	})
	return out
}

// GetLink 按 ID 查询。
func (s *Store) GetLink(id string) (domain.Link, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	l, ok := s.links[id]
	return l, ok
}

// LinkByAppName 按飞牛包标识反查。
func (s *Store) LinkByAppName(appName string) (domain.Link, bool) {
	if appName == "" {
		return domain.Link{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, l := range s.links {
		if l.EffectiveAppName() == appName {
			return l, true
		}
	}
	return domain.Link{}, false
}

// TakenAppNames 返回已被占用的包标识集合，供命名冲突消解使用。
func (s *Store) TakenAppNames() map[string]bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	taken := make(map[string]bool, len(s.links))
	for _, l := range s.links {
		if l.Kind != domain.KindShortcut || l.AppName != "" {
			taken[l.EffectiveAppName()] = true
		}
	}
	return taken
}

// UpsertLink 新增或整体替换一条链接，返回落盘后的最终状态（含自动派生的包名、时间戳）。
//
// 包名冲突在这里统一解决：调用方不需要自己处理，也就不会出现
// 「两个链接抢同一个包名、后者静默顶掉前者桌面图标」的问题。
func (s *Store) UpsertLink(l domain.Link) (domain.Link, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	if existing, ok := s.links[l.ID]; ok {
		l.CreatedAt = existing.CreatedAt
	} else {
		if l.ID == "" {
			l.ID = fmt.Sprintf("item-%d", now.UnixNano())
		}
		l.CreatedAt = now
	}
	l.UpdatedAt = now
	l.Normalize()

	// 冲突消解时排除自身，否则编辑已有链接会被判成和自己冲突。
	taken := make(map[string]bool, len(s.links))
	for id, other := range s.links {
		if id != l.ID {
			taken[other.EffectiveAppName()] = true
		}
	}
	l.AppName = domain.ResolveAppName(l, taken)

	if err := l.Validate(); err != nil {
		return domain.Link{}, err
	}

	s.links[l.ID] = l
	if err := s.persistLinks(); err != nil {
		return domain.Link{}, err
	}
	return l, nil
}

// DeleteLink 移除一条链接，返回被删除的那条（供调用方做后续清理）。
func (s *Store) DeleteLink(id string) (domain.Link, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	l, ok := s.links[id]
	if !ok {
		return domain.Link{}, domain.ErrNotFound
	}
	delete(s.links, id)
	if err := s.persistLinks(); err != nil {
		return domain.Link{}, err
	}
	return l, nil
}

// SetLinkEnabled 只翻转启用位，不触碰其它字段（用于列表上的开关）。
func (s *Store) SetLinkEnabled(id string, enabled bool) (domain.Link, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	l, ok := s.links[id]
	if !ok {
		return domain.Link{}, domain.ErrNotFound
	}
	l.Enabled = enabled
	l.UpdatedAt = time.Now()
	s.links[id] = l
	if err := s.persistLinks(); err != nil {
		return domain.Link{}, err
	}
	return l, nil
}

func (s *Store) persistLinks() error {
	data, err := json.MarshalIndent(s.listLinksLocked(), "", "  ")
	if err != nil {
		return fmt.Errorf("序列化链接失败: %w", err)
	}
	return writeFileAtomic(filepath.Join(s.dir, fileLinks), data, 0o644)
}

// ---------------------------------------------------------------- toggles

func (s *Store) loadToggles() error {
	data, ok, err := readJSONFile(filepath.Join(s.dir, fileToggles))
	if err != nil {
		return fmt.Errorf("读取开关状态失败: %w", err)
	}
	if !ok {
		return nil
	}
	var loaded map[string]bool
	if err := json.Unmarshal(data, &loaded); err != nil {
		s.quarantine(fileToggles, err)
		return nil
	}
	if loaded != nil {
		s.toggles = loaded
	}
	return nil
}

// Toggle 读取某个自动发现项的开关状态。
func (s *Store) Toggle(key string, def bool) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if v, ok := s.toggles[key]; ok {
		return v
	}
	return def
}

// SetToggle 写入某个自动发现项的开关状态。
func (s *Store) SetToggle(key string, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.toggles[key] = enabled
	data, err := json.MarshalIndent(s.toggles, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(s.dir, fileToggles), data, 0o644)
}

// ---------------------------------------------------------------- misc

// quarantine 把无法解析的文件改名备份，避免下次启动反复踩同一个坑，
// 同时保留现场供用户排查，绝不静默删除用户数据。
func (s *Store) quarantine(name string, cause error) {
	src := filepath.Join(s.dir, name)
	backup := fmt.Sprintf("%s.corrupt-%s", src, time.Now().Format("20060102-150405"))
	if err := os.Rename(src, backup); err != nil {
		slog.Error("备份损坏文件失败", "file", src, "error", err)
		return
	}
	slog.Warn("检测到损坏的持久化文件，已备份并重置", "file", src, "backup", backup, "cause", cause)
}

// ErrNotFound 由 store 暴露，方便上层不直接依赖 domain 包。
var ErrNotFound = domain.ErrNotFound

// IsNotFound 判断错误是否为「不存在」。
func IsNotFound(err error) bool { return errors.Is(err, domain.ErrNotFound) }
