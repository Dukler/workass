'use strict';

const DEFAULT_VIEWPORT = Object.freeze({ width: 1440, height: 900, deviceScaleFactor: 1 });
const MIN_VIEWPORT_WIDTH = 320;
const MAX_VIEWPORT_WIDTH = 3840;
const MIN_VIEWPORT_HEIGHT = 240;
const MAX_VIEWPORT_HEIGHT = 2160;
const MAX_CAPTURE_PIXELS = 16_000_000;
const MAX_PNG_BYTES = 8 * 1024 * 1024;

function validateViewport(width, height) {
  if (typeof width !== 'number' || !Number.isFinite(width) || !Number.isInteger(width) ||
      width < MIN_VIEWPORT_WIDTH || width > MAX_VIEWPORT_WIDTH) {
    throw new Error(`browser viewport width must be an integer from ${MIN_VIEWPORT_WIDTH} to ${MAX_VIEWPORT_WIDTH}`);
  }
  if (typeof height !== 'number' || !Number.isFinite(height) || !Number.isInteger(height) ||
      height < MIN_VIEWPORT_HEIGHT || height > MAX_VIEWPORT_HEIGHT) {
    throw new Error(`browser viewport height must be an integer from ${MIN_VIEWPORT_HEIGHT} to ${MAX_VIEWPORT_HEIGHT}`);
  }
  return { width, height, deviceScaleFactor: 1 };
}

function presentationFor(viewport, bounds) {
  const width = Math.max(0, Number(bounds && bounds.width) || 0);
  const height = Math.max(0, Number(bounds && bounds.height) || 0);
  const scale = width > 0 && height > 0
    ? Math.min(width / viewport.width, height / viewport.height)
    : 0;
  const presentedWidth = Math.floor(viewport.width * scale);
  const presentedHeight = Math.floor(viewport.height * scale);
  return {
    scale,
    bounds: {
      x: Math.floor(Number(bounds && bounds.x) || 0) + Math.floor((width - presentedWidth) / 2),
      y: Math.floor(Number(bounds && bounds.y) || 0) + Math.floor((height - presentedHeight) / 2),
      width: Math.max(1, presentedWidth),
      height: Math.max(1, presentedHeight),
    },
  };
}

// A captured CSS-pixel extent must map to a whole number of native view DIPs.
// This step is the smallest integer pixel count whose division by the host
// device scale factor is integral (2 for Retina, 5 for 1.25x, 3 for 1.5x).
function captureRasterStep(deviceScaleFactor) {
  const scale = Number(deviceScaleFactor);
  if (!Number.isFinite(scale) || scale <= 0) return 1;
  for (let pixels = 1; pixels <= 64; pixels += 1) {
    const dipExtent = pixels / scale;
    if (Math.abs(dipExtent - Math.round(dipExtent)) <= 1e-7) return pixels;
  }
  return 1;
}

function pngDimensions(data) {
  const bytes = Buffer.isBuffer(data) ? data : Buffer.from(String(data || ''), 'base64');
  if (bytes.length < 24 || bytes.toString('hex', 0, 8) !== '89504e470d0a1a0a') {
    throw new Error('browser capture did not return a valid PNG');
  }
  return { width: bytes.readUInt32BE(16), height: bytes.readUInt32BE(20), byteLength: bytes.length };
}

function assertCaptureBounds(rect, label = 'browser capture') {
  const x = Number(rect && rect.x);
  const y = Number(rect && rect.y);
  const width = Number(rect && rect.width);
  const height = Number(rect && rect.height);
  if (![x, y, width, height].every(Number.isFinite) || x < 0 || y < 0 || width <= 0 || height <= 0) {
    throw new Error(`${label} rectangle must use finite nonnegative coordinates and positive dimensions`);
  }
  if (!Number.isInteger(Math.floor(x)) || !Number.isInteger(Math.floor(y))) throw new Error(`${label} rectangle is invalid`);
  const pixels = Math.ceil(width) * Math.ceil(height);
  if (!Number.isSafeInteger(pixels) || pixels > MAX_CAPTURE_PIXELS) {
    throw new Error(`${label} exceeds the ${MAX_CAPTURE_PIXELS}-pixel limit; request a smaller clip`);
  }
  return { x, y, width, height, pixels };
}

module.exports = {
  DEFAULT_VIEWPORT,
  MAX_CAPTURE_PIXELS,
  MAX_PNG_BYTES,
  MAX_VIEWPORT_HEIGHT,
  MAX_VIEWPORT_WIDTH,
  MIN_VIEWPORT_HEIGHT,
  MIN_VIEWPORT_WIDTH,
  assertCaptureBounds,
  captureRasterStep,
  pngDimensions,
  presentationFor,
  validateViewport,
};
