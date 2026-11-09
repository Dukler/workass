import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { createServer, type ViteDevServer } from 'vite';
import {
  CHAT_TITLES_SETTINGS_KEY, DEFAULT_CHAT_TITLES, chatTitlesFromSettings, parseChatTitles, withChatTitlesInSettings,
} from '../src/chat-titles.ts';
import type { Chat } from '../src/store/types.ts';

let vite: ViteDevServer;
let StoreCtor: new () => any;
const stores: any[] = [];

before(async () => {
  vite = await createServer({
    root: fileURLToPath(new URL('..', import.meta.url)),
    logLevel: 'silent',
    server: { middlewareMode: true },
    appType: 'custom',
  });
  StoreCtor = (await vite.ssrLoadModule('/src/store/store.ts')).Store;
});

after(async () => {
  for (const store of stores) store.clearToastTimers();
  await vite.close();
});

test('the title setting defaults to the first message and survives the settings envelope', () => {
  assert.deepEqual(parseChatTitles(undefined), DEFAULT_CHAT_TITLES);
  assert.deepEqual(parseChatTitles({ mode: 'whatever', model: '  ' }), DEFAULT_CHAT_TITLES);
  assert.deepEqual(chatTitlesFromSettings({ settings: { [CHAT_TITLES_SETTINGS_KEY]: { mode: 'agent', model: 'sonnet' } } }),
    { mode: 'agent', providerId: 'claude', model: 'sonnet' });
  const merged = withChatTitlesInSettings({ modelScores: { a: 1 } }, { mode: 'agent', providerId: 'claude', model: 'opus' });
  assert.deepEqual(merged.modelScores, { a: 1 }, 'saving the title choice keeps every other setting');
  assert.deepEqual(merged[CHAT_TITLES_SETTINGS_KEY], { mode: 'agent', providerId: 'claude', model: 'opus' });
});

function chat(id: string, titleLocked: boolean): Chat {
  return {
    id, chatId: `chat-${id}`, sessionId: `session-${id}`, title: titleLocked ? 'Mi nombre' : 'Chat 3', titleLocked,
    group: null, cwd: '/tmp/workass-chat-titles', providerId: 'mock', currentModelId: null, currentModeId: null,
    pending: false, messages: [], draft: '',
  } as Chat;
}

async function sendWith(owner: Chat, mode: 'agent' | 'prompt', prompts: string[]) {
  const asked: Array<Record<string, unknown>> = [];
  const previousWindow = (globalThis as any).window;
  const subject = new StoreCtor();
  stores.push(subject);
  subject.state.chats = [owner];
  subject.state.activeId = owner.id;
  subject.state.connection = 'connected';
  subject.state.chatTitles = { mode, providerId: 'claude', model: 'haiku' };
  subject.schedulePersist = () => {};
  (globalThis as any).window = {
    api: {
      startJob: async () => ({ id: `job-${asked.length}` }),
      appChatAutoTitle: async (o: Record<string, unknown>) => { asked.push({ ...o }); return { started: true }; },
    },
  };
  try {
    for (const prompt of prompts) {
      await subject._send(subject.chat(owner.id), prompt);
      const running = subject.chat(owner.id).messages.find((m: any) => m.role === 'assistant' && m.status === 'running');
      if (running) running.status = 'done';
    }
    return { asked, subject };
  } finally {
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
  }
}

test('with an agent chosen, a chat\'s first message is sent once for a title', async () => {
  const owner = chat('tab-title-agent', false);
  const { asked, subject } = await sendWith(owner, 'agent', ['arreglá el salto del scroll cuando subo la lista', 'y también el otro']);
  assert.equal(subject.chat(owner.id).title, 'arreglá el salto del scroll…');
  assert.equal(asked.length, 1, 'only the first message names the chat');
  assert.deepEqual(asked[0], {
    tabId: owner.id, chatId: owner.chatId, text: 'arreglá el salto del scroll cuando subo la lista',
    expectTitle: 'arreglá el salto del scroll…', providerId: 'claude', model: 'haiku',
  });
});

test('the first-message setting and a chat named by hand never ask an agent', async () => {
  assert.equal((await sendWith(chat('tab-title-prompt', false), 'prompt', ['hola'])).asked.length, 0);
  assert.equal((await sendWith(chat('tab-title-locked', true), 'agent', ['hola'])).asked.length, 0);
});
