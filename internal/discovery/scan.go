package discovery

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// CommonPorts 是局域网扫描时默认探测的端口集合。
//
// 选取原则：NAS / 自建服务里最常见的 Web 与文件共享端口。
// 刻意保持精简 —— 每多一个端口，一轮 /24 扫描的耗时与连接数都线性增长，
// 而在家用网络里全端口扫描（1-65535）会明显扰动设备。
var commonPorts = []int{
	80, 81, 88, 443, 5000, 5001, 5050, 5244, 5678, 5900, 6000, 7000,
	8000, 8006, 8080, 8081, 8088, 8090, 8096, 8112, 8123, 8200, 8443,
	8888, 9000, 9090, 9091, 9443, 10000, 11443, 18080, 19999, 3000,
	3001, 4000, 5005, 5432, 6379, 7777, 8388, 9001, 10001, 12345,
}

// CommonPorts 返回默认探测端口的副本。
func CommonPorts() []int {
	out := make([]int, len(commonPorts))
	copy(out, commonPorts)
	return out
}

// ScanOptions 是一次扫描的参数。
type ScanOptions struct {
	// Hosts 是要扫描的主机列表（IP 或域名）。
	Hosts []string
	// Ports 为空时使用 CommonPorts()。
	Ports []int
	// Timeout 是单个端口的连接超时，默认 400ms。
	Timeout time.Duration
	// Concurrency 是并发上限，默认 128。
	Concurrency int
}

// ScanTCP 并发探测一组主机端口，返回其中开放的那些。
//
// 它是 ScanNetwork 的「无进度、不分阶段」版本：把所有主机 × 端口都扫一遍。
// 保留这个入口是为了让调用方在不关心进度时不必构造回调。
func ScanTCP(ctx context.Context, opts ScanOptions) []PortInfo {
	return ScanNetwork(ctx, NetworkScanOptions{
		Hosts:       opts.Hosts,
		Ports:       opts.Ports,
		Timeout:     opts.Timeout,
		Concurrency: opts.Concurrency,
		// 显式列出的目标一律全端口扫：调用方已经缩小了范围，
		// 再用探活筛一遍只会漏掉"上了线但没开常见端口"的设备。
		SkipHostPing: true,
	}, nil)
}

func dialOnce(ctx context.Context, host string, port int, timeout time.Duration) bool {
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// ProbeHostPorts 探测单台主机上的指定端口，返回开放的端口号（升序）。
func ProbeHostPorts(ctx context.Context, host string, ports []int, timeout time.Duration) []int {
	if timeout <= 0 {
		timeout = 400 * time.Millisecond
	}

	var mu sync.Mutex
	var result []int
	sem := make(chan struct{}, 64)
	var wg sync.WaitGroup

	for _, p := range ports {
		wg.Add(1)
		go func(port int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if dialOnce(ctx, host, port, timeout) {
				mu.Lock()
				result = append(result, port)
				mu.Unlock()
			}
		}(p)
	}
	wg.Wait()
	sort.Ints(result)
	return result
}

// LocalSubnets 返回本机所在的候选 /24 网段（已按可信度排序）。
//
// 具体排序与排除规则见 DetectNetwork：核心是别把 Docker 的 172.17/172.18
// 当成"局域网"，否则用户点了扫局域网，扫到的是一台设备都没有的容器网络。
func LocalSubnets() []string {
	return DetectNetwork().Subnets
}

// ExpandSubnet 把一个 /24 前缀展开成 1~254 的主机地址列表。
// 刻意跳过 .0（网络地址）与 .255（广播地址）。
func ExpandSubnet(prefix string) []string {
	out := make([]string, 0, 254)
	for i := 1; i <= 254; i++ {
		out = append(out, fmt.Sprintf("%s.%d", prefix, i))
	}
	return out
}

// NormalizeHostInput 清理用户输入的主机串（容忍直接粘贴进来的 URL）。
// 导出是给 HTTP 层的「测试连通」复用，保证前后端对同一段输入的解读一致。
func NormalizeHostInput(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	// 去掉协议前缀。
	if i := strings.Index(raw, "//"); i >= 0 {
		raw = raw[i+2:]
	}
	// 去掉路径 / 查询 / 锚点。
	if i := strings.IndexAny(raw, "/?#"); i >= 0 {
		raw = raw[:i]
	}
	// 去掉端口（本函数只关心主机部分）。
	if h, _, err := net.SplitHostPort(raw); err == nil {
		raw = h
	}
	return strings.TrimSpace(raw)
}
