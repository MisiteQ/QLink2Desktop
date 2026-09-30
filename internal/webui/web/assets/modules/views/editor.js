/* editor.js —— 链接编辑器（新建 / 编辑弹窗）
 *
 * 这是全站最复杂的表单：链接有三种形态，各自需要完全不同的字段。
 * 组织方式是「外壳固定 + 按形态重建中段」，避免用一个巨型 DOM 靠显隐切换 ——
 * 后者很容易出现「切到快捷方式后，上一个形态的端口还留在提交数据里」这类脏数据问题。
 */

import { h, append, replace, clear, field, notify, switchBox, withBusy, modal } from '../dom.js';
import { icon } from '../icons.js';
import { api, isTimeout } from '../api.js';
import { state } from '../store.js';

const KIND_META = {
  local: { label: '本机端口', hint: '服务就运行在这台设备上，桌面图标直连本机端口' },
  proxy: { label: '端口映射', hint: '服务在别的设备或公网，由本机反向代理后上桌面' },
  shortcut: { label: '网址快捷方式', hint: '桌面图标经飞牛跳转入口 302 到该网址，不占本机端口' },
};

/**
 * 打开链接编辑器。
 * @param {object|null} existing 现有 Link（编辑模式），null 表示新建
 * @param {object} preset 新建时的预填值（例如从端口列表点进来）
 * @returns {Promise<object|null>} 保存后的视图对象，取消则返回 null
 */
export function openLinkEditor(existing = null, preset = {}) {
  const isEdit = !!(existing && existing.id);

  // 表单数据是一个普通对象：所有控件都直接读写它，
  // 提交时整体转成后端 DTO，不需要在 DOM 里回捞值。
  const model = {
    name: existing?.name || preset.name || '',
    desc: existing?.desc || '',
    kind: existing?.kind || preset.kind || 'local',
    app_name: existing?.app_name || '',
    scheme: existing?.scheme || preset.scheme || 'http',
    host: existing?.host || preset.host || '',
    port: existing?.port || preset.port || 0,
    path: existing?.path || preset.path || '/',
    ui: existing?.ui || 'iframe',
    all_users: existing?.all_users ?? true,
    no_display: existing?.no_display ?? false,
    skip_tls_verify: existing?.skip_tls_verify ?? false,
    file_types: existing?.file_types || [],
    icon: { ...(existing?.icon || { source: '', ref: '', text: '', text_color: '#ffffff', bg_color: '#2563eb' }) },
    container: { ...(existing?.container || {}) },
    enabled: existing?.enabled ?? true,
  };
  if (!model.path) model.path = '/';

  return modal({
    title: isEdit ? `编辑「${existing.name}」` : '新建桌面应用',
    size: 'wide',
    body: (close) => {
      const root = h('div', { class: 'grid' });
      root.style.gap = '16px';

      /* ---------------------------------------------------- 基本信息 */
      const nameInput = h('input', {
        type: 'text', value: model.name, placeholder: '显示在飞牛桌面上的名称',
        oninput: (e) => { model.name = e.target.value; },
      });
      const descInput = h('input', {
        type: 'text', value: model.desc, placeholder: '可选，显示在应用详情里',
        oninput: (e) => { model.desc = e.target.value; },
      });

      append(root, h('div', { class: 'form-grid' }, [
        field('名称 *', nameInput),
        field('说明', descInput, isEdit ? `应用标识：${existing.app_name || existing.id}` : null),
      ]));

      /* -------------------------------------------------------- 类型 */
      const kindSeg = segmented(Object.entries(KIND_META).map(([value, meta]) => ({
        value,
        label: meta.label,
      })), model.kind, (value) => {
        model.kind = value;
        buildKindSection();
      });

      append(root, h('div', { class: 'field' }, [
        h('label', { text: '类型' }),
        kindSeg,
        h('span', { class: 'hint', text: KIND_META[model.kind].hint, id: 'kind-hint' }),
      ]));

      /* ------------------------------------------------ 形态相关字段 */
      const kindBox = h('div');
      append(root, kindBox);

      const buildKindSection = () => {
        clear(kindBox);
        const hint = root.querySelector('#kind-hint');
        if (hint) hint.textContent = KIND_META[model.kind].hint;

        if (model.kind === 'shortcut') {
          append(kindBox, h('div', { class: 'form-grid' }, [
            field('完整网址 *', h('input', {
              type: 'text', value: model.path, placeholder: 'https://example.com/',
              oninput: (e) => { model.path = e.target.value; },
            }), '不带 http:// 时会自动按 https 处理'),
            field('图标文件名类型', fileTypesInput(model), '可选，用于桌面「打开方式」的关联'),
          ]));
          append(kindBox, h('p', { class: 'muted mt-2', style: { fontSize: '.78rem' }, text:
            '桌面图标会先落到飞牛的一个跳转入口，由它立刻把你送到目标网址。' +
            '这是飞牛桌面入口的唯一可行形态：入口地址里的主机名永远是飞牛自己，' +
            '没法直接写别的主机；能跳到站外的只有这条 302 通路。' }));
          append(kindBox, h('p', { class: 'muted mt-2', style: { fontSize: '.78rem' }, text:
            '注意：外网经飞牛 Connect 访问时，跳转链路依赖飞牛的中继服务，' +
            '若中继异常可能打不开 —— 此时用浏览器直接访问目标网址即可。' }));
          return;
        }

        const portInput = h('input', {
          type: 'number', min: '1', max: '65535', value: model.port || '',
          placeholder: '例如 8080',
          oninput: (e) => { model.port = Number(e.target.value) || 0; },
        });
        const pathInput = h('input', {
          type: 'text', value: model.path, placeholder: '/',
          oninput: (e) => { model.path = e.target.value; },
        });

        const rows = [];

        if (model.kind === 'proxy') {
          rows.push(field('目标主机 *', h('input', {
            type: 'text', value: model.host,
            placeholder: '192.168.1.10 或 nas.example.com',
            oninput: (e) => { model.host = e.target.value; },
          }), '填已保存的远端主机名可自动走 SSH 隧道'));
        } else {
          rows.push(field('目标主机', h('input', {
            type: 'text', value: 'localhost', disabled: true,
          }), '本机端口形态固定指向本机回环地址'));
        }

        rows.push(field('端口 *', h('div', { class: 'row' }, [
          h('div', { class: 'grow' }, [portInput]),
          h('button', {
            class: 'btn sm accent nowrap', type: 'button',
            onclick: async (e) => {
              await withBusy(e.currentTarget, async () => {
                try {
                  const res = await api.tools.pickPort();
                  model.port = res.port;
                  portInput.value = String(res.port);
                  notify.ok(`已选空闲端口 ${res.port}`);
                } catch (err) {
                  notify.error(err.message);
                }
              });
            },
          }, ['自动选一个']),
          h('button', {
            class: 'btn sm accent nowrap', type: 'button',
            onclick: (e) => withBusy(e.currentTarget, () => openPortPicker((port) => {
              model.port = port;
              portInput.value = String(port);
            })),
          }, ['从列表选择']),
        ])));

        rows.push(field('协议', segmented([
          { value: 'http', label: 'HTTP' },
          { value: 'https', label: 'HTTPS' },
        ], model.scheme, (v) => { model.scheme = v; })));

        rows.push(field('访问路径', pathInput));

        append(kindBox, h('div', { class: 'form-grid' }, rows));

        if (model.kind === 'proxy') {
          append(kindBox, h('div', { class: 'row mt-3' }, [
            h('label', { class: 'check' }, [
              checkbox(model.skip_tls_verify, (v) => { model.skip_tls_verify = v; }),
              h('span', { class: 'check-body' }, [
                h('span', { text: '忽略后端证书校验' }),
                h('span', { class: 'hint', text: '后端使用自签证书时开启' }),
              ]),
            ]),
          ]));
        }

        /* 连通性测试 */
        const probeBox = h('div', { class: 'mt-3' });
        append(kindBox, h('div', { class: 'row mt-3' }, [
          h('button', {
            class: 'btn sm accent', type: 'button',
            onclick: (e) => withBusy(e.currentTarget, async () => {
              clear(probeBox);
              append(probeBox, h('span', { class: 'muted', text: '正在测试…' }));
              try {
                const target = model.kind === 'proxy'
                  ? buildURL(model)
                  : `http://127.0.0.1:${model.port}${model.path || '/'}`;
                const res = isEdit
                  ? await api.links.probe(existing.id, target)
                  : await api.tools.probe(target);
                clear(probeBox);
                append(probeBox, h('div', { class: 'row' }, [
                  h('span', { class: `tag ${res.ok ? 'ok' : 'danger'}` }, [
                    icon(res.ok ? 'check' : 'alert', 13),
                    res.ok ? '连通' : '不可达',
                  ]),
                  h('span', { class: 'muted', text: res.message || '' }),
                ]));
              } catch (err) {
                clear(probeBox);
                append(probeBox, h('div', { class: 'row' }, [
                  h('span', { class: 'tag danger' }, ['测试失败']),
                  h('span', { class: 'muted', text: err.message }),
                ]));
              }
            }),
          }, ['测试连通性']),
        ]));
        append(kindBox, probeBox);
      };

      buildKindSection();

      /* ---------------------------------------------------- 展示设置 */
      append(root, h('div', { class: 'card' }, [
        h('div', { class: 'card-head' }, [h('h2', { text: '桌面展示' })]),
        h('div', { class: 'card-body' }, [
          h('div', { class: 'form-grid' }, [
            field('打开方式', segmented([
              { value: 'iframe', label: '飞牛内弹窗' },
              { value: 'url', label: '新标签页' },
            ], model.ui, (v) => { model.ui = v; })),
            field('可见范围', h('div', { class: 'row' }, [
              h('label', { class: 'check' }, [
                checkbox(model.all_users, (v) => { model.all_users = v; }),
                h('span', { class: 'check-body' }, [
                  h('span', { text: '所有用户可见' }),
                  h('span', { class: 'hint', text: '关闭则仅管理员可见（外网访问可能受限）' }),
                ]),
              ]),
            ])),
          ]),
          h('div', { class: 'row mt-3' }, [
            switchBox(model.enabled, (v) => { model.enabled = v; }, { title: '启用后立即注册到飞牛桌面' }),
            h('span', { text: '保存后立即注册到飞牛桌面' }),
          ]),
        ]),
      ]));

      /* -------------------------------------------------------- 图标 */
      append(root, buildIconSection(model));

      return root;
    },

    footer: (close) => [
      isEdit ? h('button', {
        class: 'btn danger left', type: 'button',
        onclick: async () => {
          const { confirmDialog } = await import('../dom.js');
          const yes = await confirmDialog({
            title: '移除到桌面',
            message: `确定要删除「${existing.name}」吗？`,
            detail: '对应的桌面图标会被同时注销，代理监听也会停止。',
            danger: true,
            confirmText: '删除',
          });
          if (yes) close({ __delete: true });
        },
      }, ['删除']) : null,
      h('button', {
        class: 'btn', type: 'button', onclick: () => close(null),
      }, ['取消']),
      h('button', {
        class: 'btn primary', type: 'button',
        onclick: async (e) => {
          if (!model.name.trim()) {
            notify.warn('请填写名称');
            return;
          }
          if (model.kind !== 'shortcut' && (!model.port || model.port < 1 || model.port > 65535)) {
            notify.warn('请填写 1-65535 之间的端口');
            return;
          }
          if (model.kind === 'proxy' && !model.host.trim()) {
            notify.warn('端口映射形态必须填写目标主机');
            return;
          }
          await withBusy(e.currentTarget, async () => {
            const payload = toPayload(model);
            try {
              const saved = isEdit
                ? await api.links.update(existing.id, payload)
                : await api.links.create(payload);
              notify.ok(isEdit ? '已保存，正在同步到桌面' : '已创建，正在注册到飞牛桌面');
              close(saved);
            } catch (err) {
              if (isTimeout(err)) {
                // 保存是受理型操作：超时只说明**没收到回执**，不说明没保存。
                // 直接报"失败"会让用户再填一遍表单，然后撞上"名称已存在"。
                // 回查一次列表，用真实数据判定这一条到底落没落库。
                const hit = await findSaved(model.name.trim(), isEdit ? existing.id : null);
                if (hit) {
                  notify.ok(isEdit ? '已保存（响应超时，已回查确认）' : '已创建（响应超时，已回查确认）');
                  close(hit);
                } else {
                  notify.error(`保存结果未知，请稍后刷新列表确认（${err.message}）`);
                }
              } else {
                notify.error(err.message);
              }
            }
          });
        },
      }, [isEdit ? '保存' : '创建']),
    ],
  });
}

/* --------------------------------------------------------------------- */

/**
 * 保存请求超时后，回查这条链接到底在不在库里。
 *
 * 编辑态按 id 找（唯一可靠）；新建态没有 id，退而按名称找——名称在
 * 保存前已做过唯一性校验，足以判定"这一条到底建没建出来"。
 */
async function findSaved(name, id) {
  try {
    const res = await api.links.list();
    const items = res.items || [];
    if (id) return items.find((v) => v.link.id === id) || null;
    return items.find((v) => v.link.name === name) || null;
  } catch {
    return null;
  }
}

function toPayload(model) {
  const path = model.kind === 'shortcut'
    ? model.path.trim()
    : (model.path.trim().startsWith('/') ? model.path.trim() : `/${model.path.trim() || ''}`);

  return {
    name: model.name.trim(),
    desc: model.desc.trim(),
    kind: model.kind,
    app_name: model.app_name.trim(),
    scheme: model.kind === 'shortcut' ? 'https' : model.scheme,
    host: model.kind === 'local' ? 'localhost' : model.host.trim(),
    port: model.kind === 'shortcut' ? 0 : Number(model.port) || 0,
    path: path || '/',
    ui: model.ui,
    all_users: model.all_users,
    no_display: model.no_display,
    skip_tls_verify: model.skip_tls_verify,
    file_types: model.file_types,
    icon: normalizeIcon(model.icon),
    container: model.container,
    enabled: model.enabled,
  };
}

function normalizeIcon(iconModel) {
  const out = { source: iconModel.source || '', ref: '', text: '', text_color: '', bg_color: '' };
  if (out.source === 'text') {
    out.text = (iconModel.text || '').trim();
    out.text_color = iconModel.text_color || '#ffffff';
    out.bg_color = iconModel.bg_color || '#2563eb';
  } else if (out.source === 'url' || out.source === 'upload') {
    out.ref = (iconModel.ref || '').trim();
  }
  return out;
}

function buildURL(model) {
  const scheme = model.scheme || 'http';
  const host = model.host || '127.0.0.1';
  const port = model.port ? `:${model.port}` : '';
  const path = model.path || '/';
  return `${scheme}://${host}${port}${path.startsWith('/') ? path : `/${path}`}`;
}

function buildIconSection(model) {
  const preview = h('img', {
    src: iconPreviewSrc(model.icon),
    alt: '图标预览',
    style: {
      width: '64px', height: '64px', borderRadius: '14px',
      objectFit: 'cover', border: '1px solid var(--border)', background: 'var(--bg-sunken)',
    },
  });
  const fields = h('div', { class: 'grow' });

  const refreshPreview = () => {
    if (model.icon.source === 'text') {
      const wrap = h('div', {
        style: {
          width: '64px', height: '64px', borderRadius: '14px',
          display: 'flex', alignItems: 'center', justifyContent: 'center',
          background: model.icon.bg_color || '#2563eb',
          color: model.icon.text_color || '#ffffff',
          fontWeight: '700', fontSize: '1.15rem', letterSpacing: '.02em',
          border: '1px solid var(--border)',
        },
        text: (model.icon.text || 'QL').slice(0, 4).toUpperCase(),
      });
      replace(preview.parentNode, [wrap, fields]);
      previewRef.node = wrap;
    } else {
      const img = h('img', {
        src: iconPreviewSrc(model.icon), alt: '图标预览',
        style: {
          width: '64px', height: '64px', borderRadius: '14px',
          objectFit: 'cover', border: '1px solid var(--border)', background: 'var(--bg-sunken)',
        },
      });
      replace(previewRef.node.parentNode, [img, fields]);
      previewRef.node = img;
    }
  };
  const previewRef = { node: preview };

  const rebuild = () => {
    clear(fields);
    const src = model.icon.source;

    if (src === 'text') {
      append(fields, h('div', { class: 'form-grid' }, [
        field('文字', h('input', {
          type: 'text', value: model.icon.text, maxlength: '4', placeholder: '最多 4 个字符',
          oninput: (e) => { model.icon.text = e.target.value; refreshPreview(); },
        }), '支持 A-Z、0-9 与 + - . _'),
        field('文字颜色', colorInput(model.icon.text_color, (v) => { model.icon.text_color = v; refreshPreview(); })),
        field('底色', colorInput(model.icon.bg_color, (v) => { model.icon.bg_color = v; refreshPreview(); })),
      ]));
    } else if (src === 'url') {
      append(fields, field('图标网址', h('input', {
        type: 'text', value: model.icon.ref, placeholder: 'https://example.com/logo.png',
        oninput: (e) => { model.icon.ref = e.target.value; refreshPreview(); },
      }), '支持 PNG / JPEG / SVG，会自动裁成方形并生成两档尺寸'));
    } else if (src === 'upload') {
      const fileInput = h('input', {
        type: 'file', accept: 'image/*',
        style: { padding: '6px' },
        onchange: async (e) => {
          const file = e.target.files && e.target.files[0];
          if (!file) return;
          try {
            const res = await api.icons.upload(file);
            model.icon.ref = res.name;
            notify.ok('图标已上传');
            refreshPreview();
          } catch (err) {
            notify.error(`上传失败：${err.message}`);
          }
        },
      });
      append(fields, h('div', { class: 'form-grid' }, [
        field('上传图片', fileInput, '建议 256×256 以上的正方形图片'),
        model.icon.ref ? field('已选文件', h('div', { class: 'mono truncate', text: model.icon.ref })) : null,
      ]));
    } else {
      append(fields, h('p', { class: 'muted', text: '将由系统根据容器镜像名自动匹配官方图标；匹配不到时使用内置默认图标。' }));
    }
  };

  const seg = segmented([
    { value: '', label: '自动' },
    { value: 'text', label: '文字' },
    { value: 'url', label: '网址' },
    { value: 'upload', label: '上传' },
  ], model.icon.source || '', (v) => {
    model.icon.source = v;
    rebuild();
    refreshPreview();
  });

  rebuild();

  return h('div', { class: 'card' }, [
    h('div', { class: 'card-head' }, [
      h('h2', { text: '图标' }),
      h('div', { class: 'row' }, [seg]),
    ]),
    h('div', { class: 'card-body' }, [
      h('div', { class: 'row', style: { alignItems: 'flex-start', gap: '16px' } }, [preview, fields]),
    ]),
  ]);
}

function iconPreviewSrc(iconModel) {
  if (iconModel.source === 'url' && iconModel.ref) return iconModel.ref;
  if (iconModel.source === 'upload' && iconModel.ref) return api.icons.url(iconModel.ref);
  return api.icons.defaultURL(256);
}

function fileTypesInput(model) {
  const input = h('input', {
    type: 'text',
    value: (model.file_types || []).join(', '),
    placeholder: '例如 pdf, docx（可留空）',
    oninput: (e) => {
      model.file_types = e.target.value.split(',').map((s) => s.trim()).filter(Boolean);
    },
  });
  return input;
}

/* ------------------------------------------------------------- 小控件 */

function segmented(items, current, onChange) {
  const buttons = items.map((item) => h('button', {
    type: 'button',
    class: item.value === current ? 'active' : '',
    onclick: (e) => {
      const box = e.currentTarget.parentNode;
      for (const btn of box.children) btn.classList.remove('active');
      e.currentTarget.classList.add('active');
      onChange(item.value);
    },
  }, [item.label]));
  return h('div', { class: 'segmented' }, buttons);
}

function checkbox(checked, onChange) {
  return h('input', {
    type: 'checkbox', checked: !!checked,
    onchange: (e) => onChange(e.target.checked),
  });
}

function colorInput(value, onChange) {
  return h('div', { class: 'row' }, [
    h('input', {
      type: 'color', value: value || '#2563eb',
      style: { width: '42px', height: '34px', padding: '2px', cursor: 'pointer' },
      oninput: (e) => onChange(e.target.value),
    }),
    h('input', {
      type: 'text', value: value || '#2563eb',
      oninput: (e) => onChange(e.target.value),
    }),
  ]);
}

/* ------------------------------------------------------- 端口选择器 */

/** 打开一个可搜索的本机端口列表，选中后回调端口号 */
export async function openPortPicker(onPick) {
  let ports = state.discovery.ports || [];
  const listBox = h('div', { class: 'grid', style: { gridTemplateColumns: 'repeat(auto-fill,minmax(210px,1fr))' } });
  const keyword = h('input', { type: 'search', placeholder: '按端口、进程、容器名筛选…' });

  const draw = () => {
    const q = keyword.value.trim().toLowerCase();
    clear(listBox);
    const items = ports.filter((p) => {
      if (!q) return true;
      return [String(p.port), p.process, p.container?.name, p.container?.image, p.address]
        .filter(Boolean).join(' ').toLowerCase().includes(q);
    });
    if (!items.length) {
      append(listBox, h('p', { class: 'muted', text: '没有匹配的端口。可在「服务发现」页点「重新扫描」。' }));
      return;
    }
    for (const p of items) {
      append(listBox, h('button', {
        class: 'btn', type: 'button',
        style: { justifyContent: 'flex-start', textAlign: 'left', padding: '9px 12px' },
        onclick: () => {
          onPick?.(p.port);
          current.close();
        },
      }, [
        h('span', { class: 'mono', style: { fontWeight: '600' }, text: `:${p.port}` }),
        h('span', { class: 'truncate', text: p.container?.name || p.process || p.address || '' }),
      ]));
    }
  };

  const current = modal({
    title: '选择本机端口',
    body: h('div', {}, [
      h('div', { class: 'search mb-3' }, [icon('search', 14), keyword]),
      listBox,
    ]),
    footer: (close) => [h('button', { class: 'btn', type: 'button', onclick: () => close(null) }, ['关闭'])],
  });

  keyword.addEventListener('input', draw);

  if (!ports.length) {
    try {
      const res = await api.discovery.ports(false);
      ports = res.items || [];
      state.discovery.ports = ports;
      state.discovery.docker = !!res.docker;
    } catch (err) {
      notify.error(`读取端口列表失败：${err.message}`);
    }
  }
  draw();
  return current;
}
