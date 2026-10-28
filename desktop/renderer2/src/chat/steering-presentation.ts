import type { Msg } from '../store/types';

export interface SteeringPresentation {
  transcriptMessages: Msg[];
  steeringTrayMessages: Msg[];
}

function steerRoot(message: Msg): string | null {
  if (message.role !== 'user' || !message.steerState) return null;
  return message.turnRootId?.trim() || null;
}

// The actor's canonical array keeps the exact provider receipt boundary,
// including the assistant slices on either side of a live steer. Only a steer
// whose delivery is genuinely unresolved (sending/uncertain) stays owned by the
// tray while its exact root is active. Accepted/applied rows are transcript-
// visible immediately at their canonical position. No row is copied or moved.
export function projectSteeringPresentation(messages: readonly Msg[]): SteeringPresentation {
  const activeRoots = new Set<string>();
  for (const message of messages) {
    if (message.role !== 'assistant' || message.steerBoundary === 'waiting' || message.status !== 'running') continue;
    activeRoots.add(message.turnRootId?.trim() || message.id);
  }

  const steeringTrayMessages: Msg[] = [];
  const trayIds = new Set<string>();
  for (const message of messages) {
    const root = steerRoot(message);
    if (!root) continue;
    const unresolved = message.steerState === 'sending' || message.steerState === 'uncertain';
    if (unresolved && activeRoots.has(root)) {
      steeringTrayMessages.push(message);
      trayIds.add(message.id);
    }
  }

  const transcriptMessages: Msg[] = [];
  for (const message of messages) {
    // A staged continuation is identity reserved for a later consumption
    // receipt, not a visible assistant row. The accepted user owner itself is
    // already transcript-visible; receipt commit reveals this same continuation
    // after it without moving or copying the user row.
    if (message.role === 'assistant' && message.steerBoundary === 'waiting') continue;
    if (trayIds.has(message.id)) continue;
    if (isEmptySettledReply(message)) continue;
    transcriptMessages.push(message);
  }

  return { transcriptMessages, steeringTrayMessages };
}

// A reply that finished having said and done nothing. Claude Code wakes the
// agent once per finished background task, and a wake it answers silently
// still left a bubble with only a copy button and a timestamp — a column of
// blank rows in a chat running 15 reviewers (DepthVision speed, 2026-10-15).
// Failed and cancelled replies stay: their state is the message.
export function isEmptySettledReply(message: Msg): boolean {
  return message.role === 'assistant' && message.status === 'done'
    && isNoReplyText(message.content ?? '') && !message.events?.length && !message.images?.length;
}

// Claude Code's own words for "nothing to say". It writes them as a synthetic
// reply after a rate-limit stop or an interrupted turn, and the model copies
// them when a background task wakes it up with nothing to add. Claude Code
// never shows either; here they read as the end of the previous reply (user
// 2026-10-28). The Claude host stops sending them; this hides the stored ones.
// Mirrors NO_REPLY_PLACEHOLDERS in scripts/claude-native-host.mjs.
const NO_REPLY_PLACEHOLDERS = new Set(['No response requested.', '(no content)']);

function isNoReplyText(content: string): boolean {
  const text = content.trim();
  return !text || NO_REPLY_PLACEHOLDERS.has(text);
}
