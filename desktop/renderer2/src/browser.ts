import { machineOf } from './wire/machineIds.ts';

export interface WorkassBrowserBounds { x: number; y: number; width: number; height: number; }

export function sameBrowserBounds(a: WorkassBrowserBounds | null, b: WorkassBrowserBounds): boolean {
  return !!a && a.x === b.x && a.y === b.y && a.width === b.width && a.height === b.height;
}

// The Electron browser belongs to this shell process. A remote chat's ids are
// routed to its owning daemon, so mounting the local native view there would
// display and control an unrelated page. Check both ownership fields and the
// boundary tag so incomplete remote hydration also fails closed.
export function localBrowserOwnsChat(chatId: string, machineId?: string): boolean {
  return String(machineId ?? '').trim() === '' && machineOf(chatId) === '';
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
