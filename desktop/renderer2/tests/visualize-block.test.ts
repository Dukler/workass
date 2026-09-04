import assert from 'node:assert/strict';
import { fileURLToPath } from 'node:url';
import test from 'node:test';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { createServer } from 'vite';

test('visualization failure card exposes a bounded actionable reason', async (t) => {
  const server = await createServer({
    root: fileURLToPath(new URL('..', import.meta.url)),
    server: { middlewareMode: true }, appType: 'custom', logLevel: 'silent',
  });
  t.after(async () => { await server.close(); });

  const { VisualizeBlock } = await server.ssrLoadModule('/src/markdown/VisualizeBlock.tsx') as {
    VisualizeBlock: React.ComponentType<Record<string, unknown>>;
  };
  const html = renderToStaticMarkup(React.createElement(VisualizeBlock, {
    error: 'visualization path must stay inside Workass visualizations storage',
  }));

  assert.match(html, /No se pudo cargar/);
  assert.match(html, /fuera de la carpeta de visualizaciones permitida/);
  assert.match(html, /role="alert"/);
  assert.doesNotMatch(html, /Reintentar/);
});
