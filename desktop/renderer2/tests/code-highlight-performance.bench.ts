import assert from 'node:assert/strict';
import { fileURLToPath } from 'node:url';
import { after, before, test } from 'node:test';
import type { ReactNode } from 'react';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { createServer, type ViteDevServer } from 'vite';

let server: ViteDevServer;
let renderCode: (raw: string, language?: string, keyBase?: string) => ReactNode[];
let parseBlocks: (text: string) => Array<{ block: { kind: string }; sig: string }>;
let MarkdownBlock: React.ComponentType<Record<string, unknown>>;

before(async () => {
  const root = fileURLToPath(new URL('..', import.meta.url));
  server = await createServer({ root, server: { middlewareMode: true }, appType: 'custom', logLevel: 'silent' });
  ({ renderCode } = await server.ssrLoadModule('/src/markdown/inline.tsx') as { renderCode: typeof renderCode });
  ({ parseBlocks } = await server.ssrLoadModule('/src/markdown/blocks.ts') as { parseBlocks: typeof parseBlocks });
  ({ MarkdownBlock } = await server.ssrLoadModule('/src/markdown/MarkdownBlock.tsx') as { MarkdownBlock: typeof MarkdownBlock });
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

test('benchmark: a 512 KiB open streaming fence renders within one frame', () => {
  const line = 'func streamChunk(chatID string) bool { return chatID != "" }\n';
  const raw = line.repeat(Math.ceil((512 * 1024) / line.length)).slice(0, 512 * 1024);
  const [block] = parseBlocks(`\`\`\`go\n${raw}`);
  const started = process.hrtime.bigint();
  const html = renderToStaticMarkup(React.createElement(MarkdownBlock, { sb: block }));
  const elapsedMs = Number(process.hrtime.bigint() - started) / 1_000_000;

  assert.doesNotMatch(html, /class="tk-/);
  assert.ok(elapsedMs < 20, `512 KiB open fence rendering took ${elapsedMs.toFixed(1)}ms; expected <20ms`);
});
