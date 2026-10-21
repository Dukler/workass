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

/**
 * The image's URL, or '' when it must be read over the socket. A width asks
 * for a display rendition at most that wide (the daemon rounds it to one of
 * CHAT_IMAGE_WIDTHS and serves the image itself when it is narrower).
 */
export function chatImageURL(tabId: string, image: MessageImage, width = 0): string {
  const match = REF.exec(image.deferredImageRef ?? '');
  if (!match || !RASTER.has(String(image.mimeType || '').toLowerCase())) return '';
  const machineId = machineOf(tabId);
  const access = machineId ? remoteAccess?.(machineId) ?? null : localAccess();
  if (!access) return '';
  const tab = localId(tabId);
  if (!tab) return '';
  const url = `${access.origin}${CHAT_IMAGE_PATH}${encodeURIComponent(tab)}/${match[1]}?k=${encodeURIComponent(access.key)}`;
  return width > 0 ? `${url}&w=${Math.round(width)}` : url;
}

// Display renditions. A gallery draws ~2,000 px screenshots in 156 px cells;
// decoding the full image for that cost the browser 7x the decode time and
// 14x the memory of a rendition sized to the cell (300 images of the 225 MB
// chat: 1.18 s vs 0.16 s, 1.3 GB vs 92 MB decoded). The widths match the
// daemon's (chat_image_thumbs.go).
export const CHAT_IMAGE_WIDTHS = [160, 240, 320, 480, 640, 960, 1280, 1920] as const;
const RENDITION = /&w=\d+$/;

/**
 * The rendition width for an image drawn `displayWidth` CSS px wide at this
 * pixel ratio, never narrower than what the screen shows; 0 means the full
 * image (no size known, or a rendition would not be clearly smaller).
 */
export function chatImageRenditionWidth(displayWidth: number, pixelRatio: number, naturalWidth: number | undefined): number {
  if (!(displayWidth > 0) || !(typeof naturalWidth === 'number' && naturalWidth > 0)) return 0;
  const needed = Math.ceil(displayWidth * Math.max(1, Number.isFinite(pixelRatio) ? pixelRatio : 1));
  const width = CHAT_IMAGE_WIDTHS.find((candidate) => candidate >= needed) ?? 0;
  return width && width * 1.5 <= naturalWidth ? width : 0;
}

/** The full image's URL for a rendition URL; any other source is returned unchanged. */
export function fullChatImageURL(src: string): string {
  return src.includes(CHAT_IMAGE_PATH) ? src.replace(RENDITION, '') : src;
}

/** The rendition width a chat-image URL asks for; 0 for the full image. */
export function chatImageURLWidth(src: string): number {
  if (!src.includes(CHAT_IMAGE_PATH)) return 0;
  const match = /&w=(\d+)$/.exec(src);
  return match ? Number(match[1]) : 0;
}

/** Two sources that are renditions of one chat image (or the same source). */
export function sameChatImage(a: string, b: string): boolean {
  if (!a || !b) return false;
  return a === b || (a.includes(CHAT_IMAGE_PATH) && fullChatImageURL(a) === fullChatImageURL(b));
}
