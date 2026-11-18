import { useEffect, useState } from 'react';
import type { Chat, Msg } from '../store/types';
import { IcGauge, IcScheduled } from '../icons';
import { callThrow, has } from '../wire/api';
import { isTagged, localId, machineOf } from '../wire/machineIds';
import { store } from '../store/store';
import type { LimitContinueScheduled } from '../wire/types';
import { limitLineState, limitWhenText } from '../chat/usage-limit';

// The Continuar line under a reply a subscription's usage limit stopped
// (chat/usage-limit.ts). Continuar works at any time: before the limit is back
// it schedules "continue" for then (durable, in the daemon); after, it sends
// it now. Once "continue" is in the chat the reply is no longer the latest
// and this line is gone.
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
  const [sent, setSent] = useState(false);
  const [now, setNow] = useState(() => new Date());
  useEffect(() => {
    let live = true;
    invoke<{ scheduled: LimitContinueScheduled | null }>(chat, 'chat:limit-continue-status', 'limitContinueStatus', {})
      .then((reply) => { if (live) setScheduled(reply?.scheduled?.messageId === msg.id ? reply.scheduled : null); })
      .catch(() => {});
    return () => { live = false; };
  }, [chat.id, chat.chatId, msg.id]);
  // "vuelve a las 18:40" turns into "ya volvió" on its own.
  useEffect(() => {
    const timer = window.setInterval(() => setNow(new Date()), 30_000);
    return () => window.clearInterval(timer);
  }, []);
  if (sent) return null;
  const state = limitLineState(limit.resetsAt, scheduled?.at ?? null, now);
  const run = async (action: () => Promise<void>) => {
    if (busy) return;
    setBusy(true);
    try { await action(); } catch (error) {
      store.addToast('No se pudo', String((error as Error)?.message || error));
    } finally { setBusy(false); }
  };
  const continueNow = () => run(async () => {
    const reply = await invoke<{ sent?: boolean; scheduled?: LimitContinueScheduled | null }>(chat, 'chat:limit-continue', 'limitContinue', { messageId: msg.id });
    if (reply?.scheduled) setScheduled(reply.scheduled);
    else if (reply?.sent) setSent(true);
  });
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
      <button type="button" className="lact" disabled={busy} onClick={() => void continueNow()}>Continuar</button>
    </div>
  );
}
