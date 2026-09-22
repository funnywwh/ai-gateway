// Static regression checks for the tenant name an account gets when an operator enables DSH (M74).
//
// The rule — dsh-<账号拼音>-<账号ID> — lives in exactly one place, the Go side
// (internal/httpapi/admin_catalog.go, dshTenantNameForAccount), and travels to the console as the
// account row's `dsh_tenant_suggested`. That is a wiring fact, and wiring is what a text assertion
// protects without a browser: both pages used to carry their own copy of the name generator (it kept
// ASCII only, so every Chinese name collapsed into the same dsh-tenant stem), and a third copy is
// exactly what this test refuses.
//
// What the rule *produces* is pinned by Go tests (internal/httpapi, internal/pinyin), and the dialog
// pre-filling the field is pinned in a real browser by scripts/ui-harness (view `org-person`).
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';

const pages = [
  ['accounts.js', await readFile(new URL('../static/js/pages/accounts.js', import.meta.url), 'utf8')],
  ['org.js', await readFile(new URL('../static/js/pages/org.js', import.meta.url), 'utf8')],
];

for (const [name, source] of pages) {
  assert.doesNotMatch(source, /function slugFromAccount/,
    name + ' must not derive a tenant name itself: the server computes it (dsh_tenant_suggested)');
  // A stored mapping wins, so the dialog never renames an existing tenant by surprise; the server's
  // suggestion is only the fallback for an account that has none.
  assert.match(source, /const suggested = [\w.]+\.dsh_tenant \|\| [\w.]+\.dsh_tenant_suggested \|\| ''/,
    name + ' must pre-fill dsh_tenant_suggested, falling back from the stored mapping');
  // The dialog has to say what the pre-filled name means: an operator who sees dsh-chenjingfeng-10
  // for 陈景峰 needs the rule by example, and needs to know nothing is renamed.
  assert.ok(source.includes('dsh-chenjingfeng-10'),
    name + ' must show the naming rule by example in the dialog hint');
  assert.ok(source.includes('已存在的租户名不会被改动'),
    name + ' must say that an existing tenant name is kept, not renamed');
}
