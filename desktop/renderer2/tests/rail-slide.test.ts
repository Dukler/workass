import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import { columnSlideFrames, PANE_SLIDE_MS, railSlides } from '../src/store/right-pane.ts';
import { anchorBlockAt, type AnchorBox } from '../src/transcript-window.ts';

// 2026-11-08: opening the right pane rewrapped the whole transcript in one
// frame ("the font changes size and jumps"). The info rail now slides, and the
// reader's line holds through it.
//
// 2026-11-09 ("the text bounces"), measured on every painted frame in Chromium:
// - the grid slide narrowed the column every frame, so lines rewrapped 13
//   times in 220 ms; the column now takes its final width in the first frame
//   and only its left edge travels;
// - a reader following the bottom saw the text climb 127 px while the pane
//   closed and drop 148 px at once 700 ms later: the held min-height kept the
//   old, narrower column's height. It now drops with every width change.

test('only the info rail opening or closing slides', () => {
  assert.equal(railSlides(null, 'rail'), true);
  assert.equal(railSlides('rail', null), true);
  assert.equal(railSlides(null, 'browser'), false, 'the browser view lives in another process and would trail the column');
  assert.equal(railSlides('browser', null), false);
  assert.equal(railSlides('rail', 'browser'), false);
  assert.equal(railSlides('rail', 'rail'), false);
});

test('the closed rail keeps a 0px track so the two layouts interpolate', () => {
  const css = readFileSync(new URL('../src/styles/app.css', import.meta.url), 'utf8');
  assert.match(css, /#root\.no-rail \{ grid-template-columns: var\(--side-w, 288px\) 1fr 0px; \}/);
  assert.match(css, /#root\.no-side\.no-rail \{ grid-template-columns: 1fr 0px; \}/);
  assert.match(css, /#root\.no-rail:not\(\.pane-anim\) aside\.rail \{ display: none; \}/, 'the rail stays drawn while it slides out');
  assert.match(css, /#root\.pane-anim main \.scroll \{ overflow-x: hidden; \}/, 'a column wider than the sliding scroller is clipped, never scrolled sideways');
  assert.doesNotMatch(css, /transition:[^;]*grid-template-columns/, 'a CSS transition would narrow the column frame by frame again');
});

test('the transcript column takes its final width at once; only its left edge travels', () => {
  assert.equal(columnSlideFrames({ x: 297, width: 662 }, { x: 297, width: 662, marginLeft: 0 }), null, 'nothing to animate');
  // 1280 px window: the column fills the content area both ways, so it only
  // narrows, in the first frame, and does not move.
  assert.deepEqual(columnSlideFrames({ x: 297, width: 974 }, { x: 297, width: 662, marginLeft: 0 }), [
    { width: '662px', maxWidth: 'none', marginLeft: '0px' },
    { width: '662px', maxWidth: 'none', marginLeft: '0px' },
  ]);
  // 1600 px window: the centred column starts where it was and glides to
  // where it ends, at its final width all the way.
  assert.deepEqual(columnSlideFrames({ x: 424, width: 1040 }, { x: 297, width: 982, marginLeft: 0 }), [
    { width: '982px', maxWidth: 'none', marginLeft: '127px' },
    { width: '982px', maxWidth: 'none', marginLeft: '0px' },
  ]);
  assert.deepEqual(columnSlideFrames({ x: 297, width: 982 }, { x: 424, width: 1040, marginLeft: 127 }), [
    { width: '1040px', maxWidth: 'none', marginLeft: '0px' },
    { width: '1040px', maxWidth: 'none', marginLeft: '127px' },
  ]);
});

test('the slide reads the final layout before any animation puts the old columns back', () => {
  const app = readFileSync(new URL('../src/components/App.tsx', import.meta.url), 'utf8');
  const slide = app.slice(app.indexOf('function slidePane('), app.indexOf('export function App()'));
  assert.ok(slide.length > 0);
  const apply = slide.indexOf('apply();');
  const measure = slide.indexOf('columnSlideFrames(');
  const animate = slide.indexOf('root.animate(');
  // Measured after the grid animation started, the column read its old width
  // and was never frozen: the rig showed it rewrapping every frame again.
  assert.ok(apply > 0 && apply < measure && measure < animate, 'apply, then measure, then animate');
  assert.ok(slide.indexOf("removeProperty('min-height')") < apply, 'the held height goes before the rewrap');
  assert.match(slide, /duration: PANE_SLIDE_MS, easing: PANE_SLIDE_EASING/);
});

test('a followed transcript holds its height only at the width it was measured at', () => {
  const transcript = readFileSync(new URL('../src/components/Transcript.tsx', import.meta.url), 'utf8');
  const onResize = transcript.slice(transcript.indexOf('const onResize = () => {'), transcript.indexOf('const mo = new MutationObserver'));
  assert.match(onResize, /if \(width !== heldWidth\.current\) \{\s*heldWidth\.current = width;\s*heldHeight\.current = 0;\s*doc\.style\.removeProperty\('min-height'\);/);
  assert.ok(onResize.indexOf('heldWidth.current') < onResize.indexOf('const height = doc.offsetHeight;'), 'the new width is settled before the height is read');
});

test('the closing rail is never unmounted for a frame', () => {
  const rail = readFileSync(new URL('../src/components/RightRail.tsx', import.meta.url), 'utf8');
  // Decided while rendering: an effect would first render nothing (unmounting
  // the rail, its tab and its scroll) and then mount it again to slide it out.
  assert.match(rail, /if \(shownPane !== pane\) \{\s*setShownPane\(pane\);\s*setSliding\(shownPane === 'rail' && pane === null\);/);
  assert.doesNotMatch(rail, /useLayoutEffect/);
});

type Box = AnchorBox & { name: string; inline?: boolean; children: Box[] };
const box = (name: string, top: number, bottom: number, children: Box[] = [], inline = false): Box => ({
  name, inline, children, getBoundingClientRect: () => ({ top, bottom, width: 100, height: bottom - top }),
});

test('the anchor descends from a whole message to the block at the top edge', () => {
  // The edge (y=100) falls in the margin between two paragraphs, so the hit
  // test returns the message, whose own top does not move when text rewraps.
  const below = box('p2', 104, 160, [box('b', 104, 120, [], true)]);
  const message = box('message', -500, 400, [box('p1', -500, 96), below, box('p3', 170, 400)]);
  const inline = (node: Box) => !!node.inline;
  assert.equal(anchorBlockAt(message, 100, inline).name, 'p2', 'the first block below the edge');
  const spanning = box('list', 80, 300, [box('li1', 80, 90), box('li2', 92, 140, [box('code', 95, 110, [], true)])]);
  const holder = box('message', -500, 400, [spanning]);
  assert.equal(anchorBlockAt(holder, 100, inline).name, 'li2', 'into the block that spans the edge, never into inline content');
  assert.equal(anchorBlockAt(box('p', 90, 130), 100, inline).name, 'p', 'a leaf is kept');
});

test('the anchor pick undoes a layout shift before it re-reads the held element', () => {
  const transcript = readFileSync(new URL('../src/components/Transcript.tsx', import.meta.url), 'utf8');
  const pick = transcript.slice(transcript.indexOf('const pickAnchorNode = () => {'), transcript.indexOf('const recordAnchor = () => {'));
  assert.ok(pick.indexOf('holdAnchor();') > 0 && pick.indexOf('holdAnchor();') < pick.indexOf('const held = anchorNode.current;'));
  assert.match(pick, /anchorBlockAt\(hit, line, isInlineBox\)/);
});
