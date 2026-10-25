// What a reload can reuse from the read before it, so an unchanged daemon
// answers it with a few bytes instead of the whole session again.
//
// Each entry is a daemon reply kept as its JSON text together with the content
// hash (etag) the daemon named for it. It is only ever used after the daemon
// confirms that same etag is still current ({ifNoneMatch} answered
// {unchanged}), so the client never treats it as authority: the daemon stays
// the sole owner of chats and transcripts, exactly as with no cache.
//
// sessionStorage, not localStorage: the entry lives as long as the window does
// (a reload, a reconnect, an Electron in-place reload keep it) and nothing is
// left at rest once the window closes. The text is parsed afresh on every use,
// because the store mutates the objects it hydrates from.

export interface WireCacheEntry { etag: string; text: string }

const PREFIX = 'workass.renderer2.wire-cache.';
// Far above an ordinary session shell (about 0.3 MB for 250 chats), and well
// inside the browser's per-origin sessionStorage quota.
const MAX_TEXT_CHARS = 4_000_000;

function storage(): Storage | null {
  try { return typeof sessionStorage !== 'undefined' ? sessionStorage : null; } catch { return null; }
}

export function readWireCache(name: string): WireCacheEntry | null {
  const store = storage();
  if (!store) return null;
  try {
    const raw = store.getItem(PREFIX + name);
    if (!raw) return null;
    const cut = raw.indexOf('\n');
    if (cut <= 0) return null;
    const etag = raw.slice(0, cut);
    const text = raw.slice(cut + 1);
    return etag && text ? { etag, text } : null;
  } catch { return null; }
}

export function writeWireCache(name: string, etag: string, text: string): void {
  const store = storage();
  if (!store) return;
  try {
    if (!etag || etag.includes('\n') || !text || text.length > MAX_TEXT_CHARS) {
      store.removeItem(PREFIX + name);
      return;
    }
    store.setItem(PREFIX + name, `${etag}\n${text}`);
  } catch {
    // Quota or a disabled store: drop the entry so a stale one is never kept.
    try { store.removeItem(PREFIX + name); } catch { /* nothing to do */ }
  }
}

export function parseWireCache<T>(entry: WireCacheEntry | null): T | null {
  if (!entry) return null;
  try { return JSON.parse(entry.text) as T; } catch { return null; }
}

/** The daemon's "you already have it" receipt for exactly this etag. */
export function isUnchangedReply(reply: unknown, etag: string): boolean {
  return !!etag && !!reply && typeof reply === 'object'
    && (reply as { unchanged?: unknown }).unchanged === true
    && (reply as { etag?: unknown }).etag === etag;
}
