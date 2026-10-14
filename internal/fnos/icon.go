package fnos

import (
	"bytes"
	// 空导入 embed：只为 []byte 形式的 //go:embed 指令引入编译器支持，
	// 不需要 embed.FS 类型本身。
	"context"
	_ "embed"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/jpeg" // 注册 JPEG 解码器
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/MisiteQ/qlink2desktop/internal/domain"
)

// logo_256.png 是产品的官方图标（由 cmd/iconforge 从根目录的
// QLink2Desktop.png 缩放生成）。把它直接编进二进制，是为了保证
// 「拷到哪台机器、怎么部署，默认图标都长一个样」——这也是本项目
// 对自包含移植性的基本要求。单独回退用的程序化图标见 proceduralIcon。
//
//go:embed assets/logo_256.png
var embeddedLogoPNG []byte

// 图标档位。飞牛桌面按 {0} 占位符取 64 或 256。
const (
	IconSizeSmall = 64
	IconSizeLarge = 256
)

// httpClient 用于拉取远端图标，超时设得较短，避免拖慢安装流程。
var httpClient = &http.Client{Timeout: 8 * time.Second}

// cdnTemplates 是自动匹配官方服务图标的镜像列表（同一份仓库的多个 CDN 节点）。
// 顺序即优先级：先国内可达性最好的节点。
var cdnTemplates = []string{
	"https://fastly.jsdelivr.net/gh/homarr-labs/dashboard-icons/png/%s.png",
	"https://gcore.jsdelivr.net/gh/homarr-labs/dashboard-icons/png/%s.png",
	"https://testingcf.jsdelivr.net/gh/homarr-labs/dashboard-icons/png/%s.png",
	"https://cdn.jsdelivr.net/gh/homarr-labs/dashboard-icons/png/%s.png",
}

// ---------------------------------------------------------------------------
// 渲染
// ---------------------------------------------------------------------------

// RenderTextIcon 用内嵌点阵字模渲染一枚文字图标。
func RenderTextIcon(text, textColor, bgColor string, size int) (image.Image, error) {
	if size <= 0 {
		size = IconSizeLarge
	}
	content := NormalizeIconText(text)
	bg := ParseHexColor(bgColor, color.RGBA{R: 0x25, G: 0x63, B: 0xeb, A: 255})
	fg := ParseHexColor(textColor, color.RGBA{R: 0xff, G: 0xff, B: 0xff, A: 255})

	img := image.NewRGBA(image.Rect(0, 0, size, size))

	// 背景：圆角方块 + 垂直方向的轻微渐变，比纯色更有质感。
	radius := float64(size) * 0.22
	bgDark := darken(bg, 0.18)
	hw := float64(size)/2 - float64(size)*0.03
	center := float64(size) / 2

	for y := 0; y < size; y++ {
		tGrad := float64(y) / float64(size-1)
		rowColor := blend(bg, bgDark, tGrad)
		for x := 0; x < size; x++ {
			cov := roundRectCoverage(float64(x), float64(y), center, center, hw, hw, radius)
			if cov <= 0 {
				continue
			}
			img.SetRGBA(x, y, color.RGBA{
				R: rowColor.R, G: rowColor.G, B: rowColor.B,
				A: uint8(cov * 255),
			})
		}
	}

	// 前景：点阵文字按最大可用宽度等比放大，居中绘制。
	glyphW := measureText(content)
	if glyphW > 0 {
		scale := int(math.Floor(float64(size) * 0.62 / float64(glyphW)))
		if scale < 1 {
			scale = 1
		}
		textW := glyphW * scale
		textH := glyphHeight * scale
		startX := (size - textW) / 2
		startY := (size - textH) / 2

		for _, r := range content {
			rows, ok := glyph5x7[r]
			if !ok {
				continue
			}
			for gy, row := range rows {
				for gx, ch := range row {
					if ch != '#' {
						continue
					}
					px := startX + gx*scale
					py := startY + gy*scale
					for dy := 0; dy < scale; dy++ {
						for dx := 0; dx < scale; dx++ {
							putOpaque(img, px+dx, py+dy, fg)
						}
					}
				}
			}
			startX += (glyphWidth + glyphSpacing) * scale
		}
	}
	return img, nil
}

// DefaultIcon 生成内置的默认图标。
//
// 首选内嵌的产品 logo —— 它是用户在飞牛桌面 / 应用中心看到的那张图，
// 用它兜底能让「没有匹配到官方图标的容器」也带上产品的视觉身份。
// logo 因任何原因不可用时，回退到程序化绘制的链环图形
// （proceduralIcon），保证安装流程永远拿得到一张有效图标。
func DefaultIcon(size int) image.Image {
	if size <= 0 {
		size = IconSizeLarge
	}
	if logo := productLogo(); logo != nil {
		return ResizeHighQuality(logo, size, size)
	}
	return proceduralIcon(size)
}

var (
	logoOnce  sync.Once
	logoImage image.Image
)

// productLogo 返回解码后的内嵌产品图标；失败时返回 nil（不缓存失败结果之外的值）。
//
// 用 sync.Once 而不是每次调用都解码：一张 256 见方的 PNG 解码要几百微秒，
// 批量生成桌面图标时这笔开销会累积得比较明显。
func productLogo() image.Image {
	logoOnce.Do(func() {
		img, _, err := image.Decode(bytes.NewReader(embeddedLogoPNG))
		if err != nil {
			slog.Error("内嵌产品图标解码失败，将回退到程序化图标", "error", err)
			return
		}
		logoImage = img
	})
	return logoImage
}

// proceduralIcon 是内嵌 logo 不可用时的兜底：圆角渐变底 + 链环图形。
//
// 用程序化绘制而不是再塞一张 PNG：仓库里少一个二进制黑盒，
// 任何尺寸都能按需生成，且改配色只需改几个常量。
func proceduralIcon(size int) image.Image {
	if size <= 0 {
		size = IconSizeLarge
	}
	top := color.RGBA{R: 0x3b, G: 0x82, B: 0xf6, A: 255}
	bottom := color.RGBA{R: 0x1d, G: 0x4e, B: 0xd8, A: 255}
	glyphColor := color.RGBA{R: 0xff, G: 0xff, B: 0xff, A: 255}

	img := image.NewRGBA(image.Rect(0, 0, size, size))
	radius := float64(size) * 0.22
	hw := float64(size)/2 - float64(size)*0.03
	center := float64(size) / 2

	for y := 0; y < size; y++ {
		tGrad := float64(y) / float64(size-1)
		rowColor := blend(top, bottom, tGrad)
		for x := 0; x < size; x++ {
			cov := roundRectCoverage(float64(x), float64(y), center, center, hw, hw, radius)
			if cov <= 0 {
				continue
			}
			img.SetRGBA(x, y, color.RGBA{R: rowColor.R, G: rowColor.G, B: rowColor.B, A: uint8(cov * 255)})
		}
	}

	// 两个互相交叠的「环」，构成链环意象。
	s := float64(size)
	ringThickness := s * 0.088
	ringHalfW := s * 0.155
	ringHalfH := s * 0.185
	ringR := s * 0.115
	leftCx, rightCx := s*0.385, s*0.615
	cy := s * 0.5

	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			fx, fy := float64(x), float64(y)
			cov := 0.0
			for _, cx := range []float64{leftCx, rightCx} {
				outer := roundRectSDF(fx, fy, cx, cy, ringHalfW, ringHalfH, ringR)
				inner := roundRectSDF(fx, fy, cx, cy, ringHalfW-ringThickness, ringHalfH-ringThickness, ringR-ringThickness*0.5)
				outerCov := coverage(outer)
				innerCov := coverage(inner)
				if c := outerCov - innerCov; c > cov {
					cov = c
				}
			}
			if cov <= 0 {
				continue
			}
			img.SetRGBA(x, y, blend(img.RGBAAt(x, y), glyphColor, math.Min(cov, 1)))
		}
	}
	return img
}

// ToIconSet 把一张任意尺寸的图缩为 64 / 256 两档。
//
// 用 ResizeHighQuality 而不是直接双线性：用户上传的图标普遍是 512～2048 见方，
// 一步插值缩到 64 会把绝大部分源像素丢掉，小图标会明显发糊。
func ToIconSet(img image.Image) (IconSet, error) {
	if img == nil {
		return IconSet{}, errors.New("图标图像为空")
	}
	small, err := EncodePNG(ResizeHighQuality(img, IconSizeSmall, IconSizeSmall))
	if err != nil {
		return IconSet{}, err
	}
	large, err := EncodePNG(ResizeHighQuality(img, IconSizeLarge, IconSizeLarge))
	if err != nil {
		return IconSet{}, err
	}
	return IconSet{Small: small, Large: large}, nil
}

// TextIconSet 直接从文字渲染出图标集。
func TextIconSet(text, textColor, bgColor string) (IconSet, error) {
	img, err := RenderTextIcon(text, textColor, bgColor, IconSizeLarge)
	if err != nil {
		return IconSet{}, err
	}
	return ToIconSet(img)
}

// defaultIconOnce 保证内置图标只渲染一次。
//
// 渲染 + 两次双线性缩放 + PNG 编码合计要几十毫秒；
// 原实现每构造一个包就重算一遍，批量安装时这笔开销会明显拖慢启动对账。
var defaultIconOnce sync.Once
var defaultIconCache IconSet

// DefaultIconSet 返回内置默认图标集（结果缓存在进程内，可安全并发调用）。
func DefaultIconSet() IconSet {
	defaultIconOnce.Do(func() {
		set, err := ToIconSet(DefaultIcon(IconSizeLarge))
		if err != nil {
			// 理论上不可达；真出问题也要保证安装流程不断，返回空集由上层兜底。
			slog.Error("生成默认图标失败", "error", err)
			return
		}
		defaultIconCache = set
	})
	return defaultIconCache
}

// ---------------------------------------------------------------------------
// 解析
// ---------------------------------------------------------------------------

// IconRequest 描述一次图标解析的输入。
type IconRequest struct {
	Icon domain.Icon
	// UploadDir 是用户上传图标的存放目录。
	UploadDir string
	// Candidates 是自动匹配官方图标时可用的关键词（镜像名、容器名、服务名、标题）。
	Candidates []string
	// SiteURL 是这条链接指向的站点地址（带 scheme）。官方图标库匹配不上时，
	// 会拿它去要站点自己的 favicon —— 内网服务因此也能拿到自己的图标，
	// 而这是公开图标库里永远不会有的一条。
	SiteURL string
	// SkipTLSVerify 让 favicon 抓取接受自签证书（内网服务的常态）。
	SkipTLSVerify bool
	// AllowRemote 控制是否允许发起外网请求（离线环境应关闭）。
	AllowRemote bool
}

// IconSource 说明一枚图标最终是从哪一档拿到的。
//
// 这串值会原样出现在 API 响应里，前端据此决定显示什么文案。
type IconSource string

const (
	// IconSourceUpload 用户上传的图片。
	IconSourceUpload IconSource = "upload"
	// IconSourceText 用文字渲染出来的。
	IconSourceText IconSource = "text"
	// IconSourceInline 内联的 data URI / 裸 base64。
	IconSourceInline IconSource = "inline"
	// IconSourceURL 用户直接填的远端图片地址。
	IconSourceURL IconSource = "url"
	// IconSourceOfficial 按关键词命中官方图标库。
	IconSourceOfficial IconSource = "official"
	// IconSourceFavicon 从目标站点自己那儿抓到的 favicon。
	IconSourceFavicon IconSource = "favicon"
	// IconSourceDefault 前面都没命中，落到内置默认图标。
	IconSourceDefault IconSource = "default"
)

// IconResolution 是一次图标解析的完整结果：图标本身 **加上它是怎么来的**。
//
// 为什么非要把"怎么来的"也返回：图标解析是一条七档降级链，用户看到一张
// 默认图标时，背后的原因可能是"还没去找""找过但站点没响应""离线模式不允许
// 联网"这三种完全不同的情况。只回一枚图标，前端就只能显示一张图 ——
// 用户看不出这里到底发生过什么，于是得到「这个图标功能没反应」的观感。
// 把来源与失败原因一起回传，这条链才第一次对用户可见。
type IconResolution struct {
	IconSet

	Source IconSource
	// Detail 是一句给用户看的话：命中了什么关键词、问了哪个地址、为什么没找到。
	Detail string
	// Site 是本次去要 favicon 的站点地址（没走到那一档时为空）。
	Site string
	// Elapsed 是这次解析实际花掉的时间，用于向用户交代"确实去找过了"。
	Elapsed time.Duration
}

// ResolveIcon 按优先级解析出最终图标：
//
//	上传文件 → 文字渲染 → data URI / 裸 base64 → 远端 URL
//	→ 官方图标库自动匹配 → 站点自己的 favicon → 内置默认图标
//
// favicon 排在官方图标库**之后**：图标库那一套是人工挑选的，质量稳定；
// favicon 是"退而求其次但仍然对得上"的一档，只在前面都没命中时才去问 ——
// 这样既不用为每个链接都发一次站点请求，也不会拿一张 16×16 的
// favicon 去顶掉一枚干净的官方图标。
//
// 任何一步失败都不会返回错误：图标永远能拿到，只是依次降级。
// 这是刻意的设计——「桌面图标长得不对」远不如「整个安装失败」严重。
//
// 只要图标不要来源时用这个；需要向用户解释过程时用 ResolveIconEx。
func ResolveIcon(req IconRequest) IconSet { return ResolveIconEx(req).IconSet }

// ResolveIconEx 是 ResolveIcon 的完整版，额外给出图标来源与失败原因。
func ResolveIconEx(req IconRequest) IconResolution {
	start := time.Now()
	done := func(set IconSet, src IconSource, detail string) IconResolution {
		return IconResolution{
			IconSet: set, Source: src, Detail: detail,
			Site: req.SiteURL, Elapsed: time.Since(start),
		}
	}

	if img := loadFromUpload(req.Icon, req.UploadDir); img != nil {
		if set, err := ToIconSet(img); err == nil {
			return done(set, IconSourceUpload, "使用你上传的图片")
		}
		slog.Warn("上传图标解析失败，继续降级", "ref", req.Icon.Ref)
	}

	if req.Icon.Source == domain.IconText {
		if set, err := TextIconSet(req.Icon.Text, req.Icon.TextColor, req.Icon.BgColor); err == nil {
			return done(set, IconSourceText, "使用文字生成的图标")
		}
	}

	if req.AllowRemote {
		if img := loadFromInline(req.Icon.Ref); img != nil {
			if set, err := ToIconSet(img); err == nil {
				return done(set, IconSourceInline, "使用内联的图标数据")
			}
		}
		if img := loadFromURL(req.Icon.Ref); img != nil {
			if set, err := ToIconSet(img); err == nil {
				return done(set, IconSourceURL, "已从你填写的网址下载图标")
			}
		}
		if set, keyword, ok := matchOfficialIconKeyword(req.Candidates); ok {
			return done(set, IconSourceOfficial,
				fmt.Sprintf("命中官方图标库（关键词 %s）", keyword))
		}
		if set, detail, ok := siteFaviconLookup(req.SiteURL, req.SkipTLSVerify); ok {
			return done(set, IconSourceFavicon, detail)
		}
		// 走到了这里说明"找过了但没找到"。把原因说清楚，别让用户以为没去找。
		if req.Icon.Source == domain.IconText {
			if set, err := TextIconSet(req.Icon.Text, req.Icon.TextColor, req.Icon.BgColor); err == nil {
				return done(set, IconSourceText, "使用文字生成的图标")
			}
		}
		return done(DefaultIconSet(), IconSourceDefault, faviconMissDetail(req.SiteURL))
	}

	// 文字图标即便在离线模式下也应生效，这里再兜一次。
	if req.Icon.Source == domain.IconText {
		if set, err := TextIconSet(req.Icon.Text, req.Icon.TextColor, req.Icon.BgColor); err == nil {
			return done(set, IconSourceText, "使用文字生成的图标")
		}
	}

	return done(DefaultIconSet(), IconSourceDefault,
		"远端图标已关闭，未去站点查找；如需自动抓取请在设置里打开「允许远端图标」")
}

// faviconMissDetail 解释"为什么没抓到站点图标"。
//
// 空地址与有地址是两种完全不同的处境：前者是链接本身没有可问的地址
// （比如反向代理还没填后端），后者是真的问了但对方没给。混成一句话
// 会让人误以为程序没去试。
func faviconMissDetail(siteURL string) string {
	if strings.TrimSpace(siteURL) == "" {
		return "这条链接没有可用于查找图标的站点地址，已使用内置默认图标"
	}
	return fmt.Sprintf("已访问 %s 但没找到图标（页面未声明，根目录也没有 favicon.ico），已使用内置默认图标", siteURL)
}

func loadFromUpload(icon domain.Icon, dir string) image.Image {
	if icon.Source != domain.IconUpload || dir == "" {
		return nil
	}
	name := domain.SanitizeFilePart(filepath.Base(icon.Ref))
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return nil
	}
	return decodeImage(data)
}

// loadFromInline 处理 data:image/...;base64,xxx 与裸 base64。
func loadFromInline(ref string) image.Image {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil
	}

	payload := ""
	switch {
	case strings.HasPrefix(ref, "data:"):
		idx := strings.Index(ref, ",")
		if idx < 0 {
			return nil
		}
		payload = ref[idx+1:]
	default:
		// 只有看起来像 base64 的长串才尝试解码，避免把普通 URL / 文件名误判。
		if len(ref) < 64 || !looksLikeBase64(ref) {
			return nil
		}
		payload = ref
	}

	decoded, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		// 兼容 URL-safe 变体。
		decoded, err = base64.RawURLEncoding.DecodeString(payload)
		if err != nil {
			return nil
		}
	}
	return decodeImage(decoded)
}

func looksLikeBase64(s string) bool {
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			r == '+' || r == '/' || r == '=' || r == '-' || r == '_' || r == '\n' || r == '\r' {
			continue
		}
		return false
	}
	return true
}

func loadFromURL(ref string) image.Image {
	ref = strings.TrimSpace(ref)
	if !strings.HasPrefix(ref, "http://") && !strings.HasPrefix(ref, "https://") {
		return nil
	}
	data, err := fetchBytes(ref)
	if err != nil {
		slog.Debug("拉取远端图标失败", "url", ref, "error", err)
		return nil
	}
	return decodeImage(data)
}

// officialIconBudget 是官方图标库这一档的**总**预算。
//
// 这一档的请求数是乘法关系：候选数 × 关键词数 × 4 个 CDN 节点，一个都答不上
// 时最坏能有几十次请求。而 httpClient 的单次超时是 8 秒 —— 也就是说在这条
// 「用户正等着看图标」的路径上，最坏能挂好几分钟。真实场景就是这个：国内
// 网络访问 jsDelivr 经常整体不通，于是每一次候选都完整地等满超时。
//
// 卡总预算后行为变得可预期：最长等这么久，然后老老实实交给 favicon 那一档
// （对内网服务来说，那一档才是真正能命中的）。
const officialIconBudget = 6 * time.Second

// matchOfficialIcon 用候选关键词到图标镜像库自动匹配官方图标。
// 不需要知道命中哪个关键词时用它；要写给用户看的说明时用 ...Keyword 版本。
func matchOfficialIcon(candidates []string) (IconSet, bool) {
	set, _, ok := matchOfficialIconKeyword(candidates)
	return set, ok
}

// matchOfficialIconKeyword 额外返回命中的关键词，便于把结果讲给用户听。
func matchOfficialIconKeyword(candidates []string) (IconSet, string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), officialIconBudget)
	defer cancel()

	for _, raw := range candidates {
		for _, name := range iconKeywords(raw) {
			for _, tpl := range cdnTemplates {
				// 预算用尽就收工：剩下的候选只会在同一条不通的路上继续等。
				if ctx.Err() != nil {
					slog.Debug("官方图标库匹配超出总预算，转到下一档")
					return IconSet{}, "", false
				}
				data, err := fetchBytesCtx(ctx, fmt.Sprintf(tpl, name))
				if err != nil {
					continue
				}
				img := decodeImage(data)
				if img == nil {
					continue
				}
				set, err := ToIconSet(img)
				if err != nil {
					continue
				}
				slog.Info("自动匹配到官方服务图标", "keyword", name)
				return set, name, true
			}
		}
	}
	return IconSet{}, "", false
}

// iconKeywords 从一个原始名称里抽出可用于检索图标的候选关键词，按「越具体越靠前」排序。
//
// 例：linuxserver/qbittorrent:latest → ["qbittorrent", "linuxserver-qbittorrent"]
//
// 关键点：必须先剥掉 tag / digest 再拼接仓库路径，
// 否则 "linuxserver/qbittorrent:latest" 会被压成 "linuxserver-qbittorrentlatest"，
// 既匹配不到官方图标，也会污染日志。
func iconKeywords(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}

	// 1) 去掉 :tag 与 @sha256:... 部分。
	base := raw
	if i := strings.IndexAny(base, ":@"); i >= 0 {
		base = base[:i]
	}
	base = strings.Trim(base, "/")
	if base == "" {
		return nil
	}

	// 2) 末段是最具体的名字。
	name := base
	if i := strings.LastIndex(base, "/"); i >= 0 {
		name = base[i+1:]
	}

	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		s = domain.Slug(s)
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}

	add(name)
	// portainer-ce → portainer（很多镜像的官方图标用的是去后缀名）
	if i := strings.IndexAny(name, "-_."); i > 0 {
		add(name[:i])
	}
	// 带命名空间时，把整条路径压平再试一次。
	if strings.Contains(base, "/") {
		add(base)
	}
	return out
}

func fetchBytes(url string) ([]byte, error) {
	return fetchBytesCtx(context.Background(), url)
}

// fetchBytesCtx 的 ctx 让调用方能给**一整批**请求卡总预算，
// 而不是让每个请求各自等满自己的超时再叠加起来。
func fetchBytesCtx(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	// 限流，避免恶意 / 异常的远端地址把内存吃满。
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	return data, nil
}

func decodeImage(data []byte) image.Image {
	if len(data) == 0 {
		return nil
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil
	}
	return img
}

// SaveUpload 把上传的图标字节落到 iconsDir，返回安全的文件名。
func SaveUpload(iconsDir, name string, data []byte) (string, error) {
	if !looksLikeImage(data) {
		return "", fmt.Errorf("%w: 上传的文件不是可识别的图片", domain.ErrValidation)
	}
	if err := os.MkdirAll(iconsDir, 0o755); err != nil {
		return "", err
	}
	clean := domain.SanitizeFilePart(filepath.Base(name))
	if !strings.HasSuffix(strings.ToLower(clean), ".png") {
		clean = strings.TrimSuffix(clean, filepath.Ext(clean)) + ".png"
	}
	// 统一转成 PNG，避免前端拿到 webp 等飞牛不认的格式。
	img := decodeImage(data)
	if img == nil {
		return "", fmt.Errorf("%w: 图片解码失败", domain.ErrValidation)
	}
	encoded, err := EncodePNG(img)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(iconsDir, clean), encoded, 0o644); err != nil {
		return "", err
	}
	return clean, nil
}

func looksLikeImage(data []byte) bool {
	if len(data) < 8 {
		return false
	}
	// PNG
	if bytes.HasPrefix(data, []byte{0x89, 'P', 'N', 'G'}) {
		return true
	}
	// JPEG
	if bytes.HasPrefix(data, []byte{0xFF, 0xD8, 0xFF}) {
		return true
	}
	// GIF / BMP / WEBP
	if bytes.HasPrefix(data, []byte("GIF8")) || bytes.HasPrefix(data, []byte("BM")) {
		return true
	}
	if len(data) > 12 && bytes.Equal(data[0:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP")) {
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// 几何工具
// ---------------------------------------------------------------------------

// roundRectSDF 是圆角矩形的有符号距离场：内部为负，外部为正。
func roundRectSDF(px, py, cx, cy, hw, hh, r float64) float64 {
	dx := math.Abs(px-cx) - (hw - r)
	dy := math.Abs(py-cy) - (hh - r)
	if dx < 0 {
		dx = 0
	}
	if dy < 0 {
		dy = 0
	}
	outside := math.Hypot(dx, dy)
	inside := math.Min(math.Max(math.Abs(px-cx)-(hw-r), math.Abs(py-cy)-(hh-r)), 0)
	return outside + inside - r
}

// coverage 把 SDF 转成 0..1 的抗锯齿覆盖率。
func coverage(sdf float64) float64 {
	v := 0.5 - sdf
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func roundRectCoverage(px, py, cx, cy, hw, hh, r float64) float64 {
	return coverage(roundRectSDF(px, py, cx, cy, hw, hh, r))
}

func darken(c color.RGBA, amount float64) color.RGBA {
	f := 1 - amount
	return color.RGBA{R: uint8(float64(c.R) * f), G: uint8(float64(c.G) * f), B: uint8(float64(c.B) * f), A: c.A}
}

func putOpaque(img *image.RGBA, x, y int, c color.RGBA) {
	if x < 0 || y < 0 || x >= img.Bounds().Dx() || y >= img.Bounds().Dy() {
		return
	}
	img.SetRGBA(x, y, c)
}

// 保证 draw 包被使用（PadToSquare 依赖它），同时保留扩展余地。
var _ = draw.Src
