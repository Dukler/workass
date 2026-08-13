import type { ModelOption, ModeOption, ProcessSummary, LanDevice, AccessRequest, DaemonConfig, CatalogGroup, ChatCheckpoint, ChatEnvRepo, ChatEnvPayload, ChatDiffResult, SlashCommand, CommandCatalog, PlanUsageSnapshot, PermissionQuestion, ProviderRecord, ProviderUpdate, ProviderUpdateProgress, AppUpdate, SpawnedWorkItem } from '../wire/types';
import type { ModelFavorite } from '../model-favorites';
import type { ConnStatus } from '../wire/connection';
import type { ModelControlMemory } from '../model-controls';
import type { ModelScores } from '../model-scores';
import type { WorkassRuntimeProfile } from '../model-catalog';
import type { ContextUsageByProvider } from '../context-usage';

export type MsgStatus = 'pending' | 'running' | 'done' | 'failed' | 'cancelled';
export type SteerState = 'sending' | 'accepted' | 'applied' | 'uncertain';

export interface ThinkingEvent { key: string; at: number; kind: 'thinking'; text: string; }
export interface PlanEntry { status: string; content: string; }
export interface PlanEvent { key: string; at: number; kind: 'plan'; entries: PlanEntry[]; }
export interface ToolEvent {
  key: string; at: number; kind: 'tool';
  id: string | null; toolKind: string | null; title: string; status: string;
  command: string | null; terminalId: string | null; input: string | null;
  output: string | null; location: string | null;
  // Raster images returned structurally by the tool. They render openly below
  // the folded tool row while verbose textual output remains opt-in.
  images?: MessageImage[];
  // Real wall-clock timing observed by the renderer: stamped when the tool event
  // first appears (startedAt) and when it reaches a terminal status (endedAt).
  // Optional so reloaded history that predates timing degrades gracefully (no
  // duration shown) — never invented.
  startedAt?: number; endedAt?: number;
  // Subagent linkage (additive). When present, this call was made INSIDE a Task
  // subagent: subagentId = the spawning Task call's id; subagentLabel = that
  // call's title (the subagent's description); subagentProvider = model-family
  // brand ('gpt' | 'claude') for its icon. Absent on main-thread calls.
  subagentId?: string | null; subagentLabel?: string | null; subagentProvider?: string | null;
  subagentHeader?: boolean;
  // Friendly model+effort combo for the Turnos chip, e.g. "Opus4.8-xhigh".
  // Additive: absent against an older daemon → the chip is simply hidden.
  subagentModel?: string | null;
}
// Quiet in-transcript marker that the daemon auto-compacted the context (D1).
export interface CompactionEvent { key: string; at: number; kind: 'compaction'; }
// Quiet step row emitted after a successful rewind (R4/D3): "Estado restaurado
// a antes del turno N ›". Also used to render an honest rewind refusal inline.
export interface RestoredEvent { key: string; at: number; kind: 'restored'; turnSeq: number; }
export type TimelineEvent = ThinkingEvent | PlanEvent | ToolEvent | CompactionEvent | RestoredEvent;

export interface PermissionState {
  id: string; title: string; kind: string | null;
  options: Array<{ optionId: string; name: string; kind: string }>;
  resolved?: string;
  // Set when this is a question from the agent, not a permission: the card
  // renders the question and its choices instead of "Quiere ejecutar <tool>".
  question?: PermissionQuestion | null;
}

// An image that was accepted as part of a user turn. Unlike DraftImage this is
// transcript content: it renders with the sent message and rides the daemon
// session mirror so switching chats/reconnecting cannot make it disappear.
// `data` is raw base64; the renderer reconstructs a data URL at paint time.
export interface MessageImage {
  mimeType: string;
  data: string;
  name?: string;
  // Present when Workass imported ordinary ACP-authored ![label](path)
  // Markdown. It binds the image token to durable bytes and lets the renderer
  // suppress a redundant matching Open link; source-less structured ACP images
  // render as a gallery.
  source?: string;
}

// One queued follow-up: a distinct, editable, reorderable, removable message
// (queue redesign 2026-07-12) — replaces the old single concatenated `queued`
// string. Sent one at a time at each turn's end, in list order.
export interface QueuedMsg {
  id: string;
  text: string;
  source?: 'agent' | 'host';
  delivery?: 'auto' | 'queue' | 'steer';
  queuedAt?: string;
  // Attachments are part of the queued message, not part of the composer's
  // next draft. They use the same wire-safe shape as sent transcript images so
  // the queue can persist in the daemon mirror and dispatch without conversion.
  images?: MessageImage[];
  // Queueing must be an immediate interaction even when images are huge. The
  // browser-owned drafts/previews stay runtime-only while base64 is prepared
  // asynchronously; only `images` crosses the daemon/session boundary.
  draftImages?: DraftImage[];
  attachmentNames?: string[];
  attachmentState?: 'preparing' | 'ready' | 'failed';
  attachmentError?: string;
}

export interface Msg {
  id: string;
  role: 'user' | 'assistant';
  content: string;
  // Provider-typed final answer. It is absent for phase-less/older providers;
  // when present it renders once as the turn's transcript-native conclusion.
  result?: string;
  status: MsgStatus;
  at: string | null;
  events: TimelineEvent[];
  // Delivery lifecycle for an explicit live steer. It is independent of the
  // assistant turn status: sending waits for the RPC acknowledgement, accepted
  // is the official turn/steer boundary, applied is the canonical client-id
  // receipt, and uncertain is a transport outcome that must not be replayed.
  steerState?: SteerState;
  // Native Codex stages admitted input until its canonical userMessage receipt
  // arrives between sampling steps. The durable waiting pair is rendered as a
  // composer-adjacent preview, not as transcript history, until that boundary.
  steerBoundary?: 'waiting';
  steerContinuationId?: string;
  steerContinuationFor?: string;
  // A native provider turn may contain several assistant transcript segments
  // separated by user steering rows. All segments share turnRootId; only the
  // last one is turnTerminal and owns completion chrome/checkpoint actions.
  // Ordinary unsplit turns omit both fields and remain terminal by default.
  turnRootId?: string;
  turnTerminal?: boolean;
  images?: MessageImage[];
  permission?: PermissionState;
  jobId?: string;
  turnStartedAt?: number;
  // A turn cut off by a daemon disconnect (not a model/agent error). Renders a
  // quiet "sin conexión" row instead of the generic error stamp, and — when the
  // originating prompt is known — a Reintentar affordance rather than spinning.
  interrupted?: boolean;
  // The user prompt to resend when retrying an interrupted/failed-send turn.
  retryPrompt?: string;
}

// A not-yet-sent image belongs to one chat draft, just like `draft` text. Keep
// the browser File plus a zero-copy object URL on the interactive path; raw
// base64 is produced only at send time. `data` remains optional for fixture
// drafts created before object-URL previews existed.
//
// Draft images are deliberately runtime-only: persisting Files, object URLs, or
// screenshots in the localStorage/session JSON mirror can exceed its quota and
// break all chat persistence. They do survive chat switches and same-renderer
// WS reconnects while this renderer remains alive.
export interface DraftImage {
  id: string;
  name: string;
  mimeType: string;
  file?: File;
  data?: string;
  url: string;
}

export interface Workspace { path: string; name: string; }

export interface Chat {
  // The chat's most recent plan (the agent's mini step-by-step). A plan update
  // only arrives when the agent edits its todo list, and the message that
  // carried it ages out of the mirror — holding it on the chat is what keeps the
  // rail's step-by-step alive across turns and reloads (user, 2026-07-23).
  planLatest?: PlanEntry[];
  // Assistant message that authored the snapshot above, so a completed plan can
  // be cleared when a newer turn starts instead of lingering for the whole chat.
  planLatestMessageId?: string;
  // The machine this chat lives on, '' or absent for this one (remote-plan E3).
  // It is a PROPERTY OF THE CHAT, never of the window: one list holds chats from
  // every paired daemon, ordered by recency across all of them. Its id is tagged
  // at the socket boundary (`M~<machine>~<id>`), so nothing below this needs to
  // know — and toMirror() excludes these, because another machine persists them.
  machineId?: string;
  id: string;              // stable tabId
  // Stable conversation id (R4). Persisted and reused across turns so the daemon
  // accumulates checkpoints + increments turnSeq under one chatId. Distinct from
  // `id` (the UI tab). Older mirrors lack it → backfilled on load.
  chatId?: string;
  actorRevision?: number;
  sessionId: string | null;
  // Provider that owns `sessionId` right now. It can intentionally differ from
  // providerId while a different agent is staged for the next turn. Keeping
  // this provenance prevents old-engine capabilities from disabling the new
  // provider's first image and prevents reload from erasing a staged pick.
  sessionProviderId?: string | null;
  title: string;
  titleLocked: boolean;
  group: string | null;
  cwd: string | null;
  // Daemon-owned CAS revision for explicit cross-folder moves. Once non-zero,
  // every later drag must use the transactional rebind channel, even while the
  // chat is temporarily sessionless between move and next send.
  workspaceRevision?: number;
  presentationRevision?: number;
  // Daemon-issued revision for agent-control queue rows. Renderer saves echo
  // it unchanged; the daemon owns increments on enqueue/consume and accepts a
  // user edit/removal only from the current revision.
  agentQueueRevision?: number;
  // Daemon-issued fence for exact-chat provider/model/mode commits. Renderer
  // mirrors echo it unchanged so an older save cannot undo agent-side recovery.
  runtimeControlRevision?: number;
  currentModelId: string | null;
  currentModeId: string | null;
  // Last explicit effort + permission selection for each provider/model in
  // THIS chat. Switching away and back restores the same setup without leaking
  // another tab's controls or sending an id from the previous provider.
  modelControls?: ModelControlMemory;
  // R6: image attach cap + agent-advertised slash commands, learned from the
  // session info (both absent on the mock → feature-detected off).
  imageSupport?: boolean;
  commands?: SlashCommand[];
  // Rich provider catalog (commands + agents + styles). Mirrors `commands`'
  // lifecycle exactly: learned from session info, cleared with the session,
  // never persisted beyond it.
  commandCatalog?: CommandCatalog;
  // The daemon could not apply this chat's configured model/mode at session
  // startup (agent:apply action session-controls-skipped). The receipt row
  // shows while the running selection still differs from the request.
  controlsSkipped?: { requestedModelId: string; requestedModeId?: string; reason: string; error?: string; at: number };
  // R4: checkpoints for this chat, refreshed on turn end and when the rewind
  // menu / review panel opens. Used to map a completed turn (by jobId) to its
  // checkpoint for the per-turn Deshacer/Revisar affordances.
  checkpoints?: ChatCheckpoint[];
  // Provider binding (P4 registry). Set at creation from the picked group;
  // rides app-chat:new-session's additive providerId option. providerName is
  // resolved for the quiet header badge.
  providerId?: string | null;
  providerName?: string | null;
  // Which right-column occupant THIS chat shows: 'rail' (info rail), 'browser'
  // (live browser), or null (closed). `undefined` = never chosen → defaults to
  // the info rail. Per-chat so an agent opening a browser in its own chat never
  // touches the pane of the chat you are viewing (user law 2026-07-12). Persisted.
  pane?: RightPane | null;
  pending: boolean;
  sessionError?: string;
  messages: Msg[];
  draft: string;
  draftImages?: DraftImage[];
  unread?: boolean;
  // Sidebar shelf override, T3's settledOverride: 'settled' files a quiet chat
  // away by hand, 'active' pins one that age would otherwise file for you.
  // Absent = let the age rule decide.
  settled?: 'settled' | 'active';
  // Last provider-authored context reading per chat+provider. Durable so a
  // renderer/daemon restart never makes the user send a model turn merely to
  // recover the status indicator.
  contextUsageByProvider?: ContextUsageByProvider;
  // Locally-queued follow-ups (R2 fallback when appChatSteer is absent). Sent
  // one at a time at each turn's end, in order. Editable/reorderable/removable.
  queue?: QueuedMsg[];
  _initPromise?: Promise<void>;
  // Stable until the daemon returns a receipt for this exact provider-lane
  // selection. A lost app-chat:new-session reply must retry the same actor
  // operation instead of committing a second control mutation.
  _sessionOperationId?: string;
  // Monotonic runtime-only picker epoch. Session initialization snapshots it
  // so an older inherited-provider response cannot overwrite a newer explicit
  // model/provider/effort/permission selection.
  _controlRevision?: number;
  messageCount?: number;
  historyComplete?: boolean;
}

// The one right-hand column hosts a single occupant, chosen PER CHAT (never a
// global flag): the info rail, the live browser, or nothing. Kept here (not in
// right-pane.ts) so `Chat` can reference it without an import cycle.
export type RightPane = 'rail' | 'browser';

// Global right-column layout preferences (shared across chats). Which occupant
// is shown is per-chat (`Chat.pane`); these are only sizes/visibility that the
// user drags once and expects everywhere.
export interface Panes { side: boolean; railWide: boolean; sideW: number; railW: number; }

export type ThemePref = 'system' | 'light' | 'dark';
export type Density = 'compact' | 'comfortable';
export interface MachineView {
  machineId: string;
  name: string;
  address: string;
  link: 'idle' | 'connecting' | 'open' | 'ready' | 'closed' | 'rejected';
  reason: string;
  secure: boolean;
	reachable: boolean;
  paired: boolean;
  requested: boolean;
  discovered: boolean;
}

/** One fleet key as it is safe to show: identity and provenance, never the secret. */
export interface FleetKeyView {
  keyId: string;
  owner: string;
  label: string;
  createdAt: string;
}

export type SettingsSection = 'agentes' | 'modelos' | 'maquinas' | 'dispositivos' | 'engines' | 'apariencia' | 'atajos';

export interface AppState {
  chats: Chat[];
  workspaces: Workspace[];
  // Workspace folder paths the user has collapsed in the sidebar. Persisted so a
  // reload keeps them folded. Paths (normalized); '' is the unassigned "Chats".
  collapsedWorkspaces: string[];
  // Folder paths the user explicitly removed from the sidebar. Persisted, and
  // EXCLUDED from workspace inference — a folder can otherwise re-appear from a
  // chat whose bound ACP session still runs in that dir (liveSession.cwd wins
  // over the app-side cwd on reload). Re-adding the folder clears it here.
  removedWorkspaces: string[];
  activeId: string | null;
  seq: number;
  globalRevision: number;
  models: ModelOption[];
  modes: ModeOption[];
  // Provider-grouped catalog (P4). Empty against an older daemon → the composer
  // falls back to the flat model list.
  groups: CatalogGroup[];
  providers: ProviderRecord[];
  // User-authored per-model scores (Settings · Modelos). A durable PREFERENCE —
  // the user's own rating of each catalog model, not any vendor claim — keyed by
  // providerId → base modelId. Persisted through the daemon app-settings blob
  // (settings:get / settings:set), the same authority the agent-facing catalog
  // reads back — never the session mirror or localStorage.
  modelScores: ModelScores;
  // Global model shortcuts, keyed by provider + base model id. Like scores,
  // these live in daemon-owned app settings so every controller sees the same
  // favorites and a renderer reload cannot lose a click.
  modelFavorites: ModelFavorite[];
  // Latest plan-usage snapshot per providerId (chat:plan-usage event + additive
  // planUsage on usage job events). The composer's context popover renders only
  // the ACTIVE CHAT'S provider snapshot. Transient — never persisted.
  planUsageByProvider: Record<string, PlanUsageSnapshot>;
  // Runtime-only request state for the provider-scoped metadata read. This is
  // separate from chat/session pending state because plan usage must load even
  // while another provider owns the active turn.
  planUsageLoadingByProvider: Record<string, boolean>;
  theme: 'dark' | 'light';       // effective (resolved) theme applied to the DOM
  themePref: ThemePref;          // user preference; 'system' follows the OS
  density: Density;
  panes: Panes;
  mode: 'assist' | 'chats';
  hydrated: boolean;
  // Live daemon-connection health (client-side heartbeat, see wire/connection.ts).
  // Drives the "sin conexión" banner and blocks sends while offline. Transient —
  // never persisted; a fresh load starts optimistically 'connected'.
  connection: ConnStatus;
  meta?: { rootDir?: string; workspaceDir?: string; profile?: WorkassRuntimeProfile; daemon?: boolean; sessionSaveMode?: 'lean-payload-v2'; workspaceRebindMode?: 'transactional-v1' };
  hasBrowserChannels: boolean;
  processes: ProcessSummary[];
  hasProcChannels: boolean;
  // Provider-native background Bash/Agent/Workflow work keyed by exact
  // tabId+chatId. Transient; the daemon owns snapshots and durable receipts.
  spawnedWorkByChat: Record<string, SpawnedWorkItem[]>;
  obligationByChat: Record<string, { state: string; source?: string }>;
  hasSpawnedWorkChannels: boolean;
  // Latest per-chat changed-file snapshot (chat:env event + chat:env-get), keyed
  // by exact tabId+chatId. Drives the Entorno rail card. Transient — the daemon
  // recomputes it from git each turn, so it is never persisted.
  chatEnvByChat: Record<string, ChatEnvPayload>;
  // Settings view (full-view takeover). Transient — not persisted.
  settingsOpen: boolean;
  settingsSection: SettingsSection;
  // ⌘, command box. Transient, and deliberately the ONLY piece of app state it
  // owns: the box must be openable when everything downstream of the socket is
  // broken, so it never waits on hydration or on being the controller.
  commandBarOpen: boolean;
  // remote-plan E3. Empty on a daemon without the machine book, which is what
  // makes the whole surface degrade to "this machine only" rather than break.
  machines: MachineView[];
  hasMachineChannels: boolean;
  /** True once a fleet key is held on this device; the key itself never enters state. */
  hasFleetKey: boolean;
  /**
   * The keys THIS daemon accepts — ids and dates, never secrets, so the list is
   * safe to hold in state and safe to render. The secret is fetched on an
   * explicit click and lives only in the pane that showed it.
   */
  fleetKeys: FleetKeyView[];
  /** Whether this client may read a secret at all: only a client on the machine that holds it. */
  fleetCanReveal: boolean;
  hasFleetKeyChannels: boolean;
  // Dispositivos (real, feature-detected against the daemon LAN channels).
  hasDeviceChannels: boolean;
  devices: LanDevice[];
  accessRequests: AccessRequest[];
  // Daemon config snapshot (config:get) — powers Agentes + Engines honestly.
  daemonConfig?: DaemonConfig;
  hasConfigChannel: boolean;
  // R4 rewind menu (checkpoint list overlay). Transient.
  rewind: RewindState;
  // R5 Revisar diff panel (right-side, rail-wide pattern). Transient.
  review: ReviewState;
  // R7 desktop notifications. `notifEnabled` is a local opt-in preference
  // (mirrored); permission is the browser's live grant. Toasts are the in-app
  // fallback when permission is denied/unsupported.
  notifEnabled: boolean;
  notifPermission: NotificationPermission | 'unsupported';
  toasts: Toast[];
  // Update notifications (providers:updates + app:update events; both replayed to
  // fresh clients). Transient — never persisted; a fresh load repaints from the
  // daemon's replay.
  providersUpdates: ProviderUpdate[];
  providersCheckedAt?: string;
  // Live click-to-update progress, keyed by providerId (providers:update-progress
  // event + an optimistic 'running' entry set the instant the button is clicked,
  // so the UI flips before the first throttled snapshot arrives). Transient —
  // never persisted; the daemon replays the latest snapshot per provider on
  // connect. A terminal done/failed entry lingers until the next providers:updates
  // drops (success) or re-stamps (failure) the row.
  updateProgress: Record<string, ProviderUpdateProgress>;
  // Sequential click-to-update chain driven by the footer provider card. `ids` is
  // the batch snapshot (for the "(n/N)" counter and to survive entries dropping
  // out of providersUpdates as they succeed); `index` is the 1-based position of
  // the CLI currently updating; `failedId` is set when a step fails and the chain
  // stops. Transient — cleared once every CLI in the batch succeeds.
  updateChain?: { ids: string[]; index: number; failedId: string | null };
  appUpdate?: AppUpdate;
  // Transient: pulse+scroll the rail Tareas card when the header bg-process chip
  // is clicked. Not persisted.
  flashTareas?: boolean;
  // Transient: a chat image opened full-size in the lightbox overlay (click an
  // image in the transcript). null when closed. Never persisted.
  imageLightbox?: { src: string; alt: string } | null;
}

export interface RewindState {
  open: boolean;
  tabId: string | null;
  chatId: string | null;       // resolved daemon chatId (rewind needs it)
  loading: boolean;
  items: ChatCheckpoint[];
  error?: string;              // structured refusal or "unavailable" note
  focusTurn?: number;          // turn to highlight when opened from a turn affordance
  busyTurn?: number;           // a rewind is in flight for this turnSeq
  operationId?: string;        // stable until this exact rewind has a receipt
  operationTurn?: number;
}

export interface ReviewState {
  open: boolean;
  tabId: string | null;
  chatId: string | null;
  loading: boolean;
  repos: ChatEnvRepo[];
  active?: { repo: string; path: string };
  diff?: ChatDiffResult;
  diffLoading: boolean;
  error?: string;
}

export interface Toast { id: string; title: string; body: string; }
