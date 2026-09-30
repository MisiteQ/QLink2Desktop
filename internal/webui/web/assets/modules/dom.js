/* dom.js —— 极简 DOM 工具与通用交互组件
 *
 * 为什么不用框架：本面板的交互集中在「列表 + 弹窗表单」两类，
 * 手写渲染函数比引入框架更容易看清数据流向，也省掉一整套构建链。
 */

/** 创建元素：h('div', { class: 'x', onclick: fn }, [子节点…]) */
export function h(tag, props = {}, children = []) {
  const el = document.createElement(tag);
  for (const [key, value] of Object.entries(props || {})) {
    if (value === null || value === undefined || value === false) continue;
    if (key === 'class' || key === 'className') {
      el.className = value;
    } else if (key === 'text') {
      el.textContent = String(value);
    } else if (key === 'html') {
      el.innerHTML = value;
    } else if (key === 'dataset') {
      Object.assign(el.dataset, value);
    } else if (key === 'style' && typeof value === 'object') {
      Object.assign(el.style, value);
    } else if (key.startsWith('on') && typeof value === 'function') {
      el.addEventListener(key.slice(2).toLowerCase(), value);
    } else if (key in el && key !== 'list' && key !== 'type') {
      el[key] = value;
    } else {
      el.setAttribute(key, value === true ? '' : String(value));
    }
  }
  append(el, children);
  return el;
}

/** 追加任意形态的子节点（数组会被展平，null/false 会被忽略） */
export function append(parent, children) {
  const list = Array.isArray(children) ? children : [children];
  for (const child of list) {
    if (child === null || child === undefined || child === false) continue;
    if (Array.isArray(child)) {
      append(parent, child);
    } else if (child instanceof Node) {
      parent.appendChild(child);
    } else {
      parent.appendChild(document.createTextNode(String(child)));
    }
  }
  return parent;
}

export function clear(node) {
  while (node.firstChild) node.removeChild(node.firstChild);
  return node;
}

export function replace(node, children) {
  clear(node);
  append(node, children);
  return node;
}

export const $ = (sel, root = document) => root.querySelector(sel);
export const $$ = (sel, root = document) => Array.from(root.querySelectorAll(sel));

/** 用 SVG 字符串生成元素（图标统一走这里） */
export function svgFrom(markup) {
  const wrap = document.createElement('div');
  wrap.innerHTML = markup.trim();
  return wrap.firstElementChild;
}

/* -------------------------------------------------------------- 文本格式 */

export function fmtTime(value) {
  if (!value) return '—';
  const d = value instanceof Date ? value : new Date(value);
  if (Number.isNaN(d.getTime())) return '—';
  const pad = (n) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ` +
         `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
}

export function fmtRelative(value) {
  if (!value) return '从未';
  const d = value instanceof Date ? value : new Date(value);
  if (Number.isNaN(d.getTime())) return '从未';
  const diff = Date.now() - d.getTime();
  if (diff < 0) return '刚刚';
  const sec = Math.floor(diff / 1000);
  if (sec < 45) return '刚刚';
  const min = Math.floor(sec / 60);
  if (min < 60) return `${min} 分钟前`;
  const hour = Math.floor(min / 60);
  if (hour < 24) return `${hour} 小时前`;
  const day = Math.floor(hour / 24);
  if (day < 30) return `${day} 天前`;
  return fmtTime(d).slice(0, 10);
}

export function fmtBytes(n) {
  if (!n) return '0 B';
  const units = ['B', 'KB', 'MB', 'GB'];
  let i = 0, v = n;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return `${v >= 100 || i === 0 ? Math.round(v) : v.toFixed(1)} ${units[i]}`;
}

export function fmtMs(ms) {
  if (ms === null || ms === undefined) return '—';
  if (ms < 1000) return `${ms} ms`;
  return `${(ms / 1000).toFixed(2)} s`;
}

/** 把任意字符串转义为可安全放进 innerHTML 的文本 */
export function escapeHTML(s) {
  return String(s ?? '').replace(/[&<>"']/g, (c) => (
    { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]
  ));
}

/* ------------------------------------------------------------------ 提示 */

const TOAST_TTL = { ok: 2600, info: 3200, warn: 5000, error: 8000 };

/** 右下角浮层提示；返回一个可手动关闭的函数 */
export function toast(message, options = {}) {
  const kind = options.kind || 'info';
  const root = $('#toasts');
  if (!root) return () => {};

  const box = h('div', { class: `toast ${kind}` });
  const body = h('div', { class: 'toast-body' }, [
    options.title ? h('div', { class: 'toast-title', text: options.title }) : null,
    h('div', { text: String(message ?? '') }),
  ]);
  const closeBtn = h('button', {
    class: 'toast-close', type: 'button', title: '关闭',
    onclick: () => dismiss(),
  }, ['×']);

  append(box, [body, closeBtn]);
  root.appendChild(box);

  let timer = null;
  const ttl = options.ttl ?? TOAST_TTL[kind] ?? 3200;
  if (ttl > 0) timer = setTimeout(dismiss, ttl);

  function dismiss() {
    if (timer) { clearTimeout(timer); timer = null; }
    if (box.parentNode) box.parentNode.removeChild(box);
  }
  return dismiss;
}

export const notify = {
  ok: (msg, opts) => toast(msg, { ...opts, kind: 'ok' }),
  info: (msg, opts) => toast(msg, { ...opts, kind: 'info' }),
  warn: (msg, opts) => toast(msg, { ...opts, kind: 'warn' }),
  error: (msg, opts) => toast(msg, { ...opts, kind: 'error', ttl: 9000 }),
};

/* ------------------------------------------------------------------ 弹窗 */

/**
 * 打开一个弹窗。
 *
 * body 可以是节点，也可以是 (close) => 节点的函数；
 * foot 同理，用来放「取消 / 保存」这类按钮。
 */
export function modal(options = {}) {
  const root = $('#modal-root');
  const backdrop = h('div', { class: 'modal-backdrop' });
  const panel = h('div', { class: `modal ${options.size || ''}` });

  let resolveValue = null;
  let settled = false;

  function close(value) {
    if (settled) return;
    settled = true;
    document.removeEventListener('keydown', onKey);
    if (backdrop.parentNode) backdrop.parentNode.removeChild(backdrop);
    if (resolveValue) resolveValue(value);
  }
  function onKey(e) {
    if (e.key === 'Escape' && options.dismissible !== false) {
      e.stopPropagation();
      close(null);
    }
  }

  // dismissible:false 时连右上角的 × 也一并去掉。
  //
  // 早期版本只关掉了 Esc 与点背景，× 仍然能把这个"不该关掉"的弹窗关掉 ——
  // 重启等待层就因此可以被用户手滑关掉，剩下一个不知道服务有没有回来的界面。
  // 既然调用方说了不可关闭，就得三条出口全部堵住。
  const head = h('div', { class: 'modal-head' }, [
    h('h3', { text: options.title || '' }),
    options.dismissible === false ? null : h('button', {
      class: 'btn ghost icon', type: 'button', title: '关闭（Esc）',
      onclick: () => close(null),
    }, [closeIcon()]),
  ]);

  const bodyNode = h('div', { class: 'modal-body' });
  append(bodyNode, typeof options.body === 'function' ? options.body(close) : options.body);

  append(panel, [head, bodyNode]);
  if (options.footer) {
    const footNode = h('div', { class: 'modal-foot' });
    append(footNode, typeof options.footer === 'function' ? options.footer(close) : options.footer);
    append(panel, footNode);
  }

  append(backdrop, panel);
  if (options.dismissible !== false) {
    backdrop.addEventListener('mousedown', (e) => { if (e.target === backdrop) close(null); });
  }
  document.addEventListener('keydown', onKey);
  root.appendChild(backdrop);

  // 自动聚焦第一个可输入的控件，省掉一次点击。
  const focusable = panel.querySelector('input:not([type=hidden]):not([disabled]), textarea, select');
  if (focusable) setTimeout(() => focusable.focus(), 30);

  const promise = new Promise((res) => { resolveValue = res; });
  promise.close = close;
  return promise;
}

/** 确认对话框，返回 true / false */
export async function confirmDialog(options = {}) {
  const result = await modal({
    title: options.title || '请确认',
    size: 'narrow',
    body: h('div', {}, [
      h('p', { style: { margin: '0 0 6px' }, text: options.message || '' }),
      options.detail ? h('p', { class: 'muted', style: { margin: 0, fontSize: '.82rem' }, text: options.detail }) : null,
    ]),
    footer: (close) => [
      h('button', { class: 'btn', type: 'button', onclick: () => close(false) }, ['取消']),
      h('button', {
        class: `btn ${options.danger ? 'danger' : 'primary'}`,
        type: 'button',
        onclick: () => close(true),
      }, [options.confirmText || '确定']),
    ],
  });
  return result === true;
}

/* ---------------------------------------------------------------- 小构件 */

export function closeIcon() {
  return svgFrom('<svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M18 6 6 18M6 6l12 12"/></svg>');
}

export function switchBox(checked, onChange, opts = {}) {
  const input = h('input', {
    type: 'checkbox',
    checked: !!checked,
    disabled: !!opts.disabled,
    onchange: (e) => onChange(e.target.checked),
  });
  return h('label', { class: 'switch', title: opts.title || '' }, [input, h('span', { class: 'slider' })]);
}

export function field(label, control, hint) {
  return h('div', { class: 'field' }, [
    label ? h('label', { text: label }) : null,
    control,
    hint ? h('span', { class: 'hint', text: hint }) : null,
  ]);
}

export function emptyState(title, message, iconNode) {
  return h('div', { class: 'empty' }, [
    iconNode ? h('div', { class: 'empty-icon' }, [iconNode]) : null,
    h('h3', { text: title }),
    message ? h('p', { text: message }) : null,
  ]);
}

/** 表格骨架：传列定义与行渲染函数 */
export function table(columns, rows, renderRow) {
  const thead = h('thead', {}, [
    h('tr', {}, columns.map((c) => h('th', { text: c.label, style: c.width ? { width: c.width } : {} }))),
  ]);
  const tbody = h('tbody', {}, rows.map(renderRow));
  return h('div', { class: 'table-wrap' }, [h('table', { class: 'tbl' }, [thead, tbody])]);
}

/** 给按钮加上「执行中」状态，避免重复点击 */
export async function withBusy(button, task) {
  if (!button) return task();
  if (button.dataset.busy === '1') return;
  const original = button.innerHTML;
  button.dataset.busy = '1';
  button.disabled = true;
  button.innerHTML = '<span class="spinner-sm"></span>';
  try {
    return await task();
  } finally {
    button.dataset.busy = '0';
    button.disabled = false;
    button.innerHTML = original;
  }
}

/** 防抖 */
export function debounce(fn, wait = 250) {
  let t = null;
  return (...args) => {
    if (t) clearTimeout(t);
    t = setTimeout(() => { t = null; fn(...args); }, wait);
  };
}
