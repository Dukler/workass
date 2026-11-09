import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { createServer, type ViteDevServer } from 'vite';
import {
  CHAT_TITLES_SETTINGS_KEY, DEFAULT_CHAT_TITLES, chatTitleAgents, chatTitlesFromSettings, parseChatTitles,
  resolveChatTitleChoice, withChatTitlesInSettings,
} from '../src/chat-titles.ts';
import type { ProviderRecord } from '../src/wire/types.ts';
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
  assert.deepEqual(chatTitlesFromSettings({ settings: { [CHAT_TITLES_SETTINGS_KEY]: { mode: 'agent', model: 'large' } } }),
    { mode: 'agent', providerId: '', model: 'large' });
  const merged = withChatTitlesInSettings({ modelFavorites: [{ providerId: 'a', modelId: 'b' }] }, { mode: 'agent', providerId: 'agent-a', model: 'large' });
  assert.deepEqual(merged.modelFavorites, [{ providerId: 'a', modelId: 'b' }], 'saving the title choice keeps every other setting');
  assert.deepEqual(merged[CHAT_TITLES_SETTINGS_KEY], { mode: 'agent', providerId: 'agent-a', model: 'large' });
});

// The agents come from the provider list: only enabled providers that offer
// one-shot models, and the default is the first one with its first model.
const providers = [
  { id: 'agent-off', name: 'Off', enabled: false, status: 'off', oneShotModels: [{ id: 'x', name: 'X' }] },
  { id: 'agent-none', name: 'None', enabled: true, status: 'ready' },
  { id: 'agent-a', name: 'Agent A', enabled: true, status: 'ready', oneShotModels: [{ id: 'small', name: 'Small' }, { id: 'large', name: 'Large' }] },
  { id: 'agent-b', name: 'Agent B', enabled: true, status: 'ready', oneShotModels: [{ id: 'tiny', name: 'Tiny' }] },
] as ProviderRecord[];

test('the agents that can name chats are the enabled ones offering one-shot models', () => {
  const agents = chatTitleAgents(providers);
  assert.deepEqual(agents.map((a) => a.providerId), ['agent-a', 'agent-b']);
  const pick = (providerId: string, model: string) => {
    const choice = resolveChatTitleChoice({ mode: 'agent', providerId, model }, agents);
    return choice && [choice.agent.providerId, choice.model.id];
  };
  assert.deepEqual(pick('', ''), ['agent-a', 'small'], 'the default is the first agent and its first model');
  assert.deepEqual(pick('agent-a', 'large'), ['agent-a', 'large']);
  assert.deepEqual(pick('agent-b', 'gone'), ['agent-b', 'tiny'], 'a model no longer offered falls back to the first');
  assert.deepEqual(pick('agent-off', 'x'), ['agent-a', 'small'], 'a disabled agent falls back to the default');
  assert.equal(resolveChatTitleChoice({ mode: 'agent', providerId: '', model: '' }, []), null);
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
  subject.state.chatTitles = { mode, providerId: 'agent-a', model: 'large' };
  subject.state.providers = providers;
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
    expectTitle: 'arreglá el salto del scroll…', providerId: 'agent-a', model: 'large',
  });
});

test('the first-message setting and a chat named by hand never ask an agent', async () => {
  assert.equal((await sendWith(chat('tab-title-prompt', false), 'prompt', ['hola'])).asked.length, 0);
  assert.equal((await sendWith(chat('tab-title-locked', true), 'agent', ['hola'])).asked.length, 0);
});
