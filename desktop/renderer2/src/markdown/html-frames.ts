// One process-wide controller for the chat's HTML page frames (PLAN.md
// "html-frames.ts"): a single window `message` listener that only answers
// frames it registered, a single theme observer that repaints every page live,
// and lazy mounting that keeps at most HTML_RENDER_MAX_MOUNTED frames alive.
import { HTML_RENDER_MAX_MOUNTED, htmlThemeVars, readFrameMessage, type HtmlScheme } from './html-render';

declare global {
  interface Window {
    // Exposed by the Electron shell's preload once it cancels every navigation
    // of a render frame (desktop/shell/render-frame-guard.js). Absent on older
    // shells and in plain browsers: pages then render static (no scripts).
    workassRenderFrames?: { supported?: boolean; navigationGuard?: boolean };
  }
}

export interface FrameHandlers {
  iframe: HTMLIFrameElement;
  /** Fill frames (the zoom dialog) ignore size reports. */
  fill?: boolean;
  /** Static frames run no script; the host never posts to them. */
  scripted: boolean;
  loaded?: boolean;
  onSize?(height: number): void;
  onLink?(url: string): void;
  onEscape?(): void;
}

const registered = new Set<FrameHandlers>();
let installed = false;
let themeVersion = 0;
const themeListeners = new Set<() => void>();

export function renderFramesScripted(): boolean {
  return typeof window !== 'undefined' && window.workassRenderFrames?.navigationGuard === true;
}

export function currentScheme(): HtmlScheme {
  if (typeof document === 'undefined') return 'dark';
  return document.documentElement.getAttribute('data-theme') === 'light' ? 'light' : 'dark';
}

export function currentThemeVars(): Record<string, string> {
  if (typeof document === 'undefined' || typeof getComputedStyle === 'undefined') return htmlThemeVars(() => '');
  const style = getComputedStyle(document.documentElement);
  return htmlThemeVars((token) => style.getPropertyValue(token));
}

function frameFor(source: MessageEventSource | null): FrameHandlers | undefined {
  if (!source) return undefined;
  for (const handlers of registered) {
    if (handlers.iframe.contentWindow === source) return handlers;
  }
  return undefined;
}

function onMessage(event: MessageEvent): void {
  // Messages from any window we did not create (and keep registered) are dropped.
  const handlers = frameFor(event.source);
  if (!handlers || !handlers.scripted) return;
  const message = readFrameMessage(event.data);
  if (!message) return;
  if (message.type === 'size') {
    if (!handlers.fill) handlers.onSize?.(message.height);
  } else if (message.type === 'link') {
    // A page can take focus by script, so this stops opens on load; the
    // confirmation (which shows the whole URL) stops the rest (t3code
    // BrowserDocumentFrame.tsx:210-232 has the same gate, minus the confirm).
    const active = typeof navigator === 'undefined' ? undefined : navigator.userActivation?.isActive;
    if (document.activeElement === handlers.iframe && active !== false) handlers.onLink?.(message.url);
  } else if (message.type === 'escape') {
    // Esc pressed inside a page closes the topmost page overlay (the link
    // confirmation, then the zoom dialog), exactly as it does outside it.
    if (!closeTopEscapeLayer()) handlers.onEscape?.();
  }
}

/** Repaints every page in the current theme, without reloading any of them. */
export function broadcastTheme(): void {
  themeVersion++;
  const scheme = currentScheme();
  const vars = currentThemeVars();
  for (const handlers of registered) {
    if (!handlers.scripted) continue;
    try { handlers.iframe.contentWindow?.postMessage({ type: 'wa-html:theme', scheme, vars }, '*'); } catch { /* frame gone */ }
    if (handlers.loaded) handlers.iframe.style.colorScheme = scheme;
  }
  for (const listener of themeListeners) listener();
}

function install(): void {
  if (installed || typeof window === 'undefined' || typeof document === 'undefined') return;
  installed = true;
  window.addEventListener('message', onMessage);
  if (typeof MutationObserver !== 'undefined') {
    new MutationObserver(broadcastTheme).observe(document.documentElement, { attributes: true, attributeFilter: ['data-theme'] });
  }
}

export function registerHtmlFrame(handlers: FrameHandlers): () => void {
  install();
  registered.add(handlers);
  return () => { registered.delete(handlers); };
}

/** Called on the frame's load: its document now carries the theme's scheme. */
export function markFrameLoaded(handlers: FrameHandlers): void {
  handlers.loaded = true;
  // A frame whose color-scheme differs from its document's paints an opaque
  // canvas, so it stays `light` (the blank document's scheme) until the page is
  // in (t3code BrowserDocumentFrame.tsx:308-312).
  handlers.iframe.style.colorScheme = currentScheme();
  if (!handlers.scripted) return;
  // Covers a theme change that landed while the page was loading.
  try { handlers.iframe.contentWindow?.postMessage({ type: 'wa-html:theme', scheme: currentScheme(), vars: currentThemeVars() }, '*'); } catch { /* frame gone */ }
}

export function subscribeTheme(listener: () => void): () => void {
  install();
  themeListeners.add(listener);
  return () => { themeListeners.delete(listener); };
}

export function themeSnapshot(): number {
  return themeVersion;
}

// ── Lazy mounting ─────────────────────────────────────────────────────────────
// A frame mounts within three viewports of the visible transcript and unmounts
// beyond that, keeping its last height as a placeholder. At most
// HTML_RENDER_MAX_MOUNTED stay mounted; the least recently visible goes first.

interface MountSlot {
  setMounted(mounted: boolean): void;
  mounted: boolean;
  intersecting: boolean;
  seenAt: number;
}

const slots = new Map<Element, MountSlot>();
const observers = new Map<Element | null, IntersectionObserver>();
let seenClock = 0;

function evictFor(slot: MountSlot): void {
  const mounted = [...slots.values()].filter((other) => other !== slot && other.mounted);
  while (mounted.length >= HTML_RENDER_MAX_MOUNTED) {
    mounted.sort((a, b) => Number(a.intersecting) - Number(b.intersecting) || a.seenAt - b.seenAt);
    const victim = mounted.shift()!;
    victim.mounted = false;
    victim.setMounted(false);
  }
}

function onIntersect(entries: IntersectionObserverEntry[]): void {
  for (const entry of entries) {
    const slot = slots.get(entry.target);
    if (!slot) continue;
    slot.intersecting = entry.isIntersecting;
    if (entry.isIntersecting) {
      slot.seenAt = ++seenClock;
      if (!slot.mounted) {
        evictFor(slot);
        slot.mounted = true;
        slot.setMounted(true);
      }
    } else if (slot.mounted) {
      slot.mounted = false;
      slot.setMounted(false);
    }
  }
}

export function lazyMountSupported(): boolean {
  return typeof IntersectionObserver !== 'undefined';
}

export function observeFrameMount(element: Element, setMounted: (mounted: boolean) => void): () => void {
  if (!lazyMountSupported()) {
    setMounted(true);
    return () => {};
  }
  const root = element.closest('.scroll');
  let observer = observers.get(root);
  if (!observer) {
    observer = new IntersectionObserver(onIntersect, { root, rootMargin: '300% 0px 300% 0px' });
    observers.set(root, observer);
  }
  slots.set(element, { setMounted, mounted: false, intersecting: false, seenAt: 0 });
  observer.observe(element);
  return () => {
    observer!.unobserve(element);
    slots.delete(element);
  };
}

export function mountedFrameCount(): number {
  return [...slots.values()].filter((slot) => slot.mounted).length;
}

// ── Per-block memory ──────────────────────────────────────────────────────────
// The transcript windows its rows, so a block remounts as it scrolls back in.
// Its page/code choice and its last height live here, in memory only.

const LIMIT = 256;
function remember<T>(map: Map<string, T>, key: string, value: T): void {
  map.delete(key);
  map.set(key, value);
  while (map.size > LIMIT) map.delete(map.keys().next().value as string);
}

const views = new Map<string, 'page' | 'code'>();
const heights = new Map<string, number>();

export function rememberedView(key: string): 'page' | 'code' {
  return views.get(key) ?? 'page';
}
export function rememberView(key: string, view: 'page' | 'code'): void {
  remember(views, key, view);
}
export function rememberedHeight(key: string): number | undefined {
  return heights.get(key);
}
export function rememberHeight(key: string, height: number): void {
  remember(heights, key, height);
}

// ── Link confirmation ─────────────────────────────────────────────────────────
// One confirmation at a time across the whole transcript.

let closeOpenAsk: (() => void) | null = null;
export function claimLinkAsk(close: () => void): () => void {
  if (closeOpenAsk && closeOpenAsk !== close) closeOpenAsk();
  closeOpenAsk = close;
  return () => { if (closeOpenAsk === close) closeOpenAsk = null; };
}

// ── Esc ───────────────────────────────────────────────────────────────────────
// The zoom dialog and the link confirmation close on Esc, topmost first. The
// key is consumed in the capture phase so it never reaches the app's own Esc
// (which cancels a running turn).

const escapeLayers: Array<() => void> = [];
let escapeInstalled = false;

function closeTopEscapeLayer(): boolean {
  const close = escapeLayers[escapeLayers.length - 1];
  if (!close) return false;
  close();
  return true;
}

function onEscapeKey(event: KeyboardEvent): void {
  if (event.key !== 'Escape' || !escapeLayers.length) return;
  event.preventDefault();
  event.stopImmediatePropagation();
  closeTopEscapeLayer();
}

export function pushEscapeLayer(close: () => void): () => void {
  if (!escapeInstalled && typeof window !== 'undefined') {
    escapeInstalled = true;
    window.addEventListener('keydown', onEscapeKey, true);
  }
  escapeLayers.push(close);
  return () => {
    const at = escapeLayers.lastIndexOf(close);
    if (at >= 0) escapeLayers.splice(at, 1);
  };
}
