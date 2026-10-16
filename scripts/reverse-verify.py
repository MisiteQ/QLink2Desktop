#!/usr/bin/env python3
"""反向验证：把每条新守卫对应的事实改坏，确认检查真的会 FAIL。

「通过」和「没在测」长得一模一样 —— 这个脚本存在的唯一目的就是把这两者分开。
每个场景独立打补丁、单独跑一遍静态守卫、统计 FAIL 条数，然后无条件还原。

注意一个本项目踩过的坑：e2e-gateway-check.py 会去找可执行文件，**找不到就直接
退出、一条断言都不跑** —— 那看起来和"全绿"完全一样。所以这里必须确认产物存在，
并且把"跑了多少条"和"失败多少条"一起打出来。

用法：
    make reverse-verify            静态守卫的反向验证（39 个场景）
    make reverse-verify-gotest     行为型守卫的反向验证（跑 go test，5 个场景）
    make reverse-verify-browser    真视口守卫的反向验证（8 个场景，跑得慢）
    python scripts/reverse-verify.py <场景名>...            只跑指定场景

为什么要有"行为型"这一层
------------------------
静态断言只能看"代码里写了什么"，看不出"写的那行有没有真的被执行到"。
「把锐化那一行换成 return out」之后，函数名、常量、调用点全都还在 ——
只有真正量过边缘能量的单测才抓得住。所以凡是"改一下参数就悄悄失效"的
实现（缩放、锐化、预乘），都必须由行为断言来守。

为什么把它固化进仓库、而不是当一次性脚本用掉就删：
「改坏一份确认检查会 FAIL」如果每次都要现场手写，就一定会有人省掉它 ——
而这个项目已经不止一次遇到「守卫全绿，但用户报的问题依然存在」。
"""

from __future__ import annotations

import atexit
import os
import re
import shutil
import signal
import subprocess
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
EXE_CANDIDATES = [
    os.path.join(ROOT, "qlink2desktop.exe"),
    os.path.join(ROOT, ".build", "dev", "qlink2desktop.exe"),
]

# 备份目录必须落在仓库里（不能是系统临时目录）：它要能在**下一次启动**时
# 被发现，而"下一次启动"可能是重启、换终端、换 IDE 之后的事。
BACKUP_DIR = os.path.join(ROOT, ".build", "reverse-verify-backup")


class Workspace:
    """把「这次要改动的文件」先备份到磁盘，随时可以整体还原。

    为什么不能只把原文放在内存里：这个脚本会**故意**改坏源码再跑检查。
    一旦它自己被中途打断（前台超时、Ctrl-C、CI 取消、容器被杀），
    内存里的原文就跟着没了 —— 仓库里会留下一个"看起来正常、其实已经被
    改坏"的实现，而所有检查都是拿它跑出来的。

    这不是假设。缩放那一轮的反向验证就被前台超时 SIGTERM 打断过，在
    `internal/fnos/imageutil.go` 里留下了一行 `return out`（整个锐化被摘掉），
    而 `go test` 全绿 —— 因为被测的就是那份被改坏的代码。"验证脚本自己坏掉"
    与"守卫全绿"长得一模一样，所以这里必须落盘 + 下次启动先自愈。
    """

    def __init__(self, paths: list[str]):
        self.paths = sorted(set(paths))

    def _index(self) -> str:
        return os.path.join(BACKUP_DIR, "index.txt")

    def save(self) -> None:
        # 上一次留下的还没还，先还 —— 否则备份会把已经改坏的版本当成原文。
        healed = self.restore()
        if healed:
            print(f"!! 发现上一次反向验证被打断留下的改动，已还原 {healed} 个文件。\n")
        os.makedirs(BACKUP_DIR, exist_ok=True)
        index = []
        for i, rel in enumerate(self.paths):
            src = os.path.join(ROOT, rel)
            if not os.path.isfile(src):
                continue
            shutil.copy2(src, os.path.join(BACKUP_DIR, f"{i:03d}.bin"))
            index.append(rel)
        with open(self._index(), "wb") as f:
            f.write("\n".join(index).encode("utf-8"))

    def restore(self) -> int:
        """把备份里的文件全部写回（不删备份，场景之间可以反复调用）。"""
        if not os.path.isfile(self._index()):
            return 0
        with open(self._index(), "rb") as f:
            index = [ln for ln in f.read().decode("utf-8").split("\n") if ln]
        n = 0
        for i, rel in enumerate(index):
            blob = os.path.join(BACKUP_DIR, f"{i:03d}.bin")
            if not os.path.isfile(blob):
                continue
            shutil.copy2(blob, os.path.join(ROOT, rel))
            n += 1
        return n

    def clear(self) -> None:
        shutil.rmtree(BACKUP_DIR, ignore_errors=True)

    def install_healing(self) -> None:
        """正常退出与收到信号时都无条件还原。"""

        def on_signal(signum, _frame):
            n = self.restore()
            if n:
                print(f"\n!! 收到信号 {signum}，已把 {n} 个被改坏的文件还原。", flush=True)
            sys.exit(130)

        atexit.register(self.clear)
        atexit.register(self.restore)
        for sig in (getattr(signal, "SIGINT", None), getattr(signal, "SIGTERM", None)):
            if sig is None:
                continue
            try:
                signal.signal(sig, on_signal)
            except (ValueError, OSError):
                pass


# browser-e2e.py 支持分段跑；这里只用来当"全部"的兜底值。
ALL_SECTIONS = "nav,icon,discovery"

# (场景名, 相对路径, 原文, 改成)
SCENARIOS: list[tuple[str, str, str, str]] = [
    # --- 解析链结构 ---
    (
        "ResolveIcon 不再委派给 ResolveIconEx（守卫会盯着没人调的壳）",
        "internal/fnos/icon.go",
        "func ResolveIcon(req IconRequest) IconSet { return ResolveIconEx(req).IconSet }",
        "func ResolveIcon(req IconRequest) IconSet { return DefaultIconSet() }",
    ),
    (
        "favicon 那一档挪到官方图标库之前",
        "internal/fnos/icon.go",
        """		if set, keyword, ok := matchOfficialIconKeyword(req.Candidates); ok {
			return done(set, IconSourceOfficial,
				fmt.Sprintf("命中官方图标库（关键词 %s）", keyword))
		}
		if set, detail, ok := siteFaviconLookup(req.SiteURL, req.SkipTLSVerify); ok {
			return done(set, IconSourceFavicon, detail)
		}""",
        """		if set, detail, ok := siteFaviconLookup(req.SiteURL, req.SkipTLSVerify); ok {
			return done(set, IconSourceFavicon, detail)
		}
		if set, keyword, ok := matchOfficialIconKeyword(req.Candidates); ok {
			return done(set, IconSourceOfficial,
				fmt.Sprintf("命中官方图标库（关键词 %s）", keyword))
		}""",
    ),
    (
        "favicon 抓取不再受「允许远端图标」管辖（偷偷发内网请求）",
        "internal/fnos/icon.go",
        "	if req.AllowRemote {\n		if img := loadFromInline(req.Icon.Ref); img != nil {",
        "	if true {\n		if img := loadFromInline(req.Icon.Ref); img != nil {",
    ),
    (
        "官方图标库匹配去掉总预算（最坏挂几分钟）",
        "internal/fnos/icon.go",
        "	ctx, cancel := context.WithTimeout(context.Background(), officialIconBudget)",
        "	ctx, cancel := context.WithCancel(context.Background())",
    ),
    (
        "官方图标库匹配不再回报命中的关键词",
        "internal/fnos/icon.go",
        "				return set, name, true",
        "				return set, \"\", true",
    ),
    # --- 过程可见 ---
    (
        "解析结果只剩图标，不再带来源与说明",
        "internal/fnos/icon.go",
        "	Source IconSource\n	// Detail 是一句给用户看的话：命中了什么关键词、问了哪个地址、为什么没找到。\n	Detail string",
        "	Origin string",
    ),
    (
        "抓取失败不再说明问了哪个地址",
        "internal/fnos/icon.go",
        "		return done(DefaultIconSet(), IconSourceDefault, faviconMissDetail(req.SiteURL))",
        "		return done(DefaultIconSet(), IconSourceDefault, \"没找到图标\")",
    ),
    (
        "离线模式不再说明是开关关掉了抓取",
        "internal/fnos/icon.go",
        "		\"远端图标已关闭，未去站点查找；如需自动抓取请在设置里打开「允许远端图标」\")",
        "		\"没找到图标\")",
    ),
    (
        "解析接口不再宽松解码（链接多一个字段就把预览打挂）",
        "internal/httpapi/icons.go",
        "	if !decodeJSONLenient(w, r, &req) {",
        "	if !decodeJSON(w, r, &req) {",
    ),
    (
        "解析接口不再回预览图",
        "internal/httpapi/icons.go",
        '		body["preview"] = "data:image/png;base64," + base64.StdEncoding.EncodeToString(res.Large)',
        '		_ = res.Large',
    ),
    (
        "解析接口的响应丢掉 message / detail",
        "internal/httpapi/icons.go",
        '		"detail":     res.Detail,',
        '		"x-detail":   res.Detail,',
    ),
    (
        "前端不再调用解析接口（接口写了没人用）",
        "internal/webui/web/assets/modules/views/editor.js",
        "      const res = await api.icons.resolve(toPayload(model));",
        "      const res = { source: 'default', message: '未找到图标', detail: '' };",
    ),
    (
        "编辑器不再显示「正在查找」",
        "internal/webui/web/assets/modules/views/editor.js",
        "    h('span', { class: 'tag busy' }, [h('span', { class: 'spinner-sm' }), '正在查找图标…']),",
        "    h('span', { class: 'tag busy' }, ['…']),",
    ),
    (
        "状态行建了却没挂进界面",
        "internal/webui/web/assets/modules/views/editor.js",
        "          statusBox,\n        ]),",
        "        ]),",
    ),
    (
        "改地址不再触发重新查找",
        "internal/webui/web/assets/modules/views/editor.js",
        "    retarget: scheduleLookup,",
        "    retarget: () => {},",
    ),
    (
        "状态行样式被删掉",
        "internal/webui/web/assets/style.css",
        ".icon-status { display: inline-flex; align-items: center; gap: 8px; flex-wrap: wrap; }",
        "",
    ),
    # --- 列表缩略图 ---
    (
        "自动图标不再落盘（列表拿不到真实图标）",
        "internal/fnos/service.go",
        "	res := s.ResolveLinkIcon(l)\n\n	changed := s.writeAutoIcon(dir, name, res)",
        "	res := s.ResolveLinkIcon(l)\n\n	changed := false\n	_ = s.writeAutoIcon",
    ),
    (
        "视图不再带上自动图标文件名",
        "internal/httpapi/server.go",
        "		views[i].IconAuto = s.deps.Service.AutoIconFile(l)",
        "		views[i].IconAuto = \"\"",
    ),
    (
        "列表缩略图不再用解析出来的图标",
        "internal/webui/web/assets/modules/views/desktop.js",
        "  if (view.icon_auto) return api.icons.url(view.icon_auto);",
        "  if (view.icon_auto) return api.icons.defaultURL(64);",
    ),
    (
        "自动图标混进用户图标库",
        "internal/httpapi/icons.go",
        "		if fnos.IsAutoIconFile(e.Name()) {\n			continue\n		}",
        "",
    ),
    (
        "浏览器回归脚本被摘掉（过程可见失去真视口证据）",
        "scripts/browser-e2e.py",
        "                [node, ICON_SCRIPT, url],",
        "                [node, MOBILE_SCRIPT, url],",
    ),
    # --- 服务发现：端口行不再重复 ---
    (
        "扫描出口不再合并同一服务的多个套接字（一个 sshd 显示成两条）",
        "internal/discovery/scanner.go",
        "	out = GroupPorts(out)",
        "	_ = GroupPorts",
    ),
    (
        "合并挪到容器关联之前（udp 那一半再也匹配不上容器）",
        "internal/discovery/scanner.go",
        """	if s.docker != nil {
		s.docker.Enrich(out)
	}

	// 合并放在最后：先按套接字补齐进程与容器信息，再把同一服务收成一行。
	out = GroupPorts(out)""",
        """	out = GroupPorts(out)

	if s.docker != nil {
		s.docker.Enrich(out)
	}""",
    ),
    (
        "把绑定地址算进「同一个服务」的身份（同端口的 tcp/udp 永远合不到一起）",
        "internal/discovery/group.go",
        """	b.WriteByte('|')
	b.WriteString(strconv.Itoa(p.Port))
	return b.String()""",
        """	b.WriteString(p.Address)
	b.WriteByte('|')
	b.WriteString(strconv.Itoa(p.Port))
	return b.String()""",
    ),
    (
        "通配地址的判断在前端各写一份（:: 会被当成「没有地址」）",
        "internal/webui/web/assets/modules/views/discovery.js",
        "  const list = p.addresses || (p.address ? [p.address] : []);\n  if (!list.length) return '';",
        "  if (p.address === '0.0.0.0') return '全网卡可见';\n  const list = p.addresses || (p.address ? [p.address] : []);\n  if (!list.length) return '';",
    ),
    (
        "协议标签建了但不写内容（同地址同端口的两行又变得一模一样）",
        "internal/webui/web/assets/modules/views/discovery.js",
        "      h('span', { class: 'port-proto', text: protoLabel(p) }),",
        "      h('span', { class: 'port-proto', text: '' }),",
    ),
    (
        "协议标签的样式被删掉",
        "internal/webui/web/assets/style.css",
        """.port-proto {
  font-size: .6rem; font-weight: 600; letter-spacing: .05em;
  color: var(--text-muted); line-height: 1.2;
}""",
        "",
    ),
    (
        "合并前的套接字数不再报出来（101 → 8 会被当成漏了一堆端口）",
        "internal/httpapi/discovery.go",
        '	ports, sockets := s.deps.Scanner.Listening(queryBool(r, "force"))',
        '	ports, _ := s.deps.Scanner.Listening(queryBool(r, "force"))',
    ),
    (
        "合并结果里的绑定地址与协议集合字段被改名（前端只会显示空白）",
        "internal/discovery/types.go",
        '	Addresses []string `json:"addresses,omitempty"`',
        '	Addresses []string `json:"addrs,omitempty"`',
    ),
    (
        "服务发现真视口回归被摘掉（「两行长得一样」只有渲染出来才看得见）",
        "scripts/browser-e2e.py",
        "                    [node, DISCOVERY_SCRIPT, url],",
        "                    [node, MOBILE_SCRIPT, url],",
    ),
    # --- 图标缩放：别把 logo 压缩太狠 ---
    (
        "缩小时退回双线性插值（每个输出像素只采样 4 个源像素）",
        "internal/fnos/imageutil.go",
        "		out := ResizeArea(square, dstW, dstH)",
        "		out := ResizeBilinear(square, dstW, dstH)",
    ),
    (
        "锐化被摘掉（面积平均本身是低通滤波，边缘会重新变糊）",
        "internal/fnos/imageutil.go",
        "			return sharpenPremultiplied(out, sharpenAmount)",
        "			return out",
    ),
    (
        "锐化阈值被去掉（缩得很少也硬锐化，只是把噪声一起放大）",
        "internal/fnos/imageutil.go",
        "		if sw >= dstW*sharpenMinRatio || sh >= dstH*sharpenMinRatio {",
        "		if true {",
    ),
    (
        "面积平均退化成等权（等于「取一个代表点」）",
        "internal/fnos/imageutil.go",
        "					w := cx * cy",
        "					w := cx + cy",
    ),
    (
        "颜色运算不再预乘 alpha（透明区的颜色会渗进边缘）",
        "internal/fnos/imageutil.go",
        "	return premul{r: float64(c.R) * a / 255, g: float64(c.G) * a / 255, b: float64(c.B) * a / 255, a: a}",
        "	_ = a\n	return premul{r: float64(c.R), g: float64(c.G), b: float64(c.B), a: float64(c.A)}",
    ),
    (
        "放大路径不再走预乘（4 个采样点里有一个没预乘）",
        "internal/fnos/imageutil.go",
        "			c10 := premultiplied(srcRGBA.RGBAAt(x1, y0))",
        "			c10 := premul{r: float64(srcRGBA.RGBAAt(x1, y0).R), g: float64(srcRGBA.RGBAAt(x1, y0).G), b: float64(srcRGBA.RGBAAt(x1, y0).B), a: float64(srcRGBA.RGBAAt(x1, y0).A)}",
    ),
    (
        "旧的「反复折半」近似回来了（缩放有了第二条路径）",
        "internal/fnos/imageutil.go",
        "func clampInt(v, lo, hi int) int {",
        "func BoxDownscale(src image.Image) *image.RGBA { return nil }\n\nfunc clampInt(v, lo, hi int) int {",
    ),
    (
        "运行时默认图标不再与产品图标共用重采样路径",
        "internal/fnos/icon.go",
        "		return ResizeHighQuality(logo, size, size)",
        "		return ResizeBilinear(logo, size, size)",
    ),
    (
        "logo 的派生产物少生成一处（内嵌那张降到 64 档）",
        "cmd/iconforge/main.go",
        '		{filepath.Join(assetsDir, "logo_256.png"), set.Large, "内嵌默认图标"},',
        '		{filepath.Join(assetsDir, "logo_256.png"), set.Small, "内嵌默认图标"},',
    ),
]


# 真视口那一层单独验：这里的事实只有在浏览器里才看得见。
# 每条跑一次对应的那一段（--sections），所以最要紧的几条之外也放得下。
# (场景名, 相对路径, 原文, 改成, 是否要重新编译, 要跑哪一段)
BROWSER_SCENARIOS: list[tuple[str, str, str, str, bool, str]] = [
    # 注意：下面的 need_build 一律为 True。
    #
    # 前端资源是被 //go:embed 编进二进制的，改了磁盘上的 js/css **不重新编译
    # 等于没改** —— 浏览器跑的仍是旧代码。第一次跑这几条时全部 MISS，
    # 差一步就得出"浏览器守卫形同虚设"的错误结论，实际只是补丁没进二进制。
    # （同一个道理，出包之后必须解包 grep 确认新代码真的在里面。）
    (
        "编辑器不再显示「正在查找图标…」（用户要的那句话没了）",
        "internal/webui/web/assets/modules/views/editor.js",
        "'正在查找图标…'",
        "'请稍候'",
        True,
        "icon",
    ),
    (
        "状态行建了却没挂进界面",
        "internal/webui/web/assets/modules/views/editor.js",
        "          statusBox,\n        ]),",
        "        ]),",
        True,
        "icon",
    ),
    (
        "「没找到」被画成成功色（比不提示更糟）",
        "internal/webui/web/assets/modules/views/editor.js",
        "class: `tag ${ok ? 'ok' : 'warn'}`",
        "class: `tag ${ok ? 'ok' : 'ok'}`",
        True,
        "icon",
    ),
    (
        "解析接口改回严格解码（真机上就是这么挂的）",
        "internal/httpapi/icons.go",
        "	if !decodeJSONLenient(w, r, &req) {",
        "	if !decodeJSON(w, r, &req) {",
        True,
        "icon",
    ),
    # --- 服务发现：端口行还会不会重复 ---
    (
        "协议标签不写内容（同地址同端口的一对 tcp/udp 又变得一模一样）",
        "internal/webui/web/assets/modules/views/discovery.js",
        "      h('span', { class: 'port-proto', text: protoLabel(p) }),",
        "      h('span', { class: 'port-proto', text: '' }),",
        True,
        "discovery",
    ),
    (
        "地址归纳退回原样输出（界面上又出现「0.0.0.0」和一个像空白的「::」）",
        "internal/webui/web/assets/modules/views/discovery.js",
        "  if (p.scope === 'all') return '全网卡可见';\n  if (p.scope === 'local') return '仅本机';",
        "  if (p.scope === 'all') return p.address || '';\n  if (p.scope === 'local') return p.address || '';",
        True,
        "discovery",
    ),
    (
        "协议标签被样式压成不可见",
        "internal/webui/web/assets/style.css",
        ".port-proto {\n  font-size: .6rem; font-weight: 600; letter-spacing: .05em;",
        ".port-proto {\n  display: none;\n  font-size: .6rem; font-weight: 600; letter-spacing: .05em;",
        True,
        "discovery",
    ),
    (
        "重绘变成往后追加而不是替换（「最大化后又冒出很多」的真实成因）",
        "internal/webui/web/assets/modules/dom.js",
        "export function replace(node, children) {\n  clear(node);\n  append(node, children);",
        "export function replace(node, children) {\n  append(node, children);",
        True,
        "discovery",
    ),
]


# 行为型反向验证：把实现改坏，跑内部单测，确认它真的会 FAIL。
#
# 「把锐化那一行换成 return out」之后，函数名、常量、调用点一个不少 ——
# 纯静态的守卫全绿，而用户看到的图标会重新变糊。凡是"改一下参数就悄悄失效"
# 的实现（缩放、锐化、预乘），都只能靠量过像素的行为断言来守。
#
# (场景名, 相对路径, 原文, 改成, 是否要重新生成图标产物)
TEST_SCENARIOS: list[tuple[str, str, str, str, bool]] = [
    (
        "锐化被摘掉（面积平均是低通滤波，图标会重新变糊）",
        "internal/fnos/imageutil.go",
        "			return sharpenPremultiplied(out, sharpenAmount)",
        "			return out",
        False,
    ),
    (
        "颜色运算不再预乘 alpha（透明区的颜色会渗进边缘与锐化）",
        "internal/fnos/imageutil.go",
        "	return premul{r: float64(c.R) * a / 255, g: float64(c.G) * a / 255, b: float64(c.B) * a / 255, a: a}",
        "	_ = a\n	return premul{r: float64(c.R), g: float64(c.G), b: float64(c.B), a: float64(c.A)}",
        False,
    ),
    (
        "面积平均退化成等权（等于「取一个代表点」）",
        "internal/fnos/imageutil.go",
        "					w := cx * cy",
        "					w := cx + cy",
        False,
    ),
    (
        "面积平均整体不再按 alpha 加权（透明区的颜色渗进边缘）",
        "internal/fnos/imageutil.go",
        """					sumR += float64(c.R) * a * w
					sumG += float64(c.G) * a * w
					sumB += float64(c.B) * a * w
					sumA += a * w""",
        """					_ = a
					sumR += float64(c.R) * w
					sumG += float64(c.G) * w
					sumB += float64(c.B) * w
					sumA += w""",
        False,
    ),
    (
        "锐化阈值被去掉（缩得很少也硬锐化，噪声一起放大）",
        "internal/fnos/imageutil.go",
        "		if sw >= dstW*sharpenMinRatio || sh >= dstH*sharpenMinRatio {",
        "		if true {",
        False,
    ),
    (
        "logo 的派生产物只重新生成一半（漏了内嵌那张）",
        "cmd/iconforge/main.go",
        '		{filepath.Join(assetsDir, "logo_256.png"), set.Large, "内嵌默认图标"},',
        '		{filepath.Join(assetsDir, "logo_256.png"), set.Small, "内嵌默认图标"},',
        True,
    ),
]

# 上面最后一条会真的重写这些产物 —— 跑之前先快照、跑完无条件还原。
# "改了会留下痕迹"的验证最怕的就是改动没还回去，仓库里留下一个 64×64 的
# logo_256.png，而它看起来和"正常"没有任何区别。
ICON_ARTIFACTS = [
    "fnos-app/ICON.PNG",
    "fnos-app/ICON_256.PNG",
    "fnos-app/app/ui/images/icon_64.png",
    "fnos-app/app/ui/images/icon_256.png",
    "internal/fnos/assets/logo_256.png",
]


def pick_exe() -> str | None:
    for p in EXE_CANDIDATES:
        if os.path.isfile(p):
            return p
    return None


def run_static(exe: str) -> tuple[int, int, str]:
    # 必须显式指定子进程的输出编码：Windows 上默认是 GBK，
    # 按 utf-8 读回来中文会全变成替换字符，汇总行就再也匹配不上 ——
    # 表现是"基线都没跑起来"，与"守卫真的坏了"完全分不清。
    env = dict(os.environ, PYTHONIOENCODING="utf-8")
    proc = subprocess.run(
        [sys.executable, os.path.join(ROOT, "scripts", "e2e-gateway-check.py"), "--exe", exe],
        capture_output=True, text=True, cwd=ROOT,
        encoding="utf-8", errors="replace", env=env,
    )
    out = proc.stdout + proc.stderr
    # 汇总行里夹着 ANSI 颜色码（`通过 219 项<ESC>[0m，失败 0 项`）。
    # 不剥掉的话正则永远匹配不上，脚本会一口咬定"基线没跑起来" ——
    # 又一个"看起来在测、其实一条都没测"的同型陷阱。
    clean = re.sub(r"\x1b\[[0-9;]*m", "", out)
    m = re.search(r"通过\s*(\d+)\s*项，失败\s*(\d+)\s*项", clean)
    if not m:
        return -1, -1, out
    return int(m.group(1)), int(m.group(2)), out


def run_browser(exe: str, sections: str) -> tuple[int, str]:
    """跑一遍浏览器端到端（只跑指定的那一段），返回 (FAIL 条数, 原始输出)。"""
    env = dict(os.environ, PYTHONIOENCODING="utf-8")
    proc = subprocess.run(
        [sys.executable, os.path.join(ROOT, "scripts", "browser-e2e.py"),
         "--exe", exe, "--expect", "ok", "--sections", sections],
        capture_output=True, text=True, cwd=ROOT,
        encoding="utf-8", errors="replace", env=env, timeout=900,
    )
    out = proc.stdout + proc.stderr
    clean = re.sub(r"\x1b\[[0-9;]*m", "", out)
    fails = [ln.strip() for ln in clean.splitlines() if ln.strip().startswith("FAIL")]
    return len(fails), out


def find_go() -> str | None:
    """找 go 可执行文件。

    不能直接写 ["go", ...]：本机的 Go 装在 ~/sdk/goX.Y.Z/bin，**不在 PATH 里**，
    subprocess 会抛 FileNotFoundError。而一个 traceback 看起来像"守卫崩了"，
    与"这一段根本没用新版前端跑过"完全同形 —— 浏览器那几个场景需要重编译才
    算数（前端是 //go:embed 进二进制的），"编译没发生"等于整段白跑。
    """
    found = shutil.which("go")
    if found:
        return found

    exe = "go.exe" if os.name == "nt" else "go"
    home = os.path.expanduser("~")

    # 托管式 SDK：~/sdk/go1.26.2/bin/go
    sdk_root = os.path.join(home, "sdk")
    if os.path.isdir(sdk_root):
        for name in sorted(os.listdir(sdk_root), reverse=True):
            cand = os.path.join(sdk_root, name, "bin", exe)
            if os.path.isfile(cand):
                return cand

    for cand in (
        os.path.join(home, "go", "bin", exe),          # 官方安装包默认位置
        os.path.join("C:/Program Files/Go/bin", exe),
        "/usr/local/go/bin/go",
        "/usr/bin/go",
    ):
        if os.path.isfile(cand):
            return cand
    return None


def read_version() -> str:
    """从仓库根的 VERSION 读版本号。

    这里原来写死成字面量，于是每升一次版本都得记得手改 —— 而"忘了改"不会有
    任何报错，只是浏览器跑着一个版本号对不上的二进制。VERSION 本来就是版本号
    的唯一来源，直接跟它走，顺手消掉一类"迟早对不上"的维护面。
    """
    try:
        with open(os.path.join(ROOT, "VERSION"), "rb") as f:
            v = f.read().decode("utf-8").strip()
    except OSError:
        return "0.0.0"
    return v or "0.0.0"


def rebuild(exe: str) -> bool:
    """改了 Go 代码或前端资源就得重新编译 —— 否则浏览器验的还是旧二进制。"""
    go = find_go()
    if not go:
        print("!! 找不到 go，无法重新编译。")
        print("   注意：前端资源是 //go:embed 进二进制的，不重编译的话"
              "浏览器跑的还是旧代码 —— 那看起来和「守卫没抓到」一模一样。")
        return False
    env = dict(os.environ, PYTHONIOENCODING="utf-8")
    proc = subprocess.run(
        [go, "build", "-trimpath", "-ldflags", f"-s -w -X main.version={read_version()}",
         "-o", exe, "./cmd/server"],
        capture_output=True, text=True, cwd=ROOT, env=env, timeout=600,
    )
    if proc.returncode != 0:
        print(proc.stdout, proc.stderr)
    return proc.returncode == 0


def failing_names(out: str) -> list[str]:
    names = []
    in_list = False
    out = re.sub(r"\x1b\[[0-9;]*m", "", out)
    for line in out.splitlines():
        if "失败明细" in line:
            in_list = True
            continue
        if in_list:
            s = line.strip()
            if s.startswith("- "):
                names.append(s[2:].split(" ::")[0].strip())
            elif s:
                in_list = False
    return names


def run_browser_scenarios(exe: str, only: set[str]) -> int:
    scenarios = [s for s in BROWSER_SCENARIOS if not only or s[0] in only]

    # 基线只跑**这次用得到**的那几段：跑满四段要好几分钟，而"太慢"的结果
    # 一定是"有人省掉这一步"。
    base_sections = ",".join(sorted({s[5] for s in scenarios})) or ALL_SECTIONS
    base_fails, out = run_browser(exe, base_sections)
    clean = re.sub(r"\x1b\[[0-9;]*m", "", out)
    if "全部通过" not in clean and base_fails == 0:
        print("!! 浏览器基线没跑起来（既没报全部通过，也没有 FAIL 行）。")
        print(clean[-800:])
        return 2
    print(f"浏览器基线：段 {base_sections}，FAIL {base_fails} 条\n")

    ws = Workspace([s[1] for s in scenarios])
    ws.install_healing()
    ws.save()

    bad: list[str] = []
    for name, rel, old, new, need_build, sections in scenarios:
        path = os.path.join(ROOT, rel)
        with open(path, "rb") as f:
            original = f.read()
        text = original.decode("utf-8")
        if old not in text:
            print(f"  SKIP  {name}\n        补丁未命中（原文对不上）：{rel}")
            bad.append(f"{name}（补丁未命中）")
            continue

        try:
            with open(path, "wb") as f:
                f.write(text.replace(old, new, 1).encode("utf-8"))
            if need_build and not rebuild(exe):
                print(f"  SKIP  {name}\n        重新编译失败")
                bad.append(f"{name}（编译失败）")
                continue
            # 只跑相关的那一段：反向验证太慢的结果一定是"有人省掉它"。
            n_fail, bout = run_browser(exe, sections)
            bclean = re.sub(r"\x1b\[[0-9;]*m", "", bout)
            fails = [ln.strip() for ln in bclean.splitlines() if ln.strip().startswith("FAIL")]
            print(f"  {'OK   ' if n_fail > base_fails else 'MISS '} {name}")
            print(f"        FAIL {base_fails} → {n_fail}（段：{sections}）")
            for f_ in fails[:3]:
                print(f"        · {f_}")
            if n_fail <= base_fails:
                bad.append(name)
        finally:
            ws.restore()
            if need_build:
                rebuild(exe)

    print()
    if bad:
        print(f"以下场景没能让浏览器守卫 FAIL（{len(bad)} 条）：")
        for b in bad:
            print(f"  - {b}")
        return 1
    print(f"全部 {len(scenarios)} 个真视口场景都让守卫真的 FAIL 了。")
    return 0


# 行为型反向验证要跑的单测。base pattern 收窄是刻意的：
# internal/fnos 全量跑一轮要四十多秒，五条场景就是四分钟 ——
# 而「反向验证太慢」的必然结果就是「有人省掉它」。
# 这里只跑缩放 / 锐化 / 图标产物同步那几组，一轮一秒出头。
GO_TEST_RUN = "Resize|Sharpen|IconArtifacts"


def run_go_test() -> tuple[int, int, str]:
    """跑内部单测，返回 (FAIL 条数, 退出码, 原始输出)。"""
    go = find_go()
    if not go:
        return -1, -1, "找不到 go"
    env = dict(os.environ, PYTHONIOENCODING="utf-8")
    proc = subprocess.run(
        [go, "test", "./internal/fnos/", "-run", GO_TEST_RUN],
        capture_output=True, text=True, cwd=ROOT, env=env, timeout=900,
    )
    out = proc.stdout + proc.stderr
    fails = sum(1 for ln in out.splitlines() if ln.strip().startswith("--- FAIL:"))
    return fails, proc.returncode, out


def run_test_scenarios(only: set[str]) -> int:
    scenarios = [s for s in TEST_SCENARIOS if not only or s[0] in only]

    base_fails, rc, out = run_go_test()
    if base_fails == 0 and rc != 0:
        print("!! go test 基线跑不起来（不是断言失败，而是编译 / 环境问题）。")
        print(out[-1200:])
        return 2
    if base_fails > 0:
        print(f"!! 基线本身就有 {base_fails} 条 FAIL，先把它们修掉。")
        print(out[-1200:])
        return 2
    print("单测基线：internal/fnos 全绿（FAIL 0）\n")

    go = find_go()
    # 产物也要一起纳入备份：最后一条场景会真的重写它们。
    ws = Workspace([s[1] for s in scenarios] + ICON_ARTIFACTS)
    ws.install_healing()
    ws.save()

    bad: list[str] = []
    for name, rel, old, new, regen in scenarios:
        path = os.path.join(ROOT, rel)
        with open(path, "rb") as f:
            original = f.read()
        text = original.decode("utf-8")
        if old not in text:
            print(f"  SKIP  {name}\n        补丁未命中（原文对不上）：{rel}")
            bad.append(f"{name}（补丁未命中）")
            continue

        try:
            # 一律二进制回写：本项目所有文件都是 LF。
            with open(path, "wb") as f:
                f.write(text.replace(old, new, 1).encode("utf-8"))
            if regen:
                proc = subprocess.run(
                    [go, "run", "./cmd/iconforge", "-src", "QLink2Desktop.png", "-app", "fnos-app"],
                    capture_output=True, text=True, cwd=ROOT, timeout=600,
                )
                if proc.returncode != 0:
                    print(f"  SKIP  {name}\n        重新生成图标产物失败")
                    print(proc.stdout, proc.stderr)
                    bad.append(f"{name}（重生成失败）")
                    continue
            n_fail, rc, out = run_go_test()
            fails = [ln.strip() for ln in out.splitlines() if ln.strip().startswith("--- FAIL:")]
            mark = "OK   " if n_fail > 0 else "MISS "
            print(f"  {mark} {name}\n        FAIL 0 → {n_fail}")
            for f_ in fails[:3]:
                print(f"        · {f_}")
            if n_fail == 0:
                bad.append(name)
        finally:
            ws.restore()

    print()
    if bad:
        print(f"以下场景没能让单测 FAIL（{len(bad)} 条）—— 守卫可能是形同虚设的：")
        for b in bad:
            print(f"  - {b}")
        return 1
    print(f"全部 {len(scenarios)} 个行为型场景都让单测真的 FAIL 了。")
    return 0


def main() -> int:
    args = [a for a in sys.argv[1:]]
    browser_mode = "--browser" in args
    gotest_mode = "--gotest" in args
    only = {a for a in args if not a.startswith("--")}

    if gotest_mode:
        return run_test_scenarios(only)

    exe = pick_exe()
    if not exe:
        print("!! 找不到可执行文件，先 `make build`。")
        print("   注意：找不到产物时守卫会直接退出、一条都不跑 —— 那看起来和全绿一模一样。")
        return 2
    print(f"产物：{exe}\n")

    if browser_mode:
        return run_browser_scenarios(exe, only)

    scenarios = [s for s in SCENARIOS if not only or s[0] in only]
    ws = Workspace([s[1] for s in scenarios])
    ws.install_healing()
    ws.save()

    total, failed_total, out = run_static(exe)
    if total <= 0:
        print("!! 基线都没跑起来，先修好再谈反向验证。")
        print("--- 原始输出（末尾 800 字） ---")
        print(out[-800:])
        print("--- 结束 ---")
        return 2
    print(f"基线：通过 {total} 项，失败 {failed_total} 项\n")

    bad: list[str] = []
    for name, rel, old, new in scenarios:
        path = os.path.join(ROOT, rel)
        with open(path, "rb") as f:
            original = f.read()
        text = original.decode("utf-8")
        if old not in text:
            print(f"  SKIP  {name}\n        补丁未命中（原文对不上）：{rel}")
            bad.append(f"{name}（补丁未命中）")
            continue

        try:
            # 一律二进制回写：本项目所有文件都是 LF，文本模式会把 \n 变 \r\n，
            # shell 脚本会变成 bad interpreter、Go 文件会被 gofmt 判为未格式化。
            with open(path, "wb") as f:
                f.write(text.replace(old, new, 1).encode("utf-8"))
            n_pass, n_fail, out = run_static(exe)
            names = failing_names(out)
            mark = "OK   " if n_fail > failed_total else "MISS "
            print(f"  {mark} {name}\n        FAIL {failed_total} → {n_fail}（通过 {n_pass}）")
            for n in names[:3]:
                print(f"        · {n}")
            if n_fail <= failed_total:
                bad.append(name)
        finally:
            ws.restore()

    print()
    if bad:
        print(f"以下场景没能让守卫 FAIL（{len(bad)} 条）—— 守卫可能是形同虚设的：")
        for b in bad:
            print(f"  - {b}")
        return 1
    print(f"全部 {len(scenarios)} 个场景都让守卫真的 FAIL 了。")
    return 0


if __name__ == "__main__":
    sys.exit(main())
