import type { Msg } from './store/types';

export type AssistantMessagePhase = 'commentary' | 'final_answer';

export function normalizeAssistantMessagePhase(value: unknown): AssistantMessagePhase | null {
  return value === 'commentary' || value === 'final_answer' ? value : null;
}

// Mutates the already-owned live assistant row. Phase-less/unknown providers
// keep the ordinary content path; only an explicitly provider-typed final
// answer gets a second transcript surface.
export function appendAssistantChunk(msg: Pick<Msg, 'content' | 'result'>, chunk: string, phase: unknown): void {
  if (normalizeAssistantMessagePhase(phase) === 'final_answer') {
    msg.result = (msg.result ?? '') + chunk;
  } else {
    msg.content += chunk;
  }
}

export function fullAssistantText(msg: Pick<Msg, 'content' | 'result'>): string {
  const commentary = msg.content.trim();
  const result = (msg.result ?? '').trim();
  return [commentary, result].filter(Boolean).join('\n\n');
}

// Job.Result is the daemon's COMBINED provider-history text for the whole turn:
// every commentary chunk plus the final answer, accumulated from the turn root
// (steer splits included). It is a RECOVERY source for a turn this renderer
// never saw, never a second transcript surface — adopting it onto a turn we
// already streamed reprints the entire turn underneath itself. This is the same
// rule the daemon applies to its own copy (session_store.go
// applyJournalEndLocked: terminal-result authority only while no typed chunk
// has claimed the structural split).
export function adoptsTerminalJobResult(streamed: Pick<Msg, 'content' | 'result'>[]): boolean {
  return !streamed.some((msg) => msg.content !== '' || (msg.result ?? '') !== '');
}
