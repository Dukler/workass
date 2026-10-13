// Placement of the floating browser (approved mock browser-pip-v1 · B): pure,
// so the corner snapping and size limits are testable without a window.
export type FloatCorner = 'br' | 'bl' | 'tr' | 'tl';
export interface FloatGeometry { corner: FloatCorner; width: number; }
// reserve: the bottom band kept clear (the Working line and the gap above it).
export interface FloatContainer { width: number; height: number; reserve: number; }

const SIDE_MARGIN = 14;
const TOP_MARGIN = 12;
const PILL_ROOM = 38;          // the control pill sits outside the frame
const MIN_WIDTH = 220;
const MAX_WIDTH = 720;

// The widest frame that fits, given the page's proportions (height = width × aspect).
export function clampFloatWidth(width: number, aspect: number, container: FloatContainer): number {
  const ratio = aspect > 0 ? aspect : 0.625;
  const maxWidth = Math.max(MIN_WIDTH, Math.min(MAX_WIDTH, container.width - SIDE_MARGIN * 2));
  const maxHeight = Math.max(100, container.height - container.reserve - TOP_MARGIN - PILL_ROOM);
  let next = Math.max(MIN_WIDTH, Math.min(maxWidth, Math.round(width)));
  if (next * ratio > maxHeight) next = Math.max(120, Math.floor(maxHeight / ratio));
  return next;
}

// The frame's rectangle inside its container, anchored to its corner. Bottom
// corners sit on the reserved band with the pill above; top corners leave the
// pill room below.
export function floatFrame({ geometry, aspect, container }: { geometry: FloatGeometry; aspect: number; container: FloatContainer }) {
  const width = clampFloatWidth(geometry.width, aspect, container);
  const height = Math.round(width * (aspect > 0 ? aspect : 0.625));
  const right = geometry.corner === 'br' || geometry.corner === 'tr';
  const bottom = geometry.corner === 'br' || geometry.corner === 'bl';
  return {
    x: right ? container.width - SIDE_MARGIN - width : SIDE_MARGIN,
    y: bottom ? container.height - container.reserve - height : TOP_MARGIN,
    width,
    height,
  };
}

// Where a dropped frame snaps: the corner of the quadrant its centre is in.
export function nearestCorner(position: { x: number; y: number }, frame: { width: number; height: number }, container: FloatContainer): FloatCorner {
  const right = position.x + frame.width / 2 > container.width / 2;
  const bottom = position.y + frame.height / 2 > container.height / 2;
  return bottom ? (right ? 'br' : 'bl') : (right ? 'tr' : 'tl');
}

// The resize handle sits on the frame's free corner: for a frame anchored on
// the right, dragging the handle left makes it wider.
export function resizeFloatWidth(corner: FloatCorner, startWidth: number, dx: number): number {
  return corner === 'br' || corner === 'tr' ? startWidth - dx : startWidth + dx;
}
