import type { Chat, RightPane } from './types';

export type { RightPane };

/**
 * How long the info rail takes to slide open or shut (styles: #root.pane-anim).
 * Snapping rewrapped the whole transcript in one frame, which read as the text
 * changing size and jumping (user 2026-11-08).
 */
export const PANE_SLIDE_MS = 220;

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
