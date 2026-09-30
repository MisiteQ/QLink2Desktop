package fnos

import (
	"fmt"
	"html"
	"strings"
)

// cgiScriptTemplate 生成子应用的 app/ui/index.cgi。
//
// 位置遵循官方约定：desktop_uidir=ui 时，桌面入口目录就是 app/ui/，
// 因此 .url 里声明的 /cgi/ThirdParty/<app>/index.cgi/… 也由这里的脚本承接。
//
// 它是「没有本地端口」的形态（网址快捷方式）唯一能跳出飞牛主机的出口：
// 入口配置里的 url 只能表达路径，host 永远是飞牛自己，只有 CGI 能返回 302
// 把浏览器发往真正的目标站点。
//
// ---------------------------------------------------------------------------
// 为什么脚本自己把活干完，不再转交给主程序（一次真机事故换来的结论）
// ---------------------------------------------------------------------------
// 早期版本在这里做三步：
//
//  1. 在一串候选路径里找主程序二进制（变量里混着未保护的 ${TRIM_APPDEST}）；
//  2. 找统一网关的 Unix Socket；
//  3. 两者都在就 `exec 主程序 --mode cgi …`。
//
// 真机实测（带 session 访问）一步步暴露了三个独立缺陷：
//
//  1. 脚本开头有 `set -u`，而 `${TRIM_APPDEST}` 在 CGI 环境下根本不存在。
//     bash 直接以 "unbound variable" 终止 —— **一行输出都没有**，
//     网关拿不到响应头，只能返回 500。用户看到的就是"网页出错了"。
//  2. 那串 exec 参数里的 `--app` 主程序压根没定义这个 flag
//     （`RunCGI` 内部是固定用 "qlink2desktop" 的）。`flag.Parse()` 报
//     "flag provided but not defined: -app" 并以状态码 2 退出 —— 又是 500。
//  3. 更要命的是 `exec` 会**替换**进程：只要它被调用，后面那段兜底 302
//     就永远执行不到。所谓"双保险"其实是一个坏掉的开关 ——
//     主程序那条路一旦失败，兜底根本没机会接。
//
// 而且即便前两个都修好，exec 过去也是死路：**服务端没有任何 `/redirect/…`
// 路由**（全项目 grep 不到），代理过去只会得到 404。
//
// 结论："跳到目标网址"这件事**不需要主程序** —— 一个 302 响应而已，
// 纯 shell 内建命令就能完成。少一层依赖就少一个故障点，这是纯粹的净收益。
const cgiScriptTemplate = `#!/bin/bash
# 由 QLink2Desktop 自动生成 —— 请勿手工修改，重新安装会覆盖。
#
# 这个脚本只做一件事：返回 302，把浏览器送到目标网址。
#
# 注意不要用 set -u 之外的"严格模式"去引用任何环境变量：CGI 环境下
# TRIM_* 系列**不一定存在**，未保护的展开会让脚本在第零行就死掉，
# 而没有任何输出 ⇔ 网关返回 500 ⇔ 用户看到一片错误页。
set -u

# 目标地址由 Go 侧按 shell 单引号规则拼好，这里不再自己加引号。
TARGET=%s

# 没有目标地址时给一张说明页，而不是让脚本因为没有输出变成 500。
# 「明确说清楚缺少什么」永远好过「用户对着错误页猜」。
if [ -z "${TARGET}" ]; then
  printf 'Status: 400 Bad Request\r\n'
  printf 'Content-Type: text/html; charset=utf-8\r\n'
  printf '\r\n'
  printf '<!DOCTYPE html><html lang="zh-CN"><head><meta charset="utf-8"><title>未配置目标地址</title></head>'
  printf '<body style="font-family:system-ui,sans-serif;padding:24px;color:#475569">'
  printf '<p>这个桌面入口没有可跳转的目标地址，请在 QLink2Desktop 里重新编辑该链接。</p>'
  printf '</body></html>'
  exit 0
fi

printf 'Status: 302 Found\r\n'
printf 'Location: %%s\r\n' "${TARGET}"
printf 'Cache-Control: no-store\r\n'
printf 'Content-Type: text/html; charset=utf-8\r\n'
printf '\r\n'

# 响应体是兜底：绝大多数情况下浏览器拿到上面的 302 头就走了，根本不会渲染它。
# 留着是因为某些中间层会吃掉 Location 头 —— 那时这张页面还能把人送过去。
cat << 'QLINK_CGI_FALLBACK'
%s
QLINK_CGI_FALLBACK
exit 0
`

// buildCGIScript 组装 CGI 脚本。
//
// 模板里有两个占位符：302 目标（已按 shell 规则加引号）与兜底页面。
//
// 目标地址来自用户输入，**必须按 shell 的引号规则处理**，不能只做 HTML 转义 ——
// 含单引号或换行的地址会直接改掉脚本结构，等于给了用户一个在 NAS 上执行命令的入口。
//
// 兜底 HTML 是**格式化结果的一部分**（作为第二个参数的值插入），
// 所以即便目标网址里带 `%` 也不会被 fmt 再次当作占位符处理。
func buildCGIScript(target, fallbackHTML string) string {
	return fmt.Sprintf(cgiScriptTemplate, shellQuote(target), fallbackHTML)
}

// shellQuote 把字符串安全地放进 sh 的单引号里。
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// sanitizeTarget 去掉目标地址里的控制字符。
//
// 目标地址会被 CGI 原样写进 `Location:` 响应头。哪怕只有一个换行，也等于
// 给了响应拆分的口子（攻击者能在响应里插入任意头）。单引号保护不了换行 ——
// sh 的单引号可以跨行 —— 所以必须在拼进脚本之前就把控制字符清掉。
func sanitizeTarget(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

// redirectPage 是跳转模式的兜底页面：立刻跳到目标地址。
//
// 用 meta refresh + JS 双保险：飞牛桌面弹窗里偶尔会拦截纯 JS 跳转。
// CGI 那边还会先发一个真正的 302 响应头，这张页面只是"头没被处理"时的兜底。
func redirectPage(target string) string {
	target = strings.TrimSpace(target)
	if target == "" {
		// 正常流程不会走到这里（无端口形态要么是网址快捷方式、要么走端口模式）。
		// 真出现了也别说"正在跳转"——那会让用户一直等一个不存在的东西。
		return `<!DOCTYPE html>
<html lang="zh-CN"><head><meta charset="utf-8"><title>未配置目标地址</title></head>
<body style="font-family:system-ui,sans-serif;padding:24px;color:#475569">
<p>这个入口没有可跳转的目标地址，请在 QLink2Desktop 里重新编辑该链接。</p>
</body></html>`
	}
	escaped := html.EscapeString(target)
	return fmt.Sprintf(`<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<meta http-equiv="refresh" content="0; url=%s">
<title>正在跳转…</title>
<style>
  body{margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;
       font-family:-apple-system,BlinkMacSystemFont,"Segoe UI","PingFang SC","Microsoft YaHei",sans-serif;
       background:#f8fafc;color:#475569}
  a{color:#2563eb}
</style>
</head>
<body>
<p>正在跳转至 <a href="%s">%s</a>…</p>
<script>window.location.replace(%q);</script>
</body>
</html>`, escaped, escaped, escaped, target)
}
