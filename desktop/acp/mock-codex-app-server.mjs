import readline from 'node:readline';
import { createHash } from 'node:crypto';
import { appendFileSync, existsSync, readFileSync, writeFileSync } from 'node:fs';

const pending = new Map();
let requestSequence = 0;
let turnSequence = 0;
let activeTurn = null;
let activeTurnScenario = '';
let rapidSteerSequence = 0;
let pendingNativeChild = null;
const turnRecords = [];
if (process.env.WORKASS_CODEX_FIXTURE_LARGE_RESUME === '1') {
  for (let index = 0; index < 512; index++) {
    turnRecords.push({ id: `history-${index}`, status: 'completed', items: [
      { type: 'agentMessage', id: `history-item-${index}`, text: 'historical-fixture-only '.padEnd(8192, 'x') },
    ] });
  }
}
const threadMCPConfigs = new Map();
const threadMCPStartup = new Map();
const fixtureThreadId = String(process.env.WORKASS_CODEX_FIXTURE_THREAD_ID || 'fixture-codex-thread');
const goalStore = process.env.WORKASS_CODEX_FIXTURE_GOAL_STORE;
let goal = goalStore && existsSync(goalStore) ? JSON.parse(readFileSync(goalStore, 'utf8')) : null;
let goalSettings = null;
let goalContext = null;

function updateGoalFixture(status) {
  if (goal) goal = { ...goal, status, tokensUsed: goal.tokensUsed + 10, updatedAt: goal.updatedAt + 1 };
  if (goalStore) writeFileSync(goalStore, JSON.stringify(goal));
  notify(goal ? 'thread/goal/updated' : 'thread/goal/cleared', { threadId: fixtureThreadId, ...(goal ? { goal } : {}) });
}

function runGoalFixture() {
  const start = () => {
    activeTurn = `fixture-goal-${++turnSequence}`;
    notify('turn/started', { threadId: fixtureThreadId, turn: { id: activeTurn, status: 'inProgress' } });
    notify('item/agentMessage/delta', { threadId: fixtureThreadId, turnId: activeTurn, itemId: activeTurn, delta: 'Native goal checkpoint.\n' });
  };
  start();
  completeTurn(activeTurn);
  setTimeout(() => {
    if (goal?.status !== 'active') return;
    start();
    if (goal.objective.includes('[fixture:goal-hold]')) return;
    const terminal = /\[fixture:goal-(blocked|usageLimited|budgetLimited)\]/.exec(goal.objective)?.[1] || 'complete';
    updateGoalFixture(terminal);
    completeTurn(activeTurn);
  }, 25);
}

function write(message) { process.stdout.write(`${JSON.stringify(message)}\n`); }
function respond(id, result) {
  if (process.env.WORKASS_CODEX_FIXTURE_DIAGNOSTIC_TRACE && result?.thread) {
    appendFileSync(process.env.WORKASS_CODEX_FIXTURE_DIAGNOSTIC_TRACE, `${JSON.stringify({ replyBytes: Buffer.byteLength(JSON.stringify({ id, result })) })}\n`);
  }
  write({ id, result });
}
function notify(method, params) { write({ method, params }); }
function request(method, params) {
  const id = `fixture-app-${++requestSequence}`;
  write({ id, method, params });
  return new Promise((resolve) => pending.set(String(id), resolve));
}

const model = {
  id: 'gpt-fixture', model: 'gpt-fixture', displayName: 'GPT Fixture', description: 'Fixture model',
  hidden: false, isDefault: true, defaultReasoningEffort: 'high', inputModalities: ['text', 'image'],
  serviceTiers: [{ id: 'priority', name: 'Fast', description: 'Increased usage' }],
  supportedReasoningEfforts: [
    { reasoningEffort: 'low', description: 'Low' },
    { reasoningEffort: 'high', description: 'High' },
  ],
};
const secondaryModel = {
  id: 'gpt-fixture-mini', model: 'gpt-fixture-mini', displayName: 'GPT Fixture Mini', description: 'Secondary fixture model',
  hidden: false, isDefault: false, defaultReasoningEffort: 'low', inputModalities: ['text'],
  supportedReasoningEfforts: [{ reasoningEffort: 'low', description: 'Low' }],
};

function completeTurn(turnId, status = 'completed', error) {
  activeTurn = null;
	activeTurnScenario = '';
	rapidSteerSequence = 0;
	const record = turnRecords.find((turn) => turn.id === turnId);
	if (record) record.status = status;
  notify('turn/completed', {
    threadId: fixtureThreadId,
    turn: { id: turnId, status, items: [], ...(error ? { error } : {}) },
  });
}

function startMCPFixtures(threadId, configured) {
  const startup = new Map();
  threadMCPStartup.set(threadId, startup);
  const delayed = Number.parseInt(String(process.env.WORKASS_CODEX_FIXTURE_MCP_DELAY_MS || ''), 10);
  for (const name of Object.keys(configured || {})) {
    if (process.env.WORKASS_CODEX_FIXTURE_MCP_FAILED === '1') {
      startup.set(name, 'failed');
      notify('mcpServer/startupStatus/updated', {
        threadId, name, status: 'failed', error: 'fixture MCP startup failed',
      });
      continue;
    }
    if (Number.isFinite(delayed) && delayed > 0) {
      startup.set(name, 'starting');
      notify('mcpServer/startupStatus/updated', { threadId, name, status: 'starting' });
      setTimeout(() => {
        startup.set(name, 'ready');
        notify('mcpServer/startupStatus/updated', { threadId, name, status: 'ready' });
      }, delayed);
      continue;
    }
    startup.set(name, 'ready');
    notify('mcpServer/startupStatus/updated', { threadId, name, status: 'ready' });
  }
}

async function runTurn(id, params) {
  if ((params.input || []).some((item) => item.text?.includes('[fixture:goal-'))) {
    return write({ id, error: { code: -32602, message: 'native goal command incorrectly sent to turn/start' } });
  }
  const expectedTier = (params.input || []).map((item) => item.text || '').join('\n').match(/\[fixture:speed:(fast|default)\]/)?.[1];
  if (expectedTier && (params.serviceTierForTurn !== (expectedTier === 'fast' && process.env.WORKASS_CODEX_FIXTURE_SPEED_CATALOG !== 'legacy' ? 'priority' : expectedTier) || params.effort !== 'high')) {
    return write({ id, error: { code: -32602, message: 'fixture speed/effort mismatch' } });
  }

  const turnId = `fixture-turn-${++turnSequence}`;
	const turnRecord = { id: turnId, status: 'inProgress', items: [] };
	turnRecords.push(turnRecord);
  activeTurn = turnId;
  const diagnosticScenario = (params.input || []).some((item) => item.text?.includes('[fixture:diagnostic'));
  const diagnosticUsage = { last: { totalTokens: 123, inputTokens: 100, cachedInputTokens: 20, outputTokens: 23, reasoningOutputTokens: 7 }, modelContextWindow: 1000 };
  if (diagnosticScenario) {
    // Native events can precede the response. Only its exact turn may survive admission.
    notify('error', { threadId: params.threadId, turnId: 'wrong-early-turn', willRetry: true, error: { codexErrorInfo: 'unauthorized' } });
    notify('thread/tokenUsage/updated', { threadId: params.threadId, turnId, tokenUsage: diagnosticUsage });
  }
  respond(id, { turn: { id: turnId, status: 'inProgress', items: [] } });
  if (diagnosticScenario && turnSequence > 1) {
    const stale = `fixture-turn-${turnSequence - 1}`;
    notify('turn/started', { threadId: params.threadId, turn: { id: stale, status: 'inProgress' } });
    for (const [threadId, staleTurn] of [[params.threadId, stale], ['wrong-thread', turnId], [params.threadId, 'wrong-current-turn']]) {
      notify('thread/tokenUsage/updated', { threadId, turnId: staleTurn, tokenUsage: { last: { totalTokens: 999 }, modelContextWindow: 999 } });
      notify('error', { threadId, turnId: staleTurn, willRetry: true, error: { codexErrorInfo: 'unauthorized' } });
      notify('item/completed', { threadId, turnId: staleTurn, item: { type: 'contextCompaction', id: 'stale-compaction' } });
    }
  }
  notify('turn/started', { threadId: params.threadId, turn: { id: turnId, status: 'inProgress', items: [] } });
  if (diagnosticScenario && turnSequence > 1) {
    for (const [threadId, id] of [[params.threadId, `fixture-turn-${turnSequence - 1}`], ['wrong-thread', turnId], [params.threadId, 'wrong-current-turn']]) {
      notify('turn/completed', { threadId, turn: { id, status: 'failed', error: { codexErrorInfo: 'unauthorized' } } });
    }
    notify('thread/tokenUsage/updated', { threadId: params.threadId, tokenUsage: { last: { totalTokens: 999 } } });
  }
	if (params.clientUserMessageId) {
	  const userItem = { type: 'userMessage', id: `prompt-user-${turnId}`, clientId: params.clientUserMessageId, content: params.input };
	  turnRecord.items.push(userItem);
	  notify('item/started', {
		threadId: params.threadId, turnId, startedAtMs: Date.now(),
		item: userItem,
	  });
	}
  const text = (params.input || []).filter((item) => item.type === 'text').map((item) => item.text).join('\n');
  const images = (params.input || []).filter((item) => item.type === 'image');
  if (text.includes('[fixture:error-category]')) {
    const error = JSON.parse(process.env.WORKASS_CODEX_FIXTURE_DIAGNOSTIC_ERROR || '{}');
    notify('error', { threadId: params.threadId, turnId, willRetry: true, error });
    completeTurn(turnId);
    return;
  }
  if (diagnosticScenario) {
    const error = {
      message: 'Reconnecting... 2/5; token=fixture-private-value; https://private.invalid/native-id',
      codexErrorInfo: { responseStreamDisconnected: { httpStatusCode: 502, socketCode: 'private-native-id' } },
      additionalDetails: 'websocket closed by server before response.completed; api_key=fixture-key; password="fixture private phrase"; credential=fixture-credential; Bearer fixture-bearer; /private/native/path',
    };
    notify('error', { threadId: params.threadId, turnId, willRetry: true, error });
    // Official WarningNotification: message + optional threadId, never turnId.
    const warning = { message: 'Falling back from WebSockets to HTTPS transport. token=fixture-warning' };
    notify('warning', { ...warning, threadId: params.threadId });
    notify('warning', { ...warning, threadId: 'wrong-thread' });
    notify('warning', warning);
    notify('item/started', { threadId: params.threadId, turnId, item: { type: 'contextCompaction', id: 'diagnostic-compaction' } });
    notify('item/completed', { threadId: params.threadId, turnId, item: { type: 'contextCompaction', id: 'diagnostic-compaction' } });
    notify('thread/compacted', { threadId: params.threadId, turnId });
    notify('item/completed', { threadId: params.threadId, turnId, item: { type: 'contextCompaction', id: 'diagnostic-compaction' } });
    notify('thread/tokenUsage/updated', { threadId: params.threadId, turnId, tokenUsage: {
      last: { totalTokens: 0, inputTokens: -1, cachedInputTokens: '42', outputTokens: 2.5, reasoningOutputTokens: 9007199254740992 }, modelContextWindow: null,
    } });
    notify('thread/tokenUsage/updated', { threadId: params.threadId, turnId, tokenUsage: diagnosticUsage });
    if (text.includes('[fixture:diagnostic-failed]')) {
      completeTurn(turnId, 'failed', { message: 'Retry limit exhausted', codexErrorInfo: { responseTooManyFailedAttempts: { httpStatusCode: 503 } }, additionalDetails: error.additionalDetails });
    } else completeTurn(turnId);
    // Idle notifications must not emit events or become the next input's prior usage.
    notify('thread/tokenUsage/updated', { threadId: params.threadId, turnId, tokenUsage: { last: { totalTokens: 888 }, modelContextWindow: 888 } });
    notify('error', { threadId: params.threadId, turnId, willRetry: true, error });
    notify('warning', { ...warning, threadId: params.threadId });
    return;
  }
  if (text.includes('[fixture:fallback-warning]')) {
    const warning = { message: process.env.WORKASS_CODEX_FIXTURE_WARNING || 'Reconnecting... 2/5' };
    notify('warning', { ...warning, threadId: params.threadId });
    notify('warning', { ...warning, threadId: 'wrong-thread' });
    notify('warning', { ...warning, threadId: null });
    notify('warning', warning);
    completeTurn(turnId);
    notify('warning', { ...warning, threadId: params.threadId });
    return;
  }
  if (text.includes('[fixture:socket-disconnected') || text.includes('[fixture:socket-recovered]')) {
    notify('item/agentMessage/delta', {
      threadId: params.threadId, turnId, itemId: 'socket-partial', delta: 'Partial work before upstream disconnect.',
    });
    const error = {
      message: 'stream disconnected before completion: websocket closed by server before response.completed',
      codexErrorInfo: { responseStreamDisconnected: { httpStatusCode: null } },
      additionalDetails: null,
    };
    notify('error', { threadId: params.threadId, turnId, willRetry: true, error });
    if (!text.includes('[fixture:socket-recovered]')) {
      notify('error', { threadId: params.threadId, turnId, willRetry: false, error });
      completeTurn(turnId, 'failed', text.includes('[fixture:socket-disconnected-notification]') ? undefined : error);
      return;
    }
  }
  if (text.includes('[fixture:native-background]')) {
    pendingNativeChild = 'background-child';
    notify('item/completed', { threadId: params.threadId, turnId,
      item: { type: 'collabAgentToolCall', id: 'background-spawn', tool: 'spawnAgent', status: 'completed',
        senderThreadId: params.threadId, receiverThreadIds: [pendingNativeChild],
        agentsStates: { [pendingNativeChild]: { status: 'running' } }, prompt: 'Background review', model: 'gpt-fixture-mini' } });
    notify('item/agentMessage/delta', { threadId: params.threadId, turnId, itemId: 'parent-answer', delta: 'Child is still running.' });
    completeTurn(turnId);
    return;
  }
  if (text.includes('[fixture:native-discovery]')) {
    notify('thread/started', { thread: { id: 'discovered-child', agentNickname: 'Transport reviewer',
      source: { subAgent: { thread_spawn: { parent_thread_id: params.threadId, depth: 1 } } } } });
    notify('thread/started', { thread: { id: 'foreign-child', agentNickname: 'FOREIGN',
      source: { subAgent: { thread_spawn: { parent_thread_id: 'foreign-parent', depth: 1 } } } } });
    notify('turn/started', { threadId: 'discovered-child', turn: { id: 'discovered-turn', status: 'inProgress' } });
    const approval = await request('item/commandExecution/requestApproval', {
      threadId: 'discovered-child', turnId: 'discovered-turn', itemId: 'child-approval', command: 'printf child',
    });
    if (approval?.decision !== 'accept') throw new Error('native child approval lost its parent route');
    notify('turn/completed', { threadId: 'discovered-child', turn: { id: 'discovered-turn', status: 'failed', items: [] } });
    for (const kind of ['interacted', 'started', 'completed']) {
      notify('item/completed', { threadId: params.threadId, turnId,
        item: { type: 'subAgentActivity', id: `activity-${kind}`, agentThreadId: 'discovered-child', agentPath: '/root/review', kind } });
    }
  }
  if (text.includes('[fixture:native-agents]')) {
    const collab = (tool, agentsStates, receivers = ['native-child-1', 'native-child-2']) => notify('item/completed', {
      threadId: params.threadId, turnId,
      item: { type: 'collabAgentToolCall', id: `collab-${tool}`, tool, status: 'completed',
        senderThreadId: params.threadId, receiverThreadIds: receivers, agentsStates,
        prompt: 'Review transport lifecycle', model: 'gpt-fixture-mini', reasoningEffort: 'high' },
    });
    collab('spawnAgent', { 'native-child-1': { status: 'running', message: '' }, 'native-child-2': { status: 'pendingInit' } });
    notify('item/started', { threadId: 'native-child-1', turnId: 'child-turn',
      item: { type: 'commandExecution', id: 'command-fixture', command: 'printf child', status: 'inProgress' } });
    notify('item/completed', { threadId: 'native-child-1', turnId: 'child-turn',
      item: { type: 'commandExecution', id: 'command-fixture', command: 'printf child', status: 'completed', aggregatedOutput: 'child output' } });
    notify('item/agentMessage/delta', { threadId: 'native-child-1', turnId: 'child-turn', itemId: 'child-message', delta: 'Child result' });
    notify('turn/completed', { threadId: 'native-child-1', turn: { id: 'child-turn', status: 'completed', items: [] } });
    // Unknown threads and child usage/consumption must not alter the parent.
    notify('item/agentMessage/delta', { threadId: 'unrelated', itemId: 'other', delta: 'UNRELATED' });
    notify('item/started', { threadId: 'native-child-1', item: { type: 'userMessage', id: 'child-user', clientId: 'child-input' } });
    collab('wait', { 'native-child-1': { status: 'completed', message: 'Child result' }, 'native-child-2': { status: 'running' } });
    collab('interruptAgent', { 'native-child-2': { status: 'interrupted' } }, ['native-child-2']);
    // Keep the parent active so the test can prove child completion did not
    // resolve session/prompt and that native parent steering still works.
    return;
  }
  if (text.includes('[fixture:retry-')) {
    notify('error', { threadId: params.threadId, turnId, willRetry: true, error: {
      message: 'Reconnecting... 2/5',
      codexErrorInfo: { responseStreamDisconnected: { httpStatusCode: 502 } },
      additionalDetails: 'upstream connection reset; token=fixture-private-value; password="private phrase with spaces"',
    } });
    if (text.includes('[fixture:retry-failed]')) {
      completeTurn(turnId, 'failed', {
        message: 'Retry limit exhausted',
        codexErrorInfo: { responseTooManyFailedAttempts: { httpStatusCode: 503 } },
        additionalDetails: 'upstream unavailable; Bearer fixture-bearer-value',
      });
      return;
    }
  }

  notify('item/reasoning/summaryTextDelta', { threadId: params.threadId, turnId, itemId: 'reasoning-fixture', summaryIndex: 0, delta: 'Fixture reasoning' });
  if (text.includes('[fixture:rapid-steer-commentary]')) {
    activeTurnScenario = 'rapid-steer-commentary';
    rapidSteerSequence = 0;
    return;
  }
  if (text.includes('keep running')) return;
  if (text.includes('[fixture:compact-missing-turn]')) {
    notify('thread/compacted', { threadId: params.threadId });
  }
  if (text.includes('[fixture:compact]')) {
    notify('thread/compacted', {
      threadId: params.threadId,
      turnId,
      checkpointId: `fixture-checkpoint-${turnId}`,
    });
  }
  if (text.includes('exercise permission')) {
    notify('item/started', {
      threadId: params.threadId, turnId, startedAtMs: Date.now(),
      item: { type: 'commandExecution', id: 'command-fixture', command: 'printf fixture', commandActions: [], cwd: params.cwd || process.cwd(), status: 'inProgress' },
    });
    const approval = await request('item/commandExecution/requestApproval', {
      threadId: params.threadId, turnId, itemId: 'command-fixture', startedAtMs: Date.now(), command: 'printf fixture', cwd: params.cwd || process.cwd(),
    });
    if (approval?.decision !== 'accept') throw new Error('fixture command was denied');
    notify('item/completed', {
      threadId: params.threadId, turnId, completedAtMs: Date.now(),
      item: { type: 'commandExecution', id: 'command-fixture', command: 'printf fixture', commandActions: [], cwd: params.cwd || process.cwd(), status: 'completed', aggregatedOutput: 'fixture' },
    });
  }
  notify('item/agentMessage/delta', {
    threadId: params.threadId, turnId, itemId: 'message-fixture',
    delta: text.includes('[fixture:image]')
      ? `Fixture image count: ${images.length}; ${String(images[0]?.url || '').slice(0, 22)}; ${String(images[0]?.url || '').length}`
      : 'Fixture answer',
  });
  notify('thread/tokenUsage/updated', {
    threadId: params.threadId, turnId,
    tokenUsage: { last: { inputTokens: 1, outputTokens: 2, totalTokens: 3 }, total: { inputTokens: 1, outputTokens: 2, totalTokens: 3 }, modelContextWindow: 1000 },
  });
  completeTurn(turnId);
}

async function handle(message) {
  if (Object.hasOwn(message, 'id') && !message.method) {
    const resolve = pending.get(String(message.id));
    if (resolve) { pending.delete(String(message.id)); resolve(message.result); }
    return;
  }
  const { id, method, params = {} } = message;
  if (process.env.WORKASS_CODEX_FIXTURE_RPC_TRACE) {
    appendFileSync(process.env.WORKASS_CODEX_FIXTURE_RPC_TRACE, `${JSON.stringify({
      method, ...(params.threadId ? { exactThread: params.threadId === fixtureThreadId } : {}),
      ...(process.env.WORKASS_CODEX_FIXTURE_LARGE_RESUME === '1' && method === 'turn/start' ? {
        inputBytes: Buffer.byteLength(JSON.stringify(params.input)),
        inputDigest: createHash('sha256').update(JSON.stringify(params.input)).digest('hex'),
        inputCount: params.input?.length,
      } : {}),
    })}\n`);
  }
  if (method === 'initialize') return respond(id, { userAgent: 'fixture', platformFamily: 'unix', platformOs: 'fixture', codexHome: '/fixture' });
  if (method === 'initialized') return;
  if (method === 'experimentalFeature/list') return respond(id, params.cursor
    ? { data: [{ name: 'goals', enabled: process.env.WORKASS_CODEX_FIXTURE_GOALS_UNAVAILABLE !== '1' }], nextCursor: null }
    : { data: [], nextCursor: 'goal-features' });
  if (method.startsWith('thread/goal/') && params.threadId !== fixtureThreadId) throw new Error('goal changed its exact thread');
  if (method === 'thread/goal/get') return respond(id, { goal });
  if (method === 'thread/settings/update') {
    if (process.env.WORKASS_CODEX_FIXTURE_GOAL_SETTINGS_FAIL === '1') return write({ id, error: { code: -32601, message: 'settings method unavailable' } });
    if (params.model !== 'gpt-fixture' || params.effort !== 'high' || !['default', 'priority'].includes(params.serviceTier) || !params.sandboxPolicy || !params.approvalPolicy) throw new Error('goal settings lost model/effort/speed/permissions');
    goalSettings = params;
    return respond(id, {});
  }
  if (method === 'thread/inject_items') {
    if (params.threadId !== fixtureThreadId || params.items?.length !== 1 || params.items[0].role !== 'user') throw new Error('goal context thread/shape incorrect');
    goalContext = params.items;
    return respond(id, {});
  }
  if (method === 'thread/goal/clear') {
    const cleared = Boolean(goal);
    goal = null;
    respond(id, { cleared });
    updateGoalFixture();
    return;
  }
  if (method === 'thread/goal/set') {
    if (!goal && !params.objective) return write({ id, error: { code: -32602, message: 'no goal exists' } });
    if (params.status === 'active' && (!goalSettings || !goalContext)) throw new Error('goal started before settings/context');
    goal = { threadId: fixtureThreadId, objective: params.objective || goal?.objective, status: params.status,
      tokensUsed: goal?.tokensUsed || 0, timeUsedSeconds: 1, tokenBudget: null, createdAt: 1, updatedAt: (goal?.updatedAt || 0) + 1 };
    respond(id, { goal });
    updateGoalFixture(params.status);
    if (params.status === 'active') runGoalFixture();
    return;
  }
  if (method === 'model/list' && process.env.WORKASS_CODEX_FIXTURE_SPEED_CATALOG) {
    model.additionalSpeedTiers = ['fast'];
    if (process.env.WORKASS_CODEX_FIXTURE_SPEED_CATALOG === 'legacy') delete model.serviceTiers;
    else model.serviceTiers = [];
  }
  if (method === 'model/list') return respond(id, { data: [model, secondaryModel], nextCursor: null });
  if (method === 'thread/start') {
    if (process.env.WORKASS_CODEX_FIXTURE_AUTH_ERROR === '1') {
      return write({ id, error: { code: -32000, message: 'Authentication required: run codex login' } });
    }
    if (process.env.WORKASS_CODEX_FIXTURE_REQUIRE_STDIO_MCP === '1') {
      const server = params.config?.mcp_servers?.['fixture-browser'];
      if (params.config?.features?.mcp_2026_07_28 !== undefined
          || server?.command !== '/fixture/external-tool-server'
          || JSON.stringify(server?.args) !== JSON.stringify(['serve-external-mcp'])
          || server?.env?.FIXTURE_MCP_CA_FILE !== '/fixture/workass-ca.pem'
          || server?.env?.FIXTURE_MCP_ENDPOINT !== 'https://external.invalid/mcp') {
        return write({ id, error: { code: -32602, message: 'missing CA-aware stdio MCP session configuration' } });
      }
    }
    threadMCPConfigs.set(fixtureThreadId, params.config?.mcp_servers || {});
    startMCPFixtures(fixtureThreadId, params.config?.mcp_servers || {});
    return respond(id, {
    thread: { id: fixtureThreadId, preview: '', modelProvider: 'openai', createdAt: 1, updatedAt: 1, status: { type: 'idle' }, path: null, cwd: params.cwd, cliVersion: 'fixture', source: 'appServer', agentNickname: null, agentRole: null, gitInfo: null, name: null, turns: [] },
    model: model.id, reasoningEffort: 'high', modelProvider: 'openai', cwd: params.cwd,
    approvalPolicy: 'on-request', approvalsReviewer: 'user', sandbox: { type: 'workspaceWrite', writableRoots: [], networkAccess: false },
    });
  }
  if (method === 'thread/resume') {
    if (process.env.WORKASS_CODEX_FIXTURE_AUTH_ERROR === '1') {
      return write({ id, error: { code: -32000, message: 'Authentication required: run codex login' } });
    }
    if (process.env.WORKASS_CODEX_FIXTURE_MISSING_RESUME === '1') {
      return write({ id, error: { code: -32000, message: `no rollout found for thread id ${params.threadId}` } });
    }
    threadMCPConfigs.set(params.threadId, params.config?.mcp_servers || {});
    startMCPFixtures(params.threadId, params.config?.mcp_servers || {});
    const result = {
    thread: { id: params.threadId, turns: params.excludeTurns === true ? [] : turnRecords,
      ...(process.env.WORKASS_CODEX_FIXTURE_HISTORY_MODE ? { historyMode: process.env.WORKASS_CODEX_FIXTURE_HISTORY_MODE } : {}) }, model: model.id, reasoningEffort: 'high', modelProvider: 'openai', cwd: params.cwd,
    approvalPolicy: 'on-request', approvalsReviewer: 'user', sandbox: { type: 'workspaceWrite', writableRoots: [], networkAccess: false },
    };
    if (process.env.WORKASS_CODEX_FIXTURE_LARGE_RESUME === '1' && process.env.WORKASS_CODEX_FIXTURE_RPC_TRACE) {
      appendFileSync(process.env.WORKASS_CODEX_FIXTURE_RPC_TRACE, `${JSON.stringify({
        method: 'fixture/resume-shape', responseBytes: Buffer.byteLength(JSON.stringify(result)),
        returnedTurns: result.thread.turns.length, storedTurns: turnRecords.length,
        hasHistoryOverride: Object.hasOwn(params, 'history'), hasPathOverride: Object.hasOwn(params, 'path'),
      })}\n`);
    }
    return respond(id, result);
  }
  if (method === 'mcpServerStatus/list') {
    const configured = threadMCPConfigs.get(params.threadId) || {};
    const startup = threadMCPStartup.get(params.threadId) || new Map();
    const empty = process.env.WORKASS_CODEX_FIXTURE_MCP_EMPTY_CATALOG === '1';
    return respond(id, {
      data: Object.keys(configured).map((name) => ({
        name,
        authStatus: 'unsupported',
        resources: [],
        resourceTemplates: [],
        tools: empty || startup.get(name) !== 'ready' ? {} : {
          fixture_tool: { name: 'fixture_tool', description: 'Fixture tool', inputSchema: { type: 'object' } },
        },
      })),
      nextCursor: null,
    });
  }
  if (method === 'turn/start') return void runTurn(id, params).catch((error) => write({ id, error: { code: -32603, message: error.message } }));
  if (method === 'turn/steer') {
    if (process.env.WORKASS_CODEX_FIXTURE_STEER_REJECTION === 'active-turn-not-steerable') {
      return write({
        id,
        error: {
          code: -32000,
          message: 'active turn does not accept steering',
          data: { codexErrorInfo: { activeTurnNotSteerable: { turnKind: 'review' } } },
        },
      });
    }
    if (process.env.WORKASS_CODEX_FIXTURE_STEER_REJECTION === 'no-active-turn') {
      return write({ id, error: { code: -32000, message: 'no active turn' } });
    }
    respond(id, { turnId: activeTurn });
    notify('item/started', {
      threadId: params.threadId, turnId: activeTurn, startedAtMs: Date.now(),
      item: {
        type: 'userMessage',
        id: activeTurnScenario === 'rapid-steer-commentary' ? `steer-user-fixture-${rapidSteerSequence + 1}` : 'steer-user-fixture',
        clientId: params.clientUserMessageId,
        content: params.input,
      },
    });
    if (activeTurnScenario === 'rapid-steer-commentary') {
      const sequence = ++rapidSteerSequence;
      const messageId = `rapid-commentary-${sequence}`;
      const item = { type: 'agentMessage', id: messageId, phase: 'commentary', text: '' };
      notify('item/started', {
        threadId: params.threadId, turnId: activeTurn, startedAtMs: Date.now(), item,
      });
      notify('item/agentMessage/delta', {
        threadId: params.threadId, turnId: activeTurn, itemId: messageId,
        delta: `Steer ${sequence} commentary A.`,
      });
      notify('item/agentMessage/delta', {
        threadId: params.threadId, turnId: activeTurn, itemId: messageId,
        delta: ' continuation.',
      });
      notify('item/completed', {
        threadId: params.threadId, turnId: activeTurn, completedAtMs: Date.now(),
        item: { ...item, status: 'completed' },
      });
      if (sequence >= 2) completeTurn(activeTurn);
      return;
    }
    const images = (params.input || []).filter((item) => item.type === 'image');
    const text = (params.input || []).filter((item) => item.type === 'text').map((item) => item.text).join('\n');
    notify('item/agentMessage/delta', {
      threadId: params.threadId, turnId: activeTurn, itemId: 'message-steer-fixture',
      delta: text.includes('[fixture:image]')
        ? `Fixture steer image count: ${images.length}; ${String(images[0]?.url || '').slice(0, 23)}`
        : 'Redirected answer',
    });
    completeTurn(activeTurn);
    return;
  }
  if (method === 'turn/interrupt') { respond(id, {}); completeTurn(params.turnId, 'interrupted'); return; }
	if (method === 'thread/read') return respond(id, { thread: { id: params.threadId, turns: turnRecords } });
	if (method === 'thread/items/list') {
	  if (process.env.WORKASS_CODEX_FIXTURE_ITEMS_LIST_UNSUPPORTED === '1') {
		return write({ id, error: { code: -32601, message: 'thread/items/list is not supported yet' } });
	  }
	  const data = turnRecords.flatMap((turn) => turn.items.map((item) => ({ turnId: turn.id, item }))).reverse();
	  return respond(id, { data, nextCursor: null, backwardsCursor: null });
	}
  if (method === 'account/rateLimits/read') {
    if (pendingNativeChild && (!process.env.WORKASS_CODEX_FIXTURE_CHILD_RELEASE_FILE || existsSync(process.env.WORKASS_CODEX_FIXTURE_CHILD_RELEASE_FILE))) {
      notify('item/agentMessage/delta', { threadId: pendingNativeChild, itemId: 'background-answer', delta: 'Background review failed' });
      notify('turn/completed', { threadId: pendingNativeChild, turn: { id: 'background-turn', status: 'failed', items: [] } });
      pendingNativeChild = null;
    }
    return respond(id, { rateLimits: { planType: 'plus', primary: { usedPercent: 17, resetsAt: 1, windowDurationMins: 300 } } });
  }
  if (method === 'account/rateLimitResetCredit/consume') return respond(id, { consumed: true });
  if (method === 'thread/unsubscribe') return respond(id, {});
  write({ id, error: { code: -32601, message: `fixture method not found: ${method}` } });
}

readline.createInterface({ input: process.stdin }).on('line', (line) => {
  void handle(JSON.parse(line));
});
