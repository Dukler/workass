'use strict';

const crypto = require('node:crypto');
const defaultFS = require('node:fs');
const os = require('node:os');
const defaultPath = require('node:path');

const MAX_EXTERNAL_IMAGE_BYTES = 16 * 1024 * 1024;

function liveWebContents(win) {
  try {
    if (!win || win.isDestroyed?.()) return null;
    const contents = win.webContents;
    if (!contents || contents.isDestroyed?.()) return null;
    return contents;
  } catch {
    return null;
  }
}

function copyImageAt(win, payload) {
  const contents = liveWebContents(win);
  if (!contents || typeof contents.copyImageAt !== 'function') return false;
  const x = payload && payload.x;
  const y = payload && payload.y;
  if (!Number.isFinite(x) || !Number.isFinite(y)) return false;
  const [width, height] = typeof win.getContentSize === 'function' ? win.getContentSize() : [Number.MAX_SAFE_INTEGER, Number.MAX_SAFE_INTEGER];
  const roundedX = Math.round(x);
  const roundedY = Math.round(y);
  if (roundedX < 0 || roundedY < 0 || roundedX >= width || roundedY >= height) return false;
  try {
    contents.copyImageAt(roundedX, roundedY);
    return true;
  } catch {
    return false;
  }
}

// Page clipboard writes can be denied by Chromium's permission layer even for
// a user-initiated click. Text copied from the renderer stays shell-local and
// uses Electron's native clipboard implementation on every desktop platform.
function copyText(clipboard, text) {
  if (!clipboard || typeof clipboard.writeText !== 'function' || typeof text !== 'string') return false;
  try {
    clipboard.writeText(text);
    return true;
  } catch {
    return false;
  }
}

// Electron has no default context menu. Install the one native image action the
// browser would normally provide; copyImageAt lets Chromium copy the decoded
// pixels regardless of whether the source is a data URL or hosted artifact.
function installImageCopyMenu({ win, Menu }) {
  const contents = liveWebContents(win);
  if (!contents || !Menu || typeof Menu.buildFromTemplate !== 'function') return () => {};
  const onContextMenu = (_event, params = {}) => {
    if (params.mediaType !== 'image' || !liveWebContents(win)) return;
    const menu = Menu.buildFromTemplate([{
      label: 'Copy Image',
      click: () => { copyImageAt(win, { x: params.x, y: params.y }); },
    }]);
    menu.popup({ window: win });
  };
  contents.on('context-menu', onContextMenu);
  return () => {
    try {
      if (!contents.isDestroyed?.()) contents.removeListener('context-menu', onContextMenu);
    } catch { /* Electron may destroy webContents before BrowserWindow emits closed */ }
  };
}

function payloadBuffer(payload) {
  const bytes = payload && payload.bytes;
  if (Buffer.isBuffer(bytes)) return Buffer.from(bytes);
  if (bytes instanceof ArrayBuffer) return Buffer.from(bytes);
  if (ArrayBuffer.isView(bytes)) return Buffer.from(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  return null;
}

function rasterExtension(bytes) {
  if (bytes.length >= 8 && bytes.subarray(0, 8).equals(Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]))) return 'png';
  if (bytes.length >= 3 && bytes[0] === 0xff && bytes[1] === 0xd8 && bytes[2] === 0xff) return 'jpg';
  if (bytes.length >= 6 && (bytes.subarray(0, 6).toString('ascii') === 'GIF87a' || bytes.subarray(0, 6).toString('ascii') === 'GIF89a')) return 'gif';
  if (bytes.length >= 12 && bytes.subarray(0, 4).toString('ascii') === 'RIFF' && bytes.subarray(8, 12).toString('ascii') === 'WEBP') return 'webp';
  return null;
}

async function openImageExternally(payload, deps = {}) {
  const fs = deps.fs || defaultFS;
  const path = deps.path || defaultPath;
  const tempRoot = deps.tempRoot || os.tmpdir();
  const nativeShell = deps.shell;
  const randomId = deps.randomId || crypto.randomUUID;
  const bytes = payloadBuffer(payload);
  const extension = bytes && bytes.length <= MAX_EXTERNAL_IMAGE_BYTES ? rasterExtension(bytes) : null;
  if (!bytes || bytes.length === 0 || !extension || !nativeShell || typeof nativeShell.openPath !== 'function') {
    return { opened: false, cleanupPath: null };
  }

  let cleanupPath = null;
  try {
    const id = String(randomId()).replace(/[^A-Za-z0-9_-]/g, '').slice(0, 80);
    if (!id) return { opened: false, cleanupPath: null };
    cleanupPath = path.join(tempRoot, `workass-image-${id}`);
    fs.mkdirSync(cleanupPath, { mode: 0o700 });
    const imagePath = path.join(cleanupPath, `image.${extension}`);
    fs.writeFileSync(imagePath, bytes, { flag: 'wx', mode: 0o600 });
    const error = await nativeShell.openPath(imagePath);
    if (error) throw new Error('default image viewer rejected the file');
    return { opened: true, cleanupPath };
  } catch {
    if (cleanupPath) {
      try { fs.rmSync(cleanupPath, { recursive: true, force: true }); } catch { /* best effort */ }
    }
    return { opened: false, cleanupPath: null };
  }
}

module.exports = { copyImageAt, copyText, installImageCopyMenu, openImageExternally };
