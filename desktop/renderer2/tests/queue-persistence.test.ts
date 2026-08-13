import assert from 'node:assert/strict';
import test from 'node:test';
import { shouldDrainRecoveredQueue } from '../src/image-drafts.ts';
import type { QueuedMsg } from '../src/store/types.ts';

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
