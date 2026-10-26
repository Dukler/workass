import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { createServer, type ViteDevServer } from 'vite';
import {
  FLOAT_ASPECT, clampFloatWidth, floatFrame, floatPlacementFor, floatRoom, floatWidthStep, hostZoom, moveFloatFrame, parseFloatMemory,
  placementOf, positionOf, rememberFloat, resizeFloatFrame, serializeFloatMemory, snapFloatFrame,
} from '../src/components/floating-browser-layout.ts';
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

test('the frame starts at the bottom right, clear of the Working line, at the desktop proportions', () => {
  const frame = floatFrame({ width: 344, placement: { fx: 1, fy: 1 }, container });
  assert.deepEqual(frame, { x: 900 - 14 - 344, y: 700 - 46 - 215, width: 344, height: 215 });
  const top = floatFrame({ width: 344, placement: { fx: 0, fy: 0 }, container });
  assert.equal(top.x, 14);
  assert.equal(top.y, 12 + 38, 'the pill above the frame stays inside the chat area');
});

test('the frame stays exactly where it is dropped: no corner, no magnet', () => {
  const start = floatFrame({ width: 344, placement: { fx: 1, fy: 1 }, container });
  for (const drop of [{ x: 300, y: 200 }, { x: 15, y: 51 }, { x: 541, y: 438 }, { x: 200, y: 300 }, { x: 401, y: 123 }]) {
    const moved = moveFloatFrame(start, { x: drop.x - start.x, y: drop.y - start.y }, container);
    assert.deepEqual({ x: moved.x, y: moved.y }, drop);
    assert.deepEqual({ width: moved.width, height: moved.height }, { width: start.width, height: start.height }, 'a move never changes the size');
    // stored as fractions and read back in the same chat area: the same place
    const placement = placementOf(moved, moved, container);
    assert.deepEqual(positionOf(placement, moved, container), drop);
  }
});

test('a drag past an edge or a corner clamps the frame fully inside, and it follows the pointer back', () => {
  const start = floatFrame({ width: 344, placement: { fx: 0.5, fy: 0.5 }, container });
  const room = floatRoom(start, container);
  assert.deepEqual(room, { minX: 14, maxX: 900 - 14 - 344, minY: 50, maxY: 700 - 46 - 215 });
  const pushed = moveFloatFrame(start, { x: -5000, y: 9000 }, container);
  assert.deepEqual({ x: pushed.x, y: pushed.y }, { x: room.minX, y: room.maxY }, 'clamped against the left edge and the Working line');
  const corner = moveFloatFrame(start, { x: 5000, y: -5000 }, container);
  assert.deepEqual({ x: corner.x, y: corner.y }, { x: room.maxX, y: room.minY });
  const back = moveFloatFrame(start, { x: -(start.x - room.minX) + 40, y: 0 }, container);
  assert.equal(back.x, room.minX + 40, 'it follows as soon as the grip is reachable again');
});

test('a smaller chat area keeps the frame inside at the same relative place, and a larger one gives it back', () => {
  const frame = floatFrame({ width: 344, placement: { fx: 0.25, fy: 0.75 }, container });
  const narrow = { width: 600, height: 520, reserve: 46 };
  const shrunk = floatFrame({ width: 344, placement: { fx: 0.25, fy: 0.75 }, container: narrow });
  assert.equal(shrunk.width, 344, 'a chat area that still fits the frame keeps its size');
  assert.ok(shrunk.x >= 14 && shrunk.x + shrunk.width <= 600 - 14);
  assert.ok(shrunk.y >= 50 && shrunk.y + shrunk.height <= 520 - 46);
  assert.equal(Math.round((shrunk.x - 14) / (600 - 28 - 344) * 100), 25);
  assert.deepEqual(floatFrame({ width: 344, placement: { fx: 0.25, fy: 0.75 }, container }), frame);
  const tiny = floatFrame({ width: 344, placement: { fx: 1, fy: 1 }, container: { width: 260, height: 300, reserve: 46 } });
  assert.ok(tiny.x >= 14 && tiny.x + tiny.width <= 260 - 14, 'too small an area shows the frame smaller, still inside');
  assert.ok(tiny.y + tiny.height <= 300 - 46);
});

// The expansion (2026-10-25, real app): the frame's height followed the page's
// viewport, so docking in the narrow pane (a 320 × 941 layout) or full screen
// changed the frame's size on the next float. The frame now has one shape and
// only the user's size.
test('the size is the user\'s alone: one shape, whatever the page is laid out as', () => {
  assert.equal(clampFloatWidth(2000, container), 720);
  assert.equal(clampFloatWidth(10, container), 224);
  assert.equal(clampFloatWidth(Number.NaN, container), 344);
  for (const width of [224, 300, 344, 401, 517, 720]) {
    const frame = floatFrame({ width, placement: { fx: 1, fy: 1 }, container });
    assert.equal(frame.width % 8, 0, 'whole steps, so a 1440 × 900 page fills the frame to the pixel');
    assert.equal(frame.height, frame.width * 5 / 8);
  }
  const short = { width: 900, height: 300, reserve: 46 };
  assert.ok(clampFloatWidth(500, short) * FLOAT_ASPECT <= 300 - 46 - 12 - 38, 'a short chat area never runs the frame past the Working line');
});

test('a resize from any edge or corner keeps the opposite side put and the frame\'s shape', () => {
  const start = { x: 300, y: 200, width: 344, height: 215 };
  const right = start.x + start.width, bottom = start.y + start.height;
  const shape = (r: { width: number; height: number }) => assert.equal(r.height, r.width * 5 / 8);
  const nw = resizeFloatFrame(start, 'nw', { x: -80, y: -50 }, container);
  assert.ok(nw.width > start.width); shape(nw);
  assert.deepEqual([nw.x + nw.width, nw.y + nw.height], [right, bottom], 'top-left corner: the bottom-right corner stays');
  const se = resizeFloatFrame(start, 'se', { x: 80, y: 50 }, container);
  assert.deepEqual([se.x, se.y], [start.x, start.y], 'bottom-right corner: the top-left corner stays');
  assert.ok(se.width > start.width); shape(se);
  const ne = resizeFloatFrame(start, 'ne', { x: 80, y: -50 }, container);
  assert.deepEqual([ne.x, ne.y + ne.height], [start.x, bottom]);
  const sw = resizeFloatFrame(start, 'sw', { x: -80, y: 50 }, container);
  assert.deepEqual([sw.x + sw.width, sw.y], [right, start.y]);
  const e = resizeFloatFrame(start, 'e', { x: 64, y: 30 }, container);
  assert.deepEqual([e.x, e.y, e.width], [start.x, start.y, 408], 'an edge follows its own axis only');
  const w = resizeFloatFrame(start, 'w', { x: 64, y: 0 }, container);
  assert.deepEqual([w.x + w.width, w.width], [right, 280]);
  const s = resizeFloatFrame(start, 's', { x: 0, y: 40 }, container);
  assert.deepEqual([s.x, s.y, s.width], [start.x, start.y, 408]);
  const n = resizeFloatFrame(start, 'n', { x: 0, y: -40 }, container);
  assert.equal(n.y + n.height, bottom);
  assert.equal(resizeFloatFrame(start, 'se', { x: -900, y: -900 }, container).width, 224, 'the minimum');
  const grown = resizeFloatFrame(start, 'se', { x: 4000, y: 4000 }, container);
  assert.deepEqual([grown.x, grown.y], [start.x, start.y], 'growing past the chat area stops the frame instead of moving it');
  assert.ok(grown.x + grown.width <= 900 - 14 && grown.y + grown.height <= 700 - 46);
  const big = resizeFloatFrame({ x: 14, y: 50, width: 344, height: 215 }, 'se', { x: 4000, y: 4000 }, { width: 2000, height: 1400, reserve: 46 });
  assert.equal(big.width, 720, 'the maximum');
  for (const edge of ['n', 'e', 's', 'w', 'ne', 'nw', 'se', 'sw'] as const) {
    for (const delta of [{ x: 3, y: 2 }, { x: -170, y: 90 }, { x: 999, y: -999 }]) {
      const r = resizeFloatFrame(start, edge, delta, container);
      assert.ok(r.x >= 14 && r.y >= 50 && r.x + r.width <= 900 - 14 && r.y + r.height <= 700 - 46, `${edge} stays inside`);
      shape(r);
    }
  }
});

test('at any app zoom the frame lands on whole window pixels, so the page drawn in it has crisp edges', () => {
  assert.equal(hostZoom(1440, 1440), 1);
  assert.equal(hostZoom(1440, 1314), 1.2 ** 0.5, 'Cmd +');
  assert.equal(hostZoom(1440, 1577), 1.2 ** -0.5, 'Cmd −');
  assert.equal(hostZoom(1456, 1440), 1, 'a window frame a few pixels wide is not a zoom');
  const rect = { x: 230, y: 103, width: 560, height: 350 };
  assert.deepEqual(snapFloatFrame(rect, { x: 288, y: 40 }, 1), rect);
  for (const zoom of [1.2 ** 0.5, 1.2, 1.2 ** -0.5, 1.2 ** -1]) {
    const origin = { x: 287.9964904785156, y: 39.995155334472656 };
    const at = snapFloatFrame(rect, origin, zoom);
    const win = { x: (origin.x + at.x) * zoom, y: (origin.y + at.y) * zoom, w: at.width * zoom, h: at.height * zoom };
    for (const v of Object.values(win)) assert.ok(Math.abs(v - Math.round(v)) < 1e-6, `whole window pixels at zoom ${zoom}`);
    assert.equal(Math.round(win.w) % 8, 0);
    assert.ok(Math.abs(win.h - win.w * 5 / 8) < 1e-6);
    assert.ok(Math.abs(at.x - rect.x) < 1 && Math.abs(at.width - rect.width) < 8 / zoom);
  }
});

test('at the app zoom a slow resize grows 8 window pixels at a time, never skipping one', () => {
  const container = { width: 1200, height: 800, reserve: 46 };
  for (const zoom of [1.2 ** 0.5, 1.2, 1.2 ** -0.5]) {
    const step = floatWidthStep(zoom);
    const origin = { x: 288, y: 40 };
    let start = floatFrame({ width: 344, placement: { fx: 0.2, fy: 0.2 }, container, step });
    let last = Math.round(snapFloatFrame(start, origin, zoom).width * zoom);
    for (let dx = 1; dx < 200; dx += 1) {
      const next = resizeFloatFrame(start, 'e', { x: dx, y: 0 }, container, step);
      const shown = Math.round(snapFloatFrame(next, origin, zoom).width * zoom);
      assert.ok(shown - last === 0 || shown - last === 8, `zoom ${zoom}: ${last} -> ${shown}`);
      last = shown;
    }
    // and the frame at rest keeps the size the resize ended on
    const end = resizeFloatFrame(start, 'e', { x: 133, y: 0 }, container, step);
    assert.equal(floatFrame({ width: end.width, placement: { fx: 0.2, fy: 0.2 }, container, step }).width, end.width);
  }
  assert.equal(floatWidthStep(1), 8);
});

test('positions are remembered per chat, with the width shared, and the v1 corner seeds new chats', () => {
  let memory = parseFloatMemory(null, JSON.stringify({ corner: 'tl', width: 300 }));
  assert.equal(memory.width, 300);
  assert.deepEqual(floatPlacementFor(memory, 'any'), { fx: 0, fy: 0 });
  memory = rememberFloat(memory, { chatId: 'a', placement: { fx: 0.2, fy: 0.4 } });
  memory = rememberFloat(memory, { chatId: 'b', placement: { fx: 0.9, fy: 0.1 }, width: 360 });
  const reread = parseFloatMemory(serializeFloatMemory(memory), null);
  assert.deepEqual(floatPlacementFor(reread, 'a'), { fx: 0.2, fy: 0.4 });
  assert.deepEqual(floatPlacementFor(reread, 'b'), { fx: 0.9, fy: 0.1 });
  assert.deepEqual(floatPlacementFor(reread, 'c'), { fx: 1, fy: 1 });
  assert.equal(reread.width, 360);
  assert.deepEqual(parseFloatMemory('{"width":"x"}', 'nope'), { width: 344, at: {}, fallback: { fx: 1, fy: 1 } });
  let many = reread;
  for (let i = 0; i < 260; i += 1) many = rememberFloat(many, { chatId: `chat-${i}`, placement: { fx: 0, fy: 0 } });
  assert.equal(Object.keys(many.at).length, 200, 'a bounded number of chats');
  assert.ok(many.at['chat-259'] && !many.at.a, 'the oldest are forgotten first');
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
