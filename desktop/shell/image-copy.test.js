'use strict';

const assert = require('node:assert/strict');
const { EventEmitter } = require('node:events');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const test = require('node:test');
const { copyImageAt, copyText, installImageCopyMenu, openImageExternally } = require('./image-copy');

class FakeWebContents extends EventEmitter {
  constructor() {
    super();
    this.copies = [];
    this.destroyed = false;
  }
  isDestroyed() { return this.destroyed; }
  copyImageAt(x, y) { this.copies.push({ x, y }); }
  removeListener(...args) {
    if (this.destroyed) throw new Error('Object has been destroyed');
    return super.removeListener(...args);
  }
}

test('right-clicking any rendered image offers one native Copy Image action', () => {
  const webContents = new FakeWebContents();
  const win = { webContents, isDestroyed: () => false };
  const popups = [];
  const Menu = {
    buildFromTemplate(template) {
      return { popup: (options) => popups.push({ template, options }) };
    },
  };
  const remove = installImageCopyMenu({ win, Menu });

  webContents.emit('context-menu', {}, { mediaType: 'none', x: 4, y: 8 });
  assert.equal(popups.length, 0);
  webContents.emit('context-menu', {}, { mediaType: 'image', x: 44, y: 81 });
  assert.equal(popups.length, 1);
  assert.equal(popups[0].template[0].label, 'Copy Image');
  popups[0].template[0].click();
  assert.deepEqual(webContents.copies, [{ x: 44, y: 81 }]);

  remove();
  webContents.emit('context-menu', {}, { mediaType: 'image', x: 1, y: 2 });
  assert.equal(popups.length, 1);
});

test('image menu cleanup is safe after Electron destroys the window contents', () => {
  const webContents = new FakeWebContents();
  const win = { webContents, isDestroyed: () => true };
  const Menu = { buildFromTemplate: () => ({ popup() {} }) };
  const remove = installImageCopyMenu({ win, Menu });

  webContents.destroyed = true;
  assert.doesNotThrow(remove);
});

test('keyboard copy coordinates are bounded to the owning window', () => {
  const webContents = new FakeWebContents();
  const win = { webContents, isDestroyed: () => false, getContentSize: () => [900, 600] };
  assert.equal(copyImageAt(win, { x: 120.4, y: 88.8 }), true);
  assert.deepEqual(webContents.copies, [{ x: 120, y: 89 }]);
  assert.equal(copyImageAt(win, { x: -1, y: 20 }), false);
  assert.equal(copyImageAt(win, { x: 900, y: 20 }), false);
  assert.equal(copyImageAt(win, { x: 901, y: 20 }), false);
  assert.equal(copyImageAt(win, { x: '120', y: 20 }), false);
  assert.deepEqual(webContents.copies, [{ x: 120, y: 89 }]);
});

test('native text copy writes exact code bytes and fails closed', () => {
  const writes = [];
  const clipboard = { writeText: (value) => { writes.push(value); } };
  const raw = 'line 1\r\n  line 2\n';
  assert.equal(copyText(clipboard, raw), true);
  assert.deepEqual(writes, [raw]);
  assert.equal(copyText(clipboard, null), false);
  assert.equal(copyText({ writeText: () => { throw new Error('clipboard unavailable'); } }, raw), false);
});

test('external image open materializes validated raster bytes for the OS default viewer', async (t) => {
  const tempRoot = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-image-open-test-'));
  t.after(() => fs.rmSync(tempRoot, { recursive: true, force: true }));
  const opened = [];
  const png = Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00]);
  const result = await openImageExternally(
    { bytes: png.buffer.slice(png.byteOffset, png.byteOffset + png.byteLength), mimeType: 'image/png' },
    { fs, path, tempRoot, randomId: () => 'fixture', shell: { openPath: async (file) => { opened.push(file); return ''; } } },
  );
  assert.equal(result.opened, true);
  assert.equal(opened.length, 1);
  assert.match(opened[0], /workass-image-fixture[\/]image\.png$/);
  assert.deepEqual(fs.readFileSync(opened[0]), png);
  assert.equal(result.cleanupPath, path.dirname(opened[0]));
});

test('external image open rejects non-raster bytes without invoking the OS', async (t) => {
  const tempRoot = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-image-open-reject-'));
  t.after(() => fs.rmSync(tempRoot, { recursive: true, force: true }));
  let opened = false;
  const svg = Buffer.from('<svg xmlns="http://www.w3.org/2000/svg"/>');
  const result = await openImageExternally(
    { bytes: svg.buffer.slice(svg.byteOffset, svg.byteOffset + svg.byteLength), mimeType: 'image/svg+xml' },
    { fs, path, tempRoot, randomId: () => 'svg', shell: { openPath: async () => { opened = true; return ''; } } },
  );
  assert.deepEqual(result, { opened: false, cleanupPath: null });
  assert.equal(opened, false);
});

test('preload and main wire the external image action through an owned IPC handler', () => {
  const preload = fs.readFileSync(path.join(__dirname, 'preload.js'), 'utf8');
  const main = fs.readFileSync(path.join(__dirname, 'main.js'), 'utf8');
  assert.match(preload, /openImageExternal:[^\n]*workass-image:open-external/);
  assert.match(main, /ipcMain\.handle\('workass-image:open-external'/);
  assert.match(main, /if \(!own\(event\)\) return false/);
  assert.match(main, /openImageExternally\(payload, \{[^}]*shell \}\)/);
});

test('preload and main wire native text copy through an owned IPC handler', () => {
  const preload = fs.readFileSync(path.join(__dirname, 'preload.js'), 'utf8');
  const main = fs.readFileSync(path.join(__dirname, 'main.js'), 'utf8');
  assert.match(preload, /copyText:[^\n]*workass-clipboard:copy-text/);
  assert.match(main, /ipcMain\.handle\('workass-clipboard:copy-text'/);
  assert.match(main, /own\(event\) \? copyText\(clipboard, text\) : false/);
});
