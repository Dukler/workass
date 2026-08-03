import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { createServer, type ViteDevServer } from 'vite';

let vite: ViteDevServer;
let StoreCtor: new () => any;

before(async () => {
  vite = await createServer({
    root: fileURLToPath(new URL('..', import.meta.url)),
    logLevel: 'silent',
    server: { middlewareMode: true },
    appType: 'custom',
  });
  const loaded = await vite.ssrLoadModule('/src/store/store.ts');
  StoreCtor = loaded.Store;
});

after(async () => { await vite.close(); });

test('init reaches hydrated state and constructs the monitor when session:get fails', async () => {
  const previousWindow = (globalThis as any).window;
  const previousDocument = (globalThis as any).document;
  (globalThis as any).window = {
    api: {
      appMeta: async () => ({ rootDir: '/tmp', workspaceDir: '/tmp', version: 'test' }),
      getSession: async () => { throw new Error('fixture getSession failure'); },
    },
    addEventListener: () => {},
  };
  (globalThis as any).document = {
    documentElement: {
      setAttribute: () => {},
      removeAttribute: () => {},
    },
  };
  try {
    const subject = new StoreCtor();
    await subject.init();
    assert.equal(subject.state.hydrated, true);
    assert.ok((subject as any).monitor, 'connection monitor was not constructed before hydration');
    (subject as any).monitor.stop();
  } finally {
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
    if (previousDocument === undefined) delete (globalThis as any).document;
    else (globalThis as any).document = previousDocument;
  }
});
