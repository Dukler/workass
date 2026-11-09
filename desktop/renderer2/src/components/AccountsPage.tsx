import { useEffect, useRef, useState, useSyncExternalStore } from 'react';
import type { ReactNode } from 'react';
import { store, useApp } from '../store/store';
import { call, callThrow, has } from '../wire/api';
import { ModelIcon } from '../icons';
import { pushEscapeLayer } from '../markdown/html-frames';
import { settingsFromReply } from '../settings-blob';
import type { PlanUsageSnapshot, ProviderRecord } from '../wire/types';
import {
  accountResetConsentFrom, accountResetRows, accountWindowRows, extraUsageLine, hasUsableReset, manageHost, payPerUseRows,
  resetResultToast, subscriptionProviders, weeklyEntry, weeklyPace, weeklyPaceText, withAccountResetConsent,
  type AccountResetRow,
} from '../accounts';

// Cuentas (owner 2026-11-09, mock cuentas-v3): one page over the chat area,
// opened from the person icon at the foot of the left panel. Each
// subscription account shows its plan, its usage windows, how the week is
// going, its resets (with an inline confirm) and its spend; then the
// accounts that pay per use and the model servers on this machine. Resets
// left the chat box on purpose (2026-11-08) and live only here.

// ---- open / closed -------------------------------------------------------

let pageOpen = false;
const openListeners = new Set<() => void>();
function setOpen(next: boolean) {
  if (pageOpen === next) return;
  pageOpen = next;
  for (const listener of openListeners) listener();
}
export function openAccounts() { setOpen(true); }
export function closeAccounts() { setOpen(false); }
export function toggleAccounts() { setOpen(!pageOpen); }
function subscribeOpen(listener: () => void) {
  openListeners.add(listener);
  return () => { openListeners.delete(listener); };
}
const openSnapshot = () => pageOpen;
export function useAccountsOpen(): boolean {
  return useSyncExternalStore(subscribeOpen, openSnapshot, openSnapshot);
}

// ---- consent to use a provider's stored login -----------------------------

// null until the settings blob has been read once.
let consentIds: string[] | null = null;
let consentLoad: Promise<void> | null = null;
const consentListeners = new Set<() => void>();
function setConsent(next: string[]) {
  consentIds = next;
  for (const listener of consentListeners) listener();
}
function loadConsent(force = false): Promise<void> {
  if (consentLoad && !force) return consentLoad;
  consentLoad = (async () => {
    if (!has('getSettings')) { setConsent([]); return; }
    const reply = await call('getSettings');
    if (reply === undefined) { consentLoad = null; return; }
    setConsent(accountResetConsentFrom(settingsFromReply(reply)));
  })();
  return consentLoad;
}
function subscribeConsent(listener: () => void) {
  consentListeners.add(listener);
  return () => { consentListeners.delete(listener); };
}
const consentSnapshot = () => consentIds;
function useConsent(): string[] | null {
  const value = useSyncExternalStore(subscribeConsent, consentSnapshot, consentSnapshot);
  useEffect(() => { void loadConsent(); }, []);
  return value;
}

/** settings:set replaces the blob: read it, change only the consent key,
 *  write it back against the revision read (another window may save). */
async function writeConsent(providerId: string, granted: boolean): Promise<void> {
  if (!has('getSettings') || !has('setSettings')) throw new Error('Este Workass no puede guardar ajustes.');
  for (let attempt = 0; attempt < 3; attempt++) {
    const reply = await callThrow('getSettings');
    if (!reply || typeof reply !== 'object') throw new Error('No se pudieron leer los ajustes.');
    const next = withAccountResetConsent(settingsFromReply(reply), providerId, granted);
    const revision = (reply as { settingsRevision?: unknown }).settingsRevision;
    try {
      if (typeof revision === 'string' && revision) await callThrow('setSettings', next, { expectedRevision: revision });
      else await callThrow('setSettings', next);
      setConsent(accountResetConsentFrom(next));
      return;
    } catch (error) {
      if (!/settings changed since they were read/.test(String((error as Error)?.message ?? error))) throw error;
    }
  }
  throw new Error('Otra ventana estaba guardando ajustes. Probá de nuevo.');
}

// ---- helpers -----------------------------------------------------------------

function errorText(error: unknown): string {
  const text = error instanceof Error ? error.message : String(error || '');
  return text.trim() || 'Error desconocido.';
}

function newRequestKey(): string {
  return typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function'
    ? crypto.randomUUID()
    : `workass-reset-${Date.now()}-${Math.random().toString(36).slice(2)}`;
}

/** Re-render every half minute so reset times and the pace stay current. */
function useClock(): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), 30_000);
    return () => clearInterval(id);
  }, []);
  return now;
}

const PLAN_FRESH_MS = 60_000;

function IcClose() {
  return <svg viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.4" strokeLinecap="round" aria-hidden="true"><path d="M4 4l8 8M12 4l-8 8" /></svg>;
}

// A circular arrow: a reset puts the limit back to the start.
function IcReset() {
  return (
    <svg className="acct-ri" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.4" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <path d="M3.2 8a4.8 4.8 0 1 0 1.5-3.5" /><path d="M3.2 2.6v2.6h2.6" />
    </svg>
  );
}

const primedResets = new Set<string>();

// A person: the accounts you are signed in with.
function IcAccount() {
  return (
    <svg viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <circle cx="8" cy="5.6" r="2.6" /><path d="M2.8 13.6c.6-2.6 2.7-4.1 5.2-4.1s4.6 1.5 5.2 4.1" />
    </svg>
  );
}

/** The foot icon between Ajustes and Actualizaciones. Its dot says a reset
 *  can be used right now. */
export function AccountsFootButton() {
  const app = useApp();
  const open = useAccountsOpen();
  const ready = hasUsableReset(app.planUsageByProvider);
  // With consent already given, read those resets once at start so the dot
  // is right before the daemon's own unattended read comes around.
  useEffect(() => {
    const wanting = app.providers.filter((provider) => provider.enabled && !provider.disabledByUser
      && provider.accountResetConsent && !primedResets.has(provider.id));
    if (!wanting.length || !has('accountsResetsRefresh')) return;
    void loadConsent().then(() => {
      for (const provider of wanting) {
        if (!consentIds?.includes(provider.id) || primedResets.has(provider.id)) continue;
        primedResets.add(provider.id);
        void call('accountsResetsRefresh', provider.id);
      }
    });
  }, [app.providers]);
  const title = ready ? 'Cuentas · tenés un reset para usar' : 'Cuentas';
  return (
    <button type="button" className={`footbtn${open ? ' on' : ''}`} title={title} aria-label={title}
      aria-pressed={open} onClick={toggleAccounts}>
      <IcAccount />
      {ready && <span className="footdot" aria-hidden="true" />}
    </button>
  );
}

// ---- page ----------------------------------------------------------------------

export function AccountsPage() {
  const app = useApp();
  const consent = useConsent();
  const now = useClock();
  const pageRef = useRef<HTMLElement>(null);
  useEffect(() => pushEscapeLayer(closeAccounts), []);
  // The chat stays mounted under the page; take focus off its composer so
  // typing never lands in a box nobody can see.
  useEffect(() => { pageRef.current?.focus({ preventScroll: true }); }, []);
  // Like Ajustes, read the provider list fresh: which agents are on decides
  // which accounts show.
  useEffect(() => { void store.refreshProviderSettings(); }, []);

  const subscriptions = subscriptionProviders(app.providers, app.planUsageByProvider);
  const payPerUse = payPerUseRows(app.providers, app.planUsageByProvider);

  return (
    <section className="acct-page" aria-label="Cuentas" ref={pageRef} tabIndex={-1}>
      <div className="acct-head">
        <span>Cuentas</span>
        <button type="button" className="acct-x" title="Cerrar · Esc" aria-label="Cerrar" onClick={closeAccounts}><IcClose /></button>
      </div>
      <div className="acct-body">
        {subscriptions.map((provider) => (
          <AccountSection
            key={provider.id} provider={provider} snapshot={app.planUsageByProvider[provider.id]}
            consent={consent} now={now}
          />
        ))}
        {payPerUse.length > 0 && (
          <>
            <div className="acct-sec">Claves de API y modelos locales</div>
            <div className="acct-api">
              {payPerUse.map((row) => (
                <div className="acct-api-row" key={row.key}>
                  <span className="acct-api-mark" aria-hidden="true">{row.brand ? <ModelIcon provider={row.brand} /> : null}</span>
                  <span className="acct-api-name">{row.name}</span>
                  <span className="acct-api-k">{row.detail}</span>
                  <span className="acct-api-m">{row.dot && <span className={`acct-sd${row.dot === 'off' ? ' off' : ''}`} />}{row.status}</span>
                </div>
              ))}
            </div>
          </>
        )}
        {subscriptions.length === 0 && payPerUse.length === 0 && (
          <div className="acct-none"><b>No hay cuentas para mostrar</b>Prendé un agente en Ajustes · Agentes y aparece acá.</div>
        )}
      </div>
    </section>
  );
}

function AccountSection({ provider, snapshot, consent, now }: {
  provider: ProviderRecord;
  snapshot: PlanUsageSnapshot | undefined;
  consent: string[] | null;
  now: number;
}) {
  const app = useApp();
  const name = store.providerName(provider.id) ?? provider.name ?? provider.id;
  const brand = provider.assistantBrand || store.providerBrand(provider.id);
  const [plan, setPlan] = useState<{ loading: boolean; error: string }>({ loading: false, error: '' });
  const [resets, setResets] = useState<{ loading: boolean; error: string }>({ loading: false, error: '' });
  const [consentBusy, setConsentBusy] = useState(false);
  const needsConsent = !!provider.accountResetConsent;
  const consented = !!consent?.includes(provider.id);

  const refreshPlan = async () => {
    if (!has('appChatRefreshPlanUsage')) return;
    setPlan({ loading: true, error: '' });
    try {
      await callThrow('appChatRefreshPlanUsage', provider.id);
      setPlan({ loading: false, error: '' });
    } catch (error) {
      setPlan({ loading: false, error: errorText(error) });
    }
  };
  const refreshResets = async () => {
    if (!has('accountsResetsRefresh')) return;
    setResets({ loading: true, error: '' });
    try {
      await callThrow('accountsResetsRefresh', provider.id);
      setResets({ loading: false, error: '' });
    } catch (error) {
      setResets({ loading: false, error: errorText(error) });
    }
  };

  // Opening the page reads the account once, unless a read just landed.
  useEffect(() => {
    const age = snapshot ? Date.now() - Date.parse(snapshot.capturedAt) : Infinity;
    if (!(age < PLAN_FRESH_MS)) void refreshPlan();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [provider.id]);
  useEffect(() => {
    if (needsConsent && consented) void refreshResets();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [provider.id, needsConsent, consented]);

  const grant = async (granted: boolean) => {
    setConsentBusy(true);
    try {
      await writeConsent(provider.id, granted);
      // Withdrawn: the daemon refuses the read and drops what it had read.
      if (!granted) await call('accountsResetsRefresh', provider.id);
    } catch (error) {
      store.addToast('No se pudo guardar el permiso', errorText(error));
    } finally {
      setConsentBusy(false);
    }
  };

  const account = snapshot?.account;
  const windows = accountWindowRows(snapshot?.entries, now);
  const pace = weeklyPace(weeklyEntry(snapshot?.entries), now);
  const paceText = pace ? weeklyPaceText(pace, now) : null;
  const summary = snapshot?.rateLimitResetCredits;
  const rows = accountResetRows(summary, now);
  const spend = extraUsageLine(snapshot?.extraUsage);
  const host = manageHost(account?.manageUrl);
  const who = [account?.email, account?.organization].filter(Boolean).join(' · ');
  const connected = !provider.needsLogin && !!snapshot && (!!account || windows.length > 0);
  const state = provider.needsLogin ? 'sin sesión' : connected ? 'conectada' : plan.loading ? 'conectando…' : '';
  const showsResets = !!provider.accountResetSupported || needsConsent;

  let resetsBody: ReactNode = null;
  if (needsConsent && consent === null) {
    resetsBody = null;
  } else if (needsConsent && !consented) {
    resetsBody = (
      <div className="acct-consent">
        <button type="button" className="acct-use" disabled={consentBusy} onClick={() => { void grant(true); }}>
          Usar mi login de {name} para ver los resets
        </button>
        {provider.accountResetConsent?.note && <span className="acct-note">{provider.accountResetConsent.note}</span>}
      </div>
    );
  } else if (resets.error) {
    resetsBody = (
      <div className="acct-none acct-err">
        <b>No se pudieron leer los resets</b>{resets.error}
        <button type="button" className="acct-link" onClick={() => { void refreshResets(); }}>Reintentar</button>
      </div>
    );
  } else if (rows.length) {
    resetsBody = rows.map((row) => <ResetRowView key={row.key} row={row} providerId={provider.id} name={name} activeChatId={app.activeId} />);
  } else if (resets.loading && !summary) {
    resetsBody = <div className="acct-none">Buscando resets…</div>;
  } else if (snapshot || summary) {
    resetsBody = <div className="acct-none"><b>No hay resets ahora</b>Cuando tengas uno, aparece acá con su vencimiento.</div>;
  }

  return (
    <div className="acct">
      <div className="acct-h">
        {brand && <span className="acct-mark" aria-hidden="true"><ModelIcon provider={brand} /></span>}
        <span className="acct-nm">{name}</span>
        {account?.plan && <span className="acct-plan">{account.plan}</span>}
        {who && <span className="acct-who">{who}</span>}
        {state && <span className="acct-st"><span className={`acct-sd${connected ? '' : ' off'}`} />{state}</span>}
      </div>
      {paceText && (
        <div className={`acct-pace${paceText.warn ? ' bad' : ''}`}>{paceText.line}<small>{paceText.detail}</small></div>
      )}
      {windows.length > 0 && (
        <div className="acct-cols">
          {windows.map((row) => (
            <div className="acct-u" key={row.key}>
              <div className="acct-top"><span className="acct-l">{row.label}</span><span className={`acct-v${row.hot ? ' hot' : ''}`}>{row.value}</span></div>
              <div className="acct-ub"><i className={row.hot ? 'hot' : ''} style={{ width: `${row.percent ?? 0}%` }} /></div>
              {row.sub && <div className="acct-sub">{row.sub}</div>}
            </div>
          ))}
        </div>
      )}
      {!snapshot && plan.loading && <div className="acct-none">Leyendo el uso…</div>}
      {plan.error && !windows.length && (
        <div className="acct-none acct-err"><b>No se pudo leer el uso</b>{plan.error}</div>
      )}
      {showsResets && resetsBody && <div className="acct-rs">{resetsBody}</div>}
      {(spend || host || (needsConsent && consented)) && (
        <div className="acct-cred">
          {spend && <span>{spend.label} <b>{spend.value}</b>{spend.tail ? ` · ${spend.tail}` : ''}</span>}
          {host && <a href={account?.manageUrl} target="_blank" rel="noreferrer">Administrar en {host} ↗</a>}
          {needsConsent && consented && (
            <button type="button" className="acct-link" disabled={consentBusy} onClick={() => { void grant(false); }}>Dejar de usar mi login</button>
          )}
        </div>
      )}
    </div>
  );
}

function ResetRowView({ row, providerId, name, activeChatId }: { row: AccountResetRow; providerId: string; name: string; activeChatId: string | null }) {
  const [asking, setAsking] = useState(false);
  const [busy, setBusy] = useState(false);
  // One key per logical click: a retry of the same reset reuses it, so the
  // provider never spends two.
  const attempt = useRef<{ rowKey: string; requestKey: string } | null>(null);
  const canUse = has('appChatUseRateLimitReset');

  const use = async () => {
    if (busy || !canUse) return;
    if (attempt.current?.rowKey !== row.key) attempt.current = { rowKey: row.key, requestKey: newRequestKey() };
    const active = store.state.chats.find((chat) => chat.id === activeChatId);
    const sessionId = active?.providerId === providerId ? active.sessionId || undefined : undefined;
    setBusy(true);
    try {
      const result = await store.useRateLimitReset(providerId, sessionId, attempt.current.requestKey, row.creditId);
      const toast = resetResultToast(result, row, name);
      attempt.current = null;
      setAsking(false);
      store.addToast(toast.title, toast.body);
      // The refilled windows read fresh in the bars.
      void call('appChatRefreshPlanUsage', providerId);
    } catch (error) {
      store.addToast('No se pudo usar el reset', `${errorText(error)} Si reintentás, no se gastan dos.`);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className={`acct-rrow${row.usable ? ' go' : ''}`}>
      <IcReset />
      <span className="acct-tx">
        <span className="acct-tt">{row.title}</span>
        {row.detail && <span className="acct-ts">{row.detail}</span>}
      </span>
      {!row.usable ? (
        <span className="acct-why">{row.reason}</span>
      ) : asking ? (
        <span className="acct-confirm">
          <span className="acct-q">{row.question}</span>
          <button type="button" className="acct-use" disabled={busy} onClick={() => { void use(); }}>{busy ? 'Usando…' : 'Sí, usarlo'}</button>
          <button type="button" className="acct-no" disabled={busy} onClick={() => setAsking(false)}>No</button>
        </span>
      ) : (
        <button type="button" className="acct-use" disabled={!canUse} onClick={() => setAsking(true)}>Usar</button>
      )}
    </div>
  );
}
