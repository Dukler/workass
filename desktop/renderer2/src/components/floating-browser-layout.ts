// Placement of the floating browser (approved mock browser-pip-v1 · B): pure,
// so free placement, clamping and the size limits are testable without a
// window.
//
// The frame stays exactly where it is dropped. Its place is kept as fractions
// of the room it can travel in, so a chat area that shrinks or grows keeps it
// inside at the same relative position. Its size is the user's alone: the
// frame keeps one shape (the page's 1440 × 900 desktop layout) and only a
// resize changes it. Following the page's viewport made it change size
// whenever the docked pane, full screen or an agent re-laid the page out.
export interface FloatContainer { width: number; height: number; reserve: number; }
// reserve: the bottom band kept clear (the Working line and the gap above it).
export interface FloatPoint { x: number; y: number; }
export interface FloatRect { x: number; y: number; width: number; height: number; }
// Where the frame sits: 0 is the left/top end of its room, 1 the right/bottom.
export interface FloatPlacement { fx: number; fy: number; }
// Which edges a resize moves: an edge ('n', 'e', 's', 'w') or a corner ('ne', ...).
export type FloatEdge = 'n' | 'e' | 's' | 'w' | 'ne' | 'nw' | 'se' | 'sw';

export const FLOAT_ASPECT = 900 / 1440;
// The layout the page gets when it starts floating out of the docked pane or
// full screen (both re-lay it out to their own shape): the agent's default.
export const FLOAT_VIEWPORT = { width: 1440, height: 900 };
// Widths come in steps of 8 so that a 1440 × 900 page fills the frame to the
// pixel (8 × 5 is the smallest whole frame of that shape); any other width left
// a one-pixel line of the frame showing beside the page.
const WIDTH_STEP = 8;
export const DEFAULT_FLOAT_WIDTH = 344;
export const DEFAULT_PLACEMENT: FloatPlacement = { fx: 1, fy: 1 };
const SIDE_MARGIN = 14;
const TOP_MARGIN = 12;
const PILL_ROOM = 38;          // the control pill sits above the frame
const MIN_WIDTH = 224;
const MAX_WIDTH = 720;

const finite = (value: number, fallback: number) => (Number.isFinite(value) ? value : fallback);
const clamp = (value: number, min: number, max: number) => Math.min(max, Math.max(min, value));
const stepDown = (value: number) => Math.floor(value / WIDTH_STEP) * WIDTH_STEP;

// The room's outer edges for the frame: inside the chat area, the pill room
// kept above it, and clear of the reserved band at the bottom.
function bounds(container: FloatContainer) {
  return {
    left: SIDE_MARGIN,
    top: TOP_MARGIN + PILL_ROOM,
    right: container.width - SIDE_MARGIN,
    bottom: container.height - container.reserve,
  };
}

// The widest frame that fits, in whole steps (height = width × FLOAT_ASPECT).
// A chat area too small for the chosen width shows it smaller without
// forgetting the choice.
export function clampFloatWidth(width: number, container: FloatContainer, limit = Number.POSITIVE_INFINITY): number {
  const edge = bounds(container);
  const fits = Math.min(MAX_WIDTH, limit, edge.right - edge.left, (edge.bottom - edge.top) / FLOAT_ASPECT);
  const wanted = clamp(finite(width, DEFAULT_FLOAT_WIDTH), MIN_WIDTH, MAX_WIDTH);
  return Math.max(WIDTH_STEP * 15, stepDown(Math.min(wanted, fits) + 1e-6));
}

export function floatHeight(width: number): number {
  return Math.round(width * FLOAT_ASPECT);
}

// The positions a frame of this size may take.
export function floatRoom(size: { width: number; height: number }, container: FloatContainer) {
  const edge = bounds(container);
  return {
    minX: edge.left,
    minY: edge.top,
    maxX: Math.max(edge.left, edge.right - size.width),
    maxY: Math.max(edge.top, edge.bottom - size.height),
  };
}

export function clampFloatPosition(position: FloatPoint, size: { width: number; height: number }, container: FloatContainer): FloatPoint {
  const room = floatRoom(size, container);
  return {
    x: Math.round(clamp(finite(position.x, room.maxX), room.minX, room.maxX)),
    y: Math.round(clamp(finite(position.y, room.maxY), room.minY, room.maxY)),
  };
}

// A position, as the fractions that survive a change of the chat area.
export function placementOf(position: FloatPoint, size: { width: number; height: number }, container: FloatContainer, previous: FloatPlacement = DEFAULT_PLACEMENT): FloatPlacement {
  const room = floatRoom(size, container);
  const at = clampFloatPosition(position, size, container);
  return {
    fx: room.maxX > room.minX ? (at.x - room.minX) / (room.maxX - room.minX) : previous.fx,
    fy: room.maxY > room.minY ? (at.y - room.minY) / (room.maxY - room.minY) : previous.fy,
  };
}

export function positionOf(placement: FloatPlacement, size: { width: number; height: number }, container: FloatContainer): FloatPoint {
  const room = floatRoom(size, container);
  const fx = clamp(finite(placement.fx, 1), 0, 1);
  const fy = clamp(finite(placement.fy, 1), 0, 1);
  return { x: Math.round(room.minX + fx * (room.maxX - room.minX)), y: Math.round(room.minY + fy * (room.maxY - room.minY)) };
}

// The frame's rectangle inside its container.
export function floatFrame({ width, placement, container }: { width: number; placement: FloatPlacement; container: FloatContainer }): FloatRect {
  const w = clampFloatWidth(width, container);
  const size = { width: w, height: floatHeight(w) };
  return { ...positionOf(placement, size, container), ...size };
}

// A drag: the frame follows the pointer and stops at the edges of its room.
export function moveFloatFrame(start: FloatRect, delta: FloatPoint, container: FloatContainer): FloatRect {
  const at = clampFloatPosition({ x: start.x + delta.x, y: start.y + delta.y }, start, container);
  return { ...start, ...at };
}

// A resize from an edge or a corner. The frame keeps its shape, so the
// opposite edge (or corner) stays put and the other axis follows: from the
// left edge the right edge stays, from the top the bottom stays, otherwise the
// top-left stays. A corner projects the pointer's travel on the frame's
// diagonal, so the two axes never fight. The frame stops growing at the edges
// of the chat area rather than moving.
export function resizeFloatFrame(start: FloatRect, edge: FloatEdge, delta: FloatPoint, container: FloatContainer): FloatRect {
  const west = edge.includes('w');
  const east = edge.includes('e');
  const north = edge.includes('n');
  const south = edge.includes('s');
  const room = bounds(container);
  const right = start.x + start.width;
  const bottom = start.y + start.height;
  const wantedWidth = start.width + (east ? delta.x : west ? -delta.x : 0);
  const wantedHeight = start.height + (south ? delta.y : north ? -delta.y : 0);
  const desired = (east || west) && (north || south)
    ? (wantedWidth + wantedHeight * FLOAT_ASPECT) / (1 + FLOAT_ASPECT * FLOAT_ASPECT)
    : east || west ? wantedWidth : wantedHeight / FLOAT_ASPECT;
  const maxWidth = west ? right - room.left : room.right - start.x;
  const maxHeight = north ? bottom - room.top : room.bottom - start.y;
  const width = clampFloatWidth(desired, container, Math.min(maxWidth, maxHeight / FLOAT_ASPECT));
  const height = floatHeight(width);
  const at = clampFloatPosition({ x: west ? right - width : start.x, y: north ? bottom - height : start.y }, { width, height }, container);
  return { ...at, width, height };
}

// The app's zoom (View > Zoom In, Cmd +) makes a CSS pixel a fraction of a
// window pixel, while the native page sits only on whole window pixels and
// fills a 1440 × 900 shape exactly only at multiples of 8 × 5. The frame is
// snapped there, in CSS pixels, so page and frame land on the same pixels.
export function snapFloatFrame(rect: FloatRect, origin: FloatPoint, zoom: number): FloatRect {
  if (!(zoom > 0) || Math.abs(zoom - 1) < 1e-6) return rect;
  const width = Math.max(WIDTH_STEP, Math.round((rect.width * zoom) / WIDTH_STEP) * WIDTH_STEP) / zoom;
  return {
    x: Math.round((origin.x + rect.x) * zoom) / zoom - origin.x,
    y: Math.round((origin.y + rect.y) * zoom) / zoom - origin.y,
    width,
    height: width * FLOAT_ASPECT,
  };
}

// The zoom, from the window's width (window pixels) over the page's (CSS
// pixels), settled on Chromium's zoom steps (factors of 1.2 per level, in
// half levels as the View menu takes them).
export function hostZoom(outerWidth: number, innerWidth: number): number {
  if (!(outerWidth > 0) || !(innerWidth > 0)) return 1;
  const level = Math.round((Math.log(outerWidth / innerWidth) / Math.log(1.2)) * 2) / 2;
  return level === 0 ? 1 : 1.2 ** level;
}

// What the floating browser remembers: the user's width, and where it was
// dropped in each chat (most recent last, a bounded number of chats).
export interface FloatMemory { width: number; at: Record<string, FloatPlacement>; fallback: FloatPlacement; }
const MAX_REMEMBERED = 200;

function validPlacement(value: unknown): FloatPlacement | null {
  const raw = value as Partial<FloatPlacement> | null;
  if (!raw || typeof raw !== 'object' || !Number.isFinite(raw.fx) || !Number.isFinite(raw.fy)) return null;
  return { fx: clamp(Number(raw.fx), 0, 1), fy: clamp(Number(raw.fy), 0, 1) };
}

// Reads the stored value; the v1 shape (one corner for every chat) becomes
// the starting place of chats that have none yet.
export function parseFloatMemory(current: string | null, legacy: string | null): FloatMemory {
  const empty: FloatMemory = { width: DEFAULT_FLOAT_WIDTH, at: {}, fallback: DEFAULT_PLACEMENT };
  try {
    const raw = JSON.parse(current ?? 'null') as { width?: unknown; at?: Record<string, unknown> } | null;
    if (raw && Number.isFinite(raw.width)) {
      const at: Record<string, FloatPlacement> = {};
      for (const [chatId, value] of Object.entries(raw.at && typeof raw.at === 'object' ? raw.at : {})) {
        const placement = validPlacement(value);
        if (placement) at[chatId] = placement;
      }
      return { width: Number(raw.width), at, fallback: DEFAULT_PLACEMENT };
    }
  } catch { /* unreadable: start over */ }
  try {
    const old = JSON.parse(legacy ?? 'null') as { corner?: unknown; width?: unknown } | null;
    if (old && Number.isFinite(old.width)) {
      const corner = String(old.corner);
      const fallback = { fx: corner === 'bl' || corner === 'tl' ? 0 : 1, fy: corner === 'tr' || corner === 'tl' ? 0 : 1 };
      return { width: Number(old.width), at: {}, fallback };
    }
  } catch { /* unreadable: start over */ }
  return empty;
}

export function floatPlacementFor(memory: FloatMemory, chatId: string): FloatPlacement {
  return memory.at[chatId] ?? memory.fallback;
}

export function rememberFloat(memory: FloatMemory, change: { chatId?: string; placement?: FloatPlacement; width?: number }): FloatMemory {
  const at = { ...memory.at };
  if (change.chatId && change.placement) {
    delete at[change.chatId];
    at[change.chatId] = change.placement;
    const ids = Object.keys(at);
    for (const id of ids.slice(0, Math.max(0, ids.length - MAX_REMEMBERED))) delete at[id];
  }
  return { ...memory, width: change.width ?? memory.width, at };
}

export function serializeFloatMemory(memory: FloatMemory): string {
  return JSON.stringify({ width: memory.width, at: memory.at });
}
