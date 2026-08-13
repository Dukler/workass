import assert from 'node:assert/strict';
import test from 'node:test';
import { LEAN_SESSION_SAVE_MODE, localMirror, type Mirror } from '../src/store/persistence.ts';

function snapshot(): Mirror {
  return {
    v: 1,
    activeId: 'tab-active',
    seq: 7,
    theme: 'dark',
    density: 'compact',
    mode: 'chats',
    panes: { side: true, railWide: false, sideW: 288, railW: 312 },
    _workassSave: LEAN_SESSION_SAVE_MODE,
    chats: [{
      id: 'tab-active', chatId: 'chat-active', title: 'Active', titleLocked: true,
      group: null, cwd: null, currentModelId: null, currentModeId: null, draft: 'draft',
      queue: [{ id: 'q-1', text: 'queued', images: [{ mimeType: 'image/png', data: 'large' }] }],
      messages: [{ id: 'm-1', role: 'assistant', content: 'answer', status: 'done', at: null, events: [] }],
    }],
  };
}

test('localStorage keeps view preferences and no actor-owned chat state', () => {
  const source = snapshot();
  const local = localMirror(source);
  assert.equal(local.activeId, source.activeId);
  assert.equal(local.theme, source.theme);
  assert.deepEqual(local.panes, source.panes);
  assert.deepEqual(local.chats, []);
  assert.equal(local._workassSave, undefined);
  assert.equal(source.chats.length, 1, 'projection must not mutate actor data');
});
