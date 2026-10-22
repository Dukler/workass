import assert from 'node:assert/strict';
import test from 'node:test';
import { cachedToolOutput, cachedToolTitle, forgetToolOutputs, loadToolOutput, loadToolTitle } from '../src/chat/tool-detail.ts';
import { actorMessages, HISTORY_READ_OPTIONS } from '../src/chat/history.ts';
import type { MirrorMsg } from '../src/store/persistence.ts';
import type { ToolEvent } from '../src/store/types.ts';

// Tool payloads were 95% of a large chat's history bytes; history reads now ask
// for lean tools, and an opened row reads its whole output in bounded chunks.

function withApi<T>(api: Record<string, unknown>, run: () => Promise<T>): Promise<T> {
  const previous = (globalThis as { window?: unknown }).window;
  (globalThis as { window?: unknown }).window = { api };
  return run().finally(() => {
    if (previous === undefined) delete (globalThis as { window?: unknown }).window;
    else (globalThis as { window?: unknown }).window = previous;
  });
}

const deferred = (key: string): ToolEvent => ({
  key, at: 0, kind: 'tool', id: key, toolKind: 'execute', title: 'go test', status: 'completed', command: 'go test',
  terminalId: null, input: null, output: 'head\n', location: null, outputDeferred: true, outputBytes: 12,
  detail: { tabId: 'tab-tools', messageId: 'reply-1' },
});

test('every history read asks for image references and lean tool payloads', () => {
  assert.deepEqual(HISTORY_READ_OPTIONS, { deferImages: true, leanTools: true, leanTitles: true });
});

test('hydration stamps each deferred payload with the row that owns it', () => {
  const [message] = actorMessages([{
    id: 'reply-1', role: 'assistant', content: 'done', status: 'done', at: null,
    events: [
      { key: 'big', kind: 'tool', output: 'head', outputDeferred: true },
      { key: 'small', kind: 'tool', output: 'whole' },
    ],
  }] as unknown as MirrorMsg[], 'tab-tools');
  const [big, small] = message.events as ToolEvent[];
  assert.deepEqual(big.detail, { tabId: 'tab-tools', messageId: 'reply-1' });
  assert.equal(small.detail, undefined);
});

test('an opened row reads the whole output once, in order, and keeps it', async () => {
  forgetToolOutputs();
  const calls: unknown[] = [];
  const chunks: Record<number, unknown> = {
    0: { text: 'head\nmid', offset: 0, next: 8, total: 12 },
    8: { text: 'dle\n', offset: 8, next: 12, total: 12 },
  };
  await withApi({
    archiveLoad: async (tabId: string, options: { toolDetail: { offset: number } }) => {
      calls.push({ tabId, options });
      return chunks[options.toolDetail.offset];
    },
  }, async () => {
    const tool = deferred('call-1');
    assert.equal(cachedToolOutput(tool), null);
    const [first, second] = await Promise.all([loadToolOutput(tool), loadToolOutput(tool)]);
    assert.equal(first, 'head\nmiddle\n');
    assert.equal(second, first);
    assert.equal(cachedToolOutput({ ...tool, output: 'another lean head' }), first, 'a refreshed row object still finds it');
    assert.equal(await loadToolOutput(tool), first);
  });
  assert.deepEqual(calls, [
    { tabId: 'tab-tools', options: { tail: 1, toolDetail: { messageId: 'reply-1', key: 'call-1', field: 'output', offset: 0 } } },
    { tabId: 'tab-tools', options: { tail: 1, toolDetail: { messageId: 'reply-1', key: 'call-1', field: 'output', offset: 8 } } },
  ], 'concurrent and repeated opens read each chunk once');
});

test('a reply that is not a detail chunk leaves the head in place', async () => {
  forgetToolOutputs();
  await withApi({ archiveLoad: async () => [{ id: 'row' }] }, async () => {
    assert.equal(await loadToolOutput(deferred('call-2')), null, 'an older daemon answers tail:1 with rows');
  });
  await withApi({ archiveLoad: async () => ({ text: 'x', offset: 0, next: 0, total: 5 }) }, async () => {
    assert.equal(await loadToolOutput(deferred('call-3')), null, 'a chunk that does not advance is refused');
  });
  assert.equal(await loadToolOutput({ ...deferred('call-4'), outputDeferred: false }), null, 'a whole output is never fetched');
});

test('a long title arrives as its head and an opened row reads it whole, apart from the output', async () => {
  forgetToolOutputs();
  const calls: Array<{ field: string; offset: number }> = [];
  await withApi({
    archiveLoad: async (_tabId: string, options: { toolDetail: { field: string; offset: number } }) => {
      calls.push({ field: options.toolDetail.field, offset: options.toolDetail.offset });
      return options.toolDetail.field === 'title'
        ? { text: 'cat <<EOF\nwhole title\nEOF', offset: 0, next: 24, total: 24 }
        : { text: 'whole output', offset: 0, next: 12, total: 12 };
    },
  }, async () => {
    const tool: ToolEvent = { ...deferred('call-5'), title: 'cat <<EOF\n', titleDeferred: true, titleBytes: 24 };
    assert.equal(cachedToolTitle(tool), null);
    assert.equal(await loadToolTitle(tool), 'cat <<EOF\nwhole title\nEOF');
    assert.equal(cachedToolTitle(tool), 'cat <<EOF\nwhole title\nEOF');
    assert.equal(await loadToolOutput(tool), 'whole output', 'the output is its own read');
    assert.equal(await loadToolTitle({ ...tool, titleDeferred: false }), null, 'a whole title is never fetched');
  });
  assert.deepEqual(calls, [{ field: 'title', offset: 0 }, { field: 'output', offset: 0 }]);
  const [message] = actorMessages([{
    id: 'reply-2', role: 'assistant', content: '', status: 'done', at: null,
    events: [{ key: 'long', kind: 'tool', title: 'head', titleDeferred: true }],
  }] as unknown as MirrorMsg[], 'tab-tools');
  assert.deepEqual((message.events[0] as ToolEvent).detail, { tabId: 'tab-tools', messageId: 'reply-2' });
});
