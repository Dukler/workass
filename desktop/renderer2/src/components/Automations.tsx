import { useEffect, useRef, useState, useSyncExternalStore } from 'react';
import type { Chat } from '../store/types';
import { store, useApp } from '../store/store';
import { pushEscapeLayer } from '../markdown/html-frames';
import { closeAccounts } from './AccountsPage';
import {
  automationRows, cancelAutomation, chatAutomationRows, groupByDay, isSoon, rowGlyph, timeColumn, untilText,
  useAutomations, useAutomationsVersion, whenText, type AutomationRow,
} from '../automations';

// Automatizaciones (owner 2026-11-09, mock desktop/docs/mocks/scheduled
// option E): the open chat's scheduled things as the Programado section of
// Trabajos, and every chat's on a page over the chat area, opened from the
// clock at the foot of the left panel (like Cuentas beside it).

// ---- open / closed -------------------------------------------------------

let pageOpen = false;
const openListeners = new Set<() => void>();
function setOpen(next: boolean) {
  if (pageOpen === next) return;
  pageOpen = next;
  for (const listener of openListeners) listener();
}
export function openAutomations() { setOpen(true); }
export function closeAutomations() { setOpen(false); }
function subscribeOpen(listener: () => void) {
  openListeners.add(listener);
  return () => { openListeners.delete(listener); };
}
const openSnapshot = () => pageOpen;
export function useAutomationsOpen(): boolean {
  return useSyncExternalStore(subscribeOpen, openSnapshot, openSnapshot);
}

// ---- glyphs --------------------------------------------------------------

function IcClock() {
  return (
    <svg viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <circle cx="8" cy="8" r="5.9" /><path d="M8 4.8V8l2.2 1.4" />
    </svg>
  );
}
function IcRepeat() {
  return (
    <svg viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <path d="M3 7.2a3.9 3.9 0 0 1 3.9-3.9h5" /><path d="M10.2 1.6l1.8 1.7-1.8 1.7" />
      <path d="M13 8.8a3.9 3.9 0 0 1-3.9 3.9h-5" /><path d="M5.8 14.4L4 12.7l1.8-1.7" />
    </svg>
  );
}
function IcPlug() {
  return (
    <svg viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <path d="M6 2v3M10 2v3" /><path d="M4.6 5h6.8v2.4a3.4 3.4 0 0 1-6.8 0z" /><path d="M8 10.8V14" />
    </svg>
  );
}
function IcFile() {
  return (
    <svg viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <path d="M4.2 2h4.9l2.7 2.7V14H4.2z" /><path d="M9 2v2.8h2.8" />
    </svg>
  );
}
function IcProcess() {
  return (
    <svg viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <rect x="2.4" y="3" width="11.2" height="10" rx="1.6" /><path d="M5 6.6l2 1.6-2 1.6M8.6 10h2.4" />
    </svg>
  );
}
function IcClose() {
  return <svg viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.4" strokeLinecap="round" aria-hidden="true"><path d="M4 4l8 8M12 4l-8 8" /></svg>;
}
function RowGlyph({ row }: { row: AutomationRow }) {
  switch (rowGlyph(row)) {
    case 'repeat': return <IcRepeat />;
    case 'plug': return <IcPlug />;
    case 'file': return <IcFile />;
    case 'process': return <IcProcess />;
    default: return <IcClock />;
  }
}

// ---- shared bits -----------------------------------------------------------

/** Re-renders each 30 s so "en 42 min" stays true. */
function useMinute(): Date {
  const [now, setNow] = useState(() => new Date());
  useEffect(() => {
    const timer = window.setInterval(() => setNow(new Date()), 30_000);
    return () => window.clearInterval(timer);
  }, []);
  return now;
}

function rowKey(row: AutomationRow): string {
  return `${row.machineId}|${row.item.kind}|${row.item.tabId}|${row.item.chatId}|${row.item.id}|${row.item.schedule ?? ''}`;
}

function rowText(row: AutomationRow): string {
  return row.item.text || (row.item.kind === 'session-cron' ? 'Tarea del agente' : 'Mensaje programado');
}

const AGENT_ONLY = 'Vive en la sesión del agente: pedile que la quite.';

function CancelButton({ row }: { row: AutomationRow }) {
  const [busy, setBusy] = useState(false);
  if (row.item.kind !== 'message') return null;
  const cancel = async (event: React.MouseEvent) => {
    event.stopPropagation();
    if (busy) return;
    setBusy(true);
    try {
      await cancelAutomation(row);
    } catch (error) {
      store.addToast('No se pudo cancelar', String((error as Error)?.message || error));
      setBusy(false);
    }
  };
  return (
    <button type="button" className="auto-x" title="Cancelar" aria-label="Cancelar" disabled={busy} onClick={(e) => void cancel(e)}>
      <IcClose />
    </button>
  );
}

// ---- Trabajos · Programado ---------------------------------------------------

/** Trabajos shows the soonest few; the page has the rest. */
const SECTION_ROWS = 5;

/** The open chat's scheduled things, under what runs in Trabajos. */
export function ProgramadoSection({ chat }: { chat: Chat }) {
  useAutomationsVersion();
  const now = useMinute();
  const rows = chatAutomationRows(chat);
  if (rows.length === 0) return null;
  return (
    <>
      <div className="wsec">Programado<span className="wsec-h">{rows.length}</span></div>
      <div className="auto-list">
        {rows.slice(0, SECTION_ROWS).map((row) => {
          const until = row.due ? untilText(row.due, now) : '';
          return (
            <div key={rowKey(row)} className={`auto-r${isSoon(row, now) ? ' soon' : ''}`}
              title={row.item.kind === 'session-cron' ? `${rowText(row)}\n${AGENT_ONLY}` : rowText(row)}>
              <span className="auto-g" aria-hidden="true"><RowGlyph row={row} /></span>
              <span className="auto-t">{rowText(row)}</span>
              <CancelButton row={row} />
              <span className="auto-w">
                <span className="auto-when">{whenText(row, now)}</span>
                {until && <> · {until}</>}
                {row.item.kind === 'session-cron' && <> · tarea del agente</>}
              </span>
            </div>
          );
        })}
      </div>
      <button type="button" className="auto-all" onClick={() => openAutomations()}>
        {rows.length > SECTION_ROWS ? `Ver los ${rows.length} ›` : 'Ver todo lo programado ›'}
      </button>
    </>
  );
}

/** Rows of the open chat, for the Trabajos tab's count. */
export function programadoCount(chat: Chat | null): number {
  return chat ? chatAutomationRows(chat).length : 0;
}

// ---- foot button -------------------------------------------------------------

export function AutomationsFootButton() {
  const app = useApp();
  useAutomations(); // keeps the count current while the app is open
  const open = useAutomationsOpen();
  const count = automationRows().length;
  const title = count > 0 ? `Automatizaciones · ${count} programad${count === 1 ? 'o' : 'os'}` : 'Automatizaciones';
  const shown = open && !app.settingsOpen;
  const click = () => {
    if (app.settingsOpen) { store.closeSettings(); openAutomations(); return; }
    closeAccounts();
    setOpen(!pageOpen);
  };
  return (
    <button type="button" className={`footbtn${shown ? ' on' : ''}`} title={title} aria-label={title}
      aria-pressed={shown} onClick={click}>
      <IcClock />
    </button>
  );
}

// ---- page --------------------------------------------------------------------

type Filter = 'all' | 'message' | 'session-cron' | `machine:${string}`;

export function AutomationsPage() {
  const app = useApp();
  useAutomations();
  const now = useMinute();
  const pageRef = useRef<HTMLElement>(null);
  const [filter, setFilter] = useState<Filter>('all');
  useEffect(() => pushEscapeLayer(closeAutomations), []);
  useEffect(() => { pageRef.current?.focus({ preventScroll: true }); }, []);

  const all = automationRows();
  const machines = [...new Map(all.map((row) => [row.machineId, row.machineName])).entries()];
  const rows = all.filter((row) => filter === 'all'
    || (filter.startsWith('machine:') ? row.machineId === filter.slice('machine:'.length) : row.item.kind === filter));
  const chats = new Set(all.map((row) => `${row.chatKey}|${row.item.chatId}`)).size;
  const chatTitle = (row: AutomationRow) => {
    const found = app.chats.find((chat) => chat.id === row.chatKey && chat.chatId === row.item.chatId);
    return found?.title || 'Chat cerrado';
  };
  const open = (row: AutomationRow) => {
    if (!app.chats.some((chat) => chat.id === row.chatKey)) return;
    closeAutomations();
    store.switchChat(row.chatKey);
  };
  const chip = (key: Filter, label: string) => (
    <button type="button" key={key} className={`auto-chip${filter === key ? ' on' : ''}`} aria-pressed={filter === key} onClick={() => setFilter(key)}>{label}</button>
  );

  return (
    <section className="acct-page auto-page" aria-label="Automatizaciones" ref={pageRef} tabIndex={-1}>
      <div className="acct-head">
        <span>Automatizaciones</span>
        <button type="button" className="acct-x" title="Cerrar · Esc" aria-label="Cerrar" onClick={closeAutomations}><IcClose /></button>
      </div>
      <div className="auto-body">
        {all.length === 0 ? (
          <div className="acct-none"><b>Nada programado</b>Cuando un agente programa un mensaje o una tarea, aparece acá.</div>
        ) : (
          <>
            <div className="auto-sum">{all.length} pendiente{all.length === 1 ? '' : 's'} en {chats} chat{chats === 1 ? '' : 's'}</div>
            <div className="auto-chips">
              {chip('all', 'Todo')}
              {chip('message', 'Mensajes')}
              {chip('session-cron', 'Tareas de agentes')}
              {machines.length > 1 && machines.map(([id, name]) => chip(`machine:${id}`, name))}
            </div>
            {groupByDay(rows, now).map((group) => (
              <div key={group.label} className="auto-day">
                <div className="auto-dh">{group.label}</div>
                {group.rows.map((row) => {
                  const until = row.due ? untilText(row.due, now) : '';
                  const linked = app.chats.some((chat) => chat.id === row.chatKey);
                  return (
                    <div key={rowKey(row)} className={`auto-gr${isSoon(row, now) ? ' soon' : ''}${linked ? ' link' : ''}`}
                      role={linked ? 'button' : undefined} tabIndex={linked ? 0 : undefined}
                      onClick={() => open(row)} onKeyDown={(e) => { if (e.key === 'Enter') open(row); }}
                      title={row.item.kind === 'session-cron' ? AGENT_ONLY : undefined}>
                      <span className="auto-tm">{timeColumn(row)}</span>
                      <span className="auto-g" aria-hidden="true"><RowGlyph row={row} /></span>
                      <span className="auto-txt">
                        <span className="auto-t">{rowText(row)}</span>
                        <span className="auto-w">
                          {whenText(row, now)}{until && <> · {until}</>}
                          {row.item.kind === 'session-cron' && <> · tarea del agente</>}
                        </span>
                      </span>
                      <span className="auto-src">{chatTitle(row)}{row.machineId && <> · {row.machineName}</>}</span>
                      <span className="auto-act"><CancelButton row={row} /></span>
                    </div>
                  );
                })}
              </div>
            ))}
            {rows.length === 0 && <div className="rp-empty">Nada con este filtro.</div>}
          </>
        )}
      </div>
    </section>
  );
}
