package proxy

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestProbeReachable(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	defer srv.Close()

	got := Probe(context.Background(), srv.URL, 3*time.Second)
	if !got.OK {
		t.Fatalf("应探测成功，实际: %+v", got)
	}
	if got.StatusCode != http.StatusTeapot {
		t.Errorf("状态码应为 418，实际 %d", got.StatusCode)
	}
	if !strings.Contains(got.Message, "连接正常") {
		t.Errorf("提示文案不正确: %q", got.Message)
	}
}

func TestProbeUnreachable(t *testing.T) {
	t.Parallel()

	// 连到一个已经关闭的端口。
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	got := Probe(context.Background(), url, 2*time.Second)
	if got.OK {
		t.Fatal("应探测失败")
	}
	if !strings.Contains(got.Message, "连接失败") {
		t.Errorf("提示文案不正确: %q", got.Message)
	}
}

func TestProbeToleratesSelfSignedTLS(t *testing.T) {
	t.Parallel()

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// httptest 的 TLS 证书是自签的；探测必须忽略证书问题，否则会误报服务不可达。
	got := Probe(context.Background(), srv.URL, 3*time.Second)
	if !got.OK {
		t.Fatalf("自签证书不应导致探测失败: %+v", got)
	}
}

func TestProbeEmptyTarget(t *testing.T) {
	t.Parallel()

	if got := Probe(context.Background(), "   ", time.Second); got.OK {
		t.Error("空地址应探测失败")
	}
}

func TestProbeAddsScheme(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	// 去掉 http:// 前缀后仍应可用。
	host := strings.TrimPrefix(srv.URL, "http://")
	if got := Probe(context.Background(), host, 3*time.Second); !got.OK {
		t.Fatalf("应自动补全协议: %+v", got)
	}
}

func TestPortAvailable(t *testing.T) {
	t.Parallel()

	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	busy := ln.Addr().(*net.TCPAddr).Port

	if PortAvailable(busy) {
		t.Errorf("端口 %d 正在被占用，应报告不可用", busy)
	}
	if PortAvailable(0) || PortAvailable(70000) {
		t.Error("越界端口应报告不可用")
	}
}

func TestPickPortAvoidsRequestedPorts(t *testing.T) {
	t.Parallel()

	first := PickPort(DefaultProxyPortBase, nil)
	if first == 0 {
		t.Fatal("应能分配到一个端口")
	}

	avoid := map[int]bool{first: true}
	second := PickPort(DefaultProxyPortBase, avoid)
	if second == 0 {
		t.Fatal("应能分配第二个端口")
	}
	if second == first {
		t.Fatalf("应避开已被占用的端口 %d", first)
	}
}

func TestPickPortFallsBackForInvalidBase(t *testing.T) {
	t.Parallel()

	// 非法起点应回落到默认区间，而不是返回 0。
	if got := PickPort(80, nil); got == 0 {
		t.Error("非法起点应回落到默认区间")
	}
	if got := PickPort(-5, nil); got == 0 {
		t.Error("非法起点应回落到默认区间")
	}
}
