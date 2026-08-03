import assert from 'node:assert/strict';
import test from 'node:test';
import { sameBrowserBounds } from '../src/browser.ts';

test('browser view resize is skipped until its actual bounds change', () => {
  const bounds = { x: 1100, y: 48, width: 312, height: 820 };
  assert.equal(sameBrowserBounds(null, bounds), false);
  assert.equal(sameBrowserBounds({ ...bounds }, bounds), true);
  assert.equal(sameBrowserBounds({ ...bounds, height: 819 }, bounds), false);
  assert.equal(sameBrowserBounds({ ...bounds, x: 1099 }, bounds), false);
});
