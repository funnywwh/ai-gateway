// Where the console is mounted, derived from this module's own URL.
//
// The console can be served from the root (`/admin/ui/`) or under a deployment prefix
// (`/aigw/admin/ui/`), and the server does not tell the browser which: the assets are
// static and the same HTML is served either way. What is always true is the shape of an
// asset URL — `<mount>/admin/ui/js/<file>.js` — so the mount is whatever precedes the
// final `/js/` segment. Deriving it once here keeps every URL in the console relative to
// the console rather than to the origin, which is what makes one build work behind any
// prefix a reverse proxy chooses.

/** The mount prefix, without a trailing slash: '' at the root, '/aigw' under /aigw/admin/ui/. */
export function consoleBasePath(moduleURL) {
	const raw = moduleURL === undefined || moduleURL === null ? String(import.meta.url) : String(moduleURL);
	let pathname = raw;
	try {
		const parsed = new URL(raw);
		// A file: URL means the module is not being served over HTTP at all (the test
		// runner, or a developer opening a copy from disk): there is no mount to derive.
		if (parsed.protocol !== 'http:' && parsed.protocol !== 'https:') return '';
		pathname = parsed.pathname;
	} catch (err) {
		// Not an absolute URL: treat it as the path it looks like.
		pathname = raw.split('?')[0].split('#')[0];
	}
	const marker = pathname.lastIndexOf('/js/');
	const mount = marker < 0 ? '' : pathname.slice(0, marker);
	// '/admin/ui' → '', '/aigw/admin/ui' → '/aigw'. A mount that is not the console's own
	// directory keeps everything that precedes it.
	return mount.endsWith('/admin/ui') ? mount.slice(0, -'/admin/ui'.length) : mount;
}

/** The management API root, e.g. '/aigw/admin/api/v1'. */
export function apiRoot(moduleURL) {
	return consoleBasePath(moduleURL) + '/admin/api/v1';
}

/**
 * A path relative to the console root, e.g. '/aigw/admin/chat-artifact/x' or
 * '/admin/api/v1/invoices/1?format=csv'. Relative URLs here are resolved against the
 * console shell's URL (`/aigw/admin/ui/`), so a link built from one stays inside the
 * console no matter which page of it the operator is looking at.
 */
export function consolePath(path, moduleURL) {
	return consoleBasePath(moduleURL) + path;
}
