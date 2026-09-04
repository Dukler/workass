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

test('agent settings render compact status cards and accessible manual switches', async (t) => {
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
  store.state.settingsSection = 'agentes';
  store.state.providers = [
    { id: 'qwen', name: 'Qwen Code', badge: 'agent', enabled: true, status: 'ready', resolvedCommand: '/bin/qwen' },
    { id: 'devin', name: 'Devin', badge: 'agent', enabled: false, status: 'inactive', disabledByUser: true },
  ];
  store.state.groups = [{ providerId: 'qwen', providerName: 'Qwen Code', models: [{ modelId: 'qwen-3', name: 'Qwen 3' }] }];

  const html = renderToStaticMarkup(React.createElement(Settings));
  assert.match(html, /class="lrow agent-provider-row/);
  assert.match(html, /role="switch"[^>]*aria-checked="true"[^>]*aria-label="Desactivar Qwen Code"/);
  assert.match(html, /role="switch"[^>]*aria-checked="false"[^>]*aria-label="Activar Devin"/);
  assert.match(html, /Oculto del selector de modelos y de las actualizaciones/);

  const css = readFileSync(new URL('../src/styles/app.css', import.meta.url), 'utf8');
  assert.match(css, /\.stgs \.agent-switch/);
  assert.match(css, /\.stgs \.agent-provider-row\.disabled/);
});
