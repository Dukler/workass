import { useCallback, useEffect, useRef, useState } from 'react';
import type { KeyboardEvent as ReactKeyboardEvent } from 'react';
import type { Chat } from '../store/types';
import { store, useApp, useSpawnedWork } from '../store/store';
import { call } from '../wire/api';
import { IcSearch, IcAssist, IcChats, IcPlus, IcActivity, IcGear, IcFolder, ModelIcon } from '../icons';
import { buildWorkspaceGroups, normalizeWorkspacePath, type WorkspaceGroup } from '../workspaces';
import { brandForProvider } from '../update-card';
import { providerUpdateRows, selfUpdateRow, type UpdateRow } from '../update-rows';
import { agoText } from './settings/updates';
import { chatHasLiveActivity } from '../chat-activity';
import { UPDATE_ARM_MS, appUpdateWorkWarning, appUpdaterBlockerText, updatesNeedAttention, useAppUpdater } from '../app-updater';
import { pushEscapeLayer } from '../markdown/html-frames';
import { WorkspaceBrowser } from './WorkspaceBrowser';
import { AccountsFootButton, closeAccounts } from './AccountsPage';
import { AutomationsFootButton, closeAutomations } from './Automations';
import { AlertsFootButton } from './Alerts';

// What is currently being dragged in the sidebar. `id` is the chat.id for a
// thread or the workspace path for a folder. Drag state lives in <Sidebar> and
// is passed down so rows/heads can show the right drop affordance and so drops
// read it directly (dataTransfer.getData is unavailable during dragover).
type DragItem = { kind: 'chat' | 'folder'; id: string };

function ChatRow({ chat, active, drag, setDrag, onDropBefore }: {
  chat: Chat; active: boolean;
  drag: DragItem | null; setDrag: (d: DragItem | null) => void;
  onDropBefore: (targetChatId: string) => void;
}) {
  const running = chatHasLiveActivity(chat, store.spawnedWork(chat));
  const [editing, setEditing] = useState(false);
  const [value, setValue] = useState(chat.title);
  const [over, setOver] = useState(false);
  const canDrop = drag?.kind === 'chat' && drag.id !== chat.id;

  const commit = () => { setEditing(false); store.renameChat(chat.id, value); };

  return (
    <div
      className={`sess ${active ? 'on' : ''}${over && canDrop ? ' dropbefore' : ''}${drag?.kind === 'chat' && drag.id === chat.id ? ' dragging' : ''}`}
      role="button" tabIndex={0}
      draggable={!editing}
      onClick={() => { if (!editing) store.switchChat(chat.id); }}
      onDoubleClick={() => { setValue(chat.title); setEditing(true); }}
      onDragStart={(e) => { e.dataTransfer.effectAllowed = 'move'; e.dataTransfer.setData('text/plain', chat.id); setDrag({ kind: 'chat', id: chat.id }); }}
      onDragEnd={() => { setDrag(null); setOver(false); }}
      onDragOver={(e) => { if (canDrop) { e.preventDefault(); setOver(true); } }}
      onDragLeave={() => setOver(false)}
      onDrop={(e) => { if (canDrop) { e.preventDefault(); onDropBefore(chat.id); } setOver(false); }}
    >
      <span className={`dot ${running ? 'run' : ''}`} />
      {editing ? (
        <input
          className="t tedit" value={value} autoFocus spellCheck={false}
          onChange={(e) => setValue(e.target.value)}
          onBlur={commit}
          onClick={(e) => e.stopPropagation()}
          onKeyDown={(e) => {
            if (e.key === 'Enter') { e.preventDefault(); commit(); }
            else if (e.key === 'Escape') { e.preventDefault(); setEditing(false); }
          }}
        />
      ) : (
        <span className="t">{chat.title}</span>
      )}
      {chat.unread && !active && !editing && <span className="m">•</span>}
      {!editing && <span className="x" title="Cerrar" onClick={(e) => { e.stopPropagation(); store.closeChat(chat.id); }}>×</span>}
    </div>
  );
}

function IcChev({ open }: { open: boolean }) {
  return (
    <svg className={`chev ${open ? 'open' : ''}`} viewBox="0 0 16 16" fill="none" stroke="currentColor" aria-hidden="true">
      <path d="M6 4l4 4-4 4" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  );
}

// Folder (workspace) header: chevron to collapse, name toggles too, a + to start
// a chat in it, drag to reorder folders, right-click to remove it FROM WORKASS.
// It's also a drop target: a thread dropped here moves INTO the folder, a folder
// dropped here reorders. The unassigned "Chats" bucket (path '') has no drag/
// remove but still collapses and accepts thread drops (→ unassigned).
function FolderHead({ group, collapsed, drag, setDrag, onDropChat, onDropFolder }: {
  group: WorkspaceGroup; collapsed: boolean;
  drag: DragItem | null; setDrag: (d: DragItem | null) => void;
  onDropChat: (path: string) => void; onDropFolder: (path: string) => void;
}) {
  const isFolder = !!group.path;
  const [over, setOver] = useState(false);
  const [menu, setMenu] = useState<{ x: number; y: number } | null>(null);
  const toggle = () => store.toggleWorkspaceCollapsed(group.path);
  const canDrop = !!drag && !(drag.kind === 'folder' && drag.id === group.path);

  return (
    <>
      <div
        className={`scap workspace-head${over && canDrop ? ' dropin' : ''}${collapsed ? ' collapsed' : ''}${drag?.kind === 'folder' && drag.id === group.path ? ' dragging' : ''}`}
        title={group.path || undefined}
        draggable={isFolder}
        onDragStart={isFolder ? (e) => { e.dataTransfer.effectAllowed = 'move'; e.dataTransfer.setData('text/plain', group.path); setDrag({ kind: 'folder', id: group.path }); } : undefined}
        onDragEnd={() => { setDrag(null); setOver(false); }}
        onDragOver={(e) => { if (canDrop) { e.preventDefault(); setOver(true); } }}
        onDragLeave={() => setOver(false)}
        onDrop={(e) => { if (!canDrop || !drag) return; e.preventDefault(); setOver(false); if (drag.kind === 'chat') onDropChat(group.path); else onDropFolder(group.path); }}
        onContextMenu={isFolder ? (e) => { e.preventDefault(); setMenu({ x: e.clientX, y: e.clientY }); } : undefined}
      >
        <button className="chevbtn" onClick={toggle} aria-label={collapsed ? 'Expandir carpeta' : 'Colapsar carpeta'}><IcChev open={!collapsed} /></button>
        <span className="wsname" onClick={toggle}>{group.name}</span>
        <span className="sp" />
        {isFolder && <button className="wsadd" title={`Nueva conversación en ${group.name}`} onClick={(e) => { e.stopPropagation(); store.newChat(true, group.path); }}><IcPlus /></button>}
      </div>
      {menu && (
        <FolderMenu
          x={menu.x} y={menu.y} name={group.name}
          onRemove={() => { store.removeWorkspace(group.path); setMenu(null); }}
          onClose={() => setMenu(null)}
        />
      )}
    </>
  );
}

// Right-click context menu for a folder. position:fixed at the click point so it
// escapes the sidebar's overflow; click-outside / Esc close it. "Remove" only
// drops the folder from Workass's list — it never touches the folder on disk.
function FolderMenu({ x, y, name, onRemove, onClose }: { x: number; y: number; name: string; onRemove: () => void; onClose: () => void }) {
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const onDown = (e: MouseEvent) => { if (ref.current && !ref.current.contains(e.target as Node)) onClose(); };
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') { e.stopPropagation(); e.preventDefault(); onClose(); } };
    document.addEventListener('mousedown', onDown);
    document.addEventListener('keydown', onKey, true);
    return () => { document.removeEventListener('mousedown', onDown); document.removeEventListener('keydown', onKey, true); };
  }, [onClose]);
  return (
    <div className="ctxmenu" ref={ref} style={{ left: x, top: y }} role="menu" aria-label={`Carpeta ${name}`}>
      <button className="ctxitem" role="menuitem" onClick={onRemove}>Quitar de Workass</button>
      <div className="ctxnote">No borra la carpeta del disco</div>
    </div>
  );
}

// Read the current translateX (px) off an element's computed transform matrix.
// Used to RESUME a WAAPI flourish from wherever it currently is on re-toggle,
// instead of snapping back to its start (the gallery's rubberband bug).
function currentTx(el: Element): number {
  const t = getComputedStyle(el).transform;
  if (!t || t === 'none') return 0;
  try { return new DOMMatrixReadOnly(t).m41; } catch { return 0; }
}

// Assist|Chats switch — gallery variant E "Icon relay" (user pick 2026-07-11).
// ONE interactive control (role=switch): a recessed groove holding a single
// raised thumb that slides to the active half. The flourish is the icon relay —
// on toggle the OUTGOING glyph detaches as a baton and slides the track while
// the incoming icon + label spring-scale in as it arrives.
//
// ANTI-RUBBERBAND (the gallery bug this lane fixes):
//  · the THUMB is a plain CSS transform TRANSITION (declarative class) — natively
//    reversible from its current position, so rapid re-toggles never snap it.
//  · the baton / spring flourishes are the Web Animations API (element.animate);
//    we keep each Animation handle and, on re-toggle, .cancel() and start the new
//    animation FROM the current computed position. NO classList/reflow retrigger.
// This motion PLAYS REGARDLESS of prefers-reduced-motion — a deliberate user
// decision (2026-07-11): his macOS has Reduce Motion ON and he chose motion for
// this personal tool. Do not "re-fix" it back to a static swap.
export function ModesSwitch() {
  const app = useApp();
  const mode = app.mode;
  const rootRef = useRef<HTMLDivElement>(null);
  const batonAnim = useRef<Animation | null>(null);
  const flourishes = useRef<Animation[]>([]);
  const mounted = useRef(false);
  const [pressed, setPressed] = useState(false);

  const toggle = () => store.setMode(mode === 'chats' ? 'assist' : 'chats');
  const onKeyDown = (e: ReactKeyboardEvent) => {
    if (e.key === ' ' || e.key === 'Enter') { e.preventDefault(); toggle(); }
    else if (e.key === 'ArrowLeft') { e.preventDefault(); store.setMode('assist'); }
    else if (e.key === 'ArrowRight') { e.preventDefault(); store.setMode('chats'); }
  };

  // Fire the relay whenever the mode flips (skip the initial mount so a page
  // load does not animate). The effect runs post-commit, so the baton's glyph
  // (rendered as the side we just LEFT) is already correct in the DOM.
  useEffect(() => {
    if (!mounted.current) { mounted.current = true; return; }
    runRelay(mode === 'chats');
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [mode]);

  function runRelay(toChats: boolean) {
    const root = rootRef.current; if (!root) return;
    const relay = root.querySelector('.relay') as HTMLElement | null;
    if (!relay) return;
    const w = relay.getBoundingClientRect().width + 4; // half-track width + inter-half gap
    const DUR = 380;

    // --- baton: cancel prior + resume from its current computed position ---
    const curX = currentTx(relay);
    const curOp = +getComputedStyle(relay).opacity;
    batonAnim.current?.cancel();
    const fresh = curOp < 0.02;                 // no baton in flight → begin at the origin side
    const originX = toChats ? 0 : w;
    const targetX = toChats ? w : 0;
    const x0 = fresh ? originX : curX;
    const op0 = fresh ? 0 : curOp;
    const d = targetX - x0;
    relay.style.willChange = 'transform, opacity';
    const baton = relay.animate([
      { offset: 0,    transform: `translateX(${x0}px) scale(${fresh ? 0.9 : 1.06})`, opacity: op0 },
      { offset: 0.14, transform: `translateX(${x0 + d * 0.12}px) scale(1.06)`, opacity: 1 },
      { offset: 0.5,  transform: `translateX(${x0 + d * 0.5}px) scale(1.12)`, opacity: 1 },
      { offset: 0.85, transform: `translateX(${targetX}px) scale(1.05)`, opacity: 1 },
      { offset: 1,    transform: `translateX(${targetX}px) scale(0.9)`, opacity: 0 },
    ], { duration: DUR, easing: 'cubic-bezier(.4,.1,.3,1)', fill: 'both' });
    batonAnim.current = baton;
    baton.onfinish = () => {
      if (batonAnim.current === baton) { baton.cancel(); batonAnim.current = null; relay.style.willChange = ''; }
    };

    // --- incoming spring-in + outgoing duck-out (transient; cancel any prior) ---
    flourishes.current.forEach((a) => a.cancel());
    flourishes.current = [];
    // Scope to `.half.chats` / `.half.assist`, NOT bare `.chats` — the root
    // `.modes` element also carries the chats/assist state class, so a bare
    // `.chats .ico` would match the first icon under the root (the wrong half).
    const inSel = toChats ? '.half.chats' : '.half.assist';
    const outSel = toChats ? '.half.assist' : '.half.chats';
    const inIco = root.querySelector(`${inSel} .ico`) as HTMLElement | null;
    const inLbl = root.querySelector(`${inSel} .lbl`) as HTMLElement | null;
    const outIco = root.querySelector(`${outSel} .ico`) as HTMLElement | null;
    if (inIco) flourishes.current.push(inIco.animate(
      [{ opacity: 0, transform: 'scale(.4)', offset: 0 }, { opacity: 0, transform: 'scale(.4)', offset: .6 },
       { opacity: 1, transform: 'scale(1.18)', offset: .8 }, { opacity: 1, transform: 'scale(1)', offset: 1 }],
      { duration: DUR, easing: 'cubic-bezier(.3,.8,.3,1.2)' }));
    if (inLbl) flourishes.current.push(inLbl.animate(
      [{ opacity: 0, transform: 'translateX(-4px) scale(.9)', offset: 0 }, { opacity: 0, transform: 'translateX(-4px) scale(.9)', offset: .66 },
       { opacity: 1, transform: 'translateX(1px) scale(1.04)', offset: .86 }, { opacity: 1, transform: 'translateX(0) scale(1)', offset: 1 }],
      { duration: DUR, easing: 'cubic-bezier(.3,.8,.3,1.2)' }));
    if (outIco) flourishes.current.push(outIco.animate(
      [{ opacity: 1, offset: 0 }, { opacity: 0, offset: .12 }, { opacity: 0, offset: .88 }, { opacity: 1, offset: 1 }],
      { duration: DUR, easing: 'ease' }));
  }

  return (
    <div
      ref={rootRef}
      className={`modes ${mode}${pressed ? ' pressed' : ''}`}
      role="switch"
      aria-checked={mode === 'chats'}
      aria-label="Cambiar entre Assist y Chats"
      tabIndex={0}
      onClick={toggle}
      onKeyDown={onKeyDown}
      onPointerDown={() => setPressed(true)}
      onPointerUp={() => setPressed(false)}
      onPointerLeave={() => setPressed(false)}
    >
      <span className="modes-thumb" aria-hidden="true" />
      <span className="half assist"><span className="ico"><IcAssist /></span><span className="lbl">Assist</span></span>
      <span className="half chats"><span className="ico"><IcChats /></span><span className="lbl">Chats</span></span>
      {/* Baton overlay carries the OUTGOING glyph (the side we just left) across
          the track; driven entirely by WAAPI, opacity 0 at rest. */}
      <span className="relay" aria-hidden="true"><span className="rico">{mode === 'chats' ? <IcAssist /> : <IcChats />}</span></span>
    </div>
  );
}

export function Sidebar() {
  const app = useApp();
  // Spawned-work snapshots deliberately use their own low-churn topic. The
  // sidebar subscribes once so every row's activity dot settles immediately as
  // background work starts or ends without coupling those updates to APP.
  useSpawnedWork();
  const groups = buildWorkspaceGroups(app.workspaces, app.chats, app.removedWorkspaces);
  // "Añadir carpeta" opens the server-owned folder browser (WorkspaceBrowser):
  // every client picks a folder on the machine that RUNS Workass, so there is no
  // native dialog here. The workspace lands in the store only on confirmation.
  const [browsing, setBrowsing] = useState(false);
  const closeBrowser = useCallback(() => setBrowsing(false), []);
  const chooseFolder = useCallback((path: string) => { setBrowsing(false); store.addWorkspace(path); }, []);

  const collapsed = new Set(app.collapsedWorkspaces.map((p) => normalizeWorkspacePath(p)));
  const [drag, setDrag] = useState<DragItem | null>(null);
  // A row-to-row drop only reorders. Moving to another project is the distinct
  // folder-head target below, so a slightly diagonal drag cannot change cwd.
  const dropChatBefore = (targetChatId: string) => {
    if (drag?.kind === 'chat') store.reorderChat(drag.id, targetChatId);
    setDrag(null);
  };
  // Drop a thread on a folder head → move it into that folder, at the top.
  const dropChatInFolder = (path: string) => {
    if (drag?.kind !== 'chat') return;
    const key = normalizeWorkspacePath(path);
    const firstInFolder = app.chats.find((c) => normalizeWorkspacePath(c.cwd ?? '') === key && c.id !== drag.id);
    store.moveChatToWorkspace(drag.id, firstInFolder?.id ?? null, path);
    setDrag(null);
  };
  // Drop a folder on another folder head → reorder before it.
  const dropFolderBefore = (path: string) => {
    if (drag?.kind === 'folder' && drag.id !== path) store.reorderWorkspaces(drag.id, path);
    setDrag(null);
  };

  return (
    <aside className="side">
      {/* The sidebar toggle is NOT here: it is one sticky control pinned next
          to the window traffic lights (App.tsx .side-toggle) so it never moves
          or duplicates when the pane opens/closes (user law 2026-07-11). */}
      <div className="titlebar">
        <span className="tl"><span /><span /><span /></span>
        <span className="sp" />
        <button className="tico" title="Buscar (próximamente)"><IcSearch /></button>
      </div>

      <ModesSwitch />

      <button className="srow" onClick={() => store.newChat()}><IcPlus />Nueva conversación</button>
      <button className="srow" onClick={() => setBrowsing(true)}><IcFolder />Añadir carpeta</button>
      <button className="srow" onClick={() => store.setMode('assist')}><IcActivity />Actividad</button>

      {app.chats.length === 0 && <div className="side-empty">Sin conversaciones todavía.</div>}
      {groups.map((g) => {
        const isCollapsed = collapsed.has(normalizeWorkspacePath(g.path));
        return (
          <div key={g.path || 'unassigned'} className="wsgroup">
            <FolderHead
              group={g} collapsed={isCollapsed}
              drag={drag} setDrag={setDrag}
              onDropChat={dropChatInFolder} onDropFolder={dropFolderBefore}
            />
            {!isCollapsed && g.chats.map((c) => (
              <ChatRow key={c.id} chat={c} active={c.id === app.activeId} drag={drag} setDrag={setDrag} onDropBefore={dropChatBefore} />
            ))}
            {!isCollapsed && g.chats.length === 0 && <div className="side-empty">Sin conversaciones</div>}
          </div>
        );
      })}

      {/* Session-count fleet row removed by user request (2026-07-11); the
          foot is the Ajustes and Actualizaciones icons (FooterBar). */}
      <div className="foot">
        <FooterBar />
      </div>

      {/* Portals to document.body, so collapsing the sidebar (⌘B) cannot hide it. */}
      {browsing && <WorkspaceBrowser onSelect={chooseFolder} onClose={closeBrowser} />}
    </aside>
  );
}

// Update-notification cards, pinned in the sidebar footer directly ABOVE the
// account row (Claude-Code footer pattern). Data is passive: `app:update`
// (mocked self-update) + `providers:updates` (real installed-vs-latest CLI
// versions), both replayed to fresh clients so a reload repaints with no turn.
// Cards are hairline panels (11px radius); NO green fill, icon muted, hover
// swaps the background. Both Workass and provider updates use the same
// resting → running ring → sealed success / inline retry lifecycle. They render
// inside the foot's update popover (FooterBar), and nothing when neither
// updater has work to show.
function IcDownload() {
  return (
    <svg viewBox="0 0 16 16" fill="none" stroke="currentColor">
      <path d="M8 2.5v7M5.2 6.8L8 9.6l2.8-2.8" strokeLinecap="round" strokeLinejoin="round" />
      <path d="M3 11.5v1a1 1 0 0 0 1 1h8a1 1 0 0 0 1-1v-1" strokeLinecap="round" />
    </svg>
  );
}
function IcCheck() {
  return (
    <svg viewBox="0 0 16 16" fill="none" stroke="currentColor">
      <path d="M3.5 8.4l3 3 6-6.6" strokeLinecap="round" strokeLinejoin="round" strokeWidth="1.7" />
    </svg>
  );
}

// The update popover's rows (owner 2026-11-11, mock update-popover/mock-v2.html
// variant A): only what needs updating, one fixed-height row each, the action
// as quiet text in the row's right slot, progress as a hairline on its foot.
// Never restore the legacy "La actualización sigue esperando" copy: active
// chat work is not an updater gate.
export function UpdateRows() {
  const app = useApp();
  const selfUpdater = useAppUpdater();
  const selfUpdate = selfUpdater.state;
  const connected = app.connection === 'connected';
  const [actionBusy, setActionBusy] = useState(false);
  // Armed = the first click found running work; the row says what the update
  // would stop and the next click proceeds anyway. Disarms on its own so a
  // stale warning never stands in for a fresh count.
  const [armed, setArmed] = useState('');
  useEffect(() => {
    if (!armed) return undefined;
    const timer = setTimeout(() => setArmed(''), UPDATE_ARM_MS);
    return () => clearTimeout(timer);
  }, [armed]);

  // The preflight runs for minutes: its clock starts when the shell says so.
  const preflight = selfUpdate.phase === 'staging' && selfUpdate.stagingStep === 'preflight';
  const preflightSince = useRef<number | null>(null);
  const [now, setNow] = useState(() => Date.now());
  if (preflight && preflightSince.current == null) preflightSince.current = Date.now();
  if (!preflight) preflightSince.current = null;
  useEffect(() => {
    if (!preflight) return undefined;
    const timer = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(timer);
  }, [preflight]);

  const runSelf = async () => {
    if (actionBusy) return;
    setActionBusy(true);
    try {
      if (!armed) {
        // A daemon without the count answers undefined → update straight away.
        const warning = appUpdateWorkWarning(
          await call('appUpdateWork'),
          (tabId) => app.chats.find((chat) => chat.id === tabId)?.title ?? '',
        );
        if (warning) { setArmed(warning); return; }
      }
      setArmed('');
      const next = await selfUpdater.apply();
      if (next.phase === 'busy') store.addToast('La actualización necesita reintento', appUpdaterBlockerText(next.blockers));
    } catch (error) {
      store.addToast('No se pudo actualizar', String((error as Error)?.message || error));
    } finally {
      setActionBusy(false);
    }
  };

  const self = selfUpdateRow({
    state: selfUpdate, armed, actionBusy,
    preflightSeconds: preflight && preflightSince.current != null ? (now - preflightSince.current) / 1000 : null,
  });
  const rows = [
    ...(self ? [self] : []),
    ...providerUpdateRows(app.providersUpdates, app.updateProgress, app.updateChain?.failedId ?? null,
      (id, cli) => store.providerName(id) || cli),
  ];

  const onAction = (row: UpdateRow) => {
    if (!row.action || row.action.disabled) return;
    if (row.action.kind === 'self') { void runSelf(); return; }
    if (!connected) return;
    void store.updateProvider(row.providerId);
  };

  if (!rows.length) {
    const checked = agoText(latestIso(selfUpdate.checkedAt, app.providersCheckedAt));
    return (
      <div className="uprow upempty">
        <span className="upgl" aria-hidden="true"><IcCheck /></span>
        <span className="uptx"><span className="upn">Todo al día</span><span className="ups">{checked ? `Revisado ${checked}` : 'Sin nada que actualizar'}</span></span>
      </div>
    );
  }
  return (
    <div className="uprows">
      {rows.map((row) => {
        const brand = row.providerId ? brandForProvider(row.providerId, app.providers, app.groups) : '';
        return (
          <div className="uprow" key={row.key} title={row.title || undefined}>
            <span className="upgl" aria-hidden="true">{brand ? <ModelIcon provider={brand} /> : <IcDownload />}</span>
            <span className="uptx">
              <span className="upn">{row.name}</span>
              <span className={`ups${row.tone ? ` ${row.tone}` : ''}`}>{row.status}</span>
            </span>
            <span className="upslot">
              {row.action ? (
                <button type="button" className={`upact${row.action.tone ? ` ${row.action.tone}` : ''}`}
                  disabled={row.action.disabled || (row.action.kind !== 'self' && !connected)}
                  onClick={() => onAction(row)}>{row.action.label}</button>
              ) : <span className="upv">{row.version}</span>}
            </span>
            {row.progress != null && (
              <span className={`upline${row.progress === 'ind' ? ' ind' : ''}`} aria-hidden="true">
                <i style={row.progress === 'ind' ? undefined : { transform: `scaleX(${row.progress})` }} />
              </span>
            )}
          </div>
        );
      })}
    </div>
  );
}

function latestIso(...values: Array<string | null | undefined>): string {
  let best = '';
  let bestAt = -Infinity;
  for (const value of values) {
    const at = value ? Date.parse(value) : NaN;
    if (Number.isFinite(at) && at > bestAt) { best = value as string; bestAt = at; }
  }
  return best;
}

// "Buscar actualizaciones": Workass and every agent, now. Its label holds a
// fixed width so the header never shifts while it looks.
function CheckUpdatesButton() {
  const updater = useAppUpdater();
  const [checking, setChecking] = useState(false);
  const run = async () => {
    if (checking) return;
    setChecking(true);
    const started = Date.now();
    try {
      await Promise.all([
        updater.state.supported ? updater.check().catch(() => undefined) : undefined,
        store.checkProviderUpdates().catch(() => false),
      ]);
    } finally {
      // Long enough to read, even when both answer at once.
      const left = 600 - (Date.now() - started);
      if (left > 0) await new Promise((resolve) => setTimeout(resolve, left));
      setChecking(false);
    }
  };
  return (
    <button type="button" className="upact plain upcheck" disabled={checking} onClick={() => void run()}>
      {checking ? 'Buscando…' : 'Buscar actualizaciones'}
    </button>
  );
}

// The left panel's foot (owner 2026-11-08): Ajustes, Cuentas (2026-11-09,
// AccountsPage.tsx) and Actualizaciones. Ajustes draws this same bar in the
// same place, so the gear closes Ajustes again and Cuentas takes its place. It replaced an account menu that
// held a fixed name, Ajustes and, rarely, a free reset; resets live on Cuentas.
// Updates no longer slide in as cards on their own: the update icon carries a
// green dot, and its popover holds the same cards (FooterUpdateCards, whole
// lifecycle) and the installed versions.
export function FooterBar() {
  const app = useApp();
  const updater = useAppUpdater();
  const self = updater.state;
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!open) return undefined;
    const onDown = (e: MouseEvent) => { if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false); };
    document.addEventListener('mousedown', onDown);
    const pop = pushEscapeLayer(() => setOpen(false));
    return () => { document.removeEventListener('mousedown', onDown); pop(); };
  }, [open]);

  const attention = updatesNeedAttention(self, app.providersUpdates, !!store.runningUpdate(), app.updateChain);
  const title = attention ? 'Actualizaciones' : 'Todo al día';

  return (
    <div className="footbar" ref={ref}>
      <button type="button" className={`footbtn${app.settingsOpen ? ' on' : ''}`} title="Ajustes" aria-label="Ajustes"
        aria-pressed={app.settingsOpen} onClick={() => { if (!app.settingsOpen) { closeAccounts(); closeAutomations(); } store.toggleSettings(); }}>
        <IcGear />
      </button>
      <AccountsFootButton />
      <AutomationsFootButton />
      <AlertsFootButton />
      <button type="button" className={`footbtn${open ? ' on' : ''}`} title={title} aria-label={title}
        aria-haspopup="dialog" aria-expanded={open} onClick={() => setOpen((v) => !v)}>
        <IcDownload />
        {attention && <span className="footdot" aria-hidden="true" />}
      </button>
      {open && (
        <div className="footpop" role="dialog" aria-label="Actualizaciones">
          <div className="footpop-head"><span>Actualizaciones</span><CheckUpdatesButton /></div>
          <UpdateRows />
        </div>
      )}
    </div>
  );
}
