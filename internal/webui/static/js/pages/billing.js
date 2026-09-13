import { api } from '../api.js';
import { consolePath } from '../base.js';
import { el, card, pagedTable, modal, toast, badge, stat, jsonBlock, formatTime, confirmDialog, modalHead, modalBody, modalActions } from '../ui.js';
import { initCurrency, money, ledgerCurrency } from '../money.js';

export async function render({ page, actions, session, route }) {
	const readonly = session.role !== 'admin';
	await initCurrency();
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
	// 账户下拉框要一次拿全：显式请求上限 1000（配置类列表的服务端上限），分页表格不带这个 limit。
	const accounts = (await api.get('/accounts', { limit: 1000 })).data || [];
	if (!accounts.length) {
		page.append(card('账本', [el('div', { class: 'empty', text: '还没有账户，先在「账户」页创建一个' })]));
		return;
	}
	let accountID = accounts[0].id;
	const picker = el('select', {}, accounts.map((account) => el('option', { value: account.id, text: account.name })));
	const refresh = el('button', { class: 'btn', text: '刷新' });
	const topup = el('button', { class: 'btn btn-primary', text: '充值 / 赠送', disabled: readonly });
	actions.append(picker, refresh, topup);

	const report = (err) => toast(api.errorMessage(err), 'error');
	const summary = el('div', { class: 'grid' });

	async function loadBalance() {
		const balance = await api.get('/accounts/' + accountID + '/balance');
		summary.replaceChildren(
			stat('余额', money(balance.balance_micros)),
			stat('在途预留', money(balance.in_flight_micros || 0)),
			stat('可用', money((balance.balance_micros || 0) - (balance.in_flight_micros || 0))),
		);
	}

	// 两张表各持有自己的窗口，互不影响；账户与天数窗口都进请求参数。
	const ledgerView = pagedTable({
		columns: [
			{ key: 'created_at', label: '时间', render: (row) => formatTime(row.created_at) },
			{ key: 'kind', label: '类型', render: (row) => badge(row.kind, row.kind === 'charge' ? '' : 'ok') },
			{ key: 'amount_micros', label: '金额', render: (row) => money(row.amount_micros) },
			{ key: 'balance_after_micros', label: '余额', render: (row) => money(row.balance_after_micros) },
			{ key: 'note', label: '备注' },
			{ key: 'idem_key', label: '幂等键', render: (row) => el('code', { text: row.idem_key }) },
		],
		empty: '该账户在窗口内没有账本记录',
		rowActions: (row) => [el('button', { class: 'btn', text: '详情', onclick: () => showLedgerDetail(row) })],
		load: ({ limit, offset }) => api.get('/accounts/' + accountID + '/ledger', { days: 30, limit, offset }),
		onError: report,
	});

	const creditsView = pagedTable({
		columns: [
			{ key: 'created_at', label: '时间', render: (row) => formatTime(row.created_at) },
			{ key: 'kind', label: '类型' },
			{ key: 'amount_micros', label: '金额', render: (row) => money(row.amount_micros) },
			{ key: 'ref_id', label: '外部流水号', render: (row) => el('code', { text: row.ref_id || '—' }) },
			{ key: 'actor', label: '操作者' },
		],
		empty: '没有充值/赠送记录',
		rowActions: (row) => [el('button', { class: 'btn', text: '详情', onclick: () => showLedgerDetail(row) })],
		load: ({ limit, offset }) => api.get('/accounts/' + accountID + '/credits', { days: 90, limit, offset }),
		onError: report,
	});

	page.append(card('当前账户', summary), card('账本流水（含扣费）', ledgerView.node), card('额度明细（非扣费类）', creditsView.node));

	function showLedgerDetail(row) {
		const fields = [
			['时间', formatTime(row.created_at)], ['类型', row.kind], ['金额', money(row.amount_micros)],
			['余额', money(row.balance_after_micros)], ['引用类型', row.ref_type], ['引用编号', row.ref_id],
			['幂等键', row.idem_key], ['重建序号', row.rebuild_seq], ['操作者', row.actor], ['备注', row.note],
		];
		if (row.api_key_id !== undefined) fields.push(['API Key ID', row.api_key_id]);
		if (row.expires_at) fields.push(['到期时间', formatTime(row.expires_at)]);
		const table = el('table', {}, [
			el('tbody', {}, fields.map(([label, value]) => el('tr', {}, [
				el('th', { text: label }), el('td', { text: value === undefined || value === null || value === '' ? '—' : String(value) }),
			]))),
		]);
		title('账本流水 #' + row.id, table);
	}

	// 余额没有窗口，表格各自刷新当前页（刷新保持当前页，不跳回第 1 页）。
	function reloadAll() {
		return Promise.all([loadBalance().catch(report), ledgerView.refresh(), creditsView.refresh()]);
	}

	picker.addEventListener('change', () => {
		// 换账户就是换了一份列表：当前 offset 属于上一个账户，所以两张表都回到第 1 页。
		accountID = Number(picker.value);
		loadBalance().catch(report);
		ledgerView.reset();
		creditsView.reset();
	});
	refresh.addEventListener('click', () => reloadAll());
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
				{ name: 'amount', label: '金额（' + ledgerCurrency() + '，最多 6 位小数）', required: true },
				{ name: 'ref_id', label: '外部流水号（幂等键，必填）', required: true },
				{ name: 'expires_at', label: '到期时间（仅赠送，RFC3339，可留空）' },
				{ name: 'note', label: '备注' },
			],
			onSubmit: (values) => api.post('/accounts/' + accountID + '/credits', {
				kind: values.kind, amount: values.amount, ref_id: values.ref_id,
				note: values.note, expires_at: values.expires_at || undefined,
			}),
		});
		if (result) { toast(result.applied ? '已入账' : '该流水号已入账（幂等，未重复入账）', 'ok'); await reloadAll(); }
	});
	await reloadAll();
}

// ---- invoices -----------------------------------------------------------

async function renderInvoices({ page, actions, session, readonly }) {
	// 账户下拉框要一次拿全：显式请求上限 1000（配置类列表的服务端上限），分页表格不带这个 limit。
	const accounts = (await api.get('/accounts', { limit: 1000 })).data || [];
	let accountID = accounts.length ? accounts[0].id : 0;
	const picker = el('select', {}, accounts.map((account) => el('option', { value: account.id, text: account.name })));
	const build = el('button', { class: 'btn btn-primary', text: '生成账期账单', disabled: readonly || !accounts.length });
	const refresh = el('button', { class: 'btn', text: '刷新' });
	actions.append(picker, build, refresh);

	const view = pagedTable({
		columns: [
			{ key: 'period_start', label: '账期', render: (row) => formatTime(row.period_start) + ' → ' + formatTime(row.period_end) },
			{ key: 'status', label: '状态', render: (row) => badge(row.status, row.status === 'paid' ? 'ok' : row.status === 'void' ? 'danger' : '') },
			{ key: 'total_cost_micros', label: '成本', render: (row) => money(row.total_cost_micros) },
			{ key: 'total_charge_micros', label: '对客', render: (row) => money(row.total_charge_micros) },
			{ key: 'created_at', label: '生成时间', render: (row) => formatTime(row.created_at) },
		],
		empty: '还没有账单',
		rowActions: (row) => {
			const buttons = [el('button', { class: 'btn', text: '详情', onclick: () => detail(row) })];
			if (!readonly) {
				if (row.status === 'draft') buttons.push(el('button', { class: 'btn', text: '签发', onclick: () => act(row, 'issue', () => view.refresh()) }));
				if (row.status === 'issued') buttons.push(el('button', { class: 'btn btn-primary', text: '标记已付', onclick: () => act(row, 'pay', () => view.refresh()) }));
				if (row.status !== 'void' && row.status !== 'paid') buttons.push(el('button', { class: 'btn btn-danger', text: '作废', onclick: () => act(row, 'void', () => view.refresh()) }));
			}
			return buttons;
		},
		// 账户过滤下沉到服务端：分页之后浏览器里再 filter 只会筛掉本页的行，
		// 而「共 N 条」也已由服务端按账户过滤后算出。accountID=0 仍是全部账户视图。
		load: ({ limit, offset }) => api.get('/invoices', { account_id: accountID, limit, offset }),
		onError: (err) => toast(api.errorMessage(err), 'error'),
	});
	page.append(card('账期账单', view.node, [
		el('span', { class: 'muted', text: '签发后内容冻结；后付账户标记已付会写一笔还款流水' })]));

	async function detail(invoice) {
		const full = await api.get('/invoices/' + invoice.id);
		const rows = (full.lines || []).map((line) => ({
			group: line.group_key, requests: line.requests,
			prompt: line.prompt_tokens, completion: line.completion_tokens,
			cost: money(line.cost_micros), charge: money(line.charge_micros),
		}));
		const body = rows.length
			? el('table', {}, [
				el('thead', {}, [el('tr', {}, ['分组', '请求数', '输入 tokens', '输出 tokens', '成本', '对客'].map((label) => el('th', { text: label })))]),
				el('tbody', {}, rows.map((row) => el('tr', {}, [row.group, row.requests, row.prompt, row.completion, row.cost, row.charge].map((value) => el('td', { text: String(value) }))))),
			])
			: el('div', { class: 'empty', text: '该账期没有用量' });
		const csv = el('a', { class: 'btn', href: consolePath('/admin/api/v1/invoices/' + invoice.id + '?format=csv'), text: '导出 CSV' });
		const close = el('button', { class: 'btn', text: '关闭' });
		const dialog = el('div', { class: 'modal', style: 'width:min(900px,100%)' }, [
			modalHead('账单 #' + invoice.id + ' · ' + invoice.status, () => backdrop.remove()),
			modalBody([body]),
			modalActions([csv, close]),
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

	picker.addEventListener('change', () => {
		// 换账户就是换了一份服务端过滤的列表：offset 归零重读。
		accountID = Number(picker.value);
		view.reset();
	});
	refresh.addEventListener('click', () => view.refresh());
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
		if (result) { toast('账单已生成/更新', 'ok'); await view.refresh(); }
	});
	await view.refresh();
}

// ---- reconciliation -----------------------------------------------------

async function renderReconciliation({ page, actions, session, readonly }) {
	const run = el('button', { class: 'btn btn-primary', text: '立即对账', disabled: readonly });
	const replay = el('button', { class: 'btn', text: '重放失败结算', disabled: readonly });
	const refresh = el('button', { class: 'btn', text: '刷新' });
	actions.append(run, replay, refresh);
	const report = (err) => toast(api.errorMessage(err), 'error');
	const invariants = el('div');

	const view = pagedTable({
		columns: [
			{ key: 'created_at', label: '时间', render: (row) => formatTime(row.created_at) },
			{ key: 'kind', label: '类型' },
			{ key: 'usage_charge_micros', label: '用量侧', render: (row) => money(row.usage_charge_micros) },
			{ key: 'ledger_charge_micros', label: '账本侧', render: (row) => money(row.ledger_charge_micros) },
			{ key: 'diff_micros', label: '差异', render: (row) => row.diff_micros === 0
				? badge('0', 'ok') : badge(money(row.diff_micros), 'danger') },
			{ key: 'estimated_ratio_bp', label: '估算占比', render: (row) => (row.estimated_ratio_bp / 100).toFixed(2) + '%' },
		],
		empty: '还没有对账记录',
		rowActions: (row) => [el('button', { class: 'btn', text: '详情', onclick: () => {
			title('对账 #' + row.id, jsonBlock(row.details));
		} })],
		load: ({ limit, offset }) => api.get('/billing/reconciliations', { limit, offset }),
		onError: report,
	});
	page.append(card('对账记录', view.node, [
		el('span', { class: 'muted', text: '对账只读：差异必须通过 adjustment 修正，保留人工痕迹' })]));
	page.append(card('账本不变量', invariants));

	async function loadInvariants() {
		const invariantReport = await api.get('/billing/invariants');
		invariants.replaceChildren(
			invariantReport.ok
				? el('div', { class: 'grid' }, [stat('状态', '全部通过'), stat('账户数', String(invariantReport.accounts))])
				: jsonBlock(invariantReport),
		);
	}

	// 对账记录表刷新当前页；不变量报告没有窗口，每次重读。
	function reloadAll() {
		return Promise.all([view.refresh(), loadInvariants().catch(report)]);
	}

	run.addEventListener('click', async () => {
		const result = await api.post('/billing/reconcile', { days: 1 });
		toast(result.diff_micros === 0 ? '对账完成，无差异' : '对账发现差异：' + money(result.diff_micros),
			result.diff_micros === 0 ? 'ok' : 'error');
		await reloadAll();
	});
	replay.addEventListener('click', async () => {
		const result = await api.post('/billing/failures/replay', {});
		toast('重放完成：扫描 ' + result.scanned + '，成功 ' + result.replayed + '，失败 ' + result.failed,
			result.failed ? 'error' : 'ok');
		await reloadAll();
	});
	refresh.addEventListener('click', () => reloadAll());
	await reloadAll();
}

function title(heading, body) {
	const close = el('button', { class: 'btn', text: '关闭' });
	const dialog = el('div', { class: 'modal', style: 'width:min(900px,100%)' },
		[modalHead(heading, () => backdrop.remove()), modalBody([body]), modalActions([close])]);
	const backdrop = el('div', { class: 'modal-backdrop' }, [dialog]);
	close.addEventListener('click', () => backdrop.remove());
	document.getElementById('modal-root').append(backdrop);
}