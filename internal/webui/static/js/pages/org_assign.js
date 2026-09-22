// 组织归属选择器：一棵组织树 + 每行一个勾选框。账户页的「新建/编辑账户」与组织页人员行的
// 「分配组织」共用这一份实现——两条入口操作的是同一个字段（`PATCH /accounts/{id}` 的
// `org_node_ids`），抄成两份迟早有一份忘了"整表替换"这条语义。
//
// 为什么是勾选树，而不是原来的「组织节点 id（逗号分隔）」：
//   * 归属是**逐节点的直接关系**（子树继承的是节点**标签**，不是成员），所以每个节点自己一个勾选框
//     就够了，不需要父子连带、也不需要半选——勾父节点不会顺手把人塞进它的每个子部门；
//   * 组织是一棵树，"这是哪个分公司的哪个组"只有画成层级才看得出来，而节点 id 对人没有意义。
import { api } from '../api.js';
import { el, modalHead, modalBody, modalActions } from '../ui.js';
import { tree } from '../tree.js';
import { matchesQuery } from '../pinyin.js';

// openOrgPicker 让操作员勾出这个账号的归属。返回 { ids, refs }，取消时返回 null。
//
//   title     弹窗标题，默认「分配组织」
//   nodeIds   当前归属（预勾选）
//   onSubmit  给了就由它落库（失败在弹窗内报错、弹窗不关，弹窗不关才有机会改）；不给就把勾选
//             结果交回调用方（表单场景：由表单自己的「保存」落库）。与 ui.js 的 modal() 同一口径。
export function openOrgPicker({ title, nodeIds = [], onSubmit } = {}) {
  const selected = new Set(nodeIds.map(Number).filter((id) => Number.isInteger(id)));
  let nodes = [];
  // 勾选框按节点 id 登记：树每次 refresh 都会重建整行（连带勾选框），登记表让重绘后仍能回填状态。
  let boxes = new Map();

  const scopeLabel = el('span', { class: 'muted' });
  const notice = el('div', { class: 'org-assign-notice' });
  const loading = el('div', { class: 'muted', text: '正在读取组织节点…' });
  const treeHost = el('div', { class: 'org-assign-tree' });
  const save = el('button', { class: 'btn btn-primary', text: '保存', disabled: true });
  const clear = el('button', { class: 'btn', text: '清空' });

  const view = tree({
    mode: 'workspace',
    filter: true,
    filterPlaceholder: '过滤节点（支持拼音）…',
    matcher: matchesQuery,
    emptyText: '暂无组织节点',
    renderLabel: (node) => el('span', { class: 'org-assign-label' }, [
      assignBox(node),
      el('span', { class: 'tree-label', text: node.name }),
    ]),
    // 同名节点在不同父节点下是允许的（两个分公司都可以有「研发部」），所以把路径挂在行尾——
    // 只靠缩进，在过滤后（祖先被折叠掉）就看不出是哪一个了。
    renderMeta: (node) => (node.depth > 0 ? el('span', { class: 'muted', text: node.path }) : null),
    // 这一列勾选框才是控件；点整行只移动焦点（与主树一致），不改归属。
    onSelect: () => {},
  });
  // 进度行与树是兄弟：不能靠"清空容器再放树"来显示进度——那会在真正读数据**之前**先 refresh([])，
  // 而树控件用第一次 refresh 播种展开态（首次拿到空列表 = 之后只剩根节点可见，子节点永远是收起的）。
  treeHost.append(loading, view.node);

  function assignBox(node) {
    const box = el('input', { type: 'checkbox', class: 'org-assign-check' });
    // 建出来就带上当前状态：树自己在过滤/折叠时会重建行（连带勾选框），那时 renderScope() 并不
    // 会被调用——不在这里补一笔，过滤一次屏幕上就变成"全都没勾"，而集合里其实还勾着。
    box.checked = selected.has(node.id);
    // 点勾选框不该同时"选中这一行"：树的委托监听在根上，不拦住就会顺带 select。
    box.addEventListener('click', (ev) => { ev.stopPropagation(); });
    box.addEventListener('change', () => {
      if (box.checked) selected.add(node.id); else selected.delete(node.id);
      renderScope();
    });
    boxes.set(node.id, box);
    return box;
  }

  function nodeById(id) {
    return nodes.find((node) => node.id === id) || null;
  }

  // renderScope 是唯一决定"能不能保存、保存什么"的地方，也把后果写在看得见的地方。
  function renderScope() {
    for (const [id, box] of boxes) box.checked = selected.has(id);
    scopeLabel.textContent = '已选 ' + selected.size + ' / ' + nodes.length + ' 个节点';
    if (selected.size) {
      notice.className = 'org-assign-notice';
      notice.textContent = '';
    } else if (nodes.length) {
      // 空选不是"什么都没做"：整表替换语义下它就是"移出全部组织"，后果必须写在屏幕上。
      notice.className = 'org-assign-notice warn';
      notice.textContent = '未选择任何节点：保存后该账号会从全部组织移出，从节点继承来的标签授权随即失效。';
    } else {
      notice.className = 'org-assign-notice';
      notice.textContent = '';
    }
    save.disabled = nodes.length === 0;
    save.title = nodes.length === 0 ? '没有可分配的组织节点（先在左侧新建根节点）' : '';
  }

  async function load() {
    save.disabled = true;
    try {
      const payload = await api.get('/org/nodes', { limit: 1000 });
      nodes = payload.data || [];
    } catch (err) {
      nodes = [];
      notice.className = 'org-assign-notice error';
      // 服务端的原话照抄：本部署没接组织端口时它说的是"unsupported"，改写只会让人查错方向。
      notice.textContent = '读取组织节点失败：' + api.errorMessage(err);
      loading.remove();
      treeHost.append(el('div', { class: 'empty', text: '组织节点读不到，暂时无法分配' }));
      renderScope();
      return;
    }
    loading.remove();
    boxes = new Map();
    if (!nodes.length) {
      treeHost.append(el('div', { class: 'empty', text: '本部署还没有组织节点：先在组织架构页新建一个根节点' }));
    } else {
      view.refresh(nodes);
    }
    renderScope();
  }

  return new Promise((resolve) => {
    const close = (value) => { backdrop.remove(); resolve(value); };
    save.addEventListener('click', async () => {
      const ids = [...selected].sort((left, right) => left - right);
      const refs = ids.map(nodeById).filter(Boolean);
      if (onSubmit) {
        save.disabled = true;
        try {
          await onSubmit(ids);
        } catch (err) {
          // 弹窗不关：改错了还能改回来，关掉就等于让人重来一遍（节点可能刚被别人删了）。
          notice.className = 'org-assign-notice error';
          notice.textContent = api.errorMessage(err);
          save.disabled = false;
          return;
        }
      }
      close({ ids, refs });
    });
    clear.addEventListener('click', () => { selected.clear(); renderScope(); });

    const dialog = el('div', { class: 'modal org-picker-dialog' }, [
      modalHead(title || '分配组织', () => close(null)),
      modalBody([
        el('div', { class: 'muted', text: '勾选这个账号要归属的组织节点。保存是整表替换：没勾的节点会被移出，账号从这些节点继承来的标签授权随即失效。' }),
        el('div', { class: 'toolbar org-assign-bar' }, [
          scopeLabel,
          el('span', { class: 'muted org-assign-hint', text: '账号可同时属于多个节点；勾选只作用于该节点本身，不会连带勾选它的子节点（节点标签由整棵子树继承）' }),
          el('span', { class: 'org-assign-actions' }, [clear]),
        ]),
        notice,
        treeHost,
      ]),
      modalActions([
        el('button', { class: 'btn', text: '取消', onclick: () => close(null) }),
        save,
      ]),
    ]);
    const backdrop = el('div', { class: 'modal-backdrop' }, [dialog]);
    backdrop.addEventListener('click', (ev) => { if (ev.target === backdrop) close(null); });
    document.getElementById('modal-root').append(backdrop);
    load();
  });
}
