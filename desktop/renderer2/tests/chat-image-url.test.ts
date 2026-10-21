import assert from 'node:assert/strict';
import { afterEach, test } from 'node:test';
import { chatImageURL, setRemoteChatImageAccess } from '../src/chat-image-url.ts';
import type { MessageImage } from '../src/store/types.ts';

const digest = 'ab'.repeat(32);
const shot = (over: Partial<MessageImage> = {}): MessageImage => ({ mimeType: 'image/png', data: '', deferredImageRef: `images/${digest}`, ...over });

function withAccess(state: unknown) {
  (globalThis as any).window = { __workassLanAccess: state };
}

afterEach(() => {
  delete (globalThis as any).window;
  setRemoteChatImageAccess(null);
});

test('an older daemon sends no image key: the image is read over the socket', () => {
  assert.equal(chatImageURL('tab-1', shot()), '');
  withAccess({ state: 'approved', deviceId: 'd1' });
  assert.equal(chatImageURL('tab-1', shot()), '');
  withAccess({ state: 'waiting', imageKey: 'd1.k' });
  assert.equal(chatImageURL('tab-1', shot()), '', 'a device that is not approved has no key to use');
});

test('a local chat image is a same-origin URL carrying the image key', () => {
  withAccess({ state: 'approved', imageKey: 'd1.0123' });
  assert.equal(chatImageURL('tab-1', shot()), `/workass/chat-image/tab-1/${digest}?k=d1.0123`);
  assert.equal(chatImageURL('tab 1/x', shot()), `/workass/chat-image/tab%201%2Fx/${digest}?k=d1.0123`);
  assert.equal(chatImageURL('tab-1', shot({ mimeType: 'IMAGE/JPEG' })), `/workass/chat-image/tab-1/${digest}?k=d1.0123`);
});

test('only stored raster images go by URL', () => {
  withAccess({ state: 'approved', imageKey: 'd1.0123' });
  assert.equal(chatImageURL('tab-1', shot({ mimeType: 'image/svg+xml' })), '', 'SVG stays a data URL');
  assert.equal(chatImageURL('tab-1', shot({ deferredImageRef: undefined, data: 'iVBOR' })), '', 'inline bytes need no request');
  assert.equal(chatImageURL('tab-1', shot({ deferredImageRef: 'images/../devices.json' })), '');
});

test("a remote chat's image comes from its own daemon with that machine's key", () => {
  withAccess({ state: 'approved', imageKey: 'local.key' });
  setRemoteChatImageAccess((machineId) => (machineId === 'm-remote' ? { origin: 'https://192.168.0.212:8787', key: 'remote.key' } : null));
  assert.equal(chatImageURL('M~m-remote~tab-9', shot()), `https://192.168.0.212:8787/workass/chat-image/tab-9/${digest}?k=remote.key`);
  assert.equal(chatImageURL('M~m-offline~tab-9', shot()), '', 'a machine without a ready link reads over its socket');
});
