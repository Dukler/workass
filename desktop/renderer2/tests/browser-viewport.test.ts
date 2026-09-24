import assert from 'node:assert/strict';
import test from 'node:test';
import { browserViewportForBounds } from '../src/browser.ts';

test('visible browser uses pane dimensions within the shell viewport limits', () => {
  assert.deepEqual(browserViewportForBounds({ x: 10, y: 20, width: 800, height: 600 }), { width: 800, height: 600 });
  assert.deepEqual(browserViewportForBounds({ x: 0, y: 0, width: 280, height: 200 }), { width: 336, height: 240 });
  assert.deepEqual(browserViewportForBounds({ x: 0, y: 0, width: 263, height: 838 }), { width: 320, height: 1020 });
  assert.deepEqual(browserViewportForBounds({ x: 0, y: 0, width: 4000, height: 2500 }), { width: 3456, height: 2160 });
});
