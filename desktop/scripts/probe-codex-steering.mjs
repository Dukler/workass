#!/usr/bin/env node

// Protocol canary for Workass's native Codex host extension. It validates
// acknowledgement by app-server turn id; response wording/model quality is not
// an oracle.

import { spawn } from 'node:child_process';
import readline from 'node:readline';

const [command, ...args] = process.argv.slice(2);
if (!command) {
  process.stderr.write('usage: probe-codex-steering.mjs <codex-native-host> [args...]\n');
  process.exit(2);
}

const child = spawn(command, args, {
  cwd: process.cwd(),
  env: process.env,
  stdio: ['pipe', 'pipe', 'pipe'],
});
const pending = new Map();
let nextID = 0;
let sawActiveUpdate = false;
let consumedClientUserMessageId = null;
let stderr = '';

child.stderr.on('data', (chunk) => { stderr = (stderr + chunk.toString()).slice(-4000); });

function send(message) {
  child.stdin.write(`${JSON.stringify(message)}\n`);
}

function request(method, params, timeoutMs) {
  const id = ++nextID;
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      pending.delete(id);
      reject(new Error(`${method} timed out after ${timeoutMs}ms`));
    }, timeoutMs);
    pending.set(id, { method, resolve, reject, timer });
    send({ jsonrpc: '2.0', id, method, params });
  });
}

const lines = readline.createInterface({ input: child.stdout });
lines.on('line', (line) => {
  let message;
  try { message = JSON.parse(line); } catch { return; }
  if (message.method === 'session/update') {
    const kind = message.params?.update?.sessionUpdate;
    if (kind === '_workass_codex_steer_consumed') {
      consumedClientUserMessageId = message.params?.update?.clientUserMessageId ?? null;
    }
    if (kind === 'agent_thought_chunk' || kind === 'agent_message_chunk' || kind === 'tool_call') {
      sawActiveUpdate = true;
    }
    return;
  }
  if (message.method && message.id != null) {
    // This canary never authorizes tools. The test prompt asks for no tools, but
    // answering defensively prevents an unexpected permission request hanging it.
    send({ jsonrpc: '2.0', id: message.id, result: { outcome: { outcome: 'cancelled' } } });
    return;
  }
  const rec = pending.get(message.id);
  if (!rec) return;
  pending.delete(message.id);
  clearTimeout(rec.timer);
  if (message.error) rec.reject(new Error(`${rec.method}: ${message.error.message || message.error.code}`));
  else rec.resolve(message.result ?? {});
});

function waitForActiveUpdate(timeoutMs) {
  return new Promise((resolve, reject) => {
    const started = Date.now();
    const timer = setInterval(() => {
      if (sawActiveUpdate) {
        clearInterval(timer);
        resolve();
      } else if (Date.now() - started >= timeoutMs) {
        clearInterval(timer);
        reject(new Error(`no active Codex update arrived within ${timeoutMs}ms`));
      }
    }, 20);
  });
}

function waitForSteerReceipt(clientUserMessageId, timeoutMs) {
  return new Promise((resolve, reject) => {
    const started = Date.now();
    const timer = setInterval(() => {
      if (consumedClientUserMessageId === clientUserMessageId) {
        clearInterval(timer);
        resolve();
      } else if (Date.now() - started >= timeoutMs) {
        clearInterval(timer);
        reject(new Error(`no matching Codex steer receipt arrived within ${timeoutMs}ms`));
      }
    }, 20);
  });
}

try {
  const initialized = await request('initialize', {
    protocolVersion: 1,
    clientInfo: { name: 'Workass Codex Steering Probe', version: '0.1.0' },
    clientCapabilities: { auth: { terminal: false }, fs: { readTextFile: false, writeTextFile: false }, terminal: false },
  }, 30_000);
  if (initialized.agentCapabilities?._meta?.workassCodexSteerRequest !== true) {
    throw new Error('native host did not advertise workassCodexSteerRequest');
  }
  if (initialized.agentCapabilities?._meta?.workassCodexSteerReceipt !== true ||
      initialized.agentCapabilities?._meta?.workassCodexSteerRaceV1 !== true) {
    throw new Error('native host did not advertise receipt + official race handling');
  }
  const session = await request('session/new', { cwd: process.cwd(), mcpServers: [] }, 60_000);
  if (!session.sessionId) throw new Error('session/new returned no sessionId');

  const original = request('session/prompt', {
    sessionId: session.sessionId,
    prompt: [{ type: 'text', text: 'Without tools, think through a long numbered list slowly before answering.' }],
  }, 180_000);
  await waitForActiveUpdate(60_000);
  const startedAt = Date.now();
  const clientUserMessageId = `workass-steer-probe-${process.pid}-${Date.now()}`;
  const steer = await request('_workass/codex/steer', {
    sessionId: session.sessionId,
    clientUserMessageId,
    prompt: [{ type: 'text', text: 'Stop the list and answer briefly now.' }],
  }, 15_000);
  if (!steer.turnId) throw new Error('steer response returned no turnId');
  await waitForSteerReceipt(clientUserMessageId, 30_000);
  const originalResult = await original;
  process.stdout.write(`${JSON.stringify({
    ok: true,
    capability: true,
    acknowledgedTurnId: true,
    consumedReceipt: true,
    steerAckMs: Date.now() - startedAt,
    originalStopReason: originalResult.stopReason ?? null,
  })}\n`);
  try { await request('session/close', { sessionId: session.sessionId }, 5_000); } catch { /* best effort */ }
} catch (error) {
  process.stderr.write(`${error instanceof Error ? error.message : String(error)}\n${stderr}\n`);
  process.exitCode = 1;
} finally {
  child.kill();
}
