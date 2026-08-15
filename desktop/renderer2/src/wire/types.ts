// Wire-protocol types — mirror docs/WIRE-CONTRACT.md exactly.
// The daemon injects /lan-bridge.js which defines window.api before app scripts run.

import type { ModelControlMemory } from '../model-controls';

// `efforts` is an optional, ordered list of reasoning-effort stops the model
// supports (e.g. ["low","medium","high","xhigh","max","ultra"]). Additive: older
// daemons and effortless models (Claude/Qwen today) omit it → no effort control.
// Selecting effort E binds the chat to the model id `${modelId}[${E}]` through the
// existing set-model path; a bare `modelId` means no explicit effort.
export interface ModelOption { modelId: string; name: string; efforts?: string[]; }
export interface ModeOption { id: string; name: string; }

// Additive provider registry group carried on chat:catalog (P4 provider
// registry). Older daemons omit `groups`; the renderer feature-detects.
export interface CatalogGroup {
  providerId: string;
  providerName: string;
  models: ModelOption[];
  modes: ModeOption[];
  status?: string;
  latencyMs?: number;
  error?: string;
  badge?: string;
}

export interface SlashCommand { name: string; description?: string; }

// Rich per-session catalog reported by the provider (Claude Agent SDK):
// slash commands with hints/aliases, subagent types, and output styles.
// Additive everywhere it travels; absent = UNKNOWN, [] = proven empty.
export interface CatalogCommand { name: string; description?: string; argumentHint?: string; aliases?: string[]; }
export interface CatalogAgent { name: string; description?: string; model?: string; }
export interface CommandCatalog {
  commands?: CatalogCommand[];
  agents?: CatalogAgent[];
  outputStyle?: string;
  availableOutputStyles?: string[];
  commandsTruncated?: number;
  agentsTruncated?: number;
  stylesTruncated?: number;
  asOf?: number;
}

export interface AcpSessionInfo {
  sessionId: string;
  cwd: string;
  agent?: string;
  // Additive: which provider the daemon bound this session to (P4 registry).
  providerId?: string;
  providerName?: string;
  models: ModelOption[];
  currentModelId: string | null;
  modes: ModeOption[];
  currentModeId: string | null;
  // R6: image paste/attach is gated on this cap; agent-advertised slash commands
  // drive the composer autocomplete when present (feature-detected; both absent
  // on the mock/older sessions → no attach, no overlay).
  imageSupport?: boolean;
  commands?: SlashCommand[];
  // Additive rich catalog (commands + agents + output styles). When present it
  // supersedes `commands` for the composer popup; `commands` stays for older
  // daemons/providers.
  commandCatalog?: CommandCatalog;
  error?: string;
  // Existing app-chat:new-session also carries the transactional workspace
  // invalidation result when replaceSessionId is supplied. No live session is
  // returned on that branch; ensureSession creates the fresh target-cwd one.
  workspaceCommitted?: boolean;
  workspaceRebound?: boolean;
  workspaceRevision?: number;
}

export interface PublicJob {
  id: string;
  kind: string;
  key: unknown;
  title: string;
  status: string;
  startedAt: string;
  finishedAt: string | null;
  code: number | null;
  permissionMode: string;
  chatId: string | null;
  tabId: string | null;
  sessionId: string | null;
  providerId?: string | null;
  // Additive canonical turn identity (stuck-chat WP-B). Older daemons omit all
  // three fields, in which case the renderer pulls session:get instead of
  // guessing rows from an unanchored event.
  userMessageId?: string | null;
  assistantMessageId?: string | null;
  promptText?: string | null;
  result: string | null;
  error: string | null;
  stopReason: string | null;
  crashInterrupted: boolean;
  // What this turn meant for the user's REQUEST, which is a different question
  // from how the model stopped talking: a park and a finished request both end
  // with stopReason 'end_turn'. Absent on job:start and on older daemons.
  disposition?: { state: string; source?: string };
  // The daemon ended this turn itself (restart/handoff). Older daemons omit it.
  interrupted?: boolean;
  consumedSteerIds?: string[];
  // Provider-neutral assistant media. Structured ACP image blocks omit source;
  // workspace-local Markdown imports retain their target for exact rendering.
  images?: Array<{ mimeType: string; data: string; name?: string; source?: string }>;
}

export type JobEvent =
  | { type: 'start'; job: PublicJob }
  | { type: 'end'; job: PublicJob; draft?: string | null; review?: unknown }
  | { type: 'data'; id: string; stream: string; chunk: string; phase?: 'commentary' | 'final_answer' }
  | { type: 'assistant-media'; id: string; images: Array<{ mimeType: string; data: string; name?: string; source?: string }> }
  | { type: 'acp'; id: string; event: AcpEvent }
  | { type: 'usage'; sessionId: string | null; tabId?: string | null; chatId?: string | null; providerId?: string | null; updatedAt?: string; used: number; size: number; inputTokens: unknown; outputTokens: unknown; cachedReadTokens: unknown; planUsage?: PlanUsageSnapshot };

// Provider plan/rate-limit surface (chat:plan-usage event + additive planUsage on
// usage job events; WIRE-CONTRACT §5 chat:plan-usage). The daemon normalizes real
// adapter metadata into these entries — the renderer never invents fill/percent.
export interface PlanUsageQuotaModel {
  model?: string;
  totalTokens?: number;
  inputTokens?: number;
  cachedInputTokens?: number;
  cachedReadTokens?: number;
  outputTokens?: number;
  reasoningOutputTokens?: number;
  thoughtTokens?: number;
  [k: string]: unknown;
}
export interface PlanUsageEntry {
  // 'rate-limit' | 'cost' | 'quota' known; unknown kinds degrade to nothing.
  kind: string;
  // rate-limit
  id?: string;
  status?: string;
  resetsAt?: string;            // RFC3339 provider reset boundary
  usedPercent?: number;         // provider-reported 0..100 utilization
  windowMinutes?: number;       // provider-reported window when available
  limitName?: string;           // provider label for multi-bucket limits
  overageStatus?: string;
  isUsingOverage?: boolean;
  // cost
  amount?: number;
  currency?: string;
  // quota
  perModel?: PlanUsageQuotaModel[];
}
export interface PlanUsageSnapshot {
  providerId: string;
  capturedAt: string;
  entries?: PlanUsageEntry[];
  rateLimitResetCredits?: RateLimitResetCreditsSummary;
  raw?: Record<string, unknown>;
}
export interface RateLimitResetCreditsSummary {
  availableCount: number;
  // null means Codex exposed only the authoritative count; [] means it fetched
  // details and found no available detail rows. The count remains authoritative.
  credits: RateLimitResetCredit[] | null;
}
export interface RateLimitResetCredit {
  id?: string;
  resetType?: string;
  status?: string;
  grantedAt?: string;
  expiresAt?: string;
  title?: string;
  description?: string;
}
export type RateLimitResetOutcome = 'reset' | 'nothingToReset' | 'noCredit' | 'alreadyRedeemed';

// ---- Update notifications (WIRE-CONTRACT additions; daemon 9fe3af8) ----------
// `providers:updates` reports installed-vs-latest CLI versions for the detected
// ACP providers; entries only exist when BOTH versions resolved. `app:update`
// reports a workass self-update (mocked today). Both channels are replayed to
// fresh clients like providers:list/chat:catalog, so a reload paints them with
// no turn. Feature-detected (onProvidersUpdates / onAppUpdate); older bridges
// omit them → no cards.
export interface ProviderUpdate {
  providerId: string;
  cli: string;               // human CLI label, e.g. "Claude Code"
  installed: string;         // resolved installed version
  latest: string;            // resolved latest version
  updateAvailable: boolean;
  hint?: string;             // copy-only upgrade command, e.g. "claude update"
  // Additive failure fields from the last providers:update run for this provider
  // (WIRE-CONTRACT §5 providers:updates). Present after a click-to-update that
  // exited non-zero; the entry stays with updateAvailable:true so it can retry.
  lastError?: string;
  exitCode?: number;
  tail?: string;             // redacted bounded ≤4 KiB updater output tail
}
export interface ProvidersUpdates {
  checkedAt?: string;
  updates: ProviderUpdate[];
}
// Live click-to-update progress (WIRE-CONTRACT §5 providers:update-progress).
// Throttled ~1/s while running; terminal done/failed always emitted; the latest
// snapshot per provider is replayed to fresh clients. Feature-detected via
// onProvidersUpdateProgress → no live progress on an older bridge.
export interface ProviderUpdateProgress {
  providerId: string;
  status: 'running' | 'done' | 'failed';
  startedAt: string;
  tail: string;              // redacted bounded ≤4 KiB updater output tail
  exitCode?: number;         // terminal failure only
  error?: string;            // terminal failure only
}
export interface AppUpdate {
  version: string;
  notes?: string;
  mocked?: boolean;
}

// Additive: providers:list entries may carry the resolved CLI version.
export interface CliVersion { version: string; raw?: string; }

export interface ProviderRecord {
  id: string;
  name: string;
  enabled: boolean;
  status: string;
  message?: string;
  error?: string;
  fixHint?: string;
  badge?: string;
  detected?: boolean;
  latencyMs?: number;
  // Absolute executable path selected from the provider override or daemon PATH.
  resolvedCommand?: string;
  cliVersion?: CliVersion;
}

export type AcpEvent =
  | { kind: 'thinking'; text: string }
  | { kind: 'plan'; entries: Array<{ status: unknown; content: unknown }> }
  | { kind: 'steer-consumed'; clientUserMessageId: string }
  // Turn liveness pulse (additive, ~2s cadence while a turn runs): elapsed,
  // cumulative output tokens, phase (waiting/thinking/writing/tool/compacting)
  // and provider retry state. Absent on older daemons.
  | {
      kind: 'turn-heartbeat';
      elapsedMs?: number;
      outputTokens?: number;
      phase?: string;
      toolName?: string;
      retry?: { code?: number; attempt?: number } | null;
    }
  | {
      kind: 'tool';
      toolKind: string | null;
      id?: string | null;
      title: string;
      status: string;
      command?: string | null;
      terminalId?: string | null;
      input?: string | null;
      output?: string | null;
      location?: string | null;
      images?: Array<{ mimeType: string; data: string; name?: string }>;
      // Subagent linkage (additive; absent on the main thread / older daemons).
      // subagentId = the spawning tool call's id (parentToolUseId); subagentLabel
      // = that call's title; subagentProvider = model-family brand for the icon.
      subagentId?: string | null;
      subagentLabel?: string | null;
      subagentProvider?: string | null;
	  subagentHeader?: boolean;
	  // Friendly model+effort combo ("Opus4.8-xhigh") for the Turnos chip.
	  subagentModel?: string | null;
    };

export interface PermissionRequest {
  id: string;
  jobId: string | null;
  sessionId: string | null;
  // Optional exact owner hints. Current requests normally resolve through the
  // job id; these fields let a reconnect-safe request attach even if its start
  // event and local job map crossed in flight.
  tabId?: string | null;
  chatId?: string | null;
  title: string;
  kind: string | null;
  options: Array<{ optionId: string; name: string; kind: string }>;
  // Present only when the agent is ASKING something (AskUserQuestion) rather
  // than requesting a permission: the daemon forwards the model's question and
  // its choices, and `options` above carries one entry per choice. Optional, so
  // a daemon that predates it simply renders the plain permission card.
  question?: PermissionQuestion | null;
}
export interface PermissionQuestion {
  question: string;
  header: string;
  options: Array<{ label: string; description: string }>;
  multiSelect: boolean;
}
export interface PermissionResolved {
  id: string;
  jobId: string | null;
  sessionId: string | null;
  optionId: string | null;
  resolvedAt: string;
}

export interface ChatCatalog { models: ModelOption[]; modes: ModeOption[]; groups?: CatalogGroup[]; }

// Additive chat:compacted event (P4/D1 auto-compaction). Feature-detected —
// the daemon bridge may not expose onChatCompacted yet.
export interface ChatCompacted {
  chatId?: string | null;
  sessionId?: string | null;
  at?: string | null;
  summaryChars?: number;
}

export interface ProcessSummary {
  id: string;
  kind: string;
  label: string;
  pid: number | null;
  cwd: string;
  command: string;
  chatId: string | null;
  managed: boolean;
  engine: boolean;
  status: string;
  code: number | null;
  startedAt: string;
  finishedAt: string | null;
  lastLine: string;
  // Additive fields the daemon emits for ACP engines (feature-detected).
  state?: string;
  rssKb?: number;
  // Renderer-local: transient reason a stop attempt was refused by the daemon.
  _killError?: string;
}
export interface ProcChanged { processes: ProcessSummary[]; }

export interface SpawnedWorkItem {
  id: string;
  taskId: string;
  toolCallId?: string;
  tabId: string;
  chatId: string;
  providerId: string;
  kind: 'bash' | 'agent' | 'workflow' | 'background' | string;
  label: string;
  // Lifecycle, not shape: 'service' is a process expected never to finish (a
  // dev server), and absent or 'work' is work whose completion is news. A
  // service is alive without the chat being busy, so it never drives the
  // working state.
  role?: 'service' | 'work' | string;
  status: 'running' | 'exited' | 'failed' | string;
  startedAt: string;
  updatedAt: string;
  finishedAt?: string;
  outputFile?: string;
  pid?: number;
  exitCode?: number;
  summary?: string;
  lastToolName?: string;
}
// obligation is additive: what this chat still owes the user, which is a
// different question from what is running. Older daemons omit it.
export interface ChatObligation { state: string; source?: string; note?: string; promptId?: string }
export interface SpawnedWorkChanged { tabId: string; chatId: string; items: SpawnedWorkItem[]; obligation?: ChatObligation; }
export interface SpawnedWorkRead { ok: boolean; item?: SpawnedWorkItem; tail?: string; tailLimited?: boolean; error?: string; }

/** What one stop did. `alreadyFinished` is a success: the row was already over. */
export interface SpawnedWorkStop {
  ok: boolean; id?: string; status?: string; stopped?: boolean; cancelled?: boolean;
  alreadyFinished?: boolean; signalled?: number[]; forced?: number[]; summary?: string; error?: string;
}

export interface SessionReplaced {
  chatId: string | null;
  oldSessionId: unknown;
  session: AcpSessionInfo;
}

// P3b LAN device/access surface (docs/DAEMON-DEV.md, WIRE-CONTRACT §1b).
export interface LanDevice {
  deviceId: string;
  name: string;
  ip: string;
  lastSeen: string;
  controller: boolean;
}
export interface LanDevices { devices: LanDevice[]; }
export interface AccessRequest {
  requestId: string;
  ip: string;
  deviceName?: string;
  userAgent?: string;
  requestedAt?: string;
}

// Daemon config (config:get) — only the blocks Settings reads. Everything is
// optional so an older/newer daemon shape degrades to the honest empty state.
export interface DaemonConfig {
  acp?: { provider?: string; command?: string; args?: unknown[]; env?: Record<string, unknown>; probeTimeoutMs?: number };
  chat?: { spareSessions?: number; defaultModel?: string; defaultMode?: string; autoCompact?: boolean };
  ui?: { density?: string; theme?: string };
  [k: string]: unknown;
}

// app-chat:detect-acp probe result (feature-detected; bridge does not expose it today).
export interface DetectAcpResult { ok: boolean; detected?: unknown[]; results?: unknown[]; providers?: ProviderRecord[]; }

// ---- R4/R5 checkpoints, rewind, diff (daemon-owned; WIRE-CONTRACT §4b) -------
// The daemon registers chat:checkpoints/chat:rewind/chat:diff and emits
// chat:checkpoint-restored, but the browser LAN bridge does NOT yet map them to
// window.api methods, so every accessor below is feature-detected and inert
// against the current bridge. See GAPS in the batch report.
export interface ChatCheckpointRepo {
  name: string; path: string; branch?: string; ref?: string; commit?: string;
  observedTree?: string; changedFiles: number; skipped?: boolean; skipReason?: string;
}
export interface ChatCheckpoint { turnSeq: number; jobId: string; ts: string; repos: ChatCheckpointRepo[]; }
export interface ChatRewindResult { ok: boolean; chatId: string; turnSeq: number; repos: unknown[]; }
// reply.error for a refused rewind is a JSON string; this is its parsed shape.
export interface ChatStructuredError { code: string; message: string; fields?: Record<string, unknown>; }
export interface ChatDiffResult { chatId: string; turnSeq: number; repo: string; path: string; text: string; truncated: boolean; }

export interface ChatEnvFile { path: string; adds: number; dels: number; }
export interface ChatEnvRepo { name: string; branch: string; files: ChatEnvFile[]; adds: number; dels: number; filesTruncated: boolean; }
export interface ChatEnvPayload {
  chatId: string; tabId: string; cwd: string; repos: ChatEnvRepo[]; unchanged: string[];
  reposTruncated: boolean; filesTruncated: boolean; repoLimit: number; fileLimit: number; approximation: string;
}

export interface CheckpointRestored { chatId: string; turnSeq: number; repos: unknown[]; }
export interface EngineRecovered { chatId: string | null; tabId: string | null; oldSessionId: unknown; sessionId: string; at: string; }

// ---- R7 notifications (controller-only events; bridge does not subscribe yet) -
export interface ChatCommandsEvent { tabId?: string; chatId?: string; sessionId?: string; commandCatalog?: CommandCatalog | null; }
export interface ChatCommandsReply { supported?: boolean; live?: boolean; commandCatalog?: CommandCatalog | null; }

export interface NotifyEvent { title: string; body: string; tabId: string | null; ts?: string; }
export interface NotifyBacklog { items: NotifyEvent[]; }
export interface AgentApply {
  action: string;
  tabId?: string;
  chatId?: string;
  focus?: boolean;
  // action === 'session-controls-skipped' (additive): the daemon could not
  // apply the chat's stored model/mode at session startup — the chat is
  // RUNNING on something other than what the user configured.
  sessionId?: string;
  providerId?: string;
  requestedModelId?: string;
  requestedModeId?: string;
  reason?: string;
  error?: string;
}

// ---- Server-owned directory browsing ---------------------------------------
// The daemon lists ITS OWN filesystem: `path`/`parent` are absolute paths on the
// machine that runs Workass, never on the viewing device. `listDir(null)` opens
// that machine's current user home and returns its absolute path. `entries`
// holds directories only, already sorted. A folder the server could not read
// comes back with a redacted `error` and no entries. `createDir` is additive: it
// creates one direct child of the exact displayed parent without changing the
// frozen fs:list-dir semantics.
export interface DirEntry { name: string; path: string; }
export interface DirListing {
  path: string | null;
  parent: string | null;
  entries: DirEntry[];
  error?: string;
}
export interface DirCreateResult {
  name: string;
  path: string | null;
  parent: string | null;
  error?: string;
}

export interface StartJobOpts {
  kind: string;
  operationId: string;
  title?: string;
  chatId?: string;
  tabId?: string;
  sessionId?: string | null;
  cwd?: string | null;
  // Rides every turn; differing from the session's bound provider triggers the
  // daemon's cross-provider handover (context-seeded engine swap).
  providerId?: string | null;
  modelId?: string | null;
  modeId?: string | null;
  prompt?: string;
  // Renderer-generated stable ids for this optimistic turn. The daemon adopts
  // them so reconnect/archive copies cannot become duplicate message rows.
  userMessageId: string;
  assistantMessageId: string;
  // Stable originating renderer queue-row id. New daemons use it to fence
  // renderer start vs daemon adoption; old daemons ignore the additive field.
  queueId?: string;
  // Capability marker: a daemon that atomically finds this ordinary send busy
  // returns a durable FIFO receipt instead of a failed transcript turn.
  busyMode?: 'queue-v1';
  images?: Array<{ mimeType: string; data: string; name?: string }>;
  history?: Array<{ role: string; content: string; at?: string | null }>;
  context?: unknown;
}

export interface QueuedJobStart {
  queued: true;
  queueId: string;
  position: number;
  delivery: 'queue';
  queuedAt?: string;
  agentQueueRevision: number;
}

export type StartJobReply = PublicJob | QueuedJobStart;

export interface JobCancelResult {
  cancelled: boolean;
  reason: 'cancelled' | 'idle' | 'unknown' | string;
}

export interface StateDigestChat {
  tabId: string;
  chatId: string;
  actorRevision: number;
  presentationRevision?: number;
  runningJobId: string | null;
  lastMessageId: string | null;
  messageCount: number;
  queueLen: number;
  queueHeadId: string | null;
  agentQueueRevision: number;
  runtimeControlRevision: number;
  providerId: string | null;
  currentModelId: string | null;
  currentModeId: string | null;
  pendingPermissionIds: string[];
}

export interface StateDigest {
  chats: StateDigestChat[];
  globalRevision: number;
  catalogHash: Record<string, string>;
  settingsRevision: string;
  procHash: string;
}

// The subset of window.api this renderer consumes. Every method is optional so
// we can feature-detect against an older/newer bridge and degrade gracefully.
export interface WorkassApi {
  appMeta?: () => Promise<{ rootDir: string; workspaceDir: string; version: string; profile?: 'prod' | 'dev' | 'test' } & Record<string, unknown>>;
  // Additive, body-free reconciliation digest. Liveness uses appMeta so an
  // actor busy resuming a provider cannot masquerade as a daemon disconnect.
  stateDigest?: () => Promise<StateDigest>;
  getSettings?: () => Promise<unknown>;
  // Controller-gated write of the app-settings blob (settings:set). REPLACES the
  // stored blob, so callers round-trip getSettings and touch only their slice.
  // Feature-detected: an older bridge omits it → scores stay in-memory only.
  setSettings?: (settings: unknown) => Promise<unknown>;
  getConfig?: () => Promise<DaemonConfig>;
  getSession?: () => Promise<unknown>;
  saveSession?: (snap: unknown) => Promise<boolean | { ok: boolean; globalRevision: number }>;
  chatQueueReplace?: (opts: {
    tabId: string; chatId: string; operationId: string; expectedRevision: number; queue: unknown[];
  }) => Promise<{ ok: boolean; operationId: string; agentQueueRevision: number; actorRevision: number }>;
  chatCreate?: (opts: {
    tabId: string; chatId: string; operationId: string; focus: boolean; title: string; titleLocked: boolean;
    group: string | null; cwd: string | null; providerId: string | null; currentModelId: string | null;
    currentModeId: string | null; modelControls?: unknown;
  }) => Promise<{ ok: boolean; tabId: string; chatId: string; operationId: string; actorRevision: number; presentationRevision: number; globalRevision: number }>;
  chatPresentationSave?: (opts: {
    tabId: string; chatId: string; operationId: string; expectedRevision: number;
    title: string; titleLocked: boolean; group: string | null; draft: string; unread: boolean;
    settled: 'settled' | 'active' | ''; pane: 'rail' | 'browser' | null;
  }) => Promise<{ ok: boolean; operationId: string; presentationRevision: number; actorRevision: number }>;
  chatRuntimeControlsSave?: (opts: {
    tabId: string; chatId: string; operationId: string; expectedRevision: number;
    providerId: string; currentModelId: string | null; currentModeId: string | null;
    modelControls?: ModelControlMemory;
  }) => Promise<{
    ok: boolean; operationId: string; runtimeControlRevision: number; actorRevision: number;
    providerId: string; currentModelId: string | null; currentModeId: string | null;
    modelControls?: ModelControlMemory;
  }>;
  chatDelete?: (opts: { tabId: string; chatId: string; operationId: string; force: boolean }) => Promise<{ ok: boolean; operationId: string }>;
  // Folder browsing is server-owned: `listDir` walks the DAEMON's filesystem, so
  // an Electron, LAN or browser client all pick the same folders. The native
  // directory dialog (`dialog:pick-directory`, still on the bridge) is
  // deliberately NOT declared here — the user rejected it (2026-07-12) because a
  // remote client would otherwise pick a path off its own device.
  listDir?: (path: string | null) => Promise<DirListing>;
  createDir?: (parent: string, name: string) => Promise<DirCreateResult>;
  archiveAppend?: (tabId: string, messages: unknown[]) => Promise<boolean>;
  archiveLoad?: (tabId: string) => Promise<unknown[]>;
  visualizeHost?: (options: { tabId: string; chatId: string; path: string; mode?: 'wide'; title?: string }) => Promise<VisualizationRegistration>;
  appChatNewSession?: (opts: { cwd?: string | null; tabId?: string; chatId?: string; operationId: string; bridgeKey?: string; providerId?: string | null; sessionId?: string; refreshPlanUsage?: boolean; replaceSessionId?: string; workspaceRebind?: boolean; expectedWorkspaceRevision?: number }) => Promise<AcpSessionInfo>;
  // Account-scoped metadata read. It never binds/replaces a chat session and
  // never sends a provider prompt; the resulting snapshot arrives through the
  // existing chat:plan-usage event.
  appChatRefreshPlanUsage?: (providerId: string, routingChatId?: string) => Promise<{ ok: boolean; providerId: string }>;
  appChatCloseSession?: (sessionId: string) => Promise<boolean>;
  appChatReset?: () => Promise<boolean>;
  // Mid-turn steer (D2). Preload-only in the Electron host; the daemon bridge
  // may not expose it — feature-detected, with a local queue fallback.
  appChatSteer?: (
    sessionId: string,
    prompt: string,
    images?: unknown[],
    clientUserMessageId?: string,
    continuationAssistantMessageId?: string,
    boundary?: { assistantMessageId: string; contentOffset: number; resultOffset: number; eventCount: number },
  ) => Promise<{ ok: boolean; live?: boolean; queued?: boolean; daemonQueued?: boolean; interrupted?: boolean; unsupported?: boolean; strategy?: 'codex-live' | 'generic-live' | 'interrupt-queue' | 'queue' | 'uncertain'; turnId?: string; receipt?: boolean; error?: string }>;
  appChatUseRateLimitReset?: (
    providerId: string,
    sessionId: string | undefined,
    idempotencyKey: string,
    creditId?: string,
  ) => Promise<{ outcome: RateLimitResetOutcome; planUsage?: PlanUsageSnapshot }>;
  spawnedWorkList?: (tabId: string, chatId: string) => Promise<{ items: SpawnedWorkItem[]; obligation?: ChatObligation }>;
  spawnedWorkRead?: (tabId: string, chatId: string, id: string, tailBytes?: number) => Promise<SpawnedWorkRead>;
  spawnedWorkStop?: (tabId: string, chatId: string, id: string) => Promise<SpawnedWorkStop>;
  startJob?: (opts: StartJobOpts) => Promise<StartJobReply>;
  cancelJob?: (id: string) => Promise<boolean | JobCancelResult>;
  chatPermissionDecide?: (id: string, optionId: string) => Promise<{ ok: boolean }>;
  chatPendingPermissions?: () => Promise<{ permissions: PermissionRequest[] }>;
  getState?: () => Promise<unknown>;
  jiraGet?: () => Promise<unknown>;
  procList?: () => Promise<ProcChanged>;
  procKill?: (id: string, tree: boolean) => Promise<{ ok: boolean; error?: string; already?: boolean }>;
  // P3b LAN devices/access (controller-only mutations; feature-detected).
  lanDevices?: () => Promise<LanDevices>;
  lanRevoke?: (deviceId: string) => Promise<unknown>;
  lanAccessDecide?: (requestId: string, allow: boolean) => Promise<unknown>;
  // Optional agent probe — not exposed by the daemon bridge today; Probar is
  // hidden unless a future bridge surfaces it.
  appChatDetectAcp?: (opts?: Record<string, unknown>) => Promise<DetectAcpResult>;
  providersList?: () => Promise<ProviderRecord[]>;
  providersDetect?: (opts?: { provider?: string }) => Promise<DetectAcpResult>;
  // Click-to-update: run the daemon's provider-owned updater for a provider
  // (WIRE-CONTRACT §4 providers:update). Resolves { ok, providerId } once the
  // update is accepted and progress starts; rejects with a structured JSON error
  // string (providers:update-unknown-provider | -no-pending | -in-progress).
  // Feature-detected — an older bridge omits it → copy-command stays primary.
  providersUpdate?: (providerId: string) => Promise<{ ok: boolean; providerId: string }>;
  // R4/R5 (daemon channels landed; browser bridge does not map them yet — GAP).
  chatCheckpoints?: (opts: { chatId?: string; tabId?: string }) => Promise<ChatCheckpoint[]>;
  chatRewind?: (opts: { tabId: string; chatId: string; turnSeq: number; operationId: string }) => Promise<ChatRewindResult>;
  chatDiff?: (opts: { tabId: string; chatId: string; repo: string; path: string }) => Promise<ChatDiffResult>;
  chatEnvGet?: (opts: { chatId?: string; tabId?: string }) => Promise<ChatEnvPayload>;
  // R7 native bridge notify (Electron host / future daemon); renderer prefers
  // Web Notifications and only calls this when present.
  notify?: (title: string, body: string) => Promise<boolean>;
  onJobEvent?: (cb: (e: JobEvent) => void) => void;
  onChatCatalog?: (cb: (c: ChatCatalog) => void) => void;
  onChatSessionReplaced?: (cb: (e: SessionReplaced) => void) => void;
  onChatPermissionRequest?: (cb: (r: PermissionRequest) => void) => void;
  onChatPermissionResolved?: (cb: (r: PermissionResolved) => void) => void;
  // Additive provider plan-usage snapshot event (WIRE-CONTRACT §5). Replayed to
  // fresh clients like providers:list/chat:catalog. Feature-detected; older
  // bridges omit it → no plan section.
  onChatPlanUsage?: (cb: (s: PlanUsageSnapshot) => void) => void;
  onSpawnedWorkChanged?: (cb: (s: SpawnedWorkChanged) => void) => void;
  onProvidersList?: (cb: (providers: ProviderRecord[]) => void) => void;
  onProcChanged?: (cb: (e: ProcChanged) => void) => void;
  onLanAccessRequest?: (cb: (r: AccessRequest) => void) => void;
  // ---- machines (remote-plan E1/E3) --------------------------------------
  // Every one optional: a daemon built before the machine book answers "unknown
  // channel", and `has()` upstream degrades to a single-machine client.
  machinesList?: () => Promise<{ machines?: unknown[]; self?: { machineId?: string; name?: string } } | undefined>;
  machinesAdd?: (address: string) => Promise<unknown>;
  machinesForget?: (machineId: string) => Promise<unknown>;
  machinesRefresh?: () => Promise<unknown>;
  onMachinesChanged?: (cb: (payload: { machines?: unknown[]; self?: { machineId?: string; name?: string } }) => void) => void;
  // The fleet key, readable from the app instead of a terminal. Listing is
  // non-secret; the other three are refused (fleet:not-local) unless this client
  // runs on the machine that holds the key, so the secret stays off the network.
  fleetKeys?: () => Promise<{ machineId?: string; keys?: unknown[]; canReveal?: boolean } | undefined>;
  fleetReveal?: (keyId?: string) => Promise<{ keyId?: string; secret?: string } | undefined>;
  fleetMint?: () => Promise<{ keyId?: string; secret?: string; minted?: boolean } | undefined>;
  fleetForget?: (keyId: string) => Promise<unknown>;
  // Additive update-notification events (WIRE-CONTRACT additions). Both replayed
  // to fresh clients; feature-detected → no cards on an older bridge.
  onProvidersUpdates?: (cb: (e: ProvidersUpdates) => void) => void;
  // Live click-to-update progress stream (feature-detected). Latest snapshot per
  // provider replayed to fresh clients like providers:updates.
  onProvidersUpdateProgress?: (cb: (e: ProviderUpdateProgress) => void) => void;
  onAppUpdate?: (cb: (e: AppUpdate) => void) => void;
  // Additive per-chat changed-file snapshot (chat:env), emitted on session seed
  // and after every turn. NOT in the provider replay set, so a fresh client
  // hydrates via chat:env-get (see store.refreshChatEnv) and this event only
  // keeps it live. Feature-detected → no-op on an older bridge.
  onChatEnv?: (cb: (e: ChatEnvPayload) => void) => void;
  // Additive compaction indicator (D1). Feature-detected; degrades to no-op.
  onChatCompacted?: (cb: (e: ChatCompacted) => void) => void;
  // R4 rewind confirmation + R7 notify events + D4 recovery notice. All
  // feature-detected; the browser bridge does not subscribe to them yet (GAP).
  onChatCheckpointRestored?: (cb: (e: CheckpointRestored) => void) => void;
  onChatEngineRecovered?: (cb: (e: EngineRecovered) => void) => void;
  onNotify?: (cb: (e: NotifyEvent) => void) => void;
  onNotifyBacklog?: (cb: (e: NotifyBacklog) => void) => void;
  // Daemon-owned agent chat mutations ask the renderer to rehydrate the
  // authoritative session snapshot. Exact identity is diagnostic; the snapshot
  // itself remains the authority.
  onAgentApply?: (cb: (e: AgentApply) => void) => void;
  // Per-chat provider catalog: live replaces via the event, late joins via the
  // invoke (the catalog is daemon-memory-only, so hydration cannot carry it).
  onChatCommands?: (cb: (e: ChatCommandsEvent) => void) => void;
  chatCommandsGet?: (tabId: string, chatId: string) => Promise<ChatCommandsReply>;
}

export interface VisualizationRegistration {
  id: string;
  label: string;
  entry: string;
  contentType: string;
  urlPath: string;
  localUrl?: string;
  markdown: string;
  createdAt: string;
  updatedAt: string;
  mode?: '' | 'wide';
  title?: string;
}

export interface WorkassWindowApi {
  platform: string;
}

declare global {
  interface Window {
    api?: WorkassApi;
    workassWindow?: WorkassWindowApi;
    __workassSocketGen?: number;
  }
}
