import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { fileURLToPath } from 'node:url';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { createServer, type ViteDevServer } from 'vite';
import type { Chat } from '../src/store/types.ts';

let vite: ViteDevServer;
let Composer: React.ComponentType<{ chat: Chat | null }>;
let appStore: {
  state: { chats: Chat[]; activeId: string | null; connection: string };
  applyChatGoal(event: unknown): void;
  clearToastTimers(): void;
};

before(async () => {
  vite = await createServer({ root: fileURLToPath(new URL('..', import.meta.url)), logLevel: 'silent', server: { middlewareMode: true }, appType: 'custom' });
  ({ Composer } = await vite.ssrLoadModule('/src/components/Composer.tsx') as { Composer: React.ComponentType<{ chat: Chat | null }> });
  appStore = (await vite.ssrLoadModule('/src/store/store.ts') as { store: typeof appStore }).store;
});

after(async () => { appStore.clearToastTimers(); await vite.close(); });

function goalChat(): Chat {
  return {
    id: 'goal-tab', chatId: 'goal-chat', sessionId: 'goal-session', title: 'Goal', titleLocked: true, group: null, cwd: null,
    providerId: 'codex', currentModelId: null, currentModeId: null, pending: false, messages: [], draft: '',
  } as Chat;
}

function render(chat: Chat): string {
  appStore.state.chats = [chat];
  appStore.state.activeId = chat.id;
  return renderToStaticMarkup(React.createElement(Composer, { chat }));
}

// Approved mock chatbox-v10 · A1: the chat's goal is the box's own header band,
// a shade apart from the message being written; it follows each chat:goal.
test('a chat:goal becomes the composer goal band and follows its status', () => {
  const chat = goalChat();
  assert.doesNotMatch(render(chat), /goalband/);

  appStore.applyChatGoal({ tabId: 'goal-tab', chatId: 'goal-chat', providerId: 'codex', observedAt: Date.now(),
    goal: { objective: 'finish the Gold review for all 14 frames', status: 'active', tokensUsed: 1200, tokenBudget: null, timeUsedSeconds: 720 } });
  let html = render(chat);
  assert.match(html, /<div class="comp hasgoal">\s*<div class="goalband active" role="status">/);
  assert.match(html, /class="goalobj"[^>]*>finish the Gold review for all 14 frames</);
  assert.match(html, /activa · 12m/);
  assert.match(html, /aria-label="Pausar la meta"/);
  assert.match(html, /aria-label="Quitar la meta"/);
  assert.doesNotMatch(html, /Reanudar la meta/);

  appStore.applyChatGoal({ tabId: 'goal-tab', chatId: 'goal-chat', goal: { objective: 'finish the Gold review for all 14 frames', status: 'usageLimited', tokensUsed: 1, tokenBudget: null, timeUsedSeconds: 60 } });
  html = render(chat);
  assert.match(html, /class="goalband attention"/);
  assert.match(html, /límite de uso alcanzado/);
  assert.match(html, /aria-label="Reanudar la meta"/);

  appStore.applyChatGoal({ tabId: 'goal-tab', chatId: 'goal-chat', goal: { objective: 'finish the Gold review for all 14 frames', status: 'complete', tokensUsed: 1, tokenBudget: null, timeUsedSeconds: 2460 } });
  html = render(chat);
  assert.match(html, /class="goalband done"/);
  assert.match(html, /Meta cumplida/);
  assert.match(html, /en 41m/);

  appStore.applyChatGoal({ tabId: 'goal-tab', chatId: 'goal-chat', goal: null });
  assert.doesNotMatch(render(chat), /goalband/);
});

// Opening a chat asks for its goal; "none" for a chat that had none changes
// nothing on screen, so it must not re-render the app inside the chat's first
// frame. A goal arriving, or going away, still does.
test('no goal for a chat without one updates nothing on screen', () => {
  const chat = goalChat();
  appStore.state.chats = [chat];
  const store = appStore as typeof appStore & { version(topic: string): number };
  const before = store.version('app');
  appStore.applyChatGoal({ tabId: 'goal-tab', chatId: 'goal-chat', goal: null });
  assert.equal(chat.goal, null);
  assert.equal(store.version('app'), before);
  appStore.applyChatGoal({ tabId: 'goal-tab', chatId: 'goal-chat', goal: { objective: 'x', status: 'active', tokensUsed: 0, tokenBudget: null, timeUsedSeconds: 0 } });
  assert.equal(store.version('app'), before + 1);
  appStore.applyChatGoal({ tabId: 'goal-tab', chatId: 'goal-chat', goal: null });
  assert.equal(store.version('app'), before + 2);
});

test('a goal for another chat id is ignored', () => {
  const chat = goalChat();
  appStore.state.chats = [chat];
  appStore.applyChatGoal({ tabId: 'goal-tab', chatId: 'some-other-chat', goal: { objective: 'x', status: 'active', tokensUsed: 0, tokenBudget: null, timeUsedSeconds: 0 } });
  assert.equal(chat.goal, undefined);
});

// A chat with no explicit provider runs on the daemon's default; the composer
// names that model instead of a bare "Modelo" (additive chat:catalog field).
test('a chat with no provider names the daemon default model', () => {
  const state = appStore.state as typeof appStore.state & { groups: unknown[]; defaultProviderId?: string | null };
  state.groups = [{ providerId: 'claude', providerName: 'Claude Code', assistantBrand: 'claude', models: [{ modelId: 'fable-5-1', name: 'Fable 5.1' }], modes: [] }];
  state.defaultProviderId = 'claude';
  const chat = { ...goalChat(), providerId: null } as unknown as Chat;
  const html = render(chat);
  assert.match(html, /class="modelsel">.*Fable 5\.1<span class="selcv"/);
  assert.doesNotMatch(html, />Modelo</);
  state.groups = [];
  state.defaultProviderId = null;
});
