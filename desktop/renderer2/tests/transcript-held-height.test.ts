import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { test } from 'node:test';

// 2026-11-07: a followed chat was thrown a screen up after a turn that rendered
// an HTML page, and stayed there, when its window was not focused. The turn's
// re-layout shrank the page for an instant; the browser clamped the scroll up,
// the page grew back, and the scroll handler read the move as the reader
// leaving. The held min-height prevents the clamp. Layout can't be measured
// here; the live rig (scratch daemon + headless browser, Reduce Motion on, window
// unfocused, eight real turns) left the old code 1192 px and 526 px above the
// bottom and the fixed code at the bottom every time. These pin the two
// conditions that rig depended on.

const source = readFileSync(new URL('../src/components/Transcript.tsx', import.meta.url), 'utf8');

test('the page height is held whenever the reader is followed, not only when motion is allowed', () => {
  const onResize = source.slice(source.indexOf('const onResize = () => {'), source.indexOf('const mo = new MutationObserver'));
  // The only thing between "followed" and the hold is starting a new hold at
  // a new width (rail-slide.test.ts), never a motion setting.
  assert.match(onResize, /if \(stick\.current && doc\) \{[^]*?const height = doc\.getBoundingClientRect\(\)\.height;\s*if \(height > heldHeight\.current\) \{/);
  assert.doesNotMatch(onResize.slice(0, onResize.indexOf('const height = doc.getBoundingClientRect().height;')), /streamFollow|prefersReducedMotion/);
  assert.doesNotMatch(onResize, /streamFollow\.current && doc/);
});

// 2026-11-09: offsetHeight is rounded. A page 4060.4 px tall was held at
// 4061 px, so setting the hold grew it inside its own resize observer and the
// release 700 ms later shrank it again: a "ResizeObserver loop" error every
// 700 ms while a reader followed the bottom (rig, 1512 and 1920 px).
test('the hold is the exact height, so setting it never resizes the page', () => {
  const onResize = source.slice(source.indexOf('const onResize = () => {'), source.indexOf('const mo = new MutationObserver'));
  assert.doesNotMatch(onResize, /doc\.offsetHeight/);
  assert.match(onResize, /doc\.style\.minHeight = `\$\{height\}px`;/);
});

test('a turn ending does not drop the hold; only leaving the chat does', () => {
  const reset = source.slice(source.indexOf("docRef.current?.style.removeProperty('min-height');\n  }, ["), source.indexOf("docRef.current?.style.removeProperty('min-height');\n  }, [") + 90);
  assert.match(reset, /\}, \[chat\?\.id\]\);/);
  assert.doesNotMatch(reset, /runningMessage/);
});
