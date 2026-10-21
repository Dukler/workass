import assert from 'node:assert/strict';
import { afterEach, test } from 'node:test';
import {
  CHAT_IMAGE_WIDTHS, chatImageRenditionWidth, chatImageURL, chatImageURLWidth, fullChatImageURL, sameChatImage, setRemoteChatImageAccess,
} from '../src/chat-image-url.ts';
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

// A gallery cell draws a ~2,000 px screenshot 156 px wide: it asks for a
// rendition at least as wide as the screen shows it, and never for one that
// is not clearly smaller than the image.
test('a frame asks for the smallest rendition that covers it on this screen', () => {
  assert.equal(chatImageRenditionWidth(156, 2, 2091), 320);
  assert.equal(chatImageRenditionWidth(156, 1, 2091), 160);
  assert.equal(chatImageRenditionWidth(156, 1.25, 2091), 240);
  assert.equal(chatImageRenditionWidth(537, 2, 1107), 0, 'a 1,280 px rendition of a 1,107 px image is the image');
  assert.equal(chatImageRenditionWidth(600, 2, 2400), 1280, 'never below what the screen draws');
  assert.equal(chatImageRenditionWidth(700, 2, 2400), 0, 'a 1,920 px rendition of a 2,400 px image is not worth it');
  assert.equal(chatImageRenditionWidth(1500, 2, 8000), 0, 'wider than the widest rendition: the full image');
  assert.equal(chatImageRenditionWidth(156, 2, undefined), 0, 'no wire size: the full image');
  assert.equal(chatImageRenditionWidth(0, 2, 2091), 0);
  assert.ok(CHAT_IMAGE_WIDTHS.every((width, index) => index === 0 || width > CHAT_IMAGE_WIDTHS[index - 1]));
});

test('a rendition URL names its width and its full image', () => {
  withAccess({ state: 'approved', imageKey: 'd1.0123' });
  const full = chatImageURL('tab-1', shot());
  const small = chatImageURL('tab-1', shot(), 320);
  assert.equal(small, `${full}&w=320`);
  assert.equal(chatImageURLWidth(small), 320);
  assert.equal(chatImageURLWidth(full), 0);
  assert.equal(fullChatImageURL(small), full);
  assert.equal(fullChatImageURL('data:image/png;base64,AAA&w=3'), 'data:image/png;base64,AAA&w=3', 'only chat-image URLs are renditions');
  assert.equal(sameChatImage(small, full), true);
  assert.equal(sameChatImage(`${full}&w=640`, small), true);
  assert.equal(sameChatImage(small, chatImageURL('tab-2', shot(), 320)), false);
  assert.equal(sameChatImage('', full), false);
});
