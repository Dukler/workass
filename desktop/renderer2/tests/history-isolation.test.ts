import test from 'node:test';
import assert from 'node:assert/strict';
import { mergeArchivedHistory, providerHistoryMessages } from '../src/store/history-isolation.ts';
import type { Msg } from '../src/store/types.ts';

test('archive restoration preserves typed final result separately from commentary', () => {
  const merged = mergeArchivedHistory([], [{
    id: 'assistant-1', role: 'assistant', content: 'working notes', result: 'final report',
    status: 'done', at: '2026-07-14T18:00:00Z', events: [],
  }]);
  assert.equal(merged.length, 1);
  assert.equal(merged[0].content, 'working notes');
  assert.equal(merged[0].result, 'final report');
});

test('legacy fingerprint does not merge distinct typed final results', () => {
  const merged = mergeArchivedHistory([], [
    { role: 'assistant', content: 'same notes', result: 'first result', status: 'done', at: null, events: [] },
    { role: 'assistant', content: 'same notes', result: 'second result', status: 'done', at: null, events: [] },
  ]);
  assert.equal(merged.length, 2);
  assert.notEqual(merged[0].id, merged[1].id);
});

test('legacy cancelled sentinel hydrates as the same text-free assistant row', () => {
  const merged = mergeArchivedHistory([], [{
    id: 'cancelled-assistant', role: 'assistant', content: 'Detenido.', status: 'cancelled', at: '2026-07-14T18:05:00Z',
  }]);
  assert.equal(merged.length, 1);
  assert.equal(merged[0].content, '');
  assert.equal(merged[0].status, 'cancelled');
});

test('archive restoration preserves a steered row and its assistant boundary', () => {
  const anchor = { assistantMessageId: 'assistant-1', contentOffset: 12, resultOffset: 3, eventCount: 1 };
  const merged = mergeArchivedHistory([], [{
    id: 'steer-1', role: 'user', content: 'change direction', status: 'done', at: null,
    steerState: 'applied', steerAnchor: anchor,
  }]);
  assert.equal(merged[0].steerState, 'applied');
  assert.deepEqual(merged[0].steerAnchor, anchor);
});

test('a live-only interrupted turn keeps its chronological position across reload', () => {
  const live: Msg[] = [
    { id: 'u-2', role: 'user', content: 'lost prompt', status: 'done', at: '2026-07-13T11:00:00Z', events: [] },
    { id: 'a-2', role: 'assistant', content: 'died with the daemon', status: 'failed', at: '2026-07-13T11:00:30Z', jobId: 'job-lost', events: [] },
    { id: 'u-3', role: 'user', content: 'later prompt', status: 'done', at: '2026-07-14T09:00:00Z', events: [] },
    { id: 'a-3', role: 'assistant', content: 'later answer', status: 'done', at: '2026-07-14T09:00:30Z', events: [] },
  ];
  const archive = [
    { id: 'u-1', role: 'user', content: 'first', status: 'done', at: '2026-07-12T10:00:00Z' },
    { id: 'a-1', role: 'assistant', content: 'first answer', status: 'done', at: '2026-07-12T10:00:30Z' },
    { id: 'u-3', role: 'user', content: 'later prompt', status: 'done', at: '2026-07-14T09:00:00Z' },
    { id: 'a-3', role: 'assistant', content: 'later answer', status: 'done', at: '2026-07-14T09:00:30Z' },
  ];
  const merged = mergeArchivedHistory(live, archive);
  assert.deepEqual(merged.map((message) => message.id), ['u-1', 'a-1', 'u-2', 'a-2', 'u-3', 'a-3']);
});

test('previously demoted stale rows heal back to chronology and stay stable across merges', () => {
  // A prior buggy merge left old never-archived rows at the END of the live
  // sequence (three distinct dead turns). They must return to their timestamp
  // positions as separate turns, and re-merging must not reshuffle again.
  const live: Msg[] = [
    { id: 'u-9', role: 'user', content: 'newest prompt', status: 'done', at: '2026-07-16T04:02:00Z', events: [] },
    { id: 'a-9', role: 'assistant', content: 'newest answer', status: 'done', at: '2026-07-16T04:02:30Z', jobId: 'job-9', events: [] },
    { id: 'a-old-1', role: 'assistant', content: 'dead turn one', status: 'failed', at: '2026-07-12T16:31:00Z', jobId: 'job-old-1', events: [] },
    { id: 'a-old-2', role: 'assistant', content: 'dead turn two', status: 'failed', at: '2026-07-13T01:00:00Z', jobId: 'job-old-2', events: [] },
    { id: 'u-old-3', role: 'user', content: 'orphan prompt', status: 'done', at: '2026-07-13T21:13:00Z', events: [] },
  ];
  const archive = [
    { id: 'u-1', role: 'user', content: 'early', status: 'done', at: '2026-07-12T10:00:00Z' },
    { id: 'a-1', role: 'assistant', content: 'early answer', status: 'done', at: '2026-07-12T10:00:30Z' },
    { id: 'u-5', role: 'user', content: 'mid', status: 'done', at: '2026-07-14T12:00:00Z' },
    { id: 'a-5', role: 'assistant', content: 'mid answer', status: 'done', at: '2026-07-14T12:00:30Z' },
    { id: 'u-9', role: 'user', content: 'newest prompt', status: 'done', at: '2026-07-16T04:02:00Z' },
    { id: 'a-9', role: 'assistant', content: 'newest answer', status: 'done', at: '2026-07-16T04:02:30Z' },
  ];
  const expected = ['u-1', 'a-1', 'a-old-1', 'a-old-2', 'u-old-3', 'u-5', 'a-5', 'u-9', 'a-9'];
  const merged = mergeArchivedHistory(live, archive);
  assert.deepEqual(merged.map((message) => message.id), expected);
  const remerged = mergeArchivedHistory(merged, archive);
  assert.deepEqual(remerged.map((message) => message.id), expected);
});

test('a live-only active steer turn stays one contiguous block in live order', () => {
  const live: Msg[] = [
    { id: 'u-1', role: 'user', content: 'archived prompt', status: 'done', at: '2026-07-16T03:00:00Z', events: [] },
    { id: 'u-2', role: 'user', content: 'active prompt', status: 'done', at: '2026-07-16T03:33:00Z', events: [] },
    { id: 'a-2', role: 'assistant', content: '', status: 'running', at: null, jobId: 'job-2', turnRootId: 'a-2', events: [] },
    { id: 'u-steer', role: 'user', content: 'change direction', status: 'pending', at: '2026-07-16T03:34:00Z', steerState: 'sending', turnRootId: 'a-2', events: [] },
  ];
  const archive = [
    { id: 'u-1', role: 'user', content: 'archived prompt', status: 'done', at: '2026-07-16T03:00:00Z' },
    { id: 'a-1', role: 'assistant', content: 'archived answer', status: 'done', at: '2026-07-16T03:00:30Z' },
  ];
  const merged = mergeArchivedHistory(live, archive);
  assert.deepEqual(merged.map((message) => message.id), ['u-1', 'a-1', 'u-2', 'a-2', 'u-steer']);
});

test('a settled steer stays after its failed assistant despite an earlier timestamp', () => {
  // Disconnect settle: the assistant is stamped `failed` at disconnect time
  // (LATER than the steer's submit time) and settleStagedSteersAtTurnEnd
  // reveals the steer AFTER it. The original assistant carries no turnRootId —
  // only the steer points at it. Timestamp-sorting the steer individually
  // would flip that order; the turn must move as one block.
  const live: Msg[] = [
    { id: 'u-1', role: 'user', content: 'prompt', status: 'done', at: '2026-07-16T03:33:00Z', events: [] },
    { id: 'a-1', role: 'assistant', content: 'partial', status: 'failed', at: '2026-07-16T03:40:00Z', jobId: 'job-1', events: [] },
    { id: 'u-steer', role: 'user', content: 'change direction', status: 'done', at: '2026-07-16T03:34:00Z', steerState: 'uncertain', turnRootId: 'a-1', events: [] },
  ];
  const archive = [
    { id: 'u-0', role: 'user', content: 'earlier', status: 'done', at: '2026-07-16T03:00:00Z' },
    { id: 'a-0', role: 'assistant', content: 'earlier answer', status: 'done', at: '2026-07-16T03:00:30Z' },
    { id: 'u-9', role: 'user', content: 'after restart', status: 'done', at: '2026-07-16T04:00:00Z' },
    { id: 'a-9', role: 'assistant', content: 'after answer', status: 'done', at: '2026-07-16T04:00:30Z' },
  ];
  const merged = mergeArchivedHistory(live, archive);
  assert.deepEqual(merged.map((message) => message.id), ['u-0', 'a-0', 'u-1', 'a-1', 'u-steer', 'u-9', 'a-9']);
});

test('legacy anchored archive order does not flip a migrated steer back before its assistant', () => {
  // The legacy offset-anchored implementation archived the steer at submission
  // time (BEFORE the assistant row); migrateAnchoredSteers reordered the
  // mirror on first load and deleted the anchors. Reloading must keep the
  // migrated order, not resurrect the archive's steer-first order.
  const live: Msg[] = [
    { id: 'u-1', role: 'user', content: 'prompt', status: 'done', at: '2026-07-14T20:20:00Z', events: [] },
    { id: 'a-1', role: 'assistant', content: 'before steer', status: 'done', at: null, turnRootId: 'a-1', turnTerminal: false, events: [] },
    { id: 'u-steer', role: 'user', content: 'change direction', status: 'done', at: '2026-07-14T20:24:00Z', steerState: 'applied', turnRootId: 'a-1', events: [] },
    { id: 'a-1~after~u-steer', role: 'assistant', content: 'after steer', status: 'done', at: '2026-07-14T20:30:00Z', turnRootId: 'a-1', turnTerminal: true, events: [] },
  ];
  const archive = [
    { id: 'u-1', role: 'user', content: 'prompt', status: 'done', at: '2026-07-14T20:20:00Z' },
    {
      id: 'u-steer', role: 'user', content: 'change direction', status: 'done', at: '2026-07-14T20:24:00Z',
      steerState: 'applied', steerAnchor: { assistantMessageId: 'a-1', contentOffset: 12, resultOffset: 0, eventCount: 0 },
    },
    { id: 'a-1', role: 'assistant', content: 'before steerafter steer', status: 'done', at: '2026-07-14T20:30:00Z' },
  ];
  const expected = ['u-1', 'a-1', 'u-steer', 'a-1~after~u-steer'];
  const merged = mergeArchivedHistory(live, archive);
  assert.deepEqual(merged.map((message) => message.id), expected);
  const remerged = mergeArchivedHistory(merged, archive);
  assert.deepEqual(remerged.map((message) => message.id), expected);
});

test('a live-only row without any timestamp stays attached to its live neighbor', () => {
  const live: Msg[] = [
    { id: 'u-2', role: 'user', content: 'second', status: 'done', at: '2026-07-12T12:00:00Z', events: [] },
    { id: 'a-x', role: 'assistant', content: '', status: 'running', at: null, events: [] },
  ];
  const archive = [
    { id: 'u-1', role: 'user', content: 'first', status: 'done', at: '2026-07-12T11:00:00Z' },
    { id: 'u-2', role: 'user', content: 'second', status: 'done', at: '2026-07-12T12:00:00Z' },
    { id: 'u-3', role: 'user', content: 'third', status: 'done', at: '2026-07-12T13:00:00Z' },
  ];
  const merged = mergeArchivedHistory(live, archive);
  assert.deepEqual(merged.map((message) => message.id), ['u-1', 'u-2', 'a-x', 'u-3']);
});

test('provider replay preserves permanent physical steer chronology without reordering', () => {
  const messages: Msg[] = [
    { id: 'base', role: 'user', content: 'base request', status: 'done', at: null, events: [] },
    { id: 'assistant', role: 'assistant', content: 'before', status: 'done', at: null, events: [], turnRootId: 'assistant', turnTerminal: false },
    {
      id: 'steer', role: 'user', content: 'change direction', status: 'done', at: null, events: [], steerState: 'applied',
      turnRootId: 'assistant',
    },
    { id: 'assistant-after-steer', role: 'assistant', content: 'after', status: 'done', at: null, events: [], turnRootId: 'assistant', turnTerminal: true },
  ];
  assert.deepEqual(providerHistoryMessages(messages).map((message) => message.id), ['base', 'assistant', 'steer', 'assistant-after-steer']);
});
