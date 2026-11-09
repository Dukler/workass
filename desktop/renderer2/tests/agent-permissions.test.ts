import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { createServer, type ViteDevServer } from 'vite';
import {
  AGENT_PERMISSIONS_SETTINGS_KEY, DEFAULT_AGENT_PERMISSIONS, agentPermissionsFromSettings, browserProfileApi,
  parseAgentPermissions, permissionLevelMappings, withAgentPermissionsInSettings,
} from '../src/agent-permissions.ts';
import type { CatalogGroup } from '../src/wire/types.ts';

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

test('the owner\'s rules default to what Workass did before and survive the settings envelope', () => {
  assert.deepEqual(parseAgentPermissions(undefined), DEFAULT_AGENT_PERMISSIONS);
  assert.deepEqual(parseAgentPermissions({ subagentRaise: 'no', browserNavigate: 'sometimes', browserDownload: 'ask', browserUpload: 7 }),
    DEFAULT_AGENT_PERMISSIONS, 'downloads cannot ask, and nonsense is the default');
  assert.deepEqual(agentPermissionsFromSettings({ settings: { [AGENT_PERMISSIONS_SETTINGS_KEY]: { subagentRaise: false, browserNavigate: 'ASK', browserUpload: 'block' } } }),
    { subagentRaise: false, browserNavigate: 'ask', browserDownload: 'allow', browserUpload: 'block' });
  const merged = withAgentPermissionsInSettings({ chatTitles: { mode: 'agent' }, modelFavorites: [] }, { ...DEFAULT_AGENT_PERMISSIONS, browserDownload: 'block' });
  assert.deepEqual(merged.chatTitles, { mode: 'agent' }, 'saving the rules keeps every other setting');
  assert.equal((merged[AGENT_PERMISSIONS_SETTINGS_KEY] as { browserDownload: string }).browserDownload, 'block');
});

test('each level names every agent\'s own mode, and a level no agent has is left out', () => {
  const groups = [
    { providerId: 'agent-a', providerName: 'Agent A', models: [], modes: [{ id: 'careful', name: 'Ask before changes' }, { id: 'free', name: 'Full access' }],
      permissionLevels: { ask: 'careful', full: 'free' } },
    { providerId: 'agent-b', providerName: 'Agent B', models: [], modes: [{ id: 'sandbox', name: 'Read-only' }, { id: 'work', name: 'Agent' }],
      permissionLevels: { ask: 'sandbox', edit: 'work' } },
    { providerId: 'agent-old', providerName: 'Old', models: [], modes: [{ id: 'x', name: 'X' }] },
  ] as CatalogGroup[];
  assert.deepEqual(permissionLevelMappings(groups), [
    { level: 'ask', agents: [{ providerId: 'agent-a', agent: 'Agent A', mode: 'Ask before changes' }, { providerId: 'agent-b', agent: 'Agent B', mode: 'Read-only' }] },
    { level: 'edit', agents: [{ providerId: 'agent-b', agent: 'Agent B', mode: 'Agent' }] },
    { level: 'full', agents: [{ providerId: 'agent-a', agent: 'Agent A', mode: 'Full access' }] },
  ]);
  assert.deepEqual(permissionLevelMappings([]), []);
});

test('the browser sessions row needs a shell that can list and close them', () => {
  assert.equal(browserProfileApi(undefined), null);
  assert.equal(browserProfileApi({ supported: true, pages: async () => [] }), null, 'an older shell');
  const api = { profileSites: async () => ({ sites: [] }), clearSessions: async () => ({ sites: [] }) };
  assert.equal(browserProfileApi(api), api);
});

test('a rule changed in Settings is saved into the daemon blob without touching the rest', async () => {
  let blob: Record<string, unknown> = {
    chatTitles: { mode: 'agent', providerId: 'agent-a', model: 'large' }, modelFavorites: [{ providerId: 'a', modelId: 'b' }], other: 1,
  };
  const writes: Array<Record<string, unknown>> = [];
  const previousWindow = (globalThis as any).window;
  (globalThis as any).window = {
    api: {
      getSettings: async () => ({ settings: blob, settingsRevision: 'rev-1' }),
      setSettings: async (next: Record<string, unknown>) => { writes.push(next); blob = next; return next; },
    },
  };
  try {
    const subject = new StoreCtor();
    stores.push(subject);
    await subject.hydrateSettings();
    assert.deepEqual(subject.state.agentPermissions, DEFAULT_AGENT_PERMISSIONS);
    assert.deepEqual(subject.state.chatTitles, { mode: 'agent', providerId: 'agent-a', model: 'large' });
    subject.setAgentPermissions({ ...subject.state.agentPermissions, subagentRaise: false, browserUpload: 'ask' });
    clearTimeout(subject.settingsSaveTimer);
    await subject.flushModelPreferences();
    assert.equal(writes.length, 1);
    assert.deepEqual(writes[0][AGENT_PERMISSIONS_SETTINGS_KEY], { subagentRaise: false, browserNavigate: 'allow', browserDownload: 'allow', browserUpload: 'ask' });
    assert.equal(writes[0].other, 1, 'a key this page does not own is kept');
    assert.deepEqual(writes[0].chatTitles, { mode: 'agent', providerId: 'agent-a', model: 'large' });

    const reloaded = new StoreCtor();
    stores.push(reloaded);
    await reloaded.hydrateSettings();
    assert.equal(reloaded.state.agentPermissions.subagentRaise, false, 'a reload reads the saved rule');
  } finally {
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
  }
});
