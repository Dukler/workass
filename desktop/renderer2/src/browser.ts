import { machineOf } from './wire/machineIds.ts';

export interface WorkassBrowserBounds { x: number; y: number; width: number; height: number; }

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
}

export interface WorkassBrowserApi {
  supported: boolean;
  activate(payload: { chatId: string; conversationId?: string; bounds: WorkassBrowserBounds; url?: string }): Promise<WorkassBrowserState>;
  resize(payload: { chatId: string; bounds: WorkassBrowserBounds }): Promise<boolean>;
  hide(chatId: string): Promise<boolean>;
  close(chatId: string): Promise<boolean>;
  command(chatId: string, command: 'navigate' | 'back' | 'forward' | 'reload' | 'stop', value?: string): Promise<WorkassBrowserState>;
  onOpenRequest(callback: (chatId?: string) => void): () => void;
  onState(callback: (state: WorkassBrowserState) => void): () => void;
}

declare global {
  interface Window { workassBrowser?: WorkassBrowserApi; }
}

export function browserApi(): WorkassBrowserApi | undefined {
  return typeof window !== 'undefined' ? window.workassBrowser : undefined;
}
