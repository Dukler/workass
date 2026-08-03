import test from 'node:test';
import assert from 'node:assert/strict';
import { localMirror, skeletonEvents, type Mirror } from '../src/store/persistence.ts';

function mirrorWith(activeId: string): Mirror {
  const heavy = 'x'.repeat(5000);
  const events = [
    { kind: 'thinking', key: 'th-1', at: 0, text: 'razonando '.repeat(100) },
    {
      kind: 'tool', key: 'tool-1', at: 10, id: 'call-1', toolKind: 'execute', title: 'Bash',
      status: 'completed', command: 'git -C repo status --porcelain', input: heavy, output: heavy,
      location: null, terminalId: null, startedAt: 1, endedAt: 2,
      images: [{ mimeType: 'image/png', name: 'shot.png', data: heavy }],
      subagentId: 'sub-1', subagentLabel: 'review-lane', subagentProvider: 'gpt', subagentModel: 'GPT-5.6-Terra-high',
    },
    { kind: 'plan', key: 'plan-1', at: 20, entries: [{ status: 'completed', content: 'step one' }] },
    { kind: 'bgproc', key: 'bg-1', at: 30, procId: 'p1', label: 'engine', startedAt: 'now', status: 'running' },
  ];
  const message = (id: string) => ({
    id, role: 'assistant' as const, content: `body ${id}`, status: 'done' as const, at: null,
    events: structuredClone(events),
  });
  return {
    v: 1, activeId, seq: 1, theme: 'dark', density: 'compact', mode: 'chats',
    panes: { side: true, railWide: false, sideW: 288, railW: 312 },
    chats: [
      {
        id: 'tab-active', chatId: 'chat-a', title: 'Activa', titleLocked: true, group: null, cwd: null,
        currentModelId: null, currentModeId: null, draft: '',
        messages: Array.from({ length: 14 }, (_, i) => message(`a-${i}`)),
      },
      {
        id: 'tab-other', chatId: 'chat-b', title: 'Otra', titleLocked: true, group: null, cwd: null,
        currentModelId: null, currentModeId: null, draft: '',
        messages: [message('b-0')],
      },
    ],
  };
}

test('the active chat tail keeps first-paint skeletons; everything else stays event-less', () => {
  const local = localMirror(mirrorWith('tab-active'));
  const active = local.chats[0].messages;
  // Old rows above the visible tail hydrate behind a bottom-pinned viewport.
  // They stay blanked rather than filtered: scanning historical events here blew
  // the first-paint budget. The rail's step-by-step lives on `chat.planLatest`.
  assert.deepEqual(active[0].events, []);
  assert.deepEqual(active[3].events, []);
  // The tail (last 10) paints the same rows the daemon copy will confirm.
  for (let i = 4; i < active.length; i++) {
    assert.ok(active[i].events.length > 0, `tail row ${i} lost its skeleton`);
  }
  // Non-active chats paint on click, long after hydration.
  assert.deepEqual(local.chats[1].messages[0].events, []);
});

test('skeletons keep collapsed-line text and keys, never payloads', () => {
  const source = mirrorWith('tab-active').chats[0].messages[13].events;
  const lean = skeletonEvents(source) as Array<Record<string, unknown>>;
  const kinds = lean.map((event) => event.kind);
  assert.deepEqual(kinds, ['thinking', 'tool', 'plan'], 'bgproc is transient and never persisted');

  const thinking = lean[0];
  assert.equal(typeof thinking.text, 'string');
  assert.ok((thinking.text as string).length <= 240, 'thinking text must be capped');
  assert.equal(thinking.key, 'th-1');

  const tool = lean[1];
  assert.equal(tool.key, 'tool-1');
  assert.equal(tool.title, 'Bash');
  assert.equal(tool.command, 'git -C repo status --porcelain');
  assert.equal(tool.status, 'completed');
  assert.equal(tool.subagentLabel, 'review-lane');
  assert.equal(tool.subagentModel, 'GPT-5.6-Terra-high');
  assert.equal(tool.input, null, 'payloads are daemon-owned');
  assert.equal(tool.output, null, 'payloads are daemon-owned');
  assert.equal('images' in tool, false, 'image bytes never enter localStorage');

  assert.deepEqual(lean[2], source[2], 'plan rows are small and pass through');
});

test('skeletons bound the per-message event count', () => {
  const many = Array.from({ length: 300 }, (_, i) => ({
    kind: 'tool', key: `t-${i}`, at: i, title: `call ${i}`, status: 'completed',
    command: null, input: 'heavy', output: 'heavy', location: null, terminalId: null, id: `id-${i}`, toolKind: 'read',
  }));
  const lean = skeletonEvents(many) as Array<Record<string, unknown>>;
  assert.equal(lean.length, 80);
  assert.equal(lean[0].key, 't-220', 'the kept window is the visible tail');
  assert.equal(lean.at(-1)!.key, 't-299');
});

test('skeleton serialization stays far below the payload cost', () => {
  const mirror = mirrorWith('tab-active');
  const fullBytes = JSON.stringify(mirror).length;
  const localBytes = JSON.stringify(localMirror(mirror)).length;
  assert.ok(localBytes < fullBytes / 5, `skeletons retained payload weight: ${localBytes}/${fullBytes}`);
});
