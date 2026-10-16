/* 图标卡片那两件事（真浏览器）。
 *
 * 一、「自动」档到底有没有反应。守的是一个用户报过两次的故障。原话：
 *   「我弄了个百度的网站快捷方式，这个图标也没有自动搜索百度的图标啊，
 *     这个过程有个什么提示吗或者能让人知道搜索中或者搜索状态」
 *   「不光是说弄快捷方式，其他的本机端口和端口映射也是有这样的问题……
 *     而且过程也没有提示或者其他的示意」
 * 截图里编辑器「图标 → 自动」那一格永远是内置默认图，旁边一个字都没有。
 *
 * 二、「图标库」档能不能从已上传的图标里挑一枚。守的是"点了没反应"：
 *   选中之后只把文件名写进数据、界面上什么都不变，用户根本判断不出
 *   到底选上了没有。
 *
 * 为什么必须用真浏览器验：
 *   - 「正在查找…」是个**只在请求进行中存在的瞬时状态**。静态守卫能查到
 *     代码里有那行字，查不到它有没有真的被画到屏幕上、是不是一帧就闪过去、
 *     样式有没有把它压成不可见；
 *   - 「有提示」的对立面是"建了 DOM 节点却没 append"—— 那在静态文本里
 *     看不出来，在浏览器里一眼就能看出来；
 *   - 选中态是 class 与 CSS 的合作产物：class 加对了但样式没声明，
 *     界面上同样是"没反应"。
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

// 「图标库」那一档的夹具。
//
// 本机跑的时候图标库多半是空的（icons/ 目录在 .build 下，随环境变），
// 拿真实数据去断言等于"零个磁贴里没有不合格的"——空过。
// 名字用 ASCII，好和各种 URL 编码形态对齐。
const LIBRARY_FIXTURE = [
  { name: 'miwifi.png', size: 276480, mod_time: '2026-09-30T10:00:00Z', url: 'icons/miwifi.png' },
  { name: 'router.png', size: 102400, mod_time: '2026-10-01T10:00:00Z', url: 'icons/router.png' },
  { name: 'nas-dashboard.png', size: 51200, mod_time: '2026-10-02T10:00:00Z', url: 'icons/nas-dashboard.png' },
];

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

// 「图标库」档的快照。
//
// 一律先定位到图标那张卡片再往下找：`.field > .hint`、`.card-body .row > img`
// 这类选择器在整份文档里有好几个命中，"取第一个"看起来能用，实际上取的
// 是文档里恰好排在前面的另一个 —— 于是断言测的根本不是这块界面。
const READ_LIBRARY = () => {
  // 「看起来是什么样」：只比这三项，够区分 selected 与未选中，
  // 又不会把字体、圆角这类无关差异卷进来。
  //
  // 必须定义在页内：page.evaluate 只序列化这一个函数，
  // 外层的 helper 在浏览器里根本不存在（ReferenceError）。
  const tileStyle = (el) => {
    const s = getComputedStyle(el);
    return {
      background: s.backgroundColor,
      borderColor: s.borderTopColor,
      shadow: s.boxShadow,
    };
  };
  const card = Array.from(document.querySelectorAll('.modal .card')).find(
    (c) => (c.querySelector('.card-head h2') || {}).textContent === '图标');
  const tiles = card ? Array.from(card.querySelectorAll('.icon-grid .icon-tile')) : [];
  const selected = tiles.filter((t) => t.classList.contains('selected'));
  const status = card ? card.querySelector('.icon-library-status') : null;
  const img = card ? card.querySelector('.card-body > .row > img') : null;
  return {
    tiles: tiles.length,
    visible: tiles.filter((t) => t.checkVisibility({ checkOpacity: true, checkVisibilityCSS: true })).length,
    imgSizes: tiles.map((t) => {
      const i = t.querySelector('img');
      return i ? Math.round(i.getBoundingClientRect().width) : 0;
    }),
    // 渲染尺寸由 CSS 钉死，裂图同样是 60px —— 所以"没有被压成 0"和
    // "图片真的解码出来了"必须分开断言，只看前者会把 404 放过去。
    imgDecoded: tiles.map((t) => {
      const i = t.querySelector('img');
      return i ? i.naturalWidth : 0;
    }),
    names: tiles.map((t) => {
      const n = t.querySelector('.name');
      return n ? n.textContent.trim() : '';
    }),
    selected: selected.length,
    selectedNames: selected.map((t) => t.dataset.name || ''),
    status: status ? status.textContent.trim() : '',
    previewSrc: img ? (img.getAttribute('src') || '') : '',
    previewLoaded: !!img && img.naturalWidth > 0,
    previewWidth: img ? img.naturalWidth : 0,
    // 选中态那个对勾是 ::after 画出来的 —— DOM 属性里查不到，
    // 只能问伪元素的样式。"class 加上了"替代不了"用户看得见"。
    checkMark: selected.length
      ? getComputedStyle(selected[0], '::after').content
      : 'none',
    // 对勾只是选中态的一半：底色 / 描边 / 外圈由 .selected 那条规则给。
    // 只查 classList 的话，"class 加上了、样式声明没了"会整段通过 ——
    // 用户点下去界面纹丝不动。所以这里把选中与未选中的计算样式都取出来比对。
    selectedStyle: selected.length ? tileStyle(selected[0]) : null,
    plainStyle: (tiles.find((t) => !t.classList.contains('selected')) || null)
      ? tileStyle(tiles.find((t) => !t.classList.contains('selected')))
      : null,
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

    // ---- 「图标库」档的夹具 ----
    //
    // 用真实数据去断言等于空过：本机 icons/ 目录在 .build 下、随环境变，
    // 多半是空的，而"零个磁贴里没有不合格的"永远是绿的。
    //
    // 磁贴里的 <img> 也换成一张真实存在的 PNG：让它 404 的话磁贴渲染出来
    // 就是 0×0，"磁贴有尺寸"这条断言会变成一句永远成立的假话。
    let iconsListCalls = 0;
    let iconBytes = null;
    try {
      // 必须显式补上尾斜杠：这一趟是**在页面加载之前**发的裸请求，页内那条
      // 同步 <base> 守卫还没生效，相对地址会按"最后一段是文件名"解析，
      // 于是打到 /app/icons/... 上（少了应用名那一段）→ 404 → 夹具里全是空字节，
      // "缩略图不是裂图"那条断言会永远失败在夹具上，而不是失败在代码上。
      const r = await ctx.request.get(new URL('icons/default-64.png', url.replace(/\/?$/, '/')).href);
      if (r.ok()) iconBytes = await r.body();
    } catch (_) { /* 拿不到就让下面那条尺寸断言自己报出来，不在这里静默通过 */ }

    await page.route((u) => u.pathname.endsWith('/api/icons'), (route) => {
      iconsListCalls += 1;
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ items: LIBRARY_FIXTURE, total: LIBRARY_FIXTURE.length }),
      });
    });
    await page.route(
      (u) => LIBRARY_FIXTURE.some((it) => u.pathname.endsWith('/' + it.url)),
      (route) => route.fulfill({
        status: 200, contentType: 'image/png', body: iconBytes || Buffer.alloc(0),
      }),
    );

    await page.goto(url, { waitUntil: 'networkidle', timeout: 30000 });
    await page.waitForSelector('#app:not([hidden])', { timeout: 20000 });
    await page.waitForTimeout(300);

    // 打开新建弹窗。空环境下按钮文案是「新建第一个桌面应用」，
    // 有数据时是「新建」—— 按子串找，两种都命中。
    const newBtn = page.locator('button', { hasText: '新建' }).first();
    await newBtn.click({ timeout: 10000 });
    await page.waitForSelector('.modal', { timeout: 10000 });

    // 弹窗是 pop 动画进场的（from { opacity: 0 }）。动画没跑完就取样的话，
    // 整棵子树的 opacity 还不到 1，checkVisibility 一律判"不可见" ——
    // 于是守卫会随机红一次，而红的原因跟被测的行为毫无关系。
    // 等它落定再取样：解析请求被刻意拖慢 1.2s，"进行中"这个瞬时状态仍然抓得到。
    await page.waitForFunction(() => {
      const m = document.querySelector('.modal');
      if (!m) return false;
      return Number(getComputedStyle(m).opacity) >= 0.99;
    }, null, { timeout: 3000 }).catch(() => {});

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

    // ---- 「图标库」档：能不能从已有的图标里挑一枚 ----
    //
    // 守的是"点了没反应"：选中之后只把文件名写进 model、界面上什么都不变，
    // 用户根本判断不出到底选上了没有。
    const libTab = page.locator('.modal .segmented button', { hasText: '图标库' });
    const libTabCount = await libTab.count();
    check(libTabCount > 0, '图标卡片里有「图标库」这一档', `找到 ${libTabCount} 个`);

    if (libTabCount > 0) {
      await libTab.first().click();
      await page.waitForSelector('.modal .icon-grid .icon-tile', { timeout: 10000 });
      await page.waitForTimeout(200);

      const tiles = page.locator('.modal .icon-grid .icon-tile');
      const tileCount = await tiles.count();
      check(tileCount === LIBRARY_FIXTURE.length, '图库里已有的图标都被列出来了',
        `期望 ${LIBRARY_FIXTURE.length} 个，实际 ${tileCount} 个`);
      check(iconsListCalls > 0, '这一档真的去读了图标库（不是空转）',
        `实际请求 ${iconsListCalls} 次`);

      const initial = await page.evaluate(READ_LIBRARY);
      check(initial.visible === tileCount && tileCount > 0, '磁贴真的可见（不是建了节点没挂上）',
        JSON.stringify({ visible: initial.visible, tiles: initial.tiles }));
      check(initial.imgSizes.every((n) => n >= 40), '每枚图标都画出了实际尺寸（没被样式压成 0）',
        `宽度=${JSON.stringify(initial.imgSizes)}`);
      check(initial.imgDecoded.every((n) => n > 0), '每枚图标的图片都真的解码出来了（不是裂图）',
        `naturalWidth=${JSON.stringify(initial.imgDecoded)}`);
      check(initial.names.every((n) => n.length > 0), '每个磁贴都带名字（否则一堆图认不出哪个是哪个）',
        JSON.stringify(initial.names));
      check(initial.selected === 0, '刚进来时没有任何磁贴是选中态',
        `selected=${initial.selected}`);

      // 点第二枚
      //
      // 点完必须把鼠标挪开再取样式：`.pick:hover` 与 `.pick.selected` 用的是
      // 同一个底色变量，光标停在刚点的那枚上时 hover 会完美伪装成选中态 ——
      // 于是"样式规则被删掉"和"样式还在"长得一模一样。
      const want = LIBRARY_FIXTURE[1];
      await tiles.nth(1).click();
      await page.mouse.move(2, 2);
      await page.waitForTimeout(250);
      const picked = await page.evaluate(READ_LIBRARY);
      check(picked.selected === 1, '点一下就出现唯一的选中态（点击真的生效了）',
        `selected=${picked.selected}`);
      check(picked.selectedNames.length === 1 && picked.selectedNames[0] === want.name,
        '选中的正是点的那一枚', `期望 ${want.name}，实际 ${picked.selectedNames.join(',')}`);
      check(picked.status.includes(want.name), '界面上写明了选的是哪一枚',
        `实际「${picked.status}」`);
      check(picked.previewSrc.endsWith(want.url), '缩略图换成了选中的那枚图标',
        `src=${picked.previewSrc}`);
      check(picked.previewLoaded, '缩略图真的加载出来了（不是裂图）',
        `naturalWidth=${picked.previewWidth}`);
      check(picked.checkMark !== 'none', '选中态在界面上看得见（不是只改了个 class）',
        `::after content=${picked.checkMark}`);
      // 与上面那条并列：对勾还在、但 .selected 那条样式规则被删掉/改名的话，
      // class 照样加得上、对勾照样画得出来，用户看到的却是"点了没反应"。
      const s0 = picked.selectedStyle;
      const p0 = picked.plainStyle;
      check(
        !!s0 && !!p0 && (
          s0.background !== p0.background
          || s0.borderColor !== p0.borderColor
          || s0.shadow !== p0.shadow
        ),
        '选中的那枚在外观上真的和没选中的不一样（底色 / 描边 / 外圈至少差一处）',
        `选中 ${JSON.stringify(s0)} vs 未选中 ${JSON.stringify(p0)}`);

      // 再点第一枚：选中态要跟着走，不能两个都亮
      await tiles.nth(0).click();
      await page.mouse.move(2, 2);
      await page.waitForTimeout(250);
      const moved = await page.evaluate(READ_LIBRARY);
      check(moved.selected === 1 && moved.selectedNames[0] === LIBRARY_FIXTURE[0].name,
        '换选另一枚时选中态会跟着走（不是变成两个都选中）',
        `selected=${moved.selected} → ${moved.selectedNames.join(',')}`);
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
