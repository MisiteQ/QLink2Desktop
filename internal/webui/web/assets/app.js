/* app.js —— 应用入口
 *
 * 职责：
 *   1. 启动流程（健康检查 → 登录判定 → 拉取首屏数据）；
 *   2. 侧边导航与视图切换；
 *   3. 建立 SSE 长连接，把服务端事件分发到对应视图。
 *
 * 刻意不做路由框架：五六个视图用 hash 就够了，
 * 而且飞牛桌面里是 iframe 打开，复杂的 history 路由反而容易出问题。
 */

import { h, clear, replace, $, withBusy, debounce } from './modules/dom.js';
import { icon } from './modules/icons.js';
import { api, onUnauthorized, subscribeEvents } from './modules/api.js';
import { state, patch, pushLog } from './modules/store.js';
import {
  initTheme, watchSystemTheme, toggleMode, loadTheme, resolveMode, THEME_EVENT,
} from './modules/theme.js';

import * as desktop from './modules/views/desktop.js';
import * as discovery from './modules/views/discovery.js';
import * as library from './modules/views/library.js';
import * as settings from './modules/views/settings.js';
import * as system from './modules/views/system.js';
import * as help from './modules/views/help.js';

const VIEWS = { desktop, discovery, library, settings, system, help };

const NAV = [
  { key: 'desktop', label: '桌面应用', iconName: 'desktop', badge: () => state.links.length },
  { key: 'discovery', label: '服务发现', iconName: 'activity', badge: () => state.discovery.ports.length },
  { key: 'library', label: '图标库', iconName: 'image', badge: () => state.icons.length },
  { key: 'settings', label: '设置', iconName: 'gear' },
  { key: 'system', label: '系统与日志', iconName: 'terminal' },
  // 帮助放在最后一格：它不参与日常操作，但需要时必须在同一个列表里找得到，
  // 而不是藏在某个角落的链接里。
  { key: 'help', label: '使用帮助', iconName: 'help' },
];

let eventSource = null;

/* ------------------------------------------------------------------ 启动 */

async function main() {
  // 主题要在**任何绘制之前**定下来。
  //
  // index.html 里的同步脚本已经按 localStorage 抢在首帧前写过一次
  // data-theme / data-accent，这里再调一次是为了把「跟随系统」的结果
  // 与 meta[color-scheme] 对齐，并接上后续的系统偏好监听。
  initTheme();
  bindGlobalEvents();
  bindBootRetry();
  await bootstrap();
}

/** 把启动页上的「重试」按钮接上：整页重载是最可靠的重试方式。 */
function bindBootRetry() {
  const btn = $('#boot-retry');
  if (btn) btn.addEventListener('click', () => window.location.reload());
}

async function bootstrap() {
  bootMessage('正在读取配置与桌面应用…');
  bootDetail('');

  try {
    // 单请求拿全首屏数据（links + settings + system + protected）。
    //
    // 这里刻意**不再**单独请求 /api/auth/status：bootstrap 已经把 protected
    // 一起带回来了。少一个往返就少一个可能悬住的点，而"请求悬住"正是真机上
    // 最难查的一类故障 —— 控制台干净、没有报错，用户只能描述成"卡住了"。
    const res = await api.bootstrap();
    patch({
      links: res.links?.items || res.links || [],
      settings: res.settings,
      system: res.system,
      auth: { protected: !!res.protected, loggedIn: true },
      ready: true,
    });
  } catch (err) {
    if (err.status === 401) { showLogin(); return; }
    bootFailed(err, '启动请求');
    return;
  }

  // 渲染阶段单独兜底。
  //
  // 这一点很关键：早期版本只把请求包在 try 里，之后 showShell / navigate
  // 抛出的异常会变成「未被处理的 Promise 拒绝」—— 界面永久停在
  // 「正在读取配置与桌面应用…」，而控制台里只有一条很容易被忽略的
  // unhandled rejection。那种故障最难排查，因为用户能提供的只有"卡住了"。
  try {
    showShell();
    connectEvents();
    navigate(location.hash.replace('#', '') || 'desktop', false);
    loadLogsQuietly();
  } catch (err) {
    bootFailed(err, '界面渲染');
  }
}

// bootFailed 把启动失败的原因明确摆到界面上。
//
// 早期版本只把一句文案改掉然后就没有下文了，用户能反馈的只有"一直卡着"。
// 现在把失败阶段、状态码、错误类型都显示出来并给一个重试按钮，
// 故障从"不可观测"变成"可以直接描述"。
function bootFailed(err, phase = '启动') {
  const status = err && err.status ? `HTTP ${err.status}` : '无响应';
  const kind = (err && err.kind) || (err && err.name) || 'unknown';

  // 若此时已经切到登录界面（登录后再启动失败就是这种情况），
  // 必须把启动页重新显示出来，否则错误信息根本没有地方可看。
  const boot = $('#boot');
  if (boot) boot.hidden = false;
  const app = $('#app');
  if (app) app.hidden = true;
  clear($('#modal-root'));

  bootMessage(`启动失败：${(err && err.message) || err}`);
  bootDetail(`阶段 ${phase} · ${status} · ${kind}`);

  const spinner = $('.boot-spinner');
  if (spinner) spinner.hidden = true;
  const retry = $('#boot-retry');
  if (retry) retry.hidden = false;
}

function bootMessage(text) {
  const el = $('#boot-message');
  if (el) el.textContent = text;
}

function bootDetail(text) {
  const el = $('#boot-detail');
  if (!el) return;
  el.textContent = text || '';
  el.hidden = !text;
}

/* ------------------------------------------------------------------ 外壳 */

function showShell() {
  const boot = $('#boot');
  if (boot) boot.hidden = true;
  const app = $('#app');
  if (app) app.hidden = false;

  const version = $('#brand-version');
  if (version && state.system) version.textContent = `v${state.system.version || '—'}`;

  renderNav();
  renderTopbar();
  updatePips();
}

function renderNav() {
  const nav = $('#nav');
  if (!nav) return;
  replace(nav, NAV.map((item) => {
    const badge = item.badge ? item.badge() : 0;
    return h('button', {
      class: `nav-item ${state.view === item.key ? 'active' : ''}`,
      type: 'button',
      dataset: { view: item.key },
      onclick: () => navigate(item.key),
    }, [
      h('span', { class: 'nav-icon' }, [icon(item.iconName, 17)]),
      h('span', { text: item.label }),
      badge ? h('span', { class: 'nav-badge', text: String(badge) }) : null,
    ]);
  }));
}

function renderTopbar() {
  const view = VIEWS[state.view];
  if (!view) return;

  const titleEl = $('#page-title');
  const subEl = $('#page-subtitle');
  if (titleEl) titleEl.textContent = view.title || '';
  if (subEl) subEl.textContent = typeof view.subtitle === 'function' ? (view.subtitle() || '') : '';

  const actions = $('#topbar-actions');
  if (!actions) return;
  const items = typeof view.actions === 'function' ? view.actions() : [];

  // 明暗切换常驻顶栏：临时想换底色时不必先绕去设置页。
  //
  // 图标显示的是"点下去会变成什么"（暗色时显示太阳），而不是当前状态 ——
  // 这是开关类控件最不容易搞反的约定。
  //
  // 这里**不自己重画**：改完状态后由 qlink:theme-changed 的监听器统一重画，
  // 否则设置页那份（也是主题的一个入口）会停在旧状态，两处不同步。
  const darkNow = resolveMode(loadTheme().mode) === 'dark';
  items.push(h('button', {
    class: 'btn ghost icon', type: 'button',
    title: darkNow ? '切换到白天模式' : '切换到暗黑模式',
    onclick: () => toggleMode(),
  }, [icon(darkNow ? 'sun' : 'moon', 16)]));

  // 帮助入口对**每个**视图都在，包括自己没有 actions 的那些。
  //
  // 说明写在帮助页里还不够：用户是在"正要操作却不知道这一步会怎样"的
  // 那一刻需要它，而那一刻他多半在某个具体页面上。侧边栏常驻一格 + 顶栏
  // 就近一个按钮，两条路都要有。
  if (state.view !== 'help') {
    items.push(h('button', {
      class: 'btn ghost icon', type: 'button', title: '使用帮助',
      onclick: () => navigate('help'),
    }, [icon('help', 16)]));
  }

  // 搜索框只在「桌面应用」视图出现：其余视图的筛选需求各不相同。
  if (state.view === 'desktop') {
    const input = h('input', {
      type: 'search', value: state.search, placeholder: '搜索名称、端口、容器…',
      style: { width: '200px' },
      oninput: debounce((e) => {
        patch({ search: e.target.value });
        VIEWS.desktop.refresh();
        renderTopbar();
      }, 200),
    });
    items.unshift(h('div', { class: 'search', style: { maxWidth: '240px' } }, [icon('search', 14), input]));
  }

  replace(actions, items);
}

/* ------------------------------------------------------------------ 导航 */

function navigate(key, updateHash = true) {
  if (!VIEWS[key]) key = 'desktop';
  const changed = state.view !== key;
  state.view = key;
  state.mounted.add(key);

  if (updateHash && location.hash !== `#${key}`) {
    history.replaceState(null, '', `#${key}`);
  }

  renderNav();
  renderTopbar();

  const content = $('#content');
  if (!content) return;
  content.scrollTop = 0;

  // 每次都重新 mount：视图内部只保存纯数据，重建代价远低于维护增量更新逻辑，
  // 也彻底避免了「切走再切回来看到的是过期数据」。
  VIEWS[key].mount(content);
  renderSidebarState();
  void changed;
}

function renderSidebarState() {
  const pips = { 'pip-cli': cliStatus(), 'pip-live': liveStatus() };
  for (const [id, status] of Object.entries(pips)) {
    const node = document.getElementById(id);
    if (!node) continue;
    node.classList.remove('on', 'off', 'warn');
    if (status.tone) node.classList.add(status.tone);
    const text = node.querySelector('.pip-text');
    if (text) text.textContent = status.text;
  }
}

function cliStatus() {
  const info = state.system;
  if (!info) return { tone: '', text: '环境未知' };
  if (info.appcenter_cli) return { tone: 'on', text: '应用中心可用' };
  return { tone: 'warn', text: '模拟模式' };
}

function liveStatus() {
  switch (state.live) {
    case 'live': return { tone: 'on', text: '实时已连接' };
    case 'offline': return { tone: 'off', text: '实时已断开' };
    // 重启是我们自己发起的，断连是**预期结果**，不能显示成故障。
    case 'restarting': return { tone: 'warn', text: '正在重启…' };
    default: return { tone: 'warn', text: '正在连接…' };
  }
}

function updatePips() {
  renderSidebarState();
}

/* ------------------------------------------------------------- 事件流 */

function connectEvents() {
  if (eventSource) eventSource.close();

  patch({ live: 'connecting' });
  updatePips();

  eventSource = subscribeEvents({
    onOpen: () => {
      patch({ live: 'live' });
      updatePips();
    },
    onError: () => {
      patch({ live: 'offline' });
      updatePips();
    },
    onEvent: (type, payload) => {
      switch (type) {
        case 'snapshot':
          applySnapshot(payload);
          break;
        case 'links':
          // 定义发生变化：重新拉取列表（比在事件里传全量更省流，也不会漏字段）。
          void desktop.reloadLinks().then(() => renderNav());
          break;
        case 'status':
          applySnapshot(payload);
          break;
        case 'log':
          if (typeof payload === 'string') {
            pushLog(payload);
            if (state.view === 'system') system.onLogLine(payload);
          }
          break;
        case 'discovery':
          if (state.view === 'discovery') discovery.refresh();
          break;
        default:
          break;
      }
    },
  });
}

function applySnapshot(payload) {
  if (!payload) return;
  const changes = {};
  if (Array.isArray(payload.links)) changes.links = payload.links;
  if (payload.settings) changes.settings = payload.settings;
  if (payload.system) changes.system = payload.system;
  if (!Object.keys(changes).length) return;

  patch(changes);
  renderNav();
  renderTopbar();
  renderSidebarState();
  if (VIEWS[state.view]) VIEWS[state.view].refresh();
}

async function loadLogsQuietly() {
  try {
    const res = await api.system.logs(200);
    state.logs.length = 0;
    for (const line of res.lines || []) state.logs.push(line);
  } catch {
    /* 日志拉取失败不影响主流程 */
  }
}

/* ------------------------------------------------------------- 登录界面 */

function showLogin(message) {
  const boot = $('#boot');
  if (boot) boot.hidden = true;
  const app = $('#app');
  if (app) app.hidden = true;

  const root = $('#modal-root');
  clear(root);

  const password = h('input', {
    type: 'password', autocomplete: 'current-password', placeholder: '请输入访问口令',
    onkeydown: (e) => { if (e.key === 'Enter') submit(); },
  });
  const errorBox = h('div', { class: 'muted', style: { minHeight: '18px', color: 'var(--danger)' } });
  if (message) errorBox.textContent = message;

  const button = h('button', {
    class: 'btn primary', type: 'button', style: { width: '100%' },
    onclick: () => submit(),
  }, ['登录']);

  async function submit() {
    const value = password.value;
    if (!value) { errorBox.textContent = '请输入口令'; return; }
    await withBusy(button, async () => {
      try {
        await api.auth.login(value);
        clear(root);
        // bootstrap 会把 auth 状态一起带回来，不必再单独问一次
        // /api/auth/status —— 少一个请求，少一个可能悬住的点。
        await bootstrap();
      } catch (err) {
        errorBox.textContent = err.message;
        password.select();
      }
    });
  }

  const backdrop = h('div', { class: 'modal-backdrop' }, [
    h('div', { class: 'modal narrow' }, [
      h('div', { class: 'modal-body' }, [
        h('div', { style: { textAlign: 'center', marginBottom: '16px' } }, [
          h('img', {
            src: api.icons.defaultURL(64), alt: '',
            style: { width: '52px', height: '52px', borderRadius: '14px', margin: '0 auto 10px' },
          }),
          h('h3', { style: { margin: '0 0 4px' }, text: 'QLink2Desktop' }),
          h('p', { class: 'muted', style: { margin: 0, fontSize: '.82rem' }, text: '该面板已启用口令保护' }),
        ]),
        h('div', { class: 'field' }, [password]),
        errorBox,
      ]),
      h('div', { class: 'modal-foot' }, [button]),
    ]),
  ]);

  root.appendChild(backdrop);
  setTimeout(() => password.focus(), 50);
}

/* --------------------------------------------------------------- 全局 */

function bindGlobalEvents() {
  onUnauthorized(() => showLogin('登录已过期，请重新登录'));

  // 主题的唯一重画出口。
  //
  // 顶栏的明暗按钮与设置页的色卡都是主题入口，两处都要反映当前主题；
  // 各画各的必然不同步（在设置页切暗黑 → 顶栏图标不变）。所以统一在这里
  // 重画：顶栏（切换按钮图标）+ 当前视图（设置页的 segmented / 选中的色卡）。
  window.addEventListener(THEME_EVENT, () => {
    renderTopbar();
    if (VIEWS[state.view]) VIEWS[state.view].refresh();
  });

  // 系统深色偏好变化（只有 mode=auto 的用户会被影响，见 theme.js）。
  // 走同一条通道，避免"系统切了主题但设置页没跟着变"。
  watchSystemTheme(() => window.dispatchEvent(new CustomEvent(THEME_EVENT)));

  window.addEventListener('hashchange', () => {
    const key = location.hash.replace('#', '');
    if (key && key !== state.view) navigate(key, false);
  });

  window.addEventListener('qlink:navigate', (e) => navigate(e.detail));
  window.addEventListener('qlink:relogin', () => {
    patch({ auth: { protected: true, loggedIn: false } });
    if (eventSource) { eventSource.close(); eventSource = null; }
    showLogin('请重新登录');
  });
  window.addEventListener('qlink:links-updated', () => {
    renderNav();
    renderTopbar();
    if (state.view === 'desktop') desktop.refresh();
  });

  // 重启是我们主动发起的：接下来 EventSource 必然报错重连，那不是故障。
  // 先把它关掉，别让浏览器在后台疯狂重试；服务回来后由整页重载重建连接。
  window.addEventListener('qlink:restarting', () => {
    if (eventSource) { eventSource.close(); eventSource = null; }
    patch({ live: 'restarting' });
    updatePips();
  });

  // 页面重新可见时补一次状态：长时间挂在后台的标签页可能已经错过若干事件。
  document.addEventListener('visibilitychange', () => {
    if (document.visibilityState !== 'visible') return;
    if (!state.ready) return;
    if (state.live === 'offline') connectEvents();
    if (VIEWS[state.view]) VIEWS[state.view].refresh();
  });

  // 未捕获的错误也必须能在启动阶段被"看见"。
  //
  // 启动过程中的异常（尤其是渲染阶段的）如果只打到控制台，用户看到的就是
  // 一句永远不动的「正在读取配置…」，能反馈的信息为零。
  // 这里在首屏尚未就绪时把它抬到启动页上，让失败变成可描述的事实。
  window.addEventListener('error', (e) => {
    if (e && e.message) console.error('前端未捕获错误', e.message);
    if (!state.ready) {
      bootFailed(e?.error || new Error((e && e.message) || '未知脚本错误'), '脚本错误');
    }
  });

  window.addEventListener('unhandledrejection', (e) => {
    const reason = e && e.reason;
    console.error('前端未处理的 Promise 拒绝', reason);
    if (!state.ready) {
      bootFailed(reason instanceof Error ? reason : new Error(String(reason)), '未处理的异步异常');
    }
  });
}

main();
