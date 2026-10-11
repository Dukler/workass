import assert from 'node:assert/strict';
import { fileURLToPath } from 'node:url';
import { after, before, test } from 'node:test';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { createServer, type ViteDevServer } from 'vite';

let server: ViteDevServer;
let renderInline: (text: string) => React.ReactNode[];

before(async () => {
  const root = fileURLToPath(new URL('..', import.meta.url));
  server = await createServer({ root, server: { middlewareMode: true }, appType: 'custom', logLevel: 'silent' });
  ({ renderInline } = await server.ssrLoadModule('/src/markdown/inline.tsx') as { renderInline: typeof renderInline });
});

after(async () => { await server?.close(); });

const html = (text: string) => renderToStaticMarkup(React.createElement(React.Fragment, null, ...renderInline(text)));

// 2026-10-11 QA: intraword underscores were read as emphasis, so snake_case
// identifiers and tool ids lost their underscores and gained italics.
test('underscores inside words are never emphasis', () => {
  assert.equal(html('snake_case_identifier and another_one_here'), 'snake_case_identifier and another_one_here');
  assert.equal(html('use workass_browser_list and workass_browser_snapshot'), 'use workass_browser_list and workass_browser_snapshot');
  assert.equal(html('a__b__c'), 'a__b__c');
});

test('standalone underscore emphasis still renders', () => {
  assert.equal(html('this is _italic_ text'), 'this is <i>italic</i> text');
  assert.equal(html('this is __bold__ text'), 'this is <b>bold</b> text');
  assert.equal(html('(_aside_)'), '(<i>aside</i>)');
});

// 2026-10-11 QA L8: javascript:/data: links reached the DOM as anchors.
test('links with executing schemes render as plain text', () => {
  for (const target of ['javascript:window.__xss=3', 'JAVA\tSCRIPT:alert(1)', 'data:text/html,hi', 'vbscript:x']) {
    const out = html(`[label](${target})`);
    assert.ok(!out.includes('<a'), `${target} rendered an anchor: ${out}`);
    assert.ok(out.includes('label'), out);
  }
  assert.match(html('[ok](https://example.com)'), /<a [^>]*href="https:\/\/example.com"/);
});
