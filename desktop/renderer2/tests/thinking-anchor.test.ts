import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { createServer, type ViteDevServer } from 'vite';
import type { Chat, Msg } from '../src/store/types.ts';

const styles = readFileSync(new URL('../src/styles/app.css', import.meta.url), 'utf8');

let vite: ViteDevServer;
let Transcript: React.ComponentType<{ chat: Chat | null }>;
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

test('the running pulse is one scrollport-chrome row after the projected transcript', () => {
  const chat = {
    id: 'tab-thinking-anchor',
    chatId: 'chat-thinking-anchor',
    sessionId: 'session-thinking-anchor',
    title: 'Thinking anchor',
    titleLocked: true,
    group: null,
    cwd: null,
    providerId: 'codex',
    currentModelId: null,
    currentModeId: null,
    pending: false,
    draft: '',
    messages: [
      user('ordinary-user', 'ORDINARY-USER-ROW'),
      assistant('assistant-head', 'ASSISTANT-HEAD-ROW', {
        turnRootId: 'assistant-head', turnTerminal: false,
      }),
      user('steer-receipt', 'LIVE-STEER-TRAY-ONLY', {
        turnRootId: 'assistant-head', steerState: 'applied',
      }),
      assistant('assistant-tail', 'ASSISTANT-TAIL-ROW', {
        turnRootId: 'assistant-head', turnTerminal: true, status: 'running', jobId: 'job-thinking-anchor',
        events: [{ key: 'thinking-live', at: 0, kind: 'thinking', text: 'LIVE-THINKING' }],
      }),
    ],
  } as Chat;
  appStore.state.chats = [chat];
  appStore.state.activeId = chat.id;
  appStore.state.meta = { profile: 'test' };

  const html = renderToStaticMarkup(React.createElement(Transcript, { chat }));
  const pulse = html.indexOf('class="thinklive"');
  const lastMessage = html.lastIndexOf('data-chat-find-message=');
  assert.match(html, /class="transcriptviewport has-live"/);
  assert.equal(html.split('class="thinklive"').length - 1, 1);
  assert.ok(lastMessage >= 0 && pulse > lastMessage, 'the pulse follows every projected transcript row');
  assert.doesNotMatch(html, /LIVE-STEER-TRAY-ONLY/);
});

test('the live pulse has a physical bottom anchor independent of document height', () => {
  assert.match(styles, /\.transcriptviewport\s*\{[^}]*position:\s*relative;[^}]*--thinklive-h:\s*40px;/s);
  assert.match(styles, /\.thinklive\s*\{[^}]*position:\s*absolute;[^}]*bottom:\s*0;/s);
  assert.doesNotMatch(styles, /\.thinklive\s*\{[^}]*position:\s*sticky;/s);
  assert.match(styles, /\.transcriptviewport\.has-live \.doc\s*\{[^}]*padding-bottom:\s*calc\(var\(--doc-pad-b\) \+ var\(--thinklive-h\)\);/s);
});
