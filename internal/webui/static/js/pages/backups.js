import { api } from '../api.js';
import { el, card, table, modal, toast, badge, formatTime, confirmDialog } from '../ui.js';

const bytes = (value) => {
	const n = Number(value || 0);
	if (n < 1024) return n + ' B';
	if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' KiB';
	return (n / 1024 / 1024).toFixed(2) + ' MiB';
};

export async function render({ page, actions, session }) {
	const readonly = session.role !== 'admin';
	const run = el('button', { class: 'btn btn-primary', text: '立即备份', disabled: readonly });
	const prune = el('button', { class: 'btn', text: '按保留策略清理', disabled: readonly });
	const refresh = el('button', { class: 'btn', text: '刷新' });
	actions.append(run, prune, refresh);
	let view;
	const meta = el('div', { class: 'grid' });

	async function load() {
		const payload = await api.get('/backups', { limit: 100 });
		const rows = payload.data || [];
		meta.replaceChildren(
			statLike('备份目录', payload.dir || '—'),
			statLike('占用', bytes(payload.total_bytes)),
			statLike('下次自动备份', payload.next_run ? formatTime(payload.next_run) : '未启用'),
			statLike('份数', String(payload.count)),
		);
		if (!view) {
			view = table({
				columns: [
					{ key: 'started_at', label: '开始时间', render: (row) => formatTime(row.started_at) },
					{ key: 'status', label: '状态', render: (row) => badge(row.status, row.status === 'ok' ? 'ok' : 'danger') },
					{ key: 'quick_check', label: 'quick_check' },
					{ key: 'size_bytes', label: '大小', render: (row) => bytes(row.size_bytes) },
					{ key: 'trigger', label: '触发' },
					{ key: 'path', label: '文件', render: (row) => el('code', { text: row.path }) },
					{ key: 'note', label: '备注' },
				],
				rows,
				empty: '还没有备份',
				rowActions: (row) => {
					const buttons = [
						el('a', { class: 'btn', href: '/admin/api/v1/backups/' + row.id + '/download', text: '下载' }),
					];
					if (!readonly) {
						buttons.push(el('button', { class: 'btn btn-danger', text: '恢复', onclick: () => restore(row, load) }));
						buttons.push(el('button', { class: 'btn', text: '删除', onclick: () => remove(row, load) }));
					}
					return buttons;
				},
			});
			page.append(card('数据库备份', view.node, [
				el('span', { class: 'muted', text: '快照由 SQLite VACUUM INTO 生成，不阻塞写入；恢复是两阶段冷恢复，需要重启进程' })]));
			page.append(card('说明', meta));
		} else {
			view.refresh(rows);
		}
	}

	function statLike(label, value) {
		return el('div', { class: 'stat' }, [el('div', { class: 'k', text: label }), el('div', { class: 'v', text: value })]);
	}

	run.addEventListener('click', async () => {
		run.disabled = true;
		try {
			const result = await api.post('/backups', {});
			if (result.ok) {
				toast('备份完成：' + bytes(result.job.size_bytes) + '，quick_check=' + result.job.quick_check, 'ok');
			} else {
				toast('备份失败：' + result.error, 'error');
			}
			await load();
		} catch (err) {
			toast(api.errorMessage(err), 'error');
		} finally {
			run.disabled = readonly;
		}
	});

	prune.addEventListener('click', async () => {
		const result = await api.post('/backups/prune', {});
		toast('已清理 ' + result.count + ' 份（校验失败的快照不会被删除）', 'ok');
		await load();
	});
	refresh.addEventListener('click', () => load().catch((err) => toast(api.errorMessage(err), 'error')));

	async function remove(row, reload) {
		const ok = await confirmDialog('删除备份', '确认删除 ' + row.path + ' 及其文件吗？');
		if (!ok) return;
		await api.del('/backups/' + row.id);
		toast('已删除', 'ok');
		await reload();
	}

	async function restore(row, reload) {
		const confirmed = await modal({
			title: '恢复数据库快照',
			submitLabel: '暂存并计划恢复',
			fields: [
				{ name: 'ack', label: '该操作会在下次启动时用此快照替换当前数据库（当前库会保留为 .pre-restore-*）', type: 'checkbox' },
			],
			onSubmit: async (values) => {
				if (!values.ack) {
					throw new Error('请先勾选确认');
				}
				return api.post('/backups/' + row.id + '/restore', { confirm: true });
			},
		});
		if (!confirmed) return;
		toast('已暂存恢复计划：重启网关后生效', 'ok');
		await reload();
	}

	await load();
}