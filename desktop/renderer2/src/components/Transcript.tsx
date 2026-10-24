import { FloatingBrowser } from './FloatingBrowser';
import { useCallback, useEffect, useLayoutEffect, useRef, useState, type PointerEvent, type ReactNode, type RefObject, type WheelEvent } from 'react';
import type { Chat } from '../store/types';
import { store, useApp, useProc, type OlderHistoryOutcome } from '../store/store';
import { chatPane } from '../store/right-pane';
import { AssistantMessage, AssistantTurnBlock, LiveTurnPulse } from './AssistantMessage';
import { UserPill } from './messages';
import { NoticeLine } from './NoticeLine';
import { IcDoc, IcTerminal, IcChanges, IcPreview, IcRail, IcBrowser, IcChevron, IcClose, IcFork, IcSearch } from '../icons';
import { normalizeWorkspacePath, rememberLastProject, workspaceName } from '../workspaces';
import { createScrollSpring, createTranscriptPinScheduler, latestOrdinaryUserMessageId, transcriptPinnedAfterScroll, transcriptReaderLeftUnannounced } from '../transcript-scroll';
import { syncFlowBlocks } from '../flow-window';
import { projectSteeringPresentation } from '../chat/steering-presentation';
import { assistantTurnBlockRanges } from '../timeline-layout';
import { findChatMessageMatches, findMatchOffsets, isChatFindShortcut, nextFindIndex } from '../chat-find';
import { ChatFindQueryContext } from '../markdown/find-context';
import { localBrowserOwnsChat } from '../browser';
import { CHAT_MESSAGE_TAIL } from '../chat/residency';
import { forkStore } from '../store/forks';
import {
  newMessagesSince, recallTranscriptView, recordUnitHeights, rememberTranscriptView, transcriptAnchorAt, transcriptAnchorDepth,
  transcriptRestoredScrollTop, transcriptRestoreStep, transcriptWindow, transcriptWindowStale,
  TRANSCRIPT_LOAD_AHEAD_PX, type MeasuredUnit, type TranscriptAnchor, type TranscriptRestore, type TranscriptView, type TranscriptWindow,
} from '../transcript-window';

const HISTORY_PAGE = 40;
const OLDER_RETRY_MIN_MS = 1000;
const OLDER_RETRY_MAX_MS = 15000;
// A window that keeps re-cutting itself inside one commit is an estimate that
// has not converged; stop there and let the next scroll continue.
const MAX_SYNC_REWINDOWS = 4;
// How long the streaming height hold outlives the last growth (see onResize).
export const HELD_HEIGHT_RELEASE_MS = 700;
const SEARCH_WINDOW = 40;

// Transcript copy/export menu removed by user request (2026-07-11): agents
// read chats through the daemon (archives + Agent API), humans don't paste
// transcripts around. The '\u00b7\u00b7\u00b7' menu returns when it has real options.

// `title` replaces the chat title (a fork subsession shows its breadcrumb
// there); without `onFind` the search button has nothing to search.
export function Topbar({ chat, onFind, title }: { chat: Chat | null; onFind?: () => void; title?: ReactNode }) {
  const app = useApp();
  useProc(); // re-render the bg-process chip as proc:list / proc:changed land
  const pane = chatPane(chat); // this chat's right-column occupant (rail/browser/closed)
  // Workspace chip (user law 2026-07-11, Claude-Code reference): the titlebar
  // reads "<chat title> [workspace]" — ONE quiet chip with the workspace's
  // short name, not a path + group cluster repeating "workass" everywhere.
  const ws = (chat?.cwd ?? app.meta?.workspaceDir ?? '').split('/').filter(Boolean).slice(-1)[0] || null;
  // A subsession names its parent (user 2026-10-16); a forked one carries ⑂
  // before its own title — the only place the fork is shown.
  const parent = !title && chat?.parentChatId ? app.chats.find((c) => c.chatId === chat.parentChatId) : undefined;
  const subsessionTitle = parent && chat ? (
    <span className="crumbs">
      <button type="button" className="c cpar" title="Ir al chat padre" onClick={() => store.switchChat(parent.id)}>{parent.title}</button>
      <span className="sep" aria-hidden="true"><IcChevron /></span>
      {chat.subsessionForked && (
        <span className="fkg" title={`Bifurcada de «${parent.title}»`} aria-label="Subsesión bifurcada"><IcFork /></span>
      )}
      <b title={chat.title}>{chat.title}</b>
    </span>
  ) : chat?.subsessionForked && !title ? (
    // Parent gone: the fork mark is the child's only provenance, keep it.
    <span className="crumbs">
      <span className="fkg" title="Subsesión bifurcada" aria-label="Subsesión bifurcada"><IcFork /></span>
      <b title={chat.title}>{chat.title}</b>
    </span>
  ) : null;
  return (
    <div className="tbar">
      {/* No reopen toggle here: the single sticky .side-toggle (App.tsx) owns
          show/hide in both pane states (user law 2026-07-11). */}
      <IcDoc />
      {title ?? subsessionTitle ?? <b>{chat?.title ?? 'workass'}</b>}
      {ws && <span className="provbadge" title={chat?.cwd ?? 'Workspace'}>{ws}</span>}
      {/* Provider chip removed by user request (2026-07-11) — the bound agent
          is visible in the composer's model selector; the titlebar stays
          title + workspace only. */}
      <span className="sp" />
      {/* Titlebar bg-process chip removed by user request (2026-07-11):
          background processes surface in the Tareas card + inline transcript
          rows only. */}
      <span className="ticons">
        {onFind
          ? <button className="tico" title="Buscar en este chat · ⌘F" aria-label="Buscar en este chat" onClick={onFind}><IcSearch /></button>
          : <button className="tico soon" title="Buscar (no disponible en una subsesión)" aria-label="Buscar" disabled><IcSearch /></button>}
        <button className="tico soon" title="Terminal (próximamente)"><IcTerminal /></button>
        <button className="tico soon" title="Cambios (próximamente)"><IcChanges /></button>
        <button className="tico soon" title="Vista previa (próximamente)"><IcPreview /></button>
        {app.hasBrowserChannels && chat && localBrowserOwnsChat(chat.id, chat.machineId) && (
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
  inputRef, query, count, current, loading, onQuery, onMove, onClose,
}: {
  inputRef: RefObject<HTMLInputElement | null>;
  query: string;
  count: number;
  current: number;
  loading: boolean;
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
        <span className="chatfind-count" aria-live="polite">{loading ? '…' : count > 0 ? `${current + 1}/${count}` : '0/0'}</span>
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

// The first and last elements of a transcript unit that generate a box. Units
// and their message wrappers are display:contents, so the spacing between
// messages (collapsed margins) is exactly what it was before windowing.
function firstBox(parent: Element): Element | null {
  for (const child of parent.children) {
    if (child.getClientRects().length > 0) return child;
    const inner = firstBox(child);
    if (inner) return inner;
  }
  return null;
}

function lastBox(parent: Element): Element | null {
  for (let index = parent.children.length - 1; index >= 0; index--) {
    const child = parent.children[index];
    if (child.getClientRects().length > 0) return child;
    const inner = lastBox(child);
    if (inner) return inner;
  }
  return null;
}

function measureUnits(doc: Element): MeasuredUnit[] {
  const units: MeasuredUnit[] = [];
  for (const node of doc.children) {
    const key = (node as HTMLElement).dataset?.vunit;
    if (!key) continue;
    const first = firstBox(node);
    const last = lastBox(node);
    if (!first || !last) continue;
    units.push({ key, top: first.getBoundingClientRect().top, bottom: last.getBoundingClientRect().bottom });
  }
  return units;
}

// Reading position is anchored on a message, not a unit: a turn block can grow
// at its top when an older page completes it, and its key changes with it.
function measureMessages(doc: Element): MeasuredUnit[] {
  const messages: MeasuredUnit[] = [];
  for (const node of doc.querySelectorAll<HTMLElement>('[data-vunit] [data-chat-find-message]')) {
    const key = node.dataset.chatFindMessage;
    const boxed = node.getClientRects().length > 0;
    const first = boxed ? node : firstBox(node);
    const last = boxed ? node : lastBox(node);
    if (!key || !first || !last) continue;
    messages.push({ key, top: first.getBoundingClientRect().top, bottom: last.getBoundingClientRect().bottom });
  }
  return messages;
}

function prefersReducedMotion(): boolean {
  return typeof window !== 'undefined' && !!window.matchMedia?.('(prefers-reduced-motion: reduce)').matches;
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
  const userScrollDelta = useRef(0);
  const lastScrollTop = useRef(0);
  const [historyLoading, setHistoryLoading] = useState(false);
  const historyLoadingNow = useRef(false);
  // Windowing (transcript-window.ts): which units are mounted is cut around the
  // reading anchor; a tick re-cuts it when the reader nears an edge.
  const [, setWindowTick] = useState(0);
  const heights = useRef<Map<string, number>>(new Map());
  const anchor = useRef<TranscriptAnchor | null>(null);
  const mountedWindow = useRef<TranscriptWindow>({ from: 0, to: 0, topPad: 0, bottomPad: 0 });
  const syncRewindows = useRef(0);
  const viewChat = useRef<string | null>(null);
  // A remembered reading position on its way back (transcript-window.ts). Until
  // its row is loaded and put back at its offset, nothing records over it.
  const restoring = useRef<TranscriptRestore | null>(null);
  const restoreStalled = useRef(false);
  // The rows of the latest render, so leaving can say how deep the reader was.
  const latestMessages = useRef<readonly { id: string }[]>([]);
  const currentView = (): TranscriptView => {
    const pending = restoring.current;
    const held = pending ? pending.anchor : anchor.current;
    const pinned = stick.current || !held;
    const messages = latestMessages.current;
    return {
      pinned,
      anchor: pinned ? null : held,
      heights: heights.current,
      // A position still on its way back keeps what it was remembered with.
      depth: pending ? pending.depth : held ? transcriptAnchorDepth(messages, held.key) : undefined,
      newestId: pending ? pending.newestId : messages[messages.length - 1]?.id ?? null,
    };
  };
  const chatKey = chat?.id ?? '';
  if (viewChat.current !== chatKey) {
    // Leaving a chat remembers how it was read; arriving restores it (pinned
    // chats stay pinned to the newest row). Swapped during render so the first
    // frame of the arriving chat is already cut around its own position.
    if (viewChat.current) rememberTranscriptView(viewChat.current, currentView());
    const view = recallTranscriptView(chatKey);
    stick.current = view.pinned || !view.anchor;
    anchor.current = stick.current ? null : view.anchor;
    heights.current = view.heights;
    restoring.current = anchor.current ? { anchor: anchor.current, depth: view.depth, newestId: view.newestId } : null;
    restoreStalled.current = false;
    viewChat.current = chatKey;
  }
  const [findOpen, setFindOpen] = useState(false);
  const [findQuery, setFindQuery] = useState('');
  const [findIndex, setFindIndex] = useState(0);
  const findIndexRef = useRef(0);
  const findRangesRef = useRef<Range[]>([]);
  const findInputRef = useRef<HTMLInputElement>(null);
  const findReturnFocusRef = useRef<HTMLElement | null>(null);

  // A forked subsession starts with its parent's history folded (user
  // 2026-10-16): inherited rows stay hidden until "Historial heredado" opens.
  const [inheritedOpenFor, setInheritedOpenFor] = useState<string | null>(null);
  const allMessages = projectSteeringPresentation(chat?.messages ?? []).transcriptMessages;
  const inheritedFold = !!chat?.subsessionForked && allMessages.some((message) => message.inherited);
  const inheritedOpen = inheritedFold && inheritedOpenFor === chat?.id;
  const inheritedCount = allMessages.filter((message) => message.inherited).length;
  // Inherited rows are the oldest: once the first loaded row is inherited, the
  // child's own history is fully loaded and only folded history is older.
  const ownHistoryLoaded = inheritedFold && !!allMessages[0]?.inherited;
  const inheritedParent = inheritedFold ? app.chats.find((c) => c.chatId === chat?.parentChatId) : undefined;
  const visibleMessages = inheritedFold && !inheritedOpen
    ? allMessages.filter((message) => !message.inherited)
    : allMessages;
  latestMessages.current = visibleMessages;
  const artifactOrigin = store.browserArtifactMachineId(chat?.machineId);
  const total = visibleMessages.length;
  const knownTotal = chat?.historyComplete === false
    ? Math.max(total, chat.messageCount ?? 0)
    : total;
  const findMatches = findOpen ? findChatMessageMatches(visibleMessages, findQuery) : [];
  const selectedFindMatch = findMatches.length ? findMatches[Math.min(findIndex, findMatches.length - 1)] : undefined;
  const searchStart = selectedFindMatch ? Math.max(0, selectedFindMatch.messageIndex - Math.floor(SEARCH_WINDOW / 2)) : 0;
  const searchEnd = selectedFindMatch ? Math.min(total, searchStart + SEARCH_WINDOW) : 0;
  // Every resident message is part of the transcript; windowing below decides
  // which of them are mounted. A selected search result keeps its own
  // 40-message window, as before.
  const virtual = !selectedFindMatch;
  const shown = selectedFindMatch
    ? visibleMessages.slice(Math.max(0, searchEnd - SEARCH_WINDOW), searchEnd)
    : visibleMessages;
  const hidden = selectedFindMatch || (inheritedFold && !inheritedOpen && ownHistoryLoaded)
    ? 0
    : Math.max(0, knownTotal - shown.length);
  const runningMessage = [...shown].reverse().find((message) => (
    message.role === 'assistant'
    && message.status === 'running'
    && message.turnTerminal !== false
  ));
  // Fork points carry no marker here — the parent transcript never styles
  // where children started (user 2026-10-16). But the data fetch stays: the
  // chat's full fork list is remote-routed while fork:summary is not, so this
  // is the only way a chat owned by another machine ever gets its parent-side
  // subsession chip and nest.
  useEffect(() => {
    if (chat?.id && chat.chatId) forkStore.ensure({ tabId: chat.id, chatId: chat.chatId });
  }, [chat?.id, chat?.chatId]);
  // Units: one message, or one assistant turn block, each with a stable key.
  const turnBlockEnds = new Map<number, number>();
  for (const { start, end } of assistantTurnBlockRanges(shown)) turnBlockEnds.set(start, end);
  const units: Array<{ key: string; start: number; end: number }> = [];
  for (let index = 0; index < shown.length;) {
    const end = turnBlockEnds.get(index);
    if (end != null) {
      units.push({ key: `turn-block:${shown[index].id}`, start: index, end });
      index = end;
    } else {
      units.push({ key: shown[index].id, start: index, end: index + 1 });
      index++;
    }
  }
  const unitKeys = units.map((unit) => unit.key);
  // The anchor names a message; the window is cut around the unit holding it.
  const unitOfMessage = new Map<string, string>();
  for (const unit of units) for (let index = unit.start; index < unit.end; index++) unitOfMessage.set(shown[index].id, unit.key);
  const unitAnchor = (): TranscriptAnchor | null => {
    const held = anchor.current;
    const key = held && !stick.current ? unitOfMessage.get(held.key) : undefined;
    return key ? { key, offset: held!.offset } : null;
  };
  const viewportHeight = scrollRef.current?.clientHeight || (typeof window !== 'undefined' ? window.innerHeight : 800);
  const windowInput = { keys: unitKeys, heights: heights.current, viewport: viewportHeight, anchor: unitAnchor() };
  const win: TranscriptWindow = virtual
    ? transcriptWindow(windowInput)
    : { from: 0, to: units.length, topPad: 0, bottomPad: 0 };
  // While the remembered row is still older than what is loaded, the newest
  // rows are laid out but not shown: revealing them first and then jumping to
  // the remembered row once its page arrives is exactly the jump to avoid.
  const restorePending = virtual && !!restoring.current && !unitOfMessage.has(restoring.current.anchor.key);
  // While a reply streams here, the transcript follows its end with a damped
  // spring (createScrollSpring) instead of snapping a whole line up per delta
  // or bobbing through one smooth scroll per finished sentence; reduced motion
  // and every other moment (sending, switching chats, jumping) pin instantly.
  const streamFollow = useRef(false);
  streamFollow.current = !!runningMessage && !prefersReducedMotion();
  // The tallest the transcript has been during this turn. Half-written
  // markdown can briefly render shorter than what came before (a list marker,
  // a table before its separator row); held as min-height, the page never
  // shrinks under a reader pinned to the bottom, so it never drops and rises.
  const heldHeight = useRef(0);

  // Provider receipts remain canonical in state. The presentation projection
  // keeps live directions in the composer tray and settles them after their
  // assistant turn, so a receipt never inserts a bubble into rendered prose.
  const latestUserMessageId = latestOrdinaryUserMessageId(chat?.messages ?? []);

  // SCROLL CONTRACT — "↓ N mensajes nuevos" pill state. `seen` is the newest
  // message observed while pinned to bottom; when the user scrolls up, new
  // messages accrue after it without shifting the viewport. It is an identity,
  // not a count: older history loading above the reader is not new (a count
  // read a 3,000-row history load as "2990 mensajes nuevos").
  const newestId = visibleMessages[visibleMessages.length - 1]?.id ?? null;
  const seen = useRef<string | null>(newestId);
  const [newCount, setNewCount] = useState(0);
  // The screen-filling load found nothing more for this chat; do not retry it.
  const autoFillStalled = useRef(false);

  // Per-chat transient state resets on a switch. Pinned/anchor position is not
  // reset here: the arriving chat's remembered view was restored during render.
  useEffect(() => {
    autoFillStalled.current = false;
    olderStalledAt.current = -1;
    historyLoadingNow.current = false;
    setHistoryLoading(false);
    userScrollUntil.current = 0;
    seen.current = newestId;
    setNewCount(0);
  }, [chat?.id]);

  // Save the view when the transcript goes away: another chat (each chat mounts
  // its own transcript) or another pane. currentView reads refs only.
  useEffect(() => () => {
    if (viewChat.current) rememberTranscriptView(viewChat.current, currentView());
  }, []);

  // Where the reader is, as the first unit at the viewport's top edge.
  // Two granularities: the message (survives remounts, cuts the window, is
  // remembered per chat) and the exact element under the top edge, which is
  // what the reader is looking at. Both are kept as offsets in content
  // coordinates (viewport offset + scrollTop): scrolling never changes them,
  // so any change is a layout shift above the reader and is undone before
  // paint. This replaces the browser's own scroll anchoring for the
  // transcript (overflow-anchor: none), which a wheel scroll in progress
  // could override while an image above the reader arrived (+491 px).
  const anchorNode = useRef<{ node: Element; contentTop: number } | null>(null);
  const anchorContentTop = useRef<number | null>(null);
  // The exact element is found by a hit test, which on a long message cost
  // milliseconds each and ran on every scroll event, commit and resize. It now
  // runs at most once per frame, and not at all while the element it found
  // still holds the viewport's top edge. Between picks, the held element
  // keeps its own baseline (holdAnchor moves it with every correction).
  const nodePick = useRef<number | null>(null);
  const pickAnchorNode = () => {
    nodePick.current = null;
    const el = scrollRef.current;
    const doc = docRef.current;
    if (!el || !doc || restoring.current || stick.current) return;
    const viewport = el.getBoundingClientRect();
    const line = viewport.top + 1;
    const held = anchorNode.current;
    if (held && held.node.isConnected && doc.contains(held.node)) {
      const box = held.node.getBoundingClientRect();
      if (box.top <= line && box.bottom > line && box.height <= viewport.height) {
        held.contentTop = box.top - viewport.top + el.scrollTop;
        return;
      }
    }
    const hit = typeof document.elementFromPoint === 'function'
      ? document.elementFromPoint(viewport.left + viewport.width / 2, line)
      : null;
    anchorNode.current = hit && hit !== doc && doc.contains(hit) && !hit.classList.contains('vpad')
      ? { node: hit, contentTop: hit.getBoundingClientRect().top - viewport.top + el.scrollTop }
      : null;
  };
  const recordAnchor = () => {
    const el = scrollRef.current;
    const doc = docRef.current;
    // A position on its way back is not where the reader is yet: recording
    // here is what replaced a-00094 with the first mounted row (a-00073).
    if (!el || !doc || restoring.current) return;
    const viewport = el.getBoundingClientRect();
    const next = transcriptAnchorAt(measureMessages(doc), viewport.top);
    if (next) {
      anchor.current = next;
      anchorContentTop.current = next.offset + el.scrollTop;
    }
    if (!anchorNode.current) { pickAnchorNode(); return; }
    if (nodePick.current === null && typeof requestAnimationFrame === 'function') nodePick.current = requestAnimationFrame(pickAnchorNode);
  };
  useEffect(() => () => { if (nodePick.current !== null) cancelAnimationFrame(nodePick.current); }, []);
  const dropAnchor = () => {
    anchorNode.current = null;
    if (nodePick.current !== null) { cancelAnimationFrame(nodePick.current); nodePick.current = null; }
    anchorContentTop.current = null;
    // Following the bottom (a send, the new-messages pill) supersedes it.
    restoring.current = null;
  };

  // Put a remembered reading position back: its row at the offset it was read
  // at. Runs before paint, so the arriving chat's first visible frame is
  // already there. Returns false while the row still has to arrive.
  const restoreReadingPosition = (): boolean => {
    const el = scrollRef.current;
    const doc = docRef.current;
    const pending = restoring.current;
    if (!el || !doc || !pending) return false;
    const step = transcriptRestoreStep({
      messages: visibleMessages, restore: pending, historyComplete: !!chat?.historyComplete,
      loading: historyLoadingNow.current, stalled: restoreStalled.current,
    });
    if (step === 'load' || step === 'wait') return false;
    restoring.current = null;
    if (step === 'abandon') {
      // The row is gone: reopen at the newest row, never at some other row.
      stick.current = true;
      anchor.current = null;
      dropAnchor();
      setWindowTick((tick) => tick + 1);
      setScrollTop(el, el.scrollHeight);
      return true;
    }
    const node = [...doc.querySelectorAll<HTMLElement>('[data-vunit] [data-chat-find-message]')]
      .find((candidate) => candidate.dataset.chatFindMessage === pending.anchor.key);
    const box = node && (node.getClientRects().length > 0 ? node : firstBox(node));
    if (!box) return true; // loaded but not mounted: the ordinary hold takes over
    const viewportTop = el.getBoundingClientRect().top;
    setScrollTop(el, transcriptRestoredScrollTop(el.scrollTop, box.getBoundingClientRect().top, viewportTop, pending.anchor.offset));
    anchor.current = pending.anchor;
    anchorNode.current = null;
    anchorContentTop.current = box.getBoundingClientRect().top - viewportTop + el.scrollTop;
    return true;
  };

  // Undo any layout shift above the reader since the anchor was recorded:
  // prepends, estimates replaced by measurements, window slides, images. A
  // pinned reader follows the bottom instead.
  const holdAnchor = () => {
    const el = scrollRef.current;
    const doc = docRef.current;
    const held = anchor.current;
    if (!el || !doc || stick.current || !held || restoring.current) return;
    const viewportTop = el.getBoundingClientRect().top;
    let delta: number | null = null;
    const exact = anchorNode.current;
    if (exact && exact.node.isConnected && doc.contains(exact.node)) {
      delta = exact.node.getBoundingClientRect().top - viewportTop + el.scrollTop - exact.contentTop;
      // The element moved by delta in the content; that is its new baseline.
      exact.contentTop += delta;
    } else if (anchorContentTop.current !== null) {
      const node = [...doc.querySelectorAll<HTMLElement>('[data-vunit] [data-chat-find-message]')]
        .find((candidate) => candidate.dataset.chatFindMessage === held.key);
      const box = node && (node.getClientRects().length > 0 ? node : firstBox(node));
      if (box) delta = box.getBoundingClientRect().top - viewportTop + el.scrollTop - anchorContentTop.current;
    }
    if (delta !== null && Math.abs(delta) >= 0.5) setScrollTop(el, el.scrollTop + delta);
  };

  // Cut the window again when the reader has moved near an edge of it.
  const rewindowIfStale = () => {
    const el = scrollRef.current;
    if (!el || !virtual) return;
    const input = { keys: unitKeys, heights: heights.current, viewport: el.clientHeight, anchor: unitAnchor() };
    if (transcriptWindowStale(mountedWindow.current, input)) setWindowTick((tick) => tick + 1);
  };

  // Older history arrives before the reader reaches the oldest loaded row;
  // the anchor keeps the prepend from moving anything on screen.
  const olderStalledAt = useRef(-1);
  // After a failed older read, the next automatic one waits 1 s, then 2, 4, 8,
  // at most 15; a read that lands resets it.
  const olderRetry = useRef<{ at: number; delay: number; timer: number | null }>({ at: 0, delay: 0, timer: null });
  const olderRetryPending = () => performance.now() < olderRetry.current.at;
  const noteOlderOutcome = (outcome: OlderHistoryOutcome) => {
    const retry = olderRetry.current;
    if (outcome !== 'failed') { retry.delay = 0; retry.at = 0; return; }
    retry.delay = Math.min(OLDER_RETRY_MAX_MS, Math.max(OLDER_RETRY_MIN_MS, retry.delay * 2));
    retry.at = performance.now() + retry.delay;
    if (retry.timer !== null) window.clearTimeout(retry.timer);
    // Nothing else may re-render a reader who holds still at the top.
    retry.timer = window.setTimeout(() => { retry.timer = null; setWindowTick((tick) => tick + 1); }, retry.delay);
  };
  useEffect(() => () => { if (olderRetry.current.timer !== null) window.clearTimeout(olderRetry.current.timer); }, []);
  // A chat opened with nothing on screen paints its last exchange while the
  // rest of its recent slice is still arriving (store.showsFirstScreenOnly).
  // Older history is read only after that slice, exactly as when the slice
  // painted at once; the store update that completes it renders this again.
  const sliceArriving = (): boolean => !!chat && store.showsFirstScreenOnly(chat.id);
  const loadOlderAhead = () => {
    const el = scrollRef.current;
    if (!el || !virtual || stick.current || restoring.current || hidden <= 0 || historyLoadingNow.current || olderStalledAt.current === total) return;
    if (mountedWindow.current.from > 0 || el.scrollTop > TRANSCRIPT_LOAD_AHEAD_PX || olderRetryPending() || sliceArriving()) return;
    const before = total;
    // Only "nothing older" stops it; a read that failed is tried again.
    void revealOlder().then((outcome) => { if (outcome === 'empty') olderStalledAt.current = before; });
  };

  // Clamp writes to the real maximum. Programmatic scroll events are ignored by
  // onScroll unless a wheel/touch/scrollbar intent preceded them.
  const setScrollTop = (el: HTMLDivElement, top: number) => {
    const clamped = Math.max(0, Math.min(top, el.scrollHeight - el.clientHeight));
    if (Math.abs(el.scrollTop - clamped) >= 1) el.scrollTop = clamped;
    lastScrollTop.current = el.scrollTop;
  };

  const loadCompleteHistory = useCallback(async () => {
    if (!chat || chat.historyComplete) return true;
    setHistoryLoading(true);
    try {
      return await store.loadFullHistory(chat.id);
    } finally {
      setHistoryLoading(false);
    }
  }, [chat?.historyComplete, chat?.id]);

  const openFind = useCallback(() => {
    if (!findOpen) findReturnFocusRef.current = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    // Searching is a new place to read; a position still on its way back yields.
    restoring.current = null;
    setFindOpen(true);
    void loadCompleteHistory();
    requestAnimationFrame(() => { findInputRef.current?.focus(); findInputRef.current?.select(); });
  }, [findOpen, loadCompleteHistory]);

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
      // The match's neighbourhood is mounted in the frame that shows it.
      syncFlowBlocks(el, false);
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

  // Reconcile the new-message counter whenever the newest message changes.
  useEffect(() => {
    if (stick.current) { seen.current = newestId; if (newCount !== 0) setNewCount(0); }
    else {
      const n = newMessagesSince(visibleMessages, seen.current);
      if (n !== newCount) setNewCount(n);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [total, newestId]);

  const markUserScroll = (durationMs = 240, delta = 0) => {
    userScrollUntil.current = performance.now() + durationMs;
    userScrollDelta.current = delta;
    lastScrollTop.current = scrollRef.current?.scrollTop ?? 0;
  };

  // Track whether the user is pinned to the bottom; re-pinning clears the pill.
  const onScroll = () => {
    const el = scrollRef.current;
    if (!el) return;
    // Layout passes while a remembered position is on its way back move
    // nothing the reader sees (the rows are hidden); they record nothing.
    if (restoring.current) { lastScrollTop.current = el.scrollTop; return; }
    const intentSeen = performance.now() <= userScrollUntil.current;
    const unwritten = el.scrollTop - lastScrollTop.current;
    const userInitiated = intentSeen || transcriptReaderLeftUnannounced(stick.current, el, unwritten);
    const delta = (intentSeen && userScrollDelta.current) || unwritten;
    lastScrollTop.current = el.scrollTop;
    const atBottom = transcriptPinnedAfterScroll(stick.current, userInitiated, el, delta);
    const wasPinned = stick.current;
    stick.current = atBottom;
    if (atBottom) { dropAnchor(); seen.current = newestId; if (newCount !== 0) setNewCount(0); }
    else {
      if (!wasPinned) holdAnchor();
      recordAnchor();
    }
    if (wasPinned !== atBottom) setWindowTick((tick) => tick + 1);
    else rewindowIfStale();
    loadOlderAhead();
  };

  const onWheel = (event: WheelEvent<HTMLDivElement>) => {
    if (event.deltaY === 0) return;
    markUserScroll(240, event.deltaY);
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
    dropAnchor();
    seen.current = newestId;
    setNewCount(0);
    setWindowTick((tick) => tick + 1);
    setScrollTop(el, el.scrollHeight);
  };

  // One older page from the actor. The reading anchor keeps the prepend from
  // moving what is on screen. A read that failed (a timeout behind a busy
  // history lane, a race) is retried after a growing pause instead of being
  // taken for the start of the chat.
  const revealOlder = async (page = HISTORY_PAGE): Promise<OlderHistoryOutcome> => {
    if (!chat || chat.historyComplete) return 'empty';
    if (historyLoadingNow.current) return 'failed';
    historyLoadingNow.current = true;
    setHistoryLoading(true);
    const chatID = chat.id;
    try {
      const outcome: OlderHistoryOutcome = total === 0
        ? (await store.loadRecentHistory(chat.id, CHAT_MESSAGE_TAIL) ? 'loaded' : 'failed')
        : await store.loadOlderHistoryPage(chat.id, page);
      if (viewChat.current === chatID) noteOlderOutcome(outcome);
      return outcome;
    } finally {
      if (viewChat.current === chatID) {
        historyLoadingNow.current = false;
        setHistoryLoading(false);
      }
    }
  };

  // Sending is an explicit request to follow the new turn. Re-pin on the newest
  // user-message id even when the assistant placeholder is appended in the same
  // store update (so the user message is not necessarily the final array item).
  // Arriving at another chat is not a send: that chat's own view is restored.
  const followed = useRef<{ chat: string; id: string | null }>({ chat: '', id: null });
  useLayoutEffect(() => {
    const previous = followed.current;
    followed.current = { chat: chatKey, id: latestUserMessageId };
    const el = scrollRef.current;
    if (!el || !latestUserMessageId) return;
    if (previous.chat !== chatKey && !stick.current) return;
    userScrollUntil.current = 0;
    stick.current = true;
    dropAnchor();
    seen.current = newestId;
    setNewCount(0);
    setWindowTick((tick) => tick + 1);
    setScrollTop(el, el.scrollHeight);
  }, [chat?.id, latestUserMessageId]);

  // After every commit: measure the mounted units and hold the reading anchor
  // before the browser paints. A pinned reader is followed by the observers
  // below (instantly, or by the streaming spring), exactly as before.
  useLayoutEffect(() => {
    const el = scrollRef.current;
    const doc = docRef.current;
    if (!el || !doc) return;
    const scrolledFrom = el.scrollTop;
    commitLayout(doc);
    // Windows inside messages (galleries) follow a scroll this commit made or
    // content it moved before the frame paints, not one frame later.
    syncFlowBlocks(el, Math.abs(el.scrollTop - scrolledFrom) >= 1);
  });
  const commitLayout = (doc: HTMLDivElement) => {
    mountedWindow.current = win;
    recordUnitHeights(heights.current, measureUnits(doc));
    if (!virtual) { recordAnchor(); return; }
    if (restoring.current) {
      // Arriving at a chat that was left mid-read: its row goes back exactly
      // where it was. A row older than what is loaded keeps the restore
      // pending (and the rows hidden) while the effect below loads pages.
      restoreReadingPosition();
    } else if (!stick.current && anchor.current && !unitOfMessage.has(anchor.current.key)) {
      // A reading position whose row is no longer loaded reopens at the
      // newest row, never at some other row near the top.
      stick.current = true;
      anchor.current = null;
      dropAnchor();
      setWindowTick((tick) => tick + 1);
    } else if (!stick.current) { holdAnchor(); recordAnchor(); }
    // Re-cuts inside one frame are estimates converging; cap them per frame.
    if (syncRewindows.current++ === 0) requestAnimationFrame(() => { syncRewindows.current = 0; });
    if (syncRewindows.current <= MAX_SYNC_REWINDOWS) rewindowIfStale();
    loadOlderAhead();
  };

  // A window shorter than the screen left a band of nothing under the newest
  // row while older messages sat behind "Ver anteriores" (user 2026-10-15).
  // Show more until the screen is full: first what is already loaded, then
  // older history from disk, exactly as "Ver anteriores" would — filling only
  // from memory left the band whenever the older rows were still on disk
  // (user 2026-10-15). It only grows while there is nothing to scroll, so it
  // never moves what is read; a load that brings nothing stops it for the chat.
  useLayoutEffect(() => {
    const doc = docRef.current;
    const el = scrollRef.current;
    if (!doc || !el || !chat || selectedFindMatch || doc.offsetHeight >= el.clientHeight) return;
    if (hidden <= 0 || historyLoadingNow.current || chat.historyComplete || autoFillStalled.current || olderRetryPending() || sliceArriving()) return;
    const chatID = chat.id;
    const before = chat.messages.length;
    void revealOlder().then((outcome) => {
      if (outcome !== 'failed' && (store.chat(chatID)?.messages.length ?? 0) <= before) autoFillStalled.current = true;
    });
  });

  // A remembered row older than the loaded history: load older pages, the
  // largest the actor serves, until it is present (the layout effect then puts
  // it back) or it proves gone. A passive effect, so it runs after the
  // per-chat reset above has settled the loading flags of this mount.
  useEffect(() => {
    const pending = restoring.current;
    if (!pending || !virtual || !chat) return;
    const step = transcriptRestoreStep({
      messages: visibleMessages, restore: pending, historyComplete: !!chat.historyComplete,
      loading: historyLoadingNow.current, stalled: restoreStalled.current,
    });
    if (step !== 'load' || olderRetryPending() || sliceArriving()) return;
    void revealOlder(CHAT_MESSAGE_TAIL).then((outcome) => {
      if (restoring.current !== pending) return;
      // Only "nothing older" proves the row gone; a failed read is retried.
      if (outcome === 'empty') restoreStalled.current = true;
      // Re-run the restore even when the store had nothing to publish.
      setWindowTick((tick) => tick + 1);
    });
  });

  // A turn's held height lasts exactly as long as the turn.
  useLayoutEffect(() => {
    heldHeight.current = 0;
    docRef.current?.style.removeProperty('min-height');
  }, [chat?.id, runningMessage?.id]);

  // Pin to bottom whenever content grows (streaming, new cards) while stuck.
  // A resize pins inside the ResizeObserver callback: it runs after layout and
  // before paint, so a grown transcript is never painted at the old offset
  // first (the one-frame drop-then-snap that read as a bounce while text
  // streamed). Mutations still coalesce into one pre-paint animation frame. Chromium
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
      pin: () => follow(),
      requestFrame: (callback) => requestAnimationFrame(callback),
      cancelFrame: (handle) => cancelAnimationFrame(handle),
      setTimer: (callback, delayMs) => window.setTimeout(callback, delayMs),
      clearTimer: (handle) => window.clearTimeout(handle),
    });
    const spring = createScrollSpring({
      el,
      isPinned: () => stick.current,
      requestFrame: (callback) => requestAnimationFrame(callback),
      cancelFrame: (handle) => cancelAnimationFrame(handle),
    });
    function follow() {
      if (!el) return;
      if (streamFollow.current) {
        spring.kick();
        return;
      }
      spring.stop();
      setScrollTop(el, el.scrollHeight);
    }
    // The hold only bridges a momentary dip while text streams. Kept for the
    // whole turn, any real shrink (a thought line stepping aside, a live row
    // ending) stayed on screen as a band of nothing under the newest line for
    // as long as a long-running turn lasted (user 2026-10-15). Once the
    // transcript has stopped growing for a moment, the space goes back.
    let holdRelease: number | null = null;
    const releaseHoldSoon = () => {
      if (holdRelease !== null) window.clearTimeout(holdRelease);
      holdRelease = window.setTimeout(() => {
        holdRelease = null;
        heldHeight.current = 0;
        doc?.style.removeProperty('min-height');
      }, HELD_HEIGHT_RELEASE_MS);
    };
    const onResize = () => {
      if (streamFollow.current && doc) {
        const height = doc.offsetHeight;
        if (height > heldHeight.current) {
          heldHeight.current = height;
          doc.style.minHeight = `${height}px`;
          releaseHoldSoon();
        }
      }
      // Growth that is not a commit (an image arriving) is held here too.
      if (!stick.current) {
        holdAnchor();
        recordAnchor();
        return;
      }
      scheduler.dispose();
      follow();
    };
    const mo = new MutationObserver(scheduler.schedule);
    mo.observe(doc, { childList: true, subtree: true, characterData: true });
    const ro = new ResizeObserver(onResize);
    ro.observe(doc);
    ro.observe(el); // the composer growing/shrinking resizes the viewport, not the doc — re-pin then too
    const reconcile = () => scheduler.reconcile();
    document.addEventListener('visibilitychange', reconcile);
    window.addEventListener('focus', reconcile);
    window.addEventListener('pageshow', reconcile);
    scheduler.reconcile();
    return () => {
      if (holdRelease !== null) window.clearTimeout(holdRelease);
      scheduler.dispose();
      spring.stop();
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
          loading={historyLoading}
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
          <ChatFindQueryContext.Provider value={findOpen ? findQuery : ''}>
          <div className="doc" ref={docRef} style={restorePending ? { visibility: 'hidden' } : undefined} aria-busy={restorePending || undefined}>
            {inheritedFold && chat && (
              <button
                type="button" className="inh" aria-expanded={inheritedOpen}
                title={inheritedOpen ? 'Ocultar el historial heredado' : 'Mostrar el historial heredado'}
                onClick={() => setInheritedOpenFor(inheritedOpen ? null : chat.id)}
              >
                <span className="ig" aria-hidden="true"><IcFork /></span>
                <span className="it">Historial heredado</span>
                <span className="im">
                  {chat.historyComplete === false ? '' : `· ${inheritedCount} ${inheritedCount === 1 ? 'mensaje' : 'mensajes'} `}
                  {inheritedParent ? `de «${inheritedParent.title}»` : ''}
                </span>
                <span className="rule" aria-hidden="true" />
              </button>
            )}
            {!chat || total === 0 ? (
              inheritedFold ? null : <EmptyChat chat={chat} />
            ) : (
              <>
                {hidden > 0 && (
                  <button className="btn" style={{ margin: '4px auto 16px', display: 'block' }} disabled={historyLoading} onClick={() => { void revealOlder(); }}>
                    {historyLoading
                      ? 'Cargando historial…'
                      : `Ver ${Math.min(HISTORY_PAGE, hidden)} mensajes anteriores`}
                  </button>
                )}
                {win.topPad > 0 && <div className="vpad" style={{ height: win.topPad }} aria-hidden="true" />}
                {units.slice(win.from, win.to).map((unit) => {
                  const index = unit.start;
                  const m = shown[index];
                  const midRun = (at: number) => shown[at]?.role === 'assistant' && shown[at + 1]?.role === 'assistant';
                  let body: ReactNode;
                  if (unit.end - unit.start > 1) {
                    const messages = shown.slice(unit.start, unit.end);
                    const block = (
                      <AssistantTurnBlock
                        tabId={chat.id}
                        messages={messages}
                        turnSeqs={messages.map((message) => store.checkpointForJob(chat, message.jobId)?.turnSeq)}
                        artifactOrigin={artifactOrigin}
                        midRun={messages.map((_, offset) => midRun(index + offset))}
                      />
                    );
                    body = inheritedFold && messages[0].inherited ? <div className="inh-msg">{block}</div> : block;
                  } else {
                    body = (
                      <div className={`chatfind-message${inheritedFold && m.inherited ? ' inh-msg' : ''}`} data-chat-find-message={m.id}>
                        {m.role === 'user' ? (
                          m.notice
                            ? <NoticeLine notice={m.notice} />
                            : <UserPill tabId={chat.id} text={m.content} images={m.images} steerState={m.steerState} />
                        ) : (
                          <AssistantMessage
                            tabId={chat.id}
                            msg={m}
                            turnSeq={store.checkpointForJob(chat, m.jobId)?.turnSeq}
                            artifactOrigin={artifactOrigin}
                            midRun={midRun(index)}
                          />
                        )}
                      </div>
                    );
                  }
                  return <div className="vunit" data-vunit={unit.key} key={unit.key}>{body}</div>;
                })}
                {win.bottomPad > 0 && <div className="vpad" style={{ height: win.bottomPad }} aria-hidden="true" />}
              </>
            )}
          </div>
          </ChatFindQueryContext.Provider>
        </div>
        {runningMessage && <LiveTurnPulse msg={runningMessage} />}
        {newCount > 0 && (
          <button className={`newpill${runningMessage ? ' with-live' : ''}`} onClick={jumpToBottom}>
            <span className="arr">↓</span>{newCount} {newCount === 1 ? 'mensaje nuevo' : 'mensajes nuevos'}
          </button>
        )}
        {chat && <FloatingBrowser chat={chat} />}
      </div>
    </>
  );
}
