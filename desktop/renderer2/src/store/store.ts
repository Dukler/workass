// Central store: topic-based pub/sub over a mutable state tree.
//
// Perf design (PORT-SPEC): token streaming bumps ONLY `msg:<id>`, so the
// streaming assistant message re-renders in isolation; its markdown blocks are
// memoized so only the tail block actually reconciles. Structural changes
// (message list, running state, tabs, catalog) bump `app`.

import { useSyncExternalStore } from 'react';
import type { AppState, Chat, Msg, ToolEvent, ThemePref, Density, SettingsSection, Toast, DraftImage, QueuedMsg, PlanEntry } from './types';
import type { JobEvent, PublicJob, AcpEvent, PermissionRequest, PermissionResolved, ChatCatalog, ChatCompacted, ModelOption, ModeOption, ProcChanged, AccessRequest, ProcessSummary, CatalogGroup, ChatCheckpoint, CheckpointRestored, EngineRecovered, NotifyEvent, NotifyBacklog, AgentApply, ChatCommandsEvent, ChatStructuredError, StartJobOpts, PlanUsageSnapshot, ProviderRecord, ProvidersUpdates, ProviderUpdateProgress, AppUpdate, ProviderUpdate, SpawnedWorkChanged, SpawnedWorkItem, SpawnedWorkRead, SpawnedWorkStop, ChatEnvPayload, StateDigest, StateDigestChat } from '../wire/types';
import { call, callThrow, has, on, bridgeReady } from '../wire/api';
import { ConnectionMonitor, type ConnStatus } from '../wire/connection';
import { LEAN_SESSION_SAVE_MODE, loadMirror, saveMirror, type Mirror, type MirrorMsg } from './persistence';
import { browserApi } from '../browser';
import {
  afterQueuedAcceptance, appendDraftImages, attachmentWorkBoundary, draftImagePayloads, mergeMessageImages, messageImages,
  queuedAttachmentsReady, queuedDraftMessage, queuedJob, queuedMessage, releaseDraftImages,
  shouldDrainRecoveredQueue, withoutDraftImages,
} from '../image-drafts';
import { acceptPendingSteer, commitChronologicalSteer, hasSteerConsumptionReceipt, insertChronologicalSteer, markPendingSteerUncertain, rejectChronologicalSteer, settlePendingSteer, settleSendingSteersAtTurnEnd, settleStagedSteersAtTurnEnd, stageChronologicalSteer, SteeringDispatchLane, steeringBehavior, steeringDestination, steeringStagesBoundary } from '../steering';
import { chooseWorkspacePath, inheritChatControls, normalizeWorkspacePath, normalizeWorkspaces, rememberLastProject, workspaceFromPath } from '../workspaces';
import { isAutoTurnEndNotice } from '../notifications';
import { WorkspaceMoveGate, workspaceMoveAccepted, workspaceRebindSupported } from '../workspace-move';
import { chatPane, nextPane, type RightPane } from './right-pane';
import { actorMessages } from '../chat/history';
import { preserveUnchangedFullHistories, releaseInactiveHistories } from '../chat/residency';
import { redactSensitiveText } from '../redact';
import { recoverRememberedCatalogModel, resolveModelSelection } from '../model-selection';
import { adoptsTerminalJobResult, appendAssistantChunk } from '../assistant-output.ts';
import {
  contextUsageIdentityMatches,
  normalizeContextUsage,
  normalizeContextUsageByProvider,
  withContextUsage,
} from '../context-usage';
import {
  compatibleEffortId, compatibleModeId, composeModelSelection,
  imageDraftCapability,
  modelControlsChangedDuringInit, nextModelControlRevision,
  normalizeModelControlMemory, rememberModelControls, rememberedModelControls,
  restoredControlSelection, restoredProviderBinding,
} from '../model-controls';
import {
  clearAllModelScores, clearModelScore, countScoredModels,
  modelScoresFromSettings, settingsFromReply, withModelScoresInSettings,
  withNoteValue, withScoreValue, type ScoreDimension,
} from '../model-scores';
import {
  modelFavoritesFromSettings, toggleModelFavorite, withModelFavoritesInSettings,
} from '../model-favorites';
import { userFacingCatalogGroups, userFacingProviders, workassRuntimeProfile } from '../model-catalog';
import { clearPermissionById, clearPermissionsOutsideSnapshot } from '../permissions';
import { isQueuedJobStart, reconcileQueuedJobStart } from './busy-start';
import { settleTerminalToolEvents } from '../terminal-tool-reconciliation';
import { MachineRegistry, type MachineEntry } from '../wire/machineRegistry';
import { normalizeMachineNickname } from '../machine-nickname';
import { setMachineRouter } from '../wire/api';
import { tagId, tagPayload } from '../wire/machineIds';

const APP = 'app';
const PROC = 'proc';
const SPAWNED = 'spawned';
// Dedicated topic for the per-chat Entorno changed-file snapshot so the rail
// card repaints on chat:env without dragging the whole app into a re-render.
const ENV = 'env';
const DEV = 'dev';
// Dedicated topic for connection transitions so the banner and per-turn retry
// affordances can subscribe without coupling to every APP bump (which fires on
// token streaming) — connection changes are rare, so a broad re-render is cheap.
const CONN = 'conn';
// Dedicated topic for agent activity (tool/plan events). The Tareas rail
// subscribes to it so live tool calls + plan progress repaint in isolation,
// without dragging the whole app into every ACP event (token streaming stays on
// `msg:<id>`; structural turn start/end already bump APP).
const ACT = 'act';

const isToolDone = (s: string) => s === 'completed' || s === 'success';
const isToolFailed = (s: string) => s === 'failed' || s === 'error';
const HYDRATION_STEP_TIMEOUT = 15000;
const AGENT_REFRESH_TIMEOUT = 20000;
const DIGEST_SYNC_DEBOUNCE = 250;
const UPDATE_TERMINAL_TIMEOUT = 11 * 60 * 1000;

type SyncScope = 'session' | 'background' | 'permissions' | 'catalog' | 'settings' | 'processes';

type ChatPresentationSnapshot = Pick<Chat, 'title' | 'titleLocked' | 'group' | 'draft' | 'unread' | 'settled' | 'settledAt' | 'pane'>;

function presentationSnapshot(chat: ChatPresentationSnapshot): ChatPresentationSnapshot {
  return {
    title: chat.title,
    titleLocked: chat.titleLocked,
    group: chat.group,
    draft: chat.draft ?? '',
    unread: !!chat.unread,
    settled: chat.settled,
    settledAt: chat.settledAt,
    pane: chat.pane,
  };
}

function applyPresentationSnapshot(chat: Chat, snapshot: ChatPresentationSnapshot) {
  chat.title = snapshot.title;
  chat.titleLocked = snapshot.titleLocked;
  chat.group = snapshot.group;
  chat.draft = snapshot.draft;
  chat.unread = snapshot.unread;
  chat.settled = snapshot.settled;
  chat.settledAt = snapshot.settledAt;
  chat.pane = snapshot.pane;
}

function presentationFingerprint(chat: Pick<Chat, 'title' | 'titleLocked' | 'group' | 'draft' | 'unread' | 'settled' | 'settledAt' | 'pane'>): string {
  return JSON.stringify([
    chat.title, chat.titleLocked, chat.group ?? null, chat.draft ?? '', !!chat.unread,
    chat.settled ?? '', chat.settledAt ?? 0, chat.pane ?? null,
  ]);
}

function runtimeControlsFingerprint(chat: Pick<Chat, 'providerId' | 'currentModelId' | 'currentModeId' | 'modelControls'>): string {
  return JSON.stringify([
    chat.providerId ?? null, chat.currentModelId ?? null, chat.currentModeId ?? null,
    normalizeModelControlMemory(chat.modelControls) ?? null,
  ]);
}

function globalPresentationFingerprint(value: Pick<AppState, 'activeId' | 'seq' | 'workspaces' | 'collapsedWorkspaces' | 'removedWorkspaces' | 'theme' | 'themePref' | 'density' | 'panes' | 'mode' | 'notifEnabled'>): string {
  return JSON.stringify([
    value.activeId, value.seq, value.workspaces, value.collapsedWorkspaces, value.removedWorkspaces,
    value.theme, value.themePref, value.density, value.panes, value.mode, value.notifEnabled,
  ]);
}

export class Store {
  state: AppState;
  private versions = new Map<string, number>();
  private listeners = new Map<string, Set<() => void>>();
  private jobRef = new Map<string, { tabId: string; msgId: string }>(); // jobId -> exact chat/message
  // Live turn pulses keyed by job id — transient by design: never persisted,
  // dropped at job end. The transcript tail row reads them for its vitals.
  private turnHeartbeats = new Map<string, { elapsedMs: number; outputTokens: number; phase: string; toolName?: string; retry?: { code?: number; attempt?: number } | null; at: number }>();
  private chatJobs = new Map<string, { tabId: string; msgId: string }>(); // chatId -> {tabId,msgId}
  private drainingQueues = new Set<string>();
  // Preserve submission order for rapid explicit steers. Persistence and native
  // acknowledgement are part of the same lane, so a slower first RPC cannot let
  // a later direction reach Codex (or FIFO) ahead of it.
  private steerDispatches = new SteeringDispatchLane();
  private workspaceMoves = new WorkspaceMoveGate();
  private saveTimer: ReturnType<typeof setTimeout> | null = null;
  // The local mirror is a first-paint cache, not the durable copy, so its write
  // is coalesced: a keystroke used to serialize every hydrated chat and hand
  // Chromium a megabyte synchronously, per character.
  private mirrorTimer: ReturnType<typeof setTimeout> | null = null;
  // remote-plan E3. Null until a machine book with something in it exists, and
  // that is the safety property: with no machines mounted the router is never
  // installed and every call takes exactly the path it took before.
  private machines: MachineRegistry | null = null;
  private selfMachineId = '';
  private machineNameMap: Record<string, string> = {};
  // Full actor history loads per chat, on demand. Session snapshots carry only
  // a bounded tail; these sets track complete read projections in memory.
  private fullHistoriesLoaded = new Set<string>();
  private fullHistoryLoads = new Map<string, Promise<void>>();
  // Chat creation is an immediate actor command. Keep only the optimistic row
  // mounted until the authoritative snapshot echoes its id; the operation id
  // and in-flight promise ensure every later draft/queue/session operation waits
  // for that durable receipt rather than treating React as chat authority.
  private pendingChatCreates = new Set<string>();
  // A remote chat:create receipt is durable, but an older session:get may
  // already be in flight. Keep that confirmed optimistic row until a remote
  // snapshot echoes it, without causing ensureChatCreated to send it twice.
  private remoteChatCreateFences = new Set<string>();
  private pendingChatCreateOperations = new Map<string, { operationId: string; focus: boolean }>();
  private pendingChatCreatePromises = new Map<string, Promise<boolean>>();
  // A delete is a durable tombstone command, not a UI removal. Keep its one
  // logical operation id until the actor returns the matching receipt: a lost
  // reply must be retried as the same delete, because the actor rejects a new
  // operation id once the tombstone exists.
  private pendingChatDeleteOperations = new Map<string, { chatId: string; operationId: string }>();
  // Lean v2 saves are chat deltas. Track exact tab ids plus a per-id mutation
  // revision so an acknowledgement cannot clear a second edit that arrived
  // while the first snapshot was crossing the wire.
  private dirtyChats = new Set<string>();
  private dirtyChatVersions = new Map<string, number>();
  private dirtyChatRevision = 0;
  // A queue edit is user-owned state, but its renderer save can overlap the
  // periodic daemon digest. Keep the latest local queue projection fenced until
  // the save containing that exact mutation is acknowledged; otherwise a digest
  // can hydrate the previous server snapshot and make the row disappear.
  private queueMutationRevision = 0;
  private pendingQueueMutationVersions = new Map<string, number>();
  private pendingQueueSnapshots = new Map<string, QueuedMsg[] | undefined>();
  private pendingQueueOperationIds = new Map<string, string>();
  private committedPresentationFingerprints = new Map<string, string>();
  private pendingPresentationOperations = new Map<string, { fingerprint: string; operationId: string }>();
  // A presentation command can overlap the daemon's periodic digest. Keep the
  // exact renderer-owned projection beside its stable operation id until that
  // command is acknowledged, or an older actor snapshot can visibly undo a
  // settle/rename/pane click and leave the retry with nothing left to send.
  private pendingPresentationSnapshots = new Map<string, ChatPresentationSnapshot>();
  private committedRuntimeControlFingerprints = new Map<string, string>();
  private pendingRuntimeControlOperations = new Map<string, { fingerprint: string; operationId: string }>();
  private pendingRuntimeControlSaves = new Map<string, Promise<boolean>>();
  private committedGlobalPresentationFingerprint = '';
  private pendingGlobalPresentationOperation: { fingerprint: string; operationId: string } | null = null;
  // Draft edits have the same renderer-versus-digest race as queue edits. In
  // particular, submission clears the composer before archive/session work is
  // allowed to await; a digest carrying the pre-send text must not put that
  // already-owned prompt back into the box while its save is still in flight.
  private draftMutationRevision = 0;
  private pendingDraftMutationVersions = new Map<string, number>();
  private pendingDraftSnapshots = new Map<string, string>();
  // Membership/order changes and hydration boundaries must send one complete
  // tree. The revision protects a new force-full request from an older save's
  // acknowledgement in the same way dirtyChatVersions protects chat edits.
  private fullSavePending = true;
  private fullSaveRevision = 0;
  // Separate debounce for model preference writes. Scores and favorites persist
  // to the daemon app-settings blob (settings:set), not the session mirror, so
  // they share one coalescing timer independent of schedulePersist.
  private settingsSaveTimer: ReturnType<typeof setTimeout> | null = null;
  private monitor?: ConnectionMonitor;
  private agentRefresh: Promise<void> = Promise.resolve();
  private sessionHydrationPending = false;
  private digestUnsupported = false;
  private digestProbe: Promise<void> | null = null;
  private syncScopes = new Set<SyncScope>();
  private syncTimer: ReturnType<typeof setTimeout> | null = null;
  private localCatalogHashes: Record<string, string> | null = null;
  private localSettingsRevision: string | null = null;
  private localProcHash: string | null = null;
  private catalogHashEpoch = 0;
  private settingsHashEpoch = 0;
  private procHashEpoch = 0;
  private resolvedPermissionIds = new Set<string>();
  // One shared wall-clock ticker for relative-time stamps ("hace N min"). It runs
  // only while something is actually subscribed (settled turn stamps on screen),
  // so an idle app holds no timer. Without it, memoized turn stamps freeze at
  // their first render and every turn perpetually reads "hace unos segundos".
  private clockTimer: ReturnType<typeof setInterval> | null = null;

  constructor() {
    const mirror = loadMirror();
    this.state = this.fromMirror(mirror);
    for (const chat of this.state.chats) {
      this.committedPresentationFingerprints.set(chat.id, presentationFingerprint(chat));
    }
    this.committedGlobalPresentationFingerprint = globalPresentationFingerprint(this.state);
    this.markAllChatsDirty();
    this.rebuildJobRefs();
  }

  // ---- subscription plumbing -------------------------------------------
  subscribe(topic: string, cb: () => void): () => void {
    let set = this.listeners.get(topic);
    if (!set) { set = new Set(); this.listeners.set(topic, set); }
    set.add(cb);
    return () => { set!.delete(cb); };
  }
  // Relative-time stamps subscribe here so they re-render as the clock advances.
  // A single one-second interval is lazily started with the first subscriber and
  // cleared when the last one leaves. Only the leaf stamp subscribes, so visible
  // elapsed seconds stay live without repainting message bodies.
  subscribeClock(cb: () => void): () => void {
    const unsub = this.subscribe('clock', cb);
    if (!this.clockTimer) this.clockTimer = setInterval(() => this.bump('clock'), 1_000);
    return () => {
      unsub();
      const set = this.listeners.get('clock');
      if ((!set || set.size === 0) && this.clockTimer) { clearInterval(this.clockTimer); this.clockTimer = null; }
    };
  }
  version(topic: string): number { return this.versions.get(topic) ?? 0; }
  bump(topic: string): void {
    this.versions.set(topic, this.version(topic) + 1);
    const set = this.listeners.get(topic);
    if (set) set.forEach((cb) => cb());
  }
  private bumpApp(persist = true) { this.bump(APP); if (persist) this.schedulePersist(); }
  private touchChat(tabId: string) {
    if (!tabId) return;
    this.dirtyChats.add(tabId);
    this.dirtyChatVersions.set(tabId, ++this.dirtyChatRevision);
  }
  private markQueueMutation(chat: Pick<Chat, 'id' | 'queue'>) {
    const version = ++this.queueMutationRevision;
    this.pendingQueueMutationVersions.set(chat.id, version);
    // Queue rows are already renderer-owned objects. Keep the array projection
    // rather than copying image/File payloads; every later queue mutation calls
    // this method again and receives a fresh projection.
    this.pendingQueueSnapshots.set(chat.id, chat.queue);
    this.pendingQueueOperationIds.set(chat.id, rid('queue-op'));
  }
  private markPresentationMutation(chat: Chat) {
    const fingerprint = presentationFingerprint(chat);
    const pending = this.pendingPresentationOperations.get(chat.id);
    if (!pending || pending.fingerprint !== fingerprint) {
      this.pendingPresentationOperations.set(chat.id, { fingerprint, operationId: rid('presentation-op') });
    }
    this.pendingPresentationSnapshots.set(chat.id, presentationSnapshot(chat));
  }
  private scheduleQueuePersist() {
    // A running turn normally relaxes persistence to 3s to avoid mirroring its
    // streaming transcript. Queue ownership is different: the user just made a
    // durable FIFO decision, so it must beat the 5s state-digest sync window.
    this.schedulePersist(250);
  }
  private markAllChatsDirty() {
    for (const chat of this.state.chats) this.touchChat(chat.id);
  }
  private releaseInactiveHistories(activeId: string | null) {
    for (const chatId of releaseInactiveHistories(this.state.chats, activeId)) this.fullHistoriesLoaded.delete(chatId);
  }
  private preserveUnchangedFullHistories(previous: Chat[], restored: Chat[]) {
    for (const chatId of preserveUnchangedFullHistories(previous, restored)) this.fullHistoriesLoaded.add(chatId);
  }
  private requireFullSave() {
    this.markAllChatsDirty();
    this.fullSavePending = true;
    this.fullSaveRevision += 1;
  }
  private bumpChat(chat: Pick<Chat, 'id'> | string, persist = true) {
    if (persist) this.touchChat(typeof chat === 'string' ? chat : chat.id);
    this.bumpApp(persist);
  }

  // ---- lookups ---------------------------------------------------------
  active(): Chat | null { return this.state.chats.find((c) => c.id === this.state.activeId) ?? null; }
  chat(id: string): Chat | null { return this.state.chats.find((c) => c.id === id) ?? null; }
  heartbeatFor(jobId: string | null | undefined) { return jobId ? this.turnHeartbeats.get(jobId) ?? null : null; }
  isChatRunning(chatId: string | null): boolean {
    const chat = chatId ? this.chat(chatId) : this.active();
    return !!chat && chat.messages.some((m) => m.status === 'running');
  }

  private providerGroup(providerId: string | null | undefined): CatalogGroup | undefined {
    return providerId ? this.state.groups.find((group) => group.providerId === providerId) : undefined;
  }

  private reconcileCatalogControls(): boolean {
    let controlsChanged = false;
    for (const chat of this.state.chats) {
      if (chat.providerId && !chat.providerName) chat.providerName = this.providerName(chat.providerId);
      const group = this.providerGroup(chat.providerId);
      if (!group || !chat.currentModelId) continue;
      let selected = resolveModelSelection([group], group.models, chat.currentModelId);
      if (!selected.model) {
        // This renderer has an explicit picker write that the stale/replayed
        // catalog cannot yet describe. Preserve it until a fresh catalog arrives;
        // remembered-model recovery is for stored ids, not live picks.
        if ((chat._controlRevision ?? 0) > 0) continue;
        const providerMemory = chat.modelControls?.[group.providerId] ?? {};
        const recovered = recoverRememberedCatalogModel(
          group.models,
          chat.currentModelId,
          Object.keys(providerMemory),
          group.providerId,
        );
        if (recovered) {
          const remembered = rememberedModelControls(chat.modelControls, group.providerId, recovered.modelId);
          const recoveredEffort = compatibleEffortId(remembered?.effort, recovered);
          chat.currentModelId = composeModelSelection(recovered.modelId, recoveredEffort);
          selected = resolveModelSelection([group], group.models, chat.currentModelId);
          controlsChanged = true;
        }
      }
      if (!selected.base) continue;
      const compatibleModel = selected.model ? composeModelSelection(selected.base, selected.effort) : chat.currentModelId;
      if (chat.currentModelId !== compatibleModel) {
        chat.currentModelId = compatibleModel;
        controlsChanged = true;
      }
      const compatibleMode = compatibleModeId(chat.currentModeId, group.modes);
      const previousControls = rememberedModelControls(chat.modelControls, group.providerId, selected.base);
      if (chat.currentModeId !== compatibleMode) {
        chat.currentModeId = compatibleMode;
        controlsChanged = true;
      }
      chat.modelControls = rememberModelControls(chat.modelControls, group.providerId, selected.base, {
        effort: selected.effort ?? undefined,
        modeId: compatibleMode ?? undefined,
      });
      if ((!previousControls && (selected.effort || compatibleMode))
        || (selected.effort && previousControls?.effort !== selected.effort)
        || (compatibleMode && previousControls?.modeId !== compatibleMode)) controlsChanged = true;
    }
    return controlsChanged;
  }

  private rememberCurrentControls(chat: Chat): { effort: string | null; modeId: string | null } {
    const group = this.providerGroup(chat.providerId);
    const selected = resolveModelSelection(group ? [group] : this.state.groups, group?.models ?? this.state.models, chat.currentModelId);
    if (chat.providerId && selected.base) {
      chat.modelControls = rememberModelControls(chat.modelControls, chat.providerId, selected.base, {
        effort: selected.effort ?? undefined,
        modeId: chat.currentModeId ?? undefined,
      });
    }
    return { effort: selected.effort, modeId: chat.currentModeId };
  }

  private applyControlsForModel(
    chat: Chat,
    providerId: string,
    baseModelId: string,
    models: ModelOption[],
    modes: ModeOption[],
    options: {
      fallbackEffort?: string | null;
      fallbackMode?: string | null;
      providerDefaultMode?: string | null;
      explicitEffort?: string | null;
    } = {},
  ): boolean {
    const previousModel = chat.currentModelId;
    const previousMode = chat.currentModeId;
    const model = models.find((candidate) => candidate.modelId === baseModelId) ?? null;
    const remembered = rememberedModelControls(chat.modelControls, providerId, baseModelId);
    const effort = options.explicitEffort != null
      ? compatibleEffortId(options.explicitEffort, model)
      : compatibleEffortId(remembered?.effort, model, options.fallbackEffort);
    const requestedMode = remembered?.modeId ?? options.fallbackMode;
    const modeId = modes.length
      ? compatibleModeId(requestedMode, modes, options.providerDefaultMode)
      : (requestedMode ?? options.providerDefaultMode ?? null);

    chat.currentModelId = composeModelSelection(baseModelId, effort);
    chat.currentModeId = modeId;
    chat.modelControls = rememberModelControls(chat.modelControls, providerId, baseModelId, {
      effort: effort ?? undefined,
      modeId: modeId ?? undefined,
    });
    return chat.currentModelId !== previousModel || chat.currentModeId !== previousMode;
  }

  // ---- persistence -----------------------------------------------------
  private toMirror(lean = false, onlyDirty?: ReadonlySet<string>): Mirror {
    // A chat that lives on another machine is that machine's to persist. This
    // mirror is written to the LOCAL daemon, so letting a remote chat through
    // would copy it into this machine's session store and it would come back
    // after a restart as a local chat pointing at a repo that is not here
    // (remote-plan E3).
    const owned = this.state.chats.filter((chat) => !chat.machineId);
    const chats = onlyDirty && lean
      ? owned.filter((chat) => onlyDirty.has(chat.id))
      : owned;
    const snapshot: Mirror = {
      v: 1,
      _workassSave: lean ? LEAN_SESSION_SAVE_MODE : undefined,
      activeId: this.state.activeId,
      seq: this.state.seq,
      globalRevision: this.state.globalRevision,
      workspaces: this.state.workspaces,
      collapsedWorkspaces: this.state.collapsedWorkspaces,
      removedWorkspaces: this.state.removedWorkspaces,
      theme: this.state.theme,
      themePref: this.state.themePref,
      density: this.state.density,
      panes: this.state.panes,
      mode: this.state.mode,
      notifEnabled: this.state.notifEnabled,
      // modelScores are NOT mirrored here — they persist through the daemon
      // app-settings blob (settings:get / settings:set), hydrated separately.
      chats: chats.map((c) => ({
        id: c.id, chatId: c.chatId, actorRevision: c.actorRevision, title: c.title, titleLocked: c.titleLocked, group: c.group, cwd: c.cwd,
        workspaceRevision: c.workspaceRevision,
        presentationRevision: c.presentationRevision,
        agentQueueRevision: c.agentQueueRevision,
        runtimeControlRevision: c.runtimeControlRevision,
        planLatest: c.planLatest,
        planLatestMessageId: c.planLatestMessageId,
        currentModelId: c.currentModelId, currentModeId: c.currentModeId, modelControls: c.modelControls,
        pane: c.pane,
        draft: c.draft, unread: c.unread, settled: c.settled, settledAt: c.settledAt,
        lastActivityAt: c.lastActivityAt,
        queue: c.queue?.length ? c.queue.map(({ draftImages: _draftImages, ...item }) => item) : undefined,
        providerId: c.providerId ?? null,
        contextUsageByProvider: c.contextUsageByProvider,
        messageCount: c.messageCount ?? c.messages.length,
        historyComplete: c.historyComplete ?? true,
        // Actor commands persist every chat mutation. session:save carries only
        // global presentation state, so transcript rows never cross this path.
        messages: [],
      })),
    };
    return snapshot;
  }

  private serverSnapshot(lean: boolean): {
    snapshot: Mirror;
    full: boolean;
    fullRevision: number;
  } {
    const full = !lean || this.fullSavePending;
    const onlyDirty = lean && !full ? new Set(this.dirtyChats) : undefined;
    return {
      snapshot: this.toMirror(lean, onlyDirty),
      full,
      fullRevision: this.fullSaveRevision,
    };
  }

  private async saveServerSnapshot(
    snapshot: Mirror,
    full: boolean,
    fullRevision: number,
  ): Promise<void> {
    const sentDirtyVersions = new Map<string, number>();
    const sentQueueVersions = new Map<string, number>();
    const sentQueueOperations = new Map<string, string>();
    const sentDraftVersions = new Map<string, number>();
    for (const chat of snapshot.chats) {
      if (!this.dirtyChats.has(chat.id)) continue;
      sentDirtyVersions.set(chat.id, this.dirtyChatVersions.get(chat.id) ?? 0);
      const queueVersion = this.pendingQueueMutationVersions.get(chat.id);
      if (queueVersion !== undefined) {
        sentQueueVersions.set(chat.id, queueVersion);
        const operationId = this.pendingQueueOperationIds.get(chat.id);
        if (operationId) sentQueueOperations.set(chat.id, operationId);
      }
      const draftVersion = this.pendingDraftMutationVersions.get(chat.id);
      if (draftVersion !== undefined) sentDraftVersions.set(chat.id, draftVersion);
    }
    const failedCreateTabs = new Set<string>();
    const failedQueueTabs = new Set<string>();
    for (const projected of snapshot.chats) {
      if (!this.pendingChatCreates.has(projected.id)) continue;
      const live = this.chat(projected.id);
      if (!live || !(await this.ensureChatCreated(live))) failedCreateTabs.add(projected.id);
    }
    for (const [tabId, version] of sentQueueVersions) {
      const operationId = sentQueueOperations.get(tabId);
      const projected = snapshot.chats.find((chat) => chat.id === tabId);
      const live = this.chat(tabId);
      if (!operationId || !projected?.chatId || !live || failedCreateTabs.has(tabId)) continue;
      const receipt = await call('chatQueueReplace', {
        tabId, chatId: projected.chatId, operationId,
        expectedRevision: live.agentQueueRevision ?? 0,
        queue: projected.queue ?? [],
      });
      if (!receipt?.ok || receipt.operationId !== operationId) {
        failedQueueTabs.add(tabId);
        this.scheduleScopedSync(['session']);
        continue;
      }
      const current = this.chat(tabId);
      if (current && current.chatId === projected.chatId) {
        current.agentQueueRevision = receipt.agentQueueRevision;
        current.actorRevision = receipt.actorRevision;
      }
      if (this.pendingQueueMutationVersions.get(tabId) !== version || this.pendingQueueOperationIds.get(tabId) !== operationId) continue;
      this.pendingQueueMutationVersions.delete(tabId);
      this.pendingQueueSnapshots.delete(tabId);
      this.pendingQueueOperationIds.delete(tabId);
    }
    const failedPresentationTabs = new Set<string>();
    for (const [tabId] of sentDirtyVersions) {
      const projected = snapshot.chats.find((chat) => chat.id === tabId);
      const live = this.chat(tabId);
      if (!projected?.chatId || !live || failedCreateTabs.has(tabId)) {
        if (failedCreateTabs.has(tabId)) failedPresentationTabs.add(tabId);
        continue;
      }
      const fingerprint = presentationFingerprint(live);
      if (this.committedPresentationFingerprints.get(tabId) === fingerprint) continue;
      let pending = this.pendingPresentationOperations.get(tabId);
      if (!pending || pending.fingerprint !== fingerprint) {
        pending = { fingerprint, operationId: rid('presentation-op') };
        this.pendingPresentationOperations.set(tabId, pending);
      }
      this.pendingPresentationSnapshots.set(tabId, presentationSnapshot(live));
      const receipt = await call('chatPresentationSave', {
        tabId, chatId: projected.chatId, operationId: pending.operationId,
        expectedRevision: live.presentationRevision ?? 0,
        title: live.title, titleLocked: live.titleLocked, group: live.group,
        draft: live.draft ?? '', unread: !!live.unread, settled: live.settled ?? '', settledAt: live.settledAt ?? 0, pane: live.pane ?? null,
      });
      if (!receipt?.ok || receipt.operationId !== pending.operationId) {
        failedPresentationTabs.add(tabId);
        this.scheduleScopedSync(['session']);
        continue;
      }
      const current = this.chat(tabId);
      if (current && current.chatId === projected.chatId) {
        current.presentationRevision = receipt.presentationRevision;
        current.actorRevision = receipt.actorRevision;
      }
      this.committedPresentationFingerprints.set(tabId, pending.fingerprint);
      if (this.pendingPresentationOperations.get(tabId)?.operationId === pending.operationId) {
        this.pendingPresentationOperations.delete(tabId);
        this.pendingPresentationSnapshots.delete(tabId);
      }
    }
    const globalFingerprint = globalPresentationFingerprint(this.state);
    const globalChanged = this.committedGlobalPresentationFingerprint !== globalFingerprint;
    if (!this.pendingGlobalPresentationOperation || this.pendingGlobalPresentationOperation.fingerprint !== globalFingerprint) {
      this.pendingGlobalPresentationOperation = { fingerprint: globalFingerprint, operationId: rid(globalChanged ? 'global-op' : 'global-noop') };
    }
    snapshot.globalRevision = this.state.globalRevision;
    snapshot._workassGlobalOperationId = this.pendingGlobalPresentationOperation.operationId;
    const saved = await call('saveSession', snapshot);
    const savedOK = saved === true || (!!saved && typeof saved === 'object' && saved.ok === true);
    if (saved && typeof saved === 'object' && saved.ok === true) this.state.globalRevision = saved.globalRevision;
    if (savedOK && this.pendingGlobalPresentationOperation?.fingerprint === globalFingerprint) {
      this.committedGlobalPresentationFingerprint = globalFingerprint;
      this.pendingGlobalPresentationOperation = null;
    }
    if (savedOK) {
      for (const [id, version] of sentDirtyVersions) {
        if (failedPresentationTabs.has(id) || failedQueueTabs.has(id)) continue;
        if (this.dirtyChatVersions.get(id) !== version) continue;
        this.dirtyChats.delete(id);
        this.dirtyChatVersions.delete(id);
      }
      for (const [id, version] of sentDraftVersions) {
        if (this.pendingDraftMutationVersions.get(id) !== version) continue;
        this.pendingDraftMutationVersions.delete(id);
        this.pendingDraftSnapshots.delete(id);
      }
      if (full && this.fullSaveRevision === fullRevision) this.fullSavePending = false;
    }
    if (failedPresentationTabs.size || failedQueueTabs.size || [...sentQueueVersions].some(([id, version]) => this.pendingQueueMutationVersions.get(id) === version)) {
      this.schedulePersist(250);
    }
  }

  private fromMirror(m: Mirror | null): AppState {
    const base: AppState = {
      chats: [], workspaces: [], collapsedWorkspaces: [], removedWorkspaces: [], activeId: null, seq: 0, globalRevision: 0, models: [], modes: [], groups: [], providers: [], modelScores: {}, modelFavorites: [], planUsageByProvider: {}, planUsageLoadingByProvider: {},
      theme: prefersLight() ? 'light' : 'dark',
      themePref: 'system', density: 'compact',
      panes: { side: true, railWide: false, sideW: 288, railW: 312 },
      mode: 'chats', hydrated: false, connection: 'connected', hasBrowserChannels: false,
      processes: [], hasProcChannels: false, spawnedWorkByChat: {}, obligationByChat: {}, hasSpawnedWorkChannels: false, chatEnvByChat: {},
      settingsOpen: false, settingsSection: 'agentes', commandBarOpen: false,
      machines: [], hasMachineChannels: false, hasFleetKey: false,
      fleetKeys: [], fleetCanReveal: false, hasFleetKeyChannels: false,
      hasDeviceChannels: false, devices: [], accessRequests: [],
      hasConfigChannel: false,
      rewind: { open: false, tabId: null, chatId: null, loading: false, items: [] },
      review: { open: false, tabId: null, chatId: null, loading: false, repos: [], diffLoading: false },
      notifEnabled: false,
      notifPermission: notifPermission(),
      toasts: [],
      providersUpdates: [],
      updateProgress: {},
    };
    if (!m) return base;
    base.workspaces = normalizeWorkspaces(m.workspaces);
    base.collapsedWorkspaces = Array.isArray(m.collapsedWorkspaces) ? m.collapsedWorkspaces.filter((p) => typeof p === 'string') : [];
    base.removedWorkspaces = Array.isArray(m.removedWorkspaces) ? m.removedWorkspaces.filter((p): p is string => typeof p === 'string').map(normalizeWorkspacePath) : [];
    base.activeId = m.activeId ?? null;
    base.seq = m.seq ?? 0;
    base.globalRevision = Number.isInteger(m.globalRevision) ? m.globalRevision ?? 0 : 0;
    base.notifEnabled = !!m.notifEnabled;
    // Model scores/favorites are hydrated from the daemon app-settings blob
    // (hydrateSettings), not this session mirror, so defaults stay untouched.
    // themePref is authoritative; a resolved theme remains a valid explicit
    // preference when no system-mode choice was stored.
    base.themePref = m.themePref ?? (m.theme ? m.theme : 'system');
    base.theme = resolveTheme(base.themePref);
    if (m.density) base.density = m.density;
    // Merge so older mirrors (which lack the pane widths) keep the defaults. The
    // old global rail/browser booleans (a source of the cross-chat hijack) are
    // deliberately dropped here — right-column occupancy is now per-chat (`pane`).
    if (m.panes) base.panes = { ...base.panes, ...m.panes };
    if (m.mode) base.mode = m.mode;
    base.chats = (m.chats ?? []).map((c): Chat => {
      const binding = restoredProviderBinding(c.providerId, c.liveSession?.providerId);
      const providerId = binding.selectedProviderId;
      const selectedLiveSession = binding.useLiveControls ? c.liveSession : undefined;
      const controls = restoredControlSelection(c.currentModelId, c.currentModeId, selectedLiveSession ? {
        modelId: selectedLiveSession.currentModelId,
        modeId: selectedLiveSession.currentModeId,
      } : null);
      let modelControls = normalizeModelControlMemory(c.modelControls);
      const liveSelection = selectedLiveSession
        ? resolveModelSelection([], selectedLiveSession.models ?? [], controls.modelId)
        : null;
      if (providerId && liveSelection?.base) {
        modelControls = rememberModelControls(modelControls, providerId, liveSelection.base, {
          effort: liveSelection.effort ?? undefined,
          modeId: controls.modeId ?? undefined,
        });
      }
      const contextUsageByProvider = normalizeContextUsageByProvider(c.contextUsageByProvider);
      const messages = actorMessages(Array.isArray(c.messages) ? c.messages : []);
      const messageCount = Number.isInteger(c.messageCount) && (c.messageCount ?? 0) >= messages.length
        ? c.messageCount!
        : messages.length;
      return {
        id: c.id, chatId: c.chatId ?? newChatConvId(), actorRevision: Number.isInteger(c.actorRevision) ? c.actorRevision : 0, sessionId: c.liveSession?.sessionId ?? null,
        sessionProviderId: binding.sessionProviderId,
        title: c.title, titleLocked: !!c.titleLocked,
        group: c.group ?? null, cwd: c.liveSession?.cwd ?? c.cwd ?? null,
        workspaceRevision: Number.isInteger(c.workspaceRevision) ? c.workspaceRevision : 0,
        presentationRevision: Number.isInteger(c.presentationRevision) ? c.presentationRevision : 0,
        agentQueueRevision: Number.isInteger(c.agentQueueRevision) ? c.agentQueueRevision : 0,
        runtimeControlRevision: Number.isInteger(c.runtimeControlRevision) ? c.runtimeControlRevision : 0,
        // An explicit [] means "no current plan" and must survive as [], not be
        // collapsed to absent — otherwise a stale scan could restore an old plan.
        planLatest: Array.isArray(c.planLatest)
          ? c.planLatest.map((entry) => ({
            status: String((entry as PlanEntry)?.status ?? ''), content: String((entry as PlanEntry)?.content ?? ''),
          }))
          : undefined,
        planLatestMessageId: typeof c.planLatestMessageId === 'string' ? c.planLatestMessageId : undefined,
        currentModelId: controls.modelId,
        currentModeId: controls.modeId,
        modelControls,
        providerId, providerName: binding.useLiveControls ? c.liveSession?.providerName ?? null : null,
        contextUsageByProvider,
        pane: c.pane,
        imageSupport: c.liveSession?.imageSupport ?? false,
        commands: selectedLiveSession?.commands?.length ? selectedLiveSession.commands : undefined,
        // Absent = UNKNOWN, never a wipe: the snapshot cannot carry the
        // daemon-memory-only catalog, so a rebuild keeps what this client
        // already learned (chat:commands / commands-get). Optional-chained on
        // state: the constructor's first fromMirror runs before state exists,
        // and a plain this.chat() here blanked the whole window (2026-07-28).
        commandCatalog: selectedLiveSession?.commandCatalog
          ?? this.state?.chats?.find((prior) => prior.id === c.id)?.commandCatalog
          ?? undefined,
        pending: !c.liveSession?.sessionId, draft: c.draft ?? '', unread: c.unread, settled: c.settled,
        settledAt: Number.isFinite(c.settledAt) && (c.settledAt ?? 0) > 0 ? c.settledAt : undefined,
        lastActivityAt: Number.isFinite(c.lastActivityAt) && (c.lastActivityAt ?? 0) > 0 ? c.lastActivityAt : undefined,
        queue: c.queue?.map((item: QueuedMsg) => item.attachmentState === 'preparing'
            ? { ...item, attachmentState: 'failed', attachmentError: 'La preparación se interrumpió; volvé a adjuntar las imágenes.' }
            : item),
        messages,
        messageCount,
        historyComplete: c.historyComplete !== false && messageCount === messages.length,
      };
    });
    if (base.chats.length && !base.chats.find((c) => c.id === base.activeId)) base.activeId = base.chats[0].id;
    return base;
  }
  // localStorage owns only local view preferences; coalescing still avoids a
  // synchronous browser write per keystroke.
  private scheduleLocalMirror() {
    if (this.mirrorTimer) return;
    this.mirrorTimer = setTimeout(() => {
      this.mirrorTimer = null;
      saveMirror(this.toMirror(true));
    }, 250);
  }

  // Identity/structure changes must survive an immediate restart, so they write
  // through instead of waiting for the coalescing window.
  private writeLocalMirrorNow() {
    if (this.mirrorTimer) { clearTimeout(this.mirrorTimer); this.mirrorTimer = null; }
    saveMirror(this.toMirror(true));
  }

  // Session hydration carries a bounded tail. Opening a chat reads one complete
  // projection from the actor and replaces that tail directly.
  private async ensureFullHistory(chatId: string): Promise<void> {
    if (!has('archiveLoad') || !chatId) return;
    if (this.fullHistoriesLoaded.has(chatId)) return;
    const inflight = this.fullHistoryLoads.get(chatId);
    if (inflight) return inflight;
    const load = (async () => {
      let projected: unknown;
      try {
        const step = await this.guardedStep(`actor history (${chatId})`, () => call('archiveLoad', chatId));
        projected = step.ok ? step.value : undefined;
        if (!step.ok) return;
      } catch { return; }
      const chat = this.chat(chatId);
      if (!chat || !Array.isArray(projected)) return;
      try {
        chat.messages = actorMessages(projected as MirrorMsg[]);
      } catch (error) {
        console.warn(`[store] actor history rejected (${chatId})`, error);
        return;
      }
      chat.messageCount = chat.messages.length;
      chat.historyComplete = true;
      this.fullHistoriesLoaded.add(chatId);
      this.rebuildJobRefs();
      this.bumpApp(false);
    })();
    this.fullHistoryLoads.set(chatId, load);
    try { await load; } finally { this.fullHistoryLoads.delete(chatId); }
  }

  private schedulePersist(delayOverride?: number) {
    // First-paint storage contains view preferences only; the actor owns chat
    // state and the daemon receives explicit chat commands below.
    this.scheduleLocalMirror();
    // Unit/SSR surfaces and a renderer whose preload bridge is absent have no
    // daemon persistence authority. Retrying server writes in that state keeps
    // the event loop alive forever and cannot make progress.
    if (!bridgeReady()) return;
    if (this.saveTimer) clearTimeout(this.saveTimer);
    const delay = delayOverride ?? (this.state.chats.some((chat) => chat.messages.some((message) => message.status === 'running'))
      ? 3000
      : 600);
    this.saveTimer = setTimeout(() => {
      this.saveTimer = null;
      const lean = this.state.meta?.sessionSaveMode === LEAN_SESSION_SAVE_MODE;
      const save = this.serverSnapshot(lean);
      void this.saveServerSnapshot(save.snapshot, save.full, save.fullRevision);
    }, delay);
  }

  // Model, effort, permission mode, and provider are chat identity, not cosmetic
  // UI preferences. Flush those picks to the daemon immediately: a renderer
  // reconnect/tab handoff must never race the normal 600ms coalescing window and
  // restore another chat's/default controls over the user's selection.
  private async persistControlsNow() {
    for (const chat of this.state.chats) {
      if (chat.machineId || !chat.chatId || !chat.providerId) continue;
      await this.persistRuntimeControls(chat);
    }
  }

  private async persistRuntimeControls(chat: Chat): Promise<boolean> {
    const existing = this.pendingRuntimeControlSaves.get(chat.id);
    if (existing) return existing;
    const tabId = chat.id;
    const chatId = chat.chatId;
    if (!chatId || !chat.providerId) return false;
    const save = (async () => {
      if (!(await this.ensureChatCreated(chat))) return false;
      for (;;) {
        const current = this.chat(tabId);
        if (!current || current !== chat || current.chatId !== chatId || !current.providerId) return false;
        const fingerprint = runtimeControlsFingerprint(current);
        if (this.committedRuntimeControlFingerprints.get(tabId) === fingerprint) return true;
        let pending = this.pendingRuntimeControlOperations.get(tabId);
        if (!pending || pending.fingerprint !== fingerprint) {
          pending = { fingerprint, operationId: rid('runtime-controls-op') };
          this.pendingRuntimeControlOperations.set(tabId, pending);
        }
        const receipt = await call('chatRuntimeControlsSave', {
          tabId, chatId, operationId: pending.operationId,
          expectedRevision: current.runtimeControlRevision ?? 0,
          providerId: current.providerId,
          currentModelId: current.currentModelId,
          currentModeId: current.currentModeId,
          modelControls: current.modelControls,
        });
        if (!receipt?.ok || receipt.operationId !== pending.operationId) {
          this.scheduleScopedSync(['session']);
          return false;
        }
        const latest = this.chat(tabId);
        if (!latest || latest !== chat || latest.chatId !== chatId) return false;
        latest.runtimeControlRevision = receipt.runtimeControlRevision;
        latest.actorRevision = receipt.actorRevision;
        this.committedRuntimeControlFingerprints.set(tabId, pending.fingerprint);
        if (runtimeControlsFingerprint(latest) === pending.fingerprint) {
          latest.providerId = receipt.providerId;
          latest.providerName = this.providerName(receipt.providerId) ?? latest.providerName ?? null;
          latest.currentModelId = receipt.currentModelId;
          latest.currentModeId = receipt.currentModeId;
          latest.modelControls = normalizeModelControlMemory(receipt.modelControls);
          this.committedRuntimeControlFingerprints.set(tabId, runtimeControlsFingerprint(latest));
        }
        if (this.pendingRuntimeControlOperations.get(tabId)?.operationId === pending.operationId) {
          this.pendingRuntimeControlOperations.delete(tabId);
        }
        if (this.committedRuntimeControlFingerprints.get(tabId) === runtimeControlsFingerprint(latest)) return true;
      }
    })().finally(() => {
      this.pendingRuntimeControlSaves.delete(tabId);
    });
    this.pendingRuntimeControlSaves.set(tabId, save);
    return save;
  }
  // Immediate full-session flush to the daemon (clears the 600ms debounce). Used
  // for changes that MUST survive a quick restart: model/permission identity and
  // structural sidebar edits (rename, remove/reorder/collapse folder, move chat).
  // A debounced save can be dropped by a fast restart, and getSession would then
  // restore the stale session — the exact "removed folder came back" bug.
  private async flushSession(forceFull = false) {
    if (forceFull) this.requireFullSave();
    const lean = this.state.meta?.sessionSaveMode === LEAN_SESSION_SAVE_MODE;
    const save = this.serverSnapshot(lean);
    this.writeLocalMirrorNow();
    if (this.saveTimer) clearTimeout(this.saveTimer);
    this.saveTimer = null;
    await this.saveServerSnapshot(save.snapshot, save.full, save.fullRevision);
  }

  private restoreDraftImages(previous: Chat[], restored: Chat[]) {
    const byID = new Map(previous.map((chat) => [chat.id, chat.draftImages]));
    const queuedByChat = new Map(previous.map((chat) => [chat.id, new Map((chat.queue ?? []).map((item) => [item.id, item]))]));
    for (const chat of restored) {
      const images = byID.get(chat.id);
      if (images?.length) chat.draftImages = images;
      const previousQueue = queuedByChat.get(chat.id);
      for (const item of chat.queue ?? []) {
        const prior = previousQueue?.get(item.id);
        if (!prior?.draftImages?.length) continue;
        item.draftImages = prior.draftImages;
        item.attachmentState = prior.attachmentState;
        item.attachmentError = prior.attachmentError;
        if (prior.images?.length && !item.images?.length) item.images = prior.images;
      }
    }
  }

  private restorePendingDrafts(restored: Chat[]) {
    for (const chat of restored) {
      const draft = this.pendingDraftSnapshots.get(chat.id);
      if (draft !== undefined) chat.draft = draft;
    }
  }

  private preserveNewerLocalControls(previous: Chat[], restored: Chat[]) {
    const localByID = new Map(previous.map((chat) => [chat.id, chat]));
    for (const chat of restored) {
      const local = localByID.get(chat.id);
      if (!local || local.chatId !== chat.chatId || (local._controlRevision ?? 0) <= 0) continue;
      const daemonRevision = chat.runtimeControlRevision ?? 0;
      const localDaemonRevision = local.runtimeControlRevision ?? 0;
      if (daemonRevision > localDaemonRevision) continue;
      chat.providerId = local.providerId;
      chat.providerName = local.providerName;
      chat.currentModelId = local.currentModelId;
      chat.currentModeId = local.currentModeId;
      chat.modelControls = local.modelControls;
      chat._controlRevision = local._controlRevision;
      chat.runtimeControlRevision = local.runtimeControlRevision;
    }
  }

  /**
   * Carry another machine's chats across a wholesale restore from THIS daemon.
   *
   * A mounted machine's chats live in that machine's mirror and appear in no
   * snapshot this daemon can produce, so every full replacement has to put them
   * back or they are gone. Boot is not the only such path — a routine session
   * digest and a reconnect both restore wholesale — which is why this is one
   * function instead of a line repeated at each call site.
   */
  private carryRemoteChats(previous: Chat[], next: Chat[]): Chat[] {
    const remote = previous.filter((chat) => chat.machineId);
    if (!remote.length) return next;
    const present = new Set(next.map((chat) => chat.id));
    return [...next, ...remote.filter((chat) => !present.has(chat.id))];
  }

  private carryPendingCreatedChats(previous: Chat[], next: Chat[]): Chat[] {
    if (!this.pendingChatCreates.size) return next;
    const previousIndex = new Map(previous.map((chat, index) => [chat.id, index]));
    const pending = previous.filter((chat) => !chat.machineId && this.pendingChatCreates.has(chat.id));
    if (!pending.length) return next;
    const merged = [...next];
    for (const chat of pending) {
      if (merged.some((candidate) => candidate.id === chat.id)) continue;
      const at = previousIndex.get(chat.id) ?? previous.length;
      const following = previous.slice(at + 1).find((candidate) => merged.some((item) => item.id === candidate.id));
      if (following) merged.splice(merged.findIndex((candidate) => candidate.id === following.id), 0, chat);
      else merged.push(chat);
    }
    return merged;
  }

  private restoreSessionSnapshot(server: unknown): boolean {
    if (!server || typeof server !== 'object' || !Array.isArray((server as Mirror).chats)) return false;
    const authoritative = server as Mirror;
    const previousChats = this.state.chats;
    const previousWorkspaces = this.state.workspaces;
    const pendingQueues = new Map(this.pendingQueueSnapshots);
    const authoritativeChatIDs = new Set(authoritative.chats.map((chat) => chat.id));
    for (const id of this.pendingChatCreates) {
      if (authoritativeChatIDs.has(id)) this.pendingChatCreates.delete(id);
    }
    const restored = this.fromMirror(authoritative);
    this.preserveNewerLocalControls(previousChats, restored.chats);
    this.state.chats = this.carryRemoteChats(
      previousChats,
      this.carryPendingCreatedChats(previousChats, restored.chats),
    );
    this.preserveUnchangedFullHistories(previousChats, this.state.chats);
    for (const chat of restored.chats) {
      if (!this.pendingPresentationOperations.has(chat.id)) {
        this.committedPresentationFingerprints.set(chat.id, presentationFingerprint(chat));
      }
      if (!this.pendingRuntimeControlOperations.has(chat.id)) {
        this.committedRuntimeControlFingerprints.set(chat.id, runtimeControlsFingerprint(chat));
      }
    }
    this.restoreDraftImages(previousChats, this.state.chats);
    // Presentation commands are actor mutations in flight, not speculative
    // cache. A digest may carry the prior revision while their reply is still
    // crossing the wire, so retain the exact values until the matching receipt.
    for (const [chatId, snapshot] of this.pendingPresentationSnapshots) {
      const chat = this.chat(chatId);
      if (chat) applyPresentationSnapshot(chat, snapshot);
    }
    this.restorePendingDrafts(this.state.chats);
    // A digest can legitimately arrive before the renderer's queue save reply.
    // Keep the exact local projection until that reply clears its fence; the
    // daemon's next save reconciliation remains authoritative afterward.
    for (const [chatId, queue] of pendingQueues) {
      const chat = this.chat(chatId);
      if (chat) chat.queue = queue;
    }
    if (this.pendingGlobalPresentationOperation) {
      this.state.workspaces = normalizeWorkspaces([...previousWorkspaces, ...restored.workspaces]);
    } else {
      this.state.workspaces = restored.workspaces;
      this.state.collapsedWorkspaces = restored.collapsedWorkspaces;
      this.state.removedWorkspaces = restored.removedWorkspaces;
      this.state.theme = restored.theme;
      this.state.themePref = restored.themePref;
      this.state.density = restored.density;
      this.state.panes = restored.panes;
      this.state.mode = restored.mode;
      this.state.notifEnabled = restored.notifEnabled;
      this.state.globalRevision = restored.globalRevision;
      this.committedGlobalPresentationFingerprint = globalPresentationFingerprint(restored);
    }
    // Which chat you are looking at is a LOCAL choice. This runs on reconnect and
    // on the periodic session digest, so adopting the server's activeId here
    // yanks the selection out from under you every time the daemon happens to
    // write — once per second while a save loop is spinning, which is exactly how
    // it was found. The server's value is only better when the chat you had is
    // gone (deleted from another device).
    const localActive = this.state.activeId;
    const localSurvives = !!localActive && this.state.chats.some((chat) => chat.id === localActive);
    this.state.activeId = localSurvives ? localActive : restored.activeId;
    this.state.seq = restored.seq;
    if (this.state.meta?.daemon) this.releaseInactiveHistories(this.state.activeId);
    const active = this.active();
    if (active && !active.historyComplete) void this.ensureFullHistory(active.id);
    this.reconcileWorkspaces();
    this.rebuildJobRefs();
    this.requireFullSave();
    return true;
  }

  private rebuildJobRefs() {
    this.jobRef.clear();
    this.chatJobs.clear();
    for (const chat of this.state.chats) {
      for (const msg of chat.messages) {
        if (!msg.jobId || msg.status !== 'running') continue;
        this.jobRef.set(msg.jobId, { tabId: chat.id, msgId: msg.id });
        if (chat.chatId) this.chatJobs.set(chat.chatId, { tabId: chat.id, msgId: msg.id });
      }
    }
  }

  private async guardedStep<T>(
    label: string,
    task: () => Promise<T>,
    timeoutMs = HYDRATION_STEP_TIMEOUT,
  ): Promise<{ ok: true; value: T } | { ok: false; value?: undefined }> {
    let timer: ReturnType<typeof setTimeout> | null = null;
    try {
      const value = await Promise.race([
        task(),
        new Promise<T>((_, reject) => {
          timer = setTimeout(() => reject(new Error(`${label} timed out`)), timeoutMs);
        }),
      ]);
      return { ok: true, value };
    } catch (error) {
      console.warn(`[store] ${label} failed`, error);
      return { ok: false };
    } finally {
      if (timer) clearTimeout(timer);
    }
  }

  private queueAgentRefresh(reason: string, task: () => Promise<void> = () => this.onReconnected()) {
    this.agentRefresh = this.agentRefresh
      .catch((error) => { console.warn('[store] prior refresh failed', error); })
      .then(async () => {
        await this.guardedStep(`agent refresh (${reason})`, task, AGENT_REFRESH_TIMEOUT);
      })
      .catch((error) => { console.warn(`[store] agent refresh (${reason}) failed`, error); });
  }

  private ensureConnectionMonitor() {
    if (!bridgeReady() || this.monitor) return;
    this.monitor = new ConnectionMonitor(
      () => this.pingConnection(),
      (status) => this.setConnection(status),
      () => this.onReconnected(),
    );
    this.monitor.start();
  }

  private async pingConnection(): Promise<unknown> {
    const bridge = typeof window !== 'undefined' ? window.api : undefined;
    if (!bridge) throw new Error('no bridge');
    // Liveness must not wait for actor state. job:start can legitimately hold
    // the chat actor while an idle provider process resumes; using state:digest
    // as the heartbeat turned that delay into a false daemon disconnect.
    if (typeof bridge.appMeta === 'function') {
      const alive = await bridge.appMeta();
      this.probeStateDigest(bridge);
      return alive;
    }
    return this.readStateDigest(bridge);
  }

  private probeStateDigest(bridge: NonNullable<typeof window.api>) {
    if (this.digestUnsupported || this.digestProbe || typeof bridge.stateDigest !== 'function') return;
    this.digestProbe = this.readStateDigest(bridge)
      .then(() => undefined)
      .catch(() => undefined)
      .finally(() => { this.digestProbe = null; });
  }

  private async readStateDigest(bridge: NonNullable<typeof window.api>): Promise<unknown> {
    if (!this.digestUnsupported && typeof bridge.stateDigest === 'function') {
      try {
        const digest = await bridge.stateDigest();
        if (isStateDigest(digest)) {
          this.handleStateDigest(digest);
          return digest;
        }
        this.digestUnsupported = true;
      } catch (error) {
        // A rolling old daemon answers the additive invoke with an ordinary
        // channel error. Transport lifecycle failures from the LAN shim are
        // typed and must still drive the monitor offline.
        if (isTransportInvokeError(error)) throw error;
        this.digestUnsupported = true;
      }
    }
    throw new Error('no state digest channel');
  }

  private handleStateDigest(digest: StateDigest) {
    const scopes = this.digestSyncScopes(digest);
    if (scopes.size) this.scheduleScopedSync(scopes);
  }

  private digestSyncScopes(digest: StateDigest): Set<SyncScope> {
    const scopes = new Set<SyncScope>();
    if (this.sessionHydrationPending) scopes.add('session');
    if (digest.globalRevision !== this.state.globalRevision) scopes.add('session');
    const localByPair = new Map(this.state.chats.map((chat) => [`${chat.id}\u0000${chat.chatId ?? ''}`, chat]));
    for (const remote of digest.chats) {
      const chat = localByPair.get(`${remote.tabId}\u0000${remote.chatId}`);
      if (!chat || digestChatSessionDiverged(chat, remote)) {
        scopes.add('session');
        scopes.add('background');
      }
      if ((!chat && remote.pendingPermissionIds.length)
        || (chat && !sameStrings(localPendingPermissionIDs(chat), remote.pendingPermissionIds))) scopes.add('permissions');
    }
    // Only THIS daemon's chats are in its digest. Another machine's chats live in
    // that machine's mirror and are carried across every restore (carryRemoteChats),
    // so counting them here made the two sides permanently unequal: every heartbeat
    // declared the session diverged and re-restored it, wholesale, forever.
    if (digest.chats.length !== this.state.chats.filter((chat) => !chat.machineId).length) scopes.add('session');
    if (hashMapDiverged(this.localCatalogHashes, digest.catalogHash)) scopes.add('catalog');
    if (digest.settingsRevision && this.localSettingsRevision !== digest.settingsRevision) scopes.add('settings');
    if (digest.procHash && this.localProcHash !== digest.procHash) scopes.add('processes');
    return scopes;
  }

  private scheduleScopedSync(scopes: Iterable<SyncScope>) {
    for (const scope of scopes) this.syncScopes.add(scope);
    if (this.syncTimer) return;
    this.syncTimer = setTimeout(() => {
      this.syncTimer = null;
      const batch = new Set(this.syncScopes);
      this.syncScopes.clear();
      this.queueAgentRefresh('state digest', () => this.runScopedSync(batch));
    }, DIGEST_SYNC_DEBOUNCE);
  }

  private async runScopedSync(scopes: ReadonlySet<SyncScope>) {
    if (scopes.has('session')) {
      const result = await this.guardedStep('digest session:get', () => callThrow('getSession'));
      if (result.ok && this.restoreSessionSnapshot(result.value)) {
        if (this.reconcileCatalogControls()) {
          this.markAllChatsDirty();
          await this.persistControlsNow();
        }
        this.recoverIdleQueues();
        this.bumpApp(false);
      }
    }
    if (scopes.has('permissions')) {
      await this.guardedStep('digest permissions', () => this.refreshPendingPermissions());
    }
    if (scopes.has('background')) {
      await this.guardedStep('digest background', async () => { this.refreshAllSpawnedWork(); });
    }
    if (scopes.has('catalog')) {
      await this.guardedStep('digest catalog', () => this.refreshCatalogSnapshot());
    }
    if (scopes.has('settings')) {
      await this.guardedStep('digest settings', () => this.hydrateSettings());
    }
    if (scopes.has('processes')) {
      await this.guardedStep('digest processes', () => this.refreshProcesses());
    }
  }

  private async refreshCatalogSnapshot() {
    if (has('providersDetect')) {
      const detected = await call('providersDetect', {});
      if (Array.isArray(detected?.providers)) this.onProvidersList(detected.providers);
    }
    if (has('getSettings')) {
      const reply = await call('getSettings');
      if (reply && typeof reply === 'object') {
        const models = (reply as { models?: unknown }).models;
        const modes = (reply as { modes?: unknown }).modes;
        if (Array.isArray(models)) this.state.models = models as ModelOption[];
        if (Array.isArray(modes)) this.state.modes = modes as ModeOption[];
      }
    }
    if (has('providersList')) {
      const providers = await call('providersList');
      if (Array.isArray(providers)) this.onProvidersList(providers);
    }
    this.bumpApp(false);
  }

  private async refreshProcesses() {
    if (!has('procList')) return;
    const result = await call('procList');
    if (!result?.processes) return;
    this.state.processes = result.processes;
    this.captureProcHash(result.processes);
    this.bump(PROC);
  }

  private captureCatalogHashes(groups: CatalogGroup[]) {
    const epoch = ++this.catalogHashEpoch;
    void Promise.all(groups.map(async (group) => [group.providerId, await stateDigestHash(group)] as const))
      .then((entries) => {
        if (epoch !== this.catalogHashEpoch || entries.some(([, hash]) => !hash)) return;
        this.localCatalogHashes = Object.fromEntries(entries as Array<readonly [string, string]>);
      });
  }

  private captureSettingsHash(settings: unknown) {
    const epoch = ++this.settingsHashEpoch;
    void stateDigestHash(settings).then((hash) => {
      if (epoch === this.settingsHashEpoch && hash) this.localSettingsRevision = hash;
    });
  }

  private captureProcHash(processes: ProcessSummary[]) {
    const epoch = ++this.procHashEpoch;
    void stateDigestHash(processes).then((hash) => {
      if (epoch === this.procHashEpoch && hash) this.localProcHash = hash;
    });
  }

  /** Display names by machine id, for the `machine/project` slot. */
  machineNames(): Record<string, string> { return this.machineNameMap; }

  /** This machine's id, so its own chats write no prefix. */
  localMachineId(): string { return this.selfMachineId; }

  /**
   * Mount every machine in the book (remote-plan E3).
   *
   * Feature-detected twice over: a daemon without the machine book has no
   * `machinesList`, and a book with nothing in it installs no router. Either way
   * a single-machine install behaves exactly as it did before this existed.
   */
  private async mountMachines(): Promise<void> {
    try { this.state.hasFleetKey = !!localStorage.getItem('workass.fleetKey'); } catch { /* private mode */ }
    void this.reloadFleetKeys();
    if (!has('machinesList')) return;
    this.state.hasMachineChannels = true;
    let reply;
    try { reply = await call('machinesList'); } catch { return; }
    this.applyMachineBook(reply);
    on('onMachinesChanged', (payload: { machines?: unknown[]; self?: { machineId?: string } }) => { this.applyMachineBook(payload); });
  }

  private applyMachineBook(reply: { machines?: unknown[]; self?: { machineId?: string } } | undefined): void {
    const entries = Array.isArray(reply?.machines) ? (reply?.machines as MachineEntry[]) : [];
    this.selfMachineId = String(reply?.self?.machineId ?? this.selfMachineId ?? '');
    const remote = entries.filter((entry) => entry?.machineId && entry.machineId !== this.selfMachineId);
    if (!remote.length && !this.machines) { this.state.machines = []; this.bumpApp(false); return; }
    if (!this.machines) {
      this.machines = new MachineRegistry({
        local: () => (typeof window !== 'undefined' ? window.api : undefined),
        deviceName: 'Workass',
        onChange: () => { this.refreshMachineNames(); this.bumpApp(false); },
        onOpen: (machineId, info) => {
          // A reconnect reconciles; a daemon that RESTARTED lost every engine and
          // all in-memory session state, so its chats are re-read rather than
          // synchronized.
          void this.hydrateMachine(machineId, info.restarted);
        },
      });
    }
    // The key a client holds so it can enrol with a newly-found machine without
    // asking anyone. Read from storage rather than typed here: a settings field
    // writes it, and it is never persisted by the registry itself (E2/D3).
    try {
      const key = typeof localStorage !== 'undefined' ? localStorage.getItem('workass.fleetKey') : '';
      if (key) this.machines.useFleetKey(key);
    } catch { /* private mode: enrolment just needs the key again */ }
    this.machines.sync(remote, this.selfMachineId);
    // The router AND the replay: handlers registered at boot predate every
    // machine socket, so without the second argument a remote turn streams to
    // nobody and the transcript never leaves "Trabajando…".
    setMachineRouter(
      this.machines.router(),
      (method, cb) => this.machines?.subscribeRemoteMethod(method, cb as (payload: unknown) => void),
    );
    this.refreshMachineNames();
    this.bumpApp(false);
  }

  private refreshMachineNames(): void {
    this.machineNameMap = this.machines ? this.machines.names() : {};
    this.state.machines = this.machines ? this.machines.list() : [];
  }

  /**
   * Add a machine by address (remote-plan E1). The daemon probes it and answers
   * with a reason in words when it cannot be reached, because that belongs next
   * to the field you typed it into rather than in a toast that outlives it.
   */
  async addMachine(address: string): Promise<{ ok: boolean; error?: string }> {
    const typed = String(address ?? '').trim();
    if (!typed) return { ok: false, error: 'Escribí una dirección, como 192.168.1.50:8788' };
    if (!has('machinesAdd')) return { ok: false, error: 'Este daemon todavía no tiene libreta de máquinas' };
    try {
      const reply = await callThrow('machinesAdd', typed) as { ok?: boolean; error?: string } | undefined;
      if (reply && reply.ok === false) return { ok: false, error: reply.error || 'No se pudo agregar' };
      await this.reloadMachines();
      const added = this.state.machines.find((machine) => machine.address === typed || machine.address.endsWith(typed));
      if (added) this.requestMachineAccess(added.machineId);
      return { ok: true };
    } catch (err) {
      return { ok: false, error: err instanceof Error ? err.message : String(err) };
    }
  }

  requestMachineAccess(machineId: string): boolean {
    const requested = this.machines?.requestAccess(machineId) ?? false;
    if (requested) this.refreshMachineNames();
    return requested;
  }

  /** Drop a machine: it leaves the list here and its credential is forgotten. */
  async forgetMachine(machineId: string): Promise<void> {
    this.machines?.forget(machineId);
    if (has('machinesForget')) { try { await call('machinesForget', machineId); } catch { /* it is already gone from here */ } }
    await this.reloadMachines();
  }

  /** Persist a controller-local label for one remote machine. Empty clears it. */
  async setMachineNickname(machineId: string, nickname: string): Promise<{ ok: boolean; error?: string }> {
    const id = String(machineId ?? '').trim();
    if (!id) return { ok: false, error: 'La máquina ya no está disponible' };
    const normalized = normalizeMachineNickname(nickname);
    if (normalized.error) return { ok: false, error: normalized.error };
    if (!has('machinesNickname')) return { ok: false, error: 'Este daemon todavía no permite cambiar apodos' };
    try {
      const reply = await callThrow('machinesNickname', id, normalized.nickname);
      if (!reply) return { ok: false, error: 'El daemon no respondió al cambio de apodo' };
      if (reply.ok === false) return { ok: false, error: reply.error || 'No se pudo guardar el apodo' };
      this.applyMachineBook(reply);
      return { ok: true };
    } catch (err) {
      return { ok: false, error: err instanceof Error ? err.message : String(err) };
    }
  }

  /**
   * Hold the fleet key on this device. It is written to storage and handed to
   * the registry, which uses it to sign a proof — it is never sent, and it never
   * enters app state, so it cannot leak through a rendered payload.
   */
  setFleetKey(key: string): void {
    const trimmed = String(key ?? '').trim();
    try {
      if (trimmed) localStorage.setItem('workass.fleetKey', trimmed);
      else localStorage.removeItem('workass.fleetKey');
    } catch { /* private mode: it lasts for this session only */ }
    this.state.hasFleetKey = !!trimmed;
    this.machines?.useFleetKey(trimmed);
    // Reconnect so a machine that was parked can enrol with the key it now has.
    void this.reloadMachines(true);
    this.bumpApp(false);
  }

  /**
   * The keys this daemon accepts. Non-secret by construction — ids and dates —
   * so it is safe in state and safe on screen; the secret is a separate call.
   */
  async reloadFleetKeys(): Promise<void> {
    if (!has('fleetKeys')) return;
    this.state.hasFleetKeyChannels = true;
    try {
      const reply = await call('fleetKeys') as { keys?: unknown[]; canReveal?: boolean } | undefined;
      const rows = Array.isArray(reply?.keys) ? reply?.keys as Record<string, unknown>[] : [];
      this.state.fleetKeys = rows.map((row) => ({
        keyId: String(row?.keyId ?? ''),
        owner: String(row?.owner ?? ''),
        label: String(row?.label ?? ''),
        createdAt: String(row?.createdAt ?? ''),
      })).filter((row) => row.keyId);
      this.state.fleetCanReveal = reply?.canReveal === true;
      this.bumpApp(false);
    } catch { /* an older daemon simply has no keys to show */ }
  }

  /**
   * Show this machine's key so it can be carried to the next one. Deliberately
   * returned rather than stored: a secret in app state would ride every rendered
   * payload and every snapshot, and this one opens every machine you own.
   */
  async revealFleetKey(keyId?: string): Promise<{ ok: boolean; secret?: string; error?: string }> {
    if (!has('fleetReveal')) return { ok: false, error: 'Este daemon todavía no expone la clave (fleet:reveal)' };
    try {
      const reply = await callThrow('fleetReveal', keyId) as { secret?: string } | undefined;
      const secret = String(reply?.secret ?? '');
      return secret ? { ok: true, secret } : { ok: false, error: 'La respuesta no traía la clave' };
    } catch (err) {
      return { ok: false, error: fleetKeyError(err) };
    }
  }

  /** Mint the first key, which is what makes this machine the origin of a fleet. */
  async mintFleetKey(): Promise<{ ok: boolean; secret?: string; error?: string }> {
    if (!has('fleetMint')) return { ok: false, error: 'Este daemon todavía no puede generar la clave (fleet:mint)' };
    try {
      const reply = await callThrow('fleetMint') as { secret?: string } | undefined;
      await this.reloadFleetKeys();
      const secret = String(reply?.secret ?? '');
      return secret ? { ok: true, secret } : { ok: false, error: 'La respuesta no traía la clave' };
    } catch (err) {
      return { ok: false, error: fleetKeyError(err) };
    }
  }

  /**
   * Retire a key. Devices already enrolled under it keep working — their tokens
   * were derived once and do not depend on the key — so this closes the door for
   * new ones rather than locking anybody out.
   */
  async retireFleetKey(keyId: string): Promise<{ ok: boolean; error?: string }> {
    if (!has('fleetForget')) return { ok: false, error: 'Este daemon todavía no puede retirar claves (fleet:forget)' };
    try {
      await callThrow('fleetForget', keyId);
      await this.reloadFleetKeys();
      return { ok: true };
    } catch (err) {
      return { ok: false, error: fleetKeyError(err) };
    }
  }

  private async reloadMachines(reconnect = false): Promise<void> {
    if (!has('machinesList')) return;
    try {
      const reply = await call('machinesList');
      if (reconnect) { this.machines?.closeAll(); }
      this.applyMachineBook(reply);
    } catch { /* the next machines:changed will catch us up */ }
  }

  /**
   * Read one machine's chats into the unified list.
   *
   * The remote mirror goes through the SAME normalizer the local one does, so a
   * remote chat is a fully-formed chat rather than a partial that crashes a
   * renderer expecting a field. Then its ids are tagged and its machine is
   * stamped, which is everything the rest of the app needs to address it.
   */
  private async hydrateMachine(machineId: string, replaceAll = false): Promise<void> {
    const link = this.machines?.linkFor(machineId);
    if (!link) { this.machines?.setReason(machineId, 'conectada, sin enlace para leer sus chats'); return; }
    // Every exit says why. A machine that connects and then contributes nothing
    // to the list is indistinguishable from one that is simply idle, and the
    // whole feature is invisible when that happens.
    try {
      const mirror = (await link.invoke('session:get')) as Mirror | null;
      if (!mirror || !Array.isArray(mirror.chats)) {
        this.machines?.setReason(machineId, 'conectada, pero no devolvió sesión');
        return;
      }
      const normalized = this.fromMirror(mirror).chats.map((chat) => ({
        ...(tagPayload(machineId, chat) as Chat),
        machineId,
      }));
      const authoritativeIDs = new Set(normalized.map((chat) => chat.id));
      for (const id of this.pendingChatCreates) {
        if (authoritativeIDs.has(id)) this.pendingChatCreates.delete(id);
      }
      for (const id of this.remoteChatCreateFences) {
        if (authoritativeIDs.has(id)) this.remoteChatCreateFences.delete(id);
      }
      // A session refresh can race a just-issued remote chat:create. Keep only
      // the pending rows that the remote mirror has not echoed yet; confirmed
      // rows are replaced by their daemon-owned projection above.
      const pending = this.state.chats.filter((chat) =>
        chat.machineId === machineId
        && (this.pendingChatCreates.has(chat.id) || this.remoteChatCreateFences.has(chat.id))
        && !authoritativeIDs.has(chat.id));
      const others = this.state.chats.filter((chat) => chat.machineId !== machineId);
      this.state.chats = [...others, ...normalized, ...pending];
      void replaceAll;
      this.machines?.setReason(machineId, normalized.length
        ? `conectada · ${normalized.length} ${normalized.length === 1 ? 'conversación' : 'conversaciones'}`
        : 'conectada, sin conversaciones');
      this.bumpApp(true);
    } catch (err) {
      // Called as `void hydrateMachine(...)` from a socket callback, so without
      // this the rejection vanishes into an unhandled promise and the machine
      // looks healthy forever.
      this.machines?.setReason(machineId, `no pude leer sus chats: ${err instanceof Error ? err.message : String(err)}`);
    }
  }

  // ---- boot / hydrate --------------------------------------------------
  async init() {
    applyTheme(this.state.theme);
    applyDensity(this.state.density);
    this.installSystemThemeListener();
    browserApi()?.onOpenRequest((chatId?: string) => {
      // Opt-in + per-chat (user law 2026-07-12): an agent's explicit browser.open
      // marks the OWNING chat's pane and nothing else. Never switch the active
      // tab or mode, and never touch any other chat — you see the browser only
      // when you open that chat yourself. A request with no chat context is a
      // no-op (it must never hijack whatever chat you happen to be viewing).
      if (!chatId) return;
      const target = this.state.chats.find((chat) => chat.chatId === chatId || chat.id === chatId);
      if (!target) return;
      target.pane = 'browser';
      this.bumpChat(target);
    });
    // Mount the machine book before hydration, so a remote machine's chats
    // arrive in the same list rather than appearing a beat later (E3).
    void this.mountMachines();
    // Wire events before any network so nothing is missed.
    on('onJobEvent', (e) => this.onJobEvent(e as JobEvent));
    on('onChatCatalog', (c) => this.onCatalog(c as ChatCatalog));
    on('onChatPermissionRequest', (r) => this.onPermission(r as PermissionRequest));
    on('onChatPermissionResolved', (r) => this.onPermissionResolved(r as PermissionResolved));
    // Provider plan-usage snapshots — feature-detected; the daemon replays the
    // cached snapshot(s) to fresh clients like chat:catalog.
    on('onChatPlanUsage', (s) => this.onPlanUsage(s as PlanUsageSnapshot));
    on('onSpawnedWorkChanged', (s) => this.onSpawnedWorkChanged(s as SpawnedWorkChanged));
    // Per-chat changed files (Entorno rail) — feature-detected. chat:env is NOT
    // in the provider replay set, so this only keeps the card live after turns;
    // fresh-client hydration is refreshChatEnv() via chat:env-get.
    on('onChatEnv', (e) => this.onChatEnv(e as ChatEnvPayload));
    on('onProvidersList', (providers) => this.onProvidersList(providers as ProviderRecord[]));
    on('onProcChanged', (e) => this.onProcChanged(e as ProcChanged));
    on('onLanAccessRequest', (r) => this.onAccessRequest(r as AccessRequest));
    // Update notifications — feature-detected; the daemon replays the cached
    // providers:updates / app:update payloads to fresh clients like chat:catalog,
    // so a reload repaints the footer cards with no turn.
    on('onProvidersUpdates', (e) => this.onProvidersUpdates(e as ProvidersUpdates));
    // Live click-to-update progress — feature-detected; the daemon replays the
    // latest snapshot per provider to fresh clients like providers:updates.
    on('onProvidersUpdateProgress', (e) => this.onUpdateProgress(e as ProviderUpdateProgress));
    on('onAppUpdate', (e) => this.onAppUpdate(e as AppUpdate));
    // D1 compaction indicator — feature-detected; degrades to no-op if the
    // daemon bridge does not expose the channel.
    on('onChatCompacted', (e) => this.onCompacted(e as ChatCompacted));
    // R4 rewind confirmation + D4 crash-recovery notice. Feature-detected; the
    // browser bridge does not subscribe to these yet (GAP) → no-op there.
    on('onChatCheckpointRestored', (e) => this.onCheckpointRestored(e as CheckpointRestored));
    on('onChatEngineRecovered', (e) => this.onEngineRecovered(e as EngineRecovered));
    // R7 notifications — controller-only explicit agent events. Permission
    // requests retain their own attention path; ordinary turn completion never
    // creates a notification or backlog entry.
    on('onNotify', (e) => this.onNotify(e as NotifyEvent));
    on('onNotifyBacklog', (e) => this.onNotifyBacklog(e as NotifyBacklog));
    on('onChatCommands', (e) => {
      // Live catalog replace: covers exactly the case hydration cannot — a
      // same-id engine restore where no session-replaced event ever fires.
      const event = e as ChatCommandsEvent;
      const chat = event.tabId ? this.chat(event.tabId) : null;
      if (!chat) return;
      chat.commandCatalog = event.commandCatalog ?? undefined;
      this.bumpApp(false);
    });
    on('onAgentApply', (e) => {
      const event = e as AgentApply;
      if (event.action === 'session-controls-skipped') {
        // The chat is running on something other than its configured model —
        // the receipt row makes the silent downgrade visible (2026-07-27).
        const chat = event.tabId ? this.chat(event.tabId) : null;
        if (chat && event.requestedModelId) {
          chat.controlsSkipped = {
            requestedModelId: event.requestedModelId,
            requestedModeId: event.requestedModeId,
            reason: event.reason ?? '',
            error: event.error,
            at: Date.now(),
          };
          this.bumpApp(false);
        }
        return;
      }
      if (event.action !== 'session-refresh') return;
      // A session-refresh is a session/permissions delta, not a reconnection.
      // Running the full cold-reconnect reconciliation per event cost one
      // session:get plus app:meta, config:get, settings:get, proc:list,
      // permissions, chat:env-get and a spawned-work call per chat — and the
      // daemon emits one of these for every agent control operation, so a
      // single fan-out of subagents produced dozens of full hydrations.
      // The tab is not a usable discriminator: the daemon coalesces its own
      // background intents and emits one untargeted event for a merged batch.
      // Route every refresh through the bounded digest. It determines whether
      // any exact revision actually diverged before session:get is allowed to
      // replace the mirror. Directly scheduling a session pull here made every
      // renderer save echo back as a wholesale hydration.
      // A genuine reconnection is still driven by the socket-open path below.
      if (this.monitor) this.monitor.probeNow();
      else this.scheduleScopedSync(['session', 'permissions']);
    });
    if (typeof window !== 'undefined') {
      const bootSocketGen = typeof window.__workassSocketGen === 'number' ? window.__workassSocketGen : 0;
      window.addEventListener('workass:socket-open', (e) => {
        const gen = (e as CustomEvent<{ gen?: unknown }>).detail?.gen;
        if (typeof gen !== 'number' || gen <= bootSocketGen) return;
        this.queueAgentRefresh('socket-open');
      });
    }

    // Construct health/retry machinery before the first hydration await. A
    // dropped session:get can now reject and re-enter this monitor's backoff
    // path instead of parking every later reconcile behind boot forever.
    this.ensureConnectionMonitor();

    try {
    // Background tasks (Tareas) card: only shown when the daemon exposes the
    // process registry. Feature-detected so an older/newer bridge degrades.
    this.state.hasProcChannels = has('procList');
    if (this.state.hasProcChannels) {
      await this.guardedStep('boot processes', () => this.refreshProcesses());
    }
    this.state.hasSpawnedWorkChannels = has('spawnedWorkList');

    // Electron owns the browser surface locally: it is intentionally absent in
    // plain browser/LAN clients and does not change the frozen daemon protocol.
    this.state.hasBrowserChannels = browserApi()?.supported === true;

    await this.guardedStep('boot app metadata', () => this.refreshAppMeta());

    // Server session is authoritative for the SESSION (chats/prefs); the local
    // mirror already painted. Rehydrate into a fresh tree but carry over the
    // daemon-live, event-sourced state (catalog groups/models/modes, plan usage,
    // processes, update cards, devices) — those arrive via replayed events during
    // the awaits above, and a wholesale `this.state = fromMirror(...)` would wipe
    // an already-received catalog, leaving the model picker empty (regression
    // from the server-side session store).
    const sessionStep = has('getSession')
      ? await this.guardedStep('boot session:get', () => callThrow('getSession'))
      : { ok: true as const, value: undefined };
    const server = sessionStep.ok ? sessionStep.value : undefined;
    if (!sessionStep.ok) {
      this.sessionHydrationPending = true;
      this.monitor?.markDisconnected();
    }
    const receivedSession = !!server && typeof server === 'object' && Array.isArray((server as Mirror).chats);
    if (receivedSession) {
      const authoritative = server as Mirror;
      const live = this.state;
      this.state = this.fromMirror(authoritative);
      // Chats from another machine live in another machine's mirror, so a
      // wholesale replacement from THIS daemon would wipe them (E3). They are
      // carried across for the same reason groups and models are below: the
      // local session store is not their source of truth.
      this.state.chats = this.carryRemoteChats(live.chats, this.state.chats);
      if (live.activeId && this.state.chats.some((chat) => chat.id === live.activeId)) {
        this.state.activeId = live.activeId;
      }
      this.preserveNewerLocalControls(live.chats, this.state.chats);
      this.restoreDraftImages(live.chats, this.state.chats);
      this.restorePendingDrafts(this.state.chats);
      this.state.meta = live.meta;
      this.state.groups = live.groups;
      this.state.models = live.models.length ? live.models : this.state.models;
      this.state.modes = live.modes.length ? live.modes : this.state.modes;
      this.state.planUsageByProvider = live.planUsageByProvider;
      this.state.planUsageLoadingByProvider = live.planUsageLoadingByProvider;
      this.state.processes = live.processes;
      this.state.hasProcChannels = live.hasProcChannels;
      this.state.spawnedWorkByChat = live.spawnedWorkByChat;
      this.state.hasSpawnedWorkChannels = live.hasSpawnedWorkChannels;
      this.state.chatEnvByChat = live.chatEnvByChat;
      this.state.hasBrowserChannels = live.hasBrowserChannels;
      // The machine book is mounted before hydration and lives on no mirror —
      // it is this client's view of the fleet, not this daemon's session. Left
      // out, the Máquinas pane reports "no book" on a daemon that has one, and
      // the only surface that can add the FIRST machine never appears.
      this.state.machines = live.machines;
      this.state.hasMachineChannels = live.hasMachineChannels;
      this.state.hasFleetKey = live.hasFleetKey;
      this.state.fleetKeys = live.fleetKeys;
      this.state.fleetCanReveal = live.fleetCanReveal;
      this.state.hasFleetKeyChannels = live.hasFleetKeyChannels;
      this.state.providersUpdates = live.providersUpdates;
      this.state.appUpdate = live.appUpdate;
      this.state.updateProgress = live.updateProgress;
      this.state.devices = live.devices;
      this.state.accessRequests = live.accessRequests;
      this.rebuildJobRefs();
      this.requireFullSave();
      applyTheme(this.state.theme);
      this.sessionHydrationPending = false;
    }
    this.reconcileWorkspaces();
    // Catalog replay can arrive before the authoritative server mirror. The
    // mirror restore above then replaces chats with their persisted model ids,
    // so reconcile once more at this boundary or a stale Claude `default`
    // selection can survive startup and hide its effort control indefinitely.
    if (this.reconcileCatalogControls()) {
      this.markAllChatsDirty();
      await this.guardedStep('boot persist reconciled controls', () => this.persistControlsNow());
    }
    // BONUS-FIX: fresh boot with no persisted chats. newChat(false) creates the
    // chat but intentionally does NOT activate; without an activeId the whole
    // surface renders its empty state and the composer targets nothing. Set the
    // active id explicitly (keeping the restored `mode` rather than forcing it).
    if (sessionStep.ok && receivedSession && this.state.chats.length === 0) {
      const c = this.newChat(false);
      this.state.activeId = c.id;
    }

    // Dispositivos (real): device list + pending access requests, feature-detected.
    this.state.hasDeviceChannels = has('lanDevices');
    if (this.state.hasDeviceChannels) {
      await this.guardedStep('boot devices', () => this.loadDevices());
    }

    // Agentes + Engines read the daemon config snapshot (config:get). Stubbed to
    // defaults today, so controls stay read-only — no fake persistence.
    this.state.hasConfigChannel = has('getConfig');
    if (this.state.hasConfigChannel) {
      const cfgStep = await this.guardedStep('boot config', () => call('getConfig'));
      const cfg = cfgStep.ok ? cfgStep.value : undefined;
      if (cfg && typeof cfg === 'object') this.state.daemonConfig = cfg;
    }

    // Settings · Modelos scores live in the daemon app-settings blob. Hydrate
    // AFTER the session-mirror restore above (which resets modelScores to {}),
    // so the authoritative daemon value wins on every startup/reload.
    await this.guardedStep('boot settings', () => this.hydrateSettings());

    if (has('providersList')) {
      const providersStep = await this.guardedStep('boot providers', () => call('providersList'));
      const providers = providersStep.ok ? providersStep.value : undefined;
      if (Array.isArray(providers)) this.onProvidersList(providers);
    }

    // Session hydration is intentionally bounded. Paint it immediately, then
    // replace only the selected chat with the complete actor projection.
    if (has('archiveLoad')) {
      this.releaseInactiveHistories(this.state.activeId);
      if (this.state.activeId) void this.ensureFullHistory(this.state.activeId);
    }
    await this.guardedStep('boot permissions', () => this.refreshPendingPermissions());
    } catch (error) {
      console.warn('[store] partial boot', error);
    } finally {
      // Partial hydration is still a usable renderer. The monitor and digest
      // reconciliation path continues pulling any step that failed above.
      this.state.hydrated = true;
      this.bumpApp(false);
      this.recoverIdleQueues();
      this.refreshAllSpawnedWork();
    }
    // Subscription limits are account metadata, not a model turn. Attach or
    // resume only the active frontier chat after hydration so its limits are
    // populated before the user sends anything.
    const active = this.active();
    if (active) this.refreshPlanUsage(active.id);
  }

  // ---- connection health ----------------------------------------------
  isConnected(): boolean { return this.state.connection === 'connected'; }
  private setConnection(next: ConnStatus) {
    if (this.state.connection === next) return;
    this.state.connection = next;
    this.bump(CONN);       // banner + per-turn retry affordances
    this.bumpApp(false);   // composer send-gating; connection is never persisted
  }
  // A WebSocket loss is not proof that the daemon or provider process died.
  // Keep optimistic rows and job anchors intact until session:get reconciles
  // daemon-owned truth after reconnect. Marking them failed here made live
  // messages disappear and then reappear when the same daemon answered.
  // Reconcile against daemon-owned state after the socket comes back. The
  // daemon may be the SAME process (Electron/view reconnect: live sessions and
  // jobs survive) or a fresh process (no liveSession overlay). session:get is
  // what distinguishes those cases; never discard a live ACP binding merely
  // because the view's WebSocket was replaced.
  private async onReconnected() {
    // A daemon handoff may cross the version where runtime profile metadata was
    // introduced. Re-read it before restoring catalog-dependent UI so dev never
    // remains stuck on the conservative production filter until Electron reloads.
    await this.guardedStep('reconnect app metadata', () => this.refreshAppMeta());
    const sessionStep = has('getSession')
      ? await this.guardedStep('reconnect session:get', () => callThrow('getSession'))
      : { ok: true as const, value: undefined };
    const server = sessionStep.ok ? sessionStep.value : undefined;
    if (sessionStep.ok && this.restoreSessionSnapshot(server)) {
      this.sessionHydrationPending = false;
      if (this.reconcileCatalogControls()) {
        this.markAllChatsDirty();
        await this.guardedStep('reconnect persist reconciled controls', () => this.persistControlsNow());
      }
    } else if (sessionStep.ok) {
      for (const chat of this.state.chats) {
        chat.sessionId = null;
        chat.pending = true;
        chat._initPromise = undefined;
      }
      this.requireFullSave();
      this.sessionHydrationPending = false;
    } else {
      this.sessionHydrationPending = true;
    }
    this.bumpApp(false);
    if (this.state.hasProcChannels) {
      await this.guardedStep('reconnect processes', () => this.refreshProcesses());
    }
    if (this.state.hasConfigChannel) {
      const cfgStep = await this.guardedStep('reconnect config', () => call('getConfig'));
      const cfg = cfgStep.ok ? cfgStep.value : undefined;
      if (cfg && typeof cfg === 'object') this.state.daemonConfig = cfg;
    }
    // Re-read scores from the daemon settings blob: another controller/device may
    // have edited them while this view was offline (parity with getConfig above).
    await this.guardedStep('reconnect settings', () => this.hydrateSettings());
    if (this.state.hasDeviceChannels) {
      await this.guardedStep('reconnect devices', () => this.loadDevices());
    }
    await this.guardedStep('reconnect permissions', () => this.refreshPendingPermissions());
    this.refreshAllSpawnedWork();
    // The Entorno card is already mounted across a reconnect (its chat prop is
    // unchanged, so its own mount effect never re-fires) — refresh the active
    // chat's snapshot here so it recovers after a socket drop.
    void this.refreshChatEnv();
    this.recoverIdleQueues();
    this.bumpApp(false);
  }

  private recoverIdleQueues() {
    for (const chat of this.state.chats) {
      if (shouldDrainRecoveredQueue(chat.queue, this.isChatRunning(chat.id))) void this.flushNextQueued(chat);
    }
  }

  private async refreshAppMeta() {
    const meta = await call('appMeta');
    if (!meta) return;
    this.state.meta = {
      rootDir: meta.rootDir,
      workspaceDir: meta.workspaceDir,
      profile: workassRuntimeProfile(meta.profile),
      daemon: meta.daemon === true,
      sessionSaveMode: meta.sessionSaveMode === LEAN_SESSION_SAVE_MODE ? LEAN_SESSION_SAVE_MODE : undefined,
      workspaceRebindMode: meta.workspaceRebindMode === 'transactional-v1' ? 'transactional-v1' : undefined,
    };
    if (this.state.groups.length) {
      reportShellCatalog(userFacingCatalogGroups(this.state.groups, this.state.meta.profile));
    }
  }
  // Retry an interrupted / failed-send turn: drop the failed assistant row and
  // its paired user prompt, then re-send cleanly (which re-establishes a session
  // if needed). No-op while still offline — the affordance is disabled there.
  async retryTurn(tabId: string, msgId: string) {
    if (!this.isConnected()) return;
    const chat = this.chat(tabId); if (!chat) return;
    const msg = chat.messages.find((candidate) => candidate.id === msgId); if (!msg) return;
    const prompt = msg.retryPrompt;
    if (!prompt) return;
    const idx = chat.messages.indexOf(msg);
    const priorUser = [...chat.messages.slice(0, idx)].reverse().find((candidate) => candidate.role === 'user');
    const remove = new Set<string>([msg.id]);
    for (let j = idx - 1; j >= 0; j--) { if (chat.messages[j].role === 'user') { remove.add(chat.messages[j].id); break; } }
    chat.messages = chat.messages.filter((m) => !remove.has(m.id));
    this.bumpChat(chat);
    await this._send(chat, prompt, undefined, undefined, priorUser ? { userId: priorUser.id, assistantId: msg.id } : undefined);
  }

  // ---- theme / density / panes -----------------------------------------
  private systemThemeMql?: MediaQueryList;
  private installSystemThemeListener() {
    if (typeof matchMedia === 'undefined' || this.systemThemeMql) return;
    this.systemThemeMql = matchMedia('(prefers-color-scheme: light)');
    this.systemThemeMql.addEventListener('change', () => {
      if (this.state.themePref !== 'system') return;
      this.state.theme = resolveTheme('system');
      applyTheme(this.state.theme);
      this.bumpApp(false);
    });
  }
  setThemePref(pref: ThemePref) {
    this.state.themePref = pref;
    this.state.theme = resolveTheme(pref);
    applyTheme(this.state.theme);
    this.bumpApp();
  }
  setDensity(density: Density) {
    if (this.state.density === density) return;
    this.state.density = density;
    applyDensity(density);
    this.bumpApp();
  }
  toggleSide() { this.state.panes.side = !this.state.panes.side; this.bumpApp(); }
  // The info rail and the browser are the two occupants of the ONE right column,
  // now chosen PER CHAT (never a global flag): toggling only ever changes the
  // ACTIVE chat's pane, so it can't disturb any other chat's view. Selecting the
  // occupant already shown closes the column for this chat.
  private setActivePane(pane: RightPane) {
    const chat = this.active(); if (!chat) return;
    chat.pane = nextPane(chatPane(chat), pane);
    this.bumpChat(chat);
  }
  toggleRail() { this.setActivePane('rail'); }
  closeRail() { const chat = this.active(); if (!chat) return; chat.pane = null; this.bumpChat(chat); }
  toggleRailWide() { this.state.panes.railWide = !this.state.panes.railWide; this.bumpApp(); }
  toggleBrowser() { this.setActivePane('browser'); }
  setMode(mode: 'assist' | 'chats') { this.state.mode = mode; this.bumpApp(); }
  // Custom pane widths (drag handles). Clamped to design bounds; persisted via
  // the mirror. Double-click on a handle resets to the default (288 / 312).
  setPaneWidth(which: 'side' | 'rail', w: number) {
    // The rail width is shared by every right-pane occupant (info rail + browser),
    // so its upper bound is generous enough to drag out to a real browsing width.
    const clamped = which === 'side' ? clampW(w, 220, 400) : clampW(w, 260, 900);
    if (which === 'side') this.state.panes.sideW = clamped; else this.state.panes.railW = clamped;
    this.bumpApp();
  }
  // Header bg-process chip → reveal + pulse the active chat's rail Tareas card.
  focusTareas() {
    const chat = this.active();
    if (chat) chat.pane = 'rail';
    this.state.flashTareas = true;
    if (chat) this.bumpChat(chat);
    else this.bumpApp();   // flashTareas is transient (never mirrored)
    setTimeout(() => { this.state.flashTareas = false; this.bumpApp(false); }, 1300);
  }

  // Full-size image viewer overlay (transient, never mirrored): click a chat
  // image to open it; backdrop/Esc/close button dismiss.
  openImageLightbox(src: string, alt = '') {
    if (!src) return;
    this.state.imageLightbox = { src, alt };
    this.bumpApp(false);
  }
  closeImageLightbox() {
    if (!this.state.imageLightbox) return;
    this.state.imageLightbox = null;
    this.bumpApp(false);
  }

  // ---- settings view (transient; not mirrored) -------------------------
  openSettings(section?: SettingsSection) {
    if (section) this.state.settingsSection = section;
    this.state.settingsOpen = true;
    this.bumpApp(false);
    if (this.state.hasDeviceChannels) void this.loadDevices();
  }
  closeSettings() { this.state.settingsOpen = false; this.bumpApp(false); }
  toggleSettings() { this.state.settingsOpen ? this.closeSettings() : this.openSettings(); }

  // ---- ⌘, command box --------------------------------------------------
  // No loading, no fetch, no capability check: the box's whole point is being
  // reachable in the states where the rest of the app is not.
  openCommandBar() { this.state.commandBarOpen = true; this.bumpApp(false); }
  closeCommandBar() { this.state.commandBarOpen = false; this.bumpApp(false); }
  toggleCommandBar() { this.state.commandBarOpen ? this.closeCommandBar() : this.openCommandBar(); }
  setSettingsSection(section: SettingsSection) { this.state.settingsSection = section; this.bumpApp(false); }

  // ---- model scoring (Settings · Modelos) ------------------------------
  // Durable user PREFERENCES persisted through the daemon app-settings blob
  // (settings:get / settings:set) — the SAME authority the agent-facing catalog
  // reads back, never the session mirror or a second localStorage copy. Each
  // mutation bumps APP for an instant repaint (no session-mirror write) and
  // schedules a debounced, read-modify-write settings save. All value changes go
  // through the pure model-scores helpers, which clamp and prune empty entries.
  setModelScore(providerId: string, modelId: string, dimension: ScoreDimension, value: unknown) {
    const next = withScoreValue(this.state.modelScores, providerId, modelId, dimension, value);
    if (next === this.state.modelScores) return;
    this.state.modelScores = next;
    this.commitModelScores();
  }
  setModelNote(providerId: string, modelId: string, note: string) {
    const next = withNoteValue(this.state.modelScores, providerId, modelId, note);
    if (next === this.state.modelScores) return;
    this.state.modelScores = next;
    this.commitModelScores();
  }
  // Reset one model to unscored.
  resetModelScore(providerId: string, modelId: string) {
    const next = clearModelScore(this.state.modelScores, providerId, modelId);
    if (next === this.state.modelScores) return;
    this.state.modelScores = next;
    this.commitModelScores();
  }
  // Reset every model to unscored (panel-level action).
  resetAllModelScores() {
    if (countScoredModels(this.state.modelScores) === 0) return;
    this.state.modelScores = clearAllModelScores();
    this.commitModelScores();
  }
  toggleModelFavorite(providerId: string, modelId: string) {
    const next = toggleModelFavorite(this.state.modelFavorites, providerId, modelId);
    this.state.modelFavorites = next;
    this.commitModelPreferences();
  }
  // Repaint now (bumpApp(false) → no session-mirror write) and schedule the
  // debounced settings save.
  private commitModelScores() {
    this.commitModelPreferences();
  }
  private commitModelPreferences() {
    this.bumpApp(false);
    this.scheduleModelPreferencesSave();
  }
  // Read scores from the daemon app-settings blob and adopt them. Feature-detected
  // (older bridges omit getSettings → keep whatever is in memory). A malformed
  // reply degrades to "no scores" rather than throwing.
  private async hydrateSettings() {
    if (!has('getSettings')) return;
    const reply = await call('getSettings');
    if (reply === undefined) return;   // call() swallowed an error — don't wipe scores
    this.captureSettingsHash(settingsFromReply(reply));
    // A pending local edit (debounce still armed, possibly started during the
    // await above) is newer than anything the daemon can return — never let a
    // startup/reconnect re-hydrate clobber an unsaved score change.
    if (this.settingsSaveTimer) return;
    this.state.modelScores = modelScoresFromSettings(reply);
    this.state.modelFavorites = modelFavoritesFromSettings(reply);
    this.bumpApp(false);
  }
  private scheduleModelPreferencesSave() {
    if (!has('setSettings')) return;   // no write channel → in-memory only, no persistence
    if (this.settingsSaveTimer) clearTimeout(this.settingsSaveTimer);
    this.settingsSaveTimer = setTimeout(() => { void this.flushModelPreferences(); }, 600);
  }
  // Persist the current scores into the daemon app-settings blob. settings:set
  // REPLACES the blob, so we round-trip a fresh getSettings read and overwrite ONLY
  // modelScores — preserving every other key (models, permissionModes, chatMode,
  // …) and reconciling any concurrent edit made since the last read. If the read
  // fails we abort rather than write a blob that would wipe the other keys.
  private async flushModelPreferences() {
    this.settingsSaveTimer = null;
    if (!has('setSettings') || !has('getSettings')) return;
    const reply = await call('getSettings');
    if (!reply || typeof reply !== 'object') return;   // can't confirm the blob — never clobber
    const withScores = withModelScoresInSettings(settingsFromReply(reply), this.state.modelScores);
    const merged = withModelFavoritesInSettings(withScores, this.state.modelFavorites);
    await call('setSettings', merged);
  }

  // ---- devices / access requests (real; controller-gated) --------------
  async loadDevices() {
    const r = await call('lanDevices');
    if (r && Array.isArray(r.devices)) { this.state.devices = r.devices; this.bump(DEV); this.bumpApp(false); }
  }
  private onAccessRequest(r: AccessRequest) {
    if (!r || !r.requestId) return;
    if (this.state.accessRequests.some((x) => x.requestId === r.requestId)) return;
    this.state.accessRequests = [...this.state.accessRequests, r];
    this.bump(DEV); this.bumpApp(false);
  }
  private clearRequest(requestId: string) {
    this.state.accessRequests = this.state.accessRequests.filter((x) => x.requestId !== requestId);
    this.bump(DEV); this.bumpApp(false);
  }
  async approveAccess(requestId: string) {
    this.clearRequest(requestId);
    await call('lanAccessDecide', requestId, true);
    // The approved device reconnects with its issued token; give it a beat to
    // appear, then refresh the real list.
    setTimeout(() => { void this.loadDevices(); }, 600);
  }
  async rejectAccess(requestId: string) {
    this.clearRequest(requestId);
    await call('lanAccessDecide', requestId, false);
  }
  async revokeDevice(deviceId: string) {
    await call('lanRevoke', deviceId);
    await this.loadDevices();
  }

  // ---- chats -----------------------------------------------------------
  private reconcileWorkspaces() {
    const removed = new Set(this.state.removedWorkspaces.map(normalizeWorkspacePath));
    const inferred = this.state.chats
      .map((chat) => workspaceFromPath(chat.cwd ?? ''))
      .filter((workspace): workspace is NonNullable<typeof workspace> => workspace !== null);
    const fallback = workspaceFromPath(this.state.meta?.workspaceDir ?? this.state.meta?.rootDir ?? '');
    // Never recreate a folder the user removed, even if a chat's bound session
    // still runs there — that is the "removed folder came back" bug.
    this.state.workspaces = normalizeWorkspaces([...this.state.workspaces, ...inferred, ...(fallback ? [fallback] : [])])
      .filter((w) => !removed.has(normalizeWorkspacePath(w.path)));
    const defaultPath = this.state.workspaces[0]?.path ?? null;
    if (defaultPath) {
      for (const chat of this.state.chats) {
        if (!chat.cwd) {
          chat.cwd = defaultPath;
          chat.group = workspaceFromPath(defaultPath)?.name ?? chat.group;
        }
      }
    }
  }
  newChat(activate = true, workspacePath?: string | null, machineId?: string | null): Chat {
    this.state.seq += 1;
    const active = this.active();
    const cwd = chooseWorkspacePath(workspacePath, active, this.state.workspaces, this.state.meta?.workspaceDir ?? this.state.meta?.rootDir);
    const workspace = cwd ? workspaceFromPath(cwd) : null;
    const controls = inheritChatControls(active);
    // A new conversation created while reading another machine belongs to that
    // machine when it stays in the same workspace. The row-level "Nueva aquí"
    // action passes the machine explicitly, including for an inactive row.
    // Tag both durable ids before the first actor call: the machine router makes
    // its decision from those tags, so adding machineId only as display metadata
    // would still create the actor and engine on this Mac.
    const activeMachine = String(active?.machineId ?? '').trim();
    const sameAsActive = normalizeWorkspacePath(active?.cwd) === normalizeWorkspacePath(cwd);
    const targetMachine = String(
      machineId === undefined ? (sameAsActive ? activeMachine : '') : (machineId ?? ''),
    ).trim();
    const localTabId = `tab-${Date.now()}-${this.state.seq}`;
    const localChatId = newChatConvId();
    const chat: Chat = {
      id: tagId(targetMachine, localTabId), chatId: tagId(targetMachine, localChatId),
      machineId: targetMachine || undefined, sessionId: null,
      title: `Chat ${this.state.seq}`, titleLocked: false, group: workspace?.name ?? 'chats', cwd,
      currentModelId: controls.currentModelId, currentModeId: controls.currentModeId,
      providerId: controls.providerId, providerName: controls.providerName,
      // New chats paint with the right column closed from their first frame.
      // Leaving this undefined briefly selected the legacy rail fallback until
      // the daemon's explicit null arrived, producing an open-then-close flash.
      pane: null,
      pending: true, messages: [], draft: '',
    };
    this.state.chats.unshift(chat);
    this.pendingChatCreates.add(chat.id);
    this.pendingChatCreateOperations.set(chat.id, { operationId: rid('chat-create'), focus: activate });
    // Recorded here, not at the button: "Nueva aquí", the per-folder + in the
    // scope menu and addWorkspace all land here, and each of them is the user
    // telling us which project new chats should default to next.
    rememberLastProject(cwd);
    if (activate) { this.state.activeId = chat.id; this.state.mode = 'chats'; }
    this.requireFullSave();
    this.bumpApp();
    void this.ensureChatCreated(chat);
    if (activate) this.refreshPlanUsage(chat.id);
    return chat;
  }
  private async ensureChatCreated(chat: Chat): Promise<boolean> {
    if (!this.pendingChatCreates.has(chat.id)) return true;
    const chatId = chat.chatId;
    if (!chatId) {
      chat.sessionError = 'La conversación no tiene una identidad durable.';
      return false;
    }
    const existing = this.pendingChatCreatePromises.get(chat.id);
    if (existing) return existing;
    let operation = this.pendingChatCreateOperations.get(chat.id);
    if (!operation) {
      operation = { operationId: rid('chat-create'), focus: this.state.activeId === chat.id };
      this.pendingChatCreateOperations.set(chat.id, operation);
    }
    const creation = operation;
    const pending = (async (): Promise<boolean> => {
      const receipt = await call('chatCreate', {
        tabId: chat.id, chatId, operationId: creation.operationId, focus: creation.focus,
        title: chat.title, titleLocked: chat.titleLocked, group: chat.group ?? null, cwd: chat.cwd ?? null,
        providerId: chat.providerId ?? null, currentModelId: chat.currentModelId ?? null,
        currentModeId: chat.currentModeId ?? null, modelControls: chat.modelControls,
      });
      if (!receipt?.ok || receipt.tabId !== chat.id || receipt.chatId !== chatId || receipt.operationId !== creation.operationId) {
        chat.sessionError = 'El daemon no confirmó la creación durable de la conversación.';
        this.bumpApp(false);
        return false;
      }
      chat.actorRevision = receipt.actorRevision;
      chat.presentationRevision = receipt.presentationRevision;
      this.committedRuntimeControlFingerprints.set(chat.id, runtimeControlsFingerprint(chat));
      if (creation.focus && receipt.globalRevision > 0) this.state.globalRevision = receipt.globalRevision;
      chat.sessionError = undefined;
      this.pendingChatCreateOperations.delete(chat.id);
      // The remote daemon's reply is ordered after its actor commit, so creation
      // no longer needs to be retried. A stale session:get can still arrive after
      // that receipt, however, so a separate fence preserves the row until the
      // authoritative remote mirror echoes it.
      if (chat.machineId) {
        this.pendingChatCreates.delete(chat.id);
        this.remoteChatCreateFences.add(chat.id);
      }
      else this.scheduleScopedSync(['session']);
      return true;
    })().finally(() => {
      this.pendingChatCreatePromises.delete(chat.id);
    });
    this.pendingChatCreatePromises.set(chat.id, pending);
    return pending;
  }
  addWorkspace(path: string): Chat | null {
    const workspace = workspaceFromPath(path);
    if (!workspace) return null;
    const key = normalizeWorkspacePath(workspace.path);
    this.state.removedWorkspaces = this.state.removedWorkspaces.filter((p) => normalizeWorkspacePath(p) !== key); // re-adding un-removes
    this.state.workspaces = normalizeWorkspaces([...this.state.workspaces, workspace]);
    return this.newChat(true, workspace.path);
  }
  // ---- sidebar: rename / drag chats, remove / reorder / collapse folders -----
  // Double-click rename. Locks the title so the first-prompt auto-title (_send)
  // can never overwrite a name the user typed on purpose.
  renameChat(id: string, title: string) {
    const chat = this.chat(id); if (!chat) return;
    const next = title.trim().slice(0, 80);
    if (!next || next === chat.title) return;
    chat.title = next;
    chat.titleLocked = true;
    this.bumpChat(chat);
    void this.flushSession(true);
  }
  // Drag-reorder a chat. A cross-folder drop is also a cwd change, so an
  // initialized chat crosses a daemon-owned turn boundary before the renderer
  // changes folders. The move promise gates send/attachment initialization.
  moveChat(chatId: string, beforeId: string | null, folderPath?: string | null): Promise<boolean> {
    const chat = this.chat(chatId);
    if (!chat) return Promise.resolve(false);
    const targetCwd = folderPath === undefined ? chat.cwd : normalizeWorkspacePath(folderPath ?? '') || null;
    const workspaceChanged = folderPath !== undefined && normalizeWorkspacePath(chat.cwd ?? '') !== normalizeWorkspacePath(targetCwd ?? '');
    if (!workspaceChanged) {
      this.placeChat(chat, beforeId);
      this.bumpChat(chat);
      void this.flushSession(true);
      return Promise.resolve(true);
    }
    if (this.isChatRunning(chat.id)) {
      this.addToast('No se movió la conversación', 'Esperá a que termine la respuesta en curso.');
      return Promise.resolve(false);
    }
    const existing = this.workspaceMoves.current(chat.id);
    if (existing) return existing;
    return this.workspaceMoves.run(chat.id, () => this.moveChatWorkspace(chat, beforeId, targetCwd));
  }

  private placeChat(chat: Chat, beforeId: string | null) {
    const chats = this.state.chats;
    const from = chats.indexOf(chat);
    if (from < 0) return;
    chats.splice(from, 1);
    let to = beforeId ? chats.findIndex((candidate) => candidate.id === beforeId) : chats.length;
    if (to < 0) to = chats.length;
    chats.splice(to, 0, chat);
  }

  private async moveChatWorkspace(chat: Chat, beforeId: string | null, targetCwd: string | null): Promise<boolean> {
    // A session creation already in flight owns the same lifecycle boundary.
    // Let it settle, then invalidate that exact initialized session below.
    if (chat._initPromise) await chat._initPromise;
    if (this.chat(chat.id) !== chat || this.isChatRunning(chat.id)) return false;

    const currentRevision = chat.workspaceRevision ?? 0;
    if (chat.sessionId || currentRevision > 0) {
      if (!this.state.meta?.daemon || !workspaceRebindSupported(this.state.meta)) {
        this.addToast('No se movió la conversación', 'Esta versión del motor no puede cambiar la carpeta de una sesión iniciada de forma segura.');
        return false;
      }
      if (!targetCwd) {
        this.addToast('No se movió la conversación', 'Elegí una carpeta de trabajo concreta para una conversación iniciada.');
        return false;
      }
      const operationId = chat._sessionOperationId ?? (chat._sessionOperationId = rid('workspace-move'));
      const result = await call('appChatNewSession', {
        cwd: targetCwd,
        tabId: chat.id,
        chatId: chat.chatId,
        operationId,
        providerId: chat.providerId ?? null,
        replaceSessionId: chat.sessionId ?? undefined,
        workspaceRebind: true,
        expectedWorkspaceRevision: currentRevision,
      });
      if (!workspaceMoveAccepted(result, currentRevision, operationId)) {
        this.addToast('No se movió la conversación', result?.error ?? 'El motor no confirmó el cambio de carpeta.');
        return false;
      }
      // The daemon has durably committed a new workspace epoch and invalidated
      // the previous attachment. Keep it sessionless until the next real need;
      // ensureSession creates the explicit new lane without transcript replay.
      chat.sessionId = null;
      chat.pending = true;
      chat._sessionOperationId = undefined;
      chat.workspaceRevision = result!.workspaceRevision;
      chat.sessionError = undefined;
      chat.imageSupport = false;
      chat.commands = undefined;
      chat.commandCatalog = undefined;
    }

    chat.cwd = targetCwd;
    chat.group = targetCwd ? workspaceFromPath(targetCwd)?.name ?? chat.group : 'chats';
    if (targetCwd) {
      this.state.removedWorkspaces = this.state.removedWorkspaces.filter((path) => normalizeWorkspacePath(path) !== targetCwd);
    }
    this.placeChat(chat, beforeId);
    this.bumpChat(chat);
    await this.flushSession(true);
    return true;
  }
  // Remove a folder FROM WORKASS only — never touches the folder on disk. Record
  // it so inference can't recreate it (a chat's bound session may still run
  // there); its chats simply fall to the unassigned "Chats" bucket, keeping their
  // real cwd. Re-adding the folder (addWorkspace) restores it with those chats.
  removeWorkspace(path: string) {
    const key = normalizeWorkspacePath(path);
    if (!key) return;
    this.state.workspaces = this.state.workspaces.filter((w) => normalizeWorkspacePath(w.path) !== key);
    if (!this.state.removedWorkspaces.some((p) => normalizeWorkspacePath(p) === key)) {
      this.state.removedWorkspaces = [...this.state.removedWorkspaces, key];
    }
    this.state.collapsedWorkspaces = this.state.collapsedWorkspaces.filter((p) => normalizeWorkspacePath(p) !== key);
    this.bumpApp();
    void this.flushSession(true);
  }
  // Drag-reorder folders. `beforePath` is the folder it lands in front of
  // (null → end); the sidebar group order follows this workspaces list.
  reorderWorkspaces(path: string, beforePath: string | null) {
    const key = normalizeWorkspacePath(path);
    const moved = this.state.workspaces.find((w) => normalizeWorkspacePath(w.path) === key);
    if (!moved) return;
    const list = this.state.workspaces.filter((w) => normalizeWorkspacePath(w.path) !== key);
    const beforeKey = beforePath ? normalizeWorkspacePath(beforePath) : '';
    let to = beforeKey ? list.findIndex((w) => normalizeWorkspacePath(w.path) === beforeKey) : list.length;
    if (to < 0) to = list.length;
    list.splice(to, 0, moved);
    this.state.workspaces = list;
    this.bumpApp();
    void this.flushSession(true);
  }
  toggleWorkspaceCollapsed(path: string) {
    const key = normalizeWorkspacePath(path);
    const set = new Set(this.state.collapsedWorkspaces.map((p) => normalizeWorkspacePath(p)));
    if (set.has(key)) set.delete(key); else set.add(key);
    this.state.collapsedWorkspaces = [...set];
    this.bumpApp();
    void this.flushSession(true);
  }
  switchChat(id: string) {
    if (this.state.activeId === id) return;
    this.state.activeId = id;
    this.releaseInactiveHistories(id);
    void this.ensureFullHistory(id);
    const chat = this.chat(id);
    if (chat?.unread) {
      chat.unread = false;
      this.touchChat(chat.id);
    }
    // Review/rewind are snapshots of one specific chat. Leaving that tab closes
    // them so old-chat data can never remain presented beside the new active
    // transcript.
    if (this.state.rewind.open) this.state.rewind = { ...this.state.rewind, open: false, error: undefined };
    if (this.state.review.open) this.state.review = { ...this.state.review, open: false, diff: undefined, error: undefined };
    this.bumpApp();
    this.refreshPlanUsage(id);
    void this.refreshSpawnedWork(chat);
    if (chat) this.maybeFetchCommandCatalog(chat);
  }

  // The catalog is daemon-memory-only, so hydration cannot carry it: a
  // reloaded client, or one that attached after a same-id restore, asks once.
  // A chat that already has one relies on chat:commands replaces instead.
  private commandCatalogAsked = new Set<string>();
  requestCommandCatalog(chatId: string) {
    const chat = this.chat(chatId);
    if (chat) this.maybeFetchCommandCatalog(chat);
  }
  private maybeFetchCommandCatalog(chat: Chat) {
    if (chat.commandCatalog || !chat.chatId) return;
    if ((chat.providerId ?? '') !== 'claude') return;
    if (this.commandCatalogAsked.has(chat.id)) return;
    this.commandCatalogAsked.add(chat.id);
    void call('chatCommandsGet', chat.id, chat.chatId).then((reply) => {
      if (!reply?.supported || !reply.commandCatalog) return;
      const target = this.chat(chat.id);
      if (!target) return;
      target.commandCatalog = reply.commandCatalog;
      this.bumpApp(false);
    }).finally(() => {
      // Allow a later retry: the engine may simply not have attached yet.
      this.commandCatalogAsked.delete(chat.id);
    });
  }
  // Port of T3's settle/un-settle. Filing is not deletion: it drops to the shelf
  // for the retention window, then remains recoverable through archive search.
  // Un-settling stores the opposite override rather than clearing the flag,
  // because age would otherwise re-file an old chat the instant it came back.
  settleChat(id: string, settled: boolean) {
    const chat = this.chat(id);
    if (!chat) return;
    chat.settled = settled ? 'settled' : 'active';
    chat.settledAt = settled ? Date.now() : undefined;
    // Filing a chat away is also an acknowledgement of how it ended: the
    // unseen-completion guard outranks the shelf, so without this a "Listo" or
    // "Falló" row would absorb the click and stay exactly where it was.
    if (settled) chat.unread = false;
    this.markPresentationMutation(chat);
    this.bumpChat(chat);
    void this.flushSession(true);   // structural sidebar edit: must survive a fast restart
  }
  // T3's "Mark unread": arms the finished cue by hand for a chat you want to
  // come back to. Meaningless on the chat you are reading — switchChat would
  // clear it on the next visit anyway.
  markUnread(id: string) {
    const chat = this.chat(id);
    if (!chat || this.state.activeId === id || chat.unread) return;
    chat.unread = true;
    this.bumpChat(chat);
  }
  closeChat(id: string) {
    void this.closeChatDurably(id);
  }
  private async closeChatDurably(id: string) {
    const chat = this.chat(id);
    if (!chat) return;
    if (!chat.chatId) {
      this.addToast('No se cerró la conversación', 'La conversación no tiene una identidad durable.');
      return;
    }
    if (!(await this.ensureChatCreated(chat))) return;
    let pending = this.pendingChatDeleteOperations.get(id);
    if (!pending || pending.chatId !== chat.chatId) {
      pending = { chatId: chat.chatId, operationId: rid('delete-op') };
      this.pendingChatDeleteOperations.set(id, pending);
    }
    const operationId = pending.operationId;
    const receipt = await call('chatDelete', { tabId: chat.id, chatId: chat.chatId, operationId, force: true });
    const live = this.chat(id);
    if (!receipt?.ok || receipt.operationId !== operationId || !live || live.id !== chat.id || live.chatId !== chat.chatId) {
      if (this.chat(id) === chat) this.addToast('No se cerró la conversación', 'El daemon no confirmó la eliminación durable.');
      return;
    }
    if (this.pendingChatDeleteOperations.get(id)?.operationId === operationId) this.pendingChatDeleteOperations.delete(id);
    void browserApi()?.close(live.id);
    releaseDraftImages(live.draftImages ?? []);
    for (const item of live.queue ?? []) releaseDraftImages(item.draftImages ?? []);
    this.pendingChatCreates.delete(id);
    this.remoteChatCreateFences.delete(id);
    this.pendingChatCreateOperations.delete(id);
    this.pendingChatCreatePromises.delete(id);
    this.fullHistoriesLoaded.delete(id);
    this.fullHistoryLoads.delete(id);
    this.committedPresentationFingerprints.delete(id);
    this.pendingPresentationOperations.delete(id);
    this.pendingPresentationSnapshots.delete(id);
    this.committedRuntimeControlFingerprints.delete(id);
    this.pendingRuntimeControlOperations.delete(id);
    this.pendingRuntimeControlSaves.delete(id);
    this.pendingQueueMutationVersions.delete(id);
    this.pendingQueueSnapshots.delete(id);
    this.pendingQueueOperationIds.delete(id);
    this.state.chats = this.state.chats.filter((c) => c.id !== live.id);
    if (this.state.activeId === live.id) this.state.activeId = this.state.chats[0]?.id ?? null;
    if (this.state.chats.length === 0) this.newChat();
    else this.bumpApp();
    void this.flushSession(true);
  }
  setDraft(id: string, text: string) {
    const chat = this.chat(id);
    if (!chat) return;
    chat.draft = text;
    this.pendingDraftMutationVersions.set(chat.id, ++this.draftMutationRevision);
    this.pendingDraftSnapshots.set(chat.id, text);
    this.touchChat(chat.id);
    this.schedulePersist(); // no re-render; textarea owns its value
  }

  addDraftImages(id: string, images: DraftImage[]) {
    const chat = this.chat(id);
    if (!chat || images.length === 0) return;
    chat.draftImages = appendDraftImages(chat.draftImages, images);
    this.bumpApp(false);
  }
  removeDraftImages(id: string, imageIDs: Iterable<string>) {
    const chat = this.chat(id);
    if (!chat) return;
    const remove = new Set(imageIDs);
    const removed = (chat.draftImages ?? []).filter((image) => remove.has(image.id));
    const next = withoutDraftImages(chat.draftImages, remove);
    if (next.length === (chat.draftImages?.length ?? 0)) return;
    releaseDraftImages(removed);
    chat.draftImages = next.length ? next : undefined;
    this.bumpApp(false);
  }
  async ensureImageDraftSupport(id: string): Promise<boolean> {
    const chat = this.chat(id);
    if (!chat) return false;
    const move = this.workspaceMoves.current(chat.id);
    if (move && !(await move)) return false;
    if (!(await this.ensureChatCreated(chat))) return false;
    // Unknown is deliberately accepted. The daemon validates the actual target
    // provider at the turn boundary; rejecting here would discard a first image
    // merely because an inherited/previous provider still owns the session.
    return imageDraftCapability(chat.sessionId, chat.sessionProviderId, chat.providerId, chat.imageSupport) !== 'unsupported';
  }

  // ---- session ---------------------------------------------------------
  private async ensureSession(chat: Chat, refreshPlanUsage = false): Promise<void> {
    if (chat.sessionId && !refreshPlanUsage) return;
    if (chat._initPromise) return chat._initPromise;
    if (!(await this.ensureChatCreated(chat))) return;
    const controlRevision = chat._controlRevision ?? 0;
    const operationId = chat._sessionOperationId ?? (chat._sessionOperationId = rid('lane-select'));
    chat._initPromise = (async () => {
      const info = await call('appChatNewSession', {
        cwd: chat.cwd ?? null,
        tabId: chat.id,
        chatId: chat.chatId,
        operationId,
        providerId: chat.providerId ?? null,
        sessionId: refreshPlanUsage ? chat.sessionId ?? undefined : undefined,
        refreshPlanUsage,
      });
      if (!info || info.error) { chat.sessionError = info?.error ?? 'no bridge'; this.bumpApp(false); return; }
      chat._sessionOperationId = undefined;
      chat.sessionId = info.sessionId;
      chat.sessionProviderId = info.providerId ?? chat.providerId ?? null;
      chat.cwd = info.cwd ?? chat.cwd;
      chat.pending = false;
      chat.sessionError = undefined;
      // A plan-usage refresh can initialize the inherited provider while the
      // user is already picking another one. Keep the returned session id as
      // the daemon handover source, but never adopt its stale provider,
      // controls, capabilities, or model list over the explicit picker state.
      // startJob carries the newer provider/model/mode and performs the safe
      // engine handover at the turn boundary.
      if (modelControlsChangedDuringInit(controlRevision, chat._controlRevision)) {
        this.bumpApp(false);
        return;
      }
      // Record the provider the daemon actually bound (additive fields; absent on
      // an older daemon → fall back to the picked id + catalog name).
      if (info.providerId) chat.providerId = info.providerId;
      chat.providerName = info.providerName ?? this.providerName(chat.providerId) ?? chat.providerName ?? null;
      // R6 caps: image attach + agent-advertised slash commands (feature-detected;
      // the mock reports neither → both stay off).
      chat.imageSupport = !!info.imageSupport;
      chat.commands = Array.isArray(info.commands) && info.commands.length ? info.commands : undefined;
      if (info.commandCatalog !== undefined) chat.commandCatalog = info.commandCatalog ?? undefined;
      if (info.models?.length) this.state.models = info.models;
      if (info.modes?.length) this.state.modes = info.modes;
      const sessionModels = info.models?.length ? info.models : this.providerGroup(chat.providerId)?.models ?? this.state.models;
      const sessionModes = info.modes?.length ? info.modes : this.providerGroup(chat.providerId)?.modes ?? this.state.modes;
      const requestedSelection = resolveModelSelection([], sessionModels, chat.currentModelId ?? info.currentModelId);
      const defaultSelection = resolveModelSelection([], sessionModels, info.currentModelId);
      const target = requestedSelection.model ? requestedSelection : defaultSelection;
      let controlsChanged = false;
      if (chat.providerId && target.base) {
        controlsChanged = this.applyControlsForModel(chat, chat.providerId, target.base, sessionModels, sessionModes, {
          fallbackEffort: requestedSelection.effort ?? defaultSelection.effort,
          fallbackMode: chat.currentModeId,
          providerDefaultMode: info.currentModeId,
        });
      } else {
        if (!chat.currentModelId) chat.currentModelId = info.currentModelId;
        if (!chat.currentModeId) chat.currentModeId = info.currentModeId;
      }
      // Desired controls are actor-owned. The provider lane applies them inside
      // the next actor-owned turn effect; initializing a disposable session is
      // never permission to mutate native controls outside that journal.
      if (controlsChanged) {
        this.touchChat(chat.id);
        await this.persistRuntimeControls(chat);
      }
      this.bumpApp(false);
    })();
    try { await chat._initPromise; } finally { chat._initPromise = undefined; }
  }
  // Account-plan refresh is a provider metadata RPC. It may initialize/resume
  // the active Claude/Codex session, but never sends session/prompt and never
  // consumes model tokens. Concurrent open/init/send paths share _initPromise.
  refreshPlanUsage(chatId: string): void {
    const chat = this.chat(chatId);
    if (!chat || !this.isConnected() || (chat.providerId !== 'claude' && chat.providerId !== 'codex')) return;
    const providerId = chat.providerId;
    if (this.state.planUsageLoadingByProvider[providerId]) return;
    this.state.planUsageLoadingByProvider[providerId] = true;
    this.bumpApp(false);
    void (async () => {
      try {
        if (has('appChatRefreshPlanUsage')) {
          // A provider RPC failure must not fall through into chat session
          // creation: metadata availability can fail independently, while the
          // selected chat/provider binding remains untouched.
          await call('appChatRefreshPlanUsage', providerId, chat.id);
        } else {
          // Rolling-upgrade fallback: an older bridge lacks the provider metadata
          // channel. Preserve its exact-chat behavior until both halves are new.
          await this.ensureSession(chat, true);
        }
      } finally {
        if (this.state.planUsageLoadingByProvider[providerId]) {
          this.state.planUsageLoadingByProvider[providerId] = false;
          this.bumpApp(false);
        }
      }
    })();
  }
  async setModel(chatId: string, modelId: string) {
    const chat = this.chat(chatId); if (!chat) return;
    const previous = this.rememberCurrentControls(chat);
    const group = this.providerGroup(chat.providerId);
    const selected = resolveModelSelection(group ? [group] : [], group?.models ?? this.state.models, modelId);
    if (chat.providerId && selected.base) {
      this.applyControlsForModel(chat, chat.providerId, selected.base, group?.models ?? this.state.models, group?.modes ?? this.state.modes, {
        fallbackEffort: previous.effort,
        fallbackMode: previous.modeId,
        explicitEffort: selected.effort,
      });
    } else {
      chat.currentModelId = modelId;
    }
    chat._controlRevision = nextModelControlRevision(chat._controlRevision);
    this.bumpChat(chat);
    await this.persistRuntimeControls(chat);
  }
  // Grouped picker selection. On a NEW chat (no session yet) the picked group's
  // providerId binds the chat and rides app-chat:new-session at creation. On an
  // EXISTING chat the provider is already fixed, so this is just set-model for
  // the next turn (existing semantics).
  async pickModel(chatId: string, providerId: string, modelId: string) {
    const chat = this.chat(chatId); if (!chat) return;
    const previous = this.rememberCurrentControls(chat);
    // Chats are NOT bound to one agent for life (user law 2026-07-11): picking
    // another provider's model selects another lane — the daemon performs the
    // transaction on the next turn (startJob carries providerId), resuming that
    // provider's exact thread and importing only through a verified non-sampling
    // capability. The native control is applied only inside that actor-owned
    // turn effect, never as an unjournaled session-id side write.
    chat.providerId = providerId || chat.providerId || null;
    chat.providerName = this.providerName(chat.providerId) ?? chat.providerName ?? null;
    const group = this.providerGroup(chat.providerId);
    if (chat.providerId && group) {
      this.applyControlsForModel(chat, chat.providerId, modelId, group.models, group.modes, {
        fallbackEffort: previous.effort,
        fallbackMode: previous.modeId,
      });
    } else {
      chat.currentModelId = modelId;
    }
    chat._controlRevision = nextModelControlRevision(chat._controlRevision);
    this.bumpChat(chat);
    await this.persistRuntimeControls(chat);
    this.refreshPlanUsage(chat.id);
  }
  async setModeSel(chatId: string, modeId: string) {
    const chat = this.chat(chatId); if (!chat) return;
    chat.currentModeId = modeId;
    this.rememberCurrentControls(chat);
    chat._controlRevision = nextModelControlRevision(chat._controlRevision);
    this.bumpChat(chat);
    await this.persistRuntimeControls(chat);
  }

  // ---- send / cancel ---------------------------------------------------
  async sendTo(chatId: string, prompt: string, images?: StartJobOpts['images'], submittedDraft = prompt): Promise<boolean> {
    const chat = this.chat(chatId);
    if (!chat) return false;
    // Claim the exact composer value synchronously. This is the ownership
    // boundary: after it, the user
    // row (or failed-send row) owns the prompt, so a fast tab switch must see an
    // empty draft. Do not erase text typed while attachments were preparing.
    if (chat.draft === submittedDraft) this.setDraft(chat.id, '');
    return this._send(chat, prompt, images);
  }
  // Steer the running turn if the bridge supports it, else queue locally and
  // auto-send at turn end (R2). No-op when the chat is idle → normal send.
  async steerOrQueue(chatId: string, prompt: string, images?: StartJobOpts['images']): Promise<boolean> {
    const chat = this.chat(chatId);
    if (!chat || !prompt.trim()) return false;
    // Offline: steering/queuing into a dead socket would silently vanish. Keep
    // the draft intact; the banner already explains why sending is blocked.
    if (!this.isConnected()) return false;
    if (!this.isChatRunning(chat.id)) return this._send(chat, prompt, images);
    if (has('appChatSteer') && chat.sessionId) {
      const steerSessionId = chat.sessionId;
      const behavior = steeringBehavior(chat.providerId);
      // Native Codex commits input between sampling steps; the packaged Claude
      // host commits it at the result that closes the pre-steer segment.
      // Persist a hidden user/continuation pair now, but keep the current
      // assistant row streaming until the client-id receipt proves the semantic
      // boundary. Generic ACP capability steering retains its existing
      // immediate split contract.
      const now = new Date().toISOString();
      const activeAssistant = [...chat.messages].reverse().find((message) => message.role === 'assistant' && message.status === 'running');
      if (!activeAssistant) return false;
      const boundary: {
        assistantMessageId: string;
        contentOffset: number;
        resultOffset: number;
        eventCount: number;
        deferUntilConsumed?: boolean;
      } = {
        assistantMessageId: activeAssistant.id,
        contentOffset: activeAssistant.content.length,
        resultOffset: activeAssistant.result?.length ?? 0,
        eventCount: activeAssistant.events.length,
      };
      const pendingUser: Msg = {
        // The daemon persists this row redacted (BeginLiveSteer); paint the
        // same bytes so the settle/reload swap never rewrites visible text.
        id: rid('u'), role: 'user', content: redactSensitiveText(prompt), status: 'pending', at: now,
        events: [], images: messageImages(images),
      };
      const continuation: Msg = {
        id: rid('a'), role: 'assistant', content: '', status: 'running', at: null,
        events: [], jobId: activeAssistant.jobId, turnStartedAt: activeAssistant.turnStartedAt,
      };
      const stagedNativeBoundary = steeringStagesBoundary(behavior);
      if (stagedNativeBoundary) boundary.deferUntilConsumed = true;
      const inserted = stagedNativeBoundary
        ? stageChronologicalSteer(chat.messages, pendingUser, continuation)
        : insertChronologicalSteer(chat.messages, pendingUser, continuation);
      if (!inserted) return false;
      if (!stagedNativeBoundary) this.rebuildJobRefs();
      this.setDraft(chat.id, '');
      this.bumpChat(chat);
      return this.steerDispatches.run(chat.id, async () => {
        // The daemon persists the same staged ownership before turn/steer. For
        // native hosts it advances that boundary on the canonical consumption
        // receipt; older/generic adapters retain acknowledgement-time behavior.
        const r = await call('appChatSteer', steerSessionId, prompt, images ?? [], pendingUser.id, continuation.id, boundary);
        // Queue persistence and the actor digest may replace the renderer Chat
        // object while this native acknowledgement is in flight. Identity is
        // the immutable tab+chat pair, not the JavaScript object reference: the
        // replacement carries the same stable steer ids and is now the only
        // visible owner. Returning false here made Composer restore the already-
        // delivered text beside the send button after a delayed queue -> steer.
        const live = this.chat(chatId);
        if (!live || live.chatId !== chat.chatId) return false;
        if (r?.strategy === 'uncertain') {
          this.addToast('Steering no confirmado', r.error ?? 'El agente no confirmó el steer; no se reenvió para evitar duplicarlo.');
          // A timeout is explicitly not a rejection. Keep the same visible owner,
          // settle its spinner, and let a late client-id receipt upgrade it.
          markPendingSteerUncertain(live.messages, pendingUser.id);
          this.bumpChat(live);
          await this.flushSession();
          return true;
        }
        if (stagedNativeBoundary && !hasSteerConsumptionReceipt(r)) {
          // Compatibility with older patched adapters that cannot echo the
          // canonical client id: acknowledgement is their strongest boundary.
          if (commitChronologicalSteer(live.messages, pendingUser.id)) this.rebuildJobRefs();
        }
        const destination = steeringDestination(r);
        if (destination === 'queue') {
          // An explicit rejection transfers ownership in one structural commit:
          // remove the pending chronological row, rejoin adjacent assistant
          // segments, then create exactly one FIFO row.
          const removed = rejectChronologicalSteer(live.messages, pendingUser.id);
          // A receipt/acknowledgement may have beaten a late response. Once that
          // happens the transcript row owns delivery and cannot be duplicated.
          if (!removed) return true;
          this.rebuildJobRefs();
          if (r?.daemonQueued !== true) this.enqueue(live, prompt, images);
          await this.flushSession();
          if (!this.isChatRunning(live.id)) void this.flushNextQueued(live);
          return true;
        }
        // {turnId} proves admission and permanently owns delivery. The later
        // userMessage.clientId receipt chooses the transcript boundary and
        // upgrades feedback; neither outcome is ever replayed through FIFO.
        acceptPendingSteer(live.messages, pendingUser.id);
        this.bumpChat(live);
        await this.flushSession();
        return true;
      });
    }
    this.enqueue(chat, prompt, images);
    if (!this.isChatRunning(chat.id)) void this.flushNextQueued(chat);
    return true;
  }
  private enqueue(chat: Chat, prompt: string, images?: StartJobOpts['images']) {
    // Queue text is persisted through the daemon (which redacts at ingestion)
    // and is what the dispatch eventually sends; store the same redacted bytes
    // locally so neither hydration nor dispatch changes the visible row.
    (chat.queue ??= []).push(queuedMessage(rid('q'), redactSensitiveText(prompt), messageImages(images)));
    this.markQueueMutation(chat);
    this.bumpChat(chat);
    this.scheduleQueuePersist();
  }
  queueDraftMessage(chatId: string, prompt: string, drafts: DraftImage[]): boolean {
    const chat = this.chat(chatId);
    if (!chat || !prompt.trim() || !this.isConnected() || !this.isChatRunning(chat.id)) return false;
    const selected = new Set(drafts.map((draft) => draft.id));
    const owned = (chat.draftImages ?? []).filter((draft) => selected.has(draft.id));
    const item = owned.length
      ? queuedDraftMessage(rid('q'), redactSensitiveText(prompt), owned)
      : queuedMessage(rid('q'), redactSensitiveText(prompt));
    (chat.queue ??= []).push(item);
    this.setDraft(chat.id, '');
    if (owned.length) {
      const remaining = withoutDraftImages(chat.draftImages, selected);
      chat.draftImages = remaining.length ? remaining : undefined;
    }
    // The row and zero-copy previews paint now. Encoding starts only after that
    // commit and never allows the FIFO drainer to send a partial attachment.
    this.markQueueMutation(chat);
    this.bumpChat(chat);
    this.scheduleQueuePersist();
    if (owned.length) void this.prepareQueuedAttachments(chat.id, item.id);
    // The turn can end between the running check above and this push: job:end
    // already drained an empty FIFO, so nothing else would ever pick this row
    // up. Every other queue mutator ends with the same recheck.
    else if (!this.isChatRunning(chat.id)) void this.flushNextQueued(chat);
    return true;
  }
  private async prepareQueuedAttachments(chatId: string, itemId: string) {
    const initial = this.chat(chatId)?.queue?.find((item) => item.id === itemId);
    const drafts = initial?.draftImages;
    if (!initial || !drafts?.length) return;
    initial.attachmentState = 'preparing';
    initial.attachmentError = undefined;
    this.markQueueMutation(this.chat(chatId) ?? { id: chatId, queue: undefined });
    this.bumpChat(chatId);
    this.scheduleQueuePersist();
    try {
      await attachmentWorkBoundary();
      const images = await draftImagePayloads(drafts, undefined, attachmentWorkBoundary);
      const chat = this.chat(chatId);
      const item = chat?.queue?.find((queued) => queued.id === itemId);
      if (!chat || !item || item.draftImages !== drafts) return;
      if (images.length !== drafts.length) throw new Error('Una o más imágenes no se pudieron leer.');
      item.images = messageImages(images);
      item.attachmentState = 'ready';
      item.attachmentError = undefined;
      this.markQueueMutation(chat);
      this.bumpChat(chat);
      this.scheduleQueuePersist();
      // Encoding completion is the durability boundary for a queued image. Do
      // not leave the only full-resolution bytes behind the 600ms debounce.
      void this.flushSession();
      if (!this.isChatRunning(chat.id)) void this.flushNextQueued(chat);
    } catch (error) {
      const item = this.chat(chatId)?.queue?.find((queued) => queued.id === itemId);
      if (!item || item.draftImages !== drafts) return;
      item.attachmentState = 'failed';
      item.attachmentError = error instanceof Error ? error.message : 'No se pudieron preparar los adjuntos.';
      this.markQueueMutation(this.chat(chatId) ?? { id: chatId, queue: undefined });
      this.bumpChat(chatId);
      this.scheduleQueuePersist();
    }
  }
  retryQueuedAttachments(chatId: string, id: string) {
    const item = this.chat(chatId)?.queue?.find((queued) => queued.id === id);
    if (!item?.draftImages?.length || item.attachmentState !== 'failed') return;
    void this.prepareQueuedAttachments(chatId, id);
  }
  // Live-save an edit (double-click on a queue item). Mirrors setDraft: persist
  // without a re-render, since the editing textarea owns its own value.
  editQueued(chatId: string, id: string, text: string) {
    const item = this.chat(chatId)?.queue?.find((q) => q.id === id);
    if (!item) return;
    item.text = text;
    this.markQueueMutation(this.chat(chatId) ?? { id: chatId, queue: undefined });
    this.touchChat(chatId);
    this.scheduleQueuePersist();
  }
  removeQueued(chatId: string, id: string) {
    const chat = this.chat(chatId); if (!chat?.queue) return;
    const removed = chat.queue.find((q) => q.id === id);
    releaseDraftImages(removed?.draftImages ?? []);
    chat.queue = chat.queue.filter((q) => q.id !== id);
    this.markQueueMutation(chat);
    this.bumpChat(chat);
    this.scheduleQueuePersist();
    if (chat.queue.length && !this.isChatRunning(chat.id)) void this.flushNextQueued(chat);
  }
  // Drag-reorder: move `id` to `toIndex` (an index in the CURRENT array, before
  // removal); the splice adjusts for the removed slot.
  reorderQueued(chatId: string, id: string, toIndex: number) {
    const chat = this.chat(chatId); if (!chat?.queue) return;
    const from = chat.queue.findIndex((q) => q.id === id);
    if (from < 0) return;
    const arr = chat.queue.slice();
    const [item] = arr.splice(from, 1);
    let to = from < toIndex ? toIndex - 1 : toIndex;
    to = Math.max(0, Math.min(arr.length, to));
    arr.splice(to, 0, item);
    chat.queue = arr;
    this.markQueueMutation(chat);
    this.bumpChat(chat);
    this.scheduleQueuePersist();
    if (!this.isChatRunning(chat.id)) void this.flushNextQueued(chat);
  }
  cancelQueued(chatId: string) {
    const chat = this.chat(chatId); if (!chat?.queue?.length) return;
    for (const item of chat.queue) releaseDraftImages(item.draftImages ?? []);
    chat.queue = [];
    this.markQueueMutation(chat);
    this.bumpChat(chat);
    this.scheduleQueuePersist();
  }
  private async flushNextQueued(chat: Chat): Promise<void> {
    if (this.drainingQueues.has(chat.id) || this.isChatRunning(chat.id)) return;
    const next = chat.queue?.[0];
    if (!next) return;
    // Agent-authored rows are drained by the daemon so they work headlessly.
    // User and agent rows still share one FIFO and wait behind one another.
    if (next.source === 'agent' || next.source === 'host') return;
    if (!queuedAttachmentsReady(next)) return;
    this.drainingQueues.add(chat.id);
    let accepted = false;
    try {
      const payload = queuedJob(next);
      // Keep the exact FIFO owner until job:start has accepted the turn. A
      // failed/throwing send leaves the same object at the head for retry.
      accepted = await this._send(chat, payload.prompt, payload.images, next.id, {
        userId: next.id,
        assistantId: `queue-assistant-${next.id}`,
      });
      if (!accepted) return;
      // A hydration during the send replaces every chat object wholesale
      // (restoreSessionSnapshot). Removing the accepted row from the captured
      // object would then strand it in the live chat's FIFO, where it looks
      // stuck forever and is re-sent as a duplicate at the next turn end.
      const live = this.chat(chat.id) ?? chat;
      const current = live.queue ?? [];
      if (!current.some((item) => item.id === next.id)) return;
      const remaining = afterQueuedAcceptance(current, next.id, true);
      live.queue = remaining.length ? remaining : undefined;
      releaseDraftImages(next.draftImages ?? []);
      this.markQueueMutation(live);
      this.bumpChat(live);
      this.scheduleQueuePersist();
    } finally {
      this.drainingQueues.delete(chat.id);
      // A deterministic/very fast agent may emit `end` before the job:start
      // reply resolves. That end handler observes this drain lock and returns;
      // continue here once the accepted item has been removed. Never do this
      // after a failed start, or an unavailable session would retry forever.
      const tail = this.chat(chat.id) ?? chat;
      if (accepted && tail.queue?.length && !this.isChatRunning(tail.id)) void this.flushNextQueued(tail);
    }
  }
  private async _send(
    chat: Chat,
    prompt: string,
    images?: StartJobOpts['images'],
    queueId?: string,
    identity?: { userId: string; assistantId: string },
  ): Promise<boolean> {
    if (!prompt.trim() || this.isChatRunning(chat.id)) return false;
    const move = this.workspaceMoves.current(chat.id);
    if (move && !(await move)) return false;
    if (this.chat(chat.id) !== chat || this.isChatRunning(chat.id)) return false;
    const now = new Date().toISOString();
    // Offline: never queue into a dead socket. Record the turn honestly as a
    // failed send with a retry affordance rather than an eternal "Trabajando…".
    // (The composer disables Enviar while offline; this covers the send-clicked-
    // as-the-socket-drops race and any programmatic send.)
    // The daemon persists the user row redacted at ingestion (PrepareTurn);
    // paint the same bytes locally or the next hydration/reload visibly
    // rewrites the sentence. The provider still receives the raw prompt.
    const display = redactSensitiveText(prompt);
    const userId = identity?.userId ?? rid('u');
    const assistantId = identity?.assistantId ?? rid('a');
    if (!this.isConnected()) {
      const user: Msg = { id: userId, role: 'user', content: display, status: 'done', at: now, events: [], images: messageImages(images) };
      const asst: Msg = { id: assistantId, role: 'assistant', content: '', status: 'failed', at: now, events: [], interrupted: true, retryPrompt: prompt };
      chat.messages.push(user, asst);
      if (!chat.titleLocked) { chat.title = display.trim().slice(0, 34) || chat.title; chat.titleLocked = true; }
      this.bumpChat(chat);
      return false;
    }
    // Kick the heartbeat so a send into a just-dropped socket is caught fast
    // (the health poll alone could lag a few seconds).
    this.monitor?.probeNow();
    const priorUser = chat.messages.find((message) => message.id === userId && message.role === 'user');
    const priorAssistant = chat.messages.find((message) => message.id === assistantId && message.role === 'assistant');
    const user: Msg = priorUser ?? { id: userId, role: 'user', content: display, status: 'done', at: now, events: [], images: messageImages(images) };
    const asst: Msg = priorAssistant ?? { id: assistantId, role: 'assistant', content: '', status: 'running', at: null, events: [], turnStartedAt: Date.now() };
    if (priorUser) {
      user.content = display;
      user.status = 'done';
      user.images = messageImages(images);
    }
    if (priorAssistant?.status === 'failed') {
      asst.content = '';
      asst.result = undefined;
      asst.status = 'running';
      asst.at = null;
      asst.events = [];
      asst.interrupted = undefined;
      asst.retryPrompt = undefined;
      asst.jobId = undefined;
      asst.turnStartedAt = Date.now();
    }
    if (!priorUser) chat.messages.push(user);
    if (!priorAssistant) chat.messages.push(asst);
    if (!chat.titleLocked) { chat.title = display.trim().slice(0, 34) || chat.title; chat.titleLocked = true; }
    this.bumpChat(chat);

    await this.ensureSession(chat);
    if (!chat.sessionId && chat.sessionError) {
      asst.status = 'failed';
      asst.content = `No hay sesión ACP disponible (${chat.sessionError}).`;
      asst.retryPrompt = prompt;
      this.bumpChat(chat);
      return false;
    }
    // Stable conversation id (R4): every turn in this chat rides the SAME chatId
    // so the daemon accumulates checkpoints and increments turnSeq under it. A
    // fresh id per turn (the old behaviour) scattered checkpoints and reset the
    // sequence, making rewind/diff unusable.
    const chatId = chat.chatId ?? (chat.chatId = newChatConvId());
    this.chatJobs.set(chatId, { tabId: chat.id, msgId: asst.id });
    const job = await call('startJob', {
      kind: 'app-chat', operationId: userId, title: `Devin · ${chat.title}`, chatId, tabId: chat.id,
      sessionId: chat.sessionId || undefined, cwd: chat.cwd ?? null,
      // providerId rides every turn: when it differs from the session's bound
      // provider, the daemon treats it as a desired-lane selection. It starts
      // only after a verified non-sampling context import; unsupported switches
      // fail before the active provider lane is detached.
      providerId: chat.providerId ?? undefined,
      modelId: chat.currentModelId, modeId: chat.currentModeId,
      prompt, images: images && images.length ? images : undefined,
      userMessageId: userId, assistantMessageId: assistantId,
      queueId,
      busyMode: 'queue-v1',
    });
    if (isQueuedJobStart(job)) {
      const current = this.chat(chat.id) ?? chat;
      const reconciled = reconcileQueuedJobStart(
        current,
        { userId: user.id, assistantId: asst.id },
        display,
        messageImages(images),
        job,
      );
      if (!reconciled.alreadyStarted) this.chatJobs.delete(chatId);
      this.bumpChat(current);
      return true;
    }
    if (job?.id) {
      asst.jobId = job.id;
      this.jobRef.set(job.id, { tabId: chat.id, msgId: asst.id });
      return true;
    }
    if (!job) {
      asst.status = 'failed';
      asst.content = 'startJob no disponible.';
      asst.retryPrompt = prompt;
      this.bumpChat(chat);
    }
    return false;
  }
  cancelActive() {
    const chat = this.active(); if (!chat) return;
    void this.cancelChatTurn(chat.id);
  }
  async cancelChatTurn(chatId: string) {
    const chat = this.chat(chatId); if (!chat) return;
    const running = [...chat.messages].reverse().find((m) => m.status === 'running');
    if (!running) return;
    if (!running.jobId) {
      this.finalizeCancelledLocally(chat, running);
      return;
    }
    const result = await call('cancelJob', running.jobId);
    const cancelled = result === true
      || (!!result && typeof result === 'object' && (result as { cancelled?: unknown }).cancelled === true);
    if (cancelled) return;
    const refused = result === false
      || (!!result && typeof result === 'object' && (result as { cancelled?: unknown }).cancelled === false);
    if (refused) this.finalizeCancelledLocally(chat, running, running.jobId);
  }

  private finalizeCancelledLocally(chat: Chat, running: Msg, jobId?: string) {
    running.status = 'cancelled';
    running.at = new Date().toISOString();
    settleTerminalToolEvents(running.events, 'cancelled');
    if (running.permission && !running.permission.resolved) running.permission = undefined;
    if (jobId) this.jobRef.delete(jobId);
    for (const [id, ref] of this.chatJobs) {
      if (ref.tabId === chat.id && ref.msgId === running.id) this.chatJobs.delete(id);
    }
    this.bump('msg:' + running.id);
    this.bumpChat(chat);
    this.recoverIdleQueues();
    this.scheduleScopedSync(['session', 'permissions']);
  }
  async decidePermission(tabId: string, msgId: string, permId: string, optionId: string) {
    const msg = this.chat(tabId)?.messages.find((candidate) => candidate.id === msgId); if (!msg) return;
    if (msg.permission) msg.permission.resolved = optionId;
    this.bump('msg:' + msgId);
    await call('chatPermissionDecide', permId, optionId);
    if (msg.permission?.id === permId) { msg.permission = undefined; this.bump('msg:' + msgId); }
  }

  // ---- event handlers --------------------------------------------------
  private msgForRef(ref: { tabId: string; msgId: string } | undefined): Msg | null {
    if (!ref) return null;
    return this.chat(ref.tabId)?.messages.find((message) => message.id === ref.msgId) ?? null;
  }
  private resolveMsg(id: string): Msg | null {
    return this.msgForRef(this.jobRef.get(id));
  }
  private synthesizeCanonicalJobRows(chat: Chat, job: PublicJob, terminal = false): Msg | null {
    const userID = typeof job.userMessageId === 'string' ? job.userMessageId.trim() : '';
    const assistantID = typeof job.assistantMessageId === 'string' ? job.assistantMessageId.trim() : '';
    if (!userID || !assistantID) return null;
    const startedAt = job.startedAt || new Date().toISOString();
    let user = chat.messages.find((message) => message.id === userID);
    let assistant = chat.messages.find((message) => message.id === assistantID);
    if (!user) {
      user = {
        id: userID,
        role: 'user',
        content: typeof job.promptText === 'string' ? job.promptText : '',
        status: 'done',
        at: startedAt,
        events: [],
      };
      const assistantIndex = assistant ? chat.messages.indexOf(assistant) : -1;
      if (assistantIndex >= 0) chat.messages.splice(assistantIndex, 0, user);
      else chat.messages.push(user);
    }
    if (!assistant) {
      assistant = {
        id: assistantID,
        role: 'assistant',
        content: '',
        result: terminal && job.result ? job.result : undefined,
        status: terminal ? terminalMessageStatus(job) : 'running',
        at: terminal ? (job.finishedAt ?? new Date().toISOString()) : null,
        jobId: job.id,
        events: [],
        turnStartedAt: terminal ? undefined : Date.parse(startedAt) || Date.now(),
      };
      const userIndex = chat.messages.indexOf(user);
      chat.messages.splice(userIndex + 1, 0, assistant);
    }
    return assistant.role === 'assistant' ? assistant : null;
  }

  private onJobEvent(e: JobEvent) {
    switch (e.type) {
      case 'start': {
        const chatId = e.job.chatId ?? '';
        const rec = this.chatJobs.get(chatId);
        let msg = this.msgForRef(rec) ?? this.resolveMsg(e.job.id);
        if (!msg && e.job.tabId) {
          const chat = this.chat(e.job.tabId);
          msg = chat ? ([...chat.messages].reverse().find((m) => m.jobId === e.job.id || m.status === 'running') ?? null) : null;
          if (chat && msg && chatId) this.chatJobs.set(chatId, { tabId: chat.id, msgId: msg.id });
        }
        const chat = e.job.tabId ? this.chat(e.job.tabId) : (chatId ? this.chatByConvId(chatId) : null);
        if (chat) this.attachJobSession(chat, e.job);
        // New work in a shelved chat retires the shelf override: T3's server
        // auto-unsettles on real activity, and a chat you just started a turn
        // in has plainly stopped being history.
        const startIsLive = !msg || !isTerminalMessage(msg);
        if (startIsLive && chat && (chat.settled || chat.settledAt)) {
          chat.settled = undefined;
          chat.settledAt = undefined;
          this.markPresentationMutation(chat);
          this.touchChat(chat.id);
          this.schedulePersist();
        }
        if (!msg && chat) {
          msg = this.synthesizeCanonicalJobRows(chat, e.job);
          if (!msg) this.scheduleScopedSync(['session', 'permissions']);
        }
        if (msg && chat) {
          msg.jobId = e.job.id;
          if (!isTerminalMessage(msg)) msg.status = 'running';
          this.jobRef.set(e.job.id, { tabId: chat.id, msgId: msg.id });
          if (chatId) this.chatJobs.set(chatId, { tabId: chat.id, msgId: msg.id });
          this.bumpApp(false);
        }
        break;
      }
      case 'data': {
        if (e.stream !== 'stdout') return;      // stderr/system stay off the transcript
        const msg = this.resolveMsg(e.id);
        if (!msg) return;
        appendAssistantChunk(msg, e.chunk, e.phase);
        if (!isTerminalMessage(msg)) msg.status = 'running';
        this.bump('msg:' + msg.id);             // <-- isolated streaming re-render
        break;
      }
      case 'assistant-media': {
        const msg = this.resolveMsg(e.id);
        if (!msg) return;
        msg.images = mergeMessageImages(msg.images, e.images);
        if (!isTerminalMessage(msg)) msg.status = 'running';
        this.bump('msg:' + msg.id);
        break;
      }
      case 'acp': {
        if (e.event.kind === 'steer-consumed') {
          const ref = this.jobRef.get(e.id);
          const chat = ref ? this.chat(ref.tabId) : null;
          const committed = chat
            ? commitChronologicalSteer(chat.messages, e.event.clientUserMessageId)
            : undefined;
          if (committed) this.rebuildJobRefs();
          const settled = chat
            ? settlePendingSteer(chat.messages, e.event.clientUserMessageId, 'transcript')
            : undefined;
          if (chat && (committed || settled)) {
            this.touchChat(chat.id);
            this.bumpApp(false);
            void this.flushSession();
          }
          return;
        }
        if (e.event.kind === 'turn-heartbeat') {
          const pulse = e.event;
          this.turnHeartbeats.set(e.id, {
            elapsedMs: Number(pulse.elapsedMs ?? 0),
            outputTokens: Number(pulse.outputTokens ?? 0),
            phase: String(pulse.phase ?? ''),
            toolName: pulse.toolName ? String(pulse.toolName) : undefined,
            retry: pulse.retry ?? null,
            at: Date.now(),
          });
          const pulseMsg = this.resolveMsg(e.id);
          if (pulseMsg) this.bump('msg:' + pulseMsg.id);   // isolated re-render, like streaming
          return;
        }
        let msg = this.resolveMsg(e.id);
        if (!msg) return;
        if (e.event.kind === 'tool') {
          const toolEvent = e.event;
          const ref = this.jobRef.get(e.id);
          const chat = ref ? this.chat(ref.tabId) : null;
          const prior = chat?.messages.find((candidate) => candidate.role === 'assistant'
            && candidate.jobId === e.id
            && candidate.events.some((event) => event.kind === 'tool'
              && ((!!toolEvent.id && event.id === toolEvent.id) || (!!toolEvent.terminalId && event.terminalId === toolEvent.terminalId))));
          if (prior) msg = prior;
        }
        // Keep the newest plan on the CHAT too: its emitting message eventually
        // ages out of the retained window, and losing it blanked the rail's
        // step-by-step for the rest of the conversation.
        if (e.event.kind === 'plan') {
          const planRef = this.jobRef.get(e.id);
          const planChat = planRef ? this.chat(planRef.tabId) : null;
          if (planChat) {
            planChat.planLatest = e.event.entries.map((en) => ({
              status: String(en.status ?? ''), content: String(en.content ?? ''),
            }));
            planChat.planLatestMessageId = msg.id;
          }
        }
        this.applyAcp(msg, e.event);
        this.bump('msg:' + msg.id);
        this.bump(ACT);   // Tareas rail: live plan/tool activity
        break;
      }
      case 'usage': {
        // Normalize the usage signal for the context meter (R3). The daemon
        // forwards the ACP top-level used/size (populated for both native
        // provider and mock providers); fall back to token counts when a
        // provider only reports those.
        const used = e.used > 0 ? e.used : tokenTotal(e.inputTokens, e.outputTokens);
        const size = e.size > 0 ? e.size : 0;
        const usage = normalizeContextUsage({ used, size, updatedAt: e.updatedAt });
        let touched = false;
        if (usage) {
          for (const c of this.state.chats) {
            if (!contextUsageIdentityMatches(e, c)) continue;
            const providerId = e.providerId ?? c.sessionProviderId ?? c.providerId;
            if (!providerId) continue;
            c.contextUsageByProvider = withContextUsage(c.contextUsageByProvider, providerId, usage);
            this.touchChat(c.id);
            touched = true;
          }
        }
        if (touched) this.bumpApp();   // repaint + persist; usage is infrequent (per-turn)
        // Additive: usage events may also carry the provider's plan-usage snapshot.
        if (e.planUsage) this.onPlanUsage(e.planUsage);
        break;
      }
      case 'end': {
        this.turnHeartbeats.delete(e.job.id);
        const rec = this.chatJobs.get(e.job.chatId ?? '');
        let msg = this.msgForRef(rec) ?? this.resolveMsg(e.job.id);
        const jobChat = (e.job.tabId ? this.chat(e.job.tabId) : null) ?? (e.job.chatId ? this.chatByConvId(e.job.chatId) : null);
        const reattached = jobChat ? this.attachJobSession(jobChat, e.job) : false;
        if (!msg && jobChat) {
          msg = this.synthesizeCanonicalJobRows(jobChat, e.job, true);
          if (!msg) this.scheduleScopedSync(['session', 'permissions']);
        }
        if (jobChat) {
          // The terminal job snapshot is the reconnect-safe source of truth if
          // the live receipt event raced a controller disconnect.
          for (const clientUserMessageId of e.job.consumedSteerIds ?? []) {
            commitChronologicalSteer(jobChat.messages, clientUserMessageId);
            settlePendingSteer(jobChat.messages, clientUserMessageId, 'transcript');
          }
          settleStagedSteersAtTurnEnd(jobChat.messages);
          // A renderer reload can lose only the JavaScript waiter, never the
          // stable transcript owner. End its spinner honestly at the native
          // boundary; do not replay an outcome whose transport is unknown.
          settleSendingSteersAtTurnEnd(jobChat.messages);
          this.rebuildJobRefs();
          msg = this.resolveMsg(e.job.id) ?? msg;
        }
        if (msg) {
          const cancelled = e.job.code === 130 || e.job.stopReason === 'cancelled';
          const terminalStatus: Msg['status'] = cancelled ? 'cancelled' : e.job.status === 'done' ? 'done' : 'failed';
          msg.jobId = e.job.id;
          // Recovery only. The daemon's terminal result carries the WHOLE turn
          // (all commentary plus the final answer, across steer splits), so a
          // turn we streamed ourselves must keep its own typed split; adopting
          // it here reprinted every byte of the turn under the answer whenever
          // a steer or a stop ended it before a final_answer chunk arrived.
          const streamedSegments = jobChat
            ? jobChat.messages.filter((candidate) => candidate.role === 'assistant'
              && (candidate.jobId === e.job.id || candidate.id === msg.id))
            : [msg];
          if (e.job.result != null && adoptsTerminalJobResult(streamedSegments)) msg.result = e.job.result;
          if (jobChat) {
            for (const candidate of jobChat.messages) {
              if (candidate.role === 'assistant' && candidate.jobId === e.job.id) {
                settleTerminalToolEvents(candidate.events, terminalStatus);
              }
            }
          }
          settleTerminalToolEvents(msg.events, terminalStatus);
          msg.status = terminalStatus;
          // The daemon reports its own interruptions now, so a restart-killed
          // turn reads as interrupted here exactly as it does after rehydration.
          if (e.job.interrupted) msg.interrupted = true;
          if (msg.status === 'failed' && !msg.content && !msg.result) msg.content = e.job.error ? `Error: ${e.job.error}` : 'La tarea falló.';
          msg.at = e.job.finishedAt ?? new Date().toISOString();
          if (e.job.images?.length) {
            msg.images = mergeMessageImages(msg.images, e.job.images);
          }
          if (msg.permission && !msg.permission.resolved) msg.permission = undefined;
          this.jobRef.delete(e.job.id);
          this.bump('msg:' + msg.id);
        }
        this.chatJobs.delete(e.job.chatId ?? '');
        void reattached;
        const endedChat = (rec ? this.chat(rec.tabId) : null) ?? jobChat;
        if (endedChat) this.touchChat(endedChat.id);
        this.bumpApp(false);
        // A turn that finished in a chat you were not looking at is the only
        // thing that makes a chat unread. Nothing set the flag before — only
        // switchChat cleared it — so the sidebar's finished cue (v2's "Listo"
        // pill, v1's dot) could never fire outside a hand-seeded state. It
        // means UNSEEN, not merely finished: opening the chat clears it. A
        // cancel is your own doing, so it is not news to come back to.
        // A turn the daemon itself ended is not news either: every chat would
        // come back from a restart wearing an unseen-result cue for something
        // that never finished.
        if (endedChat && endedChat.id !== this.state.activeId && !e.job.interrupted
          && e.job.code !== 130 && e.job.stopReason !== 'cancelled' && !endedChat.unread) {
          endedChat.unread = true;
          this.touchChat(endedChat.id);
          this.bumpApp(false);
        }
        // R4: a turn just finished — refresh this chat's checkpoints so the
        // per-turn Deshacer/Revisar affordances light up (feature-detected).
        if (endedChat) void this.refreshCheckpoints(endedChat);
        if (endedChat?.queue?.length && !this.isChatRunning(endedChat.id)) {
          // R2: flush one explicitly queued/rejected follow-up at a time. An
          // acknowledged or transport-uncertain native steer never enters FIFO.
          void this.flushNextQueued(endedChat);
        }
        // A terminal boundary always gets one immediate renderer snapshot.
        // During the turn the daemon event-sources the same content, while the
        // ordinary debounce is deliberately relaxed to reduce mirror churn.
        void this.flushSession();
        break;
      }
    }
  }
  private attachJobSession(chat: Chat, job: PublicJob): boolean {
    const attached = !chat.sessionId && !!job.sessionId;
    const sessionChanged = !!job.sessionId && chat.sessionId !== job.sessionId;
    if (job.sessionId) {
      chat.sessionId = job.sessionId;
      chat.pending = false;
      chat.sessionError = undefined;
    }
    if (sessionChanged && job.providerId) chat.sessionProviderId = job.providerId;
    // A model/provider selected while the turn was running is staged for the
    // next turn. Do not overwrite that pick with the provider that just ended.
    if (!chat.providerId || !job.providerId || chat.providerId === job.providerId) {
      if (job.providerId) chat.providerId = job.providerId;
      chat.providerName = this.providerName(chat.providerId) ?? chat.providerName ?? null;
    }
    return attached;
  }
  private applyAcp(msg: Msg, ev: AcpEvent) {
    if (ev.kind === 'thinking') {
      // Every daemon flush is a FRESH window: bridge.go clears thoughtBuf after
      // each emit (queueThinking/flushThinking), so the windows have to
      // accumulate here. Replacing meant the row only ever held the last few
      // seconds of reasoning, and the expanded trail lost the rest of the turn.
      //
      // Windows are kept newline-separated and an exact repeat is dropped, so a
      // replayed or double-flushed window can't double the text
      // (replay-exactly-once, PORT-SPEC) — this supersedes the old endsWith
      // guard, which only caught a repeat of the tail.
      //
      // Still ONE event, never one per window: steer boundaries capture
      // `events.length` as an offset, so appending text is safe where pushing
      // events would shift them.
      const incoming = ev.text.trim();
      const existing = msg.events.find((e) => e.kind === 'thinking') as { text: string } | undefined;
      if (!incoming) { /* nothing to record */ }
      else if (!existing) msg.events.push({ key: rid('t'), at: msg.content.length, kind: 'thinking', text: incoming });
      else if (!existing.text.split('\n').includes(incoming)) existing.text += '\n' + incoming;
    } else if (ev.kind === 'plan') {
      const entries = ev.entries.map((en) => ({ status: String(en.status ?? ''), content: String(en.content ?? '') }));
      const existing = msg.events.find((e) => e.kind === 'plan') as { entries: unknown } | undefined;
      if (existing) existing.entries = entries;
      else msg.events.push({ key: rid('p'), at: msg.content.length, kind: 'plan', entries });
    } else if (ev.kind === 'tool') {
      const match = msg.events.find(
        (e): e is ToolEvent => e.kind === 'tool' && ((!!ev.id && e.id === ev.id) || (!!ev.terminalId && e.terminalId === ev.terminalId)),
      );
      if (match) {
        match.status = ev.status || match.status;
        if (ev.output != null) match.output = ev.output;
        if (ev.images != null) match.images = ev.images;
        if (ev.command != null) match.command = ev.command;
        if (ev.terminalId != null) match.terminalId = ev.terminalId;
        if (ev.location != null) match.location = ev.location;
        if (ev.toolKind != null) match.toolKind = ev.toolKind;
        // Subagent linkage arrives on the first tool_call but backfill from a
        // later tool_call_update too, in case only the update carried _meta.
        if (ev.subagentId != null && match.subagentId == null) match.subagentId = ev.subagentId;
        if (ev.subagentLabel != null && match.subagentLabel == null) match.subagentLabel = ev.subagentLabel;
        if (ev.subagentProvider != null && match.subagentProvider == null) match.subagentProvider = ev.subagentProvider;
        if (ev.subagentModel != null && match.subagentModel == null) match.subagentModel = ev.subagentModel;
        if (ev.subagentHeader != null) match.subagentHeader = ev.subagentHeader;
        // Stamp real end time the first time the call reaches a terminal status.
        if (match.endedAt == null && (isToolDone(match.status) || isToolFailed(match.status))) match.endedAt = Date.now();
      } else {
        const now = Date.now();
        msg.events.push({
          key: rid('tc'), at: msg.content.length, kind: 'tool',
          id: ev.id ?? null, toolKind: ev.toolKind ?? null, title: ev.title, status: ev.status,
          command: ev.command ?? null, terminalId: ev.terminalId ?? null, input: ev.input ?? null,
          output: ev.output ?? null, location: ev.location ?? null,
          images: ev.images,
          subagentId: ev.subagentId ?? null, subagentLabel: ev.subagentLabel ?? null,
          subagentProvider: ev.subagentProvider ?? null, subagentModel: ev.subagentModel ?? null,
		  subagentHeader: ev.subagentHeader ?? false,
          startedAt: now,
          endedAt: (isToolDone(ev.status) || isToolFailed(ev.status)) ? now : undefined,
        });
      }
    }
  }
  private onCatalog(c: ChatCatalog) {
    let controlsChanged = false;
    if (c.models?.length) this.state.models = c.models as ModelOption[];
    if (c.modes?.length) this.state.modes = c.modes as ModeOption[];
    // Additive provider groups (P4). Accept an explicit array even when empty so
    // a daemon that reports "no enabled providers" clears a stale grouping.
    if (Array.isArray(c.groups)) {
      this.captureCatalogHashes(c.groups as CatalogGroup[]);
      // Compatibility for a still-running pre-fix daemon: its Claude catalog
      // includes a duplicate `default` alias and verbose "(1M context)"
      // suffixes. The rebuilt daemon already normalizes this; doing the same at
      // the view boundary lets a shell-only rebuild activate the corrected UX
      // without restarting daemon-owned agents.
      this.state.groups = normalizeCatalogGroups(c.groups as CatalogGroup[]);
      reportShellCatalog(userFacingCatalogGroups(this.state.groups, this.state.meta?.profile));
      // Backfill provider names and reconcile stale persisted selections against
      // the authoritative provider/model/effort catalog.
      if (this.reconcileCatalogControls()) controlsChanged = true;
    }
    if (controlsChanged) {
      this.markAllChatsDirty();
      void this.persistControlsNow();
    }
    this.bumpApp(false);
  }
  // Resolve a provider's display name from the catalog groups.
  providerName(providerId: string | null | undefined): string | null {
    if (!providerId) return null;
    return this.state.groups.find((g) => g.providerId === providerId)?.providerName ?? null;
  }
  private onProvidersList(providers: ProviderRecord[]) {
    if (!Array.isArray(providers)) return;
    this.state.providers = userFacingProviders(providers, this.state.meta?.profile ?? 'prod');
    this.bumpApp(false);
  }
  async detectProvider(providerId: string) {
    if (!providerId || !has('providersDetect')) return;
    const result = await call('providersDetect', { provider: providerId });
    if (Array.isArray(result?.providers)) this.onProvidersList(result.providers);
  }
  // Keep the latest plan-usage snapshot per provider. The daemon already merges
  // entries across captures, so the renderer just replaces by providerId.
  private onPlanUsage(s: PlanUsageSnapshot) {
    if (!s || !s.providerId) return;
    this.state.planUsageByProvider[s.providerId] = s;
    this.state.planUsageLoadingByProvider[s.providerId] = false;
    this.bumpApp(false);
  }
  private onSpawnedWorkChanged(s: SpawnedWorkChanged) {
    if (!s || !s.tabId || !s.chatId || !Array.isArray(s.items)) return;
    const key = spawnedWorkChatKey(s.tabId, s.chatId);
    this.state.spawnedWorkByChat[key] = s.items;
    // `obligation` is additive. An older daemon omits the key entirely, and
    // that reply must not erase a receipt hydrated from the actor. A present
    // key is authoritative: a real obligation replaces the prior one, while
    // an explicit empty value clears it after the actor has settled it.
    if (Object.prototype.hasOwnProperty.call(s, 'obligation')) {
      if (s.obligation?.state) this.state.obligationByChat[key] = s.obligation;
      else delete this.state.obligationByChat[key];
    }
    this.bump(SPAWNED);
  }
  async refreshSpawnedWork(chat: Chat | null = this.active()) {
    if (!chat || !chat.chatId || !this.state.hasSpawnedWorkChannels) return;
    const result = await call('spawnedWorkList', chat.id, chat.chatId);
    if (Array.isArray(result?.items)) {
      const update: SpawnedWorkChanged = { tabId: chat.id, chatId: chat.chatId, items: result.items };
      if (result && Object.prototype.hasOwnProperty.call(result, 'obligation')) {
        update.obligation = result.obligation;
      }
      this.onSpawnedWorkChanged(update);
    }
  }
  private refreshAllSpawnedWork() {
    if (!this.state.hasSpawnedWorkChannels) return;
    // Existing long-lived provider tasks may not emit another lifecycle event
    // after this renderer attaches. Refresh every immutable tab+chat pair so
    // inactive sidebar rows also recover their correct live dot after startup
    // or reconnect, without activating those chats.
    for (const chat of this.state.chats) void this.refreshSpawnedWork(chat);
  }
  // What the chat still owes the user. Absent against an older daemon, in
  // which case the sidebar falls back to exactly its previous behaviour.
  obligation(chat: Chat | null = this.active()): { state: string; source?: string } | undefined {
    if (!chat?.chatId) return undefined;
    return this.state.obligationByChat[spawnedWorkChatKey(chat.id, chat.chatId)];
  }
  spawnedWork(chat: Chat | null = this.active()): SpawnedWorkItem[] {
    if (!chat?.chatId) return [];
    return this.state.spawnedWorkByChat[spawnedWorkChatKey(chat.id, chat.chatId)] ?? [];
  }
  async readSpawnedWork(chat: Chat, id: string, tailBytes = 12000): Promise<SpawnedWorkRead | undefined> {
    if (!chat.chatId || !id || !this.state.hasSpawnedWorkChannels) return undefined;
    return call('spawnedWorkRead', chat.id, chat.chatId, id, tailBytes);
  }
  /** Feature-detected separately from the read channels: a daemon that lists
   *  background work may predate stopping it, and a button that cannot work
   *  should not be drawn. */
  canStopSpawnedWork(): boolean { return has('spawnedWorkStop'); }
  async stopSpawnedWork(chat: Chat, id: string): Promise<SpawnedWorkStop | undefined> {
    if (!chat.chatId || !id || !has('spawnedWorkStop')) return undefined;
    const result = await call('spawnedWorkStop', chat.id, chat.chatId, id);
    // The daemon settles the record and raises spawned-work:changed itself. The
    // refresh is for the answers that change nothing here — an already-finished
    // row this client was still drawing as running, and a subagent whose cancel
    // settles a beat later.
    void this.refreshSpawnedWork(chat);
    return result;
  }
  // Entorno rail: the daemon's per-chat changed-file snapshot. Live events land
  // here; the card also hydrates via chat:env-get on mount/reconnect since a
  // fresh renderer may attach after this chat's last turn already ended.
  private onChatEnv(e: ChatEnvPayload) {
    if (!e || (!e.tabId && !e.chatId)) return;
    this.state.chatEnvByChat[chatEnvKey(e.tabId, e.chatId)] = e;
    this.bump(ENV);
  }
  async refreshChatEnv(chat: Chat | null = this.active()) {
    if (!chat || !chat.chatId || !has('chatEnvGet')) return;
    const env = await call('chatEnvGet', { chatId: chat.chatId, tabId: chat.id });
    // Backfill identity from the chat we asked about so a daemon reply with empty
    // ids still keys to the right card.
    if (env) this.onChatEnv({ ...env, tabId: env.tabId || chat.id, chatId: env.chatId || chat.chatId });
  }
  chatEnv(chat: Chat | null = this.active()): ChatEnvPayload | undefined {
    if (!chat?.chatId) return undefined;
    return this.state.chatEnvByChat[chatEnvKey(chat.id, chat.chatId)];
  }
  async useRateLimitReset(providerId: string, sessionId: string | undefined, idempotencyKey: string, creditId?: string) {
    if (!providerId || !idempotencyKey || !has('appChatUseRateLimitReset')) return undefined;
    const result = await callThrow('appChatUseRateLimitReset', providerId, sessionId, idempotencyKey, creditId);
    if (result?.planUsage) this.onPlanUsage(result.planUsage);
    return result;
  }
  // Update notifications. Both payloads are transient snapshots; the daemon owns
  // the check cadence and replay, so the renderer just mirrors the latest.
  private onProvidersUpdates(e: ProvidersUpdates) {
    if (!e || !Array.isArray(e.updates)) return;
    this.state.providersUpdates = e.updates;
    this.state.providersCheckedAt = e.checkedAt;
    // Drop a lingering terminal progress entry once its provider disappears from
    // the update list (a successful update makes updateAvailable:false ⇒ the entry
    // is dropped). Running/pending providers stay in the list, so their live
    // progress is untouched.
    const present = new Set(e.updates.map((u) => u.providerId));
    for (const id of Object.keys(this.state.updateProgress)) {
      if (!present.has(id) && this.state.updateProgress[id].status !== 'running') {
        delete this.state.updateProgress[id];
      }
    }
    this.bumpApp(false);
  }
  // Live click-to-update progress. The daemon owns the throttled stream and
  // replays the latest snapshot per provider; the renderer just mirrors it,
  // overwriting the optimistic 'running' entry set at click time. A terminal
  // done/failed snapshot resolves any pending waiter so the sequential chain can
  // advance to the next CLI.
  private onUpdateProgress(e: ProviderUpdateProgress) {
    if (!e || !e.providerId || !e.status) return;
    this.state.updateProgress[e.providerId] = e;
    if (e.status === 'done' || e.status === 'failed') {
      this.settleUpdateWaiter(e.providerId, e.status);
    }
    this.bumpApp(false);
  }
  // Resolvers keyed by providerId, settled when that provider's update reaches a
  // terminal progress snapshot. Lets the chain await one CLI before the next.
  private updateWaiters = new Map<string, {
    promise: Promise<'done' | 'failed'>;
    resolve: (status: 'done' | 'failed') => void;
    timer: ReturnType<typeof setTimeout>;
  }>();

  private settleUpdateWaiter(providerId: string, status: 'done' | 'failed') {
    const waiter = this.updateWaiters.get(providerId);
    if (!waiter) return;
    this.updateWaiters.delete(providerId);
    clearTimeout(waiter.timer);
    waiter.resolve(status);
  }

  // Click-to-update: run the daemon's provider-owned updater for one provider;
  // the daemon resolves the installed executable path before launching it and
  // resolve with its terminal outcome. Optimistic 'running' flips the UI
  // instantly; the live progress stream then owns the state. Structured refusals
  // are handled without throwing to the UI.
  async updateProvider(providerId: string): Promise<'done' | 'failed' | 'skipped' | 'unsupported'> {
    if (!providerId || !has('providersUpdate')) return 'unsupported';
    if (!this.isConnected()) return 'skipped';
    // Already live for this provider — wait on the existing run rather than
    // re-invoking (also the "second update while one runs" guard).
    if (this.state.updateProgress[providerId]?.status === 'running') {
      return this.awaitUpdateTerminal(providerId);
    }
    this.state.updateProgress[providerId] = {
      providerId, status: 'running', startedAt: new Date().toISOString(), tail: '',
    };
    this.bumpApp(false);
    try {
      const res = await callThrow('providersUpdate', providerId);
      if (res === undefined) {
        // Bridge lacks the method after all — drop the optimistic entry so the
        // copy-command chip stays the honest primary path.
        delete this.state.updateProgress[providerId];
        this.bumpApp(false);
        return 'unsupported';
      }
      // Accepted → the daemon streams providers:update-progress; wait for terminal.
      return this.awaitUpdateTerminal(providerId);
    } catch (err) {
      const code = structuredErrorCode(err);
      // in-progress: another run for this provider is already live — wait on it.
      if (code === 'providers:update-in-progress') return this.awaitUpdateTerminal(providerId);
      // no-pending: nothing to do (already current) — clear optimistic, treat as
      // a skip so a chain moves on. unknown-provider / anything else → failure.
      delete this.state.updateProgress[providerId];
      this.bumpApp(false);
      return code === 'providers:update-no-pending' ? 'skipped' : 'failed';
    }
  }
  // Resolve when the provider's update reaches a terminal progress snapshot. If a
  // terminal snapshot is already present (race), resolve immediately.
  private awaitUpdateTerminal(providerId: string): Promise<'done' | 'failed'> {
    const cur = this.state.updateProgress[providerId];
    if (cur && (cur.status === 'done' || cur.status === 'failed')) return Promise.resolve(cur.status);
    const existing = this.updateWaiters.get(providerId);
    if (existing) return existing.promise;
    let resolveWaiter!: (status: 'done' | 'failed') => void;
    const promise = new Promise<'done' | 'failed'>((resolve) => { resolveWaiter = resolve; });
    const timer = setTimeout(() => {
      const waiting = this.updateWaiters.get(providerId);
      if (!waiting || waiting.promise !== promise) return;
      this.updateWaiters.delete(providerId);
      const progress = this.state.updateProgress[providerId];
      if (progress?.status === 'running') {
        this.state.updateProgress[providerId] = {
          ...progress,
          status: 'failed',
          error: 'La actualización no informó un estado terminal a tiempo.',
        };
        this.bumpApp(false);
      }
      resolveWaiter('failed');
    }, UPDATE_TERMINAL_TIMEOUT);
    this.updateWaiters.set(providerId, { promise, resolve: resolveWaiter, timer });
    return promise;
  }

  // Footer card action: update every pending CLI, one at a time (the daemon allows
  // one in-flight update per provider; we chain so the next starts when the
  // previous finishes). Stops at the first failure, leaving the failed CLI + any
  // remaining pending listed for a Reintentar. A clean sweep clears the chain and
  // the card disappears with the daemon's providers:updates re-emit.
  private chainActive = false;
  async startUpdateChain() {
    if (this.chainActive || !has('providersUpdate') || !this.isConnected()) return;
    const ids = this.providerUpdatesAvailable().map((u) => u.providerId);
    if (ids.length === 0) return;
    this.chainActive = true;
    this.state.updateChain = { ids, index: 0, failedId: null };
    try {
      for (let i = 0; i < ids.length; i++) {
        // Re-read the chain object so a Reintentar-triggered restart is coherent.
        this.state.updateChain = { ids, index: i + 1, failedId: null };
        this.bumpApp(false);
        const outcome = await this.updateProvider(ids[i]);
        if (outcome === 'failed') {
          this.state.updateChain = { ids, index: i + 1, failedId: ids[i] };
          this.bumpApp(false);
          return;
        }
        // 'done' / 'skipped' / 'unsupported' → continue to the next CLI.
      }
      // Every CLI succeeded — drop the chain; providers:updates clears the card.
      this.state.updateChain = undefined;
      this.bumpApp(false);
    } finally {
      this.chainActive = false;
    }
  }
  // Live progress for a provider, if any (undefined when nothing has run).
  updateProgressFor(providerId: string): ProviderUpdateProgress | undefined {
    return this.state.updateProgress[providerId];
  }
  // The one running update (footer card flips to progress mode when present).
  runningUpdate(): ProviderUpdateProgress | null {
    for (const id of Object.keys(this.state.updateProgress)) {
      if (this.state.updateProgress[id].status === 'running') return this.state.updateProgress[id];
    }
    return null;
  }
  private onAppUpdate(e: AppUpdate) {
    if (!e || !e.version) return;
    this.state.appUpdate = e;
    this.bumpApp(false);
  }
  // Provider entries with a resolved upgrade available (drives the footer card
  // and the Ajustes·Agentes update rows).
  providerUpdatesAvailable(): ProviderUpdate[] {
    return this.state.providersUpdates.filter((u) => u.updateAvailable);
  }
  private onCompacted(e: ChatCompacted) {
    // Attach a quiet "Contexto compactado" step row to the chat's last message.
    let chat: Chat | null = null;
    if (e.sessionId) chat = this.state.chats.find((c) => c.sessionId === e.sessionId) ?? null;
    if (!chat && e.chatId) chat = this.chatByConvId(e.chatId) ?? this.chat(e.chatId);
    if (!chat) return;
    const last = chat.messages[chat.messages.length - 1];
    if (last) {
      last.events.push({ key: rid('cx'), at: last.content.length, kind: 'compaction' });
      this.bump('msg:' + last.id);
    }
    this.bumpApp(false);
  }
  private onProcChanged(e: ProcChanged) {
    if (Array.isArray(e?.processes)) {
      this.state.processes = e.processes;
      this.captureProcHash(e.processes);
      this.bump(PROC);
    }
  }
  async killProc(id: string) {
    // Optimistic: mark killing; proc:changed reconciles on success.
    this.state.processes = this.state.processes.map((p) => (p.id === id ? { ...p, status: 'killing', _killError: undefined } : p));
    this.bump(PROC);
    const res = await call('procKill', id, true);
    if (res && res.ok === false) {
      // Daemon refused (e.g. protected ACP engine) — revert and surface why.
      this.state.processes = this.state.processes.map((p) => (p.id === id ? { ...p, status: 'running', _killError: res.error || 'No se pudo detener.' } : p));
      this.bump(PROC);
    }
  }
  // Stop a running process from the Tareas card. ACP engines are protected from
  // proc:kill by the daemon, so an engine row closes its ACP session instead
  // (app-chat:close-session); every other process uses proc:kill.
  async stopProc(p: ProcessSummary) {
    if (!p.engine) return this.killProc(p.id);
    const sessionId = this.engineSessionId(p);
    if (!sessionId) {
      // Engine not mapped to a live chat session (e.g. a warm spare): we have no
      // sessionId to close and must not proc:kill a protected engine. Surface it.
      this.state.processes = this.state.processes.map((x) => (x.id === p.id ? { ...x, _killError: 'Sin sesión asociada para cerrar.' } : x));
      this.bump(PROC);
      return;
    }
    this.state.processes = this.state.processes.map((x) => (x.id === p.id ? { ...x, status: 'killing', _killError: undefined } : x));
    this.bump(PROC);
    const ok = await call('appChatCloseSession', sessionId);
    if (ok === false) {
      this.state.processes = this.state.processes.map((x) => (x.id === p.id ? { ...x, status: 'running', _killError: 'No se pudo cerrar la sesión.' } : x));
      this.bump(PROC);
      return;
    }
    // closeSession terminates the engine but the daemon does NOT emit
    // proc:changed for it, so reconcile authoritatively from proc:list — the
    // now-closed engine returns with status 'failed' and drops out of the
    // running list rendered by the Tareas card.
    if (has('procList')) {
      const r = await call('procList');
      if (r?.processes) { this.state.processes = r.processes; this.bump(PROC); }
    } else {
      // No proc:list to reconcile against: drop the row optimistically.
      this.state.processes = this.state.processes.filter((x) => x.id !== p.id);
      this.bump(PROC);
    }
  }
  // Map an engine process to its chat's ACP session. The daemon keys each engine
  // bridge by tabId (== chat.id) and embeds it in the label as "<agent> (<key>)";
  // it also carries chatId when set. Match either against a chat that has a live
  // sessionId.
  private engineSessionId(p: ProcessSummary): string | null {
    for (const c of this.state.chats) {
      if (!c.sessionId) continue;
      if (p.chatId && p.chatId === c.id) return c.sessionId;
      if (p.label && p.label.includes(`(${c.id})`)) return c.sessionId;
    }
    return null;
  }
  private onPermission(req: PermissionRequest, notify = true) {
    if (this.resolvedPermissionIds.has(req.id)) return;
    let msg = req.jobId ? this.resolveMsg(req.jobId) : null;
    if (!msg) {
      // A permission belongs to its ACP session. Never attach an unmatched
      // background request to whichever tab the user happens to be viewing.
      const chat = (req.tabId ? this.chat(req.tabId) : null)
        ?? (req.chatId ? this.chatByConvId(req.chatId) : null)
        ?? (req.sessionId
          ? this.state.chats.find((candidate) => candidate.sessionId === req.sessionId) ?? null
          : null);
      msg = chat
        ? [...chat.messages].reverse().find((m) => m.role === 'assistant'
          && ((!req.jobId || m.jobId === req.jobId) || m.status === 'running')) ?? null
        : null;
    }
    if (!msg) return;
    msg.permission = { id: req.id, title: req.title || 'una acción', kind: req.kind, options: req.options, question: req.question ?? null };
    this.bump('msg:' + msg.id);
    // R7: a permission prompt needs the user's attention — surface it if the tab
    // is unfocused so they don't leave the turn blocked.
    if (notify && documentHidden()) {
      const owner = this.state.chats.find((candidate) => candidate.messages.includes(msg));
      this.fireNotify(owner?.title ?? 'workass',
        req.question ? `Pregunta: ${req.question.question}` : `Permiso solicitado: ${req.title || 'una acción'}`,
        owner?.id);
    }
  }

  private onPermissionResolved(resolved: PermissionResolved) {
    if (!resolved?.id) return;
    this.resolvedPermissionIds.add(resolved.id);
    if (this.resolvedPermissionIds.size > 512) {
      const oldest = this.resolvedPermissionIds.values().next().value;
      if (oldest) this.resolvedPermissionIds.delete(oldest);
    }
    for (const msgId of clearPermissionById(this.state.chats, resolved.id)) this.bump('msg:' + msgId);
  }

  private async refreshPendingPermissions() {
    if (!has('chatPendingPermissions')) return;
    const result = await call('chatPendingPermissions');
    if (!Array.isArray(result?.permissions)) return;
    const pendingIds = new Set(result.permissions.map((permission) => permission.id).filter(Boolean));
    for (const msgId of clearPermissionsOutsideSnapshot(this.state.chats, pendingIds)) this.bump('msg:' + msgId);
    for (const permission of result.permissions) this.onPermission(permission, false);
  }

  // ---- R4 checkpoints / rewind -----------------------------------------
  private chatByConvId(chatId: string | null | undefined): Chat | null {
    if (!chatId) return null;
    return this.state.chats.find((c) => c.chatId === chatId) ?? null;
  }
  // Map a completed turn (by its daemon jobId) to the checkpoint recorded at its
  // end. Only turns that actually changed tracked files get a usable checkpoint.
  checkpointForJob(chat: Chat | null, jobId: string | undefined): ChatCheckpoint | null {
    if (!chat || !jobId || !chat.checkpoints) return null;
    const cp = chat.checkpoints.find((c) => c.jobId === jobId);
    if (!cp) return null;
    return cp.repos.some((r) => !r.skipped && r.changedFiles > 0) ? cp : null;
  }
  async refreshCheckpoints(chat: Chat) {
    if (!has('chatCheckpoints') || !chat.chatId) return;
    const items = await call('chatCheckpoints', { chatId: chat.chatId, tabId: chat.id });
    if (Array.isArray(items)) { chat.checkpoints = items; this.bumpApp(false); }
  }
  async openRewind(focusTurn?: number) {
    const chat = this.active();
    this.state.rewind = {
      open: true, tabId: chat?.id ?? null, chatId: chat?.chatId ?? null,
      loading: !!chat && has('chatCheckpoints'), items: chat?.checkpoints ?? [], focusTurn,
    };
    if (!chat) { this.state.rewind.loading = false; this.bumpApp(false); return; }
    if (!has('chatCheckpoints')) {
      this.state.rewind.error = 'Los puntos de control no están disponibles en este bridge (falta exponer chat:checkpoints).';
      this.bumpApp(false); return;
    }
    this.bumpApp(false);
    const items = await call('chatCheckpoints', { chatId: chat.chatId, tabId: chat.id });
    if (Array.isArray(items)) chat.checkpoints = items;
    this.state.rewind = { ...this.state.rewind, loading: false, items: chat.checkpoints ?? [] };
    this.bumpApp(false);
  }
  closeRewind() { this.state.rewind = { ...this.state.rewind, open: false, error: undefined }; this.bumpApp(false); }
  async rewindTo(turnSeq: number) {
    const chatId = this.state.rewind.chatId ?? this.active()?.chatId ?? null;
    const tabId = this.state.rewind.tabId ?? this.active()?.id ?? null;
    if (!chatId || !tabId) return;
    const operationId = this.state.rewind.operationTurn === turnSeq && this.state.rewind.operationId
      ? this.state.rewind.operationId : rid('checkpoint-restore');
    this.state.rewind = { ...this.state.rewind, busyTurn: turnSeq, operationId, operationTurn: turnSeq, error: undefined };
    this.bumpApp(false);
    try {
      const res = await callThrow('chatRewind', { tabId, chatId, turnSeq, operationId });
      if (res === undefined) {
        this.state.rewind = { ...this.state.rewind, busyTurn: undefined, error: 'chat:rewind no está disponible en este bridge.' };
        this.bumpApp(false);
        return;
      }
      // Success → the daemon emits chat:checkpoint-restored, which closes the
      // menu and drops the "Estado restaurado…" step row. Clear busy defensively
      // in case that event is not delivered (older bridge).
      this.state.rewind = { ...this.state.rewind, busyTurn: undefined, operationId: undefined, operationTurn: undefined, open: false };
      this.bumpApp(false);
    } catch (err) {
      this.state.rewind = { ...this.state.rewind, busyTurn: undefined, operationId, error: rewindErrorMessage(err) };
      this.bumpApp(false);
    }
  }
  private onCheckpointRestored(e: CheckpointRestored) {
    // The receipt belongs only to the chat named by the daemon. Falling back to
    // whichever rewind panel happened to be open let another local/remote chat's
    // receipt close this chat's panel and paint its restored marker here.
    const chat = this.chatByConvId(e?.chatId);
    if (!chat) return;
    const last = chat.messages[chat.messages.length - 1];
    if (last) {
      last.events.push({ key: rid('rw'), at: last.content.length, kind: 'restored', turnSeq: e.turnSeq });
      this.bump('msg:' + last.id);
    }
    void this.refreshCheckpoints(chat);
    const ownsOpenRewind = this.state.rewind.tabId === chat.id
      && this.state.rewind.chatId === chat.chatId;
    if (ownsOpenRewind) {
      this.state.rewind = { ...this.state.rewind, open: false, busyTurn: undefined, error: undefined };
    }
    this.bumpApp(false);
  }
  private onEngineRecovered(e: EngineRecovered) {
    const chat = this.chatByConvId(e?.chatId);
    this.addToast(chat?.title ?? 'workass', 'El motor se reinició y reanudó el turno.');
  }

  // ---- R5 Revisar diff panel -------------------------------------------
  async openReview(chat: Chat | null) {
    const c = chat ?? this.active();
    this.state.review = {
      open: true, tabId: c?.id ?? null, chatId: c?.chatId ?? null,
      loading: !!c && has('chatEnvGet'), repos: [], diffLoading: false,
    };
    if (!c) { this.state.review.loading = false; this.bumpApp(false); return; }
    if (!has('chatEnvGet')) {
      this.state.review.error = 'El detalle de cambios no está disponible en este bridge (falta exponer chat:env-get).';
      this.bumpApp(false); return;
    }
    this.bumpApp(false);
    const env = await call('chatEnvGet', { chatId: c.chatId, tabId: c.id });
    const repos = env?.repos ?? [];
    this.state.review = { ...this.state.review, loading: false, repos };
    this.bumpApp(false);
    const first = repos.find((r) => r.files.length);
    if (first) await this.selectDiffFile(first.name, first.files[0].path);
  }
  closeReview() { this.state.review = { ...this.state.review, open: false }; this.bumpApp(false); }
  async selectDiffFile(repo: string, path: string) {
    const chatId = this.state.review.chatId;
    const tabId = this.state.review.tabId;
    this.state.review = { ...this.state.review, active: { repo, path }, diffLoading: true, diff: undefined, error: undefined };
    this.bumpApp(false);
    if (!chatId || !tabId || !has('chatDiff')) {
      this.state.review = { ...this.state.review, diffLoading: false, error: 'chat:diff no está disponible en este bridge.' };
      this.bumpApp(false); return;
    }
    const diff = await call('chatDiff', { chatId, tabId, repo, path });
    this.state.review = { ...this.state.review, diffLoading: false, diff: diff ?? undefined, error: diff ? undefined : 'No se pudo cargar el diff.' };
    this.bumpApp(false);
  }

  // ---- R7 notifications -------------------------------------------------
  async setNotifEnabled(enabled: boolean) {
    if (enabled) await this.requestNotifPermission();
    this.state.notifEnabled = enabled;
    this.bumpApp();       // persists the preference
  }
  async requestNotifPermission(): Promise<NotificationPermission | 'unsupported'> {
    if (typeof Notification === 'undefined') { this.state.notifPermission = 'unsupported'; this.bumpApp(false); return 'unsupported'; }
    let perm = Notification.permission;
    if (perm === 'default') { try { perm = await Notification.requestPermission(); } catch { /* ignore */ } }
    this.state.notifPermission = perm;
    this.bumpApp(false);
    return perm;
  }
  // RENDER a notification locally: a desktop notification when enabled + granted,
  // otherwise an in-app toast so there is always feedback.
  //
  // This method must NEVER publish back to the daemon `notify` channel. Against
  // the daemon that channel re-broadcasts to THIS controller, so a notify that
  // arrived via onNotify and fell through to a `call('notify')` fallback echoed
  // forever — the "spam like crazy" the user hit. Local rendering only.
  fireNotify(title: string, body: string, tabId?: string | null) {
    if (this.state.notifEnabled && this.state.notifPermission === 'granted' && typeof Notification !== 'undefined') {
      try {
        const n = new Notification(title, { body });
        n.onclick = () => { try { window.focus(); } catch { /* ignore */ } if (tabId) this.switchChat(tabId); n.close(); };
        return;
      } catch { /* fall through to toast */ }
    }
    this.addToast(title, body);
  }
  addToast(title: string, body: string) {
    const t: Toast = { id: rid('to'), title, body };
    this.state.toasts = [...this.state.toasts, t];
    this.bumpApp(false);
    setTimeout(() => this.dismissToast(t.id), 6500);
  }
  dismissToast(id: string) {
    this.state.toasts = this.state.toasts.filter((t) => t.id !== id);
    this.bumpApp(false);
  }
  private onNotify(e: NotifyEvent) {
    if (!e || (!e.title && !e.body)) return;
    // Drop the automatic per-turn "Chat turn finished" card the user never asked
    // for. Current daemon source no longer produces it; an older running daemon
    // still does, so we suppress it here until that daemon is rebuilt.
    if (isAutoTurnEndNotice(e.body)) return;
    this.fireNotify(e.title || 'workass', e.body || '', e.tabId ?? undefined);
  }
  private onNotifyBacklog(e: NotifyBacklog) {
    if (!e || !Array.isArray(e.items)) return;
    // Catch-up burst while this device was away → quiet in-app toasts (do not
    // spawn a volley of OS notifications after the fact). The stale daemon's
    // per-turn cards are dropped here too.
    const items = e.items.filter((item) => !isAutoTurnEndNotice(item?.body));
    for (const item of items.slice(-5)) this.addToast(item.title || 'workass', item.body || '');
  }

}

function rid(p: string) { return `${p}-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 7)}`; }
// Stable per-conversation id (R4). Distinct prefix from the tab id so the two
// never collide in logs or checkpoint keys.
function newChatConvId() { return `conv-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 8)}`; }
function documentHidden(): boolean { return typeof document !== 'undefined' && document.visibilityState === 'hidden'; }
function notifPermission(): NotificationPermission | 'unsupported' {
  return typeof Notification === 'undefined' ? 'unsupported' : Notification.permission;
}
// Turn chat:rewind's structured JSON refusal (delivered as reply.error string)
// into honest Spanish. Falls back to the raw message for unknown shapes.
function rewindErrorMessage(err: unknown): string {
  const raw = err instanceof Error ? err.message : String(err ?? '');
  let parsed: ChatStructuredError | null = null;
  try { const o = JSON.parse(raw); if (o && typeof o === 'object' && typeof o.code === 'string') parsed = o as ChatStructuredError; } catch { /* not JSON */ }
  if (!parsed) return raw || 'No se pudo revertir.';
  switch (parsed.code) {
    case 'chat:rewind-outside-modification':
      return 'El repositorio se modificó fuera de este chat desde el punto de control. Revertir descartaría esos cambios, así que se canceló.';
    case 'chat:rewind-not-found':
      return 'Ese punto de control ya no está disponible.';
    case 'chat:rewind-invalid':
      return 'Solicitud de reversión inválida.';
    default:
      return parsed.message || 'No se pudo revertir.';
  }
}
// A daemon refusal arrives as a JSON body, so a raw message would put
// `{"code":"fleet:not-local",…}` in front of a human. These are the four this
// pane can actually provoke; anything else falls back to the daemon's sentence.
function fleetKeyError(err: unknown): string {
  const raw = err instanceof Error ? err.message : String(err ?? '');
  switch (structuredErrorCode(err)) {
    case 'fleet:not-local':
      return 'La clave sólo se lee en la máquina que la tiene. Abrí la app ahí, o pedila por otro medio.';
    case 'lan:not-controller':
      return 'Otro dispositivo tiene el control ahora mismo. Mandá algo desde acá y volvé a intentar.';
    case 'fleet:unknown-key':
      return 'Esa clave ya no está en esta máquina.';
    case 'fleet:unavailable':
      return 'Este daemon no guarda claves de flota.';
    default:
      break;
  }
  try { return String((JSON.parse(raw) as { message?: string })?.message || raw); } catch { return raw; }
}
// Extract the `code` from a channel's structured JSON refusal (reply.error string),
// or null when the error is not a structured refusal.
function structuredErrorCode(err: unknown): string | null {
  const raw = err instanceof Error ? err.message : String(err ?? '');
  try {
    const o = JSON.parse(raw);
    if (o && typeof o === 'object' && typeof (o as ChatStructuredError).code === 'string') {
      return (o as ChatStructuredError).code;
    }
  } catch { /* not JSON */ }
  return null;
}
function tokenTotal(...vals: unknown[]): number {
  let sum = 0;
  for (const v of vals) { const n = Number(v); if (Number.isFinite(n) && n > 0) sum += n; }
  return sum;
}
function clampW(w: number, min: number, max: number): number { return Math.round(Math.max(min, Math.min(max, w))); }
function spawnedWorkChatKey(tabId: string, chatId: string): string { return `${tabId}\u0000${chatId}`; }
// Same composite key shape as spawnedWorkChatKey — the Entorno snapshot is also
// scoped to an exact immutable tabId+chatId pair.
function chatEnvKey(tabId: string, chatId: string): string { return spawnedWorkChatKey(tabId, chatId); }
function normalizeCatalogGroups(groups: CatalogGroup[]): CatalogGroup[] {
  const claudeRank: Record<string, number> = { 'claude-fable-5[1m]': 0, 'opus[1m]': 1, sonnet: 2, haiku: 3 };
  return groups.map((group) => {
    if (group.providerId !== 'claude') return group;
    const models = group.models
      .filter((model) => model.modelId !== 'default')
      .map((model) => ({ ...model, name: model.name.replace(/\s*\(1m context\)\s*/i, '').trim() }))
      .sort((a, b) => (claudeRank[a.modelId] ?? 99) - (claudeRank[b.modelId] ?? 99));
    return { ...group, models };
  });
}
function reportShellCatalog(groups: CatalogGroup[]) {
  if (typeof fetch !== 'function') return;
  void fetch('/__workass-shell/catalog', {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ groups: groups.map((group) => ({
      providerId: group.providerId,
      status: group.status ?? '',
      models: group.models.map(({ modelId, name }) => ({ modelId, name })),
    })) }),
  }).catch(() => {});
}
function prefersLight(): boolean { return typeof matchMedia !== 'undefined' && matchMedia('(prefers-color-scheme: light)').matches; }
function resolveTheme(pref: ThemePref): 'dark' | 'light' {
  if (pref === 'light' || pref === 'dark') return pref;
  return prefersLight() ? 'light' : 'dark';
}
function applyTheme(theme: 'dark' | 'light') {
  if (typeof document === 'undefined') return;
  if (theme === 'light') document.documentElement.setAttribute('data-theme', 'light');
  else document.documentElement.removeAttribute('data-theme');
}
function applyDensity(density: Density) {
  if (typeof document === 'undefined') return;
  if (density === 'comfortable') document.documentElement.setAttribute('data-density', 'comfortable');
  else document.documentElement.removeAttribute('data-density');
}
function isTerminalMessage(message: Pick<Msg, 'status'>): boolean {
  return message.status === 'done' || message.status === 'failed' || message.status === 'cancelled';
}
function terminalMessageStatus(job: PublicJob): Msg['status'] {
  if (job.code === 130 || job.stopReason === 'cancelled') return 'cancelled';
  return job.status === 'done' ? 'done' : 'failed';
}
function localPendingPermissionIDs(chat: Chat): string[] {
  return chat.messages
    .map((message) => message.permission?.id)
    .filter((id): id is string => !!id)
    .sort();
}
function localRunningJobID(chat: Chat): string | null {
  for (let index = chat.messages.length - 1; index >= 0; index--) {
    const message = chat.messages[index];
    if (message.status === 'running' && message.jobId) return message.jobId;
  }
  return null;
}
export function digestChatSessionDiverged(chat: Chat, digest: StateDigestChat): boolean {
  const queue = chat.queue ?? [];
  return localRunningJobID(chat) !== (digest.runningJobId ?? null)
    || (chat.messages.at(-1)?.id ?? null) !== (digest.lastMessageId ?? null)
    || queue.length !== digest.queueLen
    || (queue[0]?.id ?? null) !== (digest.queueHeadId ?? null)
    || (Number.isInteger(digest.presentationRevision)
      && (chat.presentationRevision ?? 0) !== digest.presentationRevision)
    || (chat.agentQueueRevision ?? 0) !== digest.agentQueueRevision
    || (chat.runtimeControlRevision ?? 0) !== digest.runtimeControlRevision
    || (chat.providerId ?? null) !== (digest.providerId ?? null)
    || chat.currentModelId !== (digest.currentModelId ?? null)
    || chat.currentModeId !== (digest.currentModeId ?? null);
}
function sameStrings(left: readonly string[], right: readonly string[]): boolean {
  if (left.length !== right.length) return false;
  const sorted = [...right].sort();
  return left.every((value, index) => value === sorted[index]);
}
function hashMapDiverged(local: Record<string, string> | null, remote: Record<string, string>): boolean {
  const remoteKeys = Object.keys(remote ?? {}).sort();
  if (!local) return remoteKeys.length > 0;
  const localKeys = Object.keys(local).sort();
  return !sameStrings(localKeys, remoteKeys) || remoteKeys.some((key) => local[key] !== remote[key]);
}
async function stateDigestHash(value: unknown): Promise<string | null> {
  const subtle = globalThis.crypto?.subtle;
  if (!subtle) return null;
  try {
    const encoded = new TextEncoder().encode(JSON.stringify(value));
    const digest = await subtle.digest('SHA-256', encoded);
    return [...new Uint8Array(digest)].map((part) => part.toString(16).padStart(2, '0')).join('');
  } catch {
    return null;
  }
}
function isStateDigest(value: unknown): value is StateDigest {
  if (!value || typeof value !== 'object') return false;
  const digest = value as Partial<StateDigest>;
  return Array.isArray(digest.chats)
    && Number.isInteger(digest.globalRevision)
    && !!digest.catalogHash
    && typeof digest.catalogHash === 'object'
    && typeof digest.settingsRevision === 'string'
    && typeof digest.procHash === 'string';
}
function isTransportInvokeError(error: unknown): boolean {
  return error instanceof Error && error.name === 'WorkassInvokeError';
}

export const store = new Store();

// ---- React hooks --------------------------------------------------------
export function useApp(): AppState {
  useSyncExternalStore((cb) => store.subscribe(APP, cb), () => store.version(APP), () => store.version(APP));
  return store.state;
}
export function useMsgVersion(msgId: string): number {
  return useSyncExternalStore((cb) => store.subscribe('msg:' + msgId, cb), () => store.version('msg:' + msgId), () => store.version('msg:' + msgId));
}
export function useProc(): number {
  return useSyncExternalStore((cb) => store.subscribe('proc', cb), () => store.version('proc'), () => store.version('proc'));
}
// Re-renders once a second so fresh relative-time stamps visibly age. Keep it on
// the smallest possible leaf (the stamp span), never a whole message body.
export function useClock(): number {
  return useSyncExternalStore((cb) => store.subscribeClock(cb), () => store.version('clock'), () => store.version('clock'));
}
export function useSpawnedWork(): number {
  return useSyncExternalStore((cb) => store.subscribe(SPAWNED, cb), () => store.version(SPAWNED), () => store.version(SPAWNED));
}
// Entorno card subscribes to the ENV topic so a new chat:env snapshot repaints
// only the rail card, not every APP consumer.
export function useChatEnv(): number {
  return useSyncExternalStore((cb) => store.subscribe(ENV, cb), () => store.version(ENV), () => store.version(ENV));
}
// Subscribes only to connection transitions (rare) — safe to call from a
// streaming message without dragging it into every APP re-render.
export function useConnStatus(): ConnStatus {
  useSyncExternalStore((cb) => store.subscribe('conn', cb), () => store.version('conn'), () => store.version('conn'));
  return store.state.connection;
}
export function useDev(): number {
  return useSyncExternalStore((cb) => store.subscribe('dev', cb), () => store.version('dev'), () => store.version('dev'));
}
// Subscribes to agent activity (tool/plan streaming). The Tareas rail uses this
// so live calls repaint without coupling to APP (see the ACT topic).
export function useActivity(): number {
  return useSyncExternalStore((cb) => store.subscribe('act', cb), () => store.version('act'), () => store.version('act'));
}
