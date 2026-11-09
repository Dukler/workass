import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import { PANE_SLIDE_MS, railSlides } from '../src/store/right-pane.ts';
import { anchorBlockAt, type AnchorBox } from '../src/transcript-window.ts';

// 2026-11-08: opening the right pane rewrapped the whole transcript in one
// frame ("the font changes size and jumps"). The info rail now slides, and the
// reader's line holds through it.

test('only the info rail opening or closing slides', () => {
  assert.equal(railSlides(null, 'rail'), true);
  assert.equal(railSlides('rail', null), true);
  assert.equal(railSlides(null, 'browser'), false, 'the browser view lives in another process and would trail the column');
  assert.equal(railSlides('browser', null), false);
  assert.equal(railSlides('rail', 'browser'), false);
  assert.equal(railSlides('rail', 'rail'), false);
});

test('the closed rail keeps a 0px track so the two layouts interpolate, and the slide time is one number', () => {
  const css = readFileSync(new URL('../src/styles/app.css', import.meta.url), 'utf8');
  assert.match(css, /#root\.no-rail \{ grid-template-columns: var\(--side-w, 288px\) 1fr 0px; \}/);
  assert.match(css, /#root\.no-side\.no-rail \{ grid-template-columns: 1fr 0px; \}/);
  const seconds = /#root\.pane-anim \{ transition: grid-template-columns ([\d.]+)s/.exec(css);
  assert.ok(seconds, 'the slide is a transition on the grid columns');
  assert.equal(Math.round(Number(seconds![1]) * 1000), PANE_SLIDE_MS);
  assert.match(css, /#root\.no-rail:not\(\.pane-anim\) aside\.rail \{ display: none; \}/, 'the rail stays drawn while it slides out');
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
