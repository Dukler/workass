#!/usr/bin/env node

// Workass-owned Claude Code host. It speaks the existing private Workass
// session contract on stdio while using Anthropic's official Agent SDK and
// the user's installed Claude Code executable underneath. stdout is protocol
// JSON only; diagnostics go to stderr.

import { randomUUID } from 'node:crypto';
import path from 'node:path';
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
    .replace(/(api[_-]?key|token|secret|password|credential|bearer)\s*[:=]\s*\S+/gi, '$1=[redacted]')
    .slice(0, 2000);
}

function diagnostic(label, error) {
  process.stderr.write(`${label}: ${safeErrorText(error)}\n`);
}

function isMissingConversationError(error) {
  return /no conversation found with session id/i.test(String(error?.message || error || ''));
}

function isSessionInUseError(error) {
  return /session id .* is already in use/i.test(String(error?.message || error || ''));
}

function isRecoverableQueryControlError(error) {
  const text = String(error?.message || error || '');
  return isMissingConversationError(error) || /ProcessTransport is not ready for writing/i.test(text);
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

function sdkUserMessage(sessionId, blocks, uuid = randomUUID(), priority) {
  return {
    type: 'user',
    message: { role: 'user', content: promptContent(blocks) },
    parent_tool_use_id: null,
    session_id: sessionId,
    uuid,
    ...(priority ? { priority } : {}),
  };
}

function mcpServerMap(rawServers) {
  const result = {};
  for (const raw of Array.isArray(rawServers) ? rawServers : []) {
    if (!raw || typeof raw !== 'object' || typeof raw.name !== 'string' || !raw.name.trim()) continue;
    if (typeof raw.url === 'string' && raw.url.trim()) {
      result[raw.name] = {
        type: raw.type === 'sse' ? 'sse' : 'http',
        url: raw.url,
        ...(raw.headers && typeof raw.headers === 'object' ? { headers: raw.headers } : {}),
      };
      continue;
    }
    if (typeof raw.command !== 'string' || !raw.command.trim()) continue;
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

  // Claude Code says there is no conversation behind this id.
  noteConversationMissing() {
    this.established = false;
    this.materialized = false;
    this.resumeRequested = false;
  }

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
    // True while a turn the harness started — not the user — is running. Cancel
    // and turn reconciliation both key off it; without it they address the
    // previous turn.
    this.harnessTurnActive = false;
    this.steers = new SteerLedger();
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
    this.turnPhase = '';
    this.turnToolName = '';
    this.retryState = null;
    this.turnStatus = 'idle';
    this.closed = false;
    this.toolCalls = new Map();
    // Tool ids of questions the user actually answered — see completeToolResults.
    this.answeredQuestions = new Set();
    this.partialTextSeen = false;
    this.partialThoughtSeen = false;
  }

  async start() {
    const resumeExisting = this.conversation.resumeRequested;
    try {
      return await this.openQuery(resumeExisting);
    } catch (error) {
      if (resumeExisting && isMissingConversationError(error)) {
        this.conversation.noteConversationMissing();
        this.providerSessionId = this.sessionId;
        const previous = this.query;
        if (previous) this.retireQuery(previous, true);
        return this.openQuery(false);
      }
      if (!resumeExisting && isSessionInUseError(error)) {
        // session/new promised a fresh empty session, so adopting the disk
        // history behind the collided id would smuggle unknown context under
        // a canonical replay. Rotate to an id nothing has used.
        this.sessionId = randomUUID();
        this.providerSessionId = this.sessionId;
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
      systemPrompt: { type: 'preset', preset: 'claude_code' },
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
  // These callbacks are observers only. UserPromptSubmit blocks the prompt it
  // fires for and times out at 30s, so every callback does one synchronous
  // stdout write, swallows its own errors and returns {} immediately. They
  // compose with the user's own settings-file hooks, which keep working.
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

  emitTurnPhase(phase, fields, input) {
    notify(this.sessionId, {
      sessionUpdate: '_workass_claude_turn',
      phase,
      promptId: clipTurnField(input?.prompt_id),
      ...fields,
    });
  }

  emitTurnStarted(input) {
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
    // Wait for retired queries to release the provider-native session id.
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

  async enqueuePrompt(blocks) {
    if (this.activePrompt) throw new Error('A Claude turn is already running');
    await this.ensureQuery();
    const uuid = randomUUID();
    const message = sdkUserMessage(this.sessionId, blocks, uuid);
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
      };
    });
    this.input.push(message);
    return response;
  }

  // The SDK does not redirect the sampling step in flight: it closes the current
  // assistant turn and answers the pushed message in the NEXT turn of the same
  // query. That follow-up output belongs to the prompt the user steered, so the
  // turn must stay open across the boundary — settling on the first result
  // orphaned Claude's actual answer after the job had already ended.
  steer(blocks, clientUserMessageId) {
    if (!this.activePrompt) throw new Error('No active Claude turn accepts steering');
    const uuid = randomUUID();
    const message = sdkUserMessage(this.sessionId, blocks, uuid, 'now');
    // The message is retained, not just pushed: if this steer aborts the turn
    // before the model said anything, it is the message we have to ask again
    // (see redriveSteersAfterEmptyAbort).
    this.steers.retain(uuid, { clientUserMessageId: clientUserMessageId || '', message });
    this.input.push(message);
    return { turnId: uuid, receipt: Boolean(clientUserMessageId) };
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
  failOpenToolCalls(reason) {
    for (const [id, tracked] of this.toolCalls) {
      notify(this.sessionId, {
        sessionUpdate: 'tool_call_update', toolCallId: id,
        title: toolTitle(tracked.name, tracked.input), kind: toolKind(tracked.name),
        status: 'failed', content: normalizeToolResultContent(reason),
        _meta: { claudeCode: { toolName: tracked.name } },
      });
    }
    this.toolCalls.clear();
  }

  settlePrompt(result, error) {
    const active = this.activePrompt;
    if (!active) return;
    this.activePrompt = null;
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
  // Claude Code's query has returned, so it is retired and reopened once.
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
      this.settlePrompt(null, error);
      this.retireQuery(query, false);
    } catch (error) {
      if (this.closed || generation !== this.generation) return;
      diagnostic('Claude SDK stream failed', error);
      if (isMissingConversationError(error)) {
        this.conversation.noteConversationMissing();
      }
      this.settlePrompt(null, error);
      this.retireQuery(query, false);
    }
  }

  handleMessage(message) {
    if (!message || typeof message !== 'object') return;
    // Any SDK traffic proves the provider stream is alive again.
    this.retryState = null;
    const announced = typeof message.session_id === 'string' ? message.session_id.trim() : '';
    if (announced && announced !== this.providerSessionId) {
      this.providerSessionId = announced;
      notify(this.sessionId, { sessionUpdate: '_workass_claude_provider_session', providerSessionId: announced });
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
    }
    if (message.type === 'system' && message.subtype === 'compact_boundary') {
      this.noteTurnPhase('waiting');
      this.emitTurnPulse();
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
      this.acceptActivePrompt(String(message.uuid || ''));
      this.completeToolResults(message);
      return;
    }
    if (message.type === 'stream_event') {
      this.handleStreamEvent(message.event);
      return;
    }
    if (message.type === 'assistant') {
      this.handleAssistantContent(message.message?.content);
      return;
    }
    if (message.type === 'result') {
      // D8: a result stamped with a non-human origin belongs to the turn the
      // harness started, not to a human prompt that may be queued alongside it.
      // Settling activePrompt or consuming a steer boundary here would end the
      // user's turn on somebody else's result. origin is authoritative;
      // harnessTurnActive covers a build that does not stamp it.
      const originKind = String(message.origin?.kind || '');
      if ((originKind && originKind !== 'human') || (!this.activePrompt && this.harnessTurnActive)) {
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
        if (active && this.steers.awaitingBoundary() && isEmptyAbortResult(message)) {
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
      if (this.consumeSteerBoundary()) return;
      this.emitTurnEnded(message);
      if (message.usage) {
        const used = numeric(message.usage.input_tokens) + numeric(message.usage.cache_creation_input_tokens)
          + numeric(message.usage.cache_read_input_tokens) + numeric(message.usage.output_tokens);
        notify(this.sessionId, {
          sessionUpdate: 'usage_update', used, size: maxContextWindow(message.modelUsage),
          _meta: { 'workass.claude/turnUsage': message.usage },
        });
      }
      this.settlePrompt(message, null);
      return;
    }
    if (message.type === 'task_started' || message.type === 'task_progress' || message.type === 'task_updated' || message.type === 'task_notification') {
      notify(this.sessionId, { sessionUpdate: '_workass_claude_spawned_work', event: spawnedWorkEvent(message) });
    }
  }

  handleStreamEvent(event) {
    if (!event || typeof event !== 'object') return;
    if (event.type === 'content_block_start' && event.content_block?.type === 'tool_use') {
      this.startTool(event.content_block);
      return;
    }
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

  handleAssistantContent(content) {
    for (const block of Array.isArray(content) ? content : []) {
      if (block?.type === 'tool_use') this.startTool(block);
      else if (block?.type === 'text' && block.text && !this.partialTextSeen) {
        this.emitPromptUpdate({ sessionUpdate: 'agent_message_chunk', content: { type: 'text', text: block.text } });
      } else if (block?.type === 'thinking' && block.thinking && !this.partialThoughtSeen) {
        this.emitPromptUpdate({ sessionUpdate: 'agent_thought_chunk', content: { type: 'text', text: block.thinking } });
      }
    }
  }

  startTool(block) {
    const id = String(block?.id || '').trim();
    if (!id || this.toolCalls.has(id)) return;
    const name = String(block.name || 'Tool');
    this.noteTurnPhase('tool', name);
    this.acceptActivePrompt();
    const active = this.activePrompt;
    if (active) {
      this.flushPromptUpdates(active);
      active.substantiveOutput = true;
    }
    this.toolCalls.set(id, { name, input: block.input || {} });
    notify(this.sessionId, {
      sessionUpdate: 'tool_call', toolCallId: id, title: toolTitle(name, block.input),
      kind: toolKind(name), status: 'in_progress', rawInput: block.input || {},
      _meta: { claudeCode: { toolName: name } },
    });
  }

  completeToolResults(message) {
    for (const block of Array.isArray(message?.message?.content) ? message.message.content : []) {
      if (block?.type !== 'tool_result') continue;
      const id = String(block.tool_use_id || '').trim();
      const tracked = this.toolCalls.get(id);
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
        _meta: { claudeCode: { toolName: tracked.name } },
      });
      this.toolCalls.delete(id);
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
      if (!isRecoverableQueryControlError(error)) throw error;
      if (isMissingConversationError(error)) {
        this.conversation.noteConversationMissing();
      }
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
    if (!this.activePrompt) return;
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
    ++this.generation;
    this.input.close();
    if (this.query?.close) this.query.close();
    this.settlePrompt(null, new Error('Claude session closed'));
  }
}

function numeric(value) {
  return typeof value === 'number' && Number.isFinite(value) ? Math.max(0, value) : 0;
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

function toolKind(name) {
  const lower = String(name || '').toLowerCase();
  if (/bash|shell|command|terminal/.test(lower)) return 'execute';
  if (/edit|write|patch|replace|create|delete/.test(lower)) return 'edit';
  if (/read|grep|glob|search|view/.test(lower)) return 'read';
  return 'other';
}

function toolTitle(name, input) {
  const candidate = input?.description || input?.path || input?.file_path || input?.command;
  return String(candidate || name || 'Claude Code tool').slice(0, 240);
}

function spawnedWorkEvent(message) {
  if (message.type === 'task_started') {
    return { type: 'started', taskId: message.task_id, toolCallId: message.tool_use_id, description: message.description, subagentType: message.subagent_type, taskType: message.task_type, workflowName: message.workflow_name };
  }
  if (message.type === 'task_progress') {
    return { type: 'progress', taskId: message.task_id, toolCallId: message.tool_use_id, description: message.description, subagentType: message.subagent_type, lastToolName: message.last_tool_name, summary: message.summary };
  }
  if (message.type === 'task_updated') {
    return { type: 'updated', taskId: message.task_id, patch: { status: message.patch?.status, description: message.patch?.description, error: message.patch?.error, isBackgrounded: message.patch?.is_backgrounded } };
  }
  return { type: 'notification', taskId: message.task_id, toolCallId: message.tool_use_id, status: message.status, outputFile: message.output_file, summary: message.summary };
}

async function openSession(params, resume) {
  const requested = String(params?.sessionId || '').trim();
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
  await session.start();
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
        loadSession: true,
        sessionCapabilities: { resume: {}, close: {} },
        promptCapabilities: { image: true, audio: false, embeddedContext: false },
        mcpCapabilities: { http: true, sse: true },
      },
      authMethods: [],
      _meta: {
        workassClaudeSteerRequest: true,
        workassClaudeSteerReceipt: true,
        workassClaudeUsageRequest: true,
        workassTurnReconcileRequest: true,
        // Advertised so the daemon can tell "old host" (absent commandCatalog
        // = UNKNOWN) apart from "proven empty" ([] from a host that looked).
        workassClaudeCommandCatalog: true,
      },
    });
    return;
  }
  if (method === 'session/new' || method === 'session/resume' || method === 'session/load') {
    const session = await openSession(params, method !== 'session/new');
    respond(id, {
      ...(method === 'session/new' ? { sessionId: session.sessionId } : {}),
      configOptions: session.configOptions(),
      availableModels: session.availableModels(),
      ...(session.commandCatalog ? { commandCatalog: session.commandCatalog } : {}),
    });
    return;
  }
  const sessionId = String(params.sessionId || '').trim();
  const session = sessions.get(sessionId);
  if (!session) throw Object.assign(new Error('Claude session not found'), { rpcCode: -32000 });
  if (method === 'session/prompt') {
    respond(id, await session.enqueuePrompt(params.prompt));
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
  if (method === '_workass/turn/reconcile') {
    // D2: during an adopted harness turn the previous turn's status is still
    // 'completed'. Reporting that as terminal would let the daemon reconcile a
    // live turn out of existence.
    const status = session.harnessTurnActive ? 'active' : session.turnStatus;
    const terminal = ['completed', 'failed', 'interrupted'].includes(status);
    respond(id, { status, terminal, reconciled: terminal });
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
