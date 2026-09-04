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
  t.after(async () => { await server.close(); });
  const Store = (await server.ssrLoadModule('/src/store/store.ts')).Store as new () => {
    state: Record<string, any>;
    toggleProvider(providerId: string, enabled: boolean): Promise<boolean>;
  };
  const target = new Store();
  target.state.providers = [{ id: 'qwen', name: 'Qwen Code', enabled: true, status: 'ready' }];
  target.state.providersUpdates = [{
    providerId: 'qwen', cli: 'qwen', installed: '1.0.0', latest: '1.0.1', updateAvailable: true,
  }];

  assert.equal(await target.toggleProvider('qwen', false), true);
  assert.deepEqual(calls, [['toggle', 'qwen', false]]);
  assert.deepEqual(target.state.providersUpdates, []);
  assert.equal(target.state.providers[0]?.disabledByUser, true);
  (target as any).onProvidersUpdates({
    updates: [{ providerId: 'qwen', cli: 'qwen', installed: '1.0.0', latest: '1.0.1', updateAvailable: true }],
  });
  assert.deepEqual(target.state.providersUpdates, [], 'a replay cannot restore a disabled provider update');

  assert.equal(await target.toggleProvider('qwen', true), true);
  assert.deepEqual(calls, [
    ['toggle', 'qwen', false],
    ['toggle', 'qwen', true],
    ['detect', { provider: 'qwen' }],
  ]);
  assert.equal(target.state.providers[0]?.status, 'ready');
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
  t.after(async () => { await server.close(); });
  const { Settings } = await server.ssrLoadModule('/src/components/Settings.tsx') as {
    Settings: React.ComponentType;
  };
  const { store } = await server.ssrLoadModule('/src/store/store.ts') as { store: { state: Record<string, any> } };
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
