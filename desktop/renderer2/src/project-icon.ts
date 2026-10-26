import { normalizeWorkspacePath } from './workspaces.ts';
import { call, has } from './wire/api.ts';
import { machineOf } from './wire/machineIds.ts';
import type { ProjectIconResult } from './wire/types.ts';
import { isUnchangedReply, parseWireCache, readWireCache, writeWireCache } from './store/wire-cache.ts';

const MAX_PROJECT_ICON_BASE64_CHARS = 349_528;
const PROJECT_ICON_MIME_TYPES = new Set([
  'image/png',
  'image/jpeg',
  'image/gif',
  'image/webp',
  'image/x-icon',
  'image/svg+xml',
]);

const values = new Map<string, string | null>();
const pending = new Map<string, Promise<string | null>>();

// Icons this window already read from its own daemon, kept across a reload
// with the etag the daemon named them by. A reload asks {ifNoneMatch} and a
// receipt reuses the kept icon; a changed icon arrives whole, as before.
const KEPT_ICONS = 'project-icons';
const KEPT_ICON_LIMIT = 64;
const KEPT_ICON_CHARS = 1_000_000;
// byURL: the icon loads by URL from its etag (source is that URL; it is
// rebuilt with the device's current image key when the bridge has one).
type KeptIcon = { etag: string; source: string | null; byURL?: boolean };
let kept: Record<string, KeptIcon> | null = null;

// GET /workass/project-icon/<etag>?cwd=<path>&k=<image key> (daemon
// cmd/workass/project_icon_http.go). '' for the page's own origin, as chat
// images (chat-image-url.ts).
const PROJECT_ICON_PATH = '/workass/project-icon/';

export function projectIconURL(etag: string, cwd: string, imageKey: string): string {
  return `${PROJECT_ICON_PATH}${encodeURIComponent(etag)}?cwd=${encodeURIComponent(cwd)}&k=${encodeURIComponent(imageKey)}`;
}

// The read-only image capability the injected bridge keeps from
// lan:access-state; '' with an older daemon.
function localImageKey(): string {
  const state = (globalThis as { window?: { __workassLanAccess?: { state?: unknown; imageKey?: unknown } } }).window?.__workassLanAccess;
  return state && state.state === 'approved' && typeof state.imageKey === 'string' ? state.imageKey.trim() : '';
}

function keptIcons(): Record<string, KeptIcon> {
  if (!kept) {
    const stored = parseWireCache<Record<string, KeptIcon>>(readWireCache(KEPT_ICONS));
    kept = stored && typeof stored === 'object' ? stored : {};
  }
  return kept;
}

function keepIcon(key: string, etag: string, source: string | null, byURL = false) {
  const icons = keptIcons();
  delete icons[key];
  icons[key] = byURL ? { etag, source, byURL: true } : { etag, source };
  const keys = Object.keys(icons);
  for (const old of keys.slice(0, Math.max(0, keys.length - KEPT_ICON_LIMIT))) delete icons[old];
  const text = JSON.stringify(icons);
  writeWireCache(KEPT_ICONS, 'v1', text.length <= KEPT_ICON_CHARS ? text : '');
}

export function projectIconCacheKey(chatId: string, cwd: string): string {
  return `${machineOf(chatId)}\u0000${normalizeWorkspacePath(cwd)}`;
}

export function projectIconDataURL(result: ProjectIconResult | null | undefined): string | null {
  if (!result?.found) return null;
  const mimeType = String(result.mimeType ?? '').trim().toLowerCase();
  const encoded = String(result.base64 ?? '').trim();
  if (!PROJECT_ICON_MIME_TYPES.has(mimeType)
    || !encoded
    || encoded.length > MAX_PROJECT_ICON_BASE64_CHARS
    || encoded.length % 4 !== 0
    || !/^[A-Za-z0-9+/]+={0,2}$/.test(encoded)) return null;
  return `data:${mimeType};base64,${encoded}`;
}

export function cachedProjectIcon(chatId: string, cwd: string): string | null | undefined {
  const key = projectIconCacheKey(chatId, cwd);
  if (values.has(key)) return values.get(key);
  // What this window showed before a reload paints at once; the read that
  // follows revalidates it by etag.
  if (machineOf(chatId)) return undefined;
  const prior = keptIcons()[key];
  return prior ? prior.source : undefined;
}

export async function loadProjectIcon(chatId: string, cwd: string): Promise<string | null> {
  const clean = normalizeWorkspacePath(cwd);
  if (!clean || !has('projectIcon')) return null;
  const key = projectIconCacheKey(chatId, clean);
  if (values.has(key)) return values.get(key) ?? null;
  const existing = pending.get(key);
  if (existing) return existing;

  const request = (async () => {
    // Only this daemon's icons are read by etag; the router does not forward
    // the option to another machine, and an older bridge drops it.
    const local = !machineOf(chatId);
    const prior = local ? keptIcons()[key] : undefined;
    // With the device's image key a raster icon is only named on the socket
    // and loads by URL (immutable under its hash); an older daemon or bridge
    // ignores byURL and sends the bytes.
    const imageKey = local ? localImageKey() : '';
    let reply = local
      ? await call('projectIcon', chatId, clean, { ifNoneMatch: prior?.etag ?? '', ...(imageKey ? { byURL: true } : {}) })
      : await call('projectIcon', chatId, clean);
    let source: string | null;
    if (prior && isUnchangedReply(reply, prior.etag)) {
      // An icon by URL keeps the URL it loaded from (the device's image key
      // in it), and takes the current key when the bridge has one.
      source = prior.byURL && imageKey ? projectIconURL(prior.etag, clean, imageKey) : prior.source;
      if (prior.byURL && !source) {
        reply = await call('projectIcon', chatId, clean);
        source = projectIconDataURL(reply as ProjectIconResult | undefined);
      }
    } else {
      if (reply && typeof reply === 'object' && (reply as { unchanged?: unknown }).unchanged === true) {
        reply = await call('projectIcon', chatId, clean);
      }
      const etag = (reply as { etag?: unknown } | undefined)?.etag;
      const named = (reply as { byURL?: unknown } | undefined)?.byURL === true && typeof etag === 'string' && !!etag && !!imageKey;
      source = named ? projectIconURL(etag as string, clean, imageKey) : projectIconDataURL(reply as ProjectIconResult | undefined);
      if (local && typeof etag === 'string' && etag) keepIcon(key, etag, source, named);
    }
    // The icon list is bounded by the visible project list. Keep a defensive
    // cap so repeatedly mounting arbitrary remote workspaces cannot grow this
    // renderer-local cache without limit.
    if (values.size >= 512) values.clear();
    values.set(key, source);
    return source;
  })().finally(() => pending.delete(key));
  pending.set(key, request);
  return request;
}

/**
 * An icon URL that did not load (the folder's icon changed after it was named,
 * or the device lost its key): read the icon's bytes over the socket instead.
 */
export async function loadProjectIconBytes(chatId: string, cwd: string): Promise<string | null> {
  const clean = normalizeWorkspacePath(cwd);
  if (!clean || !has('projectIcon')) return null;
  const key = projectIconCacheKey(chatId, clean);
  const icons = keptIcons();
  if (icons[key]?.byURL) delete icons[key];
  const reply = await call('projectIcon', chatId, clean);
  const source = projectIconDataURL(reply as ProjectIconResult | undefined);
  values.set(key, source);
  return source;
}

export function isProjectIconURL(source: string | null | undefined): boolean {
  return typeof source === 'string' && source.startsWith(PROJECT_ICON_PATH);
}

export function clearProjectIconCache(): void {
  values.clear();
  pending.clear();
}
