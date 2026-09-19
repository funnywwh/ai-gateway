// Host half of the account-card plugin (M67).
//
// It deliberately does nothing: the row this package renders is a browser surface, and both
// ends of it — the tenant's own `GET /dshgw/session/` and `POST /dshgw/logout/` — belong to
// the gateway (that is where the session store and the only copy of a person's Feishu name
// live). The tenant's node side has no work to do, no file to read and no mailbox to poll.
//
// The file exists because a loader row names a module the host can IMPORT, and dsh's client
// module scan discovers the browser bundle through this package's `dsh.client` declaration
// and its `exports["./client"]` — the same dual-face shape the ssh and browser workspace
// plugins use. Pointing the row straight at `./client.js` would import a bundle that calls
// `window.__ModuleLoader__.load` and crash the whole plugin tree on the host, which is how
// this file came to exist: measured, not assumed.

/** Stable cordis plugin name. */
export const name = 'dshgw-account-card'

/** No services: the host half never runs anything. */
export const inject = []

/** Nothing to set up on the host; the browser half registers the row. */
export function apply() {}
