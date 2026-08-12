// Hostile error-path suite for scripts/claude-native-host.mjs, driven by the
// fixture SDK's WORKASS_CLAUDE_FIXTURE_* hostile behaviors. Each test models a
// failure the REAL Claude CLI/SDK can produce but the mock was historically too
// kind to reproduce (the "Session ID … is already in use" wedge lived for weeks
// behind exactly that kindness). The 2026-07-28 audit found four defects here
// (stranded tool rows poisoning the OAuth retry, fork-on-resume context loss,
// invisible compaction); they were fixed the same night and this suite now
// asserts the FIXED behavior.

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
  const rawLines = [];
  const parseFailures = [];
  const stderrChunks = [];
  const waiters = [];
  child.stderr.on('data', (chunk) => stderrChunks.push(String(chunk)));
  lines.on('line', (line) => {
    rawLines.push(line);
    let message;
    try {
      message = JSON.parse(line);
    } catch {
      // stdout is protocol JSON only; anything else is a purity violation the
      // tests assert on via parseFailures.
      parseFailures.push(line);
      return;
    }
    messages.push(message);
    for (const waiter of [...waiters]) {
      if (waiter.match(message)) {
        waiters.splice(waiters.indexOf(waiter), 1);
        waiter.resolve(message);
      }
    }
  });
  const waitFor = (match, timeout = 5000) => new Promise((resolve, reject) => {
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
  const stderrText = () => stderrChunks.join('');
  return { child, messages, rawLines, parseFailures, stderrText, waitFor, send };
}

function answerChunks(peer) {
  return peer.messages
    .filter((message) => message.method === 'session/update'
      && message.params?.update?.sessionUpdate === 'agent_message_chunk')
    .map((message) => String(message.params.update.content?.text || ''));
}

async function openDefaultSession(peer) {
  peer.send({ jsonrpc: '2.0', id: 1, method: 'initialize', params: {} });
  await peer.waitFor((message) => message.id === 1);
  peer.send({ jsonrpc: '2.0', id: 2, method: 'session/new', params: { cwd: repoRoot, mcpServers: [] } });
  const opened = await peer.waitFor((message) => message.id === 2);
  assert.equal(opened.error, undefined, JSON.stringify(opened));
  return opened.result.sessionId;
}

// (a) OAuth expiry striking AFTER a successful turn. The real CLI brackets the
// refresh attempt with an auth_status frame (free text, token material inside)
// and then fails the turn with the refresh-failure result. The host must retry
// the untouched prompt once against a resumed query, and none of the auth
// traffic may reach the wire.
test('hostile: mid-session OAuth expiry after a successful turn recovers without leaking auth traffic', async (t) => {
  const peer = startHost({ WORKASS_CLAUDE_FIXTURE_OAUTH_EXPIRY_MARKER: '1' });
  t.after(() => peer.child.kill('SIGKILL'));

  const sessionId = await openDefaultSession(peer);

  peer.send({ jsonrpc: '2.0', id: 3, method: 'session/prompt', params: {
    sessionId, prompt: [{ type: 'text', text: 'warm up turn' }],
  } });
  const first = await peer.waitFor((message) => message.id === 3);
  assert.equal(first.result.stopReason, 'end_turn');

  peer.send({ jsonrpc: '2.0', id: 4, method: 'session/prompt', params: {
    sessionId, prompt: [{ type: 'text', text: '[fixture:oauth-once] continue the work' }],
  } });
  const second = await peer.waitFor((message) => message.id === 4);
  assert.equal(second.error, undefined, JSON.stringify(second));
  assert.equal(second.result.stopReason, 'end_turn');

  // Exactly one visible answer per turn: the failed attempt's buffered auth
  // text was discarded, the retried attempt produced the real answer.
  assert.deepEqual(answerChunks(peer), ['Fixture answer', 'Fixture answer']);
  const wire = peer.rawLines.join('\n');
  assert.ok(!wire.includes('Failed to authenticate'), 'auth error text must never reach the wire');
  assert.ok(!wire.includes('sk-ant-oat01-FIXTURE-AUTH-SECRET'),
    'auth_status frames carry token material and must never be forwarded');
  assert.deepEqual(peer.parseFailures, []);
});

// (b) Transport death mid-tool-call: the CLI emitted tool_use and died before
// any tool_result or terminal result. The turn fails with a clear error, the
// next prompt recovers, the stranded tool row is closed with a terminal failed
// update, and the cleared ledger no longer vetoes the transient-OAuth retry.
test('hostile: transport death mid-tool-call fails the turn, closes the stranded tool row, and keeps the OAuth retry alive', async (t) => {
  const peer = startHost({
    WORKASS_CLAUDE_FIXTURE_TOOL_STREAM_DEATH: '1',
    WORKASS_CLAUDE_FIXTURE_OAUTH_EXPIRY_MARKER: '1',
  });
  t.after(() => peer.child.kill('SIGKILL'));

  const sessionId = await openDefaultSession(peer);

  peer.send({ jsonrpc: '2.0', id: 3, method: 'session/prompt', params: {
    sessionId, prompt: [{ type: 'text', text: '[fixture:tool-then-close] run something' }],
  } });
  const started = await peer.waitFor((message) => message.method === 'session/update'
    && message.params?.update?.sessionUpdate === 'tool_call'
    && message.params.update.toolCallId === 'fixture-dead-tool-1');
  assert.equal(started.params.update.status, 'in_progress');
  const failed = await peer.waitFor((message) => message.id === 3);
  assert.match(failed.error?.message || '', /closed before a terminal result/i);

  // Sane half: the session is not wedged — the next plain prompt completes.
  peer.send({ jsonrpc: '2.0', id: 4, method: 'session/prompt', params: {
    sessionId, prompt: [{ type: 'text', text: 'plain recovery turn' }],
  } });
  const recovered = await peer.waitFor((message) => message.id === 4);
  assert.equal(recovered.error, undefined, JSON.stringify(recovered));
  assert.equal(recovered.result.stopReason, 'end_turn');

  // Fixed (stuck-ui, 2026-07-28): the tool row opened by the dead transport is
  // closed with exactly one terminal failed update the moment the turn settles.
  const deadToolUpdates = peer.messages.filter((message) => message.method === 'session/update'
    && message.params?.update?.sessionUpdate === 'tool_call_update'
    && message.params.update.toolCallId === 'fixture-dead-tool-1');
  assert.equal(deadToolUpdates.length, 1, JSON.stringify(deadToolUpdates));
  assert.equal(deadToolUpdates[0].params.update.status, 'failed');

  // Fixed (wedge, 2026-07-28): the cleared ledger no longer vetoes the
  // transient-OAuth retry — the identical expiry a clean session recovers from
  // (previous test) recovers here too.
  peer.send({ jsonrpc: '2.0', id: 5, method: 'session/prompt', params: {
    sessionId, prompt: [{ type: 'text', text: '[fixture:oauth-once] after the dead tool' }],
  } });
  const retried = await peer.waitFor((message) => message.id === 5);
  assert.equal(retried.error, undefined, JSON.stringify(retried));
  assert.equal(retried.result.stopReason, 'end_turn');
  // The buffered auth text still must not leak even on this path.
  assert.deepEqual(answerChunks(peer).filter((text) => text.includes('Failed to authenticate')), []);
  assert.deepEqual(peer.parseFailures, []);
});

// (c) Context compaction mid-turn: system{status:compacting} and
// system{compact_boundary} arrive between the prompt and its answer. The turn
// completes untouched AND the pause is surfaced: the turn pulse flips to
// 'compacting' immediately, then back to 'waiting' at the boundary.
test('hostile: compact_boundary frames pass through without corrupting the turn', async (t) => {
  const peer = startHost({ WORKASS_CLAUDE_FIXTURE_COMPACT_BOUNDARY: '1' });
  t.after(() => peer.child.kill('SIGKILL'));

  const sessionId = await openDefaultSession(peer);

  peer.send({ jsonrpc: '2.0', id: 3, method: 'session/prompt', params: {
    sessionId, prompt: [{ type: 'text', text: '[fixture:compact] summarize the long session' }],
  } });
  const done = await peer.waitFor((message) => message.id === 3);
  assert.equal(done.error, undefined, JSON.stringify(done));
  assert.equal(done.result.stopReason, 'end_turn');
  assert.ok(answerChunks(peer).includes('Fixture answer'));

  // Fixed (2026-07-28): compaction reaches the wire as heartbeat phases, so
  // the client can label the multi-second pause instead of showing dead air.
  const phases = peer.messages
    .filter((message) => message.method === 'session/update'
      && message.params?.update?.sessionUpdate === '_workass_claude_turn_heartbeat')
    .map((message) => message.params.update.phase);
  assert.ok(phases.includes('compacting'), `heartbeat phases: ${JSON.stringify(phases)}`);
  assert.ok(phases.includes('waiting'), 'the boundary must flip the pulse back');

  const compaction = peer.messages
    .filter((message) => message.method === 'session/update'
      && message.params?.update?.sessionUpdate === '_workass_compaction')
    .map((message) => message.params.update);
  assert.equal(compaction[0]?.phase, 'started');
  assert.equal(compaction[1]?.phase, 'checkpoint');
  assert.ok(compaction[1]?.checkpointId);
  assert.match(compaction[1]?.digest || '', /^[a-f0-9]{64}$/);
  assert.deepEqual(answerChunks(peer).filter((text) => /context compact/i.test(text)), []);

  peer.send({ jsonrpc: '2.0', id: 4, method: 'session/prompt', params: {
    sessionId, prompt: [{ type: 'text', text: 'follow-up after compaction' }],
  } });
  const followUp = await peer.waitFor((message) => message.id === 4);
  assert.equal(followUp.result.stopReason, 'end_turn');
  assert.deepEqual(peer.parseFailures, []);
});

// (d) Fork-on-resume: the real CLI's fork family (forkSession, /clear,
// conversation_reset) continues a resumed conversation under a NEW session id;
// the old id's transcript is frozen at the fork point. The fixture's depth
// probe answers with how many user turns the transcript actually ingested, so
// context continuity is directly observable.
test('hostile: fork-on-resume — the host adopts the forked id, so no turns fall out of context', async (t) => {
  const peer = startHost({ WORKASS_CLAUDE_FIXTURE_FORK_ON_RESUME: '1' });
  t.after(() => peer.child.kill('SIGKILL'));

  const sessionId = await openDefaultSession(peer);
  assert.equal(sessionId, 'fixture-claude-session');

  // Turn 1 runs on the fresh query; the transport dies after its result.
  peer.send({ jsonrpc: '2.0', id: 3, method: 'session/prompt', params: {
    sessionId, prompt: [{ type: 'text', text: '[fixture:depth][fixture:transport-closes-after-result] uno' }],
  } });
  await peer.waitFor((message) => message.id === 3);
  assert.ok(answerChunks(peer).includes('Fixture history depth: 1'));

  // The dead transport is discovered by the next control call and replaced;
  // the replacement RESUMES, which is where the real CLI forks.
  peer.send({ jsonrpc: '2.0', id: 4, method: 'session/set_config_option', params: {
    sessionId, configId: 'model', value: 'claude-haiku-fixture',
  } });
  assert.equal((await peer.waitFor((message) => message.id === 4)).error, undefined);

  peer.send({ jsonrpc: '2.0', id: 5, method: 'session/prompt', params: {
    sessionId, prompt: [{ type: 'text', text: '[fixture:depth][fixture:transport-closes-after-result] dos' }],
  } });
  const second = await peer.waitFor((message) => message.id === 5);
  assert.equal(second.result.stopReason, 'end_turn');
  // The fork copied turn 1, so the immediate post-fork turn still sees full
  // context — this is why the bug is invisible until the SECOND replacement.
  assert.ok(answerChunks(peer).includes('Fixture history depth: 2'));

  // Fixed (2026-07-28): the forked id is adopted and announced while the ACP
  // identity stays stable — every update still addresses the original session.
  const announced = peer.messages.filter((message) => message.method === 'session/update'
    && message.params?.update?.sessionUpdate === '_workass_claude_provider_session');
  assert.ok(announced.length >= 1, 'the forked id must be announced');
  assert.match(String(announced.at(-1).params.update.providerSessionId), /-fork-/);
	assert.equal(announced.at(-1).params.update.previousProviderSessionId, 'fixture-claude-session');
	assert.equal(announced.at(-1).params.update.lineageGeneration, 2);
	assert.match(String(announced.at(-1).params.update.lineageProof), /^[0-9a-f]{64}$/);
  assert.ok(peer.messages
    .filter((message) => message.method === 'session/update')
    .every((message) => message.params.sessionId === 'fixture-claude-session'));

  peer.send({ jsonrpc: '2.0', id: 6, method: 'session/set_config_option', params: {
    sessionId, configId: 'model', value: 'claude-opus-fixture',
  } });
  assert.equal((await peer.waitFor((message) => message.id === 6)).error, undefined);

  peer.send({ jsonrpc: '2.0', id: 7, method: 'session/prompt', params: {
    sessionId, prompt: [{ type: 'text', text: '[fixture:depth] tres' }],
  } });
  const third = await peer.waitFor((message) => message.id === 7);
  assert.equal(third.result.stopReason, 'end_turn');
  // Fixed (data-loss, 2026-07-28): each resume follows the provider's current
  // id, so the transcript keeps every turn — continuation-correct depth 3.
  const depths = answerChunks(peer).filter((text) => text.startsWith('Fixture history depth:'));
  assert.deepEqual(depths, [
    'Fixture history depth: 1',
    'Fixture history depth: 2',
    'Fixture history depth: 3',
  ]);
  assert.deepEqual(peer.parseFailures, []);
});

// (e) Result frames under subtypes this host has never seen, plus unknown
// interleaved frame types. Unknown-error must fail the turn with the carried
// message and leave the session usable; unknown-non-error must settle the turn;
// unknown frames must be dropped without reaching the wire.
test('hostile: unknown result subtypes and unknown frames neither wedge the turn nor leak', async (t) => {
  const peer = startHost({ WORKASS_CLAUDE_FIXTURE_UNKNOWN_RESULTS: '1' });
  t.after(() => peer.child.kill('SIGKILL'));

  const sessionId = await openDefaultSession(peer);

  peer.send({ jsonrpc: '2.0', id: 3, method: 'session/prompt', params: {
    sessionId, prompt: [{ type: 'text', text: '[fixture:unknown-error-result] spend' }],
  } });
  const unknownError = await peer.waitFor((message) => message.id === 3);
  assert.match(unknownError.error?.message || '', /credits required/i,
    'an unknown error subtype must surface its carried message, not a generic wedge');

  peer.send({ jsonrpc: '2.0', id: 4, method: 'session/prompt', params: {
    sessionId, prompt: [{ type: 'text', text: '[fixture:unknown-frames] carry on' }],
  } });
  const framed = await peer.waitFor((message) => message.id === 4);
  assert.equal(framed.error, undefined, JSON.stringify(framed));
  assert.equal(framed.result.stopReason, 'end_turn');
  const wire = peer.rawLines.join('\n');
  for (const marker of ['fixture_frame_from_the_future', 'session_state_changed', 'rate_limit_event']) {
    assert.ok(!wire.includes(marker), `unknown frame ${marker} must not be forwarded to the client`);
  }

  peer.send({ jsonrpc: '2.0', id: 5, method: 'session/prompt', params: {
    sessionId, prompt: [{ type: 'text', text: '[fixture:unknown-success-result] finish' }],
  } });
  const unknownSuccess = await peer.waitFor((message) => message.id === 5);
  assert.equal(unknownSuccess.error, undefined, JSON.stringify(unknownSuccess));
  assert.equal(unknownSuccess.result.stopReason, 'end_turn');
  assert.ok(answerChunks(peer).includes('Fixture advisory answer'));
  assert.deepEqual(peer.parseFailures, []);
});

// (f) stderr noise interleaved with the turn's lifecycle: ANSI control codes,
// a forged JSON-RPC line, token material. None of it may corrupt stdout or be
// promoted onto the wire.
test('hostile: stderr noise interleaved with lifecycle never corrupts or leaks into the protocol stream', async (t) => {
  const peer = startHost({ WORKASS_CLAUDE_FIXTURE_STDERR_NOISE: '1' });
  t.after(() => peer.child.kill('SIGKILL'));

  const sessionId = await openDefaultSession(peer);

  peer.send({ jsonrpc: '2.0', id: 3, method: 'session/prompt', params: {
    sessionId, prompt: [{ type: 'text', text: '[fixture:stderr-noise] go' }],
  } });
  const done = await peer.waitFor((message) => message.id === 3);
  assert.equal(done.error, undefined, JSON.stringify(done));
  assert.equal(done.result.stopReason, 'end_turn');
  assert.ok(answerChunks(peer).includes('Fixture answer'));

  // Non-vacuous: the hostile noise really flowed on stderr.
  assert.ok(peer.stderrText().includes('sk-ant-oat01-STDERR-SECRET'));
  assert.ok(peer.stderrText().includes('"sessionId":"stderr-fake"'));

  // stdout purity: every line parsed, nothing from stderr crossed streams, and
  // the forged session/update on stderr never became a wire update.
  assert.deepEqual(peer.parseFailures, []);
  const wire = peer.rawLines.join('\n');
  assert.ok(!wire.includes('sk-ant-oat01-STDERR-SECRET'));
  assert.equal(peer.messages.some((message) => message.params?.sessionId === 'stderr-fake'), false);
});
