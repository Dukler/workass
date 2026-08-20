import assert from 'node:assert/strict';
import test from 'node:test';
import type { DraftImage } from '../src/store/types.ts';
import { afterQueuedAcceptance, appendDraftImages, clipboardImageFiles, createDraftImages, draftImagePayloads, imageBase64, mergeMessageImages, messageImages, messageImageSrc, queuedAttachmentsReady, queuedDraftMessage, queuedJob, queuedMessage, releaseDraftImages, withoutDraftImages } from '../src/image-drafts.ts';
import { localMirror, type Mirror } from '../src/store/persistence.ts';

function image(id: string): DraftImage {
  return { id, name: `${id}.png`, mimeType: 'image/png', data: id, url: `data:image/png;base64,${id}` };
}

test('image drafts remain owned by their chat across switches and later pastes', () => {
  const chats: Record<string, DraftImage[]> = { first: [], second: [] };
  chats.first = appendDraftImages(chats.first, [image('shot-1')]);
  let active = 'second';
  assert.deepEqual(chats[active], []);
  active = 'first';
  chats.first = appendDraftImages(chats.first, [image('shot-2')]);
  assert.deepEqual(chats[active].map((item) => item.id), ['shot-1', 'shot-2']);
  chats.first = withoutDraftImages(chats.first, ['shot-1']);
  assert.deepEqual(chats.first.map((item) => item.id), ['shot-2']);
});

test('clipboard images use files first and item fallback when files is empty', () => {
  const direct = { name: 'direct.png', type: 'image/png' } as File;
  const fallback = { name: 'fallback.png', type: 'image/png' } as File;
  const ignored = { name: 'notes.txt', type: 'text/plain' } as File;
  const item = { kind: 'file', type: 'image/png', getAsFile: () => fallback } as DataTransferItem;
  assert.deepEqual(clipboardImageFiles({ files: [direct, ignored] as unknown as FileList, items: [item] as unknown as DataTransferItemList }), [direct]);
  assert.deepEqual(clipboardImageFiles({ files: [] as unknown as FileList, items: [item] as unknown as DataTransferItemList }), [fallback]);
});

test('job payload strips only the data URL header', () => {
  assert.equal(imageBase64('data:image/png;base64,aGVsbG8='), 'aGVsbG8=');
  assert.equal(imageBase64('already-base64'), 'already-base64');
});

test('large attachment selection creates immediate zero-copy previews and defers reads until send', async () => {
  let reads = 0;
  let yields = 0;
  const files = Array.from({ length: 6 }, (_, index) => ({
    name: `large-${index}.png`, type: 'image/png', size: 24 * 1024 * 1024,
  } as File));
  const started = performance.now();
  const drafts = createDraftImages(files, (file) => `blob:preview-${(file as File).name}`);
  const elapsed = performance.now() - started;
  assert.equal(drafts.length, 6);
  assert.equal(reads, 0);
  assert.ok(elapsed < 25, `preview preparation took ${elapsed.toFixed(3)}ms`);
  assert.equal(drafts[0].data, undefined);
  assert.equal(drafts[0].file, files[0]);

  const payloads = await draftImagePayloads(drafts, async (file) => {
    reads += 1;
    return `data:${file.type};base64,${file.name}`;
  }, async () => { yields += 1; });
  assert.equal(reads, 6);
  assert.equal(yields, 6, 'large batches yield between every encoded file');
  assert.deepEqual(payloads.map((payload) => payload.data), files.map((file) => file.name));
});

test('removing object-url drafts releases only their browser previews', () => {
  const revoked: string[] = [];
  releaseDraftImages([
    { id: 'blob', name: 'blob.png', mimeType: 'image/png', url: 'blob:preview' },
    image('legacy'),
  ], (url) => revoked.push(url));
  assert.deepEqual(revoked, ['blob:preview']);
});

test('accepted images become renderable sent-message images', () => {
  const sent = messageImages([{ mimeType: 'image/png', data: 'aGVsbG8=', name: 'shot.png' }]);
  assert.deepEqual(sent, [{ mimeType: 'image/png', data: 'aGVsbG8=', name: 'shot.png' }]);
  assert.equal(messageImageSrc(sent![0]), 'data:image/png;base64,aGVsbG8=');
  assert.equal(messageImages([{ mimeType: 'text/plain', data: 'nope' }]), undefined);
});

test('live assistant media merges immediately without duplicating its terminal copy', () => {
  const first = [{ mimeType: 'image/png', data: 'c3RhcnRlZA==', name: 'Recording started', source: '/workspace/started.png' }];
  const live = mergeMessageImages(undefined, first);
  assert.deepEqual(live, first);
  assert.deepEqual(mergeMessageImages(live, first), first, 'job:end must not duplicate an image already painted live');
  assert.deepEqual(mergeMessageImages(live, [
    { mimeType: 'image/png', data: 'c3RvcHBlZA==', name: 'Recording stopped', source: '/workspace/stopped.png' },
  ]).map((item) => item.name), ['Recording started', 'Recording stopped']);
});

test('queued attachment payload stays bound until the popped job is accepted', () => {
  const attached = [{ mimeType: 'image/png', data: 'cXVldWVk', name: 'queued.png' }];
  const item = queuedMessage('q1', 'inspect queued image', attached);
  assert.deepEqual(queuedJob(item), { prompt: 'inspect queued image', images: attached });
  const queue = [item, queuedMessage('q2', 'after it')];
  assert.equal(afterQueuedAcceptance(queue, 'q1', false), queue);
  assert.deepEqual(afterQueuedAcceptance(queue, 'q1', true).map((queued) => queued.id), ['q2']);
});

test('queued drafts paint immediately and cannot drain until encoding is ready', () => {
  const draft = image('queued-draft');
  const item = queuedDraftMessage('q-draft', 'inspect it', [draft]);
  assert.equal(queuedAttachmentsReady(item), false);
  assert.equal(item.draftImages?.[0], draft);
  assert.deepEqual(item.attachmentNames, ['queued-draft.png']);
  item.images = [{ mimeType: 'image/png', data: 'ready' }];
  item.attachmentState = 'ready';
  assert.equal(queuedAttachmentsReady(item), true);
});

test('local mirror excludes all actor-owned chat payloads without mutating them', () => {
  const mirror = {
    v: 1, activeId: 'tab', seq: 1, theme: 'dark', density: 'compact', mode: 'chats',
    panes: { side: true, rail: true, railWide: false, sideW: 288, railW: 312, browser: false },
    chats: [{
      id: 'tab', title: 'Chat', titleLocked: true, group: null, cwd: null,
      currentModelId: null, currentModeId: null, draft: '',
      queue: [{ id: 'q1', text: 'queued look', images: [{ mimeType: 'image/png', data: 'queued-large' }] }],
      messages: [{ id: 'u1', role: 'user', content: 'look', status: 'done', at: null, events: [{ kind: 'tool', output: 'large tool output' }], images: [{ mimeType: 'image/png', data: 'large' }] }],
    }],
  } satisfies Mirror;
  const local = localMirror(mirror);
  assert.deepEqual(local.chats, []);
  assert.equal(mirror.chats[0].messages[0].images?.[0].data, 'large');
  assert.deepEqual(mirror.chats[0].messages[0].events, [{ kind: 'tool', output: 'large tool output' }]);
  assert.equal(mirror.chats[0].queue?.[0].images?.[0].data, 'queued-large');
});
