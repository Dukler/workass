// The tool-image grid's geometry, computed instead of laid out, so a gallery
// far from the reader can stand in as one box of exactly its height and only
// the rows near the viewport mount (one message of a screenshot chat carried
// 6,180 tool images in 1,269 galleries: 56,000 DOM nodes for one row).
//
// It mirrors app.css, and tests/tool-image-layout.test.ts reads app.css to
// keep the two in step:
//   .tool-images        { display: grid; grid-template-columns: repeat(auto-fit, minmax(150px, 1fr)); gap: 7px; }
//   .ri                 { border: 1px solid transparent }   (padding 0)
//   .ri-media           { width: 100%; aspect-ratio: var(--iw) / var(--ih); }
//   .ri-tool            { width: min(100%, natural + 2px, 340px * aspect + 2px) }
//   .ri-tool .ri-media  { max-height: 340px; }
// A grid row is as tall as its tallest frame (items stretch).

export const TOOL_IMAGE_MIN_COLUMN = 150;
export const TOOL_IMAGE_GAP = 7;
export const TOOL_IMAGE_MEDIA_MAX = 340;
export const TOOL_IMAGE_FRAME_BORDER = 2;

export type ToolImageGridLayout = {
  columns: number;
  rowHeights: number[];
  /** Total content height: every row and the gaps between them. */
  height: number;
};

export function toolImageGridLayout(width: number, frames: ReadonlyArray<readonly [number, number]>): ToolImageGridLayout {
  const count = frames.length;
  if (count === 0 || !(width > 0)) return { columns: 1, rowHeights: [], height: 0 };
  // auto-fit: as many 150px tracks as fit; tracks no item occupies collapse,
  // so a short gallery shares the whole width among its items.
  const fit = Math.max(1, Math.floor((width + TOOL_IMAGE_GAP) / (TOOL_IMAGE_MIN_COLUMN + TOOL_IMAGE_GAP)));
  const columns = Math.min(fit, count);
  const column = (width - (columns - 1) * TOOL_IMAGE_GAP) / columns;
  const media = Math.max(0, column - TOOL_IMAGE_FRAME_BORDER);
  const rowHeights: number[] = [];
  for (let start = 0; start < count; start += columns) {
    let row = 0;
    for (let index = start; index < Math.min(count, start + columns); index++) {
      const [iw, ih] = frames[index];
      // The frame is its image's size within the track (app.css .ri-tool):
      // the track's width, the image's natural size or the media cap,
      // whichever is smallest.
      const height = Math.min(media * ih / iw, TOOL_IMAGE_MEDIA_MAX, ih) + TOOL_IMAGE_FRAME_BORDER;
      if (height > row) row = height;
    }
    rowHeights.push(row);
  }
  return { columns, rowHeights, height: spanHeight(rowHeights, 0, rowHeights.length) };
}

/** Rows [from, to) and the gaps between them. */
export function spanHeight(rowHeights: readonly number[], from: number, to: number): number {
  if (to <= from) return 0;
  let height = (to - from - 1) * TOOL_IMAGE_GAP;
  for (let index = from; index < to; index++) height += rowHeights[index];
  return height;
}

export type ToolImageRows = { from: number; to: number };

/**
 * The rows that meet [viewTop, viewBottom], given where the grid's content
 * starts; all three in the same coordinates. Empty when none do.
 */
export function toolImageRowsInView(rowHeights: readonly number[], gridTop: number, viewTop: number, viewBottom: number): ToolImageRows {
  let from = -1;
  let to = -1;
  let top = gridTop;
  for (let index = 0; index < rowHeights.length; index++) {
    const bottom = top + rowHeights[index];
    if (bottom >= viewTop && top <= viewBottom) {
      if (from < 0) from = index;
      to = index + 1;
    } else if (top > viewBottom) break;
    top = bottom + TOOL_IMAGE_GAP;
  }
  return from < 0 ? { from: 0, to: 0 } : { from, to };
}

/**
 * Spacer heights that stand in for the rows before `from` and after `to`. A
 * spacer is one grid item spanning every column, so the row gap that follows
 * (or precedes) it is the one the first hidden row had.
 */
export function toolImageSpacers(layout: ToolImageGridLayout, rows: ToolImageRows): { before: number; after: number } {
  const count = layout.rowHeights.length;
  return {
    before: rows.from > 0 ? spanHeight(layout.rowHeights, 0, rows.from) : 0,
    after: rows.to < count ? spanHeight(layout.rowHeights, rows.to, count) : 0,
  };
}
