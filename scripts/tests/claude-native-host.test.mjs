import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import path from 'node:path';
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

test('official Claude SDK host keeps the steered turn open until the direction is answered', async (t) => {
  const peer = startHost();
  t.after(() => peer.child.kill('SIGKILL'));

  peer.send({ jsonrpc: '2.0', id: 1, method: 'initialize', params: {} });
  await peer.waitFor((message) => message.id === 1);
  peer.send({ jsonrpc: '2.0', id: 2, method: 'session/new', params: { cwd: repoRoot, mcpServers: [] } });
  const sessionId = (await peer.waitFor((message) => message.id === 2)).result.sessionId;

  peer.send({ jsonrpc: '2.0', id: 3, method: 'session/prompt', params: {
    sessionId,
    prompt: [{ type: 'text', text: 'keep running' }],
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
  assert.equal(peer.messages.some((message) => message.id === 3), false,
    'the steered prompt must still be open when its boundary receipt is emitted');

  // No cancel: the same session/prompt settles on the steered turn's own result.
  const settled = await peer.waitFor((message) => message.id === 3);
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
  const peer = startHost();
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
  const peer = startHost();
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
