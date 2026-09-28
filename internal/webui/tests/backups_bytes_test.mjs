// Node-only regression checks for the backup page's size formatter.
//
// The numbers on that page come from `backup_jobs.size_bytes`, and until v4.7.1 that column
// was never written after a snapshot finished — so the first thing a real deployment showed
// was "0 B" for a 12 GB file. The data fix is pinned in Go (internal/backup, internal/httpapi);
// what is pinned here is the other half of "the page tells the truth": once the number is
// real, it has to be readable, and 12,222,103,552 bytes as "11655.42 MiB" is not.
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import vm from 'node:vm';

const source = await readFile(new URL('../static/js/pages/backups.js', import.meta.url), 'utf8');

const start = source.indexOf('const bytes = (value) => {');
assert.ok(start >= 0, 'backups.js must keep its bytes() helper');
const end = source.indexOf('};', start);
assert.ok(end > start, 'bytes() must be a single arrow function the test can slice');
const bytes = vm.runInNewContext(`${source.slice(start, end + 3)}\n; bytes`);

// The sizes the columns actually render.
assert.equal(bytes(0), '0 B', 'an empty page must still read as 0 B');
assert.equal(bytes(undefined), '0 B', 'a missing field must not become NaN');
assert.equal(bytes(512), '512 B');
assert.equal(bytes(1536), '1.5 KiB');
assert.equal(bytes(3 * 1024 * 1024), '3.00 MiB');
// 12,222,103,552 B = the snapshot a real deployment took on 2026-09-27.
assert.equal(bytes(12222103552), '11.38 GiB');
assert.equal(bytes(1024 * 1024 * 1024), '1.00 GiB', 'the GiB tier starts exactly at 1 GiB');

console.log('Backup page size formatter checks passed.');
