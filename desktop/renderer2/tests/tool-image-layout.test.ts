// A tool gallery far from the reader is one box of exactly its height, and
// only the rows near the viewport mount. The height is computed, so it must
// be the height the grid lays out: the constants are read back from app.css.
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { test } from 'node:test';
import {
  TOOL_IMAGE_FRAME_BORDER, TOOL_IMAGE_GAP, TOOL_IMAGE_MEDIA_MAX, TOOL_IMAGE_MIN_COLUMN,
  spanHeight, toolImageGridLayout, toolImageRowsInView, toolImageSpacers,
} from '../src/tool-image-layout.ts';
import { placeFlowBlocks, type FlowBlock } from '../src/flow-window.ts';

const css = readFileSync(new URL('../src/styles/app.css', import.meta.url), 'utf8');
const rule = (selector: string) => {
  const at = css.indexOf(`\n${selector} {`);
  assert.ok(at >= 0, `app.css has no ${selector} rule`);
  return css.slice(at, css.indexOf('}', at));
};

test('the computed geometry uses the gallery CSS it stands in for', () => {
  assert.match(rule('.tool-images'), new RegExp(`grid-template-columns: repeat\\(auto-fit, minmax\\(${TOOL_IMAGE_MIN_COLUMN}px, 1fr\\)\\)`));
  assert.match(rule('.tool-images'), new RegExp(`gap: ${TOOL_IMAGE_GAP}px`));
  assert.match(rule('.ri'), /padding: 0;/);
  assert.match(rule('.ri'), new RegExp(`border: ${TOOL_IMAGE_FRAME_BORDER / 2}px solid`));
  assert.match(rule('.ri-media'), /width: 100%;/);
  assert.match(rule('.ri-media'), /aspect-ratio: var\(--iw\) \/ var\(--ih\);/);
  assert.match(rule('.ri-tool .ri-media'), new RegExp(`max-height: ${TOOL_IMAGE_MEDIA_MAX}px;`));
  assert.match(rule('.tool-images-pad'), /grid-column: 1 \/ -1;/);
});

test('a frame is its image\'s size, never a full-width box, and never shows a border', () => {
  // The frame hugs its image inside the grid track (or a gallery cell), so a
  // lone screenshot never stretches into a bordered, letterboxed box.
  assert.match(rule('.ri-tool'), /width: min\(100%, calc\(var\(--iw\) \* 1px \+ 2px\), calc\(340px \* var\(--iw\) \/ var\(--ih\) \+ 2px\)\);/);
  assert.match(rule('.ri-tool'), /justify-self: start;/);
  assert.match(rule('.assistant-images .assistant-inline-image'), /width: min\(100%, calc\(var\(--iw\) \* 1px \+ 2px\), calc\(620px \* var\(--iw\) \/ var\(--ih\) \+ 2px\)\);/);
  // The border keeps its width (the geometry counts it) but is never drawn.
  assert.match(rule('.ri'), /border: 1px solid transparent;/);
  // A small image keeps its natural height: a 200x100 screenshot in a 700px
  // gallery is 100px tall, not stretched to the track's width.
  assert.deepEqual(toolImageGridLayout(700, [[200, 100]]).rowHeights, [100 + TOOL_IMAGE_FRAME_BORDER]);
});

test('rows are as tall as their tallest frame, capped, with the grid gap between', () => {
  // 700px: four 150px tracks fit (4 * 150 + 3 * 7 = 621).
  const wide: [number, number] = [1920, 1080];
  const layout = toolImageGridLayout(700, [wide, wide, wide, [400, 2000], wide, wide]);
  assert.equal(layout.columns, 4);
  const column = (700 - 3 * 7) / 4;
  const wideRow = (column - 2) * 1080 / 1920 + 2;
  assert.deepEqual(layout.rowHeights, [342, wideRow]);
  assert.equal(layout.height, 342 + 7 + wideRow);
  // A short gallery shares the width among its items (auto-fit collapses).
  const pair = toolImageGridLayout(700, [wide, wide]);
  assert.equal(pair.columns, 2);
  assert.equal(pair.rowHeights[0], ((700 - 7) / 2 - 2) * 1080 / 1920 + 2);
  assert.equal(toolImageGridLayout(0, [wide]).height, 0);
  assert.equal(toolImageGridLayout(700, []).height, 0);
});

test('rows in view are the ones that meet the band', () => {
  const rows = [100, 100, 100, 100, 100];
  // Row tops: 1000, 1107, 1214, 1321, 1428.
  assert.deepEqual(toolImageRowsInView(rows, 1000, 1150, 1250), { from: 1, to: 3 });
  assert.deepEqual(toolImageRowsInView(rows, 1000, 0, 500), { from: 0, to: 0 });
  assert.deepEqual(toolImageRowsInView(rows, 1000, 5000, 6000), { from: 0, to: 0 });
  assert.deepEqual(toolImageRowsInView(rows, 1000, 0, 99999), { from: 0, to: 5 });
});

test('spacers and the mounted rows always add up to the whole grid', () => {
  const layout = toolImageGridLayout(640, Array.from({ length: 23 }, (_, index): [number, number] => [800 + index * 13, 600 + (index % 5) * 90]));
  const count = layout.rowHeights.length;
  for (let from = 0; from < count; from++) {
    for (let to = from + 1; to <= count; to++) {
      // Grid tracks: [spacer] rows [spacer], with one gap between neighbours.
      const { before, after } = toolImageSpacers(layout, { from, to });
      const tracks = [...(before > 0 ? [before] : []), ...layout.rowHeights.slice(from, to), ...(after > 0 ? [after] : [])];
      const total = tracks.reduce((sum, height) => sum + height, 0) + (tracks.length - 1) * TOOL_IMAGE_GAP;
      assert.ok(Math.abs(total - layout.height) < 1e-6, `rows ${from}-${to}: ${total} vs ${layout.height}`);
    }
  }
  assert.equal(spanHeight(layout.rowHeights, 0, count), layout.height);
});

test('placing reads each block once per layout epoch and writes after every read', () => {
  const log: string[] = [];
  const block = (name: string, top: number): FlowBlock => ({
    element: () => ({ getBoundingClientRect: () => { log.push(`read ${name}`); return { top } as DOMRect; } }) as Element,
    place: (at, _view, changed) => { log.push(`place ${name} ${at} ${changed}`); return () => log.push(`write ${name}`); },
  });
  const blocks = new Map([[block('a', 120), { epoch: 0, contentTop: 0 }], [block('b', 900), { epoch: 0, contentTop: 0 }]]);
  const apply = (writes: Array<() => void>) => { log.push('apply'); for (const write of writes) write(); };
  placeFlowBlocks(blocks, 1, 0, { top: 100, bottom: 700 }, apply);
  assert.deepEqual(log, ['read a', 'place a 120 true', 'read b', 'place b 900 true', 'apply', 'write a', 'write b']);
  log.length = 0;
  // Scrolled 300px, same layout: positions come from the cache.
  placeFlowBlocks(blocks, 1, 300, { top: 100, bottom: 700 }, apply);
  assert.deepEqual(log, ['place a -180 false', 'place b 600 false', 'apply', 'write a', 'write b']);
});
