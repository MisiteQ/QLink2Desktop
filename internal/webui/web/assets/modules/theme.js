/* theme.js —— 外观主题：明暗模式 × 色系
 *
 * 两个正交的选择：
 *   模式 mode   —— light（白天）/ dark（暗黑）/ auto（跟随系统）
 *   色系 accent —— blue / teal / green / violet / orange / rose
 *
 * 落地方式：把解析后的结果写到 <html> 的两个 data 属性上，
 * 由 style.css 用 [data-theme=...] / [data-accent=...] 选取变量：
 *
 *   <html data-theme="dark" data-accent="violet">
 *
 * 注意 data-theme 上写的**永远是解析后的 light / dark**，不是 auto ——
 * CSS 只认两种底，不必（也无法）在样式表里再处理"跟随系统"。
 *
 * 存哪里：localStorage，不进后端 settings。两个理由：
 *
 *   1. 主题必须在**首帧之前**生效。后端配置要发一次请求才拿得到，
 *      注定赶不上第一次绘制，用户会看到"先闪一下白底再变黑"；
 *      localStorage 可以被 <head> 里的同步脚本直接读到（见 index.html）。
 *   2. 它是"这台设备 / 这个浏览器"的偏好，不是服务的配置：
 *      手机上想用暗色、客厅里想用浅色，本来就不该互相覆盖。
 *
 * 因此 index.html 里有一段极短的同步脚本也读同一个键，
 * 两边必须保持一致（e2e 有静态守卫盯着键名相同）。
 */

/**
 * localStorage 键。
 *
 * 改这个字符串时，index.html 内联脚本里的同名字面量必须一起改 ——
 * 否则会出现「首帧按默认值画、进了应用才跳成用户选的主题」的闪烁。
 */
export const STORAGE_KEY = 'qlink2d.theme';

/** 模式选项（顺序即设置页里的展示顺序） */
export const MODES = [
  { value: 'light', label: '白天' },
  { value: 'dark', label: '暗黑' },
  { value: 'auto', label: '跟随系统' },
];

/**
 * 色系选项。
 *
 * swatch 只用于设置页里的色卡预览 —— 它是"这个色系长什么样"的示例色，
 * 真正的颜色由 style.css 里的 --a-h / --a-s / --a-l 派生，两者要视觉相近。
 */
export const ACCENTS = [
  { value: 'blue', label: '晴空蓝', swatch: '#2563eb' },
  { value: 'teal', label: '湖水青', swatch: '#0d9488' },
  { value: 'green', label: '新芽绿', swatch: '#16a34a' },
  { value: 'violet', label: '紫罗兰', swatch: '#7c3aed' },
  { value: 'orange', label: '暖阳橙', swatch: '#ea580c' },
  { value: 'rose', label: '玫瑰红', swatch: '#e11d48' },
];

export const DEFAULT_THEME = { mode: 'auto', accent: 'blue' };

const VALID_MODES = new Set(MODES.map((m) => m.value));
const VALID_ACCENTS = new Set(ACCENTS.map((a) => a.value));

/* ------------------------------------------------------------ 读取 / 存储 */

/** 读取当前主题偏好；损坏的数据一律退回默认值，不让界面卡在半套主题上。 */
export function loadTheme() {
  const fallback = { ...DEFAULT_THEME };
  let raw = null;
  try {
    raw = localStorage.getItem(STORAGE_KEY);
  } catch {
    return fallback; // 隐私模式等场景下 localStorage 可能直接抛错
  }
  if (!raw) return fallback;

  let parsed;
  try {
    parsed = JSON.parse(raw);
  } catch {
    return fallback;
  }
  if (!parsed || typeof parsed !== 'object') return fallback;
  return {
    mode: VALID_MODES.has(parsed.mode) ? parsed.mode : fallback.mode,
    accent: VALID_ACCENTS.has(parsed.accent) ? parsed.accent : fallback.accent,
  };
}

export function saveTheme(theme) {
  try {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(theme));
  } catch {
    /* 存不下就算了：主题不是关键数据，不能因为它让操作失败 */
  }
}

/* ---------------------------------------------------------------- 解析 */

/** 系统是否偏好深色 */
export function systemPrefersDark() {
  try {
    return !!window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches;
  } catch {
    return false;
  }
}

/** 把 auto 解析成实际的 light / dark */
export function resolveMode(mode) {
  if (mode === 'dark' || mode === 'light') return mode;
  return systemPrefersDark() ? 'dark' : 'light';
}

/** 当前生效的（已解析的）模式 */
export function effectiveMode() {
  return resolveMode(loadTheme().mode);
}

/* ---------------------------------------------------------------- 应用 */

/**
 * 写入主题。
 *
 * @param {{mode:string, accent:string}} theme 未提供的字段沿用当前值
 * @param {{persist?:boolean}} options persist=false 用于「只预览不改存档」的场合
 */
export function applyTheme(theme, options = {}) {
  const next = {
    mode: VALID_MODES.has(theme?.mode) ? theme.mode : loadTheme().mode,
    accent: VALID_ACCENTS.has(theme?.accent) ? theme.accent : loadTheme().accent,
  };
  const root = document.documentElement;
  const resolved = resolveMode(next.mode);

  root.dataset.theme = resolved;
  root.dataset.accent = next.accent;

  // 让浏览器原生控件（滚动条、下拉、日期选择）也跟着变。
  const meta = document.querySelector('meta[name="color-scheme"]');
  if (meta) meta.setAttribute('content', resolved === 'dark' ? 'dark light' : 'light dark');

  if (options.persist !== false) saveTheme(next);
  return next;
}

/** 应用存档里的主题（启动时调用一次） */
export function initTheme() {
  return applyTheme(loadTheme(), { persist: false });
}

/**
 * 主题变化事件。
 *
 * 为什么需要它：主题有**两处入口**（顶栏的明暗按钮、设置页的色卡），
 * 而这两处**本身都要显示当前主题**（按钮图标、选中的色卡）。
 * 早期实现里 applyTheme 只写 `<html>` 与 localStorage，不通知任何人 ——
 * 于是每个入口各自负责"重画自己"，另一处就停在旧状态：
 * 在设置页切了暗黑，顶栏的月亮图标不变；点顶栏按钮，设置页的 segmented
 * 还停在「白天」。看起来就像"点了没反应"。
 *
 * 现在统一由事件驱动：入口只管改状态，重画交给 app.js 的监听器统一做。
 * 加新的主题入口时**不要**自己重画，发事件就行。
 */
export const THEME_EVENT = 'qlink:theme-changed';

/**
 * 写入主题并通知全应用重画。
 *
 * 与 applyTheme 的区别只有一个：是否发事件。启动阶段（initTheme）
 * 刻意用 applyTheme，因为那时界面还没建好，也没有任何东西需要重画。
 */
export function setTheme(theme) {
  const next = applyTheme(theme);
  window.dispatchEvent(new CustomEvent(THEME_EVENT));
  return next;
}

/**
 * 在白天 / 暗黑之间切换。
 *
 * 当前是 auto 时按**此刻解析出来的结果**取反，并顺手把模式固定成显式值 ——
 * 用户点了按钮就说明他对系统给的结果不满意，再继续跟随只会显得没反应。
 */
export function toggleMode() {
  const current = loadTheme();
  const next = resolveMode(current.mode) === 'dark' ? 'light' : 'dark';
  return setTheme({ ...current, mode: next });
}

/** 切换色系 */
export function setAccent(accent) {
  return setTheme({ ...loadTheme(), accent });
}

/* ------------------------------------------------------------ 系统变化 */

/**
 * 订阅系统深色偏好的变化。
 *
 * 只有在 mode=auto 时才需要真的做点什么；显式选了白天/暗黑的用户
 * 不该被系统设置改动影响。返回一个取消订阅的函数。
 */
export function watchSystemTheme(onChange) {
  if (!window.matchMedia) return () => {};
  let mq;
  try {
    mq = window.matchMedia('(prefers-color-scheme: dark)');
  } catch {
    return () => {};
  }
  const handler = () => {
    if (loadTheme().mode !== 'auto') return;
    applyTheme(loadTheme(), { persist: false });
    if (typeof onChange === 'function') onChange();
  };
  if (mq.addEventListener) mq.addEventListener('change', handler);
  else if (mq.addListener) mq.addListener(handler); // 老版本 Chromium
  return () => {
    if (mq.removeEventListener) mq.removeEventListener('change', handler);
    else if (mq.removeListener) mq.removeListener(handler);
  };
}
