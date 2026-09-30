package httpapi

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/MisiteQ/qlink2desktop/internal/auth"
	"github.com/MisiteQ/qlink2desktop/internal/domain"
	"github.com/MisiteQ/qlink2desktop/internal/fnos"
	"github.com/MisiteQ/qlink2desktop/internal/proxy"
)

// settingsRequest 是设置更新请求体。
//
// 全部字段用指针：只有显式提供的字段才会被修改，
// 避免前端只改一个开关却把其它设置重置为默认值。
type settingsRequest struct {
	PortalPort     *int           `json:"portal_port,omitempty"`
	PortalName     *string        `json:"portal_name,omitempty"`
	PortalUI       *domain.UIType `json:"portal_ui,omitempty"`
	PortalAllUsers *bool          `json:"portal_all_users,omitempty"`
	PortalIcon     *domain.Icon   `json:"portal_icon,omitempty"`
	AutoReconcile  *bool          `json:"auto_reconcile,omitempty"`

	// IconsDir 留空字符串表示"恢复默认目录"（应用数据目录下的 icons/）。
	IconsDir *string `json:"icons_dir,omitempty"`

	// CurrentPassword 在已设置口令时必填，防止会话被劫持后直接改掉口令。
	CurrentPassword string `json:"current_password,omitempty"`
	// Password 非空表示设置或更换口令。
	Password string `json:"password,omitempty"`
	// ClearPassword 为 true 时清除口令保护。
	ClearPassword bool `json:"clear_password,omitempty"`
}

// settingsResponse 是设置查询 / 更新的响应。
type settingsResponse struct {
	Settings domain.Settings `json:"settings"`
	// Protected 表示是否启用了口令保护。
	Protected bool `json:"protected"`
	// PasswordChanged 表示本次请求是否改动了口令。
	PasswordChanged bool `json:"password_changed,omitempty"`
	// PortalPortChanged 表示门户端口是否发生变化（前端据此提示需要重启）。
	PortalPortChanged bool `json:"portal_port_changed,omitempty"`
}

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	settings := s.deps.Store.Settings()
	// 摘要与盐绝不能出网：即便它们不是明文，泄露出去也只是白白降低爆破成本。
	settings.AuthHash = ""
	settings.AuthSalt = ""

	ok(w, settingsResponse{
		Settings:  settings,
		Protected: s.deps.Store.Settings().HasPassword(),
	})
}

func (s *Server) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	var req settingsRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	current := s.deps.Store.Settings()
	portBefore := current.PortalPort

	// 图标目录变更：先校验并落到运行时，再随设置一起持久化。
	//
	// 顺序有意为之——校验不过就整单拒绝，不留"设置里写着新目录、运行时还在用旧目录"
	// 这种要等到重启才自愈的状态。持久化失败则回滚运行时，两边始终一致。
	iconsDirBefore := ""
	if s.deps.Service != nil {
		iconsDirBefore = s.deps.Service.IconsDir()
	}
	iconsTarget, iconsDirChanged := "", false
	if req.IconsDir != nil {
		if s.deps.Service == nil {
			writeError(w, fmt.Errorf("当前运行模式下不支持修改图标目录"))
			return
		}
		target := strings.TrimSpace(*req.IconsDir)
		if target == "" {
			// 空字符串表示恢复默认目录（应用数据目录下的 icons/）。
			target = s.deps.IconsDir
		}
		if err := s.deps.Service.SetIconsDir(target); err != nil {
			writeError(w, err)
			return
		}
		iconsTarget, iconsDirChanged = target, true
	}

	// 口令变更先行校验：校验不过就整单拒绝，不留下"设置改了但口令没改"的半成品状态。
	passwordChanged, err := s.applyPasswordChange(current, req)
	if err != nil {
		s.rollbackIconsDir(iconsDirBefore, iconsDirChanged)
		writeError(w, err)
		return
	}

	// 摘要计算比较耗时（多轮派生），放在写锁之外完成。
	newHash, newSalt := current.AuthHash, current.AuthSalt
	if passwordChanged {
		changed, err := s.buildPasswordChange(current, req)
		if err != nil {
			s.rollbackIconsDir(iconsDirBefore, iconsDirChanged)
			writeError(w, err)
			return
		}
		newHash, newSalt = changed.AuthHash, changed.AuthSalt
	}

	var next domain.Settings
	err = s.deps.Store.UpdateSettings(func(st *domain.Settings) error {
		next = *st
		if req.PortalPort != nil {
			next.PortalPort = *req.PortalPort
		}
		if req.PortalName != nil {
			next.PortalName = *req.PortalName
		}
		if req.PortalUI != nil {
			next.PortalUI = *req.PortalUI
		}
		if req.PortalAllUsers != nil {
			next.PortalAllUsers = *req.PortalAllUsers
		}
		if req.PortalIcon != nil {
			next.PortalIcon = *req.PortalIcon
		}
		if req.AutoReconcile != nil {
			next.AutoReconcile = *req.AutoReconcile
		}
		if iconsDirChanged {
			// 持久化的是用户输入的原值：空字符串代表"跟随默认"，
			// 而不是把当前默认目录的绝对路径固化下来——否则以后默认值变了，
			// 这条记录会把用户钉在旧路径上。
			next.IconsDir = strings.TrimSpace(derefString(req.IconsDir))
		}
		if passwordChanged {
			next.AuthHash = newHash
			next.AuthSalt = newSalt
		}
		*st = next
		return nil
	})
	if err != nil {
		s.rollbackIconsDir(iconsDirBefore, iconsDirChanged)
		writeError(w, err)
		return
	}

	if iconsDirChanged {
		s.deps.Logger.Info("图标目录已切换", "from", iconsDirBefore, "to", iconsTarget)
	}

	if passwordChanged && s.deps.Sessions != nil {
		// 改口令必须吊销所有旧会话：否则旧令牌继续有效，用户以为安全了其实没有。
		s.deps.Sessions.RevokeAll()
		s.deps.Logger.Info("访问口令已更新，全部会话已失效")
	}

	if next.PortalPort != portBefore {
		s.deps.Logger.Info("门户端口已变更（需重启生效）", "from", portBefore, "to", next.PortalPort)
	}

	// 名称 / 可见性改动可以即时写回已安装的桌面配置，不必重装整个应用。
	if s.deps.Service != nil {
		go s.deps.Service.SyncPortal(next)
	}

	safe := next
	safe.AuthHash = ""
	safe.AuthSalt = ""

	s.broadcast(EventSnapshot, map[string]any{"settings": safe})
	ok(w, settingsResponse{
		Settings:          safe,
		Protected:         next.HasPassword(),
		PasswordChanged:   passwordChanged,
		PortalPortChanged: next.PortalPort != portBefore,
	})
}

// applyPasswordChange 只做校验，不写入。返回本次是否要改口令。
func (s *Server) applyPasswordChange(current domain.Settings, req settingsRequest) (bool, error) {
	if req.ClearPassword {
		if current.HasPassword() {
			if err := s.verifyCurrentPassword(current, req); err != nil {
				return false, err
			}
		}
		return true, nil
	}
	if strings.TrimSpace(req.Password) == "" {
		return false, nil
	}
	if current.HasPassword() {
		if err := s.verifyCurrentPassword(current, req); err != nil {
			return false, err
		}
	}
	if err := validatePasswordStrength(req.Password); err != nil {
		return false, err
	}
	return true, nil
}

// buildPasswordChange 生成新的盐与摘要。
func (s *Server) buildPasswordChange(current domain.Settings, req settingsRequest) (domain.Settings, error) {
	out := current
	if req.ClearPassword {
		out.AuthHash = ""
		out.AuthSalt = ""
		return out, nil
	}
	salt := auth.NewSalt()
	if salt == "" {
		return out, fmt.Errorf("生成随机盐失败")
	}
	hash := auth.Hash(req.Password, salt)
	if hash == "" {
		return out, fmt.Errorf("计算口令摘要失败")
	}
	out.AuthSalt = salt
	out.AuthHash = hash
	return out, nil
}

func (s *Server) verifyCurrentPassword(current domain.Settings, req settingsRequest) error {
	if req.CurrentPassword == "" {
		return fmt.Errorf("%w: 修改口令需要先提供当前口令", domain.ErrValidation)
	}
	if !auth.Verify(req.CurrentPassword, current.AuthSalt, current.AuthHash) {
		return fmt.Errorf("%w: 当前口令不正确", domain.ErrValidation)
	}
	return nil
}

// validatePasswordStrength 对弱口令给出明确拒绝理由。
//
// 只设最低门槛（8 位且非纯数字）：家用场景下过于苛刻的复杂度要求
// 只会把人推向"设一个自己都记不住的密码然后贴在显示器上"。
func validatePasswordStrength(pw string) error {
	runes := []rune(pw)
	if len(runes) < 8 {
		return fmt.Errorf("%w: 口令至少需要 8 个字符", domain.ErrValidation)
	}
	allDigits := true
	for _, r := range runes {
		if r < '0' || r > '9' {
			allDigits = false
			break
		}
	}
	if allDigits {
		return fmt.Errorf("%w: 口令不能是纯数字", domain.ErrValidation)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 工具
// ---------------------------------------------------------------------------

// findFreePort 在给定起点之后找一个空闲端口，避开代理与已有服务占用的端口。
// rollbackIconsDir 在本次请求的后续步骤失败时，把图标目录退回改动前的值。
//
// 只在真的改过、且原值有效时才回滚：before 为空说明本来就"跟随默认"，
// 没有可恢复的绝对路径，此时交给下次启动用默认值，比强行写一个空目录好。
func (s *Server) rollbackIconsDir(before string, changed bool) {
	if !changed || s.deps.Service == nil || strings.TrimSpace(before) == "" {
		return
	}
	if err := s.deps.Service.SetIconsDir(before); err != nil {
		s.deps.Logger.Warn("回滚图标目录失败，请到设置页确认当前路径", "dir", before, "error", err)
	}
}

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func (s *Server) findFreePort() int {
	avoid := map[int]bool{}
	if s.deps.Proxy != nil {
		for p := range s.deps.Proxy.UsedPorts() {
			avoid[p] = true
		}
	}
	if s.deps.Store != nil {
		for _, l := range s.deps.Store.ListLinks() {
			if l.Enabled && l.Port > 0 {
				avoid[l.Port] = true
			}
		}
	}
	return proxy.PickPort(s.deps.PortBase, avoid)
}

// installedApps 返回应用中心里已注册的本项目应用（用于诊断面板）。
func (s *Server) installedApps() []string {
	if s.deps.Service == nil || !s.deps.Service.Available() {
		return nil
	}
	names, err := s.deps.Service.CLI().List()
	if err != nil {
		return nil
	}
	return fnos.ExtractManagedApps(names)
}
