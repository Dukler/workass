import { normalizeWorkspacePath } from './workspaces.ts';
import { call, has } from './wire/api.ts';
import { machineOf } from './wire/machineIds.ts';
import type { ProjectIconResult } from './wire/types.ts';

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
    const source = projectIconDataURL(await call('projectIcon', chatId, clean));
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
