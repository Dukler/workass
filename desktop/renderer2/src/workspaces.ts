import type { Chat, Workspace } from './store/types';

export interface WorkspaceGroup extends Workspace { chats: Chat[]; }

export interface InheritedChatControls {
  providerId: string | null;
  providerName: string | null;
  currentModelId: string | null;
  currentModeId: string | null;
}

export function normalizeWorkspacePath(value: string | null | undefined): string {
  let path = String(value ?? '').trim();
  if (!path) return '';
  // Keep POSIX root and Windows drive roots intact; trim only redundant trailing
  // separators so the same folder cannot be added twice under cosmetic aliases.
  while (path.length > 1 && /[\\/]$/.test(path) && !/^[A-Za-z]:[\\/]$/.test(path)) path = path.slice(0, -1);
  return path;
}

export function workspaceName(path: string): string {
  const clean = normalizeWorkspacePath(path);
  if (!clean) return 'Chats';
  const parts = clean.split(/[\\/]/).filter(Boolean);
  return parts[parts.length - 1] ?? clean;
}

function pathKey(path: string): string {
  const clean = normalizeWorkspacePath(path);
  return /^[A-Za-z]:/.test(clean) ? clean.toLowerCase() : clean;
}

export function workspaceFromPath(path: string): Workspace | null {
  const clean = normalizeWorkspacePath(path);
  return clean ? { path: clean, name: workspaceName(clean) } : null;
}

export function normalizeWorkspaces(values: Workspace[] | undefined): Workspace[] {
  const result: Workspace[] = [];
  const seen = new Set<string>();
  for (const value of values ?? []) {
    const workspace = workspaceFromPath(value?.path);
    if (!workspace) continue;
    const key = pathKey(workspace.path);
    if (seen.has(key)) continue;
    seen.add(key);
    result.push({ ...workspace, name: String(value.name || workspace.name) });
  }
  return result;
}

export function buildWorkspaceGroups(workspaces: Workspace[], chats: Chat[], removed: string[] = []): WorkspaceGroup[] {
  // Folders the user removed are excluded from inference: a chat still lives (its
  // cwd unchanged), it just falls to the unassigned "Chats" bucket instead of
  // re-creating the folder group.
  const removedKeys = new Set(removed.map(pathKey));
  const all = normalizeWorkspaces([
    ...workspaces,
    ...chats.map((chat) => workspaceFromPath(chat.cwd ?? '')).filter((item): item is Workspace => item !== null),
  ]).filter((workspace) => !removedKeys.has(pathKey(workspace.path)));
  const groups = all.map((workspace) => ({ ...workspace, chats: [] as Chat[] }));
  const byPath = new Map(groups.map((group) => [pathKey(group.path), group]));
  const unassigned: Chat[] = [];
  for (const chat of chats) {
    const group = byPath.get(pathKey(chat.cwd ?? ''));
    if (group) group.chats.push(chat);
    else unassigned.push(chat);
  }
  if (unassigned.length) groups.push({ path: '', name: 'Chats', chats: unassigned });
  return groups;
}

export function chooseWorkspacePath(explicit: string | null | undefined, active: Chat | null, workspaces: Workspace[], fallback: string | null | undefined): string | null {
  return normalizeWorkspacePath(explicit)
    || normalizeWorkspacePath(active?.cwd)
    || normalizeWorkspacePath(workspaces[0]?.path)
    || normalizeWorkspacePath(fallback)
    || null;
}

// The folder the user last STARTED a conversation in. Without it, a scope-less
// "new chat" inherits whatever chat you happen to be reading, so every chat born
// while reading a workass thread is another workass chat and there is no fast way
// out (user 2026-07-25). Renderer-local like the sidebar scope it pairs with — a
// per-window UI preference, never chat state on the wire.
const LAST_PROJECT_KEY = 'workass.sv2.lastNewChatProject';

export function rememberLastProject(path: string | null | undefined): void {
  const clean = normalizeWorkspacePath(path);
  if (!clean) return;
  try { localStorage.setItem(LAST_PROJECT_KEY, clean); } catch { /* private mode */ }
}

export function lastProject(): string | null {
  try { return normalizeWorkspacePath(localStorage.getItem(LAST_PROJECT_KEY)) || null; } catch { return null; }
}

// Where the sidebar's + puts the next chat. Scoped to a project → that project,
// always. In "Todos los proyectos" → the last folder a chat was started in, but
// only while it is still a listed group: removing a folder has to stop new chats
// landing there. null = no opinion, and newChat falls back to its own chain.
export function newChatTarget(scope: string | null, groups: WorkspaceGroup[], remembered: string | null): string | null {
  const scoped = normalizeWorkspacePath(scope);
  if (scoped) return scoped;
  const last = normalizeWorkspacePath(remembered);
  if (!last) return null;
  return groups.some((g) => normalizeWorkspacePath(g.path) === last) ? last : null;
}

// Adding a folder changes only cwd. It must not silently reset the selected
// agent/model/permission policy and turn a full-access conversation into an
// approval-per-tool session. A new chat without an active predecessor still
// lets the provider supply its normal defaults at session creation.
export function inheritChatControls(active: Chat | null): InheritedChatControls {
  return {
    providerId: active?.providerId ?? null,
    providerName: active?.providerName ?? null,
    currentModelId: active?.currentModelId ?? null,
    currentModeId: active?.currentModeId ?? null,
  };
}
