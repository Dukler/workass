import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
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
  assert.deepEqual(opened.result.configOptions.map((option) => option.id), ['mode', 'model', 'reasoning_effort']);

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

test('native Codex host maps Workass MCP through the CA-aware stdio bridge', async (t) => {
  const peer = startHost({ WORKASS_CODEX_FIXTURE_REQUIRE_STDIO_MCP: '1' });
  t.after(() => peer.child.kill('SIGKILL'));

  peer.send({ jsonrpc: '2.0', id: 1, method: 'initialize', params: {} });
  await peer.waitFor((message) => message.id === 1);
  peer.send({
    jsonrpc: '2.0', id: 2, method: 'session/new', params: {
      cwd: repoRoot,
      mcpServers: [{
        name: 'workass-browser', command: '/fixture/workass-daemon', args: ['mcp-stdio'],
        env: [
          { name: 'WORKASS_MCP_CA_FILE', value: '/fixture/workass-ca.pem' },
          { name: 'WORKASS_MCP_ENDPOINT', value: 'https://mcp.localhost:8788/workass/mcp/browser' },
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
      name: 'workass-browser', command: '/fixture/workass-daemon', args: ['mcp-stdio'],
      env: [
        { name: 'WORKASS_MCP_CA_FILE', value: '/fixture/workass-ca.pem' },
        { name: 'WORKASS_MCP_ENDPOINT', value: 'https://mcp.localhost:8788/workass/mcp/browser' },
      ],
    },
    { name: 'workass-agent', command: '/fixture/workass-daemon', args: ['mcp-stdio'], env: [] },
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
        name: 'workass-browser', command: '/fixture/workass-daemon', args: ['mcp-stdio'],
        env: [
          { name: 'WORKASS_MCP_CA_FILE', value: '/fixture/workass-ca.pem' },
          { name: 'WORKASS_MCP_ENDPOINT', value: 'https://mcp.localhost:8788/workass/mcp/browser' },
        ],
      }],
    },
  });
  const opened = await peer.waitFor((message) => message.id === 2);
  assert.equal(opened.result, undefined);
  assert.match(opened.error?.message || '', /MCP tool catalog is unavailable for: workass-browser/);
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
        name: 'workass-browser', command: '/fixture/workass-daemon', args: ['mcp-stdio'],
        env: [
          { name: 'WORKASS_MCP_CA_FILE', value: '/fixture/workass-ca.pem' },
          { name: 'WORKASS_MCP_ENDPOINT', value: 'https://mcp.localhost:8788/workass/mcp/browser' },
        ],
      }],
    },
  });
  const opened = await peer.waitFor((message) => message.id === 2);
  assert.equal(opened.result, undefined);
  assert.match(opened.error?.message || '', /workass-browser startup failed: fixture MCP startup failed/);
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
