import assert from 'node:assert/strict';
import { performance } from 'node:perf_hooks';
import test from 'node:test';
import { LEAN_SESSION_SAVE_MODE, localMirror, type Mirror } from '../src/store/persistence.ts';

function largeActorSnapshot(): Mirror {
  const heavy = 'tool output '.repeat(90);
  return {
    v: 1,
    activeId: 'tab-0',
    seq: 300,
    theme: 'dark',
    density: 'compact',
    mode: 'chats',
    panes: { side: true, railWide: false, sideW: 288, railW: 312 },
    chats: Array.from({ length: 5 }, (_, chatIndex) => ({
      id: `tab-${chatIndex}`,
      chatId: `chat-${chatIndex}`,
      title: `Chat ${chatIndex}`,
      titleLocked: true,
      group: null,
      cwd: null,
      currentModelId: null,
      currentModeId: null,
      draft: chatIndex === 0 ? 'draft' : '',
      messages: Array.from({ length: 60 }, (_, messageIndex) => ({
        id: `m-${chatIndex}-${messageIndex}`,
        role: messageIndex % 2 ? 'assistant' as const : 'user' as const,
        content: `message ${messageIndex}`,
        status: 'done' as const,
        at: null,
        events: Array.from({ length: 20 }, (_, eventIndex) => ({
          kind: 'tool', key: `t-${eventIndex}`, output: heavy,
        })),
      })),
    })),
  };
}

test('benchmark: local preference projection is independent of transcript payload size', () => {
  const snapshot = largeActorSnapshot();
  const fullBytes = JSON.stringify(snapshot).length;
  const samples: number[] = [];
  let localBytes = 0;
  for (let index = 0; index < 20; index++) {
    const started = performance.now();
    localBytes = JSON.stringify(localMirror(snapshot)).length;
    samples.push(performance.now() - started);
  }
  samples.sort((a, b) => a - b);
  const p95 = samples[Math.floor(samples.length * 0.95)];
  assert.ok(fullBytes > 5_000_000, `fixture was only ${fullBytes} bytes`);
  assert.ok(localBytes < fullBytes / 1000, `local preferences retained actor data: ${localBytes}/${fullBytes}`);
  assert.equal(snapshot.chats[0].messages[0].events.length, 20, 'projection mutated the actor snapshot');
  assert.ok(p95 < 8, `p95 local persistence preparation ${p95.toFixed(3)}ms exceeded 8ms`);
});

test('benchmark fixture still excludes transient session-save markers', () => {
  const marked = {
    ...largeActorSnapshot(),
    _workassSave: LEAN_SESSION_SAVE_MODE,
    _workassDeletedChatIds: ['chat'],
  } satisfies Mirror;
  const local = localMirror(marked);
  assert.equal(local._workassSave, undefined);
  assert.equal(local._workassDeletedChatIds, undefined);
  assert.deepEqual(local.chats, []);
});
