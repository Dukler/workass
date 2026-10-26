// Where each chat's browser page is drawn when the shell hosts it in a
// <webview> (BrowserSurfaceHost). The <webview> elements never move in the
// DOM: a moved webview loses its page. The docked pane, the floating frame and
// full screen are slots; whichever slot presents a chat's page gets it placed
// over itself, synchronously, in the same task that moved the slot, so page
// and frame land on screen in the same frame.
//
// The page always lays out at its logical viewport (the agent's 1440 × 900 or
// the pane's size) at 1:1: its element is that size in window pixels, and a
// CSS scale fits it into the slot. Moving or resizing the floating frame
// therefore never re-lays the page out.

export interface SurfaceRect { x: number; y: number; width: number; height: number; }
export interface SurfaceViewport { width: number; height: number; }

export interface SurfacePresentation {
  rect: SurfaceRect;   // CSS pixels of the app, viewport coordinates
  radius: number;      // corner radius of the slot
  z: number;           // stacking order among the app's layers
}

export interface SurfaceLayout {
  wrapper: { left: number; top: number; width: number; height: number; radius: number; z: number; interactive: boolean; shown: boolean };
  page: { width: number; height: number; scale: number; offsetX: number; offsetY: number };
}

// Stacking: above the docked pane and the transcript, below every menu,
// popover and dialog (.pop is 20); full screen sits with .fbrw-full (60), the
// layer comes first in the DOM so the bar and anything later stays above.
export const SURFACE_Z_DOCKED = 19;
export const SURFACE_Z_FLOAT = 19;
export const SURFACE_Z_FULL = 60;

const finite = (value: number, fallback: number) => (Number.isFinite(value) ? value : fallback);

// The page element's own size: the logical viewport in window pixels, so the
// guest (pinned at zoom 1 by the shell) gets exactly that many CSS pixels.
export function pageElementSize(viewport: SurfaceViewport, zoom: number): { width: number; height: number } {
  const z = zoom > 0 && Number.isFinite(zoom) ? zoom : 1;
  return { width: Math.max(1, finite(viewport.width, 1440)) / z, height: Math.max(1, finite(viewport.height, 900)) / z };
}

// Shown in a slot: fitted (letterboxed) and centered. Not shown: at the
// window's origin behind the app, still composited (an off-screen guest stops
// painting, and the agent's captures with it).
export function surfaceLayout(presentation: SurfacePresentation | null, viewport: SurfaceViewport, zoom: number): SurfaceLayout {
  const page = pageElementSize(viewport, zoom);
  if (!presentation || presentation.rect.width <= 0 || presentation.rect.height <= 0) {
    return {
      wrapper: { left: 0, top: 0, width: page.width, height: page.height, radius: 0, z: -1, interactive: false, shown: false },
      page: { ...page, scale: 1, offsetX: 0, offsetY: 0 },
    };
  }
  const { rect } = presentation;
  const scale = Math.min(rect.width / page.width, rect.height / page.height);
  const offsetX = (rect.width - page.width * scale) / 2;
  const offsetY = (rect.height - page.height * scale) / 2;
  return {
    wrapper: { left: rect.x, top: rect.y, width: rect.width, height: rect.height, radius: Math.max(0, presentation.radius), z: presentation.z, interactive: true, shown: true },
    page: { ...page, scale, offsetX: Math.abs(offsetX) < 0.01 ? 0 : offsetX, offsetY: Math.abs(offsetY) < 0.01 ? 0 : offsetY },
  };
}

// ---------------------------------------------------------------------------
// Which slot presents each chat's page. A slot that unmounts withdraws only
// its own claim, so a pane closing while the floating frame opens never
// hides the page the frame just took.
type Applier = () => void;
const claims = new Map<string, { owner: symbol; presentation: SurfacePresentation }>();
const appliers = new Map<string, Set<Applier>>();
const listeners = new Set<() => void>();

export function presentBrowserPage(chatId: string, owner: symbol, presentation: SurfacePresentation): void {
  const current = claims.get(chatId);
  if (current && current.owner === owner && samePresentation(current.presentation, presentation)) return;
  claims.set(chatId, { owner, presentation });
  applyNow(chatId);
}

export function withdrawBrowserPage(chatId: string, owner: symbol): void {
  const current = claims.get(chatId);
  if (!current || current.owner !== owner) return;
  claims.delete(chatId);
  applyNow(chatId);
}

export function browserPagePresentation(chatId: string): SurfacePresentation | null {
  return claims.get(chatId)?.presentation ?? null;
}

// Re-place a chat's page now (its viewport or the app's zoom changed).
export function refreshBrowserPage(chatId: string): void {
  applyNow(chatId);
}

// The host's per-page painter, run synchronously on every change.
export function onBrowserPagePresentation(chatId: string, apply: Applier): () => void {
  const set = appliers.get(chatId) ?? new Set<Applier>();
  set.add(apply);
  appliers.set(chatId, set);
  return () => { set.delete(apply); if (!set.size) appliers.delete(chatId); };
}

export function onAnyBrowserPagePresentation(listener: () => void): () => void {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

function applyNow(chatId: string) {
  for (const apply of Array.from(appliers.get(chatId) ?? [])) apply();
  for (const listener of Array.from(listeners)) listener();
}

function samePresentation(a: SurfacePresentation, b: SurfacePresentation): boolean {
  return a.radius === b.radius && a.z === b.z && a.rect.x === b.rect.x && a.rect.y === b.rect.y &&
    a.rect.width === b.rect.width && a.rect.height === b.rect.height;
}

// A slot element's rectangle, optionally inset (the docked pane's border).
export function slotRect(el: Element, inset = 0): SurfaceRect {
  const r = el.getBoundingClientRect();
  return { x: r.left + inset, y: r.top + inset, width: Math.max(0, r.width - inset * 2), height: Math.max(0, r.height - inset * 2) };
}
