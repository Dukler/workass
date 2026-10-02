import type { Msg, ResultEvent } from './store/types';

export type AssistantMessagePhase = 'commentary' | 'final_answer';

export function normalizeAssistantMessagePhase(value: unknown): AssistantMessagePhase | null {
  return value === 'commentary' || value === 'final_answer' ? value : null;
}

// Mutates the already-owned live assistant row. Phase-less/unknown providers
// keep the ordinary content path; only an explicitly provider-typed final
// answer is stored separately and anchored in the shared transcript timeline.
type AssistantOutput = Pick<Msg, 'content' | 'result'> & Partial<Pick<Msg, 'id' | 'events'>>;

function appendResultSpan(msg: AssistantOutput, start: number, end: number): void {
  if (!msg.events || !msg.id || end <= start) return;
  const last = msg.events.at(-1);
  if (last?.kind === 'result' && last.at === msg.content.length && last.end === start) {
    last.end = end;
    return;
  }
  msg.events.push({ key: `timeline:result:${msg.id}:${start}`, kind: 'result', at: msg.content.length, start, end });
}

export function ensureAssistantResultAnchor(msg: AssistantOutput): void {
  if (msg.result && !msg.events?.some(event => event.kind === 'result')) appendResultSpan(msg, 0, msg.result.length);
}

export function appendAssistantChunk(msg: AssistantOutput, chunk: string, phase: unknown): void {
  ensureAssistantResultAnchor(msg);
  if (normalizeAssistantMessagePhase(phase) === 'final_answer') {
    const start = msg.result?.length ?? 0;
    msg.result = (msg.result ?? '') + chunk;
    appendResultSpan(msg, start, msg.result.length);
  } else {
    msg.content += chunk;
  }
}

export function fullAssistantText(msg: AssistantOutput): string {
  const spans = (msg.events ?? []).filter((event): event is ResultEvent => event.kind === 'result')
    .map((event, order) => ({ event, order }))
    .sort((a, b) => a.event.at - b.event.at || a.order - b.order);
  if (spans.length) {
    let cursor = 0;
    let text = '';
    for (const { event } of spans) {
      const at = Math.max(cursor, Math.min(event.at, msg.content.length));
      text += msg.content.slice(cursor, at) + (msg.result ?? '').slice(event.start, event.end);
      cursor = at;
    }
    return (text + msg.content.slice(cursor)).trim();
  }
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
