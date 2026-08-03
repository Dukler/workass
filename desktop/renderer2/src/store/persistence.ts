// Local-first mirror: the store writes a snapshot to localStorage on every
// change so a reload paints instantly (before state:get / session:get resolves),
// then reconciles with the authoritative server snapshot. Same shape both sides.

import type { Panes, RightPane, ThemePref, Density, Workspace, QueuedMsg, MessageImage, SteerAnchor, PlanEntry } from './types';
import type { AcpSessionInfo } from '../wire/types';
import type { ModelControlMemory } from '../model-controls';
import type { ContextUsageByProvider, ContextUsageSnapshot } from '../context-usage';

export interface MirrorMsg {
  id: string; role: 'user' | 'assistant'; content: string;
  result?: string;
  status: 'pending' | 'running' | 'done' | 'failed' | 'cancelled'; at: string | null;
  steerState?: 'sending' | 'accepted' | 'applied' | 'uncertain';
  steerAnchor?: SteerAnchor;
  steerBoundary?: 'waiting';
  steerContinuationId?: string;
  steerContinuationFor?: string;
  turnRootId?: string;
  turnTerminal?: boolean;
  jobId?: string;
  images?: MessageImage[];
  events: unknown[];
}
export interface MirrorChat {
  id: string; chatId?: string; title: string; titleLocked: boolean; group: string | null; cwd: string | null;
  currentModelId: string | null; currentModeId: string | null; draft: string; unread?: boolean;
  settled?: 'settled' | 'active';
  pane?: RightPane | null;      // per-chat right-column occupant (rail/browser/closed)
  modelControls?: ModelControlMemory;
  queue?: QueuedMsg[];
  queued?: string;              // legacy single-blob queue; read-only, migrated to `queue` on load
  providerId?: string | null;
  contextUsageByProvider?: ContextUsageByProvider;
  usage?: ContextUsageSnapshot; // legacy single-provider snapshot; migrated on load
  workspaceRevision?: number;
  // Daemon-issued CAS token for source:"agent" queue rows. It lets an exact
  // current renderer remove one deliberately while preventing an older save
  // request from resurrecting a row the daemon already consumed.
  agentQueueRevision?: number;
  runtimeControlRevision?: number;
  // The chat's current agent plan (the rail's step-by-step). A plan update only
  // arrives when the agent edits its todo list, so the authoring message ages out
  // of the retained event window — this chat-level snapshot is what survives a
  // reload. An explicit [] means "no current plan" and must not be treated as
  // absent. Tiny and O(1): never scan historical events to rebuild it.
  planLatest?: PlanEntry[];
  planLatestMessageId?: string;
  // Runtime-only overlay supplied by session:get. saveMirror/toMirror never
  // writes it: durable provider-native ids belong to the daemon's separate
  // native-session ledger, while this object describes the current live bridge.
  liveSession?: AcpSessionInfo;
  messages: MirrorMsg[];
}
export interface Mirror {
  v: number; activeId: string | null; seq: number;
  // Capability-gated transient marker. The Go daemon strips this before disk;
  // older hosts never receive a lean snapshot.
  _workassSave?: 'lean-payload-v2';
  // Capability-gated and transient: only exact ids explicitly closed by the
  // user may be removed from the daemon-owned session snapshot.
  _workassDeletedChatIds?: string[];
  workspaces?: Workspace[];
  collapsedWorkspaces?: string[];
  removedWorkspaces?: string[];
  theme: 'dark' | 'light'; themePref?: ThemePref; density?: Density;
  panes: Panes; mode: 'assist' | 'chats';
  notifEnabled?: boolean;
  // NOTE: user-authored per-model scores (Settings · Modelos) deliberately do NOT
  // ride this session mirror. They persist through the daemon app-settings blob
  // (settings:get / settings:set) so the agent-facing catalog reads the same
  // authority — never localStorage and never the session mirror.
  chats: MirrorChat[];
}

const KEY = 'workass.renderer2.session.v1';
export const LEAN_SESSION_SAVE_MODE = 'lean-payload-v2' as const;

// The daemon owns full tool payloads. A lean save carries only renderer-owned
// timeline rows plus the tiny tool metadata that the renderer stamps locally;
// the capability-aware daemon overlays these fields onto its exact-ID events.
export function leanSessionEvents(events: readonly unknown[]): unknown[] {
  const out: unknown[] = [];
  for (const raw of events) {
    if (!raw || typeof raw !== 'object') continue;
    const event = raw as Record<string, unknown>;
    if (event.kind === 'thinking' || event.kind === 'bgproc') continue;
    if (event.kind !== 'tool') {
      out.push(raw);
      continue;
    }
    const overlay: Record<string, unknown> = { kind: 'tool' };
    for (const key of ['key', 'at', 'id', 'terminalId', 'startedAt', 'endedAt', 'subagentModel']) {
      if (event[key] !== undefined) overlay[key] = event[key];
    }
    out.push(overlay);
  }
  return out;
}

// Lean session saves may omit an image payload only after the daemon has
// confirmed a durable write containing that exact payload. Track the image
// arrays themselves (rather than row ids): queue/message ids are stable while
// an attachment can be replaced during retry, and a replacement must receive
// its own first full save.
export class DurableImagePayloads {
  private acknowledged = new WeakSet<MessageImage[]>();

  acknowledgeImages(images: MessageImage[] | undefined): void {
    if (Array.isArray(images) && images.length) this.acknowledged.add(images);
  }

  acknowledge(snapshot: Mirror, saved: boolean): void {
    if (!saved) return;
    for (const chat of snapshot.chats) {
      // session:get can contain an old or daemon-authored empty chat whose
      // `messages` field is absent. The ordinary mirror hydrator already
      // normalizes that shape to [], so this image-durability prepass must be
      // equally tolerant instead of aborting the entire renderer startup.
      for (const message of Array.isArray(chat.messages) ? chat.messages : []) {
        this.acknowledgeImages(message.images);
      }
      for (const item of Array.isArray(chat.queue) ? chat.queue : []) {
        this.acknowledgeImages(item.images);
      }
    }
  }

  replaceFromServer(snapshot: Mirror): void {
    this.acknowledged = new WeakSet<MessageImage[]>();
    // A session:get payload is direct proof that these bytes already exist in
    // the daemon-owned durable mirror, equivalent to a successful save ack.
    this.acknowledge(snapshot, true);
  }

  clear(): void {
    this.acknowledged = new WeakSet<MessageImage[]>();
  }

  omitAcknowledged(snapshot: Mirror): Mirror {
    return {
      ...snapshot,
      chats: snapshot.chats.map((chat) => ({
        ...chat,
        queue: Array.isArray(chat.queue)
          ? chat.queue.map((item) => this.withoutAcknowledgedImages(item))
          : undefined,
        messages: (Array.isArray(chat.messages) ? chat.messages : [])
          .map((message) => this.withoutAcknowledgedImages(message)),
      })),
    };
  }

  private withoutAcknowledgedImages<T extends { images?: MessageImage[] }>(row: T): T {
    if (!row.images?.length || !this.acknowledged.has(row.images)) return row;
    const { images: _images, ...lean } = row;
    return lean as T;
  }
}

export function normalizeQueued(value: unknown): string | undefined {
  return typeof value === 'string' && value.length > 0 ? value : undefined;
}

export function loadMirror(): Mirror | null {
  try {
    const raw = localStorage.getItem(KEY);
    if (!raw) return null;
    const m = JSON.parse(raw) as Mirror;
    if (!m || !Array.isArray(m.chats)) return null;
    return m;
  } catch { return null; }
}

export function saveMirror(m: Mirror): void {
  try { localStorage.setItem(KEY, JSON.stringify(localMirror(m))); } catch { /* quota / disabled — non-fatal */ }
}

// First-paint skeletons: the local mirror used to strip ALL events, so every
// reload first-painted the transcript without tool/thinking rows and hydration
// visibly popped them in a beat later. Keep a tiny skeleton of the ACTIVE
// chat's visible tail — the scroll is pinned to the bottom on reload, so rows
// hydrated above the fold never move the viewport. A skeleton carries the same
// key and the same collapsed-line text as the daemon copy (which is redacted
// at ingestion), never the heavy input/output/image payloads; enrichment is
// therefore invisible. Everything else still saves event-less to keep
// keystrokes off a multi-MiB JSON.stringify/localStorage path.
const SKELETON_MESSAGES = 10;
const SKELETON_EVENTS = 80;

function capped(value: unknown, max: number): string | null {
  return typeof value === 'string' && value ? value.slice(0, max) : null;
}

export function skeletonEvents(events: readonly unknown[]): unknown[] {
  const out: unknown[] = [];
  for (const raw of events.slice(-SKELETON_EVENTS)) {
    if (!raw || typeof raw !== 'object') continue;
    const event = raw as Record<string, unknown>;
    if (event.kind === 'bgproc') continue; // transient, never persisted
    if (event.kind === 'tool') {
      const lean: Record<string, unknown> = {
        kind: 'tool', input: null, output: null,
        title: capped(event.title, 160) ?? '',
        command: capped(event.command, 200),
        location: capped(event.location, 200),
      };
      for (const key of ['key', 'at', 'id', 'toolKind', 'status', 'terminalId', 'startedAt', 'endedAt', 'subagentId', 'subagentProvider', 'subagentHeader', 'subagentModel'] as const) {
        if (event[key] !== undefined) lean[key] = event[key];
      }
      if (event.subagentLabel !== undefined) lean.subagentLabel = capped(event.subagentLabel, 160);
      out.push(lean);
    } else if (event.kind === 'thinking') {
      out.push({ kind: 'thinking', key: event.key, at: event.at, text: capped(event.text, 240) ?? '' });
    } else {
      out.push(raw); // plan/compaction/restored rows are already small
    }
  }
  return out;
}

// Screenshots belong in the daemon-owned session mirror, not Chromium's small
// localStorage budget. The local mirror still paints text instantly; images
// reappear when session:get hydrates from the daemon a moment later.
export function localMirror(m: Mirror): Mirror {
  const {
    _workassSave: _saveMode,
    _workassDeletedChatIds: _deletedChatIds,
    ...durable
  } = m;
  return {
    ...durable,
    chats: m.chats.map((chat) => ({
      ...chat,
      // Queue screenshots follow the same policy as sent screenshots: keep
      // their metadata/text in localStorage, but let the daemon-owned mirror
      // carry the large base64 payload across renderer reloads.
      queue: Array.isArray(chat.queue)
        ? chat.queue.map(({ images, draftImages: _draftImages, ...item }) => ({
          ...item,
          // A localStorage row whose daemon-owned image bytes were intentionally
          // omitted must never look dispatchable during the hydration gap.
          attachmentState: images?.length ? 'preparing' : item.attachmentState,
        }))
        : undefined,
      messages: (Array.isArray(chat.messages) ? chat.messages : [])
        .map(({ images: _images, ...message }, index, all) => ({
        ...message,
        // Tool/subagent payloads dominate large sessions and are restored from
        // the daemon/archive after first paint. Keeping them in synchronous
        // localStorage turns an unrelated keystroke into a multi-megabyte stall.
        events: chat.id === m.activeId && index >= all.length - SKELETON_MESSAGES
          ? skeletonEvents(Array.isArray(message.events) ? message.events : [])
        // Blanked, not filtered: scanning every historical event here cost more
        // than the 8ms first-paint budget. The chat's step-by-step survives on
        // `chat.planLatest` instead, which is O(1) and tiny.
          : [],
        })),
    })),
  };
}
