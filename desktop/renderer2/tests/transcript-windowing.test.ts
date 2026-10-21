import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { fileURLToPath } from 'node:url';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { createServer, type ViteDevServer } from 'vite';
import {
  recallTranscriptView, recordUnitHeights, rememberTranscriptView, transcriptAnchorAt, transcriptWindow, transcriptWindowStale,
  forgetTranscriptViews, newMessagesSince, TRANSCRIPT_ESTIMATED_UNIT_PX, TRANSCRIPT_VIEW_MEMORY,
} from '../src/transcript-window.ts';
import { CHAT_MESSAGE_TAIL, releaseInactiveHistories } from '../src/chat/residency.ts';
import type { Chat, Msg } from '../src/store/types.ts';

// A 3,000-message chat kept every revealed row mounted: 177,790 DOM nodes,
// 258 MB of heap and a long task on every switch (seeded measurement,
// 2026-10-21). The transcript now mounts only the units around the reader.

const keys = (count: number) => Array.from({ length: count }, (_, index) => `m${index}`);
const uniform = (count: number, px: number) => new Map(keys(count).map((key) => [key, px]));

test('a pinned transcript mounts the newest units that cover the viewport and overscan', () => {
  const win = transcriptWindow({ keys: keys(3000), heights: uniform(3000, 100), viewport: 800, overscan: 1600, anchor: null });
  assert.equal(win.to, 3000);
  assert.equal(win.from, 3000 - 24);
  assert.equal(win.topPad, (3000 - 24) * 100, 'the spacer stands in for every unmounted row');
  assert.equal(win.bottomPad, 0);
});

test('a reader in the middle mounts the anchor with overscan on both sides', () => {
  const win = transcriptWindow({ keys: keys(3000), heights: uniform(3000, 100), viewport: 800, overscan: 400, anchor: { key: 'm1000', offset: -30 } });
  assert.equal(win.from, 996);
  assert.equal(win.to, 1013, 'below covers the viewport from the anchor top plus overscan');
  assert.equal(win.topPad, 996 * 100);
  assert.equal(win.bottomPad, (3000 - 1013) * 100);
});

test('prepending older history keeps the anchored row mounted at the same place in the window', () => {
  const before = transcriptWindow({ keys: keys(100).slice(40), heights: uniform(100, 100), viewport: 800, overscan: 400, anchor: { key: 'm42', offset: 0 } });
  const after = transcriptWindow({ keys: keys(100), heights: uniform(100, 100), viewport: 800, overscan: 400, anchor: { key: 'm42', offset: 0 } });
  assert.equal(before.from, 0, 'only two rows were above the anchor before the page arrived');
  assert.equal(after.from, 38, 'the new rows above the anchor are spacer, not a jump');
  assert.equal(after.topPad, 38 * 100);
  assert.equal(after.to - after.from, before.to - before.from + 2);
});

test('a missing anchor falls back to the newest rows and unknown heights use the estimate', () => {
  const win = transcriptWindow({ keys: keys(50), heights: new Map(), viewport: 800, overscan: 0, anchor: { key: 'gone', offset: 0 } });
  assert.equal(win.to, 50);
  assert.equal(win.from, 50 - Math.ceil(800 / TRANSCRIPT_ESTIMATED_UNIT_PX));
});

test('heights are pitches, so margins between rows are counted once', () => {
  const heights = new Map<string, number>();
  recordUnitHeights(heights, [
    { key: 'a', top: 0, bottom: 80 },
    { key: 'b', top: 100, bottom: 150 },
    { key: 'c', top: 170, bottom: 300 },
  ]);
  assert.deepEqual([...heights], [['a', 100], ['b', 70], ['c', 150]], 'the last row adds the median gap seen above it');
});

test('the anchor is the first row still visible at the top edge', () => {
  const units = [{ key: 'a', top: -500, bottom: -20 }, { key: 'b', top: 0, bottom: 300 }, { key: 'c', top: 320, bottom: 900 }];
  assert.deepEqual(transcriptAnchorAt(units, 10), { key: 'b', offset: -10 });
  assert.deepEqual(transcriptAnchorAt(units, -100), { key: 'a', offset: -400 });
});

test('the window is cut again only near an edge or when it grew far past the viewport', () => {
  const input = { keys: keys(3000), heights: uniform(3000, 100), viewport: 800, overscan: 1600, anchor: { key: 'm1500', offset: 0 } };
  const cut = transcriptWindow(input);
  assert.equal(transcriptWindowStale(cut, input), false);
  assert.equal(transcriptWindowStale(cut, { ...input, anchor: { key: 'm1505', offset: 0 } }), false, 'a short scroll keeps the mounted rows');
  assert.equal(transcriptWindowStale(cut, { ...input, anchor: { key: 'm1530', offset: 0 } }), true, 'nearing the bottom edge re-cuts');
  assert.equal(transcriptWindowStale({ from: 0, to: 3000 }, input), true, 'everything mounted is cut back down');
});

test('older history loading above the reader is not counted as new messages', () => {
  const rows = (ids: string[]) => ids.map((id) => ({ id }));
  assert.equal(newMessagesSince(rows(['m5', 'm6', 'm7']), 'm7'), 0);
  assert.equal(newMessagesSince(rows(['m1', 'm2', 'm3', 'm4', 'm5', 'm6', 'm7']), 'm7'), 0, 'a prepended page');
  assert.equal(newMessagesSince(rows(['m1', 'm2', 'm7', 'm8', 'm9']), 'm7'), 2, 'rows appended after the seen one');
  assert.equal(newMessagesSince(rows(['m8', 'm9']), 'gone'), 0);
});

test('a chat remembers how it was left, within a bounded memory', () => {
  forgetTranscriptViews();
  assert.deepEqual({ ...recallTranscriptView('never'), heights: null }, { pinned: true, anchor: null, heights: null });
  const heights = new Map([['m1', 42]]);
  rememberTranscriptView('reading', { pinned: false, anchor: { key: 'm1', offset: -12 }, heights });
  assert.deepEqual(recallTranscriptView('reading').anchor, { key: 'm1', offset: -12 });
  assert.equal(recallTranscriptView('reading').heights, heights);
  for (let index = 0; index < TRANSCRIPT_VIEW_MEMORY; index++) rememberTranscriptView(`other-${index}`, { pinned: true, anchor: null, heights: new Map() });
  assert.equal(recallTranscriptView('reading').pinned, true, 'the oldest view is forgotten');
});

test('recently read chats keep their loaded history; others return to the bounded tail', () => {
  const chat = (id: string, count: number) => ({
    id, messages: Array.from({ length: count }, (_, index) => ({ id: `${id}-${index}`, role: 'user', content: '', status: 'done', at: null, events: [] })),
  }) as unknown as Chat;
  const chats = [chat('active', 500), chat('recent', 500), chat('cold', 500)];
  const released = releaseInactiveHistories(chats, 'active', new Set(['recent']));
  assert.deepEqual(released, ['cold']);
  assert.equal(chats[1].messages.length, 500);
  assert.equal(chats[2].messages.length, CHAT_MESSAGE_TAIL);
});

let vite: ViteDevServer;
let Transcript: React.ComponentType<{ chat: Chat }>;
let appStore: { state: { chats: Chat[]; activeId: string | null }; clearToastTimers(): void };

before(async () => {
  vite = await createServer({ root: fileURLToPath(new URL('..', import.meta.url)), logLevel: 'silent', server: { middlewareMode: true }, appType: 'custom' });
  ({ Transcript } = await vite.ssrLoadModule('/src/components/Transcript.tsx') as { Transcript: typeof Transcript });
  appStore = (await vite.ssrLoadModule('/src/store/store.ts') as { store: typeof appStore }).store;
});
after(async () => { appStore.clearToastTimers(); await vite.close(); });

test('a 3,000-message transcript mounts a bounded window ending at the newest row', () => {
  const messages: Msg[] = [];
  for (let index = 0; index < 1500; index++) {
    messages.push({ id: `u${index}`, role: 'user', content: `question ${index}`, status: 'done', at: null, events: [] } as unknown as Msg);
    messages.push({ id: `a${index}`, role: 'assistant', content: `answer ${index}`, status: 'done', at: null, events: [] } as unknown as Msg);
  }
  const chat = {
    id: 'tab-huge', chatId: 'chat-huge', sessionId: null, title: 'Huge', titleLocked: true, group: null, cwd: '/repo',
    providerId: 'mock', currentModelId: null, currentModeId: null, pending: false, draft: '', messages,
    messageCount: 3000, historyComplete: true,
  } as unknown as Chat;
  appStore.state.chats = [chat];
  appStore.state.activeId = chat.id;
  const html = renderToStaticMarkup(React.createElement(Transcript, { chat }));
  const mounted = (html.match(/data-vunit=/g) ?? []).length;
  assert.ok(mounted > 0 && mounted <= 40, `mounted ${mounted} of 3000 units`);
  assert.match(html, /data-chat-find-message="a1499"/, 'the newest row is mounted');
  assert.doesNotMatch(html, /data-chat-find-message="u0"/, 'the oldest row is a spacer');
  assert.match(html, /class="vpad" style="height:\d+px"/);
});
