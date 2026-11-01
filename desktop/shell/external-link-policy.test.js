'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const { isSafeExternalURL, openExternalIfSafe } = require('./browser-webview-policy');

const REFUSED = [
  'file:///etc/passwd', 'FILE:///etc/passwd', ' file:///etc/passwd', 'fi\tle:///etc/passwd',
  'smb://192.0.2.1/share', 'search-ms:query=x', 'ms-msdt:/id', 'javascript:alert(1)', 'vbscript:x',
  'data:text/html,hi', 'blob:http://example.com/a', 'ftp://example.com/a', 'about:blank',
  'workass-unknown://x', 'C:\\Windows\\calc.exe', '/relative', '', null, undefined, 42,
];
const ALLOWED = ['http://example.com/a', 'https://example.com/a', 'HTTPS://example.com', 'mailto:someone@example.com', 'http://127.0.0.1:8799/workass/artifacts/a/index.html',
  'tel:+15550100', 'slack://channel?team=T1&id=C1', 'vscode://file/tmp/a.ts:3', 'VSCODE://file/tmp/a.ts'];

test('only http, https, mailto, tel, slack and vscode reach the OS', async () => {
  for (const url of REFUSED) {
    assert.equal(isSafeExternalURL(url), false, String(url));
    const opened = [];
    assert.equal(await openExternalIfSafe({ openExternal: async (target) => { opened.push(target); } }, url, () => {}), false);
    assert.deepEqual(opened, [], String(url));
  }
  for (const url of ALLOWED) {
    assert.equal(isSafeExternalURL(url), true, url);
    const opened = [];
    assert.equal(await openExternalIfSafe({ openExternal: async (target) => { opened.push(target); } }, url, () => {}), true);
    assert.deepEqual(opened, [url]);
  }
});

test('a rejected OS launch is not an unhandled rejection', async () => {
  assert.equal(await openExternalIfSafe({ openExternal: async () => { throw new Error('no handler'); } }, 'https://example.com', () => {}), true);
});

test('the app window opens links only through the policy', () => {
  const source = fs.readFileSync(path.join(__dirname, 'main.js'), 'utf8');
  assert.doesNotMatch(source, /shell\.openExternal\(/);
  assert.match(source, /setWindowOpenHandler\(\(\{ url \}\) => \{\s*openExternalIfSafe\(shell, url\);\s*return \{ action: 'deny' \};/);
});
