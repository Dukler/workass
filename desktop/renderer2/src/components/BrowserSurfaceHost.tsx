import { useEffect, useLayoutEffect, useRef, useState } from 'react';
import type { WorkassBrowserApi, WorkassBrowserHost } from '../browser';
import {
  browserPagePresentation, onBrowserPagePresentation, refreshBrowserPage, surfaceLayout, type SurfaceViewport,
} from '../browser-surface';
import { hostZoom } from './floating-browser-layout';

// Every Workass browser page this window shows, each in its own <webview>
// (the shell's webview host). Mounted once, in its own React root beside the
// app, so nothing the app re-renders or remounts (a chat switch, settings
// taking the view over) can move or drop a page. Each page sits behind the app
// until a slot (the docked pane, the floating frame, full screen) presents it;
// see browser-surface.ts.

interface WebviewElement extends HTMLElement {
  getWebContentsId(): number;
}

const viewports = new Map<string, SurfaceViewport>();

function zoomNow(): number {
  return hostZoom(window.outerWidth, window.innerWidth);
}

export function BrowserSurfaceHost({ api }: { api: WorkassBrowserApi }) {
  const [hosts, setHosts] = useState<WorkassBrowserHost[]>([]);

  useEffect(() => {
    let alive = true;
    const take = (list: WorkassBrowserHost[]) => {
      if (!alive) return;
      for (const host of list) if (host.viewport) viewports.set(host.chatId, host.viewport);
      setHosts(list);
    };
    void api.hosts?.().then(take).catch(() => {});
    const offHosts = api.onHosts?.(take);
    const offState = api.onState((state) => {
      if (!state.viewport) return;
      const previous = viewports.get(state.chatId);
      if (previous && previous.width === state.viewport.width && previous.height === state.viewport.height) return;
      viewports.set(state.chatId, state.viewport);
      refreshBrowserPage(state.chatId);
    });
    return () => { alive = false; offHosts?.(); offState(); };
  }, [api]);

  // While a press that began in the app is held (a drag of the floating
  // frame, a pane resize, a text selection), the pages ignore the pointer:
  // otherwise moving over a page hands every later event to that page and the
  // app never sees the move or the release. Hovering a handle that starts one
  // (the floating frame's grip and edges, a pane's resize handle) already
  // sets it, so the pages are out of the way before the press: the browser
  // routes the pointer by what was last drawn.
  useEffect(() => {
    const root = document.documentElement;
    const HANDLES = '.fbrw-grip, .fbrw-edge, .rzh';
    let pressed = false;
    const set = (on: boolean) => root.classList.toggle('wbs-gesture', on);
    const down = (event: PointerEvent) => {
      const target = event.target as Element | null;
      if (target && target.closest && target.closest('.wbs-page')) return;
      pressed = true;
      set(true);
    };
    const up = () => { pressed = false; set(false); };
    const over = (event: PointerEvent) => {
      const target = event.target as Element | null;
      if (target && target.closest && target.closest(HANDLES)) set(true);
    };
    const out = (event: PointerEvent) => {
      if (pressed) return;
      const next = event.relatedTarget as Element | null;
      if (!(next && next.closest && next.closest(HANDLES))) set(false);
    };
    addEventListener('pointerdown', down, true);
    addEventListener('pointerup', up, true);
    addEventListener('pointercancel', up, true);
    addEventListener('pointerover', over, true);
    addEventListener('pointerout', out, true);
    addEventListener('blur', up);
    return () => {
      removeEventListener('pointerdown', down, true);
      removeEventListener('pointerup', up, true);
      removeEventListener('pointercancel', up, true);
      removeEventListener('pointerover', over, true);
      removeEventListener('pointerout', out, true);
      removeEventListener('blur', up);
      up();
    };
  }, []);

  // A fixed order: React must never reorder these elements (a moved webview
  // loses its page).
  const ordered = [...hosts].sort((a, b) => (a.hostKey < b.hostKey ? -1 : a.hostKey > b.hostKey ? 1 : 0));
  return (
    <div className="wbs">
      {ordered.map((host) => <HostedPage key={host.hostKey} api={api} host={host} />)}
    </div>
  );
}

function HostedPage({ api, host }: { api: WorkassBrowserApi; host: WorkassBrowserHost }) {
  const wrapper = useRef<HTMLDivElement>(null);
  const page = useRef<HTMLDivElement>(null);
  const chatId = useRef(host.chatId);
  chatId.current = host.chatId;

  // The <webview> is created here, once, and never touched by React again.
  useLayoutEffect(() => {
    const box = page.current;
    if (!box) return;
    const webview = document.createElement('webview') as WebviewElement;
    webview.setAttribute('partition', 'persist:workass-browser');
    webview.setAttribute('allowpopups', '');
    webview.setAttribute('src', 'about:blank');
    webview.className = 'wbs-webview';
    let disposed = false;
    let registered = -1;
    const register = (attempt = 0) => {
      if (disposed) return;
      let id = -1;
      try { id = webview.getWebContentsId(); } catch { /* not attached yet */ }
      if (!(id > 0) || id === registered) return;
      void api.registerGuest?.(host.hostKey, id).then((reply) => {
        if (disposed) return;
        if (reply?.ok) registered = id;
        else if (attempt < 3) window.setTimeout(() => register(attempt + 1), 150 * (attempt + 1));
      }).catch(() => {
        if (!disposed && attempt < 3) window.setTimeout(() => register(attempt + 1), 150 * (attempt + 1));
      });
    };
    const onAttach = () => register(0);
    webview.addEventListener('did-attach', onAttach);
    webview.addEventListener('dom-ready', onAttach);
    box.appendChild(webview);
    return () => {
      disposed = true;
      webview.removeEventListener('did-attach', onAttach);
      webview.removeEventListener('dom-ready', onAttach);
      webview.remove();
    };
  }, [api, host.hostKey]);

  // Placed synchronously whenever its slot or its viewport changes.
  useLayoutEffect(() => {
    if (host.viewport) viewports.set(host.chatId, host.viewport);
    const apply = () => {
      const outer = wrapper.current;
      const inner = page.current;
      if (!outer || !inner) return;
      const viewport = viewports.get(chatId.current) ?? host.viewport ?? { width: 1440, height: 900 };
      const layout = surfaceLayout(browserPagePresentation(chatId.current), viewport, zoomNow());
      const w = layout.wrapper;
      outer.style.left = `${w.left}px`;
      outer.style.top = `${w.top}px`;
      outer.style.width = `${w.width}px`;
      outer.style.height = `${w.height}px`;
      outer.style.borderRadius = w.radius ? `${w.radius}px` : '';
      outer.style.zIndex = String(w.z);
      outer.style.pointerEvents = w.interactive ? 'auto' : 'none';
      outer.toggleAttribute('inert', !w.shown);
      outer.setAttribute('aria-hidden', w.shown ? 'false' : 'true');
      outer.dataset.shown = w.shown ? '1' : '0';
      const p = layout.page;
      inner.style.width = `${p.width}px`;
      inner.style.height = `${p.height}px`;
      inner.style.transform = p.scale === 1 && !p.offsetX && !p.offsetY ? '' : `translate(${p.offsetX}px, ${p.offsetY}px) scale(${p.scale})`;
    };
    apply();
    const off = onBrowserPagePresentation(host.chatId, apply);
    addEventListener('resize', apply);
    return () => { off(); removeEventListener('resize', apply); };
  }, [host.chatId, host.viewport?.width, host.viewport?.height]);

  return (
    <div ref={wrapper} className="wbs-page" data-browser-host={host.hostKey} data-chat={host.chatId}>
      <div ref={page} className="wbs-viewport" />
    </div>
  );
}
