import assert from 'node:assert/strict';
import { fileURLToPath } from 'node:url';
import { after, before, test } from 'node:test';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { createServer, type ViteDevServer } from 'vite';

let server: ViteDevServer;
let parseBlocks: (text: string) => Array<{ block: { kind: string }; sig: string }>;
let MarkdownBlock: React.ComponentType<Record<string, unknown>>;
let copyCodeText: (
  raw: string,
  clipboard?: { writeText: (text: string) => Promise<void> } | null,
  shellBridge?: { supported: boolean; copyText?: (text: string) => Promise<boolean> } | null,
) => Promise<boolean>;

before(async () => {
  const root = fileURLToPath(new URL('..', import.meta.url));
  server = await createServer({ root, server: { middlewareMode: true }, appType: 'custom', logLevel: 'silent' });
  ({ parseBlocks } = await server.ssrLoadModule('/src/markdown/blocks.ts') as { parseBlocks: typeof parseBlocks });
  ({ MarkdownBlock } = await server.ssrLoadModule('/src/markdown/MarkdownBlock.tsx') as { MarkdownBlock: typeof MarkdownBlock });
  ({ copyCodeText } = await server.ssrLoadModule('/src/markdown/CodeBlock.tsx') as { copyCodeText: typeof copyCodeText });
});

after(async () => { await server.close(); });

test('fenced code renders a language header, visible copy action, and syntax tokens', () => {
  const [block] = parseBlocks('```typescript\nconst answer: number = 42;\nconsole.log("ready"); // shown\n```');
  const html = renderToStaticMarkup(React.createElement(MarkdownBlock, { sb: block }));
  assert.match(html, /class="codebox"[^>]+data-language="typescript"/);
  assert.match(html, /class="code-lang">typescript<\/span>/);
  assert.match(html, /class="code-copy"[^>]+aria-label="Copiar código"/);
  assert.match(html, />Copiar<\/span>/);
  assert.match(html, /class="tk-kw">const<\/span>/);
  assert.match(html, /class="tk-ty">number<\/span>/);
  assert.match(html, /class="tk-num">42<\/span>/);
  assert.match(html, /class="tk-fn">log<\/span>/);
  assert.match(html, /class="tk-str">&quot;ready&quot;<\/span>/);
  assert.match(html, /class="tk-cm">\/\/ shown<\/span>/);
});

test('Pine Script fences receive language-aware highlighting', () => {
  const [block] = parseBlocks('```pine\n//@version=6\nindicator("Trend")\nif close > open\n    plot(close)\n```');
  const html = renderToStaticMarkup(React.createElement(MarkdownBlock, { sb: block }));
  assert.match(html, /data-language="pine script"/);
  assert.match(html, /class="tk-cm">\/\/@version=6<\/span>/);
  assert.match(html, /class="tk-fn">indicator<\/span>/);
  assert.match(html, /class="tk-kw">if<\/span>/);
  assert.match(html, /class="tk-fn">plot<\/span>/);
});

test('an open streaming fence stays one plain text node until it closes', () => {
  const [block] = parseBlocks('```typescript\nconst answer: number = 42;');
  const html = renderToStaticMarkup(React.createElement(MarkdownBlock, { sb: block }));
  assert.match(html, /<code>const answer: number = 42;<\/code>/);
  assert.doesNotMatch(html, /class="tk-/);
});

test('an oversized closed fence stays plain while preserving its exact text', () => {
  const raw = `const payload = "${'x'.repeat(40 * 1024)}";`;
  const [block] = parseBlocks(`\`\`\`typescript\n${raw}\n\`\`\``);
  const html = renderToStaticMarkup(React.createElement(MarkdownBlock, { sb: block }));
  assert.doesNotMatch(html, /class="tk-/);
  assert.match(html, /<code>const payload = &quot;x+/);
  assert.match(html, /x+&quot;;<\/code>/);
});

test('copy action writes the exact code bytes and reports clipboard failure', async () => {
  const writes: string[] = [];
  const raw = 'line 1\n  line 2\n';
  assert.equal(await copyCodeText(raw, { writeText: async (text) => { writes.push(text); } }), true);
  assert.deepEqual(writes, [raw]);
  assert.equal(await copyCodeText(raw, { writeText: async () => { throw new Error('denied'); } }), false);
  assert.equal(await copyCodeText(raw, null), false);
});

test('copy action prefers the Electron native clipboard and falls back to the page clipboard', async () => {
  const nativeWrites: string[] = [];
  const pageWrites: string[] = [];
  const raw = 'console.log("windows clipboard");\n';
  assert.equal(await copyCodeText(
    raw,
    { writeText: async (text) => { pageWrites.push(text); } },
    { supported: true, copyText: async (text) => { nativeWrites.push(text); return true; } },
  ), true);
  assert.deepEqual(nativeWrites, [raw]);
  assert.deepEqual(pageWrites, [], 'a successful native write must not duplicate through Chromium');

  assert.equal(await copyCodeText(
    raw,
    { writeText: async (text) => { pageWrites.push(text); } },
    { supported: true, copyText: async () => false },
  ), true);
  assert.deepEqual(pageWrites, [raw], 'plain-browser copy remains the fallback');
});
