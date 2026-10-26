import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { createServer, type ViteDevServer } from 'vite';
import type { Chat } from '../src/store/types.ts';

// User report (2026-10-25): after a reload the native page stayed on screen
// with no controls. The shell now takes the page down when the renderer
// reloads; the renderer brings back the floating or full-screen browser it was
// showing, from this window's session, so the page returns with its controls.
let vite: ViteDevServer;
let StoreCtor: new () => any;
const session = new Map<string, string>();
before(async () => {
  (globalThis as { sessionStorage?: unknown }).sessionStorage = {
    getItem: (key: string) => session.get(key) ?? null,
    setItem: (key: string, value: string) => { session.set(key, String(value)); },
    removeItem: (key: string) => { session.delete(key); },
  };
  vite = await createServer({ root: fileURLToPath(new URL('..', import.meta.url)), logLevel: 'silent', server: { middlewareMode: true }, appType: 'custom' });
  StoreCtor = (await vite.ssrLoadModule('/src/store/store.ts')).Store;
});
after(async () => {
  await vite.close();
  delete (globalThis as { sessionStorage?: unknown }).sessionStorage;
});

function chat(id: string): Chat {
  return { id, chatId: `chat-${id}`, title: id, titleLocked: true, group: null, cwd: null, providerId: 'claude',
    currentModelId: null, currentModeId: null, pending: false, messages: [], draft: '', pane: 'browser' } as Chat;
}

test('a reloaded window shows the browser where it was: floating, full screen, or not at all', () => {
  const first = new StoreCtor();
  const a = chat('tab-a');
  const b = chat('tab-b');
  first.state.chats = [a, b];
  first.state.activeId = a.id;
  first.floatBrowser();
  first.state.activeId = b.id;
  first.floatBrowser();
  first.toggleBrowserFullscreen();
  first.clearToastTimers?.();

  const reloaded = new StoreCtor();
  assert.equal(reloaded.browserPresentationFor('tab-a'), 'float');
  assert.equal(reloaded.browserPresentationFor('tab-b'), 'full');
  reloaded.state.chats = [chat('tab-a'), chat('tab-b')];
  reloaded.state.activeId = 'tab-a';
  reloaded.closeFloatingBrowser();
  reloaded.clearToastTimers?.();

  const again = new StoreCtor();
  assert.equal(again.browserPresentationFor('tab-a'), null, 'a closed browser stays closed');
  assert.equal(again.browserPresentationFor('tab-b'), 'full');
  again.clearToastTimers?.();
  session.set('workass.browserPresentation.v1', '{"tab-x":"sideways","tab-y":"float"}');
  const odd = new StoreCtor();
  assert.equal(odd.browserPresentationFor('tab-x'), null, 'an unknown value is dropped');
  assert.equal(odd.browserPresentationFor('tab-y'), 'float');
  odd.clearToastTimers?.();
});
