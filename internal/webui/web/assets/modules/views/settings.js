/* settings.js —— 「设置」视图
 *
 * 分五块：
 *   界面主题 —— 明暗模式 × 色系（本机偏好，不进后端配置）
 *   门户外观 —— 桌面图标名称、图标、打开方式、可见范围
 *   访问控制 —— 口令保护（摘要存盘、会话即时失效）
 *   行为     —— 启动时自动对账
 *   维护     —— 数据目录信息与导入导出
 */

import { h, append, replace, notify, withBusy, field, switchBox, confirmDialog } from '../dom.js';
import { icon } from '../icons.js';
import { api, isTimeout } from '../api.js';
import { state, patch } from '../store.js';
import { MODES, ACCENTS, loadTheme, setTheme, resolveMode } from '../theme.js';

export const title = '设置';
export const subtitle = () => '界面主题、门户名称、访问口令与启动行为';

export function actions() {
  return [];
}

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
  const settings = state.settings;
  if (!settings) {
    replace(root, h('div', { class: 'card' }, [
      h('div', { class: 'card-body' }, [h('div', { class: 'skeleton', style: { height: '120px' } })]),
    ]));
    return;
  }

  replace(root, [
    // 主题放在最上面：它是用户最容易想改、也最想立刻看到效果的一项。
    appearanceCard(),
    portalCard(settings),
    authCard(settings),
    behaviorCard(settings),
    maintenanceCard(settings),
  ]);
}

/* ------------------------------------------------------------ 界面主题 */

/**
 * 主题是**本机偏好**，不是服务配置，所以这一块不往后端写任何东西。
 *
 * 理由写在 theme.js 的头部，这里只说结论：它必须能在首帧之前生效
 * （因此只能读 localStorage，不能等接口），而且同一台 NAS 上不同设备
 * 想用不同底色是合理需求，不该互相覆盖。
 */
function appearanceCard() {
  const current = loadTheme();
  const resolved = resolveMode(current.mode);

  return card('界面主题', '只保存在这台设备上，不会影响其它浏览器或已创建的桌面图标', [
    h('div', { class: 'form-grid' }, [
      field('明暗模式', segmented(
        MODES.map((m) => ({ value: m.value, label: m.label })),
        current.mode,
        // 只改状态，**不自己 draw()**：顶栏那颗切换按钮也显示当前主题，
        // 两处各画各的会停在不同步的状态上。重画统一由 app.js 的
        // qlink:theme-changed 监听器做（见 theme.js 的说明）。
        (v) => setTheme({ mode: v }),
      ), current.mode === 'auto'
        ? `正在跟随系统，当前为${resolved === 'dark' ? '暗黑' : '白天'}`
        : '顶栏右侧也有一颗切换按钮，不用每次都进设置页'),
    ]),

    h('div', { class: 'mt-3' }, [
      h('label', { text: '色系' }),
      // 色卡用真实的示例色预览：用户挑的是"看起来什么样"，
      // 只写文字标签等于让他盲选。
      h('div', { class: 'swatches mt-2' }, ACCENTS.map((a) => swatchButton(a, current.accent,
        () => setTheme({ accent: a.value })))),
    ]),
  ]);
}

function swatchButton(accent, current, onPick) {
  const active = accent.value === current;
  return h('button', {
    type: 'button',
    class: `swatch ${active ? 'active' : ''}`,
    title: accent.label,
    onclick: onPick,
  }, [
    h('span', { class: 'swatch-dot', style: { background: accent.swatch } }),
    h('span', { class: 'swatch-name', text: accent.label }),
    active ? icon('check', 14) : null,
  ]);
}

/* ------------------------------------------------------------ 门户外观 */

function portalCard(settings) {
  const model = {
    portal_name: settings.portal_name || '',
    portal_port: settings.portal_port || 5900,
    portal_ui: settings.portal_ui || 'iframe',
    portal_all_users: !!settings.portal_all_users,
  };

  const nameInput = h('input', {
    type: 'text', value: model.portal_name,
    oninput: (e) => { model.portal_name = e.target.value; },
  });
  const portInput = h('input', {
    type: 'number', min: '1', max: '65535', value: model.portal_port,
    oninput: (e) => { model.portal_port = Number(e.target.value) || 5900; },
  });
  const uiSeg = segmented([
    { value: 'iframe', label: '飞牛内弹窗' },
    { value: 'url', label: '新标签页' },
  ], model.portal_ui, (v) => { model.portal_ui = v; });
  const allUsers = switchBox(model.portal_all_users, (v) => { model.portal_all_users = v; });

  const iconPreview = h('img', {
    src: api.icons.portalURL(256, Date.now()), alt: '门户图标',
    style: { width: '56px', height: '56px', borderRadius: '14px', border: '1px solid var(--border)' },
  });
  const iconModel = {
    text: settings.portal_icon?.text || 'QL',
    text_color: settings.portal_icon?.text_color || '#ffffff',
    bg_color: settings.portal_icon?.bg_color || '#2563eb',
  };
  const livePreview = h('div', {
    style: {
      width: '56px', height: '56px', borderRadius: '14px',
      display: 'flex', alignItems: 'center', justifyContent: 'center',
      fontWeight: '700', fontSize: '1.05rem',
      background: iconModel.bg_color, color: iconModel.text_color,
      border: '1px solid var(--border)',
    },
    text: iconModel.text,
  });
  const syncPreview = () => {
    livePreview.textContent = iconModel.text || 'QL';
    livePreview.style.background = iconModel.bg_color;
    livePreview.style.color = iconModel.text_color;
    iconPreview.src = api.icons.portalURL(256, Date.now());
  };

  return card('门户外观', '决定「QLink2Desktop」自身在飞牛桌面上显示成什么样', [
    h('div', { class: 'form-grid' }, [
      field('桌面名称', nameInput),
      field('服务端口', portInput, '仅在未使用飞牛统一网关（Unix Socket）时生效，修改后需重启应用'),
      field('打开方式', uiSeg),
      field('可见范围', h('div', { class: 'row' }, [
        allUsers,
        h('span', { text: '所有用户可见' }),
      ])),
    ]),
    h('div', { class: 'row mt-3', style: { alignItems: 'flex-start', gap: '14px' } }, [
      livePreview,
      h('div', { class: 'form-grid grow' }, [
        field('图标文字', h('input', {
          type: 'text', maxlength: '4', value: iconModel.text,
          oninput: (e) => { iconModel.text = e.target.value; syncPreview(); },
        }), '最多 4 个字符'),
        field('文字颜色', colorInput(iconModel.text_color, (v) => { iconModel.text_color = v; syncPreview(); })),
        field('底色', colorInput(iconModel.bg_color, (v) => { iconModel.bg_color = v; syncPreview(); })),
      ]),
    ]),
    h('div', { class: 'row mt-3' }, [
      saveButton(async () => {
        const res = await api.settings.update({
          portal_name: model.portal_name.trim(),
          portal_port: model.portal_port,
          portal_ui: model.portal_ui,
          portal_all_users: model.portal_all_users,
          portal_icon: {
            source: 'text',
            text: iconModel.text || 'QL',
            text_color: iconModel.text_color,
            bg_color: iconModel.bg_color,
          },
        });
        patch({ settings: res.settings });
        notify.ok(res.portal_port_changed ? '已保存；端口变更需要重启应用后生效' : '已保存');
        draw();
      }),
    ]),
  ]);
}

/* ------------------------------------------------------------ 访问控制 */

function authCard(settings) {
  const protectedNow = !!state.auth.protected;
  const model = { current: '', next: '', confirm: '' };

  return card('访问控制', protectedNow
    ? '已启用口令保护：打开面板需要先登录'
    : '当前**没有**口令保护——局域网内任何人都能打开并修改配置', [
    !protectedNow ? h('div', { class: 'notice-bar' }, [
      icon('alert', 16),
      h('span', { class: 'grow', text: '建议设置访问口令，尤其是在同一网络下有其它设备时。' }),
    ]) : null,

    h('div', { class: 'form-grid' }, [
      protectedNow ? field('当前口令', h('input', {
        type: 'password', autocomplete: 'current-password', value: '',
        oninput: (e) => { model.current = e.target.value; },
      })) : null,
      field('新口令', h('input', {
        type: 'password', autocomplete: 'new-password', value: '', placeholder: '至少 8 位且不能是纯数字',
        oninput: (e) => { model.next = e.target.value; },
      })),
      field('确认新口令', h('input', {
        type: 'password', autocomplete: 'new-password', value: '',
        oninput: (e) => { model.confirm = e.target.value; },
      })),
    ]),

    h('div', { class: 'row mt-3 between' }, [
      h('span', { class: 'muted', style: { fontSize: '.78rem' },
        text: '口令以多轮哈希 + 随机盐保存，服务端不保留明文；修改后所有已登录会话立即失效。' }),
      h('button', {
        class: 'btn primary', type: 'button',
        onclick: async (e) => {
          if (model.next.length < 8) { notify.warn('新口令至少 8 个字符'); return; }
          if (model.next !== model.confirm) { notify.warn('两次输入的新口令不一致'); return; }
          await withBusy(e.currentTarget, async () => {
            try {
              await api.settings.update({ current_password: model.current, password: model.next });
              notify.ok('口令已更新，请重新登录');
              window.dispatchEvent(new CustomEvent('qlink:relogin'));
            } catch (err) {
              notify.error(err.message);
            }
          });
        },
      }, [protectedNow ? '修改口令' : '设置口令']),
    ]),

    protectedNow ? h('div', { class: 'row mt-3' }, [
      h('button', {
        class: 'btn danger', type: 'button',
        onclick: async (e) => {
          const yes = await confirmDialog({
            title: '关闭口令保护',
            message: '确定要清除访问口令吗？',
            detail: '清除后任何能访问该地址的人都可以修改配置。',
            danger: true, confirmText: '清除口令',
          });
          if (!yes) return;
          if (!model.current) { notify.warn('请先在「当前口令」里填写现有口令'); return; }
          await withBusy(e.currentTarget, async () => {
            try {
              await api.settings.update({ current_password: model.current, clear_password: true });
              notify.ok('口令保护已关闭');
              await refreshAll();
            } catch (err) {
              notify.error(err.message);
            }
          });
        },
      }, ['关闭口令保护']),
      h('button', {
        class: 'btn warn', type: 'button',
        onclick: async () => {
          try {
            await api.auth.logout();
            window.dispatchEvent(new CustomEvent('qlink:relogin'));
          } catch (err) {
            notify.error(err.message);
          }
        },
      }, ['退出登录']),
    ]) : null,
  ]);
}

/* -------------------------------------------------------------- 行为 */

function behaviorCard(settings) {
  return card('启动行为', null, [
    h('label', { class: 'check' }, [
      h('input', {
        type: 'checkbox', checked: !!settings.auto_reconcile,
        onchange: async (e) => {
          try {
            const res = await api.settings.update({ auto_reconcile: e.target.checked });
            patch({ settings: res.settings });
            notify.ok(e.target.checked ? '已开启启动自动对账' : '已关闭启动自动对账');
          } catch (err) {
            e.target.checked = !e.target.checked;
            notify.error(err.message);
          }
        },
      }),
      h('span', { class: 'check-body' }, [
        h('span', { text: '启动时自动补齐 / 恢复桌面图标' }),
        h('span', { class: 'hint', text: '关闭后仅恢复反向代理监听，不主动改动飞牛应用中心里的图标' }),
      ]),
    ]),
  ]);
}

/* -------------------------------------------------------------- 维护 */

function maintenanceCard(settings) {
  const info = state.system || {};
  // 留空 = 用应用数据目录下的 icons/（后端为此约定的语义）。
  const model = { icons_dir: (settings && settings.icons_dir) || '' };

  return card('数据与维护', '导出的文件可以直接编辑后再手工回填', [
    h('div', { class: 'form-grid' }, [
      readOnlyField('数据目录', info.data_dir),
      readOnlyField('日志目录', info.log_dir),
      field('图标目录', h('div', { class: 'row' }, [
        h('input', {
          type: 'text', class: 'mono', value: model.icons_dir, style: { flex: '1', minWidth: '0' },
          placeholder: info.icons_dir || '留空表示使用应用数据目录',
          oninput: (e) => { model.icons_dir = e.target.value; },
        }),
        h('button', {
          class: 'btn primary nowrap', type: 'button',
          onclick: (e) => withBusy(e.currentTarget, async () => {
            try {
              const res = await api.settings.update({ icons_dir: model.icons_dir.trim() });
              patch({ settings: res.settings });
              notify.ok('图标目录已更新');
              // 生效路径由后端回报，重新拉一次「系统信息」才能看到新值；
              // 设置本身已经在响应里，不必再问一遍。
              patch({ system: await api.system.info() });
              draw();
            } catch (err) {
              notify.error(err.message);
            }
          }),
        }, ['保存']),
      ]), '绝对路径；留空表示用应用数据目录下的 icons/。已有图标不会自动迁移，换目录后需要重新上传'),
    ]),
    h('div', { class: 'row mt-3' }, [
      h('span', { class: 'muted', style: { fontSize: '.78rem' },
        text: `当前生效：${info.icons_dir || '—'}` }),
    ]),
    h('div', { class: 'row mt-3' }, [
      h('a', { class: 'btn accent', href: api.links.exportURL(), download: '' }, [icon('download', 15), '导出链接定义']),
      h('button', {
        class: 'btn accent', type: 'button',
        onclick: async (e) => withBusy(e.currentTarget, async () => {
          const yes = await confirmDialog({
            title: '立即对账',
            message: '把飞牛应用中心里的图标收敛到当前配置？',
            detail: '已就绪的项不会被改动，只有缺失、配置过期或被停用的项会被处理。',
            confirmText: '开始对账',
          });
          if (!yes) return;
          try {
            // 对账是受理型接口：返回只代表"已提交"，结果要看每条链接的状态。
            // 后台会通过 SSE 一路把状态推过来，所以这里不需要自己轮询。
            const res = await api.system.reconcile();
            const pending = res?.pending ?? 0;
            notify.ok(pending > 1
              ? `对账已在后台开始（队列中 ${pending} 项），状态会自动更新`
              : '对账已在后台开始，状态会自动更新');
          } catch (err) {
            notify.error(isTimeout(err)
              ? `对账未能确认，请稍后回到「桌面应用」查看状态（${err.message}）`
              : err.message);
          }
        }),
      }, [icon('refresh', 15), '立即对账']),
      h('button', {
        class: 'btn danger', type: 'button',
        onclick: async (e) => withBusy(e.currentTarget, async () => {
          const yes = await confirmDialog({
            title: '清理遗留图标',
            message: '卸载「QLink2Desktop 创建、但已不在链接列表中」的桌面图标？',
            detail: '只处理 qlink2d. 命名空间下本项目自己创建的图标，其它任何应用（包括其它工具生成的图标）都不会被触碰。此操作不可撤销。',
            confirmText: '确认清理',
          });
          if (!yes) return;
          try {
            // 同样是受理型：卸载要在后台跑 appcenter-cli，可能持续几分钟。
            const res = await api.system.cleanupOrphans();
            notify.ok(res?.pending > 1
              ? `清理已在后台开始（队列中 ${res.pending} 项）`
              : '清理已在后台开始，完成后可回到本页核对');
          } catch (err) {
            notify.error(isTimeout(err)
              ? `清理未能确认，请稍后刷新本页核对（${err.message}）`
              : err.message);
          }
        }),
      }, [icon('trash', 15), '清理遗留图标']),
    ]),
  ]);
}

/* ------------------------------------------------------------------ 辅助 */

function card(heading, desc, body) {
  return h('div', { class: 'card' }, [
    h('div', { class: 'card-head' }, [
      h('div', {}, [h('h2', { text: heading }), desc ? h('p', { text: desc }) : null]),
    ]),
    h('div', { class: 'card-body' }, body),
  ]);
}

function readOnlyField(label, value) {
  return field(label, h('input', { type: 'text', value: value || '—', readonly: true, class: 'mono' }));
}

function saveButton(onClick) {
  return h('button', {
    class: 'btn primary', type: 'button',
    onclick: (e) => withBusy(e.currentTarget, onClick),
  }, ['保存']);
}

function segmented(items, current, onChange) {
  return h('div', { class: 'segmented' }, items.map((item) => h('button', {
    type: 'button',
    class: item.value === current ? 'active' : '',
    onclick: (e) => {
      for (const btn of e.currentTarget.parentNode.children) btn.classList.remove('active');
      e.currentTarget.classList.add('active');
      onChange(item.value);
    },
  }, [item.label])));
}

function colorInput(value, onChange) {
  return h('div', { class: 'row' }, [
    h('input', {
      type: 'color', value: value || '#2563eb',
      style: { width: '42px', height: '34px', padding: '2px', cursor: 'pointer' },
      oninput: (e) => onChange(e.target.value),
    }),
    h('input', { type: 'text', value: value || '#2563eb', oninput: (e) => onChange(e.target.value) }),
  ]);
}

async function refreshAll() {
  try {
    const [auth, settings] = await Promise.all([api.auth.status(), api.settings.get()]);
    patch({ auth, settings: settings.settings });
    draw();
  } catch (err) {
    if (err.status !== 401) notify.error(err.message);
  }
}
