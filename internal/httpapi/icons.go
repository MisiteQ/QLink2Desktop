package httpapi

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/MisiteQ/qlink2desktop/internal/domain"
	"github.com/MisiteQ/qlink2desktop/internal/fnos"
)

// ---------------------------------------------------------------------------
// 图标
// ---------------------------------------------------------------------------

// iconItem 是图标库中的一项。
type iconItem struct {
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
	// URL 是可直接放进 <img src> 的地址。
	//
	// 必须是**相对路径**，不能写成 "/icons/..."：
	// 本应用在飞牛上是被统一网关挂在 /app/qlink2desktop/ 下访问的，
	// 根绝对路径会解析到 NAS 根目录，图标全部变成裂图。
	// 这条约束见 docs/ARCHITECTURE.md §2.9，前端所有资源同理。
	URL string `json:"url"`
}

func (s *Server) handleListIcons(w http.ResponseWriter, r *http.Request) {
	items, err := s.listIcons()
	if err != nil {
		writeError(w, err)
		return
	}
	ok(w, map[string]any{"items": items, "total": len(items)})
}

// iconsDir 返回当前生效的图标目录。
//
// 优先问 Service：图标目录是**可以在设置页运行时改**的，而那个值由
// fnos.Service 持有。Deps.IconsDir 只是装配期的默认值，作为兜底
// （没有 Service 的只读调试模式下才用得上）。
// 两个地方各记一份必然会漂移——之前就是这样，改完设置只有一半生效。
func (s *Server) iconsDir() string {
	if s.deps.Service != nil {
		return s.deps.Service.IconsDir()
	}
	return s.deps.IconsDir
}

func (s *Server) listIcons() ([]iconItem, error) {
	dir := s.iconsDir()
	if strings.TrimSpace(dir) == "" {
		return []iconItem{}, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []iconItem{}, nil
		}
		return nil, fmt.Errorf("读取图标目录失败: %w", err)
	}

	items := make([]iconItem, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".png") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		items = append(items, iconItem{
			Name:    e.Name(),
			Size:    info.Size(),
			ModTime: info.ModTime(),
			URL:     iconURL(e.Name()),
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ModTime.After(items[j].ModTime) })
	return items, nil
}

// handleServeIcon 提供图标文件。
//
// 该端点位于鉴权之外：飞牛应用中心的桌面 <img> 标签无法携带会话令牌，
// 若加上鉴权，桌面上所有图标都会变成裂图。
// 图标内容本身不敏感，公开可读是可接受的取舍。
func (s *Server) handleServeIcon(w http.ResponseWriter, r *http.Request) {
	raw := pathValue(r, "name")
	if raw == "" {
		http.NotFound(w, r)
		return
	}

	// 动态生成的图标（默认图标 / 门户图标预览），按尺寸渲染而不是读磁盘。
	if m := generatedIconPattern.FindStringSubmatch(raw); m != nil {
		s.serveGeneratedIcon(w, m[1], m[2])
		return
	}

	name := domain.SanitizeFilePart(filepath.Base(raw))
	// 名字被清洗过说明原始输入里带了路径分隔符等危险字符，直接拒绝而不是静默纠正。
	if name != filepath.Base(raw) || name == "" {
		http.NotFound(w, r)
		return
	}

	path := filepath.Join(s.iconsDir(), name)
	data, err := os.ReadFile(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
}

var generatedIconPattern = regexp.MustCompile(`^(default|portal)-(\d{1,4})\.png$`)

func (s *Server) serveGeneratedIcon(w http.ResponseWriter, kind, sizeRaw string) {
	size, err := strconv.Atoi(sizeRaw)
	if err != nil || size < 16 || size > 1024 {
		size = fnos.IconSizeLarge
	}

	switch kind {
	case "default":
		data, err := fnos.EncodePNG(fnos.DefaultIcon(size))
		if err != nil {
			writeError(w, err)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write(data)

	case "portal":
		icon := s.deps.Store.Settings().PortalIcon
		img, err := fnos.RenderTextIcon(icon.Text, icon.TextColor, icon.BgColor, size)
		if err != nil {
			writeError(w, err)
			return
		}
		data, err := fnos.EncodePNG(img)
		if err != nil {
			writeError(w, err)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		// 门户图标随设置变化，不能长缓存，否则用户改了配色看不到效果。
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(data)

	default:
		http.NotFound(w, nil)
	}
}

// handleUploadIcon 接收上传的图标。
//
// 同时支持两种提交方式，因为不同前端场景各有便利：
//   - multipart/form-data（表单里 file 字段）——浏览器原生上传；
//   - 裸二进制请求体 + ?name=xxx —— 剪贴板粘贴、脚本调用更简单。
func (s *Server) handleUploadIcon(w http.ResponseWriter, r *http.Request) {
	if strings.TrimSpace(s.iconsDir()) == "" {
		writeError(w, fmt.Errorf("未配置图标目录"))
		return
	}

	name, data, err := readIconUpload(r)
	if err != nil {
		writeError(w, err)
		return
	}

	saved, err := fnos.SaveUpload(s.iconsDir(), name, data)
	if err != nil {
		writeError(w, err)
		return
	}

	s.deps.Logger.Info("已保存上传图标", "name", saved, "bytes", len(data))
	ok(w, map[string]any{
		"name": saved,
		"url":  iconURL(saved),
		"icon": domain.Icon{Source: domain.IconUpload, Ref: saved},
	})
}

// iconURL 拼出图标在**当前页面上下文里**可直接使用的相对地址。
//
// 刻意不加前导斜杠：应用可能挂在统一网关的 /app/<name>/ 前缀下，
// 也可能是开发机上直连根路径，只有相对地址在两种形态下都成立。
// 前端 api.icons.url() 是同一约定的另一份实现，两者必须保持一致。
func iconURL(name string) string {
	return "icons/" + url.PathEscape(name)
}

// maxUploadBytes 是图标上传的体积上限（前端会先压缩，1MB 足够）。
const maxUploadBytes = 1 << 20

func readIconUpload(r *http.Request) (string, []byte, error) {
	contentType := r.Header.Get("Content-Type")

	if strings.HasPrefix(contentType, "multipart/form-data") {
		// 限制内存中缓存的表单大小，超出部分自动落临时文件。
		if err := r.ParseMultipartForm(maxUploadBytes); err != nil {
			return "", nil, fmt.Errorf("%w: 表单解析失败: %v", domain.ErrValidation, err)
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			return "", nil, fmt.Errorf("%w: 表单里没有 file 字段", domain.ErrValidation)
		}
		defer file.Close()

		data, err := readLimited(file, maxUploadBytes)
		if err != nil {
			return "", nil, err
		}
		return header.Filename, data, nil
	}

	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		name = "icon.png"
	}
	data, err := readLimited(r.Body, maxUploadBytes)
	if err != nil {
		return "", nil, err
	}
	return name, data, nil
}

// readLimited 读取至多 limit 字节；超出则报错而不是静默截断。
//
// 多读 1 个字节用于区分"刚好等于上限"与"超过上限"，
// 否则一个恰好 limit+1 字节的文件会被悄悄截断成合法图片。
func readLimited(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, fmt.Errorf("读取上传内容失败: %w", err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%w: 图标文件过大（超过 %d KB）", domain.ErrValidation, limit>>10)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("%w: 上传内容为空", domain.ErrValidation)
	}
	return data, nil
}

func (s *Server) handleDeleteIcon(w http.ResponseWriter, r *http.Request) {
	name := domain.SanitizeFilePart(filepath.Base(pathValue(r, "name")))
	if name == "" || name != filepath.Base(pathValue(r, "name")) {
		writeError(w, domain.NotFound("图标"))
		return
	}

	path := filepath.Join(s.iconsDir(), name)
	if _, err := os.Stat(path); err != nil {
		writeError(w, domain.NotFound("图标"))
		return
	}
	if err := os.Remove(path); err != nil {
		writeError(w, fmt.Errorf("删除图标失败: %w", err))
		return
	}
	s.deps.Logger.Info("已删除图标", "name", name)
	ok(w, map[string]bool{"ok": true})
}
