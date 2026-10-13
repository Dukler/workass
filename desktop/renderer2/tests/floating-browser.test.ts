import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { createServer, type ViteDevServer } from 'vite';
import { clampFloatWidth, floatFrame, nearestCorner, resizeFloatWidth } from '../src/components/floating-browser-layout.ts';
import type { Chat } from '../src/store/types.ts';

// The floating browser (approved mock browser-pip-v1 · B + C, 2026-10-13).
let vite: ViteDevServer;
let StoreCtor: new () => any;
before(async () => {
  vite = await createServer({ root: fileURLToPath(new URL('..', import.meta.url)), logLevel: 'silent', server: { middlewareMode: true }, appType: 'custom' });
  StoreCtor = (await vite.ssrLoadModule('/src/store/store.ts')).Store;
});
after(async () => { await vite.close(); });

const container = { width: 900, height: 700, reserve: 46 };

test('the frame sits in its corner, clear of the Working line, at the page proportions', () => {
  const frame = floatFrame({ geometry: { corner: 'br', width: 340 }, aspect: 0.625, container });
  assert.deepEqual(frame, { x: 900 - 14 - 340, y: 700 - 46 - 213, width: 340, height: 213 });
  const top = floatFrame({ geometry: { corner: 'tl', width: 340 }, aspect: 0.625, container });
  assert.equal(top.x, 14);
  assert.equal(top.y, 12);
});

test('a tall page or a small window shrinks the frame instead of overflowing', () => {
  assert.equal(clampFloatWidth(2000, 0.625, container), 720);
  const tall = clampFloatWidth(500, 1.6, container);
  assert.ok(tall * 1.6 <= 700 - 46 - 12 - 38, 'a tall page never runs past the space above the Working line');
  assert.equal(clampFloatWidth(10, 0.625, container), 220);
});

test('a dropped frame snaps to the corner of the quadrant its centre is in', () => {
  const size = { width: 300, height: 190 };
  assert.equal(nearestCorner({ x: 600, y: 480 }, size, container), 'br');
  assert.equal(nearestCorner({ x: 20, y: 480 }, size, container), 'bl');
  assert.equal(nearestCorner({ x: 600, y: 20 }, size, container), 'tr');
  assert.equal(nearestCorner({ x: 20, y: 20 }, size, container), 'tl');
  assert.equal(resizeFloatWidth('br', 340, -40), 380, 'dragging the free corner outward grows a right-anchored frame');
  assert.equal(resizeFloatWidth('bl', 340, 40), 380);
});

function chat(id: string, pane?: 'rail' | 'browser' | null): Chat {
  return { id, chatId: `chat-${id}`, title: id, titleLocked: true, group: null, cwd: null, providerId: 'claude',
    currentModelId: null, currentModeId: null, pending: false, messages: [], draft: '', ...(pane !== undefined ? { pane } : {}) } as Chat;
}

test('an agent page floats over its chat unless that chat already shows the browser pane', () => {
  const subject = new StoreCtor();
  const withRail = chat('tab-rail');
  const withPane = chat('tab-pane', 'browser');
  subject.state.chats = [withRail, withPane];
  subject.state.activeId = 'tab-other';
  subject.onBrowserOpenRequest('chat-tab-rail');
  subject.onBrowserOpenRequest('chat-tab-pane');
  subject.onBrowserOpenRequest(undefined);
  assert.equal(subject.browserPresentationFor('tab-rail'), 'float');
  assert.equal(withRail.pane, undefined, 'the info rail stays where it was');
  assert.equal(subject.browserPresentationFor('tab-pane'), null, 'a chat showing the pane keeps it');
  assert.equal(subject.state.activeId, 'tab-other', 'never switches the active chat');
  subject.clearToastTimers?.();
});

test('float, full screen, separate window and dock move one page between places, never two at once', () => {
  const subject = new StoreCtor();
  const target = chat('tab-move', 'browser');
  subject.state.chats = [target];
  subject.state.activeId = target.id;
  subject.floatBrowser();
  assert.equal(target.pane, null, 'floating closes the pane');
  assert.equal(subject.browserPresentationFor(target.id), 'float');
  subject.toggleBrowserFullscreen();
  assert.equal(subject.browserPresentationFor(target.id), 'full');
  subject.toggleBrowserFullscreen();
  assert.equal(subject.browserPresentationFor(target.id), 'float');
  subject.setBrowserPresentation(target.id, 'window');
  subject.onBrowserState({ chatId: target.id, poppedOut: false });
  assert.equal(subject.browserPresentationFor(target.id), 'float', 'closing the separate window floats the page again');
  subject.dockBrowser();
  assert.equal(target.pane, 'browser');
  assert.equal(subject.browserPresentationFor(target.id), null);
  subject.floatBrowser();
  subject.toggleBrowser();
  assert.equal(target.pane, 'browser', 'the pane toggle takes the page back from the floating player');
  assert.equal(subject.browserPresentationFor(target.id), null);
  subject.clearToastTimers?.();
});
