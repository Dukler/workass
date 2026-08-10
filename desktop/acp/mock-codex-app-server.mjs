import readline from 'node:readline';

const pending = new Map();
let requestSequence = 0;
let turnSequence = 0;
let activeTurn = null;

function write(message) { process.stdout.write(`${JSON.stringify(message)}\n`); }
function respond(id, result) { write({ id, result }); }
function notify(method, params) { write({ method, params }); }
function request(method, params) {
  const id = `fixture-app-${++requestSequence}`;
  write({ id, method, params });
  return new Promise((resolve) => pending.set(String(id), resolve));
}

const model = {
  id: 'gpt-fixture', model: 'gpt-fixture', displayName: 'GPT Fixture', description: 'Fixture model',
  hidden: false, isDefault: true, defaultReasoningEffort: 'high', inputModalities: ['text', 'image'],
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

function completeTurn(turnId, status = 'completed') {
  activeTurn = null;
  notify('turn/completed', {
    threadId: 'fixture-codex-thread',
    turn: { id: turnId, status, items: [] },
  });
}

async function runTurn(id, params) {
  const turnId = `fixture-turn-${++turnSequence}`;
  activeTurn = turnId;
  respond(id, { turn: { id: turnId, status: 'inProgress', items: [] } });
  notify('turn/started', { threadId: params.threadId, turn: { id: turnId, status: 'inProgress', items: [] } });
  const text = (params.input || []).filter((item) => item.type === 'text').map((item) => item.text).join('\n');
  const images = (params.input || []).filter((item) => item.type === 'image');
  notify('item/reasoning/summaryTextDelta', { threadId: params.threadId, turnId, itemId: 'reasoning-fixture', summaryIndex: 0, delta: 'Fixture reasoning' });
  if (text.includes('keep running')) return;
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
  if (method === 'initialize') return respond(id, { userAgent: 'fixture', platformFamily: 'unix', platformOs: 'fixture', codexHome: '/fixture' });
  if (method === 'initialized') return;
  if (method === 'model/list') return respond(id, { data: [model, secondaryModel], nextCursor: null });
  if (method === 'thread/start') {
    if (process.env.WORKASS_CODEX_FIXTURE_AUTH_ERROR === '1') {
      return write({ id, error: { code: -32000, message: 'Authentication required: run codex login' } });
    }
    if (process.env.WORKASS_CODEX_FIXTURE_REQUIRE_MCP_2026 === '1') {
      const server = params.config?.mcp_servers?.workass_agent;
      if (params.config?.features?.mcp_2026_07_28 !== true
          || server?.url !== 'https://mcp.localhost:18788/workass/mcp/agent'
          || server?.http_headers?.Authorization !== 'Bearer fixture-owner') {
        return write({ id, error: { code: -32602, message: 'missing stateless MCP session configuration' } });
      }
    }
    return respond(id, {
    thread: { id: 'fixture-codex-thread', preview: '', modelProvider: 'openai', createdAt: 1, updatedAt: 1, status: { type: 'idle' }, path: null, cwd: params.cwd, cliVersion: 'fixture', source: 'appServer', agentNickname: null, agentRole: null, gitInfo: null, name: null, turns: [] },
    model: model.id, reasoningEffort: 'high', modelProvider: 'openai', cwd: params.cwd,
    approvalPolicy: 'on-request', approvalsReviewer: 'user', sandbox: { type: 'workspaceWrite', writableRoots: [], networkAccess: false },
    });
  }
  if (method === 'thread/resume') {
    if (process.env.WORKASS_CODEX_FIXTURE_AUTH_ERROR === '1') {
      return write({ id, error: { code: -32000, message: 'Authentication required: run codex login' } });
    }
    return respond(id, {
    thread: { id: params.threadId, turns: [] }, model: model.id, reasoningEffort: 'high', modelProvider: 'openai', cwd: params.cwd,
    approvalPolicy: 'on-request', approvalsReviewer: 'user', sandbox: { type: 'workspaceWrite', writableRoots: [], networkAccess: false },
    });
  }
  if (method === 'turn/start') return void runTurn(id, params).catch((error) => write({ id, error: { code: -32603, message: error.message } }));
  if (method === 'turn/steer') {
    respond(id, { turnId: activeTurn });
    notify('item/started', {
      threadId: params.threadId, turnId: activeTurn, startedAtMs: Date.now(),
      item: { type: 'userMessage', id: 'steer-user-fixture', clientId: params.clientUserMessageId, content: params.input },
    });
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
  if (method === 'thread/read') return respond(id, { thread: { id: params.threadId, turns: activeTurn ? [{ id: activeTurn, status: 'inProgress', items: [] }] : [] } });
  if (method === 'account/rateLimits/read') return respond(id, { rateLimits: { planType: 'plus', primary: { usedPercent: 17, resetsAt: 1, windowDurationMins: 300 } } });
  if (method === 'account/rateLimitResetCredit/consume') return respond(id, { consumed: true });
  if (method === 'thread/unsubscribe') return respond(id, {});
  write({ id, error: { code: -32601, message: `fixture method not found: ${method}` } });
}

readline.createInterface({ input: process.stdin }).on('line', (line) => {
  void handle(JSON.parse(line));
});
