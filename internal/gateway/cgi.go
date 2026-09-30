package gateway

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/cgi"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"
)

// RunCGI 以 CGI 进程身份运行：把网关交给我们的请求转发给正在监听套接字的主服务。
//
// 触发时机是「网关把 index.cgi 当作可执行文件启动」，因此这段代码运行在一个
// 短命的子进程里、通过 stdin/stdout 与网关对话。它自身不持有任何业务状态。
//
// 若主服务没在监听（例如应用未启用），返回一个说明页而不是让网关拿到空响应 ——
// 后者在浏览器里表现为一片空白，用户完全不知道发生了什么。
func RunCGI(socketPath string) {
	path := ResolveSocket(socketPath, "qlink2desktop")

	req, err := cgi.Request()
	if err != nil {
		writeFatal(os.Stdout, fmt.Sprintf("无法解析 CGI 请求：%v", err))
		return
	}

	// 剥掉 CGI 路径前缀，例如：
	// /cgi/ThirdParty/qlink2d.xxx/index.cgi/redirect/qlink2d.xxx/_
	//                                        ^^^^^^^^^^^^^^^^^^^^^^^^^^^^ 这部分才是业务路径
	rewriteCGIPath(req)

	proxy := newSocketProxy(path)
	rw := newCGIResponseWriter(os.Stdout, req)
	proxy.ServeHTTP(rw, req)
}

// rewriteCGIPath 把 CGI 请求里的路径还原成业务路径。
func rewriteCGIPath(req *http.Request) {
	p := req.URL.Path
	if idx := strings.Index(p, "index.cgi"); idx >= 0 {
		p = p[idx+len("index.cgi"):]
		if p == "" {
			p = "/"
		}
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	req.URL.Path = p
	if req.URL.RawQuery != "" {
		req.RequestURI = p + "?" + req.URL.RawQuery
	} else {
		req.RequestURI = p
	}
}

// newSocketProxy 构造一个直接拨号到 Unix 套接字的反向代理。
func newSocketProxy(socketPath string) *httputil.ReverseProxy {
	target := &url.URL{Scheme: "http", Host: "unix"}
	proxy := httputil.NewSingleHostReverseProxy(target)
	origDirector := proxy.Director

	proxy.Director = func(req *http.Request) {
		origDirector(req)

		// 把原始 Host / 协议透传给后端：门户据此判断"当前是不是 https"，
		// 从而决定登录 Cookie 要不要带 Secure 标志。
		if host := originalHost(req); host != "" {
			req.Host = host
			req.Header.Set("X-Forwarded-Host", host)
		}
		req.Header.Set("X-Forwarded-Proto", originalProto())
	}

	proxy.Transport = &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		},
		// 网关侧是同步等待的，超时设短一些，让失败尽快以说明页呈现。
		ResponseHeaderTimeout: 30 * time.Second,
	}

	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(unavailableHTML))
	}

	return proxy
}

// originalHost 还原浏览器实际访问的主机名。
//
// CGI 环境里 r.Host 往往是 localhost，直接用会让后端的跳转地址变成
// "http://localhost/..."，用户在公网访问时就会被踢回自己家里。
func originalHost(req *http.Request) string {
	for _, key := range []string{"HTTP_X_FORWARDED_HOST", "HTTP_HOST", "SERVER_NAME"} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" && v != "localhost" && v != "127.0.0.1" {
			return v
		}
	}
	if req.Host != "" && req.Host != "localhost" && req.Host != "127.0.0.1" {
		return req.Host
	}
	return ""
}

func originalProto() string {
	if p := strings.TrimSpace(os.Getenv("HTTP_X_FORWARDED_PROTO")); p != "" {
		return p
	}
	if os.Getenv("HTTPS") == "on" || os.Getenv("SERVER_PORT") == "443" {
		return "https"
	}
	return "http"
}

// ---------------------------------------------------------------------------
// CGI 响应写出
// ---------------------------------------------------------------------------

// cgiResponseWriter 把 http.ResponseWriter 的调用翻译成 CGI 响应报文。
//
// 刻意不使用 net/http/cgi 内部的那套实现：这里需要显式控制状态行与
// 头部分隔符的空行，任何一处偏差都会让网关一直等待、页面永远转圈。
type cgiResponseWriter struct {
	out      *os.File
	req      *http.Request
	wrote    bool
	header   http.Header
	status   int
	isStream bool
}

func newCGIResponseWriter(out *os.File, req *http.Request) *cgiResponseWriter {
	return &cgiResponseWriter{
		out:    out,
		req:    req,
		header: make(http.Header),
		status: http.StatusOK,
	}
}

func (w *cgiResponseWriter) Header() http.Header { return w.header }

func (w *cgiResponseWriter) WriteHeader(status int) {
	if w.wrote {
		return
	}
	w.status = status
	w.flushHeaders()
}

func (w *cgiResponseWriter) Write(p []byte) (int, error) {
	if !w.wrote {
		w.flushHeaders()
	}
	n, err := w.out.Write(p)
	if w.isStream {
		if f, ok := any(w.out).(interface{ Sync() error }); ok {
			_ = f.Sync()
		}
		return n, err
	}
	return n, err
}

// Flush 让 SSE 能真正逐条推给网关。
func (w *cgiResponseWriter) Flush() {
	if !w.wrote {
		w.flushHeaders()
	}
	// os.File 没有 Flush；对管道而言 Write 已经是即时可见的。
	w.isStream = true
}

func (w *cgiResponseWriter) flushHeaders() {
	w.wrote = true

	if ct := w.header.Get("Content-Type"); ct == "" {
		w.header.Set("Content-Type", "text/html; charset=utf-8")
	}
	// Status 行可选，但在需要非 200 状态码时必须给出。
	_, _ = fmt.Fprintf(w.out, "Status: %d %s\r\n", w.status, http.StatusText(w.status))
	for key, values := range w.header {
		for _, v := range values {
			_, _ = fmt.Fprintf(w.out, "%s: %s\r\n", key, v)
		}
	}
	_, _ = fmt.Fprint(w.out, "\r\n")
}

func writeFatal(out *os.File, msg string) {
	_, _ = fmt.Fprintf(out,
		"Status: 500 Internal Server Error\r\nContent-Type: text/html; charset=utf-8\r\n\r\n"+
			"<!DOCTYPE html><html><head><meta charset=\"utf-8\"><title>启动失败</title></head>"+
			"<body style=\"font-family:system-ui;padding:2rem\"><h2>QLink2Desktop 启动失败</h2><p>%s</p></body></html>",
		msg)
}

const unavailableHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>服务未就绪</title>
<style>
  body { margin:0; min-height:100vh; display:flex; align-items:center; justify-content:center;
         background:#f8fafc; color:#334155;
         font-family:-apple-system,BlinkMacSystemFont,"Segoe UI","PingFang SC","Microsoft YaHei",sans-serif; }
  .card { max-width:400px; width:88%; padding:2.25rem 1.75rem; text-align:center;
          background:#fff; border-radius:14px; box-shadow:0 8px 24px -8px rgba(15,23,42,.12); }
  h1 { font-size:1.15rem; margin:0 0 .6rem; color:#e11d48; }
  p  { font-size:.9rem; line-height:1.65; margin:.35rem 0; color:#64748b; }
</style>
</head>
<body>
  <div class="card">
    <h1>QLink2Desktop 服务未就绪</h1>
    <p>后台服务当前不在运行状态。</p>
    <p>请到「飞牛应用中心」确认 QLink2Desktop 已启用，然后刷新本页。</p>
  </div>
</body>
</html>`
