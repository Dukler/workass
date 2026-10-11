import assert from 'node:assert/strict';
import { fileURLToPath } from 'node:url';
import { after, before, test } from 'node:test';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { createServer, type ViteDevServer } from 'vite';

let server: ViteDevServer;
let parseBlocks: (text: string) => Array<{ block: any; sig: string }>;
let MarkdownBlock: React.ComponentType<Record<string, unknown>>;

before(async () => {
  const root = fileURLToPath(new URL('..', import.meta.url));
  server = await createServer({ root, server: { middlewareMode: true }, appType: 'custom', logLevel: 'silent' });
  ({ parseBlocks } = await server.ssrLoadModule('/src/markdown/blocks.ts') as any);
  ({ MarkdownBlock } = await server.ssrLoadModule('/src/markdown/MarkdownBlock.tsx') as any);
});
after(async () => { await server?.close(); });

// 2026-10-11 QA: nested list items were flattened into the parent item's text.
test('nested list items render as a nested list', () => {
  const blocks = parseBlocks('1. first\n2. second\n   - nested a\n   - nested b\n3. third');
  assert.equal(blocks.length, 1);
  const list = blocks[0].block;
  assert.equal(list.kind, 'list');
  assert.deepEqual(list.items.map((item: any) => item.text), ['first', 'second', 'third']);
  assert.deepEqual(list.items[1].sub, { ordered: false, items: [{ text: 'nested a' }, { text: 'nested b' }] });
  const html = renderToStaticMarkup(React.createElement(MarkdownBlock, { sb: blocks[0] }));
  assert.match(html, /<li>second<ul class="doclist"><li>nested a<\/li><li>nested b<\/li><\/ul><\/li>/);
});

test('continuation lines and plain lists are unchanged', () => {
  const [block] = parseBlocks('- one\n  still one\n- two').map((b) => b.block);
  assert.deepEqual(block.items, [{ text: 'one\nstill one' }, { text: 'two' }]);
  assert.equal(parseBlocks('- a\n1. b').length, 2, 'a sibling of the other kind starts a new list');
});

// GFM pieces Claude Code renders and Workass showed raw (2026-10-11 QA).
test('strikethrough, bare URLs and task items render', () => {
  const html = (text: string) => renderToStaticMarkup(React.createElement(React.Fragment, null,
    ...parseBlocks(text).map((sb, i) => React.createElement(MarkdownBlock, { key: i, sb }))));
  assert.match(html('~~gone~~ stays'), /<s>gone<\/s> stays/);
  const link = html('see https://example.com/a?b=1. next');
  assert.match(link, /<a href="https:\/\/example.com\/a\?b=1" target="_blank" rel="noreferrer">https:\/\/example.com\/a\?b=1<\/a>\. next/);
  assert.match(html('[label](https://example.com/x)'), /<a href="https:\/\/example.com\/x"[^>]*>label<\/a>/);
  const tasks = html('- [ ] todo\n- [x] done');
  assert.match(tasks, /<li class="task"><input type="checkbox"[^>]*aria-label="Pendiente"\/>todo<\/li>/);
  assert.match(tasks, /<li class="task"><input type="checkbox"[^>]*aria-label="Hecho" checked=""\/>done<\/li>/);
  assert.doesNotMatch(tasks, /aria-label="Pendiente"[^>]*checked/);
});
