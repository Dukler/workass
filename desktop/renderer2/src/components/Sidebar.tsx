import { useCallback, useEffect, useRef, useState } from 'react';
import type { KeyboardEvent as ReactKeyboardEvent, ReactNode } from 'react';
import type { Chat } from '../store/types';
import { store, useApp, useSpawnedWork } from '../store/store';
import { has } from '../wire/api';
import { IcSearch, IcAssist, IcChats, IcPlus, IcActivity, IcGear, IcFolder, ModelIcon } from '../icons';
import { buildWorkspaceGroups, normalizeWorkspacePath, type WorkspaceGroup } from '../workspaces';
import { nextUpdatePhase, brandForProvider, DONE_HOLD_MS, EXIT_MS, type UpdatePhase } from '../update-card';
import { availableRateLimitReset, prepareRateLimitResetAttempt, rateLimitResetExpiry, type RateLimitResetAttempt } from '../plan-usage';
import { chatHasLiveActivity } from '../chat-activity';
import { appUpdaterBlockerText, appUpdaterCardTitle, appUpdaterPhaseText, appUpdaterReceiptIsRecent, useAppUpdater } from '../app-updater';
import { WorkspaceBrowser } from './WorkspaceBrowser';

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

      {/* Session-count fleet row removed by user request (2026-07-11) — the
          footer is the update cards (when present) + the account menu. */}
      <div className="foot">
        <FooterUpdateCards />
        <AccountMenu />
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
// Cards use the .acctpop panel family (hairline, 11px radius); NO green fill,
// icon muted, hover lifts like .acct. Both Workass and provider updates use the
// same resting → running ring → sealed success / inline retry lifecycle. Renders
// nothing when neither updater has work to show.
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

// Last non-empty line of a redacted updater tail (single truncated line under
// the progress card / row). Empty until the first throttled snapshot lands.
function tailLine(tail: string | undefined): string {
  if (!tail) return '';
  const lines = tail.split(/\r?\n/).map((l) => l.trim()).filter(Boolean);
  return lines.length ? lines[lines.length - 1] : '';
}

function copyUpdateHint(hint: string) {
  const done = () => store.addToast('Comando copiado', hint);
  try {
    const p = navigator.clipboard?.writeText(hint);
    if (p && typeof p.then === 'function') { p.then(done).catch(() => store.addToast('Comando', hint)); }
    else done();
  } catch { store.addToast('Comando', hint); }
}

// The footer provider-updates card IS the update button: one click runs the
// daemon updater for every pending CLI, sequentially, with live progress + a
// border-illumination spinner right here — no navigation, so a coding session is
// never interrupted. Per-CLI management lives in Ajustes·Agentes.
export function FooterUpdateCards() {
  const app = useApp();
  const selfUpdater = useAppUpdater();
  const selfUpdate = selfUpdater.state;
  const provUpdates = app.providersUpdates.filter((u) => u.updateAvailable);
  const running = store.runningUpdate();
  const chain = app.updateChain;
  const failedId = chain?.failedId ?? null;
  const canUpdate = has('providersUpdate');
  const connected = app.connection === 'connected';

  const cliFor = (id: string) => app.providersUpdates.find((u) => u.providerId === id)?.cli || id;

  // Success-path lifecycle. `active` is true while a CLI streams or the chain is
  // still walking between CLIs (no failure yet); this keeps the card in `running`
  // across the microscopic gaps between sequential updates so it never flickers
  // to resting. On the terminal edge the machine seals to `done`, and the timers
  // below carry it done → exiting → hidden so it slides out instead of blinking.
  const active = !!running || (!!chain && !failedId);
  const pending = provUpdates.length;
  const [phase, setPhase] = useState<UpdatePhase>(() => nextUpdatePhase('hidden', { active, pending }));
  // Remember the CLI + provider that ran live, so the sealed "done" card can name
  // it and keep showing its brand mark after the live progress entry is gone.
  const lastCli = useRef('');
  const lastProviderId = useRef('');
  if (running) { lastCli.current = cliFor(running.providerId); lastProviderId.current = running.providerId; }

  // Data-driven transitions. A failure preempts this machine (its own card), so
  // reset to a neutral phase and let a later success re-run cleanly.
  useEffect(() => {
    if (failedId) { setPhase(pending > 0 ? 'resting' : 'hidden'); return; }
    setPhase((prev) => nextUpdatePhase(prev, { active, pending }));
  }, [active, pending, failedId]);

  // Time-driven transitions: hold the green seal, then run the slide-out.
  useEffect(() => {
    if (phase === 'done') {
      const t = setTimeout(() => setPhase('exiting'), DONE_HOLD_MS);
      return () => clearTimeout(t);
    }
    if (phase === 'exiting') {
      const t = setTimeout(() => setPhase('hidden'), EXIT_MS);
      return () => clearTimeout(t);
    }
    return undefined;
  }, [phase]);

  const sealing = phase === 'done' || phase === 'exiting';
  const selfActive = ['checking', 'downloading', 'staging', 'installing'].includes(selfUpdate.phase);
  const selfPending = ['available', 'ready', 'busy'].includes(selfUpdate.phase);
  const selfCheckFailed = selfUpdate.phase === 'check_failed';
  const selfFailed = selfUpdate.phase === 'failed' || selfUpdate.phase === 'rollback_healthy';
  const [selfPhase, setSelfPhase] = useState<UpdatePhase>('hidden');
  const [selfActionBusy, setSelfActionBusy] = useState(false);

  // Mirror the provider card's one-element lifecycle. A fresh terminal receipt
  // may seal after the updater restarts Electron; an old receipt stays hidden so
  // reopening Workass never replays a stale success animation.
  useEffect(() => {
    if (selfCheckFailed || selfFailed) { setSelfPhase('hidden'); return; }
    if (selfActive) { setSelfPhase('running'); return; }
    if (selfUpdate.phase === 'healthy') {
      setSelfPhase((prev) => prev === 'running' || appUpdaterReceiptIsRecent(selfUpdate.receipt) ? 'done' : 'hidden');
      return;
    }
    if (selfPending) { setSelfPhase('resting'); return; }
    setSelfPhase('hidden');
  }, [selfActive, selfCheckFailed, selfFailed, selfPending, selfUpdate.phase, selfUpdate.receipt]);

  useEffect(() => {
    if (selfPhase === 'done') {
      const timer = setTimeout(() => setSelfPhase('exiting'), DONE_HOLD_MS);
      return () => clearTimeout(timer);
    }
    if (selfPhase === 'exiting') {
      const timer = setTimeout(() => setSelfPhase('hidden'), EXIT_MS);
      return () => clearTimeout(timer);
    }
    return undefined;
  }, [selfPhase]);

  const selfSealing = selfPhase === 'done' || selfPhase === 'exiting';
  const showProvider = !!failedId || !!running || pending > 0 || sealing;
  const showSelfUpdate = selfUpdate.supported && (selfCheckFailed || selfFailed || selfPhase !== 'hidden');
  if (!showSelfUpdate && !showProvider) return null;

  const one = pending === 1 ? provUpdates[0] : null;
  const provTitle = one ? `${one.cli} ${one.latest}` : 'Actualizaciones de agentes';
  const provSub = one
    ? `instalada ${one.installed}`
    : provUpdates.map((u) => `${u.cli} ${u.latest}`).join(' · ');
  const doneSub = lastCli.current ? `${lastCli.current} actualizado` : 'Actualización completa';
  // Provider whose brand mark sits on the right of the live card: the running CLI,
  // the one that just sealed, or the single pending update. Empty when several
  // agents are pending at once (no single brand to show).
  const iconProviderId = running ? running.providerId : sealing ? lastProviderId.current : one ? one.providerId : '';
  const iconBrand = brandForProvider(iconProviderId, app.providers, app.groups);

  // Resting card is the update button (or opens Ajustes on an older bridge). It
  // shares the .updcard box as a role=button div so it lives inside the same
  // .updslot element across the whole lifecycle — the enter/exit slide plays on
  // that stable wrapper, never re-firing when the inner mode swaps.
  const restOpen = canUpdate ? () => void store.startUpdateChain() : () => store.openSettings('agentes');
  const restDisabled = canUpdate && !connected;

  const selfRunning = selfPhase === 'running';
  const selfAction = selfPending || selfCheckFailed || selfFailed ? selfUpdater.apply : null;
  const selfTitle = appUpdaterCardTitle(selfUpdate);
  const selfActionLabel = selfActionBusy
    ? 'Comprobando…'
    : selfUpdate.phase === 'busy'
    ? 'Reintentar'
    : 'Actualizar';
  const runSelfAction = async () => {
    if (!selfAction || selfActionBusy) return;
    setSelfActionBusy(true);
    try {
      const next = await selfAction();
      if (next.phase === 'busy') {
        store.addToast('La actualización sigue esperando', appUpdaterBlockerText(next.blockers));
      }
    } catch (error) {
      store.addToast('No se pudo actualizar', String((error as Error)?.message || error));
    } finally {
      setSelfActionBusy(false);
    }
  };

  return (
    <div className="updcards">
      {showSelfUpdate && (
        <div className={`updslot${selfPhase === 'exiting' ? ' card-exit' : ''}`}>
          {selfCheckFailed || selfFailed ? (
            <div className="updcard upd-fail" title={appUpdaterPhaseText(selfUpdate)}>
              <span className="uico ufail" aria-hidden="true"><IcDownload /></span>
              <span className="ubody">
                <span className="ut">{selfTitle}</span>
                <span className="us">{appUpdaterPhaseText(selfUpdate)}</span>
                <span className="ufail-act">
                  <button className="uretry" disabled={selfActionBusy} onClick={() => void runSelfAction()}>
                    {selfActionBusy ? 'Reintentando…' : selfUpdate.availableVersion ? 'Actualizar' : 'Reintentar'}
                  </button>
                </span>
              </span>
            </div>
          ) : (
            <button
              type="button"
              className={`updcard${selfRunning ? ' upd-run' : ''}${selfSealing ? ' upd-run upd-done' : ''}${selfActionBusy ? ' upd-off' : ''}`}
              disabled={selfRunning || selfSealing || !selfAction || selfActionBusy}
              onClick={() => void runSelfAction()}
              title={appUpdaterPhaseText(selfUpdate)}
            >
              <span className="updring" aria-hidden="true" />
              <span className={`uico${selfSealing ? ' udone' : ''}`} aria-hidden="true">
                <span className="uglyph" key={selfSealing ? 'check' : 'dl'}>
                  {selfSealing ? <IcCheck /> : <IcDownload />}
                </span>
              </span>
              <span className="ubody" key={selfRunning ? selfUpdate.phase : selfSealing ? 'done' : 'rest'}>
                <span className="ut">{selfTitle}</span>
                <span className="us">{appUpdaterPhaseText(selfUpdate)}</span>
              </span>
              {selfAction && !selfRunning && !selfSealing && <span className="uarrow updo" aria-hidden="true">{selfActionLabel}</span>}
            </button>
          )}
        </div>
      )}
      {showProvider && (
        <div className={`updslot${phase === 'exiting' ? ' card-exit' : ''}`}>
          {failedId ? (
            // Failure: the failed CLI inline with a Reintentar button + the manual
            // command as a copy-chip fallback; remaining pending CLIs stay listed.
            <div className="updcard upd-fail" key="fail">
              <span className="uico ufail" aria-hidden="true"><IcDownload /></span>
              <span className="ubody">
                <span className="ut">{cliFor(failedId)}: error{typeof app.updateProgress[failedId]?.exitCode === 'number' ? ` (exit ${app.updateProgress[failedId]!.exitCode})` : ''}</span>
                {provUpdates.some((u) => u.providerId !== failedId) && (
                  <span className="us">Pendientes: {provUpdates.filter((u) => u.providerId !== failedId).map((u) => u.cli).join(' · ')}</span>
                )}
                <span className="ufail-act">
                  <button
                    className="uretry"
                    disabled={!connected}
                    onClick={() => void store.startUpdateChain()}
                    title={connected ? 'Reintentar la actualización' : 'Sin conexión con el daemon'}
                  >Reintentar</button>
                  {(app.providersUpdates.find((u) => u.providerId === failedId)?.hint) && (
                    <code
                      className="ucopy" role="button" tabIndex={0} title="Copiar comando"
                      onClick={() => copyUpdateHint(app.providersUpdates.find((u) => u.providerId === failedId)!.hint!)}
                      onKeyDown={(e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); copyUpdateHint(app.providersUpdates.find((u) => u.providerId === failedId)!.hint!); } }}
                    >{app.providersUpdates.find((u) => u.providerId === failedId)!.hint}</code>
                  )}
                </span>
              </span>
            </div>
          ) : (
            // One stable "live" card across resting → running → done, so the
            // enter slide plays once and the ring seals in place — no re-blink on
            // each transition. Child order is fixed (ring, icon, body, arrow) so
            // React never morphs one slot's element into another; the ring stays
            // in the DOM (hidden via CSS) while resting to hold that order. The
            // glyph and body are keyed by mode so they cross-fade on every swap
            // instead of hard-cutting.
            <div
              key="live"
              className={`updcard${running ? ' upd-run' : ''}${sealing ? ' upd-run upd-done' : ''}${!running && !sealing && restDisabled ? ' upd-off' : ''}`}
              {...(running || sealing ? {} : {
                role: 'button',
                tabIndex: restDisabled ? -1 : 0,
                'aria-disabled': restDisabled || undefined,
                onClick: restDisabled ? undefined : restOpen,
                onKeyDown: restDisabled ? undefined : (e: ReactKeyboardEvent) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); restOpen(); } },
              })}
              title={
                running ? `Actualizando ${cliFor(running.providerId)}…`
                  : sealing ? doneSub
                  : canUpdate ? (connected ? 'Actualizar ahora' : 'Sin conexión con el daemon') : 'Ver en Ajustes · Agentes'
              }
            >
              {/* The border ring is the only spinner while updating (no glyph
                  spinner "line" in the icon slot). The left glyph is the status
                  mark — a static download while pending/updating, a check once
                  sealed — keyed by identity so it only cross-fades at that swap. */}
              <span className="updring" aria-hidden="true" />
              <span className={`uico${sealing ? ' udone' : ''}`} aria-hidden="true">
                <span className="uglyph" key={sealing ? 'check' : 'dl'}>
                  {sealing ? <IcCheck /> : <IcDownload />}
                </span>
              </span>
              <span className="ubody" key={running ? 'run' : sealing ? 'done' : 'rest'}>
                {running ? (
                  <>
                    <span className="ut">
                      Actualizando {cliFor(running.providerId)}…
                      {chain && chain.ids.length > 1 && <span className="ucount"> ({chain.index}/{chain.ids.length})</span>}
                    </span>
                    <span className="us mono">{tailLine(running.tail) || 'iniciando…'}</span>
                  </>
                ) : sealing ? (
                  <>
                    <span className="ut">Listo</span>
                    <span className="us">{doneSub}</span>
                  </>
                ) : (
                  <>
                    <span className="ut">{provTitle}</span>
                    <span className="us">{provSub}</span>
                  </>
                )}
              </span>
              {/* Right slot: the model provider's brand mark (replaces the old
                  "Actualizar" label). Empty when no single brand applies. */}
              {iconBrand && <span className="uend mico" aria-hidden="true"><ModelIcon provider={iconBrand} /></span>}
            </div>
          )}
        </div>
      )}
    </div>
  );
}

// Account menu (Claude-Code style): the bottom account row is a button that
// opens a small popup anchored ABOVE it. `items` is a flat array so adding
// future rows is a one-line change. Click-outside and Esc close it; the global
// ⌘, shortcut (App.tsx) still opens Settings directly.
type MenuItem = { key: string; icon: ReactNode; label: string; kbd?: string; onSelect: () => void };

export function AccountMenu() {
  const app = useApp();
  const [open, setOpen] = useState(false);
  const [resetBusy, setResetBusy] = useState(false);
  const resetAttempt = useRef<RateLimitResetAttempt | null>(null);
  const ref = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!open) return;
    const onDown = (e: MouseEvent) => { if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false); };
    // Capture Esc so the menu closes before App.tsx's layered Esc handler runs.
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') { e.stopPropagation(); e.preventDefault(); setOpen(false); } };
    document.addEventListener('mousedown', onDown);
    document.addEventListener('keydown', onKey, true);
    return () => { document.removeEventListener('mousedown', onDown); document.removeEventListener('keydown', onKey, true); };
  }, [open]);

  const label = 'workass';
  const resetEntry = Object.values(app.planUsageByProvider)
    .map((snapshot) => ({ snapshot, reset: availableRateLimitReset(snapshot.rateLimitResetCredits) }))
    .find((entry) => entry.reset != null);
  const reset = resetEntry?.reset ?? null;
  const resetProviderId = resetEntry?.snapshot.providerId ?? '';
  const resetProviderName = store.providerName(resetProviderId) ?? resetProviderId;
  const resetProviderBrand = store.providerBrand(resetProviderId);
  const active = app.chats.find((chat) => chat.id === app.activeId);
  const resetSessionId = active?.providerId === resetProviderId ? active.sessionId : undefined;
  const canUseReset = has('appChatUseRateLimitReset');
  const useEarnedReset = async () => {
    if (!reset || resetBusy || !canUseReset) return;
    const creditId = reset.credit?.id;
    resetAttempt.current = prepareRateLimitResetAttempt(resetAttempt.current, reset, () => (
      typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function'
        ? crypto.randomUUID()
        : `workass-reset-${Date.now()}-${Math.random().toString(36).slice(2)}`
    ));
    setResetBusy(true);
    try {
      const result = await store.useRateLimitReset(resetProviderId, resetSessionId || undefined, resetAttempt.current.idempotencyKey, creditId);
      switch (result?.outcome) {
        case 'reset':
        case 'alreadyRedeemed':
          resetAttempt.current = null;
          store.addToast('Reset aplicado', `${resetProviderName || 'El proveedor'} actualizó tus límites.`);
          break;
        case 'nothingToReset':
          resetAttempt.current = null;
          store.addToast('Reset guardado', 'Todavía no hay una ventana elegible para reiniciar.');
          break;
        case 'noCredit':
          resetAttempt.current = null;
          store.addToast('Reset no disponible', 'El proveedor ya no encuentra ese reset gratis.');
          break;
        default:
          throw new Error('El proveedor no devolvió un resultado de reset reconocido.');
      }
    } catch (error) {
      const detail = error instanceof Error ? error.message : String(error || 'Error desconocido');
      store.addToast('No se pudo confirmar el reset', `${detail} Podés reintentar sin gastar dos créditos.`);
    } finally {
      setResetBusy(false);
    }
  };
  const items: MenuItem[] = [
    { key: 'settings', icon: <IcGear />, label: 'Ajustes', kbd: '⌘,', onSelect: () => { setOpen(false); store.openSettings(); } },
  ];

  return (
    <div className="acctmenu" ref={ref}>
      {open && (
        <div className="acctpop" role="menu" aria-label="Menú de cuenta">
          <div className="acctpop-head">Martin · {label}</div>
          {reset && (
            <div className="acctcredit" role="group" aria-label={`${reset.count} reset gratis disponible`}>
              <div className="acctcredit-top">
                <span className="acctcredit-mark" aria-hidden>{resetProviderBrand && <ModelIcon provider={resetProviderBrand} />}</span>
                <span className="acctcredit-title">{resetProviderName || 'Proveedor'} reset</span>
                <span className="acctcredit-count">×{reset.count}</span>
              </div>
              <div className="acctcredit-copy">
                {reset.credit?.description || 'Crédito disponible para reiniciar tus límites.'}
              </div>
              <div className="acctcredit-foot">
                <span className="acctcredit-expiry">{rateLimitResetExpiry(reset.credit?.expiresAt)}</span>
                <button className="acctcredit-use" disabled={!canUseReset || resetBusy} onClick={() => { void useEarnedReset(); }}>
                  {resetBusy ? 'Aplicando…' : 'Usar reset'}
                </button>
              </div>
            </div>
          )}
          <div className="acctpop-div" />
          {items.map((it) => (
            <button key={it.key} className="acctpop-item" role="menuitem" onClick={it.onSelect}>
              <span className="ai">{it.icon}</span>
              <span className="al">{it.label}</span>
              {it.kbd && <span className="ak">{it.kbd}</span>}
            </button>
          ))}
        </div>
      )}
      <button className={`acct ${open ? 'on' : ''}`} onClick={() => setOpen((v) => !v)} aria-haspopup="menu" aria-expanded={open}>
        <span className="ava" aria-hidden="true" />
        <span className="acct-name">{label}</span>
        <span className="chev">▾</span>
      </button>
    </div>
  );
}
