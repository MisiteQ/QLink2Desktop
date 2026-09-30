/* store.js —— 极简应用状态
 *
 * 只用「一个可变对象 + 订阅回调」：
 * 这个面板的数据规模是几十条链接，不需要不可变更新或细粒度依赖追踪。
 * 保持简单反而让数据流更容易推理：谁改了 state，谁就该负责刷新自己的视图。
 */

export const state = {
  /** 首屏快照是否已经拿到 */
  ready: false,
  /** 版本号 */
  version: '',
  /** 登录状态 */
  auth: { protected: false, loggedIn: true },

  /** 链接（定义 + 运行态） */
  links: [],
  /** 全局设置 */
  settings: null,
  /** 系统概览 */
  system: null,

  /** 当前视图 */
  view: 'desktop',
  /** 已经渲染过的视图（用于判断是否需要 mount） */
  mounted: new Set(),

  /** 列表筛选 */
  filter: 'all',
  search: '',

  /** 正在执行中的链接 ID */
  busy: new Set(),
  /** 实时通道状态：connecting / live / offline / restarting */
  live: 'connecting',

  /** 日志缓冲（来自 SSE） */
  logs: [],
  logPaused: false,

  /** 发现结果 */
  discovery: { ports: [], docker: false, loading: false, at: 0 },
  dockLabels: [],
  hosts: [],
  icons: [],

  /**
   * 网络扫描。
   *
   * 扫描任务的状态必须放在全局 state 里，而不是某个视图的闭包里 ——
   * 它跑在后台、耗时几十秒，期间任何一次重绘（SSE 推送、切页签、
   * 窗口重新可见）都会重建视图。状态留在闭包里就等于进度和结果全丢。
   */
  scan: {
    /** 本机网络环境快照（网段 / 上级网段 / 网关） */
    info: null,
    /** 目标选择：auto | parent | custom */
    mode: 'auto',
    /** 自定义范围文本 */
    spec: '',
    /** 最近一次拿到的任务快照 */
    job: null,
    /** 提交请求是否在进行中 */
    starting: false,
    /** 错误信息（页面内展示，不用 toast —— 扫描失败时用户正盯着这块界面） */
    error: '',
  },
};

const listeners = new Set();

/** 订阅状态变化；返回取消订阅的函数 */
export function subscribe(fn) {
  listeners.add(fn);
  return () => listeners.delete(fn);
}

/** 合并式更新并通知订阅者 */
export function patch(changes) {
  Object.assign(state, changes);
  notify();
}

/** 强制通知（就地修改嵌套对象后调用） */
export function notify() {
  for (const fn of listeners) {
    try {
      fn(state);
    } catch (err) {
      console.error('状态订阅者执行失败', err);
    }
  }
}

/* ------------------------------------------------------------ 便捷读取 */

export function linkByID(id) {
  return state.links.find((v) => v.link && v.link.id === id) || null;
}

export function isBusy(id) {
  return state.busy.has(id);
}

export function markBusy(id, busy) {
  if (busy) state.busy.add(id);
  else state.busy.delete(id);
  notify();
}

/** 按筛选条件取出链接 */
export function visibleLinks() {
  const keyword = state.search.trim().toLowerCase();
  return state.links.filter((view) => {
    const link = view.link || {};
    if (state.filter === 'enabled' && !link.enabled) return false;
    if (state.filter === 'disabled' && link.enabled) return false;
    if (state.filter === 'failed' && (view.status?.phase || '') !== 'failed') return false;
    if (!keyword) return true;
    const haystack = [
      link.name, link.desc, link.host, link.path, link.app_name,
      link.kind, String(link.port || ''),
      link.container?.name, link.container?.image,
    ].filter(Boolean).join(' ').toLowerCase();
    return haystack.includes(keyword);
  });
}

/** 追加日志行，保持固定长度 */
export function pushLog(line) {
  if (state.logPaused) return;
  state.logs.push(line);
  if (state.logs.length > 800) state.logs.splice(0, state.logs.length - 800);
  if (logSink) {
    try {
      logSink(line);
    } catch (err) {
      console.error('日志渲染回调失败', err);
    }
  }
}

/**
 * 日志视图注册的追加回调。
 *
 * 用一个回调而不是让日志视图订阅全局状态，是因为日志是高频事件：
 * 每秒几条到几十条，走全局状态通知会连带刷新整个页面。
 */
let logSink = null;
export function setLogSink(fn) { logSink = fn; }

