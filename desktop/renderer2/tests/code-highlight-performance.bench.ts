import assert from 'node:assert/strict';
import { fileURLToPath } from 'node:url';
import { after, before, test } from 'node:test';
import type { ReactNode } from 'react';
import { createServer, type ViteDevServer } from 'vite';

let server: ViteDevServer;
let renderCode: (raw: string, language?: string, keyBase?: string) => ReactNode[];

before(async () => {
  const root = fileURLToPath(new URL('..', import.meta.url));
  server = await createServer({ root, server: { middlewareMode: true }, appType: 'custom', logLevel: 'silent' });
  ({ renderCode } = await server.ssrLoadModule('/src/markdown/inline.tsx') as { renderCode: typeof renderCode });
});

after(async () => { await server.close(); });

test('benchmark: a 128 KiB code block highlights in linear-time budget', () => {
  const line = 'func projectRecentArchive(chatID string, tail int) bool { return tail > 0 && chatID != "" }\n';
  const raw = line.repeat(Math.ceil((128 * 1024) / line.length)).slice(0, 128 * 1024);
  const started = process.hrtime.bigint();
  const nodes = renderCode(raw, 'go', 'large-code');
  const elapsedMs = Number(process.hrtime.bigint() - started) / 1_000_000;

  assert.ok(nodes.length > 1_000, 'fixture must exercise many syntax tokens');
  assert.ok(elapsedMs < 400, `128 KiB code highlighting took ${elapsedMs.toFixed(1)}ms; expected <400ms`);
});
