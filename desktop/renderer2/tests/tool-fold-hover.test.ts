import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

const css = readFileSync(new URL('../src/styles/app.css', import.meta.url), 'utf8');

test('multi-call fold hover brightens the whole line without changing resting colors', () => {
  assert.match(css, /\.toolgroup > \.tg-summary:hover \.tg-title,\s*\.toolgroup > \.tg-summary:hover \.tg-count,\s*\.toolgroup > \.tg-summary:hover \.tg-dur,\s*\.toolgroup > \.tg-summary:hover \.tg-ic\s*\{\s*color:\s*var\(--ink\)/);
  assert.match(css, /\.tg-count\s*\{[^}]*color:\s*var\(--muted\)/);
  assert.match(css, /\.tg-dur\s*\{[^}]*color:\s*var\(--faint\)/);
  // a failed group no longer reddens its summary title (read as "weird", 2026-07-16)
  assert.doesNotMatch(css, /\.toolgroup\[data-status="failed"\] \.tg-title\s*\{\s*color:\s*var\(--del\)/);
});

test('single-call fold hover affordance does not depend on output availability', () => {
  assert.match(css, /\.toolsolo > \.evt \.evt-title\s*\{[^}]*text-decoration:\s*underline dotted/);
  assert.match(css, /\.toolsolo > \.evt:hover \.evt-title\s*\{[^}]*color:\s*var\(--ink\)/);
  assert.doesNotMatch(css, /\.toolsolo > \.evt\.hasout \.evt-title/);
  // failure is carried by a red command/path, NOT a red title + ✕ (2026-07-16)
  assert.doesNotMatch(css, /\.evt\[data-status="failed"\] \.evt-title\s*\{\s*color:\s*var\(--del\)/);
  assert.match(css, /\.evt\[data-status="failed"\] \.evt-loc\s*\{\s*color:\s*var\(--del\)/);
});
