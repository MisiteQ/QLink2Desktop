/* api.js —— 后端接口封装
 *
 * 约定：
 *   - 所有失败都以 ApiError 抛出，携带 HTTP 状态码与后端返回的 kind，
 *     调用方据此分支（如 401 跳登录、400 显示在表单里）；
 *   - 只有这一处直接接触 fetch，接口路径变更时只需要改这个文件。
 */

export class ApiError extends Error {
  constructor(message, status = 0, kind = '') {
    super(message);
    this.name = 'ApiError';
    this.status = status;
    this.kind = kind;
  }
  get isUnauthorized() { return this.status === 401; }
}

/**
 * 判断一个错误是不是「没等到响应」，而不是「被服务端拒绝了」。
 *
 * 这两件事对用户的意义完全不同：被拒绝是真的失败了，可以直接报；
 * 而超时只说明"不知道结果是什么"。在受理型接口（保存 / 删除 / 对账）上，
 * 请求大概率已经成功执行了，只是响应没能按时回到浏览器。
 * 调用方遇到它应当**回查真实状态**，而不是把错误甩给用户。
 */
export function isTimeout(err) { return !!err && err.kind === 'timeout'; }

let unauthorizedHandler = null;
/** 注册 401 回调，由 app.js 用来弹出登录界面 */
export function onUnauthorized(fn) { unauthorizedHandler = fn; }

// 默认超时。
//
// 为什么必须有超时：浏览器原生的 fetch **没有超时**，只要服务端（或中间的
// 统一网关）不返回，Promise 就永远不 settle。真机上的表现是界面永久停在
// 「正在读取配置…」，Console 干净、Network 里那条请求一直 pending ——
// 既没有报错也没有线索，用户只能描述成"卡住了"。
// 有了超时，同样的故障会变成一句明确的错误文案 + 状态码，可被直接排查。
const DEFAULT_TIMEOUT_MS = 15000;

// 启动请求给更宽的窗口：服务刚起来时要做首轮对账，可能偏慢。
const BOOTSTRAP_TIMEOUT_MS = 25000;

// 写操作（新建 / 保存 / 删除 / 启用 / 同步 / 对账 / 清理）的超时预算。
//
// 后端对这类动作已经改成「受理即返回」，理论上毫秒级完成，但这里仍然给
// 一个明显宽于普通请求的窗口，理由有两条：
//   1. 请求要经过飞牛统一网关转发，偶发的转发延迟不该被算成失败；
//   2. 万一某条路径退回同步执行（例如后台队列满时的兜底），
//      15 秒的默认预算会立刻把「还在做」误报成「失败了」——
//      这正是之前那张「请求超时」截图的成因。
// 超时后调用方**必须**重新拉取列表核实，而不是直接把错误甩给用户。
export const MUTATION_TIMEOUT_MS = 45000;

async function request(method, path, body, options = {}) {
  const timeoutMs = options.timeout ?? DEFAULT_TIMEOUT_MS;
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), timeoutMs);
  // 调用方自己的取消信号也要生效（例如切换视图时取消上一次请求）。
  if (options.signal) {
    if (options.signal.aborted) controller.abort();
    else options.signal.addEventListener('abort', () => controller.abort(), { once: true });
  }

  const init = {
    method,
    credentials: 'same-origin',
    headers: { Accept: 'application/json' },
    signal: controller.signal,
  };

  if (body !== undefined && body !== null) {
    if (typeof FormData !== 'undefined' && body instanceof FormData) {
      init.body = body;
    } else {
      init.headers['Content-Type'] = 'application/json';
      init.body = JSON.stringify(body);
    }
  }

  let res;
  try {
    res = await fetch(path, init);
  } catch (err) {
    if (err && err.name === 'AbortError') {
      // 区分「我们主动超时」与「调用方取消」：前者要报给用户，后者是正常流程。
      if (options.signal && options.signal.aborted) throw err;
      throw new ApiError(`请求超时：${Math.round(timeoutMs / 1000)} 秒内没有响应（${path}）`, 0, 'timeout');
    }
    throw new ApiError(`无法连接到服务，请确认应用正在运行（${path}）`, 0, 'network');
  } finally {
    clearTimeout(timer);
  }

  if (res.status === 401) {
    if (unauthorizedHandler) unauthorizedHandler();
    throw new ApiError('登录已过期，请重新登录', 401, 'unauthorized');
  }

  const contentType = res.headers.get('content-type') || '';
  const isJSON = contentType.includes('application/json');

  if (!res.ok) {
    let message = `请求失败（HTTP ${res.status}）`;
    let kind = '';
    if (isJSON) {
      try {
        const payload = await res.json();
        if (payload && payload.error) message = payload.error;
        if (payload && payload.kind) kind = payload.kind;
      } catch { /* 响应体不是合法 JSON，保留默认文案 */ }
    }
    throw new ApiError(message, res.status, kind);
  }

  if (res.status === 204) return null;
  if (isJSON) return res.json();
  return res.text();
}

const get = (p, o) => request('GET', p, null, o);
const post = (p, b, o) => request('POST', p, b, o);
const put = (p, b, o) => request('PUT', p, b, o);
const del = (p, o) => request('DELETE', p, null, o);

const qs = (params) => {
  const s = new URLSearchParams();
  for (const [k, v] of Object.entries(params || {})) {
    if (v !== undefined && v !== null && v !== '') s.set(k, String(v));
  }
  const out = s.toString();
  return out ? `?${out}` : '';
};

export const api = {
  /* ---------------------------------------------------------- 系统基础 */
  // 健康检查是**无鉴权**端点（注册在公开路由上），所以重启等待期间拿它探活
  // 不会因为会话还没恢复而误判成失败。
  health: (o) => get('api/health', o),

  auth: {
    status: () => get('api/auth/status'),
    login: (password) => post('api/auth/login', { password }),
    logout: () => post('api/auth/logout', {}),
  },

  /* ---------------------------------------------------------- 启动合集 */
  // 一次请求拿全首屏数据。见后端 handleBootstrap 的注释：
  // 网关在部分版本上不能可靠地并发转发同一页面的多个并行请求，
  // 启动路径必须压缩成单请求，否则会悬在「正在读取配置…」。
  //
  // 这个响应同时带回 protected，所以启动时**不需要**再单独问一次
  // /api/auth/status —— 少一个请求就少一个可能悬住的点。
  bootstrap: (o) => get('api/bootstrap', { timeout: BOOTSTRAP_TIMEOUT_MS, ...(o || {}) }),

  /* -------------------------------------------------------------- 链接 */
  links: {
    list: () => get('api/links'),
    get: (id) => get(`api/links/${encodeURIComponent(id)}`),
    create: (payload, o) => post('api/links', payload, { timeout: MUTATION_TIMEOUT_MS, ...(o || {}) }),
    update: (id, payload, o) => put(`api/links/${encodeURIComponent(id)}`, payload, { timeout: MUTATION_TIMEOUT_MS, ...(o || {}) }),
    remove: (id, o) => del(`api/links/${encodeURIComponent(id)}`, { timeout: MUTATION_TIMEOUT_MS, ...(o || {}) }),
    setEnabled: (id, enabled, o) => post(`api/links/${encodeURIComponent(id)}/enabled`, { enabled }, { timeout: MUTATION_TIMEOUT_MS, ...(o || {}) }),
    sync: (id, o) => post(`api/links/${encodeURIComponent(id)}/sync`, {}, { timeout: MUTATION_TIMEOUT_MS, ...(o || {}) }),
    probe: (id, url) => post(`api/links/${encodeURIComponent(id)}/probe`, url ? { url } : {}),
    exportURL: () => 'api/links/export',
  },

  /* -------------------------------------------------------------- 设置 */
  settings: {
    get: () => get('api/settings'),
    update: (payload) => post('api/settings', payload),
  },

  /* ---------------------------------------------------------- 服务发现 */
  discovery: {
    ports: (force) => get(`api/discovery/ports${qs({ force: force ? 1 : '' })}`),
    subnets: () => get('api/discovery/subnets'),
    dockLabels: () => get('api/discovery/docklabels'),
    setDockLabel: (key, enabled) => post('api/discovery/docklabels/enable', { key, enabled }, { timeout: MUTATION_TIMEOUT_MS }),
    // 扫描是后台任务：POST 只负责开始（202），进度与结果靠 scanStatus 轮询。
    // 早先它是一个同步接口，一个 /24 网段要跑十几秒 —— 必然撞上 15 秒的
    // 默认预算，用户看到的是「请求超时：15 秒内没有响应（api/discovery/scan）」，
    // 而扫描其实还在后台跑，结果永远回不来。
    scan: (payload) => post('api/discovery/scan', payload, { timeout: MUTATION_TIMEOUT_MS }),
    scanStatus: () => get('api/discovery/scan'),
    cancelScan: () => del('api/discovery/scan', { timeout: MUTATION_TIMEOUT_MS }),
  },

  /* ---------------------------------------------------------- 远端主机 */
  hosts: {
    list: () => get('api/hosts'),
    save: (payload) => post('api/hosts', payload),
    remove: (id) => del(`api/hosts/${encodeURIComponent(id)}`),
    probe: (id) => post(`api/hosts/${encodeURIComponent(id)}/probe`, {}),
    ports: (id, ports) => get(`api/hosts/${encodeURIComponent(id)}/ports${qs({ ports })}`),
  },

  /* -------------------------------------------------------------- 图标 */
  icons: {
    list: () => get('api/icons'),
    upload: (file) => {
      const form = new FormData();
      form.append('file', file, file.name || 'icon.png');
      return request('POST', 'api/icons/upload', form);
    },
    remove: (name) => del(`api/icons/${encodeURIComponent(name)}`),
    url: (name) => `icons/${encodeURIComponent(name)}`,
    defaultURL: (size = 256) => `icons/default-${size}.png`,
    portalURL: (size = 256, nonce = '') => `icons/portal-${size}.png${nonce ? `?v=${nonce}` : ''}`,
  },

  /* -------------------------------------------------------------- 工具 */
  tools: {
    pickPort: () => get('api/tools/port'),
    probe: (url) => post('api/tools/probe', { url }),
  },

  /* -------------------------------------------------------------- 系统 */
  system: {
    info: () => get('api/system'),
    // 对账与清理都是受理型接口：返回 202 只代表"已提交"，
    // 真实结果要看每条链接的状态（后台会推 SSE 过来）。
    reconcile: () => post('api/system/reconcile', {}, { timeout: MUTATION_TIMEOUT_MS }),
    cleanupOrphans: () => post('api/system/cleanup-orphans', {}, { timeout: MUTATION_TIMEOUT_MS }),
    // 重启比上面两个更特殊：停止命令会终止执行它的进程，所以**连接断开才是
    // 正常结果**。这里仍然给一个宽窗口，但调用方必须把 timeout / network
    // 当成"已经开始了"而不是"失败了"，然后自己去探活。
    restart: () => post('api/system/restart', {}, { timeout: MUTATION_TIMEOUT_MS }),
    logs: (lines = 300) => get(`api/logs${qs({ lines })}`),
    downloadLogURL: () => 'api/logs/download',
    clientLog: (level, message, fields) => post('api/logs/client', { level, message, fields }),
  },
};

/**
 * 订阅服务端事件流（SSE）。
 *
 * EventSource 无法自定义请求头，因此令牌必须走查询参数；
 * 浏览器会自动带上会话 Cookie，所以正常情况下查询参数是空的。
 */
export function subscribeEvents({ onEvent, onOpen, onError }) {
  const source = new EventSource('api/events');

  const types = ['snapshot', 'links', 'status', 'log', 'discovery'];
  for (const type of types) {
    source.addEventListener(type, (ev) => {
      let data = null;
      try { data = JSON.parse(ev.data); } catch { data = null; }
      onEvent?.(type, data);
    });
  }

  source.onopen = () => onOpen?.();
  source.onerror = () => onError?.(source.readyState);
  return source;
}
