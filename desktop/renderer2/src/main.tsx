import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import './styles/tokens.css';
import './styles/app.css';
import { App } from './components/App';
import { BrowserSurfaceHost } from './components/BrowserSurfaceHost';
import { browserApi, browserHostedInRenderer } from './browser';
import { store } from './store/store';

// The preload exposes the host platform synchronously. Apply shell classes
// before React mounts so the first painted titlebar already has the right OS
// geometry (macOS traffic lights vs. Windows native caption controls).
const electron = navigator.userAgent.includes('Electron');
document.documentElement.classList.toggle('electron', electron);
document.documentElement.classList.toggle(
  'electron-windows',
  electron && window.workassWindow?.platform === 'win32',
);

// Kick off boot (wire events, hydrate from server, load archives). The local
// mirror already seeded the store synchronously in the constructor, so the
// first paint is instant.
void store.init();

const root = document.getElementById('root');
if (root) {
  createRoot(root).render(
    <StrictMode>
      <App />
    </StrictMode>,
  );
}

// The Workass browser pages (the shell's webview host): their own root, placed
// before the app's so that, at equal stacking order, the app's bars, menus and
// dialogs stay above a page.
const browser = browserApi();
if (root && browser && browserHostedInRenderer(browser)) {
  const layer = document.createElement('div');
  layer.id = 'browser-surfaces';
  document.body.insertBefore(layer, root);
  createRoot(layer).render(<BrowserSurfaceHost api={browser} />);
}
