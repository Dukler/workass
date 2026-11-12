import test from 'node:test';
import assert from 'node:assert/strict';
import { primeSentImages, sentImageSrc } from '../src/sent-images.ts';

// The desktop's own send: the receipt's references, in send order, get the
// bytes in hand, so the row by reference paints without fetching them back.
test('a sent image is kept under the reference its receipt names', () => {
  primeSentImages(
    [{ mimeType: 'image/png', data: 'AAAA' }, { mimeType: 'image/jpeg', data: 'BBBB' }],
    [{ deferredImageRef: 'images/' + 'a'.repeat(64) }, { deferredImageRef: 'images/' + 'b'.repeat(64) }],
  );
  assert.equal(sentImageSrc('images/' + 'a'.repeat(64)), 'data:image/png;base64,AAAA');
  assert.equal(sentImageSrc('images/' + 'b'.repeat(64)), 'data:image/jpeg;base64,BBBB');
});

test('a receipt that does not line up with what was sent primes nothing', () => {
  primeSentImages([{ mimeType: 'image/png', data: 'CCCC' }], [{ deferredImageRef: 'images/' + 'c'.repeat(64) }, { deferredImageRef: 'images/' + 'd'.repeat(64) }]);
  assert.equal(sentImageSrc('images/' + 'c'.repeat(64)), undefined);
  primeSentImages([{ mimeType: 'image/png', data: '', deferredImageRef: 'images/' + 'e'.repeat(64) }], [{ deferredImageRef: 'images/' + 'e'.repeat(64) }]);
  assert.equal(sentImageSrc('images/' + 'e'.repeat(64)), undefined);
});
