import { useEffect, useLayoutEffect, useRef, useState } from 'react';
import type { PointerEvent as ReactPointerEvent } from 'react';
import {
  browserApi, browserViewportForBounds, localBrowserOwnsChat, sameBrowserBounds,
  type WorkassBrowserApi, type WorkassBrowserBounds,
} from '../browser';
import { store, useApp } from '../store/store';
import { chatPane } from '../store/right-pane';
import type { Chat } from '../store/types';
import { IcClose, IcFullscreen, IcFullscreenExit, IcGrip, IcPopOut, IcRail } from '../icons';
import {
  clampFloatWidth, floatFrame, nearestCorner, resizeFloatWidth, type FloatCorner, type FloatGeometry,
} from './floating-browser-layout';

// The chat's browser floating over the transcript (approved mock
// browser-pip-v1 · B, 2026-10-13, after t3code's preview mini player): the page
// keeps its own layout and is shown scaled into a small frame. The native page
// view paints above every HTML element, so its controls sit in a pill just
// outside the frame, never on it, and the page steps aside while a menu, a
// dialog or the open Working list would land on it.
const GEOMETRY_KEY = 'workass.floatingBrowser.v1';
const OVERLAYS = '.pop, [role="menu"], [role="dialog"], .thinklive.open, .lightbox, .cmdbar';

function loadGeometry(): FloatGeometry {
  try {
    const raw = JSON.parse(localStorage.getItem(GEOMETRY_KEY) ?? 'null') as Partial<FloatGeometry> | null;
    if (raw && ['br', 'bl', 'tr', 'tl'].includes(String(raw.corner)) && Number.isFinite(raw.width)) {
      return { corner: raw.corner as FloatCorner, width: Number(raw.width) };
    }
  } catch { /* storage unavailable */ }
  return { corner: 'br', width: 340 };
}

function saveGeometry(geometry: FloatGeometry) {
  try { localStorage.setItem(GEOMETRY_KEY, JSON.stringify(geometry)); } catch { /* storage unavailable */ }
}

function rectOf(el: Element): WorkassBrowserBounds {
  const rect = el.getBoundingClientRect();
  return { x: Math.round(rect.left), y: Math.round(rect.top), width: Math.max(1, Math.round(rect.width)), height: Math.max(1, Math.round(rect.height)) };
}

function overlaps(a: DOMRect, b: WorkassBrowserBounds): boolean {
  return a.width > 0 && a.height > 0 && a.left < b.x + b.width && a.right > b.x && a.top < b.y + b.height && a.bottom > b.y;
}

export function FloatingBrowser({ chat }: { chat: Chat }) {
  const app = useApp();
  const api = browserApi();
  const mode = store.browserPresentationFor(chat.id);
  if (!api?.supported || !app.hasBrowserChannels || !localBrowserOwnsChat(chat.id, chat.machineId)) return null;
  if (chatPane(chat) === 'browser' || (mode !== 'float' && mode !== 'full')) return null;
  return <FloatingBrowserView key={chat.id} api={api} chat={chat} mode={mode} />;
}

function FloatingBrowserView({ api, chat, mode }: { api: WorkassBrowserApi; chat: Chat; mode: 'float' | 'full' }) {
  const app = useApp();
  const host = useRef<HTMLDivElement>(null);
  const frame = useRef<HTMLDivElement>(null);
  const [geometry, setGeometry] = useState<FloatGeometry>(loadGeometry);
  const [container, setContainer] = useState<{ width: number; height: number; reserve: number } | null>(null);
  const [aspect, setAspect] = useState(0.625);
  const [drag, setDrag] = useState<{ x: number; y: number } | null>(null);
  const [url, setUrl] = useState('');
  const [blocked, setBlocked] = useState(false);
  const full = mode === 'full';
  // Whether the frame element is on screen (full screen, or floating once measured).
  const framed = full || container !== null;

  // The space the player floats in: the transcript, minus the Working line
  // and the gap above it at the bottom.
  useLayoutEffect(() => {
    const el = host.current?.parentElement;
    if (!el) return;
    const measure = () => {
      const reserve = parseFloat(getComputedStyle(el).getPropertyValue('--thinklive-h')) || 32;
      setContainer({ width: el.clientWidth, height: el.clientHeight, reserve: reserve + 14 });
    };
    measure();
    const observer = new ResizeObserver(measure);
    observer.observe(el);
    return () => observer.disconnect();
  }, []);

  // The page's own proportions decide the frame's height.
  useEffect(() => api.onState((state) => {
    if (state.chatId !== chat.id) return;
    const viewport = state.viewport;
    if (viewport && viewport.width > 0) setAspect(viewport.height / viewport.width);
    setUrl(state.url === 'about:blank' ? '' : state.url);
  }), [api, chat.id]);

  // A menu, dialog or the open Working list over the frame hides the page
  // until it goes, as the image lightbox always did.
  useEffect(() => {
    let frameRequest = 0;
    const check = () => {
      frameRequest = 0;
      const el = frame.current;
      if (!el) return;
      const bounds = rectOf(el);
      const covered = !!app.imageLightbox || Array.from(document.querySelectorAll(OVERLAYS))
        .some((overlay) => !el.contains(overlay) && !host.current?.contains(overlay) && overlaps(overlay.getBoundingClientRect(), bounds));
      setBlocked((current) => (current === covered ? current : covered));
    };
    const schedule = () => { if (!frameRequest) frameRequest = requestAnimationFrame(check); };
    const observer = new MutationObserver(schedule);
    observer.observe(document.body, { subtree: true, childList: true, attributes: true, attributeFilter: ['class', 'open', 'role'] });
    schedule();
    return () => { observer.disconnect(); if (frameRequest) cancelAnimationFrame(frameRequest); };
  }, [app.imageLightbox]);

  // Hand the native page view the frame's rectangle; full screen also gives
  // the page a viewport of its real size, floating keeps its own and fits it.
  useLayoutEffect(() => {
    const el = frame.current;
    if (!el || blocked) return;
    let disposed = false;
    let activated = false;
    let syncing = false;
    let pending: WorkassBrowserBounds | null = null;
    let last: WorkassBrowserBounds | null = null;
    const viewportFor = (bounds: WorkassBrowserBounds) => (full ? browserViewportForBounds(bounds) : undefined);
    const flush = async () => {
      if (!activated || syncing) return;
      syncing = true;
      try {
        while (!disposed && pending) {
          const bounds = pending;
          pending = null;
          if (sameBrowserBounds(last, bounds)) continue;
          await api.resize({ chatId: chat.id, bounds, viewport: viewportFor(bounds) });
          last = bounds;
        }
      } catch { /* the next sync retries */ } finally {
        syncing = false;
        if (!disposed && pending) void flush();
      }
    };
    const sync = () => {
      if (disposed || !el.isConnected) return;
      const bounds = rectOf(el);
      if (sameBrowserBounds(last, bounds)) return;
      pending = bounds;
      void flush();
    };
    const observer = new ResizeObserver(sync);
    observer.observe(el);
    // A drag moves the frame without resizing it; the drag effect says so.
    el.addEventListener('workass-frame-moved', sync);
    const root = document.getElementById('root');
    const layout = new MutationObserver(sync);
    if (root) layout.observe(root, { attributes: true, attributeFilter: ['class', 'style'] });
    addEventListener('resize', sync);
    const bounds = rectOf(el);
    void api.activate({ chatId: chat.id, conversationId: chat.chatId, bounds, viewport: viewportFor(bounds) }).then((state) => {
      if (disposed) return;
      if (state.viewport && state.viewport.width > 0) setAspect(state.viewport.height / state.viewport.width);
      setUrl(state.url === 'about:blank' ? '' : state.url);
      activated = true;
      last = bounds;
      sync();
    }).catch(() => { /* the pane's retry covers a page still loading */ });
    return () => {
      disposed = true;
      observer.disconnect();
      el.removeEventListener('workass-frame-moved', sync);
      layout.disconnect();
      removeEventListener('resize', sync);
      void api.hide(chat.id);
    };
    // The floating frame exists only once its container is measured, so
    // `placed` (frame present) is part of what this effect depends on.
  }, [api, chat.id, chat.chatId, blocked, full, framed]);

  // The page follows the frame as it is dragged, resized or re-placed.
  useLayoutEffect(() => {
    frame.current?.dispatchEvent(new Event('workass-frame-moved'));
  }, [drag, geometry, container, aspect]);

  // Esc leaves full screen while the app has focus.
  useEffect(() => {
    if (!full) return;
    const onKey = (event: KeyboardEvent) => { if (event.key === 'Escape') store.toggleBrowserFullscreen(); };
    addEventListener('keydown', onKey);
    return () => removeEventListener('keydown', onKey);
  }, [full]);

  const canPopOut = typeof api.popOut === 'function';
  const controls = (
    <>
      <button type="button" className="fbrw-btn" title="Volver al panel" aria-label="Volver al panel" onClick={() => store.dockBrowser()}><IcRail /></button>
      <button type="button" className="fbrw-btn" title={full ? 'Salir de pantalla completa' : 'Pantalla completa'} aria-label={full ? 'Salir de pantalla completa' : 'Pantalla completa'} onClick={() => store.toggleBrowserFullscreen()}>
        {full ? <IcFullscreenExit /> : <IcFullscreen />}
      </button>
      {canPopOut && <button type="button" className="fbrw-btn" title="Ventana aparte" aria-label="Ventana aparte" onClick={() => store.popOutBrowser()}><IcPopOut /></button>}
      <button type="button" className="fbrw-btn" title="Cerrar navegador" aria-label="Cerrar navegador" onClick={() => store.closeFloatingBrowser()}><IcClose /></button>
    </>
  );

  if (full) {
    return (
      <div ref={host} className="fbrw-full" role="region" aria-label="Navegador a pantalla completa">
        <div className="fbrw-fullbar">
          <span className="fbrw-url mono" title={url}>{url || 'Navegador'}</span>
          {controls}
        </div>
        <div ref={frame} className="fbrw-fullview" />
      </div>
    );
  }

  if (!container) return <div ref={host} className="fbrw-host" />;
  const placed = floatFrame({ geometry, aspect, container });
  const left = drag ? drag.x : placed.x;
  const top = drag ? drag.y : placed.y;

  const beginMove = (event: ReactPointerEvent<HTMLElement>) => {
    if (event.button !== 0) return;
    const startX = event.clientX, startY = event.clientY, fromX = left, fromY = top;
    const target = event.currentTarget;
    target.setPointerCapture(event.pointerId);
    let latest = { x: fromX, y: fromY };
    const move = (next: PointerEvent) => {
      latest = { x: fromX + next.clientX - startX, y: fromY + next.clientY - startY };
      setDrag(latest);
    };
    const end = () => {
      target.removeEventListener('pointermove', move);
      target.removeEventListener('pointerup', end);
      target.removeEventListener('pointercancel', end);
      const next = { ...geometry, corner: nearestCorner(latest, placed, container) };
      setDrag(null);
      setGeometry(next);
      saveGeometry(next);
    };
    target.addEventListener('pointermove', move);
    target.addEventListener('pointerup', end);
    target.addEventListener('pointercancel', end);
  };

  const beginResize = (event: ReactPointerEvent<HTMLElement>) => {
    if (event.button !== 0) return;
    const startX = event.clientX, startWidth = placed.width;
    const target = event.currentTarget;
    target.setPointerCapture(event.pointerId);
    const move = (next: PointerEvent) => {
      const width = clampFloatWidth(resizeFloatWidth(geometry.corner, startWidth, next.clientX - startX), aspect, container);
      setGeometry((current) => ({ ...current, width }));
    };
    const end = () => {
      target.removeEventListener('pointermove', move);
      target.removeEventListener('pointerup', end);
      target.removeEventListener('pointercancel', end);
      setGeometry((current) => { saveGeometry(current); return current; });
    };
    target.addEventListener('pointermove', move);
    target.addEventListener('pointerup', end);
    target.addEventListener('pointercancel', end);
  };

  const pillBelow = geometry.corner === 'tr' || geometry.corner === 'tl';
  const pillLeft = geometry.corner === 'bl' || geometry.corner === 'tl';
  return (
    <div ref={host} className="fbrw-host">
      <section className="fbrw" aria-label="Navegador flotante" style={{ left, top, width: placed.width, height: placed.height }}>
        <div ref={frame} className="fbrw-frame" />
        <div className={`fbrw-pill${pillBelow ? ' below' : ''}${pillLeft ? ' left' : ''}`}>
          <button type="button" className="fbrw-btn fbrw-grip" title="Mover" aria-label="Mover el navegador" onPointerDown={beginMove}><IcGrip /></button>
          {controls}
        </div>
        <span className={`fbrw-resize at-${geometry.corner}`} title="Cambiar el tamaño" aria-hidden="true" onPointerDown={beginResize} />
      </section>
    </div>
  );
}
