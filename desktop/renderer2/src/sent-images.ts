import type { MessageImage } from './store/types';
import { messageImageSrc } from './image-drafts.ts';

// The images this renderer just sent, by the reference the daemon gave them.
// A sent row first paints from its own bytes; the next refresh turns it into
// a row by reference, and without this the frame went back to a placeholder
// and fetched the image it had sent a moment before. The job:start receipt
// names each image's reference (userImages, in the order they were sent), so
// the bytes in hand are kept under it and the row by reference paints at once.
const sent = new Map<string, string>();
const SENT_LIMIT = 32;

export function primeSentImages(local: readonly MessageImage[] | undefined, receipt: ReadonlyArray<{ deferredImageRef?: string }> | undefined): void {
  if (!local?.length || !receipt?.length || local.length !== receipt.length) return;
  local.forEach((image, index) => {
    const ref = receipt[index]?.deferredImageRef;
    if (!ref || image.deferredImageRef || !image.data) return;
    const src = messageImageSrc(image);
    if (!src) return;
    sent.delete(ref);
    sent.set(ref, src);
  });
  while (sent.size > SENT_LIMIT) sent.delete(sent.keys().next().value as string);
}

export function sentImageSrc(ref: string): string | undefined {
  return sent.get(ref);
}
