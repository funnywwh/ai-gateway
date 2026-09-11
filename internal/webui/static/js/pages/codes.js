import { api } from '../api.js';
import { el, card, table, modal, toast, badge, formatTime, confirmDialog, modalHead, modalBody, modalActions } from '../ui.js';
import { initCurrency, money, ledgerCurrency } from '../money.js';


export async function render({ page, actions, session }) {
	const readonly = session.role !== 'admin';
	await initCurrency();
	const create = el('button', { class: 'btn btn-primary', text: '生成兑换码', disabled: readonly });
	const redeem = el('button', { class: 'btn', text: '核销兑换码', disabled: readonly });
	const refresh = el('button', { class: 'btn', text: '刷新' });
	actions.append(create, redeem, refresh);

	const batchInput = el('input', { placeholder: '按批次过滤（留空显示全部）' });
	let view;

	async function load() {
		const payload = await api.get('/redemption-codes', { batch_id: batchInput.value.trim() || undefined, limit: 200 });
		const rows = payload.data || [];
		if (!view) {
			view = table({
				columns: [
					{ key: 'hash_prefix', label: '哈希前缀', render: (row) => el('code', { text: row.hash_prefix }) },
					{ key: 'amount_micros', label: '面额', render: (row) => money(row.amount_micros) },
					{ key: 'status', label: '状态', render: (row) => badge(row.status, row.status === 'unused' ? 'ok' : row.status === 'expired' ? 'warn' : '') },
					{ key: 'batch_id', label: '批次', render: (row) => el('code', { text: row.batch_id || '—' }) },
					{ key: 'expires_at', label: '到期', render: (row) => formatTime(row.expires_at) },
					{ key: 'redeemed_at', label: '核销时间', render: (row) => formatTime(row.redeemed_at) },
					{ key: 'created_at', label: '生成时间', render: (row) => formatTime(row.created_at) },
				],
				rows,
				empty: '还没有兑换码',
			});
			page.append(card('兑换码', view.node, [
				batchInput,
				el('span', { class: 'muted', text: '库里只存哈希：明文只在生成时显示一次，泄露数据库也无法核销' })]));
		} else {
			view.refresh(rows);
		}
	}

	refresh.addEventListener('click', () => load().catch((err) => toast(api.errorMessage(err), 'error')));
	batchInput.addEventListener('change', () => load().catch((err) => toast(api.errorMessage(err), 'error')));

	create.addEventListener('click', async () => {
		const result = await modal({
			title: '生成兑换码',
			fields: [
				{ name: 'count', label: '数量', type: 'number', value: 10, required: true },
				{ name: 'amount', label: '每张面额（' + ledgerCurrency() + '）', value: '10.00', required: true },
				{ name: 'batch_id', label: '批次号（可留空自动生成）' },
				{ name: 'expires_at', label: '到期时间（RFC3339，可留空）', placeholder: '2027-01-01T00:00:00Z' },
				{ name: 'note', label: '备注' },
			],
			onSubmit: async (values) => {
				const first = await api.post('/redemption-codes', {
					count: Number(values.count), amount: values.amount,
					batch_id: values.batch_id, note: values.note,
					expires_at: values.expires_at || undefined,
				});
				showCodes(first);
				return first;
			},
		});
		if (result) await load();
	});

	redeem.addEventListener('click', async () => {
		const accounts = (await api.get('/accounts')).data || [];
		if (!accounts.length) { toast('请先创建一个账户', 'error'); return; }
		const result = await modal({
			title: '核销兑换码',
			fields: [
				{ name: 'code', label: '兑换码', required: true },
				{ name: 'account_id', label: '入账账户', type: 'select', options: accounts.map((a) => ({ value: a.id, label: a.name })) },
			],
			onSubmit: (values) => api.post('/redemption-codes/redeem', {
				code: values.code, account_id: Number(values.account_id),
			}),
		});
		if (result) {
			toast('已核销 ' + money(result.amount_micros), 'ok');
			await load();
		}
	});
	await load();
}

// showCodes displays the one-time plaintext list with a copy helper.
function showCodes(payload) {
	const list = (payload.codes || []).join('\n');
	const box = el('textarea', { rows: 12 });
	box.value = list;
	const copy = el('button', { class: 'btn', text: '复制全部' });
	copy.addEventListener('click', async () => {
		try { await navigator.clipboard.writeText(list); toast('已复制', 'ok'); }
		catch (err) { box.select(); document.execCommand('copy'); }
	});
	const done = el('button', { class: 'btn btn-primary', text: '我已保存' });
	const dialog = el('div', { class: 'modal', style: 'width:min(720px,100%)' }, [
		modalHead('兑换码已生成（' + (payload.count || 0) + ' 张）', () => backdrop.remove()),
		modalBody([
			el('p', { class: 'muted', text: '明文只显示这一次；数据库里只有哈希，关闭后无法再次获取。' }),
			box,
		]),
		modalActions([copy, done]),
	]);
	const backdrop = el('div', { class: 'modal-backdrop' }, [dialog]);
	done.addEventListener('click', () => backdrop.remove());
	document.getElementById('modal-root').append(backdrop);
	box.select();
}

export { confirmDialog };
