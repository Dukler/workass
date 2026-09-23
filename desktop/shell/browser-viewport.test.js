'use strict';

const assert = require('node:assert/strict');
const test = require('node:test');
const {
  DEFAULT_VIEWPORT, MAX_CAPTURE_PIXELS, assertCaptureBounds, captureRasterStep, presentationFor, validateViewport,
} = require('./browser-viewport');

test('browser logical viewport defaults independently from presentation bounds', () => {
  assert.deepEqual(DEFAULT_VIEWPORT, { width: 1440, height: 900, deviceScaleFactor: 1 });
  const fit = presentationFor(DEFAULT_VIEWPORT, { x: 10, y: 20, width: 312, height: 500 });
  assert.equal(fit.scale, 312 / 1440);
  assert.equal(fit.bounds.width, 312);
  assert.equal(fit.bounds.height, Math.floor(900 * (312 / 1440)));
  assert.deepEqual(presentationFor(DEFAULT_VIEWPORT, { x: 0, y: 0, width: 0, height: 0 }).scale, 0);
});

test('viewport validation rejects values rather than silently clamping', () => {
  assert.deepEqual(validateViewport(320, 240), { width: 320, height: 240, deviceScaleFactor: 1 });
  assert.deepEqual(validateViewport(3840, 2160), { width: 3840, height: 2160, deviceScaleFactor: 1 });
  for (const value of [undefined, NaN, Infinity, 320.5, 319, 3841, '1440']) {
    assert.throws(() => validateViewport(value, 900), /width must be an integer/);
  }
  for (const value of [undefined, NaN, Infinity, 240.5, 239, 2161, '900']) {
    assert.throws(() => validateViewport(1440, value), /height must be an integer/);
  }
});

test('capture bounds are finite, positive and bounded before allocation', () => {
  assert.deepEqual(assertCaptureBounds({ x: 0, y: 0, width: 1440, height: 900 }).pixels, 1_296_000);
  assert.throws(() => assertCaptureBounds({ x: 0, y: 0, width: 0, height: 20 }), /positive dimensions/);
  assert.throws(() => assertCaptureBounds({ x: -1, y: 0, width: 20, height: 20 }), /nonnegative coordinates/);
  assert.throws(() => assertCaptureBounds({ x: 0, y: 0, width: MAX_CAPTURE_PIXELS + 1, height: 1 }), /pixel limit/);
});

test('full-page raster steps align CSS pixels to native view scale', () => {
  assert.equal(captureRasterStep(1), 1);
  assert.equal(captureRasterStep(2), 2);
  assert.equal(captureRasterStep(1.25), 5);
  assert.equal(captureRasterStep(1.5), 3);
  assert.equal(captureRasterStep(1.75), 7);
  assert.equal(captureRasterStep(Number.NaN), 1);
});
