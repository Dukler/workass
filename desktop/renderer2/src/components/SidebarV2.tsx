import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import type { Chat, Msg } from '../store/types';
import { store, useApp, useSpawnedWork } from '../store/store';
import { IcSearch, IcPlus, IcFolder, IcChevron, IcClose, ModelIcon } from '../icons';
import { buildWorkspaceGroups, lastProject, newChatTarget, normalizeWorkspacePath, workspaceLabelForChat, type WorkspaceGroup } from '../workspaces';
import { chatHasLiveActivity, isServiceWork } from '../chat-activity';
import { machineWhere, remoteMachineBadge } from '../machine-label';
import { WorkspaceBrowser } from './WorkspaceBrowser';
import { ProjectIcon } from './ProjectIcon';
import { ModesSwitch, FooterUpdateCards, AccountMenu } from './Sidebar';

// Sidebar v2 — port of T3 Code's "Sidebar v2" beta (nightly 0.0.29.20260724,
// commit 41a430a8), read from the source maps it ships rather than copied by
// eye. Metrics below are the literal Tailwind pixels from that source; see
// app.css `.sv2-*` for the matching geometry.
//
// Structure taken verbatim: the folder TREE becomes a project SCOPE FILTER;
// rows have two densities (full card / 36px slim), but density follows an
// explicit lifecycle boundary: ordinary threads stay full-size and only the
// settled shelf/archive compacts them. One STATUS PILL per row has a fixed hue
// per state.
//
// T3 drives density and lifecycle from stored settle/snooze state. Workass
// keeps a smaller lifecycle: a quiet shelf followed by a hidden, searchable
// archive. T3's PR/branch/diff line remains absent.
//
// Deliberately NOT ported: T3's footer settings button (excluded by the user)
// and its nightly-stage header backdrop (branding).

// T3's sidebarAutoSettleAfterDays, whose default is 3. Age files a quiet chat
// away on its own; the row action and the menu do it on demand.
const AUTO_SETTLE_MS = 3 * 24 * 60 * 60 * 1000;
export const ARCHIVE_AFTER_SETTLED_MS = 5 * 24 * 60 * 60 * 1000;
const LIFECYCLE_TICK_MS = 60 * 60 * 1000;
const TAIL_PAGE = 24;            // T3 pages its settled tail; deep history is rare
const TOOLTIP_DELAY_MS = 150;    // T3's TooltipProvider delay
const SCOPE_KEY = 'workass.sv2.scope';

/* ---- icons: the lucide glyphs T3 uses, at its sizes ---------------------- */
function IcFolderPlus() {
  return (
    <svg viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.4" aria-hidden="true">
      <path d="M2 4.5h4L7.5 6H14v6.5H2z" strokeLinejoin="round" />
      <path d="M8 8v3M6.5 9.5h3" strokeLinecap="round" />
    </svg>
  );
}
function IcCircleDashed() {
  return (
    <svg viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.5" aria-hidden="true">
      <circle cx="8" cy="8" r="5.4" strokeDasharray="2.6 2.4" strokeLinecap="round" />
    </svg>
  );
}
function IcCircleCheck() {
  return (
    <svg viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.5" aria-hidden="true">
      <circle cx="8" cy="8" r="5.4" />
      <path d="M5.6 8.2l1.7 1.7 3.1-3.4" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  );
}
function IcAlert() {
  return (
    <svg viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.5" aria-hidden="true">
      <circle cx="8" cy="8" r="5.4" />
      <path d="M8 5.2v3.4M8 10.8v.2" strokeLinecap="round" />
    </svg>
  );
}
// T3's row affordances: Check settles a thread, Undo2 pulls it back out.
function IcCheck() {
  return (
    <svg viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.6" aria-hidden="true">
      <path d="M3.2 8.6l3 3 6.6-7.2" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  );
}
function IcUndo() {
  return (
    <svg viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.5" aria-hidden="true">
      <path d="M3 7.5h6.6a3 3 0 010 6H6.2" strokeLinecap="round" strokeLinejoin="round" />
      <path d="M5.4 4.6L2.6 7.5l2.8 2.9" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  );
}

function projectRemoteBadge(machineId?: string | null) {
  return remoteMachineBadge(machineId, store.machineNames(), store.localMachineId());
}

function WorkspaceProjectIcon({ group, markRemote = false }: { group: WorkspaceGroup; markRemote?: boolean }) {
  return (
    <ProjectIcon
      chatId={group.chats[0]?.id ?? ''}
      cwd={group.path}
      remote={markRemote ? projectRemoteBadge(group.machineId) : null}
    />
  );
}

/* ---- status ------------------------------------------------------------- */
// Port of T3's resolveSidebarV2Status, bound to the Workass facts that mean the
// same thing. Order is T3's: something needing a human outranks something
// merely running.
type Status = 'approval' | 'attention' | 'working' | 'parked' | 'failed' | 'done' | 'ready';

function lastOf(list: readonly Msg[], match: (m: Msg) => boolean): Msg | undefined {
  for (let i = list.length - 1; i >= 0; i--) if (match(list[i])) return list[i];
  return undefined;
}

export function resolveStatus(chat: Chat, live: boolean, active: boolean, obligation?: { state: string }): Status {
  // T3: hasPendingApprovals.
  if (chat.messages.some((m) => m.permission && !m.permission.resolved)) return 'approval';
  // T3: session running|starting.
  if (live) return 'working';
  // What the chat still OWES you, which is a different question from what is
  // running. A turn ending is not the same as a request being finished: an
  // async agent parks by ending its turn, and before this the row said "Listo"
  // — the finished cue fired most confidently exactly when it was most wrong.
  // Absent against an older daemon, in which case everything below is
  // unchanged.
  // Filing is the user's acknowledgement of terminal attention. It does not
  // erase the daemon's obligation receipt; it only quiets that pill until a
  // later job:start clears the settle override. Unread/failed cues below remain
  // independent, so "Marcar como no leída" still does exactly what it says.
  if ((obligation?.state === 'needs_input' || obligation?.state === 'stalled') && chat.settled !== 'settled') return 'attention';
  if (obligation?.state === 'parked') return 'parked';
  // T3 reads `session.status === "error"` — a property of the LIVE session, not
  // of transcript history, so an error stops being one as soon as the thread
  // moves on. Ours must expire the same way or a chat that broke last week
  // shouts in red forever. Two exclusions: an `interrupted` turn is a daemon
  // restart rather than an agent error, and a failure is only news while it is
  // unseen — the chat you are READING already shows the error in its transcript,
  // so a red pill on that row reports nothing the screen does not.
  const lastAssistant = lastOf(chat.messages, (m) => m.role === 'assistant');
  if (!active && chat.unread && lastAssistant?.status === 'failed' && !lastAssistant.interrupted) {
    return 'failed';
  }
  // T3: "Done" is the unseen completion, not merely a finished turn.
  if (chat.unread) return 'done';
  return 'ready';
}

// T3's labels are English; Workass's UI is Spanish, so labels are localized
// while the hue / glyph / animation contract is kept exactly.
const STATUS_PILL: Record<Exclude<Status, 'ready'>, { label: string; icon: 'working' | 'done' | 'alert' | null; tone: string }> = {
  approval: { label: 'Permiso', icon: 'alert', tone: 'approval' },
  // Reuses the approval hue rather than inventing one: both mean the same
  // thing to the reader — this row is waiting on you, not on a machine.
  attention: { label: 'Atención', icon: 'alert', tone: 'approval' },
  // Deliberately toneless. A parked chat is not a problem and not an
  // achievement; it inherits the row's own muted colour and says only that
  // something will come back on its own.
  parked: { label: 'En pausa', icon: null, tone: '' },
  working: { label: 'Trabajando', icon: 'working', tone: 'working' },
  failed: { label: 'Falló', icon: null, tone: 'failed' },
  done: { label: 'Listo', icon: 'done', tone: 'done' },
};

// Ordering is the persisted chat-list order. Status and recency are visible
// metadata, not hidden sort keys: otherwise a successful drag is immediately
// undone by the next render and rows jump while the user is targeting them.

export function lastTouchedAt(chat: Chat): number {
  const projected = Number(chat.lastActivityAt);
  const lifecycleAt = Number.isFinite(projected) && projected > 0 ? projected : 0;
  for (let i = chat.messages.length - 1; i >= 0; i--) {
    const at = chat.messages[i]?.at;
    if (at) { const t = Date.parse(at); if (!Number.isNaN(t)) return Math.max(lifecycleAt, t); }
  }
  return lifecycleAt;
}

// A chat can be working because a TURN is running or because background work
// outlived it, so the elapsed clock follows whichever is actually alive. A
// running service is excluded for the same reason it does not make the chat
// working: a server started this morning would otherwise date the clock to it.
function workingSince(chat: Chat, work: readonly { status: string; startedAt?: string; role?: string }[]): number {
  const running = chat.messages.find((m) => m.status === 'running');
  const turnAt = running?.at ? Date.parse(running.at) : NaN;
  if (!Number.isNaN(turnAt) && turnAt > 0) return turnAt;
  let earliest = 0;
  for (const item of work) {
    if (item.status !== 'running' || !item.startedAt || isServiceWork(item)) continue;
    const at = Date.parse(item.startedAt);
    if (Number.isNaN(at)) continue;
    if (earliest === 0 || at < earliest) earliest = at;
  }
  return earliest;
}

function shortAgo(ms: number): string {
  if (!ms) return '';
  const s = Math.max(0, Date.now() - ms) / 1000;
  if (s < 60) return 'ahora';
  if (s < 3600) return `${Math.floor(s / 60)}m`;
  if (s < 86400) return `${Math.floor(s / 3600)}h`;
  if (s < 86400 * 30) return `${Math.floor(s / 86400)}d`;
  return `${Math.floor(s / (86400 * 30))}me`;
}

// T3's WorkingDuration: seconds under a minute, then minutes. Repaints only its
// own span so a ticking row never re-renders the list.
function WorkingDuration({ since }: { since: number }) {
  const [, force] = useState(0);
  useEffect(() => {
    const id = window.setInterval(() => force((n) => n + 1), 1000);
    return () => window.clearInterval(id);
  }, []);
  if (!since) return null;
  const s = Math.max(0, Math.floor((Date.now() - since) / 1000));
  return <span className="sv2-dur">{s < 60 ? `${s}s` : `${Math.floor(s / 60)}m`}</span>;
}

type Row = {
  chat: Chat; project: string; card: boolean; settled: boolean; archived: boolean; touched: number;
  status: Status; since: number; bg: number; error: string; order: number;
};

// Port of T3's effectiveSettled. Live work, pending approval, parked work, and
// unseen news cannot be filed. Merely selecting a thread is not activity and
// must not pull it out of the settled shelf; only real new work clears settled
// state in the store/actor. Terminal attention is different: it is news the
// user can explicitly acknowledge, but it never auto-files by age.
export function resolveSettled(chat: Chat, status: Status, _active: boolean, now: number, touched: number): boolean {
  if (chat.unread || status === 'approval' || status === 'working' || status === 'parked') return false;
  if (chat.settled === 'settled') return true;
  if (chat.settled === 'active') return false;
  if (status !== 'ready') return false;
  // T3 returns false outright when a thread has no activity to date. A chat
  // with no messages has a zero timestamp, and treating that as "last touched
  // in 1970" would drop every freshly created chat straight onto the shelf.
  if (!touched) return false;
  return now - touched >= AUTO_SETTLE_MS;
}

function settledSince(chat: Chat, status: Status, now: number, touched: number): number {
  if (!resolveSettled(chat, status, false, now, touched)) return 0;
  if (chat.settled === 'settled') {
    const explicit = Number(chat.settledAt);
    if (Number.isFinite(explicit) && explicit > 0) return explicit;
    // Settled rows without this timestamp use their last real activity as the
    // honest lower bound. A row with neither timestamp nor activity necessarily
    // predates the shelf timestamp contract, so it belongs in the archive too.
    return touched || 1;
  }
  return touched ? touched + AUTO_SETTLE_MS : 0;
}

export function resolveArchived(chat: Chat, status: Status, now: number, touched: number): boolean {
  const since = settledSince(chat, status, now, touched);
  return since > 0 && now - since >= ARCHIVE_AFTER_SETTLED_MS;
}

export function isFullSizeSidebarRow(settled: boolean, archived: boolean): boolean {
  return !settled && !archived;
}

export function orderSidebarRows<T extends { order: number }>(rows: readonly T[]): T[] {
  return [...rows].sort((a, b) => a.order - b.order);
}

export function orderSearchRows(rows: readonly Row[]): Row[] {
  return [...rows].sort((a, b) =>
    Number(a.archived) - Number(b.archived)
    || Number(b.card) - Number(a.card)
    || a.order - b.order);
}

// T3's canSettle, the client-side twin of the guards above: "anything the
// partition refuses to CLASSIFY as settled must also be refused as a settle
// TARGET". Theirs asks the server and toasts on rejection; we have nobody to
// ask, so the affordance is withheld instead — and it has to be, because a chat
// with a live turn wipes the override on its very next job event.
export function canSettle(status: Status): boolean {
  // Approval, live work, and work that will resume by itself are not history.
  // Attention is terminal user-facing news, so filing it is the acknowledgement.
  return status !== 'approval' && status !== 'working' && status !== 'parked';
}

type Tip = { row: Row; top: number } | null;
type SidebarSection = 'live' | 'settled' | 'archived';
type DragItem = { id: string; section: SidebarSection };

function rowSection(row: Pick<Row, 'settled' | 'archived'>): SidebarSection {
  return row.archived ? 'archived' : row.settled ? 'settled' : 'live';
}

/* ---- row ---------------------------------------------------------------- */
function SidebarV2Row({ row, active, drag, setDrag, onDropBefore, onTip, onMenu, onSettle, onUnsettle }: {
  row: Row; active: boolean;
  drag: DragItem | null; setDrag: (item: DragItem | null) => void;
  onDropBefore: (targetChatId: string) => void;
  onTip: (tip: Tip) => void;
  onMenu: (row: Row, x: number, y: number) => void;
  onSettle: (row: Row) => void;
  onUnsettle: (row: Row) => void;
}) {
  const { chat, card, status } = row;
  const [editing, setEditing] = useState(false);
  const [value, setValue] = useState(chat.title);
  const [over, setOver] = useState(false);
  const hoverTimer = useRef<number | null>(null);
  const section = rowSection(row);
  const canDrop = !!drag && drag.id !== chat.id && drag.section === section;
  const commit = () => { setEditing(false); store.renameChat(chat.id, value); };
  const pill = row.archived
    ? { label: 'Archivado', icon: null, tone: 'archived' }
    : status === 'ready' ? null : STATUS_PILL[status];

  useEffect(() => () => { if (hoverTimer.current) window.clearTimeout(hoverTimer.current); }, []);

  // T3: in-flight rows fade as a whole — "there is nothing for the user to do
  // yet", so prominence is reserved for rows that need a human. The pill keeps
  // its hue so waiting rows stay findable.
  const inFlight = status === 'working' || status === 'approval';
  const recede = (status === 'ready' || status === 'working') && !chat.unread && !active;

  const surface = [
    'sv2-row', card ? 'card' : 'slim',
    active ? 'on' : '', over && canDrop ? 'dropbefore' : '', drag?.id === chat.id ? 'dragging' : '',
    chat.unread && !active ? 'unread' : '', row.archived ? 'archived' : '', recede ? 'recede' : '', inFlight && !active ? 'inflight' : '',
  ].filter(Boolean).join(' ');

  const openTip = (e: React.MouseEvent) => {
    if (editing) return;
    const rect = (e.currentTarget as HTMLElement).getBoundingClientRect();
    if (hoverTimer.current) window.clearTimeout(hoverTimer.current);
    hoverTimer.current = window.setTimeout(() => onTip({ row, top: rect.top }), TOOLTIP_DELAY_MS);
  };
  const closeTip = () => {
    if (hoverTimer.current) { window.clearTimeout(hoverTimer.current); hoverTimer.current = null; }
    onTip(null);
  };

  const title = editing ? (
    <input
      className="sv2-title tedit" value={value} autoFocus spellCheck={false}
      onChange={(e) => setValue(e.target.value)}
      onBlur={commit}
      onClick={(e) => e.stopPropagation()}
      onKeyDown={(e) => {
        if (e.key === 'Enter') { e.preventDefault(); commit(); }
        else if (e.key === 'Escape') { e.preventDefault(); setEditing(false); }
      }}
    />
  ) : (
    <span className="sv2-title">{chat.title}</span>
  );

  // Right slot: the pill (or the stamp) yields to the row's actions on hover,
  // rather than the row growing a column for them.
  const slot = (
    <span className="sv2-slot">
      <span className="sv2-when">
        {pill ? (
          <span className={`sv2-pill ${pill.tone}`}>
            {pill.icon === 'working' && <IcCircleDashed />}
            {pill.icon === 'done' && <IcCircleCheck />}
            {pill.icon === 'alert' && <IcAlert />}
            <span role="status">{pill.label}</span>
            {status === 'working' && <WorkingDuration since={row.since} />}
          </span>
        ) : shortAgo(row.touched)}
      </span>
      {!editing && (
        <span className="sv2-acts">
          {/* T3's row action is settle, never delete: Check on a live row, Undo
              on a shelved one. Deleting a conversation is irreversible, so it
              lives in the context menu behind a confirmation instead of one
              stray click away from the row you meant to file. */}
          {row.settled || row.archived ? (
            <button
              className="sv2-act" title="Reactivar" aria-label="Reactivar conversación"
              onClick={(e) => { e.stopPropagation(); closeTip(); onUnsettle(row); }}
            ><IcUndo /></button>
          ) : canSettle(status) ? (
            <button
              className="sv2-act" title="Poner en reposo" aria-label="Poner la conversación en reposo"
              onClick={(e) => { e.stopPropagation(); closeTip(); onSettle(row); }}
            ><IcCheck /></button>
          ) : null}
        </span>
      )}
    </span>
  );

  return (
    <li className={card ? 'sv2-li card' : 'sv2-li'}>
      <div
        className={surface} role="button" tabIndex={0}
        data-sv2-row={chat.id}
        draggable={!editing}
        onMouseEnter={openTip}
        onMouseLeave={closeTip}
        onClick={() => { if (!editing) { closeTip(); store.switchChat(chat.id); } }}
        onDoubleClick={() => { closeTip(); setValue(chat.title); setEditing(true); }}
        onContextMenu={(e) => { e.preventDefault(); closeTip(); onMenu(row, e.clientX, e.clientY); }}
        onDragStart={(e) => { e.dataTransfer.effectAllowed = 'move'; e.dataTransfer.setData('text/plain', chat.id); setDrag({ id: chat.id, section }); closeTip(); }}
        onDragEnd={() => { setDrag(null); setOver(false); }}
        onDragOver={(e) => { if (canDrop) { e.preventDefault(); setOver(true); } }}
        onDragLeave={() => setOver(false)}
        onDrop={(e) => { if (canDrop) { e.preventDefault(); onDropBefore(chat.id); } setOver(false); }}
      >
        {card ? (
          <>
            <div className="sv2-l1">
              <span className="sv2-fav"><ProjectIcon chatId={chat.id} cwd={chat.cwd ?? ''} /></span>
              <Where row={row} />
              {slot}
            </div>
            {/* The provider glyph rides the TITLE line, pinned right. On line 1
                it sat between the project name and a variable-width status, so
                it slid left every time the duration ticked (57s → 5m) or the
                status changed — an icon that dances on a timer. Nothing on this
                line changes width, so here it is anchored. */}
            <div className="sv2-l2">
              {title}
              <span className="sv2-prov" aria-hidden="true">
                <ModelIcon provider={chat.providerId === 'codex' ? 'gpt' : chat.providerId} />
              </span>
            </div>
            {/* T3's third line is branch / PR / diff, which it almost always
                has; we almost never do. Rendering it unconditionally inherited
                its 78px geometry without its content, so most cards carried a
                strip of dead space. The line now appears only when there IS
                something live to say, and the card lands on T3's exact 78px
                when it does — 60px when it doesn't. */}
            {row.bg > 0 && (
              <div className="sv2-l3">
                <span className="sv2-meta">{row.bg} en segundo plano</span>
              </div>
            )}
          </>
        ) : (
          <>
            <span className="sv2-fav"><ProjectIcon chatId={chat.id} cwd={chat.cwd ?? ''} /></span>
            {title}
            {slot}
          </>
        )}
      </div>
    </li>
  );
}

/* ---- hover details (T3's SidebarV2ThreadTooltip) ------------------------- */
// Where a conversation lives: `builder/workass` (remote-plan E3, approved
// 2026-07-26). The machine is a prefix on the slot the project already had, not
// a new element — which is why this builds a string instead of a new row.
function Where({ row }: { row: Row }) {
  const where = machineWhere(row.project, row.chat.machineId, store.machineNames(), store.localMachineId());
  if (!where.machine) return <span className="sv2-proj">{where.project}</span>;
  return (
    <span className="sv2-where" title={where.full}>
      <span className="sv2-mach">{where.machine}</span>
      <span className="sv2-mach-sep">/</span>
      <span className="sv2-proj">{where.project}</span>
    </span>
  );
}

function RowTooltip({ tip }: { tip: NonNullable<Tip> }) {
  const { row } = tip;
  const top = Math.min(tip.top, Math.max(8, window.innerHeight - 150));
  return (
    <div className="sv2-tip" style={{ top }} role="tooltip">
      <div className="sv2-tip-title">{row.chat.title}</div>
      <div className="sv2-tip-rows">
        <div className="sv2-tip-row"><ProjectIcon chatId={row.chat.id} cwd={row.chat.cwd ?? ''} /><span>{row.project}</span></div>
        {row.chat.cwd && <div className="sv2-tip-row sv2-tip-path"><span>{row.chat.cwd}</span></div>}
        {row.chat.providerId && (
          <div className="sv2-tip-row">
            <ModelIcon provider={row.chat.providerId === 'codex' ? 'gpt' : row.chat.providerId} />
            <span>{row.chat.providerId === 'codex' ? 'Codex' : 'Claude Code'}</span>
          </div>
        )}
        {row.error && <div className="sv2-tip-row err"><IcAlert /><span>{row.error}</span></div>}
      </div>
    </div>
  );
}

/* ---- menus -------------------------------------------------------------- */
function useDismiss(onClose: () => void) {
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const onDown = (e: MouseEvent) => { if (ref.current && !ref.current.contains(e.target as Node)) onClose(); };
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') { e.preventDefault(); onClose(); } };
    document.addEventListener('mousedown', onDown);
    document.addEventListener('keydown', onKey, true);
    return () => { document.removeEventListener('mousedown', onDown); document.removeEventListener('keydown', onKey, true); };
  }, [onClose]);
  return ref;
}

function ScopeProjectItem({ group, scope, onPick, onClose }: {
  group: WorkspaceGroup;
  scope: string | null;
  onPick: (path: string | null) => void;
  onClose: () => void;
}) {
  const remote = projectRemoteBadge(group.machineId);
  const target = remote ? `${remote.machine}/${group.name}` : group.name;
  return (
    <button
      className={`sv2-mi ${scope === normalizeWorkspacePath(group.path) ? 'on' : ''}`}
      role="menuitem"
      title={remote?.title}
      onClick={() => { onPick(group.path ? normalizeWorkspacePath(group.path) : ''); onClose(); }}
    >
      <WorkspaceProjectIcon group={group} markRemote /><span className="sv2-mtxt">{group.name}</span>
      {group.path && (
        <>
          <button
            className="sv2-mact create" title={`Nueva conversación en ${target}`}
            aria-label={`Crear conversación en ${target}`}
            onClick={(e) => { e.stopPropagation(); store.newChat(true, group.path, group.machineId ?? ''); onClose(); }}
          ><IcPlus /></button>
          <button
            className="sv2-mact danger" title="Quitar de Workass (no borra del disco)"
            aria-label={`Quitar ${group.name} de Workass`}
            onClick={(e) => { e.stopPropagation(); store.removeWorkspace(group.path); onClose(); }}
          ><IcClose /></button>
        </>
      )}
    </button>
  );
}

function ScopeMenu({ groups, scope, onPick, onClose }: {
  groups: WorkspaceGroup[]; scope: string | null;
  onPick: (path: string | null) => void; onClose: () => void;
}) {
  const ref = useDismiss(onClose);
  return (
    <div className="sv2-menu" ref={ref} role="menu">
      <button className={`sv2-mi ${scope === null ? 'on' : ''}`} role="menuitem" onClick={() => { onPick(null); onClose(); }}>
        <IcFolder /><span className="sv2-mtxt">Todos los proyectos</span>
      </button>
      {groups.map((group) => (
        <ScopeProjectItem
          key={`${group.machineId ?? ''}:${group.path || 'unassigned'}`}
          group={group} scope={scope} onPick={onPick} onClose={onClose}
        />
      ))}
    </div>
  );
}

function RowMenu({ row, x, y, onClose, onRename, onSettle, onUnsettle }: {
  row: Row; x: number; y: number; onClose: () => void; onRename: (id: string) => void;
  onSettle: (row: Row) => void; onUnsettle: (row: Row) => void;
}) {
  const ref = useDismiss(onClose);
  // T3's menu order: lifecycle first, then rename/mark-unread, then delete last
  // as a destructive item. Deletion clears a conversation for good, so it asks
  // once in place — this renderer has no dialog surface, and a native confirm()
  // would freeze the window mid-turn.
  const [confirming, setConfirming] = useState(false);
  return (
    <div className="sv2-ctx" ref={ref} style={{ left: x, top: y }} role="menu">
      {row.settled || row.archived ? (
        <button className="sv2-ci" role="menuitem" onClick={() => { onUnsettle(row); onClose(); }}>Reactivar</button>
      ) : (
        // A menu item that vanishes reads as a missing feature, so a chat that
        // cannot be filed yet keeps its item and says why.
        <button
          className="sv2-ci" role="menuitem" disabled={!canSettle(row.status)}
          title={canSettle(row.status) ? undefined : 'Primero termina o responde lo que tiene en curso'}
          onClick={() => { onSettle(row); onClose(); }}
        >Poner en reposo</button>
      )}
      <button className="sv2-ci" role="menuitem" onClick={() => { onRename(row.chat.id); onClose(); }}>Renombrar</button>
      <button className="sv2-ci" role="menuitem" onClick={() => { store.markUnread(row.chat.id); onClose(); }}>Marcar como no leída</button>
      <button className="sv2-ci" role="menuitem" onClick={() => { store.newChat(true, row.chat.cwd ?? undefined, row.chat.machineId ?? ''); onClose(); }}>Nueva aquí</button>
      <button
        className="sv2-ci danger" role="menuitem"
        onClick={() => { if (confirming) { store.closeChat(row.chat.id); onClose(); } else setConfirming(true); }}
      >{confirming ? 'Confirmar: borra el historial' : 'Eliminar'}</button>
    </div>
  );
}

/* ---- sidebar ------------------------------------------------------------ */
export function SidebarV2() {
  const app = useApp();
  useSpawnedWork();
  const groups = buildWorkspaceGroups(app.workspaces, app.chats, app.removedWorkspaces);
  const [browsing, setBrowsing] = useState(false);
  const [scope, setScope] = useState<string | null>(() => {
    try { const v = localStorage.getItem(SCOPE_KEY); return v === null || v === '__all' ? null : v; } catch { return null; }
  });
  const [menu, setMenu] = useState(false);
  const [drag, setDrag] = useState<DragItem | null>(null);
  const [tailOpen, setTailOpen] = useState(true);
  const [tailShown, setTailShown] = useState(TAIL_PAGE);
  const [query, setQuery] = useState('');
  const [lifecycleNow, setLifecycleNow] = useState(() => Date.now());
  const [tip, setTip] = useState<Tip>(null);
  const [ctx, setCtx] = useState<{ row: Row; x: number; y: number } | null>(null);
  const [renameId, setRenameId] = useState<string | null>(null);
  const searchRef = useRef<HTMLInputElement>(null);

  const closeBrowser = useCallback(() => setBrowsing(false), []);
  const chooseFolder = useCallback((path: string) => { setBrowsing(false); store.addWorkspace(path); }, []);
  const closeMenu = useCallback(() => setMenu(false), []);
  const closeCtx = useCallback(() => setCtx(null), []);

  const pickScope = useCallback((next: string | null) => {
    setScope(next);
    try { localStorage.setItem(SCOPE_KEY, next === null ? '__all' : next); } catch { /* private mode */ }
  }, []);

  // ⌘K focuses the filter — the chip beside it is T3's, and a chip that does
  // nothing is worse than no chip.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && (e.key === 'k' || e.key === 'K')) {
        e.preventDefault(); searchRef.current?.focus(); searchRef.current?.select();
      }
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, []);

  useEffect(() => {
    const id = window.setInterval(() => setLifecycleNow(Date.now()), LIFECYCLE_TICK_MS);
    return () => window.clearInterval(id);
  }, []);

  const dropChatBefore = (targetChatId: string) => {
    if (drag) store.reorderChat(drag.id, targetChatId);
    setDrag(null);
  };

  const rows = useMemo(() => {
    const out: Row[] = [];
    const now = lifecycleNow;
    const needle = query.trim().toLowerCase();
    const manualOrder = new Map(app.chats.map((chat, index) => [chat.id, index]));
    for (const g of groups) {
      const key = normalizeWorkspacePath(g.path);
      if (scope !== null && scope !== key) continue;
      for (const chat of g.chats) {
        const project = workspaceLabelForChat(g, chat);
        if (needle && !`${chat.title} ${project}`.toLowerCase().includes(needle)) continue;
        const work = store.spawnedWork(chat);
        const live = chatHasLiveActivity(chat, work);
        const active = chat.id === app.activeId;
        const touched = lastTouchedAt(chat);
        const status = resolveStatus(chat, live, active, store.obligation(chat));
        const failing = status === 'failed' ? lastOf(chat.messages, (m) => m.role === 'assistant') : undefined;
        const settled = resolveSettled(chat, status, active, now, touched);
        const archived = resolveArchived(chat, status, now, touched);
        out.push({
          chat, project, touched, status, settled, archived,
          since: status === 'working' ? (workingSince(chat, work) || touched) : 0,
          bg: work.filter((item) => item.status === 'running').length,
          error: (failing?.content || '').split('\n')[0].slice(0, 140),
          order: manualOrder.get(chat.id) ?? Number.MAX_SAFE_INTEGER,
          // Density follows lifecycle, never age or selection. Every ordinary
          // thread stays full-size; only settled/archive rows are compact.
          card: isFullSizeSidebarRow(settled, archived),
        });
      }
    }
    return out;
  }, [groups, scope, query, app.activeId, app.chats, lifecycleNow]);

  const searching = query.trim().length > 0;
  // The live list is full-size cards. Settled/archive rows alone use the compact
  // representation; archived rows are absent from normal browsing and are
  // appended after every live/shelved search result.
  const cards = useMemo(
    () => orderSidebarRows(rows.filter((r) => !r.archived && !r.settled)),
    [rows],
  );
  const tail = useMemo(() => orderSidebarRows(rows.filter((r) => !r.archived && r.settled)), [rows]);
  const archived = useMemo(() => orderSidebarRows(rows.filter((r) => r.archived)), [rows]);
  const searchRows = useMemo(() => orderSearchRows(rows), [rows]);
  const tailVisible = tailOpen ? tail.slice(0, tailShown) : [];
  const tailHidden = tail.length - tailVisible.length;

  const scopeLabel = scope === null
    ? 'Todos los proyectos'
    : (groups.find((g) => normalizeWorkspacePath(g.path) === scope)?.name ?? 'Todos los proyectos');
  const scopeGroup = scope === null
    ? undefined
    : groups.find((g) => normalizeWorkspacePath(g.path) === scope);

  // Where + puts the next chat: the project you are scoped to, and in "Todos los
  // proyectos" the last one you started a chat in.
  const newTarget = newChatTarget(scope, groups, lastProject());
  const newTargetGroup = newTarget
    ? groups.find((g) => normalizeWorkspacePath(g.path) === newTarget)
    : undefined;
  const newTargetName = newTargetGroup?.name ?? null;
  const newTargetRemote = projectRemoteBadge(newTargetGroup?.machineId);
  const newTargetLabel = newTargetName
    ? (newTargetRemote ? `${newTargetRemote.machine}/${newTargetName}` : newTargetName)
    : null;

  // T3 settles the thread you are READING and then navigates forward. Keep that
  // explicit filing gesture distinct from merely opening an already-settled
  // thread, which now stays put until real new activity reactivates it.
  const settleRow = useCallback((row: Row) => {
    store.settleChat(row.chat.id, true);
    if (row.chat.id !== app.activeId) return;
    const at = cards.findIndex((r) => r.chat.id === row.chat.id);
    const rest = cards.filter((r) => r.chat.id !== row.chat.id);
    const next = rest[at] ?? rest[at - 1];   // the row that takes its place, else the one above
    if (next) store.switchChat(next.chat.id);
  }, [cards, app.activeId]);
  const unsettleRow = useCallback((row: Row) => { store.settleChat(row.chat.id, false); }, []);

  const rowProps = {
    drag, setDrag, onDropBefore: dropChatBefore, onTip: setTip,
    onMenu: (row: Row, x: number, y: number) => setCtx({ row, x, y }),
    onSettle: settleRow, onUnsettle: unsettleRow,
  };

  return (
    <aside className="side sv2" data-sidebar-version="v2">
      <div className="titlebar">
        <span className="tl"><span /><span /><span /></span>
        <span className="sp" />
      </div>

      <ModesSwitch />

      {/* Search + new chat — T3's first group: one wide control plus one square. */}
      <div className="sv2-tools">
        <div className="sv2-wide sv2-searchbox">
          <IcSearch />
          <input
            ref={searchRef} className="sv2-search" placeholder="Buscar" spellCheck={false}
            value={query} onChange={(e) => setQuery(e.target.value)}
            onKeyDown={(e) => { if (e.key === 'Escape') { e.preventDefault(); setQuery(''); e.currentTarget.blur(); } }}
          />
          {query
            ? <button className="sv2-clear" title="Limpiar" onClick={() => { setQuery(''); searchRef.current?.focus(); }}><IcClose /></button>
            : <kbd className="sv2-kbd">⌘K</kbd>}
        </div>
        <button
          className="sv2-sq"
          title={newTargetLabel ? `Nueva conversación en ${newTargetLabel}` : 'Nueva conversación'}
          onClick={() => store.newChat(true, newTarget, newTargetGroup?.machineId ?? '')}
        ><IcPlus /></button>
      </div>

      {/* Project scope + add folder — this row replaces the folder tree. */}
      <div className="sv2-tools sv2-scoperow">
        <button className="sv2-wide" onClick={() => setMenu((v) => !v)} aria-haspopup="menu" aria-expanded={menu}>
          {scopeGroup ? <WorkspaceProjectIcon group={scopeGroup} markRemote /> : <IcFolder />}
          <span className="sv2-wtxt">{scopeLabel}</span>
          <span className={`sv2-chev ${menu ? 'open' : ''}`}><IcChevron /></span>
        </button>
        <button className="sv2-sq" title="Añadir carpeta" onClick={() => setBrowsing(true)}><IcFolderPlus /></button>
        {menu && <ScopeMenu groups={groups} scope={scope} onPick={pickScope} onClose={closeMenu} />}
      </div>

      <div className="sv2-list" onScroll={() => setTip(null)}>
        {(searching ? searchRows.length === 0 : cards.length + tail.length === 0) && (
          <div className="side-empty">{searching
            ? 'Nada coincide con la búsqueda.'
            : archived.length ? 'Las conversaciones están archivadas. Buscalas arriba.' : 'Sin conversaciones todavía.'}</div>
        )}
        <ul role="list">
          {searching ? searchRows.map((row) => (
            <SidebarV2Row
              key={`${row.chat.id}:search`} row={row} active={row.chat.id === app.activeId}
              {...rowProps}
            />
          )) : (
            <>
              {cards.map((row) => (
                // Keyed per density, as T3 is: a row that changes density fades
                // in place instead of sliding through every row in between.
                <SidebarV2Row
                  key={`${row.chat.id}:${row.card ? 'card' : 'slim'}`} row={row} active={row.chat.id === app.activeId}
                  {...rowProps}
                />
              ))}
              {tail.length > 0 && (
                <li className="sv2-shelf-li">
                  <button className="sv2-shelf" onClick={() => setTailOpen((v) => !v)} aria-expanded={tailOpen}>
                    <span className="sv2-shelf-label">{tailOpen ? 'En reposo' : `En reposo (${tail.length})`}</span>
                    <span className="sv2-shelf-rule" />
                    <span className={`sv2-shelf-chev ${tailOpen ? 'open' : ''}`}><IcChevron /></span>
                  </button>
                </li>
              )}
              {tailVisible.map((row) => (
                <SidebarV2Row
                  key={`${row.chat.id}:slim`} row={row} active={row.chat.id === app.activeId}
                  {...rowProps}
                />
              ))}
              {tailOpen && tailHidden > 0 && (
                <li className="sv2-more-li">
                  <button className="sv2-more" onClick={() => setTailShown((n) => n + TAIL_PAGE)}>
                    Mostrar {Math.min(tailHidden, TAIL_PAGE)} más
                  </button>
                </li>
              )}
            </>
          )}
        </ul>
      </div>

      {/* T3's footer minus its settings button (user exclusion). */}
      <div className="foot">
        <FooterUpdateCards />
        <AccountMenu />
      </div>

      {tip && !ctx && <RowTooltip tip={tip} />}
      {ctx && (
        <RowMenu
          row={ctx.row} x={ctx.x} y={ctx.y} onClose={closeCtx}
          onRename={(id) => setRenameId(id)}
          onSettle={settleRow} onUnsettle={unsettleRow}
        />
      )}
      {renameId && <RenameBridge id={renameId} done={() => setRenameId(null)} />}
      {browsing && <WorkspaceBrowser onSelect={chooseFolder} onClose={closeBrowser} />}
    </aside>
  );
}

// The context menu's "Renombrar" has to reach into a row that owns its own edit
// state. Rather than lift that state into the list (and re-render every row on
// every keystroke), this bridges one rename through a prompt-free inline edit
// by focusing the row's title via a DOM event the row listens for.
function RenameBridge({ id, done }: { id: string; done: () => void }) {
  useEffect(() => {
    const el = document.querySelector<HTMLElement>(`[data-sv2-row="${CSS.escape(id)}"]`);
    el?.dispatchEvent(new MouseEvent('dblclick', { bubbles: true }));
    done();
  }, [id, done]);
  return null;
}
