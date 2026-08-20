import type { Msg } from '../store/types';

export interface SteeringPresentation {
  transcriptMessages: Msg[];
  steeringTrayMessages: Msg[];
}

function steerRoot(message: Msg): string | null {
  if (message.role !== 'user' || !message.steerState) return null;
  return message.turnRootId?.trim() || null;
}

// Delivery chronology and visual chronology serve different jobs. The actor's
// canonical array keeps the exact provider receipt boundary, including the
// assistant slices on either side of a live steer. The transcript must not use
// that boundary to insert a user bubble into text the person already watched.
//
// While the turn is live, its steer rows have one visible owner in the tray by
// the composer. Once the turn is terminal, the same rows are projected after
// all assistant slices for that turn. No row is copied or mutated, and ordinary
// user messages retain their canonical positions.
export function projectSteeringPresentation(messages: readonly Msg[]): SteeringPresentation {
  const steerRoots = new Set<string>();
  for (const message of messages) {
    const root = steerRoot(message);
    if (root) steerRoots.add(root);
  }

  const assistantRoot = (message: Msg): string | null => {
    if (message.role !== 'assistant') return null;
    return message.turnRootId?.trim() || (steerRoots.has(message.id) ? message.id : null);
  };

  const activeRoots = new Set<string>();
  for (const message of messages) {
    const root = assistantRoot(message);
    if (root && (message.status === 'running' || message.status === 'pending')) activeRoots.add(root);
  }

  const steeringTrayMessages: Msg[] = [];
  const trayIds = new Set<string>();
  const settledByRoot = new Map<string, Msg[]>();
  for (const message of messages) {
    const root = steerRoot(message);
    if (!root) continue;
    if (message.steerBoundary === 'waiting' || activeRoots.has(root)) {
      steeringTrayMessages.push(message);
      trayIds.add(message.id);
      continue;
    }
    const rows = settledByRoot.get(root);
    if (rows) rows.push(message);
    else settledByRoot.set(root, [message]);
  }

  const lastAssistantByRoot = new Map<string, Msg>();
  for (const message of messages) {
    if (message.steerBoundary === 'waiting') continue;
    const root = assistantRoot(message);
    if (root && settledByRoot.has(root)) lastAssistantByRoot.set(root, message);
  }
  const projectedRoots = new Set(lastAssistantByRoot.keys());

  const transcriptMessages: Msg[] = [];
  for (const message of messages) {
    if (message.steerBoundary === 'waiting' || trayIds.has(message.id)) continue;
    const root = steerRoot(message);
    if (root && projectedRoots.has(root)) continue;

    transcriptMessages.push(message);
    const assistantTurnRoot = assistantRoot(message);
    if (assistantTurnRoot && lastAssistantByRoot.get(assistantTurnRoot) === message) {
      transcriptMessages.push(...(settledByRoot.get(assistantTurnRoot) ?? []));
    }
  }

  return { transcriptMessages, steeringTrayMessages };
}
