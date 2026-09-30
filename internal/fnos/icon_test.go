package fnos

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/MisiteQ/qlink2desktop/internal/domain"
)

func mustDecode(t *testing.T, data []byte) image.Image {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("PNG 解码失败: %v", err)
	}
	return img
}

func TestParseHexColor(t *testing.T) {
	t.Parallel()

	fallback := color.RGBA{R: 1, G: 2, B: 3, A: 255}
	cases := []struct {
		in   string
		want color.RGBA
	}{
		{"#ff0000", color.RGBA{R: 255, A: 255}},
		{"#f00", color.RGBA{R: 255, A: 255}},
		{"#00FF00", color.RGBA{G: 255, A: 255}},
		{"#0000ff", color.RGBA{B: 255, A: 255}},
		{"  #FFFFFF  ", color.RGBA{R: 255, G: 255, B: 255, A: 255}},
		{"rgb(10, 20, 30)", color.RGBA{R: 10, G: 20, B: 30, A: 255}},
		{"", fallback},
		{"not-a-color", fallback},
		{"#gg0000", fallback},
		{"#12345", fallback},
	}
	for _, tc := range cases {
		if got := ParseHexColor(tc.in, fallback); got != tc.want {
			t.Errorf("ParseHexColor(%q) = %+v, 期望 %+v", tc.in, got, tc.want)
		}
	}
}

func TestNormalizeIconText(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"ql":        "QL",
		"  alist  ": "ALIS",
		"ab":        "AB",
		"abcdefg":   "ABCD",
		"中文":        "QL", // 无法渲染 → 默认
		"":          "QL",
		"!@#":       "QL",
		"n1":        "N1",
	}
	for in, want := range cases {
		if got := NormalizeIconText(in); got != want {
			t.Errorf("NormalizeIconText(%q) = %q, 期望 %q", in, got, want)
		}
	}
}

func TestRenderTextIconProducesOpaquePixels(t *testing.T) {
	t.Parallel()

	img, err := RenderTextIcon("QL", "#ffffff", "#2563eb", 256)
	if err != nil {
		t.Fatalf("渲染文字图标失败: %v", err)
	}
	b := img.Bounds()
	if b.Dx() != 256 || b.Dy() != 256 {
		t.Fatalf("尺寸应为 256x256，实际 %dx%d", b.Dx(), b.Dy())
	}

	// 中心区域必须有绘制内容（不考虑圆角外透明区）。
	rgba, ok := img.(*image.RGBA)
	if !ok {
		t.Fatalf("应返回 RGBA 图像，实际 %T", img)
	}
	if a := rgba.RGBAAt(128, 128).A; a == 0 {
		t.Error("中心像素不应完全透明")
	}
	// 背景色应接近传入的蓝色。
	corner := rgba.RGBAAt(128, 20)
	if corner.B < 150 {
		t.Errorf("背景应呈现蓝色调，实际 %+v", corner)
	}
}

func TestRenderTextIconUsesGivenColors(t *testing.T) {
	t.Parallel()

	const size = 128
	img, err := RenderTextIcon("A", "#000000", "#ffffff", size)
	if err != nil {
		t.Fatal(err)
	}
	rgba := img.(*image.RGBA)

	// 采样点上方的空白背景带：既在圆角矩形内部，又完全落在文字包围盒之上。
	// （文字垂直居中，顶带约在 8.6% 高度处结束，取 5% 处最稳。）
	bg := rgba.RGBAAt(size/2, size*5/100)
	if bg.R < 200 || bg.G < 200 || bg.B < 200 {
		t.Errorf("白色背景未生效，实际 %+v", bg)
	}

	// 同时确认文字确实被画成了黑色，否则「背景白」也可能是整张图没渲染。
	var darkCount int
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			if c := rgba.RGBAAt(x, y); c.R < 40 && c.G < 40 && c.B < 40 && c.A == 255 {
				darkCount++
			}
		}
	}
	if darkCount == 0 {
		t.Error("未在图中找到任何文字像素")
	}
}

func TestDefaultIconIsUsable(t *testing.T) {
	t.Parallel()

	set := DefaultIconSet()
	if !set.Valid() {
		t.Fatal("默认图标集应始终有效")
	}

	small := mustDecode(t, set.Small)
	if small.Bounds().Dx() != IconSizeSmall {
		t.Errorf("小图应为 %d，实际 %d", IconSizeSmall, small.Bounds().Dx())
	}
	large := mustDecode(t, set.Large)
	if large.Bounds().Dx() != IconSizeLarge {
		t.Errorf("大图应为 %d，实际 %d", IconSizeLarge, large.Bounds().Dx())
	}
}

func TestToIconSetPadsAndScales(t *testing.T) {
	t.Parallel()

	// 造一张 200x100 的非正方形图，验证会补成正方形而不是被拉伸变形。
	src := image.NewRGBA(image.Rect(0, 0, 200, 100))
	for y := 0; y < 100; y++ {
		for x := 0; x < 200; x++ {
			src.SetRGBA(x, y, color.RGBA{R: 255, A: 255})
		}
	}

	set, err := ToIconSet(src)
	if err != nil {
		t.Fatalf("ToIconSet 失败: %v", err)
	}
	if !set.Valid() {
		t.Fatal("应产出两档图标")
	}
	large := mustDecode(t, set.Large)
	if large.Bounds().Dx() != IconSizeLarge || large.Bounds().Dy() != IconSizeLarge {
		t.Fatalf("应输出正方形，实际 %v", large.Bounds())
	}
	// 补边区域应是透明的（上方/下方），中间内容保持红色。
	if _, _, _, a := large.At(128, 2).RGBA(); a>>8 == 255 {
		t.Error("补边区域应保持透明")
	}
	if r, _, _, _ := large.At(128, 128).RGBA(); r>>8 < 200 {
		t.Error("内容区域应保留原有红色")
	}
}

func TestToIconSetRejectsNil(t *testing.T) {
	t.Parallel()

	if _, err := ToIconSet(nil); err == nil {
		t.Fatal("nil 图像应被拒绝")
	}
}

func TestIconKeywords(t *testing.T) {
	t.Parallel()

	cases := map[string][]string{
		"linuxserver/qbittorrent:latest": {"qbittorrent", "linuxserver-qbittorrent"},
		"portainer/portainer-ce":         {"portainer-ce", "portainer", "portainer-portainer-ce"},
		"alist":                          {"alist"},
		"":                               nil,
		"a/b/c":                          {"c", "a-b-c"},
	}
	for input, want := range cases {
		got := iconKeywords(input)
		if len(want) == 0 {
			if len(got) != 0 {
				t.Errorf("iconKeywords(%q) = %v, 期望空", input, got)
			}
			continue
		}
		// 首个候选必须是最具体的名字。
		if len(got) == 0 || got[0] != want[0] {
			t.Errorf("iconKeywords(%q) 首项 = %v, 期望 %q", input, got, want[0])
		}
		for _, w := range want {
			found := false
			for _, g := range got {
				if g == w {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("iconKeywords(%q) 缺少候选 %q，实际 %v", input, w, got)
			}
		}
	}
}

func TestSaveUploadConvertsToPNG(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// 造一张 JPEG。
	var jpegBuf bytes.Buffer
	src := image.NewRGBA(image.Rect(0, 0, 32, 32))
	if err := png.Encode(&jpegBuf, src); err != nil {
		t.Fatal(err)
	}

	name, err := SaveUpload(dir, "../../evil name.png", jpegBuf.Bytes())
	if err != nil {
		t.Fatalf("SaveUpload 失败: %v", err)
	}
	if filepath.Dir(name) != "." {
		t.Errorf("文件名不应包含路径分隔符: %q", name)
	}
	if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
		t.Fatalf("文件未落盘: %v", err)
	}
}

func TestSaveUploadRejectsNonImage(t *testing.T) {
	t.Parallel()

	if _, err := SaveUpload(t.TempDir(), "x.txt", []byte("hello world")); err == nil {
		t.Fatal("非图片内容应被拒绝")
	}
}

func TestResolveIconFallbacks(t *testing.T) {
	t.Parallel()

	// 1) 完全无来源 → 内置默认图标。
	got := ResolveIcon(IconRequest{})
	if !got.Valid() {
		t.Fatal("无来源时必须回落到默认图标")
	}

	// 2) 文字图标（离线也应生效）。
	got = ResolveIcon(IconRequest{Icon: domain.Icon{Source: domain.IconText, Text: "NAS", BgColor: "#111111"}})
	if !got.Valid() {
		t.Fatal("文字图标应离线可用")
	}

	// 3) 不存在的上传文件 → 默认图标。
	got = ResolveIcon(IconRequest{
		Icon:      domain.Icon{Source: domain.IconUpload, Ref: "nope.png"},
		UploadDir: t.TempDir(),
	})
	if !got.Valid() {
		t.Fatal("上传缺失时应回落到默认图标")
	}
}

func TestResolveIconPrefersDataURI(t *testing.T) {
	t.Parallel()

	// 生成一张纯红图并转成 data URI。
	src := image.NewRGBA(image.Rect(0, 0, 16, 16))
	for y := 0; y < 16; y++ {
		for x := 0; x < 16; x++ {
			src.SetRGBA(x, y, color.RGBA{R: 255, A: 255})
		}
	}
	data, err := EncodePNG(src)
	if err != nil {
		t.Fatal(err)
	}
	uri := "data:image/png;base64," + base64Encode(data)

	got := ResolveIcon(IconRequest{
		Icon:        domain.Icon{Source: domain.IconURL, Ref: uri},
		AllowRemote: true,
	})
	if !got.Valid() {
		t.Fatal("data URI 应被解析")
	}
	img := mustDecode(t, got.Large)
	if r, _, _, _ := img.At(128, 128).RGBA(); r>>8 < 200 {
		t.Error("应使用 data URI 里的红色图像，而不是默认图标")
	}
}

func TestParseHexColorRoundTrip(t *testing.T) {
	t.Parallel()

	// 与 image/color 的十六进制表示互校。
	for _, hex := range []string{"#000000", "#ffffff", "#3b82f6", "#1d4ed8"} {
		got := ParseHexColor(hex, color.RGBA{})
		var want color.RGBA
		if _, err := fmtSscanHex(hex, &want); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("ParseHexColor(%q) = %+v, 期望 %+v", hex, got, want)
		}
	}
}

// -------------------------------------------------------------------------- 小工具

func base64Encode(b []byte) string {
	const table = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	var out []byte
	for i := 0; i < len(b); i += 3 {
		var chunk [3]byte
		n := copy(chunk[:], b[i:])
		v := uint(chunk[0])<<16 | uint(chunk[1])<<8 | uint(chunk[2])
		out = append(out, table[v>>18&0x3f], table[v>>12&0x3f])
		if n > 1 {
			out = append(out, table[v>>6&0x3f])
		} else {
			out = append(out, '=')
		}
		if n > 2 {
			out = append(out, table[v&0x3f])
		} else {
			out = append(out, '=')
		}
	}
	return string(out)
}

func fmtSscanHex(hex string, out *color.RGBA) (int, error) {
	c := ParseHexColor(hex, color.RGBA{})
	*out = c
	return 1, nil
}
