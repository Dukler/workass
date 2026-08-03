import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

const tokens = readFileSync(new URL('../src/styles/tokens.css', import.meta.url), 'utf8');
const app = readFileSync(new URL('../src/styles/app.css', import.meta.url), 'utf8');

function block(pattern: RegExp, source: string): string {
  const match = source.match(pattern);
  assert.ok(match, `missing CSS block ${pattern}`);
  return match[1];
}

test('existing base surfaces put the lighter tone on the left and darker tone on main/right', () => {
  const dark = block(/:root\s*\{([\s\S]*?)\}/, tokens);
  const light = block(/:root\[data-theme="light"\]\s*\{([\s\S]*?)\}/, tokens);

  // Keep the approved color pairs byte-for-byte; only their pane assignment is
  // inverted. --side owns the left pane, while --bg owns main + right.
  assert.match(dark, /--side:\s*#151413;/);
  assert.match(dark, /--bg:\s*#0f0e0d;/);
  assert.match(light, /--side:\s*#faf9f7;/);
  assert.match(light, /--bg:\s*#f2f0ed;/);

  assert.match(block(/^#root\s*\{([\s\S]*?)\}/m, app), /background:\s*var\(--bg\);/);
  assert.match(block(/aside\.side\s*\{([\s\S]*?)\}/, app), /background:\s*var\(--side\);/);
  assert.doesNotMatch(block(/aside\.rail\s*\{([\s\S]*?)\}/, app), /background:\s*var\(--side\)/);
  assert.match(block(/\.stgs-nav\s*\{([\s\S]*?)\}/, app), /background:\s*var\(--side\);/);
  assert.match(block(/\.stgs-main\s*\{([\s\S]*?)\}/, app), /background:\s*var\(--bg\);/);
});
