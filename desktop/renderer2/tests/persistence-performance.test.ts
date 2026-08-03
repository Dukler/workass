import assert from 'node:assert/strict';
import { performance } from 'node:perf_hooks';
import test from 'node:test';
import { DurableImagePayloads, LEAN_SESSION_SAVE_MODE, leanSessionEvents, localMirror, type Mirror } from '../src/store/persistence.ts';

function largeEventMirror(): Mirror {
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

test('first-paint persistence cost is independent of historical tool payload size', () => {
  const mirror = largeEventMirror();
  const fullBytes = JSON.stringify(mirror).length;
  const samples: number[] = [];
  let localBytes = 0;
  for (let index = 0; index < 20; index++) {
    const started = performance.now();
    const local = localMirror(mirror);
    localBytes = JSON.stringify(local).length;
    samples.push(performance.now() - started);
  }
  samples.sort((a, b) => a - b);
  const p95 = samples[Math.floor(samples.length * 0.95)];
  console.log(`local persistence fullBytes=${fullBytes} localBytes=${localBytes} p95=${p95.toFixed(3)}ms max=${samples.at(-1)!.toFixed(3)}ms`);

  assert.ok(fullBytes > 5_000_000, `fixture was only ${fullBytes} bytes`);
  assert.ok(localBytes < fullBytes / 20, `local mirror retained heavy events: ${localBytes}/${fullBytes}`);
  assert.equal(mirror.chats[0].messages[0].events.length, 20, 'compaction mutated the authoritative mirror');
  assert.ok(p95 < 8, `p95 local persistence preparation ${p95.toFixed(3)}ms exceeded 8ms`);
});

test('lean daemon event overlay keeps local metadata and drops heavy tool payloads', () => {
	assert.equal(LEAN_SESSION_SAVE_MODE, 'lean-payload-v2');
	const events = [
    { kind: 'thinking', key: 'thinking', text: 'private chain' },
    {
      kind: 'tool', key: 'tool-key', at: 4, id: 'tool-id', title: 'Read', status: 'completed',
      input: 'large input', output: 'large output', startedAt: 10, endedAt: 20, subagentModel: 'gpt-subagent',
    },
    { kind: 'compaction', key: 'compact', at: 5 },
    { kind: 'restored', key: 'restored', at: 6, turnSeq: 2 },
  ];
  const lean = leanSessionEvents(events) as Array<Record<string, unknown>>;
  assert.deepEqual(lean, [
    { kind: 'tool', key: 'tool-key', at: 4, id: 'tool-id', startedAt: 10, endedAt: 20, subagentModel: 'gpt-subagent' },
    events[2],
    events[3],
  ]);
  assert.equal('output' in lean[0], false);
  assert.equal('input' in lean[0], false);

	const marked = {
		...largeEventMirror(),
		_workassSave: LEAN_SESSION_SAVE_MODE,
		_workassDeletedChatIds: ['chat'],
	} satisfies Mirror;
	assert.equal(localMirror(marked)._workassSave, undefined, 'transient server marker must not enter localStorage');
	assert.equal(localMirror(marked)._workassDeletedChatIds, undefined, 'transient delete marker must not enter localStorage');
});

test('queued image payload crosses the lean save boundary once and recovers from the daemon snapshot', () => {
  const imageData = 'q'.repeat(4 * 1024 * 1024);
  const source: Mirror = {
    v: 1,
    activeId: 'tab-image',
    seq: 1,
    theme: 'dark',
    density: 'compact',
    mode: 'chats',
    panes: { side: true, railWide: false, sideW: 288, railW: 312 },
    chats: [{
      id: 'tab-image',
      chatId: 'chat-image',
      title: 'Image queue',
      titleLocked: true,
      group: null,
      cwd: null,
      currentModelId: null,
      currentModeId: null,
      draft: '',
      queue: [{
        id: 'queued-image',
        text: 'inspect the screenshot',
        attachmentState: 'ready',
        images: [{ mimeType: 'image/png', name: 'queued.png', data: imageData }],
      }],
      messages: [],
    }],
  };

  const durability = new DurableImagePayloads();
  const firstSave = durability.omitAcknowledged(source);
  const firstSerialized = JSON.stringify(firstSave);
  const firstBytes = firstSerialized.length;
  assert.equal(firstSave.chats[0].queue?.[0].images?.[0].data.length, imageData.length,
    'the first save must carry the only durable copy');

  durability.acknowledge(firstSave, false);
  const failedRetry = durability.omitAcknowledged(source);
  assert.equal(failedRetry.chats[0].queue?.[0].images?.[0].data.length, imageData.length,
    'a rejected/failed save must not suppress the retry payload');

  durability.acknowledge(firstSave, true);
  const laterSave = durability.omitAcknowledged(source);
  const laterBytes = JSON.stringify(laterSave).length;
  assert.equal(laterSave.chats[0].queue?.[0].images, undefined,
    'only a successful daemon acknowledgement makes the payload lean');
  assert.equal(source.chats[0].queue?.[0].images?.[0].data.length, imageData.length,
    'lean projection must not mutate the renderer-owned recovery copy');
  assert.ok(laterBytes < firstBytes / 100,
    `acknowledged queue payload was still serialized: ${laterBytes}/${firstBytes}`);

  durability.clear();
  assert.equal(durability.omitAcknowledged(source).chats[0].queue?.[0].images?.[0].data.length, imageData.length,
    'a reconnect without a daemon snapshot must resend the renderer-held recovery copy');
  durability.acknowledge(firstSave, true);

  // Replacing an attachment under the same queue id is a new payload and must
  // receive its own first durable save; tracking row ids alone would lose it.
  source.chats[0].queue![0].images = [{ mimeType: 'image/png', name: 'replacement.png', data: 'replacement' }];
  assert.equal(durability.omitAcknowledged(source).chats[0].queue?.[0].images?.[0].data, 'replacement');

  // Simulate process/reload recovery: session:get returns the durable first
  // snapshot with new object identities. The tracker learns those exact bytes,
  // while localStorage remains lightweight and non-dispatchable until hydrate.
  const recovered = JSON.parse(firstSerialized) as Mirror;
  const recoveredDurability = new DurableImagePayloads();
  recoveredDurability.replaceFromServer(recovered);
  assert.equal(recoveredDurability.omitAcknowledged(recovered).chats[0].queue?.[0].images, undefined);
  const local = localMirror(recovered);
  assert.equal(local.chats[0].queue?.[0].images, undefined);
  assert.equal(local.chats[0].queue?.[0].attachmentState, 'preparing');
  assert.equal(recovered.chats[0].queue?.[0].images?.[0].data.length, imageData.length);

  console.log(`queued image persistence firstBytes=${firstBytes} acknowledgedBytes=${laterBytes}`);
});

test('image durability hydration tolerates a legacy chat with no messages array', () => {
  const snapshot = largeEventMirror() as unknown as {
    chats: Array<Record<string, unknown>>;
  };
  delete snapshot.chats[1].messages;

  const durability = new DurableImagePayloads();
  assert.doesNotThrow(() => durability.replaceFromServer(snapshot as unknown as Mirror));
  assert.doesNotThrow(() => durability.omitAcknowledged(snapshot as unknown as Mirror));
  assert.doesNotThrow(() => localMirror(snapshot as unknown as Mirror));
});
