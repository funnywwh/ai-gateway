// The console derives its mount prefix from its own module URL, which is the only thing
// that tells one build apart between a root deployment and a prefixed one. There is no
// browser in this environment, but the derivation is a pure function of that URL, so it
// can be pinned directly — and a wrong answer here is a console that 404s on every call.
//
// Usage: node scripts/ui-base-test.mjs
import { pathToFileURL } from 'node:url';
import { fileURLToPath } from 'node:url';
import path from 'node:path';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const base = await import(pathToFileURL(path.join(root, 'internal/webui/static/js/base.js')).href);

const cases = [
	['http://127.0.0.1:8088/admin/ui/js/api.js', '', '/admin/api/v1'],
	['http://127.0.0.1:8088/admin/ui/js/pages/billing.js', '', '/admin/api/v1'],
	['https://gpt.iotalking.top/aigw/admin/ui/js/api.js', '/aigw', '/aigw/admin/api/v1'],
	['https://gpt.iotalking.top/aigw/admin/ui/js/pages/chat_artifact.js', '/aigw', '/aigw/admin/api/v1'],
	['https://host/gateway/aigw/admin/ui/js/api.js', '/gateway/aigw', '/gateway/aigw/admin/api/v1'],
	// A trailing-slash spelling of the console root, and a query the browser may append.
	['https://host/aigw/admin/ui/js/api.js?v=2', '/aigw', '/aigw/admin/api/v1'],
	// Not an asset URL at all (a bare path, as a test or a tool might pass): no /js/ to key
	// on, so the mount is empty rather than a guess.
	['/js/api.js', '', '/admin/api/v1'],
];

let failed = 0;
for (const [url, wantMount, wantAPI] of cases) {
	const mount = base.consoleBasePath(url);
	const api = base.apiRoot(url);
	if (mount !== wantMount || api !== wantAPI) {
		failed++;
		console.log(`[FAIL] consoleBasePath(${url}) = ${JSON.stringify(mount)}, apiRoot = ${JSON.stringify(api)}`);
		console.log(`       want mount ${JSON.stringify(wantMount)}, api ${JSON.stringify(wantAPI)}`);
	}
}

// consolePath is what the console uses for links it builds itself (CSV export, backup
// download, preview frame): it must stay inside the mount.
for (const [url, wanted] of [
	['https://gpt.iotalking.top/aigw/admin/ui/js/api.js', '/aigw/admin/api/v1/backups/7/download'],
	['http://127.0.0.1:8088/admin/ui/js/api.js', '/admin/api/v1/backups/7/download'],
]) {
	const got = base.consolePath('/admin/api/v1/backups/7/download', url);
	if (got !== wanted) {
		failed++;
		console.log(`[FAIL] consolePath under ${url} = ${JSON.stringify(got)}, want ${JSON.stringify(wanted)}`);
	}
}

// The module's own URL is the default argument: importing it under a prefixed path is the
// real deployment, and it must not need an argument to get this right.
if (base.consoleBasePath() !== '') {
	failed++;
	console.log(`[FAIL] consoleBasePath() on the on-disk module = ${JSON.stringify(base.consoleBasePath())}, want ''`);
}

if (failed) {
	console.log(`\n${failed} base-path check(s) failed`);
	process.exit(1);
}
console.log(`base path: ${cases.length + 3} checks passed`);
