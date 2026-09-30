package app

import (
	"hash/fnv"
	"sort"

	"github.com/MisiteQ/qlink2desktop/internal/domain"
	"github.com/MisiteQ/qlink2desktop/internal/proxy"
)

// portSpan 是「基准端口 + 稳定偏移」里偏移量的取值范围。
//
// 1000 个槽位对家用场景（几十条链接）绰绰有余，
// 同时把代理端口限制在 base..base+999 这个可预测区间内 ——
// 用户配防火墙、写反代规则时不需要去猜端口号。
const portSpan = 1000

// stablePortStart 由链接 ID 推导出一个稳定的起始端口。
//
// 用哈希而不是递增计数器，是为了让端口的分配**不依赖历史**：
// 删掉中间某条链接不会导致后面所有链接的端口整体前移，
// 也就不会引发一连串无谓的图标重装。
func stablePortStart(base int, id string) int {
	if base <= 1024 || base > 65535-portSpan {
		base = proxy.DefaultProxyPortBase
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(id))
	return base + int(h.Sum32()%portSpan)
}

// pickStablePort 从 start 起向后寻找一个既没被本进程占用、内核也没占用的端口。
func pickStablePort(start int, used map[int]bool) int {
	const attempts = 400
	for i := 0; i < attempts; i++ {
		p := start + i
		if p > 65535 {
			// 越过上界就回到基准端口区间重新找。
			p = proxy.DefaultProxyPortBase + (p - 65535) - 1
		}
		if p <= 1024 {
			continue
		}
		if used[p] {
			continue
		}
		if proxy.PortAvailable(p) {
			return p
		}
	}
	// 兜底：交给通用分配器，它会用系统随机端口收尾。
	return proxy.PickPort(start, used)
}

// sortLinksForAllocation 按稳定顺序排列链接。
//
// 排序的意义不在美观，而在**可复现**：端口是逐个避让分配的，
// 若顺序依赖 map 迭代（Go 里是随机的），同一组链接在两次启动后
// 可能拿到不同的端口，于是每次开机都会触发一轮图标重装。
// 按创建时间 + ID 排序消除了这个随机性。
func sortLinksForAllocation(links []domain.Link) []domain.Link {
	out := append([]domain.Link(nil), links...)
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// needsProxy 报告该链接是否需要由本机拉起一个反向代理监听。
//
// 这是「端口是运行态、不是定义」这条原则的落地点：
// 只有真正需要经过本机转发的形态才占端口。
func needsProxy(l domain.Link) bool {
	if !l.Enabled {
		return false
	}
	switch l.Kind {
	case domain.KindProxy:
		// 远端 / 公网服务：必须经过本机代理才能借到飞牛 Connect 的外网穿透。
		return true
	case domain.KindLocalPort:
		// 本机服务一律直连，不占端口。
		//
		// 早期只有开了「开屏提示」的本机端口才会套一层代理（提示页需要一次
		// HTTP 响应才能渲染）。该功能已移除，这条路径也就不再需要 ——
		// 少一次转发、少一个故障点。
		return false
	default:
		return false
	}
}
