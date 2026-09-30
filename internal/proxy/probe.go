package proxy

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// ProbeResult 是一次连通性探测的结果。
type ProbeResult struct {
	OK         bool   `json:"ok"`
	StatusCode int    `json:"status_code,omitempty"`
	LatencyMs  int64  `json:"latency_ms"`
	Message    string `json:"message"`
}

// Probe 探测目标地址是否可达。
//
// 刻意忽略 TLS 证书错误：这个探测的用途是回答「能不能连上」，
// 而不是「证书是否可信」。很多 NAS 服务用自签证书，
// 如果这里因为证书失败就报不可达，会误导用户以为服务挂了。
func Probe(ctx context.Context, target string, timeout time.Duration) ProbeResult {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	urlStr := strings.TrimSpace(target)
	if urlStr == "" {
		return ProbeResult{Message: "目标地址为空"}
	}
	if !strings.HasPrefix(urlStr, "http://") && !strings.HasPrefix(urlStr, "https://") {
		urlStr = "http://" + urlStr
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// 不跟随超过 3 跳；重定向本身也说明服务是活的。
			if len(via) >= 3 {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return ProbeResult{Message: fmt.Sprintf("地址非法: %v", err)}
	}

	start := time.Now()
	resp, err := client.Do(req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return ProbeResult{LatencyMs: latency, Message: fmt.Sprintf("连接失败: %v", err)}
	}
	defer resp.Body.Close()

	return ProbeResult{
		OK:         true,
		StatusCode: resp.StatusCode,
		LatencyMs:  latency,
		Message:    fmt.Sprintf("连接正常（HTTP %d，%dms）", resp.StatusCode, latency),
	}
}

// PortAvailable 报告本机端口是否可被监听。
func PortAvailable(port int) bool {
	if port <= 0 || port > 65535 {
		return false
	}
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

// 代理端口分配的默认起点与区间。
const (
	DefaultProxyPortBase = 18000
	maxPort              = 65535
)

// PickPort 从 base 开始寻找一个空闲端口。
//
// avoid 用于避开已知会被用到的端口（例如其它链接正在使用的代理端口、
// 或用户已经占用的服务端口），避免「刚分配的端口立刻被别的服务抢走」。
func PickPort(base int, avoid map[int]bool) int {
	if base <= 1024 || base >= maxPort {
		base = DefaultProxyPortBase
	}
	for p := base; p < maxPort; p++ {
		if avoid != nil && avoid[p] {
			continue
		}
		if PortAvailable(p) {
			return p
		}
	}
	// 随机端口兜底。
	if ln, err := net.Listen("tcp", ":0"); err == nil {
		defer ln.Close()
		if tcp, ok := ln.Addr().(*net.TCPAddr); ok {
			return tcp.Port
		}
	}
	return 0
}
