package fnos

import (
	"encoding/binary"
	"image"
	"image/color"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MisiteQ/qlink2desktop/internal/domain"
)

// ---------------------------------------------------------------------------
// 造样本
// ---------------------------------------------------------------------------

// solidPNG 造一张纯色 PNG。
func solidPNG(t *testing.T, w, h int, c color.RGBA) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, c)
		}
	}
	data, err := EncodePNG(img)
	if err != nil {
		t.Fatalf("生成 PNG 失败: %v", err)
	}
	return data
}

// dibHeader 造一段 BITMAPINFOHEADER。height 传 2*h 才是合法 ICO 内容的写法，
// 但为了让测试能单独覆盖"高度不是两倍"的畸形输入，这里留给调用方决定。
func dibHeader(w, height, bpp int) []byte {
	b := make([]byte, 40)
	binary.LittleEndian.PutUint32(b[0:4], 40)
	binary.LittleEndian.PutUint32(b[4:8], uint32(w))
	binary.LittleEndian.PutUint32(b[8:12], uint32(height))
	binary.LittleEndian.PutUint16(b[12:14], 1)
	binary.LittleEndian.PutUint16(b[14:16], uint16(bpp))
	return b
}

// buildICO 把一档图像封进 ICO 容器。
func buildICO(w, h, bpp int, blob []byte) []byte {
	dir := make([]byte, 6)
	binary.LittleEndian.PutUint16(dir[2:4], 1) // type = icon
	binary.LittleEndian.PutUint16(dir[4:6], 1) // count = 1

	entry := make([]byte, 16)
	entry[0] = byte(w % 256)
	entry[1] = byte(h % 256)
	binary.LittleEndian.PutUint16(entry[6:8], uint16(bpp))
	binary.LittleEndian.PutUint32(entry[8:12], uint32(len(blob)))
	binary.LittleEndian.PutUint32(entry[12:16], uint32(6+16))

	return append(append(dir, entry...), blob...)
}

// dib32 造一张 32 位 DIB（每行自下而上、行尾补齐 4 字节）。
func dib32(w, h int, px func(x, y int) color.RGBA) []byte {
	out := append([]byte{}, dibHeader(w, h*2, 32)...)
	stride := w * 4
	for y := h - 1; y >= 0; y-- { // 自下而上
		for x := 0; x < w; x++ {
			c := px(x, y)
			out = append(out, c.B, c.G, c.R, c.A)
		}
	}
	_ = stride
	// AND 掩码：全 0（不透明），长度为 andStride*h。
	out = append(out, make([]byte, ((w+31)/32)*4*h)...)
	return out
}

// ---------------------------------------------------------------------------
// 地址归一
// ---------------------------------------------------------------------------

func TestSiteBaseURL(t *testing.T) {
	t.Parallel()

	cases := []struct{ in, want string }{
		{"https://example.com/a/b?c=1", "https://example.com"},
		{"http://192.168.1.5:8080/admin", "http://192.168.1.5:8080"},
		// 没写 scheme 时按 https 试，与 ShortcutURL 的默认保持一致。
		{"example.com", "https://example.com"},
		// 非 http(s) 一律拒绝：不能拿 file:// 之类的东西去发请求。
		{"ftp://example.com", ""},
		{"", ""},
		{"   ", ""},
	}
	for _, c := range cases {
		if got := siteBaseURL(c.in); got != c.want {
			t.Errorf("siteBaseURL(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// 页面声明解析
// ---------------------------------------------------------------------------

func TestParseHTMLAttrs(t *testing.T) {
	t.Parallel()

	// 属性顺序、引号风格都不固定 —— 三种写法都要认。
	for _, tag := range []string{
		`<link rel="icon" href="/a.png">`,
		`<link href='/a.png' rel='icon'>`,
		`<link rel=icon href=/a.png>`,
	} {
		attrs := parseHTMLAttrs(tag)
		if attrs["rel"] != "icon" || attrs["href"] != "/a.png" {
			t.Errorf("解析 %q 得到 %#v", tag, attrs)
		}
	}
}

func TestDeclaredIconURLs(t *testing.T) {
	t.Parallel()

	page := `<html><head>
		<link rel="icon" href="/fav16.png">
		<link rel="apple-touch-icon" sizes="180x180" href="touch.png">
		<link rel="mask-icon" href="/mask.svg">
		<link rel="icon" href="data:image/png;base64,AAAA">
		<meta name="msapplication-TileImage" content="https://cdn.example.com/tile.png">
	</head></html>`

	pageURL, _ := url.Parse("https://example.com/sub/page.html")
	got := declaredIconURLs(page, pageURL)

	// apple-touch-icon 排在最前（通常是 180×180 的 PNG，缩放质量最好）；
	// mask-icon（标准库解不了的 SVG）与 data: URI 直接跳过。
	want := []string{
		"https://example.com/sub/touch.png",
		"https://example.com/fav16.png",
		"https://cdn.example.com/tile.png",
	}
	if len(got) != len(want) {
		t.Fatalf("候选数量 %d，期望 %d：%v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("候选[%d] = %q，期望 %q", i, got[i], want[i])
		}
	}
}

func TestDeclaredIconURLsIgnoresNonIconLinks(t *testing.T) {
	t.Parallel()

	page := `<link rel="stylesheet" href="/a.css"><link rel="preload" href="/b.js">`
	if got := declaredIconURLs(page, nil); len(got) != 0 {
		t.Fatalf("不该把样式表 / 预加载当成图标：%v", got)
	}
}

// ---------------------------------------------------------------------------
// ICO 解码（标准库没有这个能力，必须自己守）
// ---------------------------------------------------------------------------

func TestDecodeICOPngEntry(t *testing.T) {
	t.Parallel()

	png := solidPNG(t, 8, 8, color.RGBA{R: 0x11, G: 0x99, B: 0x33, A: 255})
	img := decodeICO(buildICO(8, 8, 32, png))
	if img == nil {
		t.Fatal("ICO 里内嵌的 PNG 应当能解出来")
	}
	r, g, b, _ := img.At(4, 4).RGBA()
	if r>>8 != 0x11 || g>>8 != 0x99 || b>>8 != 0x33 {
		t.Fatalf("像素颜色不对：%02x%02x%02x", r>>8, g>>8, b>>8)
	}
}

func TestDecodeICO32BitDIB(t *testing.T) {
	t.Parallel()

	blob := dib32(4, 4, func(x, y int) color.RGBA {
		if x < 2 {
			return color.RGBA{R: 0xFF, A: 255}
		}
		return color.RGBA{B: 0xFF, A: 255}
	})
	img := decodeICO(buildICO(4, 4, 32, blob))
	if img == nil {
		t.Fatal("32 位 DIB 应当能解出来")
	}
	if got := img.Bounds().Dx(); got != 4 {
		t.Fatalf("宽度 = %d，期望 4", got)
	}
	// 左半边红、右半边蓝 —— 顺带验证「行是自下而上存」这件事没搞反。
	if r, _, b, _ := img.At(0, 0).RGBA(); r>>8 != 0xFF || b>>8 != 0 {
		t.Fatalf("左上角应为红色，实际 %04x/%04x", r>>8, b>>8)
	}
	if r, _, b, _ := img.At(3, 3).RGBA(); b>>8 != 0xFF || r>>8 != 0 {
		t.Fatalf("右下角应为蓝色，实际 %04x/%04x", r>>8, b>>8)
	}
}

func TestDecodeICO32BitAllZeroAlphaTreatedOpaque(t *testing.T) {
	t.Parallel()

	// 老工具生成的 32 位 ICO 常常整张 alpha 都是 0；
	// 照原样用会得到一枚全透明的图标（在桌面上就是"没有图标"）。
	blob := dib32(4, 4, func(int, int) color.RGBA { return color.RGBA{R: 0x20, G: 0x40, B: 0x60} })
	img := decodeICO(buildICO(4, 4, 32, blob))
	if img == nil {
		t.Fatal("应当能解出来")
	}
	if a := img.At(1, 1); func() uint32 { _, _, _, al := a.RGBA(); return al >> 8 }() != 0xFF {
		t.Fatal("alpha 全为 0 的 32 位 ICO 应当按不透明处理")
	}
}

func TestDecodeICO8BitPaletteUsesAndMask(t *testing.T) {
	t.Parallel()

	const w, h = 8, 2
	blob := append([]byte{}, dibHeader(w, h*2, 8)...)
	// 调色板：0 = 红，1 = 蓝
	for i := 0; i < 256; i++ {
		c := []byte{0, 0, 0, 0}
		if i == 0 {
			c = []byte{0, 0, 0xFF, 0} // BGR
		} else if i == 1 {
			c = []byte{0xFF, 0, 0, 0}
		}
		blob = append(blob, c...)
	}
	stride := ((8*w + 31) / 32) * 4
	for y := 0; y < h; y++ {
		row := make([]byte, stride)
		for x := 0; x < w; x++ {
			if x < 4 {
				row[x] = 0
			} else {
				row[x] = 1
			}
		}
		blob = append(blob, row...)
	}
	// AND 掩码：第 0 行（自上而下）的后四个像素置透明。
	andStride := ((w + 31) / 32) * 4
	blob = append(blob, 0x0F, 0) // 自下而上的第 0 行 → 屏幕上的最后一行
	blob = append(blob, make([]byte, andStride-2)...)

	img := decodeICO(buildICO(w, h, 8, blob))
	if img == nil {
		t.Fatal("8 位调色板 ICO 应当能解出来")
	}
	if r, _, _, a := img.At(0, 1).RGBA(); r>>8 != 0xFF || a>>8 != 0xFF {
		t.Fatalf("左下角应为不透明的红，实际 r=%02x a=%02x", r>>8, a>>8)
	}
	if _, _, _, a := img.At(7, 1).RGBA(); a>>8 != 0 {
		t.Fatalf("被 AND 掩码标记的像素应当透明，实际 a=%02x", a>>8)
	}
}

func TestDecodeICOPicksLargestEntry(t *testing.T) {
	t.Parallel()

	// 同一枚 favicon 里并排放着 16 与 32 两档，必须挑大的 ——
	// 用 16×16 放大到 256 会糊成一团。
	small := solidPNG(t, 16, 16, color.RGBA{R: 0xFF, A: 255})
	large := solidPNG(t, 32, 32, color.RGBA{B: 0xFF, A: 255})

	dir := make([]byte, 6)
	binary.LittleEndian.PutUint16(dir[2:4], 1)
	binary.LittleEndian.PutUint16(dir[4:6], 2)

	mk := func(w, h int, blob []byte, offset int) []byte {
		e := make([]byte, 16)
		e[0], e[1] = byte(w), byte(h)
		binary.LittleEndian.PutUint16(e[6:8], 32)
		binary.LittleEndian.PutUint32(e[8:12], uint32(len(blob)))
		binary.LittleEndian.PutUint32(e[12:16], uint32(offset))
		return e
	}
	entries := append(mk(16, 16, small, 6+32), mk(32, 32, large, 6+32+len(small))...)
	ico := append(append(dir, entries...), append(small, large...)...)

	img := decodeICO(ico)
	if img == nil {
		t.Fatal("应当能解出来")
	}
	if img.Bounds().Dx() != 32 {
		t.Fatalf("应当取最大的那一档（32），实际 %d", img.Bounds().Dx())
	}
}

func TestDecodeICOReturnsNilOnGarbage(t *testing.T) {
	t.Parallel()

	for _, in := range [][]byte{
		nil,
		[]byte("not an ico at all"),
		[]byte{0, 0, 1, 0}, // 头都不全
		append([]byte{0, 0, 1, 0, 0, 0}, make([]byte, 32)...), // count = 0
	} {
		if img := decodeICO(in); img != nil {
			t.Fatalf("垃圾输入不该解出图像：%v", in)
		}
	}
}

func TestDecodeIconImagePrefersICODecoder(t *testing.T) {
	t.Parallel()

	// image.Decode 对 ICO 会报 unknown format —— 这正是必须自己解的原因。
	blob := dib32(4, 4, func(int, int) color.RGBA { return color.RGBA{G: 0xAA, A: 255} })
	ico := buildICO(4, 4, 32, blob)
	if decodeImage(ico) != nil {
		t.Fatal("前提变了：标准库居然能解 ICO 了")
	}
	if decodeIconImage(ico) == nil {
		t.Fatal("decodeIconImage 应当走 ICO 分支")
	}
}

// ---------------------------------------------------------------------------
// 端到端：从站点抓
// ---------------------------------------------------------------------------

func TestFetchSiteFaviconFromDeclaredLink(t *testing.T) {
	t.Parallel()

	icon := solidPNG(t, 32, 32, color.RGBA{R: 0xAB, G: 0xCD, B: 0xEF, A: 255})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<head><link rel="icon" href="/brand.png"></head>`))
		case "/brand.png":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(icon)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	got, from := fetchSiteFavicon(srv.URL+"/", false)
	if got == nil {
		t.Fatal("应当从页面声明的 <link rel=icon> 抓到图标")
	}
	r, g, b, _ := got.At(1, 1).RGBA()
	if r>>8 != 0xAB || g>>8 != 0xCD || b>>8 != 0xEF {
		t.Fatalf("抓到的不是站点声明的图标：%02x%02x%02x", r>>8, g>>8, b>>8)
	}
	// 取到图标的那条地址要能回报给用户 —— 否则界面上只能说"找到了"，
	// 说不出"从哪儿找到的"，出问题时用户无从自查。
	if !strings.HasSuffix(from, "/brand.png") {
		t.Fatalf("应当回报真正取到图标的那条地址，实际 %q", from)
	}
}

func TestFetchSiteFaviconFallsBackToRootICO(t *testing.T) {
	t.Parallel()

	ico := buildICO(8, 8, 32, solidPNG(t, 8, 8, color.RGBA{R: 0x22, G: 0x44, B: 0x88, A: 255}))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 页面是个纯前端应用，HTML 里没有任何图标声明 ——
		// 这时根目录的 /favicon.ico 就是唯一线索（也很常见）。
		if r.URL.Path == "/favicon.ico" {
			w.Header().Set("Content-Type", "image/x-icon")
			_, _ = w.Write(ico)
			return
		}
		_, _ = w.Write([]byte(`<html><head><title>no icon here</title></head></html>`))
	}))
	defer srv.Close()

	got, from := fetchSiteFavicon(srv.URL+"/app/dashboard", false)
	if got == nil {
		t.Fatal("页面没声明图标时应当退到根目录 /favicon.ico")
	}
	r, _, b, _ := got.At(1, 1).RGBA()
	if r>>8 != 0x22 || b>>8 != 0x88 {
		t.Fatalf("抓到的不对：%02x/%02x", r>>8, b>>8)
	}
	// 关键：取的是**站点根**，不是当前路径下 —— 带路径的服务也把图标放根上。
	if !strings.HasSuffix(from, "/favicon.ico") || strings.Contains(from, "/app/") {
		t.Fatalf("应当取站点根目录的 /favicon.ico，实际 %q", from)
	}
}

func TestFetchSiteFaviconReturnsNilWhenNothingThere(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	if got, from := fetchSiteFavicon(srv.URL, false); got != nil || from != "" {
		t.Fatal("站点什么都没有时应返回 nil，由上层回落到内置默认图标")
	}
}

func TestResolveIconUsesSiteFaviconWhenLibraryMisses(t *testing.T) {
	t.Parallel()

	icon := solidPNG(t, 64, 64, color.RGBA{R: 0x07, G: 0x77, B: 0x07, A: 255})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/favicon.ico" {
			_, _ = w.Write(icon)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	// Candidates 传空：官方图标库那一档就不会发出任何外网请求，
	// 于是这次解析能干净地走到 favicon 这一档。
	got := ResolveIcon(IconRequest{
		Icon:        domain.Icon{Source: domain.IconAuto},
		Candidates:  nil,
		SiteURL:     srv.URL,
		AllowRemote: true,
	})
	if !got.Valid() {
		t.Fatal("应当拿到 favicon 并成功生成图标集")
	}
	img := mustDecode(t, got.Large)
	r, g, b, _ := img.At(128, 128).RGBA()
	if r>>8 != 0x07 || g>>8 != 0x77 || b>>8 != 0x07 {
		t.Fatalf("用到的应当是站点 favicon，实际 %02x%02x%02x", r>>8, g>>8, b>>8)
	}
}

func TestResolveIconSkipsFaviconWhenRemoteDisabled(t *testing.T) {
	t.Parallel()

	// 离线模式（AllowRemote=false）下绝不允许发请求 ——
	// 用户明确关掉远端图标时，一个"内网地址"看起来无害，但仍然是我们主动发起的连接。
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		http.NotFound(w, r)
	}))
	defer srv.Close()

	got := ResolveIcon(IconRequest{SiteURL: srv.URL})
	if !got.Valid() {
		t.Fatal("应当回落到内置默认图标")
	}
	if hit {
		t.Fatal("AllowRemote=false 时不该发起任何请求")
	}
}

func TestSiteFaviconSetCachesMisses(t *testing.T) {
	t.Parallel()

	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.NotFound(w, r)
	}))
	defer srv.Close()

	for i := 0; i < 3; i++ {
		if _, ok := siteFaviconSet(srv.URL, false); ok {
			t.Fatal("这个站点没有 favicon")
		}
	}
	// 失败结果也要缓存：否则每次桌面图标对账都会为同一个地址白等一次超时。
	if calls > 2 {
		t.Fatalf("同一个地址被重复抓取了 %d 次，缓存没生效", calls)
	}
}

func TestIconSiteURLPerKind(t *testing.T) {
	t.Parallel()

	// 都用 Normalize 过的链接 —— 落盘前一定会过这一步，测试要与真实输入一致。
	shortcut := domain.Link{Kind: domain.KindShortcut, Path: "https://example.com/x"}
	shortcut.Normalize()
	if got := iconSiteURL(shortcut); got != "https://example.com/x" {
		t.Errorf("快捷方式应指向它自己的网址，实际 %q", got)
	}

	// 本机端口形态不论 Host 填了什么，BackendURL 一律指向回环地址 ——
	// 这正是 favicon 要问的地址（服务就在本机）。
	local := domain.Link{Kind: domain.KindLocalPort, Scheme: "http", Host: "example.com", Port: 8096}
	local.Normalize()
	if got := iconSiteURL(local); got != "http://127.0.0.1:8096/" {
		t.Errorf("本机端口应指向回环地址，实际 %q", got)
	}

	// 没填地址时不能凭空造一个出来，要交给上层跳过这一档。
	if got := iconSiteURL(domain.Link{Kind: domain.KindProxy}); got != "" {
		t.Errorf("地址不全时应返回空串，实际 %q", got)
	}
}

// ---------------------------------------------------------------------------
// 解析结果必须能解释自己
// ---------------------------------------------------------------------------
//
// 下面这组测试守的是一件产品行为：用户看到一张图标时，界面要能说出
// **这枚图标是怎么来的**。没有这层信息，"没找到图标"和"根本没去找"
// 在界面上长得一模一样 —— 那正是用户报「图标功能没反应」的根源。

func TestResolveIconExReportsFaviconSource(t *testing.T) {
	t.Parallel()

	icon := solidPNG(t, 64, 64, color.RGBA{R: 0x11, G: 0x99, B: 0x33, A: 255})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/favicon.ico" {
			_, _ = w.Write(icon)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	res := ResolveIconEx(IconRequest{
		Icon:        domain.Icon{Source: domain.IconAuto},
		SiteURL:     srv.URL,
		AllowRemote: true,
	})
	if res.Source != IconSourceFavicon {
		t.Fatalf("来源应当是 favicon，实际 %q", res.Source)
	}
	// Detail 里要带出真正取到图标的地址，否则用户没法自查。
	if !strings.Contains(res.Detail, "/favicon.ico") {
		t.Fatalf("说明里应当写明从哪条地址取到图标，实际 %q", res.Detail)
	}
	if res.Site != srv.URL {
		t.Fatalf("应当回报本次询问的站点地址，实际 %q", res.Site)
	}
}

func TestResolveIconExExplainsMiss(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	res := ResolveIconEx(IconRequest{
		Icon:        domain.Icon{Source: domain.IconAuto},
		SiteURL:     srv.URL,
		AllowRemote: true,
	})
	if res.Source != IconSourceDefault {
		t.Fatalf("没抓到时应回落到默认，实际 %q", res.Source)
	}
	// 关键：必须说清"去问过了但对方没有"，而不是一句"没找到" ——
	// 后者与"压根没去问"在用户眼里毫无区别。
	if !strings.Contains(res.Detail, srv.URL) {
		t.Fatalf("说明里应当写明询问过的地址，实际 %q", res.Detail)
	}
}

func TestResolveIconExExplainsRemoteDisabled(t *testing.T) {
	t.Parallel()

	res := ResolveIconEx(IconRequest{Icon: domain.Icon{Source: domain.IconAuto}})
	if res.Source != IconSourceDefault {
		t.Fatalf("远端关闭时应回落到默认，实际 %q", res.Source)
	}
	if !strings.Contains(res.Detail, "允许远端图标") {
		t.Fatalf("应当说明是开关关掉了抓取，实际 %q", res.Detail)
	}
}

// ---------------------------------------------------------------------------
// 自动图标落盘
// ---------------------------------------------------------------------------
//
// 这一段守的是「列表里显示的那张缩略图」。后端本来就会把图标装到桌面，
// 但面板列表只认用户填的 Icon —— 自动档于是永远画内置默认图，
// 用户据此判断"图标没自动找到"。

func TestEnsureAutoIconLandsFaviconOnDisk(t *testing.T) {
	t.Parallel()

	icon := solidPNG(t, 64, 64, color.RGBA{R: 0x40, G: 0x80, B: 0xC0, A: 255})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/favicon.ico" {
			_, _ = w.Write(icon)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	dir := t.TempDir()
	svc := NewService(Options{CLI: &FakeCLI{}, IconsDir: dir, AllowRemoteIcon: true})
	l := domain.Link{ID: "item-abc123", Name: "演示", Kind: domain.KindShortcut, Path: srv.URL}
	l.Normalize()

	changed, res := svc.EnsureAutoIcon(l)
	if !changed {
		t.Fatal("第一次解析应当写下一个图标文件")
	}
	if res.Source != IconSourceFavicon {
		t.Fatalf("来源应当是 favicon，实际 %q", res.Source)
	}
	name := svc.AutoIconFile(l)
	if name == "" {
		t.Fatal("落盘之后应当能报出文件名，否则列表拿不到它")
	}
	if !IsAutoIconFile(name) {
		t.Fatalf("文件名要能被认成程序产物，实际 %q", name)
	}
	// 文件真的在，而且是一张能解得开的 PNG。
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("自动图标文件不存在：%v", err)
	}
	if mustDecode(t, data) == nil {
		t.Fatal("落盘的必须是可解码的图片")
	}
}

func TestEnsureAutoIconIsIdempotent(t *testing.T) {
	t.Parallel()

	icon := solidPNG(t, 64, 64, color.RGBA{R: 0x10, G: 0x20, B: 0x30, A: 255})
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path == "/favicon.ico" {
			_, _ = w.Write(icon)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	svc := NewService(Options{CLI: &FakeCLI{}, IconsDir: t.TempDir(), AllowRemoteIcon: true})
	l := domain.Link{ID: "item-idem", Name: "演示", Kind: domain.KindShortcut, Path: srv.URL}
	l.Normalize()

	if changed, _ := svc.EnsureAutoIcon(l); !changed {
		t.Fatal("第一次应当发生变化")
	}
	// 关键：第二次必须报"没变"。视图组装每次列表请求都会走到这里，
	// 若它恒定报 changed，服务端就会持续广播 links 事件、前端持续重拉 ——
	// 一个自己喂自己的循环。
	if changed, _ := svc.EnsureAutoIcon(l); changed {
		t.Fatal("同一组输入重复解析不该再报变化，否则会形成广播循环")
	}
	after := calls
	svc.EnsureAutoIcon(l)
	if calls != after {
		t.Fatalf("同一组输入不该重复发起网络请求：%d → %d", after, calls)
	}
	before := calls

	// 输入变了（换了地址）就必须重来一次，否则会一直挂着旧地址的图标。
	//
	// 断言的是"又去问了"，不是"文件变了" —— 换地址后取到的可能仍是一模一样
	// 的一枚图标（同一个站点的 favicon），那时不重写文件才是对的
	// （重写会刷新 mtime，而 mtime 是图标库列表的排序依据）。
	before = calls
	l2 := l
	l2.Path = srv.URL + "/other"
	svc.EnsureAutoIcon(l2)
	if calls == before {
		t.Fatal("地址变了必须重新解析，否则会一直挂着旧地址的图标")
	}
}

func TestEnsureAutoIconSkipsNonAutoSources(t *testing.T) {
	t.Parallel()

	svc := NewService(Options{CLI: &FakeCLI{}, IconsDir: t.TempDir(), AllowRemoteIcon: true})
	l := domain.Link{
		ID: "item-text", Name: "演示", Kind: domain.KindShortcut, Path: "https://example.invalid",
		Icon: domain.Icon{Source: domain.IconText, Text: "QL"},
	}
	l.Normalize()

	// 用户明确指定了图标，就不该在背后另存一份 —— 否则他改了文字图标，
	// 列表里那张旧的自动图标还会赖着不走。
	if changed, _ := svc.EnsureAutoIcon(l); changed {
		t.Fatal("非「自动」档不该落盘")
	}
	if got := svc.AutoIconFile(l); got != "" {
		t.Fatalf("非「自动」档不该报出自动图标文件名，实际 %q", got)
	}
}

func TestRemoveAutoIconClearsFileAndState(t *testing.T) {
	t.Parallel()

	icon := solidPNG(t, 64, 64, color.RGBA{R: 0x01, G: 0x02, B: 0x03, A: 255})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/favicon.ico" {
			_, _ = w.Write(icon)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	dir := t.TempDir()
	svc := NewService(Options{CLI: &FakeCLI{}, IconsDir: dir, AllowRemoteIcon: true})
	l := domain.Link{ID: "item-del", Name: "演示", Kind: domain.KindShortcut, Path: srv.URL}
	l.Normalize()
	svc.EnsureAutoIcon(l)

	svc.RemoveAutoIcon(l)
	if got := svc.AutoIconFile(l); got != "" {
		t.Fatalf("删除后不该再报出图标文件，实际 %q", got)
	}
	// 状态也要一起清：否则"删了再建同一条"会命中旧的签名，永远不再解析。
	if changed, _ := svc.EnsureAutoIcon(l); !changed {
		t.Fatal("删除状态后重新解析应当重新落盘")
	}
}

func TestIsAutoIconFile(t *testing.T) {
	t.Parallel()
	if !IsAutoIconFile(autoIconPrefix + "item-x.png") {
		t.Fatal("自动图标要被认出来")
	}
	if IsAutoIconFile("mylogo.png") {
		t.Fatal("用户自己传的图标不能被误判成程序产物")
	}
}

func TestResolveIconExReportsOfficialKeyword(t *testing.T) {
	t.Parallel()

	// 这一档要发外网请求，测不了真实命中；直接验证"命中了要说清关键词"
	// 这条契约由 ResolveIconEx 的实现保证。
	// 用文字档做正向锚点：来源与说明都要对得上。
	res := ResolveIconEx(IconRequest{
		Icon: domain.Icon{Source: domain.IconText, Text: "QL"},
	})
	if res.Source != IconSourceText {
		t.Fatalf("文字档来源应当是 text，实际 %q", res.Source)
	}
	if res.Detail == "" {
		t.Fatal("任何一档都必须给出可展示的说明")
	}
}
