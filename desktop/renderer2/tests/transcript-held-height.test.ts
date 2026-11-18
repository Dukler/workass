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

// 2026-11-18 ("spamming the right pane makes the text bounce up A LOT"): with a
// turn running, every render re-cut the window, and right after the column
// changed width the top spacer still stood in for unmounted rows by their
// heights at the old width. The re-cut corrected it by ~200 px; the hold kept
// the old total, so that much empty page sat under the last line until the
// release 700 ms later. Rig (real chats, forced re-renders, 1440x900): worst
// one-frame move of visible text 151-194 px before, at most a glide frame
// (22-87 px) after, and no drop at the release. The hold now moves with the
// unit it was taken under, measured by its first box (units are display:
// contents and have no box of their own).
test('a commit moves the held height by however much the held unit moved', () => {
  const commit = source.slice(source.indexOf('const commitLayout = (doc: HTMLDivElement) => {'), source.indexOf('// A window shorter than the screen'));
  assert.match(commit, /fitTopPad\(\);\s*rebaseHeldHeight\(doc\);/);
  const rebase = source.slice(source.indexOf('const rebaseHeldHeight = '), source.indexOf('const commitLayout = '));
  assert.match(rebase, /if \(!stick\.current \|\| heldHeight\.current <= 0 \|\| !held\) return;/);
  assert.match(rebase, /if \(doc\.offsetWidth !== heldWidth\.current\) return;/);
  assert.match(rebase, /heldHeight\.current = Math\.max\(0, heldHeight\.current \+ moved\);/);
  const unitTop = source.slice(source.indexOf('const unitTop = '), source.indexOf('const lastUnit = '));
  assert.match(unitTop, /firstBox\(unit\)/);
  const onResize = source.slice(source.indexOf('const onResize = () => {'), source.indexOf('const mo = new MutationObserver'));
  assert.match(onResize, /heldHeight\.current = height;\s*heldUnit\.current = lastUnit\(doc\);/);
});
