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
let Composer: React.ComponentType<{ chat: Chat | null }>;
let appStore: { state: Record<string, unknown>; clearToastTimers(): void; applyAcp(msg: Msg, ev: unknown): void };
let mergeLiveTimelineEvents: (authoritative: Msg['events'], live: Msg['events'], includePlan: boolean) => Msg['events'];

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
  ({ Composer } = await vite.ssrLoadModule('/src/components/Composer.tsx') as {
    Composer: React.ComponentType<{ chat: Chat | null }>;
  });
  ({ store: appStore, mergeLiveTimelineEvents } = await vite.ssrLoadModule('/src/store/store.ts') as {
    store: typeof appStore;
    mergeLiveTimelineEvents: typeof mergeLiveTimelineEvents;
  });
});

after(async () => { appStore.clearToastTimers(); await vite.close(); });

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
      user('steer-receipt', 'LIVE-STEER-TRANSCRIPT', {
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
  const pulse = html.indexOf('class="thinklive');
  const lastMessage = html.lastIndexOf('data-chat-find-message=');
  assert.match(html, /class="transcriptviewport has-live"/);
  assert.equal(html.split('class="thinklive').length - 1, 1);
  assert.ok(lastMessage >= 0 && pulse > lastMessage, 'the pulse follows every projected transcript row');
  assert.equal(html.split('LIVE-STEER-TRANSCRIPT').length - 1, 1);
});

test('the live pulse has a physical bottom anchor independent of document height', () => {
  assert.match(styles, /\.transcriptviewport\s*\{[^}]*position:\s*relative;[^}]*--thinklive-h:\s*58px;/s);
  assert.match(styles, /\.thinklive\s*\{[^}]*position:\s*absolute;[^}]*bottom:\s*0;/s);
  assert.doesNotMatch(styles, /\.thinklive\s*\{[^}]*position:\s*sticky;/s);
  assert.match(styles, /\.transcriptviewport\.has-live \.doc\s*\{[^}]*padding-bottom:\s*calc\(var\(--doc-pad-b\) \+ var\(--thinklive-h\)\);/s);
});

test('a requested Stop replaces live work chrome immediately without claiming completion', () => {
  const chat = {
    id: 'tab-stop-feedback', chatId: 'chat-stop-feedback', sessionId: 'session-stop-feedback',
    title: 'Stop feedback', titleLocked: true, group: null, cwd: null,
    providerId: 'codex', currentModelId: null, currentModeId: null,
    pending: false, draft: '',
    messages: [assistant('assistant-stop-feedback', '', {
      status: 'running', jobId: 'job-stop-feedback', stopState: 'requesting',
      events: [{ key: 'thinking-before-stop', at: 0, kind: 'thinking', text: 'OLD-LIVE-THINKING' }],
    })],
  } as Chat;
  appStore.state.chats = [chat];
  appStore.state.activeId = chat.id;
  appStore.state.connection = 'connected';

  const transcript = renderToStaticMarkup(React.createElement(Transcript, { chat }));
  const composer = renderToStaticMarkup(React.createElement(Composer, { chat }));
  assert.match(transcript, /Deteniendo…/);
  assert.doesNotMatch(transcript, /detenido|OLD-LIVE-THINKING/);
  assert.match(composer, /class="send stop stopping/);
  assert.match(composer, /class="stopspin"/);
  assert.match(composer, /disabled=""/);
});

// Approved mock working-indicator v5 · option 3 (2026-10-12): "Working" with
// the call running now, the latest thought kept above it (thinking-v6 · B), and
// the turn's tool rows kept out of the transcript while it runs (they open
// from the Working line).
test('a running turn shows Working with the current call and keeps its tool rows inside it', () => {
  const now = Date.now();
  const chat = {
    id: 'tab-working-line', chatId: 'chat-working-line', sessionId: 'session-working-line',
    title: 'Working line', titleLocked: true, group: null, cwd: '/repo',
    providerId: 'claude', currentModelId: null, currentModeId: null,
    pending: false, draft: '',
    messages: [
      user('working-user', 'do the thing'),
      assistant('working-assistant', 'PROSE-BEFORE-TOOLS', {
        status: 'running', jobId: 'job-working-line', turnStartedAt: now - 72_000,
        events: [
          { key: 'thinking-headline', at: 0, kind: 'thinking', text: 'Clarifying execution issues' },
          { key: 'tool-done', at: 18, kind: 'tool', id: 't1', toolKind: 'read', title: 'Read', status: 'completed',
            command: null, terminalId: null, input: null, output: null, location: '/repo/tools/silver_review.py',
            startedAt: now - 60_000, endedAt: now - 59_800 },
          { key: 'tool-live', at: 18, kind: 'tool', id: 't2', toolKind: 'execute', title: 'Bash', status: 'in_progress',
            command: 'python render_review_panel.py --silver', terminalId: null, input: null, output: null, location: null,
            startedAt: now - 14_000 },
        ],
      }),
    ],
  } as Chat;
  appStore.state.chats = [chat];
  appStore.state.activeId = chat.id;

  const html = renderToStaticMarkup(React.createElement(Transcript, { chat }));
  assert.match(html, /class="thinklive working"/);
  assert.match(html, /Working<span class="workdots"/);
  assert.match(html, /2 tools/);
  assert.match(html, /class="worknow mono"[^>]*>.*python render_review_panel\.py --silver/);
  assert.match(html, /class="workthought"[^>]*>.*Clarifying execution issues.*<button type="button" class="workhead"/, 'the latest thought stays above the line');
  assert.doesNotMatch(html, /silver_review\.py/, 'finished tool rows wait inside the Working line, not in the transcript');
  assert.match(html, /PROSE-BEFORE-TOOLS/);
});

// Once settled, the turn's steps fold into one closed "Worked" row at its head:
// no "Pensó" line and no tool rows interleave with the answer. Subagent task
// rows and images a tool returned stay visible.
test('a settled turn folds its thinking and tool calls into one Worked row', () => {
  const now = Date.now();
  const chat = {
    id: 'tab-worked-row', chatId: 'chat-worked-row', sessionId: 'session-worked-row',
    title: 'Worked row', titleLocked: true, group: null, cwd: '/repo',
    providerId: 'claude', currentModelId: null, currentModeId: null,
    pending: false, draft: '',
    messages: [
      user('worked-user', 'do the thing'),
      assistant('worked-assistant', 'PROSE-OPENING. PROSE-ANSWER', {
        status: 'done', at: new Date(now).toISOString(),
        events: [
          { key: 'thinking-settled', at: 0, kind: 'thinking', text: 'Clarifying execution issues' },
          { key: 'tool-read', at: 14, kind: 'tool', id: 't1', toolKind: 'read', title: 'Read', status: 'completed',
            command: null, terminalId: null, input: null, output: null, location: '/repo/tools/silver_review.py',
            startedAt: now - 30_000, endedAt: now - 29_800 },
          { key: 'tool-run', at: 14, kind: 'tool', id: 't2', toolKind: 'execute', title: 'Bash', status: 'completed',
            command: 'python render_review_panel.py --silver', terminalId: null, input: null, output: null, location: null,
            startedAt: now - 29_000, endedAt: now - 6_000 },
          { key: 'tool-task', at: 14, kind: 'tool', id: 't3', toolKind: 'agent', title: 'SUBAGENT-TASK-ROW', status: 'completed',
            command: null, terminalId: null, input: null, output: null, location: null,
            subagentId: 't3', subagentHeader: true, startedAt: now - 20_000, endedAt: now - 10_000 },
        ],
      }),
    ],
  } as Chat;
  appStore.state.chats = [chat];
  appStore.state.activeId = chat.id;

  const html = renderToStaticMarkup(React.createElement(Transcript, { chat }));
  assert.doesNotMatch(html, /class="thinklive/);
  assert.match(html, /class="workfold"><button type="button" class="workhead" aria-expanded="false"><span class="worklabel">Worked<\/span><span class="workmeta">24s · 3 tools<\/span>/);
  assert.ok(html.indexOf('class="workfold"') < html.indexOf('PROSE-OPENING'), 'the Worked row heads the turn');
  assert.doesNotMatch(html, />Pensó</, 'thinking settles inside the Worked row, not the transcript');
  assert.doesNotMatch(html, /silver_review\.py|render_review_panel/, 'tool rows wait inside the closed Worked row');
  assert.match(html, /SUBAGENT-TASK-ROW/, 'subagent task rows stay where they landed');
});

function thoughtTexts(msg: Msg): string[] {
  return msg.events.filter((event) => event.kind === 'thinking').map((event) => (event as { text: string }).text);
}

// One thinking event per thought, where it happened: windows back to back
// extend the thought in progress, a call in between starts the next one, and
// a replayed window is dropped wherever it landed (daemon upsertThinkingTimeline).
test('thinking windows split into thoughts around the calls between them', () => {
  const msg = assistant('split-thoughts', '', { status: 'running' });
  appStore.applyAcp(msg, { kind: 'thinking', text: 'Locating the review tooling.' });
  appStore.applyAcp(msg, { kind: 'thinking', text: 'It should live under tools/.' });
  msg.events.push({ key: 'read', at: 0, kind: 'tool', id: 'read', toolKind: 'read', title: 'Read', status: 'completed',
    command: null, terminalId: null, input: null, output: null, location: 'tools/silver_review.py' });
  appStore.applyAcp(msg, { kind: 'thinking', text: 'The floor mask fills the gap.' });
  appStore.applyAcp(msg, { kind: 'thinking', text: 'Locating the review tooling.' });
  appStore.applyAcp(msg, { kind: 'thinking', text: 'The floor mask fills the gap.' });
  assert.deepEqual(msg.events.map((event) => event.kind), ['thinking', 'tool', 'thinking']);
  assert.deepEqual(thoughtTexts(msg), ['Locating the review tooling.\nIt should live under tools/.', 'The floor mask fills the gap.']);
});

// A snapshot behind the live stream keeps its thoughts; live windows join the
// thought they share a window with, and a thought it has not seen yet stays
// its own, after the rest.
test('live thoughts merge into the actor snapshot without collapsing into one', () => {
  const tool = { key: 'timeline:tool:read', at: 0, kind: 'tool' as const, id: 'read', toolKind: 'read', title: 'Read', status: 'completed',
    command: null, terminalId: null, input: null, output: null, location: null };
  const authoritative: Msg['events'] = [
    { key: 'timeline:thinking:a', at: 0, kind: 'thinking', text: 'First thought.' },
    tool,
    { key: 'timeline:thinking:a:1', at: 0, kind: 'thinking', text: 'Second thought.' },
  ];
  const live: Msg['events'] = [
    { key: 't-live-1', at: 0, kind: 'thinking', text: 'First thought.' },
    { ...tool, key: 'read' },
    { key: 't-live-2', at: 0, kind: 'thinking', text: 'Second thought.\nIt keeps going.' },
    { key: 't-live-3', at: 0, kind: 'thinking', text: 'Third thought.' },
  ];
  const merged = mergeLiveTimelineEvents(authoritative, live, false);
  assert.deepEqual(merged.map((event) => event.kind === 'tool' ? 'tool' : event.key), ['timeline:thinking:a', 'tool', 'timeline:thinking:a:1', 't-live-3']);
  assert.deepEqual(thoughtTexts({ ...assistant('merged', ''), events: merged }), ['First thought.', 'Second thought.\nIt keeps going.', 'Third thought.']);
});
