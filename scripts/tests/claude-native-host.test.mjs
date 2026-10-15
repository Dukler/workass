import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import path from 'node:path';
import os from 'node:os';
import { mkdtemp, writeFile, rm } from 'node:fs/promises';
import readline from 'node:readline';
import test from 'node:test';
import { fileURLToPath } from 'node:url';

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..');
const host = path.join(repoRoot, 'scripts', 'claude-native-host.mjs');
const fixtureSDK = path.join(repoRoot, 'desktop', 'acp', 'mock-claude-agent-sdk.mjs');

function startHost(env = {}) {
  const child = spawn(process.execPath, [host], {
    cwd: repoRoot,
    stdio: ['pipe', 'pipe', 'pipe'],
    env: {
      ...process.env,
      WORKASS_CLAUDE_SDK_MODULE: fixtureSDK,
      WORKASS_CLAUDE_EXECUTABLE: '/fixture/claude',
      WORKASS_CLAUDE_SESSION_ID: 'fixture-claude-session',
      ...env,
    },
  });
  const lines = readline.createInterface({ input: child.stdout });
  const messages = [];
  const waiters = [];
  lines.on('line', (line) => {
    const message = JSON.parse(line);
    messages.push(message);
    for (const waiter of [...waiters]) {
      if (waiter.match(message)) {
        waiters.splice(waiters.indexOf(waiter), 1);
        waiter.resolve(message);
      }
    }
  });
  const waitFor = (match, timeout = 3000) => new Promise((resolve, reject) => {
    const existing = messages.find(match);
    if (existing) return resolve(existing);
    const waiter = { match, resolve };
    waiters.push(waiter);
    const timer = setTimeout(() => {
      const index = waiters.indexOf(waiter);
      if (index >= 0) waiters.splice(index, 1);
      reject(new Error(`timed out waiting for host message; got ${JSON.stringify(messages)}`));
    }, timeout);
    waiter.resolve = (value) => {
      clearTimeout(timer);
      resolve(value);
    };
  });
  const send = (message) => child.stdin.write(`${JSON.stringify(message)}\n`);
  return { child, messages, waitFor, send };
}

test('official Claude SDK host provides session, streaming, steering, and permission surfaces without Zed ACP', async (t) => {
  const peer = startHost();
  t.after(() => peer.child.kill('SIGKILL'));

  peer.send({ jsonrpc: '2.0', id: 1, method: 'initialize', params: { clientInfo: { name: 'test', version: '1' } } });
  const initialized = await peer.waitFor((message) => message.id === 1);
  assert.equal(initialized.result.agentInfo.name, 'Claude Code');
  assert.equal(initialized.result.agentCapabilities.loadSession, undefined);
  assert.deepEqual(initialized.result.agentCapabilities.sessionCapabilities.resume, {});
  assert.deepEqual(initialized.result.agentCapabilities.mcpCapabilities, { http: false, sse: false });
  assert.equal(initialized.result._meta.workassClaudeSteerRequest, true);
	assert.equal(initialized.result._meta.workassStableTurnInputV1, true);

  peer.send({ jsonrpc: '2.0', id: 2, method: 'session/new', params: { cwd: repoRoot, mcpServers: [] } });
  const opened = await peer.waitFor((message) => message.id === 2);
  const sessionId = opened.result.sessionId;
  assert.equal(sessionId, 'fixture-claude-session');
  assert.equal(opened.result._meta.workassProviderRealm.verified, true);
  assert.match(opened.result._meta.workassProviderRealm.accountScope, /^account-[0-9a-f]{32}$/);
  assert.equal(JSON.stringify(opened.result._meta).includes('fixture@example.test'), false);
  assert.deepEqual(opened.result.configOptions.map((option) => option.id), ['model', 'mode', 'effort']);
  assert.equal(opened.result.configOptions[0].options[0].name, 'Claude Opus Fixture');

  peer.send({ jsonrpc: '2.0', id: 22, method: 'session/set_config_option', params: {
    sessionId, configId: 'mode', value: 'bypassPermissions',
  } });
  const fullAccess = await peer.waitFor((message) => message.id === 22);
  assert.equal(fullAccess.error, undefined);
  assert.equal(fullAccess.result.configOptions.find((option) => option.id === 'mode').currentValue, 'bypassPermissions');

  peer.send({ jsonrpc: '2.0', id: 20, method: 'session/set_config_option', params: {
    sessionId, configId: 'model', value: 'claude-haiku-fixture',
  } });
  const haiku = await peer.waitFor((message) => message.id === 20);
  assert.deepEqual(haiku.result.configOptions.map((option) => option.id), ['model', 'mode']);
  peer.send({ jsonrpc: '2.0', id: 21, method: 'session/set_config_option', params: {
    sessionId, configId: 'model', value: 'claude-opus-fixture',
  } });
  const opus = await peer.waitFor((message) => message.id === 21);
  assert.deepEqual(opus.result.configOptions.map((option) => option.id), ['model', 'mode', 'effort']);

  peer.send({ jsonrpc: '2.0', id: 3, method: 'session/prompt', params: {
    sessionId,
    prompt: [{ type: 'text', text: 'exercise permission' }],
	clientUserMessageId: 'workass-operation-1',
  } });
	const consumed = await peer.waitFor((message) => message.method === 'session/update'
	&& message.params.update.sessionUpdate === '_workass_input_consumed');
	assert.equal(consumed.params.update.clientUserMessageId, 'workass-operation-1');
  const permission = await peer.waitFor((message) => message.method === 'session/request_permission');
  assert.equal(permission.params.sessionId, sessionId);
  peer.send({ jsonrpc: '2.0', id: permission.id, result: { outcome: { outcome: 'selected', optionId: 'allow_once' } } });
  await peer.waitFor((message) => message.method === 'session/update' && message.params.update.sessionUpdate === 'agent_message_chunk');
  const promptResult = await peer.waitFor((message) => message.id === 3);
  assert.equal(promptResult.result.stopReason, 'end_turn');

  peer.send({ jsonrpc: '2.0', id: 4, method: 'session/prompt', params: {
    sessionId,
    prompt: [{ type: 'text', text: 'keep running' }],
  } });
  await peer.waitFor((message) => message.method === 'session/update' && message.params.update.sessionUpdate === 'agent_thought_chunk');
  peer.send({ jsonrpc: '2.0', id: 5, method: '_workass/claude/steer', params: {
    sessionId,
    prompt: [{ type: 'text', text: 'redirect' }],
    clientUserMessageId: 'client-steer-1',
  } });
  const steer = await peer.waitFor((message) => message.id === 5);
  assert.ok(steer.result.turnId);
  await peer.waitFor((message) => message.method === 'session/update' && message.params.update.sessionUpdate === '_workass_claude_steer_consumed');
  peer.send({ jsonrpc: '2.0', method: 'session/cancel', params: { sessionId } });
  await peer.waitFor((message) => message.id === 4);
});

// The real CLI streams a tool call: content_block_start carries an empty
// input, and the command or path only arrives with the assistant message.
// The host must pass that input on, or every row is a bare action name.
test('official Claude SDK host forwards the input of a streamed tool call once it arrives', async (t) => {
  const peer = startHost();
  t.after(() => peer.child.kill('SIGKILL'));
  peer.send({ jsonrpc: '2.0', id: 1, method: 'initialize', params: { clientInfo: { name: 'test', version: '1' } } });
  await peer.waitFor((message) => message.id === 1);
  peer.send({ jsonrpc: '2.0', id: 2, method: 'session/new', params: { cwd: repoRoot, mcpServers: [] } });
  const sessionId = (await peer.waitFor((message) => message.id === 2)).result.sessionId;
  peer.send({ jsonrpc: '2.0', id: 3, method: 'session/prompt', params: {
    sessionId, prompt: [{ type: 'text', text: '[fixture:streamed-tool] read it' }],
  } });
  await peer.waitFor((message) => message.id === 3);
  const updates = peer.messages
    .filter((message) => message.method === 'session/update' && message.params.update.toolCallId === 'fixture-streamed-tool-1')
    .map((message) => message.params.update);
  assert.equal(updates[0].sessionUpdate, 'tool_call');
  assert.deepEqual(updates[0].rawInput, {});
  const filled = updates.find((update) => update.sessionUpdate === 'tool_call_update' && update.status === 'in_progress');
  assert.ok(filled, `no update carried the streamed input: ${JSON.stringify(updates)}`);
  assert.deepEqual(filled.rawInput, { file_path: '/repo/tools/silver_review.py' });
  assert.deepEqual(filled.locations, [{ path: '/repo/tools/silver_review.py' }]);
  assert.equal(filled.title, '/repo/tools/silver_review.py');
  assert.equal(updates.filter((update) => update.sessionUpdate === 'tool_call').length, 1, 'the call is announced once');
  assert.equal(updates.at(-1).status, 'completed');
});

// Claude Code reports subagents and background shells as system messages with
// a task_* subtype. Each must reach Workass as a spawned-work event, or the
// work is invisible (user report 2026-10-13).
test('official Claude SDK host forwards Claude Code task lifecycle system messages', async (t) => {
  const peer = startHost();
  t.after(() => peer.child.kill('SIGKILL'));
  peer.send({ jsonrpc: '2.0', id: 1, method: 'initialize', params: { clientInfo: { name: 'test', version: '1' } } });
  await peer.waitFor((message) => message.id === 1);
  peer.send({ jsonrpc: '2.0', id: 2, method: 'session/new', params: { cwd: repoRoot, mcpServers: [] } });
  const sessionId = (await peer.waitFor((message) => message.id === 2)).result.sessionId;
  peer.send({ jsonrpc: '2.0', id: 3, method: 'session/prompt', params: {
    sessionId, prompt: [{ type: 'text', text: '[fixture:background-agent] go' }],
  } });
  await peer.waitFor((message) => message.id === 3);
  const events = peer.messages
    .filter((message) => message.method === 'session/update' && message.params.update.sessionUpdate === '_workass_claude_spawned_work')
    .map((message) => message.params.update.event)
    .filter((event) => event.taskId === 'fixture-agent-1');
  assert.deepEqual(events.map((event) => event.type), ['started', 'progress', 'notification']);
  assert.equal(events[0].toolCallId, 'toolu_fixture_agent_1');
  assert.equal(events[0].description, 'Classify workspace tmp dirs');
  assert.equal(events[0].subagentType, 'general-purpose');
  assert.equal(events[1].lastToolName, 'Bash');
  assert.equal(events[2].status, 'completed');
  assert.equal(events[2].outputFile, '/private/tmp/claude-501/fixture/tasks/fixture-agent-1.output');
});

// Claude Code honours human-only features, the "ultracode" keyword among them,
// only on prompts marked origin {kind:"human"}. The host marks exactly the
// prompts the daemon says a person typed (user request 2026-10-13).
test('official Claude SDK host marks only human-typed prompts with a human origin', async (t) => {
  const peer = startHost();
  t.after(() => peer.child.kill('SIGKILL'));
  peer.send({ jsonrpc: '2.0', id: 1, method: 'initialize', params: { clientInfo: { name: 'test', version: '1' } } });
  const initialized = await peer.waitFor((message) => message.id === 1);
  assert.equal(initialized.result._meta.workassNativeCommandInputV1, true);
  peer.send({ jsonrpc: '2.0', id: 2, method: 'session/new', params: { cwd: repoRoot, mcpServers: [] } });
  const sessionId = (await peer.waitFor((message) => message.id === 2)).result.sessionId;
  const echo = async (id, meta) => {
    const before = peer.messages.length;
    peer.send({ jsonrpc: '2.0', id, method: 'session/prompt', params: {
      sessionId, prompt: [{ type: 'text', text: 'ultracode [fixture:echo-origin]' }], ...(meta ? { _meta: meta } : {}),
    } });
    await peer.waitFor((message) => message.id === id);
    return peer.messages.slice(before)
      .filter((message) => message.method === 'session/update' && message.params.update.sessionUpdate === 'agent_message_chunk')
      .map((message) => message.params.update.content.text).join('');
  };
  assert.equal(await echo(3, { workassCommandInput: { text: 'ultracode', humanAuthored: true } }), 'origin={"kind":"human"}');
  assert.equal(await echo(4, { workassCommandInput: { text: 'wake', humanAuthored: false } }), 'origin=null');
  assert.equal(await echo(5, null), 'origin=null');
});

test('native Claude host preserves external MCP stdio configuration', async (t) => {
  const peer = startHost({ WORKASS_CLAUDE_FIXTURE_REQUIRE_STDIO_MCP: '1' });
  t.after(() => peer.child.kill('SIGKILL'));

  peer.send({ jsonrpc: '2.0', id: 1, method: 'initialize', params: {} });
  await peer.waitFor((message) => message.id === 1);
  peer.send({ jsonrpc: '2.0', id: 2, method: 'session/new', params: {
    cwd: repoRoot,
    mcpServers: [{
      name: 'fixture-browser', command: '/fixture/external-tool-server', args: ['serve-external-mcp'],
      env: [
        { name: 'FIXTURE_MCP_CA_FILE', value: '/fixture/workass-ca.pem' },
        { name: 'FIXTURE_MCP_ENDPOINT', value: 'https://external.invalid/mcp' },
      ],
    }],
  } });
  const opened = await peer.waitFor((message) => message.id === 2);
  peer.send({ jsonrpc: '2.0', id: 3, method: 'session/prompt', params: {
    sessionId: opened.result.sessionId, prompt: [{ type: 'text', text: 'verify MCP mapping' }],
  } });
  const prompted = await peer.waitFor((message) => message.id === 3);
  assert.equal(prompted.error, undefined, JSON.stringify(prompted));
});

test('official Claude SDK host keeps the steered turn open until the direction is answered', async (t) => {
  // A CLI without command_lifecycle frames: the priority "now" boundary model.
  const peer = startHost({ WORKASS_CLAUDE_FIXTURE_LEGACY_STEER: '1' });
  t.after(() => peer.child.kill('SIGKILL'));

  peer.send({ jsonrpc: '2.0', id: 1, method: 'initialize', params: {} });
  await peer.waitFor((message) => message.id === 1);
  peer.send({ jsonrpc: '2.0', id: 2, method: 'session/new', params: { cwd: repoRoot, mcpServers: [] } });
  const sessionId = (await peer.waitFor((message) => message.id === 2)).result.sessionId;

  peer.send({ jsonrpc: '2.0', id: 3, method: 'session/prompt', params: {
    sessionId,
    prompt: [{ type: 'text', text: '[fixture:steer-boundary]' }],
  } });
  await peer.waitFor((message) => message.method === 'session/update' && message.params.update.sessionUpdate === 'agent_thought_chunk');

  peer.send({ jsonrpc: '2.0', id: 4, method: '_workass/claude/steer', params: {
    sessionId,
    prompt: [{ type: 'text', text: 'redirect now' }],
    clientUserMessageId: 'client-steer-boundary',
  } });
  assert.equal((await peer.waitFor((message) => message.id === 4)).result.receipt, true);

  // The SDK closes the pre-steer segment with its own terminal result and
  // answers the direction in the NEXT turn. Settling on that first result ended
  // the job while Claude was still answering, so the reply arrived after
  // job:end and the direction looked lost. The receipt marks that boundary.
  const receipt = await peer.waitFor((message) => message.method === 'session/update'
    && message.params.update.sessionUpdate === '_workass_claude_steer_consumed');
  assert.equal(receipt.params.update.clientUserMessageId, 'client-steer-boundary');
  // The provider can stream the answer and terminal result before this JS
  // continuation resumes. Assert captured wire order, which proves the
  // direction was answered before the prompt's terminal reply.
  const settled = await peer.waitFor((message) => message.id === 3);
  const receiptIndex = peer.messages.indexOf(receipt);
  const answerIndex = peer.messages.findIndex((message) => message.method === 'session/update'
    && message.params.update.sessionUpdate === 'agent_message_chunk'
    && String(message.params.update.content?.text || '').includes('Fixture answer'));
  const settledIndex = peer.messages.indexOf(settled);
  assert.ok(receiptIndex >= 0 && answerIndex > receiptIndex && settledIndex > answerIndex,
    `expected steer receipt, answer, then terminal reply; indexes=${receiptIndex},${answerIndex},${settledIndex}`);
  assert.equal(settled.result.stopReason, 'end_turn');
});

test('official Claude SDK host fails a prompt when the provider stream closes without a terminal result', async (t) => {
  const peer = startHost();
  t.after(() => peer.child.kill('SIGKILL'));

  peer.send({ jsonrpc: '2.0', id: 1, method: 'initialize', params: {} });
  await peer.waitFor((message) => message.id === 1);
  peer.send({ jsonrpc: '2.0', id: 2, method: 'session/new', params: { cwd: repoRoot, mcpServers: [] } });
  const opened = await peer.waitFor((message) => message.id === 2);
  peer.send({ jsonrpc: '2.0', id: 3, method: 'session/prompt', params: {
    sessionId: opened.result.sessionId,
    prompt: [{ type: 'text', text: '[fixture:close-without-result]' }],
  } });
  const failed = await peer.waitFor((message) => message.id === 3);
  assert.match(failed.error?.message || '', /closed before a terminal result/i);

  peer.send({ jsonrpc: '2.0', id: 4, method: 'session/set_config_option', params: {
    sessionId: opened.result.sessionId, configId: 'mode', value: 'bypassPermissions',
  } });
  const restored = await peer.waitFor((message) => message.id === 4);
  assert.equal(restored.error, undefined);
  peer.send({ jsonrpc: '2.0', id: 5, method: 'session/prompt', params: {
    sessionId: opened.result.sessionId,
    prompt: [{ type: 'text', text: 'follow-up after provider stream recovery' }],
  } });
  const followUp = await peer.waitFor((message) => message.id === 5);
  assert.equal(followUp.result.stopReason, 'end_turn');
});

test('official Claude SDK host retries one untouched prompt after a stale OAuth refresh result', async (t) => {
  const peer = startHost({ WORKASS_CLAUDE_FIXTURE_TRANSIENT_OAUTH_RESULT: '1' });
  t.after(() => peer.child.kill('SIGKILL'));

  peer.send({ jsonrpc: '2.0', id: 1, method: 'initialize', params: {} });
  await peer.waitFor((message) => message.id === 1);
  peer.send({ jsonrpc: '2.0', id: 2, method: 'session/new', params: { cwd: repoRoot, mcpServers: [] } });
  const opened = await peer.waitFor((message) => message.id === 2);
  peer.send({ jsonrpc: '2.0', id: 3, method: 'session/prompt', params: {
    sessionId: opened.result.sessionId,
    prompt: [{ type: 'text', text: 'retry this logical turn exactly once' }],
  } });
  const completed = await peer.waitFor((message) => message.id === 3);
  assert.equal(completed.error, undefined, JSON.stringify(completed));
  assert.equal(completed.result.stopReason, 'end_turn');
  const answerChunks = peer.messages
    .filter((message) => message.method === 'session/update'
      && message.params?.update?.sessionUpdate === 'agent_message_chunk')
    .map((message) => String(message.params.update.content?.text || ''));
  assert.deepEqual(answerChunks, ['Fixture answer']);
});

test('official Claude SDK host rejects non-retryable terminal provider errors', async (t) => {
  const peer = startHost();
  t.after(() => peer.child.kill('SIGKILL'));

  peer.send({ jsonrpc: '2.0', id: 1, method: 'initialize', params: {} });
  await peer.waitFor((message) => message.id === 1);
  peer.send({ jsonrpc: '2.0', id: 2, method: 'session/new', params: { cwd: repoRoot, mcpServers: [] } });
  const opened = await peer.waitFor((message) => message.id === 2);
  peer.send({ jsonrpc: '2.0', id: 3, method: 'session/prompt', params: {
    sessionId: opened.result.sessionId,
    prompt: [{ type: 'text', text: '[fixture:terminal-error]' }],
  } });
  const failed = await peer.waitFor((message) => message.id === 3);
  assert.match(failed.error?.message || '', /fixture terminal provider failure/i);
});

test('official Claude SDK host retries a persistent OAuth refresh failure only once', async (t) => {
  const peer = startHost({ WORKASS_CLAUDE_FIXTURE_PERSISTENT_OAUTH_RESULT: '1' });
  t.after(() => peer.child.kill('SIGKILL'));

  peer.send({ jsonrpc: '2.0', id: 1, method: 'initialize', params: {} });
  await peer.waitFor((message) => message.id === 1);
  peer.send({ jsonrpc: '2.0', id: 2, method: 'session/new', params: { cwd: repoRoot, mcpServers: [] } });
  const opened = await peer.waitFor((message) => message.id === 2);
  peer.send({ jsonrpc: '2.0', id: 3, method: 'session/prompt', params: {
    sessionId: opened.result.sessionId,
    prompt: [{ type: 'text', text: 'fail after one bounded retry' }],
  } });
  const failed = await peer.waitFor((message) => message.id === 3);
  assert.match(failed.error?.message || '', /oauth session expired and could not be refreshed/i);
  const visibleErrors = peer.messages.filter((message) => message.method === 'session/update'
    && message.params?.update?.sessionUpdate === 'agent_message_chunk'
    && String(message.params?.update?.content?.text || '').includes('Failed to authenticate'));
  assert.equal(visibleErrors.length, 0);
});

test('official Claude SDK host cancellation settles even when the provider interrupt hangs', async (t) => {
  const peer = startHost({ WORKASS_CLAUDE_FIXTURE_INTERRUPT_HANG: '1' });
  t.after(() => peer.child.kill('SIGKILL'));

  peer.send({ jsonrpc: '2.0', id: 1, method: 'initialize', params: {} });
  await peer.waitFor((message) => message.id === 1);
  peer.send({ jsonrpc: '2.0', id: 2, method: 'session/new', params: { cwd: repoRoot, mcpServers: [] } });
  const opened = await peer.waitFor((message) => message.id === 2);
  peer.send({ jsonrpc: '2.0', id: 3, method: 'session/prompt', params: {
    sessionId: opened.result.sessionId,
    prompt: [{ type: 'text', text: 'keep running' }],
  } });
  await peer.waitFor((message) => message.method === 'session/update' && message.params.update.sessionUpdate === 'agent_thought_chunk');
  peer.send({ jsonrpc: '2.0', method: 'session/cancel', params: { sessionId: opened.result.sessionId } });
  const cancelled = await peer.waitFor((message) => message.id === 3, 1000);
  assert.equal(cancelled.result.stopReason, 'cancelled');

  peer.send({ jsonrpc: '2.0', id: 4, method: 'session/prompt', params: {
    sessionId: opened.result.sessionId,
    prompt: [{ type: 'text', text: 'queued follow-up after cancellation' }],
  } });
  const followUp = await peer.waitFor((message) => message.id === 4);
  assert.equal(followUp.result.stopReason, 'end_turn');
});

test('official Claude SDK host waits for the prior SDK query to release its session id before reopening', async (t) => {
  const peer = startHost({
    WORKASS_CLAUDE_FIXTURE_EXCLUSIVE_SESSION: '1',
    WORKASS_CLAUDE_FIXTURE_CLOSE_DELAY_MS: '40',
  });
  t.after(() => peer.child.kill('SIGKILL'));

  peer.send({ jsonrpc: '2.0', id: 1, method: 'initialize', params: {} });
  await peer.waitFor((message) => message.id === 1);
  peer.send({ jsonrpc: '2.0', id: 2, method: 'session/new', params: { cwd: repoRoot, mcpServers: [] } });
  const opened = await peer.waitFor((message) => message.id === 2);
  peer.send({ jsonrpc: '2.0', id: 3, method: 'session/set_config_option', params: {
    sessionId: opened.result.sessionId, configId: 'mode', value: 'bypassPermissions',
  } });
  assert.equal((await peer.waitFor((message) => message.id === 3)).error, undefined);
  peer.send({ jsonrpc: '2.0', id: 4, method: 'session/set_config_option', params: {
    sessionId: opened.result.sessionId, configId: 'model', value: 'claude-opus-fixture',
  } });
  assert.equal((await peer.waitFor((message) => message.id === 4)).error, undefined);
  peer.send({ jsonrpc: '2.0', id: 5, method: 'session/prompt', params: {
    sessionId: opened.result.sessionId,
    prompt: [
      { type: 'image', mimeType: 'image/png', data: 'iVBORw0KGgo=' },
      { type: 'text', text: '[fixture:image] first turn after launch-time controls' },
    ],
  } });
  const imageAnswer = await peer.waitFor((message) => message.method === 'session/update'
    && message.params?.update?.sessionUpdate === 'agent_message_chunk'
    && String(message.params?.update?.content?.text || '').includes('Fixture image count'));
  assert.match(imageAnswer.params.update.content.text, /Fixture image count: 1; image\/png; 12/);
  const result = await peer.waitFor((message) => message.id === 5);
  assert.equal(result.error, undefined, JSON.stringify(result));
  assert.equal(result.result.stopReason, 'end_turn');
});

test('official Claude SDK host preserves image blocks on prompts and live steering', async (t) => {
  // A CLI without command_lifecycle frames: the priority "now" boundary model.
  const peer = startHost({ WORKASS_CLAUDE_FIXTURE_LEGACY_STEER: '1' });
  t.after(() => peer.child.kill('SIGKILL'));

  peer.send({ jsonrpc: '2.0', id: 1, method: 'initialize', params: {} });
  await peer.waitFor((message) => message.id === 1);
  peer.send({ jsonrpc: '2.0', id: 2, method: 'session/new', params: { cwd: repoRoot, mcpServers: [] } });
  const opened = await peer.waitFor((message) => message.id === 2);
  peer.send({ jsonrpc: '2.0', id: 3, method: 'session/prompt', params: {
    sessionId: opened.result.sessionId,
    prompt: [
      { type: 'image', mimeType: 'image/png', data: 'iVBORw0KGgo=' },
      { type: 'text', text: '[fixture:image] first image turn' },
    ],
  } });
  const imageAnswer = await peer.waitFor((message) => message.method === 'session/update'
    && message.params?.update?.sessionUpdate === 'agent_message_chunk'
    && String(message.params?.update?.content?.text || '').includes('Fixture image count'));
  assert.match(imageAnswer.params.update.content.text, /Fixture image count: 1; image\/png; 12/);
  assert.equal((await peer.waitFor((message) => message.id === 3)).result.stopReason, 'end_turn');

  peer.send({ jsonrpc: '2.0', id: 4, method: 'session/prompt', params: {
    sessionId: opened.result.sessionId,
    prompt: [{ type: 'text', text: 'keep running' }],
  } });
  await peer.waitFor((message) => message.method === 'session/update'
    && message.params?.update?.sessionUpdate === 'agent_thought_chunk');
  peer.send({ jsonrpc: '2.0', id: 5, method: '_workass/claude/steer', params: {
    sessionId: opened.result.sessionId,
    prompt: [
      { type: 'image', mimeType: 'image/jpeg', data: '/9j/2Q==' },
      { type: 'text', text: '[fixture:image] steer image' },
    ],
    clientUserMessageId: 'image-steer',
  } });
  assert.equal((await peer.waitFor((message) => message.id === 5)).error, undefined);
  const steerImage = await peer.waitFor((message) => message.method === 'session/update'
    && message.params?.update?.sessionUpdate === 'agent_message_chunk'
    && String(message.params?.update?.content?.text || '').includes('Fixture image count: 1; image/jpeg; 8'));
  assert.ok(steerImage);
  peer.send({ jsonrpc: '2.0', method: 'session/cancel', params: { sessionId: opened.result.sessionId } });
  await peer.waitFor((message) => message.id === 4);
});

test('official Claude SDK host replaces a dead between-turn transport before accepting the next prompt', async (t) => {
  const peer = startHost();
  t.after(() => peer.child.kill('SIGKILL'));

  peer.send({ jsonrpc: '2.0', id: 1, method: 'initialize', params: {} });
  await peer.waitFor((message) => message.id === 1);
  peer.send({ jsonrpc: '2.0', id: 2, method: 'session/new', params: { cwd: repoRoot, mcpServers: [] } });
  const opened = await peer.waitFor((message) => message.id === 2);

  peer.send({ jsonrpc: '2.0', id: 3, method: 'session/prompt', params: {
    sessionId: opened.result.sessionId,
    prompt: [{ type: 'text', text: '[fixture:transport-closes-after-result]' }],
  } });
  const first = await peer.waitFor((message) => message.id === 3);
  assert.equal(first.result.stopReason, 'end_turn');

  peer.send({ jsonrpc: '2.0', id: 4, method: 'session/set_config_option', params: {
    sessionId: opened.result.sessionId, configId: 'model', value: 'claude-haiku-fixture',
  } });
  const recovered = await peer.waitFor((message) => message.id === 4);
  assert.equal(recovered.error, undefined);

  peer.send({ jsonrpc: '2.0', id: 5, method: 'session/prompt', params: {
    sessionId: opened.result.sessionId,
    prompt: [{ type: 'text', text: 'second turn after transport replacement' }],
  } });
  const second = await peer.waitFor((message) => message.id === 5);
  assert.equal(second.result.stopReason, 'end_turn');
});

test('official Claude SDK host fails closed when the exact resumed conversation is missing', async (t) => {
  const peer = startHost({ WORKASS_CLAUDE_FIXTURE_MISSING_RESUME: '1' });
  t.after(() => peer.child.kill('SIGKILL'));

  peer.send({ jsonrpc: '2.0', id: 1, method: 'initialize', params: {} });
  await peer.waitFor((message) => message.id === 1);
  peer.send({ jsonrpc: '2.0', id: 2, method: 'session/resume', params: {
    sessionId: 'fixture-missing-resume', cwd: repoRoot, mcpServers: [],
  } });
  const resumed = await peer.waitFor((message) => message.id === 2);
  assert.equal(resumed.error?.code, -32044);
  assert.match(resumed.error?.message || '', /candidate was never materialized/i);
  assert.equal(resumed.result, undefined);
});

test('real official Claude SDK host completes two turns after pre-prompt controls', {
  skip: process.env.WORKASS_REAL_FRONTIER !== '1',
}, async (t) => {
  const sdkModule = String(process.env.WORKASS_REAL_CLAUDE_SDK_MODULE || '').trim();
  const executable = String(process.env.WORKASS_REAL_CLAUDE_EXECUTABLE || '').trim();
  assert.ok(sdkModule, 'WORKASS_REAL_CLAUDE_SDK_MODULE is required');
  assert.ok(executable, 'WORKASS_REAL_CLAUDE_EXECUTABLE is required');
  const peer = startHost({
    WORKASS_CLAUDE_SDK_MODULE: sdkModule,
    WORKASS_CLAUDE_EXECUTABLE: executable,
    WORKASS_CLAUDE_SESSION_ID: '',
  });
  t.after(() => peer.child.kill('SIGKILL'));

  peer.send({ jsonrpc: '2.0', id: 1, method: 'initialize', params: {} });
  await peer.waitFor((message) => message.id === 1, 90_000);
  peer.send({ jsonrpc: '2.0', id: 2, method: 'session/new', params: { cwd: repoRoot, mcpServers: [] } });
  const opened = await peer.waitFor((message) => message.id === 2, 90_000);

  peer.send({ jsonrpc: '2.0', id: 3, method: 'session/set_config_option', params: {
    sessionId: opened.result.sessionId, configId: 'mode', value: 'bypassPermissions',
  } });
  assert.equal((await peer.waitFor((message) => message.id === 3, 30_000)).error, undefined);
  peer.send({ jsonrpc: '2.0', id: 4, method: 'session/set_config_option', params: {
    sessionId: opened.result.sessionId, configId: 'model', value: 'haiku',
  } });
  assert.equal((await peer.waitFor((message) => message.id === 4, 30_000)).error, undefined);

  peer.send({ jsonrpc: '2.0', id: 5, method: 'session/prompt', params: {
    sessionId: opened.result.sessionId,
    prompt: [{ type: 'text', text: 'Without tools, reply with one short word.' }],
  } });
  assert.equal((await peer.waitFor((message) => message.id === 5, 180_000)).result.stopReason, 'end_turn');

  peer.send({ jsonrpc: '2.0', id: 6, method: 'session/prompt', params: {
    sessionId: opened.result.sessionId,
    prompt: [{ type: 'text', text: 'Without tools, reply with one different short word.' }],
  } });
  assert.equal((await peer.waitFor((message) => message.id === 6, 180_000)).result.stopReason, 'end_turn');
});

// A card is answered by a person, not by a peer that is merely slow. The
// plumbing guard that protects fs/terminal RPCs must never reach it: expiring a
// permission resolved null, which the model reads as "the user chose nothing"
// while their card is still on screen and clickable (user 2026-07-25).
test('official Claude SDK host lets a permission outlive the plumbing timeout', async (t) => {
  const peer = startHost({ WORKASS_ACP_PEER_TIMEOUT_MS: '120' });
  t.after(() => peer.child.kill('SIGKILL'));

  peer.send({ jsonrpc: '2.0', id: 1, method: 'initialize', params: { clientInfo: { name: 'test', version: '1' } } });
  await peer.waitFor((message) => message.id === 1);
  peer.send({ jsonrpc: '2.0', id: 2, method: 'session/new', params: { cwd: repoRoot, mcpServers: [] } });
  const opened = await peer.waitFor((message) => message.id === 2);

  peer.send({ jsonrpc: '2.0', id: 3, method: 'session/prompt', params: {
    sessionId: opened.result.sessionId,
    prompt: [{ type: 'text', text: 'exercise permission' }],
  } });
  const permission = await peer.waitFor((message) => message.method === 'session/request_permission');

  // Five times the plumbing cap: a user reading the card, not a dead peer.
  await new Promise((resolve) => setTimeout(resolve, 600));
  peer.send({ jsonrpc: '2.0', id: permission.id, result: { outcome: { outcome: 'selected', optionId: 'allow_once' } } });

  // The fixture throws unless the tool was allowed, so end_turn proves the late
  // answer still landed.
  const promptResult = await peer.waitFor((message) => message.id === 3);
  assert.equal(promptResult.result.stopReason, 'end_turn', JSON.stringify(promptResult));
});

// A direction given in the first seconds of a turn used to kill it: the steer's
// priority "now" aborts Claude Code's query, and with nothing committed yet the
// conversation ends on our own message, which Claude Code reports as a failed
// turn ([ede_diagnostic] result_type=user … stop_reason=null). The job died and
// took the direction with it, so the user had to retype it to get any answer
// (user report 2026-07-27).
test('official Claude SDK host re-asks a direction that outran the model instead of failing the turn', async (t) => {
  // A CLI without command_lifecycle frames: the priority "now" boundary model.
  const peer = startHost({ WORKASS_CLAUDE_FIXTURE_LEGACY_STEER: '1' });
  t.after(() => peer.child.kill('SIGKILL'));

  peer.send({ jsonrpc: '2.0', id: 1, method: 'initialize', params: {} });
  await peer.waitFor((message) => message.id === 1);
  peer.send({ jsonrpc: '2.0', id: 2, method: 'session/new', params: { cwd: repoRoot, mcpServers: [] } });
  const sessionId = (await peer.waitFor((message) => message.id === 2)).result.sessionId;

  // A turn that has started but committed nothing — the exact window.
  peer.send({ jsonrpc: '2.0', id: 3, method: 'session/prompt', params: {
    sessionId,
    prompt: [{ type: 'text', text: '[fixture:silent-turn]' }],
  } });
  // Steering only answers while a prompt is live, so a receipt here is also the
  // proof that the direction landed INSIDE the silent turn.
  peer.send({ jsonrpc: '2.0', id: 4, method: '_workass/claude/steer', params: {
    sessionId,
    prompt: [{ type: 'text', text: 'redirect before the first word' }],
    clientUserMessageId: 'client-steer-early',
  } });
  assert.equal((await peer.waitFor((message) => message.id === 4)).result.receipt, true);

  // The direction is acknowledged and then ANSWERED: the prompt settles on a
  // real result, never on the provider's invalid-ending error.
  const receipt = await peer.waitFor((message) => message.method === 'session/update'
    && message.params.update.sessionUpdate === '_workass_claude_steer_consumed');
  assert.equal(receipt.params.update.clientUserMessageId, 'client-steer-early');

  const settled = await peer.waitFor((message) => message.id === 3);
  assert.equal(settled.error, undefined, 'the turn must survive a steer that outran the model');
  assert.equal(settled.result.stopReason, 'end_turn');
  assert.ok(peer.messages.some((message) => message.method === 'session/update'
    && message.params.update.sessionUpdate === 'agent_message_chunk'
    && String(message.params.update.content?.text || '').includes('Fixture answer')),
  'the re-asked direction must produce the answer the user was owed');
  assert.equal(peer.messages.some((message) => JSON.stringify(message).includes('ede_diagnostic')), false,
    'the provider diagnostic must never reach the client');
});

test('official Claude SDK host survives a first turn that dies after the session id is materialized', async (t) => {
  const peer = startHost({ WORKASS_CLAUDE_FIXTURE_PERSISTENT_SESSION_FILES: '1' });
  t.after(() => peer.child.kill('SIGKILL'));

  peer.send({ jsonrpc: '2.0', id: 1, method: 'initialize', params: {} });
  await peer.waitFor((message) => message.id === 1);
  peer.send({ jsonrpc: '2.0', id: 2, method: 'session/new', params: { cwd: repoRoot, mcpServers: [] } });
  const opened = await peer.waitFor((message) => message.id === 2);

  // First-ever turn: the query dies before ANY message flowed back, but the
  // provider has already written this id's transcript. This was the permanent
  // wedge (prod 2026-07-27, tab-62): every reopen re-passed sessionId, Claude
  // Code refused "Session ID ... is already in use", and the chat stayed dead
  // until a daemon restart.
  peer.send({ jsonrpc: '2.0', id: 3, method: 'session/prompt', params: {
    sessionId: opened.result.sessionId,
    prompt: [{ type: 'text', text: '[fixture:close-without-result]' }],
  } });
  const failed = await peer.waitFor((message) => message.id === 3);
  assert.match(failed.error?.message || '', /closed before a terminal result/i);

  peer.send({ jsonrpc: '2.0', id: 4, method: 'session/prompt', params: {
    sessionId: opened.result.sessionId,
    prompt: [{ type: 'text', text: 'is anyone alive in there' }],
  } });
  const followUp = await peer.waitFor((message) => message.id === 4);
  assert.equal(followUp.error, undefined, JSON.stringify(followUp));
  assert.equal(followUp.result.stopReason, 'end_turn');
});

// Claude commands surface (docs/specs/claude-commands-surface.md §3/§8): the
// catalog is captured from initializationResult(), clamped per §2, exposed on
// the open reply, replaced wholesale on commands_changed, and local command
// output renders as assistant text instead of an empty turn.
test('official Claude SDK host clamps, exposes, and replaces the Claude command catalog', async (t) => {
  const peer = startHost({ WORKASS_CLAUDE_FIXTURE_COMMAND_CATALOG: '1' });
  t.after(() => peer.child.kill('SIGKILL'));

  peer.send({ jsonrpc: '2.0', id: 1, method: 'initialize', params: {} });
  const initialized = await peer.waitFor((message) => message.id === 1);
  assert.equal(initialized.result._meta.workassClaudeCommandCatalog, true,
    'the daemon needs the advertisement to tell "old host" apart from "proven empty"');

  peer.send({ jsonrpc: '2.0', id: 2, method: 'session/new', params: { cwd: repoRoot, mcpServers: [] } });
  const opened = await peer.waitFor((message) => message.id === 2);
  const catalog = opened.result.commandCatalog;
  assert.ok(catalog, 'the open reply must carry the command catalog');
  assert.equal(catalog.commands.length, 512, '600 fixture commands clamp to 512');
  assert.equal(catalog.commandsTruncated, 88);
  assert.equal(catalog.commands[0].name, 'fixture-command-0');
  assert.equal(catalog.commands[0].description.length, 200, 'too-long fields are clipped, never dropped');
  assert.equal(catalog.commands[0].argumentHint.length, 80);
  assert.deepEqual(catalog.commands[0].aliases, ['al-0', 'al-1', 'al-2', 'al-3']);
  assert.equal(catalog.commands[1].aliases, undefined);
  assert.deepEqual(catalog.agents, [
    { name: 'Explore', description: 'Fixture explore agent', model: 'sonnet' },
    { name: 'Plan', description: 'Fixture plan agent' },
  ]);
  assert.equal(catalog.outputStyle, 'default');
  assert.deepEqual(catalog.availableOutputStyles, ['default', 'explanatory']);
  assert.equal(catalog.agentsTruncated, 0);
  assert.equal(catalog.stylesTruncated, 0);
  assert.ok(Number.isFinite(catalog.asOf) && catalog.asOf > 0);

  // commands_changed replaces the commands axis wholesale and notifies with
  // the FULL catalog; the other axes ride along untouched.
  peer.send({ jsonrpc: '2.0', id: 3, method: 'session/prompt', params: {
    sessionId: opened.result.sessionId,
    prompt: [{ type: 'text', text: '[fixture:commands-changed] swap the list' }],
  } });
  const changed = await peer.waitFor((message) => message.method === 'session/update'
    && message.params.update.sessionUpdate === '_workass_claude_commands');
  const pushed = changed.params.update.commandCatalog;
  assert.deepEqual(pushed.commands.map((command) => command.name), ['changed-one', 'changed-two']);
  assert.equal(pushed.commandsTruncated, 0);
  assert.equal(pushed.agents.length, 2, 'the notify carries the whole catalog, not just the commands');
  assert.ok(pushed.asOf >= catalog.asOf);
  assert.equal((await peer.waitFor((message) => message.id === 3)).result.stopReason, 'end_turn');

  // Local command output must land as assistant text, not vanish.
  peer.send({ jsonrpc: '2.0', id: 4, method: 'session/prompt', params: {
    sessionId: opened.result.sessionId,
    prompt: [{ type: 'text', text: '[fixture:local-command-output] show usage' }],
  } });
  const localOutput = await peer.waitFor((message) => message.method === 'session/update'
    && message.params.update.sessionUpdate === 'agent_message_chunk'
    && Array.isArray(message.params.update.content));
  assert.deepEqual(localOutput.params.update.content,
    [{ type: 'text', text: 'Fixture local command output: /usage tokens table' }]);
  assert.equal((await peer.waitFor((message) => message.id === 4)).result.stopReason, 'end_turn');
});

// A mid-session engine restart never produces an open reply, and the restarted
// CLI may have been upgraded on disk — so every successful reopen outside a
// session/new|resume|load request re-announces the catalog (spec H3).
test('official Claude SDK host re-announces the catalog when a mid-session restart reopens the query', async (t) => {
  const peer = startHost({ WORKASS_CLAUDE_FIXTURE_COMMAND_CATALOG: '1' });
  t.after(() => peer.child.kill('SIGKILL'));

  peer.send({ jsonrpc: '2.0', id: 1, method: 'initialize', params: {} });
  await peer.waitFor((message) => message.id === 1);
  peer.send({ jsonrpc: '2.0', id: 2, method: 'session/new', params: { cwd: repoRoot, mcpServers: [] } });
  const opened = await peer.waitFor((message) => message.id === 2);
  assert.ok(opened.result.commandCatalog, 'open reply carries the catalog');

  peer.send({ jsonrpc: '2.0', id: 3, method: 'session/prompt', params: {
    sessionId: opened.result.sessionId,
    prompt: [{ type: 'text', text: '[fixture:transport-closes-after-result]' }],
  } });
  assert.equal((await peer.waitFor((message) => message.id === 3)).result.stopReason, 'end_turn');
  const countNotifies = () => peer.messages.filter((message) => message.method === 'session/update'
    && message.params?.update?.sessionUpdate === '_workass_claude_commands').length;
  assert.equal(countNotifies(), 0, 'an open request hands the catalog back on its reply, never as a notify');

  // The dead transport is discovered by the next control write; the reopen it
  // forces is the queryNeedsRestart path.
  peer.send({ jsonrpc: '2.0', id: 4, method: 'session/set_config_option', params: {
    sessionId: opened.result.sessionId, configId: 'mode', value: 'bypassPermissions',
  } });
  assert.equal((await peer.waitFor((message) => message.id === 4)).error, undefined);
  peer.send({ jsonrpc: '2.0', id: 5, method: 'session/prompt', params: {
    sessionId: opened.result.sessionId,
    prompt: [{ type: 'text', text: 'turn after the engine restart' }],
  } });
  const announced = await peer.waitFor((message) => message.method === 'session/update'
    && message.params.update.sessionUpdate === '_workass_claude_commands');
  assert.equal(announced.params.update.commandCatalog.commands.length, 512,
    'the reopened engine re-announces its own full catalog');
  assert.equal((await peer.waitFor((message) => message.id === 5)).result.stopReason, 'end_turn');
  assert.equal(countNotifies(), 1);
});

test('official Claude SDK host rotates the requested id when session/new collides with an existing transcript', async (t) => {
  const peer = startHost({
    WORKASS_CLAUDE_FIXTURE_PERSISTENT_SESSION_FILES: '1',
    WORKASS_CLAUDE_FIXTURE_PREEXISTING_SESSIONS: 'fixture-claude-session',
  });
  t.after(() => peer.child.kill('SIGKILL'));

  peer.send({ jsonrpc: '2.0', id: 1, method: 'initialize', params: {} });
  await peer.waitFor((message) => message.id === 1);
  peer.send({ jsonrpc: '2.0', id: 2, method: 'session/new', params: { cwd: repoRoot, mcpServers: [] } });
  const opened = await peer.waitFor((message) => message.id === 2);
  assert.equal(opened.error, undefined, JSON.stringify(opened));
  assert.ok(opened.result.sessionId);
  assert.notEqual(opened.result.sessionId, 'fixture-claude-session',
    'a fresh session must never adopt a transcript that already exists');

  peer.send({ jsonrpc: '2.0', id: 3, method: 'session/prompt', params: {
    sessionId: opened.result.sessionId,
    prompt: [{ type: 'text', text: 'fresh after rotation' }],
  } });
  const done = await peer.waitFor((message) => message.id === 3);
  assert.equal(done.error, undefined, JSON.stringify(done));
  assert.equal(done.result.stopReason, 'end_turn');
});

 test('native Workass instructions append at creation and exact resume', async (t) => {
  const dir = await mkdtemp(path.join(os.tmpdir(), 'workass-instruction-test-'));
  t.after(() => rm(dir, { recursive: true, force: true }));
  const instructions = path.join(dir, 'instructions.md');
  await writeFile(instructions, 'Fixture Workass instructions.');
  const peer = startHost({ WORKASS_INSTRUCTIONS_FILE: instructions, WORKASS_FIXTURE_NATIVE_INSTRUCTIONS: '1' });
  t.after(() => peer.child.kill('SIGKILL'));
  peer.send({ jsonrpc: '2.0', id: 901, method: 'initialize', params: {} });
  await peer.waitFor(m => m.id === 901);
  peer.send({ jsonrpc: '2.0', id: 902, method: 'session/new', params: { cwd: repoRoot, mcpServers: [] } });
  const created = await peer.waitFor(m => m.id === 902);
  assert.equal(created.error, undefined);
  const sessionId = created.result.sessionId;
  peer.send({ jsonrpc: '2.0', id: 903, method: 'session/prompt', params: { sessionId, prompt: [{ type: 'text', text: 'ordinary request' }] } });
  // Fixtures may request permission; retain the normal approval surface.
  const permission = await peer.waitFor(m => m.method === 'session/request_permission' || m.id === 903);
  if (permission.method) peer.send({ jsonrpc: '2.0', id: permission.id, result: { outcome: { outcome: 'selected', optionId: 'allow_once' } } });
  assert.equal((await peer.waitFor(m => m.id === 903)).error, undefined);
  peer.send({ jsonrpc: '2.0', id: 904, method: 'session/close', params: { sessionId } });
  assert.equal((await peer.waitFor(m => m.id === 904)).error, undefined);
  peer.send({ jsonrpc: '2.0', id: 905, method: 'session/resume', params: { sessionId, cwd: repoRoot, mcpServers: [] } });
  assert.equal((await peer.waitFor(m => m.id === 905)).error, undefined);
 });

async function openFixtureSession(peer) {
  peer.send({ jsonrpc: '2.0', id: 1, method: 'initialize', params: {} });
  await peer.waitFor((message) => message.id === 1);
  peer.send({ jsonrpc: '2.0', id: 2, method: 'session/new', params: { cwd: repoRoot, mcpServers: [] } });
  return (await peer.waitFor((message) => message.id === 2)).result.sessionId;
}

const sessionUpdate = (kind, extra = () => true) => (message) => message.method === 'session/update'
  && message.params.update.sessionUpdate === kind && extra(message.params.update);

// Claude Code 2.1.x folds a message typed while it works into the running turn
// at its next boundary (priority "next") and reports it by uuid; a priority
// "now" steer aborted the tools in flight and cut the turn.
test('Claude Code 2.1 steer folds into the running turn and settles with its one result', async (t) => {
  const peer = startHost();
  t.after(() => peer.child.kill('SIGKILL'));
  const sessionId = await openFixtureSession(peer);
  peer.send({ jsonrpc: '2.0', id: 3, method: 'session/prompt', params: {
    sessionId, clientUserMessageId: 'client-fold-prompt', prompt: [{ type: 'text', text: '[fixture:steer-boundary]' }],
  } });
  await peer.waitFor(sessionUpdate('agent_thought_chunk'));
  peer.send({ jsonrpc: '2.0', id: 4, method: '_workass/claude/steer', params: {
    sessionId, prompt: [{ type: 'text', text: 'redirect please' }], clientUserMessageId: 'client-fold-steer',
  } });
  assert.equal((await peer.waitFor((message) => message.id === 4)).result.receipt, true);
  const receipt = await peer.waitFor(sessionUpdate('_workass_claude_steer_consumed'));
  assert.equal(receipt.params.update.clientUserMessageId, 'client-fold-steer');
  const settled = await peer.waitFor((message) => message.id === 3);
  assert.equal(settled.result.stopReason, 'end_turn');
  const answer = peer.messages.find(sessionUpdate('agent_message_chunk', (update) => String(update.content?.text || '').includes('Fixture folded answer')));
  assert.ok(answer, 'the direction was answered inside the steered turn');
  assert.match(answer.params.update.content.text, /\(next\)/, 'steers use Claude Code\'s own priority');
  assert.ok(peer.messages.indexOf(answer) < peer.messages.indexOf(settled), 'answered before the turn settled');
  const starts = peer.messages.filter(sessionUpdate('_workass_claude_turn', (update) => update.phase === 'started'));
  assert.equal(starts.length, 1, 'a folded message is not a new turn');
});

// 2026-10-11 live incident: a prompt sent while a turn Claude Code started on
// its own was running got folded into that turn. Its result carried the
// harness origin, so the prompt never settled; the job ran for two hours and a
// later steer at it was recorded as never delivered.
test('Claude Code 2.1 prompt folded into a harness-started turn settles with that turn', async (t) => {
  const peer = startHost();
  t.after(() => peer.child.kill('SIGKILL'));
  const sessionId = await openFixtureSession(peer);
  peer.send({ jsonrpc: '2.0', id: 3, method: 'session/prompt', params: {
    sessionId, clientUserMessageId: 'client-before-wake', prompt: [{ type: 'text', text: 'start work [fixture:harness-open]' }],
  } });
  assert.equal((await peer.waitFor((message) => message.id === 3)).result.stopReason, 'end_turn');
  await peer.waitFor(sessionUpdate('_workass_claude_turn', (update) => update.phase === 'started' && update.humanAuthored === false));
  peer.send({ jsonrpc: '2.0', id: 5, method: 'session/prompt', params: {
    sessionId, clientUserMessageId: 'client-folded', prompt: [{ type: 'text', text: 'what is the status?' }],
  } });
  const consumed = await peer.waitFor(sessionUpdate('_workass_input_consumed', (update) => update.clientUserMessageId === 'client-folded'));
  assert.ok(consumed, 'the folded prompt was receipted when Claude Code took it in');
  const settled = await peer.waitFor((message) => message.id === 5);
  assert.equal(settled.result.stopReason, 'end_turn');
  const ended = peer.messages.filter(sessionUpdate('_workass_claude_turn', (update) => update.phase === 'ended' && update.originKind === 'task-notification'));
  assert.equal(ended.length, 1, 'the harness turn ended once, with the prompt');
});

test('Claude Code 2.1 accepts a steer into a turn it started itself', async (t) => {
  const peer = startHost();
  t.after(() => peer.child.kill('SIGKILL'));
  const sessionId = await openFixtureSession(peer);
  peer.send({ jsonrpc: '2.0', id: 3, method: 'session/prompt', params: {
    sessionId, clientUserMessageId: 'client-before-wake-2', prompt: [{ type: 'text', text: 'start work [fixture:harness-open]' }],
  } });
  await peer.waitFor((message) => message.id === 3);
  await peer.waitFor(sessionUpdate('_workass_claude_turn', (update) => update.phase === 'started' && update.humanAuthored === false));
  peer.send({ jsonrpc: '2.0', id: 4, method: '_workass/claude/steer', params: {
    sessionId, prompt: [{ type: 'text', text: 'also check the logs' }], clientUserMessageId: 'client-harness-steer',
  } });
  const reply = await peer.waitFor((message) => message.id === 4);
  assert.equal(reply.error, undefined, `steer into a harness turn was rejected: ${JSON.stringify(reply.error)}`);
  assert.equal(reply.result.receipt, true);
  await peer.waitFor(sessionUpdate('_workass_claude_steer_consumed', (update) => update.clientUserMessageId === 'client-harness-steer'));
  await peer.waitFor(sessionUpdate('_workass_claude_turn', (update) => update.phase === 'ended'));
});

// A subagent's messages carry parent_tool_use_id. Without passing it on, every
// subagent tool call read as the main agent's own, and a labeling run filled
// the chat with 1,200 image reads (DepthVision speed, 2026-10-15). Its text is
// not the main agent's either, and its open calls outlive the main turn.
test('official Claude SDK host files a subagent tool call under its Agent call', async (t) => {
  const peer = startHost();
  t.after(() => peer.child.kill('SIGKILL'));
  peer.send({ jsonrpc: '2.0', id: 1, method: 'initialize', params: { clientInfo: { name: 'test', version: '1' } } });
  await peer.waitFor((message) => message.id === 1);
  peer.send({ jsonrpc: '2.0', id: 2, method: 'session/new', params: { cwd: repoRoot, mcpServers: [] } });
  const sessionId = (await peer.waitFor((message) => message.id === 2)).result.sessionId;
  peer.send({ jsonrpc: '2.0', id: 3, method: 'session/prompt', params: {
    sessionId, prompt: [{ type: 'text', text: '[fixture:subagent-reads] label' }],
  } });
  await peer.waitFor((message) => message.id === 3);
  const read = (message) => message.method === 'session/update' && message.params.update.toolCallId === 'toolu_sub_read';
  const done = await peer.waitFor((message) => read(message) && message.params.update.status === 'completed');
  const updates = peer.messages.filter(read).map((message) => message.params.update);
  for (const update of updates) {
    assert.equal(update._meta?.claudeCode?.parentToolUseId, 'toolu_sub_agent', `unattributed: ${JSON.stringify(update)}`);
  }
  assert.equal(updates.filter((update) => update.status === 'failed').length, 0, 'the main turn ending failed a subagent call');
  assert.equal(done.params.update.sessionUpdate, 'tool_call_update');
  const agentCall = peer.messages.find((message) => message.method === 'session/update' && message.params.update.toolCallId === 'toolu_sub_agent');
  assert.equal(agentCall.params.update._meta?.claudeCode?.parentToolUseId, undefined, 'the main agent call has no parent');
  const text = peer.messages
    .filter((message) => message.method === 'session/update' && message.params.update.sessionUpdate === 'agent_message_chunk')
    .map((message) => JSON.stringify(message.params.update.content)).join('');
  assert.doesNotMatch(text, /SUBAGENT/, 'subagent text reached the main message');
});
