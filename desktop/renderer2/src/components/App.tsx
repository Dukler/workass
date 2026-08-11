import { useEffect, useRef, useState } from 'react';
import type { MouseEvent as ReactMouseEvent } from 'react';
import { store, useApp } from '../store/store';
import { chatPane } from '../store/right-pane';
// Sidebar v2 (T3 Code port). v1 still lives in ./Sidebar (unmutated, and still
// the source of ModesSwitch/FooterUpdateCards/AccountMenu) — reverting is
// importing { Sidebar } from './Sidebar' and swapping the one element below.
import { SidebarV2 } from './SidebarV2';
import { Transcript } from './Transcript';
import { Composer } from './Composer';
import { ConnectionBanner } from './ConnectionBanner';
import { RightRail } from './RightRail';
import { Settings } from './Settings';
import { RewindMenu } from './RewindMenu';
import { ReviewPanel } from './ReviewPanel';
import { Toasts } from './Toasts';
import { CommandBar } from './CommandBar';
import { ImageLightbox } from './ImageLightbox';
import { ImageClipboardController } from './ImageClipboardController';
import { IcSidebar } from '../icons';

function AssistStub() {
  return (
    <div className="scroll">
      <div className="doc-empty">
        <div className="big">Assist</div>
        <div>El panel de monitoreo de Teams y Jira llegará en una próxima fase.</div>
        <div style={{ marginTop: 4 }}>Por ahora, usá <b style={{ fontWeight: 600 }}>Chats</b> para conversar con tu agente.</div>
      </div>
    </div>
  );
}

// Smallest chat column we protect: below this the transcript reads as a squished
// dead band, so a wide right pane is clamped to leave at least this much for the
// chat (user fix 2026-07-12 — a manually/agent-opened pane must not push the
// chat off-screen). On a wide monitor the full pane width is honoured.
const MIN_MAIN = 600;

export function App() {
  const app = useApp();
  const chat = store.active();
  // The right column's occupant is per-chat: read the ACTIVE chat's choice.
  const activePane = chatPane(chat);

  // Recompute the pane-width clamp when the window resizes (the grid CSS already
  // drops the rail entirely below 1180px; this handles the range above it).
  const [winW, setWinW] = useState(() => (typeof window !== 'undefined' ? window.innerWidth : 1440));
  useEffect(() => {
    const onResize = () => setWinW(window.innerWidth);
    addEventListener('resize', onResize);
    return () => removeEventListener('resize', onResize);
  }, []);

  // Inside the Electron shell the OS owns the real macOS and Windows caption
  // controls. Flag the document so CSS can hide the browser-mode stand-ins and
  // turn the titlebar rows into drag regions where the frame permits it.
  useEffect(() => {
    const electron = navigator.userAgent.includes('Electron');
    document.documentElement.classList.toggle('electron', electron);
    document.documentElement.classList.toggle('electron-windows', electron && window.workassWindow?.platform === 'win32');
  }, []);

  // Reflect pane / settings state onto #root (the grid host lives in index.html).
  // Custom drag widths drive CSS vars the grid templates consume; rail-wide folds
  // into the effective rail width so ⤢ composes with a custom width.
  useEffect(() => {
    const root = document.getElementById('root');
    if (!root) return;
    root.style.setProperty('--side-w', `${app.panes.sideW}px`);
    const railBase = app.panes.railWide ? Math.max(app.panes.railW, 470) : app.panes.railW;
    // Never let the (shared, drag-persisted) right pane grow so wide it shoves the
    // transcript into a dead band: cap it to whatever leaves MIN_MAIN for the chat.
    // Honoured fully when the window is wide enough; shrinks the pane, not the chat.
    const maxRail = Math.max(320, winW - app.panes.sideW - MIN_MAIN);
    const railEff = Math.min(railBase, maxRail);
    root.style.setProperty('--rail-w', `${railEff}px`);
    if (app.settingsOpen) { root.className = 'app settings'; return; }
    const cls = ['app'];
    if (!app.panes.side) cls.push('no-side');
    // The browser owns the shared right column while it's open (same --rail-w
    // width as the info rail); keep the column shown and ignore rail-wide.
    if (activePane === 'browser') {
      cls.push('browser');
    } else if (activePane === 'rail') {
      if (app.panes.railWide) cls.push('rail-wide');
    } else {
      cls.push('no-rail');
    }
    root.className = cls.join(' ');
  }, [app.panes.side, app.panes.railWide, app.panes.sideW, app.panes.railW, app.settingsOpen, activePane, winW]);

  // ⌘, opens the command box (user, 2026-07-26 — after a promotion left the app
  // running but not the controller with no in-app way out). Ajustes moved into
  // that box rather than off the map; it is one of its commands, two keystrokes
  // away. ⌘, + Enter runs the RELOAD, deliberately: the state this exists for is
  // the one where typing may be the only thing still working.
  //
  // Its own listener, in the CAPTURE phase, because a recovery key must not be
  // swallowed by whichever field holds focus. Everything else stays on the
  // bubble listener below — moving Esc to capture put it AHEAD of the lightbox
  // and popover handlers, so one Esc closed an image AND cancelled the turn.
  useEffect(() => {
    const h = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key === ',') { e.preventDefault(); store.toggleCommandBar(); }
    };
    addEventListener('keydown', h, true);
    return () => removeEventListener('keydown', h, true);
  }, []);

  // ⌘B toggles the sidebar. Esc is layered (R4):
  //  · closes an open overlay (command box / settings / rewind / review) first;
  //  · while a turn runs, a single Esc cancels it;
  //  · a double-Esc (two presses within ~550ms) opens the rewind menu.
  const lastEsc = useRef(0);
  useEffect(() => {
    const h = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && (e.key === 'b' || e.key === 'B')) { if (!store.state.settingsOpen) { e.preventDefault(); store.toggleSide(); } return; }
      if (e.key !== 'Escape') return;
      if (store.state.commandBarOpen) { store.closeCommandBar(); lastEsc.current = 0; return; }
      if (store.state.settingsOpen) { store.closeSettings(); lastEsc.current = 0; return; }
      if (store.state.rewind.open) { store.closeRewind(); lastEsc.current = 0; return; }
      if (store.state.review.open) { store.closeReview(); lastEsc.current = 0; return; }
      const now = Date.now();
      const isDouble = now - lastEsc.current < 550;
      lastEsc.current = now;
      if (store.isChatRunning(null)) { store.cancelActive(); return; }  // single Esc cancels; a follow-up Esc opens the menu
      if (isDouble) { lastEsc.current = 0; void store.openRewind(); }
    };
    addEventListener('keydown', h);
    return () => removeEventListener('keydown', h);
  }, []);

  // Settings takes the whole view over — but the command box has to survive that
  // takeover, because "I am in settings and the app is wedged" is exactly one of
  // the states it exists for.
  if (app.settingsOpen) return <><Settings /><CommandBar /><Toasts /></>;

  return (
    <>
      {/* One sticky sidebar toggle, pinned to the right of the green traffic
          light. position:fixed keeps it in the SAME physical spot whether the
          sidebar is open or closed (user law 2026-07-11); ⌘B (above) mirrors it. */}
      <button
        className="tico side-toggle"
        title={app.panes.side ? 'Colapsar panel · ⌘B' : 'Mostrar conversaciones · ⌘B'}
        aria-label={app.panes.side ? 'Colapsar panel' : 'Mostrar conversaciones'}
        aria-pressed={app.panes.side}
        onClick={() => store.toggleSide()}
      >
        <IcSidebar />
      </button>
      <SidebarV2 />
      {app.panes.side && <ResizeHandle which="side" />}
      <main>
        {app.mode === 'assist' ? <AssistStub /> : <Transcript key={`transcript:${chat?.id ?? 'none'}`} chat={chat} />}
        {app.mode !== 'assist' && <ConnectionBanner />}
        {app.mode !== 'assist' && <Composer key={`composer:${chat?.id ?? 'none'}`} chat={chat} />}
      </main>
      <RightRail key={`rail:${chat?.id ?? 'none'}`} chat={chat} />
      {/* One shared resize handle for the right column — drives --rail-w whether
          it hosts the info rail or the browser (single shared width). */}
      {activePane && <ResizeHandle which="rail" />}
      <RewindMenu />
      <ReviewPanel />
      <CommandBar />
      <Toasts />
      <ImageLightbox />
      <ImageClipboardController />
    </>
  );
}

// Drag handle on a pane's inner edge. Live-updates the CSS var during drag for a
// smooth resize, commits (persists) on release. Double-click resets the default.
// Sidebar 220–400px, rail 260–900px (shared by info rail + browser). Composes
// with ⌘B / rail ⤢ (which only flip pane visibility / effective width).
function ResizeHandle({ which }: { which: 'side' | 'rail' }) {
  const cfg = which === 'side'
    ? { aside: 'aside.side', varName: '--side-w', min: 220, max: 400, def: 288 }
    : { aside: 'aside.rail', varName: '--rail-w', min: 260, max: 900, def: 312 };
  const onDown = (e: ReactMouseEvent) => {
    e.preventDefault();
    const root = document.getElementById('root');
    const el = document.querySelector(cfg.aside) as HTMLElement | null;
    if (!root) return;
    const startX = e.clientX;
    const startW = el ? el.getBoundingClientRect().width
      : (which === 'side' ? store.state.panes.sideW : store.state.panes.railW);
    let last = startW;
    const move = (ev: MouseEvent) => {
      const dx = ev.clientX - startX;
      last = Math.max(cfg.min, Math.min(cfg.max, which === 'side' ? startW + dx : startW - dx));
      root.style.setProperty(cfg.varName, `${last}px`);
    };
    const up = () => {
      document.removeEventListener('mousemove', move);
      document.removeEventListener('mouseup', up);
      document.body.classList.remove('resizing');
      store.setPaneWidth(which, last);
    };
    document.body.classList.add('resizing');
    document.addEventListener('mousemove', move);
    document.addEventListener('mouseup', up);
  };
  return (
    <div
      className={`rzh ${which}-h`}
      onMouseDown={onDown}
      onDoubleClick={() => store.setPaneWidth(which, cfg.def)}
      role="separator"
      aria-orientation="vertical"
      title="Arrastrá para redimensionar · doble clic para restablecer"
    />
  );
}
