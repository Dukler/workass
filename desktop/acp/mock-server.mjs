#!/usr/bin/env node

import process from 'node:process';
import fs from 'node:fs';
import path from 'node:path';

const sessions = new Map();
let sessionSeq = 0;
let inputBuffer = '';
let clientRequestSeq = 0;
const pendingClientResponses = new Map();
const backgroundFDs = new Map();
const traceFile = process.env.WORKASS_MOCK_ACP_TRACE_FILE || '';
const persistentSessionFile = process.env.WORKASS_MOCK_ACP_SESSION_STORE || '';
const sessionCapability = String(process.env.WORKASS_MOCK_ACP_SESSION_CAPABILITY || (persistentSessionFile ? 'both' : 'none')).toLowerCase();
const failResume = process.env.WORKASS_MOCK_ACP_FAIL_RESUME === '1';
const mismatchedAttachmentId = String(process.env.WORKASS_MOCK_ACP_MISMATCHED_ATTACHMENT_ID || '').trim();
const operationReadback = process.env.WORKASS_MOCK_ACP_OPERATION_READBACK === '1';
const contextImport = process.env.WORKASS_MOCK_ACP_CONTEXT_IMPORT === '1';
const tinyPngBase64 = 'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAusB9Y9Z5m8AAAAASUVORK5CYII=';
// Test fixture for renderer plan-limit development only. This deliberately
// uses a mock marker alongside the Claude-shaped key so no paid adapter turn is
// needed to exercise daemon normalization/UI rendering.
const mockPlanUsageRateLimit = {
  status: 'allowed',
  resetsAt: 4102444800,
  rateLimitType: 'five_hour',
  overageStatus: 'allowed',
  isUsingOverage: false,
};

const delayMs = Math.max(0, Number(process.env.WORKASS_MOCK_ACP_DELAY_MS || 15));
const burstChunks = Math.max(1, Math.min(20000, Number(process.env.WORKASS_MOCK_ACP_BURST_CHUNKS || 4096)));
const burstChunkBytes = Math.max(32, Math.min(4096, Number(process.env.WORKASS_MOCK_ACP_BURST_CHUNK_BYTES || 128)));
const sleep = (ms = delayMs) => new Promise((resolve) => setTimeout(resolve, ms));

function loadPersistentSessions() {
  if (!persistentSessionFile) return;
  try {
    const raw = JSON.parse(fs.readFileSync(persistentSessionFile, 'utf8'));
    for (const session of Array.isArray(raw?.sessions) ? raw.sessions : []) {
      if (!session?.id) continue;
      const operations = session.operations && typeof session.operations === 'object' ? session.operations : {};
      // A persisted "active" operation belonged to the ACP process that just
      // disappeared. The replacement process can authoritatively read it back,
      // but it cannot keep executing that dead process's turn. Model the native
      // provider receipt as interrupted so Workass can settle the exact input
      // without replaying it or leaving a permanent spinner.
      for (const operation of Object.values(operations)) {
        if (operation && operation.status === 'active') operation.status = 'interrupted';
      }
      sessions.set(String(session.id), {
        id: String(session.id), cwd: String(session.cwd || process.cwd()),
        model: String(session.model || 'mock-deterministic'), mode: String(session.mode || 'ask'),
        turn: Math.max(0, Number(session.turn || 0)), cancelled: false, steers: [],
        turnStatus: 'idle', pendingPromptId: null, reconcileRelease: true,
		operations,
		contextImports: session.contextImports && typeof session.contextImports === 'object' ? session.contextImports : {},
		activeOperationId: '',
      });
    }
  } catch {
    // Missing/corrupt fixture state behaves like an unavailable provider thread.
  }
}

function persistSessions() {
  if (!persistentSessionFile) return;
  try {
    const tmp = `${persistentSessionFile}.${process.pid}.tmp`;
    fs.mkdirSync(path.dirname(persistentSessionFile), { recursive: true });
    fs.writeFileSync(tmp, JSON.stringify({
      v: 1,
      sessions: [...sessions.values()].map(({ cancelled, steers, turnStatus, pendingPromptId, reconcileRelease, ...session }) => session),
    }), { mode: 0o600 });
    fs.renameSync(tmp, persistentSessionFile);
  } catch (error) {
    process.stderr.write(`mock persistent session write failed: ${error?.message || error}\n`);
  }
}

loadPersistentSessions();

function write(message) {
  process.stdout.write(`${JSON.stringify(message)}\n`);
}

function respond(id, result) {
  write({ jsonrpc: '2.0', id, result });
}

function fail(id, code, message) {
  write({ jsonrpc: '2.0', id, error: { code, message } });
}

function notify(sessionId, update) {
  write({ jsonrpc: '2.0', method: 'session/update', params: { sessionId, update } });
}

function tracePrompt(sessionId, text) {
  if (!traceFile) return;
  try {
    fs.appendFileSync(traceFile, `${JSON.stringify({ pid: process.pid, sessionId, text })}\n`);
  } catch {
    // Diagnostics must not touch stdout; tracing is best-effort test support.
  }
}

function requestClient(method, params, timeoutMs = 30000) {
  const id = `mock-client-${++clientRequestSeq}`;
  write({ jsonrpc: '2.0', id, method, params });
  return new Promise((resolve) => {
    const timer = setTimeout(() => {
      pendingClientResponses.delete(id);
      resolve(null);
    }, timeoutMs);
    pendingClientResponses.set(id, (result) => {
      clearTimeout(timer);
      resolve(result || null);
    });
  });
}

function configOptions(session) {
  return [
    {
      id: 'model', category: 'model', name: 'Model', type: 'select', currentValue: session.model,
      options: [
        { value: 'mock-deterministic[low]', name: 'Mock deterministic (low)' },
        { value: 'mock-deterministic[high]', name: 'Mock deterministic (high)' },
      ],
    },
    {
      id: 'mode', category: 'mode', name: 'Mode', type: 'select', currentValue: session.mode,
      options: [
        { value: 'ask', name: 'Ask' },
        { value: 'bypass', name: 'Bypass' },
      ],
    },
  ];
}

function promptText(prompt) {
  if (!Array.isArray(prompt)) return '';
  return prompt.filter((block) => block && block.type === 'text').map((block) => block.text || '').join('\n').trim();
}

function drainSteers(session) {
  const steers = Array.isArray(session.steers) ? session.steers.splice(0) : [];
  return steers.map((text) => String(text || '').trim()).filter(Boolean);
}

// Test-only lifecycle barrier. A prompt containing this marker remains active
// until the generic ACP steer notification arrives (or the optional one-shot
// release file exists), giving live-delivery tests an explicit protocol
// boundary instead of depending on [mock:slow] timing.
function releaseHeldPrompt(session) {
  const release = session.releaseHeldPrompt;
  session.releaseHeldPrompt = null;
  if (typeof release === 'function') release();
}

function holdPromptUntilSteer(session) {
  const releaseFile = String(process.env.WORKASS_MOCK_ACP_HOLD_FILE || '').trim();
  if (releaseFile && fs.existsSync(releaseFile)) return Promise.resolve();
  return new Promise((resolve) => {
    let settled = false;
    let watcher = null;
    const finish = () => {
      if (settled) return;
      settled = true;
      if (watcher) watcher.close();
      session.releaseHeldPrompt = null;
      resolve();
    };
    session.releaseHeldPrompt = finish;
    if (!releaseFile) return;
    try {
      watcher = fs.watch(path.dirname(releaseFile), (_eventType, filename) => {
        if (String(filename || '') === path.basename(releaseFile) && fs.existsSync(releaseFile)) finish();
      });
      watcher.on('error', () => {});
    } catch {
      // The steer notification remains the primary release path. Tests use an
      // existing temporary directory, so a watcher failure is not expected.
    }
  });
}

function completeOperation(session, status) {
  const operationId = String(session.activeOperationId || '').trim();
  if (!operationId || !session.operations?.[operationId]) return;
  session.operations[operationId].status = status;
  session.activeOperationId = '';
  persistSessions();
}

function usageMeta(inputTokens, outputTokens) {
  return {
    'workass.mock/inputTokens': inputTokens,
    'workass.mock/outputTokens': outputTokens,
    'workass.mock/planUsageFixture': true,
    '_claude/rateLimit': mockPlanUsageRateLimit,
  };
}

function burstChunk(index) {
  const prefix = `Burst ${String(index + 1).padStart(5, '0')} `;
  const suffix = index % 16 === 15 ? '\n\n' : ' ';
  return (prefix + 'render cadence '.repeat(Math.ceil(burstChunkBytes / 15))).slice(0, burstChunkBytes - suffix.length) + suffix;
}

async function runPrompt(id, params) {
  const session = sessions.get(params.sessionId);
  if (!session) {
    fail(id, -32000, 'Unknown mock ACP session.');
    return;
  }

  const text = promptText(params.prompt);
  tracePrompt(session.id, text);
  session.turnStatus = 'active';
  session.pendingPromptId = null;
  session.reconcileRelease = !text.includes('[mock:lost-terminal-unreleased]');
	const operationId = String(params.clientUserMessageId || '').trim();
	if (operationReadback && operationId) {
		const turnId = `mock-native-turn-${session.turn + 1}`;
		session.operations ||= {};
		session.operations[operationId] = { turnId, status: 'active', consumed: true };
		session.activeOperationId = operationId;
		persistSessions();
		notify(session.id, { sessionUpdate: '_workass_input_consumed', clientUserMessageId: operationId, turnId });
	}
  if (text.includes('[mock:error]')) {
    session.turnStatus = 'failed';
	completeOperation(session, 'failed');
    fail(id, -32001, 'Deterministic mock failure.');
    return;
  }
  if (text.includes('[mock:crash]')) {
    notify(session.id, {
      sessionUpdate: 'agent_thought_chunk',
      content: { type: 'text', text: 'Mock crash marker reached.' },
    });
    await sleep(Math.max(delayMs, 20));
    process.stderr.write('mock crash marker exiting mid-turn\n');
    process.exit(44);
  }
  const pause = () => sleep(text.includes('[mock:slow]') ? Math.max(delayMs, 250) : delayMs);
  const toolCallId = `mock-tool-${session.turn + 1}`;
  session.turn += 1;
  session.cancelled = false;
  persistSessions();

  if (text.includes('[mock:hold-until-steer]')) {
    await holdPromptUntilSteer(session);
  }

  if (text.includes('[mock:burst]')) {
    // Deliberately emit far faster than a display can paint. A zero-delay timer
    // between chunks gives the daemon's 16 ms coalescer a chance to flush while
    // still driving roughly 1,000 ACP notifications/second. The renderer should
    // therefore receive frame-sized batches, never one React update per token.
    for (let i = 0; i < burstChunks && !session.cancelled; i++) {
      notify(session.id, {
        sessionUpdate: 'agent_message_chunk',
        content: { type: 'text', text: burstChunk(i) },
      });
      await sleep(0);
    }
    notify(session.id, {
      sessionUpdate: 'usage_update',
      used: burstChunks * burstChunkBytes,
      size: burstChunks * burstChunkBytes * 2,
      _meta: usageMeta(Math.max(1, Math.ceil(text.length / 4)), burstChunks * burstChunkBytes),
    });
    session.turnStatus = session.cancelled ? 'interrupted' : 'completed';
	completeOperation(session, session.turnStatus);
    respond(id, { stopReason: session.cancelled ? 'cancelled' : 'end_turn' });
    return;
  }

  if (text.includes('[mock:spawned-work]') || text.includes('[mock:spawned-work-running]')) {
    const taskId = `mock-bg-${session.turn}`;
    const outputFile = path.join(process.env.TMPDIR || '/tmp', 'claude-mock', session.id, 'tasks', `${taskId}.output`);
    fs.mkdirSync(path.dirname(outputFile), { recursive: true, mode: 0o700 });
    fs.writeFileSync(outputFile, 'mock background work started\n', { mode: 0o600 });
    const fd = fs.openSync(outputFile, 'a');
    backgroundFDs.set(taskId, fd);
    notify(session.id, {
      sessionUpdate: 'tool_call', toolCallId, title: 'Bash', kind: 'execute', status: 'in_progress',
      rawInput: { command: 'node mock-background-helper.mjs', run_in_background: true },
      _meta: { claudeCode: { toolName: 'Bash' } },
    });
    notify(session.id, {
      sessionUpdate: 'tool_call_update', toolCallId, title: 'Bash', kind: 'execute', status: 'completed',
      content: { type: 'text', text: `Command running in background with ID: ${taskId}. Output is being written to: ${outputFile}` },
      _meta: { claudeCode: { toolName: 'Bash' } },
    });
    notify(session.id, {
      sessionUpdate: '_workass_claude_spawned_work',
      event: { type: 'started', taskId, toolCallId, description: 'Mock background helper', taskType: 'bash' },
    });
    notify(session.id, {
      sessionUpdate: '_workass_claude_spawned_work',
      event: { type: 'snapshot', tasks: [{ taskId, taskType: 'bash', description: 'Mock background helper' }] },
    });
    if (!text.includes('[mock:spawned-work-running]')) {
      await pause();
      fs.writeSync(fd, 'mock background work completed\n');
      fs.closeSync(fd);
      backgroundFDs.delete(taskId);
      notify(session.id, {
        sessionUpdate: '_workass_claude_spawned_work',
        event: { type: 'notification', taskId, toolCallId, status: 'completed', outputFile, summary: 'Mock background helper completed' },
      });
      notify(session.id, { sessionUpdate: '_workass_claude_spawned_work', event: { type: 'snapshot', tasks: [] } });
    }
    await pause();
  }

  notify(session.id, {
    sessionUpdate: 'plan',
    entries: [
      { content: 'Inspect the deterministic fixture', status: 'in_progress' },
      { content: 'Return a streamed response', status: 'pending' },
    ],
  });
  await pause();

  notify(session.id, {
    sessionUpdate: 'agent_thought_chunk',
    content: { type: 'text', text: 'Running the deterministic ACP path.' },
  });
  await pause();

  notify(session.id, {
    sessionUpdate: 'tool_call',
    toolCallId,
    title: 'Read mock workspace state',
    kind: 'read',
    status: 'in_progress',
    rawInput: { path: 'mock://workspace' },
  });
  await pause();

  notify(session.id, {
    sessionUpdate: 'tool_call_update',
    toolCallId,
    title: 'Read mock workspace state',
    kind: 'read',
    // A single failed tool call, for the failed-row treatment (neutral title,
    // red command/path, no ✕). The turn itself still completes normally. The
    // location gives the failed row a visible path so the red cue shows.
    status: text.includes('[mock:tool-fail]') ? 'failed' : 'completed',
    ...(text.includes('[mock:tool-fail]') ? { locations: [{ path: 'docs/game-streaming.md' }] } : {}),
    content: text.includes('[mock:tool-image]') ? [
      { type: 'text', text: 'deterministic fixture ready' },
      {
        type: 'image',
        mimeType: 'image/png',
        name: 'Deterministic tool image',
        // Valid 1×1 PNG. This fixture asserts transport/rendering, never model quality.
        data: tinyPngBase64,
      },
    ] : { type: 'text', text: 'deterministic fixture ready' },
  });
  await pause();

  let permissionText = '';
  if (text.includes('[mock:permission]')) {
    const result = await requestClient('session/request_permission', {
      sessionId: session.id,
      toolCall: { title: 'Mock permission gate', kind: 'execute' },
      options: [
        { optionId: 'allow-once', name: 'Allow once', kind: 'allow_once' },
        { optionId: 'reject', name: 'Reject', kind: 'reject_once' },
      ],
    });
    const outcome = result?.outcome?.outcome || 'cancelled';
    const optionId = result?.outcome?.optionId || '';
    permissionText = outcome === 'selected' ? ` Permission outcome: selected ${optionId}.` : ' Permission outcome: cancelled.';
    await pause();
  }

  const steerText = drainSteers(session);
  const steerSuffix = steerText.length ? ` Steer input: ${steerText.join(' | ')}.` : '';
  let responseText = `${text || 'empty prompt received'}${permissionText}${steerSuffix}`;
  if (text.includes('[mock:assistant-image]')) {
    const imagePath = path.join(session.cwd, `.workass-mock-assistant-${session.turn}.png`);
    fs.mkdirSync(path.dirname(imagePath), { recursive: true, mode: 0o700 });
    fs.writeFileSync(imagePath, Buffer.from(tinyPngBase64, 'base64'), { mode: 0o600 });
    // This is the ordinary Markdown shape emitted by real ACP agents. Workass,
    // not the fixture/provider, imports it into durable transcript media.
    responseText = `[Open deterministic preview](<${imagePath}>)\n![Deterministic preview](<${imagePath}>)`;
  }

  if (!session.cancelled) {
    const prefix = `Mock ACP turn ${session.turn}: `;
    const typedPhases = text.includes('[mock:phases]');
    notify(session.id, {
      sessionUpdate: 'agent_message_chunk',
      content: { type: 'text', text: prefix },
      ...(typedPhases ? { _meta: { workassAssistantPhase: 'commentary' } } : {}),
    });
    await pause();
    notify(session.id, {
      sessionUpdate: 'agent_message_chunk',
      content: { type: 'text', text: responseText },
      ...(typedPhases ? { _meta: { workassAssistantPhase: 'final_answer' } } : {}),
    });
    notify(session.id, {
      sessionUpdate: 'usage_update',
      used: text.includes('[mock:bigusage]') ? 85 : Math.max(8, text.length),
      size: text.includes('[mock:bigusage]') ? 100 : 8192,
      _meta: usageMeta(Math.max(1, Math.ceil(text.length / 4)), 8),
    });
    notify(session.id, {
      sessionUpdate: 'plan',
      entries: [
        { content: 'Inspect the deterministic fixture', status: 'completed' },
        { content: 'Return a streamed response', status: 'completed' },
      ],
    });
  }

  // Reproduce a provider-native turn that completed and streamed every visible
  // update, while the ACP adapter lost the terminal session/prompt response.
  // Workass must reconcile this state instead of leaving the chat running
  // forever. The recovery extension is intentionally added by the fix, not by
	// this failing fixture step.
	session.turnStatus = session.cancelled ? 'interrupted' : 'completed';
	if (text.includes('[mock:active-without-terminal]')) {
		session.turnStatus = 'active';
		if (session.activeOperationId && session.operations?.[session.activeOperationId]) {
			session.operations[session.activeOperationId].status = 'active';
			persistSessions();
		}
		session.pendingPromptId = id;
		return;
	}
	completeOperation(session, session.turnStatus);
  if (text.includes('[mock:lost-terminal')) {
    session.pendingPromptId = id;
    return;
  }

  respond(id, { stopReason: session.cancelled ? 'cancelled' : 'end_turn' });
}

async function handleRequest(message) {
  const { id, method, params = {} } = message;
  if (method === 'initialize') {
    respond(id, {
      protocolVersion: Number(params.protocolVersion || 1),
      agentInfo: { name: 'Workass Mock ACP', version: '0.1.0' },
      agentCapabilities: {
        sessionCapabilities: sessionCapability === 'both' || sessionCapability === 'resume' ? { resume: {}, close: {} } : { close: {} },
        loadSession: sessionCapability === 'both' || sessionCapability === 'load',
        sessionSteer: true,
        steerNotification: true,
        promptCapabilities: { image: false, audio: false, embeddedContext: false },
        mcpCapabilities: { http: false, sse: false },
      },
      authMethods: [],
      _meta: {
        deterministic: true,
        sessionSteer: true,
        steerNotification: true,
        workassTurnReconcileRequest: true,
		...(operationReadback ? { workassStableTurnInputV1: true, workassOperationReadbackV1: true } : {}),
		...(contextImport ? { workassContextImportV1: {
		  mode: 'non_sampling', receipt: 'operation_readback_v1', idempotent: true,
		  maxEvents: 64, maxBytes: 1048576,
		} } : {}),
      },
    });
    return;
  }
  if (method === 'session/new') {
    const session = {
      id: `mock-session-${process.pid}-${++sessionSeq}`,
      cwd: params.cwd || process.cwd(),
      model: 'mock-deterministic',
      mode: 'ask',
      turn: 0,
      cancelled: false,
      steers: [],
      turnStatus: 'idle',
      pendingPromptId: null,
      reconcileRelease: true,
	  operations: {},
	  contextImports: {},
	  activeOperationId: '',
    };
    sessions.set(session.id, session);
    persistSessions();
    respond(id, { sessionId: session.id, configOptions: configOptions(session) });
    return;
  }
  if (method === 'session/resume' || method === 'session/load') {
    const allowed = method === 'session/resume'
      ? sessionCapability === 'both' || sessionCapability === 'resume'
      : sessionCapability === 'both' || sessionCapability === 'load';
    if (!allowed) return fail(id, -32601, `Mock ACP method not enabled: ${method}`);
    tracePrompt(String(params.sessionId || ''), `[mock:lifecycle] ${method}`);
    if (method === 'session/resume' && failResume) return fail(id, -32098, 'Mock exact resume failure.');
    const session = sessions.get(params.sessionId);
    if (!session) return fail(id, -32044, 'Unknown persistent mock ACP session.');
    session.cwd = params.cwd || session.cwd;
    session.cancelled = false;
    session.steers = [];
    session.turnStatus = 'idle';
    session.pendingPromptId = null;
    session.reconcileRelease = true;
    persistSessions();
    if (method === 'session/load') {
      // Stable ACP v1 load replays history through session/update. Workass must
      // discard these projection events while attaching the already-owned lane.
      notify(session.id, {
        sessionUpdate: 'agent_message_chunk',
        content: { type: 'text', text: `[mock:loaded-history] turn=${session.turn}` },
      });
    }
    respond(id, {
      configOptions: configOptions(session),
      ...(mismatchedAttachmentId ? { sessionId: mismatchedAttachmentId } : {}),
    });
    return;
  }
  if (method === 'session/set_config_option') {
    const session = sessions.get(params.sessionId);
    if (!session) return fail(id, -32000, 'Unknown mock ACP session.');
    if (params.configId === 'model') session.model = String(params.value);
    if (params.configId === 'mode') session.mode = String(params.value);
    persistSessions();
    respond(id, { configOptions: configOptions(session) });
    return;
  }
  if (method === 'session/prompt') {
    await runPrompt(id, params);
    return;
  }
  if (method === '_workass/turn/reconcile') {
    const session = sessions.get(params.sessionId);
    if (!session) return fail(id, -32000, 'Unknown mock ACP session.');
	if (params.clientUserMessageId) {
	  if (!operationReadback) return fail(id, -32601, 'Mock operation readback is not enabled.');
	  const operation = session.operations?.[String(params.clientUserMessageId)] || null;
	  if (!operation) {
		respond(id, { found: false, consumed: false, status: 'absent', terminal: false });
		return;
	  }
	  const terminal = ['completed', 'failed', 'interrupted'].includes(operation.status);
	  respond(id, { found: true, consumed: Boolean(operation.consumed), turnId: operation.turnId, status: operation.status, terminal });
	  return;
	}
    const status = String(session.turnStatus || 'unknown');
    const terminal = status === 'completed' || status === 'failed' || status === 'interrupted';
    let reconciled = false;
    if (terminal && session.pendingPromptId !== null && session.reconcileRelease !== false) {
      const promptId = session.pendingPromptId;
      session.pendingPromptId = null;
      respond(promptId, { stopReason: status === 'completed' ? 'end_turn' : 'cancelled' });
      reconciled = true;
    }
    respond(id, { status, terminal, reconciled });
    return;
  }
  if (method === '_workass/context/import') {
	if (!contextImport) return fail(id, -32601, 'Mock context import is not enabled.');
	const session = sessions.get(params.sessionId);
	if (!session) return fail(id, -32000, 'Unknown mock ACP session.');
	const operationId = String(params.operationId || '');
	const digest = String(params.digest || '');
	const from = Number(params.from);
	const to = Number(params.to);
	if (!operationId || !digest || !Number.isSafeInteger(from) || !Number.isSafeInteger(to) || from < 0 || to <= from || !Array.isArray(params.messages)) {
	  return fail(id, -32602, 'Invalid context import identity.');
	}
	const previous = session.contextImports?.[operationId];
	if (previous && (previous.digest !== digest || previous.from !== from || previous.to !== to)) {
	  return fail(id, -32000, 'Context import operation identity conflict.');
	}
	session.contextImports ??= {};
	session.contextImports[operationId] = { operationId, digest, from, to, confirmed: true };
	persistSessions();
	respond(id, { operationId, digest, from, to, found: true, confirmed: true, ambiguous: false });
	return;
  }
  if (method === '_workass/context/import/readback') {
	if (!contextImport) return fail(id, -32601, 'Mock context import is not enabled.');
	const session = sessions.get(params.sessionId);
	if (!session) return fail(id, -32000, 'Unknown mock ACP session.');
	const operationId = String(params.operationId || '');
	const digest = String(params.digest || '');
	const from = Number(params.from);
	const to = Number(params.to);
	const previous = session.contextImports?.[operationId];
	if (!previous) {
	  respond(id, { operationId, digest, from, to, found: false, confirmed: false, ambiguous: false });
	  return;
	}
	if (previous.digest !== digest || previous.from !== from || previous.to !== to) {
	  return fail(id, -32000, 'Context import readback identity conflict.');
	}
	respond(id, { operationId, digest, from, to, found: true, confirmed: true, ambiguous: false });
	return;
  }
  if (method === 'session/close') {
    // session/close releases this adapter's live attachment; it must not delete
    // the durable provider thread that exact session attachment will reopen.
    if (!persistentSessionFile) sessions.delete(params.sessionId);
    respond(id, {});
    return;
  }
  fail(id, -32601, `Mock ACP method not found: ${method}`);
}

function handleNotification(message) {
  if (message.method === 'session/cancel') {
    const session = sessions.get(message.params?.sessionId);
    if (session) {
      session.cancelled = true;
      session.turnStatus = 'interrupted';
	  completeOperation(session, 'interrupted');
      releaseHeldPrompt(session);
      if (session.pendingPromptId !== null) {
        const promptId = session.pendingPromptId;
        session.pendingPromptId = null;
        respond(promptId, { stopReason: 'cancelled' });
      }
    }
  }
  if (message.method === '_session/steer') {
    const session = sessions.get(message.params?.sessionId);
    if (session) {
      session.steers.push(promptText(message.params?.prompt));
      releaseHeldPrompt(session);
    }
  }
}

function acceptLine(line) {
  let message;
  try { message = JSON.parse(line); }
  catch { return; }
  if (!message || message.jsonrpc !== '2.0') return;
  if (Object.prototype.hasOwnProperty.call(message, 'id') && !message.method) {
    const finish = pendingClientResponses.get(message.id);
    if (finish) {
      pendingClientResponses.delete(message.id);
      finish(message.result);
    }
    return;
  }
  if (!message.method) return;
  if (!Object.prototype.hasOwnProperty.call(message, 'id')) {
    handleNotification(message);
    return;
  }
  // The real adapters service the capability-gated liveness request while an
  // async session/prompt handler is still pending. Keep the oracle concurrent
  // at this one boundary so a long permission or tool wait can authoritatively
  // report "active" instead of looking like a dead adapter.
  if (message.method === '_workass/turn/reconcile') {
    void handleRequest(message).catch((err) => process.stderr.write(`${err.stack || err}\n`));
    return;
  }
  lineQueue = lineQueue.then(() => handleRequest(message)).catch((err) => process.stderr.write(`${err.stack || err}\n`));
}

let lineQueue = Promise.resolve();
process.stdin.setEncoding('utf8');
process.stdin.on('data', (chunk) => {
  inputBuffer += chunk;
  let newline;
  while ((newline = inputBuffer.indexOf('\n')) !== -1) {
    const line = inputBuffer.slice(0, newline).trim();
    inputBuffer = inputBuffer.slice(newline + 1);
    if (line) acceptLine(line);
  }
});
