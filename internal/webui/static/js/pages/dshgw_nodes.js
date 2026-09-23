// 「DSH 节点」页（M77）：把控制面 + 多台工作节点看成一个机群。
//
// 这一页要回答三个只靠命令行的运维答不上来的问题：
//   1. 现在有哪些节点、它们此刻是否活着、能不能提供这个部署需要的租户侧能力；
//   2. 某一台机器装到哪一步了（阶段表 + 日志尾部 + 主机指纹确认）；
//   3. 哪个租户跑在哪台机器上，以及怎么把它挪走。
//
// 两条与后端约定一致的行为，写在最前面以免后人改坏：
//   - 清单默认**不探测**（`probe=false`）：页面刷新不该变成对机群的负载；「探测」是一个显式按钮。
//   - 部署是**异步**的：POST 之后用进度端点轮询，指纹确认与失败现场都从那一份状态里读。
import { api } from '../api.js';
import {
  el, card, badge, toast, modal, confirmDialog, formatTime, table,
  modalHead, modalBody, modalActions, jsonBlock,
} from '../ui.js';

// customDialog 是框架里"只读内容"的弹窗形态（modal() 只会渲染表单字段）：部署抽屉与审计查看
// 都需要自己摆 DOM，所以用 ui.js 已导出的三件套拼一个，和 billing/codes 页保持一致。
function customDialog(heading, body, { wide = false } = {}) {
  const close = el('button', { class: 'btn', text: '关闭' });
  // 动作行由这里建好并交回调用方（而不是让调用方回头 querySelector 自己的 DOM）：
  // 需要另一种动作组合的调用方直接 replaceChildren，不必知道对话框内部的层次。
  const actions = modalActions([close]);
  const dialog = el('div', { class: 'modal', style: wide ? 'width:min(900px,100%)' : '' }, [
    modalHead(heading, () => backdrop.remove()),
    modalBody([body]),
    actions,
  ]);
  const backdrop = el('div', { class: 'modal-backdrop' }, [dialog]);
  close.addEventListener('click', () => backdrop.remove());
  document.getElementById('modal-root').append(backdrop);
  return { backdrop, dialog, actions, close };
}

// 状态文案：后端给的是机器状态词，这里把它们翻成运维看得懂的一句话，并决定徽标颜色。
const STATE_LABELS = {
  pending: ['未部署', 'warn'],
  deploying: ['部署中', 'warn'],
  ready: ['就绪', 'ok'],
  failed: ['部署失败', 'danger'],
  unreachable: ['不可达', 'danger'],
};

export function stateInfo(state) {
  const [label, kind] = STATE_LABELS[state] || [state || '未知', ''];
  return { label, kind };
}

// featureSummary 把节点的能力报告压成一行中文：运维扫一眼就知道这台机器能不能满足需要。
export function featureSummary(features) {
  const parts = [];
  if (!features) return '—';
  const plugins = Array.isArray(features.tenant_plugins) ? features.tenant_plugins : [];
  if (plugins.length > 0) parts.push('插件 ' + plugins.length);
  if (features.host_shares > 0) parts.push('主机目录 ' + features.host_shares);
  if (features.ssh_workspaces) parts.push('SSH 工作区');
  if (features.browser_workspaces) parts.push('浏览器目录');
  if (parts.length === 0) return '—';
  return parts.join(' · ');
}

// deployPhaseRows 把一次部署的阶段表翻成「名字 + 结果 + 耗时」，抽屉与轮询共用。
export function deployPhaseRows(status) {
  const phases = Array.isArray(status && status.phases) ? status.phases : [];
  return phases.map((phase) => ({
    name: phase.name,
    ok: phase.ok !== false,
    detail: phase.detail || '',
    millis: Number(phase.millis || 0),
  }));
}

// supervisionLabel 说明这台节点是「跟着机器重启」还是「只是现在活着」——两者是不同承诺。
export function supervisionLabel(status) {
  if (!status || status.running) return '';
  return status.systemd ? 'systemd 用户单元（重启机器会自动拉起）' : 'detached 启动（重启机器不会自动拉起）';
}

export async function render({ page, actions, session }) {
  const readonly = session.role !== 'admin';
  const refresh = el('button', { class: 'btn', text: '刷新' });
  const probeAll = el('button', { class: 'btn', text: '探测全部' });
  const add = el('button', { class: 'btn btn-primary', text: '添加节点', disabled: readonly });
  actions.append(refresh, probeAll, add);

  const nodeMeta = el('div', { class: 'grid' });
  // nodeCache 是最近一次清单：迁移弹窗的候选落点来自它，不额外请求。
  let nodeCache = [];
  const nodeTable = el('div');
  const tenantTable = el('div');
  page.append(card('工作节点', nodeTable, [
    el('span', { class: 'muted', text: '清单默认读记录里的最近结果；「探测全部」会真的连一遍每台机器' })]));
  page.append(card('DSH 租户与落点', tenantTable, [
    el('span', { class: 'muted', text: '租户数据在节点本地磁盘上：迁移只改落点，不搬文件' })]));
  page.append(card('说明', nodeMeta));

  refresh.addEventListener('click', () => load(false));
  probeAll.addEventListener('click', () => load(true));
  add.addEventListener('click', () => openAddDialog(() => load(false)));

  await load(false);

  async function load(probe) {
    try {
      const [nodes, tenants] = await Promise.all([
        api.get('/dshgw/nodes', probe ? { probe: 'true' } : undefined),
        api.get('/dshgw/tenants'),
      ]);
      nodeCache = nodes.nodes || [];
      renderNodes(nodeCache, nodes.probing === true);
      renderTenants(tenants.tenants || []);
      renderMeta(nodes.nodes || [], tenants.tenants || []);
    } catch (err) {
      toast(api.errorMessage(err), 'error');
    }
  }

  function renderMeta(nodes, tenants) {
    const ready = nodes.filter((node) => node.state === 'ready').length;
    const unreachable = nodes.filter((node) => node.state === 'unreachable' || node.reachable === false).length;
    const remote = tenants.filter((tenant) => tenant.node && tenant.node !== 'local').length;
    nodeMeta.replaceChildren(
      statLike('节点', String(nodes.length)),
      statLike('就绪', String(ready)),
      statLike('异常', String(unreachable)),
      statLike('分布在节点上的租户', String(remote)),
      statLike('本机租户', String(tenants.length - remote)),
    );
  }

  function renderNodes(nodes, probing) {
    nodeTable.replaceChildren(table({
      columns: [
        { key: 'name', label: '节点', render: (row) => el('span', {}, [
          el('strong', { text: row.name }),
          row.default ? el('span', { class: 'muted', text: ' · 默认' }) : null,
        ].filter(Boolean)) },
        { key: 'state', label: '状态', render: (row) => {
          const info = stateInfo(row.state);
          const parts = [badge(info.label, info.kind)];
          if (row.phase && row.state === 'deploying') parts.push(el('span', { class: 'muted', text: ' ' + row.phase }));
          return el('span', {}, parts);
        } },
        { key: 'source', label: '来源', render: (row) => el('span', { text: row.source === 'config' ? '配置文件' : '控制台' }) },
        { key: 'url', label: '地址', render: (row) => el('code', { text: row.url || '—' }) },
        { key: 'ssh', label: '目标机', render: (row) => el('code', { text: (row.ssh_host || '—') + (row.ssh_user ? ' @' + row.ssh_user : '') }) },
        { key: 'version', label: '版本', render: (row) => el('span', { text: row.revision ? shortRevision(row.revision) : '—' }) },
        { key: 'tenants', label: '租户', render: (row) => el('span', { text: String(row.tenants || 0) + (row.running ? ' / 运行 ' + row.running : '') }) },
        { key: 'features', label: '能力', render: (row) => el('span', { text: featureSummary(row.features) }) },
        { key: 'probed_at', label: '最近探测', render: (row) => el('span', { text: row.probed_at ? formatTime(row.probed_at) : '未探测' }) },
      ],
      rows: nodes,
      empty: probing ? '没有节点' : '还没有注册任何工作节点（本部署的租户都在控制面本机）',
      rowActions: (row) => {
        const buttons = [
          el('button', { class: 'btn', text: '探测', onclick: () => probeOne(row) }),
          el('button', { class: 'btn', text: '部署进度', onclick: () => openDeployDrawer(row.name, true) }),
          el('button', { class: 'btn', text: '审计', onclick: () => openAudit(row) }),
        ];
        if (!readonly) {
          buttons.push(el('button', { class: 'btn', text: '部署/升级', onclick: () => startDeploy(row.name) }));
          buttons.push(el('button', { class: 'btn', text: '轮换令牌', onclick: () => rotateToken(row.name) }));
          buttons.push(el('button', { class: 'btn', text: '对账', onclick: () => reconcile(row.name) }));
          buttons.push(el('button', { class: 'btn', text: '编辑', onclick: () => openEditDialog(row) }));
          buttons.push(el('button', { class: 'btn btn-danger', text: '删除', onclick: () => removeNode(row) }));
        }
        return buttons;
      },
    }).node);
  }

  function renderTenants(tenants) {
    tenantTable.replaceChildren(table({
      columns: [
        { key: 'name', label: '租户', render: (row) => el('strong', { text: row.name }) },
        { key: 'account', label: '账户', render: (row) => el('span', { text: row.account || '—' }) },
        { key: 'node', label: '节点', render: (row) => el('span', { text: row.node || 'local' }) },
        { key: 'running', label: '运行', render: (row) => badge(row.running ? '运行中' : '已停止', row.running ? 'ok' : 'warn') },
        { key: 'suspended', label: '停用标记', render: (row) => el('span', { text: row.suspended ? '是' : '否' }) },
        { key: 'ports', label: '端口', render: (row) => el('code', { text: String(row.public_port) + ' → ' + String(row.worker_port) }) },
        { key: 'handshake', label: '握手', render: (row) => el('span', { text: row.handshake || '—' }) },
        { key: 'last_login', label: '最后登录', render: (row) => el('span', { text: row.last_login ? formatTime(row.last_login) : '—' }) },
      ],
      rows: tenants,
      empty: '还没有 DSH 租户',
      rowActions: (row) => {
        const buttons = [];
        if (row.tenant_url) {
          buttons.push(el('a', { class: 'btn', href: row.tenant_url, target: '_blank', rel: 'noreferrer', text: '打开' }));
        }
        if (!readonly) {
          buttons.push(el('button', { class: 'btn', text: '重启', onclick: () => restartTenant(row) }));
          buttons.push(el('button', { class: 'btn', text: '迁移落点', onclick: () => moveTenant(row) }));
        }
        return buttons;
      },
    }).node);
  }

  async function probeOne(row) {
    try {
      const payload = await api.post('/dshgw/nodes/' + encodeURIComponent(row.name) + '/probe', {});
      const view = payload.node || {};
      if (payload.reachable === false) {
        toast(row.name + ' 不可达：' + (payload.error || view.error || '未知原因'), 'error');
      } else {
        toast(row.name + ' 可达（' + (view.revision ? shortRevision(view.revision) : 'unknown') + '）', 'ok');
      }
      load(false);
    } catch (err) {
      toast(api.errorMessage(err), 'error');
    }
  }

  async function reconcile(name) {
    try {
      const payload = await api.post('/dshgw/nodes/' + encodeURIComponent(name) + '/reconcile', {});
      const result = payload.result || {};
      const started = (result.started || []).length;
      const stopped = (result.stopped || []).length;
      const pruned = (result.pruned || []).length;
      toast('对账完成：启动 ' + started + '，停止 ' + stopped + '，剪除 ' + pruned, 'ok');
      load(false);
    } catch (err) {
      toast(api.errorMessage(err), 'error');
    }
  }

  async function rotateToken(name) {
    const ok = await confirmDialog('轮换节点令牌', '将同时更新 ' + name + ' 上的令牌文件与控制面记录，' +
      '部署期间该节点的租户流量会短暂失败。继续？');
    if (!ok) return;
    await deployRequest(name, '/dshgw/nodes/' + encodeURIComponent(name) + '/rotate-token', {});
  }

  // startDeploy 一次点击 = 一次部署；指纹门在同一处被处理（后端拒绝 + 指纹在错误里）。
  async function startDeploy(name) {
    await deployRequest(name, '/dshgw/nodes/' + encodeURIComponent(name) + '/deploy', {});
  }

  async function deployRequest(name, path, body) {
    try {
      await api.post(path, body);
    } catch (err) {
      const message = api.errorMessage(err);
      const fingerprint = (message.match(/SHA256:[A-Za-z0-9+/=]+/) || [])[0];
      if (fingerprint) {
        // 首次部署必须由人确认主机密钥：把它作为一个明确的决定呈现，而不是让运维去日志里找。
        const host = name;
        const ok = await confirmDialog('确认 ' + host + ' 的主机密钥',
          '这台机器报出的指纹是 ' + fingerprint + '。确认它与你在目标机上核对过的一致后才会开始上传。');
        if (!ok) return;
        await deployRequest(name, path, Object.assign({}, body, { accept_host_key: fingerprint }));
        return;
      }
      toast(message, 'error');
      return;
    }
    openDeployDrawer(name, false);
  }

  async function openAudit(row) {
    try {
      const payload = await api.get('/dshgw/nodes/' + encodeURIComponent(row.name) + '/audit', { lines: 100 });
      const lines = payload.lines || [];
      const body = el('div', {}, lines.length === 0
        ? [el('div', { class: 'muted', text: '这台节点还没有审计事件' })]
        : lines.map((line) => {
          let parsed = line;
          try { parsed = JSON.parse(line); } catch (err) { /* 原样显示一行读不懂的记录 */ }
          return jsonBlock(parsed);
        }));
      customDialog(row.name + ' 最近的安全事件（' + lines.length + '）', body, { wide: true });
    } catch (err) {
      toast(api.errorMessage(err), 'error');
    }
  }

  // openDeployDrawer 是「部署到哪一步了」的窗口：阶段表 + 日志尾部 + 指纹 + 监督方式。
  // 部署进行中时每 1.5 秒轮询一次，结束后停止（没有后台定时器）。
  async function openDeployDrawer(name, openOnly) {
    const phases = el('div', {}, []);
    const log = el('pre', { class: 'json' });
    const stateLine = el('div', { class: 'muted' });
    const body = el('div', {}, [
      el('strong', { text: '节点 ' + name }),
      stateLine,
      el('div', { class: 'muted', text: '阶段' }),
      phases,
      el('div', { class: 'muted', text: openOnly ? '日志尾部' : '日志尾部（部署进行中会持续刷新）' }),
      log,
    ]);
    const dialog = customDialog('部署进度', body, { wide: true });
    let timer = null;
    let stopped = false;
    dialog.close.addEventListener('click', () => { stopped = true; if (timer) clearTimeout(timer); });
    await poll();
    async function poll() {
      if (stopped) return;
      try {
        const payload = await api.get('/dshgw/nodes/' + encodeURIComponent(name) + '/deploy');
        const status = payload.deploy || {};
        renderStatus(status);
        if (status.running) timer = setTimeout(poll, 1500);
        else load(false);
      } catch (err) {
        stateLine.textContent = api.errorMessage(err);
      }
    }
    function renderStatus(status) {
      void openOnly;
      const rows = deployPhaseRows(status);
      phases.replaceChildren(...rows.map((row) => el('div', {}, [
        badge(row.ok ? 'ok' : 'FAILED', row.ok ? 'ok' : 'danger'),
        el('span', { text: ' ' + row.name + (row.detail ? ' — ' + row.detail : '') }),
        el('span', { class: 'muted', text: row.millis ? '  ' + row.millis + 'ms' : '' }),
      ])));
      const info = stateInfo(status.state);
      stateLine.textContent = [
        info.label,
        status.phase && status.running ? '阶段 ' + status.phase : '',
        status.fingerprint ? '指纹 ' + status.fingerprint : '',
        status.rotated_token ? '（本次轮换了令牌）' : '',
        supervisionLabel(status),
        status.error ? '错误：' + status.error : '',
      ].filter(Boolean).join(' · ');
      log.textContent = status.log_tail || '（暂无日志）';
    }
  }

  function openAddDialog(done) {
    modal({
      title: '添加工作节点',
      submitLabel: '注册',
      wide: true,
      fields: [
        { name: 'name', label: '节点名', value: '', placeholder: 'node-b（^[a-z][a-z0-9-]{0,25}$，不能是 local）' },
        { name: 'listen', label: '节点监听地址', value: '', placeholder: '192.168.190.87:18400（host:port，不能是 0.0.0.0）' },
        { name: 'ssh_host', label: 'ssh 主机', value: '', placeholder: '192.168.190.87' },
        { name: 'ssh_user', label: 'ssh 账号', value: '', placeholder: 'winger（只支持密钥认证）' },
        { name: 'ssh_port', label: 'ssh 端口', value: '22' },
        { name: 'ssh_key_file', label: '私钥路径（控制面）', value: '', placeholder: '留空=<state>/node-ssh/<节点名>/id_ed25519' },
        { name: 'deploy_dir', label: '部署目录（目标机）', value: '', placeholder: '留空=/srv/dshgw-node' },
        { name: 'node_bin', label: 'Node 路径（目标机）', value: '', placeholder: '留空=与预检一致' },
        { name: 'bin_js', label: 'dsh 启动脚本（目标机）', value: '', placeholder: '/opt/dsh/lib/bin.js' },
        { name: 'worker_port_lo', label: 'worker 端口下界', value: '' },
        { name: 'worker_port_hi', label: 'worker 端口上界', value: '' },
        { name: 'default', label: '设为默认落点（true/false）', value: 'false' },
      ],
      onSubmit: async (values) => {
        const body = {};
        const numeric = ['ssh_port', 'worker_port_lo', 'worker_port_hi'];
        Object.keys(values).forEach((key) => {
          const value = String(values[key] == null ? '' : values[key]).trim();
          if (value === '') return;
          if (key === 'default') { body.default = value === 'true'; return; }
          if (numeric.indexOf(key) >= 0) { body[key] = Number(value); return; }
          body[key] = value;
        });
        try {
          await api.post('/dshgw/nodes', body);
          toast('已注册 ' + body.name + '；下一步用「部署/升级」安装', 'ok');
          done();
          return true;
        } catch (err) {
          toast(api.errorMessage(err), 'error');
          return false;
        }
      },
    });
  }

  function openEditDialog(row) {
    modal({
      title: '编辑节点 ' + row.name,
      submitLabel: '保存',
      wide: true,
      fields: [
        { name: 'listen', label: '节点监听地址', value: row.listen || '' },
        { name: 'ssh_host', label: 'ssh 主机', value: row.ssh_host || '' },
        { name: 'ssh_user', label: 'ssh 账号', value: row.ssh_user || '' },
        { name: 'deploy_dir', label: '部署目录（目标机）', value: row.deploy_dir || '' },
        { name: 'node_bin', label: 'Node 路径（目标机）', value: '', placeholder: '留空=不变' },
        { name: 'bin_js', label: 'dsh 启动脚本（目标机）', value: '', placeholder: '留空=不变' },
        { name: 'default', label: '设为默认落点（true/false）', value: 'false' },
      ],
      onSubmit: async (values) => {
        const body = {};
        Object.keys(values).forEach((key) => {
          const value = String(values[key] == null ? '' : values[key]).trim();
          if (value === '') return;
          body[key] = key === 'default' ? value === 'true' : value;
        });
        try {
          await api.patch('/dshgw/nodes/' + encodeURIComponent(row.name), body);
          toast('已保存（路径变了要重新部署）', 'ok');
          load(false);
          return true;
        } catch (err) {
          toast(api.errorMessage(err), 'error');
          return false;
        }
      },
    });
  }

  // removeNode 把两种删除摆成两个明确的按钮：「忘记这台机器」（目标机不动）与「彻底删除」
  // （停掉它并删掉目标机上的部署与数据）。合成一个"确定要删除吗"会让运维在下一次点击里失去一半语义。
  function removeNode(row) {
    const drop = el('button', { class: 'btn btn-danger', text: '彻底删除（含目标机数据）' });
    const forget = el('button', { class: 'btn', text: '仅移除记录' });
    const body = el('div', {}, [
      el('p', { text: '「仅移除记录」只删除控制面的记录，' + row.ssh_host + ' 上的一切保持不变（再部署一次就能回来）。' }),
      el('p', { text: '「彻底删除」会停止该节点、删除目标机上的部署产物与状态目录——包括它承载的租户数据，不可恢复。' }),
    ]);
    const dialog = customDialog('删除节点 ' + row.name, body);
    dialog.actions.replaceChildren(forget, drop, dialog.close);
    forget.addEventListener('click', async () => {
      try {
        await api.del('/dshgw/nodes/' + encodeURIComponent(row.name));
        toast('已移除记录，目标机未改动', 'ok');
        dialog.backdrop.remove();
        load(false);
      } catch (err) {
        toast(api.errorMessage(err), 'error');
      }
    });
    drop.addEventListener('click', () => purgeNode(row, dialog));
  }

  // purgeNode 需要把节点名再打一遍：这是唯一会删除租户数据的按钮。
  function purgeNode(row, parent) {
    modal({
      title: '彻底删除 ' + row.name,
      submitLabel: '删除并清空目标机',
      fields: [
        { name: 'confirm', label: '输入节点名以确认', value: '', required: true, placeholder: row.name },
      ],
      onSubmit: async (values) => {
        if (String(values.confirm).trim() !== row.name) {
          toast('节点名不匹配，未执行', 'error');
          return false;
        }
        try {
          await api.del('/dshgw/nodes/' + encodeURIComponent(row.name) + '?purge=true&confirm=' + encodeURIComponent(row.name));
          toast('已停掉并清空 ' + row.name, 'ok');
          if (parent) parent.backdrop.remove();
          load(false);
          return true;
        } catch (err) {
          toast(api.errorMessage(err), 'error');
          return false;
        }
      },
    });
  }

  async function restartTenant(row) {
    try {
      await api.post('/dshgw/tenants/' + encodeURIComponent(row.name) + '/restart', {});
      toast('已重启 ' + row.name, 'ok');
      load(false);
    } catch (err) {
      toast(api.errorMessage(err), 'error');
    }
  }

  // moveTenant 只改落点，并把「数据要自己搬」写在弹窗里——这是本页最容易误解的一步。
  function moveTenant(row) {
    modal({
      title: '迁移 ' + row.name + ' 的落点',
      submitLabel: '保存落点',
      fields: [
        { name: 'node', label: '目标节点', type: 'select', value: row.node || 'local',
          options: knownNodeNames(row.node), hint: 'local = 控制面本机' },
      ],
      onSubmit: async (values) => {
        const node = String(values.node || '').trim();
        if (node === '') { toast('请填写目标节点', 'error'); return false; }
        const ok = await confirmDialog('确认迁移落点',
          '这个接口**不会搬运数据**：' + row.name + ' 的工作区还在原来那台机器的磁盘上。' +
          '正确顺序是先停租户、把数据拷到新节点、再改落点、最后启动。继续？');
        if (!ok) return false;
        try {
          await api.post('/dshgw/tenants/' + encodeURIComponent(row.name) + '/node', { node });
          toast('落点已改为 ' + node, 'ok');
          load(false);
          return true;
        } catch (err) {
          toast(api.errorMessage(err), 'error');
          return false;
        }
      },
    });
  }

  // knownNodeNames 是本页已知的落点清单（含当前落点与 local），让迁移是一次选择而不是一次拼写。
  function knownNodeNames(current) {
    const names = ['local'];
    nodeCache.forEach((node) => { if (names.indexOf(node.name) < 0) names.push(node.name); });
    if (current && names.indexOf(current) < 0) names.push(current);
    return names;
  }

  function statLike(label, value) {
    return el('div', { class: 'stat' }, [el('div', { class: 'k', text: label }), el('div', { class: 'v', text: value })]);
  }

  function shortRevision(revision) {
    const text = String(revision || '');
    return text.length > 7 ? text.slice(0, 7) : text;
  }

}
