import { el, card } from '../ui.js';

export async function render({ page, route }) {
  page.append(card(route.title + '（尚未实现）', [
    el('p', { text: '该页面属于里程碑 ' + (route.milestone || '未定') + '，后端接口还没有落地，因此这里不做任何假装可用的操作。' }),
    el('p', { class: 'muted', text: '规划见 docs/TODO.md 与 docs/billing.md / docs/pricing.md / docs/backup.md。' }),
  ]));
}