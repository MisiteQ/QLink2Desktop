package fnos

import (
	"image"
	"image/color"
	"math"
	"testing"
)

// --------------------------------------------------------------------------
// 缩放的质量守卫
//
// 这一组测试守的是一个用户报过的问题：「logo 的图别压缩太狠，有用户反映
// 图标都不清楚了」。图标只有 64 / 256 两档，而来源普遍是 512～2048 见方，
// 也就是说这条链上做的**全是缩小** —— 缩得越多越糊，正是问题的来源。
//
// 所以这里断言的不是"函数返回了一张图"（那个永远为真），而是
// 「边缘有没有被摊平」「半透明边界有没有被透明区的颜色带脏」这两件事。
// 用「同尺寸」「只调参数」的方式改坏实现时，只有这种断言会 FAIL。
// --------------------------------------------------------------------------

func solidImage(w, h int, c color.RGBA) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, c)
		}
	}
	return img
}

// stripeImage 生成灰度竖条（周期 = 2*stripeW）。
//
// 用的是 96/168 这种**中间调**而不是纯黑纯白：纯黑白在锐化时会被直接钳到
// 0/255，越界的那部分被吃掉，表现是"加了锐化和没加一模一样" ——
// 第一版就是纯黑白，于是这条断言在实现根本没锐化时也照样通过（差一步就
// 把"锐化没生效"当成"测试通过了"）。
func stripeImage(size, stripeW int) *image.RGBA {
	light := color.RGBA{R: 168, G: 168, B: 168, A: 255}
	dark := color.RGBA{R: 96, G: 96, B: 96, A: 255}
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			if (x/stripeW)%2 == 0 {
				img.SetRGBA(x, y, light)
			} else {
				img.SetRGBA(x, y, dark)
			}
		}
	}
	return img
}

// discImage 生成一枚实心圆盘；圆盘外**故意**留一个与圆盘无关的颜色。
//
// 这不是为了刁难：全透明像素的 RGB 在 PNG 里本来就没有意义（很多带
// tRNS / 调色板的图就是全 0，但标准没有任何东西保证这一点）。断言
// 「它一个像素都不许渗进来」，才能把"忘了预乘"这种实现钉死。
func discImage(size int, inside color.RGBA, outside color.RGBA) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	c := float64(size) / 2
	r := c - float64(size)*0.06
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			dx, dy := float64(x)+0.5-c, float64(y)+0.5-c
			if math.Hypot(dx, dy) <= r {
				img.SetRGBA(x, y, inside)
			} else {
				img.SetRGBA(x, y, outside)
			}
		}
	}
	return img
}

// edgeEnergy 用 alpha 加权亮度拉普拉斯的平均绝对值衡量"看起来有多清楚"。
//
// 面积平均会把边缘摊平（这个值下降），锐化把它拉回来（这个值上升）——
// 用户的抱怨对应的就是这个数值太小。
func edgeEnergy(img *image.RGBA) float64 {
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	lum := func(c color.RGBA) float64 {
		return 0.299*float64(c.R) + 0.587*float64(c.G) + 0.114*float64(c.B)
	}
	var sum, n float64
	for y := 1; y < h-1; y++ {
		for x := 1; x < w-1; x++ {
			c := img.RGBAAt(x, y)
			if c.A < 16 {
				continue
			}
			sum += math.Abs(4*lum(c) -
				lum(img.RGBAAt(x-1, y)) - lum(img.RGBAAt(x+1, y)) -
				lum(img.RGBAAt(x, y-1)) - lum(img.RGBAAt(x, y+1)))
			n++
		}
	}
	if n == 0 {
		return 0
	}
	return sum / n
}

func TestResizeAreaKeepsSolidColorExact(t *testing.T) {
	t.Parallel()

	want := color.RGBA{R: 0x12, G: 0x9a, B: 0xf0, A: 255}
	got := ResizeArea(solidImage(64, 64, want), 16, 16)
	for y := 0; y < 16; y++ {
		for x := 0; x < 16; x++ {
			if c := got.RGBAAt(x, y); c != want {
				t.Fatalf("纯色图缩放后颜色变了：(%d,%d) = %+v，期望 %+v", x, y, c, want)
			}
		}
	}
}

// 面积平均必须是"按覆盖面积加权"，不是"取一个代表点"。
func TestResizeAreaAveragesByCoveredArea(t *testing.T) {
	t.Parallel()

	// 4x4 → 2x2：每个输出像素正好覆盖 2x2 的源像素，期望值可以手算。
	src := image.NewRGBA(image.Rect(0, 0, 4, 4))
	vals := [4][4]uint8{
		{10, 20, 30, 40},
		{50, 60, 70, 80},
		{90, 100, 110, 120},
		{130, 140, 150, 160},
	}
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			src.SetRGBA(x, y, color.RGBA{R: vals[y][x], G: vals[y][x], B: vals[y][x], A: 255})
		}
	}

	got := ResizeArea(src, 2, 2)
	for by := 0; by < 2; by++ {
		for bx := 0; bx < 2; bx++ {
			var sum int
			for dy := 0; dy < 2; dy++ {
				for dx := 0; dx < 2; dx++ {
					sum += int(vals[by*2+dy][bx*2+dx])
				}
			}
			want := uint8(float64(sum)/4 + 0.5)
			if c := got.RGBAAt(bx, by); c.R != want || c.A != 255 {
				t.Errorf("(%d,%d) = %+v，期望 R=%d A=255", bx, by, c, want)
			}
		}
	}
}

// 整数倍缩放时每个源像素的权重都一样，等权平均也能算出同样的结果 ——
// 所以"按覆盖面积加权"必须用**非整数倍**来验。
//
// 反向验证时把权重改成 cx+cy（等权的一种），上面那条照样全绿，
// 只有这条会 FAIL。
func TestResizeAreaWeightsByCoverageFraction(t *testing.T) {
	t.Parallel()

	// 3 → 2：两端各有一个"半覆盖"的输出像素。
	// 按覆盖面积加权是 30 / 150，等权的话是 38.6 / 141.4。
	src := image.NewRGBA(image.Rect(0, 0, 3, 1))
	for x, v := range []uint8{0, 90, 180} {
		src.SetRGBA(x, 0, color.RGBA{R: v, G: v, B: v, A: 255})
	}

	got := ResizeArea(src, 2, 1)
	for i, want := range []uint8{30, 150} {
		if c := got.RGBAAt(i, 0); c.R != want || c.A != 255 {
			t.Errorf("第 %d 个输出像素 = %+v，按覆盖面积加权应为 R=%d A=255", i, c, want)
		}
	}
}

// 全透明像素的颜色不许渗进边缘 —— 这是"预乘 alpha"存在的全部理由。
func TestResizeAreaDoesNotBleedTransparentColor(t *testing.T) {
	t.Parallel()

	const n = 64
	src := image.NewRGBA(image.Rect(0, 0, n, n))
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			if x < 30 {
				src.SetRGBA(x, y, color.RGBA{R: 255, A: 255})
			} else {
				// alpha 为 0 的蓝色：参与运算就是对"预乘"二字的背叛。
				src.SetRGBA(x, y, color.RGBA{B: 255})
			}
		}
	}

	got := ResizeArea(src, 8, 8)
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			if c := got.RGBAAt(x, y); c.B != 0 {
				t.Fatalf("透明区域的蓝色渗进了 (%d,%d)：%+v", x, y, c)
			}
		}
	}

	// 第 3 列压在 30 这个分界上：6 个红像素 + 2 个透明像素。
	edge := got.RGBAAt(3, 4)
	if edge.A == 0 || edge.A == 255 {
		t.Fatalf("这个用例本该压在半透明边界上，实际 A=%d", edge.A)
	}
	if edge.R < 250 {
		t.Errorf("半透明边界被透明区带暗了：%+v（应为「红但半透明」，「暗红」就是没预乘）", edge)
	}
}

// 放大路径同样要在预乘空间插值，否则图标外圈会套上一圈灰描边。
//
// 断言必须盯**颜色通道**而不只是亮度：只查 R 的话，预乘与不预乘会算出同一个
// 结果（r 与 a 同比例缩小，反预乘时又被除回去），这条用例就白写了 ——
// 反向验证正是这么发现它的第一版是形同虚设的。
func TestResizeBilinearUpscaleKeepsOpaqueEdgeColor(t *testing.T) {
	t.Parallel()

	src := image.NewRGBA(image.Rect(0, 0, 2, 2))
	for y := 0; y < 2; y++ {
		src.SetRGBA(0, y, color.RGBA{R: 255, A: 255})
		src.SetRGBA(1, y, color.RGBA{B: 255}) // 全透明，但 RGB 是蓝色
	}

	got := ResizeBilinear(src, 8, 8)
	partial := false
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			c := got.RGBAAt(x, y)
			if c.A == 0 {
				continue
			}
			if c.R < 250 {
				t.Fatalf("不透明内容在放大后变暗了：(%d,%d) = %+v —— 非预乘插值会把"+
					"透明区的黑色渗进边缘", x, y, c)
			}
			if c.B > 8 {
				t.Fatalf("透明区的蓝色渗进了边缘 (%d,%d)：%+v", x, y, c)
			}
			if c.A < 255 {
				partial = true
			}
		}
	}
	if !partial {
		t.Fatal("这个用例本该产生半透明过渡像素，否则它证伪不了任何东西")
	}
}

// 缩得越狠越糊 —— 这道锐化就是为它加的。没有它，用户报的"图标不清楚"会回来。
func TestResizeHighQualitySharpensHeavilyDownscaled(t *testing.T) {
	t.Parallel()

	// 512 → 64 是 8 倍，正是用户上传图标（512～2048 见方）的典型处境。
	src := stripeImage(512, 32)

	blurred := edgeEnergy(ResizeArea(PadToSquare(src), 64, 64))
	sharp := edgeEnergy(ResizeHighQuality(src, 64, 64))
	t.Logf("512 → 64 的边缘能量：面积平均 %.2f，实际管线 %.2f（%.2f 倍）", blurred, sharp, sharp/blurred)

	if sharp <= blurred*1.1 {
		t.Errorf("从 512 缩到 64 必须补一道锐化：边缘能量 面积平均 %.2f → "+
			"ResizeHighQuality %.2f，至少应高出 10%%", blurred, sharp)
	}
}

// 反过来：缩得很少时不许锐化，硬锐化只会把噪声和锯齿一起放大。
//
// 这条同时也是"锐化阈值真的生效"的证据 —— 把阈值判断摘掉、无脑锐化，
// 下面这个"必须与面积平均逐像素相同"就会 FAIL。
func TestResizeHighQualityLeavesNearNativeScaleAlone(t *testing.T) {
	t.Parallel()

	src := stripeImage(200, 20) // 200 → 180 只有 1.1 倍
	want := ResizeArea(PadToSquare(src), 180, 180)
	got := ResizeHighQuality(src, 180, 180)

	for y := 0; y < 180; y++ {
		for x := 0; x < 180; x++ {
			if a, b := want.RGBAAt(x, y), got.RGBAAt(x, y); a != b {
				t.Fatalf("1.1 倍缩小不该锐化：(%d,%d) 面积平均 %+v，实际 %+v", x, y, a, b)
			}
		}
	}
}

func TestResizeHighQualityIsIdentityOnSameSize(t *testing.T) {
	t.Parallel()

	src := stripeImage(64, 4)
	got := ResizeHighQuality(src, 64, 64)
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			if a, b := src.RGBAAt(x, y), got.RGBAAt(x, y); a != b {
				t.Fatalf("同尺寸必须逐像素相同：(%d,%d) %+v → %+v", x, y, a, b)
			}
		}
	}
}

// 整条链（面积平均 + 锐化）都不许把透明区的颜色带出来。
func TestResizeHighQualityIgnoresGarbageColorUnderAlphaZero(t *testing.T) {
	t.Parallel()

	src := discImage(256, color.RGBA{R: 255, G: 120, A: 255}, color.RGBA{B: 255})
	got := ResizeHighQuality(src, 64, 64)
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			if c := got.RGBAAt(x, y); c.B > 8 {
				t.Fatalf("透明区的蓝色渗进了缩略图 (%d,%d)：%+v", x, y, c)
			}
		}
	}
}

// 锐化只动颜色，不动 alpha：轮廓的透明度是抗锯齿的成果，
// 动它会在图标外圈留下一圈忽明忽暗的硬边。
func TestSharpenPremultipliedKeepsAlphaUntouched(t *testing.T) {
	t.Parallel()

	src := discImage(64, color.RGBA{R: 255, G: 120, A: 255}, color.RGBA{})

	// 先造出真正的半透明边缘（面积平均的结果），否则这个断言没东西可看。
	blurred := ResizeArea(src, 32, 32)
	got := sharpenPremultiplied(blurred, sharpenAmount)

	for y := 0; y < 32; y++ {
		for x := 0; x < 32; x++ {
			if a, b := blurred.RGBAAt(x, y).A, got.RGBAAt(x, y).A; a != b {
				t.Fatalf("锐化改了 alpha：(%d,%d) %d → %d", x, y, a, b)
			}
		}
	}
}

func TestSharpenPremultipliedSkipsTinyImages(t *testing.T) {
	t.Parallel()

	tiny := solidImage(2, 2, color.RGBA{R: 255, A: 255})
	if got := sharpenPremultiplied(tiny, sharpenAmount); got != tiny {
		t.Error("2x2 及更小的图没有「邻域」可言，应当原样返回而不是硬算一遍")
	}
}

func TestResizeDegenerateInputs(t *testing.T) {
	t.Parallel()

	if got := ResizeHighQuality(solidImage(1, 1, color.RGBA{R: 255, A: 255}), 64, 64); got.Bounds().Dx() != 64 {
		t.Errorf("1x1 放大到 64 应得到 64，实际 %d", got.Bounds().Dx())
	}
	if got := ResizeHighQuality(solidImage(8, 8, color.RGBA{A: 255}), 0, 0); got.Bounds().Dx() != 1 {
		t.Errorf("目标尺寸为 0 时应回落到 1x1，实际 %d", got.Bounds().Dx())
	}
	if got := ResizeArea(image.NewRGBA(image.Rect(0, 0, 0, 0)), 8, 8); got.Bounds().Dx() != 8 {
		t.Errorf("空的源图应得到一张 8x8 的透明图，实际 %d", got.Bounds().Dx())
	}
	if got := ResizeBilinear(solidImage(4, 4, color.RGBA{A: 255}), -3, 5); got.Bounds().Dx() != 1 {
		t.Errorf("负的目标尺寸应回落到 1x1，实际 %d", got.Bounds().Dx())
	}
}
