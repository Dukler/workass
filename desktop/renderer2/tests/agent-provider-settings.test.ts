import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import test from 'node:test';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { createServer } from 'vite';

test('manual provider disable hides its updates and enable probes it again', async (t) => {
  const previousWindow = (globalThis as any).window;
  const calls: unknown[][] = [];
  (globalThis as any).window = {
    api: {
      providersToggle: async (id: string, enabled: boolean) => {
        calls.push(['toggle', id, enabled]);
        return [{
          id, name: 'Qwen Code', enabled, status: enabled ? 'inactive' : 'inactive',
          disabledByUser: !enabled,
        }];
      },
      providersDetect: async (opts: { provider?: string }) => {
        calls.push(['detect', opts]);
        return { providers: [{ id: 'qwen', name: 'Qwen Code', enabled: true, status: 'ready' }] };
      },
    },
  };
  t.after(() => {
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
  });

  const server = await createServer({
    root: fileURLToPath(new URL('..', import.meta.url)),
    server: { middlewareMode: true }, appType: 'custom', logLevel: 'silent',
  });
  let singleton: { clearToastTimers(): void } | undefined;
  let target: { clearToastTimers(): void; state: Record<string, any>; toggleProvider(providerId: string, enabled: boolean): Promise<boolean> } | undefined;
  t.after(async () => { target?.clearToastTimers(); singleton?.clearToastTimers(); await server.close(); });
  const module = await server.ssrLoadModule('/src/store/store.ts') as {
    store: { clearToastTimers(): void };
    Store: new () => {
      state: Record<string, any>;
      clearToastTimers(): void;
      toggleProvider(providerId: string, enabled: boolean): Promise<boolean>;
    };
  };
  singleton = module.store;
  const targetStore = new module.Store();
  target = targetStore;
  targetStore.state.providers = [{ id: 'qwen', name: 'Qwen Code', enabled: true, status: 'ready' }];
  targetStore.state.providersUpdates = [{
    providerId: 'qwen', cli: 'qwen', installed: '1.0.0', latest: '1.0.1', updateAvailable: true,
  }];

  assert.equal(await targetStore.toggleProvider('qwen', false), true);
  assert.deepEqual(calls, [['toggle', 'qwen', false]]);
  assert.deepEqual(targetStore.state.providersUpdates, []);
  assert.equal(targetStore.state.providers[0]?.disabledByUser, true);
  (targetStore as any).onProvidersUpdates({
    updates: [{ providerId: 'qwen', cli: 'qwen', installed: '1.0.0', latest: '1.0.1', updateAvailable: true }],
  });
  assert.deepEqual(targetStore.state.providersUpdates, [], 'a replay cannot restore a disabled provider update');

  assert.equal(await targetStore.toggleProvider('qwen', true), true);
  assert.deepEqual(calls, [
    ['toggle', 'qwen', false],
    ['toggle', 'qwen', true],
    ['detect', { provider: 'qwen' }],
  ]);
  assert.equal(targetStore.state.providers[0]?.status, 'ready');
});

// providers:detect probes every CLI and local server (2-4.5 s) and holds up
// the connection; a changed catalog is the daemon's own news, so the digest
// re-reads providers:list and the providers:catalog snapshot. The Settings
// refresh still runs detection.
test('a catalog digest re-reads providers:list and the catalog snapshot, never a detection pass', async (t) => {
  const previousWindow = (globalThis as any).window;
  const calls: unknown[][] = [];
  const group = { providerId: 'codex', providerName: 'Codex', models: [], modes: [], permissionIntents: null, status: 'ready', latencyMs: 812, error: 'a -> b && <c>' };
  const bridge: Record<string, unknown> = {
    providersDetect: async (opts: { provider?: string }) => {
      calls.push(['detect', opts]);
      return { providers: [{ id: 'qwen', name: 'Qwen Code', enabled: true, status: 'ready' }] };
    },
    providersList: async () => {
      calls.push(['list']);
      return [{ id: 'codex', name: 'Codex', enabled: true, status: 'ready' }];
    },
    providersCatalog: async () => {
      calls.push(['catalog']);
      return { models: [], modes: [], groups: [group], defaultProviderId: 'codex' };
    },
  };
  (globalThis as any).window = { api: bridge };
  t.after(() => {
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
  });

  const server = await createServer({
    root: fileURLToPath(new URL('..', import.meta.url)),
    server: { middlewareMode: true }, appType: 'custom', logLevel: 'silent',
  });
  const stores: Array<{ clearToastTimers(): void }> = [];
  t.after(async () => { for (const store of stores) store.clearToastTimers(); await server.close(); });
  const module = await server.ssrLoadModule('/src/store/store.ts') as {
    store: { clearToastTimers(): void };
    Store: new () => { state: Record<string, any>; clearToastTimers(): void; detectProvider(providerId: string): Promise<void> };
  };
  stores.push(module.store);
  const subject = new module.Store();
  stores.push(subject);

  // The daemon hashes the group exactly as Go's json.Marshal writes it.
  const { goJSON } = await server.ssrLoadModule('/src/store/digest-hash.ts') as { goJSON(value: unknown): string };
  const { createHash } = await import('node:crypto');
  const daemonHash = createHash('sha256').update(goJSON(group)).digest('hex');
  const digest = { chats: [], globalRevision: subject.state.globalRevision, catalogHash: { codex: daemonHash }, settingsRevision: '', procHash: '' };
  const scopes: Set<string> = (subject as any).digestSyncScopes(digest);
  assert.equal(scopes.has('catalog'), true, 'a diverged catalog hash schedules the catalog scope');
  await (subject as any).runScopedSync(new Set(['catalog']));
  assert.deepEqual(calls, [['list'], ['catalog']]);
  assert.deepEqual(subject.state.providers.map((provider: { id: string }) => provider.id), ['codex']);
  assert.deepEqual(subject.state.groups.map((entry: { providerId: string }) => entry.providerId), ['codex'], 'the catalog read replaced the groups');
  await waitFor(() => !(subject as any).digestSyncScopes(digest).has('catalog'));
  assert.equal((subject as any).digestSyncScopes(digest).has('catalog'), false, 'the catalog hash converged on the digest');

  // The explicit Settings refresh keeps its detection pass.
  await subject.detectProvider('qwen');
  assert.deepEqual(calls, [['list'], ['catalog'], ['detect', { provider: 'qwen' }]]);

  // A bridge without providers:list (the oldest daemons) is a quiet no-op.
  calls.length = 0;
  delete bridge.providersList;
  delete bridge.providersCatalog;
  delete bridge.providersDetect;
  await assert.doesNotReject((subject as any).runScopedSync(new Set(['catalog'])));
  assert.deepEqual(calls, []);
});

async function waitFor(predicate: () => boolean, timeoutMs = 2000): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  while (!predicate() && Date.now() < deadline) await new Promise((resolve) => setTimeout(resolve, 5));
}

// A daemon from before providers:catalog re-publishes a catalog only from a
// detection pass, and its background refresh re-measures latency without
// publishing. Without a bounded fallback its renderer kept stale groups and
// re-read providers:list on every heartbeat, forever.
test('an older daemon converges through one bounded detection pass, and a stuck digest stops re-syncing', async (t) => {
  const previousWindow = (globalThis as any).window;
  const calls: unknown[][] = [];
  let published: ((catalog: unknown) => void) | null = null;
  const group = { providerId: 'qwen', providerName: 'Qwen Code', models: [], modes: [], status: 'ready', latencyMs: 2048 };
  const bridge: Record<string, unknown> = {
    providersList: async () => { calls.push(['list']); return [{ id: 'qwen', name: 'Qwen Code', enabled: true, status: 'ready' }]; },
    providersDetect: async (opts: unknown) => {
      calls.push(['detect', opts]);
      // Detection re-publishes chat:catalog before it replies.
      published?.({ models: [], modes: [], groups: [group] });
      return { providers: [{ id: 'qwen', name: 'Qwen Code', enabled: true, status: 'ready' }] };
    },
    onChatCatalog: (callback: (catalog: unknown) => void) => { published = callback; },
  };
  (globalThis as any).window = { api: bridge };
  t.after(() => {
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
  });
  const server = await createServer({
    root: fileURLToPath(new URL('..', import.meta.url)),
    server: { middlewareMode: true }, appType: 'custom', logLevel: 'silent',
  });
  const stores: Array<{ clearToastTimers(): void }> = [];
  t.after(async () => { for (const store of stores) store.clearToastTimers(); await server.close(); });
  const module = await server.ssrLoadModule('/src/store/store.ts') as {
    store: { clearToastTimers(): void };
    Store: new () => { state: Record<string, any>; clearToastTimers(): void };
  };
  stores.push(module.store);
  const subject = new module.Store();
  stores.push(subject);
  published = (catalog: unknown) => (subject as any).onCatalog(catalog);

  const { goJSON } = await server.ssrLoadModule('/src/store/digest-hash.ts') as { goJSON(value: unknown): string };
  const { createHash } = await import('node:crypto');
  const digest = { chats: [], globalRevision: subject.state.globalRevision, catalogHash: { qwen: createHash('sha256').update(goJSON(group)).digest('hex') }, settingsRevision: '', procHash: '' };
  assert.equal((subject as any).digestSyncScopes(digest).has('catalog'), true);
  await (subject as any).runScopedSync(new Set(['catalog']));
  assert.deepEqual(calls, [['list'], ['detect', {}]], 'one detection pass, the only re-publish an older daemon has');
  await waitFor(() => !(subject as any).digestSyncScopes(digest).has('catalog'));
  assert.equal((subject as any).digestSyncScopes(digest).has('catalog'), false, 'converged on the re-published catalog');

  // The daemon's catalog drifts again (latency re-measured, nothing published).
  calls.length = 0;
  const drifted = { ...digest, catalogHash: { qwen: 'drifted-hash' } };
  assert.equal((subject as any).digestSyncScopes(drifted).has('catalog'), true, 'a new divergence is synced once');
  await (subject as any).runScopedSync(new Set(['catalog']));
  assert.deepEqual(calls, [['list']], 'the detection fallback is rate-limited');
  for (let beat = 0; beat < 5; beat++) {
    assert.equal((subject as any).digestSyncScopes(drifted).has('catalog'), false, 'the same stuck state is not re-synced every heartbeat');
  }
  // Once the fallback window has passed, it may detect once more.
  (subject as any).catalogDetectFallbackAt = Date.now() - 11 * 60 * 1000;
  assert.equal((subject as any).digestSyncScopes(drifted).has('catalog'), true);
});

test('agent settings render a quiet list with provider marks and accessible manual switches', async (t) => {
  const previousWindow = (globalThis as any).window;
  (globalThis as any).window = { api: { providersToggle: async () => [] } };
  t.after(() => {
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
  });

  const server = await createServer({
    root: fileURLToPath(new URL('..', import.meta.url)),
    server: { middlewareMode: true }, appType: 'custom', logLevel: 'silent',
  });
  let store: { state: Record<string, any>; clearToastTimers(): void } | undefined;
  t.after(async () => { store?.clearToastTimers(); await server.close(); });
  const { Settings } = await server.ssrLoadModule('/src/components/Settings.tsx') as {
    Settings: React.ComponentType;
  };
  ({ store } = await server.ssrLoadModule('/src/store/store.ts') as { store: { state: Record<string, any>; clearToastTimers(): void } });
  (store as any).applyMachineBook({
    machines: [{ machineId: 'm-san', name: 'San-laptop', status: 'ok' }],
    self: { machineId: 'm-mac', name: 'Dukler Mac Studio' },
  });
  assert.equal((store as any).localMachineName(), 'Dukler Mac Studio');
  store.state.machines[0].link = 'ready';
  store.state.machines[0].paired = true;
  store.state.settingsSection = 'agentes';
  store.state.providers = [
    { id: 'codex', name: 'Codex', badge: 'native', assistantBrand: 'gpt', enabled: true, status: 'ready', resolvedCommand: '/bin/codex' },
    { id: 'qwen', name: 'Qwen Code', badge: 'agent', enabled: true, status: 'ready', resolvedCommand: '/bin/qwen' },
  ];
  (store as any).onProvidersList([
    { id: 'devin', name: 'Devin', badge: 'agent', enabled: false, status: 'inactive', disabledByUser: true },
  ], 'm-san');
  store.state.providersUpdates = [{ providerId: 'codex', cli: 'codex', installed: '1.0.0', latest: '1.0.1', updateAvailable: true }];
  store.state.groups = [{ providerId: 'codex', providerName: 'Codex', assistantBrand: 'gpt', models: [{ modelId: 'gpt', name: 'GPT' }] }];

  const html = renderToStaticMarkup(React.createElement(Settings));
  assert.match(html, /class="lrow agent-provider-row/);
  assert.doesNotMatch(html, /agent-machine-head|data-machine-scope/);
  assert.match(html, /class="agent-provider-location local"[^>]*>[\s\S]*?Este equipo[\s\S]*?Dukler Mac Studio/);
  assert.match(html, /class="agent-provider-location remote"[^>]*>[\s\S]*?Remoto[\s\S]*?San-laptop/);
  assert.doesNotMatch(html, />Local</);
  assert.match(html, /aria-label="Agentes por ubicación"/);
  assert.match(html, /class="ic agent-provider-icon brand"/);
  assert.match(html, /viewBox="0 0 425 425"/, 'Devin uses its mark even when an older remote omits assistantBrand');
  assert.match(html, /fill="#6950ef"/, 'Qwen uses its mark rather than a placeholder letter');
  assert.match(html, /class="agent-status-dot run"/);
  assert.match(html, /role="switch"[^>]*aria-checked="true"[^>]*aria-label="Desactivar Qwen Code en Dukler Mac Studio"/);
  assert.match(html, /role="switch"[^>]*aria-checked="false"[^>]*aria-label="Activar Devin en San-laptop"/);
  assert.doesNotMatch(html, /agent-summary|agent-provider-state|agent-switch-text/);
  assert.doesNotMatch(html, />[0-9]+ modelos?</);
  assert.doesNotMatch(html, />Listo</);
  assert.doesNotMatch(html, /Versiones de CLIs|cli-lrow|Actualización/);

  const css = readFileSync(new URL('../src/styles/app.css', import.meta.url), 'utf8');
  assert.match(css, /\.stgs \.agent-switch/);
  assert.match(css, /\.stgs \.agent-status-dot\.run/);
  assert.match(css, /\.stgs \.agent-provider-location\.remote/);
  assert.doesNotMatch(css, /\.stgs \.agent-machine-head/);
  assert.match(css, /\.stgs \.agent-provider-row\.disabled/);
});
