// The console's build badge (js/brand.js), pinned without a browser.
//
// The badge is the only place an operator sees which release is live, and it is wired to
// a public endpoint that sits outside the management API — a URL the console gets wrong
// silently (the badge just stays empty). Its module takes the version lookup as an
// argument precisely so this file can drive it with a three-line DOM shim; the browser
// harness carries the same assertions for anyone with a browser (`--views brand`).
//
// Usage: node scripts/ui-badge-test.mjs
import { pathToFileURL } from 'node:url';
import { fileURLToPath } from 'node:url';
import path from 'node:path';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');

// ---------------------------------------------------------------------------
// Minimal DOM: only what js/ui.js el() touches.
// ---------------------------------------------------------------------------
class FakeNode {
	constructor(tag) {
		this.tagName = String(tag || '').toUpperCase();
		this.children = [];
		this.attributes = {};
		this._text = '';
	}
	set className(value) { this.attributes.class = value; }
	get className() { return this.attributes.class || ''; }
	set textContent(value) {
		this._text = String(value);
		this.children = [];
	}
	get textContent() {
		return this._text + this.children.map((child) => child.textContent).join('');
	}
	set innerHTML(value) { this._text = String(value); this.children = []; }
	setAttribute(name, value) { this.attributes[name] = String(value); }
	getAttribute(name) { return this.attributes[name]; }
	addEventListener() {}
	append(...nodes) {
		for (const node of nodes) {
			this.children.push(node instanceof FakeNode ? node : new FakeNode('#text')._withText(node));
		}
	}
	_withText(value) { this._text = String(value); return this; }
	// Enough of the query API for the assertions below. The selector vocabulary is
	// deliberately tiny — a class, or a class with one tag child (`> span`) — so a typo
	// fails loudly instead of silently matching nothing and passing.
	querySelector(selector) { return this.querySelectorAll(selector)[0] || null; }
	querySelectorAll(selector) {
		const matches = (node, part) => (part.startsWith('.')
			? node.className.split(/\s+/).includes(part.slice(1))
			: node.tagName === part.toUpperCase());
		const [parentPart, childPart] = selector.includes('>')
			? selector.split('>').map((part) => part.trim())
			: [null, selector.trim()];
		if (childPart === '') throw new Error(`unsupported selector: ${selector}`);
		const walk = (node, part) => {
			const out = [];
			for (const child of node.children) {
				if (matches(child, part)) out.push(child);
				out.push(...walk(child, part));
			}
			return out;
		};
		if (!parentPart) return walk(this, childPart);
		const direct = [];
		for (const parent of walk(this, parentPart)) {
			for (const grand of parent.children) {
				if (matches(grand, childPart)) direct.push(grand);
			}
		}
		return direct;
	}
}

globalThis.Node = FakeNode;
globalThis.document = { createElement: (tag) => new FakeNode(tag), createTextNode: (text) => new FakeNode('#text')._withText(text) };

const { renderBrand } = await import(pathToFileURL(path.join(root, 'internal/webui/static/js/brand.js')).href);

let failed = 0;
const check = (name, ok, detail) => {
	if (ok) return;
	failed++;
	console.log(`[FAIL] ${name}${detail === undefined ? '' : ': ' + detail}`);
};

// The badge is appended from a promise; the module only ever uses microtasks.
const settled = async () => { for (let i = 0; i < 300; i++) await Promise.resolve(); };

// 1. The normal case: version and revision both shown, both under the badge class.
{
	const box = renderBrand(() => Promise.resolve({ version: '0.1.0', revision: '56df9b5' }));
	await settled();
	check('product name leads the brand', box.textContent.startsWith('AI Gateway'), box.textContent);
	check('version is rendered', box.querySelector('.brand-version')?.textContent === 'v0.1.0',
		box.querySelector('.brand-version')?.textContent);
	check('revision is rendered', box.querySelector('.brand-revision')?.textContent === '56df9b5',
		box.querySelector('.brand-revision')?.textContent);
	check('both live in one badge group', box.querySelectorAll('.brand-build > span').length === 2);
}

// 2. A source build carries no git revision; the literal "none" must never be printed
//    where a commit hash belongs.
{
	const box = renderBrand(() => Promise.resolve({ version: '0.1.0', revision: 'none' }));
	await settled();
	check('"none" is not shown as a revision', box.querySelector('.brand-revision') === null);
	check('version still shows without a revision', box.textContent === 'AI Gatewayv0.1.0', box.textContent);
}

// 3. An unreadable endpoint leaves the badge empty: the console has to keep working, and
//    the failure must not surface as an unhandled rejection.
{
	let unhandled = null;
	const onUnhandled = (reason) => { unhandled = reason; };
	process.on('unhandledRejection', onUnhandled);
	const box = renderBrand(() => Promise.reject(new Error('offline')));
	await settled();
	await Promise.resolve();
	process.off('unhandledRejection', onUnhandled);
	check('a failed lookup renders no badge', box.querySelector('.brand-build') === null);
	check('a failed lookup leaves the name intact', box.textContent === 'AI Gateway', box.textContent);
	check('a failed lookup does not reject into the console', unhandled === null, String(unhandled));
}

// 4. The version endpoint is public and lives outside the management API, so the URL the
//    console asks for is mount + '/version' — never '/admin/api/v1/...'.
{
	const { serverRoot } = await import(pathToFileURL(path.join(root, 'internal/webui/static/js/base.js')).href);
	for (const [url, want] of [
		['http://127.0.0.1:8088/admin/ui/js/api.js', '/version'],
		['https://mnl.iotalking.top/aigw/admin/ui/js/api.js', '/aigw/version'],
	]) {
		const got = serverRoot(url) + '/version';
		check(`version URL under ${url}`, got === want, `${got} want ${want}`);
	}
}

if (failed) {
	console.log(`\n${failed} build-badge check(s) failed`);
	process.exit(1);
}
console.log('build badge: 11 checks passed');
