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
