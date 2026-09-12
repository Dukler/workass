import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { createHash } from 'node:crypto';
import { mkdtemp, readFile, rm } from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import readline from 'node:readline';
import test from 'node:test';
import { fileURLToPath } from 'node:url';

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..');
const host = path.join(repoRoot, 'scripts', 'codex-native-host.mjs');
const fixture = path.join(repoRoot, 'desktop', 'acp', 'mock-codex-app-server.mjs');

function startHost(env = {}) {
  const child = spawn(process.execPath, [host], {
    cwd: repoRoot,
    stdio: ['pipe', 'pipe', 'pipe'],
    env: {
      ...process.env,
      WORKASS_CODEX_EXECUTABLE: process.execPath,
      WORKASS_CODEX_APP_SERVER_ARGS: JSON.stringify([fixture]),
      ...env,
    },
  });
  const messages = [];
  const waiters = [];
  readline.createInterface({ input: child.stdout }).on('line', (line) => {
    const message = JSON.parse(line);
    messages.push(message);
    for (const waiter of [...waiters]) {
      if (!waiter.match(message)) continue;
      waiters.splice(waiters.indexOf(waiter), 1);
      waiter.resolve(message);
    }
  });
  const waitFor = (match, timeout = 3000) => new Promise((resolve, reject) => {
    const existing = messages.find(match);
    if (existing) return resolve(existing);
    const waiter = { match, resolve: null };
    const timer = setTimeout(() => {
      const index = waiters.indexOf(waiter);
      if (index >= 0) waiters.splice(index, 1);
      reject(new Error(`timed out waiting for Codex host message; got ${JSON.stringify(messages)}`));
    }, timeout);
    waiter.resolve = (message) => { clearTimeout(timer); resolve(message); };
    waiters.push(waiter);
  });
  return {
    child, messages, waitFor,
    send: (message) => child.stdin.write(`${JSON.stringify(message)}\n`),
  };
}

test('native Codex host drives app-server directly with turns, steering, permissions, and limits', async (t) => {
  const peer = startHost();
  t.after(() => peer.child.kill('SIGKILL'));

  peer.send({ jsonrpc: '2.0', id: 1, method: 'initialize', params: { protocolVersion: 1, clientInfo: { name: 'test', version: '1' } } });
  const initialized = await peer.waitFor((message) => message.id === 1);
  assert.equal(initialized.result.agentInfo.name, 'Codex');
  assert.equal(initialized.result.agentCapabilities.loadSession, undefined);
  assert.deepEqual(initialized.result.agentCapabilities.sessionCapabilities.resume, {});
  assert.deepEqual(initialized.result.agentCapabilities.mcpCapabilities, { http: false, sse: false });
  assert.equal(initialized.result._meta.workassCodexSteerRequest, true);
  assert.equal(initialized.result._meta.workassCodexSteerRaceV1, true);
	assert.equal(initialized.result._meta.workassStableTurnInputV1, true);
	assert.equal(initialized.result._meta.workassOperationReadbackV1, undefined);

  peer.send({ jsonrpc: '2.0', id: 2, method: 'session/new', params: { cwd: repoRoot, mcpServers: [] } });
  const opened = await peer.waitFor((message) => message.id === 2);
  assert.equal(opened.result.sessionId, 'fixture-codex-thread');
  assert.equal(opened.result._meta.workassProviderRealm.verified, false);
  assert.match(opened.result._meta.workassProviderRealm.installScope, /^install-[0-9a-f]{32}$/);
  assert.deepEqual(opened.result.configOptions.map((option) => option.id), ['service_tier', 'mode', 'model', 'reasoning_effort']);

  peer.send({ jsonrpc: '2.0', id: 3, method: 'session/prompt', params: {
    sessionId: opened.result.sessionId,
    prompt: [{ type: 'text', text: 'exercise permission' }],
	clientUserMessageId: 'workass-operation-1',
  } });
	const consumed = await peer.waitFor((message) => message.method === 'session/update'
	&& message.params.update.sessionUpdate === '_workass_input_consumed');
	assert.equal(consumed.params.update.clientUserMessageId, 'workass-operation-1');
  const permission = await peer.waitFor((message) => message.method === 'session/request_permission');
  assert.equal(permission.params.toolCall.rawInput.command, 'printf fixture');
  peer.send({ jsonrpc: '2.0', id: permission.id, result: { outcome: { outcome: 'selected', optionId: 'allow_once' } } });
  await peer.waitFor((message) => message.method === 'session/update' && message.params.update.sessionUpdate === 'agent_message_chunk');
	const firstResult = await peer.waitFor((message) => message.id === 3);
	assert.equal(firstResult.result.stopReason, 'end_turn');
	peer.send({ jsonrpc: '2.0', id: 31, method: '_workass/turn/reconcile', params: {
	  sessionId: opened.result.sessionId, clientUserMessageId: 'workass-operation-1',
	} });
	assert.equal((await peer.waitFor((message) => message.id === 31)).error.code, -32601);
	peer.send({ jsonrpc: '2.0', id: 33, method: 'session/close', params: { sessionId: opened.result.sessionId } });
	assert.equal((await peer.waitFor((message) => message.id === 33)).error, undefined);
	peer.send({ jsonrpc: '2.0', id: 34, method: 'session/resume', params: {
	  sessionId: opened.result.sessionId, cwd: repoRoot, mcpServers: [],
	} });
	assert.equal((await peer.waitFor((message) => message.id === 34)).error, undefined);

  peer.send({ jsonrpc: '2.0', id: 4, method: 'session/prompt', params: {
    sessionId: opened.result.sessionId,
    prompt: [{ type: 'text', text: 'keep running' }],
  } });
  await peer.waitFor((message) => message.method === 'session/update' && message.params.update.sessionUpdate === 'agent_thought_chunk');
  peer.send({ jsonrpc: '2.0', id: 5, method: '_workass/codex/steer', params: {
    sessionId: opened.result.sessionId,
    prompt: [{ type: 'text', text: 'redirect' }],
    clientUserMessageId: 'codex-client-steer-1',
  } });
  const steer = await peer.waitFor((message) => message.id === 5);
  assert.equal(steer.result.turnId, 'fixture-turn-2', JSON.stringify(steer));
  await peer.waitFor((message) => message.method === 'session/update' && message.params.update.sessionUpdate === '_workass_codex_steer_consumed');
  await peer.waitFor((message) => message.id === 4);

  peer.send({ jsonrpc: '2.0', id: 6, method: '_workass/codex/rate-limits', params: {} });
  const limits = await peer.waitFor((message) => message.id === 6);
  assert.equal(limits.result.rateLimits.primary.usedPercent, 17);
});

test('native Codex host preserves commentary item boundaries across two rapid steers', async (t) => {
  const peer = startHost();
  t.after(() => peer.child.kill('SIGKILL'));

  peer.send({ jsonrpc: '2.0', id: 1, method: 'initialize', params: {} });
  await peer.waitFor((message) => message.id === 1);
  peer.send({ jsonrpc: '2.0', id: 2, method: 'session/new', params: { cwd: repoRoot, mcpServers: [] } });
  const opened = await peer.waitFor((message) => message.id === 2);

  peer.send({ jsonrpc: '2.0', id: 3, method: 'session/prompt', params: {
    sessionId: opened.result.sessionId,
    prompt: [{ type: 'text', text: '[fixture:rapid-steer-commentary] keep the turn open' }],
  } });
  await peer.waitFor((message) => message.method === 'session/update'
    && message.params?.update?.sessionUpdate === 'agent_thought_chunk');

  for (const [id, clientUserMessageId] of [[4, 'rapid-steer-1'], [5, 'rapid-steer-2']]) {
    peer.send({ jsonrpc: '2.0', id, method: '_workass/codex/steer', params: {
      sessionId: opened.result.sessionId,
      prompt: [{ type: 'text', text: clientUserMessageId }],
      clientUserMessageId,
    } });
    assert.equal((await peer.waitFor((message) => message.id === id)).error, undefined);
    await peer.waitFor((message) => message.method === 'session/update'
      && message.params?.update?.sessionUpdate === '_workass_codex_steer_consumed'
      && message.params.update.clientUserMessageId === clientUserMessageId);
  }
  assert.equal((await peer.waitFor((message) => message.id === 3)).result.stopReason, 'end_turn');

  const commentary = peer.messages.filter((message) => message.method === 'session/update'
    && message.params?.update?.sessionUpdate === 'agent_message_chunk');
  assert.equal(commentary.map((message) => message.params.update.content.text).join(''),
    'Steer 1 commentary A. continuation.\n\nSteer 2 commentary A. continuation.');
  assert.ok(commentary.every((message) => message.params.update._meta?.codex?.phase === 'commentary'));
});

test('native Codex host rejects non-live steering without interrupting or queueing the active turn', async (t) => {
  for (const [fixtureRejection, reason] of [
    ['active-turn-not-steerable', 'active-turn-not-steerable'],
    ['no-active-turn', 'no-active-turn'],
  ]) {
    const peer = startHost({ WORKASS_CODEX_FIXTURE_STEER_REJECTION: fixtureRejection });
    t.after(() => peer.child.kill('SIGKILL'));

    peer.send({ jsonrpc: '2.0', id: 1, method: 'initialize', params: {} });
    await peer.waitFor((message) => message.id === 1);
    peer.send({ jsonrpc: '2.0', id: 2, method: 'session/new', params: { cwd: repoRoot, mcpServers: [] } });
    const opened = await peer.waitFor((message) => message.id === 2);
    peer.send({ jsonrpc: '2.0', id: 3, method: 'session/prompt', params: {
      sessionId: opened.result.sessionId,
      prompt: [{ type: 'text', text: 'keep running' }],
    } });
    await peer.waitFor((message) => message.method === 'session/update'
      && message.params?.update?.sessionUpdate === 'agent_thought_chunk');

    peer.send({ jsonrpc: '2.0', id: 4, method: '_workass/codex/steer', params: {
      sessionId: opened.result.sessionId,
      prompt: [{ type: 'text', text: 'do not queue or interrupt this' }],
      clientUserMessageId: `rejected-${fixtureRejection}`,
    } });
    const rejected = await peer.waitFor((message) => message.id === 4);
    assert.equal(rejected.error, undefined);
    assert.equal(rejected.result.disposition, 'rejected');
    assert.equal(rejected.result.reason, reason);
    await assert.rejects(
      peer.waitFor((message) => message.id === 3, 150),
      /timed out waiting for Codex host message/,
      'a rejected steer must leave the original turn running',
    );

    peer.send({ jsonrpc: '2.0', method: 'session/cancel', params: { sessionId: opened.result.sessionId } });
    assert.equal((await peer.waitFor((message) => message.id === 3)).result.stopReason, 'cancelled');
  }
});

test('native Codex host preserves external MCP stdio configuration', async (t) => {
  const peer = startHost({ WORKASS_CODEX_FIXTURE_REQUIRE_STDIO_MCP: '1' });
  t.after(() => peer.child.kill('SIGKILL'));

  peer.send({ jsonrpc: '2.0', id: 1, method: 'initialize', params: {} });
  await peer.waitFor((message) => message.id === 1);
  peer.send({
    jsonrpc: '2.0', id: 2, method: 'session/new', params: {
      cwd: repoRoot,
      mcpServers: [{
        name: 'fixture-browser', command: '/fixture/external-tool-server', args: ['serve-external-mcp'],
        env: [
          { name: 'FIXTURE_MCP_CA_FILE', value: '/fixture/workass-ca.pem' },
          { name: 'FIXTURE_MCP_ENDPOINT', value: 'https://external.invalid/mcp' },
        ],
      }],
    },
  });
  const opened = await peer.waitFor((message) => message.id === 2);
  assert.equal(opened.error, undefined, JSON.stringify(opened));
  assert.equal(opened.result.sessionId, 'fixture-codex-thread');
});

test('native Codex host waits for delayed official MCP readiness and proves every configured catalog', async (t) => {
  const peer = startHost({
    WORKASS_CODEX_FIXTURE_REQUIRE_STDIO_MCP: '1',
    WORKASS_CODEX_FIXTURE_MCP_DELAY_MS: '75',
  });
  t.after(() => peer.child.kill('SIGKILL'));

  const mcpServers = [
    {
      name: 'fixture-browser', command: '/fixture/external-tool-server', args: ['serve-external-mcp'],
      env: [
        { name: 'FIXTURE_MCP_CA_FILE', value: '/fixture/workass-ca.pem' },
        { name: 'FIXTURE_MCP_ENDPOINT', value: 'https://external.invalid/mcp' },
      ],
    },
    { name: 'fixture-agent', command: '/fixture/external-tool-server', args: ['serve-external-mcp'], env: [] },
  ];

  peer.send({ jsonrpc: '2.0', id: 1, method: 'initialize', params: {} });
  await peer.waitFor((message) => message.id === 1);
  peer.send({
    jsonrpc: '2.0', id: 2, method: 'session/new', params: {
      cwd: repoRoot,
      mcpServers,
    },
  });
  const opened = await peer.waitFor((message) => message.id === 2);
  assert.equal(opened.error, undefined, JSON.stringify(opened));
  assert.equal(opened.result.sessionId, 'fixture-codex-thread');
  peer.send({ jsonrpc: '2.0', id: 3, method: 'session/close', params: { sessionId: opened.result.sessionId } });
  await peer.waitFor((message) => message.id === 3);
  peer.send({
    jsonrpc: '2.0', id: 4, method: 'session/resume', params: {
      sessionId: opened.result.sessionId, cwd: repoRoot, mcpServers,
    },
  });
  const resumed = await peer.waitFor((message) => message.id === 4);
  assert.equal(resumed.error, undefined, JSON.stringify(resumed));
});

test('native Codex host refuses a session whose configured MCP has no discovered tools', async (t) => {
  const peer = startHost({
    WORKASS_CODEX_FIXTURE_REQUIRE_STDIO_MCP: '1',
    WORKASS_CODEX_FIXTURE_MCP_EMPTY_CATALOG: '1',
  });
  t.after(() => peer.child.kill('SIGKILL'));

  peer.send({ jsonrpc: '2.0', id: 1, method: 'initialize', params: {} });
  await peer.waitFor((message) => message.id === 1);
  peer.send({
    jsonrpc: '2.0', id: 2, method: 'session/new', params: {
      cwd: repoRoot,
      mcpServers: [{
        name: 'fixture-browser', command: '/fixture/external-tool-server', args: ['serve-external-mcp'],
        env: [
          { name: 'FIXTURE_MCP_CA_FILE', value: '/fixture/workass-ca.pem' },
          { name: 'FIXTURE_MCP_ENDPOINT', value: 'https://external.invalid/mcp' },
        ],
      }],
    },
  });
  const opened = await peer.waitFor((message) => message.id === 2);
  assert.equal(opened.result, undefined);
  assert.match(opened.error?.message || '', /MCP tool catalog is unavailable for: fixture-browser/);
});

test('native Codex host reports terminal MCP startup failure instead of publishing the session', async (t) => {
  const peer = startHost({
    WORKASS_CODEX_FIXTURE_REQUIRE_STDIO_MCP: '1',
    WORKASS_CODEX_FIXTURE_MCP_FAILED: '1',
  });
  t.after(() => peer.child.kill('SIGKILL'));

  peer.send({ jsonrpc: '2.0', id: 1, method: 'initialize', params: {} });
  await peer.waitFor((message) => message.id === 1);
  peer.send({
    jsonrpc: '2.0', id: 2, method: 'session/new', params: {
      cwd: repoRoot,
      mcpServers: [{
        name: 'fixture-browser', command: '/fixture/external-tool-server', args: ['serve-external-mcp'],
        env: [
          { name: 'FIXTURE_MCP_CA_FILE', value: '/fixture/workass-ca.pem' },
          { name: 'FIXTURE_MCP_ENDPOINT', value: 'https://external.invalid/mcp' },
        ],
      }],
    },
  });
  const opened = await peer.waitFor((message) => message.id === 2);
  assert.equal(opened.result, undefined);
  assert.match(opened.error?.message || '', /fixture-browser startup failed: fixture MCP startup failed/);
});

test('native Codex host classifies an absent provisional candidate without parsing in chat state', async (t) => {
  const peer = startHost({ WORKASS_CODEX_FIXTURE_MISSING_RESUME: '1' });
  t.after(() => peer.child.kill('SIGKILL'));

  peer.send({ jsonrpc: '2.0', id: 1, method: 'initialize', params: {} });
  await peer.waitFor((message) => message.id === 1);
  peer.send({ jsonrpc: '2.0', id: 2, method: 'session/resume', params: {
    sessionId: 'fixture-provisional-candidate', cwd: repoRoot, mcpServers: [],
  } });
  const resumed = await peer.waitFor((message) => message.id === 2);
  assert.equal(resumed.error?.code, -32044);
  assert.match(resumed.error?.message || '', /candidate was never materialized/i);
  assert.equal(resumed.result, undefined);
});

test('native Codex compaction is a semantic checkpoint, never synthetic assistant text', async (t) => {
  const peer = startHost();
  t.after(() => peer.child.kill('SIGKILL'));

  peer.send({ jsonrpc: '2.0', id: 1, method: 'initialize', params: {} });
  await peer.waitFor((message) => message.id === 1);
  peer.send({ jsonrpc: '2.0', id: 2, method: 'session/new', params: { cwd: repoRoot, mcpServers: [] } });
  const opened = await peer.waitFor((message) => message.id === 2);

  peer.send({ jsonrpc: '2.0', id: 3, method: 'session/prompt', params: {
    sessionId: opened.result.sessionId,
    prompt: [{ type: 'text', text: '[fixture:compact] continue after native compaction' }],
    clientUserMessageId: 'compact-operation-1',
  } });
  const checkpoint = await peer.waitFor((message) => message.method === 'session/update'
    && message.params?.update?.sessionUpdate === '_workass_compaction');
  assert.equal(checkpoint.params.update.phase, 'checkpoint');
  assert.ok(checkpoint.params.update.checkpointId);
  assert.match(checkpoint.params.update.digest, /^[0-9a-f]{64}$/);
  assert.equal((await peer.waitFor((message) => message.id === 3)).result.stopReason, 'end_turn');
  assert.equal(peer.messages.some((message) => message.params?.update?.sessionUpdate === 'agent_message_chunk'
    && /context compacted/i.test(String(message.params?.update?.content?.text || ''))), false);
});

test('native Codex host preserves image blocks on prompts and live steering', async (t) => {
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
  assert.match(imageAnswer.params.update.content.text, /Fixture image count: 1; data:image\/png;base64/);
  assert.equal((await peer.waitFor((message) => message.id === 3)).result.stopReason, 'end_turn');

  peer.send({ jsonrpc: '2.0', id: 4, method: 'session/prompt', params: {
    sessionId: opened.result.sessionId,
    prompt: [{ type: 'text', text: 'keep running' }],
  } });
  await peer.waitFor((message) => message.method === 'session/update'
    && message.params?.update?.sessionUpdate === 'agent_thought_chunk');
  peer.send({ jsonrpc: '2.0', id: 5, method: '_workass/codex/steer', params: {
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
    && String(message.params?.update?.content?.text || '').includes('Fixture steer image count'));
  assert.match(steerImage.params.update.content.text, /Fixture steer image count: 1; data:image\/jpeg;base64/);
  await peer.waitFor((message) => message.id === 4);
});

// The plumbing guard that protects fs/terminal RPCs must never expire a card a
// person is still reading — resolving it null reads as a cancelled permission
// and denies the tool behind their back (user 2026-07-25).
test('native Codex host lets a permission outlive the plumbing timeout', async (t) => {
  const peer = startHost({ WORKASS_ACP_PEER_TIMEOUT_MS: '120' });
  t.after(() => peer.child.kill('SIGKILL'));

  peer.send({ jsonrpc: '2.0', id: 1, method: 'initialize', params: { protocolVersion: 1, clientInfo: { name: 'test', version: '1' } } });
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

  await peer.waitFor((message) => message.method === 'session/update' && message.params.update.sessionUpdate === 'agent_message_chunk');
  const promptResult = await peer.waitFor((message) => message.id === 3);
  assert.equal(promptResult.result.stopReason, 'end_turn', JSON.stringify(promptResult));
});


test('native Codex retries preserve failure details and terminal authority', async (t) => {
  const peer = startHost();
  t.after(() => peer.child.kill('SIGKILL'));
  peer.send({ id: 1, method: 'initialize', params: {} });
  await peer.waitFor((m) => m.id === 1);
  peer.send({ id: 2, method: 'session/new', params: { cwd: repoRoot, mcpServers: [] } });
  const sessionId = (await peer.waitFor((m) => m.id === 2)).result.sessionId;
  for (const [id, scenario] of [[3, 'retry-failed'], [4, 'retry-recovered']]) {
    peer.send({ id, method: 'session/prompt', params: {
      sessionId, prompt: [{ type: 'text', text: `[fixture:${scenario}]` }],
    } });
    const result = await peer.waitFor((m) => m.id === id);
    if (id === 3) {
      assert.match(result.error?.message || '', /Retry limit exhausted/);
      assert.match(result.error.message, /responseTooManyFailedAttempts/);
      assert.match(result.error.message, /HTTP 503/);
      assert.match(result.error.message, /upstream unavailable/);
      assert.doesNotMatch(result.error.message, /Reconnecting|fixture-bearer-value/);
    } else {
      assert.equal(result.error, undefined);
      assert.equal(result.result.stopReason, 'end_turn');
    }
  }
  const notices = peer.messages.filter((m) => m.params?.update?.sessionUpdate === 'agent_message_chunk')
    .map((m) => m.params.update.content.text).join('');
  assert.match(notices, /responseStreamDisconnected/);
  assert.match(notices, /HTTP 502/);
  assert.match(notices, /upstream connection reset/);
  assert.doesNotMatch(JSON.stringify(peer.messages), /fixture-private-value|fixture-bearer-value|private phrase|with spaces/);
});

test('native Codex upstream WebSocket disconnect preserves partial work and exact resume without replay', async (t) => {
  for (const scenario of ['socket-disconnected', 'socket-disconnected-notification', 'socket-recovered']) {
    const temp = await mkdtemp(path.join(os.tmpdir(), 'workass-codex-socket-'));
    t.after(() => rm(temp, { recursive: true, force: true }));
    const trace = path.join(temp, 'rpc.jsonl');
    const peer = startHost({ WORKASS_CODEX_FIXTURE_RPC_TRACE: trace });
    t.after(() => peer.child.kill('SIGKILL'));
    peer.send({ id: 1, method: 'initialize', params: {} });
    await peer.waitFor((m) => m.id === 1);
    peer.send({ id: 2, method: 'session/new', params: { cwd: repoRoot, mcpServers: [] } });
    const sessionId = (await peer.waitFor((m) => m.id === 2)).result.sessionId;
    peer.send({ id: 3, method: 'session/prompt', params: {
      sessionId, clientUserMessageId: 'socket-original', prompt: [{ type: 'text', text: `[fixture:${scenario}]` }],
    } });
    const failed = await peer.waitFor((m) => m.id === 3);
    if (scenario === 'socket-recovered') {
      assert.equal(failed.error, undefined);
      assert.equal(failed.result.stopReason, 'end_turn', 'a native retry notice cannot poison successful completion');
    } else {
      assert.match(failed.error?.message || '', /websocket closed by server before response\.completed/);
      assert.match(failed.error.message, /responseStreamDisconnected/);
      assert.equal(failed.result, undefined);
    }
    assert.ok(peer.messages.some((m) => m.params?.update?.content?.text === 'Partial work before upstream disconnect.'));

    peer.send({ id: 4, method: 'session/close', params: { sessionId } });
    assert.equal((await peer.waitFor((m) => m.id === 4)).error, undefined);
    peer.send({ id: 5, method: 'session/resume', params: { sessionId, cwd: repoRoot, mcpServers: [] } });
    assert.equal((await peer.waitFor((m) => m.id === 5)).error, undefined);
    peer.send({ id: 6, method: 'session/prompt', params: {
      sessionId, clientUserMessageId: 'socket-distinct', prompt: [{ type: 'text', text: 'A distinct user request.' }],
    } });
    assert.equal((await peer.waitFor((m) => m.id === 6)).result.stopReason, 'end_turn');
    const calls = (await readFile(trace, 'utf8')).trim().split('\n').map(JSON.parse);
    assert.equal(calls.filter((c) => c.method === 'turn/start').length, 2, 'one native admission per distinct user request');
    assert.equal(calls.filter((c) => c.method === 'thread/start').length, 1, 'never replace the native thread');
    assert.equal(calls.filter((c) => c.method === 'thread/resume').length, 1);
    assert.ok(calls.every((c) => c.exactThread !== false));
    assert.ok(calls.every((c) => !['thread/read', 'thread/items/list'].includes(c.method)), 'no terminal polling or replay readback');
    assert.equal(peer.messages.filter((m) => m.id === 3).length, 1, 'one native terminal reply');
  }
});

test('native Codex large exact resume omits display history and preserves native context and current input', async (t) => {
  const temp = await mkdtemp(path.join(os.tmpdir(), 'workass-codex-large-resume-'));
  t.after(() => rm(temp, { recursive: true, force: true }));
  const trace = path.join(temp, 'rpc.jsonl');
  const peer = startHost({ WORKASS_CODEX_FIXTURE_RPC_TRACE: trace, WORKASS_CODEX_FIXTURE_LARGE_RESUME: '1' });
  t.after(() => peer.child.kill('SIGKILL'));
  peer.send({ id: 1, method: 'initialize', params: {} });
  await peer.waitFor((m) => m.id === 1);
  const sessionId = 'fixture-codex-thread';
  peer.send({ id: 2, method: 'session/resume', params: { sessionId, cwd: repoRoot, mcpServers: [] } });
  const resumed = await peer.waitFor((m) => m.id === 2);
  assert.equal(resumed.error, undefined);
  assert.equal(resumed.result.configOptions.find((o) => o.id === 'model').currentValue, 'gpt-fixture');
  assert.equal(resumed.result.configOptions.find((o) => o.id === 'reasoning_effort').currentValue, 'high');
  const currentText = 'Current bounded input. '.repeat(5000);
  peer.send({ id: 3, method: 'session/prompt', params: {
    sessionId, prompt: [{ type: 'text', text: currentText }], clientUserMessageId: 'large-resume-current',
  } });
  assert.equal((await peer.waitFor((m) => m.id === 3)).result.stopReason, 'end_turn');
  peer.send({ id: 4, method: 'session/close', params: { sessionId } });
  assert.equal((await peer.waitFor((m) => m.id === 4)).error, undefined);
  peer.send({ id: 5, method: 'session/resume', params: { sessionId, cwd: repoRoot, mcpServers: [] } });
  assert.equal((await peer.waitFor((m) => m.id === 5)).error, undefined);
  const calls = (await readFile(trace, 'utf8')).trim().split('\n').map(JSON.parse);
  const shapes = calls.filter((c) => c.method === 'fixture/resume-shape');
  assert.deepEqual(shapes.map((c) => c.storedTurns), [512, 513], 'native history survives both attachments and the distinct input');
  assert.ok(shapes.every((c) => c.responseBytes < 2048), 'resume must not transfer the synthetic 4 MiB display history');
  assert.ok(shapes.every((c) => c.returnedTurns === 0 && !c.hasHistoryOverride && !c.hasPathOverride));
  assert.equal(calls.filter((c) => c.method === 'thread/resume').length, 2);
  assert.ok(calls.every((c) => c.exactThread !== false));
  assert.ok(calls.every((c) => !['thread/start', 'thread/read', 'thread/turns/list', 'thread/items/list', 'thread/inject_items'].includes(c.method)));
  const starts = calls.filter((c) => c.method === 'turn/start');
  assert.equal(starts.length, 1, 'only the distinct current input starts a turn');
  assert.equal(starts[0].inputCount, 1);
  const expectedInput = JSON.stringify([{ type: 'text', text: currentText, text_elements: [] }]);
  assert.equal(starts[0].inputBytes, Buffer.byteLength(expectedInput));
  assert.equal(starts[0].inputDigest, createHash('sha256').update(expectedInput).digest('hex'));
  assert.doesNotMatch(JSON.stringify(peer.messages), /historical-fixture-only/);
});

test('native Codex children use stable subagent cards without completing or consuming the parent turn', async (t) => {
  const peer = startHost();
  t.after(() => peer.child.kill('SIGKILL'));
  peer.send({ jsonrpc: '2.0', id: 1, method: 'initialize', params: {} });
  await peer.waitFor((message) => message.id === 1);
  peer.send({ jsonrpc: '2.0', id: 2, method: 'session/new', params: { cwd: repoRoot, mcpServers: [] } });
  const sessionId = (await peer.waitFor((message) => message.id === 2)).result.sessionId;
  peer.send({ jsonrpc: '2.0', id: 3, method: 'session/prompt', params: {
    sessionId, prompt: [{ type: 'text', text: '[fixture:native-agents]' }],
  } });
  await peer.waitFor((message) => message.params?.update?.title === 'interruptAgent');
  const updates = peer.messages.filter((message) => message.method === 'session/update').map((message) => message.params.update);
  const headers = updates.filter((update) => update.toolCallId && update.toolCallId === update._meta?.workassSubagent?.id);
  assert.equal(new Set(headers.map((header) => header.toolCallId)).size, 2);
  assert.equal(headers[0].sessionUpdate, 'tool_call', 'header must establish daemon tool ownership');
  assert.equal(headers[0].status, 'in_progress', 'completed spawn must not complete the child');
  assert.equal(headers[0].content, undefined, 'an empty child message is not an error');
  assert.equal(headers[0].title, 'Review transport lifecycle');
  assert.equal(headers[0]._meta.workassSubagent.model, 'gpt-fixture-mini[high]');
  assert.equal(headers.filter((header) => header.toolCallId === headers[1].toolCallId).at(-1).status, 'cancelled');
  const childTool = updates.find((update) => update.title === 'printf child' && update.status === 'completed');
  assert.ok(childTool);
  assert.equal(childTool.toolCallId, `${headers[0].toolCallId}:command-fixture`);
  assert.equal(childTool._meta.workassSubagent.id, headers[0].toolCallId);
  assert.ok(headers.some((header) => header.status === 'completed' && header.content?.text === 'Child result'));
  assert.equal(updates.some((update) => update.sessionUpdate === 'agent_message_chunk'), false);
  assert.equal(updates.some((update) => update.clientUserMessageId === 'child-input'), false);
  assert.equal(peer.messages.some((message) => message.id === 3), false, 'child completion must not resolve parent prompt');
  peer.send({ jsonrpc: '2.0', id: 4, method: '_workass/codex/steer', params: {
    sessionId, prompt: [{ type: 'text', text: 'finish the parent' }], clientUserMessageId: 'parent-steer',
  } });
  assert.equal((await peer.waitFor((message) => message.id === 4)).result.turnId, 'fixture-turn-1');
  assert.equal((await peer.waitFor((message) => message.id === 3)).result.stopReason, 'end_turn');
  const nextStart = peer.messages.length;
  peer.send({ jsonrpc: '2.0', id: 5, method: 'session/prompt', params: {
    sessionId, prompt: [{ type: 'text', text: '[fixture:native-agents]' }],
  } });
  await peer.waitFor((message) => peer.messages.indexOf(message) >= nextStart && message.params?.update?.title === 'interruptAgent');
  const nextHeaders = peer.messages.slice(nextStart).map((message) => message.params?.update)
    .filter((update) => update?.toolCallId && update.toolCallId === update._meta?.workassSubagent?.id);
  assert.equal(nextHeaders[0].toolCallId, headers[0].toolCallId, 'follow-up must preserve the native child identity');
  assert.equal(nextHeaders[0].sessionUpdate, 'tool_call', 'a new parent turn needs fresh tool ownership');
  assert.equal(nextHeaders[0].status, 'in_progress', 'authoritative running state revives a finished child');
  const firstWork = updates.find((update) => update.sessionUpdate === '_workass_codex_spawned_work' && update.event.toolCallId === headers[0].toolCallId);
  const nextWork = peer.messages.slice(nextStart).map((message) => message.params?.update)
    .find((update) => update?.sessionUpdate === '_workass_codex_spawned_work' && update.event.toolCallId === headers[0].toolCallId);
  assert.equal(firstWork.event.type, 'started');
  assert.equal(nextWork.event.type, 'started');
  assert.notEqual(nextWork.event.taskId, firstWork.event.taskId, 'a new execution must not reuse the previous terminal receipt');
  peer.send({ jsonrpc: '2.0', id: 6, method: '_workass/codex/steer', params: {
    sessionId, prompt: [{ type: 'text', text: 'finish again' }], clientUserMessageId: 'parent-steer-2',
  } });
  assert.equal((await peer.waitFor((message) => message.id === 6)).result.turnId, 'fixture-turn-2');
  assert.equal((await peer.waitFor((message) => message.id === 5)).result.stopReason, 'end_turn');
});

test('native child discovery requires parent provenance and preserves permission and failure routing', async (t) => {
  const peer = startHost();
  t.after(() => peer.child.kill('SIGKILL'));
  peer.send({ jsonrpc: '2.0', id: 1, method: 'initialize', params: {} });
  await peer.waitFor((message) => message.id === 1);
  peer.send({ jsonrpc: '2.0', id: 2, method: 'session/new', params: { cwd: repoRoot, mcpServers: [] } });
  const sessionId = (await peer.waitFor((message) => message.id === 2)).result.sessionId;
  peer.send({ jsonrpc: '2.0', id: 3, method: 'session/prompt', params: {
    sessionId, prompt: [{ type: 'text', text: '[fixture:native-discovery]' }],
  } });
  const permission = await peer.waitFor((message) => message.method === 'session/request_permission');
  assert.equal(permission.params.sessionId, sessionId);
  peer.send({ jsonrpc: '2.0', id: permission.id, result: { outcome: { outcome: 'selected', optionId: 'allow_once' } } });
  assert.equal((await peer.waitFor((message) => message.id === 3)).result.stopReason, 'end_turn');
  const updates = peer.messages.filter((message) => message.method === 'session/update').map((message) => message.params.update);
  assert.ok(updates.some((update) => update.title === 'Transport reviewer' && update.status === 'failed'));
  const activity = updates.filter((update) => update.title === '/root/review');
  assert.deepEqual(activity.map((update) => update.status), ['failed', 'in_progress', 'completed']);
  assert.equal(new Set(updates.filter((update) => update._meta?.workassSubagent).map((update) => update.toolCallId)).size, 1,
    'thread discovery and activity items must update the same child');
  assert.ok(updates.some((update) => update.sessionUpdate === 'agent_message_chunk' && update.content.text === 'Fixture answer'));
  assert.equal(updates.some((update) => update.title === 'FOREIGN'), false);
});


test('Fast is orthogonal to effort and Standard explicitly clears the per-turn override', async (t) => {
  const peer = startHost();
  t.after(() => peer.child.kill('SIGKILL'));
  peer.send({ jsonrpc:'2.0',id:1,method:'initialize',params:{} });
  await peer.waitFor((m)=>m.id===1);
  peer.send({jsonrpc:'2.0',id:2,method:'session/new',params:{cwd:repoRoot,mcpServers:[]}});
  const opened=(await peer.waitFor((m)=>m.id===2)).result;
  assert.deepEqual(opened.availableModels[0].serviceTiers,['default','fast']);
  let id=3;
  for (const tier of ['fast','default']) {
    const configID=id++;
    peer.send({jsonrpc:'2.0',id:configID,method:'session/set_config_option',params:{sessionId:opened.sessionId,configId:'service_tier',value:tier}});
    const config=(await peer.waitFor((m)=>m.id===configID)).result.configOptions;
    assert.equal(config.find((o)=>o.id==='reasoning_effort').currentValue,'high');
    assert.equal(config.find((o)=>o.id==='service_tier').currentValue,tier);
    const promptID=id++;
    peer.send({jsonrpc:'2.0',id:promptID,method:'session/prompt',params:{sessionId:opened.sessionId,prompt:[{type:'text',text:`[fixture:speed:${tier}]`}]}});
    assert.equal((await peer.waitFor((m)=>m.id===promptID)).result?.stopReason,'end_turn');
  }
  peer.send({jsonrpc:'2.0',id:99,method:'session/set_config_option',params:{sessionId:opened.sessionId,configId:'service_tier',value:'invented'}});
  assert.ok((await peer.waitFor((m)=>m.id===99)).error);
});


test('Fast availability follows modern catalog authority and legacy advertised speed tiers', async (t) => {
  for (const catalog of ['legacy','none']) {
    const peer=startHost({WORKASS_CODEX_FIXTURE_SPEED_CATALOG:catalog});
    t.after(()=>peer.child.kill('SIGKILL'));
    peer.send({jsonrpc:'2.0',id:1,method:'initialize',params:{}});
    await peer.waitFor((m)=>m.id===1);
    peer.send({jsonrpc:'2.0',id:2,method:'session/new',params:{cwd:repoRoot,mcpServers:[]}});
    const opened=(await peer.waitFor((m)=>m.id===2)).result;
    assert.deepEqual(opened.availableModels[0].serviceTiers,catalog==='legacy'?['default','fast']:[]);
    assert.deepEqual(opened.availableModels[1].serviceTiers,[]);
    peer.send({jsonrpc:'2.0',id:3,method:'session/set_config_option',params:{sessionId:opened.sessionId,configId:'service_tier',value:'fast'}});
    const reply=await peer.waitFor((m)=>m.id===3);
    if(catalog==='none') { assert.ok(reply.error);continue; }
    assert.ok(reply.result);
    peer.send({jsonrpc:'2.0',id:4,method:'session/prompt',params:{sessionId:opened.sessionId,prompt:[{type:'text',text:'[fixture:speed:fast]'}]}});
    assert.equal((await peer.waitFor((m)=>m.id===4)).result?.stopReason,'end_turn');
  }
});

async function goalPeer(t, env = {}) {
  const peer = startHost(env);
  t.after(() => peer.child.kill('SIGKILL'));
  let sequence = 5000;
  peer.rpc = async (method, params = {}) => {
    const id = ++sequence;
    peer.send({ id, method, params });
    return peer.waitFor((message) => message.id === id);
  };
  await peer.rpc('initialize');
  const opened = await peer.rpc('session/new', { cwd: repoRoot, mcpServers: [] });
  assert.equal(opened.error, undefined);
  peer.sessionId = opened.result.sessionId;
  peer.goal = (text, extra = {}) => peer.rpc('session/prompt', { sessionId: peer.sessionId, prompt: [{ type: 'text', text }], ...extra });
  peer.control = (text, clientId = text) => peer.rpc('_workass/codex/steer', { sessionId: peer.sessionId, prompt: [{ type: 'text', text }], clientUserMessageId: clientId });
  peer.opened = opened;
  return peer;
}

test('native /goal applies settings and context before start, spans native continuations, and reports completion', async (t) => {
  const peer = await goalPeer(t);
  assert.equal(peer.opened.result.commandCatalog.commands[0].name, 'goal');
  await peer.rpc('session/set_config_option', { sessionId: peer.sessionId, configId: 'service_tier', value: 'fast' });
  const result = await peer.goal('Workass context with quoted /goal clear; User request: create it', {
    clientUserMessageId: 'goal-operation',
    _meta: { workassCommandInput: { humanAuthored: true, text: '/goal [fixture:goal-complete] finish the migration' } },
  });
  assert.equal(result.error, undefined, JSON.stringify(result));
  assert.equal(result.result.stopReason, 'end_turn');
  const updates = peer.messages.filter((message) => message.method === 'session/update').map((message) => message.params.update);
  assert.equal(updates.filter((update) => update.content?.text?.includes('Native goal checkpoint.\n')).length, 2);
  assert.equal(updates.filter((update) => update.sessionUpdate === '_workass_input_consumed' && update.clientUserMessageId === 'goal-operation').length, 1);
  assert.ok(updates.some((update) => update.toolCallId === 'workass-native-goal' && update.content?.text.includes('Goal: complete')));
  const status = await peer.goal('/goal');
  assert.equal(status.error, undefined);
  assert.equal((await peer.goal('/goal clear')).error, undefined);
  assert.equal((await peer.goal('/goal resume')).error.code, -32603);
});

test('native /goal live controls get receipts, preserve current work, and Stop pauses continuation', async (t) => {
  const peer = await goalPeer(t);
  const running = peer.goal('/goal [fixture:goal-hold] keep checking');
  await peer.waitFor((message) => message.params?.update?.content?.text?.startsWith('Native goal checkpoint.'));
  const status = await peer.control('/goal', 'goal-status');
  assert.equal(status.result.disposition, 'command-applied');
  const paused = await peer.control('/goal pause', 'goal-pause');
  assert.equal(paused.result.disposition, 'command-applied');
  await peer.waitFor((message) => message.params?.update?.sessionUpdate === '_workass_codex_steer_consumed' && message.params.update.clientUserMessageId === 'goal-pause');
  // Pause between native turns must settle without manufacturing turn/start.
  // If the second turn already began, explicit Stop owns its interruption.
  peer.send({ method: 'session/cancel', params: { sessionId: peer.sessionId } });
  await running;
  const resumed = peer.goal('/goal resume');
  const secondStatus = await peer.control('/goal');
  assert.equal(secondStatus.error, undefined);
  peer.send({ method: 'session/cancel', params: { sessionId: peer.sessionId } });
  assert.equal((await resumed).result.stopReason, 'cancelled');
  assert.equal((await peer.goal('/goal')).error, undefined);
  assert.ok(peer.messages.some((message) => message.params?.update?.content?.text?.includes('Goal: paused')));
});

test('native goal state is read on exact resume and blocked goals terminate the owned run', async (t) => {
  const peer = await goalPeer(t);
  assert.equal((await peer.goal('/goal [fixture:goal-blocked] cannot proceed')).error, undefined);
  await peer.rpc('session/close', { sessionId: peer.sessionId });
  const resumed = await peer.rpc('session/resume', { sessionId: peer.sessionId, cwd: repoRoot, mcpServers: [] });
  assert.equal(resumed.error, undefined);
  const count = peer.messages.length;
  assert.equal((await peer.goal('/goal')).error, undefined);
  assert.ok(peer.messages.slice(count).some((message) => message.params?.update?.content?.text?.includes('Goal: blocked')));
  assert.equal((await peer.goal('/goal different objective')).error.code, -32603);
});

test('native goals fail closed on missing capability/settings and never parse internal context as intent', async (t) => {
  for (const env of [{ WORKASS_CODEX_FIXTURE_GOALS_UNAVAILABLE: '1' }, { WORKASS_CODEX_FIXTURE_GOAL_SETTINGS_FAIL: '1' }]) {
    const peer = await goalPeer(t, env);
    const result = await peer.goal('/goal [fixture:goal-complete] do work');
    assert.ok(result.error);
    assert.ok(!peer.messages.some((message) => message.params?.update?.content?.text?.startsWith('Native goal checkpoint.')));
  }
  const peer = await goalPeer(t);
  const internal = await peer.goal('/goal quoted command', { _meta: { workassCommandInput: { humanAuthored: false, text: '/goal quoted command' } } });
  assert.equal(internal.error, undefined);
  assert.ok(!peer.messages.some((message) => message.params?.update?.toolCallId === 'workass-native-goal'));
  const attachment = await peer.goal('/goal images', { prompt: [{ type: 'image', mimeType: 'image/png', data: 'AA==' }, { type: 'text', text: '/goal images' }] });
  assert.ok(attachment.error);
});

test('native goal budget and usage limits settle, and cancellation before setup never starts inference', async (t) => {
  for (const status of ['usageLimited', 'budgetLimited']) {
    const peer = await goalPeer(t);
    const result = await peer.goal(`/goal [fixture:goal-${status}] stop at the native limit`);
    assert.equal(result.error, undefined);
    assert.ok(peer.messages.some((message) => message.params?.update?.content?.text?.includes(`Goal: ${status}`)));
  }
  const peer = await goalPeer(t);
  const pending = peer.goal('/goal [fixture:goal-hold] must not start');
  peer.send({ method: 'session/cancel', params: { sessionId: peer.sessionId } });
  await pending;
  const queried = await peer.goal('/goal');
  assert.equal(queried.error, undefined);
  assert.ok(!peer.messages.some((message) => message.params?.update?.content?.text?.startsWith('Native goal checkpoint.')));
});

function runtimeDiagnostics(peer, clientId) {
  return peer.messages.filter((message) => message.params?.update?.sessionUpdate === '_workass_diagnostic'
    && (clientId === undefined || message.params.update.clientUserMessageId === clientId)).map((message) => message.params.update);
}

test('native diagnostics preserve current-input correlation through retries, compaction, usage, and successful completion', async (t) => {
  const peer = await goalPeer(t);
  assert.deepEqual(runtimeDiagnostics(peer), [], 'attachment/history emits no prompt diagnostics');
  await peer.rpc('session/set_config_option', { sessionId: peer.sessionId, configId: 'service_tier', value: 'fast' });
  const text = '[fixture:diagnostic] café 🦊';
  const data = 'AAECAw==';
  const result = await peer.goal(text, { clientUserMessageId: 'diagnostic-first', prompt: [
    { type: 'text', text }, { type: 'image', mimeType: 'image/png', data },
  ] });
  assert.equal(result.result?.stopReason, 'end_turn');
  const updates = runtimeDiagnostics(peer, 'diagnostic-first');
  assert.ok(updates.length > 5);
  assert.ok(updates.every((update) => update.schemaVersion === 1));
  const events = updates.map((update) => update.event);
  const input = events.find((event) => event.kind === 'input');
  assert.equal(input.inputBytes, Buffer.byteLength(JSON.stringify([
    { type: 'text', text, text_elements: [] }, { type: 'image', url: `data:image/png;base64,${data}` },
  ])));
  assert.equal(input.textBytes, Buffer.byteLength(text));
  assert.equal(input.imageCount, 1);
  assert.equal(input.imageDataBytes, Buffer.byteLength(data));
  assert.equal(input.effort, 'high');
  assert.equal(input.serviceTier, 'priority');
  assert.equal(input.resumed, false);
  assert.equal(input.historyMode, 'unknown');
  assert.match(input.hostInstanceId, /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/);
  assert.ok(input.resumeReplyBytes > 0);
  assert.ok(Number.isSafeInteger(input.resumeElapsedMs) && input.resumeElapsedMs >= 0);
  assert.deepEqual(events.filter((event) => event.kind === 'error'), [{ kind: 'error', willRetry: true,
    category: 'stream_disconnected', reason: 'peer_closed', closeDetailsAvailable: false, httpStatus: 502, retryAttempt: 2, retryLimit: 5 }]);
  assert.deepEqual(events.filter((event) => event.kind === 'fallback'), [{ kind: 'fallback', transport: 'https', scope: 'thread' }]);
  assert.deepEqual(events.filter((event) => event.kind === 'compaction').map((event) => event.phase), ['started', 'completed']);
  assert.deepEqual(events.filter((event) => event.kind === 'turn').map((event) => event.phase), ['started', 'completed']);
  assert.deepEqual(events.filter((event) => event.kind === 'usage'), [
    { kind: 'usage', used: 123, size: 1000, input: 100, cachedInput: 20, output: 23, reasoningOutput: 7 },
    { kind: 'usage', used: 0 },
    { kind: 'usage', used: 123, size: 1000, input: 100, cachedInput: 20, output: 23, reasoningOutput: 7 },
  ]);
  assert.doesNotMatch(JSON.stringify(events), /fixture-private|fixture-key|private phrase|fixture-credential|fixture-bearer|fixture-warning|private\.invalid|native-id|\/private|api_key|password|credential|Bearer|additionalDetails|socketCode|café/);
  const consumed = peer.messages.findIndex((message) => message.params?.update?.sessionUpdate === '_workass_input_consumed');
  const completion = peer.messages.findIndex((message) => message.params?.update?.event?.phase === 'completed' && message.params.update.event.kind === 'turn');
  assert.ok(completion > consumed, 'consumption must not erase the diagnostic client id');
});

test('native diagnostics fence stale and wrong-thread events and use only observed prior usage', async (t) => {
  const peer = await goalPeer(t);
  assert.equal((await peer.goal('[fixture:diagnostic]', { clientUserMessageId: 'fence-one' })).result.stopReason, 'end_turn');
  const first = runtimeDiagnostics(peer, 'fence-one');
  assert.equal((await peer.goal('[fixture:diagnostic]', { clientUserMessageId: 'fence-two' })).result.stopReason, 'end_turn');
  assert.deepEqual(runtimeDiagnostics(peer, 'fence-one'), first, 'the retired input cannot gain late events');
  const second = runtimeDiagnostics(peer, 'fence-two').map((update) => update.event);
  assert.deepEqual(second.filter((event) => event.prior), [
    { kind: 'usage', used: 123, size: 1000, input: 100, cachedInput: 20, output: 23, reasoningOutput: 7, prior: true },
  ]);
  assert.equal(second.filter((event) => event.kind === 'error').length, 1);
  assert.deepEqual(second.filter((event) => event.kind === 'fallback'), [{ kind: 'fallback', transport: 'https', scope: 'thread' }]);
  assert.equal(second.some((event) => event.category === 'authentication' || [888, 999].includes(event.used)), false);
  assert.deepEqual(second.filter((event) => event.kind === 'turn').map((event) => event.phase), ['started', 'completed']);
  assert.deepEqual(second.filter((event) => event.kind === 'compaction').map((event) => event.phase), ['started', 'completed']);
  assert.equal(second.find((event) => event.kind === 'input').hostInstanceId, first.find((update) => update.event.kind === 'input').event.hostInstanceId);
});

test('native diagnostics record terminal exhaustion without inventing fallback or close details', async (t) => {
  const peer = await goalPeer(t);
  const result = await peer.goal('[fixture:retry-failed]', { clientUserMessageId: 'diagnostic-failure' });
  assert.ok(result.error);
  const events = runtimeDiagnostics(peer).map((update) => update.event);
  assert.deepEqual(events.filter((event) => event.kind === 'error').map(({ category, willRetry, httpStatus }) => ({ category, willRetry, httpStatus })), [
    { category: 'stream_disconnected', willRetry: true, httpStatus: 502 },
    { category: 'retry_exhausted', willRetry: false, httpStatus: 503 },
  ]);
  assert.equal(events.filter((event) => event.kind === 'error').at(-1).reason, 'retry_exhausted');
  assert.equal(events.some((event) => event.kind === 'fallback'), false);
  assert.deepEqual(events.filter((event) => event.kind === 'turn').map((event) => event.phase), ['started', 'failed']);
  assert.ok(events.filter((event) => event.kind === 'error').every((event) => event.closeDetailsAvailable === false));
});

test('native diagnostics measure exact resume RPC line bytes without replaying history', async (t) => {
  const temp = await mkdtemp(path.join(os.tmpdir(), 'workass-codex-diagnostic-resume-'));
  t.after(() => rm(temp, { recursive: true, force: true }));
  const hosts = new Set();
  for (const mode of ['paginated', 'legacy', 'unrecognized-private-mode']) {
    const trace = path.join(temp, `${mode}.jsonl`);
    const peer = await goalPeer(t, { WORKASS_CODEX_FIXTURE_DIAGNOSTIC_TRACE: trace,
      WORKASS_CODEX_FIXTURE_HISTORY_MODE: mode, WORKASS_CODEX_FIXTURE_LARGE_RESUME: '1' });
    await peer.rpc('session/close', { sessionId: peer.sessionId });
    assert.equal((await peer.rpc('session/resume', { sessionId: peer.sessionId, cwd: repoRoot, mcpServers: [] })).error, undefined);
    assert.deepEqual(runtimeDiagnostics(peer), []);
    await peer.goal('Distinct current input', { clientUserMessageId: `resume-${mode}` });
    const input = runtimeDiagnostics(peer).find((update) => update.event.kind === 'input').event;
    const receipts = (await readFile(trace, 'utf8')).trim().split('\n').map(JSON.parse);
    assert.equal(input.resumeReplyBytes, receipts.at(-1).replyBytes, 'count the JSON-RPC envelope, not reserialized result bytes');
    assert.equal(input.resumed, true);
    assert.equal(input.historyMode, mode === 'unrecognized-private-mode' ? 'unknown' : mode);
    assert.ok(input.resumeReplyBytes < 2048);
    assert.ok(Number.isSafeInteger(input.resumeElapsedMs) && input.resumeElapsedMs >= 0);
    hosts.add(input.hostInstanceId);
    assert.doesNotMatch(JSON.stringify(runtimeDiagnostics(peer).map((update) => update.event)), /historical-fixture-only|fixture-codex-thread|unrecognized-private-mode/);
  }
  assert.equal(hosts.size, 3, 'the opaque host identity changes across host processes');
});

test('native diagnostics require a client id and correlate goal continuations and cancellation to the owning input', async (t) => {
  const peer = await goalPeer(t);
  await peer.goal('[fixture:diagnostic]');
  assert.deepEqual(runtimeDiagnostics(peer), []);
  await peer.goal('/goal [fixture:goal-complete] finish', { clientUserMessageId: 'diagnostic-goal' });
  const events = runtimeDiagnostics(peer, 'diagnostic-goal').map((update) => update.event);
  assert.deepEqual(events.filter((event) => event.kind === 'turn').map((event) => event.phase), ['started', 'completed', 'started', 'completed']);
  assert.equal(events.filter((event) => event.kind === 'input').length, 1);
  await peer.goal('/goal clear');
  const running = peer.goal('keep running', { clientUserMessageId: 'diagnostic-cancel' });
  await peer.waitFor((message) => message.params?.update?.clientUserMessageId === 'diagnostic-cancel' && message.params.update.event?.phase === 'started');
  peer.send({ method: 'session/cancel', params: { sessionId: peer.sessionId } });
  assert.equal((await running).result.stopReason, 'cancelled');
  assert.deepEqual(runtimeDiagnostics(peer, 'diagnostic-cancel').filter((update) => update.event.kind === 'turn').map((update) => update.event.phase), ['started', 'interrupted']);
});

test('native diagnostic error classification accepts only neutral enums and native numeric HTTP status', async (t) => {
  for (const [codexErrorInfo, message, category, reason, status, additionalDetails] of [
    [{ httpConnectionFailed: { httpStatusCode: 401 } }, 'connection failed', 'connection_failed', 'connection_failed', 401],
    ['contextWindowExceeded', 'context full', 'context_limit', 'other'],
    ['rateLimitExceeded', 'limit', 'rate_limit', 'other'],
    ['unauthorized', 'login required', 'authentication', 'other'],
    ['internalServerError', 'server failed', 'server_error', 'other'],
    [{ responseStreamDisconnected: { httpStatusCode: '502' } }, 'stream idle timeout', 'stream_disconnected', 'idle_timeout'],
    ['responseStreamDisconnected', 'Reconnecting', 'stream_disconnected', 'peer_closed', undefined,
      'websocket closed by server before response.completed; token=fixture-private; https://private.invalid/native-id; /private/socket; password="private phrase"'],
    ['responseStreamDisconnected', 'Reconnecting', 'stream_disconnected', 'idle_timeout', undefined,
      'stream idle timeout; api_key=fixture-private; Bearer fixture-private'],
    ['responseStreamDisconnected', 'Reconnecting', 'stream_disconnected', 'other', undefined, { message: 'peer closed' }],
    [{ 'secret-shaped-native-category': { httpStatusCode: -1 } }, 'token=fixture-value', 'other', 'other'],
    ['toString', 'unrecognized inherited property', 'other', 'other'],
  ]) {
    const peer = await goalPeer(t, { WORKASS_CODEX_FIXTURE_DIAGNOSTIC_ERROR: JSON.stringify({ codexErrorInfo, message, additionalDetails }) });
    assert.equal((await peer.goal('[fixture:error-category]', { clientUserMessageId: 'category-input' })).result.stopReason, 'end_turn');
    const errors = runtimeDiagnostics(peer).filter((update) => update.event.kind === 'error').map((update) => update.event);
    assert.deepEqual(errors, [{ kind: 'error', willRetry: true, category, reason, closeDetailsAvailable: false,
      ...(status ? { httpStatus: status } : {}) }]);
  }
});

test('native fallback warnings without turn ids are counted only as active exact-thread observations', async (t) => {
  for (const [message, count] of [
    ['Falling back from WebSockets to HTTP(S) transport. token=fixture-private', 1],
    ['Falling back from WebSockets to HTTPS transport. password="private phrase"', 1],
    ['Reconnecting... 2/5', 0],
    ['WebSocket transport warning', 0],
    ['Falling back from WebSockets to HTTP transport.', 0],
  ]) {
    const peer = await goalPeer(t, { WORKASS_CODEX_FIXTURE_WARNING: message });
    for (const clientUserMessageId of ['warning-first', 'warning-second']) {
      assert.equal((await peer.goal('[fixture:fallback-warning]', { clientUserMessageId })).result.stopReason, 'end_turn');
      const fallbacks = runtimeDiagnostics(peer, clientUserMessageId).filter((update) => update.event.kind === 'fallback');
      assert.deepEqual(fallbacks.map((update) => update.event), Array.from({ length: count }, () => ({
        kind: 'fallback', transport: 'https', scope: 'thread',
      })), 'wrong-thread, missing-thread, and idle warnings cannot add observations');
    }
    assert.equal(runtimeDiagnostics(peer).filter((update) => update.event.kind === 'fallback').length, count * 2);
  }
});

test('legacy native compaction emits one diagnostic completion and leaves the checkpoint contract intact', async (t) => {
  const peer = await goalPeer(t);
  assert.equal((await peer.goal('[fixture:compact]', { clientUserMessageId: 'legacy-compaction' })).result.stopReason, 'end_turn');
  assert.deepEqual(runtimeDiagnostics(peer).filter((update) => update.event.kind === 'compaction').map((update) => update.event), [
    { kind: 'compaction', phase: 'completed' },
  ]);
  assert.equal(peer.messages.filter((message) => message.params?.update?.sessionUpdate === '_workass_compaction').length, 1);
});

test('legacy compaction without a turn id cannot claim diagnostic turn attribution', async (t) => {
  const peer = await goalPeer(t);
  assert.equal((await peer.goal('[fixture:compact-missing-turn]', { clientUserMessageId: 'unscoped-compaction' })).result.stopReason, 'end_turn');
  assert.deepEqual(runtimeDiagnostics(peer).filter((update) => update.event.kind === 'compaction'), []);
  assert.equal(peer.messages.filter((message) => message.params?.update?.sessionUpdate === '_workass_compaction').length, 1,
    'the existing semantic checkpoint behavior remains unchanged');
});
