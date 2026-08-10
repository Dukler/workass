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
  assert.equal(initialized.result._meta.workassCodexSteerRequest, true);
  assert.equal(initialized.result._meta.workassCodexSteerRaceV1, true);

  peer.send({ jsonrpc: '2.0', id: 2, method: 'session/new', params: { cwd: repoRoot, mcpServers: [] } });
  const opened = await peer.waitFor((message) => message.id === 2);
  assert.equal(opened.result.sessionId, 'fixture-codex-thread');
  assert.deepEqual(opened.result.configOptions.map((option) => option.id), ['mode', 'model', 'reasoning_effort']);

  peer.send({ jsonrpc: '2.0', id: 3, method: 'session/prompt', params: {
    sessionId: opened.result.sessionId,
    prompt: [{ type: 'text', text: 'exercise permission' }],
  } });
  const permission = await peer.waitFor((message) => message.method === 'session/request_permission');
  assert.equal(permission.params.toolCall.rawInput.command, 'printf fixture');
  peer.send({ jsonrpc: '2.0', id: permission.id, result: { outcome: { outcome: 'selected', optionId: 'allow_once' } } });
  await peer.waitFor((message) => message.method === 'session/update' && message.params.update.sessionUpdate === 'agent_message_chunk');
  const firstResult = await peer.waitFor((message) => message.id === 3);
  assert.equal(firstResult.result.stopReason, 'end_turn');

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

test('native Codex host opts URL servers into stateless MCP 2026 per session', async (t) => {
  const peer = startHost({ WORKASS_CODEX_FIXTURE_REQUIRE_MCP_2026: '1' });
  t.after(() => peer.child.kill('SIGKILL'));

  peer.send({ jsonrpc: '2.0', id: 1, method: 'initialize', params: {} });
  await peer.waitFor((message) => message.id === 1);
  peer.send({
    jsonrpc: '2.0', id: 2, method: 'session/new', params: {
      cwd: repoRoot,
      mcpServers: [{
        name: 'workass agent',
        url: 'https://mcp.localhost:18788/workass/mcp/agent',
        headers: [{ name: 'Authorization', value: 'Bearer fixture-owner' }],
      }],
    },
  });
  const opened = await peer.waitFor((message) => message.id === 2);
  assert.equal(opened.error, undefined, JSON.stringify(opened));
  assert.equal(opened.result.sessionId, 'fixture-codex-thread');
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
