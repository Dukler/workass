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
    transcriptMessages.push(message);
  }

  return { transcriptMessages, steeringTrayMessages };
}
