import assert from 'node:assert/strict';
import { test } from 'node:test';
import {
  browserPagePresentation, onBrowserPagePresentation, pageElementSize, presentBrowserPage, surfaceLayout, withdrawBrowserPage,
  SURFACE_Z_FLOAT,
} from '../src/browser-surface.ts';

// The webview host draws each page in a box of its own, placed over the slot
// that shows it (browser-surface.ts).

test('the page element is its viewport in window pixels, whatever the app zoom', () => {
  assert.deepEqual(pageElementSize({ width: 1440, height: 900 }, 1), { width: 1440, height: 900 });
  assert.deepEqual(pageElementSize({ width: 1440, height: 900 }, 1.25), { width: 1152, height: 720 });
});

test('a floating frame shows the 1440 x 900 page scaled into it, filling it', () => {
  const layout = surfaceLayout({ rect: { x: 100, y: 60, width: 344, height: 215 }, radius: 12, z: SURFACE_Z_FLOAT }, { width: 1440, height: 900 }, 1);
  assert.deepEqual(layout.wrapper, { left: 100, top: 60, width: 344, height: 215, radius: 12, z: SURFACE_Z_FLOAT, interactive: true, shown: true });
  assert.ok(Math.abs(layout.page.scale - 344 / 1440) < 1e-9);
  assert.equal(layout.page.width * layout.page.scale, 344);
  assert.ok(Math.abs(layout.page.height * layout.page.scale - 215) < 0.01);
  assert.equal(layout.page.offsetX, 0);
});

test('resizing the frame changes only the scale: the page keeps its layout size', () => {
  const viewport = { width: 1440, height: 900 };
  const small = surfaceLayout({ rect: { x: 0, y: 0, width: 344, height: 215 }, radius: 12, z: 19 }, viewport, 1.2);
  const large = surfaceLayout({ rect: { x: 0, y: 0, width: 720, height: 450 }, radius: 12, z: 19 }, viewport, 1.2);
  assert.deepEqual([small.page.width, small.page.height], [large.page.width, large.page.height]);
  assert.ok(large.page.scale > small.page.scale);
});

test('an agent viewport of another shape is letterboxed and centered in the slot', () => {
  const layout = surfaceLayout({ rect: { x: 0, y: 0, width: 600, height: 700 }, radius: 0, z: 19 }, { width: 390, height: 844 }, 1);
  assert.ok(Math.abs(layout.page.scale - 700 / 844) < 1e-9);
  assert.ok(layout.page.offsetX > 0);
  assert.equal(layout.page.offsetY, 0);
});

test('a page no slot shows sits behind the app at its viewport size, still painting', () => {
  const layout = surfaceLayout(null, { width: 1440, height: 900 }, 1);
  assert.deepEqual(layout.wrapper, { left: 0, top: 0, width: 1440, height: 900, radius: 0, z: -1, interactive: false, shown: false });
  assert.equal(layout.page.scale, 1);
});

test('a slot withdraws only its own claim, so a pane closing never hides the frame that took the page', () => {
  const pane = Symbol('pane');
  const frame = Symbol('frame');
  const applied: string[] = [];
  const off = onBrowserPagePresentation('chat', () => applied.push(browserPagePresentation('chat') ? 'shown' : 'hidden'));
  presentBrowserPage('chat', pane, { rect: { x: 0, y: 0, width: 300, height: 600 }, radius: 9, z: 19 });
  presentBrowserPage('chat', frame, { rect: { x: 10, y: 10, width: 344, height: 215 }, radius: 12, z: 19 });
  withdrawBrowserPage('chat', pane);
  assert.equal(browserPagePresentation('chat')?.radius, 12);
  presentBrowserPage('chat', frame, { rect: { x: 10, y: 10, width: 344, height: 215 }, radius: 12, z: 19 });
  assert.deepEqual(applied, ['shown', 'shown'], 'placed synchronously on every change, not on repeats');
  withdrawBrowserPage('chat', frame);
  assert.equal(browserPagePresentation('chat'), null);
  assert.deepEqual(applied, ['shown', 'shown', 'hidden']);
  off();
});
