package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/MisiteQ/qlink2desktop/internal/domain"
)

// ---------------------------------------------------------------------------
// 请求体
// ---------------------------------------------------------------------------

// linkRequest 是链接的新增 / 修改请求体。
//
// 刻意包含 id / created_at / updated_at 三个只读字段：
// 前端的编辑表单往往直接把一行数据（甚至整个视图对象）回传，
// 如果这里不声明它们，严格解码会报"未知字段"，用户会看到一个莫名其妙的错误。
// 声明后当作噪声忽略即可。
type linkRequest struct {
	ID        string    `json:"id,omitempty"`
	CreatedAt time.Time `json:"created_at,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`

	Name    string `json:"name"`
	Desc    string `json:"desc,omitempty"`
	Kind    string `json:"kind"`
	AppName string `json:"app_name,omitempty"`

	Scheme string `json:"scheme,omitempty"`
	Host   string `json:"host,omitempty"`
	Port   int    `json:"port,omitempty"`
	Path   string `json:"path,omitempty"`

	UI        string            `json:"ui,omitempty"`
	AllUsers  bool              `json:"all_users,omitempty"`
	NoDisplay bool              `json:"no_display,omitempty"`
	Icon      *domain.Icon      `json:"icon,omitempty"`
	FileTypes []string          `json:"file_types,omitempty"`
	Container *domain.Container `json:"container,omitempty"`

	SkipTLSVerify bool `json:"skip_tls_verify,omitempty"`

	// Enabled 用指针以区分「未提供」（新建时默认启用）与「显式关闭」。
	Enabled *bool `json:"enabled,omitempty"`
}

// toDomain 把请求体合并进一个基准 Link。
//
// base 为空 Link 表示新建；传现有 Link 表示修改（保留 CreatedAt 等只读字段）。
func (req linkRequest) toDomain(base domain.Link) (domain.Link, error) {
	kind, err := domain.ParseKind(req.Kind)
	if err != nil {
		return domain.Link{}, err
	}

	l := base
	l.Name = req.Name
	l.Desc = req.Desc
	l.Kind = kind
	l.AppName = strings.TrimSpace(req.AppName)

	l.Scheme = req.Scheme
	l.Host = req.Host
	l.Port = req.Port
	l.Path = req.Path

	l.UI = domain.UIType(req.UI)
	l.AllUsers = req.AllUsers
	l.NoDisplay = req.NoDisplay
	l.FileTypes = req.FileTypes
	l.SkipTLSVerify = req.SkipTLSVerify

	if req.Icon != nil {
		l.Icon = *req.Icon
	}
	if req.Container != nil {
		l.Container = *req.Container
	}
	// 快捷方式形态不承载容器信息，避免用户从一个形态切到另一个时留下脏数据。
	if kind == domain.KindShortcut {
		l.Container = domain.Container{}
		l.Port = 0
		l.Host = ""
	}

	if req.Enabled != nil {
		l.Enabled = *req.Enabled
	}

	l.Normalize()
	if err := l.Validate(); err != nil {
		return domain.Link{}, err
	}
	return l, nil
}

// decodeLinkRequest 解析链接请求体。
//
// 这里用宽松解码（忽略未知字段）而非严格解码：
// 链接的前端表单字段多，且经常整体回传对象，严格模式带来的
// "未知字段" 报错对用户毫无帮助，只会制造挫败感。
func decodeLinkRequest(w http.ResponseWriter, r *http.Request) (*linkRequest, bool) {
	if r.Body == nil {
		writeError(w, fmt.Errorf("%w: 请求体为空", domain.ErrValidation))
		return nil, false
	}
	body := http.MaxBytesReader(w, r.Body, maxBodyBytes)
	var req linkRequest
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		writeError(w, fmt.Errorf("%w: 请求体解析失败: %v", domain.ErrValidation, err))
		return nil, false
	}
	return &req, true
}

// ---------------------------------------------------------------------------
// 处理器
// ---------------------------------------------------------------------------

func (s *Server) handleListLinks(w http.ResponseWriter, r *http.Request) {
	ok(w, map[string]any{
		"items": s.views(),
		"total": len(s.views()),
	})
}

func (s *Server) handleGetLink(w http.ResponseWriter, r *http.Request) {
	id := pathValue(r, "id")
	if s.deps.Coordinator != nil {
		if v, found := s.deps.Coordinator.View(id); found {
			ok(w, v)
			return
		}
		writeError(w, domain.ErrNotFound)
		return
	}
	l, found := s.deps.Store.GetLink(id)
	if !found {
		writeError(w, domain.ErrNotFound)
		return
	}
	ok(w, domain.NewView(l, domain.Status{Phase: domain.PhaseUnknown}))
}

func (s *Server) handleCreateLink(w http.ResponseWriter, r *http.Request) {
	req, valid := decodeLinkRequest(w, r)
	if !valid {
		return
	}

	l, err := req.toDomain(domain.Link{})
	if err != nil {
		writeError(w, err)
		return
	}
	// 新建默认启用：用户的意图就是"放到桌面"，再让他多点一次开关很别扭。
	l.Enabled = true
	if req.Enabled != nil {
		l.Enabled = *req.Enabled
	}

	saved, err := s.deps.Store.UpsertLink(l)
	if err != nil {
		writeError(w, err)
		return
	}

	s.deps.Logger.Info("新建链接", "id", saved.ID, "name", saved.Name,
		"kind", string(saved.Kind), "appName", saved.EffectiveAppName())

	// 先落盘再受理：即使注册失败，用户重开面板也能看到这条记录与失败原因，
	// 而不是"我以为加上了，结果什么都没有"。
	//
	// 注意这里**不等待**注册结果——它可能要跑几分钟的 appcenter-cli。
	// 接口立刻返回「排队中」，真实结果走 SSE 状态推送。
	s.queueSync(saved.ID)

	s.broadcast(EventLinksChanged, nil)
	ok(w, s.viewOf(saved.ID, saved))
}

func (s *Server) handleUpdateLink(w http.ResponseWriter, r *http.Request) {
	id := pathValue(r, "id")
	existing, found := s.deps.Store.GetLink(id)
	if !found {
		writeError(w, domain.ErrNotFound)
		return
	}

	req, valid := decodeLinkRequest(w, r)
	if !valid {
		return
	}

	l, err := req.toDomain(existing)
	if err != nil {
		writeError(w, err)
		return
	}

	saved, err := s.deps.Store.UpsertLink(l)
	if err != nil {
		writeError(w, err)
		return
	}
	s.deps.Logger.Info("更新链接", "id", saved.ID, "name", saved.Name)

	// 同新建：只受理，不等待。见 handleCreateLink 的注释。
	s.queueSync(saved.ID)

	s.broadcast(EventLinksChanged, nil)
	ok(w, s.viewOf(saved.ID, saved))
}

// handleDeleteLink 删除一条链接。
//
// 响应只承诺两件事：定义已经从面板上消失、代理监听已经回收。
// 桌面图标的注销（要调 appcenter-cli）交给后台队列——真机上正是这一步
// 曾经让前端报出「请求超时」，而服务端其实正在正常卸载。
func (s *Server) handleDeleteLink(w http.ResponseWriter, r *http.Request) {
	id := pathValue(r, "id")

	if s.deps.Coordinator != nil {
		deleted, err := s.deps.Coordinator.QueueRemove(id)
		if err != nil {
			writeError(w, err)
			return
		}
		s.deps.Logger.Info("删除链接", "id", id, "appName", deleted.EffectiveAppName())
	} else {
		if _, err := s.deps.Store.DeleteLink(id); err != nil {
			writeError(w, err)
			return
		}
		s.deps.Logger.Info("删除链接", "id", id)
	}

	s.broadcast(EventLinksChanged, nil)
	ok(w, map[string]bool{"ok": true})
}

// toggleRequest 是启用开关的请求体。
type toggleRequest struct {
	Enabled bool `json:"enabled"`
}

func (s *Server) handleToggleLink(w http.ResponseWriter, r *http.Request) {
	id := pathValue(r, "id")
	var req toggleRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	if s.deps.Coordinator != nil {
		if err := s.deps.Coordinator.QueueSetEnabled(id, req.Enabled); err != nil {
			writeError(w, err)
			return
		}
	} else {
		if _, err := s.deps.Store.SetLinkEnabled(id, req.Enabled); err != nil {
			writeError(w, err)
			return
		}
	}

	l, found := s.deps.Store.GetLink(id)
	if !found {
		writeError(w, domain.ErrNotFound)
		return
	}
	s.broadcast(EventLinksChanged, nil)
	ok(w, s.viewOf(id, l))
}

// handleSyncLink 强制重新生成并注册桌面图标。
//
// 用途：用户在应用中心手动删掉了图标、或改了容器端口后想让配置立刻收敛，
// 不必重启整个服务。
//
// 同样只受理：返回 202 而不是 200，让前端清楚知道"还没做完"。
func (s *Server) handleSyncLink(w http.ResponseWriter, r *http.Request) {
	id := pathValue(r, "id")
	if _, found := s.deps.Store.GetLink(id); !found {
		writeError(w, domain.ErrNotFound)
		return
	}
	s.queueSync(id)
	s.broadcast(EventLinksChanged, nil)
	accepted(w, "已在后台重新同步，完成后状态会自动更新", s.pendingJobs())
}

// probeLinkRequest 允许覆盖探测目标；留空则用链接自身的后端地址。
type probeLinkRequest struct {
	URL string `json:"url,omitempty"`
}

func (s *Server) handleProbeLink(w http.ResponseWriter, r *http.Request) {
	id := pathValue(r, "id")
	l, found := s.deps.Store.GetLink(id)
	if !found {
		writeError(w, domain.ErrNotFound)
		return
	}

	target := l.ShortcutURL()
	if l.Kind != domain.KindShortcut {
		target = l.BackendURL()
	}
	// 允许前端在保存前先试连一个还没落库的地址。
	var req probeLinkRequest
	if r.Body != nil && r.ContentLength != 0 {
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req)
	}
	if strings.TrimSpace(req.URL) != "" {
		target = strings.TrimSpace(req.URL)
	}

	ok(w, s.probe(r.Context(), target))
}

// handleExportLinks 导出全部链接定义为 JSON 文件。
//
// 刻意导出裸定义（不含运行态）：这份文件可以直接改一改再导入，
// 也可以放进版本库做备份。
func (s *Server) handleExportLinks(w http.ResponseWriter, r *http.Request) {
	links := s.deps.Store.ListLinks()
	payload, err := json.MarshalIndent(map[string]any{
		"version":     s.deps.Version,
		"exported_at": time.Now().Format(time.RFC3339),
		"links":       links,
	}, "", "  ")
	if err != nil {
		writeError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="qlink2desktop-links-%s.json"`, time.Now().Format("20060102-150405")))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(payload)
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

// queueSync 受理一次桌面同步（有 Coordinator 时走完整编排）。
//
// 接口层**没有**同步版本：Sync 会调用 appcenter-cli，单条命令最长 3 分钟，
// 放进请求路径必然撞上前端的网络预算。见 Coordinator 接口的注释。
func (s *Server) queueSync(id string) {
	if s.deps.Coordinator == nil {
		return
	}
	s.deps.Coordinator.QueueSync(id)
}

// pendingJobs 返回后台待处理数量（无 Coordinator 时为 0）。
func (s *Server) pendingJobs() int {
	if s.deps.Coordinator == nil {
		return 0
	}
	return s.deps.Coordinator.Pending()
}

// viewOf 优先走 Coordinator 的实时状态，退化时用零值状态。
func (s *Server) viewOf(id string, fallback domain.Link) domain.View {
	if s.deps.Coordinator != nil {
		if v, found := s.deps.Coordinator.View(id); found {
			return v
		}
	}
	return domain.NewView(fallback, domain.Status{Phase: domain.PhaseUnknown})
}

// ensureLink 读取一条链接，不存在时写出 404 并返回 false。
func (s *Server) mustLink(w http.ResponseWriter, id string) (domain.Link, bool) {
	l, found := s.deps.Store.GetLink(id)
	if !found {
		writeError(w, domain.ErrNotFound)
		return domain.Link{}, false
	}
	return l, true
}

// isValidationErr 便于处理器做分支。
func isValidationErr(err error) bool { return errors.Is(err, domain.ErrValidation) }
