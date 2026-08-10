// Server-owned folder model behind the workspace browser.
//
// Every client — the Electron shell on the daemon host, and any LAN/browser
// client — browses the folders of the machine that RUNS Workass, through the
// frozen fs:list-dir channel (window.api.listDir). Native dialogs, shell pickers
// and window.prompt are banned here (user, 2026-07-12): a viewing device must
// never be able to name or pick one of ITS OWN paths as a workspace.
//
// The module deliberately has no runtime imports (types only) so the repo's
// `node --experimental-strip-types --test` runner can exercise it directly. The
// bridge call itself lives in WorkspaceBrowser.tsx, which goes through wire/api
// like every other consumer.

import type { DirCreateResult, DirEntry, DirListing } from './wire/types';

/** Shown only while the initial listDir(null) request has not resolved yet. */
export const SERVER_ROOTS_LABEL = 'Carpetas del servidor';

const INVALID_REPLY = 'El servidor respondió algo que no pude leer.';
const UNKNOWN_ERROR = 'Error desconocido del servidor.';
const WRONG_FOLDER = 'El servidor abrió una carpeta diferente de la solicitada.';
const WRONG_CREATED_FOLDER = 'El servidor creó una carpeta diferente de la solicitada.';

function text(value: unknown): string {
  return typeof value === 'string' ? value.trim() : '';
}

/** Absolute server paths only; '' and non-strings collapse to null (the roots). */
function optionalPath(value: unknown): string | null {
  return text(value) || null;
}

export function normalizeEntries(value: unknown): DirEntry[] {
  if (!Array.isArray(value)) return [];
  const entries: DirEntry[] = [];
  for (const item of value) {
    if (!item || typeof item !== 'object') continue;
    const raw = item as { name?: unknown; path?: unknown };
    const path = text(raw.path);
    if (!path) continue;
    entries.push({ name: text(raw.name) || path, path });
  }
  return entries;
}

/** Client-side feedback only; the daemon repeats these checks authoritatively. */
export function folderNameError(value: string): string | null {
  const name = value.trim();
  if (!name) return 'Escribí un nombre para la carpeta.';
  if (name === '.' || name === '..' || name.includes('/') || name.includes('\\') || name.includes('\0')) {
    return 'Usá un solo nombre, sin separadores de ruta.';
  }
  return null;
}

/**
 * A create reply is exact-address data just like a listing: the UI may enter it
 * only when the daemon confirms the same parent and name the user submitted.
 */
export function normalizeCreatedDirectory(raw: unknown, requestedParent: string, requestedName: string): DirCreateResult {
  const name = requestedName.trim();
  if (!raw || typeof raw !== 'object') {
    return { name, path: null, parent: requestedParent, error: INVALID_REPLY };
  }
  const reply = raw as { name?: unknown; path?: unknown; parent?: unknown; error?: unknown };
  const error = text(reply.error);
  if (error) return { name, path: null, parent: requestedParent, error };
  const repliedName = text(reply.name);
  const path = optionalPath(reply.path);
  const parent = optionalPath(reply.parent);
  if (!path || parent !== requestedParent || repliedName !== name) {
    return { name, path: null, parent: requestedParent, error: WRONG_CREATED_FOLDER };
  }
  return { name, path, parent };
}

/**
 * Defensive parse of an fs:list-dir reply. `requested` is the path we asked for,
 * so a reply that omits `path` still anchors the dialog where the user is; the
 * default-home request legitimately resolves `null` to the server user's
 * absolute home path. Older daemons may still return their legacy `path: null`
 * shortcut listing.
 */
export function normalizeListing(raw: unknown, requested: string | null): DirListing {
  if (!raw || typeof raw !== 'object') return errorListing(requested, INVALID_REPLY);
  const reply = raw as { path?: unknown; parent?: unknown; entries?: unknown; error?: unknown };
  const repliedPath = optionalPath(reply.path);
  // fs:list-dir is an exact-address operation, not a redirect. Accept an
  // omitted path for compatibility with older daemons, but never let a reply
  // paint a different folder than the row the user selected. Without this
  // check a malformed/stale bridge reply makes the picker appear to jump at
  // random even though the click target itself was correct.
  const resolvedDefaultHome = requested === null && repliedPath !== null;
  if (reply.path !== undefined && repliedPath !== requested && !resolvedDefaultHome) {
    return errorListing(requested, WRONG_FOLDER);
  }
  const listing: DirListing = {
    path: repliedPath ?? requested,
    parent: optionalPath(reply.parent),
    entries: normalizeEntries(reply.entries),
  };
  const error = text(reply.error);
  if (error) listing.error = error;
  return listing;
}

export interface FolderPointerNavigation {
  x: number;
  y: number;
  at: number;
}

/**
 * A first click replaces the directory rows. The second click of a normal
 * double-click can therefore land on a NEW row at the same screen coordinate
 * and open an unrelated child. Ignore only that click-through gesture; quick
 * intentional clicks at different coordinates and keyboard activation remain
 * unaffected.
 */
export function isFolderClickThrough(
  previous: FolderPointerNavigation | null,
  next: FolderPointerNavigation,
  windowMS = 420,
  tolerancePX = 5,
): boolean {
  return !!previous
    && next.at >= previous.at
    && next.at - previous.at <= windowMS
    && Math.abs(next.x - previous.x) <= tolerancePX
    && Math.abs(next.y - previous.y) <= tolerancePX;
}

/** A listing that carries nothing but an honest failure (dead bridge, bad reply). */
export function errorListing(requested: string | null, err: unknown): DirListing {
  return { path: requested, parent: null, entries: [], error: describeError(err) };
}

function describeError(err: unknown): string {
  if (err instanceof Error) return text(err.message) || UNKNOWN_ERROR;
  if (typeof err === 'string') return text(err) || UNKNOWN_ERROR;
  if (err && typeof err === 'object' && 'message' in err) {
    const message = text((err as { message: unknown }).message);
    if (message) return message;
  }
  return UNKNOWN_ERROR;
}

/** The roots listing is a set of shortcuts, not a folder — there is nothing to confirm. */
export function isServerRoots(listing: DirListing | null): boolean {
  return !!listing && listing.path === null;
}

/** Only a folder the server actually opened can become a workspace. */
export function canSelectFolder(listing: DirListing | null): boolean {
  return !!listing && !listing.error && !!listing.path;
}

/** The absolute server path for the nav bar; the roots have no path to show. */
export function pathLabel(listing: DirListing | null): string {
  return listing?.path ?? SERVER_ROOTS_LABEL;
}

export function folderCountLabel(listing: DirListing | null): string {
  const count = listing?.entries.length ?? 0;
  if (count === 0) return 'Sin subcarpetas';
  return count === 1 ? '1 carpeta' : `${count} carpetas`;
}
