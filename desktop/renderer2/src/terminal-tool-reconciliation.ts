import type { TimelineEvent, ToolEvent } from './store/types';

const ACTIVE_TOOL_STATUS = new Set(['', 'in_progress', 'pending', 'running']);

function terminalToolStatus(turnStatus: string): string | null {
  switch (turnStatus.trim().toLowerCase()) {
    case 'done':
    case 'completed':
    case 'success':
      return 'completed';
    case 'failed':
    case 'error':
      return 'failed';
    case 'cancelled':
    case 'canceled':
      return 'cancelled';
    default:
      return null;
  }
}

// job:end is authoritative for foreground work. If a provider/tool transport
// omitted its final tool_call_update, keeping that call visually running after
// the parent turn ended is a lie. Background work has its own durable surface.
export function settleTerminalToolEvents(
  events: TimelineEvent[],
  turnStatus: string,
  endedAt = Date.now(),
): number {
  const status = terminalToolStatus(turnStatus);
  if (!status) return 0;
  let changed = 0;
  for (const event of events) {
    if (event.kind !== 'tool' || !ACTIVE_TOOL_STATUS.has(event.status.trim().toLowerCase())) continue;
    const tool = event as ToolEvent;
    tool.status = status;
    if (tool.endedAt == null) tool.endedAt = endedAt;
    changed += 1;
  }
  return changed;
}
