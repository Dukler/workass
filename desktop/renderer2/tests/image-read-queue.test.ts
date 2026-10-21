import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { createServer, type ViteDevServer } from 'vite';
import type { MessageImage } from '../src/store/types.ts';

// Image reads share the daemon's history lane with history pages, in arrival
// order. Scrolling a 225 MB chat queued ~6,000 tool-image reads; the history
// page behind them timed out and the upward load fell back to the button.
type Request = { promise: Promise<string | null>; release: () => void };
let vite: ViteDevServer;
let mod: {
  IMAGE_READS_AT_ONCE: number;
  requestImageSrc(tabId: string, image: MessageImage): Request;
  loadImageSrc(tabId: string, image: MessageImage): Promise<string | null>;
  imageReadsPending(): { waiting: number; reading: number };
};

before(async () => {
  vite = await createServer({ root: fileURLToPath(new URL('..', import.meta.url)), logLevel: 'silent', server: { middlewareMode: true }, appType: 'custom' });
  mod = await vite.ssrLoadModule('/src/components/MessageImage.tsx') as typeof mod;
});
after(async () => { await vite.close(); });

const image = (ref: string): MessageImage => ({ mimeType: 'image/png', data: '', name: ref, deferredImageRef: `images/${ref}` });
const tick = () => new Promise((resolve) => setTimeout(resolve, 0));

function fakeDaemon() {
  const started: string[] = [];
  const answers = new Map<string, (value: unknown) => void>();
  const previous = (globalThis as any).window;
  (globalThis as any).window = {
    api: {
      archiveLoad: (_tab: string, options: { imageRef: string }) => new Promise((resolve) => {
        started.push(options.imageRef);
        answers.set(options.imageRef, resolve);
      }),
    },
  };
  return {
    started,
    answer: async (ref: string) => { answers.get(`images/${ref}`)?.([{ data: 'iVBORw0KGgo=' }]); await tick(); await tick(); },
    restore: () => { if (previous === undefined) delete (globalThis as any).window; else (globalThis as any).window = previous; },
  };
}

test('only a few image reads are on the wire, the newest first, and one nobody needs is dropped', async () => {
  const daemon = fakeDaemon();
  try {
    const requests = Array.from({ length: 10 }, (_, index) => mod.requestImageSrc('tab-q', image(`q${index}`)));
    await tick();
    assert.equal(daemon.started.length, mod.IMAGE_READS_AT_ONCE, 'a bounded number of reads at once');
    assert.deepEqual(daemon.started, ['images/q0', 'images/q1', 'images/q2'], 'the first ones start as they arrive');
    // The reader scrolled on: rows 3-7 left the transcript before their turn.
    for (const request of requests.slice(3, 8)) request.release();
    await daemon.answer('q0');
    assert.equal(daemon.started.at(-1), 'images/q9', 'the newest request is served next');
    await daemon.answer('q1');
    await daemon.answer('q2');
    await daemon.answer('q9');
    await daemon.answer('q8');
    assert.deepEqual(daemon.started, ['images/q0', 'images/q1', 'images/q2', 'images/q9', 'images/q8'], 'released reads never reached the daemon');
    assert.deepEqual(mod.imageReadsPending(), { waiting: 0, reading: 0 });
    assert.equal(await requests[3].promise, null, 'a dropped read settles without bytes');
    assert.equal(await requests[0].promise, 'data:image/png;base64,iVBORw0KGgo=');
  } finally {
    daemon.restore();
  }
});

test('opening an image reads it even when no mounted row holds it, and a dropped image is asked for again', async () => {
  const daemon = fakeDaemon();
  try {
    const held = Array.from({ length: mod.IMAGE_READS_AT_ONCE }, (_, index) => mod.requestImageSrc('tab-p', image(`p${index}`)));
    const dropped = mod.requestImageSrc('tab-p', image('again'));
    dropped.release();
    const opened = mod.loadImageSrc('tab-p', image('opened'));
    await tick();
    await daemon.answer('p0');
    assert.equal(daemon.started.at(-1), 'images/opened', 'an explicit open is never dropped');
    await daemon.answer('opened');
    assert.equal(await opened, 'data:image/png;base64,iVBORw0KGgo=');
    for (let index = 1; index < held.length; index++) await daemon.answer(`p${index}`);
    assert.equal(await dropped.promise, null);
    const back = mod.requestImageSrc('tab-p', image('again'));
    await tick();
    assert.equal(daemon.started.at(-1), 'images/again', 'the row came back into view, so it is read now');
    await daemon.answer('again');
    assert.equal(await back.promise, 'data:image/png;base64,iVBORw0KGgo=');
  } finally {
    daemon.restore();
  }
});
