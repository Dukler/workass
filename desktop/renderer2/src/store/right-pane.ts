import type { Chat, RightPane } from './types';

export type { RightPane };

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
