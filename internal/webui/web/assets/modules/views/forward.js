/* forward.js —— 「端口转发」视图
 *
 * 管理四层（TCP / UDP）端口转发规则：把本机一个端口收到的流量，
 * 原样转送到另一台设备（或本机另一个端口）的服务上。
 *
 * 它和「端口映射」那种反向代理是两件事：
 *   反向代理工作在应用层，只认 HTTP，但能按域名路由、能跳过自签证书；
 *   端口转发工作在传输层，看不见内容，因此 SSH / RDP / SMB / 数据库 /
 *   DNS / 游戏这些都能原样穿过去。
 * 界面上刻意把这一点说清楚，否则用户很难判断该用哪一个。
 */

import {
  h, replace, clear, append, notify, confirmDialog, emptyState,
  switchBox, field, table, modal, withBusy, fmtBytes, fmtTime, segmented,
} from '../dom.js';
import { icon } from '../icons.js';
import { api } from '../api.js';
import { state, patch } from '../store.js';

export const title = '端口转发';

export const subtitle = () => {
  const all = state.forwards || [];
  const on = all.filter((v) => v.status && v.status.running).length;
  if (!all.length) return '把本机端口的流量转送到内网其它设备';
  return `共 ${all.length} 条规则，${on} 条正在监听`;
};

const PROTOCOLS = [
  { value: 'tcp', label: 'TCP' },
  { value: 'udp', label: 'UDP' },
  { value: 'both', label: 'TCP + UDP' },
];

const ACCESS_MODES = [
  { value: '', label: '不限制' },
  { value: 'allow', label: '白名单' },
  { value: 'deny', label: '黑名单' },
];

const ACCESS_HINT = {
  '': '任何人只要能访问到这台设备，都能连这个端口。',
  allow: '只有列表内的来源可以连，其余全部拒绝。适合没有认证的服务。',
  deny: '列表内的来源会被拒绝，其余放行。',
};

export function actions() {
  return [
    h('button', {
      class: 'btn ghost', type: 'button', title: '查一下某个端口现在被谁占着',
      onclick: () => openPortCheck(),
    }, [icon('search', 15), '检查端口']),
    h('button', {
      class: 'btn ghost', type: 'button', title: '重新拉取规则与运行态',
      onclick: (e) => withBusy(e.currentTarget, load),
    }, [icon('refresh', 15), '刷新']),
    h('button', {
      class: 'btn primary', type: 'button',
      onclick: () => openEditor(null),
    }, [icon('plus', 15), '新建规则']),
  ];
}

/**
 * 端口占用检测。
 *
 * 存在的理由：绑定失败是**事后**才知道的，用户还需要自己猜"是谁占着"。
 * 而这个功能最常见的困惑恰恰是"同样的设置在别的工具上能跑，在这里报被占用"——
 * 与其让他去满机器找，不如点一下直接看到占用者是谁、占的是哪一侧。
 */
function openPortCheck() {
  const model = { protocol: 'tcp', port: 0 };
  const result = h('div');

  const portInput = h('input', {
    type: 'number', min: '1', max: '65535', placeholder: '例如 18080',
    oninput: (e) => { model.port = Number(e.target.value) || 0; },
  });

  const runBtn = h('button', { class: 'btn primary', type: 'button' }, [icon('search', 15), '检查']);
  runBtn.onclick = () => withBusy(runBtn, async () => {
    if (!model.port) {
      notify.warn('请先填写要检查的端口号');
      return;
    }
    replace(result, h('div', { class: 'cell-sub', text: '检查中…' }));
    try {
      const res = await api.forwards.checkPort(model.protocol, model.port);
      replace(result, renderPortCheck(res));
    } catch (err) {
      replace(result, h('div', { class: 'hint', style: { color: 'var(--danger)' }, text: err.message }));
    }
  });

  modal({
    title: '检查端口占用',
    body: h('div', { class: 'grid', style: { gap: '14px' } }, [
      field('协议', segmented(PROTOCOLS.filter((p) => p.value !== 'both'), model.protocol,
        (v) => { model.protocol = v; })),
      field('监听端口', portInput, '填你打算用的端口号；TCP 与 UDP 的端口互不影响'),
      result,
    ]),
    footer: (close) => [
      h('button', { class: 'btn', type: 'button', onclick: () => close(null) }, ['关闭']),
      runBtn,
    ],
  });
  portInput.focus?.();
}

/** 把探测结果画成一段看得懂的说明，并给出可执行的下一步。 */
function renderPortCheck(res) {
  if (res && res.available) {
    // "能监听"可能是"退到单栈后能监听"：另一侧的访客连不上，
    // 所以不能给一个无条件的绿灯。
    return h('div', {}, [
      h('span', {
        class: res.degraded ? 'tag warn' : 'tag ok',
        text: res.degraded ? '可以监听（只能单栈）' : '可以监听',
      }),
      h('div', { class: 'cell-sub', style: { marginTop: '6px' }, text: res.message || '' }),
    ]);
  }

  const tag = res && res.in_use_by_rule
    ? h('span', { class: 'tag danger', text: '已被本应用的一条规则占用' })
    : (res && res.self)
      ? h('span', { class: 'tag warn', text: '被本应用自己占用' })
      : h('span', { class: 'tag danger', text: '被其它程序占用' });

  const nodes = [tag];
  if (res && res.message) {
    nodes.push(h('div', { class: 'cell-sub', style: { marginTop: '6px' }, text: res.message }));
  }
  if (res && (res.holders || []).length) {
    nodes.push(h('ul', { style: { margin: '8px 0 0', paddingLeft: '18px' } },
      res.holders.map((hd) => h('li', {
        class: 'cell-sub mono',
        text: `${hd.name || '未知进程'} · PID ${hd.pid}${hd.side ? ` · 占用 ${hd.side} 侧` : ''}`,
      }))));
  }
  if (res && res.self && !res.in_use_by_rule) {
    nodes.push(h('div', { class: 'hint', text:
      '占用者是本应用自己，最常见的是「桌面应用」里某条链接的反向代理端口' +
      '（18000 段由本应用自动分配）。停用那条链接、或给本条规则换一个端口即可。' }));
  } else if (res && !res.in_use_by_rule) {
    nodes.push(h('div', { class: 'hint', text:
      '换一个端口，或把上面那个程序停掉。如果只是想要另一侧能用，' +
      '也可以把监听地址填成 0.0.0.0（只用 IPv4）或 ::（只用 IPv6）绕开被占的一侧。' }));
  }
  return h('div', {}, nodes);
}

let root = null;

export function mount(container) {
  root = container;
  load();
}

export function refresh() {
  if (root) draw();
}

async function load() {
  try {
    const res = await api.forwards.list();
    patch({ forwards: res.items || [] });
  } catch (err) {
    if (err.status !== 401) notify.error(`读取转发规则失败：${err.message}`);
  }
  refresh();
}

/* ------------------------------------------------------------------ 列表 */

function draw() {
  if (!root) return;
  const items = state.forwards || [];

  if (!items.length) {
    replace(root, h('div', { class: 'card' }, [
      h('div', { class: 'card-body' }, [
        emptyState('还没有转发规则', '把本机一个端口的流量转送到内网其它设备，' +
          '例如把 192.168.31.50 的远程桌面映射成本机的 13389。', icon('shuffle', 40)),
        h('div', { class: 'row', style: { justifyContent: 'center', marginTop: '14px' } }, [
          h('button', { class: 'btn primary', type: 'button', onclick: () => openEditor(null) },
            [icon('plus', 15), '新建转发规则']),
        ]),
      ]),
    ]));
    return;
  }

  const cols = [
    { label: '规则', width: '22%' },
    { label: '监听', width: '12%' },
    { label: '目标', width: '18%' },
    { label: '状态', width: '14%' },
    { label: '流量', width: '14%' },
    { label: '操作', width: '20%' },
  ];

  replace(root, [
    h('div', { class: 'card' }, [
      h('div', { class: 'card-head' }, [
        h('div', {}, [
          h('h2', { text: '转发规则' }),
          h('p', { text: '转发在传输层进行，不解析内容，因此 SSH / RDP / 数据库 / 游戏都能穿过去。' +
            '需要按域名路由或跳过自签证书的 HTTP 服务，请改用「端口映射」类型的桌面应用。' }),
        ]),
      ]),
      h('div', { class: 'card-body tight' }, [
        table(cols, items, row),
      ]),
    ]),
  ]);
}

/** 状态标签：运行中是绿的、启动失败是红的、已停用是灰的。 */
function statusTag(item) {
  const st = item.status || {};
  if (!item.forward.enabled) return h('span', { class: 'tag', text: '已停用' });
  if (st.error) {
    return h('span', { class: 'tag danger', title: st.error, text: '启动失败' });
  }
  if (st.running) {
    const protos = (st.protocols || []).join('/').toUpperCase();
    // 退让到单栈时必须说出来。
    //
    // 监听地址留空时绑定会自动退让，"监听成功"因此不等于"两种协议都能连"：
    // 退到仅 IPv6 时 IPv4 的访客是连不上的，而 `[::]:18080` 在"双栈"与
    // "仅 IPv6"两种情况下字符串完全一样 —— 光看地址分不出来。
    const degraded = st.bind_mode === 'ipv4' || st.bind_mode === 'ipv6';
    if (degraded) {
      const side = st.bind_mode === 'ipv4' ? '仅 IPv4' : '仅 IPv6';
      return h('span', {
        class: 'tag warn', text: `监听中 ${protos} · ${side}`,
        title: `该端口的另一侧被别的程序占用，已退化为单栈监听：只有 ${side} 的访客能连上。` +
          '想恢复双栈，要么把占用者停掉，要么换一个端口。',
      });
    }
    return h('span', { class: 'tag ok', text: `监听中 ${protos}` });
  }
  return h('span', { class: 'tag warn', text: '未监听' });
}

function row(item) {
  const f = item.forward || {};
  const c = (item.status && item.status.counters) || {};

  const toggle = switchBox(f.enabled, async (on) => {
    try {
      await api.forwards.setEnabled(f.id, on);
      await load();
    } catch (err) {
      notify.error(err.message);
      await load();
    }
  }, { title: f.enabled ? '停止这条规则' : '启动这条规则' });

  return h('tr', {}, [
    h('td', {}, [
      h('div', { class: 'cell-main', text: f.name || '(未命名)' }),
      h('div', { class: 'cell-sub' }, [
        h('span', { text: protocolLabel(f.protocol) }),
        f.desc ? h('span', { text: ` · ${f.desc}` }) : null,
        (f.access_mode && (f.access_list || []).length)
          ? h('span', { class: 'tag accent', style: { marginLeft: '6px' },
            text: `${f.access_mode === 'allow' ? '白名单' : '黑名单'} ${f.access_list.length} 条` })
          : null,
      ]),
    ]),
    h('td', {}, [
      h('div', { class: 'mono', text: `:${f.listen_port}` }),
      f.listen_host
        ? h('div', { class: 'cell-sub mono', text: f.listen_host })
        : h('div', { class: 'cell-sub', text: '全部地址' }),
    ]),
    h('td', {}, [
      h('div', { class: 'mono', text: `${f.target_host}:${f.target_port}` }),
    ]),
    h('td', {}, [
      h('div', {}, [statusTag(item)]),
      h('div', { class: 'cell-sub', text: `累计 ${c.total || 0} 次 · 活跃 ${c.active || 0}` }),
    ]),
    h('td', {}, [
      h('div', { class: 'cell-sub', text: `↓ ${fmtBytes(c.bytes_in || 0)} ↑ ${fmtBytes(c.bytes_out || 0)}` }),
      (c.denied || c.failed || c.limited)
        ? h('div', { class: 'cell-sub', style: { color: 'var(--warn)' },
          text: `拒绝 ${c.denied || 0} · 失败 ${c.failed || 0}${c.limited ? ` · 超限 ${c.limited}` : ''}` })
        : null,
    ]),
    h('td', {}, [
      h('div', { class: 'row', style: { gap: '4px', flexWrap: 'wrap' } }, [
        toggle,
        iconButton('target', '测试目标是否可达', async (btn) => {
          await withBusy(btn, async () => {
            try {
              const res = await api.forwards.probe(f.id);
              if (res.ok) {
                notify.ok(`目标可达，耗时 ${res.latency_ms} ms${res.note ? `（${res.note}）` : ''}`);
              } else {
                notify.warn(`目标不可达：${res.error}`);
              }
            } catch (err) {
              notify.error(err.message);
            }
          });
        }),
        iconButton('clock', '最近连接记录', () => showConnections(f)),
        iconButton('edit', '编辑', () => openEditor(f)),
        iconButton('link', '为这个端口创建桌面图标', async () => { await addToDesktop(f); }),
        iconButton('trash', '删除', async () => {
          const yes = await confirmDialog({
            title: '删除转发规则',
            message: `确定删除「${f.name}」吗？`,
            detail: `监听端口 :${f.listen_port} 会被立即释放。`,
            danger: true, confirmText: '删除',
          });
          if (!yes) return;
          try {
            await api.forwards.remove(f.id);
            notify.ok('已删除');
            await load();
          } catch (err) {
            notify.error(err.message);
          }
        }, 'danger'),
      ]),
    ]),
  ]);
}

function iconButton(name, title, onclick, extra = '') {
  return h('button', {
    class: `btn ghost icon ${extra}`.trim(), type: 'button', title,
    onclick: (e) => onclick(e.currentTarget),
  }, [icon(name, 15)]);
}

function protocolLabel(p) {
  const found = PROTOCOLS.find((x) => x.value === p);
  return found ? found.label : (p || 'tcp').toUpperCase();
}

/**
 * 为某个转发端口创建一个桌面图标。
 *
 * 这是端口转发与本项目主线的交接点：转发把服务搬到了本机的一个端口上，
 * 之后它就是一个普通的「本机端口」桌面应用，可以直接复用现有的链接能力。
 */
async function addToDesktop(f) {
  const yes = await confirmDialog({
    title: '放到桌面',
    message: `为「${f.name}」在本机 :${f.listen_port} 创建一个桌面图标？`,
    detail: '目标只有是网页服务时图标才有意义（SSH、数据库这类无法在桌面窗口里打开）。',
    confirmText: '创建',
  });
  if (!yes) return;
  try {
    await api.links.create({
      name: f.name,
      kind: 'local',
      scheme: 'http',
      port: f.listen_port,
      path: '/',
      ui: 'iframe',
      enabled: true,
    });
    notify.ok('已创建桌面图标，可在「桌面应用」里看到');
    window.dispatchEvent(new CustomEvent('qlink:links-updated'));
  } catch (err) {
    notify.error(`创建失败：${err.message}`);
  }
}

/* -------------------------------------------------------------- 连接记录 */

async function showConnections(f) {
  let items = [];
  try {
    const res = await api.forwards.connections(f.id);
    items = res.items || [];
  } catch (err) {
    notify.error(err.message);
    return;
  }

  const body = h('div', {});
  if (!items.length) {
    append(body, h('p', { class: 'muted', text: '还没有连接记录。规则刚启用时这里是空的，' +
      '有客户端连进来之后才会出现。' }));
  } else {
    append(body, table(
      [
        { label: '时间', width: '18%' },
        { label: '来源', width: '20%' },
        { label: '结果', width: '14%' },
        { label: '↓/↑', width: '16%' },
        { label: '耗时', width: '10%' },
      ],
      items,
      (e) => h('tr', {}, [
        h('td', { class: 'cell-sub', text: fmtTime(e.at) }),
        h('td', { class: 'mono', text: e.source }),
        h('td', {}, [
          h('span', {
            class: `tag ${e.outcome === 'ok' ? 'ok' : e.outcome === 'denied' ? 'warn' : 'danger'}`,
            text: outcomeLabel(e.outcome),
          }),
        ]),
        h('td', { class: 'cell-sub', text: `${fmtBytes(e.bytes_in)} / ${fmtBytes(e.bytes_out)}` }),
        h('td', { class: 'cell-sub', text: `${e.duration_ms} ms` }),
      ]),
    ));
    const failed = items.filter((e) => e.error);
    if (failed.length) {
      append(body, h('div', { class: 'notice-bar danger mt-2' }, [
        h('div', { text: `最近 ${failed.length} 次失败：${failed[failed.length - 1].error}` }),
      ]));
    }
  }

  await modal({
    title: `「${f.name}」最近连接`,
    size: 'wide',
    body,
    footer: (close) => [
      h('button', { class: 'btn', type: 'button', onclick: () => close(null) }, ['关闭']),
    ],
  });
}

function outcomeLabel(o) {
  switch (o) {
    case 'ok': return '已完成';
    case 'denied': return '已拒绝';
    case 'dial_failed': return '目标不可达';
    case 'limited': return '已达上限';
    default: return o || '—';
  }
}

/* ------------------------------------------------------------------ 编辑 */

/** 打开规则编辑器；返回保存后的视图对象，取消则返回 null。 */
export function openEditor(existing = null) {
  const isEdit = !!(existing && existing.id);

  // 表单数据是一个普通对象：所有控件都直接读写它，提交时整体转成后端 DTO。
  const model = {
    name: existing?.name || '',
    desc: existing?.desc || '',
    protocol: existing?.protocol || 'tcp',
    listen_host: existing?.listen_host || '',
    listen_port: existing?.listen_port || 0,
    target_host: existing?.target_host || '',
    target_port: existing?.target_port || 0,
    access_mode: existing?.access_mode || '',
    access_list: (existing?.access_list || []).join('\n'),
    max_conns: existing?.max_conns || 0,
    enabled: existing?.enabled ?? true,
  };

  return modal({
    title: isEdit ? `编辑「${existing.name}」` : '新建转发规则',
    size: 'wide',
    body: (close) => {
      const box = h('div', { class: 'grid' });
      box.style.gap = '16px';

      /* ------------------------------------------------------ 基本信息 */
      append(box, h('div', { class: 'form-grid' }, [
        field('名称 *', h('input', {
          type: 'text', value: model.name, placeholder: '例如：客厅 NAS 远程桌面',
          oninput: (e) => { model.name = e.target.value; },
        })),
        field('说明', h('input', {
          type: 'text', value: model.desc, placeholder: '可选，方便日后回忆这条规则是干什么的',
          oninput: (e) => { model.desc = e.target.value; },
        })),
      ]));

      /* ---------------------------------------------------------- 协议 */
      append(box, h('div', { class: 'field' }, [
        h('label', { text: '协议' }),
        segmented(PROTOCOLS, model.protocol, (v) => { model.protocol = v; }),
        h('span', { class: 'hint', text: '不确定就选 TCP。DNS、部分游戏与实时音视频需要 UDP。' }),
      ]));

      /* -------------------------------------------------- 监听与目标 */
      append(box, h('div', { class: 'form-grid' }, [
        field('监听端口 *', h('input', {
          type: 'number', min: '1', max: '65535', value: model.listen_port || '',
          placeholder: '例如 13389',
          oninput: (e) => { model.listen_port = Number(e.target.value) || 0; },
        }), '本机上对外开放的端口'),
        field('监听地址', h('input', {
          type: 'text', value: model.listen_host, placeholder: '留空 = 全部地址（IPv4 + IPv6）',
          oninput: (e) => { model.listen_host = e.target.value; },
        }), '留空 = 自动适配：先按 IPv4 + IPv6 双栈监听，若这个端口有一侧被别的程序' +
          '占着，会自动退化成另一侧的单栈监听（列表里会标出来）。' +
          '只想让本机访问填 127.0.0.1；只给 IPv4 用填 0.0.0.0；只给 IPv6 用填 ::。'),
        field('目标主机 *', h('input', {
          type: 'text', value: model.target_host, placeholder: '例如 192.168.31.50',
          oninput: (e) => { model.target_host = e.target.value; },
        }), '可以是 IP，也可以是域名'),
        field('目标端口 *', h('input', {
          type: 'number', min: '1', max: '65535', value: model.target_port || '',
          placeholder: '例如 3389',
          oninput: (e) => { model.target_port = Number(e.target.value) || 0; },
        }), '目标服务真正在听的端口'),
      ]));

      /* ------------------------------------------------------ 访问控制 */
      const accessHint = h('span', { class: 'hint', id: 'access-hint', text: ACCESS_HINT[model.access_mode] });
      const listBox = h('div');
      const buildAccessList = () => {
        clear(listBox);
        accessHint.textContent = ACCESS_HINT[model.access_mode] || '';
        if (!model.access_mode) return;
        append(listBox, field('地址列表（每行一个）', h('textarea', {
          rows: '3', value: model.access_list,
          placeholder: '192.168.31.5\n192.168.31.0/24',
          style: { fontFamily: 'var(--font-mono, monospace)', fontSize: '.82rem' },
          oninput: (e) => { model.access_list = e.target.value; },
        }), '支持单个 IP 与 CIDR 网段，例如 10.0.0.0/8'));
      };
      buildAccessList();

      append(box, h('div', { class: 'field' }, [
        h('label', { text: '访问控制' }),
        segmented(ACCESS_MODES, model.access_mode, (v) => {
          model.access_mode = v;
          buildAccessList();
        }),
        accessHint,
        listBox,
      ]));

      /* ------------------------------------------------------ 高级项 */
      append(box, h('div', { class: 'form-grid' }, [
        field('并发上限', h('input', {
          type: 'number', min: '0', value: model.max_conns || 0,
          oninput: (e) => { model.max_conns = Number(e.target.value) || 0; },
        }), '0 = 不限制。用来兜住被扫描时的资源占用'),
        h('div', { class: 'field' }, [
          h('label', { text: '启用' }),
          h('div', { class: 'row' }, [
            switchBox(model.enabled, (v) => { model.enabled = v; }),
            h('span', { class: 'hint', text: '停用后规则保留，但会立刻释放端口' }),
          ]),
        ]),
      ]));

      append(box, h('p', { class: 'muted', style: { fontSize: '.78rem', margin: 0 }, text:
        '注意：目标不能是本机的同一个端口（会形成死循环），也不能与另一条规则' +
        '互为转发目标。这两种配置会在保存时被拒绝。' }));

      return box;
    },
    footer: (close) => [
      h('button', { class: 'btn', type: 'button', onclick: () => close(null) }, ['取消']),
      h('button', {
        class: 'btn primary', type: 'button',
        onclick: async (e) => { await submit(e.currentTarget, close); },
      }, [isEdit ? '保存' : '创建']),
    ],
  });

  async function submit(button, close) {
    if (!model.name.trim()) { notify.warn('请填写规则名称'); return; }
    if (!model.listen_port) { notify.warn('请填写监听端口'); return; }
    if (!model.target_host.trim()) { notify.warn('请填写目标主机'); return; }
    if (!model.target_port) { notify.warn('请填写目标端口'); return; }

    const payload = {
      name: model.name.trim(),
      desc: model.desc.trim(),
      protocol: model.protocol,
      listen_host: model.listen_host.trim(),
      listen_port: model.listen_port,
      target_host: model.target_host.trim(),
      target_port: model.target_port,
      access_mode: model.access_mode,
      access_list: model.access_list
        .split('\n').map((s) => s.trim()).filter(Boolean),
      max_conns: model.max_conns,
      enabled: model.enabled,
    };

    let res = null;
    try {
      await withBusy(button, async () => {
        res = isEdit
          ? await api.forwards.update(existing.id, payload)
          : await api.forwards.create(payload);
      });
    } catch (err) {
      notify.error(err.message);
      return;
    }

    close(res);
    await load();

    // 保存成功 ≠ 监听已经起来。目标还没启动、端口被别的程序占着，
    // 都属于"配置存下了但暂时没生效"，必须如实告诉用户，
    // 否则他会以为一切正常，直到发现连不上。
    if (res && res.warning) {
      notify.warn(`规则已保存，但未能启动：${res.warning}`, { ttl: 9000 });
    } else if (res && payload.enabled) {
      notify.ok('规则已保存并开始监听');
    } else {
      notify.ok('规则已保存（当前为停用状态）');
    }
  }
}
