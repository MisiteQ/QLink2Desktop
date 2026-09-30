package store

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"time"

	"github.com/MisiteQ/qlink2desktop/internal/domain"
)

const fileHosts = "hosts.json"

// loadHosts 读取远端主机列表。
func (s *Store) loadHosts() error {
	data, ok, err := readJSONFile(filepath.Join(s.dir, fileHosts))
	if err != nil {
		return fmt.Errorf("读取主机列表失败: %w", err)
	}
	if !ok {
		return nil
	}
	var loaded []domain.Host
	if err := json.Unmarshal(data, &loaded); err != nil {
		s.quarantine(fileHosts, err)
		return nil
	}
	now := time.Now()
	for idx, h := range loaded {
		if h.ID == "" {
			h.ID = fmt.Sprintf("host-%d-%d", now.UnixNano(), idx)
		}
		h.Normalize()
		s.hosts[h.ID] = h
	}
	return nil
}

// ListHosts 返回按名称排序的主机列表。
func (s *Store) ListHosts() []domain.Host {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]domain.Host, 0, len(s.hosts))
	for _, h := range s.hosts {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// GetHost 按 ID 查询主机。
func (s *Store) GetHost(id string) (domain.Host, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	h, ok := s.hosts[id]
	return h, ok
}

// SaveHost 新增或更新一台主机。
func (s *Store) SaveHost(h domain.Host) (domain.Host, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	if existing, ok := s.hosts[h.ID]; ok {
		h.CreatedAt = existing.CreatedAt
	} else {
		if h.ID == "" {
			h.ID = fmt.Sprintf("host-%d", now.UnixNano())
		}
		h.CreatedAt = now
	}
	h.UpdatedAt = now
	h.Normalize()
	if err := h.Validate(); err != nil {
		return domain.Host{}, err
	}

	s.hosts[h.ID] = h
	return h, s.persistHosts()
}

// DeleteHost 移除一台主机。
func (s *Store) DeleteHost(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.hosts[id]; !ok {
		return domain.ErrNotFound
	}
	delete(s.hosts, id)
	return s.persistHosts()
}

// RecordHostProbe 记录一次探测结果。
func (s *Store) RecordHostProbe(id string, ok bool, errMsg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, found := s.hosts[id]
	if !found {
		return
	}
	h.LastOK = ok
	h.LastError = errMsg
	h.CheckedAt = time.Now()
	s.hosts[id] = h
	_ = s.persistHosts()
}

func (s *Store) persistHosts() error {
	list := make([]domain.Host, 0, len(s.hosts))
	for _, h := range s.hosts {
		list = append(list, h)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })

	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化主机列表失败: %w", err)
	}
	return writeFileAtomic(filepath.Join(s.dir, fileHosts), data, 0o600)
}
