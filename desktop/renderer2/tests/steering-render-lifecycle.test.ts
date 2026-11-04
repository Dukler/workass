import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { fileURLToPath } from 'node:url';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { createServer, type ViteDevServer } from 'vite';
import type { Chat, Msg } from '../src/store/types.ts';
import { appendAssistantChunk, fullAssistantText } from '../src/assistant-output.ts';

let vite: ViteDevServer;
let Transcript: React.ComponentType<{ chat: Chat | null }>;
let QueueList: React.ComponentType<{ chat: Chat }>;
let QueuedSteerButton: (props: { chatId: string; queuedId: string; title: string }) => React.ReactElement;
let appStore: { state: Record<string, unknown>; clearToastTimers(): void; steerQueued(chatId: string, queuedId: string): Promise<boolean> };

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
  ({ QueueList, QueuedSteerButton } = await vite.ssrLoadModule('/src/components/QueueList.tsx') as {
    QueueList: React.ComponentType<{ chat: Chat }>;
    QueuedSteerButton: (props: { chatId: string; queuedId: string; title: string }) => React.ReactElement;
  });
  appStore = (await vite.ssrLoadModule('/src/store/store.ts') as {
    store: typeof appStore;
  }).store;
});

after(async () => { appStore.clearToastTimers(); await vite.close(); });

function user(id: string, content: string, extra: Partial<Msg> = {}): Msg {
  return { id, role: 'user', content, status: 'done', at: null, events: [], ...extra };
}

function assistant(id: string, content: string, extra: Partial<Msg> = {}): Msg {
  return { id, role: 'assistant', content, status: 'done', at: null, events: [], ...extra };
}

// A running reply shows only finished lines (stream-reveal.ts), so live fixture text ends its line.
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
      user('steer', 'STEER-DIRECTION-TEXT', { turnRootId: 'head', steerState: 'sending' }),
      assistant('tail', 'ASSISTANT-AFTER-DIRECTION\n', {
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

test('actual transcript and composer tray transfer one steer owner at acknowledgement, then retain it through terminal', () => {
  const chat = chatWithLiveSteer();
  const canonicalOrder = chat.messages.map((message) => message.id);

  const unresolvedTranscript = renderTranscript(chat);
  const unresolvedTray = renderQueue(chat);
  assert.doesNotMatch(unresolvedTranscript, /STEER-DIRECTION-TEXT/);
  assert.equal(occurrences(unresolvedTray, 'STEER-DIRECTION-TEXT'), 1);
  assert.match(unresolvedTray, /Steering · 1/);

  chat.messages[2].status = 'done';
  chat.messages[2].steerState = 'applied';
  const liveTranscript = renderTranscript(chat);
  const liveTray = renderQueue(chat);
  assert.equal(occurrences(liveTranscript, 'STEER-DIRECTION-TEXT'), 1);
  assert.doesNotMatch(liveTray, /STEER-DIRECTION-TEXT|Steering · 1/);
  assert.ok(
    liveTranscript.indexOf('ASSISTANT-BEFORE-DIRECTION') < liveTranscript.indexOf('STEER-DIRECTION-TEXT')
      && liveTranscript.indexOf('STEER-DIRECTION-TEXT') < liveTranscript.indexOf('ASSISTANT-AFTER-DIRECTION'),
    'acknowledged steering renders once at canonical chronology while the turn remains live',
  );

  chat.messages.at(-1)!.status = 'done';
  const settledTranscript = renderTranscript(chat);
  const settledTray = renderQueue(chat);
  const before = settledTranscript.indexOf('ASSISTANT-BEFORE-DIRECTION');
  const after = settledTranscript.indexOf('ASSISTANT-AFTER-DIRECTION');
  const steer = settledTranscript.indexOf('STEER-DIRECTION-TEXT');
  assert.ok(before >= 0 && before < steer && steer < after, 'terminal keeps the same canonical steer position');
  assert.doesNotMatch(settledTray, /STEER-DIRECTION-TEXT|Steering · 1/);
  assert.deepEqual(chat.messages.map((message) => message.id), canonicalOrder, 'rendering never rewrites actor chronology');

  const reloaded = JSON.parse(JSON.stringify(chat)) as Chat;
  const reloadedTranscript = renderTranscript(reloaded);
  assert.ok(
    reloadedTranscript.indexOf('STEER-DIRECTION-TEXT') < reloadedTranscript.indexOf('ASSISTANT-AFTER-DIRECTION'),
    'the same terminal presentation survives a JSON hydration boundary',
  );
  assert.deepEqual(reloaded.messages.map((message) => message.id), canonicalOrder);
});

const liveCapabilities = { stableInputIdentity: true, liveSteer: true, steerConsumptionReceipt: true, consumptionReceipt: true, turnReadback: false };
const stopAndSendCapabilities = { ...liveCapabilities, liveSteer: false, steerConsumptionReceipt: false, stopAndSend: true };
const noSteerCapabilities = { ...liveCapabilities, liveSteer: false, steerConsumptionReceipt: false };
const fullBridge = { appChatSteer: async () => ({}), chatQueueReplace: async () => ({}), cancelJob: async () => ({}) };

// `has()` reads the bridge from window.api, as the composer's checks do.
function renderQueueWithBridge(chat: Chat, api: Record<string, unknown> = fullBridge): string {
  const g = globalThis as { window?: unknown };
  const previous = g.window;
  g.window = { api };
  try { return renderQueue(chat); } finally { if (previous === undefined) delete g.window; else g.window = previous; }
}

function steerButtons(markup: string): string[] {
  return [...markup.matchAll(/<button class="qsteer" title="([^"]*)" aria-label="([^"]*)"/g)].map((match) => {
    assert.equal(match[1], match[2], 'tooltip and accessible name are the same words');
    return match[1];
  });
}

test('every queued row of a running turn offers the composer steer its lane supports, left of the remove button', () => {
  const chat = chatWithLiveSteer();
  chat.queue = [{ id: 'q-1', text: 'QUEUED-ONE' }, { id: 'q-2', text: 'QUEUED-TWO' }];

  chat.deliveryCapabilities = liveCapabilities;
  const live = renderQueueWithBridge(chat);
  assert.deepEqual(steerButtons(live), ['Dirigir ahora', 'Dirigir ahora']);
  assert.equal(occurrences(live, '<span class="qacts"><button class="qsteer"'), 2, 'the steer button opens the action group');
  assert.ok(live.indexOf('class="qsteer"') < live.indexOf('class="qx"'), 'steer sits immediately left of remove');
  assert.match(live, /<svg viewBox="0 0 14 14" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M3 2.5v3.2a3 3 0 0 0 3 3h5"><\/path><path d="M8.6 6.3 11 8.7 8.6 11.1"><\/path><\/svg>/);
  assert.doesNotMatch(live, /⌘|Enter/, 'the composer shortcut steers the draft, not a queued row');

  chat.deliveryCapabilities = stopAndSendCapabilities;
  assert.deepEqual(steerButtons(renderQueueWithBridge(chat)), ['Detener y enviar', 'Detener y enviar']);

  // A lane that supports both is a live lane: real live steering always wins.
  chat.deliveryCapabilities = { ...liveCapabilities, stopAndSend: true };
  assert.deepEqual(steerButtons(renderQueueWithBridge(chat)), ['Dirigir ahora', 'Dirigir ahora']);
});

test('no steer button renders when the turn is idle, stopping, or the lane cannot steer', () => {
  const chat = chatWithLiveSteer();
  chat.queue = [{ id: 'q-1', text: 'QUEUED-ONE' }];
  const cases: Array<[string, () => string]> = [
    ['no capability', () => { chat.deliveryCapabilities = noSteerCapabilities; return renderQueueWithBridge(chat); }],
    ['no capabilities reported', () => { chat.deliveryCapabilities = undefined; return renderQueueWithBridge(chat); }],
    ['live lane without the steer channel', () => {
      chat.deliveryCapabilities = liveCapabilities;
      return renderQueueWithBridge(chat, { chatQueueReplace: fullBridge.chatQueueReplace, cancelJob: fullBridge.cancelJob });
    }],
    ['stop-and-send lane without the stop channel', () => {
      chat.deliveryCapabilities = stopAndSendCapabilities;
      return renderQueueWithBridge(chat, { appChatSteer: fullBridge.appChatSteer, chatQueueReplace: fullBridge.chatQueueReplace });
    }],
    ['stop already requested', () => {
      chat.deliveryCapabilities = liveCapabilities;
      chat.messages.at(-1)!.stopState = 'requesting';
      try { return renderQueueWithBridge(chat); } finally { delete chat.messages.at(-1)!.stopState; }
    }],
    ['idle chat', () => {
      chat.deliveryCapabilities = liveCapabilities;
      chat.messages.at(-1)!.status = 'done';
      try { return renderQueueWithBridge(chat); } finally { chat.messages.at(-1)!.status = 'running'; }
    }],
  ];
  for (const [name, render] of cases) {
    const markup = render();
    assert.match(markup, /QUEUED-ONE/, name);
    assert.match(markup, /class="qx"/, name);
    assert.doesNotMatch(markup, /qsteer|disabled/, `${name}: nothing rendered, not a disabled button`);
  }
});

test('subagent notices and rows whose attachments are not ready get no steer button', () => {
  const chat = chatWithLiveSteer();
  chat.deliveryCapabilities = liveCapabilities;
  const draft = { id: 'draft-1', name: 'shot.png', url: 'blob:shot', mimeType: 'image/png' } as never;
  chat.queue = [
    { id: 'notice', text: 'NOTICE', notice: { title: 'Revisor', status: 'done' } } as never,
    { id: 'preparing', text: 'PREPARING', draftImages: [draft], attachmentState: 'preparing' } as never,
    { id: 'failed', text: 'FAILED', draftImages: [draft], attachmentState: 'failed', attachmentError: 'unreadable' } as never,
    { id: 'ready', text: 'READY-ROW', images: [{ mimeType: 'image/png', data: 'aGVsbG8=', name: 'shot.png' }] },
  ];
  const markup = renderQueueWithBridge(chat);
  assert.deepEqual(steerButtons(markup), ['Dirigir ahora']);
  assert.ok(markup.indexOf('READY-ROW') < markup.indexOf('class="qsteer"'), 'only the ready row carries the button');
  assert.equal(occurrences(markup, 'class="qx"'), 4, 'every row keeps its remove button');
});

test('pressing the steer button hands that exact row to the store without dragging or editing the row', async () => {
  const calls: Array<[string, string]> = [];
  const original = appStore.steerQueued;
  appStore.steerQueued = async (chatId: string, queuedId: string) => { calls.push([chatId, queuedId]); return true; };
  try {
    const button = QueuedSteerButton({ chatId: 'tab-steering-render', queuedId: 'q-2', title: 'Dirigir ahora' });
    const stopped: string[] = [];
    button.props.onMouseDown({ stopPropagation: () => stopped.push('mousedown') });
    button.props.onDoubleClick({ stopPropagation: () => stopped.push('dblclick') });
    assert.deepEqual(stopped, ['mousedown', 'dblclick']);
    assert.deepEqual(calls, []);
    button.props.onClick({});
    assert.deepEqual(calls, [['tab-steering-render', 'q-2']]);
  } finally {
    appStore.steerQueued = original;
  }
});

test('a goal checkpoint final stays before subsequent tools and commentary through terminal and hydration', () => {
  const chat = chatWithLiveSteer();
  const message = assistant('goal-output', '', { status: 'running' });
  appendAssistantChunk(message, 'GOAL-OPENING.\n\n', 'commentary');
  appendAssistantChunk(message, 'GOAL-CHECKPOINT 🧭.\n\n', 'final_answer');
  message.events.push({ key: 'later-tool', at: message.content.length, kind: 'tool', id: 'later-tool', toolKind: 'execute', title: 'GOAL-LATER-TOOL', status: 'completed', command: 'echo goal', terminalId: null, input: null, output: null, location: null });
  appendAssistantChunk(message, 'GOAL-CONTINUATION.\n\n', 'commentary');
  appendAssistantChunk(message, 'GOAL-COMPLETION.\n', 'final_answer');
  chat.messages = [user('goal-input', '/goal fixture'), message];
  const verify = (subject: Chat) => {
    const markup = renderTranscript(subject);
    // Tool calls never interleave with the prose: the Working line owns them
    // while the turn runs, and the settled turn's closed Worked row after.
    const running = subject.messages[1].status === 'running';
    const markers = ['GOAL-OPENING', 'GOAL-CHECKPOINT', 'GOAL-CONTINUATION', 'GOAL-COMPLETION'];
    const offsets = markers.map(marker => markup.indexOf(marker));
    assert.ok(offsets.every((offset, index) => offset >= 0 && (index === 0 || offset > offsets[index - 1])), 'final-answer phases render at their capture boundary, followed by later work');
    assert.equal(markup.includes('GOAL-LATER-TOOL'), false);
    if (!running) assert.ok(markup.indexOf('>Worked<') >= 0 && markup.indexOf('>Worked<') < offsets[0], 'the settled turn opens with its Worked row');
    assert.equal(occurrences(markup, 'GOAL-CHECKPOINT'), 1);
    assert.equal(occurrences(markup, 'GOAL-COMPLETION'), 1);
    assert.equal(fullAssistantText(subject.messages[1]), 'GOAL-OPENING.\n\nGOAL-CHECKPOINT 🧭.\n\nGOAL-CONTINUATION.\n\nGOAL-COMPLETION.');
  };
  verify(chat);
  message.status = 'done';
  verify(chat);
  verify(JSON.parse(JSON.stringify(chat)) as Chat);
});

test('a previously saved final answer advances above new continuation text without losing its phase', () => {
  const chat = chatWithLiveSteer();
  const message = assistant('legacy-goal-output', 'EARLIER-COMMENTARY.\n\n', { status: 'running', result: 'SAVED-GOAL-CHECKPOINT.\n\n' });
  appendAssistantChunk(message, 'NEW-GOAL-CONTINUATION.\n', 'commentary');
  chat.messages = [user('legacy-goal-input', '/goal fixture'), message];
  const markup = renderTranscript(chat);
  assert.ok(markup.indexOf('SAVED-GOAL-CHECKPOINT') < markup.indexOf('NEW-GOAL-CONTINUATION'));
  assert.equal(message.result, 'SAVED-GOAL-CHECKPOINT.\n\n');
  assert.equal(fullAssistantText(message), 'EARLIER-COMMENTARY.\n\nSAVED-GOAL-CHECKPOINT.\n\nNEW-GOAL-CONTINUATION.');
});
