// The bell and its Parte (owner 2026-11-10, desktop/docs/mocks/connectors-alerts/
// popover-v2.html section 0 and variant A; docs/specs/connectors-alerts.md).
//
// The bell sits at the foot of the left panel, between Automatizaciones and
// Actualizaciones. It carries no number: a filled dot in the worst undecided
// severity's colour, a hollow ring when every firing alert has a chat or is
// silenced, nothing when none fire. It swings once when an alert is announced
// (a new episode; a Grafana repeat never moves it). Deliberately not gated on
// prefers-reduced-motion: this Mac runs Reduce Motion on and the owner wants it.
//
// Clicking it opens the Parte over whatever is on screen: one paragraph per
// (connector, machine), "Hoy" for what resolved, the Registro, the verbs, and
// the hand-off sentence. A chat opens only from "Abrir sin enviar" / "Abrir y
// enviar", through the app's normal new-chat path with Ajustes · General's
// agent, and then the alert is linked to it on the daemon that holds it.

import { useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react';
import { createPortal } from 'react-dom';
import type { Alert, ConnectorTarget } from '../wire/types';
import { store, useApp } from '../store/store';
import { pushEscapeLayer } from '../markdown/html-frames';
import { splitId, tagId } from '../wire/machineIds';
import { has } from '../wire/api';
import { newChatAgents, newChatModel } from '../general-settings';
import { buildWorkspaceGroups, normalizeWorkspacePath } from '../workspaces';
import {
  agoText, attachmentsText, bellState, buildHandoff, clientMachine, clock, composeHandoff, firingSegments, groupSituations,
  isSilenced, isUndecided, ownerSegments, pastSegments, registroLines, repeatText, resolvedEntries,
  resolveTarget, setAside, SEVERITY_WORD, shortFolder, situationTitle,
  type AlertEntry, type ChatInfo, type Seg, type Situation,
} from '../alerts-model';
import {
  actOnEntries, alertEntries, alertsSupported, closeParte, currentSwing, startAlerts, toggleParte,
  useAlertsVersion, useParteOpen,
} from '../alerts';
import { closeAccounts } from './AccountsPage';
import { closeAutomations } from './Automations';
import { workingSince } from './SidebarV2';
import { WorkspaceBrowser } from './WorkspaceBrowser';

// ---- glyphs -------------------------------------------------------------------

export function IcBell() {
  return (
    <svg viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <path d="M4 11V7a4 4 0 0 1 8 0v4l1.2 1.5H2.8z" /><path d="M6.5 14a1.6 1.6 0 0 0 3 0" />
    </svg>
  );
}
function IcChevron({ open }: { open: boolean }) {
  return (
    <svg className="pa-chev" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      {open ? <path d="M4.5 6.5L8 10l3.5-3.5" /> : <path d="M6 4l4 4-4 4" />}
    </svg>
  );
}

const SEV_CLASS = { critical: 'c', warning: 'w', info: 'i' } as const;

/** Re-renders each 30 s so "hace 6 min" stays true. */
function useNow(): Date {
  const [now, setNow] = useState(() => new Date());
  useEffect(() => {
    const timer = window.setInterval(() => setNow(new Date()), 30_000);
    return () => window.clearInterval(timer);
  }, []);
  return now;
}

// ---- chats an alert is linked to ------------------------------------------------

function linkedChatKey(alert: Alert): string | null {
  if (!alert.link?.tabId) return null;
  const machine = clientMachine(alert.link.machineId, store.localMachineId());
  return machine ? tagId(machine, alert.link.tabId) : alert.link.tabId;
}

function chatOf(alert: Alert): ChatInfo | null {
  const key = linkedChatKey(alert);
  if (!key) return null;
  const chat = store.chat(key);
  if (!chat) return null;
  const running = store.isChatRunning(chat.id);
  const since = running ? workingSince(chat, []) : 0;
  return { key: chat.id, title: chat.title, working: running ? new Date(since || Date.now()) : null };
}

function goToChat(alert: Alert) {
  const key = linkedChatKey(alert);
  if (!key) return;
  closeParte();
  if (store.state.settingsOpen) store.closeSettings();
  closeAccounts();
  closeAutomations();
  store.switchChat(key);
}

function Segs({ segs }: { segs: Seg[] }) {
  return (
    <>
      {segs.map((seg, i) => typeof seg === 'string' ? <span key={i}>{seg}</span>
        : 'rule' in seg ? <span key={i} className="rule">{seg.rule}</span>
        : <span key={i} className="mut">{seg.muted}</span>)}
    </>
  );
}

// ---- the bell -------------------------------------------------------------------------

let swungSeq = 0;

export function AlertsFootButton() {
  useAlertsVersion();
  const open = useParteOpen();
  const now = useNow();
  const ref = useRef<HTMLButtonElement>(null);
  const [swinging, setSwinging] = useState(false);
  useEffect(() => { startAlerts(); }, []);
  const seq = currentSwing();
  // One swing per announcement, even when the bar remounts (Ajustes draws it too).
  useEffect(() => {
    if (seq > swungSeq) { swungSeq = seq; setSwinging(true); }
  }, [seq]);
  if (!alertsSupported()) return null;
  const state = bellState(alertEntries().map((e) => e.alert), now);
  const title = state.kind === 'dot' ? 'Alertas · hay algo sin decidir'
    : state.kind === 'ring' ? 'Alertas · todas tienen chat o están silenciadas' : 'Alertas';
  return (
    <>
      <button type="button" ref={ref} className={`footbtn bell${open ? ' on' : ''}${swinging ? ' swing' : ''}`}
        title={title} aria-label={title} aria-haspopup="dialog" aria-expanded={open}
        onAnimationEnd={() => setSwinging(false)} onClick={() => toggleParte()}>
        <IcBell />
        {state.kind === 'dot' && <span className={`footdot sev-${SEV_CLASS[state.severity]}`} aria-hidden="true" />}
        {state.kind === 'ring' && <span className="footdot ring" aria-hidden="true" />}
      </button>
      {open && <Parte anchor={ref} />}
    </>
  );
}

// ---- the Parte --------------------------------------------------------------------------

type Open = { kind: 'registro' | 'said' | 'hand'; key: string } | null;

function Parte({ anchor }: { anchor: React.RefObject<HTMLButtonElement | null> }) {
  useApp();
  useAlertsVersion();
  const now = useNow();
  const ref = useRef<HTMLDivElement>(null);
  const [pos, setPos] = useState<{ left: number; bottom: number } | null>(null);
  const [open, setOpen] = useState<Open>(null);
  const [list, setList] = useState<'' | 'before' | 'aside'>('');

  useLayoutEffect(() => {
    const place = () => {
      const button = anchor.current;
      if (!button) return;
      const bar = (button.closest('.foot') ?? button.closest('.footbar') ?? button) as HTMLElement;
      const side = (button.closest('aside') ?? bar) as HTMLElement;
      const barRect = bar.getBoundingClientRect();
      setPos({ left: side.getBoundingClientRect().left + 10, bottom: window.innerHeight - barRect.top + 6 });
    };
    place();
    window.addEventListener('resize', place);
    return () => window.removeEventListener('resize', place);
  }, [anchor]);

  useEffect(() => {
    const onDown = (e: MouseEvent) => {
      const target = e.target as Node;
      if (ref.current?.contains(target) || anchor.current?.contains(target)) return;
      // The folder browser is its own layer above the Parte.
      if ((target as Element).closest?.('.ovl, [role="dialog"][aria-modal="true"]')) return;
      closeParte();
    };
    document.addEventListener('mousedown', onDown);
    const pop = pushEscapeLayer(() => closeParte());
    return () => { document.removeEventListener('mousedown', onDown); pop(); };
  }, [anchor]);

  const entries = alertEntries();
  const situations = groupSituations(entries, now);
  const today = resolvedEntries(entries, now, 'today');
  const before = resolvedEntries(entries, now, 'before');
  const aside = setAside(entries, now);
  const handing = open?.kind === 'hand' ? situations.find((s) => s.key === open.key) : undefined;
  const shown = handing ? [handing] : situations;
  const toggle = (kind: 'registro' | 'said', key: string) =>
    setOpen((cur) => (cur && cur.kind === kind && cur.key === key ? null : { kind, key }));

  const body = (
    <div className="parte" role="dialog" aria-label="Alertas" ref={ref}
      style={pos ? { left: pos.left, bottom: pos.bottom } : { visibility: 'hidden' }}>
      <div className="pa-cap"><span className="caps">Ahora</span><span className="mono">{clock(now)}</span></div>
      {shown.length === 0 && (
        <p className="mut">{entries.length || aside.length ? 'Nada disparando.' : <NoConnectors />}</p>
      )}
      {shown.map((s) => (
        <SituationBlock key={s.key} s={s} now={now} open={open?.key === s.key ? open.kind : null}
          onToggle={(kind) => toggle(kind, s.key)} onHand={() => setOpen({ kind: 'hand', key: s.key })}
          onBack={() => setOpen(null)} />
      ))}
      {!handing && (today.length > 0 || before.length > 0 || aside.length > 0) && (
        <div className="past">
          <div className="pa-cap"><span className="caps">Hoy</span></div>
          {today.length === 0 && <p className="mut">Nada se resolvió hoy.</p>}
          {today.map((e) => <PastLine key={`${e.machineKey}|${e.alert.id}`} e={e} now={now} />)}
          {(before.length > 0 || aside.length > 0) && (
            <div className="verbs">
              {before.length > 0 && (
                <button type="button" className={`tg${list === 'before' ? ' on' : ''}`} onClick={() => setList(list === 'before' ? '' : 'before')}>Días anteriores</button>
              )}
              {aside.length > 0 && (
                <button type="button" className={`tg${list === 'aside' ? ' on' : ''}`} onClick={() => setList(list === 'aside' ? '' : 'aside')}>Silenciadas</button>
              )}
            </div>
          )}
          {list === 'before' && <div className="pa-list">{before.map((e) => <PastLine key={`${e.machineKey}|${e.alert.id}`} e={e} now={now} />)}</div>}
          {list === 'aside' && <div className="pa-list">{aside.map((e) => <AsideLine key={`${e.machineKey}|${e.alert.id}`} e={e} now={now} />)}</div>}
        </div>
      )}
    </div>
  );
  return createPortal(body, document.body);
}

function NoConnectors() {
  return (
    <>Sin alertas. Los conectores se añaden en{' '}
      <button type="button" className="pa-link" onClick={() => { closeParte(); store.openSettings('conectores'); }}>Ajustes · Conectores</button>.
    </>
  );
}

function PastLine({ e, now }: { e: AlertEntry; now: Date }) {
  const { when, text } = pastSegments(e, now, chatOf);
  return <p className="mut pa-past"><span className="mono sm">{when}</span> · <Segs segs={text} /></p>;
}

function AsideLine({ e, now }: { e: AlertEntry; now: Date }) {
  const silenced = isSilenced(e.alert, now);
  const until = silenced && e.alert.silencedUntil ? clock(new Date(e.alert.silencedUntil)) : '';
  const act = () => void actOnEntries([e], (ids) => (silenced ? { ids, action: 'unsilence' } : { ids, action: 'undismiss' }))
    .catch((err) => store.addToast('No se pudo', String((err as Error)?.message || err)));
  return (
    <p className="mut pa-past">
      {situationTitle(e)} · <span className="rule">{e.alert.ruleTitle}</span>{' '}
      {silenced ? `silenciada hasta las ${until}` : 'descartada'}.{' '}
      <button type="button" className="pa-link" onClick={act}>{silenced ? 'Quitar el silencio' : 'Volver a mostrar'}</button>
    </p>
  );
}

function errorToast(title: string) {
  return (err: unknown) => store.addToast(title, String((err as Error)?.message || err));
}

function SituationBlock({ s, now, open, onToggle, onHand, onBack }: {
  s: Situation; now: Date; open: 'registro' | 'said' | 'hand' | null;
  onToggle: (kind: 'registro' | 'said') => void; onHand: () => void; onBack: () => void;
}) {
  const alerts = s.alerts.map((e) => e.alert);
  const undecided = s.alerts.filter((e) => isUndecided(e.alert, now));
  const linked = s.alerts.filter((e) => e.alert.link);
  const firstLinked = linked[0]?.alert;
  const sev = SEV_CLASS[s.severity];
  const grafana = alerts.find((a) => a.generatorURL)?.generatorURL;
  const repeats = repeatText(alerts);
  const silence = () => void actOnEntries(s.alerts, (ids) => ({ ids, action: 'silence', minutes: 10 })).catch(errorToast('No se pudo silenciar'));
  const dismiss = () => void actOnEntries(s.alerts, (ids) => ({ ids, action: 'dismiss' })).catch(errorToast('No se pudo descartar'));
  return (
    <div className="sit">
      <div className="sit-h" title={s.connector ? `Grafana · ${s.connector.name} · escucha ${s.machineName}` : undefined}>
        <b>{s.title}</b><span className={`sev ${sev}`}>{SEVERITY_WORD[s.severity]}</span>
        <span className="sp" /><span className="sm mut">{agoText(s.since, now)}</span>
      </div>
      {open !== 'hand' && (
        <p><Segs segs={[...firingSegments(alerts, now), ' ', ...ownerSegments(alerts, chatOf, now)]} /></p>
      )}
      {open === 'registro' && (
        <div className="ev">
          <div className="log">
            {registroLines(alerts).map((line, i) => (
              <div key={i} className={`ll${line.quiet ? ' quiet' : ''}`}><i>{line.at}</i><span>{line.text}</span></div>
            ))}
          </div>
          {(repeats || grafana) && (
            <p className="sm mut">{repeats}{repeats && grafana ? ' ' : ''}
              {grafana && <a className="pa-ext" href={grafana} target="_blank" rel="noreferrer">Abrir en Grafana ↗</a>}
            </p>
          )}
        </div>
      )}
      {open === 'said' && firstLinked && <SaidTail alert={firstLinked} />}
      {open !== 'hand' && (
        <div className="verbs">
          {undecided.length > 0 && <button type="button" className="main" onClick={onHand}>Dársela a un agente</button>}
          {firstLinked && <button type="button" className={undecided.length ? '' : 'main'} onClick={() => goToChat(firstLinked)}>Ir al chat</button>}
          <button type="button" onClick={silence} title="Silenciar 10 min">{undecided.length ? 'Silenciar 10 min' : 'Silenciar'}</button>
          {undecided.length > 0 && <button type="button" onClick={dismiss}>Descartar</button>}
          <span className="sp" />
          {undecided.length > 0 || !firstLinked ? (
            <button type="button" className={`tg${open === 'registro' ? ' on' : ''}`} aria-expanded={open === 'registro'} onClick={() => onToggle('registro')}>
              Registro<IcChevron open={open === 'registro'} />
            </button>
          ) : (
            <button type="button" className={`tg${open === 'said' ? ' on' : ''}`} aria-expanded={open === 'said'} onClick={() => onToggle('said')}>
              Qué dijo<IcChevron open={open === 'said'} />
            </button>
          )}
        </div>
      )}
      {open === 'hand' && <HandOff s={s} entries={undecided.length ? undecided : s.alerts} now={now} onBack={onBack} />}
    </div>
  );
}

// ---- "Qué dijo" ---------------------------------------------------------------------------

function SaidTail({ alert }: { alert: Alert }) {
  const key = linkedChatKey(alert);
  const chat = key ? store.chat(key) : undefined;
  const last = chat ? [...chat.messages].reverse().find((m) => m.role === 'assistant' && m.content?.trim()) : undefined;
  const model = chat ? store.catalogGroupsForChat(chat).flatMap((g) => g.models).find((m) => m.modelId === chat.currentModelId)?.name : '';
  const who = [chat?.providerName, model].filter(Boolean).join(' · ');
  const text = last?.content.trim() ?? '';
  const at = alert.link?.at ? new Date(alert.link.at) : null;
  return (
    <div className="ev">
      <div className="tail">
        <div className="tail-h">{who ? `${who} · ` : ''}lo último que dijo</div>
        {text ? (text.length > 320 ? `${text.slice(0, 320)}…` : text) : <span className="mut">Todavía no dijo nada{chat ? '' : ' (el chat no está abierto acá)'}.</span>}
      </div>
      <p className="sm mut">{at && !Number.isNaN(at.getTime()) ? `Lo abriste vos a las ${clock(at)}. ` : ''}Si la alerta se resuelve, el chat no recibe nada.</p>
    </div>
  );
}

// ---- the hand-off -----------------------------------------------------------------------------

function machineLabel(machine: string): string {
  if (!machine) return store.localMachineName();
  const m = store.state.machines.find((x) => x.machineId === machine);
  return m?.name || m?.reportedName || machine;
}

function machineReady(machine: string): boolean {
  if (!machine) return true;
  return store.state.machines.some((m) => m.machineId === machine && m.paired && m.link === 'ready');
}

/** "Claude Code · Opus 5.5": Ajustes · General's agent as a chat on that machine starts. */
function defaultAgentText(machine: string): string {
  const defaults = store.state.general.newChat;
  if (!defaults.providerId) return 'como el chat abierto';
  const groups = store.catalogGroupsForChat({ id: machine ? tagId(machine, 'alerts') : 'alerts', machineId: machine || undefined });
  const group = newChatAgents(groups).find((g) => g.providerId === defaults.providerId);
  const model = group ? newChatModel(defaults, group) : null;
  if (!group || !model) return `${defaults.providerId}, que no está listo en ${machineLabel(machine)}: usa el del chat abierto`;
  return `${group.providerName || group.providerId} · ${model.name}`;
}

/** Folders this client knows on a machine: its sidebar's workspaces. */
function knownFolders(machine: string): string[] {
  const app = store.state;
  const groups = buildWorkspaceGroups(app.workspaces, app.chats, app.removedWorkspaces, app.deferredChats?.cwds ?? []);
  return groups.filter((g) => (g.machineId ?? '') === machine && normalizeWorkspacePath(g.path)).map((g) => g.path).slice(0, 8);
}

function HandOff({ s, entries, now, onBack }: { s: Situation; entries: AlertEntry[]; now: Date; onBack: () => void }) {
  const alerts = entries.map((e) => e.alert);
  const handoff = useMemo(() => buildHandoff(alerts, s.connector, now),
    // Built once when the hand-off opens; the owner edits from there.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    []);
  const initial = useMemo(() => {
    const t: ConnectorTarget = resolveTarget(s.connector, alerts.map((a) => a.ruleKey));
    return { machine: clientMachine(t.machineId, store.localMachineId()), cwd: t.cwd };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);
  const [say, setSay] = useState(handoff.say);
  const [machine, setMachine] = useState(initial.machine);
  const [cwd, setCwd] = useState(initial.cwd);
  const [picking, setPicking] = useState(false);
  const [busy, setBusy] = useState(false);
  const area = useRef<HTMLTextAreaElement>(null);

  useLayoutEffect(() => {
    const el = area.current;
    if (!el) return;
    el.style.height = '0px';
    el.style.height = `${el.scrollHeight}px`;
  }, [say]);
  useEffect(() => {
    const el = area.current;
    if (!el) return;
    el.focus({ preventScroll: true });
    el.setSelectionRange(el.value.length, el.value.length);
  }, []);

  const ready = machineReady(machine);
  const why = !cwd.trim() ? 'Elegí la carpeta: tocá la máquina y la carpeta de arriba.'
    : !ready ? `${machineLabel(machine)} no está conectada.`
    : !say.trim() ? 'El mensaje está vacío.' : '';

  const go = async (send: boolean) => {
    if (busy || why) return;
    setBusy(true);
    try {
      const message = composeHandoff(say, handoff);
      if (store.state.settingsOpen) store.closeSettings();
      closeAccounts();
      closeAutomations();
      const chat = store.newChat(true, cwd.trim(), machine);
      if (send) {
        const sent = await store.sendTo(chat.id, message);
        if (!sent) { store.setDraft(chat.id, message); store.addToast('No se pudo enviar', 'El mensaje quedó escrito en el chat nuevo.'); }
      } else {
        store.setDraft(chat.id, message);
      }
      const tab = splitId(chat.id);
      const conv = splitId(chat.chatId ?? '');
      await actOnEntries(entries, (ids) => ({
        ids, action: 'link', link: { machineId: machine || store.localMachineId(), tabId: tab.id, chatId: conv.id },
      })).catch(errorToast('El chat se abrió, pero la alerta no quedó enlazada'));
      closeParte();
      store.switchChat(chat.id);
    } finally {
      setBusy(false);
    }
  };

  const token = `${machineLabel(machine)} · ${cwd ? shortFolder(cwd) : 'elegí una carpeta'}`;
  return (
    <div className="hand">
      <p>Abrir un chat nuevo en{' '}
        <button type="button" className={`tok${picking ? ' on' : ''}`} aria-expanded={picking} onClick={() => setPicking((v) => !v)}>{token}</button>
        {' '}y decirle:</p>
      {picking && (
        <TargetPicker machine={machine} cwd={cwd}
          onMachine={(m) => { setMachine(m); if (m !== machine) setCwd(''); }}
          onFolder={(path) => { setCwd(path); setPicking(false); }} />
      )}
      <div className="say">
        <textarea ref={area} value={say} rows={2} aria-label="Mensaje para el agente" spellCheck={false}
          onChange={(e) => setSay(e.target.value)} />
      </div>
      <p className="sm mut">{attachmentsText(handoff)}</p>
      <p className="sm mut">Con el agente de siempre (Ajustes · General: {defaultAgentText(machine)}). La carpeta sale de Conectores; tocala para cambiarla solo esta vez.</p>
      <div className="hand-b">
        <button type="button" className="btn q" onClick={onBack}>Volver</button>
        <span className="pair" title={why || undefined}>
          <button type="button" className="btn" disabled={busy || !!why} onClick={() => void go(false)}>Abrir sin enviar</button>
          <button type="button" className="btn pri" disabled={busy || !!why} onClick={() => void go(true)}>Abrir y enviar</button>
        </span>
      </div>
    </div>
  );
}

function TargetPicker({ machine, cwd, onMachine, onFolder }: {
  machine: string; cwd: string; onMachine: (m: string) => void; onFolder: (path: string) => void;
}) {
  const app = useApp();
  const [typed, setTyped] = useState('');
  const [browsing, setBrowsing] = useState(false);
  const self = store.localMachineId();
  const machines = ['', ...app.machines.filter((m) => m.paired && m.link === 'ready' && m.machineId !== self).map((m) => m.machineId)];
  const folders = knownFolders(machine);
  const canBrowse = !machine && has('listDir');
  return (
    <div className="ag">
      <span className="mut sm">Máquina</span>
      <span className="o">
        {machines.map((m) => (
          <button key={m || 'local'} type="button" className={`oi${m === machine ? ' on' : ''}`} onClick={() => onMachine(m)}>{machineLabel(m)}</button>
        ))}
      </span>
      <span className="mut sm">Carpeta</span>
      <span className="o">
        {folders.map((path) => (
          <button key={path} type="button" className={`oi mono${normalizeWorkspacePath(path) === normalizeWorkspacePath(cwd) ? ' on' : ''}`} title={path}
            onClick={() => onFolder(path)}>{shortFolder(path)}</button>
        ))}
        {canBrowse && <button type="button" className="oi" onClick={() => setBrowsing(true)}>Buscar…</button>}
        <input className="oi-in mono" value={typed} placeholder={folders.length ? 'u otra ruta…' : 'ruta de la carpeta…'}
          aria-label="Otra carpeta" spellCheck={false}
          onChange={(e) => setTyped(e.target.value)}
          onKeyDown={(e) => { if (e.key === 'Enter' && typed.trim()) { e.preventDefault(); onFolder(typed.trim()); } }} />
      </span>
      {browsing && <WorkspaceBrowser onSelect={(path) => { setBrowsing(false); onFolder(path); }} onClose={() => setBrowsing(false)} />}
    </div>
  );
}
