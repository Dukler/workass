import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState, useSyncExternalStore, type PointerEvent, type WheelEvent } from 'react';
import type { Chat } from '../store/types';
import { store, useApp, useProc } from '../store/store';
import { chatPane } from '../store/right-pane';
import { AssistantMessage, LiveTurnPulse } from './AssistantMessage';
import { UserPill } from './messages';
import { IcDoc, IcTerminal, IcChanges, IcPreview, IcRail, IcBrowser, IcChevron } from '../icons';
import { normalizeWorkspacePath, rememberLastProject, workspaceName } from '../workspaces';
import { createTranscriptPinScheduler, transcriptPinnedAfterScroll } from '../transcript-scroll';
import { isWaitingSteerBoundary } from '../steering';
import { assistantTurnBlockRanges, buildCoalescedTurnBlockTimelineSegments } from '../timeline-layout';
import type { TranscriptTimelineSegment } from '../timeline-layout';

const WINDOW = 40; // hand-rolled windowing: render the last N, reveal older on demand

function useTurnBlockMessageVersions(messageIdsKey: string): void {
  const messageIds = useMemo(() => messageIdsKey ? messageIdsKey.split('\0') : [], [messageIdsKey]);
  const subscribe = useCallback((callback: () => void) => {
    const unsubscribers = messageIds.map((messageId) => store.subscribe(`msg:${messageId}`, callback));
    return () => { for (const unsubscribe of unsubscribers) unsubscribe(); };
  }, [messageIds]);
  const snapshot = useCallback(() => {
    let version = 0;
    for (const messageId of messageIds) version += store.version(`msg:${messageId}`);
    return version;
  }, [messageIds]);
  useSyncExternalStore(subscribe, snapshot, snapshot);
}

// Transcript copy/export menu removed by user request (2026-07-11): agents
// read chats through the daemon (archives + Agent API), humans don't paste
// transcripts around. The '\u00b7\u00b7\u00b7' menu returns when it has real options.

function Topbar({ chat }: { chat: Chat | null }) {
  const app = useApp();
  useProc(); // re-render the bg-process chip as proc:list / proc:changed land
  const pane = chatPane(chat); // this chat's right-column occupant (rail/browser/closed)
  // Workspace chip (user law 2026-07-11, Claude-Code reference): the titlebar
  // reads "<chat title> [workspace]" — ONE quiet chip with the workspace's
  // short name, not a path + group cluster repeating "workass" everywhere.
  const ws = (chat?.cwd ?? app.meta?.workspaceDir ?? '').split('/').filter(Boolean).slice(-1)[0] || null;
  return (
    <div className="tbar">
      {/* No reopen toggle here: the single sticky .side-toggle (App.tsx) owns
          show/hide in both pane states (user law 2026-07-11). */}
      <IcDoc />
      <b>{chat?.title ?? 'workass'}</b>
      {ws && <span className="provbadge" title={chat?.cwd ?? 'Workspace'}>{ws}</span>}
      {/* Provider chip removed by user request (2026-07-11) — the bound agent
          is visible in the composer's model selector; the titlebar stays
          title + workspace only. */}
      <span className="sp" />
      {/* Titlebar bg-process chip removed by user request (2026-07-11):
          background processes surface in the Tareas card + inline transcript
          rows only. */}
      <span className="ticons">
        <button className="tico" title="Terminal (próximamente)"><IcTerminal /></button>
        <button className="tico" title="Cambios (próximamente)"><IcChanges /></button>
        <button className="tico" title="Vista previa (próximamente)"><IcPreview /></button>
        {app.hasBrowserChannels && (
          <button className={`tico ${pane === 'browser' ? 'on' : ''}`} title="Navegador" aria-pressed={pane === 'browser'} onClick={() => store.toggleBrowser()}><IcBrowser /></button>
        )}
        <button className={`tico ${pane === 'rail' ? 'on' : ''}`} title="Mostrar/ocultar panel" onClick={() => store.toggleRail()}><IcRail /></button>
      </span>
    </div>
  );
}

// The empty chat asks one question and names the folder it will run in. The
// project is the only control: it unfolds the other REGISTERED folders in place,
// so starting a chat in the wrong project costs one click to fix instead of a
// trip through the sidebar (user 2026-07-25, approved mock B1).
function EmptyChat({ chat }: { chat: Chat | null }) {
  const app = useApp();
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement>(null);
  const current = normalizeWorkspacePath(chat?.cwd);
  const others = app.workspaces.filter((w) => normalizeWorkspacePath(w.path) !== current);

  useEffect(() => {
    if (!open) return;
    const away = (e: MouseEvent) => { if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false); };
    const esc = (e: KeyboardEvent) => { if (e.key === 'Escape') setOpen(false); };
    document.addEventListener('mousedown', away);
    document.addEventListener('keydown', esc);
    return () => { document.removeEventListener('mousedown', away); document.removeEventListener('keydown', esc); };
  }, [open]);

  function pick(path: string) {
    setOpen(false);
    if (!chat) return;
    // moveChat's beforeId is a DROP target and null means "put it last", so pass
    // this chat's current successor: switching folders must not also reorder it.
    const at = app.chats.findIndex((item) => item.id === chat.id);
    const beforeId = app.chats[at + 1]?.id ?? null;
    void store.moveChat(chat.id, beforeId, path).then((moved) => {
      // Choosing here is the same signal as "Nueva aquí": the next + follows.
      if (moved) rememberLastProject(path);
    });
  }

  const label = chat ? (current ? workspaceName(current) : 'elegí un proyecto') : null;
  const canSwitch = !!chat && others.length > 0;
  return (
    <div className="emptychat" ref={ref}>
      <div className="ask">¿Qué construimos hoy?</div>
      {label !== null && (canSwitch ? (
        <button className={`proj ${open ? 'open' : ''}`} aria-expanded={open} aria-haspopup="menu"
          onClick={() => setOpen((v) => !v)}>
          {label}<span className="chev"><IcChevron /></span>
        </button>
      ) : <div className="proj still">{label}</div>)}
      {open && (
        <div className="plist" role="menu">
          {others.map((w) => (
            <button key={w.path} role="menuitem" title={w.path} onClick={() => pick(w.path)}>{w.name}</button>
          ))}
        </div>
      )}
    </div>
  );
}

export function Transcript({ chat }: { chat: Chat | null }) {
  // Subscribe to app so the per-turn Deshacer/Revisar affordances appear once
  // this chat's checkpoints load (they bump `app`).
  const app = useApp();
  const scrollRef = useRef<HTMLDivElement>(null);
  const docRef = useRef<HTMLDivElement>(null);
  const stick = useRef(true);
  // A scroll event does not say whether it came from a person or from assigning
  // scrollTop. Keep a short, explicit input-intent window instead of counting
  // programmatic events: Chromium is allowed to coalesce several writes into one
  // event, which made the old counter drift and freeze following mid-stream.
  const userScrollUntil = useRef(0);
  const [reveal, setReveal] = useState(WINDOW);

  const visibleMessages = chat?.messages.filter((message) => !isWaitingSteerBoundary(message)) ?? [];
  const total = visibleMessages.length;
  const shown = visibleMessages.slice(Math.max(0, total - reveal));
  const hidden = total - shown.length;
  const runningMessage = [...shown].reverse().find((message) => (
    message.role === 'assistant'
    && message.status === 'running'
    && message.turnTerminal !== false
  ));
  const profile = app.meta?.profile ?? 'prod';
  const turnBlocks = assistantTurnBlockRanges(shown);
  const turnBlockMessageIds = turnBlocks.flatMap(({ start, end }) => shown.slice(start, end).map((message) => message.id));
  useTurnBlockMessageVersions(turnBlockMessageIds.join('\0'));
  const coalescedSegments = new Map<string, TranscriptTimelineSegment[]>();
  for (const { start, end } of turnBlocks) {
    const blockMessages = shown.slice(start, end);
    const blockSegments = buildCoalescedTurnBlockTimelineSegments(blockMessages, profile);
    for (let index = 0; index < blockMessages.length; index++) {
      coalescedSegments.set(blockMessages[index].id, blockSegments[index]);
    }
  }
  // The canonical array is already the permanent transcript chronology. Native
  // steering physically inserts user rows between assistant segments; rendering
  // must never regroup or anchor them at paint time.
  let latestUserMessageId: string | null = null;
  if (chat) {
    for (let i = visibleMessages.length - 1; i >= 0; i--) {
      if (visibleMessages[i].role !== 'user') continue;
      latestUserMessageId = visibleMessages[i].id;
      break;
    }
  }

  // SCROLL CONTRACT — "↓ N mensajes nuevos" pill state. `seen` is the message
  // count last observed while pinned to bottom; when the user scrolls up, new
  // messages accrue against it without shifting the viewport.
  const seen = useRef(total);
  const [newCount, setNewCount] = useState(0);
  // Windowing: remember distance-from-bottom across a "ver anteriores" reveal so
  // prepending older messages keeps the read position fixed.
  const keepFromBottom = useRef<number | null>(null);

  // Reset the window when switching chats.
  useEffect(() => {
    setReveal(WINDOW);
    stick.current = true;
    userScrollUntil.current = 0;
    seen.current = total;
    setNewCount(0);
  }, [chat?.id]);

  // Reconcile the new-message counter whenever the total changes.
  useEffect(() => {
    if (stick.current) { seen.current = total; if (newCount !== 0) setNewCount(0); }
    else { const n = Math.max(0, total - seen.current); if (n !== newCount) setNewCount(n); }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [total]);

  // Clamp writes to the real maximum. Programmatic scroll events are ignored by
  // onScroll unless a wheel/touch/scrollbar intent preceded them.
  const setScrollTop = (el: HTMLDivElement, top: number) => {
    const clamped = Math.max(0, Math.min(top, el.scrollHeight - el.clientHeight));
    if (Math.abs(el.scrollTop - clamped) < 1) return;
    el.scrollTop = clamped;
  };

  const markUserScroll = (durationMs = 240) => {
    userScrollUntil.current = performance.now() + durationMs;
  };

  // Track whether the user is pinned to the bottom; re-pinning clears the pill.
  const onScroll = () => {
    const el = scrollRef.current;
    if (!el) return;
    const userInitiated = performance.now() <= userScrollUntil.current;
    const atBottom = transcriptPinnedAfterScroll(stick.current, userInitiated, el);
    stick.current = atBottom;
    if (atBottom) { seen.current = total; if (newCount !== 0) setNewCount(0); }
  };

  const onWheel = (event: WheelEvent<HTMLDivElement>) => {
    markUserScroll();
    // Stop a pending observer frame before the browser applies an upward wheel
    // delta; otherwise that frame can pull the reader back to the bottom once.
    if (event.deltaY < 0) stick.current = false;
  };

  const onPointerDown = (event: PointerEvent<HTMLDivElement>) => {
    // Native scrollbar interactions target the scroll container itself. Do not
    // disable following for an ordinary click on message text or a tool card.
    if (event.target === event.currentTarget) markUserScroll(60_000);
  };

  const onPointerUp = () => markUserScroll();

  const onTouchStart = () => markUserScroll(60_000);
  const onTouchEnd = () => markUserScroll();

  const jumpToBottom = () => {
    const el = scrollRef.current;
    if (!el) return;
    userScrollUntil.current = 0;
    stick.current = true;
    seen.current = total;
    setNewCount(0);
    setScrollTop(el, el.scrollHeight);
  };

  const revealOlder = () => {
    const el = scrollRef.current;
    if (el) keepFromBottom.current = el.scrollHeight - el.scrollTop;
    setReveal((r) => r + WINDOW);
  };

  // Restore read position after older messages prepend (windowing).
  useLayoutEffect(() => {
    const el = scrollRef.current;
    if (el && keepFromBottom.current != null) {
      setScrollTop(el, el.scrollHeight - keepFromBottom.current);
      keepFromBottom.current = null;
    }
  }, [reveal]);

  // Sending is an explicit request to follow the new turn. Re-pin on the newest
  // user-message id even when the assistant placeholder is appended in the same
  // store update (so the user message is not necessarily the final array item).
  useLayoutEffect(() => {
    const el = scrollRef.current;
    if (!el || !latestUserMessageId) return;
    userScrollUntil.current = 0;
    stick.current = true;
    seen.current = total;
    setNewCount(0);
    setScrollTop(el, el.scrollHeight);
  }, [chat?.id, latestUserMessageId]);

  // Pin to bottom whenever content grows (streaming, new cards) while stuck.
  // Foreground updates coalesce into one pre-paint animation frame. Chromium
  // pauses rAF in hidden/occluded windows, so background updates instead use one
  // coalesced timer and focus/visibility restoration reconciles synchronously.
  // This keeps layout work bounded without letting an alt-tabbed stream drift.
  // Streaming deltas arrive in child components (AssistantMessage) that Transcript
  // never re-renders for, so both observers watch the doc subtree, not React.
  useLayoutEffect(() => {
    const doc = docRef.current;
    const el = scrollRef.current;
    if (!doc || !el) return;
    const scheduler = createTranscriptPinScheduler({
      isPinned: () => stick.current,
      isForeground: () => document.visibilityState === 'visible' && document.hasFocus(),
      pin: () => setScrollTop(el, el.scrollHeight),
      requestFrame: (callback) => requestAnimationFrame(callback),
      cancelFrame: (handle) => cancelAnimationFrame(handle),
      setTimer: (callback, delayMs) => window.setTimeout(callback, delayMs),
      clearTimer: (handle) => window.clearTimeout(handle),
    });
    const mo = new MutationObserver(scheduler.schedule);
    mo.observe(doc, { childList: true, subtree: true, characterData: true });
    const ro = new ResizeObserver(scheduler.schedule);
    ro.observe(doc);
    ro.observe(el); // the composer growing/shrinking resizes the viewport, not the doc — re-pin then too
    const reconcile = () => scheduler.reconcile();
    document.addEventListener('visibilitychange', reconcile);
    window.addEventListener('focus', reconcile);
    window.addEventListener('pageshow', reconcile);
    scheduler.reconcile();
    return () => {
      scheduler.dispose();
      mo.disconnect();
      ro.disconnect();
      document.removeEventListener('visibilitychange', reconcile);
      window.removeEventListener('focus', reconcile);
      window.removeEventListener('pageshow', reconcile);
    };
  }, [chat?.id]);

  return (
    <>
      <Topbar chat={chat} />
      <div className={`transcriptviewport${runningMessage ? ' has-live' : ''}`}>
        <div
          className="scroll"
          ref={scrollRef}
          onScroll={onScroll}
          onWheel={onWheel}
          onPointerDown={onPointerDown}
          onPointerUp={onPointerUp}
          onPointerCancel={onPointerUp}
          onTouchStart={onTouchStart}
          onTouchEnd={onTouchEnd}
          onTouchCancel={onTouchEnd}
        >
          <div className="doc" ref={docRef}>
            {!chat || total === 0 ? (
              <EmptyChat chat={chat} />
            ) : (
              <>
                {hidden > 0 && (
                  <button className="btn" style={{ margin: '4px auto 16px', display: 'block' }} onClick={revealOlder}>
                    Ver {Math.min(WINDOW, hidden)} mensajes anteriores
                  </button>
                )}
                {shown.map((m) => {
                  if (m.role === 'user') return <UserPill key={m.id} text={m.content} images={m.images} steerState={m.steerState} />;
                  return (
                    <AssistantMessage
                      key={m.id}
                      tabId={chat.id}
                      msg={m}
                      turnSeq={store.checkpointForJob(chat, m.jobId)?.turnSeq}
                      profile={profile}
                      coalescedSegments={coalescedSegments.get(m.id)}
                    />
                  );
                })}
              </>
            )}
          </div>
        </div>
        {runningMessage && <LiveTurnPulse msg={runningMessage} />}
        {newCount > 0 && (
          <button className={`newpill${runningMessage ? ' with-live' : ''}`} onClick={jumpToBottom}>
            <span className="arr">↓</span>{newCount} {newCount === 1 ? 'mensaje nuevo' : 'mensajes nuevos'}
          </button>
        )}
      </div>
    </>
  );
}
