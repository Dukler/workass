'use strict';

// The page in its own small always-on-top window, for the webview host (after
// t3code's "pop preview into separate window"). A <webview> guest cannot move
// to another window, so the page stays in Workass and this window shows it
// live: each frame the page paints arrives through Electron's frame
// subscription (only when it paints, at most PIP_MAX_FPS), and the person's
// pointer (hover, clicks, double clicks, drags and text selection, scrolls)
// and keyboard (typing, shortcuts, composed text) in the window are sent back
// to the page as trusted input. Closing the window brings the page back into
// the app.

const PIP_MAX_FPS = 20;
const PIP_JPEG_QUALITY = 82;
const PIP_INITIAL_WIDTH = 420;

const PIP_PAGE = `<!doctype html>
<html><head><meta charset="utf-8">
<meta http-equiv="Content-Security-Policy" content="default-src 'none'; img-src blob:; style-src 'unsafe-inline'; script-src 'unsafe-inline'">
<meta name="color-scheme" content="dark">
<style>
html, body { width: 100%; height: 100%; margin: 0; overflow: hidden; background: #1b1a18; }
img { position: absolute; inset: 0; width: 100%; height: 100%; object-fit: contain; user-select: none; -webkit-user-drag: none; }
#keys { position: absolute; left: -9999px; top: 0; width: 1px; height: 1px; opacity: 0; }
</style></head>
<body><img id="page" alt=""><textarea id="keys" aria-hidden="true"></textarea>
<script>
const img = document.getElementById('page');
let current = '';
window.workassPip.onFrame((frame) => {
  const url = URL.createObjectURL(new Blob([frame.data], { type: 'image/jpeg' }));
  img.onload = () => { if (current) URL.revokeObjectURL(current); current = url; };
  img.src = url;
});
// Where on the page a point of the window falls (object-fit: contain).
const pagePoint = (event) => {
  const box = img.getBoundingClientRect();
  const w = img.naturalWidth || 1, h = img.naturalHeight || 1;
  const scale = Math.min(box.width / w, box.height / h);
  const left = box.left + (box.width - w * scale) / 2, top = box.top + (box.height - h * scale) / 2;
  const x = (event.clientX - left) / (w * scale), y = (event.clientY - top) / (h * scale);
  return x >= 0 && y >= 0 && x <= 1 && y <= 1 ? { x, y } : null;
};
const mods = (event) => ({ alt: event.altKey, ctrl: event.ctrlKey, meta: event.metaKey, shift: event.shiftKey });
const keys = document.getElementById('keys');
const focusKeys = () => { if (document.activeElement !== keys) keys.focus({ preventScroll: true }); };
addEventListener('mousedown', (event) => { focusKeys(); event.preventDefault(); const p = pagePoint(event); if (p) window.workassPip.input({ type: 'mousePressed', button: event.button, buttons: event.buttons, clickCount: event.detail || 1, ...mods(event), ...p }); });
addEventListener('mouseup', (event) => { const p = pagePoint(event); if (p) window.workassPip.input({ type: 'mouseReleased', button: event.button, buttons: event.buttons, clickCount: event.detail || 1, ...mods(event), ...p }); });
addEventListener('mousemove', (event) => { const p = pagePoint(event); if (p) window.workassPip.input({ type: 'mouseMoved', buttons: event.buttons, ...mods(event), ...p }); });
addEventListener('contextmenu', (event) => event.preventDefault());
addEventListener('wheel', (event) => { const p = pagePoint(event); if (p) window.workassPip.input({ type: 'mouseWheel', deltaX: event.deltaX, deltaY: event.deltaY, ...mods(event), ...p }); }, { passive: true });
// Keys go to the page as keys; text an input method composes arrives whole.
keys.addEventListener('keydown', (event) => {
  if (event.isComposing || event.keyCode === 229) return;
  event.preventDefault();
  window.workassPip.input({ type: 'keyDown', key: event.key, code: event.code, keyCode: event.keyCode, text: event.key === 'Enter' ? String.fromCharCode(13) : (event.key.length === 1 && !event.metaKey && !event.ctrlKey ? event.key : ''), ...mods(event) });
});
keys.addEventListener('keyup', (event) => {
  if (event.isComposing) return;
  event.preventDefault();
  window.workassPip.input({ type: 'keyUp', key: event.key, code: event.code, keyCode: event.keyCode, ...mods(event) });
});
keys.addEventListener('compositionend', (event) => { if (event.data) window.workassPip.input({ type: 'insertText', text: event.data }); keys.value = ''; });
focusKeys();
</script></body></html>`;

function pipPageURL() {
  return `data:text/html;charset=utf-8,${encodeURIComponent(PIP_PAGE)}`;
}

// Window content size that keeps the page's shape at about the same area.
function pipContentSize(viewport, width = PIP_INITIAL_WIDTH) {
  const aspect = Math.max(1, Number(viewport && viewport.height) || 900) / Math.max(1, Number(viewport && viewport.width) || 1440);
  return [Math.round(width), Math.max(160, Math.min(720, Math.round(width * aspect)))];
}

const PIP_TEXT_LIMIT = 2000;
// Mac editing shortcuts the page needs as commands, as Chromium's own key
// handling produces them.
const PIP_COMMANDS = { a: 'selectAll', c: 'copy', v: 'paste', x: 'cut', z: 'undo' };

function pipModifiers(input) {
  return (input.alt ? 1 : 0) | (input.ctrl ? 2 : 0) | (input.meta ? 4 : 0) | (input.shift ? 8 : 0);
}

// One input from the window as the CDP command for the page: a pointer at
// fractions (0..1) of the page (in its CSS pixels), a key, or composed text.
// Anything else, or out of bounds, is null.
function pipInputToPage(input, viewport) {
  const type = String(input && input.type || '');
  const modifiers = pipModifiers(input || {});
  if (type === 'insertText') {
    const text = String(input.text ?? '');
    if (!text || text.length > PIP_TEXT_LIMIT) return null;
    return { method: 'Input.insertText', params: { text } };
  }
  if (type === 'keyDown' || type === 'keyUp') {
    const key = String(input.key ?? '');
    if (!key || key.length > 40) return null;
    const params = { type, key, code: String(input.code ?? '').slice(0, 40), windowsVirtualKeyCode: Number(input.keyCode) || 0, modifiers };
    if (type === 'keyDown') {
      const text = typeof input.text === 'string' && input.text.length <= 4 ? input.text : '';
      if (text) Object.assign(params, { text, unmodifiedText: text });
      else params.type = 'rawKeyDown';
      const command = (input.meta || input.ctrl) && !input.alt ? PIP_COMMANDS[key.toLowerCase()] : null;
      if (command) params.commands = [command === 'undo' && input.shift ? 'redo' : command];
    }
    return { method: 'Input.dispatchKeyEvent', params };
  }
  const fx = Number(input && input.x);
  const fy = Number(input && input.y);
  if (!Number.isFinite(fx) || !Number.isFinite(fy) || fx < 0 || fy < 0 || fx > 1 || fy > 1) return null;
  if (!['mousePressed', 'mouseReleased', 'mouseMoved', 'mouseWheel'].includes(type)) return null;
  const x = Math.round(fx * viewport.width);
  const y = Math.round(fy * viewport.height);
  if (type === 'mouseWheel') {
    const clamp = (value) => Math.max(-2000, Math.min(2000, Number(value) || 0));
    return { method: 'Input.dispatchMouseEvent', params: { type, x, y, deltaX: clamp(input.deltaX), deltaY: clamp(input.deltaY), modifiers } };
  }
  const buttons = Math.max(0, Math.min(7, Number(input.buttons) || 0));
  if (type === 'mouseMoved') {
    const held = buttons & 1 ? 'left' : buttons & 2 ? 'right' : buttons & 4 ? 'middle' : 'none';
    return { method: 'Input.dispatchMouseEvent', params: { type, x, y, button: held, buttons, modifiers } };
  }
  const button = Number(input.button) === 2 ? 'right' : Number(input.button) === 1 ? 'middle' : 'left';
  const clickCount = Math.max(1, Math.min(3, Number(input.clickCount) || 1));
  return { method: 'Input.dispatchMouseEvent', params: { type, x, y, button, buttons: type === 'mousePressed' ? (buttons || 1) : buttons, clickCount, modifiers } };
}

// Opens the window; returns { window, close(), follow(guest) }. sendInput(method,
// params) delivers a CDP input command to the page; onClosed() runs once when
// the window goes. follow(guest) re-subscribes the window to a replaced page
// (after an app renderer reload): it never keeps showing a page that is gone.
function openGuestPip({ BrowserWindow, guest, title, viewport, preload, sendInput, onClosed, embedder, subscribe = null }) {
  const [width, height] = pipContentSize(viewport);
  const win = new BrowserWindow({
    width, height, useContentSize: true, minWidth: 240, minHeight: 160, show: false, title,
    alwaysOnTop: true, fullscreenable: true, backgroundColor: '#1b1a18',
    webPreferences: { preload, contextIsolation: true, sandbox: true, nodeIntegration: false, spellcheck: false },
  });
  try { win.setAlwaysOnTop(true, 'floating'); } catch { /* platform without levels */ }
  try { win.setAspectRatio(width / height); } catch { /* best effort */ }
  win.webContents.setWindowOpenHandler(() => ({ action: 'deny' }));
  win.webContents.on('will-navigate', (event) => event.preventDefault());
  let lastSent = 0;
  let pending = null;
  let timer = null;
  let unsubscribe = null;
  const deliver = () => {
    timer = null;
    if (!pending || win.isDestroyed()) return;
    const image = pending;
    pending = null;
    lastSent = Date.now();
    try {
      // At most the window's own pixels (a 1440 x 900 page arrives at 2x).
      const [contentWidth] = win.getContentSize();
      const limit = Math.max(240, Math.round(contentWidth * 2));
      const size = image.getSize();
      const fitted = size.width > limit ? image.resize({ width: limit, quality: 'good' }) : image;
      const data = fitted.toJPEG(PIP_JPEG_QUALITY);
      if (data && data.length) win.webContents.send('workass-pip:frame', { data });
    } catch { /* the window is closing */ }
  };
  const onFrame = (image) => {
    pending = image;
    if (timer) return;
    const wait = Math.max(0, 1000 / PIP_MAX_FPS - (Date.now() - lastSent));
    timer = setTimeout(deliver, wait);
  };
  // Keep the page painting while Workass itself is covered or minimized.
  const throttling = [];
  for (const contents of [guest, embedder]) {
    if (!contents || contents.isDestroyed?.() || typeof contents.setBackgroundThrottling !== 'function') continue;
    try { contents.setBackgroundThrottling(false); throttling.push(contents); } catch { /* best effort */ }
  }
  const onInput = (event, input) => {
    if (event.sender !== win.webContents) return;
    const mapped = pipInputToPage(input, viewport);
    if (mapped) void Promise.resolve(sendInput(mapped.method, mapped.params)).catch(() => {});
  };
  win.webContents.ipc.on('workass-pip:input', onInput);
  let closed = false;
  const cleanup = () => {
    if (closed) return;
    closed = true;
    clearTimeout(timer);
    if (unsubscribe) { try { unsubscribe(); } catch { /* already ended */ } unsubscribe = null; }
    for (const contents of throttling) {
      try { if (!contents.isDestroyed()) contents.setBackgroundThrottling(true); } catch { /* closing */ }
    }
    try { onClosed(); } catch { /* observer errors stay local */ }
  };
  win.on('closed', cleanup);
  let shown = guest;
  let loaded = false;
  const attachFrames = () => {
    if (closed || win.isDestroyed() || !shown || shown.isDestroyed?.()) return;
    if (unsubscribe) { try { unsubscribe(); } catch { /* the old page is gone */ } unsubscribe = null; }
    try {
      if (typeof subscribe === 'function') unsubscribe = subscribe(onFrame);
      else {
        const page = shown;
        page.beginFrameSubscription(false, onFrame);
        unsubscribe = () => { if (!page.isDestroyed()) page.endFrameSubscription(); };
      }
    } catch { /* no live frames */ }
    // The first frame, even if the page does not paint again.
    void shown.capturePage().then((image) => { if (!image.isEmpty()) onFrame(image); }).catch(() => {});
  };
  win.webContents.once('did-finish-load', () => { loaded = true; attachFrames(); });
  void win.loadURL(pipPageURL());
  try { win.show(); } catch { /* headless fake */ }
  return {
    window: win,
    follow: (next) => {
      shown = next;
      if (next && typeof next.setBackgroundThrottling === 'function' && !throttling.includes(next)) {
        try { next.setBackgroundThrottling(false); throttling.push(next); } catch { /* best effort */ }
      }
      if (loaded) attachFrames();
    },
    close: () => { cleanup(); try { if (!win.isDestroyed()) win.destroy(); } catch { /* already gone */ } },
  };
}

module.exports = { openGuestPip, pipContentSize, pipInputToPage, pipPageURL, PIP_MAX_FPS };
