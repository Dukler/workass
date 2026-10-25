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
type KeptIcon = { etag: string; source: string | null };
let kept: Record<string, KeptIcon> | null = null;

function keptIcons(): Record<string, KeptIcon> {
  if (!kept) {
    const stored = parseWireCache<Record<string, KeptIcon>>(readWireCache(KEPT_ICONS));
    kept = stored && typeof stored === 'object' ? stored : {};
  }
  return kept;
}

function keepIcon(key: string, etag: string, source: string | null) {
  const icons = keptIcons();
  delete icons[key];
  icons[key] = { etag, source };
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
  return values.get(projectIconCacheKey(chatId, cwd));
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
    let reply = local
      ? await call('projectIcon', chatId, clean, { ifNoneMatch: prior?.etag ?? '' })
      : await call('projectIcon', chatId, clean);
    let source: string | null;
    if (prior && isUnchangedReply(reply, prior.etag)) {
      source = prior.source;
    } else {
      if (reply && typeof reply === 'object' && (reply as { unchanged?: unknown }).unchanged === true) {
        reply = await call('projectIcon', chatId, clean);
      }
      source = projectIconDataURL(reply as ProjectIconResult | undefined);
      const etag = (reply as { etag?: unknown } | undefined)?.etag;
      if (local && typeof etag === 'string' && etag) keepIcon(key, etag, source);
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

export function clearProjectIconCache(): void {
  values.clear();
  pending.clear();
}
