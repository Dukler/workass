// Filename-first labelling for the Entorno rail. The daemon (internal/acp/env.go)
// reports the current/most-recent turn's changed files as repo-relative paths;
// this module turns those long paths into a dense, basename-led display:
//
//   - the basename is always the primary label (never a kilometer-long path),
//   - a dimmed parent context is shown ONLY to disambiguate same-name files,
//   - the context is the shortest trailing directory tail that is unique among
//     the colliding files, prefixed with "…/" when higher segments are omitted.
//
// Pure and framework-free so it can be unit-tested without React. Separators are
// normalized so a Windows daemon ("a\\b\\x.ts") and a POSIX one agree.

import type { ChatEnvPayload } from './wire/types';

export interface EnvFileRow {
  // Original repo-relative path — the full value for the row tooltip + copy.
  path: string;
  // Basename: the primary, filename-first label.
  name: string;
  // Shortest parent-path context that disambiguates same-name files; '' when the
  // basename is already unique. May start with "…/" when higher segments are cut.
  parent: string;
  adds: number;
  dels: number;
}

export interface EnvGroup {
  name: string;
  branch: string;
  rows: EnvFileRow[];
  adds: number;
  dels: number;
  filesTruncated: boolean;
}

export interface EnvView {
  groups: EnvGroup[];
  fileCount: number;
  adds: number;
  dels: number;
  hasChanges: boolean;
  reposTruncated: boolean;
  filesTruncated: boolean;
  unchanged: string[];
}

// Split on either separator, collapse runs, drop empties and "." segments so
// "./a//b" and ".\\a\\b" both yield ["a", "b"].
const SEP = /[/\\]+/;

export function splitPath(path: string): string[] {
  return String(path ?? '')
    .split(SEP)
    .map((seg) => seg.trim())
    .filter((seg) => seg !== '' && seg !== '.');
}

export function basename(path: string): string {
  const segs = splitPath(path);
  if (segs.length) return segs[segs.length - 1];
  // A path that is only separators/dots has no real basename; fall back to the
  // trimmed original so the row is never blank.
  return String(path ?? '').trim();
}

// Smallest trailing tail of `mine` that no `other` shares at the same length.
// When `mine` is itself a suffix of a longer sibling at every one of its own
// lengths, we return the full parent (no ellipsis) — the longer sibling is then
// forced to show more, so the two still read differently.
function shortestDistinguishingParent(mine: string[], others: string[][]): string {
  for (let k = 1; k <= mine.length; k++) {
    const tail = mine.slice(mine.length - k).join('/');
    const collides = others.some((other) => other.length >= k && other.slice(other.length - k).join('/') === tail);
    if (!collides) return k < mine.length ? '…/' + tail : tail;
  }
  return mine.join('/');
}

export function labelChangedFiles(files: ReadonlyArray<{ path: string; adds: number; dels: number }>): EnvFileRow[] {
  const parsed = files.map((file) => {
    const segs = splitPath(file.path);
    const name = segs.length ? segs[segs.length - 1] : String(file.path ?? '').trim() || '—';
    return { file, name, parentSegs: segs.slice(0, -1) };
  });

  const byName = new Map<string, number[]>();
  parsed.forEach((entry, i) => {
    const bucket = byName.get(entry.name);
    if (bucket) bucket.push(i);
    else byName.set(entry.name, [i]);
  });

  return parsed.map((entry, i) => {
    const group = byName.get(entry.name) ?? [i];
    let parent = '';
    if (group.length > 1 && entry.parentSegs.length > 0) {
      const others = group.filter((j) => j !== i).map((j) => parsed[j].parentSegs);
      parent = shortestDistinguishingParent(entry.parentSegs, others);
    }
    return { path: entry.file.path, name: entry.name, parent, adds: entry.file.adds, dels: entry.file.dels };
  });
}

// Shape a raw ChatEnvPayload into the view the card renders. Repos with no
// changed files are dropped (the payload keeps them in `unchanged`); the
// per-repo diffstat is summed from the rows actually shown so header/foot totals
// never disagree with the list.
export function envView(payload: ChatEnvPayload | null | undefined): EnvView {
  const repos = Array.isArray(payload?.repos) ? payload!.repos : [];
  const groups: EnvGroup[] = repos
    .filter((repo) => repo && Array.isArray(repo.files) && repo.files.length > 0)
    .map((repo) => {
      const rows = labelChangedFiles(repo.files);
      const adds = rows.reduce((sum, row) => sum + (row.adds || 0), 0);
      const dels = rows.reduce((sum, row) => sum + (row.dels || 0), 0);
      return { name: repo.name, branch: repo.branch ?? '', rows, adds, dels, filesTruncated: !!repo.filesTruncated };
    });

  const fileCount = groups.reduce((sum, group) => sum + group.rows.length, 0);
  return {
    groups,
    fileCount,
    adds: groups.reduce((sum, group) => sum + group.adds, 0),
    dels: groups.reduce((sum, group) => sum + group.dels, 0),
    hasChanges: fileCount > 0,
    reposTruncated: !!payload?.reposTruncated,
    filesTruncated: !!payload?.filesTruncated,
    unchanged: Array.isArray(payload?.unchanged) ? payload!.unchanged : [],
  };
}

// A quiet one-liner for the empty state — names the watched repo(s) when known
// rather than sitting as a dead box.
export function emptyEnvSummary(view: EnvView): string {
  const count = view.unchanged.length;
  if (count === 0) return 'Sin cambios todavía.';
  if (count === 1) return `Sin cambios · ${view.unchanged[0]}`;
  return `Sin cambios · ${count} repos`;
}
