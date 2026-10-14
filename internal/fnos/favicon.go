package fnos

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// 站点图标（favicon）
// ---------------------------------------------------------------------------
//
// 「自动」图标模式的最后一档：当镜像名匹配不到官方图标库、用户也没指定
// 文字 / 网址 / 上传时，**去目标站点自己那儿要一枚图标**。
//
// 顺序是：页面里声明的 <link rel="icon"> / apple-touch-icon → /favicon.ico。
// 为什么不只试 /favicon.ico：现代站点往往只声明 PNG（甚至 SVG）而不放根目录的
// .ico；反过来，老站点只有 .ico 而没有任何声明。两条路都要走。
//
// 为什么不能只靠 URL 推断：用户填的是服务地址（可能是 192.168.x.x:8080），
// 所以这里做的是「按 Web 惯例去问对方」，而不是查什么公开的图标数据库 ——
// 内网服务也能因此拿到自己的图标，这是它相对官方图标库的真正价值。

const (
	// faviconBudget 是一次 favicon 解析的**总**预算，而不是单个请求的超时。
	// 一个链接最多试几个候选地址，如果每个都单独设 5 秒，最坏会拖到二十几秒；
	// 而这段代码跑在「保存 / 放到桌面」这条用户正在等的路径上。
	// 用 context 卡总预算，无论试几个候选，到点就收工。
	faviconBudget = 5 * time.Second
	// 页面只读前面这一截：图标声明一定在 <head> 里，正文再大也与图标无关。
	faviconHTMLLimit = 512 << 10
	// 图标本身不会很大，给足余量即可。
	faviconImageLimit = 4 << 20
	// 候选上限：再多也只是把预算耗在必然失败的地址上。
	faviconMaxCandidates = 4
	// 缓存条数。favicon 在一次会话里几乎不变，缓存的价值在于
	// 「桌面图标对账」这类会反复解析同一批链接的流程。
	faviconCacheCap = 64
)

// userAgent 有些站点会拒绝空 UA 的请求，给一个正常的浏览器标识。
const faviconUserAgent = "Mozilla/5.0 (compatible; QLink2Desktop/1.0; +favicon)"

// ---------------------------------------------------------------------------
// 抓取
// ---------------------------------------------------------------------------

var (
	faviconClientsMu sync.Mutex
	faviconClients   = map[bool]*http.Client{}
)

// faviconClient 按「是否跳过证书校验」缓存客户端。
//
// 内网服务用自签证书是常态，而这类站点恰恰是最需要抓 favicon 的
// （它们不会有官方图标库条目）。链接上开了「忽略自签证书」就跟着开。
func faviconClient(insecure bool) *http.Client {
	faviconClientsMu.Lock()
	defer faviconClientsMu.Unlock()
	if c, ok := faviconClients[insecure]; ok {
		return c
	}
	tr := &http.Transport{
		// 自签证书是内网服务的常态，且必须由用户在链接上显式开启才走到这里。
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: insecure}, //nolint:gosec
		ResponseHeaderTimeout: faviconBudget,
	}
	c := &http.Client{
		Transport: tr,
		Timeout:   faviconBudget,
		// 默认允许 10 跳。图标抓取不需要跟这么深，而且每一跳都在烧预算。
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
	faviconClients[insecure] = c
	return c
}

type faviconResult struct {
	set IconSet
	ok  bool
	// detail 说明这次结果是怎么来的 —— 成功时是取到图标的那条地址，
	// 失败时是"问了但对方没给"。它随缓存一起留存，于是重复查询
	// 也能给出同样的解释，而不是第二次就变成一句干巴巴的"没找到"。
	detail string
}

var (
	faviconCacheMu    sync.Mutex
	faviconCache      = map[string]faviconResult{}
	faviconCacheOrder []string
)

// siteFaviconSet 抓取并缓存「站点自己的图标」。第二个返回值表示是否真的拿到了。
//
// 失败结果同样进缓存：一个没有 favicon 的地址会被反复解析（每次对账都会），
// 不记下"这里什么都没有"就等于每次都白等一次超时。
func siteFaviconSet(siteURL string, insecure bool) (IconSet, bool) {
	set, _, ok := siteFaviconLookup(siteURL, insecure)
	return set, ok
}

// siteFaviconLookup 是 siteFaviconSet 的完整版，额外返回一句可展示的说明。
func siteFaviconLookup(siteURL string, insecure bool) (IconSet, string, bool) {
	siteURL = strings.TrimSpace(siteURL)
	if siteURL == "" {
		return IconSet{}, "这条链接没有可用于查找图标的站点地址", false
	}
	key := strings.ToLower(siteURL)

	faviconCacheMu.Lock()
	if hit, ok := faviconCache[key]; ok {
		faviconCacheMu.Unlock()
		return hit.set, hit.detail, hit.ok
	}
	faviconCacheMu.Unlock()

	res := faviconResult{
		detail: fmt.Sprintf("已访问 %s，但页面里没有图标声明、根目录也没有 favicon.ico", siteURL),
	}
	if img, from := fetchSiteFavicon(siteURL, insecure); img != nil {
		if set, err := ToIconSet(img); err == nil {
			res = faviconResult{
				set: set, ok: true,
				detail: fmt.Sprintf("已从站点取得图标：%s", from),
			}
			slog.Info("从站点抓到 favicon 作为桌面图标", "site", siteURL, "from", from)
		}
	}

	faviconCacheMu.Lock()
	if _, exists := faviconCache[key]; !exists {
		if len(faviconCacheOrder) >= faviconCacheCap {
			// 简单的 FIFO 淘汰：这里没有"热点"概念，
			// 保持实现简单、行为可预期比命中率更重要。
			oldest := faviconCacheOrder[0]
			faviconCacheOrder = faviconCacheOrder[1:]
			delete(faviconCache, oldest)
		}
		faviconCache[key] = res
		faviconCacheOrder = append(faviconCacheOrder, key)
	}
	faviconCacheMu.Unlock()

	return res.set, res.detail, res.ok
}

// fetchSiteFavicon 按「页面声明 → 根目录惯例」的顺序取一枚图标。
// 第二个返回值是真正取到图标的那条地址（失败时为空串），用来向用户交代来源。
func fetchSiteFavicon(siteURL string, insecure bool) (image.Image, string) {
	ctx, cancel := context.WithTimeout(context.Background(), faviconBudget)
	defer cancel()
	client := faviconClient(insecure)

	var candidates []string
	if page, pageURL := fetchPageHead(ctx, client, siteURL); page != "" {
		candidates = append(candidates, declaredIconURLs(page, pageURL)...)
	}
	if base := siteBaseURL(siteURL); base != "" {
		// 根目录的 /favicon.ico 是 Web 惯例的最后一道。
		// 注意取的是**根**，不是当前路径下 —— 带路径的服务（/admin）也把
		// 图标放在站点根上。
		candidates = append(candidates, base+"/favicon.ico")
	}

	seen := map[string]bool{}
	tried := 0
	for _, u := range candidates {
		if u == "" || seen[u] {
			continue
		}
		seen[u] = true
		if tried >= faviconMaxCandidates || ctx.Err() != nil {
			break
		}
		tried++
		if img := fetchImageBytes(ctx, client, u); img != nil {
			return img, u
		}
	}
	return nil, ""
}

// fetchPageHead 取页面开头一段，用于读图标声明。失败不算错 —— 接下来还会试
// /favicon.ico，那才是覆盖面最广的一条路。
func fetchPageHead(ctx context.Context, client *http.Client, pageURL string) (string, *url.URL) {
	parsed, err := url.Parse(pageURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return "", nil
	}
	req.Header.Set("User-Agent", faviconUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")

	resp, err := client.Do(req)
	if err != nil {
		slog.Debug("读取页面以寻找图标声明失败", "url", pageURL, "error", err)
		return "", parsed
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", parsed
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, faviconHTMLLimit))
	if err != nil && len(body) == 0 {
		return "", parsed
	}
	return string(body), parsed
}

func fetchImageBytes(ctx context.Context, client *http.Client, rawURL string) image.Image {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("User-Agent", faviconUserAgent)
	req.Header.Set("Accept", "image/*,*/*;q=0.8")

	resp, err := client.Do(req)
	if err != nil {
		slog.Debug("拉取站点图标失败", "url", rawURL, "error", err)
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, faviconImageLimit))
	if err != nil && len(data) == 0 {
		return nil
	}
	return decodeIconImage(data)
}

// siteBaseURL 把任意站点地址归一成 scheme://host[:port]，失败返回空串。
func siteBaseURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		// 用户可能只填了 example.com —— 按 https 试，与 ShortcutURL 的默认一致。
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// ---------------------------------------------------------------------------
// 页面里的图标声明
// ---------------------------------------------------------------------------

var (
	htmlLinkTagRe = regexp.MustCompile(`(?is)<link\b[^>]*>`)
	htmlMetaTagRe = regexp.MustCompile(`(?is)<meta\b[^>]*>`)
	htmlAttrRe    = regexp.MustCompile(`(?is)([a-zA-Z_:][-a-zA-Z0-9_:.]*)\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'>]+))`)
)

// declaredIconURLs 从页面 HTML 里抽出图标地址，并解析成绝对地址。
//
// 用手写扫描而不是引入 HTML 解析库：本项目的依赖表里只有标准库，
// 而这里要读的东西极其有限 —— <link>/<meta> 标签 + 属性对。
// 引入一个解析库只为了这个，收益远不及它带来的依赖与体积成本。
func declaredIconURLs(page string, pageURL *url.URL) []string {
	var touch, plain, tile []string

	for _, tag := range htmlLinkTagRe.FindAllString(page, -1) {
		attrs := parseHTMLAttrs(tag)
		rel := strings.ToLower(attrs["rel"])
		href := attrs["href"]
		if rel == "" || href == "" {
			continue
		}
		switch {
		// apple-touch-icon 一般就是 180×180 的 PNG，比 16×16 的 .ico
		// 缩放出来干净得多，所以排在前面。
		case strings.Contains(rel, "apple-touch-icon"):
			touch = append(touch, href)
		case strings.Contains(rel, "icon"):
			// mask-icon 是给浏览器 UI 用的单色 SVG，标准库解不了，直接跳过。
			if strings.Contains(rel, "mask-icon") {
				continue
			}
			plain = append(plain, href)
		}
	}

	for _, tag := range htmlMetaTagRe.FindAllString(page, -1) {
		attrs := parseHTMLAttrs(tag)
		if strings.EqualFold(strings.TrimSpace(attrs["name"]), "msapplication-TileImage") {
			if c := attrs["content"]; c != "" {
				tile = append(tile, c)
			}
		}
	}

	out := make([]string, 0, len(touch)+len(plain)+len(tile))
	for _, raw := range append(append(touch, plain...), tile...) {
		if abs := resolveIconHref(raw, pageURL); abs != "" {
			out = append(out, abs)
		}
	}
	return out
}

func parseHTMLAttrs(tag string) map[string]string {
	attrs := map[string]string{}
	for _, m := range htmlAttrRe.FindAllStringSubmatch(tag, -1) {
		name := strings.ToLower(m[1])
		// FindAllStringSubmatch 里分组 2/3/4 分别对应双引号 / 单引号 / 裸值，
		// 未命中的那些是空串，按顺序取第一个非空的即可。
		value := m[2]
		if value == "" {
			value = m[3]
		}
		if value == "" {
			value = m[4]
		}
		if _, exists := attrs[name]; !exists {
			attrs[name] = value
		}
	}
	return attrs
}

// resolveIconHref 把 href 解析成可以发起请求的绝对地址；不适用时返回空串。
//
// data: URI 直接跳过：它要解码成图得先剥一层 base64，而站点的 favicon
// 极少内联在 HTML 里，不值得为这条路加一段单独的解析。
func resolveIconHref(href string, pageURL *url.URL) string {
	href = htmlUnescape(strings.TrimSpace(href))
	if href == "" || strings.HasPrefix(href, "data:") {
		return ""
	}
	if pageURL == nil {
		if strings.HasPrefix(href, "http://") || strings.HasPrefix(href, "https://") {
			return href
		}
		return ""
	}
	ref, err := url.Parse(href)
	if err != nil {
		return ""
	}
	abs := pageURL.ResolveReference(ref)
	if abs.Scheme != "http" && abs.Scheme != "https" {
		return ""
	}
	return abs.String()
}

// htmlUnescape 只处理属性值里真正会出现的几个实体。
// 完整实现要一张几百项的实体表，而 favicon 的 href 里能出现的只有这几种。
var htmlEntities = strings.NewReplacer(
	"&amp;", "&",
	"&#38;", "&",
	"&quot;", `"`,
	"&#39;", "'",
	"&apos;", "'",
	"&lt;", "<",
	"&gt;", ">",
)

func htmlUnescape(s string) string { return htmlEntities.Replace(s) }

// ---------------------------------------------------------------------------
// 解码
// ---------------------------------------------------------------------------

// decodeIconImage 解一枚站点图标。
//
// 先试 ICO：/favicon.ico 绝大多数就是 ICO 格式，而标准库**没有** ICO 解码器
// （image.Decode 会直接报 unknown format），不自己解的话这条最主流的路径
// 会全程失败 —— 那正是这个功能最容易"看起来没生效"的原因。
func decodeIconImage(data []byte) image.Image {
	if img := decodeICO(data); img != nil {
		return img
	}
	return decodeImage(data)
}

// decodeICO 从 ICO 容器里取出**尺寸最大**的那一档并解码。
//
// 同一枚 favicon.ico 里通常并排放着 16/32/48 甚至 256 各一档。取最大的是刻意的：
// 桌面图标要按 64 / 256 两档输出，用 16×16 放大上去会糊成一团。
func decodeICO(data []byte) image.Image {
	// ICONDIR：reserved(2)=0, type(2)=1（1=图标，2=光标）, count(2)
	if len(data) < 6 || data[0] != 0 || data[1] != 0 || data[2] != 1 || data[3] != 0 {
		return nil
	}
	count := int(binary.LittleEndian.Uint16(data[4:6]))
	if count <= 0 {
		return nil
	}

	type entry struct {
		w, h, bpp, size, offset int
	}
	entries := make([]entry, 0, count)
	for i := 0; i < count; i++ {
		off := 6 + i*16
		if off+16 > len(data) {
			break
		}
		w, h := int(data[off]), int(data[off+1])
		// 目录项里 0 表示 256 —— 一个字节放不下 256。
		if w == 0 {
			w = 256
		}
		if h == 0 {
			h = 256
		}
		entries = append(entries, entry{
			w:      w,
			h:      h,
			bpp:    int(binary.LittleEndian.Uint16(data[off+6 : off+8])),
			size:   int(binary.LittleEndian.Uint32(data[off+8 : off+12])),
			offset: int(binary.LittleEndian.Uint32(data[off+12 : off+16])),
		})
	}

	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].w*entries[i].h > entries[j].w*entries[j].h
	})

	for _, e := range entries {
		if e.offset < 0 || e.offset >= len(data) {
			continue
		}
		end := e.offset + e.size
		if e.size <= 0 || end > len(data) {
			end = len(data)
		}
		if img := decodeICOEntry(data[e.offset:end], e.w, e.h); img != nil {
			return img
		}
	}
	return nil
}

func decodeICOEntry(blob []byte, w, h int) image.Image {
	// 现代做法：直接把一张 PNG 塞进 ICO 里。
	// image.Decode 对 DIB 头（首字节 0x28）不会误判，可以放心先试。
	if img := decodeImage(blob); img != nil {
		return img
	}
	return decodeDIB(blob, w, h)
}

// decodeDIB 解 ICO 内嵌的「设备无关位图」（BMP 去掉文件头的样子）。
//
// 与 .bmp 的差别有一处最容易踩：**高度是整个图像的两倍**，
// 多出来的那一半是 1 位 AND 掩码（老式的透明通道，1 = 透明）。
// 32 位图自带 alpha，掩码通常被忽略；24 位及以下必须靠它，
// 否则图标边缘会糊出一圈黑框。
func decodeDIB(b []byte, w, h int) image.Image {
	const headerSize = 40
	if len(b) < headerSize || w <= 0 || h <= 0 {
		return nil
	}
	hdr := int(binary.LittleEndian.Uint32(b[0:4]))
	if hdr < headerSize || hdr > len(b) {
		return nil
	}
	bpp := int(binary.LittleEndian.Uint16(b[14:16]))
	if bpp != 1 && bpp != 4 && bpp != 8 && bpp != 24 && bpp != 32 {
		return nil
	}
	// 只认不压缩与 BI_BITFIELDS；其余（RLE 等）在 ICO 里基本不出现。
	if comp := int(binary.LittleEndian.Uint32(b[16:20])); comp != 0 && comp != 3 {
		return nil
	}

	palCount := 0
	if bpp <= 8 {
		palCount = 1 << bpp
	}
	palBytes := palCount * 4
	pixels := hdr + palBytes
	stride := ((bpp*w + 31) / 32) * 4
	andStride := ((w + 31) / 32) * 4
	if pixels+stride*h > len(b) {
		return nil
	}

	var palette []color.RGBA
	if palCount > 0 {
		if hdr+palBytes > len(b) {
			return nil
		}
		palette = make([]color.RGBA, palCount)
		for i := 0; i < palCount; i++ {
			o := hdr + i*4
			palette[i] = color.RGBA{R: b[o+2], G: b[o+1], B: b[o], A: 255}
		}
	}

	img := image.NewRGBA(image.Rect(0, 0, w, h))
	andBase := pixels + stride*h
	anyAlpha := false

	for y := 0; y < h; y++ {
		// DIB 的行是**自下而上**存的。
		src := pixels + (h-1-y)*stride
		and := andBase + (h-1-y)*andStride
		for x := 0; x < w; x++ {
			var c color.RGBA
			switch bpp {
			case 32:
				o := src + x*4
				c = color.RGBA{R: b[o+2], G: b[o+1], B: b[o], A: b[o+3]}
			case 24:
				o := src + x*3
				c = color.RGBA{R: b[o+2], G: b[o+1], B: b[o], A: 255}
			case 8:
				c = palette[int(b[src+x])]
			case 4:
				v := b[src+x/2]
				idx := int(v >> 4)
				if x%2 == 1 {
					idx = int(v & 0x0f)
				}
				c = palette[idx]
			case 1:
				idx := int((b[src+x/8] >> (7 - uint(x%8))) & 1)
				c = palette[idx]
			}
			// AND 掩码：1 表示透明。32 位图自带 alpha，不叠加。
			if bpp != 32 && and+andStride <= len(b) {
				if (b[and+x/8]>>(7-uint(x%8)))&1 == 1 {
					c.A = 0
				}
			}
			if c.A > 0 {
				anyAlpha = true
			}
			img.SetRGBA(x, y, c)
		}
	}

	// 有些老工具生成的 32 位 ICO 完全不填 alpha（整张全 0），
	// 照原样用会得到一个全透明的图标。这种情况按「不透明」处理。
	if bpp == 32 && !anyAlpha {
		for i := 3; i < len(img.Pix); i += 4 {
			img.Pix[i] = 255
		}
	}
	return img
}
