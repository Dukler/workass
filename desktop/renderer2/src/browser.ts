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
}

export interface WorkassBrowserApi {
  supported: boolean;
  activate(payload: { chatId: string; conversationId?: string; bounds: WorkassBrowserBounds; url?: string }): Promise<WorkassBrowserState>;
  resize(payload: { chatId: string; bounds: WorkassBrowserBounds }): Promise<boolean>;
  hide(chatId: string): Promise<boolean>;
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
