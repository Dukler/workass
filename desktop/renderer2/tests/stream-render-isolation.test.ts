import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { fileURLToPath } from 'node:url';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { createServer, type ViteDevServer } from 'vite';
import type { Chat, Msg } from '../src/store/types.ts';
import { buildCoalescedTurnBlockTimelineSegments } from '../src/timeline-layout.ts';

let vite: ViteDevServer;
let Transcript: React.ComponentType<{ chat: Chat | null }>;
let appStore: {
  state: Record<string, unknown>;
  bump(topic: string): void;
};

before(async () => {
  vite = await createServer({
    root: fileURLToPath(new URL('..', import.meta.url)),
    logLevel: 'silent',
    server: { middlewareMode: true },
    appType: 'custom',
  });
  ({ Transcript } = await vite.ssrLoadModule('/src/components/Transcript.tsx') as {
    Transcript: React.ComponentType<{ chat: Chat | null }>;
  });
  appStore = (await vite.ssrLoadModule('/src/store/store.ts') as {
    store: { state: Record<string, unknown>; bump(topic: string): void };
  }).store;
});

after(async () => { await vite.close(); });

function user(id: string, content: string, extra: Partial<Msg> = {}): Msg {
  return { id, role: 'user', content, status: 'done', at: null, events: [], ...extra };
}

function assistant(id: string, content: string, extra: Partial<Msg> = {}): Msg {
  return { id, role: 'assistant', content, status: 'done', at: null, events: [], ...extra };
}

function occurrences(haystack: string, needle: string): number {
  return haystack.split(needle).length - 1;
}

function renderTranscript(chat: Chat): string {
  appStore.state.chats = [chat];
  appStore.state.activeId = chat.id;
  appStore.state.meta = { profile: 'test' };
  return renderToStaticMarkup(React.createElement(Transcript, { chat }));
}

test('a live steered block renders each row once, updates only its mutable tail, then relocates its steer', () => {
  const head = assistant('assistant-head', 'SEALED-HEAD-CONTENT', {
    turnRootId: 'assistant-head', turnTerminal: false,
  });
  const steer = user('steer-receipt', 'STEER-RECEIPT-CONTENT', {
    turnRootId: 'assistant-head', steerState: 'applied',
  });
  const tail = assistant('assistant-tail', 'MUTABLE-TAIL-V1', {
    turnRootId: 'assistant-head', turnTerminal: true, status: 'running', jobId: 'job-stream-isolation',
  });
  const chat = {
    id: 'tab-stream-isolation',
    chatId: 'chat-stream-isolation',
    sessionId: 'session-stream-isolation',
    title: 'Stream isolation',
    titleLocked: true,
    group: null,
    cwd: null,
    providerId: 'codex',
    currentModelId: null,
    currentModeId: null,
    pending: false,
    draft: '',
    messages: [
      user('ordinary-prompt', 'ORDINARY-PROMPT-CONTENT'),
      head,
      steer,
      tail,
    ],
  } as Chat;
  const canonicalOrder = chat.messages.map((message) => message.id);

  const initial = renderTranscript(chat);
  assert.equal(occurrences(initial, 'data-chat-find-message="assistant-head"'), 1);
  assert.equal(occurrences(initial, 'data-chat-find-message="assistant-tail"'), 1);
  assert.equal(occurrences(initial, 'SEALED-HEAD-CONTENT'), 1);
  assert.equal(occurrences(initial, 'MUTABLE-TAIL-V1'), 1);
  assert.doesNotMatch(initial, /STEER-RECEIPT-CONTENT/);

  const beforeSegments = buildCoalescedTurnBlockTimelineSegments([head, tail]);
  tail.content = 'MUTABLE-TAIL-V2-LATEST';
  appStore.bump(`msg:${tail.id}`);
  const afterSegments = buildCoalescedTurnBlockTimelineSegments([head, tail]);
  assert.equal(afterSegments[0], beforeSegments[0], 'the sealed head keeps its memo input by reference');
  assert.notEqual(afterSegments[1], beforeSegments[1], 'only the mutable tail receives a new memo input');

  const streamed = renderTranscript(chat);
  assert.equal(occurrences(streamed, 'data-chat-find-message="assistant-head"'), 1);
  assert.equal(occurrences(streamed, 'data-chat-find-message="assistant-tail"'), 1);
  assert.equal(occurrences(streamed, 'SEALED-HEAD-CONTENT'), 1);
  assert.equal(occurrences(streamed, 'MUTABLE-TAIL-V2-LATEST'), 1);
  assert.doesNotMatch(streamed, /MUTABLE-TAIL-V1|STEER-RECEIPT-CONTENT/);

  tail.status = 'done';
  appStore.bump(`msg:${tail.id}`);
  const settled = renderTranscript(chat);
  const headOffset = settled.indexOf('SEALED-HEAD-CONTENT');
  const tailOffset = settled.indexOf('MUTABLE-TAIL-V2-LATEST');
  const steerOffset = settled.indexOf('STEER-RECEIPT-CONTENT');
  assert.ok(headOffset >= 0 && headOffset < tailOffset && tailOffset < steerOffset);
  assert.equal(occurrences(settled, 'SEALED-HEAD-CONTENT'), 1);
  assert.equal(occurrences(settled, 'MUTABLE-TAIL-V2-LATEST'), 1);
  assert.equal(occurrences(settled, 'STEER-RECEIPT-CONTENT'), 1);
  assert.deepEqual(chat.messages.map((message) => message.id), canonicalOrder, 'presentation never mutates actor chronology');
});
