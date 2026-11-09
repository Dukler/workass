import assert from 'node:assert/strict';
import { after, before, describe, test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { createServer, type ViteDevServer } from 'vite';

// 2026-11-08: a page frame forgot its height on every reload (an update
// restarts the app), so each page grew from its placeholder the first time it
// scrolled into view and moved everything under the reader. Heights now
// outlive a reload.

// The localStorage stub lives inside this suite: run in one process with other
// files (the suite's --test-isolation=none), a file-level hook would leave it in
// place for every other file's tests.
describe('page frame heights', () => {
  const store = new Map<string, string>();
  let vite: ViteDevServer;
  let frames: { rememberedHeight(key: string): number | undefined; rememberHeight(key: string, height: number): void };

  before(async () => {
    store.set('workass.htmlFrameHeights', JSON.stringify([['2026:abc', 372], ['bad', 'x'], ['zero', 0]]));
    (globalThis as unknown as { localStorage: unknown }).localStorage = {
      getItem: (key: string) => store.get(key) ?? null,
      setItem: (key: string, value: string) => { store.set(key, value); },
      removeItem: (key: string) => { store.delete(key); },
    };
    vite = await createServer({ root: fileURLToPath(new URL('..', import.meta.url)), logLevel: 'silent', server: { middlewareMode: true }, appType: 'custom' });
    frames = await vite.ssrLoadModule('/src/markdown/html-frames.ts') as typeof frames;
  });

  after(async () => { await vite.close(); delete (globalThis as { localStorage?: unknown }).localStorage; });

  test('a height saved before the reload is there after it; broken entries are dropped', () => {
    assert.equal(frames.rememberedHeight('2026:abc'), 372);
    assert.equal(frames.rememberedHeight('bad'), undefined);
    assert.equal(frames.rememberedHeight('zero'), undefined);
  });

  test('a new height is saved for the next reload', async () => {
    frames.rememberHeight('2026:def', 461);
    await new Promise((resolve) => setTimeout(resolve, 650));
    const saved = new Map(JSON.parse(store.get('workass.htmlFrameHeights') ?? '[]'));
    assert.equal(saved.get('2026:def'), 461);
    assert.equal(saved.get('2026:abc'), 372);
  });
});
