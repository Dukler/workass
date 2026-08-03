import assert from 'node:assert/strict';
import test from 'node:test';
import type { Chat, Workspace } from '../src/store/types.ts';
import { buildWorkspaceGroups, chooseWorkspacePath, inheritChatControls, lastProject, newChatTarget, normalizeWorkspacePath, normalizeWorkspaces, rememberLastProject, workspaceName } from '../src/workspaces.ts';

function chat(id: string, cwd: string | null): Chat {
  return { id, chatId: id, sessionId: null, title: id, titleLocked: false, group: null, cwd, currentModelId: null, currentModeId: null, pending: true, messages: [], draft: '' };
}

test('normalizes folder aliases without corrupting roots', () => {
  assert.equal(normalizeWorkspacePath('/Users/me/project///'), '/Users/me/project');
  assert.equal(normalizeWorkspacePath('/'), '/');
  assert.equal(normalizeWorkspacePath('C:\\work\\'), 'C:\\work');
  assert.equal(normalizeWorkspacePath('C:\\'), 'C:\\');
  assert.equal(workspaceName('/Users/me/project'), 'project');
});

test('folder groups persist even when empty and collect chats by cwd', () => {
  const workspaces: Workspace[] = [{ path: '/repo/workass', name: 'workass' }, { path: '/repo/other', name: 'other' }];
  const groups = buildWorkspaceGroups(workspaces, [chat('a', '/repo/workass'), chat('b', '/repo/other')]);
  assert.deepEqual(groups.map((group) => [group.path, group.chats.map((item) => item.id)]), [
    ['/repo/workass', ['a']], ['/repo/other', ['b']],
  ]);
  assert.equal(buildWorkspaceGroups(workspaces, [chat('a', '/repo/workass')])[1].chats.length, 0);
});

test('a removed folder is not resurrected by a chat whose cwd still points there', () => {
  // The "removed folder came back" bug: a chat keeps its cwd (its bound session
  // still runs there), but the folder the user removed must NOT re-appear — the
  // chat falls to the unassigned "Chats" bucket instead.
  const workspaces: Workspace[] = [{ path: '/repo/workass', name: 'workass' }];
  const chats = [chat('a', '/repo/workass'), chat('b', '/repo/network')];
  const groups = buildWorkspaceGroups(workspaces, chats, ['/repo/network']);
  assert.equal(groups.some((g) => g.path === '/repo/network'), false);      // folder gone
  const unassigned = groups.find((g) => g.path === '');
  assert.deepEqual(unassigned?.chats.map((c) => c.id), ['b']);              // its chat → Chats
  // Re-adding it (empty removed list) brings the folder back with its chat.
  const back = buildWorkspaceGroups(workspaces, chats, []);
  assert.deepEqual(back.find((g) => g.path === '/repo/network')?.chats.map((c) => c.id), ['b']);
});

test('new chats prefer an explicit folder, then active chat, then saved folders', () => {
  const saved = normalizeWorkspaces([{ path: '/repo/one/', name: 'one' }, { path: '/repo/one', name: 'duplicate' }, { path: '/repo/two', name: 'two' }]);
  assert.equal(saved.length, 2);
  assert.equal(chooseWorkspacePath('/repo/two', chat('a', '/repo/one'), saved, '/fallback'), '/repo/two');
  assert.equal(chooseWorkspacePath(null, chat('a', '/repo/one'), saved, '/fallback'), '/repo/one');
  assert.equal(chooseWorkspacePath(null, null, saved, '/fallback'), '/repo/one');
});

test('the sidebar + targets the scoped project, else the last one a chat started in', () => {
  // The bug this encodes: with no target the store falls back to the ACTIVE
  // chat's cwd, so every chat started while reading a workass thread was another
  // workass chat and the project selector never had a say.
  const groups = buildWorkspaceGroups(
    normalizeWorkspaces([{ path: '/repo/workass', name: 'workass' }, { path: '/repo/depthcut', name: 'depthcut' }]),
    [chat('a', '/repo/workass')],
  );
  assert.equal(newChatTarget('/repo/depthcut', groups, '/repo/workass'), '/repo/depthcut');   // scope wins over memory
  assert.equal(newChatTarget('/repo/depthcut/', groups, null), '/repo/depthcut');             // alias-normalized
  assert.equal(newChatTarget(null, groups, '/repo/depthcut'), '/repo/depthcut');              // "all" → last started in
  assert.equal(newChatTarget('', groups, '/repo/depthcut'), '/repo/depthcut');                // unassigned bucket == "all"
  assert.equal(newChatTarget(null, groups, '/repo/removed'), null);                           // removing a folder stops it
  assert.equal(newChatTarget(null, groups, null), null);                                      // nothing remembered yet
});

test('the remembered project survives a reload and is never cleared by an empty path', () => {
  const cell = new Map<string, string>();
  const original = (globalThis as { localStorage?: Storage }).localStorage;
  (globalThis as { localStorage?: unknown }).localStorage = {
    getItem: (key: string) => cell.get(key) ?? null,
    setItem: (key: string, value: string) => { cell.set(key, String(value)); },
  };
  try {
    assert.equal(lastProject(), null);
    rememberLastProject('/repo/depthcut/');
    assert.equal(lastProject(), '/repo/depthcut');
    rememberLastProject('');            // a chat with no resolvable cwd must not erase the memory
    rememberLastProject(null);
    assert.equal(lastProject(), '/repo/depthcut');
  } finally {
    (globalThis as { localStorage?: unknown }).localStorage = original;
  }
});

test('a folder-created chat inherits the active agent, model, and permission mode', () => {
  const active = chat('active', '/repo/workass');
  active.providerId = 'codex';
  active.providerName = 'Codex ACP';
  active.currentModelId = 'gpt-5.6-sol[xhigh]';
  active.currentModeId = 'agent-full-access';
  assert.deepEqual(inheritChatControls(active), {
    providerId: 'codex', providerName: 'Codex ACP',
    currentModelId: 'gpt-5.6-sol[xhigh]', currentModeId: 'agent-full-access',
  });
  assert.deepEqual(inheritChatControls(null), {
    providerId: null, providerName: null, currentModelId: null, currentModeId: null,
  });
});
