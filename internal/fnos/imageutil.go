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

// ResizeHighQuality 在缩放幅度较大时先做若干次盒式折半，再收尾插值。
//
// 为什么需要它：双线性插值每个输出像素只采样源图上的 4 个点。
// 把一张 2048 见方的图标缩到 64（32 倍）时，96% 以上的源像素根本没被读到，
// 细线条会断裂、文字会糊成一团。
//
// 先逐次折半（每次取 2x2 的平均，等价于区域平均），能让每个输出像素
// 真正"看到"整片源区域；剩下的最后一两步再交给双线性做平滑。
// 放大（目标大于源图）时不做折半，直接双线性。
func ResizeHighQuality(src image.Image, dstW, dstH int) *image.RGBA {
	if dstW <= 0 || dstH <= 0 {
		return image.NewRGBA(image.Rect(0, 0, 1, 1))
	}
	// 先补成正方形，保证后续折半过程中长宽比不变。
	var cur image.Image = PadToSquare(src)
	for {
		sw, sh := cur.Bounds().Dx(), cur.Bounds().Dy()
		// 缩到目标两倍以内就停手：再折半会低于目标尺寸，反而损失精度。
		if sw <= dstW*2 || sh <= dstH*2 || sw < 2 || sh < 2 {
			break
		}
		cur = BoxDownscale(cur)
	}
	return ResizeBilinear(cur, dstW, dstH)
}

// BoxDownscale 用 2x2 盒式平均把图像缩小一半（奇数边长时最后一行/列单独处理）。
//
// 颜色按 alpha 预乘后求平均：直接对非预乘的 RGB 取平均，
// 会让透明区域的黑色渗进边缘，产生一圈脏描边。
func BoxDownscale(src image.Image) *image.RGBA {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	if sw <= 0 || sh <= 0 {
		return image.NewRGBA(image.Rect(0, 0, 1, 1))
	}

	srcRGBA := image.NewRGBA(image.Rect(0, 0, sw, sh))
	draw.Draw(srcRGBA, srcRGBA.Bounds(), src, b.Min, draw.Src)

	dw, dh := (sw+1)/2, (sh+1)/2
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))

	for y := 0; y < dh; y++ {
		for x := 0; x < dw; x++ {
			var sumR, sumG, sumB, sumA, count uint32
			for dy := 0; dy < 2; dy++ {
				sy := y*2 + dy
				if sy >= sh {
					continue
				}
				for dx := 0; dx < 2; dx++ {
					sx := x*2 + dx
					if sx >= sw {
						continue
					}
					c := srcRGBA.RGBAAt(sx, sy)
					a := uint32(c.A)
					sumR += uint32(c.R) * a
					sumG += uint32(c.G) * a
					sumB += uint32(c.B) * a
					sumA += a
					count++
				}
			}
			if count == 0 || sumA == 0 {
				continue // 整块全透明，保持零值即可
			}
			dst.SetRGBA(x, y, color.RGBA{
				R: uint8(sumR / sumA),
				G: uint8(sumG / sumA),
				B: uint8(sumB / sumA),
				A: uint8(sumA / count),
			})
		}
	}
	return dst
}

// ResizeBilinear 用双线性插值缩放图像。
//
// 标准库没有缩放能力，而引入第三方图像库会破坏「零依赖单二进制」这个前提。
// 双线性插值对这个场景（把用户上传的图标缩到 64/256）已经完全够用，
// 且比早期版本的最近邻缩放在小尺寸下明显更干净。
//
// 注意：缩小幅度很大时应当改用 ResizeHighQuality，否则会丢细节。
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

			c00 := srcRGBA.RGBAAt(x0, y0)
			c10 := srcRGBA.RGBAAt(x1, y0)
			c01 := srcRGBA.RGBAAt(x0, y1)
			c11 := srcRGBA.RGBAAt(x1, y1)

			dst.SetRGBA(x, y, color.RGBA{
				R: bilerp(c00.R, c10.R, c01.R, c11.R, wx, wy),
				G: bilerp(c00.G, c10.G, c01.G, c11.G, wx, wy),
				B: bilerp(c00.B, c10.B, c01.B, c11.B, wx, wy),
				A: bilerp(c00.A, c10.A, c01.A, c11.A, wx, wy),
			})
		}
	}
	return dst
}

func bilerp(c00, c10, c01, c11 uint8, wx, wy float64) uint8 {
	top := float64(c00)*(1-wx) + float64(c10)*wx
	bottom := float64(c01)*(1-wx) + float64(c11)*wx
	v := top*(1-wy) + bottom*wy
	if v < 0 {
		v = 0
	}
	if v > 255 {
		v = 255
	}
	return uint8(v + 0.5)
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
