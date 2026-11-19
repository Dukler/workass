import { useEffect, useState } from 'react';
import type { Chat, Msg } from '../store/types';
import { IcGauge, IcScheduled } from '../icons';
import { callThrow, has } from '../wire/api';
import { isTagged, localId, machineOf } from '../wire/machineIds';
import { store } from '../store/store';
import type { LimitContinueScheduled } from '../wire/types';
import { limitLineState, limitWhenText } from '../chat/usage-limit';

// The line under a reply a subscription's usage limit stopped
// (chat/usage-limit.ts): which subscription, and when it comes back. With
// Ajustes · General · "Continuar solo" on, the daemon has scheduled "continue"
// for that moment; the line says so and offers Cancelar. There is no
// Continuar button (owner 2026-11-19: a limit can hit while nobody is there).
// Once "continue" is in the chat the reply is no longer the latest and this
// line is gone.
type Args = Record<string, string>;

function invoke<T>(chat: Chat, channel: 'chat:limit-continue' | 'chat:limit-continue-cancel' | 'chat:limit-continue-status', method: 'limitContinue' | 'limitContinueCancel' | 'limitContinueStatus', args: Args): Promise<T> {
  const tabId = isTagged(chat.id) ? localId(chat.id) : chat.id;
  const full = { ...args, tabId, chatId: chat.chatId ?? '' };
  if (isTagged(chat.id)) return store.invokeOnMachine<T>(machineOf(chat.id), channel, full);
  if (!has(method)) return Promise.reject(new Error('Este Workass no puede continuar chats todavía.'));
  return callThrow(method, full as never) as Promise<T>;
}

export function UsageLimitLine({ chat, msg }: { chat: Chat; msg: Msg }) {
  const limit = msg.usageLimit!;
  const [scheduled, setScheduled] = useState<LimitContinueScheduled | null>(null);
  const [busy, setBusy] = useState(false);
  const [now, setNow] = useState(() => new Date());
  // The daemon schedules as the turn fails, which can land just after this
  // line mounts: read it again with every tick of the clock below.
  useEffect(() => {
    let live = true;
    const read = () => invoke<{ scheduled: LimitContinueScheduled | null }>(chat, 'chat:limit-continue-status', 'limitContinueStatus', {})
      .then((reply) => { if (live) setScheduled(reply?.scheduled?.messageId === msg.id ? reply.scheduled : null); })
      .catch(() => {});
    void read();
    const soon = window.setTimeout(() => void read(), 3_000);
    const timer = window.setInterval(() => { setNow(new Date()); void read(); }, 30_000);
    return () => { live = false; window.clearTimeout(soon); window.clearInterval(timer); };
  }, [chat.id, chat.chatId, msg.id]);
  const state = limitLineState(limit.resetsAt, scheduled?.at ?? null, now);
  const run = async (action: () => Promise<void>) => {
    if (busy) return;
    setBusy(true);
    try { await action(); } catch (error) {
      store.addToast('No se pudo', String((error as Error)?.message || error));
    } finally { setBusy(false); }
  };
  const cancel = () => run(async () => {
    if (!scheduled) return;
    await invoke(chat, 'chat:limit-continue-cancel', 'limitContinueCancel', { id: scheduled.id });
    setScheduled(null);
  });
  const label = limit.label || 'Suscripción';
  if (state.kind === 'scheduled') {
    return (
      <div className="note limitline" role="status">
        <span className="ic" aria-hidden="true"><IcScheduled /></span>
        <span className="lnm">{label}</span>
        <span className="st">· «continue» sale {limitWhenText(state.at, now)}</span>
        <button type="button" className="lact quiet" disabled={busy} onClick={() => void cancel()}>Cancelar</button>
      </div>
    );
  }
  const back = state.back === 'unknown' ? '' : state.back === 'back' ? ' · ya volvió' : ` · vuelve ${limitWhenText(state.back, now)}`;
  return (
    <div className="note limitline" role="status">
      <span className="ic" aria-hidden="true"><IcGauge /></span>
      <span className="lnm">{label}</span>
      <span className="st">· límite de uso{back}</span>
    </div>
  );
}
