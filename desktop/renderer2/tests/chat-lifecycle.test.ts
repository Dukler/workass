import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { createServer, type ViteDevServer } from 'vite';
import {
  ARCHIVE_AFTER_CHOICES, CHAT_LIFECYCLE_SETTINGS_KEY, DEFAULT_CHAT_LIFECYCLE, SETTLE_AFTER_CHOICES, archiveAfterMs,
  chatLifecycleFromSettings, lifecycleChoices, lifecycleDaysLabel, parseChatLifecycle, settleAfterMs, withChatLifecycleInSettings,
  type ChatLifecycle,
} from '../src/chat-lifecycle.ts';
import { ARCHIVE_AFTER_SETTLED_MS, SIDEBAR_LIFECYCLE_TICK_MS, pageIdleBefore, pageSettledBefore } from '../src/store/session-page.ts';
import type { Chat } from '../src/store/types.ts';

// «Poner en reposo los chats sin actividad» and «Archivar los chats en reposo»
// (Ajustes · Mantenimiento). The defaults keep the sidebar exactly as it was:
// nothing files by age (user law 2026-09-21) and a settled chat archives after
// five days.

const DAY = 24 * 60 * 60 * 1000;

let vite: ViteDevServer;
let StoreCtor: new () => any;
const stores: any[] = [];
let resolveSettled: (chat: unknown, status: string, active: boolean, now: number, touched: number, policy?: ChatLifecycle) => boolean;
let resolveArchived: (chat: unknown, status: string, now: number, touched: number, policy?: ChatLifecycle) => boolean;

before(async () => {
  vite = await createServer({
    root: fileURLToPath(new URL('..', import.meta.url)),
    logLevel: 'silent',
    server: { middlewareMode: true, ws: false },
    appType: 'custom',
  });
  StoreCtor = (await vite.ssrLoadModule('/src/store/store.ts')).Store;
  const sidebar = await vite.ssrLoadModule('/src/components/SidebarV2.tsx');
  resolveSettled = sidebar.resolveSettled;
  resolveArchived = sidebar.resolveArchived;
});

after(async () => {
  for (const store of stores) store.clearToastTimers();
  await vite.close();
});

function chat(over: Partial<Chat> = {}): Chat {
  return {
    id: 'tab-quiet', chatId: 'chat-quiet', title: 'quiet', group: null, cwd: '/tmp/workass-lifecycle', providerId: 'mock',
    currentModelId: null, currentModeId: null, pending: false, messages: [], draft: '', ...over,
  } as Chat;
}

test('the policy defaults to no age filing and the five-day archive, and only whole days survive', () => {
  assert.deepEqual(DEFAULT_CHAT_LIFECYCLE, { settleAfterDays: 0, archiveAfterDays: 5 });
  assert.equal(archiveAfterMs(DEFAULT_CHAT_LIFECYCLE), ARCHIVE_AFTER_SETTLED_MS, 'the default archive is the paged shell\'s');
  assert.deepEqual(parseChatLifecycle(undefined), DEFAULT_CHAT_LIFECYCLE);
  assert.deepEqual(parseChatLifecycle({ settleAfterDays: 7, archiveAfterDays: 0 }), { settleAfterDays: 7, archiveAfterDays: 0 });
  assert.deepEqual(parseChatLifecycle({ settleAfterDays: -1, archiveAfterDays: 1.5 }), DEFAULT_CHAT_LIFECYCLE);
  assert.deepEqual(parseChatLifecycle({ settleAfterDays: '7', archiveAfterDays: 400 }), DEFAULT_CHAT_LIFECYCLE);
  assert.deepEqual(chatLifecycleFromSettings({ settings: { [CHAT_LIFECYCLE_SETTINGS_KEY]: { settleAfterDays: 30, archiveAfterDays: 14 } } }),
    { settleAfterDays: 30, archiveAfterDays: 14 });
  assert.deepEqual(chatLifecycleFromSettings({}), DEFAULT_CHAT_LIFECYCLE);
  const merged = withChatLifecycleInSettings({ chatTitles: { mode: 'agent' } }, { settleAfterDays: 7, archiveAfterDays: 30 });
  assert.deepEqual(merged.chatTitles, { mode: 'agent' }, 'saving the policy keeps every other setting');
  assert.deepEqual(merged[CHAT_LIFECYCLE_SETTINGS_KEY], { settleAfterDays: 7, archiveAfterDays: 30 });
  assert.equal(settleAfterMs({ settleAfterDays: 7, archiveAfterDays: 5 }), 7 * DAY);
});

test('the choices name each value in plain words and keep a value set elsewhere', () => {
  assert.deepEqual(SETTLE_AFTER_CHOICES.map(lifecycleDaysLabel), ['3 días', '7 días', '30 días', 'Nunca']);
  assert.deepEqual(ARCHIVE_AFTER_CHOICES.map(lifecycleDaysLabel), ['5 días', '14 días', '30 días', 'Nunca']);
  assert.equal(lifecycleDaysLabel(1), '1 día');
  assert.deepEqual(lifecycleChoices(SETTLE_AFTER_CHOICES, 7), [3, 7, 30, 0]);
  assert.deepEqual(lifecycleChoices(SETTLE_AFTER_CHOICES, 10), [3, 7, 10, 30, 0], 'a stored value is shown in its place');
});

test('with the default policy nothing files by age and a settled chat archives after five days', () => {
  const now = Date.now();
  assert.equal(resolveSettled(chat(), 'ready', false, now, now - 400 * DAY), false);
  assert.equal(resolveArchived(chat(), 'ready', now, now - 400 * DAY), false);
  const settled = chat({ settled: 'settled', settledAt: now - 4 * DAY });
  assert.equal(resolveArchived(settled, 'ready', now, now - 4 * DAY), false);
  assert.equal(resolveArchived({ ...settled, settledAt: now - 5 * DAY }, 'ready', now, now - 5 * DAY), true);
});

test('«Poner en reposo» files a quiet ready chat by age and only that', () => {
  const now = Date.now();
  const policy = { settleAfterDays: 7, archiveAfterDays: 30 };
  assert.equal(resolveSettled(chat(), 'ready', false, now, now - 6 * DAY, policy), false);
  assert.equal(resolveSettled(chat(), 'ready', false, now, now - 7 * DAY, policy), true);
  assert.equal(resolveSettled(chat(), 'ready', true, now, now - 8 * DAY, policy), true, 'merely looking at a chat is not activity');
  assert.equal(resolveSettled(chat(), 'ready', false, now, 0, policy), false, 'a chat with no activity is never filed');
  assert.equal(resolveSettled(chat({ settled: 'active' }), 'ready', false, now, now - 40 * DAY, policy), false, 'Reactivar holds');
  for (const status of ['approval', 'working', 'parked', 'attention', 'failed', 'done']) {
    assert.equal(resolveSettled(chat(), status, false, now, now - 40 * DAY, policy), false, `a ${status} chat is not filed`);
  }
  assert.equal(resolveSettled(chat({ unread: true }), 'ready', false, now, now - 40 * DAY, policy), false);
  // It archives archiveAfterDays after it settled, that is after touched + settleAfterDays.
  assert.equal(resolveArchived(chat(), 'ready', now, now - 36 * DAY, policy), false);
  assert.equal(resolveArchived(chat(), 'ready', now, now - 37 * DAY, policy), true);
});

test('«Archivar» follows its days, and Nunca keeps every settled chat in the shelf', () => {
  const now = Date.now();
  const settled = chat({ settled: 'settled', settledAt: now - 20 * DAY });
  assert.equal(resolveArchived(settled, 'ready', now, now - 20 * DAY, { settleAfterDays: 0, archiveAfterDays: 14 }), true);
  assert.equal(resolveArchived(settled, 'ready', now, now - 20 * DAY, { settleAfterDays: 0, archiveAfterDays: 30 }), false);
  assert.equal(resolveArchived(chat({ settled: 'settled', settledAt: now - 400 * DAY }), 'ready', now, 0, { settleAfterDays: 0, archiveAfterDays: 0 }), false);
  assert.equal(resolveSettled(settled, 'ready', false, now, now - 20 * DAY, { settleAfterDays: 0, archiveAfterDays: 0 }), true);
});

test('the paged shell boundaries follow the policy', () => {
  const now = 1_800_000_000_000;
  const margin = now - ARCHIVE_AFTER_SETTLED_MS - pageSettledBefore(now);
  assert.ok(margin > SIDEBAR_LIFECYCLE_TICK_MS, 'the default boundary keeps its margin');
  assert.equal(pageSettledBefore(now, 30 * DAY), now - 30 * DAY - margin);
  assert.equal(pageSettledBefore(now, 0), 0, 'with no archive the daemon leaves nothing out');
  assert.equal(pageIdleBefore(now, 7 * DAY, 30 * DAY), now - 37 * DAY - margin);
  assert.equal(pageIdleBefore(now, 0, 30 * DAY), 0);
  assert.equal(pageIdleBefore(now, 7 * DAY, 0), 0);
});

function withWindow<T>(api: Record<string, unknown>, run: () => Promise<T>): Promise<T> {
  const previous = (globalThis as any).window;
  (globalThis as any).window = { api };
  return run().finally(() => {
    if (previous === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previous;
  });
}

test('the store asks the daemon to keep exactly the chats its policy files away', () => {
  const subject = new StoreCtor();
  stores.push(subject);
  subject.state.chatLifecycle = { ...DEFAULT_CHAT_LIFECYCLE };
  const off = subject.sessionPageRequest();
  assert.equal(off.idleBefore, undefined, 'no age filing, no idleBefore');
  assert.ok(off.settledBefore > 0);
  subject.state.chatLifecycle = { settleAfterDays: 7, archiveAfterDays: 30 };
  const on = subject.sessionPageRequest();
  assert.ok(on.idleBefore > 0 && on.idleBefore < on.settledBefore - 6 * DAY);
  subject.state.chatLifecycle = { settleAfterDays: 7, archiveAfterDays: 0 };
  const never = subject.sessionPageRequest();
  assert.equal(never.settledBefore, 0);
  assert.equal(never.idleBefore, undefined);
});

test('the policy is saved into the settings blob with every other key, and read back from it', async () => {
  const subject = new StoreCtor();
  stores.push(subject);
  subject.schedulePersist = () => {};
  let stored: Record<string, unknown> = { chatTitles: { mode: 'agent', providerId: 'a', model: 'm' }, modelFavorites: [{ providerId: 'p', modelId: 'x' }], notifications: 'all' };
  const writes: Array<Record<string, unknown>> = [];
  const api = {
    getSettings: async () => ({ settings: stored, settingsRevision: 'rev-1' }),
    setSettings: async (next: Record<string, unknown>) => { writes.push(next); stored = next; return next; },
  };
  await withWindow(api, async () => {
    await subject.hydrateSettings();
    subject.setChatLifecycle({ settleAfterDays: 7, archiveAfterDays: 30 });
    clearTimeout(subject.settingsSaveTimer);
    await subject.flushModelPreferences();
  });
  assert.equal(writes.length, 1);
  assert.deepEqual(writes[0][CHAT_LIFECYCLE_SETTINGS_KEY], { settleAfterDays: 7, archiveAfterDays: 30 });
  assert.equal(writes[0].notifications, 'all', 'a key this window does not own survives');
  assert.deepEqual(writes[0].chatTitles, { mode: 'agent', providerId: 'a', model: 'm' });

  const other = new StoreCtor();
  stores.push(other);
  await withWindow(api, () => other.hydrateSettings());
  assert.deepEqual(other.state.chatLifecycle, { settleAfterDays: 7, archiveAfterDays: 30 }, 'another window adopts the saved policy');
});
