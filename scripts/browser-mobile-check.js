/* 窄屏 / 宽屏下的导航可用性验证（真浏览器）。
 *
 * 守的是一个真实故障：用户报「手机模式下左侧的侧边栏没有了，不能切换功能区」。
 * 根因是窄屏规则 `.nav-item span:not(.nav-badge){display:none}` 本意"只留图标"，
 * 但图标本身也是 span —— 图标与文字一起被藏掉，七个功能项全变成
 * 看不见、只有 37×14px 的空白按钮。
 *
 * 为什么这层验证必不可少：
 *   - 图标 span 一直在 DOM 里，`hidden` 属性从未被设置 —— "元素存在"类断言看不见它；
 *   - 用 JS/Playwright 的 .click() 能强制点中任何元素，**包括看不见、点不准的**，
 *     所以"点一下能切换"也会假通过。必须量尺寸、必须看计算样式。
 *
 * 用法：node browser-mobile-check.js <url>
 * 输出：逐条 PASS/FAIL，末尾一行汇总；有失败时退出码非 0。
 */
const { chromium } = require('playwright-core');

const EDGE_CANDIDATES = [
  'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe',
  'C:/Program Files/Microsoft/Edge/Application/msedge.exe',
  '/usr/bin/microsoft-edge',
  '/usr/bin/google-chrome',
  '/usr/bin/chromium',
];
const fs = require('fs');

// 断点与 style.css 的 @media (max-width: 860px) 必须一致。
const NARROW_MAX = 860;
// 手机竖屏最小常见宽度 → iPhone 14/15 → 大屏手机 → 窄窗口（宽屏侧边栏形态）。
const SIZES = [
  [360, 640],
  [390, 844],
  [430, 932],
  [1280, 800],
];

const url = process.argv[2];
if (!url) {
  console.error('用法: node browser-mobile-check.js <url>');
  process.exit(2);
}

function findBrowser() {
  for (const p of EDGE_CANDIDATES) {
    try { if (fs.existsSync(p)) return p; } catch (_) { /* ignore */ }
  }
  return null;
}

let pass = 0;
let fail = 0;
const check = (ok, label, detail = '') => {
  if (ok) { pass += 1; console.log(`  PASS  ${label}`); } else { fail += 1; console.log(`  FAIL  ${label}${detail ? '    ' + detail : ''}`); }
};

async function inspect(page) {
  return page.evaluate(() => {
    const vis = (el) => !!el && el.checkVisibility({ checkOpacity: true, checkVisibilityCSS: true });
    const sidebar = document.querySelector('.sidebar');
    const content = document.querySelector('.content');
    const items = Array.from(document.querySelectorAll('.nav .nav-item'));
    const sb = sidebar ? sidebar.getBoundingClientRect() : null;

    return {
      navCount: items.length,
      // 逐项：图标看得见吗、图标里真有 svg 吗、有没有可见文字、按钮多大
      items: items.map((it) => {
        const iconEl = it.querySelector('.nav-icon');
        const svg = it.querySelector('.nav-icon svg');
        const label = Array.from(it.querySelectorAll('span'))
          .filter((s) => !s.classList.contains('nav-icon') && !s.classList.contains('nav-badge'))
          .filter(vis)
          .map((s) => s.textContent.trim())
          .filter(Boolean)
          .join('');
        const r = it.getBoundingClientRect();
        // 图标的**实际渲染尺寸**：只断言"图标元素存在/可见"抓不住
        // "图标被改小"——元素一直都在，只是小到看不清。必须量它的 svg 盒子。
        const ir = svg ? svg.getBoundingClientRect() : null;
        return {
          label,
          iconVisible: vis(iconEl),
          svgVisible: vis(svg),
          // getBoundingClientRect 拿到的是布局盒，被 display:none 时是 0
          w: Math.round(r.width),
          h: Math.round(r.height),
          left: Math.round(r.left),
          right: Math.round(r.right),
          iconW: ir ? Math.round(ir.width) : 0,
          iconH: ir ? Math.round(ir.height) : 0,
        };
      }),
      sidebar: sb ? { top: Math.round(sb.top), bottom: Math.round(sb.bottom), left: Math.round(sb.left), right: Math.round(sb.right), height: Math.round(sb.height) } : null,
      // 样式表有没有真的加载：网关前缀不对时 style.css 会 404，
      // 而 404 之后页面仍然"能渲染"，只是完全没样式 —— 那才是最难查的形态。
      sidebarPosition: sidebar ? getComputedStyle(sidebar).position : '',
      navDirection: (() => {
        const n = document.querySelector('.nav');
        return n ? getComputedStyle(n).flexDirection : '';
      })(),
      contentPadBottom: content ? getComputedStyle(content).paddingBottom : '',
      pipCount: document.querySelectorAll('.sidebar-foot .pip').length,
      pipVisible: vis(document.querySelector('.sidebar-foot .pip')),
      // 徽标：图标右上角那个计数。这里同时收它的文字与位置。
      badges: Array.from(document.querySelectorAll('.nav .nav-item')).map((it) => {
        const b = it.querySelector('.nav-badge');
        return {
          label: (it.querySelector('.nav-label') || {}).textContent || '',
          text: b ? b.textContent.trim() : null,
          right: b ? Math.round(b.getBoundingClientRect().right) : 0,
        };
      }),
      vw: window.innerWidth,
      vh: window.innerHeight,
      navScrollW: (() => { const n = document.querySelector('.nav'); return n ? { scroll: n.scrollWidth, client: n.clientWidth } : null; })(),
    };
  });
}

(async () => {
  const exe = findBrowser();
  if (!exe) {
    console.error('!! 找不到 Chromium 系浏览器（Edge / Chrome / Chromium）');
    process.exit(2);
  }

  const browser = await chromium.launch({ executablePath: exe, headless: true });
  // 脚本异常与资源告警分开收：前者一定是 bug，后者在本项目里有已知噪声。
  const pageErrors = [];
  const resErrors = [];

  try {
    for (const [W, H] of SIZES) {
      const narrow = W <= NARROW_MAX;
      const ctx = await browser.newContext({
        viewport: { width: W, height: H },
        deviceScaleFactor: 1,
        isMobile: narrow,
        hasTouch: narrow,
      });
      const page = await ctx.newPage();
      page.on('console', (m) => { if (m.type() === 'error') resErrors.push(`[${W}x${H}] ${m.text()}`); });
      page.on('pageerror', (e) => pageErrors.push(`[${W}x${H}] ${e}`));

      await page.goto(url, { waitUntil: 'networkidle', timeout: 30000 });
      await page.waitForSelector('#app:not([hidden])', { timeout: 20000 });
      await page.waitForTimeout(400);
      const s = await inspect(page);
      const tag = (t) => `${W}×${H} ${t}`;

      // 先确认导航真的渲染出来了 —— 否则下面那些"逐项都看得见"会因为
      // 零个元素而空过（0 项里没有不合格的，断言就"通过"了）。
      check(s.navCount >= 6, tag('导航项渲染齐全'), `只有 ${s.navCount} 项`);

      // ---- 通用：图标与名字都得看得见 ----
      const badIcon = s.items.filter((i) => !i.iconVisible || !i.svgVisible).map((i) => i.label || '?');
      check(badIcon.length === 0, tag('每个功能项的图标都看得见'), badIcon.join(' | '));

      const noName = s.items.filter((i) => !i.label);
      check(noName.length === 0, tag('每个功能项都看得出是什么（有可见文字）'),
        `${noName.length}/${s.items.length} 项没文字`);

      // ---- 通用：相邻项不重叠 ----
      // 判据必须跟着排列方向走：窄屏是横向一排（比左右），宽屏是左侧竖列（比上下）。
      // 只比左右的话，竖列里每一项的 left/right 本来就一样，会假报重叠。
      const rowWise = s.navDirection === 'row';
      let overlap = '';
      for (let i = 1; i < s.items.length; i++) {
        const a = s.items[i - 1];
        const b = s.items[i];
        if (rowWise ? b.left < a.right - 1 : b.top < a.bottom - 1) {
          overlap = `第 ${i - 1}/${i} 项重叠（${rowWise ? '横向' : '纵向'}排列）`;
          break;
        }
      }
      check(!overlap, tag('功能项之间不重叠'), overlap);

      // ---- 通用：样式表确实生效 ----
      // 这条替代"控制台一条错误都不许有"：入口不带尾斜杠时，Chromium 预扫描器
      // 会抢跑预取几个相对资源、拿到 404（已知且无法从 HTML 侧消除，见 MEMORY）。
      // 与其盯着噪声，不如直接问"样式到底应用上了没有"。
      const styleApplied = narrow ? s.sidebarPosition === 'fixed' : s.sidebarPosition !== 'fixed';
      check(styleApplied, tag('样式表真的生效（拿计算样式作证）'),
        `position=${s.sidebarPosition} flex-direction=${s.navDirection}`);

      if (narrow) {
        // ---- 窄屏：导航必须是底部固定条 ----
        check(s.sidebar !== null && s.sidebar.bottom >= H - 2 && s.sidebar.top > H / 2,
          tag('导航固定在底部'),
          JSON.stringify(s.sidebar));

        // 44px 是触摸目标的下限；手指点不准就等于不能切换
        const tiny = s.items.filter((i) => i.h < 44 || i.w < 40);
        check(tiny.length === 0, tag('触摸目标够大（≥44×40）'),
          tiny.map((i) => `${i.label}=${i.w}x${i.h}`).join(' '));

        // ---- 窄屏：底部条的「尺寸」也要合格，不只是「能用」 ----
        // 用户第二次报的就是「底部稍微调整一下，太难看了」。
        // 而"图标看得见 + 点得动"这两条断言，在图标被改成 14px、条高被改成 30px 时
        // **照样全绿** —— 反向验证时它们一条都没 FAIL。
        // 所以这里直接量渲染盒：图标多大、整条多高、留白够不够。
        const smallIcon = s.items.filter((i) => i.iconW < 20 || i.iconH < 20);
        check(smallIcon.length === 0, tag('导航图标渲染尺寸够大（≥20px）'),
          smallIcon.map((i) => `${i.label}=${i.iconW}x${i.iconH}`).join(' '));

        // --navbar-h 是 58px（≤400px 屏为 56px）。低于 52px 说明有人把条改矮了
        // 却没同步 .content 的留白，或者干脆把变量删了退回写死的小数字。
        check(s.sidebar !== null && s.sidebar.height >= 52, tag('底部条本身够高（≥52px）'),
          `height=${s.sidebar ? s.sidebar.height : 'null'}`);

        // 底部条不占文档流，内容区必须自己留出高度
        const pad = Number.parseInt(s.contentPadBottom, 10) || 0;
        check(pad >= 60, tag('内容区给底部条留了空间'), `padding-bottom=${s.contentPadBottom}`);

        // 两处尺寸必须**相互匹配**：--navbar-h 同时喂给 .sidebar 的 min-height 和
        // .content 的 padding-bottom。任一侧写死成别的数字，就会出现
        // "卡片被压在栏下面" 或 "底部空一大截"，而上面两条各自单独看都是绿的。
        check(pad >= (s.sidebar ? s.sidebar.height : 0), tag('内容区留白 ≥ 底部条实际高度'),
          `padding-bottom=${pad} < 条高=${s.sidebar ? s.sidebar.height : 'null'}`);

        // 真的点一下（用坐标点击，不用 .click() —— 后者能点中看不见的元素）
        const target = s.items.find((i) => i.label && i.label !== '桌面应用') || s.items[1];
        const before = await page.locator('#page-title').textContent();
        if (target) {
          await page.mouse.click((target.left + target.right) / 2, s.sidebar.top + (s.sidebar.bottom - s.sidebar.top) / 2);
          await page.waitForTimeout(500);
        }
        const after = await page.locator('#page-title').textContent();
        check(!!after && after !== before, tag('按真实坐标点击能切换功能区'),
          `「${(before || '').trim()}」→「${(after || '').trim()}」，目标=「${target ? target.label : '?'}」`);
      } else {
        // ---- 宽屏：侧边栏在左侧，且没有被改成底部条 ----
        check(s.sidebar !== null && s.sidebar.left === 0 && s.sidebar.height >= H - 2,
          tag('宽屏仍是左侧栏'), JSON.stringify(s.sidebar));
      }

      // ---- 通用：状态点可见 ----
      check(s.pipCount > 0 && s.pipVisible, tag('状态指示点可见'),
        `count=${s.pipCount} visible=${s.pipVisible}`);

      // ---- 通用：徽标一进页面就该有，不能等用户点进那个页面 ----
      // 用户报的："侧边栏标记数量的这个部分，每次都得点击一下才能显示出来那个数值"。
      // 根因是徽标数字取自各视图自己 mount() 时才拉的列表 —— 没进过那个页面
      // 就没有数据。数据由 browser-e2e.py 在下游统一造好（空环境下 0 项不显示
      // 徽标是刻意的，所以不造数据这条断言会因为集合为空而空过）。
      const withBadge = s.badges.filter((b) => b.text !== null);
      check(withBadge.length >= 3, tag('徽标一进页面就在（不需要先点进那个页面）'),
        `只有 ${withBadge.length} 个徽标：` + s.badges.map((b) => `${b.label}=${b.text}`).join(' '));

      // 徽标不能跑到屏幕外：它是靠 left: calc(50% + Npx) 定位的，
      // 超窄屏上若算错，数字会被裁掉一半。
      const badgeOut = withBadge.filter((b) => b.right > s.vw);
      check(badgeOut.length === 0, tag('徽标都在屏幕内'),
        badgeOut.map((b) => `${b.label} 右缘=${b.right}/${s.vw}`).join(' '));

      // 计数为 0 时不该显示一枚写着 0 的角标
      const zeroBadge = s.badges.filter((b) => b.text === '0');
      check(zeroBadge.length === 0, tag('计数为 0 时不显示徽标'), zeroBadge.map((b) => b.label).join(' '));

      // 截图落到临时目录：留在仓库根会污染工作区（且 .gitignore 没覆盖它）
      await page.screenshot({
        path: require('path').join(require('os').tmpdir(), `qlink2d-mobile-${W}x${H}.png`),
      }).catch(() => {});
      await ctx.close();
    }
  } catch (e) {
    fail += 1;
    console.log(`  FAIL  脚本异常 ${e}`);
  } finally {
    await browser.close();
  }

  check(pageErrors.length === 0, '没有未捕获的脚本异常', pageErrors.slice(0, 3).join(' | '));
  if (resErrors.length) {
    // 不判失败：入口不带尾斜杠时，Chromium 预扫描器会抢跑预取几个相对资源并拿到
    // 404 —— 已知、且无法从 HTML 侧消除。"样式表真的生效"那条才是硬断言。
    console.log(`  NOTE  资源加载告警 ${resErrors.length} 条（无尾斜杠入口下的预扫描噪声，不影响应用）`);
    for (const t of resErrors.slice(0, 2)) console.log(`        ${t}`);
  }

  console.log(`\n通过 ${pass} 项，失败 ${fail} 项`);
  process.exit(fail === 0 ? 0 : 1);
})();
