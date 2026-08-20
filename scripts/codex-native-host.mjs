#!/usr/bin/env node

// Workass-owned Codex host. It translates the stable private Workass session
// contract into the official `codex app-server` protocol directly. No ACP or
// Zed compatibility package participates in this process tree.

import { spawn } from 'node:child_process';
import { createHash } from 'node:crypto';
import { open } from 'node:fs/promises';
import path from 'node:path';
import process from 'node:process';
import readline from 'node:readline';

const sessions = new Map();
const pendingWorkassRequests = new Map();
let workassRequestSequence = 0;
let app;
let initializePromise;
let providerRealm;
let modelCatalog = [];
const MAX_MCP_STARTUP_STATES = 512;

function safeErrorText(value) {
  return String(value?.message || value || 'Codex request failed')
    .replace(/(api[_-]?key|token|secret|password|credential|bearer)\s*[:=]\s*\S+/gi, '$1=[redacted]')
    .slice(0, 2000);
}

function diagnostic(label, error) {
  process.stderr.write(`${label}: ${safeErrorText(error)}\n`);
}

function write(message) { process.stdout.write(`${JSON.stringify(message)}\n`); }
function respond(id, result) { write({ jsonrpc: '2.0', id, result }); }
function fail(id, code, error, data) {
  write({ jsonrpc: '2.0', id, error: { code, message: safeErrorText(error), ...(data !== undefined ? { data } : {}) } });
}
function notify(sessionId, update) {
  write({ jsonrpc: '2.0', method: 'session/update', params: { sessionId, update } });
}

// Plumbing RPCs keep a guard so a client that never answers cannot wedge a
// turn. Anything a HUMAN answers passes 0 and waits: a permission card is not a
// slow peer, and expiring it here resolved null — read as "the user chose
// nothing" while the card was still on their screen (user 2026-07-25). The
// origin harness owns that deadline now.
const WORKASS_REQUEST_TIMEOUT_MS = (() => {
  const configured = Number.parseInt(String(process.env.WORKASS_ACP_PEER_TIMEOUT_MS || ''), 10);
  return Number.isFinite(configured) && configured > 0 ? configured : 120_000;
})();

function requestWorkass(method, params, timeoutMs = WORKASS_REQUEST_TIMEOUT_MS) {
  const id = `codex-host-${++workassRequestSequence}`;
  write({ jsonrpc: '2.0', id, method, params });
  return new Promise((resolve) => {
    const timer = timeoutMs > 0
      ? setTimeout(() => {
        pendingWorkassRequests.delete(id);
        resolve(null);
      }, timeoutMs)
      : null;
    pendingWorkassRequests.set(id, (message) => {
      if (timer) clearTimeout(timer);
      resolve(message?.result ?? null);
    });
  });
}

class AppServerPeer {
  constructor(executable, args) {
    this.sequence = 0;
    this.pending = new Map();
    this.mcpStartup = new Map();
    this.mcpStartupWaiters = new Set();
    this.mcpStartupRevision = 0;
    this.closing = false;
    this.child = spawn(executable, args, { stdio: ['pipe', 'pipe', 'pipe'], env: process.env });
    readline.createInterface({ input: this.child.stdout }).on('line', (line) => this.accept(line));
    this.child.stderr.on('data', (chunk) => diagnostic('codex app-server', chunk.toString('utf8')));
    this.child.on('error', (error) => this.terminate(error));
    this.child.on('exit', (code, signal) => {
      const error = new Error(`codex app-server exited (${signal || `code ${code}`})`);
      this.terminate(error);
      if (!this.closing) setTimeout(() => process.exit(1), 0);
    });
  }

  accept(line) {
    let message;
    try { message = JSON.parse(line); } catch { diagnostic('codex app-server emitted invalid JSON', 'parse failure'); return; }
    if (Object.hasOwn(message, 'id') && !message.method) {
      const pending = this.pending.get(String(message.id));
      if (!pending) return;
      this.pending.delete(String(message.id));
      if (message.error) {
        const error = Object.assign(new Error(message.error.message || 'Codex app-server request failed'), {
          code: message.error.code,
          data: message.error.data,
        });
        pending.reject(error);
      } else pending.resolve(message.result || {});
      return;
    }
    if (Object.hasOwn(message, 'id') && message.method) {
      void handleAppRequest(message).then(
        (result) => this.write({ id: message.id, result }),
        (error) => this.write({ id: message.id, error: { code: -32603, message: safeErrorText(error) } }),
      );
      return;
    }
    if (message.method) {
      if (message.method === 'mcpServer/startupStatus/updated') {
        this.recordMCPStartupStatus(message.params || {});
      }
      void handleAppNotification(message.method, message.params || {});
    }
  }

  request(method, params = {}) {
    const id = `workass-${++this.sequence}`;
    return new Promise((resolve, reject) => {
      this.pending.set(id, { resolve, reject });
      try { this.write({ id, method, params }); }
      catch (error) { this.pending.delete(id); reject(error); }
    });
  }

  notify(method, params) { this.write({ method, ...(params === undefined ? {} : { params }) }); }
  recordMCPStartupStatus(params) {
    const name = String(params?.name || '').trim();
    if (!name) return;
    const threadId = String(params?.threadId || '').trim();
    const key = `${threadId}\0${name}`;
    this.mcpStartup.delete(key);
    this.mcpStartup.set(key, {
      revision: ++this.mcpStartupRevision,
      status: String(params?.status || '').trim(),
      error: params?.error ? safeErrorText(params.error) : '',
    });
    while (this.mcpStartup.size > MAX_MCP_STARTUP_STATES) {
      this.mcpStartup.delete(this.mcpStartup.keys().next().value);
    }
    for (const waiter of [...this.mcpStartupWaiters]) {
      if (!waiter.names.has(name) || (threadId && threadId !== waiter.threadId)) continue;
      this.mcpStartupWaiters.delete(waiter);
      clearTimeout(waiter.timer);
      waiter.resolve();
    }
  }
  mcpStartupStatus(threadId, name, afterRevision) {
    const normalizedName = String(name || '').trim();
    const candidates = [
      this.mcpStartup.get(`${String(threadId || '').trim()}\0${normalizedName}`),
      this.mcpStartup.get(`\0${normalizedName}`),
    ].filter((status) => status && status.revision > afterRevision);
    return candidates.reduce((latest, status) => (
      !latest || status.revision > latest.revision ? status : latest
    ), null);
  }
  waitForMCPStartupChange(threadId, names, waitMs) {
    return new Promise((resolve, reject) => {
      const waiter = {
        threadId: String(threadId || '').trim(), names: new Set(names), resolve, reject, timer: null,
      };
      waiter.timer = setTimeout(() => {
        this.mcpStartupWaiters.delete(waiter);
        resolve();
      }, waitMs);
      this.mcpStartupWaiters.add(waiter);
    });
  }
  write(message) {
    if (!this.child.stdin.writable) throw new Error('codex app-server stdin is closed');
    this.child.stdin.write(`${JSON.stringify(message)}\n`);
  }
  terminate(error) {
    for (const pending of this.pending.values()) pending.reject(error);
    this.pending.clear();
    for (const waiter of this.mcpStartupWaiters) {
      clearTimeout(waiter.timer);
      waiter.reject(error);
    }
    this.mcpStartupWaiters.clear();
  }
  close() {
    this.closing = true;
    this.child.stdin.end();
    this.child.kill('SIGTERM');
  }
}

function appServerArgs() {
  const configured = String(process.env.WORKASS_CODEX_APP_SERVER_ARGS || '').trim();
  if (!configured) return ['app-server'];
  const parsed = JSON.parse(configured);
  if (!Array.isArray(parsed) || !parsed.every((value) => typeof value === 'string')) {
    throw new Error('WORKASS_CODEX_APP_SERVER_ARGS must be a JSON string array');
  }
  return parsed;
}

function opaqueRealmScope(kind, value) {
  const normalized = String(value || '').trim();
  if (!normalized) return '';
  return `${kind}-${createHash('sha256').update(normalized).digest('hex').slice(0, 32)}`;
}

async function ensureInitialized() {
  if (!initializePromise) {
    initializePromise = (async () => {
      const executable = String(process.env.WORKASS_CODEX_EXECUTABLE || '').trim();
      if (!executable) throw new Error('WORKASS_CODEX_EXECUTABLE is required');
      app = new AppServerPeer(executable, appServerArgs());
      const initialized = await app.request('initialize', {
        clientInfo: { name: 'workass', title: 'Workass', version: String(process.env.WORKASS_VERSION || 'dev') },
        capabilities: { experimentalApi: true },
      });
      const accountIdentity = initialized?.account?.email || initialized?.account?.id || initialized?.user?.email || '';
      providerRealm = {
        accountScope: opaqueRealmScope('account', accountIdentity) || 'unverified-account',
        installScope: opaqueRealmScope('install', `${executable}\0${initialized?.codexHome || ''}`),
        verified: Boolean(accountIdentity),
      };
      app.notify('initialized');
      modelCatalog = await fetchModels();
      if (!modelCatalog.length) throw new Error('Codex app-server returned no models');
    })();
  }
  return initializePromise;
}

async function fetchModels() {
  const models = [];
  let cursor = null;
  do {
    const response = await app.request('model/list', { cursor, limit: null });
    models.push(...(Array.isArray(response.data) ? response.data : []));
    cursor = response.nextCursor || null;
  } while (cursor);
  return models.filter((model) => model && !model.hidden);
}

const modes = [
  { value: 'read-only', name: 'Read-only', description: 'Requires approval to edit files and run commands.' },
  { value: 'agent', name: 'Agent', description: 'Read and edit files, and run commands.' },
  { value: 'agent-full-access', name: 'Agent (full access)', description: 'Run with full file and network access.' },
];

function configOptions(session) {
  const model = session.modelRow();
  const effortOptions = (model?.supportedReasoningEfforts || []).map((option) => ({
    value: option.reasoningEffort,
    name: option.reasoningEffort,
    description: option.description || '',
  }));
  return [
    { id: 'mode', category: 'mode', name: 'Mode', type: 'select', currentValue: session.mode, options: modes },
    {
      id: 'model', category: 'model', name: 'Model', type: 'select', currentValue: session.model,
      options: modelCatalog.map((row) => ({ value: row.id, name: row.displayName || row.id, description: row.description || '' })),
    },
    ...(effortOptions.length ? [{
      id: 'reasoning_effort', category: 'thought_level', name: 'Reasoning effort', type: 'select',
      currentValue: session.effort, options: effortOptions,
    }] : []),
  ];
}

function availableModels() {
  return modelCatalog.map((row) => ({ modelId: row.id, name: row.displayName || row.id, description: row.description || '' }));
}

class CodexSession {
  constructor({ threadId, cwd, model, effort, mcpServers }) {
    this.threadId = threadId;
    this.cwd = cwd;
    this.model = model;
    this.effort = effort;
    this.mode = 'agent';
    this.mcpServers = mcpServers;
    this.activeTurnId = '';
	this.activePromptClientId = '';
    this.activePrompt = null;
    this.turnStatus = 'idle';
    this.lastUsage = null;
    this.agentPhases = new Map();
    this.turnError = null;
    this.completedTurns = new Map();
    this.turnStartedPromise = null;
    this.resolveTurnStarted = null;
    this.rejectTurnStarted = null;
  }

  modelRow() { return modelCatalog.find((row) => row.id === this.model) || modelCatalog[0]; }

  operationInTurns(turns, clientId) {
    const rows = Array.isArray(turns) ? turns : [];
    for (let index = rows.length - 1; index >= 0; index -= 1) {
      const turn = rows[index];
      const item = (Array.isArray(turn?.items) ? turn.items : [])
        .find((candidate) => candidate?.type === 'userMessage' && candidate.clientId === clientId);
      if (item) return turn;
    }
    return null;
  }

  operationReadback(turn) {
    if (!turn) return { found: false, consumed: false, status: 'absent', terminal: false };
    const status = String(turn.status || 'unknown');
    return {
      found: true, consumed: true, turnId: String(turn.id || ''), status,
      terminal: ['completed', 'failed', 'interrupted', 'cancelled', 'canceled'].includes(status),
    };
  }

  itemsListUnavailable(error) {
    const message = String(error?.message || '');
    return Number(error?.code) === -32601
      || /thread\/items\/list.*(?:not supported|not found|unknown method)/i.test(message);
  }

  async reconcileOperation(clientUserMessageId) {
    const clientId = String(clientUserMessageId || '').trim();
    if (!clientId) throw new Error('operation reconciliation requires clientUserMessageId');
    if (this.activePromptClientId === clientId && this.activeTurnId) {
      return {
        found: true, consumed: true, turnId: this.activeTurnId, status: this.turnStatus,
        terminal: ['completed', 'failed', 'interrupted'].includes(this.turnStatus),
      };
    }
    let matched = null;
    try {
      let cursor = null;
      for (let page = 0; page < 8 && !matched; page += 1) {
        const response = await app.request('thread/items/list', {
          threadId: this.threadId, cursor, limit: 256, sortDirection: 'desc',
        });
        matched = (Array.isArray(response?.data) ? response.data : [])
          .find((entry) => entry?.item?.type === 'userMessage' && entry.item.clientId === clientId) || null;
        cursor = response?.nextCursor || null;
        if (!cursor) break;
      }
    } catch (error) {
      if (!this.itemsListUnavailable(error)) throw error;
      // Codex 0.148 publishes thread/items/list in its generated schema while
      // the live app-server still rejects the method as unsupported. The
      // official thread/read response carries the current turn ledger and the
      // stable user-message clientId, which is sufficient for this exact
      // operation readback without sampling or replaying the prompt.
      const response = await app.request('thread/read', { threadId: this.threadId, includeTurns: true });
      return this.operationReadback(this.operationInTurns(response?.thread?.turns, clientId));
    }
    if (!matched) return { found: false, consumed: false, status: 'absent', terminal: false };
    const response = await app.request('thread/read', { threadId: this.threadId, includeTurns: true });
    const turn = (response?.thread?.turns || []).find((candidate) => candidate.id === matched.turnId);
    return this.operationReadback(turn || { id: matched.turnId, status: 'unknown' });
  }

  startPrompt(blocks, clientUserMessageId = '') {
    if (this.activePrompt) throw new Error('A Codex turn is already running');
    this.turnStatus = 'starting';
	this.activePromptClientId = String(clientUserMessageId || '').trim();
    this.turnError = null;
    this.turnStartedPromise = new Promise((resolve, reject) => {
      this.resolveTurnStarted = resolve;
      this.rejectTurnStarted = reject;
    });
    // A failed turn/start can happen before a concurrent steer begins awaiting
    // this promise. Attach a rejection observer immediately so Node never turns
    // that protocol race into an unhandled-rejection process crash.
    this.turnStartedPromise.catch(() => {});
    const promise = new Promise((resolve, reject) => { this.activePrompt = { resolve, reject }; });
    void app.request('turn/start', {
      threadId: this.threadId,
      input: codexInput(blocks),
	  ...(clientUserMessageId ? { clientUserMessageId } : {}),
      cwd: this.cwd,
      model: this.model,
      effort: this.effort,
      summary: 'auto',
      ...modeTurnParams(this.mode),
    }).then((response) => {
      const turnId = String(response?.turn?.id || '');
      if (!turnId) throw new Error('Codex turn/start returned no turn id');
      this.setActiveTurn(turnId);
      this.turnStatus = response.turn.status || 'inProgress';
      const early = this.completedTurns.get(turnId);
      if (early) { this.completedTurns.delete(turnId); this.complete(early); }
    }).catch((error) => this.failPrompt(error));
    return promise;
  }

  complete(turn) {
    if (!this.activePrompt || (this.activeTurnId && turn.id !== this.activeTurnId)) {
      this.completedTurns.set(turn.id, turn);
      return;
    }
    const prompt = this.activePrompt;
    this.activePrompt = null;
    this.activeTurnId = '';
    this.turnStartedPromise = null;
    this.resolveTurnStarted = null;
    this.rejectTurnStarted = null;
    this.turnStatus = turn.status;
    if (turn.status === 'failed') {
      prompt.reject(this.turnError || new Error(turn.error?.message || 'Codex turn failed'));
      return;
    }
    prompt.resolve({
      stopReason: turn.status === 'interrupted' ? 'cancelled' : 'end_turn',
      ...(this.lastUsage ? { usage: this.lastUsage } : {}),
    });
  }

  failPrompt(error) {
    if (!this.activePrompt) return;
    const prompt = this.activePrompt;
    this.activePrompt = null;
    this.activeTurnId = '';
    this.rejectTurnStarted?.(error);
    this.turnStartedPromise = null;
    this.resolveTurnStarted = null;
    this.rejectTurnStarted = null;
    this.turnStatus = 'failed';
    prompt.reject(error);
  }

  async steer(blocks, clientUserMessageId) {
    if (!this.activeTurnId && this.activePrompt && this.turnStartedPromise) {
      await this.turnStartedPromise;
    }
    if (!this.activeTurnId) return { disposition: 'next-turn', reason: 'no-active-turn' };
    let expectedTurnId = this.activeTurnId;
    for (let attempt = 0; attempt < 2; attempt += 1) {
      try {
        const result = await app.request('turn/steer', {
          threadId: this.threadId,
          expectedTurnId,
          input: codexInput(blocks),
          ...(clientUserMessageId ? { clientUserMessageId } : {}),
        });
        this.activeTurnId = result.turnId;
        return { turnId: result.turnId, receipt: Boolean(clientUserMessageId) };
      } catch (error) {
        const nonSteerable = error.data?.codexErrorInfo?.activeTurnNotSteerable;
        if (nonSteerable) return { disposition: 'queue', reason: 'active-turn-not-steerable', turnKind: nonSteerable.turnKind };
        const actual = actualTurnFromMismatch(error.message);
        if (attempt === 0 && actual && actual !== expectedTurnId) {
          expectedTurnId = actual;
          this.activeTurnId = actual;
          continue;
        }
        if (/no active turn/i.test(error.message)) {
          this.activeTurnId = '';
          return { disposition: 'next-turn', reason: 'no-active-turn' };
        }
        throw error;
      }
    }
    return { disposition: 'next-turn', reason: 'no-active-turn' };
  }

  async interrupt() {
    const turnId = this.activeTurnId;
    if (!turnId) return;
    await app.request('turn/interrupt', { threadId: this.threadId, turnId });
  }

  setActiveTurn(turnId) {
    this.activeTurnId = turnId;
    this.resolveTurnStarted?.(turnId);
    this.resolveTurnStarted = null;
    this.rejectTurnStarted = null;
  }

  setConfig(configId, value) {
    const normalized = String(value || '').trim();
    if (configId === 'mode') {
      if (!modes.some((mode) => mode.value === normalized)) throw new Error(`Unsupported Codex mode: ${normalized}`);
      this.mode = normalized;
    } else if (configId === 'model') {
      const selected = modelCatalog.find((model) => model.id === normalized);
      if (!selected) throw new Error(`Unsupported Codex model: ${normalized}`);
      this.model = normalized;
      if (!(selected.supportedReasoningEfforts || []).some((option) => option.reasoningEffort === this.effort)) {
        this.effort = selected.defaultReasoningEffort;
      }
    } else if (configId === 'reasoning_effort') {
      if (!(this.modelRow()?.supportedReasoningEfforts || []).some((option) => option.reasoningEffort === normalized)) {
        throw new Error(`Unsupported Codex reasoning effort: ${normalized}`);
      }
      this.effort = normalized;
    } else throw new Error(`Unknown Codex config option: ${configId}`);
    const options = configOptions(this);
    notify(this.threadId, { sessionUpdate: 'config_options_update', configOptions: options });
    return { configOptions: options };
  }
}

function modeTurnParams(mode) {
  if (mode === 'read-only') return {
    approvalPolicy: 'on-request',
    sandboxPolicy: { type: 'readOnly', networkAccess: false },
  };
  if (mode === 'agent-full-access') return {
    approvalPolicy: 'never',
    sandboxPolicy: { type: 'dangerFullAccess' },
  };
  return {
    approvalPolicy: 'on-request',
    sandboxPolicy: { type: 'workspaceWrite', writableRoots: [], networkAccess: false, excludeTmpdirEnvVar: false, excludeSlashTmp: false },
  };
}

function actualTurnFromMismatch(message) {
  const match = String(message || '').match(/expected active turn id `[^`]+` but found `([^`]+)`/);
  return match?.[1] || '';
}

function codexInput(blocks) {
  const input = [];
  for (const block of Array.isArray(blocks) ? blocks : []) {
    if (block?.type === 'text') input.push({ type: 'text', text: String(block.text || ''), text_elements: [] });
    else if (block?.type === 'image' && block.data) {
      input.push({ type: 'image', url: `data:${block.mimeType || block.mime_type || 'image/png'};base64,${block.data}` });
    }
  }
  return input;
}

function sessionConfig(cwd, mcpServers) {
  const servers = {};
  for (const raw of Array.isArray(mcpServers) ? mcpServers : []) {
    if (!raw || typeof raw.name !== 'string') continue;
    const name = raw.name.replace(/\s/gu, '_');
    if (typeof raw.command !== 'string' || !raw.command.trim()) {
      throw new Error(`Codex MCP server ${raw.name} requires a stdio command`);
    }
    const env = Array.isArray(raw.env)
      ? Object.fromEntries(raw.env.map((entry) => [entry.name, entry.value]))
      : (raw.env || {});
    servers[name] = { command: raw.command, args: Array.isArray(raw.args) ? raw.args : [], env };
  }
  return {
    projects: { [cwd]: { trust_level: 'trusted' } },
    // Codex 0.147 ships MCP 2026-07-28 behind an explicit feature while
    // retaining the stateful lifecycle by default. The Workass stdio bridge
    // carries that stateless protocol to the daemon without mutating the
    // user's global Codex configuration.
    features: { mcp_2026_07_28: true },
    ...(Object.keys(servers).length ? { mcp_servers: servers } : {}),
  };
}

async function requireMCPToolCatalogs(threadId, config, afterStartupRevision) {
  const required = new Set(Object.keys(config?.mcp_servers || {}));
  if (!required.size) return;
  const configuredTimeout = Number.parseInt(String(process.env.WORKASS_CODEX_MCP_STARTUP_TIMEOUT_MS || ''), 10);
  const timeoutMs = Number.isFinite(configuredTimeout) && configuredTimeout > 0 ? configuredTimeout : 15_000;
  const deadline = Date.now() + timeoutMs;
  while (true) {
    const found = new Set();
    const cursors = new Set();
    let cursor = null;
    do {
      const response = await app.request('mcpServerStatus/list', {
        threadId,
        detail: 'toolsAndAuthOnly',
        cursor,
        limit: null,
      });
      for (const row of Array.isArray(response?.data) ? response.data : []) {
        const name = String(row?.name || '');
        if (!required.has(name)) continue;
        const tools = row?.tools && typeof row.tools === 'object' && !Array.isArray(row.tools)
          ? Object.keys(row.tools)
          : [];
        if (tools.length) found.add(name);
      }
      cursor = response?.nextCursor == null ? null : String(response.nextCursor);
      if (cursor) {
        if (cursors.has(cursor)) throw new Error('Codex MCP server status pagination repeated a cursor');
        cursors.add(cursor);
      }
    } while (cursor && found.size < required.size);

    const missing = [...required].filter((name) => !found.has(name));
    if (!missing.length) return;
    for (const name of missing) {
      const startup = app.mcpStartupStatus(threadId, name, afterStartupRevision);
      if (startup?.status === 'failed' || startup?.status === 'cancelled') {
        const reason = startup.error ? `: ${startup.error}` : '';
        throw new Error(`Codex MCP server ${name} startup ${startup.status}${reason}`);
      }
      if (startup?.status === 'ready') {
        throw new Error(`Codex MCP tool catalog is unavailable for: ${name}`);
      }
    }
    const remaining = deadline - Date.now();
    if (remaining <= 0) {
      throw new Error(`Codex MCP tool catalog is unavailable for: ${missing.join(', ')}`);
    }
    await app.waitForMCPStartupChange(threadId, missing, Math.min(100, remaining));
  }
}

async function openSession(params, resume) {
  await ensureInitialized();
  const cwd = String(params.cwd || process.cwd());
  const config = sessionConfig(cwd, params.mcpServers);
  const common = { cwd, config };
  const mcpStartupRevision = app.mcpStartupRevision;
  const requestedThreadId = String(params.sessionId || '').trim();
  if (resume && !requestedThreadId) {
    throw new Error('Codex session/resume requires the exact provider thread id');
  }
  let response;
  try {
    response = resume
      ? await app.request('thread/resume', { ...common, threadId: requestedThreadId })
      : await app.request('thread/start', common);
  } catch (error) {
    if (resume && /no rollout found for thread id/i.test(String(error?.message || ''))) {
      throw Object.assign(new Error('Codex provider candidate was never materialized'), { rpcCode: -32044 });
    }
    throw error;
  }
  const threadId = String(response?.thread?.id || requestedThreadId || '');
  if (!threadId) throw new Error('Codex app-server returned no thread id');
  if (resume && threadId !== requestedThreadId) {
    throw new Error('Codex session/resume returned a different provider thread id');
  }
  // A spawned stdio child is not proof that Codex discovered any tools from it.
  // Refuse to publish the provider session until the official app-server status
  // surface proves every configured MCP server has a nonempty tool catalog.
  await requireMCPToolCatalogs(threadId, config, mcpStartupRevision);
  let session = sessions.get(threadId);
  if (!session) {
    const defaultModel = modelCatalog.find((model) => model.isDefault) || modelCatalog[0];
    const selectedModel = modelCatalog.find((model) => model.id === response.model) || defaultModel;
    session = new CodexSession({
      threadId, cwd, mcpServers: params.mcpServers,
      model: selectedModel.id,
      effort: response.reasoningEffort || selectedModel.defaultReasoningEffort,
    });
    sessions.set(threadId, session);
  }
  return session;
}

async function handleAppRequest(message) {
  const params = message.params || {};
  const session = sessions.get(String(params.threadId || ''));
  if (message.method === 'item/commandExecution/requestApproval') {
    return approvalResponse(await requestApproval(session, {
      toolCallId: params.itemId, kind: 'execute', title: params.reason || params.command || 'Run command',
      rawInput: { command: params.command || '', cwd: params.cwd || '' },
    }), 'command', params);
  }
  if (message.method === 'item/fileChange/requestApproval') {
    return approvalResponse(await requestApproval(session, {
      toolCallId: params.itemId, kind: 'edit', title: params.reason || 'Edit files', rawInput: params,
    }), 'file', params);
  }
  if (message.method === 'item/permissions/requestApproval') {
    const option = await requestApproval(session, {
      toolCallId: params.itemId, kind: 'other', title: params.reason || 'Permissions request', rawInput: params,
    }, true);
    if (option === 'allow_permissions_session' || option === 'allow_always') {
      return { permissions: params.permissions || {}, scope: 'session', strictAutoReview: false };
    }
    if (option === 'allow_permissions_turn' || option === 'allow_once') {
      return { permissions: params.permissions || {}, scope: 'turn', strictAutoReview: false };
    }
    return { permissions: {}, scope: 'turn', strictAutoReview: true };
  }
  if (message.method === 'mcpServer/elicitation/request') return { action: 'cancel', content: null, _meta: null };
  if (message.method === 'item/tool/requestUserInput') return { answers: {} };
  throw new Error(`Unsupported Codex app-server client request: ${message.method}`);
}

async function requestApproval(session, toolCall, permissions = false) {
  if (!session) return 'reject_once';
  const options = permissions ? [
    { optionId: 'allow_permissions_session', name: 'Allow for session', kind: 'allow_always' },
    { optionId: 'allow_permissions_turn', name: 'Allow once', kind: 'allow_once' },
    { optionId: 'reject_permissions', name: 'Reject', kind: 'reject_once' },
  ] : [
    { optionId: 'allow_once', name: 'Allow once', kind: 'allow_once' },
    { optionId: 'allow_always', name: 'Allow for session', kind: 'allow_always' },
    { optionId: 'reject_once', name: 'Reject', kind: 'reject_once' },
  ];
  const result = await requestWorkass('session/request_permission', { sessionId: session.threadId, toolCall, options }, 0);
  return result?.outcome?.outcome === 'selected' ? result.outcome.optionId : 'cancel';
}

function approvalResponse(option, kind, params) {
  if (option === 'allow_once') return { decision: 'accept' };
  if (option === 'allow_always') return { decision: 'acceptForSession' };
  if (kind === 'command' && option === 'accept_execpolicy_amendment' && params.proposedExecpolicyAmendment) {
    return { decision: { acceptWithExecpolicyAmendment: { execpolicy_amendment: params.proposedExecpolicyAmendment } } };
  }
  return { decision: option === 'cancel' ? 'cancel' : 'decline' };
}

async function handleAppNotification(method, params) {
  const session = sessions.get(String(params.threadId || ''));
  if (!session) return;
  if (method === 'turn/started') {
    if (params.turn?.id) session.setActiveTurn(params.turn.id);
    session.turnStatus = params.turn?.status || 'inProgress';
    return;
  }
  if (method === 'turn/completed') { session.complete(params.turn || {}); return; }
  if (method === 'item/agentMessage/delta') {
    notify(session.threadId, {
      sessionUpdate: 'agent_message_chunk', content: { type: 'text', text: params.delta || '' },
      _meta: { codex: { phase: session.agentPhases.get(params.itemId) || undefined } },
    });
    return;
  }
  if (method === 'item/reasoning/summaryTextDelta' || method === 'item/reasoning/textDelta') {
    notify(session.threadId, { sessionUpdate: 'agent_thought_chunk', content: { type: 'text', text: params.delta || '' } });
    return;
  }
  if (method === 'item/reasoning/summaryPartAdded') {
    notify(session.threadId, { sessionUpdate: 'agent_thought_chunk', content: { type: 'text', text: '\n\n' } });
    return;
  }
  if (method === 'item/started') { await emitItem(session, params.item, false); return; }
  if (method === 'item/completed') { await emitItem(session, params.item, true); return; }
  if (method === 'item/commandExecution/outputDelta') {
    notify(session.threadId, { sessionUpdate: 'tool_call_update', toolCallId: params.itemId, content: { type: 'text', text: params.delta || '' } });
    return;
  }
  if (method === 'item/mcpToolCall/progress') {
    notify(session.threadId, { sessionUpdate: 'tool_call_update', toolCallId: params.itemId, content: { type: 'text', text: params.message || '' } });
    return;
  }
  if (method === 'turn/plan/updated') {
    notify(session.threadId, {
      sessionUpdate: 'plan', entries: (params.plan || []).map((step) => ({
        content: step.step, status: step.status === 'inProgress' ? 'in_progress' : step.status,
      })),
    });
    return;
  }
  if (method === 'thread/tokenUsage/updated') {
    session.lastUsage = params.tokenUsage?.last || null;
    const used = Number(params.tokenUsage?.last?.totalTokens || 0);
    const size = Number(params.tokenUsage?.modelContextWindow || 0);
    if (used > 0 && size > 0) notify(session.threadId, { sessionUpdate: 'usage_update', used, size });
    return;
  }
  if (method === 'error') {
    const error = new Error(params.error?.message || 'Codex turn failed');
    session.turnError = error;
    notify(session.threadId, { sessionUpdate: 'agent_message_chunk', content: { type: 'text', text: `${safeErrorText(error)}\n\n` } });
    return;
  }
  if (method === 'thread/compacted') {
	const checkpointId = String(params.threadId || session.threadId || '').trim();
	const digest = createHash('sha256').update(JSON.stringify({ checkpointId, compacted: params })).digest('hex');
	notify(session.threadId, { sessionUpdate: '_workass_compaction', phase: 'checkpoint', checkpointId, digest });
	return;
  }
}

async function emitItem(session, item, completed) {
  if (!item || typeof item !== 'object') return;
  if (item.type === 'agentMessage') { session.agentPhases.set(item.id, item.phase); return; }
  if (item.type === 'userMessage') {
	if (item.clientId && item.clientId === session.activePromptClientId) {
	  notify(session.threadId, { sessionUpdate: '_workass_input_consumed', clientUserMessageId: item.clientId });
	  session.activePromptClientId = '';
	} else if (item.clientId) {
	  notify(session.threadId, { sessionUpdate: '_workass_codex_steer_consumed', clientUserMessageId: item.clientId });
	}
	return;
  }
  if (item.type === 'reasoning' && completed && Array.isArray(item.summary) && item.summary.length) {
    notify(session.threadId, { sessionUpdate: 'agent_thought_chunk', content: { type: 'text', text: item.summary.join('\n\n') } });
    return;
  }
  const update = await itemUpdate(item, completed);
  if (update) notify(session.threadId, update);
}

async function itemUpdate(item, completed) {
  const base = {
    sessionUpdate: completed ? 'tool_call_update' : 'tool_call',
    toolCallId: item.id,
    status: toolStatus(item.status, completed),
  };
  if (item.type === 'commandExecution') return {
    ...base, kind: 'execute', title: item.command || 'Run command',
    rawInput: { command: item.command || '', cwd: item.cwd || '' },
    ...(completed && item.aggregatedOutput ? { content: { type: 'text', text: item.aggregatedOutput } } : {}),
  };
  if (item.type === 'fileChange') return {
    ...base, kind: 'edit', title: 'Editing files', rawInput: { changes: item.changes || [] },
    locations: (item.changes || []).map((change) => ({ path: change.path })),
    content: (item.changes || []).map((change) => ({ type: 'diff', path: change.path, text: change.diff || '' })),
  };
  if (item.type === 'mcpToolCall') return {
    ...base, kind: 'other', title: `mcp.${item.server}.${item.tool}`,
    rawInput: { server: item.server, tool: item.tool, arguments: item.arguments },
    ...(completed ? { content: normalizeCodexContent(item.result?.content), rawOutput: item.error || item.result } : {}),
    _meta: { is_mcp_tool_call: true },
  };
  if (item.type === 'dynamicToolCall') return {
    ...base, kind: 'other', title: item.tool, rawInput: { arguments: item.arguments },
    ...(completed ? { content: normalizeCodexContent(item.contentItems) } : {}),
  };
  if (item.type === 'webSearch') return {
    ...base, kind: 'search', title: item.query ? `Web search: ${item.query}` : 'Web search', rawInput: item,
  };
  if (item.type === 'collabAgentToolCall') return {
    ...base, kind: 'other', title: item.tool, rawInput: {
      prompt: item.prompt, model: item.model, reasoningEffort: item.reasoningEffort,
      senderThreadId: item.senderThreadId, receiverThreadIds: item.receiverThreadIds,
      agentsStates: item.agentsStates, status: item.status,
    },
  };
  if (item.type === 'imageView') {
    const image = await inlineRaster(item.path);
    return { ...base, kind: 'read', title: `View image ${item.path}`, rawInput: { path: item.path }, locations: [{ path: item.path }], ...(image ? { content: [image] } : {}) };
  }
  return null;
}

function toolStatus(status, completed) {
  if (status === 'inProgress') return 'in_progress';
  if (status === 'completed') return 'completed';
  if (status === 'failed' || status === 'declined') return 'failed';
  return completed ? 'completed' : 'in_progress';
}

function normalizeCodexContent(content) {
  const out = [];
  for (const block of Array.isArray(content) ? content : []) {
    if (block?.type === 'inputText' || block?.type === 'text') out.push({ type: 'text', text: block.text || '' });
    else if (block?.type === 'inputImage' && block.imageUrl?.startsWith('data:')) {
      const match = block.imageUrl.match(/^data:([^;,]+);base64,(.*)$/s);
      if (match) out.push({ type: 'image', mimeType: match[1], data: match[2] });
    } else if (block && typeof block === 'object') out.push(block);
  }
  return out;
}

async function inlineRaster(filePath) {
  let handle;
  try {
    handle = await open(filePath, 'r');
    const stat = await handle.stat();
    if (!stat.isFile() || stat.size <= 0 || stat.size > 6 * 1024 * 1024) return null;
    const data = Buffer.allocUnsafe(stat.size);
    let offset = 0;
    while (offset < data.length) {
      const { bytesRead } = await handle.read(data, offset, data.length - offset, offset);
      if (!bytesRead) return null;
      offset += bytesRead;
    }
    const mimeType = rasterMime(data);
    return mimeType ? { type: 'image', mimeType, data: data.toString('base64') } : null;
  } catch { return null; }
  finally { await handle?.close().catch(() => {}); }
}

function rasterMime(data) {
  if (data.length >= 8 && data.subarray(0, 8).equals(Buffer.from([137, 80, 78, 71, 13, 10, 26, 10]))) return 'image/png';
  if (data.length >= 3 && data[0] === 255 && data[1] === 216 && data[2] === 255) return 'image/jpeg';
  if (data.length >= 6 && ['GIF87a', 'GIF89a'].includes(data.toString('ascii', 0, 6))) return 'image/gif';
  if (data.length >= 12 && data.toString('ascii', 0, 4) === 'RIFF' && data.toString('ascii', 8, 12) === 'WEBP') return 'image/webp';
  return '';
}

async function handleWorkassRequest(message) {
  const { id, method, params = {} } = message;
  if (method === 'initialize') {
    await ensureInitialized();
    respond(id, {
      protocolVersion: Number(params.protocolVersion || 1),
      agentInfo: { name: 'Codex', version: 'official-app-server' },
      agentCapabilities: {
        sessionCapabilities: { resume: {}, close: {} },
        promptCapabilities: { image: true, audio: false, embeddedContext: false },
        // Workass MCP is served over private-CA HTTPS. The packaged stdio
        // bridge pins that CA; handing the URL directly to app-server makes
        // registration appear present while startup fails with zero tools.
        mcpCapabilities: { http: false, sse: false },
      },
      authMethods: [],
      _meta: {
        workassCodexSteerRequest: true, workassCodexSteerReceipt: true, workassCodexSteerRaceV1: true,
        workassCodexRateLimitsRequest: true, workassCodexRateLimitResetRequest: true,
		workassTurnReconcileRequest: true, workassStableTurnInputV1: true, workassOperationReadbackV1: true,
      },
    });
    return;
  }
  if (method === 'session/new' || method === 'session/resume') {
    const session = await openSession(params, method !== 'session/new');
    respond(id, {
      ...(method === 'session/new' ? { sessionId: session.threadId } : {}),
      configOptions: configOptions(session), availableModels: availableModels(),
      _meta: { workassProviderRealm: providerRealm },
    });
    return;
  }
  const session = sessions.get(String(params.sessionId || ''));
  if (!session && !method.startsWith('_workass/codex/rate-limit')) {
    throw Object.assign(new Error('Codex session not found'), { rpcCode: -32000 });
  }
  if (method === 'session/prompt') {
	respond(id, await session.startPrompt(params.prompt, String(params.clientUserMessageId || '').trim()));
	return;
	}
  if (method === 'session/set_config_option') { respond(id, session.setConfig(String(params.configId || ''), params.value)); return; }
  if (method === '_workass/codex/steer') { respond(id, await session.steer(params.prompt, String(params.clientUserMessageId || ''))); return; }
  if (method === '_workass/codex/rate-limits') { await ensureInitialized(); respond(id, await app.request('account/rateLimits/read', {})); return; }
  if (method === '_workass/codex/rate-limit-reset/consume') {
    await ensureInitialized();
    const outcome = await app.request('account/rateLimitResetCredit/consume', {
      idempotencyKey: params.idempotencyKey,
      ...(params.creditId ? { creditId: params.creditId } : {}),
    });
    const rateLimits = await app.request('account/rateLimits/read', {});
    respond(id, { ...outcome, rateLimits });
    return;
  }
  if (method === '_workass/turn/reconcile') {
	if (params.clientUserMessageId) {
	  respond(id, await session.reconcileOperation(params.clientUserMessageId));
	  return;
	}
    if (session.activeTurnId) {
      const response = await app.request('thread/read', { threadId: session.threadId, includeTurns: true });
      const turn = (response.thread?.turns || []).find((candidate) => candidate.id === session.activeTurnId);
      const status = turn?.status || session.turnStatus;
      const terminal = ['completed', 'failed', 'interrupted'].includes(status);
      if (terminal && turn) session.complete(turn);
      respond(id, { status, terminal, reconciled: terminal });
    } else respond(id, { status: session.turnStatus, terminal: ['completed', 'failed', 'interrupted'].includes(session.turnStatus), reconciled: false });
    return;
  }
  if (method === 'session/close') {
    if (session.activeTurnId) await session.interrupt().catch(() => {});
    await app.request('thread/unsubscribe', { threadId: session.threadId }).catch(() => {});
    sessions.delete(session.threadId);
    respond(id, {});
    return;
  }
  throw Object.assign(new Error(`Codex host method not found: ${method}`), { rpcCode: -32601 });
}

function handleWorkassNotification(message) {
  const session = sessions.get(String(message.params?.sessionId || ''));
  if (!session) return;
  if (message.method === 'session/cancel') void session.interrupt().catch((error) => session.failPrompt(error));
  if (message.method === '_session/steer') void session.steer(message.params?.prompt, '').catch((error) => diagnostic('Codex steer failed', error));
}

readline.createInterface({ input: process.stdin, crlfDelay: Infinity }).on('line', (line) => {
  let message;
  try { message = JSON.parse(line); } catch { diagnostic('Codex host received invalid JSON', 'parse failure'); return; }
  if (Object.hasOwn(message, 'id') && !message.method) {
    const pending = pendingWorkassRequests.get(String(message.id));
    if (pending) { pendingWorkassRequests.delete(String(message.id)); pending(message); }
    return;
  }
  if (!message?.method) return;
  if (!Object.hasOwn(message, 'id')) { handleWorkassNotification(message); return; }
  void handleWorkassRequest(message).catch((error) => fail(message.id, error.rpcCode || error.code || -32603, error, error.data));
}).on('close', () => app?.close());
