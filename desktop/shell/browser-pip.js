'use strict';

// The page in its own small always-on-top window, for the webview host (after
// t3code's "pop preview into separate window"). A <webview> guest cannot move
// to another window, so the page stays in Workass and this window shows it
// live: each frame the page paints arrives through Electron's frame
// subscription (only when it paints, at most PIP_MAX_FPS), and a click or a
// scroll in the window is sent back to the page as trusted input. Closing the
// window brings the page back into the app.

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
</style></head>
<body><img id="page" alt="">
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
addEventListener('mousedown', (event) => { const p = pagePoint(event); if (p) window.workassPip.input({ type: 'mousePressed', button: event.button, ...p }); });
addEventListener('mouseup', (event) => { const p = pagePoint(event); if (p) window.workassPip.input({ type: 'mouseReleased', button: event.button, ...p }); });
addEventListener('wheel', (event) => { const p = pagePoint(event); if (p) window.workassPip.input({ type: 'mouseWheel', deltaX: event.deltaX, deltaY: event.deltaY, ...p }); }, { passive: true });
</script></body></html>`;

function pipPageURL() {
  return `data:text/html;charset=utf-8,${encodeURIComponent(PIP_PAGE)}`;
}

// Window content size that keeps the page's shape at about the same area.
function pipContentSize(viewport, width = PIP_INITIAL_WIDTH) {
  const aspect = Math.max(1, Number(viewport && viewport.height) || 900) / Math.max(1, Number(viewport && viewport.width) || 1440);
  return [Math.round(width), Math.max(160, Math.min(720, Math.round(width * aspect)))];
}

// A pointer at fractions (0..1) of the page, in its CSS pixels.
function pipInputToPage(input, viewport) {
  const fx = Number(input && input.x);
  const fy = Number(input && input.y);
  if (!Number.isFinite(fx) || !Number.isFinite(fy) || fx < 0 || fy < 0 || fx > 1 || fy > 1) return null;
  const type = String(input.type || '');
  if (!['mousePressed', 'mouseReleased', 'mouseWheel'].includes(type)) return null;
  const x = Math.round(fx * viewport.width);
  const y = Math.round(fy * viewport.height);
  if (type === 'mouseWheel') {
    const clamp = (value) => Math.max(-2000, Math.min(2000, Number(value) || 0));
    return { type, x, y, deltaX: clamp(input.deltaX), deltaY: clamp(input.deltaY) };
  }
  const button = Number(input.button) === 2 ? 'right' : Number(input.button) === 1 ? 'middle' : 'left';
  return { type, x, y, button, clickCount: 1, buttons: type === 'mousePressed' ? 1 : 0 };
}

// Opens the window; returns { window, close() }. sendInput(event) delivers a
// CDP mouse event to the page; onClosed() runs once when the window goes.
function openGuestPip({ BrowserWindow, guest, title, viewport, preload, sendInput, onClosed, embedder }) {
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
  let subscribed = false;
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
    if (mapped) void Promise.resolve(sendInput(mapped)).catch(() => {});
  };
  win.webContents.ipc.on('workass-pip:input', onInput);
  let closed = false;
  const cleanup = () => {
    if (closed) return;
    closed = true;
    clearTimeout(timer);
    if (subscribed && !guest.isDestroyed()) { try { guest.endFrameSubscription(); } catch { /* already ended */ } }
    for (const contents of throttling) {
      try { if (!contents.isDestroyed()) contents.setBackgroundThrottling(true); } catch { /* closing */ }
    }
    try { onClosed(); } catch { /* observer errors stay local */ }
  };
  win.on('closed', cleanup);
  win.webContents.once('did-finish-load', () => {
    if (win.isDestroyed() || guest.isDestroyed()) return;
    try { guest.beginFrameSubscription(false, onFrame); subscribed = true; } catch { /* no live frames */ }
    // The first frame, even if the page does not paint again.
    void guest.capturePage().then((image) => { if (!image.isEmpty()) onFrame(image); }).catch(() => {});
  });
  void win.loadURL(pipPageURL());
  try { win.show(); } catch { /* headless fake */ }
  return {
    window: win,
    close: () => { cleanup(); try { if (!win.isDestroyed()) win.destroy(); } catch { /* already gone */ } },
  };
}

module.exports = { openGuestPip, pipContentSize, pipInputToPage, pipPageURL, PIP_MAX_FPS };
