/* 服务发现里那些「重复的端口行」到底还在不在（真浏览器）。
 *
 * 用户的原话：
 *   「为什么扫描出来的端口是重复的，有 0.0.0.0 的有不带的，
 *     然后窗口最大化之后又再重复出来了很多」
 *
 * 为什么必须用真浏览器验：
 *   - 「两张长得一模一样的卡片」是一个**视觉事实**。静态守卫能查到代码里
 *     有协议标签、有地址归纳，查不到它们有没有真的画到屏幕上 ——
 *     也查不到"C 行和 D 行渲染出来是否完全相同"。
 *   - 「最大化之后又冒出很多」更是只有把视口改掉再数一遍才能证伪。
 *
 * 这里刻意把接口的响应**替换成夹具**，而不是等真机扫出端口：
 *   - 本机（Windows/macOS）根本没有 /proc/net，扫描结果恒为空，
 *     等真实数据的话这一段会"零个元素里没有不合格的"式空过；
 *   - 夹具里**故意放一对没有合并的 tcp/udp**（同一个端口、同一个进程、
 *     同一个地址），用来验证即便后端哪天不再合并，光靠渲染也不会出现
 *     两条一模一样的行 —— 这是用户看到的现象本身。
 * 夹具的形状（addresses / protos / scope）由 Go 侧的单测
 * （internal/discovery/group_test.go 的 TestPortInfoJSONCarriesGroupedFields）
 * 钉住，两边合起来才是完整的一条链。
 *
 * 用法：node browser-discovery-check.js <url>
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

const url = process.argv[2];
if (!url) {
  console.error('用法: node browser-discovery-check.js <url>');
  process.exit(2);
}

// 夹具：模拟一台飞牛 NAS 上合并后的真实输出。
const FIXTURE = {
  items: [
    // IPv4/IPv6 通配各一份，已合并成一行
    { port: 22, proto: 'tcp', protos: ['tcp'], address: '0.0.0.0',
      addresses: ['0.0.0.0', '::'], scope: 'all', process: 'sshd', pid: 100, user: '0' },
    { port: 80, proto: 'tcp', protos: ['tcp'], address: '0.0.0.0',
      addresses: ['0.0.0.0', '::'], scope: 'all', process: 'nginx', pid: 101, user: '0' },
    // tcp + udp 已合并成一行，协议要显示成 TCP+UDP
    { port: 111, proto: 'tcp', protos: ['tcp', 'udp'], address: '0.0.0.0',
      addresses: ['0.0.0.0', '::'], scope: 'all', process: 'rpcbind', pid: 102, user: '0' },
    // 绑在多个网卡地址上（nmbd 就长这样）
    { port: 137, proto: 'udp', protos: ['udp'], address: '172.17.0.1',
      addresses: ['172.17.0.1', '172.25.0.1', '172.26.0.1', '192.168.31.80'],
      scope: 'specific', process: 'nmbd', pid: 103, user: '0' },
    // 只绑回环
    { port: 631, proto: 'tcp', protos: ['tcp'], address: '127.0.0.1',
      addresses: ['127.0.0.1'], scope: 'local', process: 'cupsd', pid: 104, user: '0' },
    // 容器映射
    { port: 5003, proto: 'tcp', protos: ['tcp', 'udp'], address: '0.0.0.0',
      addresses: ['0.0.0.0', '::'], scope: 'all', process: 'docker-proxy', pid: 200, user: '0',
      container: { id: 'abcdef123456', name: 'kspeeder', image: 'registry.kspeeder.com/lima/kspeeder:latest', private_port: 5003 } },

    // ---- 下面两个是**故意没有合并**的一对 ----
    // 模拟"后端哪天不再合并"时的输入：同一个端口、同一个进程、同一个地址，
    // 唯一的区别只有协议。它们渲染出来必须仍然能分辨 ——
    // 这正是用户当年看到的那两条一模一样的行。
    { port: 445, proto: 'tcp', protos: ['tcp'], address: '0.0.0.0',
      addresses: ['0.0.0.0'], scope: 'all', process: 'smbd', pid: 110, user: '0' },
    { port: 445, proto: 'udp', protos: ['udp'], address: '0.0.0.0',
      addresses: ['0.0.0.0'], scope: 'all', process: 'smbd', pid: 110, user: '0' },
  ],
  total: 8,
  sockets: 101,
  docker: true,
  elapsed_ms: 24,
};

const EXPECTED_ROWS = FIXTURE.items.length;

let pass = 0;
let fail = 0;
const check = (ok, label, detail = '') => {
  if (ok) { pass += 1; console.log(`  PASS  ${label}`); }
  else { fail += 1; console.log(`  FAIL  ${label}${detail ? '    ' + detail : ''}`); }
};

// 读一页端口行。
//
// 一律用 checkVisibility 判可见、getBoundingClientRect 判尺寸 ——
// "属性已设置""节点在 DOM 里"都替代不了"用户真的看得见"。
const READ_ROWS = () => Array.from(document.querySelectorAll('.port-item')).map((el) => {
  const num = el.querySelector('.port-num > span');
  const proto = el.querySelector('.port-proto');
  const line1 = el.querySelector('.line1');
  const line2 = el.querySelector('.line2');
  const txt = (n) => (n ? n.textContent.trim() : '');
  return {
    port: txt(num),
    proto: txt(proto),
    protoVisible: !!proto && proto.checkVisibility({ checkOpacity: true, checkVisibilityCSS: true }),
    protoHeight: proto ? Math.round(proto.getBoundingClientRect().height) : 0,
    line1: txt(line1),
    line2: txt(line2),
    title: el.getAttribute('title') || '',
    // 一行在界面上的"身份"：用户就是这么分辨两行是不是重复的
    identity: [txt(num), txt(proto), txt(line1), txt(line2)].join(' | '),
  };
});

(async () => {
  const exe = EDGE_CANDIDATES.find((p) => { try { return fs.existsSync(p); } catch (_) { return false; } });
  if (!exe) {
    console.error('!! 找不到 Chromium 系浏览器（Edge / Chrome / Chromium）');
    process.exit(2);
  }

  const browser = await chromium.launch({ executablePath: exe, headless: true });
  const pageErrors = [];

  try {
    const ctx = await browser.newContext({ viewport: { width: 1280, height: 900 }, deviceScaleFactor: 1 });
    const page = await ctx.newPage();
    page.on('pageerror', (e) => pageErrors.push(String(e)));

    await page.route('**/api/discovery/ports*', (route) => route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify(FIXTURE),
    }));

    await page.goto(`${url}#discovery`, { waitUntil: 'networkidle', timeout: 30000 });
    await page.waitForSelector('#app:not([hidden])', { timeout: 20000 });
    await page.waitForSelector('.port-item', { timeout: 15000 });
    await page.waitForTimeout(300);

    // 先把"零个元素"这条空过路径堵死：行数不对的话下面全是空过。
    const rows = await page.evaluate(READ_ROWS);
    check(rows.length === EXPECTED_ROWS,
      '端口行都渲染出来了（不是零个元素里没有不合格的）',
      `期望 ${EXPECTED_ROWS} 行，实际 ${rows.length} 行`);

    // ---- 用户报的「重复」 --------------------------------------------------
    // 每一行都得说得出自己**为什么**不是另一行。协议藏在第二行末尾时，
    // smbd 445 的 tcp / udp 两行会渲染成完全相同的文字。
    const identities = rows.map((r) => r.identity);
    const dupes = identities.filter((x, i) => identities.indexOf(x) !== i);
    check(dupes.length === 0,
      '没有任何两行看起来是一样的（这正是用户看到的「重复」）',
      dupes.length ? `重复的行：${JSON.stringify([...new Set(dupes)])}` : '');

    // 单独把那一对"故意没合并"的挑出来再确认一次：断言要能 FAIL，
    // 否则前面那条只是在数一个必然为空的集合。
    const smbd = rows.filter((r) => r.line1 === 'smbd' && r.port === ':445');
    check(smbd.length === 2 && new Set(smbd.map((r) => r.identity)).size === 2,
      '同一个端口上的一对 tcp/udp 仍然分辨得出来（协议就是唯一的区别）',
      JSON.stringify(smbd.map((r) => r.identity)));

    check(rows.every((r) => r.proto.length > 0),
      '每一行都写出了协议（不是只在副标题为空时才顺手显示）',
      JSON.stringify(rows.map((r) => `${r.port}=${r.proto || '空'}`)));
    check(rows.every((r) => r.protoVisible && r.protoHeight >= 8),
      '协议标签真的可见、有高度（没被样式压没）',
      JSON.stringify(rows.map((r) => `${r.port}:${r.protoHeight}px`)));
    check(rows.some((r) => r.proto === 'TCP+UDP'),
      '同时监听 tcp 与 udp 的服务写成「TCP+UDP」',
      JSON.stringify(rows.map((r) => r.proto)));

    // ---- 地址不再是一串「0.0.0.0」和一个看起来像空白的「::」 ---------------
    check(rows.every((r) => r.line2.length > 0),
      '每一行的副标题都有内容（不再出现「不带地址」的那种行）',
      JSON.stringify(rows.map((r) => `${r.port}=${r.line2 || '空'}`)));
    check(rows.every((r) => !/^(::|0\.0\.0\.0)$/.test(r.line2) && !r.line2.includes('::')),
      '地址被归纳成人话，而不是把 :: / 0.0.0.0 原样丢出来',
      JSON.stringify(rows.map((r) => r.line2)));
    check(rows.some((r) => r.line2.includes('全网卡可见')) && rows.some((r) => r.line2.includes('仅本机')),
      '「所有网卡」与「仅本机」两种范围都说清楚了',
      JSON.stringify(rows.map((r) => r.line2)));
    check(rows.some((r) => /\d+ 个网卡地址/.test(r.line2)),
      '绑在多个网卡地址上的服务被归纳成「N 个网卡地址」而不是列 11 条',
      JSON.stringify(rows.map((r) => r.line2)));
    // 被归纳掉的信息不能真的丢：悬停要给得回来。
    const multi = rows.find((r) => /\d+ 个网卡地址/.test(r.line2));
    check(!!multi && multi.title.includes('172.17.0.1') && multi.title.includes('192.168.31.80'),
      '悬停时能看到被归纳掉的完整绑定地址',
      multi ? multi.title : '找不到那一行');

    // ---- 少了一大截要能解释 -------------------------------------------------
    check(await page.locator('.stat-label', { hasText: '监听服务' }).count() > 0,
      '统计卡说的是「监听服务」而不是「监听端口」（数的粒度变了）');
    const hinted = await page.locator(`.stat[title*="101"]`).count();
    check(hinted > 0,
      '并说明了合并前有多少个内核套接字（101 → 8 不能让人以为漏了）',
      `带 101 提示的统计卡 ${hinted} 个`);

    // ---- 用户报的「窗口最大化之后又再重复出来了很多」 -----------------------
    // 这一条只有改视口再数一遍才能证伪：静态文本里看不出任何"重复"的迹象。
    //
    // 说明它的失效方式：应用里没有任何 resize 处理，改视口本身不会重绘，
    // 所以真正会让它挂的是"重绘时往后追加而不是替换"——那条路径由下面
    // 「重新扫描」那一步直接盯住（重绘是必然发生的）。
    for (const vp of [{ width: 1680, height: 1000, label: '最大化' },
                      { width: 760, height: 800, label: '缩到窄屏' }]) {
      await page.setViewportSize({ width: vp.width, height: vp.height });
      await page.waitForTimeout(400);
      const after = await page.evaluate(READ_ROWS);
      const ids = after.map((r) => r.identity);
      check(after.length === rows.length && new Set(ids).size === ids.length,
        `窗口${vp.label}到 ${vp.width}×${vp.height} 后行数不变、也没有重复`,
        `${rows.length} → ${after.length} 行，去重后 ${new Set(ids).size}`);
    }

    // ---- 重绘必须是"替换"而不是"追加" --------------------------------------
    // 这一条是对上面那条的正面取证：重绘一定会发生（点一次「重新扫描」
    // 会走两遍 draw），如果 replace 忘了清空旧内容，行数会成倍增长 ——
    // 那正是用户说的「又再重复出来了很多」。
    await page.setViewportSize({ width: 1280, height: 900 });
    await page.waitForTimeout(200);
    const rescan = page.locator('button', { hasText: '重新扫描' }).first();
    if (await rescan.count() > 0) {
      await rescan.click();
      await page.waitForTimeout(1200);
      const after = await page.evaluate(READ_ROWS);
      const ids = after.map((r) => r.identity);
      check(after.length === rows.length && new Set(ids).size === ids.length,
        '点「重新扫描」后行数仍然不变（重绘是替换，不是往后追加）',
        `${rows.length} → ${after.length} 行，去重后 ${new Set(ids).size}`);
    } else {
      check(false, '点「重新扫描」后行数仍然不变（重绘是替换，不是往后追加）', '找不到重新扫描按钮');
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
