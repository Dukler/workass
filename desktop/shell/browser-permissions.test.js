'use strict';

// Ajustes · Permisos in the shell: the owner's download rule reaches the
// browser with each agent action and holds the agent's downloads, and the
// profile's signed-in sites are counted and signed out of.

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { takeDownload } = require('./browser-actions');
const { BrowserManager, cookieSite } = require('./browser-manager');

function downloadItem(name) {
  const events = {};
  return {
    cancelled: false, path: '',
    getFilename: () => name, getURL: () => 'https://files.example/' + name,
    getTotalBytes: () => 10, getReceivedBytes: () => 10,
    setSavePath(file) { this.path = file; },
    on(event, fn) { events[event] = fn; }, once(event, fn) { events[event] = fn; },
    cancel() { this.cancelled = true; },
  };
}

function manager(profile = {}) {
  return new BrowserManager({
    win: {}, WebContentsView: class {}, session: { fromPartition: () => profile },
    chromeVersion: '140.0.7339.1', platform: 'darwin',
  });
}

test('an agent download is cancelled and reported while the owner blocks downloads', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'wk-perm-dl-'));
  try {
    const entry = { chatId: 'perm-chat', visible: false, agentActiveAt: Date.now() };
    const blocked = downloadItem('report.pdf');
    assert.equal(takeDownload({ downloadsRoot: root, agentDownloadPolicy: 'block' }, entry, blocked), true);
    assert.equal(blocked.cancelled, true);
    assert.equal(blocked.path, '', 'nothing was saved');
    const event = entry.events.find((item) => item.type === 'download_blocked');
    assert.ok(event && /settings/.test(event.reason), 'the agent reads why');

    const allowed = downloadItem('report.pdf');
    assert.equal(takeDownload({ downloadsRoot: root, agentDownloadPolicy: 'allow' }, entry, allowed), true);
    assert.equal(allowed.cancelled, false);
    assert.ok(allowed.path.startsWith(root), 'an allowed download is saved in the chat folder');
  } finally {
    fs.rmSync(root, { recursive: true, force: true });
  }
});

test('a person\'s own download is not the agent rule\'s', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'wk-perm-dl-'));
  try {
    const person = downloadItem('mine.zip');
    assert.equal(takeDownload({ downloadsRoot: root, agentDownloadPolicy: 'block' }, { chatId: 'c', visible: true, agentActiveAt: 0 }, person), false);
    assert.equal(person.cancelled, false);
  } finally {
    fs.rmSync(root, { recursive: true, force: true });
  }
});

test('each agent action brings the owner\'s current download rule', async () => {
  const browser = manager();
  assert.equal(browser.agentDownloadPolicy, 'allow', 'allowed until a daemon says otherwise');
  await browser.browserControl('browser.list', { chatId: 'perm-chat', downloadPolicy: 'block' });
  assert.equal(browser.agentDownloadPolicy, 'block');
  await browser.browserControl('browser.list', { chatId: 'perm-chat' });
  assert.equal(browser.agentDownloadPolicy, 'block', 'a call with no rule keeps the last one');
  await browser.browserControl('browser.list', { chatId: 'perm-chat', downloadPolicy: 'nonsense' });
  assert.equal(browser.agentDownloadPolicy, 'block', 'an unknown rule is ignored');
  await browser.browserControl('browser.list', { chatId: 'perm-chat', downloadPolicy: 'allow' });
  assert.equal(browser.agentDownloadPolicy, 'allow');
});

test('cookie domains are counted by site', () => {
  assert.equal(cookieSite('.accounts.example.com'), 'example.com');
  assert.equal(cookieSite('example.com'), 'example.com');
  assert.equal(cookieSite('www.example.co.uk'), 'example.co.uk');
  assert.equal(cookieSite('home.banco.com.ar'), 'banco.com.ar');
  assert.equal(cookieSite('mail.example.io'), 'example.io');
  assert.equal(cookieSite('127.0.0.1'), '127.0.0.1');
  assert.equal(cookieSite('localhost'), 'localhost');
});

test('the profile lists its signed-in sites and closes every session', async () => {
  let cookies = [
    { domain: '.example.com' }, { domain: 'accounts.example.com' }, { domain: '.shop.example.co.uk' }, { domain: '' },
  ];
  const cleared = [];
  const profile = {
    cookies: { get: async (filter) => { assert.deepEqual(filter, {}); return cookies; } },
    clearStorageData: async (options) => { cleared.push(options === undefined ? 'all' : options); cookies = []; },
    clearAuthCache: async () => { cleared.push('auth'); },
  };
  const browser = manager(profile);
  assert.deepEqual(await browser.profileSites(), { sites: ['example.co.uk', 'example.com'] });
  assert.deepEqual(await browser.clearProfileSessions(), { sites: [] });
  assert.deepEqual(cleared, ['all', 'auth'], 'every storage, not only cookies');
});
