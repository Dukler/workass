// Continuar on a reply a subscription's usage limit stopped (owner
// 2026-11-18, mock desktop/docs/mocks/limit-resume/option-a-v2.html). The
// line belongs to the chat's latest reply only: once anything is written or
// answered after it (the "continue" itself included), it is gone.
import type { Msg } from '../store/types';

const WEEKDAY = ['dom', 'lun', 'mar', 'mié', 'jue', 'vie', 'sáb'];
const MONTH = ['ene', 'feb', 'mar', 'abr', 'may', 'jun', 'jul', 'ago', 'sep', 'oct', 'nov', 'dic'];
const pad = (n: number) => String(n).padStart(2, '0');
const clockText = (d: Date) => `${pad(d.getHours())}:${pad(d.getMinutes())}`;
// Automatizaciones' day words ("hoy", "mañana", "vie 21 nov"), kept here so
// this stays free of the store.
function dayText(d: Date, now: Date): string {
  const start = (x: Date) => new Date(x.getFullYear(), x.getMonth(), x.getDate()).getTime();
  const days = Math.round((start(d) - start(now)) / 86_400_000);
  if (days === 0) return 'hoy';
  if (days === 1) return 'mañana';
  return `${WEEKDAY[d.getDay()]} ${d.getDate()} ${MONTH[d.getMonth()]}`;
}

type Row = Pick<Msg, 'id' | 'role' | 'notice' | 'usageLimit'>;

/** The reply a limit stopped that is still where the chat stands, or null. */
export function currentLimitedReply<T extends Row>(messages: readonly T[]): T | null {
  for (let index = messages.length - 1; index >= 0; index--) {
    const message = messages[index];
    if (message.role === 'user' && message.notice) continue; // Workass's wake-up, not the person
    return message.role === 'assistant' && message.usageLimit ? message : null;
  }
  return null;
}

/** "a las 18:40", "mañana a las 09:00", "vie 21 nov a las 18:40". */
export function limitWhenText(at: Date, now: Date): string {
  const day = dayText(at, now);
  return day === 'hoy' ? `a las ${clockText(at)}` : `${day} a las ${clockText(at)}`;
}

export type LimitLineState =
  | { kind: 'stopped'; back: 'unknown' | 'back' | Date }
  | { kind: 'scheduled'; at: Date };

/** What the line says, from the reply's limit and a scheduled Continuar. */
export function limitLineState(resetsAt: string | undefined, scheduledAt: string | null, now: Date): LimitLineState {
  if (scheduledAt) {
    const at = new Date(scheduledAt);
    if (!Number.isNaN(at.getTime())) return { kind: 'scheduled', at };
  }
  const reset = resetsAt ? new Date(resetsAt) : null;
  if (!reset || Number.isNaN(reset.getTime())) return { kind: 'stopped', back: 'unknown' };
  return { kind: 'stopped', back: reset.getTime() <= now.getTime() ? 'back' : reset };
}
