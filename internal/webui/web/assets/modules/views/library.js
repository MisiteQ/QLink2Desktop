/* library.js —— 「图标库」视图
 *
 * 管理用户上传的自定义图标。上传后会自动转成 PNG 并保留原图尺寸，
 * 在生成飞牛子应用包时再按 64 / 256 两档缩放。
 */

import { h, replace, notify, confirmDialog, emptyState, fmtBytes, fmtRelative } from '../dom.js';
import { icon } from '../icons.js';
import { api } from '../api.js';
import { state, patch } from '../store.js';

export const title = '图标库';
export const subtitle = () => {
  const n = (state.icons || []).length;
  return n ? `已保存 ${n} 个自定义图标` : '上传图片后可在链接编辑器里选用';
};

export function actions() {
  const input = h('input', {
    type: 'file', accept: 'image/*', multiple: true, style: { display: 'none' },
    onchange: (e) => uploadFiles(Array.from(e.target.files || []), input),
  });
  return [
    input,
    h('a', { class: 'btn accent', href: api.icons.defaultURL(256), download: 'qlink2default.png', title: '下载内置默认图标' },
      [icon('download', 15), '默认图标']),
    h('button', {
      class: 'btn primary', type: 'button',
      onclick: () => input.click(),
    }, [icon('upload', 15), '上传图标']),
  ];
}

let root = null;

export function mount(container) {
  root = container;
  load();
}

export function refresh() {
  if (root) draw();
}

function draw() {
  if (!root) return;
  const items = state.icons || [];

  if (!items.length) {
    replace(root, h('div', { class: 'card' }, [
      h('div', { class: 'card-body' }, [
        emptyState('图标库是空的', '上传 PNG / JPEG / WebP，系统会自动转为 PNG 并裁成方形。', icon('image', 40)),
      ]),
    ]));
    return;
  }

  replace(root, h('div', { class: 'card' }, [
    h('div', { class: 'card-head' }, [
      h('div', {}, [
        h('h2', { text: '已上传的图标' }),
        h('p', { text: '点击图标可在新标签页查看原始尺寸' }),
      ]),
    ]),
    h('div', { class: 'card-body' }, [
      h('div', { class: 'icon-grid' }, items.map(tile)),
    ]),
  ]));
}

function tile(item) {
  return h('div', { class: 'icon-tile' }, [
    h('button', {
      class: 'btn ghost icon danger tile-del', type: 'button', title: '删除',
      onclick: async () => {
        const yes = await confirmDialog({
          title: '删除图标', message: `确定删除「${item.name}」吗？`,
          detail: '已引用该图标的链接会回退到内置默认图标。', danger: true, confirmText: '删除',
        });
        if (!yes) return;
        try {
          await api.icons.remove(item.name);
          notify.ok('已删除');
          await load();
        } catch (err) {
          notify.error(err.message);
        }
      },
    }, [icon('trash', 14)]),
    h('a', { href: item.url, target: '_blank', rel: 'noopener' }, [
      h('img', { src: item.url, alt: item.name, loading: 'lazy' }),
    ]),
    h('div', { class: 'name', title: item.name, text: item.name }),
    h('div', { class: 'name muted', text: `${fmtBytes(item.size)} · ${fmtRelative(item.mod_time)}` }),
  ]);
}

async function load() {
  try {
    const res = await api.icons.list();
    patch({ icons: res.items || [] });
  } catch (err) {
    if (err.status !== 401) notify.error(`读取图标失败：${err.message}`);
  }
  refresh();
}

async function uploadFiles(files, input) {
  if (!files.length) return;
  let ok = 0;
  for (const file of files) {
    try {
      await api.icons.upload(file);
      ok += 1;
    } catch (err) {
      notify.error(`${file.name} 上传失败：${err.message}`);
    }
  }
  // 清空 input，否则连续选同一个文件不会触发 change。
  input.value = '';
  if (ok) notify.ok(`已上传 ${ok} 个图标`);
  await load();
}
