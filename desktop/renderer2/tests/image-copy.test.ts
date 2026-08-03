import assert from 'node:assert/strict';
import test from 'node:test';
import {
  copyImageElement,
  imageCopyPoint,
  imageCopyTargetForShortcut,
  isImageExternalOpenGesture,
  isImageCopyShortcut,
  openImageElementExternally,
} from '../src/image-copy.ts';

test('Cmd/Ctrl+C is recognized without stealing modified or ordinary copy keys', () => {
  assert.equal(isImageCopyShortcut({ key: 'c', metaKey: true, ctrlKey: false, altKey: false, shiftKey: false }), true);
  assert.equal(isImageCopyShortcut({ key: 'C', metaKey: false, ctrlKey: true, altKey: false, shiftKey: false }), true);
  assert.equal(isImageCopyShortcut({ key: 'c', metaKey: true, ctrlKey: false, altKey: true, shiftKey: false }), false);
  assert.equal(isImageCopyShortcut({ key: 'v', metaKey: true, ctrlKey: false, altKey: false, shiftKey: false }), false);
});

test('selected text keeps native copy ownership even when an image remains focused', () => {
  const image = {} as HTMLImageElement;
  const focusedImage = { tagName: 'IMG' };
  assert.equal(imageCopyTargetForShortcut(
    image,
    { isCollapsed: false, toString: () => 'selected transcript text' },
    focusedImage,
  ), null);
  assert.equal(imageCopyTargetForShortcut(
    image,
    { isCollapsed: true, toString: () => '' },
    focusedImage,
  ), image);
});

test('selected input text also keeps native copy ownership', () => {
  const image = {} as HTMLImageElement;
  assert.equal(imageCopyTargetForShortcut(
    image,
    { isCollapsed: true, toString: () => '' },
    { tagName: 'TEXTAREA', selectionStart: 2, selectionEnd: 8 },
  ), null);
});

test('keyboard image copy targets the visible center of a clipped thumbnail', () => {
  assert.deepEqual(imageCopyPoint({ left: -20, top: 10, right: 180, bottom: 110, width: 200, height: 100 }, 120, 80), { x: 60, y: 45 });
  assert.equal(imageCopyPoint({ left: 200, top: 200, right: 300, bottom: 300, width: 100, height: 100 }, 120, 80), null);
});

test('Cmd+click opens externally on macOS while an ordinary click and Ctrl+click remain unchanged', () => {
  const click = { button: 0, metaKey: false, ctrlKey: false, altKey: false, shiftKey: false };
  assert.equal(isImageExternalOpenGesture(click, 'MacIntel'), false);
  assert.equal(isImageExternalOpenGesture({ ...click, metaKey: true }, 'MacIntel'), true);
  assert.equal(isImageExternalOpenGesture({ ...click, ctrlKey: true }, 'MacIntel'), false);
  assert.equal(isImageExternalOpenGesture({ ...click, ctrlKey: true }, 'Win32'), true);
  assert.equal(isImageExternalOpenGesture({ ...click, metaKey: true, shiftKey: true }, 'MacIntel'), false);
  assert.equal(isImageExternalOpenGesture({ ...click, button: 1, metaKey: true }, 'MacIntel'), false);
});

test('external image open sends bounded raster bytes to the native shell bridge', async () => {
  const calls: Array<{ bytes: ArrayBuffer; mimeType: string }> = [];
  const priorWindow = Object.getOwnPropertyDescriptor(globalThis, 'window');
  const priorFetch = Object.getOwnPropertyDescriptor(globalThis, 'fetch');
  Object.defineProperty(globalThis, 'window', {
    configurable: true,
    value: {
      workassClipboard: {
        supported: true,
        copyImageAt: async () => true,
        openImageExternal: async (payload: { bytes: ArrayBuffer; mimeType: string }) => { calls.push(payload); return true; },
      },
    },
  });
  Object.defineProperty(globalThis, 'fetch', {
    configurable: true,
    value: async () => ({
      ok: true,
      status: 200,
      blob: async () => new Blob([new Uint8Array([0x89, 0x50, 0x4e, 0x47])], { type: 'image/png' }),
    }),
  });
  try {
    const image = { currentSrc: 'blob:workass-image', src: '', alt: 'Preview' } as HTMLImageElement;
    assert.equal(await openImageElementExternally(image), true);
    assert.equal(calls.length, 1);
    assert.equal(calls[0].mimeType, 'image/png');
    assert.deepEqual([...new Uint8Array(calls[0].bytes)], [0x89, 0x50, 0x4e, 0x47]);
  } finally {
    if (priorWindow) Object.defineProperty(globalThis, 'window', priorWindow);
    else Reflect.deleteProperty(globalThis, 'window');
    if (priorFetch) Object.defineProperty(globalThis, 'fetch', priorFetch);
    else Reflect.deleteProperty(globalThis, 'fetch');
  }
});

test('Electron image copy delegates decoded pixels at the visible image point', async () => {
  const calls: Array<{ x: number; y: number }> = [];
  const priorWindow = Object.getOwnPropertyDescriptor(globalThis, 'window');
  Object.defineProperty(globalThis, 'window', {
    configurable: true,
    value: {
      innerWidth: 800,
      innerHeight: 600,
      workassClipboard: {
        supported: true,
        copyImageAt: async (point: { x: number; y: number }) => { calls.push(point); return true; },
      },
    },
  });
  try {
    const image = {
      getBoundingClientRect: () => ({ left: 20, top: 40, right: 220, bottom: 140, width: 200, height: 100 }),
    } as HTMLImageElement;
    assert.equal(await copyImageElement(image), true);
    assert.deepEqual(calls, [{ x: 120, y: 90 }]);
  } finally {
    if (priorWindow) Object.defineProperty(globalThis, 'window', priorWindow);
    else Reflect.deleteProperty(globalThis, 'window');
  }
});
