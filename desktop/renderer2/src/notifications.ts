// Notification hygiene helpers.
//
// The pre-fix daemon automatically emitted a "Chat turn finished" notification
// on every turn end — a card the user explicitly never asked for. That producer
// is gone in current daemon source, but a still-running OLDER daemon keeps
// sending it, so the client drops it defensively. Combined with the store no
// longer echoing an inbound notify back to the daemon's `notify` channel (which
// re-broadcasts to this same controller and looped forever — the "spam like
// crazy"), a turn ending now produces no notification at all.

/**
 * True for the daemon's automatic per-turn "turn finished / ended" card. The
 * daemon builds these as the fixed strings "Chat turn finished" (status done)
 * and "Chat turn ended: <status>" (otherwise) — see turnEndNotifyPayload. Any
 * other notification (an agent's explicit notify, a permission alert) is kept.
 */
export function isAutoTurnEndNotice(body: unknown): boolean {
  const text = typeof body === 'string' ? body.trim() : '';
  return text === 'Chat turn finished' || text.startsWith('Chat turn ended');
}
