// R8 transcript export — render a chat's timeline as a document-style Markdown
// string. Tool/step events become blockquotes so the prose stays readable when
// pasted into an issue, PR, or notes doc.

import type { Chat, Msg, TimelineEvent, ToolEvent, PlanEvent, ThinkingEvent, RestoredEvent } from './store/types';

const TOOL_STATUS: Record<string, string> = {
  in_progress: 'en curso', pending: 'pendiente', completed: 'listo', failed: 'falló', error: 'falló',
};
const PLAN_MARK: Record<string, string> = { completed: 'x', in_progress: '~', pending: ' ' };

function eventMarkdown(ev: TimelineEvent): string[] {
  if (ev.kind === 'thinking') {
    const t = (ev as ThinkingEvent).text.trim().split('\n')[0].slice(0, 200);
    return [`> _Pensó:_ ${t}`];
  }
  if (ev.kind === 'plan') {
    return (ev as PlanEvent).entries.map((e) => `> - [${PLAN_MARK[e.status] ?? ' '}] ${e.content}`);
  }
  if (ev.kind === 'compaction') return ['> _Contexto compactado._'];
  if (ev.kind === 'restored') return [`> _Estado restaurado a antes del turno ${(ev as RestoredEvent).turnSeq}._`];
  const t = ev as ToolEvent;
  const status = TOOL_STATUS[t.status] ?? t.status;
  const head = `> **${t.title || 'Herramienta'}** — ${status}`;
  const out: string[] = [head];
  if (t.command) out.push(`> \`${t.command}\``);
  else if (t.location) out.push(`> \`${t.location}\``);
  return out;
}

function assistantBody(m: Msg): string[] {
  const events = [...m.events].filter((e) => e.kind !== 'thinking' || true).sort((a, b) => a.at - b.at);
  const out: string[] = [];
  let cursor = 0;
  for (const ev of events) {
    const at = Math.min(Math.max(ev.at, 0), m.content.length);
    if (at > cursor) { const prose = m.content.slice(cursor, at).trim(); if (prose) out.push(prose, ''); cursor = at; }
    out.push(...eventMarkdown(ev), '');
  }
  const tail = m.content.slice(cursor).trim();
  if (tail) out.push(tail, '');
  if (out.length === 0) out.push('_(sin contenido)_', '');
  return out;
}

export function chatToMarkdown(chat: Chat): string {
  const lines: string[] = [];
  lines.push(`# ${chat.title || 'Conversación'}`, '');
  const meta: string[] = [];
  if (chat.providerName) meta.push(`Proveedor: ${chat.providerName}`);
  if (chat.cwd) meta.push(`Directorio: \`${chat.cwd}\``);
  meta.push(`Exportado: ${new Date().toLocaleString('es')}`);
  lines.push(`_${meta.join(' · ')}_`, '', '---', '');

  for (const m of chat.messages) {
    if (m.status === 'pending') continue;
    if (m.role === 'user') {
      lines.push('### Tú', '', m.content.trim() || '_(vacío)_', '');
    } else {
      const tag = m.status === 'failed' ? ' (error)' : m.status === 'cancelled' ? ' (detenido)' : '';
      lines.push(`### Agente${tag}`, '', ...assistantBody(m));
    }
  }
  return lines.join('\n').replace(/\n{3,}/g, '\n\n').trim() + '\n';
}

// Trigger a client-side download of the markdown as a .md file.
export function downloadMarkdown(chat: Chat) {
  const md = chatToMarkdown(chat);
  const safe = (chat.title || 'conversacion').replace(/[^\w.-]+/g, '-').slice(0, 60) || 'conversacion';
  const blob = new Blob([md], { type: 'text/markdown;charset=utf-8' });
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = `${safe}.md`;
  document.body.appendChild(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}
