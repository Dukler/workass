import assert from 'node:assert/strict';
import { fileURLToPath } from 'node:url';
import test from 'node:test';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { createServer } from 'vite';

// A Workass wake-up notice is one quiet line, never a user bubble (mock
// completion-notice.html, approved 2026-10-16): agent glyph, name, a gray
// outcome word, the result folded behind a chevron.
test('a subagent notice renders as one quiet line, result folded, never red', async (t) => {
  const root = fileURLToPath(new URL('..', import.meta.url));
  const server = await createServer({ root, server: { middlewareMode: true }, appType: 'custom', logLevel: 'silent' });
  t.after(async () => { await server.close(); });
  const { NoticeLine } = await server.ssrLoadModule('/src/components/NoticeLine.tsx') as {
    NoticeLine: React.ComponentType<{ notice: { kind: string; title?: string; status?: string; body?: string } }>;
  };
  const done = renderToStaticMarkup(React.createElement(NoticeLine, { notice: { kind: 'subagent', title: 'Revisión del diff', status: 'done', body: 'Dos defectos' } }));
  assert.match(done, /<button type="button" class="note" aria-expanded="false"/);
  assert.match(done, /class="nm">Revisión del diff<\/span><span class="st">terminó<\/span><span class="cv"/);
  assert.doesNotMatch(done, /Dos defectos/, 'the result starts folded');
  assert.doesNotMatch(done, /userpill|Internal agent completion notice/);
  const failed = renderToStaticMarkup(React.createElement(NoticeLine, { notice: { kind: 'subagent', title: 'Pruebas', status: 'failed' } }));
  assert.match(failed, /class="st">falló<\/span>/);
  assert.doesNotMatch(failed, /class="cv"|aria-expanded/, 'nothing to unfold without a result');
});
