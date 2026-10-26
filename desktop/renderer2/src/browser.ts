import { machineOf } from './wire/machineIds.ts';

export interface WorkassBrowserBounds { x: number; y: number; width: number; height: number; }

export function browserViewportForBounds(bounds: WorkassBrowserBounds): { width: number; height: number } {
  const minScale = Math.max(320 / bounds.width, 240 / bounds.height);
  const maxScale = Math.min(3840 / bounds.width, 2160 / bounds.height);
  const scale = Math.max(minScale, Math.min(1, maxScale));
  return {
    width: Math.min(3840, Math.max(320, Math.round(bounds.width * scale))),
    height: Math.min(2160, Math.max(240, Math.round(bounds.height * scale))),
  };
}

export function sameBrowserBounds(a: WorkassBrowserBounds | null, b: WorkassBrowserBounds): boolean {
  return !!a && a.x === b.x && a.y === b.y && a.width === b.width && a.height === b.height;
}

// The Electron browser belongs to this controller shell. It may display a
// remote chat's hosted artifact, but its page and controls remain local to this
// shell; the machine tag keeps entries for equal chat ids isolated.
export function localBrowserOwnsChat(chatId: string, machineId?: string): boolean {
  if (!String(chatId ?? '').trim()) return false;
  const machine = String(machineId ?? '').trim();
  return machine === machineOf(chatId);
}

export function hostedArtifactURL(target: string, origin?: string): string {
  const value = String(target ?? '').trim();
  const base = String(origin ?? '').trim().replace(/\/+$/, '');
  if (!value.startsWith('/workass/artifacts/')) return value;
  if (origin !== undefined && !base) return '';
  if (!base) return value;
  return `${base}${value}`;
}

export interface WorkassBrowserState {
  chatId: string;
  url: string;
  title: string;
  loading: boolean;
  error: string | null;
  canGoBack: boolean;
  canGoForward: boolean;
  cdpAttached: boolean;
  persistent: boolean;
  viewport?: { width: number; height: number; deviceScaleFactor: number };
  effectiveViewport?: { width: number; height: number; deviceScaleFactor: number; scrollX?: number; scrollY?: number } | null;
  viewportGeneration?: number;
  documentGeneration?: number;
  visible?: boolean;
  // The page sits in its own always-on-top window (popOut).
  poppedOut?: boolean;
  // Newer shells: 'webview' when the page is the guest of a <webview> this
  // renderer mounts (BrowserSurfaceHost), with the key that names it.
  host?: 'webview' | 'view';
  hostKey?: string;
  tabId?: number | null;
}

// One page the webview host asks this renderer to mount.
export interface WorkassBrowserHost {
  hostKey: string;
  chatId: string;
  tabId: number | null;
  viewport: { width: number; height: number; deviceScaleFactor?: number };
  visible: boolean;
  poppedOut: boolean;
}

export interface WorkassBrowserApi {
  supported: boolean;
  // 'webview': this renderer mounts the pages (newer shells); absent or
  // 'view': the shell stacks native views over the window.
  hostMode?(): 'webview' | 'view';
  registerGuest?(hostKey: string, webContentsId: number): Promise<{ ok: boolean; reason?: string; tabId?: number }>;
  hosts?(): Promise<WorkassBrowserHost[]>;
  onHosts?(callback: (hosts: WorkassBrowserHost[]) => void): () => void;
  // radius: the page's corner radius where it is shown (the floating frame's);
  // a newer shell also clips the page to it.
  activate(payload: { chatId: string; conversationId?: string; bounds: WorkassBrowserBounds; viewport?: { width: number; height: number }; url?: string; radius?: number }): Promise<WorkassBrowserState>;
  resize(payload: { chatId: string; bounds: WorkassBrowserBounds; viewport?: { width: number; height: number }; radius?: number }): Promise<boolean>;
  hide(chatId: string): Promise<boolean>;
  // Optional: an older shell has no separate window.
  popOut?(chatId: string): Promise<WorkassBrowserState>;
  popIn?(chatId: string): Promise<boolean>;
  // Optional, newer shells: a drag or resize places the page synchronously,
  // once per animation frame, without the awaited round trip of resize.
  place?(chatId: string, bounds: WorkassBrowserBounds): void;
  close(chatId: string): Promise<boolean>;
  command(chatId: string, command: 'navigate' | 'back' | 'forward' | 'reload' | 'stop', value?: string): Promise<WorkassBrowserState>;
  setViewport(chatId: string, width: number, height: number): Promise<WorkassBrowserState>;
  resetViewport(chatId: string): Promise<WorkassBrowserState>;
  onOpenRequest(callback: (chatId?: string) => void): () => void;
  onState(callback: (state: WorkassBrowserState) => void): () => void;
}

declare global {
  interface Window { workassBrowser?: WorkassBrowserApi; }
}

export function browserApi(): WorkassBrowserApi | undefined {
  return typeof window !== 'undefined' ? window.workassBrowser : undefined;
}

// The page is drawn by this renderer, in a <webview>.
export function browserHostedInRenderer(api: WorkassBrowserApi | undefined = browserApi()): boolean {
  return typeof api?.hostMode === 'function' && typeof api.registerGuest === 'function' && api.hostMode() === 'webview';
}

// One page: a page the shell made before this renderer asked for the webview
// host stays a native view (over all HTML) until it is replaced, so it is
// treated as one. A state that does not say follows the renderer's host.
export function browserPageHosted(api: WorkassBrowserApi | undefined, host?: 'webview' | 'view'): boolean {
  return browserHostedInRenderer(api) && host !== 'view';
}
