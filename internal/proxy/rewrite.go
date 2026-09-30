package proxy

import (
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// newDirector 构造请求改写函数。
//
// 反向代理最容易踩的坑几乎都在请求头上，这里逐条说明处理理由：
//
//  1. **Host 改成后端 Host。** 否则大量后端（Gitea、qBittorrent、Nginx vhost）
//     会因为 Host 不匹配而返回 404 或直接拒绝。
//
//  2. **Origin / Referer 改写为后端 origin。**
//     这些服务普遍开启 CSRF 校验：请求里 Origin 是 `http://localhost:18000`，
//     而后端认为自己应该是 `http://192.168.1.10:3000`，于是判定为跨站攻击并拒绝写入。
//     原始值放进 X-Original-Origin 备查。
//
//  3. **路径前缀去重。** 若目标地址自带路径前缀（如 `http://host/gitea`），
//     而客户端请求路径也已包含该前缀，标准库会再拼一次，形成 `/gitea/gitea/...`。
func newDirector(target *url.URL) func(*http.Request) {
	baseDirector := func(req *http.Request) {
		req.URL.Scheme = target.Scheme
		req.URL.Host = target.Host
		req.URL.Path, req.URL.RawPath = joinURLPath(target, req.URL)
		if target.RawQuery == "" || req.URL.RawQuery == "" {
			req.URL.RawQuery = target.RawQuery + req.URL.RawQuery
		} else {
			req.URL.RawQuery = target.RawQuery + "&" + req.URL.RawQuery
		}
		if _, ok := req.Header["User-Agent"]; !ok {
			// 显式置空可避免标准库补上 Go 的默认 UA，某些后端会按 UA 拒绝。
			req.Header.Set("User-Agent", "")
		}
	}

	targetOrigin := (&url.URL{Scheme: schemeOf(target), Host: target.Host}).String()

	return func(req *http.Request) {
		incomingHost := req.Host
		if incomingHost == "" {
			incomingHost = req.Header.Get("Host")
		}
		incomingOrigin := req.Header.Get("Origin")
		incomingReferer := req.Header.Get("Referer")

		baseDirector(req)
		req.Host = target.Host

		if incomingOrigin != "" {
			req.Header.Set("Origin", targetOrigin)
			req.Header.Set("X-Original-Origin", incomingOrigin)
		}
		if incomingReferer != "" {
			if refURL, err := url.Parse(incomingReferer); err == nil {
				refURL.Scheme = schemeOf(target)
				refURL.Host = target.Host
				req.Header.Set("Referer", refURL.String())
				req.Header.Set("X-Original-Referer", incomingReferer)
			}
		}

		// 标准转发头
		if clientIP, _, err := net.SplitHostPort(req.RemoteAddr); err == nil {
			if prior := req.Header.Get("X-Forwarded-For"); prior != "" {
				clientIP = prior + ", " + clientIP
			}
			req.Header.Set("X-Forwarded-For", clientIP)
		}
		if req.TLS != nil {
			req.Header.Set("X-Forwarded-Proto", "https")
		} else {
			req.Header.Set("X-Forwarded-Proto", "http")
		}
		if incomingHost != "" {
			req.Header.Set("X-Forwarded-Host", incomingHost)
		}

	}
}

// joinURLPath 合并目标路径与请求路径，并消除重复前缀。
func joinURLPath(target, req *url.URL) (path, rawPath string) {
	base := strings.TrimRight(target.Path, "/")
	reqPath := req.Path
	if reqPath == "" {
		reqPath = "/"
	}
	if base == "" {
		return reqPath, req.RawPath
	}
	// 请求路径已经带了目标前缀 → 不再重复拼接。
	if reqPath == base || strings.HasPrefix(reqPath, base+"/") {
		return reqPath, req.RawPath
	}
	return base + reqPath, req.RawPath
}

func schemeOf(u *url.URL) string {
	if u.Scheme == "" {
		return "http"
	}
	return u.Scheme
}

// modifyResponse 修正后端响应，使其能在飞牛桌面的 iframe 里正常工作。
//
// 五项处理，每一项都对应一个真实踩过的坑：
//
//  1. **Location 改写成相对路径。** 后端返回 `http://192.168.1.10:3000/login` 时，
//     浏览器会直接跳到那个内网地址，绕过代理（在外网环境下必然失败）。
//
//  2. **删除 X-Frame-Options。** 绝大多数应用默认 `DENY`/`SAMEORIGIN`，
//     会导致飞牛桌面弹窗里一片空白。
//
//  3. **移除 CSP 的 frame-ancestors 指令。** 新一点的框架改用 CSP 实现同样的限制。
//
//  4. **补 CORS 头。** 应用内部的 AJAX 会以 `http://localhost:18000` 为源，
//     与后端 origin 不同，缺了这些头会被浏览器拦下。
//
//  5. **剥掉 Set-Cookie 的 Domain 属性。** 后端常带 `Domain=192.168.1.10`，
//     浏览器会拒绝把它存到当前域名下，表现为「登录成功但一刷新就掉登录」。
func modifyResponse(resp *http.Response) error {
	rewriteLocation(resp)

	resp.Header.Del("X-Frame-Options")
	relaxCSP(resp)
	applyCORS(resp)
	stripCookieDomain(resp)

	return nil
}

func rewriteLocation(resp *http.Response) {
	loc := resp.Header.Get("Location")
	if loc == "" {
		return
	}
	locURL, err := url.Parse(loc)
	if err != nil || locURL.Host == "" {
		return // 已经是相对路径
	}
	if resp.Request != nil && resp.Request.URL != nil && !strings.EqualFold(locURL.Host, resp.Request.URL.Host) {
		return // 跳去别的主机，不属于本代理职责
	}
	rel := locURL.RequestURI()
	if locURL.Fragment != "" {
		rel += "#" + locURL.Fragment
	}
	if rel == "" {
		rel = "/"
	}
	resp.Header.Set("Location", rel)
}

func relaxCSP(resp *http.Response) {
	csp := resp.Header.Get("Content-Security-Policy")
	if csp == "" {
		return
	}
	var kept []string
	for _, part := range strings.Split(csp, ";") {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(part)), "frame-ancestors") {
			continue
		}
		if strings.TrimSpace(part) != "" {
			kept = append(kept, strings.TrimSpace(part))
		}
	}
	if len(kept) == 0 {
		resp.Header.Del("Content-Security-Policy")
		return
	}
	resp.Header.Set("Content-Security-Policy", strings.Join(kept, "; "))
}

func applyCORS(resp *http.Response) {
	if resp.Request == nil {
		return
	}
	origin := resp.Request.Header.Get("X-Original-Origin")
	if origin == "" {
		return
	}
	h := resp.Header
	h.Set("Access-Control-Allow-Origin", origin)
	h.Set("Access-Control-Allow-Credentials", "true")
	h.Set("Access-Control-Allow-Methods", allowedMethods)
	h.Set("Access-Control-Allow-Headers", allowedHeaders)
	h.Add("Vary", "Origin")
}

func stripCookieDomain(resp *http.Response) {
	cookies := resp.Header["Set-Cookie"]
	if len(cookies) == 0 {
		return
	}
	cleaned := make([]string, 0, len(cookies))
	for _, c := range cookies {
		var kept []string
		for _, attr := range strings.Split(c, ";") {
			if strings.HasPrefix(strings.ToLower(strings.TrimSpace(attr)), "domain=") {
				continue
			}
			if strings.TrimSpace(attr) != "" {
				kept = append(kept, strings.TrimSpace(attr))
			}
		}
		cleaned = append(cleaned, strings.Join(kept, "; "))
	}
	resp.Header["Set-Cookie"] = cleaned
}

// --------------------------------------------------------------------------
// CORS 预检
// --------------------------------------------------------------------------

const (
	allowedMethods = "GET, POST, PUT, DELETE, PATCH, OPTIONS, HEAD"
	allowedHeaders = "*, Authorization, Content-Type, X-Requested-With, Cookie"
)

func isPreflight(r *http.Request) bool {
	return r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != ""
}

func writePreflight(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin == "" {
		origin = "*"
	}
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", origin)
	h.Set("Access-Control-Allow-Credentials", "true")
	h.Set("Access-Control-Allow-Methods", allowedMethods)
	h.Set("Access-Control-Allow-Headers", allowedHeaders)
	h.Set("Access-Control-Max-Age", "86400")
	w.WriteHeader(http.StatusNoContent)
}

// writeProxyError 用一张可读的错误页取代标准库默认的空白 502，
// 让用户一眼看出「是代理连不上后端」而不是「页面崩了」。
func writeProxyError(w http.ResponseWriter, target string, cause error) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusBadGateway)
	fmt.Fprintf(w, `<!DOCTYPE html>
<html lang="zh-CN"><head><meta charset="utf-8"><title>代理连接失败</title>
<style>
  body{margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;
       font-family:-apple-system,BlinkMacSystemFont,"Segoe UI","PingFang SC","Microsoft YaHei",sans-serif;
       background:#f8fafc;color:#0f172a}
  .card{max-width:520px;padding:24px;background:#fff;border:1px solid #e2e8f0;border-radius:14px;
        box-shadow:0 10px 25px -5px rgba(0,0,0,.08)}
  h2{margin:0 0 12px;font-size:1.05rem}
  p{margin:6px 0;font-size:.9rem;color:#475569;line-height:1.6}
  code{background:#f1f5f9;padding:2px 6px;border-radius:4px;font-size:.85em;word-break:break-all}
</style></head>
<body><div class="card">
  <h2>无法连接到目标服务</h2>
  <p>目标地址：<code>%s</code></p>
  <p>错误详情：<code>%s</code></p>
  <p>请确认目标服务正在运行，且本机可以访问该地址。</p>
</div></body></html>`, html.EscapeString(target), html.EscapeString(cause.Error()))
}
