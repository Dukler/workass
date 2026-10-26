import { useEffect, useLayoutEffect, useRef, useState } from 'react';
import type { PointerEvent as ReactPointerEvent } from 'react';
import { flushSync } from 'react-dom';
import {
  browserApi, browserViewportForBounds, localBrowserOwnsChat, sameBrowserBounds,
  type WorkassBrowserApi, type WorkassBrowserBounds,
} from '../browser';
import { store, useApp } from '../store/store';
import { chatPane } from '../store/right-pane';
import type { Chat } from '../store/types';
import { IcClose, IcFullscreen, IcFullscreenExit, IcGrip, IcPopOut, IcRail } from '../icons';
import {
  FLOAT_VIEWPORT, floatFrame, floatPlacementFor, hostZoom, moveFloatFrame, parseFloatMemory, placementOf, rememberFloat,
  resizeFloatFrame, serializeFloatMemory, snapFloatFrame, type FloatContainer, type FloatEdge, type FloatMemory, type FloatPlacement,
  type FloatRect,
} from './floating-browser-layout';

// The chat's browser floating over the transcript (approved mock
// browser-pip-v1 · B, 2026-10-13, after t3code's preview mini player): the page
// keeps its own layout and is shown scaled into a small frame. The native page
// view paints above every HTML element, so its controls sit in a pill just
// outside the frame, never on it, and the page steps aside while a menu, a
// dialog or the open Working list would land on it.
//
// Moving and resizing it (2026-10-25, measured frame by frame in the real
// app): the frame goes exactly where it is dropped, and it resizes from any
// edge or corner. While a gesture lasts, the page itself follows the pointer:
// the shell places the native view once per animation frame. The HTML around
// it (pill, shadow, outline) is drawn by another compositor that never lands
// on the same frames as the native view, so it steps out of sight for the
// gesture and comes back around the page once it has stopped; anything HTML
// that moved alongside the page trailed or led it by a frame. The overlap
// check waits for the drop, so a frame dragged across the Working list never
// blinks out.
const MEMORY_KEY = 'workass.floatingBrowser.v2';
const LEGACY_KEY = 'workass.floatingBrowser.v1';
const OVERLAYS = '.pop, [role="menu"], [role="dialog"], .thinklive.open, .lightbox, .cmdbar';
// The floating page has the frame's rounded corners (.fbrw-frame); full screen is square.
const FLOAT_RADIUS = 12;

function loadMemory(): FloatMemory {
  try { return parseFloatMemory(localStorage.getItem(MEMORY_KEY), localStorage.getItem(LEGACY_KEY)); } catch { return parseFloatMemory(null, null); }
}

function remember(change: { chatId?: string; placement?: FloatPlacement; width?: number }) {
  try { localStorage.setItem(MEMORY_KEY, serializeFloatMemory(rememberFloat(loadMemory(), change))); } catch { /* storage unavailable */ }
}

// Unrounded: the shell rounds once, after the app's zoom (rounding here too put
// the page off its frame by up to two pixels at any zoom but 100%).
function rectOf(el: Element): WorkassBrowserBounds {
  const rect = el.getBoundingClientRect();
  return { x: rect.left, y: rect.top, width: Math.max(1, rect.width), height: Math.max(1, rect.height) };
}

function overlaps(a: DOMRect, b: WorkassBrowserBounds): boolean {
  return a.width > 0 && a.height > 0 && a.left < b.x + b.width && a.right > b.x && a.top < b.y + b.height && a.bottom > b.y;
}

const nextFrames = (count: number) => new Promise<void>((resolve) => {
  const step = (left: number) => (left <= 0 ? resolve() : requestAnimationFrame(() => step(left - 1)));
  step(count);
});

export function FloatingBrowser({ chat }: { chat: Chat }) {
  const app = useApp();
  const api = browserApi();
  const mode = store.browserPresentationFor(chat.id);
  if (!api?.supported || !app.hasBrowserChannels || !localBrowserOwnsChat(chat.id, chat.machineId)) return null;
  if (chatPane(chat) === 'browser' || (mode !== 'float' && mode !== 'full')) return null;
  return <FloatingBrowserView key={chat.id} api={api} chat={chat} mode={mode} />;
}

type Gesture = { kind: 'move' | FloatEdge; armed: boolean };
const EDGES: FloatEdge[] = ['n', 's', 'e', 'w', 'nw', 'ne', 'sw', 'se'];

function FloatingBrowserView({ api, chat, mode }: { api: WorkassBrowserApi; chat: Chat; mode: 'float' | 'full' }) {
  const app = useApp();
  const host = useRef<HTMLDivElement>(null);
  const frame = useRef<HTMLDivElement>(null);
  const [width, setWidth] = useState(() => loadMemory().width);
  const [placement, setPlacement] = useState<FloatPlacement>(() => floatPlacementFor(loadMemory(), chat.id));
  const [container, setContainer] = useState<FloatContainer | null>(null);
  const containerRef = useRef(container);
  containerRef.current = container;
  // The frame while a gesture moves it; null at rest.
  const [live, setLive] = useState<FloatRect | null>(null);
  // A gesture is under way: the HTML around the page is out of sight.
  const [moving, setMoving] = useState(false);
  const gesture = useRef<Gesture | null>(null);
  const sync = useRef<() => void>(() => {});
  const checkOverlap = useRef<() => void>(() => {});
  // Out of the docked pane or full screen, the page gets back its desktop layout once.
  const relayout = useRef(true);
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
      const next = { width: el.clientWidth, height: el.clientHeight, reserve: reserve + 14 };
      setContainer((current) => (current && current.width === next.width && current.height === next.height && current.reserve === next.reserve ? current : next));
    };
    measure();
    const observer = new ResizeObserver(measure);
    observer.observe(el);
    return () => observer.disconnect();
  }, []);

  useEffect(() => api.onState((state) => {
    if (state.chatId !== chat.id) return;
    setUrl(state.url === 'about:blank' ? '' : state.url);
  }), [api, chat.id]);

  // A menu, dialog or the open Working list over the frame hides the page
  // until it goes, as the image lightbox always did. Never mid-gesture: a
  // frame dragged across the Working list must not blink out; the drop checks.
  useEffect(() => {
    let frameRequest = 0;
    const check = () => {
      frameRequest = 0;
      const el = frame.current;
      if (!el || gesture.current) return;
      const bounds = rectOf(el);
      const covered = !!app.imageLightbox || Array.from(document.querySelectorAll(OVERLAYS))
        .some((overlay) => !el.contains(overlay) && !host.current?.contains(overlay) && overlaps(overlay.getBoundingClientRect(), bounds));
      setBlocked((current) => (current === covered ? current : covered));
    };
    const schedule = () => { if (!frameRequest) frameRequest = requestAnimationFrame(check); };
    checkOverlap.current = schedule;
    const observer = new MutationObserver(schedule);
    observer.observe(document.body, { subtree: true, childList: true, attributes: true, attributeFilter: ['class', 'open', 'role'] });
    schedule();
    return () => { observer.disconnect(); if (frameRequest) cancelAnimationFrame(frameRequest); checkOverlap.current = () => {}; };
  }, [app.imageLightbox]);

  useLayoutEffect(() => { if (full) relayout.current = true; }, [full]);

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
    const radius = full ? 0 : FLOAT_RADIUS;
    const flush = async () => {
      if (!activated || syncing) return;
      syncing = true;
      try {
        while (!disposed && pending) {
          const bounds = pending;
          pending = null;
          if (sameBrowserBounds(last, bounds)) continue;
          await api.resize({ chatId: chat.id, bounds, viewport: viewportFor(bounds), radius });
          last = bounds;
        }
      } catch { /* the next sync retries */ } finally {
        syncing = false;
        if (!disposed && pending) void flush();
      }
    };
    const run = () => {
      // A gesture places the page itself (a shell that cannot keeps resize).
      if (disposed || !el.isConnected || (gesture.current && api.place)) return;
      const bounds = rectOf(el);
      if (sameBrowserBounds(last, bounds)) return;
      pending = bounds;
      void flush();
    };
    sync.current = run;
    const observer = new ResizeObserver(run);
    observer.observe(el);
    const root = document.getElementById('root');
    const layout = new MutationObserver(run);
    if (root) layout.observe(root, { attributes: true, attributeFilter: ['class', 'style'] });
    addEventListener('resize', run);
    const bounds = rectOf(el);
    const viewport = full ? browserViewportForBounds(bounds) : (relayout.current ? FLOAT_VIEWPORT : undefined);
    if (!full) relayout.current = false;
    void api.activate({ chatId: chat.id, conversationId: chat.chatId, bounds, viewport, radius }).then((state) => {
      if (disposed) return;
      setUrl(state.url === 'about:blank' ? '' : state.url);
      activated = true;
      last = bounds;
      // Shown again mid-gesture: the page goes where the gesture has it.
      if (gesture.current?.armed) api.place?.(chat.id, rectOf(el));
      run();
    }).catch(() => { /* the pane's retry covers a page still loading */ });
    return () => {
      disposed = true;
      observer.disconnect();
      layout.disconnect();
      removeEventListener('resize', run);
      if (sync.current === run) sync.current = () => {};
      void api.hide(chat.id);
    };
    // The floating frame exists only once its container is measured, so
    // `framed` (frame present) is part of what this effect depends on.
  }, [api, chat.id, chat.chatId, blocked, full, framed]);

  // The page follows the frame when it is re-placed or the chat area changes.
  useLayoutEffect(() => { sync.current(); }, [live, placement, width, container]);

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
  const placed = floatFrame({ width, placement, container });
  const rect = live ?? placed;
  // Where the frame is drawn: snapped to whole window pixels at the app's zoom.
  const snapped = (next: FloatRect) => {
    const origin = host.current?.getBoundingClientRect();
    return origin ? snapFloatFrame(next, { x: origin.left, y: origin.top }, hostZoom(window.outerWidth, window.innerWidth)) : next;
  };
  const shown = snapped(rect);

  // The page's rectangle in window coordinates for a frame at `next`.
  const windowBounds = (next: FloatRect): WorkassBrowserBounds => {
    const origin = host.current?.getBoundingClientRect();
    const at = snapped(next);
    return { x: (origin?.left ?? 0) + at.x, y: (origin?.top ?? 0) + at.y, width: at.width, height: at.height };
  };

  // One pointer gesture: the grip moves the frame, an edge or a corner resizes
  // it. It always ends, and ends once: release, cancel, lost capture, the
  // window losing focus, or Esc (which puts the frame back as it was).
  const track = (event: ReactPointerEvent<HTMLElement>, kind: Gesture['kind']) => {
    if (event.button !== 0 || gesture.current || !container) return;
    event.preventDefault();
    event.stopPropagation();
    const target = event.currentTarget;
    const pointerId = event.pointerId;
    try { target.setPointerCapture(pointerId); } catch { /* the pointer is already gone */ }
    const start = rect;
    const startX = event.clientX, startY = event.clientY;
    const active: Gesture = { kind, armed: false };
    gesture.current = active;
    const native = typeof api.place === 'function';
    let ended = false;
    let latest = start;
    const frameAt = (clientX: number, clientY: number) => {
      const box = containerRef.current ?? container;
      const delta = { x: clientX - startX, y: clientY - startY };
      return kind === 'move' ? moveFloatFrame(start, delta, box) : resizeFloatFrame(start, kind, delta, box);
    };
    const follow = (next: FloatRect) => {
      if (native) api.place!(chat.id, windowBounds(next));
      setLive(next);
    };
    // Once the pointer moves, the HTML around the page goes out of sight first
    // (a plain click on the grip changes nothing); the page starts to follow
    // only once that frame is on screen, so nothing is left behind. What the
    // pointer did meanwhile (two frames) is caught up over a few frames,
    // halving the gap each time, instead of in one jump.
    let catchingUp = false;
    let arming: Promise<void> | null = native ? null : Promise.resolve();
    if (!native) active.armed = true;
    const begin = () => {
      if (arming) return arming;
      flushSync(() => setMoving(true));
      arming = nextFrames(2).then(() => {
        if (ended) return;
        active.armed = true;
        if (kind !== 'move') { follow(latest); return; }
        let shown = start;
        catchingUp = true;
        const ease = () => {
          if (ended) return;
          const dx = latest.x - shown.x, dy = latest.y - shown.y;
          if (Math.abs(dx) <= 2 && Math.abs(dy) <= 2) {
            catchingUp = false;
            follow(latest);
            return;
          }
          shown = { ...latest, x: Math.round(shown.x + dx / 2), y: Math.round(shown.y + dy / 2) };
          follow(shown);
          requestAnimationFrame(ease);
        };
        ease();
      });
      return arming;
    };
    const onMove = (next: PointerEvent) => {
      if (next.pointerId !== pointerId || ended) return;
      latest = frameAt(next.clientX, next.clientY);
      if (!arming && (latest.x !== start.x || latest.y !== start.y || latest.width !== start.width)) void begin();
      if (active.armed && !catchingUp) follow(latest);
    };
    const finish = (commit: boolean) => {
      if (ended) return;
      ended = true;
      target.removeEventListener('pointermove', onMove);
      target.removeEventListener('pointerup', onUp);
      target.removeEventListener('pointercancel', onCancel);
      target.removeEventListener('lostpointercapture', onCancel);
      removeEventListener('blur', onCancel);
      removeEventListener('keydown', onKey, true);
      try { if (target.hasPointerCapture(pointerId)) target.releasePointerCapture(pointerId); } catch { /* already released */ }
      const final = commit ? latest : start;
      if (!arming && final === start) {
        // A press that never moved: nothing went out of sight, nothing to put back.
        if (gesture.current === active) gesture.current = null;
        return;
      }
      void (async () => {
        try {
          // Even a release with no move before it waits for the HTML to be out of sight.
          await begin();
          const box = containerRef.current ?? container;
          if (native) api.place!(chat.id, windowBounds(final));
          const nextPlacement = placementOf(final, final, box, placement);
          setPlacement(nextPlacement);
          if (kind !== 'move') setWidth(final.width);
          remember({ chatId: chat.id, placement: nextPlacement, ...(kind !== 'move' ? { width: final.width } : {}) });
          setLive(final);
          // The HTML comes back around the page once the page has settled there.
          if (native && kind !== 'move') await api.resize({ chatId: chat.id, bounds: windowBounds(final), radius: FLOAT_RADIUS }).catch(() => false);
          if (native) await nextFrames(2);
        } finally {
          if (gesture.current === active) gesture.current = null;
          setLive(null);
          setMoving(false);
          checkOverlap.current();
        }
      })();
    };
    const onUp = (next: PointerEvent) => {
      if (next.pointerId !== pointerId) return;
      latest = frameAt(next.clientX, next.clientY);
      finish(true);
    };
    const onCancel = () => finish(true);
    const onKey = (next: KeyboardEvent) => { if (next.key === 'Escape') finish(false); };
    target.addEventListener('pointermove', onMove);
    target.addEventListener('pointerup', onUp);
    target.addEventListener('pointercancel', onCancel);
    target.addEventListener('lostpointercapture', onCancel);
    addEventListener('blur', onCancel);
    addEventListener('keydown', onKey, true);
  };

  // Resizing from any edge or corner: thin zones just outside the frame (the
  // page covers its inside), shown only by the cursor.
  return (
    <div ref={host} className="fbrw-host">
      <section className={`fbrw${moving ? ' is-moving' : ''}`} aria-label="Navegador flotante" style={{ left: shown.x, top: shown.y, width: shown.width, height: shown.height }}>
        <div ref={frame} className="fbrw-frame" />
        {EDGES.map((edge) => (
          <span key={edge} className={`fbrw-edge e-${edge}`} title={edge.length === 2 ? 'Cambiar el tamaño' : undefined} aria-hidden="true" onPointerDown={(event) => track(event, edge)} />
        ))}
        <div className="fbrw-pill">
          <button type="button" className="fbrw-btn fbrw-grip" title="Mover" aria-label="Mover el navegador" onPointerDown={(event) => track(event, 'move')}><IcGrip /></button>
          {controls}
        </div>
      </section>
    </div>
  );
}
