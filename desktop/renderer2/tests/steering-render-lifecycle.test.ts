import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { fileURLToPath } from 'node:url';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { createServer, type ViteDevServer } from 'vite';
import type { Chat, Msg } from '../src/store/types.ts';

let vite: ViteDevServer;
let Transcript: React.ComponentType<{ chat: Chat | null }>;
let QueueList: React.ComponentType<{ chat: Chat }>;
let appStore: { state: Record<string, unknown> };

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
  ({ QueueList } = await vite.ssrLoadModule('/src/components/QueueList.tsx') as {
    QueueList: React.ComponentType<{ chat: Chat }>;
  });
  appStore = (await vite.ssrLoadModule('/src/store/store.ts') as {
    store: { state: Record<string, unknown> };
  }).store;
});

after(async () => { await vite.close(); });

function user(id: string, content: string, extra: Partial<Msg> = {}): Msg {
  return { id, role: 'user', content, status: 'done', at: null, events: [], ...extra };
}

function assistant(id: string, content: string, extra: Partial<Msg> = {}): Msg {
  return { id, role: 'assistant', content, status: 'done', at: null, events: [], ...extra };
}

function chatWithLiveSteer(): Chat {
  return {
    id: 'tab-steering-render',
    chatId: 'chat-steering-render',
    sessionId: 'session-steering-render',
    title: 'Steering render',
    titleLocked: true,
    group: null,
    cwd: null,
    providerId: 'codex',
    currentModelId: null,
    currentModeId: null,
    pending: false,
    draft: '',
    messages: [
      user('prompt', 'INITIAL-PROMPT'),
      assistant('head', 'ASSISTANT-BEFORE-DIRECTION', { turnRootId: 'head', turnTerminal: false }),
      user('steer', 'STEER-DIRECTION-TEXT', { turnRootId: 'head', steerState: 'applied' }),
      assistant('tail', 'ASSISTANT-AFTER-DIRECTION', {
        turnRootId: 'head', turnTerminal: true, status: 'running', jobId: 'job-steering-render',
      }),
    ],
  } as Chat;
}

function renderTranscript(chat: Chat): string {
  appStore.state.chats = [chat];
  appStore.state.activeId = chat.id;
  appStore.state.meta = { profile: 'test' };
  return renderToStaticMarkup(React.createElement(Transcript, { chat }));
}

function renderQueue(chat: Chat): string {
  return renderToStaticMarkup(React.createElement(QueueList, { chat }));
}

function occurrences(haystack: string, needle: string): number {
  return haystack.split(needle).length - 1;
}

test('actual transcript and composer tray transfer one steer owner at the terminal boundary', () => {
  const chat = chatWithLiveSteer();
  const canonicalOrder = chat.messages.map((message) => message.id);

  const liveTranscript = renderTranscript(chat);
  const liveTray = renderQueue(chat);
  assert.doesNotMatch(liveTranscript, /STEER-DIRECTION-TEXT/);
  assert.equal(occurrences(liveTray, 'STEER-DIRECTION-TEXT'), 1);
  assert.match(liveTray, /Steering · 1/);
  assert.ok(
    liveTranscript.indexOf('ASSISTANT-BEFORE-DIRECTION') < liveTranscript.indexOf('ASSISTANT-AFTER-DIRECTION'),
    'live assistant slices retain their canonical order',
  );

  chat.messages.at(-1)!.status = 'done';
  const settledTranscript = renderTranscript(chat);
  const settledTray = renderQueue(chat);
  const before = settledTranscript.indexOf('ASSISTANT-BEFORE-DIRECTION');
  const after = settledTranscript.indexOf('ASSISTANT-AFTER-DIRECTION');
  const steer = settledTranscript.indexOf('STEER-DIRECTION-TEXT');
  assert.ok(before >= 0 && before < after && after < steer, 'settled steer renders after every assistant slice');
  assert.doesNotMatch(settledTray, /STEER-DIRECTION-TEXT|Steering · 1/);
  assert.deepEqual(chat.messages.map((message) => message.id), canonicalOrder, 'rendering never rewrites actor chronology');

  const reloaded = JSON.parse(JSON.stringify(chat)) as Chat;
  const reloadedTranscript = renderTranscript(reloaded);
  assert.ok(
    reloadedTranscript.indexOf('ASSISTANT-AFTER-DIRECTION') < reloadedTranscript.indexOf('STEER-DIRECTION-TEXT'),
    'the same terminal presentation survives a JSON hydration boundary',
  );
  assert.deepEqual(reloaded.messages.map((message) => message.id), canonicalOrder);
});
