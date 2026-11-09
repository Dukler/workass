import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { createServer, type ViteDevServer } from 'vite';
import {
  DEFAULT_GENERAL_SETTINGS, busyEnterIntent, generalSettingsFromSettings, newChatAgents, parseGeneralSettings,
  withGeneralSettingsInSettings,
} from '../src/general-settings.ts';
import { composerKeyAction } from '../src/composer-submit.ts';
import { finishedBackgroundWork, finishedWorkText, freshEnding } from '../src/notices.ts';
import { buildWorkspaceGroups, newChatTarget } from '../src/workspaces.ts';
import type { Chat } from '../src/store/types.ts';
import type { CatalogGroup, PublicJob, SpawnedWorkItem } from '../src/wire/types.ts';

// Ajustes · General: new-chat defaults, what Enter does while a turn runs, the
// notices and the dictation language all round-trip the daemon blob, and each
// one changes what the app actually does.

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

test('the General settings default as before and keep every other key on save', () => {
  assert.deepEqual(parseGeneralSettings(undefined), DEFAULT_GENERAL_SETTINGS);
  assert.deepEqual(generalSettingsFromSettings({ settings: {} }), DEFAULT_GENERAL_SETTINGS);
  const parsed = generalSettingsFromSettings({ settings: {
    newChat: { providerId: ' agent-a ', model: 'large', effort: 'high', modeId: 'edit', folder: '/tmp/project' },
    whileWorking: 'steer',
    notices: { needsYou: false, turnEnded: 'no', jobs: false, sound: true },
    dictation: { lang: 'EN' },
  } });
  assert.deepEqual(parsed, {
    newChat: { providerId: 'agent-a', model: 'large', effort: 'high', modeId: 'edit', folder: '/tmp/project' },
    whileWorking: 'steer',
    notices: { needsYou: false, turnEnded: true, jobs: false, sound: true },
    dictation: { lang: 'en' },
  });
  assert.deepEqual(parseGeneralSettings({ newChat: { model: 'large', effort: 'high', folder: 'relative' } }).newChat,
    DEFAULT_GENERAL_SETTINGS.newChat, 'without an agent no model or effort is kept, and a relative folder means the last one');
  assert.equal(parseGeneralSettings({ newChat: { providerId: 'a', effort: 'high' } }).newChat.effort, '', 'an effort needs its model');
  const merged = withGeneralSettingsInSettings({ chatTitles: { mode: 'agent' }, modelFavorites: [1] }, parsed);
  assert.deepEqual(merged.chatTitles, { mode: 'agent' });
  assert.deepEqual(merged.modelFavorites, [1]);
  assert.deepEqual(merged.newChat, parsed.newChat);
  assert.equal(merged.whileWorking, 'steer');
});

test('Enter while a turn runs does what the setting says and ⌘Enter the other', () => {
  assert.equal(busyEnterIntent('queue', false), 'queue');
  assert.equal(busyEnterIntent('queue', true), 'steer');
  assert.equal(busyEnterIntent('steer', false), 'steer');
  assert.equal(busyEnterIntent('steer', true), 'queue');
  assert.equal(busyEnterIntent('steer', false, false), 'queue', 'an agent that cannot be steered keeps the queue on Enter');
  const enter = { key: 'Enter', shiftKey: false, metaKey: false, ctrlKey: false };
  assert.equal(composerKeyAction(true, enter, false), 'queue', 'no preference is the old law');
  assert.equal(composerKeyAction(true, enter, false, { whileWorking: 'steer', canSteer: true }), 'steer');
  assert.equal(composerKeyAction(true, { ...enter, metaKey: true }, false, { whileWorking: 'steer', canSteer: true }), 'queue');
  assert.equal(composerKeyAction(true, { ...enter, ctrlKey: true }, false, { whileWorking: 'steer', canSteer: true }), 'queue');
  assert.equal(composerKeyAction(false, enter, false, { whileWorking: 'steer', canSteer: true }), 'send', 'an idle chat always sends');
  assert.equal(composerKeyAction(true, enter, true, { whileWorking: 'steer', canSteer: true }), 'pick', 'autocomplete still owns Enter');
});

function chat(id: string, overrides: Partial<Chat> = {}): Chat {
  return {
    id, chatId: `chat-${id}`, sessionId: `session-${id}`, title: id, titleLocked: true, group: null,
    cwd: '/tmp/workass-general/a', providerId: 'agent-b', providerName: 'Agent B', currentModelId: 'b-model', currentModeId: 'b-mode',
    pending: false, messages: [], draft: '', ...overrides,
  } as Chat;
}

test('the folder of a new chat: the last one, the open chat\'s, or one fixed folder', () => {
  const chats = [chat('tab-1', { cwd: '/tmp/workass-general/a' }), chat('tab-2', { cwd: '/tmp/workass-general/b' })];
  const groups = buildWorkspaceGroups([], chats);
  const last = '/tmp/workass-general/b';
  assert.equal(newChatTarget(null, groups, last), last, 'the default is the last folder');
  assert.equal(newChatTarget(null, groups, last, 'active', '/tmp/workass-general/a'), '/tmp/workass-general/a');
  assert.equal(newChatTarget(null, groups, last, '/tmp/workass-general/a'), '/tmp/workass-general/a');
  assert.equal(newChatTarget(null, groups, last, '/tmp/workass-general/gone'), last, 'a fixed folder that is gone falls back to the last one');
  assert.equal(newChatTarget('/tmp/workass-general/a', groups, last, '/tmp/workass-general/b'), '/tmp/workass-general/a', 'a scope always wins');
});

const groups: CatalogGroup[] = [
  {
    providerId: 'agent-a', providerName: 'Agent A', status: 'ready',
    models: [{ modelId: 'small', name: 'Small', efforts: ['low', 'high'] }, { modelId: 'large', name: 'Large', efforts: ['low', 'medium', 'high', 'max'] }],
    modes: [{ id: 'ask', name: 'Ask' }, { id: 'edit', name: 'Edit' }, { id: 'all', name: 'All' }],
  },
  { providerId: 'agent-b', providerName: 'Agent B', status: 'ready', models: [{ modelId: 'b-model', name: 'B' }], modes: [{ id: 'b-mode', name: 'B mode' }] },
  { providerId: 'agent-off', providerName: 'Off', status: 'needs-login', models: [{ modelId: 'x', name: 'X' }], modes: [] },
];

function subject(general: Partial<typeof DEFAULT_GENERAL_SETTINGS> = {}): any {
  const store = new StoreCtor();
  stores.push(store);
  store.state.chats = [chat('tab-open')];
  store.state.activeId = 'tab-open';
  store.state.groups = groups;
  store.state.general = parseGeneralSettings({ ...DEFAULT_GENERAL_SETTINGS, ...general });
  store.schedulePersist = () => {};
  store.ensureChatCreated = async () => true;
  store.refreshPlanUsage = () => {};
  return store;
}

test('only ready agents with models can start new chats', () => {
  assert.deepEqual(newChatAgents(groups).map((g) => g.providerId), ['agent-a', 'agent-b']);
});

test('with no agent chosen a new chat copies the open chat, as it always did', () => {
  const created = subject().newChat(false, '/tmp/workass-general/a');
  assert.equal(created.providerId, 'agent-b');
  assert.equal(created.currentModelId, 'b-model');
  assert.equal(created.currentModeId, 'b-mode');
});

test('a chosen agent, model, effort and permissions start the new chat', () => {
  const store = subject({ newChat: { providerId: 'agent-a', model: 'large', effort: 'max', modeId: 'edit', folder: 'last' } });
  const created = store.newChat(false, '/tmp/workass-general/a');
  assert.equal(created.providerId, 'agent-a');
  assert.equal(created.providerName, 'Agent A');
  assert.equal(created.currentModelId, 'large[max]');
  assert.equal(created.currentModeId, 'edit');
});

test('defaults the catalog no longer offers fall back to the agent\'s own', () => {
  const store = subject({ newChat: { providerId: 'agent-a', model: 'gone', effort: 'max', modeId: 'gone', folder: 'last' } });
  const created = store.newChat(false, '/tmp/workass-general/a');
  assert.equal(created.currentModelId, 'small[high]', 'the first model at its default effort');
  assert.equal(created.currentModeId, null, 'the agent picks its own default mode');
  const off = subject({ newChat: { providerId: 'agent-off', model: '', effort: '', modeId: '', folder: 'last' } }).newChat(false, '/tmp/workass-general/a');
  assert.equal(off.providerId, 'agent-b', 'an agent that is not ready leaves the open chat as the model');
});

function withLocalStorage<T>(values: Record<string, string>, run: () => T): T {
  const previous = Object.getOwnPropertyDescriptor(globalThis, 'localStorage');
  const data = new Map(Object.entries(values));
  Object.defineProperty(globalThis, 'localStorage', {
    configurable: true,
    value: { getItem: (k: string) => data.get(k) ?? null, setItem: (k: string, v: string) => { data.set(k, v); }, removeItem: (k: string) => { data.delete(k); } },
  });
  try { return run(); } finally {
    if (previous) Object.defineProperty(globalThis, 'localStorage', previous);
    else delete (globalThis as any).localStorage;
  }
}

test('a new chat with no folder named opens where the setting says', () => {
  const lastKey = 'workass.sv2.lastNewChatProject';
  withLocalStorage({ [lastKey]: '/tmp/workass-general/b' }, () => {
    const make = (folder: string) => {
      const store = subject({ newChat: { ...DEFAULT_GENERAL_SETTINGS.newChat, folder } });
      store.state.chats.push(chat('tab-b', { cwd: '/tmp/workass-general/b' }));
      store.state.chats.push(chat('tab-c', { cwd: '/tmp/workass-general/c' }));
      return store.newChat(false).cwd;
    };
    assert.equal(make('last'), '/tmp/workass-general/b');
    assert.equal(make('active'), '/tmp/workass-general/a');
    assert.equal(make('/tmp/workass-general/c'), '/tmp/workass-general/c');
    const explicit = subject({ newChat: { ...DEFAULT_GENERAL_SETTINGS.newChat, folder: '/tmp/workass-general/c' } });
    assert.equal(explicit.newChat(false, '/tmp/workass-general/a').cwd, '/tmp/workass-general/a', 'a folder the caller names wins');
  });
});

function endJob(overrides: Partial<PublicJob> = {}): PublicJob {
  return {
    id: 'job-other', kind: 'app-chat', key: null, title: 'turn', status: 'done',
    startedAt: new Date(Date.now() - 5000).toISOString(), finishedAt: new Date().toISOString(), code: 0,
    permissionMode: 'agent', chatId: 'chat-tab-other', tabId: 'tab-other', sessionId: 'session-tab-other', providerId: 'agent-b',
    userMessageId: 'user-x', assistantMessageId: 'assistant-x', promptText: 'x', result: 'done', error: null, stopReason: null,
    crashInterrupted: false, ...overrides,
  } as PublicJob;
}

function noticeSubject(notices: Partial<typeof DEFAULT_GENERAL_SETTINGS.notices> = {}) {
  const store = subject({ notices: { ...DEFAULT_GENERAL_SETTINGS.notices, ...notices } });
  store.state.chats.push(chat('tab-other', { title: 'Otro chat' }));
  const fired: Array<[string, string, string | undefined]> = [];
  let sounds = 0;
  store.fireNotify = (title: string, body: string, tabId?: string) => {
    fired.push([title, body, tabId]);
    if (store.state.general.notices.sound) store.playNoticeSound();
  };
  store.playNoticeSound = () => { sounds++; };
  return { store, fired, sounds: () => sounds };
}

test('a turn that ends in a chat you are not looking at is announced when the setting is on', () => {
  const on = noticeSubject();
  on.store.onJobEvent({ type: 'end', job: endJob() });
  assert.deepEqual(on.fired, [['Otro chat', 'Terminó.', 'tab-other']]);

  const off = noticeSubject({ turnEnded: false });
  off.store.onJobEvent({ type: 'end', job: endJob() });
  assert.deepEqual(off.fired, []);

  const watching = noticeSubject();
  watching.store.state.activeId = 'tab-other';
  watching.store.onJobEvent({ type: 'end', job: endJob() });
  assert.deepEqual(watching.fired, [], 'the chat you are looking at says it itself');

  const cancelled = noticeSubject();
  cancelled.store.onJobEvent({ type: 'end', job: endJob({ status: 'failed', code: 130, stopReason: 'cancelled' }) });
  assert.deepEqual(cancelled.fired, [], 'a stop is your own doing');

  const old = noticeSubject();
  old.store.onJobEvent({ type: 'end', job: endJob({ finishedAt: new Date(Date.now() - 10 * 60_000).toISOString() }) });
  assert.deepEqual(old.fired, [], 'a late replay is old news');

  const failed = noticeSubject({ sound: true });
  failed.store.onJobEvent({ type: 'end', job: endJob({ status: 'failed', code: 1 }) });
  assert.deepEqual(failed.fired, [['Otro chat', 'El turno falló.', 'tab-other']]);
  assert.equal(failed.sounds(), 1, 'the sound plays with the notice');
});

test('a permission is announced once, and not at all with the setting off', () => {
  const pending = () => [{ id: 'assistant-p', role: 'assistant', content: '', status: 'running', at: null, events: [], jobId: 'job-p' }];
  const request = { id: 'perm-1', jobId: 'job-p', tabId: 'tab-other', chatId: 'chat-tab-other', title: 'Run tests', options: [] };
  const on = noticeSubject();
  on.store.chat('tab-other').messages = pending();
  on.store.onPermission(request);
  on.store.onPermission(request);
  assert.deepEqual(on.fired, [['Otro chat', 'Permiso solicitado: Run tests', 'tab-other']], 'a replayed request is not announced again');

  const off = noticeSubject({ needsYou: false });
  off.store.chat('tab-other').messages = pending();
  off.store.onPermission(request);
  assert.deepEqual(off.fired, []);
});

function work(id: string, overrides: Partial<SpawnedWorkItem> = {}): SpawnedWorkItem {
  return {
    id, taskId: id, tabId: 'tab-other', chatId: 'chat-tab-other', providerId: 'agent-b', kind: 'external', label: `job ${id}`,
    status: 'running', startedAt: new Date(Date.now() - 60_000).toISOString(), updatedAt: new Date().toISOString(), ...overrides,
  } as SpawnedWorkItem;
}

test('background work that ends is the only work announced', () => {
  const now = Date.now();
  const recent = new Date(now - 1000).toISOString();
  const prev = [work('train'), work('server', { role: 'service' }), work('sub', { kind: 'agent' }), work('stopped'), work('done-before', { status: 'exited' })];
  const next = [
    work('train', { status: 'exited', exitCode: 0, finishedAt: recent }),
    work('server', { role: 'service', status: 'failed', exitCode: 1, finishedAt: recent }),
    work('sub', { kind: 'agent', status: 'exited', finishedAt: recent }),
    work('stopped', { status: 'exited', outcome: 'cancelled', finishedAt: recent }),
    work('done-before', { status: 'exited' }),
    work('new', { status: 'exited', finishedAt: recent }),
  ];
  const ended = finishedBackgroundWork(prev, next, now);
  assert.deepEqual(ended.map((item) => item.id), ['train', 'server']);
  assert.deepEqual(ended.map(finishedWorkText), ['Terminó: job train', 'Falló: job server']);
  assert.deepEqual(finishedBackgroundWork(undefined, next, now), [], 'a first read announces nothing');
  assert.equal(freshEnding(new Date(now - 10 * 60_000).toISOString(), now), false);

  const { store, fired } = noticeSubject();
  const changed = (items: SpawnedWorkItem[]) => ({ tabId: 'tab-other', chatId: 'chat-tab-other', items });
  store.noticeFinishedWork(changed([work('train')]));
  store.onSpawnedWorkChanged(changed([work('train')]));
  store.noticeFinishedWork(changed([work('train', { status: 'exited', exitCode: 0, finishedAt: new Date().toISOString() })]));
  assert.deepEqual(fired, [['Otro chat', 'Terminó: job train', 'tab-other']]);

  const quiet = noticeSubject({ jobs: false });
  quiet.store.onSpawnedWorkChanged(changed([work('train')]));
  quiet.store.noticeFinishedWork(changed([work('train', { status: 'failed', finishedAt: new Date().toISOString() })]));
  assert.deepEqual(quiet.fired, []);
});

test('the real fireNotify plays the sound only with the setting on, and a toast still shows', () => {
  for (const sound of [false, true]) {
    const store = subject({ notices: { ...DEFAULT_GENERAL_SETTINGS.notices, sound } });
    let played = 0;
    store.playNoticeSound = () => { played++; };
    store.fireNotify('Chat', 'Terminó.', 'tab-open');
    assert.equal(played, sound ? 1 : 0);
    assert.equal(store.state.toasts.length, 1);
  }
});

test('changing a General setting saves it into the daemon blob without touching other keys', async () => {
  const store = subject();
  const writes: Array<Record<string, unknown>> = [];
  const previousWindow = (globalThis as any).window;
  (globalThis as any).window = {
    api: {
      getSettings: async () => ({ settings: { chatTitles: { mode: 'agent', providerId: '', model: '' }, prApprover: 'someone' }, settingsRevision: 'rev-1' }),
      setSettings: async (blob: Record<string, unknown>) => { writes.push(blob); return blob; },
    },
  };
  try {
    store.state.chatTitles = { mode: 'agent', providerId: '', model: '' };
    store.setGeneral({ ...store.state.general, whileWorking: 'steer', notices: { ...store.state.general.notices, sound: true } });
    clearTimeout(store.settingsSaveTimer);
    await store.flushModelPreferences();
  } finally {
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
  }
  assert.equal(writes.length, 1);
  assert.equal(writes[0].prApprover, 'someone');
  assert.equal(writes[0].whileWorking, 'steer');
  assert.deepEqual(writes[0].notices, { needsYou: true, turnEnded: true, jobs: true, sound: true });
  assert.deepEqual(writes[0].dictation, { lang: 'es' });
});
