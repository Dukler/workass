import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { after, before, test } from 'node:test';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { createServer, type ViteDevServer } from 'vite';
import type { ToolEvent } from '../src/store/types.ts';

let server: ViteDevServer;
let ToolGroup: React.ComponentType<{ tools: ToolEvent[] }>;

before(async () => {
  const root = fileURLToPath(new URL('..', import.meta.url));
  server = await createServer({
    root,
    server: { middlewareMode: true },
    appType: 'custom',
    logLevel: 'silent',
  });
  ({ ToolGroup } = await server.ssrLoadModule('/src/components/messages.tsx') as {
    ToolGroup: React.ComponentType<{ tools: ToolEvent[] }>;
  });
});

after(async () => { await server.close(); });

test('a collapsed multi-call tool group does not mount tool details or their heavy payloads', () => {
  const tools: ToolEvent[] = [
    {
      key: 'heavy-1', at: 0, kind: 'tool', id: 'heavy-1', toolKind: 'execute',
      title: 'Run expensive command', status: 'completed', command: 'heavy-command-payload',
      terminalId: null, input: null, output: 'heavy-output-payload', location: null,
    },
    {
      key: 'heavy-2', at: 1, kind: 'tool', id: 'heavy-2', toolKind: 'execute',
      title: 'Run second command', status: 'completed', command: 'second-command-payload',
      terminalId: null, input: null, output: 'second-output-payload', location: null,
    },
  ];

  // A run of 2+ calls collapses to one summary line; the per-call details (and
  // their heavy payloads) stay unmounted until it is expanded.
  const html = renderToStaticMarkup(React.createElement(ToolGroup, { tools }));
  assert.match(html, /2 comandos/);
  assert.match(html, /2 llamadas/);
  assert.doesNotMatch(html, /class="tg-body"/);
  assert.doesNotMatch(html, /class="evt/);
  assert.doesNotMatch(html, /Run expensive command|Run second command/);
  assert.doesNotMatch(html, /heavy-command-payload|heavy-output-payload|second-command-payload|second-output-payload/);
});

// Regression (user report 2026-07-15): a group of exactly ONE call wrapped the
// call in a summary line AND a detail row, so the title showed twice and the
// output took two opens. A single call now renders inline as its own line: one
// title, no group wrapper, output still lazy behind the row's own toggle.
test('a single tool call renders inline — title once, no group wrapper, output lazy', () => {
  const tool: ToolEvent = {
    key: 'solo', at: 0, kind: 'tool', id: 'solo', toolKind: 'execute',
    title: 'mcp__workass-agent__workass_read_chat', status: 'completed', command: null,
    terminalId: null, input: null, output: 'verbose-read-chat-output', location: null,
  };

  const html = renderToStaticMarkup(React.createElement(ToolGroup, { tools: [tool] }));
  // The row itself IS the collapsed line now — one evt, no summary/body wrapper.
  assert.match(html, /class="toolsolo"/);
  assert.match(html, /class="evt/);
  assert.doesNotMatch(html, /class="tg-summary"|class="tg-body"/);
  // The title appears exactly ONCE (the old summary+detail showed it twice).
  const titleCount = html.split('mcp__workass-agent__workass_read_chat').length - 1;
  assert.equal(titleCount, 1);
  // The heavy output stays lazy until the row is clicked open.
  assert.doesNotMatch(html, /verbose-read-chat-output/);
});

test('a single-call image result renders as assistant media before its tool row', () => {
  const tool: ToolEvent = {
    key: 'image-tool', at: 0, kind: 'tool', id: 'image-tool', toolKind: 'read',
    title: 'Render comparison', status: 'completed', command: null,
    terminalId: null, input: null, output: 'hidden verbose output', location: null,
    images: [{ mimeType: 'image/png', data: 'cG5n', name: 'Option A' }],
  };

  const html = renderToStaticMarkup(React.createElement(ToolGroup, { tools: [tool] }));
  const gallery = html.indexOf('class="tool-images"');
  const toolRow = html.indexOf('class="toolsolo"');
  assert.ok(gallery >= 0 && toolRow > gallery, 'image media must be a sibling before the tool row');
  assert.match(html, /class="tool-images"/);
  assert.match(html, /src="data:image\/png;base64,cG5n"/);
  assert.match(html, /alt="Option A"/);
  assert.doesNotMatch(html, /hidden verbose output/);
});

test('a multi-call image result renders as assistant media before its grouped tool row', () => {
  const tools: ToolEvent[] = [
    {
      key: 'image-tool', at: 0, kind: 'tool', id: 'image-tool', toolKind: 'read',
      title: 'View image', status: 'completed', command: null,
      terminalId: null, input: null, output: 'hidden image-tool output', location: null,
      images: [{ mimeType: 'image/png', data: 'cG5n', name: 'Visual QA' }],
    },
    {
      key: 'command-tool', at: 1, kind: 'tool', id: 'command-tool', toolKind: 'execute',
      title: 'Inspect source', status: 'completed', command: 'hidden-command',
      terminalId: null, input: null, output: 'hidden command output', location: null,
    },
  ];

  const html = renderToStaticMarkup(React.createElement(ToolGroup, { tools }));
  const group = html.indexOf('class="toolgroup"');
  const summary = html.indexOf('class="tg-summary"');
  const gallery = html.indexOf('class="tool-images"');
  assert.ok(gallery >= 0 && group > gallery, 'image media must be a sibling before the grouped tool row');
  assert.ok(summary > group, 'collapsed status line must remain inside its own group');
  assert.match(html, /aria-expanded="false"/);
  assert.match(html, /src="data:image\/png;base64,cG5n"/);
  assert.doesNotMatch(html, /<details|class="tg-body"|hidden image-tool output|hidden-command|hidden command output/);
});

// v2 contract (approved mock toolrow-redesign, 2026-07-15): the tool NAME never
// yields to the detail (no flex-shrink) so «Terminal» can't collapse to «T…»,
// but a pathological sentence-length title is still capped so it can't blow the
// row width open.
test('tool titles never shrink below their text, yet stay capped against overflow', () => {
  const css = readFileSync(new URL('../src/styles/app.css', import.meta.url), 'utf8');
  const rule = css.match(/\.evt-title\s*\{([^}]*)\}/)?.[1] ?? '';
  assert.match(rule, /flex:\s*0\s+0\s+auto/);
  assert.match(rule, /max-width:\s*65%/);
  assert.match(rule, /overflow:\s*hidden/);
  assert.match(rule, /text-overflow:\s*ellipsis/);
  assert.match(rule, /white-space:\s*nowrap/);
});
