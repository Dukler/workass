import assert from 'node:assert/strict';
import test from 'node:test';
import { LEAN_SESSION_SAVE_MODE, localMirror, type Mirror } from '../src/store/persistence.ts';

function actorSnapshot(): Mirror {
  return {
    v: 1,
    activeId: 'tab-0',
    seq: 3,
    theme: 'dark',
    density: 'compact',
    mode: 'chats',
    panes: { side: true, railWide: false, sideW: 288, railW: 312 },
    chats: [{
      id: 'tab-0',
      chatId: 'chat-0',
      title: 'Chat 0',
      titleLocked: true,
      group: null,
      cwd: null,
      currentModelId: null,
      currentModeId: null,
      draft: 'draft',
      messages: [{
        id: 'm-0',
        role: 'assistant',
        content: 'retained transcript content',
        status: 'done',
        at: null,
        events: [{ kind: 'tool', key: 't-0', output: 'retained tool output' }],
      }],
    }],
  };
}

test('local preference persistence excludes actor-owned transcript payloads', () => {
  const snapshot = actorSnapshot();
  const local = localMirror(snapshot);

  assert.deepEqual(local.chats, []);
  assert.equal(local.activeId, snapshot.activeId);
  assert.equal(local.theme, snapshot.theme);
  assert.equal(snapshot.chats[0].messages[0].events.length, 1, 'projection mutated the actor snapshot');
  assert.ok(JSON.stringify(local).length < JSON.stringify(snapshot).length);
});

test('transient session-save markers never enter localStorage', () => {
  const marked = {
    ...actorSnapshot(),
    _workassSave: LEAN_SESSION_SAVE_MODE,
    _workassDeletedChatIds: ['chat'],
  } satisfies Mirror;
  const local = localMirror(marked);
  assert.equal(local._workassSave, undefined);
  assert.equal(local._workassDeletedChatIds, undefined);
  assert.deepEqual(local.chats, []);
});
