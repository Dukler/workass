// state:digest carries SHA-256 hashes of Go's json.Marshal output; the renderer
// hashes what it received and compares. json.Marshal escapes <, > and & (and
// U+2028/U+2029) inside strings, while JSON.stringify does not, so any catalog
// group, setting or process command holding one of them (an error "a -> b", a
// command "npm run dev > log 2>&1") hashed differently on each side forever and
// re-synced on every heartbeat. Encode exactly as Go does.
const GO_ESCAPED = /[<>&\u2028\u2029]/g;

export function goJSON(value: unknown): string {
  const json = JSON.stringify(value) ?? 'null';
  return json.replace(GO_ESCAPED, (char) => `\\u${char.charCodeAt(0).toString(16).padStart(4, '0')}`);
}

export async function stateDigestHash(value: unknown): Promise<string | null> {
  const subtle = globalThis.crypto?.subtle;
  if (!subtle) return null;
  try {
    const encoded = new TextEncoder().encode(goJSON(value));
    const digest = await subtle.digest('SHA-256', encoded);
    return [...new Uint8Array(digest)].map((part) => part.toString(16).padStart(2, '0')).join('');
  } catch {
    return null;
  }
}

/** A stable identity for one digest's catalog hash map, keyed by provider. */
export function catalogHashKey(hashes: Record<string, string> | null | undefined): string {
  const entries = Object.entries(hashes ?? {}).sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0));
  return JSON.stringify(entries);
}
