package fnos

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"strconv"
	"strings"
)

// ParseHexColor 解析 #rgb / #rrggbb / #aarrggbb / rgb(...) 形式的颜色。
// 无法解析时返回 fallback，绝不返回错误——图标配色是锦上添花，不该阻断安装。
func ParseHexColor(s string, fallback color.RGBA) color.RGBA {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return fallback
	}

	if strings.HasPrefix(s, "rgb") {
		inner := strings.TrimSuffix(strings.TrimPrefix(s, "rgb("), ")")
		inner = strings.TrimPrefix(inner, "a")
		parts := strings.Split(strings.Trim(inner, "()"), ",")
		if len(parts) >= 3 {
			var vals [4]uint8
			vals[3] = 255
			ok := true
			for i := 0; i < len(parts) && i < 4; i++ {
				v, err := strconv.Atoi(strings.TrimSpace(parts[i]))
				if err != nil || v < 0 || v > 255 {
					ok = false
					break
				}
				vals[i] = uint8(v)
			}
			if ok {
				return color.RGBA{R: vals[0], G: vals[1], B: vals[2], A: 255}
			}
		}
		return fallback
	}

	s = strings.TrimPrefix(s, "#")
	switch len(s) {
	case 3:
		s = string([]byte{s[0], s[0], s[1], s[1], s[2], s[2]})
	case 6:
		// 标准形式
	case 8:
		// #aarrggbb → 取后 6 位（本应用不使用透明度）
		s = s[2:]
	default:
		return fallback
	}

	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return fallback
	}
	return color.RGBA{R: uint8(v >> 16), G: uint8(v >> 8 & 0xff), B: uint8(v & 0xff), A: 255}
}

// EncodePNG 把图像编码为 PNG 字节。
func EncodePNG(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("PNG 编码失败: %w", err)
	}
	return buf.Bytes(), nil
}

// PadToSquare 把任意尺寸的图像居中放到透明正方形画布上，避免被拉伸变形。
func PadToSquare(src image.Image) *image.RGBA {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	side := w
	if h > side {
		side = h
	}
	if side <= 0 {
		side = 1
	}

	dst := image.NewRGBA(image.Rect(0, 0, side, side))
	offset := image.Pt((side-w)/2, (side-h)/2)
	draw.Draw(dst, image.Rect(offset.X, offset.Y, offset.X+w, offset.Y+h), src, b.Min, draw.Src)
	return dst
}

// ---------------------------------------------------------------------------
// 缩放
//
// 图标这条链上的每一张图都要过这里：产品图标的 5 个产物、运行时的默认图标、
// 桌面快捷方式去站点抓的 favicon、用户上传的图标、文字渲染出来的图标。
// 它也是「图标清不清楚」的唯一决定处 —— 飞牛桌面图标只有 64 / 256 两档，
// 而来源普遍是 512～2048 见方，每一次都是十几倍到二十几倍的**缩小**。
// ---------------------------------------------------------------------------

// sharpenMinRatio 是开始锐化的最小缩小倍数。
//
// 缩小 2 倍以内丢掉的细节有限，硬上锐化只会把噪声和锯齿一起放大；
// 而 6×～24× 的缩小时，3×3 的轻度锐化能把被平均摊平的边缘拉回来 ——
// 受控实验里 64 档的锐度指标 64 → 98，边缘的彩色噪点反而更少。
const sharpenMinRatio = 2

// sharpenAmount 是轻度锐化的强度。
//
// 0.20 是实测选出来的：再往上（0.35）锐度只多一点点，半透明边缘的彩色噪点
// 却明显变脏 —— 图标外圈会泛起一圈说不出是什么、但就是"不干净"的颜色。
const sharpenAmount = 0.20

// ResizeHighQuality 是缩放图像的统一入口。
//
// 缩小（图标场景里的绝对主流：2048 → 64 是 32 倍）走精确面积平均；
// 放大走双线性插值。两者都在**预乘 alpha 空间**做颜色运算。
//
// 为什么不能直接用双线性缩小：它每个输出像素只采样源图上的 4 个点，
// 缩 32 倍时 99.6% 的源像素根本没被读到 —— 细线条断裂、文字糊成一团。
// 面积平均则让每个输出像素真正"看到"它覆盖的那一整片源区域。
//
// 为什么面积平均之后还要再锐化：面积平均本身是个低通滤波，边缘一定会被摊平，
// 缩得越狠摊得越平。这正是"压缩太狠、图标不清楚"的来历 ——
// 所以缩到 2× 以上时补一道轻度锐化，把边缘拉回来一点。
func ResizeHighQuality(src image.Image, dstW, dstH int) *image.RGBA {
	if dstW <= 0 || dstH <= 0 {
		return image.NewRGBA(image.Rect(0, 0, 1, 1))
	}
	// 先补成正方形：非正方形源图应当居中留白，而不是被压扁变形。
	square := PadToSquare(src)
	sw, sh := square.Bounds().Dx(), square.Bounds().Dy()

	if sw == dstW && sh == dstH {
		// 尺寸一样就别走"预乘 → 插值 → 反预乘"这一趟：uint8 往返会带进
		// ±1 的误差，而这里本该是恒等变换。
		return square
	}

	if sw >= dstW && sh >= dstH {
		out := ResizeArea(square, dstW, dstH)
		if sw >= dstW*sharpenMinRatio || sh >= dstH*sharpenMinRatio {
			return sharpenPremultiplied(out, sharpenAmount)
		}
		return out
	}
	return ResizeBilinear(square, dstW, dstH)
}

// ResizeArea 用精确面积平均缩小图像。
//
// 「精确」指每个源像素按它与输出像素覆盖区**重叠的面积**加权，
// 而不是"折半折到差不多、剩下的交给插值"那种近似 —— 少一层近似就少一层模糊。
//
// 颜色在预乘 alpha 空间累加：直接对非预乘的 RGB 求平均，透明区域里的黑色
// 会渗进边缘，图标外圈会糊出一圈脏描边（旧实现只在折半那一步预乘过，
// 收尾那次双线性插值又把它渗了回去）。
func ResizeArea(src image.Image, dstW, dstH int) *image.RGBA {
	if dstW <= 0 || dstH <= 0 {
		return image.NewRGBA(image.Rect(0, 0, 1, 1))
	}
	sb := src.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	if sw == 0 || sh == 0 {
		return image.NewRGBA(image.Rect(0, 0, dstW, dstH))
	}

	// 先读成一张 RGBA 缓冲，避免内层循环反复走 image.Image 接口。
	srcRGBA := image.NewRGBA(image.Rect(0, 0, sw, sh))
	draw.Draw(srcRGBA, srcRGBA.Bounds(), src, sb.Min, draw.Src)

	dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
	stepX := float64(sw) / float64(dstW)
	stepY := float64(sh) / float64(dstH)

	for y := 0; y < dstH; y++ {
		top := float64(y) * stepY
		bottom := top + stepY
		sy0, sy1 := int(top), int(math.Ceil(bottom))
		for x := 0; x < dstW; x++ {
			left := float64(x) * stepX
			right := left + stepX
			sx0, sx1 := int(left), int(math.Ceil(right))

			var sumR, sumG, sumB, sumA, sumW float64
			for sy := sy0; sy < sy1 && sy < sh; sy++ {
				cy := math.Min(bottom, float64(sy+1)) - math.Max(top, float64(sy))
				if cy <= 0 {
					continue
				}
				for sx := sx0; sx < sx1 && sx < sw; sx++ {
					cx := math.Min(right, float64(sx+1)) - math.Max(left, float64(sx))
					if cx <= 0 {
						continue
					}
					w := cx * cy
					c := srcRGBA.RGBAAt(sx, sy)
					a := float64(c.A) / 255
					sumR += float64(c.R) * a * w
					sumG += float64(c.G) * a * w
					sumB += float64(c.B) * a * w
					sumA += a * w
					sumW += w
				}
			}
			if sumW == 0 || sumA == 0 {
				// 这一格覆盖的全是透明区域（或压根没有源像素）——
				// 保持全透明即可，不要凭空"补"一个颜色出来。
				continue
			}
			dst.SetRGBA(x, y, color.RGBA{
				R: clampByte(sumR / sumA),
				G: clampByte(sumG / sumA),
				B: clampByte(sumB / sumA),
				A: clampByte(sumA / sumW * 255),
			})
		}
	}
	return dst
}

// premul 是一个预乘 alpha 之后的颜色（0..255 的浮点，便于做加权运算）。
type premul struct {
	r, g, b, a float64
}

// premultiplied 把常规 RGBA 转成预乘表示。
//
// 为什么非预乘的 RGB 不能直接参与运算：一像素的 alpha 为 0 时，它的 RGB
// 是什么颜色对结果毫无意义（PNG 里通常就是全 0，但没有任何东西保证这一点）。
// 不预乘就等于让这些"不存在的颜色"按同样的权重参与平均和锐化。
func premultiplied(c color.RGBA) premul {
	a := float64(c.A)
	return premul{r: float64(c.R) * a / 255, g: float64(c.G) * a / 255, b: float64(c.B) * a / 255, a: a}
}

// unpremultiply 把预乘颜色还原成常规 RGBA。
func unpremultiply(p premul) color.RGBA {
	if p.a <= 0 {
		return color.RGBA{}
	}
	return color.RGBA{
		R: clampByte(p.r * 255 / p.a),
		G: clampByte(p.g * 255 / p.a),
		B: clampByte(p.b * 255 / p.a),
		A: clampByte(p.a),
	}
}

// sharpenPremultiplied 在预乘 alpha 空间做一道 3×3 的轻度锐化。
//
// 为什么必须回到预乘空间：直接对非预乘的 RGB 做拉普拉斯，会把"只存在于
// alpha 上的跳变"当成颜色跳变来放大，图标边缘于是泛起品红 / 青色噪点 ——
// 实测在 64 档非常明显。预乘之后透明区的 RGB 被拉到 0，边缘的颜色才真正
// 只由"看得见的那些像素"决定（同一组参数下边缘色噪 76 → 58，白线回到白色）。
//
// 只锐化颜色、不锐化 alpha：轮廓的透明度是抗锯齿的成果，
// 动它会在图标外圈留下一圈忽明忽暗的硬边。
func sharpenPremultiplied(src *image.RGBA, amount float64) *image.RGBA {
	w, h := src.Bounds().Dx(), src.Bounds().Dy()
	if w < 3 || h < 3 || amount <= 0 {
		// 太小就没有"邻域"可言，直接还给调用方，不做无意义的运算。
		return src
	}

	// 拉普拉斯核：中心 5、四邻各 -1（系数和为 1，不会整体提亮或压暗）。
	kernel := [9]float64{0, -1, 0, -1, 5, -1, 0, -1, 0}

	out := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			center := premultiplied(src.RGBAAt(x, y))
			var r, g, b float64
			i := 0
			for dy := -1; dy <= 1; dy++ {
				for dx := -1; dx <= 1; dx++ {
					s := premultiplied(src.RGBAAt(clampInt(x+dx, 0, w-1), clampInt(y+dy, 0, h-1)))
					r += s.r * kernel[i]
					g += s.g * kernel[i]
					b += s.b * kernel[i]
					i++
				}
			}
			out.SetRGBA(x, y, unpremultiply(premul{
				r: center.r*(1-amount) + r*amount,
				g: center.g*(1-amount) + g*amount,
				b: center.b*(1-amount) + b*amount,
				a: center.a,
			}))
		}
	}
	return out
}

// ResizeBilinear 用双线性插值缩放图像。
//
// 标准库没有缩放能力，而引入第三方图像库会破坏「零依赖单二进制」这个前提。
// 放大（目标大于源图）时用它是合适的：每个输出像素落在 4 个源像素之间，
// 采样点数量足够。**缩小幅度较大时不要用它** —— 用 ResizeHighQuality。
//
// 颜色同样在预乘空间插值：非预乘插值会让透明区域的黑色渗进边缘，
// 一张 32 见方的图标放大到 256 就会套上一圈灰扑扑的描边。
func ResizeBilinear(src image.Image, dstW, dstH int) *image.RGBA {
	if dstW <= 0 || dstH <= 0 {
		return image.NewRGBA(image.Rect(0, 0, 1, 1))
	}

	sb := src.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	if sw == 0 || sh == 0 {
		return image.NewRGBA(image.Rect(0, 0, dstW, dstH))
	}

	dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))

	// 预先把源图读成一张 RGBA 缓冲，避免在内层循环里反复走 image.Image 接口。
	srcRGBA := image.NewRGBA(image.Rect(0, 0, sw, sh))
	draw.Draw(srcRGBA, srcRGBA.Bounds(), src, sb.Min, draw.Src)

	scaleX := float64(sw) / float64(dstW)
	scaleY := float64(sh) / float64(dstH)

	for y := 0; y < dstH; y++ {
		// 采样点取像素中心，避免整体偏移半个像素导致边缘发虚。
		fy := (float64(y)+0.5)*scaleY - 0.5
		y0 := int(math.Floor(fy))
		wy := fy - float64(y0)
		y1 := clampInt(y0+1, 0, sh-1)
		y0 = clampInt(y0, 0, sh-1)

		for x := 0; x < dstW; x++ {
			fx := (float64(x)+0.5)*scaleX - 0.5
			x0 := int(math.Floor(fx))
			wx := fx - float64(x0)
			x1 := clampInt(x0+1, 0, sw-1)
			x0 = clampInt(x0, 0, sw-1)

			c00 := premultiplied(srcRGBA.RGBAAt(x0, y0))
			c10 := premultiplied(srcRGBA.RGBAAt(x1, y0))
			c01 := premultiplied(srcRGBA.RGBAAt(x0, y1))
			c11 := premultiplied(srcRGBA.RGBAAt(x1, y1))

			dst.SetRGBA(x, y, unpremultiply(premul{
				r: bilerp(c00.r, c10.r, c01.r, c11.r, wx, wy),
				g: bilerp(c00.g, c10.g, c01.g, c11.g, wx, wy),
				b: bilerp(c00.b, c10.b, c01.b, c11.b, wx, wy),
				a: bilerp(c00.a, c10.a, c01.a, c11.a, wx, wy),
			}))
		}
	}
	return dst
}

// bilerp 在四个采样值之间做双线性插值（浮点进出，取整统一交给 clampByte）。
func bilerp(c00, c10, c01, c11, wx, wy float64) float64 {
	top := c00*(1-wx) + c10*wx
	bottom := c01*(1-wx) + c11*wx
	return top*(1-wy) + bottom*wy
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// clampByte 把浮点颜色分量收进 0..255 并四舍五入成字节。
//
// 锐化会算出负值（边缘暗侧）和超过 255 的值（边缘亮侧），
// 不收就会被 uint8 转换回绕成一团乱码。
func clampByte(v float64) uint8 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(v + 0.5)
}

// blend 按 alpha 把 fg 叠加到 bg 上（两者均为不透明底色的场景可传 a=1）。
func blend(bg, fg color.RGBA, a float64) color.RGBA {
	if a <= 0 {
		return bg
	}
	if a >= 1 {
		return fg
	}
	return color.RGBA{
		R: uint8(float64(bg.R)*(1-a) + float64(fg.R)*a + 0.5),
		G: uint8(float64(bg.G)*(1-a) + float64(fg.G)*a + 0.5),
		B: uint8(float64(bg.B)*(1-a) + float64(fg.B)*a + 0.5),
		A: 255,
	}
}
