/* 「自动」图标那一档到底有没有反应（真浏览器）。
 *
 * 守的是一个用户报过两次的故障。原话：
 *   「我弄了个百度的网站快捷方式，这个图标也没有自动搜索百度的图标啊，
 *     这个过程有个什么提示吗或者能让人知道搜索中或者搜索状态」
 *   「不光是说弄快捷方式，其他的本机端口和端口映射也是有这样的问题……
 *     而且过程也没有提示或者其他的示意」
 * 截图里编辑器「图标 → 自动」那一格永远是内置默认图，旁边一个字都没有。
 *
 * 为什么必须用真浏览器验：
 *   - 「正在查找…」是个**只在请求进行中存在的瞬时状态**。静态守卫能查到
 *     代码里有那行字，查不到它有没有真的被画到屏幕上、是不是一帧就闪过去、
 *     样式有没有把它压成不可见；
 *   - 「有提示」的对立面是"建了 DOM 节点却没 append"—— 那在静态文本里
 *     看不出来，在浏览器里一眼就能看出来。
 *
 * 为了让"进行中"这一态稳定可观测，脚本会**故意把解析请求拖慢**：
 * 靠时序碰运气的话，本机连接被拒是毫秒级返回的，那一态根本抓不到。
 *
 * 用法：node browser-icon-check.js <url>
 * 输出：逐条 PASS/FAIL，末尾一行汇总；有失败时退出码非 0。
 */
const { chromium } = require('playwright-core');
const fs = require('fs');

const EDGE_CANDIDATES = [
  'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe',
  'C:/Program Files/Microsoft/Edge/Application/msedge.exe',
  '/usr/bin/microsoft-edge',
  '/usr/bin/google-chrome',
  '/usr/bin/chromium',
];

// 人为拖慢解析请求的时长：要明显长于一次 DOM 往返，短于脚本的耐心。
const RESOLVE_DELAY_MS = 1200;

const url = process.argv[2];
if (!url) {
  console.error('用法: node browser-icon-check.js <url>');
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
  if (ok) { pass += 1; console.log(`  PASS  ${label}`); }
  else { fail += 1; console.log(`  FAIL  ${label}${detail ? '    ' + detail : ''}`); }
};

// 状态行快照。一律用 checkVisibility 判可见性、getBoundingClientRect 判尺寸 ——
// "属性已设置"替代不了"用户真的看得见"（本项目在这上面栽过一次）。
const READ_STATUS = () => {
  const box = document.querySelector('.icon-status');
  const tag = box ? box.querySelector('.tag') : null;
  const img = document.querySelector('.card-body .row > img');
  const btn = document.querySelector('.icon-status') ? box.parentElement : null;
  return {
    exists: !!box,
    visible: !!box && box.checkVisibility({ checkOpacity: true, checkVisibilityCSS: true }),
    text: box ? box.textContent.trim() : '',
    tagText: tag ? tag.textContent.trim() : '',
    tagClass: tag ? tag.className : '',
    spinner: !!box && !!box.querySelector('.spinner-sm'),
    spinnerVisible: !!box && !!box.querySelector('.spinner-sm')
      && box.querySelector('.spinner-sm').checkVisibility({ checkOpacity: true, checkVisibilityCSS: true }),
    // 状态行的高度：被 CSS 压成 0 高时 textContent 照样有内容，但用户什么都看不到
    height: box ? Math.round(box.getBoundingClientRect().height) : 0,
    retryButtons: box && btn
      ? Array.from(btn.querySelectorAll('button')).map((b) => b.textContent.trim())
      : [],
    previewSrc: img ? (img.getAttribute('src') || '') : '',
  };
};

(async () => {
  const exe = findBrowser();
  if (!exe) {
    console.error('!! 找不到 Chromium 系浏览器（Edge / Chrome / Chromium）');
    process.exit(2);
  }

  const browser = await chromium.launch({ executablePath: exe, headless: true });
  const pageErrors = [];
  let resolveCalls = 0;

  try {
    const ctx = await browser.newContext({ viewport: { width: 1280, height: 900 }, deviceScaleFactor: 1 });
    const page = await ctx.newPage();
    page.on('pageerror', (e) => pageErrors.push(String(e)));

    // 拖慢解析：让"进行中"这一态稳定可观测。
    let holdFirst = true;
    // 顺便记下解析响应的状态码：这一档最容易出的错不是"没抓到"，
    // 而是接口把请求打了回来（比如严格解码撞上前端多传的字段），
    // 那时界面只会显示"查找失败"，看不出是接口 4xx 还是真的没图标。
    const resolveStatuses = [];
    await page.route('**/api/icons/resolve', async (route) => {
      resolveCalls += 1;
      if (holdFirst) {
        holdFirst = false;
        await new Promise((r) => setTimeout(r, RESOLVE_DELAY_MS));
      }
      const resp = await route.fetch();
      resolveStatuses.push(resp.status());
      await route.fulfill({ response: resp });
    });

    await page.goto(url, { waitUntil: 'networkidle', timeout: 30000 });
    await page.waitForSelector('#app:not([hidden])', { timeout: 20000 });
    await page.waitForTimeout(300);

    // 打开新建弹窗。空环境下按钮文案是「新建第一个桌面应用」，
    // 有数据时是「新建」—— 按子串找，两种都命中。
    const newBtn = page.locator('button', { hasText: '新建' }).first();
    await newBtn.click({ timeout: 10000 });
    await page.waitForSelector('.modal', { timeout: 10000 });

    // 先确认图标卡片真的渲染出来了 —— 否则下面全是"零个元素里没有不合格的"式空过。
    const iconHeading = await page.locator('.card-head h2', { hasText: '图标' }).count();
    check(iconHeading > 0, '编辑器里图标卡片渲染出来了', `找到 ${iconHeading} 个`);
    check(resolveCalls > 0, '一进「自动」档就发起了图标解析请求（不是干等着）',
      `实际发出 ${resolveCalls} 次`);

    // ---- 进行中：用户要的「能让人知道搜索中」 ----
    const loading = await page.evaluate(READ_STATUS);
    check(loading.exists && loading.visible, '「自动」档有状态行，且真的可见',
      JSON.stringify(loading));
    check(loading.spinner && loading.spinnerVisible, '查找期间有转圈指示',
      `spinner=${loading.spinner} visible=${loading.spinnerVisible}`);
    check(loading.text.includes('正在查找'),
      '查找期间显示「正在查找图标…」（用户明确要的那句话）',
      `实际显示「${loading.text}」`);
    check(loading.height >= 16, '状态行有实际高度（没被样式压成 0）', `height=${loading.height}`);
    check(loading.tagClass.includes('busy'),
      '进行中用的是专门的进行中样式', `class=${loading.tagClass}`);

    // ---- 终态：必须收敛成一个说得清的结果 ----
    await page.waitForFunction(() => {
      const box = document.querySelector('.icon-status');
      return !!box && !box.textContent.includes('正在查找');
    }, { timeout: 40000 });

    const done = await page.evaluate(READ_STATUS);
    const terminal = ['已匹配到官方图标库', '已从站点抓取到图标', '未找到图标', '查找失败',
      '使用你上传的图片', '使用文字图标', '已从你填写的网址下载图标', '使用内联的图标数据'];
    check(done.visible && terminal.some((t) => done.text.includes(t)),
      '查找结束后给出明确结论（不是停在转圈，也不是一片空白）',
      `实际显示「${done.text}」`);
    check(done.text.length > 4, '结论里带了说明文字，而不只是一个标签',
      `「${done.text}」`);
    // 没找到时必须是**警告色**而不是成功色：把"没找到"画成成功，比不提示更糟。
    check(!done.text.includes('未找到') || done.tagClass.includes('warn'),
      '「没找到」用警告色而不是成功色', `class=${done.tagClass}`);
    check(resolveStatuses.every((s) => s === 200),
      '解析接口每次都返回 200（不是被自己的参数校验挡在门外）',
      `状态码=${JSON.stringify(resolveStatuses)}`);
    check(done.previewSrc.startsWith('data:image/png;base64,'),
      '状态行旁边给出了可直接显示的预览图（列表/桌面用同一枚）',
      `src=${done.previewSrc.slice(0, 40)}`);

    // ---- 重新查找：改地址 / 换形态要能重来 ----
    const before = resolveCalls;
    const retry = page.locator('button', { hasText: '重新查找' }).first();
    check(await retry.count() > 0, '提供了「重新查找」按钮');
    if (await retry.count() > 0) {
      await retry.click();
      await page.waitForTimeout(600);
      check(resolveCalls > before, '点「重新查找」会真的再解析一次',
        `${before} → ${resolveCalls}`);
    }

    // ---- 换地址要自动重查（否则一直挂着旧地址的结果） ----
    const before2 = resolveCalls;
    const portInput = page.locator('.modal input[type="number"]').first();
    if (await portInput.count() > 0) {
      await portInput.fill('18080');
      // 输入侧有 700ms 防抖
      await page.waitForTimeout(1600);
      check(resolveCalls > before2, '改了端口会自动重新解析（不是只在打开时查一次）',
        `${before2} → ${resolveCalls}`);
    } else {
      check(false, '改了端口会自动重新解析（不是只在打开时查一次）', '找不到端口输入框');
    }

    await ctx.close();
  } catch (e) {
    fail += 1;
    console.log(`  FAIL  脚本异常 ${e}`);
  } finally {
    await browser.close();
  }

  check(pageErrors.length === 0, '没有未捕获的脚本异常', pageErrors.slice(0, 3).join(' | '));

  console.log(`\n通过 ${pass} 项，失败 ${fail} 项`);
  process.exit(fail === 0 ? 0 : 1);
})();
