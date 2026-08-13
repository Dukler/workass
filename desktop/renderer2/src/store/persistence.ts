// Local-first mirror: the store writes a snapshot to localStorage on every
// change so a reload paints instantly (before state:get / session:get resolves),
// then reconciles with the authoritative server snapshot. Same shape both sides.

import type { Panes, RightPane, ThemePref, Density, Workspace, QueuedMsg, MessageImage, PlanEntry, PermissionState } from './types';
import type { AcpSessionInfo } from '../wire/types';
import type { ModelControlMemory } from '../model-controls';
import type { ContextUsageByProvider } from '../context-usage';

export interface MirrorMsg {
  id: string; role: 'user' | 'assistant'; content: string;
  result?: string;
  status: 'pending' | 'running' | 'done' | 'failed' | 'cancelled'; at: string | null;
  steerState?: 'sending' | 'accepted' | 'applied' | 'uncertain';
  steerBoundary?: 'waiting';
  steerContinuationId?: string;
  steerContinuationFor?: string;
  turnRootId?: string;
  turnTerminal?: boolean;
  jobId?: string;
  permission?: PermissionState;
  turnStartedAt?: number;
  interrupted?: boolean;
  retryPrompt?: string;
  images?: MessageImage[];
  events: unknown[];
}
export interface MirrorChat {
  id: string; chatId?: string; actorRevision?: number; title: string; titleLocked: boolean; group: string | null; cwd: string | null;
  currentModelId: string | null; currentModeId: string | null; draft: string; unread?: boolean;
  settled?: 'settled' | 'active';
  pane?: RightPane | null;      // per-chat right-column occupant (rail/browser/closed)
  modelControls?: ModelControlMemory;
  queue?: QueuedMsg[];
  providerId?: string | null;
  contextUsageByProvider?: ContextUsageByProvider;
  messageCount?: number;
  historyComplete?: boolean;
  workspaceRevision?: number;
  presentationRevision?: number;
  // Daemon-issued CAS token for source:"agent" queue rows. It lets an exact
  // current renderer remove one deliberately while preventing an older save
  // request from recreating a row the daemon already consumed.
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
  v: number; activeId: string | null; seq: number; globalRevision?: number;
  _workassGlobalOperationId?: string;
  // Capability-gated transient marker. The Go daemon strips this before disk;
  // older hosts never receive a lean snapshot.
  _workassSave?: 'lean-payload-v2';
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

// localStorage owns only local view preferences. Chat identity, queues,
// transcripts, provider bindings, and attachments all come from actor state.
export function localMirror(m: Mirror): Mirror {
  const durable: Mirror & Record<string, unknown> = { ...m };
  delete durable._workassSave;
  delete durable._workassDeletedChatIds;
  return {
    ...durable,
    chats: [],
  };
}
