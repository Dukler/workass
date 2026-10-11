import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { createServer, type ViteDevServer } from 'vite';

let vite: ViteDevServer;
let titleFromPrompt: (text: string, max?: number) => string;

before(async () => {
  vite = await createServer({
    root: fileURLToPath(new URL('..', import.meta.url)),
    logLevel: 'silent',
    server: { middlewareMode: true },
    appType: 'custom',
  });
  titleFromPrompt = (await vite.ssrLoadModule('/src/store/store.ts')).titleFromPrompt;
});

after(async () => { await vite.close(); });

// 2026-10-11 QA L5: "[mock:tool-image] # Heading one\n\nS" — raw newlines and a mid-word cut.
test('auto-title collapses whitespace and cuts at a word boundary', () => {
  assert.equal(titleFromPrompt('short prompt'), 'short prompt');
  assert.equal(titleFromPrompt('[mock:tool-image] # Heading one\n\nSecond paragraph here'), '[mock:tool-image] # Heading one…');
  assert.equal(titleFromPrompt('x'.repeat(50)), `${'x'.repeat(34)}…`);
});
