// Command iconforge 从一张高清源图生成项目所需的全部图标资源。
//
// 之所以把它做成一等公民的子命令、而不是塞进构建脚本里调 Python/PIL：
//
//  1. 复用 internal/fnos 里已有的缩放实现，产品图标与运行时生成的
//     桌面图标走完全相同的重采样路径，观感一致；
//  2. 开发机上通常没有 PIL —— 少一个外部依赖，就少一类"打包脚本在别人机器上跑不起来"；
//  3. 图标属于可复现产物，用 go run ./cmd/iconforge 一条命令就能重新生成。
//
// 生成清单：
//
//	fnos-app/ICON.PNG                      64x64   包管理器展示
//	fnos-app/ICON_256.PNG                  256x256 包管理器展示
//	fnos-app/app/ui/images/icon_64.png      64x64   桌面入口 icon_{0}.png
//	fnos-app/app/ui/images/icon_256.png     256x256 桌面入口 icon_{0}.png
//	internal/fnos/assets/logo_256.png       256x256 内嵌默认图标（运行时兜底）
package main

import (
	"flag"
	"fmt"
	"image"
	_ "image/jpeg" // 让 image.Decode 也认 JPEG 源图
	"log"
	"os"
	"path/filepath"

	"github.com/MisiteQ/qlink2desktop/internal/fnos"
)

func main() {
	src := flag.String("src", "QLink2Desktop.png", "源图路径（建议 1024 见方以上）")
	appDir := flag.String("app", "fnos-app", "fnos 应用包目录")
	assetsDir := flag.String("assets", filepath.Join("internal", "fnos", "assets"), "内嵌资源目录")
	flag.Parse()

	if err := run(*src, *appDir, *assetsDir); err != nil {
		log.Fatalf("图标生成失败: %v", err)
	}
}

func run(srcPath, appDir, assetsDir string) error {
	img, err := loadImage(srcPath)
	if err != nil {
		return err
	}
	b := img.Bounds()
	log.Printf("源图 %s：%dx%d", srcPath, b.Dx(), b.Dy())

	set, err := fnos.ToIconSet(img)
	if err != nil {
		return err
	}
	if !set.Valid() {
		return fmt.Errorf("缩放结果为空")
	}

	targets := []struct {
		path string
		data []byte
		note string
	}{
		{filepath.Join(appDir, "ICON.PNG"), set.Small, "应用包小图标"},
		{filepath.Join(appDir, "ICON_256.PNG"), set.Large, "应用包大图标"},
		{filepath.Join(appDir, "app", "ui", "images", "icon_64.png"), set.Small, "桌面入口小图标"},
		{filepath.Join(appDir, "app", "ui", "images", "icon_256.png"), set.Large, "桌面入口大图标"},
		{filepath.Join(assetsDir, "logo_256.png"), set.Large, "内嵌默认图标"},
	}

	for _, t := range targets {
		if err := os.MkdirAll(filepath.Dir(t.path), 0o755); err != nil {
			return fmt.Errorf("创建目录失败: %w", err)
		}
		if err := os.WriteFile(t.path, t.data, 0o644); err != nil {
			return fmt.Errorf("写入 %s 失败: %w", t.path, err)
		}
		log.Printf("已生成 %-56s %6d 字节  (%s)", filepath.ToSlash(t.path), len(t.data), t.note)
	}
	return nil
}

// loadImage 读取并解码源图（PNG / JPEG 均可）。
func loadImage(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("打开源图失败: %w", err)
	}
	defer f.Close()

	img, format, err := image.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("解码源图失败（支持 PNG / JPEG）: %w", err)
	}
	log.Printf("源图格式: %s", format)
	return img, nil
}
