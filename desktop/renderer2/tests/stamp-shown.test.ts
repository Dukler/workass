import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { test } from 'node:test';

// The reply stamp's time ticks only while it can be seen (AssistantMessage
// RelStamp, energy lane 2026-11-13). That is only right while the time is
// hidden unless the reply is hovered or the stamp holds keyboard focus.
test('the reply time shows only on hover or keyboard focus', () => {
  const css = readFileSync(new URL('../src/styles/app.css', import.meta.url), 'utf8');
  assert.match(css, /\.stamp-btn, \.stamp-time \{ opacity: 0; \}/);
  assert.match(css, /\.amsg:hover \.stamp-btn, \.amsg:hover \.stamp-time, \.stamp:focus-within \.stamp-btn, \.stamp:focus-within \.stamp-time \{ opacity: 1; \}/);
  const source = readFileSync(new URL('../src/components/AssistantMessage.tsx', import.meta.url), 'utf8');
  assert.match(source, /reply\.addEventListener\('mouseenter'/);
  assert.match(source, /stamp\?\.addEventListener\('focusin'/);
});

// Finished fades let go (fill backwards), so they do not stay active animations.
test('the streaming fade does not hold its end state', () => {
  const css = readFileSync(new URL('../src/styles/app.css', import.meta.url), 'utf8');
  assert.match(css, /\.amsg\.streaming \.mdfresh \{ animation: mdfresh 280ms steps\(1, end\) backwards; \}/);
  const fade = css.slice(css.indexOf('@keyframes mdfresh {'));
  assert.match(fade.slice(0, fade.indexOf('\n}') + 2), /100% \{ opacity: 1; \}\n\}$/, 'ends at the element\'s own opacity');
});
