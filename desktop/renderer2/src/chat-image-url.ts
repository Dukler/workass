import type { MessageImage } from './store/types';
import { localId, machineOf } from './wire/machineIds.ts';

// A chat image by URL (daemon GET /workass/chat-image/<tab>/<digest>?k=<key>):
// the browser fetches, decodes and caches it off the main thread. Over the
// socket each image was a base64 string in a JSON reply that the main thread
// parsed and turned into a data URL, and history pages waited behind it.
//
// The key is the read-only image capability an approved device receives on
// lan:access-state (`imageKey`). A daemon that predates the endpoint sends no
// key, so the URL is '' and the image is read over the socket exactly as
// before. Raster types only: an SVG stays a data URL from the socket read.

export interface ChatImageAccess {
  /** '' for the page's own origin (the local daemon, or the shell proxying it). */
  origin: string;
  key: string;
}

const RASTER = new Set(['image/png', 'image/jpeg', 'image/jpg', 'image/gif', 'image/webp']);
const REF = /^images\/([0-9a-f]{64})$/;
export const CHAT_IMAGE_PATH = '/workass/chat-image/';

let remoteAccess: ((machineId: string) => ChatImageAccess | null) | null = null;

/** The store's machine registry answers for remote machines' chats. */
export function setRemoteChatImageAccess(resolve: ((machineId: string) => ChatImageAccess | null) | null): void {
  remoteAccess = resolve;
}

// The injected bridge keeps the latest lan:access-state on window. Read per
// call: a reconnect (or a re-paired device) replaces the key.
function localAccess(): ChatImageAccess | null {
  const state = (globalThis as { window?: { __workassLanAccess?: { state?: unknown; imageKey?: unknown } } }).window?.__workassLanAccess;
  const key = state && state.state === 'approved' && typeof state.imageKey === 'string' ? state.imageKey.trim() : '';
  return key ? { origin: '', key } : null;
}

/** The image's URL, or '' when it must be read over the socket. */
export function chatImageURL(tabId: string, image: MessageImage): string {
  const match = REF.exec(image.deferredImageRef ?? '');
  if (!match || !RASTER.has(String(image.mimeType || '').toLowerCase())) return '';
  const machineId = machineOf(tabId);
  const access = machineId ? remoteAccess?.(machineId) ?? null : localAccess();
  if (!access) return '';
  const tab = localId(tabId);
  if (!tab) return '';
  return `${access.origin}${CHAT_IMAGE_PATH}${encodeURIComponent(tab)}/${match[1]}?k=${encodeURIComponent(access.key)}`;
}
