/* 浏览器级验证：在真实 Chromium（Edge）里加载页面，检查启动期自诊断。
 *
 * 为什么要用浏览器而不是只看字符串：看门狗的价值全在"运行时行为"——
 * 模块 404 时它到底有没有把版本号与失败资源渲染到页面上。
 * 这也正是真机上反复出现的那类故障（app.js 加载失败 → 页面只会一直转圈）。
 *
 * 用法：node browser-boot-check.js <url> [等待毫秒]
 * 输出：一行 JSON，便于脚本断言。
 */
const { chromium } = require('playwright-core');

const EDGE = 'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe';

(async () => {
  const url = process.argv[2];
  const waitMs = Number(process.argv[3] || 15000);
  if (!url) {
    console.error('用法: node browser-boot-check.js <url> [等待毫秒]');
    process.exit(2);
  }

  const browser = await chromium.launch({ executablePath: EDGE, headless: true });
  const page = await browser.newPage();

  // 记录所有 4xx/5xx，用来和页面自己报出来的清单对照。
  const httpErrors = [];
  page.on('response', (r) => {
    if (r.status() >= 400) httpErrors.push(`${r.status()} ${r.url()}`);
  });
  const consoleErrors = [];
  page.on('console', (m) => {
    if (m.type() === 'error') consoleErrors.push(m.text());
  });

  await page.goto(url, { waitUntil: 'domcontentloaded' });

  // 看门狗是 12s 触发，这里给足余量。
  await page.waitForTimeout(waitMs);

  const snapshot = await page.evaluate(() => {
    const boot = document.getElementById('boot');
    const app = document.getElementById('app');
    const detail = document.getElementById('boot-detail');
    const retry = document.getElementById('boot-retry');
    const version = document.getElementById('boot-version');
    const spinner = document.querySelector('.boot-spinner');

    // 判定"是否真的看得见"必须看**计算样式**，不能只看 .hidden 属性。
    // 原因：[hidden] 的 display:none 来自浏览器默认样式表，而作者样式表里
    // 任何一条设置 display 的规则（.boot{display:flex} / .btn{display:inline-flex}）
    // 都会把它压过去 —— 此时 el.hidden=true 但元素照样显示。
    // 早期版本正是栽在这里：只看 .hidden 属性，把"遮罩永远盖住应用"判成了正常。
    // 注意：不能只看元素自己的 display。父链上有 display:none 时，
    // 子元素的 getComputedStyle().display 仍然报它自己的值（如 'block'），
    // 会得出"还看得见"的错误结论 —— 启动页里的转圈就属于这种情况。
    // checkVisibility() 会连带把祖先链算进去，是这里唯一正确的判据。
    const seen = (el) => {
      if (!el) return false;
      if (typeof el.checkVisibility === 'function') {
        return el.checkVisibility({ checkOpacity: true, checkVisibilityCSS: true });
      }
      const cs = getComputedStyle(el);
      if (cs.display === 'none' || cs.visibility === 'hidden' || Number(cs.opacity) === 0) return false;
      // 兜底：offsetParent 为空说明自身或祖先不可见（position:fixed 例外）。
      if (el.offsetParent === null && cs.position !== 'fixed') return false;
      return true;
    };
    const display = (el) => (el ? getComputedStyle(el).display : null);

    return {
      finalUrl: window.location.pathname,
      // 属性态（排查用）
      shellHiddenAttr: !!app && app.hidden,
      bootHiddenAttr: !!boot && boot.hidden,
      // 真实可见性（断言用）
      shellVisible: seen(app),
      bootVisible: seen(boot),
      bootDisplay: display(boot),
      appDisplay: display(app),
      retryDisplay: display(retry),
      spinnerDisplay: display(spinner),
      hiddenCssWorks: (() => {
        // 探针：给一个 display 由作者样式控制的元素加上 hidden，
        // 若计算样式仍是可见的，说明页面缺少 [hidden]{display:none} 兜底规则。
        if (!boot) return null;
        const had = boot.hidden;
        boot.hidden = true;
        const stillSeen = seen(boot);
        boot.hidden = had;
        return !stillSeen;
      })(),
      versionText: version ? version.textContent.trim() : null,
      detailText: detail ? detail.textContent : null,
      detailVisible: seen(detail),
      retryVisible: seen(retry),
      spinnerHidden: !seen(spinner),
      diagFailed: (window.__qlinkDiag && window.__qlinkDiag.failed) || [],
      diagEntryFailed: !!(window.__qlinkDiag && window.__qlinkDiag.entryFailed),
    };
  });

  snapshot.httpErrors = httpErrors;
  snapshot.consoleErrors = consoleErrors.slice(0, 10);
  snapshot.url = url;
  console.log(JSON.stringify(snapshot, null, 2));

  await browser.close();
})().catch((e) => {
  console.error('FAILED: ' + e.message);
  process.exit(1);
});
