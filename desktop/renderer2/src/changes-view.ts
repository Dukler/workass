// «Cambios» — what the activity pane's second tab draws from a changes:state
// snapshot (approved mock rail-cambios-v2). Pure, so the grouping, the folding
// and the diff peek are tested without React.
//
//   - only the repos this chat works in or edited (2026-11-05: another chat's
//     repo is not this chat's business);
//   - one row per repo with changes, the most recently changed first; repos
//     with nothing changed collapse into one «N repos sin cambios» line;
//   - inside a repo, files grouped by who changed them: THIS chat first, then
//     other chats by their most recent edit, then «Fuera de Workass» for the
//     files no chat's agent edited;
//   - the repo with the most recent change starts unfolded, the rest folded.

import type { ChangesFile, ChangesRepo, ChangesState } from './wire/types';

export type BarCell = 'a' | 'd' | '';

export interface ChangesFileRow {
  path: string;
  name: string;
  folder: string;
  adds: number;
  dels: number;
  isNew: boolean;
  deleted: boolean;
  binary: boolean;
  at: number;
}

export interface ChangesGroup {
  kind: 'this' | 'chat' | 'outside';
  key: string;
  chatId: string;
  title: string;
  providerId: string;
  at: number;
  files: ChangesFileRow[];
}

export interface ChangesRepoRow {
  root: string;
  name: string;
  branch: string;
  ahead: number;
  behind: number;
  adds: number;
  dels: number;
  bar: BarCell[];
  checking: boolean;
  latest: number;
  /** Every changed file; `shown` of them are listed. */
  fileCount: number;
  shown: number;
  filesTruncated: boolean;
  groups: ChangesGroup[];
}

export interface ChangesView {
  repos: ChangesRepoRow[];
  clean: { count: number; names: string[] };
  fileCount: number;
  /** Nothing changed anywhere, and the daemon has said so. */
  empty: boolean;
  /** No answer yet: draw nothing rather than claim it is clean. */
  pending: boolean;
  /** The repo that starts unfolded. */
  openRoot: string;
}

/** Who the pane belongs to, and the renderer's own names for chats. */
export interface ChangesContext {
  chatId: string;
  chatTitle?: (chatId: string) => string | undefined;
  chatProvider?: (chatId: string) => string | undefined;
}

const SEP = /[/\\]+/;

/** Filename first; the muted folder is the file's own folder when it is short
 *  (one or two levels), else just its last level. */
export function fileLabel(path: string): { name: string; folder: string } {
  const segs = String(path ?? '').split(SEP).filter((seg) => seg && seg !== '.');
  const name = segs.length ? segs[segs.length - 1] : String(path ?? '');
  const dirs = segs.slice(0, -1);
  const folder = dirs.length <= 2 ? dirs.join('/') : dirs[dirs.length - 1];
  return { name, folder };
}

/** The five-square change bar: one square per ten changed lines (at least
 *  one, at most five), split between additions and removals by their share,
 *  each side keeping a square when it has any lines. */
export function changeBar(adds: number, dels: number): BarCell[] {
  const a = Math.max(0, adds || 0);
  const d = Math.max(0, dels || 0);
  const total = a + d;
  const cells: BarCell[] = ['', '', '', '', ''];
  if (total === 0) return cells;
  let filled = Math.min(5, Math.max(1, Math.ceil(total / 10)));
  if (a > 0 && d > 0) filled = Math.max(filled, 2);
  let addCells = a === 0 ? 0 : d === 0 ? filled : Math.round((filled * a) / total);
  if (a > 0 && d > 0) addCells = Math.min(filled - 1, Math.max(1, addCells));
  for (let i = 0; i < filled; i++) cells[i] = i < addCells ? 'a' : 'd';
  return cells;
}

function fileRow(file: ChangesFile): ChangesFileRow {
  const { name, folder } = fileLabel(file.path);
  return {
    path: file.path, name, folder,
    adds: Math.max(0, file.adds || 0), dels: Math.max(0, file.dels || 0),
    isNew: !!file.new, deleted: !!file.deleted, binary: !!file.binary, at: file.at || 0,
  };
}

function byRecent(a: ChangesFileRow, b: ChangesFileRow): number {
  return (b.at - a.at) || a.path.localeCompare(b.path);
}

function groupsOf(repo: ChangesRepo, chats: Map<string, { title: string; providerId: string }>, ctx: ChangesContext): ChangesGroup[] {
  const byChat = new Map<string, ChangesGroup>();
  const outside: ChangesGroup = { kind: 'outside', key: 'outside', chatId: '', title: 'Fuera de Workass', providerId: '', at: 0, files: [] };
  for (const file of repo.files ?? []) {
    const row = fileRow(file);
    const chatId = String(file.chatId ?? '');
    if (!chatId) { outside.files.push(row); continue; }
    let group = byChat.get(chatId);
    if (!group) {
      const known = chats.get(chatId);
      const title = ctx.chatTitle?.(chatId) || known?.title || 'Chat sin título';
      const providerId = ctx.chatProvider?.(chatId) || known?.providerId || '';
      group = { kind: chatId === ctx.chatId ? 'this' : 'chat', key: `chat:${chatId}`, chatId, title, providerId, at: 0, files: [] };
      byChat.set(chatId, group);
    }
    group.files.push(row);
    group.at = Math.max(group.at, row.at);
  }
  const groups = [...byChat.values()];
  for (const group of groups) group.files.sort(byRecent);
  outside.files.sort((a, b) => a.path.localeCompare(b.path));
  groups.sort((a, b) => {
    if (a.kind !== b.kind) return a.kind === 'this' ? -1 : b.kind === 'this' ? 1 : 0;
    return (b.at - a.at) || a.title.localeCompare(b.title);
  });
  if (outside.files.length) groups.push(outside);
  return groups;
}

/** Whether a repo belongs in this chat's tab. A daemon from before scoping
 *  names no chats, and then every repo does. */
function isChatsRepo(repo: ChangesRepo, chatId: string): boolean {
  return !Array.isArray(repo.chatIds) || (!!chatId && repo.chatIds.includes(chatId));
}

export function changesView(state: ChangesState | null | undefined, ctx: ChangesContext): ChangesView {
  const chats = new Map<string, { title: string; providerId: string }>();
  for (const chat of state?.chats ?? []) chats.set(chat.chatId, { title: chat.title ?? '', providerId: chat.providerId ?? '' });
  const repos: ChangesRepoRow[] = [];
  const cleanNames: string[] = [];
  let answered = false;
  // A snapshot built for other chats' tabs says nothing about this one yet.
  const scoped = !Array.isArray(state?.scope) || state.scope.includes(ctx.chatId);
  const mine = scoped ? (state?.repos ?? []).filter((repo) => isChatsRepo(repo, ctx.chatId)) : [];
  for (const repo of mine) {
    if (!repo.checked) continue;
    answered = true;
    const files = repo.files ?? [];
    if (files.length === 0) { cleanNames.push(repo.name); continue; }
    const groups = groupsOf(repo, chats, ctx);
    const latest = Math.max(repo.activityAt || 0, ...files.map((file) => file.at || 0));
    repos.push({
      root: repo.root, name: repo.name, branch: repo.branch || (repo.detached ? 'HEAD' : ''),
      ahead: Math.max(0, repo.ahead || 0), behind: Math.max(0, repo.behind || 0),
      adds: repo.adds || 0, dels: repo.dels || 0, bar: changeBar(repo.adds || 0, repo.dels || 0),
      checking: !!repo.checking, latest, fileCount: Math.max(files.length, repo.fileCount || 0), shown: files.length,
      filesTruncated: !!repo.filesTruncated, groups,
    });
  }
  repos.sort((a, b) => (b.latest - a.latest) || a.name.localeCompare(b.name));
  cleanNames.sort((a, b) => a.localeCompare(b));
  const fileCount = repos.reduce((sum, repo) => sum + repo.fileCount, 0);
  // Pending: no snapshot yet, or repos whose first answer has not come. A
  // snapshot with no repos at all is an answer: nothing was touched.
  const pending = !state || !scoped || (!answered && mine.length > 0);
  return {
    repos, clean: { count: cleanNames.length, names: cleanNames }, fileCount,
    empty: !pending && repos.length === 0, pending,
    openRoot: repos[0]?.root ?? '',
  };
}

export function cleanLine(count: number): string {
  return count === 1 ? '1 repo sin cambios' : `${count} repos sin cambios`;
}

export function filesWord(count: number): string {
  return count === 1 ? '1 archivo' : `${count} archivos`;
}

// ── Diff peek ─────────────────────────────────────────────────────────────

/** '@' is a hunk header, '…' unchanged lines a peek leaves out. */
export interface DiffLine { kind: '+' | '-' | ' ' | '@' | '…'; text: string }

export interface DiffPeek {
  lines: DiffLine[];
  hunks: number;
  /** New-file line numbers the shown lines cover; 0 when unknown. */
  from: number;
  to: number;
}

/** The lines of a unified diff, without its file headers. */
export function diffLines(text: string): DiffLine[] {
  const out: DiffLine[] = [];
  let inHunk = false;
  for (const raw of String(text ?? '').split('\n')) {
    if (raw.startsWith('@@')) { inHunk = true; out.push({ kind: '@', text: raw }); continue; }
    if (!inHunk) continue;
    if (raw.startsWith('\\')) continue; // "\ No newline at end of file"
    const kind = raw[0];
    if (kind === '+' || kind === '-' || kind === ' ') out.push({ kind, text: raw.slice(1) });
    else if (raw === '') continue;
  }
  return out;
}

/** At most `size` rows of the file's first hunk, tight around what changed:
 *  each run of changed lines with one line of context before it, one line of
 *  context after the last run shown, and a gap mark where unchanged lines are
 *  left out. Also the new-file line range shown and how many hunks there are. */
export function diffPeek(text: string, size = 8): DiffPeek {
  const all = diffLines(text);
  const hunks = all.filter((line) => line.kind === '@').length;
  const firstChange = all.findIndex((line) => line.kind === '+' || line.kind === '-');
  if (firstChange < 0) return { lines: [], hunks, from: 0, to: 0 };
  // The first change's hunk, with each line's new-file number (0 for a removal).
  let header = firstChange;
  while (header > 0 && all[header].kind !== '@') header -= 1;
  const match = /\+(\d+)/.exec(all[header]?.text ?? '');
  let next = match ? Number(match[1]) : 0;
  const hunk: Array<{ line: DiffLine; number: number }> = [];
  for (let i = all[header]?.kind === '@' ? header + 1 : header; i < all.length && all[i].kind !== '@'; i++) {
    const line = all[i];
    hunk.push({ line, number: line.kind === '-' || next <= 0 ? 0 : next });
    if (line.kind !== '-' && next > 0) next += 1;
  }
  const changed = (i: number) => hunk[i]?.line.kind === '+' || hunk[i]?.line.kind === '-';
  // Rows, not lines, are bounded: a gap mark is a row too. Lines are added in
  // order, so a line costs a gap row when it does not follow the last one.
  const keep: number[] = [];
  let rows = 0;
  const cost = (i: number) => (keep.length && i > keep[keep.length - 1] + 1 ? 2 : 1);
  const add = (i: number) => {
    if (i < 0 || i >= hunk.length || (keep.length && i <= keep[keep.length - 1]) || rows + cost(i) > size) return false;
    rows += cost(i);
    keep.push(i);
    return true;
  };
  let last = -1;
  for (let i = 0; i < hunk.length && rows < size; i++) {
    if (!changed(i)) continue;
    // A run's leading context only when the run's first line still fits too.
    if (!changed(i - 1) && hunk[i - 1]?.line.kind === ' ' && rows + cost(i - 1) + 1 <= size) add(i - 1);
    if (add(i)) last = i;
  }
  if (last >= 0 && hunk[last + 1]?.line.kind === ' ') add(last + 1);
  const lines: DiffLine[] = [];
  let from = 0;
  let to = 0;
  let previous = -1;
  for (const i of keep) {
    if (previous >= 0 && i > previous + 1) lines.push({ kind: '…', text: '' });
    previous = i;
    lines.push(hunk[i].line);
    if (hunk[i].number > 0) {
      if (!from) from = hunk[i].number;
      to = hunk[i].number;
    }
  }
  return { lines, hunks, from, to };
}

export function changesWord(hunks: number): string {
  return hunks === 1 ? '1 cambio' : `${hunks} cambios`;
}
