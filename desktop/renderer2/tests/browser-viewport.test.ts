import assert from 'node:assert/strict';
import test from 'node:test';
import { BROWSER_VIEWPORT_PRESETS, browserViewportInputError, browserViewportPreset } from '../src/browser.ts';

test('browser viewport presets describe deliberate logical page sizes', () => {
  assert.deepEqual(BROWSER_VIEWPORT_PRESETS.desktop, { width: 1440, height: 900 });
  assert.deepEqual(BROWSER_VIEWPORT_PRESETS.laptop, { width: 1280, height: 800 });
  assert.deepEqual(BROWSER_VIEWPORT_PRESETS.narrow, { width: 390, height: 844 });
  assert.equal(browserViewportPreset(), 'desktop');
  assert.equal(browserViewportPreset(1280, 800), 'laptop');
  assert.equal(browserViewportPreset(390, 844), 'narrow');
  assert.equal(browserViewportPreset(1024, 768), 'custom');
});

test('custom browser dimensions accept only whole values in the supported range', () => {
  assert.equal(browserViewportInputError('320', '240'), null);
  assert.equal(browserViewportInputError('3840', '2160'), null);
  assert.match(browserViewportInputError('', '844') ?? '', /ancho/iu);
  assert.match(browserViewportInputError('390.5', '844') ?? '', /ancho/iu);
  assert.match(browserViewportInputError('390', '2161') ?? '', /alto/iu);
  assert.match(browserViewportInputError('390', '8e2') ?? '', /alto/iu);
});
