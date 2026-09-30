/* system.js —— 「系统」视图
 *
 * 左边是运行环境概览（用于回答「为什么某个功能不可用」），
 * 右边是实时日志（用于回答「刚才到底发生了什么」）。
 */

import { h, clear, replace, notify, withBusy, fmtTime, modal, confirmDialog } from '../dom.js';
import { icon } from '../icons.js';
import { api } from '../api.js';
import { state, patch, setLogSink } from '../store.js';

export const title = '系统';
export const subtitle = () => {
  const info = state.system;
  if (!info) return '';
  return `版本 ${info.version || '—'} · 已运行 ${info.uptime || '—'}`;
};

export function actions() {
  return [
    h('button', {
      class: 'btn accent', type: 'button',
      onclick: (e) => withBusy(e.currentTarget, () => reload()),
    }, [icon('refresh', 15), '刷新']),
    restartButton('btn warn'),
  ];
}

let root = null;
let logBox = null;
let followTail = true;

export function mount(container) {
  root = container;
  // 日志走独立通道：它是高频事件，不能每次都触发整页重绘。
  setLogSink(appendLogLine);
  draw();
}

export function refresh() {
  if (root) draw();
}

/** 由 SSE 推送触发 */
export function onLogLine(line) {
  appendLogLine(line);
}

function draw() {
  if (!root) return;
  const info = state.system || {};

  replace(root, [
    h('div', { class: 'grid cols-4 mb-3' }, [
      stat('appcenter_cli' in info ? (info.appcenter_cli ? '飞牛应用中心' : '模拟模式') : '—',
        info.appcenter_cli ? '可用' : '未检测到', info.appcenter_cli ? 'ok' : 'warn', 'server'),
      stat('代理监听', String(info.proxy_routes ?? 0), '个路由', 'accent', 'link'),
      stat('桌面应用', `${info.enabled_links ?? 0}/${info.links ?? 0}`, '已启用 / 总数', 'ok', 'desktop'),
      // 后台队列深度：异步模型下「排队中」能不能自愈，全靠这个数字可被观测。
      // 它长期不为 0 就说明有任务卡住了（而不是界面坏了）。
      stat('后台队列', String(info.pending_jobs ?? 0), '件待处理',
        (info.pending_jobs ?? 0) > 0 ? 'accent' : '', 'clock'),
      stat('实时连接', String(info.sse_subscribers ?? 0), '个订阅者', '', 'zap'),
    ]),

    h('div', { class: 'grid cols-2' }, [
      capabilityCard(info),
      runtimeCard(info),
    ]),

    serviceCard(info),
    installedCard(info),
    logCard(),
  ]);

  // 重建日志容器后补齐已有内容。
  if (logBox && state.logs.length) {
    for (const line of state.logs) appendLogLine(line, true);
    scrollLog();
  }
}

function stat(label, value, unit, tone, iconName) {
  return h('div', { class: 'stat' }, [
    h('div', { class: 'stat-label' }, [icon(iconName, 13), label]),
    h('div', { class: 'stat-value' }, [
      value,
      unit ? h('small', { text: unit }) : null,
    ]),
    tone ? h('div', { class: 'mt-2' }, [h('span', { class: `tag ${tone}` }, [toneText(tone)])]) : null,
  ]);
}

function toneText(tone) {
  return { ok: '正常', warn: '注意', accent: '运行中', danger: '异常' }[tone] || '';
}

function capabilityCard(info) {
  const rows = [
    ['飞牛应用中心命令行', info.appcenter_cli
      ? { text: info.cli_path || '已检测到', tone: 'ok' }
      : { text: '未检测到，仅构建安装包而不注册', tone: 'warn' }],
    ['Docker 守护进程', info.docker
      ? { text: '可用，可读取容器与端口映射', tone: 'ok' }
      : { text: '不可用，容器相关功能已隐藏', tone: '' }],
    ['ssh 客户端', info.ssh
      ? { text: '可用，支持 SSH 隧道', tone: 'ok' }
      : { text: '未找到，远程主机只能直连', tone: 'warn' }],
    ['访问口令', info.password_protected
      ? { text: `已启用，当前 ${info.sessions ?? 0} 个会话`, tone: 'ok' }
      : { text: '未启用，局域网内可自由访问', tone: 'warn' }],
    ['默认存储卷', info.default_volume ? { text: `卷 ${info.default_volume}`, tone: '' } : { text: '未知', tone: '' }],
  ];

  return h('div', { class: 'card' }, [
    h('div', { class: 'card-head' }, [
      h('div', {}, [h('h2', { text: '运行能力' }), h('p', { text: '这些开关直接决定哪些功能可用' })]),
    ]),
    h('div', { class: 'card-body tight' }, [
      h('div', { class: 'table-wrap' }, [
        h('table', { class: 'tbl' }, [
          h('tbody', {}, rows.map(([label, status]) => h('tr', {}, [
            h('td', { style: { width: '42%' }, text: label }),
            h('td', {}, [h('span', { class: `tag ${status.tone}`, text: status.text })]),
          ]))),
        ]),
      ]),
    ]),
  ]);
}

function runtimeCard(info) {
  const rows = [
    ['版本', info.version || '—'],
    ['已运行', info.uptime || '—'],
    ['启动于', info.started_at ? fmtTime(info.started_at) : '—'],
    ['Go 运行时', info.go_version || '—'],
    ['平台', `${info.os || '—'} / ${info.arch || '—'}`],
    ['数据目录', info.data_dir || '—'],
    ['日志目录', info.log_dir || '—'],
    ['代理端口起点', String(info.port_base ?? '—')],
  ];
  return h('div', { class: 'card' }, [
    h('div', { class: 'card-head' }, [h('div', {}, [h('h2', { text: '运行环境' })])]),
    h('div', { class: 'card-body tight' }, [
      h('div', { class: 'table-wrap' }, [
        h('table', { class: 'tbl' }, [
          h('tbody', {}, rows.map(([k, v]) => h('tr', {}, [
            h('td', { class: 'muted', style: { width: '42%' }, text: k }),
            h('td', { class: 'mono truncate', title: String(v), text: String(v) }),
          ]))),
        ]),
      ]),
    ]),
  ]);
}

/* -------------------------------------------------------------- 服务控制 */

/** 「重启服务」按钮：顶栏与「服务控制」卡片共用同一个构件。
 *
 * 飞牛应用中心命令行不可用（模拟模式）时直接置灰 —— 重启靠的就是
 * appcenter-cli 的 stop/start，与其让用户点了之后收到一句报错，
 * 不如在按下去之前就把原因说清楚。
 */
function restartButton(cls) {
  const cli = !!(state.system && state.system.appcenter_cli);
  return h('button', {
    class: cls,
    type: 'button',
    disabled: !cli,
    title: cli ? '停止并重新启动本应用服务' : '未检测到飞牛应用中心命令行，无法重启',
    onclick: () => restartService(),
  }, [icon('power', 15), '重启服务']);
}

function serviceCard(info) {
  const cli = !!info.appcenter_cli;
  return h('div', { class: 'card' }, [
    h('div', { class: 'card-head' }, [
      h('div', {}, [
        h('h2', { text: '服务控制' }),
        h('p', { text: '重启本应用自身，用于让端口与代理等运行态彻底重建' }),
      ]),
      h('span', { class: `tag ${cli ? 'ok' : 'warn'}`, text: cli ? '可重启' : '模拟模式' }),
    ]),
    h('div', { class: 'card-body' }, [
      h('div', { class: 'row', style: { alignItems: 'flex-start', justifyContent: 'space-between' } }, [
        h('div', { class: 'muted', style: { fontSize: '.82rem', lineHeight: '1.8' } }, [
          h('div', { text: '· 桌面图标与全部配置都在数据目录里，重启不会丢' }),
          h('div', { text: '· 重启期间已建立的代理连接会中断，服务回来后自动恢复' }),
          h('div', { text: '· 服务恢复后页面会自动重新加载，不必手动刷新' }),
        ]),
        restartButton('btn warn'),
      ]),
    ]),
  ]);
}

/** 重启等待的轮询间隔与总预算 */
const RESTART_POLL_MS = 2000;
const RESTART_WAIT_MS = 120000;

async function restartService() {
  const yes = await confirmDialog({
    title: '重启本应用服务？',
    message: '服务会先停止再启动，期间本页面会短暂断开。',
    detail: '桌面图标与配置都不会受影响；通常十几秒内恢复，恢复后页面会自动重新加载。',
    confirmText: '重启',
    danger: true,
  });
  if (!yes) return;

  // 先通知外壳：这是**我们主动**停掉的服务，接下来 SSE 的断开属于预期结果。
  // 否则它会被当成故障，界面上立刻飘一个「实时已断开」，而实际上一切正常。
  window.dispatchEvent(new CustomEvent('qlink:restarting'));

  try {
    await api.system.restart();
  } catch (err) {
    if (err && err.status === 401) return;                   // 登录过期交给全局流程
    // 只有被服务端**明确拒绝**（4xx，例如当前运行模式不支持）才需要报给用户。
    if (err && err.status >= 400 && err.status < 500) {
      notify.error(`重启未受理：${err.message}`);
      return;
    }
    // 超时 / 连不上 / 网关 5xx —— 全都说明进程已经在重启了。
    //
    // 这一点是这类功能最容易做错的地方：停止命令会杀掉执行它的进程，
    // 请求本来就等不到响应。把一次**故意的**断开报成"失败"，
    // 用户会以为重启没成功，然后再点一次。
  }
  waitForServiceBack();
}

/** 弹出一个不可关闭的等待层，轮询 /api/health，服务一回来就整页重载。 */
function waitForServiceBack() {
  const started = Date.now();
  const detail = h('p', {
    class: 'muted', style: { margin: '8px 0 0', fontSize: '.82rem' },
    text: '正在等待服务重新上线…',
  });
  const manual = h('button', {
    class: 'btn sm accent', type: 'button', hidden: true,
    onclick: () => window.location.reload(),
  }, ['手动刷新']);

  modal({
    title: '服务正在重启',
    size: 'narrow',
    dismissible: false,
    body: h('div', { style: { textAlign: 'center', padding: '4px 0 2px' } }, [
      h('div', { class: 'boot-spinner', style: { margin: '0 auto 14px' } }),
      h('p', { style: { margin: 0 }, text: '重启指令已受理，服务停止后本页会短暂断开' }),
      detail,
      h('div', { class: 'mt-3' }, [manual]),
    ]),
  });

  // 为什么要**连续两次**探活成功才重载：
  // 进程刚起来的那一瞬间，飞牛统一网关可能还没把 socket 转发接回去 ——
  // 此刻 health 能通、紧跟着的 bootstrap 却会吃到 502，页面于是重载进
  // 「启动失败」界面。真机上确实观察到过重启后接口短暂 502。
  // 多花两秒换掉这个窗口，比让用户看到一张假的失败页划算得多。
  let healthyStreak = 0;

  const timer = setInterval(async () => {
    const elapsed = Math.round((Date.now() - started) / 1000);
    try {
      await api.health({ timeout: 3000 });
      healthyStreak++;
      if (healthyStreak >= 2) {
        clearInterval(timer);
        detail.textContent = '服务已恢复，正在重新加载页面…';
        // 进程是全新的：链接状态、日志缓冲、SSE 连接全部失效。整页重载
        // 比增量修复省事得多，也不会留下一个半新半旧的界面。
        setTimeout(() => window.location.reload(), 500);
        return;
      }
      detail.textContent = `服务已恢复，正在确认与飞牛网关的连接（${healthyStreak}/2）…`;
      return;
    } catch {
      healthyStreak = 0; // 一次不通就从头数
    }

    detail.textContent = `已等待 ${elapsed} 秒…`;
    if (Date.now() - started > RESTART_WAIT_MS) {
      clearInterval(timer);
      detail.textContent = `已等待 ${elapsed} 秒仍未恢复，请到飞牛应用中心查看应用状态。`;
      manual.hidden = false;
    }
  }, RESTART_POLL_MS);
}

function installedCard(info) {
  const apps = info.installed_apps || [];
  return h('div', { class: 'card' }, [
    h('div', { class: 'card-head' }, [
      h('div', {}, [
        h('h2', { text: '飞牛应用中心中由本项目创建的图标' }),
        h('p', { text: '只列出本项目管理命名空间（qlink2d. 及历史前缀）下的包，不会展示其它原生应用' }),
      ]),
      h('span', { class: 'tag', text: `${apps.length} 个` }),
    ]),
    h('div', { class: 'card-body' }, [
      apps.length
        ? h('div', { class: 'row' }, apps.map((name) => h('span', { class: 'tag mono', text: name })))
        : h('p', { class: 'muted', text: info.appcenter_cli ? '当前没有由本项目创建的桌面图标。' : '未检测到应用中心命令行，无法列出。' }),
    ]),
  ]);
}

function logCard() {
  logBox = h('div', { class: 'log-view' });
  const autoscroll = h('input', { type: 'checkbox', checked: true, onchange: (e) => { followTail = e.target.checked; } });

  logBox.addEventListener('scroll', () => {
    // 用户手动往上翻时暂停自动滚动，回到底部再恢复 ——
    // 否则一边看历史日志一边被新日志拽走，非常难用。
    const atBottom = logBox.scrollHeight - logBox.scrollTop - logBox.clientHeight < 24;
    if (atBottom !== followTail) {
      followTail = atBottom;
      autoscroll.checked = atBottom;
    }
  });

  return h('div', { class: 'card' }, [
    h('div', { class: 'card-head' }, [
      h('div', {}, [
        h('h2', { text: '实时日志' }),
        h('p', { text: '服务端日志会实时推送；这里只保留最近 800 行' }),
      ]),
      h('div', { class: 'row' }, [
        h('label', { class: 'check', style: { fontSize: '.78rem' } }, [autoscroll, h('span', { text: '自动滚动' })]),
        h('button', {
          class: 'btn sm warn', type: 'button',
          onclick: () => { state.logs.length = 0; if (logBox) clear(logBox); },
        }, ['清空']),
        h('a', { class: 'btn sm accent', href: api.system.downloadLogURL(), download: '' }, [icon('download', 14), '下载']),
        h('button', {
          class: 'btn sm accent', type: 'button',
          onclick: (e) => withBusy(e.currentTarget, () => loadLogs()),
        }, ['重新加载']),
      ]),
    ]),
    h('div', { class: 'card-body' }, [logBox]),
  ]);
}

function appendLogLine(line, silent = false) {
  if (!logBox) return;
  const cls = /\sERROR\s/.test(line) ? 'log-line err' : (/\sWARN\s/.test(line) ? 'log-line warn' : '');
  logBox.appendChild(h('div', { class: cls, text: line }));
  // 控制行数，避免长时间挂着页面把内存吃满。
  while (logBox.childElementCount > 800) logBox.removeChild(logBox.firstChild);
  if (followTail || silent === 'force') scrollLog();
}

function scrollLog() {
  if (logBox && followTail) {
    logBox.scrollTop = logBox.scrollHeight;
  }
}

async function loadLogs() {
  try {
    const res = await api.system.logs(400);
    state.logs.length = 0;
    for (const line of res.lines || []) state.logs.push(line);
    if (logBox) {
      clear(logBox);
      for (const line of state.logs) appendLogLine(line, true);
    }
    followTail = true;
    scrollLog();
  } catch (err) {
    if (err.status !== 401) notify.error(`读取日志失败：${err.message}`);
  }
}

async function reload() {
  try {
    const info = await api.system.info();
    patch({ system: info });
  } catch (err) {
    if (err.status !== 401) notify.error(err.message);
  }
  draw();
  await loadLogs();
}
