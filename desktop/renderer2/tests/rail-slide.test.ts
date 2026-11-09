import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import * as rightPane from '../src/store/right-pane.ts';
import { columnSlideFrames, railSlides } from '../src/store/right-pane.ts';
import { blockFlipFrames, flipBlocks, type BlockBox } from '../src/transcript-flip.ts';
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
  // The held height goes only with a width change, before anything paints;
  // with the width fixed it stays, so a followed transcript does not move.
  const drop = slide.indexOf("removeProperty('min-height')");
  assert.ok(drop > apply && drop < animate, 'the held height is dropped after the final layout is read, before the slide starts');
  assert.match(slide, /if \(Math\.abs\(box\.width - before\[i\]\.width\) > 0\.5\) doc\.style\.removeProperty\('min-height'\);/);
  assert.match(slide, /duration: PANE_SLIDE_MS, easing: PANE_SLIDE_EASING/);
});

test('a followed transcript holds its height only at the width it was measured at', () => {
  const transcript = readFileSync(new URL('../src/components/Transcript.tsx', import.meta.url), 'utf8');
  const onResize = transcript.slice(transcript.indexOf('const onResize = () => {'), transcript.indexOf('const mo = new MutationObserver'));
  assert.match(onResize, /if \(width !== heldWidth\.current\) \{\s*heldWidth\.current = width;\s*heldHeight\.current = 0;\s*doc\.style\.removeProperty\('min-height'\);/);
  assert.ok(onResize.indexOf('heldWidth.current') < onResize.indexOf('const height = doc.getBoundingClientRect().height;'), 'the new width is settled before the height is read');
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

// 2026-11-09 (owner): a column fixed at the width it has beside the pane
// ("option 1") cost ~270 px of text with the pane closed. The column fills the
// window again, up to 1040 px; the one rewrap glides instead of jumping.
test('the reading column is never narrowed to make room for the pane', () => {
  const app = readFileSync(new URL('../src/components/App.tsx', import.meta.url), 'utf8');
  assert.doesNotMatch(app, /setProperty\('--read'/);
  assert.equal('readingWidth' in rightPane, false);
  const css = readFileSync(new URL('../src/styles/app.css', import.meta.url), 'utf8');
  assert.match(css, /--read: 1040px;/);
  assert.match(css, /\.doc \{ max-width: var\(--read\); margin: 0 auto;/);
});

const ease = (() => {
  // The slide's curve, cubic-bezier(.2, .7, .3, 1).
  const cx = 0.6, bx = 3 * (0.3 - 0.2) - cx, ax = 1 - cx - bx, cy = 2.1, by = 3 * (1 - 0.7) - cy, ay = 1 - cy - by;
  const x = (t: number) => ((ax * t + bx) * t + cx) * t;
  const y = (t: number) => ((ay * t + by) * t + cy) * t;
  return (p: number) => { let lo = 0, hi = 1; for (let i = 0; i < 50; i++) { const m = (lo + hi) / 2; if (x(m) < p) lo = m; else hi = m; } return y((lo + hi) / 2); };
})();
const translateOf = (frame: Keyframe) => Number(/translateY\((-?[\d.]+)px\)/.exec(String(frame.transform))![1]);
const clipOf = (frame: Keyframe) => (frame.clipPath ? Number(/inset\(0px -100vw (-?[\d.]+)px -100vw\)/.exec(String(frame.clipPath))![1]) : 0);

test('every block starts where it was drawn and glides to its new place', () => {
  assert.equal(blockFlipFrames({ top: 40, height: 66 }, { top: 40, height: 66 }), null, 'nothing moved');
  assert.equal(blockFlipFrames({ top: 40, height: 66 }, { top: 40.3, height: 60 }), null, 'a shrink in place needs no glide');
  // Moved up 129 px by the rewrap below it (a followed reader opening the pane at 1280 px).
  const moved = blockFlipFrames({ top: 160, height: 22 }, { top: 31, height: 22 })!;
  assert.deepEqual(moved, [{ transform: 'translateY(129px)' }, { transform: 'translateY(0px)' }]);
  // Grew from 3 lines to 4: drawn at its old height first, its new last line uncovered as it goes.
  const grown = blockFlipFrames({ top: 300, height: 65 }, { top: 250, height: 87 })!;
  assert.equal(translateOf(grown[0]), 50);
  assert.equal(clipOf(grown[0]), 22);
  assert.equal(translateOf(grown[1]), 0);
  assert.equal(clipOf(grown[1]), 0);
});

test('two blocks gliding never draw over each other, and the gap between them is the new one', () => {
  // Old layout: A at 100 (3 lines), B right under it after a 12 px gap.
  // The column narrowed: A grew a line and everything moved up (bottom kept).
  const gap = 12;
  const was: BlockBox[] = [{ top: 100, height: 65 }, { top: 177, height: 43 }];
  const is: BlockBox[] = [{ top: 20, height: 87 }, { top: 119, height: 65 }];
  const frames = was.map((w, i) => blockFlipFrames(w, is[i])!);
  for (let step = 0; step <= 22; step++) {
    const left = 1 - ease(step / 22);
    const drawn = frames.map((f, i) => {
      const top = is[i].top + translateOf(f[0]) * left;
      return { top, bottom: top + is[i].height - clipOf(f[0]) * left };
    });
    assert.ok(Math.abs(drawn[1].top - drawn[0].bottom - gap) < 1e-6, `gap at ${step}/22`);
  }
});

test('the glide moves the innermost blocks that draw something, never one inside another', () => {
  type Fake = { name: string; style: Partial<CSSStyleDeclaration>; box: { top: number; bottom: number; height: number }; children: Fake[]; text?: string };
  const plain = { display: 'block', position: 'static', overflowX: 'visible', overflowY: 'visible', backgroundColor: 'rgba(0, 0, 0, 0)', backgroundImage: 'none', boxShadow: 'none', borderTopWidth: '0px', borderBottomWidth: '0px', borderLeftWidth: '0px', borderRightWidth: '0px' };
  const node = (name: string, style: Partial<CSSStyleDeclaration>, top: number, bottom: number, children: Fake[] = [], text?: string): Fake => ({ name, style: { ...plain, ...style }, box: { top, bottom, height: bottom - top }, children, text });
  const toElement = (fake: Fake): Element => {
    const kids = fake.children.map(toElement);
    const textNodes = fake.text ? [{ nodeType: 3, textContent: fake.text }] : [];
    return {
      name: fake.name, offsetParent: null, children: kids, childNodes: [...textNodes, ...kids.map((k) => Object.assign(k, { nodeType: 1 }))],
      firstChild: kids[0] ?? textNodes[0] ?? null, getBoundingClientRect: () => fake.box, style: fake.style,
    } as unknown as Element;
  };
  const tree = toElement(node('doc', {}, 0, 2000, [
    node('vunit', { display: 'contents' }, 0, 0, [
      node('amsg', {}, 0, 900, [
        node('p1', {}, 0, 60, [], 'text'),
        node('list', {}, 70, 140, [node('li1', { display: 'list-item' }, 70, 100, [], 'one'), node('li2', { display: 'list-item' }, 100, 140, [], 'two')]),
        node('codebox', { backgroundColor: 'rgb(30, 30, 30)' }, 150, 260, [node('pre', {}, 160, 250, [], 'code')]),
        node('tablewrap', { overflowX: 'auto', overflowY: 'auto' }, 270, 400, [node('table', { display: 'table' }, 270, 400, [], 'cells')]),
        node('far', {}, 1500, 1600, [], 'off screen'),
      ]),
    ]),
    node('userpill', { display: 'flex' }, 910, 960, [node('bubble', {}, 910, 960, [], 'hi')]),
  ]));
  const styleOf = (el: Element) => (el as unknown as { style: CSSStyleDeclaration }).style;
  const names = flipBlocks(tree, -100, 1000, styleOf).map((el) => (el as unknown as { name: string }).name);
  assert.deepEqual(names, ['p1', 'li1', 'li2', 'codebox', 'tablewrap', 'userpill']);
});

test('the glide is measured before the commit and started after the transcript has settled', () => {
  const app = readFileSync(new URL('../src/components/App.tsx', import.meta.url), 'utf8');
  // getSnapshotBeforeUpdate runs before React changes any DOM; read from an
  // effect, the transcript's own window re-cut had already moved the text.
  const snapshot = app.slice(app.indexOf('class RailSlideSnapshot'), app.indexOf('export function App()'));
  assert.match(snapshot, /getSnapshotBeforeUpdate\(prev[^)]*\) \{\s*if \(prev\.chatId === this\.props\.chatId && railSlides\(prev\.pane, this\.props\.pane\)\) \{/);
  assert.match(snapshot, /duration: PANE_SLIDE_MS, easing: PANE_SLIDE_EASING/);
  assert.match(snapshot, /for \(const view of columnSlideViews\(\)\) view\.capture\(timing\);/);
  assert.match(app, /<RailSlideSnapshot chatId=\{chat\?\.id \?\? null\} pane=\{activePane\} \/>/);
  const transcript = readFileSync(new URL('../src/components/Transcript.tsx', import.meta.url), 'utf8');
  const onResize = transcript.slice(transcript.indexOf('const onResize = () => {'), transcript.indexOf('const mo = new MutationObserver'));
  // After the hold or the pin, in the resize observer: after this frame's layout effects, before paint.
  assert.ok(onResize.indexOf('holdAnchor();') < onResize.indexOf('glideFromCapture();'));
  assert.match(onResize, /\} else if \(pendingGlide\.current\) \{\s*\/\/[^\n]*\n\s*scheduler\.dispose\(\);\s*spring\.stop\(\);\s*setScrollTop\(el, el\.scrollHeight\);/, 'a followed reader lands on the bottom at once; the glide is the motion');
});

test('the reader stays held while blocks glide: positions are read where they are laid out', () => {
  const transcript = readFileSync(new URL('../src/components/Transcript.tsx', import.meta.url), 'utf8');
  // Pausing the anchor during the glide let the window re-cut throw a
  // scrolled-up reader ~1100 px (rig, first slide after a reload).
  assert.doesNotMatch(transcript, /gliding\(\)/);
  const hold = transcript.slice(transcript.indexOf('const holdAnchor = () => {'), transcript.indexOf('// Cut the window again'));
  assert.match(hold, /exact\.node\.getBoundingClientRect\(\)\.top - glideShift\(exact\.node\)/);
  assert.match(hold, /box\.getBoundingClientRect\(\)\.top - glideShift\(box\)/);
  assert.match(transcript, /measureMessages\(doc, glideShift\)/);
  assert.match(transcript, /measureUnits\(doc, glideShift\)/);
});
