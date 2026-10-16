package fnos

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// 用户换 logo 时说的是「你给我把系统涉及到这个 logo 的都重新修改一下」。
//
// 这句话之所以值得一条测试来守，是因为这个 logo 派生出的产物散在 5 个位置：
//
//	fnos-app/ICON.PNG / ICON_256.PNG                应用中心展示
//	fnos-app/app/ui/images/icon_64.png / icon_256.png  桌面入口
//	internal/fnos/assets/logo_256.png               go:embed 进二进制的默认图标
//
// 少重新生成任何一个，都不会有任何编译期或运行期报错 —— 表现出来只是
// "有的地方图标换了、有的还是旧的"（浏览器标签和面板里的品牌图标走的正是
// 内嵌那张，所以最容易漏过去）。字节比对是唯一能钉住它的方式。
//
// 由 cmd/iconforge 统一生成：go run ./cmd/iconforge -src QLink2Desktop.png -app fnos-app
func TestShippedIconArtifactsStayInSync(t *testing.T) {
	t.Parallel()

	root := filepath.Join("..", "..")
	read := func(rel string) []byte {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("读取 %s 失败：%v", rel, err)
		}
		return data
	}

	small := read(filepath.Join("fnos-app", "ICON.PNG"))
	large := read(filepath.Join("fnos-app", "ICON_256.PNG"))

	pairs := []struct {
		rel  string
		want []byte
	}{
		{filepath.Join("fnos-app", "app", "ui", "images", "icon_64.png"), small},
		{filepath.Join("fnos-app", "app", "ui", "images", "icon_256.png"), large},
	}

	for _, p := range pairs {
		if got := read(p.rel); !bytes.Equal(got, p.want) {
			t.Errorf("%s 与同档位的 ICON 产物不一致（%d vs %d 字节）—— "+
				"改了 logo 却只重新生成了一部分，界面上的图标会档位间不一致",
				filepath.ToSlash(p.rel), len(got), len(p.want))
		}
	}

	// 内嵌那张是浏览器标签与面板品牌图标的来源（DefaultIcon → ResizeHighQuality），
	// 漏掉它的话"应用中心图标换了、页面里没换"。
	if !bytes.Equal(embeddedLogoPNG, large) {
		t.Errorf("internal/fnos/assets/logo_256.png（%d 字节）与 ICON_256.PNG（%d 字节）不一致 —— "+
			"内嵌图标是运行时所有默认图标的来源，漏了它就会出现"+
			"「桌面图标换了、页面里没换」", len(embeddedLogoPNG), len(large))
	}
}
