import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState, useSyncExternalStore, type PointerEvent, type RefObject, type WheelEvent } from 'react';
import type { Chat } from '../store/types';
import { store, useApp, useProc } from '../store/store';
import { chatPane } from '../store/right-pane';
import { AssistantMessage, LiveTurnPulse } from './AssistantMessage';
import { UserPill } from './messages';
import { IcDoc, IcTerminal, IcChanges, IcPreview, IcRail, IcBrowser, IcChevron, IcClose, IcSearch } from '../icons';
import { normalizeWorkspacePath, rememberLastProject, workspaceName } from '../workspaces';
import { createTranscriptPinScheduler, transcriptPinnedAfterScroll } from '../transcript-scroll';
import { isWaitingSteerBoundary } from '../steering';
import { assistantTurnBlockRanges, buildCoalescedTurnBlockTimelineSegments } from '../timeline-layout';
import type { TranscriptTimelineSegment } from '../timeline-layout';
import { findChatMessageMatches, findMatchOffsets, isChatFindShortcut, nextFindIndex } from '../chat-find';

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

function Topbar({ chat, onFind }: { chat: Chat | null; onFind: () => void }) {
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
        <button className="tico" title="Buscar en este chat · ⌘F" aria-label="Buscar en este chat" onClick={onFind}><IcSearch /></button>
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

type HighlightRegistryLike = {
  set(name: string, value: unknown): void;
  delete(name: string): boolean;
};

type HighlightConstructor = new (...ranges: Range[]) => unknown;

const FIND_HIGHLIGHT = 'workass-chat-find';
const FIND_CURRENT_HIGHLIGHT = 'workass-chat-find-current';

function highlightRegistry(): HighlightRegistryLike | null {
  if (typeof CSS === 'undefined') return null;
  return (CSS as typeof CSS & { highlights?: HighlightRegistryLike }).highlights ?? null;
}

function highlightConstructor(): HighlightConstructor | null {
  return (globalThis as typeof globalThis & { Highlight?: HighlightConstructor }).Highlight ?? null;
}

function clearFindHighlights(): void {
  const registry = highlightRegistry();
  registry?.delete(FIND_HIGHLIGHT);
  registry?.delete(FIND_CURRENT_HIGHLIGHT);
}

function paintFindHighlights(ranges: Range[], current: number): void {
  const registry = highlightRegistry();
  const HighlightClass = highlightConstructor();
  if (!registry || !HighlightClass) return;
  registry.set(FIND_HIGHLIGHT, new HighlightClass(...ranges));
  if (ranges[current]) registry.set(FIND_CURRENT_HIGHLIGHT, new HighlightClass(ranges[current]));
  else registry.delete(FIND_CURRENT_HIGHLIGHT);
}

function searchableTextNodes(root: Element): Text[] {
  const nodes: Text[] = [];
  const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT, {
    acceptNode(node) {
      const parent = node.parentElement;
      if (!node.textContent || !parent || parent.closest('[hidden], [aria-hidden="true"], script, style')) {
        return NodeFilter.FILTER_REJECT;
      }
      return NodeFilter.FILTER_ACCEPT;
    },
  });
  for (let node = walker.nextNode(); node; node = walker.nextNode()) nodes.push(node as Text);
  return nodes;
}

function rangesInMessage(root: Element, query: string): Range[] {
  const nodes = searchableTextNodes(root);
  if (nodes.length === 0) return [];
  const spans: Array<{ node: Text; start: number; end: number }> = [];
  let text = '';
  for (const node of nodes) {
    const start = text.length;
    text += node.data;
    spans.push({ node, start, end: text.length });
  }
  const ranges: Range[] = [];
  for (const match of findMatchOffsets(text, query)) {
    const first = spans.find((span) => span.end > match.start);
    const last = spans.find((span) => span.end >= match.end);
    if (!first || !last) continue;
    const range = document.createRange();
    range.setStart(first.node, match.start - first.start);
    range.setEnd(last.node, match.end - last.start);
    ranges.push(range);
  }
  return ranges;
}

function transcriptFindRanges(doc: Element, messageId: string, query: string): Range[] {
  const ranges: Range[] = [];
  const message = [...doc.querySelectorAll<HTMLElement>('[data-chat-find-message]')]
    .find((candidate) => candidate.dataset.chatFindMessage === messageId);
  if (!message) return ranges;
  for (const text of message.querySelectorAll('[data-chat-find-text]')) {
    ranges.push(...rangesInMessage(text, query));
  }
  return ranges;
}

function ChatFindBar({
  inputRef, query, count, current, onQuery, onMove, onClose,
}: {
  inputRef: RefObject<HTMLInputElement | null>;
  query: string;
  count: number;
  current: number;
  onQuery: (value: string) => void;
  onMove: (direction: 1 | -1) => void;
  onClose: () => void;
}) {
  return (
    <div className="chatfind" role="search" aria-label="Buscar en el historial del chat">
      <div className="chatfind-box">
        <IcSearch />
        <input
          ref={inputRef}
          value={query}
          placeholder="Buscar en este chat"
          aria-label="Buscar en este chat"
          spellCheck={false}
          onChange={(event) => onQuery(event.target.value)}
          onKeyDown={(event) => {
            if (event.key === 'Enter') { event.preventDefault(); onMove(event.shiftKey ? -1 : 1); }
          }}
        />
        <span className="chatfind-count" aria-live="polite">{count > 0 ? `${current + 1}/${count}` : '0/0'}</span>
        <button disabled={count === 0} title="Coincidencia anterior · ⇧↩" aria-label="Coincidencia anterior" onClick={() => onMove(-1)}>‹</button>
        <button disabled={count === 0} title="Coincidencia siguiente · ↩" aria-label="Coincidencia siguiente" onClick={() => onMove(1)}>›</button>
        <button title="Cerrar búsqueda · Esc" aria-label="Cerrar búsqueda" onClick={onClose}><IcClose /></button>
      </div>
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
    // beforeId is a DROP target and null means "put it last", so pass this
    // chat's current successor: switching folders must not also reorder it.
    const at = app.chats.findIndex((item) => item.id === chat.id);
    const beforeId = app.chats[at + 1]?.id ?? null;
    void store.moveChatToWorkspace(chat.id, beforeId, path).then((moved) => {
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
  const [findOpen, setFindOpen] = useState(false);
  const [findQuery, setFindQuery] = useState('');
  const [findIndex, setFindIndex] = useState(0);
  const findIndexRef = useRef(0);
  const findRangesRef = useRef<Range[]>([]);
  const findInputRef = useRef<HTMLInputElement>(null);
  const findReturnFocusRef = useRef<HTMLElement | null>(null);

  const visibleMessages = chat?.messages.filter((message) => !isWaitingSteerBoundary(message)) ?? [];
  const total = visibleMessages.length;
  const findMatches = findOpen ? findChatMessageMatches(visibleMessages, findQuery) : [];
  const selectedFindMatch = findMatches.length ? findMatches[Math.min(findIndex, findMatches.length - 1)] : undefined;
  const searchStart = selectedFindMatch ? Math.max(0, selectedFindMatch.messageIndex - Math.floor(WINDOW / 2)) : 0;
  const searchEnd = selectedFindMatch ? Math.min(total, searchStart + WINDOW) : 0;
  const shown = selectedFindMatch
    ? visibleMessages.slice(Math.max(0, searchEnd - WINDOW), searchEnd)
    : visibleMessages.slice(Math.max(0, total - reveal));
  const hidden = selectedFindMatch ? 0 : total - shown.length;
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

  // Clamp writes to the real maximum. Programmatic scroll events are ignored by
  // onScroll unless a wheel/touch/scrollbar intent preceded them.
  const setScrollTop = (el: HTMLDivElement, top: number) => {
    const clamped = Math.max(0, Math.min(top, el.scrollHeight - el.clientHeight));
    if (Math.abs(el.scrollTop - clamped) < 1) return;
    el.scrollTop = clamped;
  };

  const openFind = useCallback(() => {
    if (!findOpen) findReturnFocusRef.current = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    setFindOpen(true);
    requestAnimationFrame(() => { findInputRef.current?.focus(); findInputRef.current?.select(); });
  }, [findOpen]);

  const closeFind = useCallback(() => {
    setFindOpen(false);
    setFindQuery('');
    setFindIndex(0);
    findIndexRef.current = 0;
    findRangesRef.current = [];
    clearFindHighlights();
    const target = findReturnFocusRef.current;
    findReturnFocusRef.current = null;
    requestAnimationFrame(() => target?.focus());
  }, []);

  // Capture ⌘F/Ctrl+F before Chromium's page search and before the composer.
  // Escape closes this bar first, so it can never cancel a running turn.
  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      if (isChatFindShortcut(event)) {
        event.preventDefault();
        openFind();
        return;
      }
      if (findOpen && event.key === 'Escape') {
        event.preventDefault();
        event.stopImmediatePropagation();
        closeFind();
      }
    };
    window.addEventListener('keydown', onKey, true);
    return () => window.removeEventListener('keydown', onKey, true);
  }, [closeFind, findOpen, openFind]);

  // Search the whole loaded semantic ledger in memory, but render only the
  // 40-message window containing the selected result. This preserves transcript
  // windowing even for very large chats.
  useEffect(() => {
    const count = findMatches.length;
    const current = count ? Math.min(findIndexRef.current, count - 1) : 0;
    findIndexRef.current = current;
    if (current !== findIndex) setFindIndex(current);
  }, [findIndex, findMatches.length]);

  const scrollToFindRange = useCallback((range: Range | undefined) => {
    const el = scrollRef.current;
    if (!el || !range) return;
    const match = range.getBoundingClientRect();
    const viewport = el.getBoundingClientRect();
    stick.current = false;
    if (match.top < viewport.top + 24 || match.bottom > viewport.bottom - 24) {
      setScrollTop(el, el.scrollTop + match.top - viewport.top - Math.min(120, el.clientHeight * 0.25));
    }
  }, []);

  // Rebuild non-mutating CSS highlights after the selected history window
  // paints, and as a streaming assistant changes text while search stays open.
  useLayoutEffect(() => {
    const doc = docRef.current;
    if (!findOpen || !findQuery || !selectedFindMatch || !doc) {
      findRangesRef.current = [];
      clearFindHighlights();
      return;
    }
    let frame = 0;
    const rebuild = () => {
      frame = 0;
      const ranges = transcriptFindRanges(doc, selectedFindMatch.messageId, findQuery);
      findRangesRef.current = ranges;
      const occurrence = ranges.length ? Math.min(selectedFindMatch.occurrence, ranges.length - 1) : 0;
      paintFindHighlights(ranges, occurrence);
      scrollToFindRange(ranges[occurrence]);
    };
    const schedule = () => { if (!frame) frame = requestAnimationFrame(rebuild); };
    rebuild();
    const observer = new MutationObserver(schedule);
    observer.observe(doc, { childList: true, subtree: true, characterData: true });
    return () => {
      if (frame) cancelAnimationFrame(frame);
      observer.disconnect();
      clearFindHighlights();
    };
  }, [chat?.id, findOpen, findQuery, scrollToFindRange, selectedFindMatch?.messageId, selectedFindMatch?.occurrence]);

  useEffect(() => () => clearFindHighlights(), []);

  const changeFindQuery = (value: string) => {
    findIndexRef.current = 0;
    setFindIndex(0);
    setFindQuery(value);
  };

  const moveFind = (direction: 1 | -1) => {
    const next = nextFindIndex(findIndexRef.current, findMatches.length, direction);
    findIndexRef.current = next;
    setFindIndex(next);
  };

  // Reconcile the new-message counter whenever the total changes.
  useEffect(() => {
    if (stick.current) { seen.current = total; if (newCount !== 0) setNewCount(0); }
    else { const n = Math.max(0, total - seen.current); if (n !== newCount) setNewCount(n); }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [total]);

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
      <Topbar chat={chat} onFind={openFind} />
      {findOpen && (
        <ChatFindBar
          inputRef={findInputRef}
          query={findQuery}
          count={findMatches.length}
          current={findIndex}
          onQuery={changeFindQuery}
          onMove={moveFind}
          onClose={closeFind}
        />
      )}
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
                  return (
                    <div className="chatfind-message" data-chat-find-message={m.id} key={m.id}>
                      {m.role === 'user' ? (
                        <UserPill text={m.content} images={m.images} steerState={m.steerState} />
                      ) : (
                        <AssistantMessage
                          tabId={chat.id}
                          msg={m}
                          turnSeq={store.checkpointForJob(chat, m.jobId)?.turnSeq}
                          profile={profile}
                          coalescedSegments={coalescedSegments.get(m.id)}
                        />
                      )}
                    </div>
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
