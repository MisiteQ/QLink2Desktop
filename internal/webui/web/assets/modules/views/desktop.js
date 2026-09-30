/* desktop.js —— 「桌面应用」视图
 *
 * 这是面板的主视图：列出所有链接，展示实时状态，并提供增删改查。
 */

import {
  h, append, clear, replace, notify, withBusy, confirmDialog,
  emptyState, fmtRelative, switchBox,
} from '../dom.js';
import { icon } from '../icons.js';
import { api, isTimeout } from '../api.js';
import { state, patch, visibleLinks, markBusy, isBusy } from '../store.js';
import { openLinkEditor } from './editor.js';

export const title = '桌面应用';
export const subtitle = () => {
  const total = state.links.length;
  const enabled = state.links.filter((v) => v.link.enabled).length;
  const failed = state.links.filter((v) => v.status.phase === 'failed').length;
  // 「处理中」必须可见：异步模型下这些项会在后台排队几分钟，
  // 没有这个计数的话，用户只能看到一行孤零零的「排队中」，分不清是卡住还是在干活。
  const busy = state.links.filter((v) => BUSY_PHASES.has(v.status.phase)).length;
  if (!total) return '还没有任何应用被放到桌面';
  const parts = [`共 ${total} 项`, `已启用 ${enabled}`];
  if (busy) parts.push(`处理中 ${busy}`);
  if (failed) parts.push(`失败 ${failed}`);
  return parts.join(' · ');
};

/** 仍处于「进行中」的阶段；与后端 domain.Phase.Busy() 保持一致。 */
const BUSY_PHASES = new Set(['pending', 'installing', 'upgrading']);

const PHASE_META = {
  unknown: { text: '未知', cls: '' },
  pending: { text: '排队中', cls: 'accent' },
  installing: { text: '处理中', cls: 'accent' },
  upgrading: { text: '升级中', cls: 'accent' },
  installed: { text: '已就绪', cls: 'ok' },
  stopped: { text: '已停用', cls: '' },
  failed: { text: '失败', cls: 'danger' },
};

const KIND_LABEL = { local: '本机端口', proxy: '端口映射', shortcut: '网址' };

/* ------------------------------------------------------------------ 工具栏 */

export function actions() {
  return [
    h('div', { class: 'segmented' }, ['all', 'enabled', 'disabled', 'failed'].map((value) => h('button', {
      type: 'button',
      class: state.filter === value ? 'active' : '',
      onclick: (e) => {
        for (const btn of e.currentTarget.parentNode.children) btn.classList.remove('active');
        e.currentTarget.classList.add('active');
        patch({ filter: value });
        refresh();
      },
    }, [{ all: '全部', enabled: '已启用', disabled: '已停用', failed: '失败' }[value]]))),

    h('a', {
      class: 'btn accent', href: api.links.exportURL(), download: '',
      title: '导出全部链接定义为 JSON',
    }, [icon('download', 15), '导出']),

    h('button', {
      class: 'btn primary', type: 'button',
      onclick: () => createLink(),
    }, [icon('plus', 15), '新建']),
  ];
}

/* -------------------------------------------------------------------- 渲染 */

let root = null;

export function mount(container) {
  root = container;
  draw();
}

export function refresh() {
  if (root) draw();
}

function draw() {
  if (!root) return;
  const items = visibleLinks();

  if (!state.links.length) {
    replace(root, h('div', { class: 'card' }, [
      h('div', { class: 'card-body' }, [
        emptyState(
          '还没有任何应用',
          '可以从「服务发现」里挑一个正在监听的端口，或直接新建一条链接。',
          icon('desktop', 40),
        ),
      ]),
      h('div', { class: 'card-body row', style: { justifyContent: 'center', paddingTop: 0 } }, [
        h('button', { class: 'btn primary', type: 'button', onclick: () => createLink() }, [
          icon('plus', 15), '新建第一个桌面应用',
        ]),
        h('button', {
          class: 'btn accent', type: 'button',
          onclick: () => window.dispatchEvent(new CustomEvent('qlink:navigate', { detail: 'discovery' })),
        }, [icon('activity', 15), '去服务发现看看']),
      ]),
    ]));
    return;
  }

  if (!items.length) {
    replace(root, h('div', { class: 'card' }, [
      h('div', { class: 'card-body' }, [
        emptyState('没有匹配项', '换个筛选条件或清空搜索词试试。', icon('filter', 36)),
      ]),
    ]));
    return;
  }

  const wrap = h('div', { class: 'card' }, [
    h('div', { class: 'table-wrap' }, [
      h('table', { class: 'tbl' }, [
        h('thead', {}, [h('tr', {}, [
          h('th', { text: '应用' }),
          h('th', { text: '类型', style: { width: '92px' } }),
          h('th', { text: '目标', style: { width: '238px' } }),
          h('th', { text: '状态', style: { width: '150px' } }),
          h('th', { text: '启用', style: { width: '62px' } }),
          h('th', { text: '', style: { width: '150px' } }),
        ])]),
        h('tbody', {}, items.map(row)),
      ]),
    ]),
  ]);
  replace(root, wrap);
}

function row(view) {
  const { link, status } = view;
  const meta = PHASE_META[status.phase] || PHASE_META.unknown;
  const busy = isBusy(link.id);

  const statusCell = h('div', {}, [
    h('span', { class: `tag ${meta.cls}` }, [
      busy ? h('span', { class: 'spinner-sm' }) : null,
      meta.text,
      status.detail && !busy ? ` ${status.detail}` : null,
    ]),
    status.last_error
      ? h('div', { class: 'cell-sub', text: status.last_error, title: status.last_error })
      : (link.enabled && status.proxy_port
          ? h('div', { class: 'cell-sub', text: `代理端口 ${status.proxy_port}` })
          : openHint(link)),
  ]);

  return h('tr', {}, [
    h('td', {}, [
      h('div', { class: 'cell-main' }, [
        h('img', {
          class: 'thumb', src: thumbSrc(link), alt: '',
          onerror: (e) => { e.currentTarget.src = api.icons.defaultURL(64); },
        }),
        h('div', { class: 'grow' }, [
          h('div', { class: 'cell-title truncate', title: link.name, text: link.name }),
          h('div', { class: 'cell-sub truncate' }, [
            link.desc ? link.desc : h('code', { text: link.app_name || link.id }),
          ]),
        ]),
      ]),
    ]),

    h('td', {}, [h('span', { class: 'tag' }, [KIND_LABEL[link.kind] || link.kind])]),

    h('td', {}, [
      h('div', { class: 'truncate mono', style: { fontSize: '.78rem' }, title: targetText(link), text: targetText(link) }),
      link.container?.name
        ? h('div', { class: 'cell-sub truncate', text: `容器 ${link.container.name}` })
        : null,
    ]),

    h('td', {}, [statusCell]),

    h('td', {}, [
      switchBox(link.enabled, async (checked) => {
        markBusy(link.id, true);
        refresh();
        try {
          const updated = await api.links.setEnabled(link.id, checked);
          const next = state.links.map((v) => (v.link.id === link.id ? updated : v));
          patch({ links: next });
          notify.ok(checked ? '已启用，正在同步到桌面' : '已停用，正在移除桌面图标');
        } catch (err) {
          if (isTimeout(err)) {
            await verifyToggle(link, checked);
          } else {
            notify.error(err.message);
          }
        } finally {
          markBusy(link.id, false);
          refresh();
        }
      }, { disabled: busy, title: link.enabled ? '点击停用' : '点击启用' }),
    ]),

    h('td', { class: 'actions' }, [
      h('div', { class: 'row end' }, [
        h('button', {
          class: 'btn ghost icon', type: 'button', title: '重新同步到桌面',
          disabled: busy,
          onclick: (e) => withBusy(e.currentTarget, async () => {
            markBusy(link.id, true);
            try {
              await api.links.sync(link.id);
              notify.ok('已提交同步，状态会自动更新');
            } catch (err) {
              // 同步是幂等的：超时不等于没生效，重新拉一次状态即可确认。
              if (isTimeout(err)) {
                await reloadLinks();
                notify.warn(`同步请求未收到确认，已刷新状态（${err.message}）`);
              } else {
                notify.error(err.message);
              }
            } finally {
              markBusy(link.id, false);
              refresh();
            }
          }),
        }, [icon('refresh', 15)]),

        h('button', {
          class: 'btn ghost icon', type: 'button', title: '编辑',
          disabled: busy,
          onclick: () => editLink(link),
        }, [icon('edit', 15)]),

        h('button', {
          class: 'btn ghost icon danger', type: 'button', title: '删除',
          disabled: busy,
          onclick: () => removeLink(link),
        }, [icon('trash', 15)]),
      ]),
    ]),
  ]);
}

/* ------------------------------------------------------------------ 行为 */

function thumbSrc(link) {
  const ic = link.icon || {};
  if (ic.source === 'url' && ic.ref) return ic.ref;
  if (ic.source === 'upload' && ic.ref) return api.icons.url(ic.ref);
  return api.icons.defaultURL(64);
}

function targetText(link) {
  if (link.kind === 'shortcut') return link.path || '';
  const scheme = link.scheme || 'http';
  const host = link.kind === 'local' ? 'localhost' : (link.host || '');
  const path = link.path && link.path !== '/' ? link.path : '';
  return `${scheme}://${host}:${link.port}${path}`;
}

// openHint 说明「点开这个桌面图标之后会发生什么」。
//
// 这一行存在的理由：三种形态对用户来说差别全在"点击之后"，
// 而它们在列表里长得几乎一样。与其让用户去试，不如直接把结论写在旁边。
function openHint(link) {
  if (!link.enabled) return null;
  if (link.kind === 'shortcut') {
    // 快捷方式没有本机端口，只能由飞牛的 CGI 入口 302 到目标网址 ——
    // 桌面入口的地址拼接规则决定了没法直接写外站主机，详见 ARCHITECTURE。
    return h('div', { class: 'cell-sub', text: '经飞牛跳转入口 302 到目标网址，不占本机端口' });
  }
  return null;
}

async function createLink(preset) {
  const result = await openLinkEditor(null, preset);
  if (!result) return;
  await Promise.allSettled([reloadLinks()]);
}

async function editLink(link) {
  const result = await openLinkEditor(link);
  if (!result) return;
  if (result.__delete) {
    await removeLink(link, true);
    return;
  }
  await reloadLinks();
}

async function removeLink(link, skipConfirm = false) {
  if (!skipConfirm) {
    const yes = await confirmDialog({
      title: '删除链接',
      message: `确定要删除「${link.name}」吗？`,
      detail: '对应的桌面图标会被注销；此操作不可撤销。',
      danger: true,
      confirmText: '删除',
    });
    if (!yes) return;
  }
  markBusy(link.id, true);
  refresh();
  try {
    await api.links.remove(link.id);
    patch({ links: state.links.filter((v) => v.link.id !== link.id) });
    notify.ok('已删除，桌面图标正在后台注销');
  } catch (err) {
    if (isTimeout(err)) {
      await verifyRemoved(link);
    } else {
      notify.error(err.message);
    }
  } finally {
    markBusy(link.id, false);
    refresh();
  }
}

/* ---------------------------------------------------------- 超时后的核实 */

/**
 * 删除请求超时后，重新拉列表确认这条链接到底还在不在。
 *
 * 为什么必须核实而不是直接报错：后端对删除是「先落盘、再后台注销图标」，
 * 请求几乎总是已经生效了，超时的往往只是响应回程（网关转发抖动、标签页
 * 被休眠…）。此时弹一句"请求超时"等于把一次成功的删除报成失败，
 * 用户多半会再点一次，然后撞上"链接不存在"的二次报错。
 *
 * 真实状态只有列表说了算——这也是整个异步模型下唯一可靠的判据。
 */
async function verifyRemoved(link) {
  try {
    const res = await api.links.list();
    const items = res.items || [];
    patch({ links: items });
    if (items.some((v) => v.link.id === link.id)) {
      notify.error(`删除未能确认：${link.name} 仍在列表中`);
    } else {
      notify.ok('已删除（响应超时，已回查确认）');
      window.dispatchEvent(new CustomEvent('qlink:links-updated'));
    }
  } catch (err2) {
    notify.error(`删除结果未知，请稍后刷新页面确认（${err2.message}）`);
  }
}

/** 启用开关超时后的核实：以列表里真实的 enabled 值判定成败。 */
async function verifyToggle(link, wanted) {
  try {
    const res = await api.links.list();
    const items = res.items || [];
    patch({ links: items });
    const now = items.find((v) => v.link.id === link.id);
    if (!now) {
      notify.warn('该链接已不存在，可能已被删除');
    } else if (now.link.enabled === wanted) {
      notify.ok(wanted ? '已启用（响应超时，已回查确认）' : '已停用（响应超时，已回查确认）');
    } else {
      notify.error('开关未能确认，请稍后再试');
    }
  } catch (err2) {
    notify.error(`开关状态未知，请稍后刷新页面确认（${err2.message}）`);
  }
}

/** 从后端重新拉取链接列表 */
export async function reloadLinks() {
  try {
    const res = await api.links.list();
    patch({ links: res.items || [] });
    refresh();
    window.dispatchEvent(new CustomEvent('qlink:links-updated'));
  } catch (err) {
    if (err.status !== 401) notify.error(`读取列表失败：${err.message}`);
  }
}
