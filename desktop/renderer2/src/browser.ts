import { machineOf } from './wire/machineIds.ts';

export interface WorkassBrowserBounds { x: number; y: number; width: number; height: number; }

export const BROWSER_VIEWPORT_PRESETS = Object.freeze({
  desktop: Object.freeze({ width: 1440, height: 900 }),
  laptop: Object.freeze({ width: 1280, height: 800 }),
  narrow: Object.freeze({ width: 390, height: 844 }),
});

export function browserViewportPreset(width?: number, height?: number): string {
  if (width == null || height == null) return 'desktop';
  for (const [name, dimensions] of Object.entries(BROWSER_VIEWPORT_PRESETS)) {
    if (width === dimensions.width && height === dimensions.height) return name;
  }
  return 'custom';
}

export function browserViewportInputError(width: string, height: string): string | null {
  const parsedWidth = width.trim();
  if (!/^\d+$/.test(parsedWidth)) return 'El ancho debe ser un número entero entre 320 y 3840.';
  const widthValue = Number(parsedWidth);
  if (!Number.isSafeInteger(widthValue) || widthValue < 320 || widthValue > 3840) return 'El ancho debe ser un número entero entre 320 y 3840.';
  const parsedHeight = height.trim();
  if (!/^\d+$/.test(parsedHeight)) return 'El alto debe ser un número entero entre 240 y 2160.';
  const heightValue = Number(parsedHeight);
  if (!Number.isSafeInteger(heightValue) || heightValue < 240 || heightValue > 2160) return 'El alto debe ser un número entero entre 240 y 2160.';
  return null;
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
