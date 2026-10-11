package store

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"time"

	"github.com/MisiteQ/qlink2desktop/internal/domain"
)

// ---------------------------------------------------------------- forwards

func (s *Store) loadForwards() error {
	data, ok, err := readJSONFile(filepath.Join(s.dir, fileForwards))
	if err != nil {
		return fmt.Errorf("读取转发规则失败: %w", err)
	}
	if !ok {
		return nil
	}

	var loaded []domain.Forward
	if err := json.Unmarshal(data, &loaded); err != nil {
		s.quarantine(fileForwards, err)
		return nil
	}

	now := time.Now()
	for idx, f := range loaded {
		if f.ID == "" {
			f.ID = fmt.Sprintf("fwd-%d-%d", now.UnixNano(), idx)
		}
		if f.CreatedAt.IsZero() {
			f.CreatedAt = now.Add(-time.Duration(idx) * time.Second)
		}
		f.Normalize()
		s.forwards[f.ID] = f
	}
	return nil
}

// ListForwards 返回按创建时间倒序排列的全部转发规则。
func (s *Store) ListForwards() []domain.Forward {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]domain.Forward, 0, len(s.forwards))
	for _, f := range s.forwards {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID > out[j].ID
	})
	return out
}

// GetForward 按 ID 查询。
func (s *Store) GetForward(id string) (domain.Forward, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	f, ok := s.forwards[id]
	return f, ok
}

// UpsertForward 新增或整体替换一条转发规则。
//
// 与 UpsertLink 不同，这里**不做**标识冲突消解：
// 转发规则只在面板内部使用，不派生飞牛包名，因此重名是允许且无害的
// （用户完全可能有两条都叫"远程桌面"、监听不同端口的规则）。
// 强行去重要么静默改名、要么报错，两种都是自找麻烦。
func (s *Store) UpsertForward(f domain.Forward) (domain.Forward, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	if existing, ok := s.forwards[f.ID]; ok {
		f.CreatedAt = existing.CreatedAt
	} else {
		if f.ID == "" {
			f.ID = fmt.Sprintf("fwd-%d", now.UnixNano())
		}
		f.CreatedAt = now
	}
	f.UpdatedAt = now
	f.Normalize()

	if err := f.Validate(); err != nil {
		return domain.Forward{}, err
	}

	s.forwards[f.ID] = f
	if err := s.persistForwards(); err != nil {
		return domain.Forward{}, err
	}
	return f, nil
}

// DeleteForward 移除一条转发规则，返回被删除的那条。
func (s *Store) DeleteForward(id string) (domain.Forward, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	f, ok := s.forwards[id]
	if !ok {
		return domain.Forward{}, domain.ErrNotFound
	}
	delete(s.forwards, id)
	if err := s.persistForwards(); err != nil {
		return domain.Forward{}, err
	}
	return f, nil
}

// SetForwardEnabled 只翻转启用位，不触碰其它字段（用于列表上的开关）。
func (s *Store) SetForwardEnabled(id string, enabled bool) (domain.Forward, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	f, ok := s.forwards[id]
	if !ok {
		return domain.Forward{}, domain.ErrNotFound
	}
	f.Enabled = enabled
	f.UpdatedAt = time.Now()
	s.forwards[id] = f
	if err := s.persistForwards(); err != nil {
		return domain.Forward{}, err
	}
	return f, nil
}

func (s *Store) persistForwards() error {
	list := make([]domain.Forward, 0, len(s.forwards))
	for _, f := range s.forwards {
		list = append(list, f)
	}
	sort.Slice(list, func(i, j int) bool {
		if !list[i].CreatedAt.Equal(list[j].CreatedAt) {
			return list[i].CreatedAt.After(list[j].CreatedAt)
		}
		return list[i].ID > list[j].ID
	})
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化转发规则失败: %w", err)
	}
	return writeFileAtomic(filepath.Join(s.dir, fileForwards), data, 0o644)
}
