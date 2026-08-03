import assert from 'node:assert/strict';
import test from 'node:test';
import { shouldDrainRecoveredQueue } from '../src/image-drafts.ts';
import { normalizeQueued } from '../src/store/persistence.ts';
import type { QueuedMsg } from '../src/store/types.ts';

test('queued follow-ups survive JSON mirror serialization and hydration', () => {
  const queued = 'first queued follow-up\n\nsecond queued follow-up';
  const saved = { queued: normalizeQueued(queued) };
  const restored = JSON.parse(JSON.stringify(saved)) as { queued?: unknown };
  assert.equal(normalizeQueued(restored.queued), queued);
});

test('empty or invalid queue values hydrate as absent', () => {
  assert.equal(normalizeQueued(''), undefined);
  assert.equal(normalizeQueued(null), undefined);
  assert.equal(normalizeQueued({ text: 'not a string' }), undefined);
});

test('queued attachments stay bound to their message through daemon mirror JSON', () => {
  const queue: QueuedMsg[] = [{
    id: 'q-image',
    text: 'inspect this screenshot',
    images: [{ mimeType: 'image/png', data: 'aGVsbG8=', name: 'screen.png' }],
  }];
  const restored = JSON.parse(JSON.stringify({ queue })) as { queue: QueuedMsg[] };
  assert.deepEqual(restored.queue, queue);
  assert.equal(restored.queue[0].images?.[0].data, 'aGVsbG8=');
});

test('hydration and reconnect recover only an idle renderer-owned queue head', () => {
  assert.equal(shouldDrainRecoveredQueue([{ id: 'local', text: 'continue once' }], false), true);
  assert.equal(shouldDrainRecoveredQueue([{ id: 'local', text: 'continue once' }], true), false);
  assert.equal(shouldDrainRecoveredQueue([{ id: 'agent', text: 'wake once', source: 'agent' }], false), false);
  assert.equal(shouldDrainRecoveredQueue([{ id: 'host', text: 'follow once', source: 'host' }], false), false);
  assert.equal(shouldDrainRecoveredQueue([{ id: 'preparing', text: 'image', attachmentState: 'preparing' }], false), false);
  assert.equal(shouldDrainRecoveredQueue(undefined, false), false);
});
