import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

// 2026-11-09: the owner chose markdown style "C" (mock .dev/mocks/markdown-v2.html):
// horizontal rules only, content-width tables with alternating rows, aligned
// numbers, underlined links, a keyboard-reachable scroll region for wide tables.
const css = readFileSync(new URL('../src/styles/app.css', import.meta.url), 'utf8');
const block = readFileSync(new URL('../src/markdown/MarkdownBlock.tsx', import.meta.url), 'utf8');
const rule = (selector: string) => {
  const at = css.indexOf(`${selector} {`);
  assert.ok(at >= 0, `missing rule ${selector}`);
  return css.slice(at, css.indexOf('}', at));
};

test('tables have horizontal rules only, fit their content and stripe alternate rows', () => {
  assert.match(rule('table.mdtable'), /width: max-content; max-width: 100%/);
  assert.match(rule('table.mdtable'), /font-variant-numeric: tabular-nums/);
  assert.doesNotMatch(rule('table.mdtable th, table.mdtable td'), /border:/, 'no vertical lines or boxed cells');
  assert.match(rule('table.mdtable thead'), /border-bottom: 1\.5px/);
  assert.match(rule('table.mdtable tbody tr:nth-child(2n) td'), /background/);
});

test('links are underlined, never told apart by colour alone', () => {
  assert.match(rule('.p a, .doclist a, table.mdtable a, .mdquote a'), /text-decoration: underline/);
  assert.doesNotMatch(css, /\.p a \{ color: var\(--link\); text-decoration: none; \}/);
});

test('a wide table is a focusable, named scroll region', () => {
  assert.match(block, /className="mdtable-wrap" tabIndex=\{0\} role="region" aria-label="Tabla"/);
  assert.match(rule('.mdtable-wrap:focus-visible'), /outline/);
});
