import type { Chat, RightPane } from './types';

export type { RightPane };

/**
 * How long the info rail takes to slide open or shut (App: slidePane).
 * Snapping rewrapped the whole transcript in one frame, which read as the text
 * changing size and jumping (user 2026-11-08).
 */
export const PANE_SLIDE_MS = 220;
export const PANE_SLIDE_EASING = 'cubic-bezier(.2, .7, .3, 1)';

/** Where a transcript column sits: its left edge and its width, in px. */
export interface ColumnBox { x: number; width: number }

/**
 * The transcript column while the rail slides (user 2026-11-09: "the text
 * bounces"). Animating the grid alone narrowed the column every frame, so every
 * line rewrapped about 13 times in 220 ms. The column takes its final width at
 * once instead (one rewrap, in the first frame, whose moves the transcript
 * glides: transcript-flip.ts), and only its left edge travels, from where it
 * was to where it ends, with the rail. Null when nothing about it changes.
 */
export function columnSlideFrames(before: ColumnBox, after: ColumnBox & { marginLeft: number }): Keyframe[] | null {
  const shift = before.x - after.x;
  if (Math.abs(shift) < 0.5 && Math.abs(before.width - after.width) < 0.5) return null;
  const frame = (marginLeft: number): Keyframe => ({ width: `${after.width}px`, maxWidth: 'none', marginLeft: `${marginLeft}px` });
  return [frame(after.marginLeft + shift), frame(after.marginLeft)];
}

/** Whether going from `before` to `after` in the same chat slides the rail.
 *  Only the info rail opening or closing does: the browser's native view
 *  follows its slot in another process and would trail a moving column. */
export function railSlides(before: RightPane | null, after: RightPane | null): boolean {
  return (before === 'rail' && after === null) || (before === null && after === 'rail');
}

/**
 * The right column hosts ONE occupant, chosen per chat. A chat that has never
 * chosen (`pane === undefined`) defaults to the info rail — the historical
 * global default; `null` means the user explicitly closed the column.
 */
export function chatPane(chat: Pick<Chat, 'pane'> | null | undefined): RightPane | null {
  if (!chat) return null;
  return chat.pane === undefined ? 'rail' : chat.pane;
}

/**
 * Clicking a pane toggle: the same occupant closes the column; a different one
 * (or none) switches to it. Mirrors the old radio-with-off behaviour, now scoped
 * to a single chat's `pane`.
 */
export function nextPane(current: RightPane | null, clicked: RightPane): RightPane | null {
  return current === clicked ? null : clicked;
}
