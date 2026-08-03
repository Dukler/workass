import assert from 'node:assert/strict';
import test from 'node:test';
import type { Msg } from '../src/store/types.ts';
import { dedupeMessages, mergeArchivedHistory } from '../src/store/history-isolation.ts';

function msg(id: string, role: 'user' | 'assistant', content: string): Msg {
  return { id, role, content, status: 'done', at: '2026-07-12T12:00:00Z', events: [] };
}

test('duplicate message ids collapse to one live row', () => {
  const first = msg('same-id', 'assistant', 'stale');
  const live = msg('same-id', 'assistant', 'live');
  assert.deepEqual(dedupeMessages([first, live]), [live]);
});

test('archive hydration overlays one chat instead of concatenating histories by count', () => {
  const current = [msg('u-2', 'user', 'second'), msg('a-2', 'assistant', 'current answer')];
  const archive = [
    { id: 'u-1', role: 'user', content: 'first', status: 'done', at: '2026-07-12T11:00:00Z' },
    { id: 'a-1', role: 'assistant', content: 'first answer', status: 'done', at: '2026-07-12T11:00:01Z' },
    { id: 'u-2', role: 'user', content: 'second', status: 'done', at: '2026-07-12T12:00:00Z' },
    { id: 'a-2', role: 'assistant', content: 'archived answer', status: 'done', at: '2026-07-12T12:00:00Z' },
  ];
  const merged = mergeArchivedHistory(current, archive);
  assert.deepEqual(merged.map((message) => message.id), ['u-1', 'a-1', 'u-2', 'a-2']);
  assert.equal(merged[3].content, 'current answer');
});

test('legacy archive records without ids do not duplicate matching live rows', () => {
  const current = [msg('live-a', 'assistant', 'same answer')];
  const archive = [{ role: 'assistant', content: 'same answer', status: 'done', at: '2026-07-12T12:00:00Z' }];
  assert.deepEqual(mergeArchivedHistory(current, archive), current);
});
