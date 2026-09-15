// A reusable tree view.
//
// It knows nothing about the organization structure, about the API, or about any page: it
// takes a flat array of nodes plus callbacks and renders a hierarchy. That is what makes the
// same control usable both in the sidebar (compact, for navigating) and in a page body (full,
// with row actions and metadata) — the only difference between the two placements is `mode`,
// which changes density and how much secondary information is shown, never the data or the
// interaction.
//
// Data contract: nodes are flat, each carrying `id` and `parent_id` (null/undefined/0 for a
// root), plus whatever renderLabel/renderMeta/actions need. Sibling order is the caller's:
// the array is used as given, so a server-side ORDER BY is honoured. Any other hierarchy
// (chat sessions, model routes) can pass the same shape.
//
// Implementation notes:
//   - Only visible rows exist in the DOM, so collapsing a large subtree frees it.
//   - One delegated click listener on the root, not one per row.
//   - refresh(nodes) rebuilds the visible rows. Deliberate: the lists this renders are tens to
//     hundreds of nodes, where rebuilding is simpler than diffing and cannot drift.
//   - Keyboard support follows the ARIA tree pattern with a roving tabindex: one row is
//     focusable at a time, arrows move/expand/collapse, Enter or Space selects.

import { el, clear } from './ui.js';

const INDENT = 14;

export function tree({
  nodes = [],
  selectedId = null,
  mode = 'workspace',
  expandDepth = Infinity,
  collapsible = true,
  filter = mode === 'workspace',
  filterPlaceholder = '过滤…',
  renderLabel,
  renderMeta,
  actions,
  emptyText = '暂无数据',
  onSelect,
  onToggle,
  onAction,
} = {}) {
  const compact = mode === 'sidebar';
  const state = {
    nodes: [],
    // Ids whose children are shown. A node absent from this set is collapsed.
    expanded: new Set(),
    selected: selectedId === undefined ? null : selectedId,
    // The row that owns the roving tabindex. Keyboard navigation moves it independently of the
    // selection, so arrowing through the tree does not fire a selection per keystroke.
    focus: null,
    query: '',
    // Set once the caller supplies data, so later refreshes keep the operator's expansion.
    seeded: false,
  };

  const root = el('div', {
    class: 'tree tree-' + (compact ? 'sidebar' : 'workspace'),
    role: 'tree',
    'aria-label': compact ? '树形导航' : '树形结构',
  });
  const body = el('div', { class: 'tree-body' });
  let search = null;

  if (filter) {
    search = el('input', { class: 'tree-filter', type: 'search', placeholder: filterPlaceholder });
    search.addEventListener('input', () => {
      state.query = search.value.trim().toLowerCase();
      render();
    });
    root.append(search);
  }
  root.append(body);

  // --- model --------------------------------------------------------------

  // indexOf groups the flat list into children lists. Ids are used as map keys, so an id that
  // appears twice keeps its first row (the caller's data is not ours to repair here).
  function indexOf(list) {
    const byId = new Map();
    const children = new Map();
    for (const node of list) {
      if (!node || node.id === undefined || node.id === null || byId.has(node.id)) continue;
      byId.set(node.id, node);
      const parent = parentOf(node);
      if (!children.has(parent)) children.set(parent, []);
      children.get(parent).push(node.id);
    }
    return { byId, children };
  }

  function parentOf(node) {
    return node.parent_id === undefined || node.parent_id === null ? 0 : node.parent_id;
  }

  function findNode(id) {
    if (id === null || id === undefined) return null;
    return state.nodes.find((node) => node && String(node.id) === String(id)) || null;
  }

  function hasChildren(id) {
    return state.nodes.some((node) => node && parentOf(node) === id);
  }

  function labelOf(node) {
    return renderLabel ? renderLabel(node) : node.name;
  }

  function matches(node) {
    if (!state.query) return true;
    return String(labelOf(node)).toLowerCase().includes(state.query);
  }

  // visibleRows returns the rows to draw, in render order, each with its depth.
  //
  // Two modes. Normally a depth-first walk that does not descend into collapsed nodes. While a
  // filter is active, the matched nodes plus their ancestors are shown and expansion is
  // ignored — a filter that hid its own matches behind a collapsed parent would be useless.
  function visibleRows() {
    const { byId, children } = indexOf(state.nodes);
    const rows = [];
    const visible = new Set();

    if (state.query) {
      const keep = new Set();
      for (const node of byId.values()) {
        if (!matches(node)) continue;
        // The match and every ancestor of it.
        let cursor = node;
        const guard = new Set();
        while (cursor && !guard.has(cursor.id)) {
          guard.add(cursor.id);
          keep.add(cursor.id);
          cursor = byId.get(parentOf(cursor));
        }
      }
      const walk = (id, depth) => {
        const node = byId.get(id);
        if (!node || !keep.has(id)) return;
        rows.push({ id, node, depth });
        for (const child of children.get(id) || []) walk(child, depth + 1);
      };
      for (const rootId of children.get(0) || []) walk(rootId, 0);
      // Nodes whose parent is missing are shown as roots rather than dropped, so dangling data
      // is visible instead of silently absent.
      for (const [id, node] of byId) {
        if (keep.has(id) && !rows.some((row) => row.id === id)) rows.push({ id, node, depth: 0 });
      }
      return rows;
    }

    const walk = (id, depth) => {
      const node = byId.get(id);
      if (!node) return;
      visible.add(id);
      rows.push({ id, node, depth });
      if (!state.expanded.has(id)) return;
      for (const child of children.get(id) || []) walk(child, depth + 1);
    };
    for (const rootId of children.get(0) || []) walk(rootId, 0);
    // A node the walk never reached is either hidden by a collapse (normal) or unreachable from
    // any root (a cycle, or a parent that is not in the list). Only the second case is a
    // problem, and the two must be told apart by reachability rather than by what got drawn —
    // treating every undrawn node as unreachable would re-show collapsed children.
    for (const [id, node] of byId) {
      if (visible.has(id) || reachable(byId, id)) continue;
      visible.add(id);
      rows.push({ id, node, depth: 0 });
    }
    return rows;
  }

  // reachable reports whether id leads up to a root at all, ignoring expansion state.
  function reachable(byId, id) {
    const guard = new Set();
    let cursor = byId.get(id);
    while (cursor) {
      if (guard.has(cursor.id)) return false; // a cycle never reaches a root
      guard.add(cursor.id);
      const parent = parentOf(cursor);
      if (parent === 0) return true;
      cursor = byId.get(parent);
    }
    return false;
  }

  // --- render -------------------------------------------------------------

  function render() {
    const rows = visibleRows();
    const focusId = rows.some((row) => String(row.id) === String(state.focus)) ? state.focus : (rows[0] ? rows[0].id : null);
    state.focus = focusId;

    clear(body);
    if (!rows.length) {
      body.append(el('div', { class: 'tree-empty empty', text: state.query ? '无匹配节点' : emptyText }));
      return;
    }
    for (const row of rows) body.append(renderRow(row, focusId));
  }

  function renderRow({ id, node, depth }, focusId) {
    const kids = hasChildren(id);
    const expanded = state.expanded.has(id);
    const selected = String(state.selected) === String(id);
    const toggle = el('span', {
      class: 'tree-toggle' + (kids ? '' : ' leaf') + (expanded && kids ? ' open' : ''),
      text: kids ? (expanded ? '▾' : '▸') : '·',
      'aria-hidden': 'true',
    });
    const label = labelOf(node);
    const meta = !compact && renderMeta ? renderMeta(node) : null;
    const rowActions = !compact && actions ? actions(node) : null;
    return el('div', {
      class: 'tree-row' + (selected ? ' selected' : '') + (kids ? '' : ' leaf'),
      role: 'treeitem',
      'aria-level': String(depth + 1),
      'aria-selected': selected ? 'true' : 'false',
      'aria-expanded': kids ? String(expanded) : null,
      tabindex: String(String(focusId) === String(id) ? 0 : -1),
      dataset: { id: String(id) },
      style: 'padding-left:' + (8 + depth * INDENT) + 'px',
    }, [
      toggle,
      typeof label === 'string' ? el('span', { class: 'tree-label', text: label }) : label,
      // Secondary information is hidden in the compact placement: the sidebar has no room for
      // it, and the full placement is where an operator reads it.
      meta === null || meta === undefined ? null : el('span', { class: 'tree-meta' }, [
        typeof meta === 'string' || typeof meta === 'number' ? String(meta) : meta,
      ]),
      rowActions && rowActions.length ? el('span', { class: 'tree-actions' }, rowActions) : null,
    ]);
  }

  // applyFocus moves the roving tabindex and the browser focus together, without re-rendering:
  // a re-render would drop focus and make arrowing feel like it restarts.
  function applyFocus() {
    for (const row of body.querySelectorAll('.tree-row')) {
      row.tabIndex = String(row.dataset.id) === String(state.focus) ? 0 : -1;
    }
    const target = body.querySelector('.tree-row[data-id="' + cssEscape(String(state.focus)) + '"]');
    if (target && document.activeElement !== target) target.focus();
  }

  // --- interaction --------------------------------------------------------

  function setExpanded(id, expanded) {
    if (expanded) state.expanded.add(id);
    else state.expanded.delete(id);
    render();
    const node = findNode(id);
    if (node && onToggle) onToggle(node, expanded);
  }

  function select(id, notify) {
    state.selected = id;
    state.focus = id;
    render();
    applyFocus();
    if (notify && onSelect) onSelect(findNode(id));
  }

  root.addEventListener('click', (ev) => {
    const row = ev.target.closest ? ev.target.closest('.tree-row') : null;
    if (!row || !body.contains(row)) return;
    const node = findNode(row.dataset.id);
    if (!node) return;
    // Row actions carry their own listeners through onAction; a click inside that group must
    // not also select or toggle the row it sits in.
    const actionHost = ev.target.closest ? ev.target.closest('.tree-actions') : null;
    if (actionHost && row.contains(actionHost)) {
      const button = ev.target.closest('.tree-action');
      if (button && onAction) onAction(button.dataset.action, node);
      return;
    }
    if (collapsible && ev.target.closest('.tree-toggle') && hasChildren(node.id)) {
      setExpanded(node.id, !state.expanded.has(node.id));
      return;
    }
    select(node.id, true);
  });

  root.addEventListener('keydown', (ev) => {
    const rows = visibleRows();
    // The position comes from the row the browser actually focused when there is one: focus can
    // arrive by Tab, by a click, or by the page calling focus() directly, and in every one of
    // those cases that row — not the last selection — is where the operator is looking.
    let current = state.focus;
    const focused = document.activeElement;
    if (focused && focused.classList && focused.classList.contains('tree-row') && root.contains(focused)) {
      current = focused.dataset.id;
      if (String(state.focus) !== String(current)) state.focus = current;
    }
    const at = rows.findIndex((row) => String(row.id) === String(current));
    if (at < 0) return;
    const row = rows[at];
    switch (ev.key) {
      case 'ArrowDown':
        if (at + 1 < rows.length) state.focus = rows[at + 1].id;
        break;
      case 'ArrowUp':
        if (at > 0) state.focus = rows[at - 1].id;
        break;
      case 'ArrowRight':
        // Open a closed node, otherwise step into its first child.
        if (collapsible && hasChildren(row.id) && !state.expanded.has(row.id)) {
          setExpanded(row.id, true);
          // setExpanded re-renders, so the focused element it replaced is gone. Without this the
          // browser drops focus to <body> and every later keystroke is dispatched outside the
          // tree — keyboard navigation would work exactly once.
          applyFocus();
          return prevent(ev);
        }
        if (at + 1 < rows.length && rows[at + 1].depth > row.depth) state.focus = rows[at + 1].id;
        break;
      case 'ArrowLeft': {
        // Close an open node, otherwise step out to the parent.
        if (collapsible && state.expanded.has(row.id)) {
          setExpanded(row.id, false);
          applyFocus();
          return prevent(ev);
        }
        const parent = parentOf(row.node);
        if (parent !== 0 && findNode(parent)) state.focus = parent;
        break;
      }
      case 'Enter':
      case ' ':
        select(row.id, true);
        return prevent(ev);
      case 'Home':
        state.focus = rows[0].id;
        break;
      case 'End':
        state.focus = rows[rows.length - 1].id;
        break;
      default:
        return; // let every other key through (the filter box is a separate element)
    }
    applyFocus();
    prevent(ev);
  });

  // initialExpansion opens the ancestors of the requested depth: with expandDepth 1 the roots
  // are open, so the rows at depth 0 and 1 are visible and everything below starts collapsed.
  // A sidebar wants exactly that — the top of the structure, without burying the global
  // navigation under somebody's full org chart — while a page passes Infinity and opens all.
  function initialExpansion(list) {
    const expanded = new Set();
    const hasKids = (id) => list.some((other) => other && parentOf(other) === id);
    for (const node of list) {
      if (!node || !hasKids(node.id)) continue;
      if (expandDepth === Infinity || depthOf(node, list) < expandDepth) expanded.add(node.id);
    }
    return expanded;
  }

  function depthOf(node, list) {
    const byId = new Map(list.filter(Boolean).map((item) => [item.id, item]));
    let cursor = node;
    let depth = 0;
    const guard = new Set();
    while (cursor && parentOf(cursor) !== 0 && !guard.has(cursor.id)) {
      guard.add(cursor.id);
      cursor = byId.get(parentOf(cursor));
      depth += 1;
    }
    return depth;
  }

  return {
    node: root,
    // refresh replaces the data. The expansion of nodes that still exist is kept, so a reload
    // after an edit does not collapse the branch the operator is working in.
    refresh(next) {
      state.nodes = Array.isArray(next) ? next : [];
      const alive = new Set(state.nodes.map((node) => node && node.id));
      if (!state.seeded) {
        state.seeded = true;
        state.expanded = initialExpansion(state.nodes);
      } else {
        state.expanded = new Set([...state.expanded].filter((id) => alive.has(id)));
      }
      if (state.selected !== null && !alive.has(state.selected)) {
        state.selected = null;
        state.focus = null;
      }
      render();
    },
    setSelected(id) {
      state.selected = id;
      state.focus = id;
      render();
    },
    select(id) { select(id, false); },
    expand(id) { setExpanded(id, true); },
    collapse(id) { setExpanded(id, false); },
    expandAll() {
      state.expanded = new Set(
        state.nodes.filter((node) => node && state.nodes.some((other) => other && parentOf(other) === node.id)).map((node) => node.id));
      render();
    },
    collapseAll() {
      state.expanded = new Set();
      render();
    },
    expandedIds() { return [...state.expanded]; },
    selectedId() { return state.selected; },
    visibleIds() { return visibleRows().map((row) => row.id); },
    setFilter(value) {
      state.query = String(value || '').trim().toLowerCase();
      if (search) search.value = value || '';
      render();
    },
    destroy() { clear(root); },
  };
}

function prevent(ev) {
  ev.preventDefault();
}

function cssEscape(value) {
  if (window.CSS && typeof window.CSS.escape === 'function') return window.CSS.escape(value);
  return value.replace(/["\\]/g, '\\$&');
}
