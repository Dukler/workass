#!/usr/bin/env node

// Workass-owned Claude Code host. It speaks the existing private Workass
// session contract on stdio while using Anthropic's official Agent SDK and
// the user's installed Claude Code executable underneath. stdout is protocol
// JSON only; diagnostics go to stderr.

import { createHash, randomUUID } from 'node:crypto';
import os from 'node:os';
import path from 'node:path';
import { readFileSync } from 'node:fs';
import process from 'node:process';
import readline from 'node:readline';
import { pathToFileURL } from 'node:url';

const sessions = new Map();
const pendingPeerRequests = new Map();
// Mirrors subagentQuestionOptionID in internal/acp/manager.go: the daemon
// answers a spawned subagent's question with this instead of showing a card.
const SUBAGENT_QUESTION_OPTION = 'question-subagent';
let peerRequestSequence = 0;
let sdkPromise;

const transientOAuthRefreshError = 'Failed to authenticate: OAuth session expired and could not be refreshed';

function write(message) {
  process.stdout.write(`${JSON.stringify(message)}\n`);
}

function respond(id, result) {
  write({ jsonrpc: '2.0', id, result });
}

function fail(id, code, message) {
  write({ jsonrpc: '2.0', id, error: { code, message: safeErrorText(message) } });
}

function safeErrorText(value) {
  return String(value?.message || value || 'Claude Code request failed')
    .replace(/(api[_-]?key|token|secret(?:[_-]?(?:access[_-]?)?key)?|password|credential|bearer)["']?\s*[:=]\s*\S+/gi, '$1=[redacted]')
    .replace(/Bearer\s+(?!\[redacted\])\S+/gi, 'Bearer [redacted]')
    .slice(0, 2000);
}

function opaqueRealmScope(kind, value) {
  const normalized = String(value || '').trim();
  if (!normalized) return '';
  return `${kind}-${createHash('sha256').update(normalized).digest('hex').slice(0, 32)}`;
}

function diagnostic(label, error) {
  process.stderr.write(`${label}: ${safeErrorText(error)}\n`);
}

function isSessionInUseError(error) {
  return /session id .* is already in use/i.test(String(error?.message || error || ''));
}

function isRecoverableQueryTransportError(error) {
  const text = String(error?.message || error || '');
  return /ProcessTransport is not ready for writing/i.test(text);
}

function claudeResultError(result) {
  const messages = [];
  for (const value of Array.isArray(result?.errors) ? result.errors : []) {
    const text = String(value || '').trim();
    if (text && !messages.includes(text)) messages.push(text);
  }
  const resultText = String(result?.result || '').trim();
  if (resultText && !messages.includes(resultText)) messages.push(resultText);
  if (messages.length) return messages.join('; ');
  const subtype = String(result?.subtype || '').trim();
  return subtype ? `Claude Code turn failed: ${subtype}` : 'Claude Code turn failed';
}

function isTransientOAuthRefreshFailure(value) {
  return /failed to authenticate:\s*oauth session expired and could not be refreshed/i
    .test(String(value?.message || value || ''));
}

function notify(sessionId, update) {
  write({ jsonrpc: '2.0', method: 'session/update', params: { sessionId, update } });
}

// Turn-lifecycle payloads are bounded on both axes: 20 entries per list and 200
// chars per field. The harness caps its own free text at 1000 chars, but that
// text never enters these payloads at all.
const maxTurnListEntries = 20;

function clipTurnField(value) {
  if (value === undefined || value === null) return '';
  return String(value).slice(0, 200);
}

function boundedTurnList(name, items, project) {
  const kept = items.slice(0, maxTurnListEntries).map(project);
  return { [name]: kept, [`${name}Truncated`]: Math.max(0, items.length - kept.length) };
}

// Claude commands surface (docs/specs/claude-commands-surface.md §2): the
// commandCatalog that rides open replies and _workass_claude_commands updates.
// Clip a too-long field, never drop it; DROP overflow entries and count them in
// the *Truncated counters (precedent: boundedTurnList/clipTurnField above).
// Memory-only on every hop — the catalog is never persisted anywhere. An absent
// source list is omitted from the catalog (UNKNOWN); [] is proven empty; the
// two must never collapse.
const maxCatalogCommands = 512;
const maxCatalogAgents = 128;
const maxCatalogStyles = 64;
const maxCatalogNameChars = 80;
const maxCatalogDescriptionChars = 200;
const maxCatalogAliases = 4;

function clipCatalogField(value, max) {
  if (value === undefined || value === null) return '';
  return String(value).slice(0, max);
}

function clampCatalogCommands(commands) {
  const kept = commands.slice(0, maxCatalogCommands).map((command) => ({
    name: clipCatalogField(command?.name, maxCatalogNameChars),
    description: clipCatalogField(command?.description, maxCatalogDescriptionChars),
    argumentHint: clipCatalogField(command?.argumentHint, maxCatalogNameChars),
    ...(Array.isArray(command?.aliases) && command.aliases.length
      ? { aliases: command.aliases.slice(0, maxCatalogAliases).map((alias) => clipCatalogField(alias, maxCatalogNameChars)) }
      : {}),
  }));
  return { commands: kept, commandsTruncated: Math.max(0, commands.length - kept.length) };
}

function clampCommandCatalog(initialized) {
  const catalog = { asOf: Date.now() };
  if (Array.isArray(initialized?.commands)) Object.assign(catalog, clampCatalogCommands(initialized.commands));
  if (Array.isArray(initialized?.agents)) {
    catalog.agents = initialized.agents.slice(0, maxCatalogAgents).map((agent) => ({
      name: clipCatalogField(agent?.name, maxCatalogNameChars),
      description: clipCatalogField(agent?.description, maxCatalogDescriptionChars),
      ...(agent?.model ? { model: clipCatalogField(agent.model, maxCatalogNameChars) } : {}),
    }));
    catalog.agentsTruncated = Math.max(0, initialized.agents.length - catalog.agents.length);
  }
  if (typeof initialized?.output_style === 'string' && initialized.output_style) {
    catalog.outputStyle = clipCatalogField(initialized.output_style, maxCatalogNameChars);
  }
  if (Array.isArray(initialized?.available_output_styles)) {
    catalog.availableOutputStyles = initialized.available_output_styles
      .slice(0, maxCatalogStyles)
      .map((style) => clipCatalogField(style, maxCatalogNameChars));
    catalog.stylesTruncated = Math.max(0, initialized.available_output_styles.length - catalog.availableOutputStyles.length);
  }
  return catalog;
}

// Plumbing RPCs (fs reads, terminals) get a guard so a client that never
// answers cannot wedge a turn. Anything a HUMAN answers passes 0 and waits:
// this timer used to resolve null at 120s, which the model reads as "the user
// chose nothing" while their card is still on screen and clickable
// (user 2026-07-25). The origin harness owns that deadline now.
const PEER_REQUEST_TIMEOUT_MS = (() => {
  const configured = Number.parseInt(String(process.env.WORKASS_ACP_PEER_TIMEOUT_MS || ''), 10);
  return Number.isFinite(configured) && configured > 0 ? configured : 120_000;
})();

function peerRequest(method, params, timeoutMs = PEER_REQUEST_TIMEOUT_MS) {
  const id = `claude-host-${++peerRequestSequence}`;
  write({ jsonrpc: '2.0', id, method, params });
  return new Promise((resolve) => {
    const timer = timeoutMs > 0
      ? setTimeout(() => {
        pendingPeerRequests.delete(id);
        resolve(null);
      }, timeoutMs)
      : null;
    pendingPeerRequests.set(String(id), (message) => {
      if (timer) clearTimeout(timer);
      resolve(message?.result ?? null);
    });
  });
}

async function loadSDK() {
  if (!sdkPromise) {
    const configured = String(process.env.WORKASS_CLAUDE_SDK_MODULE || '').trim();
    if (!configured) throw new Error('WORKASS_CLAUDE_SDK_MODULE is required');
    const specifier = path.isAbsolute(configured) ? pathToFileURL(configured).href : configured;
    sdkPromise = import(specifier);
  }
  return sdkPromise;
}

class AsyncInputQueue {
  constructor() {
    this.values = [];
    this.waiters = [];
    this.closed = false;
  }

  push(value) {
    if (this.closed) throw new Error('Claude input queue is closed');
    const waiter = this.waiters.shift();
    if (waiter) waiter({ value, done: false });
    else this.values.push(value);
  }

  close() {
    this.closed = true;
    for (const waiter of this.waiters.splice(0)) waiter({ value: undefined, done: true });
  }

  [Symbol.asyncIterator]() { return this; }

  next() {
    if (this.values.length) return Promise.resolve({ value: this.values.shift(), done: false });
    if (this.closed) return Promise.resolve({ value: undefined, done: true });
    return new Promise((resolve) => this.waiters.push(resolve));
  }
}

function promptContent(blocks) {
  const content = [];
  for (const block of Array.isArray(blocks) ? blocks : []) {
    if (!block || typeof block !== 'object') continue;
    if (block.type === 'text' && typeof block.text === 'string') {
      content.push({ type: 'text', text: block.text });
      continue;
    }
    if (block.type === 'image' && typeof block.data === 'string') {
      content.push({
        type: 'image',
        source: {
          type: 'base64',
          media_type: block.mimeType || block.mime_type || 'image/png',
          data: block.data,
        },
      });
    }
  }
  return content;
}

// humanAuthored marks a prompt a person typed, the way Claude Code marks one
// typed in its own interface (origin {kind:"human"}). Claude Code honours
// human-only features from that mark — the "ultracode" keyword that opts a
// prompt into Workflow orchestration among them — so without it a Workass chat
// could never use ultracode (user request 2026-10-13). Wake-ups and messages
// from other agents are never marked.
// A Workass wake-up notice (a tracked subagent finished) carries no origin:
// Claude Code 2.1.301 drops an SDK-supplied {kind:"task-notification"}
// (verified 2026-10-16; "human" is kept), and the no-lifecycle result routing
// below treats any non-human result origin as a turn Claude Code started
// itself. The <task-notification> body the daemon writes is what frames it.
function sdkUserMessage(sessionId, blocks, uuid = randomUUID(), priority, humanAuthored = false) {
  return {
    type: 'user',
    message: { role: 'user', content: promptContent(blocks) },
    parent_tool_use_id: null,
    session_id: sessionId,
    uuid,
    ...(priority ? { priority } : {}),
    ...(humanAuthored ? { origin: { kind: 'human' } } : {}),
  };
}

function stableMessageUUID(sessionId, operationId) {
  const value = String(operationId || '').trim();
  if (!value) return randomUUID();
  const bytes = Buffer.from(createHash('sha256').update(`${sessionId}\0${value}`).digest().subarray(0, 16));
  bytes[6] = (bytes[6] & 0x0f) | 0x50;
  bytes[8] = (bytes[8] & 0x3f) | 0x80;
  const hex = bytes.toString('hex');
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
}

function mcpServerMap(rawServers) {
  const result = {};
  for (const raw of Array.isArray(rawServers) ? rawServers : []) {
    if (!raw || typeof raw !== 'object' || typeof raw.name !== 'string' || !raw.name.trim()) continue;
    if (typeof raw.command !== 'string' || !raw.command.trim()) {
      throw new Error(`Claude MCP server ${raw.name} requires a stdio command`);
    }
    let env;
    if (Array.isArray(raw.env)) {
      env = Object.fromEntries(raw.env
        .filter((entry) => entry && typeof entry.name === 'string')
        .map((entry) => [entry.name, String(entry.value || '')]));
    } else if (raw.env && typeof raw.env === 'object') {
      env = Object.fromEntries(Object.entries(raw.env).map(([key, value]) => [key, String(value)]));
    }
    result[raw.name] = {
      command: raw.command,
      args: Array.isArray(raw.args) ? raw.args.map(String) : [],
      ...(env && Object.keys(env).length ? { env } : {}),
    };
  }
  return result;
}

function modelRows(models) {
  const rows = [];
  for (const raw of Array.isArray(models) ? models : []) {
    const value = String(raw?.value || raw?.model || raw?.id || '').trim();
    if (!value || rows.some((row) => row.value === value)) continue;
    const supportedEffortLevels = Array.isArray(raw?.supportedEffortLevels)
      ? raw.supportedEffortLevels.map(String).filter((effort) => ['low', 'medium', 'high', 'xhigh', 'max'].includes(effort))
      : [];
    rows.push({
      value,
      name: String(raw?.displayName || raw?.name || value),
      ...(raw?.description ? { description: String(raw.description) } : {}),
      ...(raw?.resolvedModel ? { resolvedModel: String(raw.resolvedModel) } : {}),
      supportedEffortLevels: raw?.supportsEffort === true ? supportedEffortLevels : [],
    });
  }
  const defaultRow = rows.find((row) => row.value === 'default' && row.resolvedModel);
  const explicitDefault = defaultRow && rows.find((row) => row.value !== 'default' && row.resolvedModel === defaultRow.resolvedModel);
  if (defaultRow && explicitDefault) defaultRow.name = explicitDefault.name;
  return rows;
}

const modeRows = [
  { value: 'default', name: 'Ask before changes' },
  { value: 'acceptEdits', name: 'Accept edits' },
  { value: 'plan', name: 'Plan' },
  { value: 'bypassPermissions', name: 'Full access' },
];
const effortRows = ['low', 'medium', 'high', 'xhigh', 'max'].map((value) => ({
  value,
  name: value === 'xhigh' ? 'Extra high' : value[0].toUpperCase() + value.slice(1),
}));

function effortRowsForModel(model) {
  const supported = new Set(Array.isArray(model?.supportedEffortLevels) ? model.supportedEffortLevels : []);
  return effortRows.filter((row) => supported.has(row.value));
}

// The provider-side truth about a session id, with named transitions. These
// were three loose booleans on the session whose interplay hid the
// 2026-07-27 first-turn-cancel wedge; the ledger makes each move explicit.
class ConversationLedger {
  constructor(resumeRequested) {
    // The session was opened over an existing provider conversation.
    this.resumeRequested = Boolean(resumeRequested);
    // Claude Code has echoed conversation traffic for this id (a user echo or
    // a clean result). The OAuth retry legitimately rolls this back.
    this.established = Boolean(resumeRequested);
    // This id's transcript exists on disk provider-side. Never rolls back:
    // once true, a fresh --session-id start with it is refused forever, so
    // every reopen from then on must resume.
    this.materialized = Boolean(resumeRequested);
  }

  resumeExisting() { return this.established || this.materialized || this.resumeRequested; }

  noteConversationSeen() {
    this.established = true;
    this.materialized = true;
  }

  noteMaterialized() { this.materialized = true; }

  snapshotEstablished() { return this.established; }
  restoreEstablished(value) { this.established = Boolean(value); }
}

// One retained direction per steer and one boundary owed per direction: each
// pushed steer costs exactly one extra terminal result from the SDK.
class SteerLedger {
  constructor() {
    this.pending = new Map();
    // FIFO of steer uuids awaiting their terminal-result boundary.
    this.boundaries = [];
  }

  retain(uuid, entry) {
    this.pending.set(uuid, entry);
    this.boundaries.push(uuid);
  }

  consumeBoundary() {
    const uuid = this.boundaries.shift();
    if (uuid === undefined) return null;
    const entry = this.pending.get(uuid) ?? {};
    this.pending.delete(uuid);
    return { uuid, clientUserMessageId: entry.clientUserMessageId || '' };
  }

  // Every steer still waiting for its boundary, in the order the user gave them.
  pendingMessages() {
    return this.boundaries
      .map((uuid) => this.pending.get(uuid)?.message)
      .filter(Boolean);
  }

  awaitingBoundary() { return this.boundaries.length > 0; }

  drop() {
    this.boundaries.length = 0;
    this.pending.clear();
  }
}

class ClaudeSession {
  constructor({ sessionId, cwd, mcpServers, resume }) {
    this.sessionId = sessionId;
    // The id the PROVIDER currently speaks under. Claude's fork family
    // (/clear, forkSession, conversation_reset) moves the conversation to a
    // new id while our ACP identity stays put; resuming the old id would
    // freeze context at the fork point (hostile-fixture finding, 2026-07-28).
    this.providerSessionId = sessionId;
	this.providerLineage = 1;
    this.cwd = cwd;
    this.mcpServers = mcpServers;
    this.conversation = new ConversationLedger(resume);
    this.input = new AsyncInputQueue();
    this.query = null;
    this.queryRetirement = Promise.resolve();
    this.generation = 0;
    this.models = [];
    // Claude commands surface: the clamped catalog captured from the newest
    // initializationResult(). null until the engine reports one (UNKNOWN).
    this.commandCatalog = null;
    // True while this session is answering its session/new|resume|load
    // request: those openQuery() runs hand the catalog back on the open reply,
    // so the _workass_claude_commands notify is reserved for mid-session
    // engine restarts, which produce no open reply at all.
    this.openReplyPending = true;
    this.currentModel = '';
    this.currentMode = 'default';
    this.currentEffort = 'high';
    this.activePrompt = null;
    // Stop-hook evidence for the turn currently ending, held until its terminal
    // result so the daemon receives one atomic turn-end record.
    this.stopEvidence = null;
    // True while a turn the harness started — not the user — is running.
    // Explicit cancel keys off it so it cannot address the previous turn.
    this.harnessTurnActive = false;
    this.steers = new SteerLedger();
    // Claude Code reports each pushed message's fate by its uuid
    // (command_lifecycle: queued -> started -> completed | cancelled). A
    // message typed while a turn runs is FOLDED into that turn at its next
    // boundary — a turn Claude Code started on its own included — so counting
    // results cannot tell whose turn ended. Once the CLI speaks the lifecycle,
    // it alone decides consumption and which turn a result closes; an older
    // CLI keeps the legacy priority-"now" boundary model below.
    this.lifecycleSeen = false;
    // uuid -> { state, kind: 'prompt' | 'steer', owner, clientUserMessageId, reported }
    this.commands = new Map();
    // prompt_id of the CLI turn last announced. Claude Code fires
    // UserPromptSubmit again for a message it folds into the running turn, with
    // that turn's prompt_id; a new turn gets a new one. Hooks can overtake the
    // message stream, so the turn's result cannot be what closes it here.
    this.turnPromptId = '';
    this.startedTurns = false;
    this.queryNeedsRestart = false;
    // Turn pulse: elapsed/tokens/phase while a turn is live, plus provider
    // retry state classified from Claude Code's stderr. A max-effort turn can
    // think silently for minutes and was indistinguishable from a dead chat
    // (2026-07-27); this is the liveness signal the UI reads.
    this.heartbeat = null;
    this.turnStartedAt = 0;
    this.turnTokensSettled = 0;
    this.turnTokensCurrent = 0;
    // What the last main-thread API call carried into the model: the context
    // window's fill. The result's usage is the whole turn summed across calls.
    this.contextTokens = 0;
    this.turnPhase = '';
    this.turnToolName = '';
    this.retryState = null;
    this.turnStatus = 'idle';
    this.closed = false;
    this.toolCalls = new Map();
    // A subagent's own tool calls (parent_tool_use_id set), kept apart from the
    // main turn's ledger: they are attributed to their Agent call, never end
    // with the main turn, and never veto its retries.
    this.subagentToolCalls = new Map();
    // Tool ids of questions the user actually answered — see completeToolResults.
    this.answeredQuestions = new Set();
    this.partialTextSeen = false;
    this.partialThoughtSeen = false;
    this.providerRealm = null;
  }

  async start() {
    const resumeExisting = this.conversation.resumeRequested;
    try {
      return await this.openQuery(resumeExisting);
    } catch (error) {
      if (!resumeExisting && isSessionInUseError(error)) {
        // session/new promised a fresh empty session, so adopting the disk
        // history behind the collided id would smuggle unknown context under
        // a canonical replay. Rotate to an id nothing has used.
        this.sessionId = randomUUID();
        this.providerSessionId = this.sessionId;
		this.providerLineage = 1;
        const previous = this.query;
        if (previous) this.retireQuery(previous, true);
        return this.openQuery(false);
      }
      throw error;
    }
  }

  modelRow() {
    return this.models.find((model) => model.value === this.currentModel);
  }

  queryOptions(resumeExisting) {
    const executable = String(process.env.WORKASS_CLAUDE_EXECUTABLE || '').trim();
    if (!executable) throw new Error('WORKASS_CLAUDE_EXECUTABLE is required');
    const servers = mcpServerMap(this.mcpServers);
    return {
      cwd: this.cwd,
      pathToClaudeCodeExecutable: executable,
      systemPrompt: { type: 'preset', preset: 'claude_code', ...(process.env.WORKASS_INSTRUCTIONS_FILE ? { append: readFileSync(process.env.WORKASS_INSTRUCTIONS_FILE, 'utf8') } : {}) },
      // A delegated child does not load the user's own instruction file. The
      // repo's does still load: the laws a child works under belong to the
      // repository, not to the person who happens to be driving.
      settingSources: process.env.WORKASS_SUBAGENT === '1'
        ? ['project', 'local']
        : ['user', 'project', 'local'],
      tools: { type: 'preset', preset: 'claude_code' },
      includePartialMessages: true,
      env: process.env,
      permissionMode: this.currentMode,
      // This only permits a later explicit bypassPermissions selection. The
      // current permissionMode still decides whether permissions are bypassed.
      allowDangerouslySkipPermissions: true,
      ...(this.currentModel ? { model: this.currentModel } : {}),
      ...(effortRowsForModel(this.modelRow()).some((row) => row.value === this.currentEffort)
        ? { effort: this.currentEffort }
        : {}),
      ...(Object.keys(servers).length ? { mcpServers: servers } : {}),
      ...(resumeExisting ? { resume: this.providerSessionId } : { sessionId: this.sessionId }),
      stderr: (data) => this.noteStderr(data),
      canUseTool: (toolName, input, options) => this.canUseTool(toolName, input, options),
      hooks: this.lifecycleHooks(),
    };
  }

  // The harness answers done-vs-parked itself. Stop carries the background work
  // still in flight and the wakes scheduled to re-invoke this session, and
  // UserPromptSubmit is the only birth signal a harness-started turn produces —
  // no SDKUserMessage is emitted for it, so without this hook that turn is
  // invisible and its output is discarded downstream.
  //
  // These callbacks are observers only — all but PreToolUse, the home-scan
  // guard. UserPromptSubmit blocks the prompt it fires for and times out at
  // 30s, so every callback does one synchronous stdout write, swallows its own
  // errors and returns {} immediately. They compose with the user's own
  // settings-file hooks, which keep working.
  lifecycleHooks() {
    const observe = (handler) => async (input) => {
      try {
        // A hook firing inside a subagent carries agent_id. That is the
        // subagent's turn, not this session's: acting on it would end the
        // session's turn on every Agent tool call.
        if (!input || input.agent_id) return {};
        handler(input);
      } catch (error) {
        diagnostic('Claude lifecycle hook failed', error);
      }
      return {};
    };
    return {
      // A hook, not canUseTool: bypassPermissions never asks canUseTool, but
      // runs every PreToolUse hook. No agent_id filter either — a subagent's
      // scan pops the same prompt.
      ...(process.platform === 'darwin'
        ? { PreToolUse: [{ matcher: 'Bash|Glob|Grep', hooks: [(input) => this.guardHomeScan(input)] }] }
        : {}),
      UserPromptSubmit: [{ hooks: [observe((input) => this.emitTurnStarted(input))] }],
      Stop: [{ hooks: [observe((input) => this.captureStopEvidence(input))] }],
      StopFailure: [{ hooks: [observe((input) => this.emitTurnPhase('failed', {
        error: clipTurnField(input.error_details || input.error),
      }, input))] }],
      Notification: [{ hooks: [observe((input) => this.emitTurnPhase('attention', {
        notificationType: clipTurnField(input.notification_type),
      }, input))] }],
    };
  }

  // A deny comes back as the call's is_error tool_result, so the row fails
  // with this reason like any other refused call, and the model reads it and
  // retargets. A guard that throws allows: it must never wedge a tool call.
  async guardHomeScan(input) {
    try {
      const reason = homeScanDenial(input?.tool_name, input?.tool_input, { cwd: input?.cwd || this.cwd, home: os.homedir() });
      if (reason) {
        return { hookSpecificOutput: { hookEventName: 'PreToolUse', permissionDecision: 'deny', permissionDecisionReason: reason } };
      }
    } catch (error) {
      diagnostic('Claude home-scan guard failed', error);
    }
    return {};
  }

  emitTurnPhase(phase, fields, input) {
    notify(this.sessionId, {
      sessionUpdate: '_workass_claude_turn',
      phase,
      promptId: clipTurnField(input?.prompt_id),
      ...fields,
    });
  }

  emitTurnStarted(input) {
    // Claude Code fires UserPromptSubmit again for a message it folds into the
    // running turn. That is the same turn: announcing a new one would end the
    // harness turn's bookkeeping, or adopt a phantom second turn.
    const promptId = clipTurnField(input?.prompt_id);
    if (promptId && promptId === this.turnPromptId) return;
    this.turnPromptId = promptId;
    this.stopEvidence = null;
    this.harnessTurnActive = this.activePrompt === null;
    if (this.harnessTurnActive) this.startTurnPulse();
    // activePrompt is assigned synchronously in enqueuePrompt BEFORE the message
    // is pushed, and openQuery runs against an empty input queue, so a human
    // turn always has it set by the time the SDK can fire this hook. A
    // misclassification can therefore only read harness-born work as human,
    // which costs the adoption and never invents a turn.
    this.emitTurnPhase('started', { humanAuthored: this.activePrompt !== null }, input);
  }

  // Stop fires just before the terminal result. Hold its evidence and ship it
  // with the result so the daemon gets one atomic turn-end record, ordered
  // ahead of the session/prompt reply on the same stdout stream.
  captureStopEvidence(input) {
    this.stopEvidence = {
      promptId: clipTurnField(input.prompt_id),
      // undefined is UNKNOWN (an older CLI without the field); [] is a proof of
      // quiet. They must never collapse together, so an absent list is omitted
      // from the payload rather than sent as empty.
      lists: {
        ...(Array.isArray(input.background_tasks)
          ? boundedTurnList('backgroundTasks', input.background_tasks, (task) => ({
            id: clipTurnField(task?.id),
            type: clipTurnField(task?.type),
            status: clipTurnField(task?.status),
          }))
          : {}),
        ...(Array.isArray(input.session_crons)
          ? boundedTurnList('sessionCrons', input.session_crons, (cron) => ({
            schedule: clipTurnField(cron?.schedule),
            recurring: cron?.recurring === true,
          }))
          : {}),
      },
    };
  }

  // Emitted once per user-visible turn end, from every result path that really
  // ends the turn. Free text from the harness — a task's command line or
  // description, a cron's prompt — is never copied into the payload at all, so
  // there is no field for a secret to travel in.
  emitTurnEnded(message) {
    const evidence = this.stopEvidence;
    this.stopEvidence = null;
    this.harnessTurnActive = false;
    if (!this.activePrompt) {
      this.stopTurnPulse();
      this.failOpenToolCalls('La herramienta murió con la consulta al proveedor antes de terminar.');
    }
    notify(this.sessionId, {
      sessionUpdate: '_workass_claude_turn',
      phase: 'ended',
      promptId: evidence?.promptId || '',
      ...(evidence?.lists || {}),
      terminalReason: clipTurnField(message?.terminal_reason),
      stopReason: clipTurnField(message?.stop_reason),
      originKind: clipTurnField(message?.origin?.kind),
      // False means no Stop hook ran for this turn — an older CLI, hooks
      // disabled by policy, or a turn that died before Stop. The daemon must
      // fall back to its prior behaviour rather than read absence as quiet.
      harnessEvidence: Boolean(evidence),
    });
  }

  async openQuery(resumeExisting) {
    // Wait for completed queries to release the provider-native session id.
    // Against SDK 0.3.217 close() returns undefined (the await is free); the
    // moment close() returns its cleanup promise this is what prevents
    // "Session ID ... is already in use" on reopen-after-retire. The flag
    // that used to gate this was forced on by the daemon since the first
    // promotion, so unconditional is what always ran.
    await this.queryRetirement;
    const sdk = await loadSDK();
    const generation = ++this.generation;
    const query = sdk.query({ prompt: this.input, options: this.queryOptions(resumeExisting) });
    this.query = query;
    this.queryNeedsRestart = false;
    void this.consume(query, generation);
    const initialized = await query.initializationResult();
    const accountIdentity = initialized?.account?.email || initialized?.account?.id || '';
    const executable = String(process.env.WORKASS_CLAUDE_EXECUTABLE || '').trim();
    this.providerRealm = {
      accountScope: opaqueRealmScope('account', accountIdentity) || 'unverified-account',
      installScope: opaqueRealmScope('install', executable),
      verified: Boolean(accountIdentity),
    };
    this.models = modelRows(initialized?.models);
    if (!this.currentModel && this.models.length) this.currentModel = this.models[0].value;
    if (initialized && typeof initialized === 'object') {
      this.commandCatalog = clampCommandCatalog(initialized);
    }
    // A mid-session engine restart (queryNeedsRestart path) never produces an
    // open reply, and the restarted CLI may have been upgraded on disk — so
    // re-announce the catalog. Replace is idempotent; receivers replace
    // wholesale. Open requests carry it on their reply instead.
    if (this.commandCatalog && !this.openReplyPending) {
      notify(this.sessionId, { sessionUpdate: '_workass_claude_commands', commandCatalog: this.commandCatalog });
    }
    return initialized;
  }

  configOptions() {
    const models = this.models.length ? this.models : [{ value: this.currentModel || 'default', name: 'Default' }];
    const modelOptions = models.map((model) => ({
      value: model.value, name: model.name, ...(model.description ? { description: model.description } : {}),
    }));
    const modelEfforts = effortRowsForModel(this.modelRow());
    return [
      { id: 'model', category: 'model', name: 'Model', type: 'select', currentValue: this.currentModel || models[0].value, options: modelOptions },
      { id: 'mode', category: 'mode', name: 'Mode', type: 'select', currentValue: this.currentMode, options: modeRows },
      ...(modelEfforts.length ? [{
        id: 'effort', category: 'thought_level', name: 'Effort', type: 'select',
        currentValue: modelEfforts.some((row) => row.value === this.currentEffort) ? this.currentEffort : modelEfforts[0].value,
        options: modelEfforts,
      }] : []),
    ];
  }

  availableModels() {
    return this.models.map((model) => ({ modelId: model.value, name: model.name, description: model.description || '' }));
  }

  // AskUserQuestion is not a permission — it is the model asking the user
  // something, and canUseTool is the only channel the SDK gives us for it. Left
  // generic it rendered as "run AskUserQuestion?" with allow/reject and no
  // question in sight, so no answer could ever be given (user 2026-07-25).
  // Each question is asked on its own, with ITS OWN options as the choices.
  async askUserQuestion(input, options = {}) {
    const questions = Array.isArray(input?.questions) ? input.questions.slice(0, 4) : [];
    const answers = [];
    for (const entry of questions) {
      const choices = (Array.isArray(entry?.options) ? entry.options : []).slice(0, 4)
        .filter((choice) => choice && String(choice.label || '').trim());
      if (!choices.length) continue;
      const header = String(entry?.header || '').trim() || 'Pregunta';
      const result = await peerRequest('session/request_permission', {
        sessionId: this.sessionId,
        toolCall: {
          toolCallId: options.toolUseID,
          title: header,
          kind: 'other',
          rawInput: {
            question: String(entry?.question || '').trim(),
            header,
            options: choices.map((choice) => ({
              label: String(choice.label).trim(),
              description: String(choice.description || '').trim(),
            })),
            multiSelect: entry?.multiSelect === true,
          },
        },
        // 'answer' is deliberately NOT an allow/reject kind: the renderer labels
        // known permission kinds ("Permitir una vez") and would otherwise
        // overwrite the choice text. Cancel keeps reject_once so the daemon's
        // timeout fallback still lands on "no answer".
        options: [
          ...choices.map((choice, index) => ({
            optionId: `answer-${index}`,
            name: String(choice.label).trim(),
            kind: 'answer',
          })),
          { optionId: 'question-cancel', name: 'Responder en el chat', kind: 'reject_once' },
        ],
      }, 0);
      const outcome = result?.outcome || {};
      if (outcome.outcome !== 'selected') return null;
      // The daemon answers this itself when the asker is a spawned subagent: its
      // question belongs to the parent, not to whoever is looking at the chat.
      if (String(outcome.optionId || '') === SUBAGENT_QUESTION_OPTION) return SUBAGENT_QUESTION_OPTION;
      const picked = /^answer-(\d+)$/.exec(String(outcome.optionId || ''));
      if (!picked) return null;
      const choice = choices[Number(picked[1])];
      if (!choice) return null;
      answers.push(`${header}: ${String(choice.label).trim()}`);
    }
    return answers.length ? answers.join(' · ') : null;
  }

  // A subagent has no user of its own: whatever it needs to know belongs to the
  // agent that spawned it, which holds the chat and the context to answer — or
  // to ask the user itself, in which case that question parks and waits. Parking
  // the SUBAGENT on a card instead would block a background lane on a human and
  // let several of them pile cards into one chat (user 2026-07-25).
  subagentQuestionHandback(input) {
    const questions = Array.isArray(input?.questions) ? input.questions.slice(0, 4) : [];
    const lines = questions.map((entry) => {
      const header = String(entry?.header || '').trim();
      const question = String(entry?.question || '').trim();
      if (!question) return '';
      const labels = (Array.isArray(entry?.options) ? entry.options : [])
        .map((choice) => String(choice?.label || '').trim()).filter(Boolean);
      return `${header ? `${header}: ` : ''}${question}${labels.length ? ` (${labels.join(' / ')})` : ''}`;
    }).filter(Boolean);
    return lines.length
      ? `Un subagente no puede preguntarle al usuario. Devolvé esta pregunta en tu resultado para que el agente principal la responda o se la pase al usuario — ${lines.join(' · ')}`
      : 'Un subagente no puede preguntarle al usuario: devolvé la pregunta en tu resultado para que la resuelva el agente principal.';
  }

  async canUseTool(toolName, input, options = {}) {
    if (toolName === 'AskUserQuestion') {
      // Two kinds of subagent ask this: a Task subagent inside this turn
      // (agentID), and a spawned subagent whose whole session is delegated (the
      // daemon answers with SUBAGENT_QUESTION_OPTION). Both hand the question up.
      if (options.agentID) {
        return { behavior: 'deny', message: this.subagentQuestionHandback(input), toolUseID: options.toolUseID };
      }
      const answered = await this.askUserQuestion(input, options);
      if (answered === SUBAGENT_QUESTION_OPTION) {
        return { behavior: 'deny', message: this.subagentQuestionHandback(input), toolUseID: options.toolUseID };
      }
      // PermissionResult is only allow{updatedInput} or deny{message} — there is
      // no way to hand the SDK a tool result — so the answer travels as the deny
      // message, which is what the model reads.
      if (answered) this.answeredQuestions.add(String(options.toolUseID || ''));
      return {
        behavior: 'deny',
        message: answered
          ? `El usuario respondió — ${answered}`
          : 'El usuario no eligió ninguna opción; preguntale en el chat y seguí con su respuesta.',
        toolUseID: options.toolUseID,
      };
    }
    const title = String(options.title || options.displayName || toolName || 'Claude Code action');
    const result = await peerRequest('session/request_permission', {
      sessionId: this.sessionId,
      toolCall: {
        toolCallId: options.toolUseID,
        title,
        kind: toolKind(toolName),
        rawInput: input,
        ...(options.description ? { description: String(options.description) } : {}),
      },
      options: [
        { optionId: 'allow_once', name: 'Allow once', kind: 'allow_once' },
        ...(Array.isArray(options.suggestions) && options.suggestions.length
          ? [{ optionId: 'allow_always', name: 'Always allow', kind: 'allow_always' }]
          : []),
        { optionId: 'reject_once', name: 'Reject', kind: 'reject_once' },
      ],
    }, 0);
    const outcome = result?.outcome || {};
    if (outcome.outcome !== 'selected') {
      return { behavior: 'deny', message: 'Permission was cancelled', toolUseID: options.toolUseID };
    }
    if (outcome.optionId === 'allow_once' || outcome.optionId === 'allow_always') {
      return {
        behavior: 'allow',
        updatedInput: input,
        toolUseID: options.toolUseID,
        ...(outcome.optionId === 'allow_always' && Array.isArray(options.suggestions)
          ? { updatedPermissions: options.suggestions }
          : {}),
      };
    }
    return { behavior: 'deny', message: 'Permission was denied', toolUseID: options.toolUseID };
  }

  async enqueuePrompt(blocks, clientUserMessageId = '', humanAuthored = false) {
	if (this.activePrompt) throw new Error('A Claude turn is already running');
	const uuid = stableMessageUUID(this.sessionId, clientUserMessageId);
    const message = sdkUserMessage(this.sessionId, blocks, uuid, undefined, humanAuthored);
    this.startedTurns = true;
    this.turnStatus = 'active';
    this.partialTextSeen = false;
    this.partialThoughtSeen = false;
    this.startTurnPulse();
    const response = new Promise((resolve, reject) => {
      this.activePrompt = {
        uuid,
        resolve,
        reject,
        message,
        accepted: false,
        authRefreshRetries: 0,
        bufferedUpdates: [],
        bufferedText: '',
        substantiveOutput: false,
        establishedBeforePrompt: this.conversation.snapshotEstablished(),
      inputSubmitted: false,
	  clientUserMessageId: String(clientUserMessageId || '').trim(),
        earlySteers: [],
      };
    });
    const active = this.activePrompt;
    try {
      // Reserve the turn before the first await. JSON-RPC requests are handled
      // concurrently so a steer can arrive in the same event-loop window as
      // session/prompt; exposing no active prompt here rejected a direction
      // that was ordered after the prompt on stdin.
      await this.ensureQuery();
      if (this.activePrompt !== active || this.closed) return response;
      this.trackCommand(uuid, 'prompt', active, active.clientUserMessageId);
      this.input.push(message);
      active.inputSubmitted = true;
      // The original prompt must enter the SDK queue first. Steers accepted
      // while ensureQuery was reopening the provider follow it in wire order.
      for (const earlySteer of active.earlySteers.splice(0)) this.input.push(earlySteer);
    } catch (error) {
      if (this.activePrompt === active) this.settlePrompt(null, error);
    }
    return response;
  }

  // The SDK does not redirect the sampling step in flight: it closes the current
  // assistant turn and answers the pushed message in the NEXT turn of the same
  // query. That follow-up output belongs to the prompt the user steered, so the
  // turn must stay open across the boundary — settling on the first result
  // orphaned Claude's actual answer after the job had already ended.
  steer(blocks, clientUserMessageId) {
    if (this.lifecycleSeen) return this.foldSteer(blocks, clientUserMessageId);
    if (!this.activePrompt) throw new Error('No active Claude turn accepts steering');
    const uuid = randomUUID();
    const message = sdkUserMessage(this.sessionId, blocks, uuid, 'now');
    // The message is retained, not just pushed: if this steer aborts the turn
    // before the model said anything, it is the message we have to ask again
    // (see redriveSteersAfterEmptyAbort).
    this.steers.retain(uuid, { clientUserMessageId: clientUserMessageId || '', message });
    if (this.activePrompt.inputSubmitted) this.input.push(message);
    else this.activePrompt.earlySteers.push(message);
    return { turnId: uuid, receipt: Boolean(clientUserMessageId) };
  }

  // Claude Code's own delivery for a message typed while it works: priority
  // "next", folded into the running turn at its next tool boundary, with the
  // tools in flight left alone ("now" aborted them and cut the turn). The
  // consumption receipt is the lifecycle's "started" for this exact uuid. A
  // turn Claude Code started itself accepts directions the same way.
  foldSteer(blocks, clientUserMessageId) {
    const owner = this.activePrompt;
    if (!owner && !this.harnessTurnActive) throw new Error('No active Claude turn accepts steering');
    const uuid = randomUUID();
    const message = sdkUserMessage(this.sessionId, blocks, uuid, 'next');
    this.trackCommand(uuid, 'steer', owner, clientUserMessageId);
    if (owner && !owner.inputSubmitted) owner.earlySteers.push(message);
    else this.input.push(message);
    return { turnId: uuid, receipt: Boolean(clientUserMessageId) };
  }

  trackCommand(uuid, kind, owner, clientUserMessageId) {
    this.commands.set(uuid, {
      state: 'pushed', kind, owner: owner || null,
      clientUserMessageId: String(clientUserMessageId || '').trim(), reported: false,
    });
  }

  observeCommandLifecycle(message) {
    this.lifecycleSeen = true;
    const uuid = String(message.command_uuid || '');
    const command = this.commands.get(uuid);
    if (!command) return;
    const state = String(message.state || '');
    command.state = state;
    if (state === 'started' && !command.reported) {
      command.reported = true;
      if (command.kind === 'prompt' && command.owner === this.activePrompt) {
        this.reportInputConsumed(command.owner);
        this.acceptActivePrompt(uuid);
      } else if (command.kind === 'steer' && command.clientUserMessageId) {
        notify(this.sessionId, { sessionUpdate: '_workass_claude_steer_consumed', clientUserMessageId: command.clientUserMessageId });
      }
    }
    // A folded command can report completed BEFORE the result of the turn it
    // joined: keep a prompt's commands until that prompt settles.
    if ((state === 'completed' || state === 'cancelled') && !command.owner) this.commands.delete(uuid);
  }

  reportInputConsumed(active) {
    if (!active || active.inputConsumedReported || !active.clientUserMessageId) return;
    active.inputConsumedReported = true;
    notify(this.sessionId, { sessionUpdate: '_workass_input_consumed', clientUserMessageId: active.clientUserMessageId });
  }

  // Claude Code has taken the active prompt (started, or already finished or
  // dropped it), so the turn that is ending is the prompt's. Still queued
  // means it runs after this other turn.
  activePromptInTurn() {
    const active = this.activePrompt;
    const state = active ? this.commands.get(active.uuid)?.state : undefined;
    return state === 'started' || state === 'completed' || state === 'cancelled';
  }

  // A direction that missed the ending turn's last boundary is still queued:
  // Claude Code runs it next, and the user-visible turn continues into it.
  activePromptHasQueuedSteer() {
    for (const command of this.commands.values()) {
      if (command.kind === 'steer' && command.owner === this.activePrompt
          && (command.state === 'pushed' || command.state === 'queued')) return true;
    }
    return false;
  }

  forgetPromptCommands(active) {
    for (const [uuid, command] of this.commands) {
      if (command.owner === active) this.commands.delete(uuid);
    }
  }

  // Each pushed steer costs exactly one extra terminal result: the first closes
  // the pre-steer segment, the next belongs to the steered turn. Report the
  // consumption receipt at that boundary — the SDK never echoes a user message
  // carrying our uuid, so the echo alone is not an observable boundary.
  consumeSteerBoundary() {
    const consumed = this.steers.consumeBoundary();
    if (!consumed) return false;
    if (consumed.clientUserMessageId) {
      notify(this.sessionId, { sessionUpdate: '_workass_claude_steer_consumed', clientUserMessageId: consumed.clientUserMessageId });
    }
    return true;
  }

  pendingSteerMessages() {
    return this.steers.pendingMessages();
  }

  dropSteerBoundaries() {
    this.steers.drop();
  }

  // A turn that ends with tool rows still open leaves them spinning on the
  // client forever, and the stale ledger entries veto the transient-OAuth
  // retry for the rest of the session (hostile-fixture findings, 2026-07-28).
  // Any tool still tracked at a turn boundary is dead by definition —
  // completed tools left the ledger via completeToolResults.
  failOpenToolCalls(reason, ledger = this.toolCalls) {
    for (const [id, tracked] of ledger) {
      notify(this.sessionId, {
        sessionUpdate: 'tool_call_update', toolCallId: id,
        title: toolTitle(tracked.name, tracked.input), kind: toolKind(tracked.name),
        status: 'failed', content: normalizeToolResultContent(reason),
        _meta: toolMeta(tracked),
      });
    }
    ledger.clear();
  }

  settlePrompt(result, error) {
    const active = this.activePrompt;
    if (!active) return;
    this.activePrompt = null;
    this.forgetPromptCommands(active);
    this.failOpenToolCalls('La herramienta murió con la consulta al proveedor antes de terminar.');
    if (!this.harnessTurnActive) this.stopTurnPulse();
    // Nothing can answer an outstanding direction once the turn is over; a
    // stale boundary would hold the NEXT turn open waiting for a second result.
    this.dropSteerBoundaries();
    if (error) {
      if (!isTransientOAuthRefreshFailure(error)) this.flushPromptUpdates(active);
      this.turnStatus = 'failed';
      active.reject(error);
      return;
    }
    this.flushPromptUpdates(active);
    const interrupted = isInterruptedResult(result);
    this.turnStatus = interrupted ? 'interrupted' : 'completed';
    active.resolve({
      stopReason: interrupted ? 'cancelled' : String(result?.stop_reason || 'end_turn'),
      ...(result?.usage ? { usage: result.usage } : {}),
      ...(result?.modelUsage ? { modelUsage: result.modelUsage } : {}),
    });
  }

  emitPromptUpdate(update) {
    const active = this.activePrompt;
    const text = update?.sessionUpdate === 'agent_message_chunk'
      ? String(update?.content?.text || '')
      : '';
    if (active && text && !active.substantiveOutput) {
      const combined = active.bufferedText + text;
      const normalized = combined.toLowerCase();
      if (transientOAuthRefreshError.toLowerCase().startsWith(normalized)
          || isTransientOAuthRefreshFailure(combined)) {
        active.bufferedText = combined;
        active.bufferedUpdates.push(update);
        return;
      }
      this.flushPromptUpdates(active);
      active.substantiveOutput = true;
    } else if (active && update?.sessionUpdate === 'agent_thought_chunk') {
      this.flushPromptUpdates(active);
      active.substantiveOutput = true;
    }
    notify(this.sessionId, update);
  }

  flushPromptUpdates(active = this.activePrompt) {
    if (!active) return;
    for (const update of active.bufferedUpdates.splice(0)) notify(this.sessionId, update);
    active.bufferedText = '';
  }

  // A max-effort turn can think silently for minutes; without a pulse the UI
  // cannot tell it from a dead chat. Additive update kind — consumers that
  // predate it ignore it.
  startTurnPulse() {
    this.turnStartedAt = Date.now();
    this.turnTokensSettled = 0;
    this.turnTokensCurrent = 0;
    this.turnPhase = 'waiting';
    this.turnToolName = '';
    this.retryState = null;
    if (this.heartbeat) return;
    this.heartbeat = setInterval(() => this.emitTurnPulse(), 2000);
    this.heartbeat.unref?.();
  }

  stopTurnPulse() {
    if (!this.heartbeat) return;
    clearInterval(this.heartbeat);
    this.heartbeat = null;
  }

  emitTurnPulse() {
    if (!this.activePrompt && !this.harnessTurnActive) return;
    notify(this.sessionId, {
      sessionUpdate: '_workass_claude_turn_heartbeat',
      elapsedMs: Math.max(0, Date.now() - this.turnStartedAt),
      outputTokens: this.turnTokensSettled + this.turnTokensCurrent,
      phase: this.turnPhase,
      ...(this.turnToolName ? { toolName: this.turnToolName } : {}),
      ...(this.retryState ? { retry: { code: this.retryState.code, attempt: this.retryState.attempt } } : {}),
    });
  }

  noteTurnPhase(phase, toolName = '') {
    this.turnPhase = phase;
    this.turnToolName = toolName;
    this.retryState = null;
  }

  // Claude Code prints transient API failures to stderr and retries silently;
  // surfacing them is the difference between "provider degraded" and the user
  // concluding the app is broken (2026-07-27). The pass-through keeps the
  // daemon's bounded stderr tails intact.
  noteStderr(data) {
    const text = String(data || '');
    process.stderr.write(text);
    const match = /API Error[^\n]*?\b([45]\d{2})\b[^\n]*retry/i.exec(text)
      || /retry[^\n]*API Error[^\n]*?\b([45]\d{2})\b/i.exec(text);
    if (match) {
      this.retryState = { code: Number(match[1]), attempt: (this.retryState?.attempt || 0) + 1 };
      this.emitTurnPulse();
    }
  }

  acceptActivePrompt(uuid = '') {
    const active = this.activePrompt;
    if (!active || active.accepted || (uuid && uuid !== active.uuid)) return;
    active.accepted = true;
  }

  async retryActivePromptAfterOAuthRefresh(query, active) {
    if (this.activePrompt !== active || active.substantiveOutput || active.authRefreshRetries >= 1 || this.toolCalls.size) {
      this.settlePrompt(null, new Error('Claude authentication refresh failed'));
      return;
    }
    active.authRefreshRetries += 1;
    // The turn restarts from the original message, so a direction pushed into
    // the abandoned attempt has no boundary left to wait for.
    this.dropSteerBoundaries();
    active.bufferedUpdates.length = 0;
    active.bufferedText = '';
    active.substantiveOutput = false;
    active.accepted = false;
    this.conversation.restoreEstablished(active.establishedBeforePrompt);
    this.partialTextSeen = false;
    this.partialThoughtSeen = false;
    this.retireQuery(query, true);
    process.stderr.write('Claude OAuth refresh retry: reopening the provider query once\n');
    try {
      await this.ensureQuery();
      if (this.activePrompt !== active || this.closed) return;
      this.trackCommand(active.uuid, 'prompt', active, active.clientUserMessageId);
      this.input.push(active.message);
    } catch (error) {
      this.settlePrompt(null, error);
    }
  }

  // The steer aborted the turn before the model committed a word, so Claude Code
  // ended the query on our own message and called it a failure. The user's
  // direction is the only thing at stake — ask it again instead of handing them
  // a dead turn to retype into. Nothing is discarded: the aborted segment
  // produced no content (that is precisely why the ending was invalid) and the
  // original prompt is already in the session transcript, so the reopened query
  // resumes with it and answers the direction. Same shape as the OAuth retry:
  // Claude Code's query has returned, so it is complete and reopened once.
  async redriveSteersAfterEmptyAbort(query, active) {
    const queued = this.pendingSteerMessages();
    if (!queued.length) {
      this.settlePrompt(null, new Error('Claude Code ended the turn on the steered message'));
      return;
    }
    // The aborted segment's own result: this error stands in for it, so the
    // first direction's receipt is owed to the client right now. Later steers
    // keep their boundaries — one per extra result the reopened query will emit.
    this.consumeSteerBoundary();
    active.bufferedUpdates.length = 0;
    active.bufferedText = '';
    active.substantiveOutput = false;
    this.partialTextSeen = false;
    this.partialThoughtSeen = false;
    this.retireQuery(query, true);
    process.stderr.write('Claude steer landed before any answer: reopening the query to ask it again\n');
    try {
      await this.ensureQuery();
      if (this.activePrompt !== active || this.closed) return;
      // Pushed as plain prompts, never priority "now": there is no live turn
      // left to interrupt, and re-arming the interrupt is what caused this.
      for (const message of queued) {
        const { priority, ...plain } = message;
        this.input.push(plain);
      }
    } catch (error) {
      this.settlePrompt(null, error);
    }
  }

  async consume(query, generation) {
    try {
      for await (const message of query) {
        if (this.closed || generation !== this.generation) return;
        this.handleMessage(message);
      }
      if (this.closed || generation !== this.generation) return;
      const error = new Error('Claude SDK stream closed before a terminal result');
      this.endOrphanedHarnessTurn();
      this.settlePrompt(null, error);
      this.retireQuery(query, false);
    } catch (error) {
      if (this.closed || generation !== this.generation) return;
      diagnostic('Claude SDK stream failed', error);
      this.endOrphanedHarnessTurn();
      this.settlePrompt(null, error);
      this.retireQuery(query, false);
    }
  }

  // A provider-started turn has no activePrompt for settlePrompt to fail. When
  // its stream dies it still needs its own end, or the daemon's adopted turn
  // runs forever and every later prompt queues behind it.
  endOrphanedHarnessTurn() {
    if (!this.harnessTurnActive || this.activePrompt) return;
    this.emitTurnEnded({ terminal_reason: 'stream_closed', stop_reason: 'stream_closed' });
  }

  handleMessage(message) {
    if (!message || typeof message !== 'object') return;
    // Any SDK traffic proves the provider stream is alive again.
    this.retryState = null;
    const announced = typeof message.session_id === 'string' ? message.session_id.trim() : '';
    if (announced && announced !== this.providerSessionId) {
	  const previousProviderSessionId = this.providerSessionId;
	  const lineageGeneration = ++this.providerLineage;
	  const lineageProof = createHash('sha256')
		.update(`${this.sessionId}\0${previousProviderSessionId}\0${announced}\0${lineageGeneration}`)
		.digest('hex');
      this.providerSessionId = announced;
	  notify(this.sessionId, {
		sessionUpdate: '_workass_claude_provider_session',
		previousProviderSessionId, providerSessionId: announced, lineageGeneration, lineageProof,
	  });
    }
    if (message.type === 'command_lifecycle') {
      this.observeCommandLifecycle(message);
      return;
    }
    if (message.session_id && (message.type === 'user'
        || (message.type === 'result' && message.is_error !== true))) {
      this.conversation.noteConversationSeen();
    }
    if (message.type === 'system' && message.subtype === 'status' && message.status === 'compacting') {
      // Auto-compaction pauses the turn for seconds; without a pulse the
      // client sees dead air exactly when patience is scarcest.
      this.noteTurnPhase('compacting');
      this.emitTurnPulse();
	  notify(this.sessionId, { sessionUpdate: '_workass_compaction', phase: 'started' });
    }
    if (message.type === 'system' && message.subtype === 'compact_boundary') {
      this.noteTurnPhase('waiting');
      this.emitTurnPulse();
	  const checkpointId = String(message.uuid || message.session_id || this.providerSessionId || this.sessionId || '').trim();
	  const digest = createHash('sha256').update(JSON.stringify({ checkpointId, compactMetadata: message.compact_metadata || null })).digest('hex');
	  notify(this.sessionId, { sessionUpdate: '_workass_compaction', phase: 'checkpoint', checkpointId, digest });
    }
    if (message.type === 'system' && message.subtype === 'commands_changed' && Array.isArray(message.commands)) {
      // Full-list REPLACE semantics (SDKCommandsChangedMessage): swap the
      // commands axis, restamp asOf, and hand receivers the WHOLE catalog so
      // they replace wholesale. Commands only — no change push exists for
      // agents or output styles in this SDK build.
      this.commandCatalog = {
        ...(this.commandCatalog || {}),
        ...clampCatalogCommands(message.commands),
        asOf: Date.now(),
      };
      notify(this.sessionId, { sessionUpdate: '_workass_claude_commands', commandCatalog: this.commandCatalog });
      return;
    }
    if (message.type === 'system' && message.subtype === 'local_command_output') {
      // A local command's output (/usage, /cost) is meant to render as
      // assistant text; without this forward a picked local command produces
      // an empty turn.
      const text = typeof message.content === 'string' ? message.content : '';
      if (text) {
        notify(this.sessionId, { sessionUpdate: 'agent_message_chunk', content: [{ type: 'text', text }] });
      }
      return;
    }
    if (message.type === 'user') {
      // The echoed user message does not carry the uuid we pushed, so it is not
      // the receipt boundary; the terminal result of the pre-steer segment is.
	  const echoedUUID = String(message.uuid || '');
	  const active = this.activePrompt;
	  if (active && echoedUUID === active.uuid) this.reportInputConsumed(active);
	  this.acceptActivePrompt(echoedUUID);
      this.completeToolResults(message);
      return;
    }
    // A subagent's messages carry parent_tool_use_id: the Agent call that
    // started it. Without it every subagent tool call read as the main
    // agent's own, so a labeling run filled the chat with 1,200 image reads
    // (DepthVision speed, 2026-10-15).
    const parentToolUseId = String(message.parent_tool_use_id || '').trim();
    if (message.type === 'stream_event') {
      this.handleStreamEvent(message.event, parentToolUseId);
      return;
    }
    if (message.type === 'assistant') {
      if (!parentToolUseId && message.message?.usage) this.contextTokens = contextTokens(message.message.usage);
      this.handleAssistantContent(message.message?.content, parentToolUseId);
      return;
    }
    if (message.type === 'result') {
      // D8: a result stamped with a non-human origin belongs to the turn the
      // harness started, not to a human prompt that may be queued alongside it.
      // Settling activePrompt or consuming a steer boundary here would end the
      // user's turn on somebody else's result. origin is authoritative;
      // harnessTurnActive covers a build that does not stamp it.
      const lifecycle = this.lifecycleSeen;
      const originKind = String(message.origin?.kind || '');
      // With the lifecycle report a result closes the prompt exactly when the
      // prompt was taken into this turn — folded into a turn Claude Code
      // started on its own included, whatever origin the result carries.
      // Otherwise it is that other turn's result and the prompt runs next.
      if (lifecycle
        ? !this.activePromptInTurn()
        : ((originKind && originKind !== 'human') || (!this.activePrompt && this.harnessTurnActive))) {
        this.emitTurnEnded(message);
        return;
      }
      if (isInterruptedResult(message)) {
        this.acceptActivePrompt();
        this.emitTurnEnded(message);
        this.settlePrompt(message, null);
        return;
      }
      if (message.is_error === true) {
        const error = new Error(claudeResultError(message));
        const active = this.activePrompt;
        // A steer that outran the model's first word: recoverable, and the turn
        // must survive it — the direction is re-asked, not lost with the job.
        if (!lifecycle && active && this.steers.awaitingBoundary() && isEmptyAbortResult(message)) {
          void this.redriveSteersAfterEmptyAbort(this.query, active);
          return;
        }
        if (active && !active.substantiveOutput && active.authRefreshRetries < 1 && !this.toolCalls.size
            && (isTransientOAuthRefreshFailure(error)
              || isTransientOAuthRefreshFailure(active.bufferedText)
              || active.bufferedUpdates.some((update) => isTransientOAuthRefreshFailure(update?.content?.text)))) {
          void this.retryActivePromptAfterOAuthRefresh(this.query, active);
          return;
        }
        if (active && isTransientOAuthRefreshFailure(error)) {
          active.bufferedUpdates.length = 0;
          active.bufferedText = '';
        }
        // Emitted only past the redrive and OAuth-retry early returns above:
        // those results do not end the user-visible turn, and announcing an
        // ending for a turn that continues is the phantom-state bug inverted.
        this.emitTurnEnded(message);
        this.settlePrompt(null, error);
        return;
      }
      this.acceptActivePrompt();
      // A steered turn is still the same user-visible turn: hand the receipt to
      // the client and keep streaming the answer to the direction into it.
      if (lifecycle ? this.activePromptHasQueuedSteer() : this.consumeSteerBoundary()) return;
      this.emitTurnEnded(message);
      // The result's usage sums every API call of the turn: cache reads are
      // counted once per tool round, so a long agentic turn reported 1.2M of a
      // 1.0M window (2026-10-19). The context is the last call's prompt.
      if (message.usage && this.contextTokens > 0) {
        notify(this.sessionId, {
          sessionUpdate: 'usage_update', used: this.contextTokens, size: maxContextWindow(message.modelUsage),
          _meta: { 'workass.claude/turnUsage': message.usage },
        });
      }
      this.settlePrompt(message, null);
      return;
    }
    // Claude Code reports its subagents and background shells as
    // {type: 'system', subtype: 'task_started' | ...}. Matching only a bare
    // type: 'task_started' meant not one lifecycle event ever reached Workass:
    // native subagents were invisible and background shells were found only
    // by their result text (user report 2026-10-13).
    const taskEvent = taskEventKind(message);
    if (taskEvent) {
      notify(this.sessionId, { sessionUpdate: '_workass_claude_spawned_work', event: spawnedWorkEvent(message, taskEvent) });
    }
    // Claude Code's own live list of background tasks. It is how Claude Code
    // knows a task is gone when no ended event comes (an interrupt, a stop);
    // Workass settles a background row missing from it (spawned_work.go).
    if (message.type === 'system' && message.subtype === 'background_tasks_changed' && Array.isArray(message.tasks)) {
      notify(this.sessionId, {
        sessionUpdate: '_workass_claude_spawned_work',
        event: { type: 'snapshot', tasks: message.tasks.map((task) => ({
          taskId: task.task_id, taskType: task.task_type, description: task.description,
          subagentType: task.subagent_type, parentTaskId: task.parent_task_id, backgrounded: true,
        })) },
      });
    }
  }

  handleStreamEvent(event, parentToolUseId = '') {
    if (!event || typeof event !== 'object') return;
    if (event.type === 'content_block_start' && event.content_block?.type === 'tool_use') {
      this.startTool(event.content_block, parentToolUseId);
      return;
    }
    // A subagent's text, thinking and token counts are not the main turn's.
    if (parentToolUseId) return;
    if (event.type === 'message_start') {
      // The next assistant message opens: fold the previous one's cumulative
      // count into the settled total so the pulse never counts twice.
      this.turnTokensSettled += this.turnTokensCurrent;
      this.turnTokensCurrent = 0;
      return;
    }
    if (event.type === 'message_delta') {
      const tokens = event.usage?.output_tokens;
      if (Number.isFinite(tokens)) this.turnTokensCurrent = tokens;
      return;
    }
    if (event.type !== 'content_block_delta') return;
    if (event.delta?.type === 'text_delta' && event.delta.text) {
      this.partialTextSeen = true;
      this.noteTurnPhase('writing');
      this.emitPromptUpdate({ sessionUpdate: 'agent_message_chunk', content: { type: 'text', text: event.delta.text } });
    }
    if (event.delta?.type === 'thinking_delta' && event.delta.thinking) {
      this.partialThoughtSeen = true;
      this.noteTurnPhase('thinking');
      this.emitPromptUpdate({ sessionUpdate: 'agent_thought_chunk', content: { type: 'text', text: event.delta.thinking } });
    }
  }

  handleAssistantContent(content, parentToolUseId = '') {
    for (const block of Array.isArray(content) ? content : []) {
      if (block?.type === 'tool_use') this.startTool(block, parentToolUseId);
      else if (parentToolUseId) continue;
      else if (block?.type === 'text' && block.text && !this.partialTextSeen) {
        this.emitPromptUpdate({ sessionUpdate: 'agent_message_chunk', content: { type: 'text', text: block.text } });
      } else if (block?.type === 'thinking' && block.thinking && !this.partialThoughtSeen) {
        this.emitPromptUpdate({ sessionUpdate: 'agent_thought_chunk', content: { type: 'text', text: block.thinking } });
      }
    }
  }

  startTool(block, parentToolUseId = '') {
    const id = String(block?.id || '').trim();
    if (!id) return;
    const tracked = this.toolCalls.get(id) || this.subagentToolCalls.get(id);
    if (tracked) {
      // A streamed call opens at content_block_start with an empty input; its
      // command, path and description only arrive with the assistant message.
      // Without this update every row read "Ejecutar un comando" with nothing
      // after it (user 2026-10-12).
      const input = toolInput(block.input);
      if (Object.keys(input).length && !Object.keys(tracked.input).length) {
        tracked.input = input;
        notify(this.sessionId, {
          sessionUpdate: 'tool_call_update', toolCallId: id, title: toolTitle(tracked.name, input),
          kind: toolKind(tracked.name), status: 'in_progress', rawInput: input, ...toolLocations(input),
          _meta: toolMeta(tracked),
        });
      }
      return;
    }
    const name = String(block.name || 'Tool');
    if (parentToolUseId) {
      // A subagent's call: shown under its Agent row, and none of the main
      // turn's phase, acceptance or retry state.
      const child = { name, input: toolInput(block.input), parent: parentToolUseId };
      this.subagentToolCalls.set(id, child);
      notify(this.sessionId, {
        sessionUpdate: 'tool_call', toolCallId: id, title: toolTitle(name, child.input),
        kind: toolKind(name), status: 'in_progress', rawInput: child.input, ...toolLocations(child.input),
        _meta: toolMeta(child),
      });
      return;
    }
    this.noteTurnPhase('tool', name);
    this.acceptActivePrompt();
    const active = this.activePrompt;
    if (active) {
      this.flushPromptUpdates(active);
      active.substantiveOutput = true;
    }
    const input = toolInput(block.input);
    this.toolCalls.set(id, { name, input });
    notify(this.sessionId, {
      sessionUpdate: 'tool_call', toolCallId: id, title: toolTitle(name, input),
      kind: toolKind(name), status: 'in_progress', rawInput: input, ...toolLocations(input),
      _meta: toolMeta({ name }),
    });
  }

  completeToolResults(message) {
    for (const block of Array.isArray(message?.message?.content) ? message.message.content : []) {
      if (block?.type !== 'tool_result') continue;
      const id = String(block.tool_use_id || '').trim();
      const ledger = this.toolCalls.has(id) ? this.toolCalls : this.subagentToolCalls;
      const tracked = ledger.get(id);
      if (!id || !tracked) continue;
      // An answer to AskUserQuestion has no tool-result channel in the SDK, so it
      // travels as a denial (canUseTool above) and comes back as is_error — the
      // row said "falló" on the very click that worked (user 2026-07-26). A
      // question the user answered completed; only an unanswered one did not.
      const answeredQuestion = this.answeredQuestions.delete(id);
      notify(this.sessionId, {
        sessionUpdate: 'tool_call_update', toolCallId: id, title: toolTitle(tracked.name, tracked.input),
        kind: toolKind(tracked.name), status: block.is_error && !answeredQuestion ? 'failed' : 'completed',
        content: normalizeToolResultContent(block.content),
        _meta: toolMeta(tracked),
      });
      ledger.delete(id);
    }
  }

  async setConfig(configId, value) {
    const normalized = String(value || '').trim();
    if (configId === 'model') {
      const selected = this.models.find((model) => model.value === normalized);
      if (!selected) throw new Error(`Unsupported Claude model: ${normalized}`);
      if (this.conversation.established) {
        await this.ensureQuery();
        await this.applyQueryControl(() => this.query.setModel(normalized));
      }
      this.currentModel = normalized;
      const supported = effortRowsForModel(selected);
      if (supported.length && !supported.some((row) => row.value === this.currentEffort)) {
        this.currentEffort = supported.find((row) => row.value === 'high')?.value || supported[0].value;
      }
      if (!this.conversation.established) this.invalidateQueryForOptions();
    } else if (configId === 'mode') {
      if (!modeRows.some((mode) => mode.value === normalized)) throw new Error(`Unsupported Claude mode: ${normalized}`);
      if (this.conversation.established) {
        await this.ensureQuery();
        await this.applyQueryControl(() => this.query.setPermissionMode(normalized));
      }
      this.currentMode = normalized;
      if (!this.conversation.established) this.invalidateQueryForOptions();
    } else if (configId === 'effort') {
      if (!effortRowsForModel(this.modelRow()).some((effort) => effort.value === normalized)) {
        throw new Error(`Unsupported Claude effort for ${this.currentModel || 'current model'}: ${normalized}`);
      }
      if (this.activePrompt) throw new Error('Cannot change Claude effort during an active turn');
      this.currentEffort = normalized;
      this.invalidateQueryForOptions();
    } else {
      throw new Error(`Unknown Claude config option: ${configId}`);
    }
    notify(this.sessionId, { sessionUpdate: 'config_options_update', configOptions: this.configOptions() });
    return { configOptions: this.configOptions() };
  }

  async applyQueryControl(apply) {
    try {
      await apply();
    } catch (error) {
      if (!isRecoverableQueryTransportError(error)) throw error;
      this.invalidateQueryForOptions();
    }
  }

  invalidateQueryForOptions() {
    const previous = this.query;
    if (previous) this.retireQuery(previous, true);
  }

  retireQuery(query, closeQuery) {
    if (!query || this.query !== query) return;
    ++this.generation;
    const previousInput = this.input;
    this.query = null;
    this.queryNeedsRestart = true;
    this.failOpenToolCalls('La consulta al proveedor terminó con esta herramienta del subagente todavía corriendo.', this.subagentToolCalls);
    this.input = new AsyncInputQueue();
    previousInput.close();
    if (closeQuery && query.close) {
      let closeResult;
      try {
        closeResult = query.close();
      } catch (error) {
        closeResult = Promise.reject(error);
      }
      // Settled before chaining: a rejecting close() must neither poison every
      // future reopen through queryRetirement nor kill the host as an
      // unhandled rejection — the host has no process-level handler.
      const settled = Promise.resolve(closeResult)
        .catch((error) => diagnostic('Claude query close failed', error));
      this.queryRetirement = this.queryRetirement.then(() => settled);
    }
  }

  async ensureQuery() {
    if (this.query && !this.queryNeedsRestart) return;
    const resumeExisting = this.conversation.resumeExisting();
    try {
      await this.openQuery(resumeExisting);
    } catch (error) {
      if (resumeExisting || !isSessionInUseError(error)) throw error;
      // The refusal is the receipt that this id's transcript exists — the
      // first-turn-cancel wedge: the query died before any message proved the
      // conversation, but Claude Code had already persisted the id. A fresh
      // start can never succeed again; resuming is the only live path.
      this.conversation.noteMaterialized();
      const failed = this.query;
      if (failed) this.retireQuery(failed, true);
      await this.openQuery(true);
    }
  }

  async usage() {
    if (typeof this.query?.usage_EXPERIMENTAL_MAY_CHANGE_DO_NOT_RELY_ON_THIS_API_YET === 'function') {
      return this.query.usage_EXPERIMENTAL_MAY_CHANGE_DO_NOT_RELY_ON_THIS_API_YET();
    }
    if (typeof this.query?.getContextUsage === 'function') return this.query.getContextUsage();
    return {};
  }

  async interrupt() {
    // D2: a harness-born turn has no activePrompt, so gating on it alone made
    // session/cancel a silent no-op on exactly the turns the user is most
    // likely to want stopped — the stop-button-no-op incident reborn.
    if (this.query && !this.activePrompt && this.harnessTurnActive) {
      try {
        await Promise.resolve(this.query.interrupt());
      } catch (error) {
        diagnostic('Claude harness-turn interrupt failed', error);
      }
      return;
    }
    if (!this.query || !this.activePrompt) return;
    const query = this.query;
    // Stop belongs to the prompt that was running when it was asked. If that
    // prompt ends on its own while the interrupt is in flight, the next queued
    // prompt may already own the query; it must not be cancelled in its place.
    const target = this.activePrompt;
    let timer;
    const timeout = new Promise((resolve) => {
      timer = setTimeout(() => resolve({ timedOut: true }), 500);
      timer.unref?.();
    });
    let outcome;
    try {
      outcome = await Promise.race([
        Promise.resolve(query.interrupt()).then(() => ({ acknowledged: true }), (error) => ({ error })),
        timeout,
      ]);
    } finally {
      clearTimeout(timer);
    }
    if (this.activePrompt !== target || this.query !== query) return;
    this.settlePrompt({ stop_reason: 'cancelled', subtype: 'error_during_execution', is_error: false, errors: ['interrupted by user'] }, null);
    if (outcome?.error) diagnostic('Claude interrupt failed', outcome.error);
    // If interrupt acknowledgement did not itself emit a terminal result, the
    // old query cannot safely own the next queued turn.
    this.retireQuery(query, true);
  }

  close() {
    this.closed = true;
    this.stopTurnPulse();
    this.failOpenToolCalls('La sesión Claude se cerró con esta herramienta todavía corriendo.');
    this.failOpenToolCalls('La sesión Claude se cerró con esta herramienta todavía corriendo.', this.subagentToolCalls);
    ++this.generation;
    this.input.close();
    if (this.query?.close) this.query.close();
    this.settlePrompt(null, new Error('Claude session closed'));
  }
}

function numeric(value) {
  return typeof value === 'number' && Number.isFinite(value) ? Math.max(0, value) : 0;
}

function contextTokens(usage) {
  return numeric(usage.input_tokens) + numeric(usage.cache_creation_input_tokens)
    + numeric(usage.cache_read_input_tokens) + numeric(usage.output_tokens);
}

function maxContextWindow(modelUsage) {
  let value = 0;
  for (const usage of Object.values(modelUsage && typeof modelUsage === 'object' ? modelUsage : {})) {
    value = Math.max(value, numeric(usage?.contextWindow));
  }
  return value;
}

function isInterruptedResult(result) {
  const errors = Array.isArray(result?.errors) ? result.errors.join(' ').toLowerCase() : '';
  return result?.stop_reason === 'cancelled' || errors.includes('interrupt') || errors.includes('aborted');
}

// A steer is delivered with priority "now", which makes Claude Code abort the
// live query on the spot. When it lands before the model has committed anything
// — the user redirected within the first seconds of a turn — the conversation
// ends on the user message WE pushed, with no stop reason. Claude Code judges
// that ending invalid and reports a failed turn carrying its own diagnostic:
//   [ede_diagnostic] result_type=user last_content_type=n/a stop_reason=null
// Nothing was lost but the direction the user just gave, so failing the turn is
// the one wrong answer: the job died, the steer went with it, and the user had
// to retype the message to get any answer at all (user report 2026-07-27).
function isEmptyAbortResult(result) {
  const errors = Array.isArray(result?.errors) ? result.errors.join(' ') : '';
  return /\bresult_type=user\b/.test(errors) && /\bstop_reason=null\b/.test(errors);
}

function normalizeToolResultContent(content) {
  if (typeof content === 'string') return { type: 'text', text: content };
  if (!Array.isArray(content)) return { type: 'text', text: '' };
  return content.map((block) => {
    if (block?.type === 'image' && block.source?.type === 'base64') {
      return { type: 'image', mimeType: block.source.media_type, data: block.source.data };
    }
    return block;
  });
}

// Every tool update names its tool; a subagent's also names the Agent call
// that ran it, which is how the daemon files it under that subagent.
function toolMeta(tracked) {
  const claudeCode = { toolName: tracked.name };
  if (tracked.parent) claudeCode.parentToolUseId = tracked.parent;
  return { claudeCode };
}

function toolKind(name) {
  const lower = String(name || '').toLowerCase();
  if (/bash|shell|command|terminal/.test(lower)) return 'execute';
  if (/edit|write|patch|replace|create|delete/.test(lower)) return 'edit';
  if (/read|grep|glob|search|view/.test(lower)) return 'read';
  return 'other';
}

function toolInput(input) {
  return input && typeof input === 'object' && !Array.isArray(input) ? input : {};
}

// The file a call works on, as ACP locations, so the row can show its path.
function toolLocations(input) {
  const target = input.file_path || input.notebook_path || input.path;
  return typeof target === 'string' && target.trim() ? { locations: [{ path: target.trim() }] } : {};
}

function toolTitle(name, input) {
  const candidate = input?.description || input?.path || input?.file_path || input?.command;
  return String(candidate || name || 'Claude Code tool').slice(0, 240);
}

// Home-scan guard (macOS). A process attributed to Workass that reads inside
// another app's container makes macOS ask "Workass would like to access data
// from other apps" (TCC kTCCServiceSystemPolicyAppData), and Allow never
// persists for that service: the next scan asks again seconds later
// (2026-10-21). An agent's `find ~`, rg or Glob over the home folder walks
// straight into ~/Library/Containers and ~/Library/Group Containers.
//
// Refused: a recursive walk rooted at a path that CONTAINS those two folders —
// /, /Users, ~, ~/Library, or either container folder itself. Specific
// subfolders and non-recursive commands pass, and so does anything this cannot
// read with confidence: it catches the common broad scans, it is not a shell
// parser. Returns the reason to hand the agent, or '' to allow.
export function homeScanDenial(toolName, toolInput, { cwd = '', home = '' } = {}) {
  if (typeof home !== 'string' || !path.isAbsolute(home)) return '';
  const resolvedHome = path.resolve(home);
  const input = toolInput && typeof toolInput === 'object' ? toolInput : {};
  const start = typeof cwd === 'string' && path.isAbsolute(cwd) ? path.resolve(cwd) : null;
  let root = '';
  if (toolName === 'Bash') root = bashScanRoot(input.command, start, resolvedHome, 0);
  else if (toolName === 'Glob') root = globToolScanRoot(input, start, resolvedHome);
  else if (toolName === 'Grep') root = scanRootHit({ path: toolPath(input.path, start, resolvedHome) ?? start }, null, resolvedHome);
  if (!root) return '';
  return `Search a specific folder instead — scanning ${displayScanRoot(root, resolvedHome)} recursively walks into other apps' data under ~/Library, which makes macOS show the user a privacy prompt. Target the project or an exact subfolder (e.g. ~/Workspace/<repo>, ~/Library/Logs/<app>).`;
}

function displayScanRoot(root, home) {
  const lower = root.toLowerCase();
  if (lower === home.toLowerCase()) return '~';
  if (lower.startsWith(`${home.toLowerCase()}/`)) return `~${root.slice(home.length)}`;
  return root;
}

// The root a walk starts from, against each protected folder: a hit when every
// segment of the root names (or, unquoted, globs) the protected path's segment
// at that depth. A depth limit that stops short of reading the container
// folder lets a shallow look (`find ~ -maxdepth 2`) through. APFS is
// case-insensitive by default, so ~/library is ~/Library.
function scanRootHit(target, maxDepth, home) {
  if (!target?.path) return '';
  const segments = target.path.split('/').filter(Boolean);
  const homeSegments = home.split('/').filter(Boolean);
  for (const tail of [['Library', 'Containers'], ['Library', 'Group Containers']]) {
    const protectedSegments = [...homeSegments, ...tail];
    if (segments.length > protectedSegments.length) continue;
    if (!segments.every((segment, index) => scanSegmentMatches(segment, protectedSegments[index], target.glob))) continue;
    if (maxDepth !== null && maxDepth <= protectedSegments.length - segments.length) continue;
    return target.path;
  }
  return '';
}

function scanSegmentMatches(pattern, literal, glob) {
  if (!glob || !/[*?[]/.test(pattern)) return pattern.toLowerCase() === literal.toLowerCase();
  let source = '';
  for (let index = 0; index < pattern.length; index++) {
    const char = pattern[index];
    if (char === '*') source += '.*';
    else if (char === '?') source += '.';
    else if (char === '[' && pattern.indexOf(']', index + 2) > 0) {
      const close = pattern.indexOf(']', index + 2);
      const body = pattern.slice(index + 1, close).replace(/^!/, '^').replace(/\\/g, '\\\\');
      source += `[${body}]`;
      index = close;
    } else source += char.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
  }
  try {
    return new RegExp(`^${source}$`, 'i').test(literal);
  } catch {
    return false;
  }
}

function expandToolTilde(text, home) {
  return text === '~' || text.startsWith('~/') ? home + text.slice(1) : text;
}

// Claude Code's file tools take ~ but no shell variables.
function toolPath(value, cwd, home) {
  if (typeof value !== 'string' || !value.trim()) return null;
  const expanded = expandToolTilde(value.trim(), home);
  if (path.isAbsolute(expanded)) return path.resolve(expanded);
  return cwd ? path.resolve(cwd, expanded) : null;
}

// Glob lists every file under its base before it filters, so the base is what
// counts. A pattern's literal leading folders narrow that base (`~/Library/**`
// is rooted at ~/Library; `Workspace/**` from ~ at ~/Workspace).
function globToolScanRoot(input, cwd, home) {
  const pattern = typeof input.pattern === 'string' ? expandToolTilde(input.pattern.trim(), home) : '';
  let base = path.isAbsolute(pattern) ? '/' : (toolPath(input.path, cwd, home) ?? cwd);
  if (!base) return '';
  for (const segment of pattern.split('/').filter(Boolean)) {
    if (/[*?[{]/.test(segment)) break;
    base = path.join(base, segment);
  }
  return scanRootHit({ path: path.resolve(base) }, null, home);
}

// Words of one shell command line, split into simple commands. Quotes,
// escapes, $HOME/${HOME}, comments, redirections and heredoc bodies are
// honoured; $(…) and `…` bodies are returned for a separate pass and make the
// word that holds them unresolvable. null means the line could not be read
// (an unterminated quote, say) — and an unread line is allowed.
function shellCommands(source, home) {
  const commands = [];
  const substitutions = [];
  const heredocs = [];
  const length = source.length;
  let words = [];
  let piped = false;
  let word = null;
  let redirectTarget = false;
  let index = 0;
  const variable = (name) => (name === 'HOME' ? home : name === 'USER' || name === 'LOGNAME' ? path.basename(home) : null);
  const endWord = () => {
    if (!word) return;
    if (redirectTarget) redirectTarget = false;
    else words.push(word);
    word = null;
  };
  const endCommand = (nextPiped) => {
    endWord();
    if (words.length) commands.push({ words, piped });
    words = [];
    piped = nextPiped;
    redirectTarget = false;
  };
  const closingParen = (open) => {
    let depth = 0;
    for (let at = open; at < length; at++) {
      const char = source[at];
      if (char === '\\') at++;
      else if (char === "'") {
        at = source.indexOf("'", at + 1);
        if (at < 0) return -1;
      } else if (char === '"') {
        at++;
        while (at < length && source[at] !== '"') at += source[at] === '\\' ? 2 : 1;
        if (at >= length) return -1;
      } else if (char === '(') depth++;
      else if (char === ')' && --depth === 0) return at;
    }
    return -1;
  };
  // Returns the index after the expansion, or -1 when it cannot be read.
  const expand = (at) => {
    if (source[at] === '`') {
      const close = source.indexOf('`', at + 1);
      if (close < 0) return -1;
      substitutions.push(source.slice(at + 1, close));
      word.unresolvable = true;
      return close + 1;
    }
    const next = source[at + 1];
    if (next === '(') {
      const close = closingParen(at + 1);
      if (close < 0) return -1;
      if (source[at + 2] !== '(') substitutions.push(source.slice(at + 2, close));
      word.unresolvable = true;
      return close + 1;
    }
    if (next === '{') {
      const close = source.indexOf('}', at + 2);
      if (close < 0) return -1;
      const value = variable(source.slice(at + 2, close));
      if (value === null) word.unresolvable = true;
      else word.text += value;
      return close + 1;
    }
    const name = /^[A-Za-z_][A-Za-z0-9_]*/.exec(source.slice(at + 1))?.[0];
    if (name) {
      const value = variable(name);
      if (value === null) word.unresolvable = true;
      else word.text += value;
      return at + 1 + name.length;
    }
    if (next && /[0-9?$!#@*-]/.test(next)) {
      word.unresolvable = true;
      return at + 2;
    }
    word.text += '$';
    return at + 1;
  };
  while (index < length) {
    const char = source[index];
    if (char === '\n') {
      endCommand(false);
      index++;
      while (heredocs.length) {
        const { delimiter, stripTabs } = heredocs.shift();
        let found = false;
        while (index < length && !found) {
          const end = source.indexOf('\n', index) < 0 ? length : source.indexOf('\n', index);
          const line = source.slice(index, end);
          found = (stripTabs ? line.replace(/^\t+/, '') : line) === delimiter;
          index = end + 1;
        }
      }
      continue;
    }
    if (char === ' ' || char === '\t' || char === '\r') { endWord(); index++; continue; }
    if (char === '#' && !word) {
      const end = source.indexOf('\n', index);
      index = end < 0 ? length : end;
      continue;
    }
    if (char === ';' || char === '(' || char === ')') { endCommand(false); index++; continue; }
    if (char === '|') {
      if (source[index + 1] === '|') { endCommand(false); index += 2; continue; }
      endCommand(true);
      index += source[index + 1] === '&' ? 2 : 1;
      continue;
    }
    if (char === '&') {
      if (source[index + 1] === '&') { endCommand(false); index += 2; continue; }
      if (source[index + 1] === '>') {
        endWord();
        redirectTarget = true;
        index += source[index + 2] === '>' ? 3 : 2;
        continue;
      }
      endCommand(false);
      index++;
      continue;
    }
    if (char === '>' || char === '<') {
      // A bare fd number before the operator (2>) belongs to the redirection.
      if (word && !word.quoted && !word.unresolvable && /^\d+$/.test(word.text)) word = null;
      else endWord();
      if (char === '<' && source[index + 1] === '<' && source[index + 2] !== '<') {
        index += 2;
        const stripTabs = source[index] === '-';
        if (stripTabs) index++;
        while (source[index] === ' ' || source[index] === '\t') index++;
        let delimiter = '';
        while (index < length && !/[\s;&|<>()]/.test(source[index])) {
          const quote = source[index];
          if (quote === "'" || quote === '"') {
            const close = source.indexOf(quote, index + 1);
            if (close < 0) return null;
            delimiter += source.slice(index + 1, close);
            index = close + 1;
          } else if (quote === '\\') {
            delimiter += source[index + 1] || '';
            index += 2;
          } else {
            delimiter += quote;
            index++;
          }
        }
        if (!delimiter) return null;
        heredocs.push({ delimiter, stripTabs });
        continue;
      }
      index++;
      while (source[index] === '>' || source[index] === '<' || source[index] === '|' || source[index] === '&') index++;
      redirectTarget = true;
      continue;
    }
    if (!word) word = { text: '', tilde: false, glob: false, quoted: false, unresolvable: false };
    if (char === "'") {
      const close = source.indexOf("'", index + 1);
      if (close < 0) return null;
      word.text += source.slice(index + 1, close);
      word.quoted = true;
      index = close + 1;
      continue;
    }
    if (char === '"') {
      word.quoted = true;
      index++;
      while (index < length && source[index] !== '"') {
        const inner = source[index];
        if (inner === '\\' && '"\\$`\n'.includes(source[index + 1] ?? '')) {
          word.text += source[index + 1];
          index += 2;
        } else if (inner === '$' || inner === '`') {
          index = expand(index);
          if (index < 0) return null;
        } else {
          word.text += inner;
          index++;
        }
      }
      if (index >= length) return null;
      index++;
      continue;
    }
    if (char === '\\') {
      word.text += source[index + 1] ?? '';
      word.quoted = true;
      index += 2;
      continue;
    }
    if (char === '$' || char === '`') {
      index = expand(index);
      if (index < 0) return null;
      continue;
    }
    if (char === '~' && !word.text && !word.quoted) word.tilde = true;
    if (char === '*' || char === '?' || char === '[') word.glob = true;
    word.text += char;
    index++;
  }
  endCommand(false);
  return { commands, substitutions };
}

function resolveShellPath(word, cwd, home) {
  if (!word || word.unresolvable || !word.text) return null;
  let text = word.text;
  if (word.tilde) {
    const tilde = /^~([^/]*)(.*)$/.exec(text);
    if (tilde && !tilde[1]) text = home + tilde[2];
    else if (tilde && /^[A-Za-z0-9._-]+$/.test(tilde[1])) text = path.join(path.dirname(home), tilde[1]) + tilde[2];
  }
  if (path.isAbsolute(text)) return { path: path.resolve(text), glob: word.glob };
  return cwd ? { path: path.resolve(cwd, text), glob: word.glob } : null;
}

// Prefixes that run the next word as the command, with their value-taking
// short options. `command -v find` only looks a name up.
const SCAN_COMMAND_WRAPPERS = { sudo: 'ugCDhprtU', env: 'uCS', nice: 'n', timeout: 'sk', command: '', builtin: '', exec: 'a', nohup: '', time: 'fo', caffeinate: 'tw' };
const SCAN_SHELL_KEYWORDS = new Set(['{', '}', '!', 'if', 'then', 'elif', 'else', 'do', 'while', 'until']);

function unwrapScanCommand(words) {
  let rest = words;
  for (let guard = 0; guard < 8 && rest.length; guard++) {
    while (rest.length && ((!rest[0].quoted && SCAN_SHELL_KEYWORDS.has(rest[0].text))
      || /^[A-Za-z_][A-Za-z0-9_]*=/.test(rest[0].text))) rest = rest.slice(1);
    const name = rest.length ? path.basename(rest[0].text) : '';
    const valueFlags = SCAN_COMMAND_WRAPPERS[name];
    if (valueFlags === undefined) return rest;
    rest = rest.slice(1);
    while (rest.length && rest[0].text.startsWith('-') && rest[0].text !== '-') {
      const flag = rest[0].text;
      rest = rest.slice(1);
      if (flag === '--') break;
      if (name === 'command' && /[vV]/.test(flag)) return [];
      if (flag.length === 2 && valueFlags.includes(flag[1])) rest = rest.slice(1);
    }
    if (name === 'timeout') rest = rest.slice(1);
  }
  return rest;
}

// Short and long option parsing shared by the walkers below. Values are kept
// as words so a value that is a path (fd --search-path ~) still resolves.
function parseScanOptions(args, { valueShort = '', valueLong = [], stopShort = '', stopLong = [] } = {}) {
  const shorts = new Map();
  const longs = new Map();
  const positionals = [];
  let options = true;
  for (let index = 0; index < args.length; index++) {
    const word = args[index];
    const text = word.text;
    if (!options || text === '-' || !text.startsWith('-')) {
      positionals.push(word);
      continue;
    }
    if (text === '--') {
      options = false;
      continue;
    }
    if (text.startsWith('--')) {
      const equals = text.indexOf('=');
      const name = equals < 0 ? text.slice(2) : text.slice(2, equals);
      if (stopLong.includes(name)) break;
      if (equals >= 0) longs.set(name, { ...word, text: text.slice(equals + 1), tilde: false });
      else longs.set(name, valueLong.includes(name) ? args[++index] : true);
      continue;
    }
    let stopped = false;
    for (let at = 1; at < text.length; at++) {
      const flag = text[at];
      if (stopShort.includes(flag)) {
        stopped = true;
        break;
      }
      if (valueShort.includes(flag)) {
        shorts.set(flag, at + 1 < text.length ? { ...word, text: text.slice(at + 1), tilde: false } : args[++index]);
        break;
      }
      shorts.set(flag, true);
    }
    if (stopped) break;
  }
  return { shorts, longs, positionals };
}

function scanDepth(value) {
  const depth = Number.parseInt(String(typeof value === 'object' && value ? value.text : ''), 10);
  return Number.isFinite(depth) && depth >= 0 ? depth : null;
}

// Each walker maps its arguments to { paths, useCwd, maxDepth, base? }, or null
// when the invocation does not walk. useCwd: with no path it walks the cwd —
// except a searcher fed by a pipe, which reads that instead.
const SCAN_WALKERS = {
  find(args) {
    let index = 0;
    const paths = [];
    while (index < args.length && /^-[HLPEXdsx]+$/.test(args[index].text)) index++;
    for (; index < args.length; index++) {
      const text = args[index].text;
      if (text === '-f' && args[index + 1]) paths.push(args[++index]);
      else if (text.startsWith('-') || text === '(' || text === '!') break;
      else paths.push(args[index]);
    }
    const expression = args.slice(index);
    // A walk that prunes the very folders in question stays out of them.
    if (expression.some((word) => word.text === '-prune')
      && expression.some((word) => /library|containers|users/i.test(word.text))) return null;
    const limit = expression.findIndex((word) => word.text === '-maxdepth');
    return { paths, useCwd: true, maxDepth: limit >= 0 ? scanDepth(expression[limit + 1]) : null };
  },
  rg(args, { piped }) {
    const { shorts, longs, positionals } = parseScanOptions(args, {
      valueShort: 'ABCEMTdefgjmrt',
      valueLong: ['after-context', 'before-context', 'context', 'encoding', 'max-columns', 'type-not', 'max-depth', 'maxdepth',
        'regexp', 'file', 'glob', 'iglob', 'threads', 'max-count', 'replace', 'type', 'type-add', 'type-clear', 'colors', 'color',
        'sort', 'sortr', 'max-filesize', 'path-separator', 'pre', 'pre-glob', 'context-separator', 'field-match-separator',
        'field-context-separator', 'engine', 'dfa-size-limit', 'regex-size-limit', 'ignore-file', 'hostname-bin', 'hyperlink-format'],
    });
    if (['help', 'version', 'type-list', 'generate'].some((name) => longs.has(name)) || shorts.has('h') || shorts.has('V')) return null;
    const patternGiven = longs.has('files') || shorts.has('e') || shorts.has('f') || longs.has('regexp') || longs.has('file');
    return {
      paths: patternGiven ? positionals : positionals.slice(1),
      useCwd: !piped,
      maxDepth: scanDepth(shorts.get('d') ?? longs.get('max-depth') ?? longs.get('maxdepth')),
    };
  },
  grep(args, { piped }) {
    const { shorts, longs, positionals } = parseScanOptions(args, {
      valueShort: 'ABCDdefm',
      valueLong: ['after-context', 'before-context', 'context', 'devices', 'directories', 'regexp', 'file', 'max-count', 'label',
        'include', 'exclude', 'exclude-dir', 'exclude-from', 'binary-files', 'group-separator'],
    });
    const directories = shorts.get('d') ?? longs.get('directories');
    const recursive = shorts.has('r') || shorts.has('R') || longs.has('recursive') || longs.has('dereference-recursive')
      || directories?.text === 'recurse';
    if (!recursive) return null;
    const patternGiven = shorts.has('e') || shorts.has('f') || longs.has('regexp') || longs.has('file');
    return { paths: patternGiven ? positionals : positionals.slice(1), useCwd: !piped, maxDepth: null };
  },
  ag(args, { piped }) {
    const { shorts, longs, positionals } = parseScanOptions(args, {
      valueShort: 'ABCGgmp',
      valueLong: ['after', 'before', 'context', 'file-search-regex', 'ignore', 'ignore-dir', 'max-count', 'path-to-ignore', 'depth', 'pager', 'workers'],
    });
    return { paths: shorts.has('g') ? positionals : positionals.slice(1), useCwd: !piped, maxDepth: scanDepth(longs.get('depth')) };
  },
  fd(args) {
    const { shorts, longs, positionals } = parseScanOptions(args, {
      valueShort: 'dtejEcSo',
      stopShort: 'xX',
      valueLong: ['max-depth', 'maxdepth', 'min-depth', 'exact-depth', 'type', 'extension', 'exclude', 'color', 'threads', 'size',
        'changed-within', 'changed-before', 'change-newer-than', 'change-older-than', 'newer', 'older', 'owner', 'base-directory',
        'path-separator', 'search-path', 'max-results', 'ignore-file', 'format', 'batch-size', 'max-buffer-time'],
      stopLong: ['exec', 'exec-batch'],
    });
    if (shorts.has('h') || shorts.has('V') || longs.has('help') || longs.has('version')) return null;
    const paths = positionals.slice(1);
    if (typeof longs.get('search-path') === 'object') paths.push(longs.get('search-path'));
    const base = longs.get('base-directory');
    return {
      paths,
      useCwd: true,
      base: typeof base === 'object' ? base : null,
      maxDepth: scanDepth(shorts.get('d') ?? longs.get('max-depth') ?? longs.get('maxdepth') ?? longs.get('exact-depth')),
    };
  },
  // du always walks the whole tree to total it; -d/-s only shorten the report.
  du(args) {
    const { positionals } = parseScanOptions(args, {
      valueShort: 'BIdtX',
      valueLong: ['max-depth', 'block-size', 'exclude', 'exclude-from', 'threshold', 'time-style', 'files0-from'],
    });
    return { paths: positionals, useCwd: true, maxDepth: null };
  },
  ls(args) {
    const { shorts, longs, positionals } = parseScanOptions(args, {
      valueLong: ['ignore', 'hide', 'width', 'tabsize', 'format', 'sort', 'time-style', 'indicator-style', 'quoting-style', 'block-size'],
    });
    if (!shorts.has('R') && !longs.has('recursive')) return null;
    return { paths: positionals, useCwd: true, maxDepth: null };
  },
  tree(args) {
    const { shorts, longs, positionals } = parseScanOptions(args, {
      valueShort: 'LPIoHT',
      valueLong: ['filelimit', 'charset', 'timefmt', 'sort', 'hintro', 'houtro', 'infofile', 'gitfile'],
    });
    if (longs.has('help') || longs.has('version')) return null;
    return { paths: positionals, useCwd: true, maxDepth: scanDepth(shorts.get('L')) };
  },
};
for (const [alias, walker] of [['gfind', 'find'], ['egrep', 'grep'], ['fgrep', 'grep'], ['ggrep', 'grep'], ['fdfind', 'fd'], ['gdu', 'du'], ['gls', 'ls']]) {
  SCAN_WALKERS[alias] = SCAN_WALKERS[walker];
}

const SCAN_SHELLS = new Set(['sh', 'bash', 'zsh', 'dash', 'ksh']);

function bashScanRoot(command, cwd, home, depth) {
  if (depth > 3 || typeof command !== 'string' || !command.trim()) return '';
  const parsed = shellCommands(command, home);
  if (!parsed) return '';
  for (const body of parsed.substitutions) {
    const hit = bashScanRoot(body, cwd, home, depth + 1);
    if (hit) return hit;
  }
  let current = cwd;
  for (const { words: all, piped } of parsed.commands) {
    // zsh expands ** by walking every folder below it, whatever the command.
    for (const word of all) {
      if (!word.glob || !word.text.includes('**')) continue;
      const target = resolveShellPath(word, current, home);
      if (!target) continue;
      const segments = target.path.split('/');
      const globstar = segments.findIndex((segment) => segment.includes('**'));
      if (globstar < 0) continue;
      const hit = scanRootHit({ path: segments.slice(0, globstar).join('/') || '/', glob: true }, null, home);
      if (hit) return hit;
    }
    const words = unwrapScanCommand(all);
    if (!words.length) continue;
    const name = path.basename(words[0].text);
    if (name === 'cd' || name === 'pushd') {
      const target = words.slice(1).find((word) => word.text === '-' || !word.text.startsWith('-'));
      if (!target) current = home;
      else current = target.text === '-' ? null : resolveShellPath(target, current, home)?.path ?? null;
      continue;
    }
    if (SCAN_SHELLS.has(name)) {
      const flag = words.findIndex((word) => /^-[a-z]*c$/.test(word.text));
      const hit = flag > 0 && words[flag + 1] ? bashScanRoot(words[flag + 1].text, current, home, depth + 1) : '';
      if (hit) return hit;
      continue;
    }
    const plan = SCAN_WALKERS[name]?.(words.slice(1), { piped });
    if (!plan) continue;
    const base = plan.base ? resolveShellPath(plan.base, current, home)?.path ?? null : current;
    const targets = plan.paths.length
      ? plan.paths.map((word) => resolveShellPath(word, base, home))
      : plan.useCwd && base ? [{ path: base }] : [];
    for (const target of targets) {
      const hit = scanRootHit(target, plan.maxDepth, home);
      if (hit) return hit;
    }
  }
  return '';
}

const TASK_EVENT_KINDS = new Set(['task_started', 'task_progress', 'task_updated', 'task_notification']);

function taskEventKind(message) {
  const kind = message?.type === 'system' ? message.subtype : message?.type;
  return TASK_EVENT_KINDS.has(kind) ? kind : null;
}

function spawnedWorkEvent(message, kind = taskEventKind(message)) {
  if (kind === 'task_started') {
    return { type: 'started', taskId: message.task_id, toolCallId: message.tool_use_id, description: message.description, subagentType: message.subagent_type, taskType: message.task_type, workflowName: message.workflow_name, parentTaskId: message.parent_task_id, backgrounded: message.is_backgrounded === true };
  }
  if (kind === 'task_progress') {
    return { type: 'progress', taskId: message.task_id, toolCallId: message.tool_use_id, description: message.description, subagentType: message.subagent_type, lastToolName: message.last_tool_name, summary: message.summary };
  }
  if (kind === 'task_updated') {
    return { type: 'updated', taskId: message.task_id, patch: { status: message.patch?.status, description: message.patch?.description, error: message.patch?.error, isBackgrounded: message.patch?.is_backgrounded } };
  }
  return { type: 'notification', taskId: message.task_id, toolCallId: message.tool_use_id, status: message.status, outputFile: message.output_file, summary: message.summary };
}

async function openSession(params, resume) {
  const requested = String(params?.sessionId || '').trim();
  if (resume && !requested) {
    throw new Error('Claude session/resume requires the exact provider thread id');
  }
  const injected = String(process.env.WORKASS_CLAUDE_SESSION_ID || '').trim();
  const sessionId = requested || injected || randomUUID();
  const existing = sessions.get(sessionId);
  if (existing) return existing;
  const session = new ClaudeSession({
    sessionId,
    cwd: String(params?.cwd || process.cwd()),
    mcpServers: params?.mcpServers,
    resume,
  });
  try {
    await session.start();
  } catch (error) {
    if (resume && /no conversation found with session id/i.test(String(error?.message || ''))) {
      throw Object.assign(new Error('Claude provider candidate was never materialized'), { rpcCode: -32044 });
    }
    throw error;
  }
  // The open request's openQuery runs are over: any later reopen is a
  // mid-session restart and must re-announce the command catalog itself.
  session.openReplyPending = false;
  // Keyed by the session's OWN id: start() may have rotated it away from the
  // requested id when session/new collided with an existing transcript.
  sessions.set(session.sessionId, session);
  return session;
}

async function handleRequest(message) {
  const { id, method, params = {} } = message;
  if (method === 'initialize') {
    respond(id, {
      protocolVersion: Number(params.protocolVersion || 1),
      agentInfo: { name: 'Claude Code', version: 'official-agent-sdk' },
      agentCapabilities: {
        sessionCapabilities: { resume: {}, close: {} },
        promptCapabilities: { image: true, audio: false, embeddedContext: false },
        // External MCP servers retain the host's supported stdio transport.
        mcpCapabilities: { http: false, sse: false },
      },
      authMethods: [],
      _meta: {
        workassClaudeSteerRequest: true,
        // Ask the daemon to say which prompts a person typed
        // (_meta.workassCommandInput.humanAuthored); see sdkUserMessage.
        workassNativeCommandInputV1: true,
        workassClaudeSteerReceipt: true,
        workassClaudeUsageRequest: true,
		workassStableTurnInputV1: true,
        // Advertised so the daemon can tell "old host" (absent commandCatalog
        // = UNKNOWN) apart from "proven empty" ([] from a host that looked).
        workassClaudeCommandCatalog: true,
      },
    });
    return;
  }
  if (method === 'session/new' || method === 'session/resume') {
    const session = await openSession(params, method !== 'session/new');
    respond(id, {
      ...(method === 'session/new' ? { sessionId: session.sessionId } : {}),
      configOptions: session.configOptions(),
      availableModels: session.availableModels(),
      ...(session.commandCatalog ? { commandCatalog: session.commandCatalog } : {}),
      _meta: { workassProviderRealm: session.providerRealm },
    });
    return;
  }
  const sessionId = String(params.sessionId || '').trim();
  const session = sessions.get(sessionId);
  if (!session) throw Object.assign(new Error('Claude session not found'), { rpcCode: -32000 });
  if (method === 'session/prompt') {
	respond(id, await session.enqueuePrompt(params.prompt, String(params.clientUserMessageId || '').trim(),
	  params._meta?.workassCommandInput?.humanAuthored === true));
    return;
  }
  if (method === 'session/set_config_option') {
    respond(id, await session.setConfig(String(params.configId || ''), params.value));
    return;
  }
  if (method === '_workass/claude/steer') {
    respond(id, session.steer(params.prompt, String(params.clientUserMessageId || '')));
    return;
  }
  if (method === '_workass/claude/usage') {
    respond(id, await session.usage());
    return;
  }
  if (method === 'session/close') {
    session.close();
    sessions.delete(sessionId);
    respond(id, {});
    return;
  }
  throw Object.assign(new Error(`Claude host method not found: ${method}`), { rpcCode: -32601 });
}

function handleNotification(message) {
  const session = sessions.get(String(message.params?.sessionId || ''));
  if (!session) return;
  if (message.method === 'session/cancel') void session.interrupt().catch((error) => diagnostic('Claude interrupt failed', error));
  if (message.method === '_session/steer') {
    try { session.steer(message.params?.prompt, ''); } catch (error) { diagnostic('Claude steer failed', error); }
  }
}

// One-shot native fork (no ACP loop, no sampling): copies a Claude session into
// a new resumable session with the official SDK and prints exactly one JSON
// object on stdout. Used for forked subsessions and forked subagents
// (user 2026-10-16). Input on stdin: {sessionId, dir, beforeUserMessageIds?};
// with beforeUserMessageIds (candidate uuids of ONE user prompt) the copy ends
// at the message right before that prompt, otherwise it is a full copy. Never falls back to a full copy
// when a requested boundary cannot be found.
async function forkSessionCommand() {
  const chunks = [];
  for await (const chunk of process.stdin) chunks.push(chunk);
  const request = JSON.parse(Buffer.concat(chunks).toString('utf8') || '{}');
  const sessionId = String(request.sessionId || '').trim();
  const dir = String(request.dir || '').trim();
  const before = (Array.isArray(request.beforeUserMessageIds) ? request.beforeUserMessageIds : [])
    .map((id) => String(id || '').trim()).filter(Boolean);
  if (!sessionId || !dir) throw new Error('fork requires sessionId and dir');
  const sdk = await loadSDK();
  if (typeof sdk.forkSession !== 'function') throw new Error('this Claude Agent SDK cannot fork sessions');
  let upToMessageId;
  if (before.length) {
    const messages = await sdk.getSessionMessages(sessionId, { dir });
    const index = messages.findIndex((message) => before.includes(message?.uuid));
    if (index <= 0) throw new Error('fork boundary message is not in the Claude session');
    upToMessageId = messages[index - 1].uuid;
  }
  const result = await sdk.forkSession(sessionId, { dir, ...(upToMessageId ? { upToMessageId } : {}) });
  const forked = String(result?.sessionId || '').trim();
  if (!forked) throw new Error('Claude Agent SDK returned no forked session id');
  process.stdout.write(JSON.stringify({ sessionId: forked }) + '\n');
}

function serveACP() {
  const lines = readline.createInterface({ input: process.stdin, crlfDelay: Infinity });
  lines.on('line', (line) => {
    let message;
    try {
      message = JSON.parse(line);
    } catch {
      diagnostic('Claude host received invalid JSON', 'parse failure');
      return;
    }
    if (message && Object.hasOwn(message, 'id') && !message.method) {
      const pending = pendingPeerRequests.get(String(message.id));
      if (pending) {
        pendingPeerRequests.delete(String(message.id));
        pending(message);
      }
      return;
    }
    if (!message?.method) return;
    if (!Object.hasOwn(message, 'id')) {
      handleNotification(message);
      return;
    }
    void handleRequest(message).catch((error) => fail(message.id, error?.rpcCode || -32603, error));
  });

  lines.on('close', () => {
    for (const session of sessions.values()) session.close();
    sessions.clear();
  });

}

// Imported (tests read homeScanDenial) it serves nothing. Only a definite
// false skips serving: a Node without import.meta.main must still start the host.
if (import.meta.main !== false) {
  if (process.argv.includes('--workass-fork-session')) {
    forkSessionCommand().then(
      () => process.exit(0),
      (error) => {
        process.stderr.write(`Claude fork failed: ${String(error?.message || error)}\n`);
        process.exit(1);
      },
    );
  } else {
    serveACP();
  }
}
