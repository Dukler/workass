import { useRef, useState } from 'react';
import { store, useApp } from '../store/store';
import { has } from '../wire/api';
import { ModelIcon } from '../icons';
import { availableRateLimitReset, prepareRateLimitResetAttempt, rateLimitResetExpiry, type RateLimitResetAttempt } from '../plan-usage';

// A free reset of a provider's limits lives with plan usage, in the
// composer's usage popover; the usage ring carries a green dot while one is
// waiting. It used to sit in the sidebar's account menu, which is gone
// (owner 2026-11-08).

/** Every provider with an earned reset waiting, in the store's order. */
export function useAvailableResets() {
  const app = useApp();
  return Object.values(app.planUsageByProvider)
    .map((snapshot) => ({ providerId: snapshot.providerId, reset: availableRateLimitReset(snapshot.rateLimitResetCredits) }))
    .filter((entry): entry is { providerId: string; reset: NonNullable<typeof entry.reset> } => entry.reset != null);
}

export function ResetCreditRows() {
  const resets = useAvailableResets();
  if (!resets.length) return null;
  return <>{resets.map((entry) => <ResetCreditRow key={entry.providerId} providerId={entry.providerId} reset={entry.reset} />)}</>;
}

function ResetCreditRow({ providerId, reset }: { providerId: string; reset: NonNullable<ReturnType<typeof availableRateLimitReset>> }) {
  const app = useApp();
  const [busy, setBusy] = useState(false);
  const attempt = useRef<RateLimitResetAttempt | null>(null);
  const name = store.providerName(providerId) ?? providerId;
  const brand = store.providerBrand(providerId);
  const active = app.chats.find((chat) => chat.id === app.activeId);
  const sessionId = active?.providerId === providerId ? active.sessionId : undefined;
  const canUse = has('appChatUseRateLimitReset');
  const use = async () => {
    if (busy || !canUse) return;
    const creditId = reset.credit?.id;
    attempt.current = prepareRateLimitResetAttempt(attempt.current, reset, () => (
      typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function'
        ? crypto.randomUUID()
        : `workass-reset-${Date.now()}-${Math.random().toString(36).slice(2)}`
    ));
    setBusy(true);
    try {
      const result = await store.useRateLimitReset(providerId, sessionId || undefined, attempt.current.idempotencyKey, creditId);
      switch (result?.outcome) {
        case 'reset':
        case 'alreadyRedeemed':
          attempt.current = null;
          store.addToast('Reset aplicado', `${name || 'El proveedor'} actualizó tus límites.`);
          break;
        case 'nothingToReset':
          attempt.current = null;
          store.addToast('Reset guardado', 'Todavía no hay una ventana elegible para reiniciar.');
          break;
        case 'noCredit':
          attempt.current = null;
          store.addToast('Reset no disponible', 'El proveedor ya no encuentra ese reset gratis.');
          break;
        default:
          throw new Error('El proveedor no devolvió un resultado de reset reconocido.');
      }
    } catch (error) {
      const detail = error instanceof Error ? error.message : String(error || 'Error desconocido');
      store.addToast('No se pudo confirmar el reset', `${detail} Podés reintentar sin gastar dos créditos.`);
    } finally {
      setBusy(false);
    }
  };
  const expiry = rateLimitResetExpiry(reset.credit?.expiresAt);
  return (
    <div className="rstrow" role="group" aria-label={`${reset.count} reset gratis de ${name}`}>
      {brand && <span className="rstmark" aria-hidden><ModelIcon provider={brand} /></span>}
      <span className="rsttx">
        <span className="rstt">Reset gratis de {name || 'tu proveedor'} <span className="rstn">×{reset.count}</span></span>
        <span className="rsts">{reset.credit?.description || 'Reinicia tus límites'}{expiry ? ` · ${expiry}` : ''}</span>
      </span>
      <button type="button" className="rstuse" disabled={!canUse || busy} onClick={() => { void use(); }}>
        {busy ? 'Aplicando…' : 'Usar reset'}
      </button>
    </div>
  );
}
