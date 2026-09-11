import { api } from '../api.js';
import { el, card, table, modal, toast, badge, stat, jsonBlock, formatTime, confirmDialog, modalHead } from '../ui.js';

const MICRO = 1_000_000;
const money = (micros) => (Number(micros || 0) / MICRO).toFixed(6);

export async function render({ page, actions, session, route }) {
	const readonly = session.role !== 'admin';
	switch (route.path) {
	case '/invoices':
		return renderInvoices({ page, actions, session, readonly });
	case '/reconciliation':
		return renderReconciliation({ page, actions, session, readonly });
	default:
		return renderLedger({ page, actions, session, readonly });
	}
}

// ---- ledger and credits -------------------------------------------------

async function renderLedger({ page, actions, readonly }) {
	const accounts = (await api.get('/accounts')).data || [];
	if (!accounts.length) {
		page.append(card('账本', [el('div', { class: 'empty', text: '还没有账户，先在「账户」页创建一个' })]));
		return;
	}
	let accountID = accounts[0].id;
	const picker = el('select', {}, accounts.map((account) => el('option', { value: account.id, text: account.name })));
	const refresh = el('button', { class: 'btn', text: '刷新' });
	const topup = el('button', { class: 'btn btn-primary', text: '充值 / 赠送', disabled: readonly });
	actions.append(picker, refresh, topup);

	const summary = el('div', { class: 'grid' });
	const ledgerView = el('div');
	const creditsView = el('div');

	page.append(card('当前账户', summary), card('账本流水（含扣费）', ledgerView), card('额度明细（非扣费类）', creditsView));

	async function load() {
		const [balance, ledger, credits] = await Promise.all([
			api.get('/accounts/' + accountID + '/balance'),
			api.get('/accounts/' + accountID + '/ledger', { days: 30, limit: 200 }),
			api.get('/accounts/' + accountID + '/credits', { days: 90, limit: 200 }),
		]);
		summary.replaceChildren(
			stat('余额（USD）', money(balance.balance_micros)),
			stat('在途预留（USD）', money(balance.in_flight_micros || 0)),
			stat('可用（USD）', money((balance.balance_micros || 0) - (balance.in_flight_micros || 0))),
		);
		ledgerView.replaceChildren(ledgerTable(ledger.data || []).node);
		creditsView.replaceChildren(creditsTable(credits.data || []).node);
	}

	function ledgerTable(rows) {
		return table({
			columns: [
				{ key: 'created_at', label: '时间', render: (row) => formatTime(row.created_at) },
				{ key: 'kind', label: '类型', render: (row) => badge(row.kind, row.kind === 'charge' ? '' : 'ok') },
				{ key: 'amount_micros', label: '金额（USD）', render: (row) => money(row.amount_micros) },
				{ key: 'balance_after_micros', label: '余额（USD）', render: (row) => money(row.balance_after_micros) },
				{ key: 'note', label: '备注' },
				{ key: 'idem_key', label: '幂等键', render: (row) => el('code', { text: row.idem_key }) },
			],
			rows,
			empty: '该账户在窗口内没有账本记录',
		});
	}

	function creditsTable(rows) {
		return table({
			columns: [
				{ key: 'created_at', label: '时间', render: (row) => formatTime(row.created_at) },
				{ key: 'kind', label: '类型' },
				{ key: 'amount_micros', label: '金额（USD）', render: (row) => money(row.amount_micros) },
				{ key: 'ref_id', label: '外部流水号', render: (row) => el('code', { text: row.ref_id || '—' }) },
				{ key: 'actor', label: '操作者' },
			],
			rows,
			empty: '没有充值/赠送记录',
		});
	}

	picker.addEventListener('change', () => { accountID = Number(picker.value); load().catch((err) => toast(api.errorMessage(err), 'error')); });
	refresh.addEventListener('click', () => load().catch((err) => toast(api.errorMessage(err), 'error')));
	topup.addEventListener('click', async () => {
		const result = await modal({
			title: '入账',
			fields: [
				{ name: 'kind', label: '类型', type: 'select', options: [
					{ value: 'topup', label: 'topup 充值' },
					{ value: 'credit_grant', label: 'credit_grant 赠送' },
					{ value: 'adjustment', label: 'adjustment 调整（可为负）' },
					{ value: 'refund', label: 'refund 退款' },
				] },
				{ name: 'amount_usd', label: '金额（USD，最多 6 位小数）', required: true },
				{ name: 'ref_id', label: '外部流水号（幂等键，必填）', required: true },
				{ name: 'expires_at', label: '到期时间（仅赠送，RFC3339，可留空）' },
				{ name: 'note', label: '备注' },
			],
			onSubmit: (values) => api.post('/accounts/' + accountID + '/credits', {
				kind: values.kind, amount_usd: values.amount_usd, ref_id: values.ref_id,
				note: values.note, expires_at: values.expires_at || undefined,
			}),
		});
		if (result) { toast(result.applied ? '已入账' : '该流水号已入账（幂等，未重复入账）', 'ok'); await load(); }
	});
	await load();
}

// ---- invoices -----------------------------------------------------------

async function renderInvoices({ page, actions, session, readonly }) {
	const accounts = (await api.get('/accounts')).data || [];
	let accountID = accounts.length ? accounts[0].id : 0;
	const picker = el('select', {}, accounts.map((account) => el('option', { value: account.id, text: account.name })));
	const build = el('button', { class: 'btn btn-primary', text: '生成账期账单', disabled: readonly || !accounts.length });
	const refresh = el('button', { class: 'btn', text: '刷新' });
	actions.append(picker, build, refresh);
	let view;

	async function load() {
		const payload = await api.get('/invoices', { limit: 100 });
		const rows = (payload.data || []).filter((invoice) => !accountID || invoice.account_id === accountID);
		if (!view) {
			view = table({
				columns: [
					{ key: 'period_start', label: '账期', render: (row) => formatTime(row.period_start) + ' → ' + formatTime(row.period_end) },
					{ key: 'status', label: '状态', render: (row) => badge(row.status, row.status === 'paid' ? 'ok' : row.status === 'void' ? 'danger' : '') },
					{ key: 'total_cost_micros', label: '成本（USD）', render: (row) => money(row.total_cost_micros) },
					{ key: 'total_charge_micros', label: '对客（USD）', render: (row) => money(row.total_charge_micros) },
					{ key: 'created_at', label: '生成时间', render: (row) => formatTime(row.created_at) },
				],
				rows,
				empty: '还没有账单',
				rowActions: (row) => {
					const buttons = [el('button', { class: 'btn', text: '详情', onclick: () => detail(row) })];
					if (!readonly) {
						if (row.status === 'draft') buttons.push(el('button', { class: 'btn', text: '签发', onclick: () => act(row, 'issue', load) }));
						if (row.status === 'issued') buttons.push(el('button', { class: 'btn btn-primary', text: '标记已付', onclick: () => act(row, 'pay', load) }));
						if (row.status !== 'void' && row.status !== 'paid') buttons.push(el('button', { class: 'btn btn-danger', text: '作废', onclick: () => act(row, 'void', load) }));
					}
					return buttons;
				},
			});
			page.append(card('账期账单', view.node, [
				el('span', { class: 'muted', text: '签发后内容冻结；后付账户标记已付会写一笔还款流水' })]));
		} else {
			view.refresh(rows);
		}
	}

	async function detail(invoice) {
		const full = await api.get('/invoices/' + invoice.id);
		const rows = (full.lines || []).map((line) => ({
			group: line.group_key, requests: line.requests,
			prompt: line.prompt_tokens, completion: line.completion_tokens,
			cost: money(line.cost_micros), charge: money(line.charge_micros),
		}));
		const body = rows.length
			? el('table', {}, [
				el('thead', {}, [el('tr', {}, ['分组', '请求数', '输入 tokens', '输出 tokens', '成本 USD', '对客 USD'].map((label) => el('th', { text: label })))]),
				el('tbody', {}, rows.map((row) => el('tr', {}, [row.group, row.requests, row.prompt, row.completion, row.cost, row.charge].map((value) => el('td', { text: String(value) }))))),
			])
			: el('div', { class: 'empty', text: '该账期没有用量' });
		const csv = el('a', { class: 'btn', href: '/admin/api/v1/invoices/' + invoice.id + '?format=csv', text: '导出 CSV' });
		const close = el('button', { class: 'btn', text: '关闭' });
		const dialog = el('div', { class: 'modal', style: 'width:min(900px,100%)' }, [
			modalHead('账单 #' + invoice.id + ' · ' + invoice.status, () => backdrop.remove()), body,
			el('div', { class: 'modal-actions' }, [csv, close]),
		]);
		const backdrop = el('div', { class: 'modal-backdrop' }, [dialog]);
		close.addEventListener('click', () => backdrop.remove());
		document.getElementById('modal-root').append(backdrop);
	}

	async function act(invoice, action, reload) {
		const ok = await confirmDialog('确认操作', '对账单 #' + invoice.id + ' 执行 ' + action + ' 吗？');
		if (!ok) return;
		try {
			await api.post('/invoices/' + invoice.id + '/' + action, {});
		} catch (err) {
			toast(api.errorMessage(err), 'error');
			return;
		}
		toast('已' + action, 'ok');
		await reload();
	}

	picker.addEventListener('change', () => { accountID = Number(picker.value); load().catch((err) => toast(api.errorMessage(err), 'error')); });
	refresh.addEventListener('click', () => load().catch((err) => toast(api.errorMessage(err), 'error')));
	build.addEventListener('click', async () => {
		const result = await modal({
			title: '生成账期账单',
			fields: [
				{ name: 'period', label: '账期', type: 'select', options: [
					{ value: 'current', label: '当前账期' },
					{ value: 'previous', label: '上一个账期' },
				] },
				{ name: 'group_by', label: '行分组', type: 'select', options: ['model', 'key', 'day'] },
				{ name: 'note', label: '备注' },
			],
			onSubmit: (values) => api.post('/accounts/' + accountID + '/invoices', values),
		});
		if (result) { toast('账单已生成/更新', 'ok'); await load(); }
	});
	await load();
}

// ---- reconciliation -----------------------------------------------------

async function renderReconciliation({ page, actions, session, readonly }) {
	const run = el('button', { class: 'btn btn-primary', text: '立即对账', disabled: readonly });
	const replay = el('button', { class: 'btn', text: '重放失败结算', disabled: readonly });
	const refresh = el('button', { class: 'btn', text: '刷新' });
	actions.append(run, replay, refresh);
	let view;
	let invariants = el('div');

	async function load() {
		const [payload, invariantReport] = await Promise.all([
			api.get('/billing/reconciliations', { limit: 50 }),
			api.get('/billing/invariants'),
		]);
		const rows = payload.data || [];
		if (!view) {
			view = table({
				columns: [
					{ key: 'created_at', label: '时间', render: (row) => formatTime(row.created_at) },
					{ key: 'kind', label: '类型' },
					{ key: 'usage_charge_micros', label: '用量侧（USD）', render: (row) => money(row.usage_charge_micros) },
					{ key: 'ledger_charge_micros', label: '账本侧（USD）', render: (row) => money(row.ledger_charge_micros) },
					{ key: 'diff_micros', label: '差异（USD）', render: (row) => row.diff_micros === 0
						? badge('0', 'ok') : badge(money(row.diff_micros), 'danger') },
					{ key: 'estimated_ratio_bp', label: '估算占比', render: (row) => (row.estimated_ratio_bp / 100).toFixed(2) + '%' },
				],
				rows,
				empty: '还没有对账记录',
				rowActions: (row) => [el('button', { class: 'btn', text: '详情', onclick: () => {
					title('对账 #' + row.id, jsonBlock(row.details));
				} })],
			});
			page.append(card('对账记录', view.node, [
				el('span', { class: 'muted', text: '对账只读：差异必须通过 adjustment 修正，保留人工痕迹' })]));
			page.append(card('账本不变量', invariants));
		} else {
			view.refresh(rows);
		}
		invariants.replaceChildren(
			invariantReport.ok
				? el('div', { class: 'grid' }, [stat('状态', '全部通过'), stat('账户数', String(invariantReport.accounts))])
				: jsonBlock(invariantReport),
		);
	}

	run.addEventListener('click', async () => {
		const result = await api.post('/billing/reconcile', { days: 1 });
		toast(result.diff_micros === 0 ? '对账完成，无差异' : '对账发现差异：' + money(result.diff_micros) + ' USD',
			result.diff_micros === 0 ? 'ok' : 'error');
		await load();
	});
	replay.addEventListener('click', async () => {
		const result = await api.post('/billing/failures/replay', {});
		toast('重放完成：扫描 ' + result.scanned + '，成功 ' + result.replayed + '，失败 ' + result.failed,
			result.failed ? 'error' : 'ok');
		await load();
	});
	refresh.addEventListener('click', () => load().catch((err) => toast(api.errorMessage(err), 'error')));
	await load();
}

function title(heading, body) {
	const close = el('button', { class: 'btn', text: '关闭' });
	const dialog = el('div', { class: 'modal', style: 'width:min(900px,100%)' },
		[modalHead(heading, () => backdrop.remove()), body, el('div', { class: 'modal-actions' }, [close])]);
	const backdrop = el('div', { class: 'modal-backdrop' }, [dialog]);
	close.addEventListener('click', () => backdrop.remove());
	document.getElementById('modal-root').append(backdrop);
}