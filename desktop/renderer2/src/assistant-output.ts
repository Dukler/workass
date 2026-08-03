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

// Old builds persisted this synthetic sentence as assistant prose. Status
// chrome now owns cancellation, so hide only the exact legacy sentinel while
// retaining every real partial response verbatim.
export function restoredAssistantContent(role: unknown, status: unknown, content: unknown): string {
  const text = content == null ? '' : String(content);
  return role === 'assistant' && status === 'cancelled' && text.trim() === 'Detenido.' ? '' : text;
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

// Heal turns already persisted by the build that adopted the combined result
// unconditionally: an exact content/result twin is that duplication, never an
// agent answering itself verbatim.
export function restoredAssistantResult(content: unknown, result: unknown): string | undefined {
  if (result == null) return undefined;
  const text = String(result);
  return text !== '' && text === (content == null ? '' : String(content)) ? undefined : text;
}
