/* discovery.js —— 「服务发现」视图
 *
 * 三块内容共用一页，因为它们解决的是同一个问题：
 * 「有什么东西可以被放到桌面上」。
 *   本机端口   —— 已经在这台机器上跑着的服务（容器来源与其它来源分组展示）
 *   局域网扫描 —— 别的设备上跑着的服务
 *   远程主机   —— 保存过的远端机器，供端口映射与 SSH 隧道使用
 *
 * 历史上「容器一键启用」是一个独立页签，但它和本机端口里的「来自容器」
 * 说的是同一件事——一个是从容器声明的桌面标签直接启用，一个是从扫描到的
 * 容器端口新建链接。分在两个页签里，用户得先想清楚"我这个需求该去哪个页签"，
 * 而这本来是同一种意图。现在它们合并到「本机端口」的同一张卡里。
 */

import {
  h, append, replace, notify, withBusy, confirmDialog,
  emptyState, fmtRelative, field, table, modal,
} from '../dom.js';
import { icon } from '../icons.js';
import { api } from '../api.js';
import { state, patch } from '../store.js';
import { openLinkEditor } from './editor.js';

export const title = '服务发现';
export const subtitle = () => '扫描本机端口、Docker 容器与局域网服务，一键放到飞牛桌面';

export function actions() {
  return [
    h('button', {
      class: 'btn accent', type: 'button',
      onclick: (e) => withBusy(e.currentTarget, () => scanLocal(true)),
    }, [icon('refresh', 15), '重新扫描']),
  ];
}

let root = null;
let tab = 'ports';

export function mount(container) {
  root = container;
  draw();
  if (!state.discovery.ports.length) scanLocal(false);
  loadDockLabels();
  loadHosts();
  // 网络环境每次进来都重新探测：用户很可能刚插上网线、刚换了网段。
  loadNetInfo();
}

export function refresh() {
  if (root) draw();
}

/* -------------------------------------------------------------------- 渲染 */

function draw() {
  if (!root) return;
  replace(root, [
    h('div', { class: 'segmented mb-3' }, [
      tabButton('ports', '本机端口'),
      tabButton('lan', '局域网扫描'),
      tabButton('hosts', '远程主机'),
    ]),
    bodyFor(tab),
  ]);
}

function tabButton(value, label) {
  return h('button', {
    type: 'button',
    class: tab === value ? 'active' : '',
    onclick: (e) => {
      for (const btn of e.currentTarget.parentNode.children) btn.classList.remove('active');
      e.currentTarget.classList.add('active');
      tab = value;
      draw();
    },
  }, [label]);
}

function bodyFor(name) {
  switch (name) {
    case 'lan': return lanBody();
    case 'hosts': return hostsBody();
    default: return portsBody();
  }
}

/* --------------------------------------------------------------- 本机端口 */

function portsBody() {
  const box = h('div');

  if (state.discovery.loading) {
    append(box, h('div', { class: 'grid cols-3' }, Array.from({ length: 6 }, () => h('div', { class: 'stat' }, [
      h('div', { class: 'skeleton', style: { width: '42%' } }),
      h('div', { class: 'skeleton mt-2', style: { width: '68%', height: '18px' } }),
    ]))));
    return box;
  }

  const ports = state.discovery.ports || [];
  const labels = state.dockLabels || [];

  // 端口一个都没扫到、也没有可一键启用的容器，才是真的空。
  // 少了 labels 这个条件，一个"容器的端口没被扫到、但声明了桌面标签"的
  // 环境会直接被空状态挡住，白丢一个可用的入口。
  if (!ports.length && !labels.length) {
    append(box, h('div', { class: 'card' }, [
      h('div', { class: 'card-body' }, [
        emptyState('没有发现监听中的端口', '确认服务已启动，然后点右上角「重新扫描」。', icon('activity', 38)),
      ]),
    ]));
    return box;
  }

  if (!state.discovery.docker) {
    append(box, h('div', { class: 'notice-bar' }, [
      icon('info', 16),
      h('span', { class: 'grow', text: '未检测到 Docker 守护进程，容器信息不可用（端口扫描本身不受影响）。' }),
    ]));
  }

  const containerPorts = ports.filter((p) => p.container);
  const otherPorts = ports.filter((p) => !p.container);
  const managed = ports.filter((p) => p.managed).length;

  append(box, h('div', { class: 'grid cols-4 mb-3' }, [
    statCard('activity', '监听端口', String(ports.length)),
    statCard('check', '已放到桌面', String(managed)),
    statCard('box', '来自容器', String(containerPorts.length + labels.length)),
    statCard('zap', '扫描耗时', state.discovery.at ? `${state.discovery.elapsed || 0} ms` : '—'),
  ]));

  append(box, containerCard(labels, containerPorts));

  if (otherPorts.length) {
    append(box, h('div', { class: 'card' }, [
      h('div', { class: 'card-head' }, [
        h('div', {}, [
          h('h2', { text: '其它服务' }),
          h('p', { text: '没有容器信息的本机监听端口；点击「放到桌面」即可生成一个飞牛桌面图标' }),
        ]),
      ]),
      h('div', { class: 'card-body' }, [
        h('div', { class: 'port-grid' }, otherPorts.map(portItem)),
      ]),
    ]));
  }

  return box;
}

/* --------------------------------------------------- 来自容器（含一键启用） */

// containerCard 把两类「来自容器」的入口放在同一张卡里。
//
// 上半部分是容器自己在 compose 里声明的桌面标签 —— 用户已经表达了意图，
// 这里只需要一个开关确认；下半部分是扫描到的容器端口 —— 意图还得用户现定，
// 于是走「放到桌面」进入编辑器。两者是同一件事的两个入口，不该分居两页。
function containerCard(labels, ports) {
  const body = [];

  if (labels.length) {
    body.push(h('div', { class: 'card-body tight' }, [dockLabelTable(labels)]));
  } else {
    body.push(h('div', { class: 'card-body' }, [
      h('p', { class: 'muted', text:
        '没有识别到声明了桌面标签的容器。在 compose 里加上 labels 即可一键启用，例如：' +
        'watchcow: "1"、watchcow.port: "5244"、watchcow.title: "影音库"' }),
    ]));
  }

  if (ports.length) {
    body.push(h('div', { class: 'card-body' }, [
      h('div', { class: 'port-grid' }, ports.map(portItem)),
    ]));
  }

  return h('div', { class: 'card' }, [
    h('div', { class: 'card-head' }, [
      h('div', {}, [
        h('h2', { text: '来自容器' }),
        h('p', { text: '声明了桌面标签的容器可一键启用；也可以直接从下面扫描到的容器端口新建链接' }),
      ]),
    ]),
    ...body,
  ]);
}

function dockLabelTable(list) {
  return table([
    { label: '容器' },
    { label: '标题' },
    { label: '端口', width: '130px' },
    { label: '启用', width: '70px' },
  ], list, (item) => h('tr', {}, [
    h('td', {}, [
      h('div', { class: 'cell-title', text: item.container_name || '—' }),
      h('div', { class: 'cell-sub mono', text: item.key }),
    ]),
    h('td', {}, [
      h('div', { text: item.title || '—' }),
      item.path ? h('div', { class: 'cell-sub mono', text: item.path }) : null,
    ]),
    h('td', { class: 'num' }, [h('span', { class: 'tag mono', text: `${item.scheme || 'http'}:${item.port || '—'}` })]),
    h('td', {}, [
      h('label', { class: 'switch' }, [
        h('input', {
          type: 'checkbox', checked: !!item.enabled,
          onchange: (e) => toggleDockLabel(item, e.target.checked, e.target),
        }),
        h('span', { class: 'slider' }),
      ]),
    ]),
  ]));
}

function portItem(p) {
  const name = p.container?.name || p.process || '未知服务';
  const sub = [
    p.container?.image ? p.container.image : null,
    p.address || null,
    p.user || null,
  ].filter(Boolean).join(' · ');

  return h('div', { class: 'port-item' }, [
    h('div', { class: 'port-num', text: `:${p.port}` }),
    h('div', { class: 'meta' }, [
      h('div', { class: 'line1 truncate', title: name, text: name }),
      h('div', { class: 'line2 truncate', title: sub, text: sub || p.proto.toUpperCase() }),
    ]),
    p.managed
      ? h('span', { class: 'tag ok nowrap' }, [icon('check', 12), '已在桌面'])
      : h('button', {
          class: 'btn sm ok nowrap', type: 'button',
          onclick: () => addPort(p),
        }, ['放到桌面']),
  ]);
}

async function addPort(p) {
  const fallbackName = p.container?.name || p.process || `端口 ${p.port}`;
  const result = await openLinkEditor(null, {
    kind: 'local',
    port: p.port,
    name: fallbackName,
    path: '/',
    scheme: 'http',
    container: p.container
      ? { name: p.container.name, id: p.container.id, image: p.container.image, service: p.container.service }
      : undefined,
  });
  if (result) scanLocal(false);
}

function statCard(iconName, label, value) {
  return h('div', { class: 'stat' }, [
    h('div', { class: 'stat-label' }, [icon(iconName, 13), label]),
    h('div', { class: 'stat-value', text: value }),
  ]);
}

async function scanLocal(force) {
  patch({ discovery: { ...state.discovery, loading: true } });
  refresh();
  const started = Date.now();
  try {
    const res = await api.discovery.ports(force);
    patch({
      discovery: {
        ports: res.items || [],
        docker: !!res.docker,
        loading: false,
        at: Date.now(),
        elapsed: res.elapsed_ms || (Date.now() - started),
      },
    });
    if (force) notify.ok(`扫描完成，发现 ${(res.items || []).length} 个监听端口`);
  } catch (err) {
    patch({ discovery: { ...state.discovery, loading: false } });
    if (err.status !== 401) notify.error(`扫描失败：${err.message}`);
  }
  refresh();
}

/* ----------------------------------------------------------- 容器一键启用 */

async function toggleDockLabel(item, enabled, input) {
  input.disabled = true;
  try {
    await api.discovery.setDockLabel(item.key, enabled);
    notify.ok(enabled ? `已启用「${item.title || item.container_name}」` : '已停用');
    await Promise.all([loadDockLabels(), reloadLinksQuiet()]);
  } catch (err) {
    input.checked = !enabled;
    notify.error(err.message);
  } finally {
    input.disabled = false;
    refresh();
  }
}

async function loadDockLabels() {
  try {
    const res = await api.discovery.dockLabels();
    patch({ dockLabels: res.items || [] });
    refresh();
  } catch (err) {
    if (err.status !== 401) console.warn('读取容器标签失败', err);
  }
}

/* ------------------------------------------------------------ 局域网扫描 */

let scanTimer = null;

// startPolling 按固定间隔拉取扫描进度。
//
// 为什么是轮询而不是 SSE：进度每几百毫秒变一次，把它塞进事件流等于用
// 推送通道做轮询，而且一旦某条事件丢了，进度条就会永久停住。
// 轮询是"每次都拿到全量真相"，天然能自愈。
function startPolling() {
  stopPolling();
  scanTimer = setInterval(async () => {
    // 视图已经被切走（root 脱离文档）就停表：本视图没有卸载钩子，
    // 不停的话扫描期间会在后台一直轮询一个已经不存在的界面。
    if (!root || !root.isConnected) { stopPolling(); return; }
    try {
      const job = await api.discovery.scanStatus();
      patch({ scan: { ...state.scan, job } });
      if (!job.running) stopPolling();
      refresh();
    } catch {
      // 拉取失败就停表：继续空转只会让界面看起来"在动"，实际什么都没更新。
      stopPolling();
    }
  }, 800);
}

function stopPolling() {
  if (scanTimer) {
    clearInterval(scanTimer);
    scanTimer = null;
  }
}

function lanBody() {
  const info = state.scan.info;
  const job = state.scan.job;
  const running = !!(job && job.running);

  const box = h('div', {});

  // 进入页签时若发现后台还有一轮扫描在跑（例如刚从别的页签切回来），
  // 把轮询接上，而不是让用户对着一个静止的进度条发呆。
  if (running && !scanTimer) startPolling();

  append(box, targetCard(info, running));

  if (state.scan.error) {
    append(box, h('div', { class: 'notice-bar danger mt-3' }, [
      icon('alert', 16),
      h('span', { class: 'grow', text: state.scan.error }),
    ]));
  }

  if (job) append(box, progressCard(job, running));

  return box;
}

// targetCard 是「扫哪里」这一块。
function targetCard(info, running) {
  const primary = info && info.primary ? `${info.primary}.0/24` : '';
  const parent = info && info.parent ? `${info.parent}.0/24` : '';

  const customInput = h('input', {
    type: 'text', value: state.scan.spec,
    placeholder: '例如 192.168.1.10-60、192.168.1、192.168.1.0/24、10.0.0.1',
    oninput: (e) => { state.scan.spec = e.target.value; },
    onkeydown: (e) => { if (e.key === 'Enter') beginScan(); },
  });

  const modeButton = (value, label, enabled, hint) => h('button', {
    type: 'button',
    class: state.scan.mode === value ? 'active' : '',
    disabled: !enabled,
    title: hint || '',
    onclick: (e) => {
      for (const btn of e.currentTarget.parentNode.children) btn.classList.remove('active');
      e.currentTarget.classList.add('active');
      state.scan.mode = value;
      refresh();
    },
  }, [label]);

  const modes = h('div', { class: 'segmented' }, [
    modeButton('auto', `本机网段${primary ? `（${primary}）` : ''}`, true,
      '扫描飞牛所在网段里的其它设备'),
    modeButton('parent', parent ? `上级网段（${parent}）` : '上级网段（未检测到）', !!parent,
      parent ? '扫描默认网关所在的网段（通常是路由器上游那一段）'
             : '默认网关与本机在同一网段，说明本段就是最外层；要扫别处请用「自定义范围」'),
    modeButton('custom', '自定义范围', true, '自己指定要扫描的地址或范围'),
  ]);

  const infoLine = netSummary(info);

  return h('div', { class: 'card' }, [
    h('div', { class: 'card-head' }, [
      h('div', {}, [
        h('h2', { text: '局域网扫描' }),
        h('p', { text: '扫描同网段设备上开放的常见服务端口，点端口号即可直接创建桌面链接' }),
      ]),
    ]),
    h('div', { class: 'card-body' }, [
      infoLine,
      h('div', { class: 'mt-3' }, [modes]),
      state.scan.mode === 'custom'
        ? h('div', { class: 'mt-2' }, [
            customInput,
            h('p', { class: 'muted mt-1', style: { fontSize: '.78rem' }, text:
              '支持：单台 192.168.1.10、整段 192.168.1、区间 192.168.1.10-60、' +
              `掩码 192.168.1.0/24、主机名；多个用逗号分隔。单次最多 ${(info && info.limit) || 1024} 台。` }),
          ])
        : null,
      h('div', { class: 'row mt-3' }, [
        h('button', {
          class: 'btn primary', type: 'button',
          onclick: (e) => withBusy(e.currentTarget, () => beginScan()),
        }, [icon('search', 15), running ? '重新开始' : '开始扫描']),
        running
          ? h('button', {
              class: 'btn danger', type: 'button',
              onclick: () => cancelScan(),
            }, ['停止扫描'])
          : null,
      ]),
      h('p', { class: 'muted mt-2', style: { fontSize: '.78rem' }, text:
        '说明：目标超过 32 台时先做一轮「在线探测」，再只对在线设备细扫端口；' +
        '如果某台设备在线但一个常见端口都没开，它可能被跳过 —— 这时把它的 IP 单独填进「自定义范围」再扫。' }),
    ]),
  ]);
}

// netSummary 展示探测到的网络环境，顺便解释"上级网段为什么是空的"。
function netSummary(info) {
  if (!info) {
    return h('div', { class: 'row' }, [
      h('span', { class: 'muted', text: '正在探测本机网络环境…' }),
    ]);
  }

  const parts = [];
  if (info.local_ips?.length) parts.push(`本机 IP ${info.local_ips.join('、')}`);
  if (info.gateway) parts.push(`网关 ${info.gateway}`);
  if (info.primary) parts.push(`本机网段 ${info.primary}.0/24`);
  parts.push(info.parent ? `上级网段 ${info.parent}.0/24` : '上级网段 未检测到（本段已包含网关）');

  return h('div', {}, [
    h('div', { class: 'row', style: { flexWrap: 'wrap', gap: '8px' } }, [
      h('span', { class: 'muted', style: { fontSize: '.78rem' }, text: parts.join(' · ') }),
    ]),
    info.warning
      ? h('div', { class: 'notice-bar mt-2' }, [icon('info', 16), h('span', { class: 'grow', text: info.warning })])
      : null,
  ]);
}

// progressCard 是扫描进度与结果。
function progressCard(job, running) {
  const items = job.items || [];
  const percent = job.done
    ? 100
    : (job.probes_total > 0 ? Math.min(100, Math.round((job.probes_done / job.probes_total) * 100)) : 0);

  const card = h('div', { class: 'card mt-3' }, [
    h('div', { class: 'card-head' }, [
      h('div', {}, [
        h('h2', { text: job.done ? (job.canceled ? '扫描已取消' : '扫描结果') : '正在扫描' }),
        h('p', { text: job.range || '' }),
      ]),
      h('span', { class: `tag ${job.done ? (job.canceled ? '' : 'ok') : 'accent'}` }, [
        running ? h('span', { class: 'spinner-sm' }) : null,
        job.stage || '',
      ]),
    ]),
    h('div', { class: 'card-body' }, [
      h('div', { class: 'progress' }, [
        h('div', { class: 'progress-fill', style: { width: `${percent}%` } }),
      ]),
      h('div', { class: 'row between mt-2', style: { fontSize: '.78rem' } }, [
        h('span', { class: 'muted', text:
          `探测 ${job.probes_done || 0} / ${job.probes_total || 0} · ` +
          `在线主机 ${job.hosts_alive || 0} / ${job.hosts_total || 0}` }),
        h('span', { class: 'muted', text:
          job.done ? `发现 ${job.found || 0} 个开放端口 · 耗时 ${job.elapsed_ms || 0} ms` : `${percent}%` }),
      ]),
      h('div', { style: { marginTop: '16px' } }, [resultsTable(items, job)]),
    ]),
  ]);
  return card;
}

function resultsTable(items, job) {
  if (!items.length) {
    return h('p', { class: 'muted', text: job.done
      ? '这一轮没有发现开放端口。可以试试「自定义范围」指定某台设备的 IP 单独扫描。'
      : '正在探测，结果会随扫描进度实时出现在这里…' });
  }

  // 按主机聚合，比一长串「IP:端口」更好读。
  const byHost = new Map();
  for (const item of items) {
    if (!byHost.has(item.address)) byHost.set(item.address, []);
    byHost.get(item.address).push(item);
  }

  const rows = Array.from(byHost.entries()).map(([address, list]) => h('tr', {}, [
    h('td', {}, [h('span', { class: 'mono', text: address })]),
    h('td', {}, [h('div', { class: 'row', style: { flexWrap: 'wrap', gap: '6px' } }, list.map((p) => (
      p.managed
        ? h('span', { class: 'tag ok nowrap' }, [icon('check', 12), `:${p.port}`])
        : h('button', {
            class: 'tag mono accent', type: 'button',
            style: { cursor: 'pointer' },
            title: '点击创建桌面链接',
            onclick: () => openLinkEditor(null, {
              kind: 'proxy', host: address, port: p.port,
              name: `${address}:${p.port}`, path: '/', scheme: 'http',
            }),
          }, [`:${p.port}`])
    )))]),
  ]));

  return table([
    { label: '主机' },
    { label: `开放端口（${items.length} 个，点击可创建链接）` },
  ], rows, (r) => r);
}

async function beginScan() {
  const mode = state.scan.mode;
  const spec = (state.scan.spec || '').trim();
  if (mode === 'custom' && !spec) {
    // 页面内提示而不是 toast：用户此刻正看着输入框，错误应该出现在那一行旁边。
    patch({ scan: { ...state.scan, error: '请先填写要扫描的范围' } });
    refresh();
    return;
  }

  patch({ scan: { ...state.scan, starting: true, error: '' } });
  refresh();

  try {
    const job = await api.discovery.scan(mode === 'custom' ? { mode, spec } : { mode });
    patch({ scan: { ...state.scan, starting: false, job } });
    if (job.running) startPolling();
    else notify.ok(`扫描完成，发现 ${job.found || 0} 个开放端口`);
  } catch (err) {
    patch({ scan: { ...state.scan, starting: false, error: err.message } });
  }
  refresh();
}

async function cancelScan() {
  try {
    const job = await api.discovery.cancelScan();
    patch({ scan: { ...state.scan, job } });
    startPolling(); // 继续轮询到任务真正落终态
  } catch (err) {
    notify.error(err.message);
  }
  refresh();
}

async function loadNetInfo() {
  try {
    const info = await api.discovery.subnets();
    const next = { ...state.scan, info };
    // 没有上级网段时把选择落回本机网段，避免停在一个点不动的按钮上。
    if (next.mode === 'parent' && !info.parent) next.mode = 'auto';
    patch({ scan: next });
    refresh();
  } catch (err) {
    if (err.status !== 401) console.warn('读取网络环境失败', err);
  }
}

/* -------------------------------------------------------------- 远程主机 */

function hostsBody() {
  const hosts = state.hosts || [];
  return h('div', {}, [
    h('div', { class: 'card' }, [
      h('div', { class: 'card-head' }, [
        h('div', {}, [
          h('h2', { text: '远程主机' }),
          h('p', { text: '保存下来的远端机器；在「端口映射」里填写其地址即可自动经 SSH 隧道转发' }),
        ]),
        h('button', { class: 'btn primary', type: 'button', onclick: () => editHost(null) }, [
          icon('plus', 15), '添加主机',
        ]),
      ]),
      // 这一页是全套功能里最"看不出用途"的一个：列表长得和别的页一样，
      // 但不解释的话没人知道登记一台机器之后会发生什么。把用途直接写在
      // 表格上方，比让人去别处找答案更省事。
      h('div', { class: 'card-body' }, [
        h('p', { class: 'muted', style: { margin: '0 0 8px', fontSize: '.82rem', lineHeight: '1.7' }, text:
          '登记一台远端机器，是为了让「端口映射」多一条路：当目标端口不通、但 SSH 能连上时，' +
          '本应用会改用 SSH 隧道去访问那个端口。登记本身不会建立任何连接，' +
          '也不影响已经存在的链接 —— 只有链接的目标主机匹配上这里登记的地址或名称时才会走隧道。' }),
        h('div', { class: 'row' }, [
          h('button', {
            class: 'btn sm accent', type: 'button',
            onclick: () => window.dispatchEvent(new CustomEvent('qlink:navigate', { detail: 'help' })),
          }, [icon('help', 14), '查看完整说明']),
        ]),
      ]),
      h('div', { class: 'card-body tight' }, [
        hosts.length
          ? table([
              { label: '名称' },
              { label: '地址' },
              { label: '上次探测', width: '190px' },
              { label: '', width: '190px' },
            ], hosts, hostRow)
          : h('div', { class: 'card-body' }, [
              emptyState('还没有远端主机', '添加后即可把别的设备上的服务映射到本机并放到桌面。', icon('server', 34)),
            ]),
      ]),
    ]),
  ]);
}

function hostRow(host) {
  const statusTag = host.checked_at
    ? h('span', { class: `tag ${host.last_ok ? 'ok' : 'danger'}` }, [
        icon(host.last_ok ? 'check' : 'alert', 12),
        host.last_ok ? '连通' : '不可达',
      ])
    : h('span', { class: 'tag', text: '未探测' });

  return h('tr', {}, [
    h('td', {}, [
      h('div', { class: 'cell-title', text: host.name || host.address }),
      h('div', { class: 'cell-sub' }, [
        host.user ? `${host.user}@` : '',
        host.auth?.key_path ? `密钥 ${host.auth.key_path}` : '默认密钥/agent',
        host.has_password ? ' · 已存密码' : '',
      ]),
    ]),
    h('td', {}, [h('div', { class: 'mono', text: host.address }), h('div', { class: 'cell-sub', text: `SSH ${host.port}` })]),
    h('td', {}, [
      h('div', { class: 'row' }, [statusTag]),
      h('div', { class: 'cell-sub', text: host.checked_at ? fmtRelative(host.checked_at) : '' }),
    ]),
    h('td', { class: 'actions' }, [
      h('div', { class: 'row end' }, [
        h('button', {
          class: 'btn sm accent', type: 'button',
          onclick: (e) => withBusy(e.currentTarget, async () => {
            try {
              const res = await api.hosts.probe(host.id);
              notify[res.ok ? 'ok' : 'error'](res.message);
              await loadHosts();
            } catch (err) {
              notify.error(err.message);
            }
          }),
        }, ['测试']),
        h('button', { class: 'btn ghost icon', type: 'button', title: '编辑', onclick: () => editHost(host) }, [icon('edit', 15)]),
        h('button', {
          class: 'btn ghost icon danger', type: 'button', title: '删除',
          onclick: async () => {
            const yes = await confirmDialog({
              title: '删除主机', message: `确定删除「${host.name || host.address}」吗？`,
              detail: '已创建的链接会保留，但不再走 SSH 隧道。', danger: true, confirmText: '删除',
            });
            if (!yes) return;
            try {
              await api.hosts.remove(host.id);
              notify.ok('已删除');
              await loadHosts();
            } catch (err) {
              notify.error(err.message);
            }
          },
        }, [icon('trash', 15)]),
      ]),
    ]),
  ]);
}

async function loadHosts() {
  try {
    const res = await api.hosts.list();
    patch({ hosts: res.items || [] });
    refresh();
  } catch (err) {
    if (err.status !== 401) console.warn('读取主机列表失败', err);
  }
}

async function editHost(host) {
  const model = {
    id: host?.id || '',
    name: host?.name || '',
    address: host?.address || '',
    port: host?.port || 22,
    user: host?.user || '',
    key_path: host?.auth?.key_path || '',
    password: '',
    insecure_skip_host_key: host?.auth?.insecure_skip_host_key ?? false,
  };

  const saved = await modal({
    title: host ? `编辑主机「${host.name || host.address}」` : '添加远程主机',
    size: 'wide',
    body: h('div', {}, [
      h('div', { class: 'form-grid' }, [
        field('名称', h('input', {
          type: 'text', value: model.name, placeholder: '例如 客厅 NAS',
          oninput: (e) => { model.name = e.target.value; },
        })),
        field('地址 *', h('input', {
          type: 'text', value: model.address, placeholder: '192.168.1.20 或 nas.local',
          oninput: (e) => { model.address = e.target.value; },
        })),
        field('SSH 端口', h('input', {
          type: 'number', value: model.port, min: '1', max: '65535',
          oninput: (e) => { model.port = Number(e.target.value) || 22; },
        })),
        field('用户名', h('input', {
          type: 'text', value: model.user, placeholder: 'root',
          oninput: (e) => { model.user = e.target.value; },
        })),
        field('私钥路径', h('input', {
          type: 'text', value: model.key_path, placeholder: '/root/.ssh/id_ed25519',
          oninput: (e) => { model.key_path = e.target.value; },
        }), '留空则使用默认密钥与 ssh-agent'),
        field('登录密码', h('input', {
          type: 'password', value: '',
          placeholder: host?.has_password ? '已保存，留空表示不修改' : '需要系统安装 sshpass',
          oninput: (e) => { model.password = e.target.value; },
        })),
      ]),
      h('div', { class: 'row mt-3' }, [
        h('label', { class: 'check' }, [
          h('input', {
            type: 'checkbox', checked: model.insecure_skip_host_key,
            onchange: (e) => { model.insecure_skip_host_key = e.target.checked; },
          }),
          h('span', { class: 'check-body' }, [
            h('span', { text: '跳过主机密钥校验' }),
            h('span', { class: 'hint', text: '仅在明确知道风险时开启；这会失去中间人攻击的防护' }),
          ]),
        ]),
      ]),
    ]),
    footer: (close) => [
      h('button', { class: 'btn', type: 'button', onclick: () => close(null) }, ['取消']),
      h('button', {
        class: 'btn primary', type: 'button',
        onclick: async (e) => {
          if (!model.address.trim()) { notify.warn('请填写地址'); return; }
          await withBusy(e.currentTarget, async () => {
            try {
              const result = await api.hosts.save({
                id: model.id,
                name: model.name.trim(),
                address: model.address.trim(),
                port: model.port,
                user: model.user.trim(),
                auth: {
                  password: model.password,
                  key_path: model.key_path.trim(),
                  insecure_skip_host_key: model.insecure_skip_host_key,
                },
              });
              notify.ok('已保存');
              close(result);
            } catch (err) {
              notify.error(err.message);
            }
          });
        },
      }, ['保存']),
    ],
  });

  if (saved) await loadHosts();
}

/* ------------------------------------------------------------------ 辅助 */

async function reloadLinksQuiet() {
  try {
    const res = await api.links.list();
    patch({ links: res.items || [] });
    window.dispatchEvent(new CustomEvent('qlink:links-updated'));
    refresh();
  } catch (err) {
    if (err.status !== 401) console.warn('刷新链接失败', err);
  }
}
