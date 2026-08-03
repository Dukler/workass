#!/usr/bin/env node

// Protocol canary for Workass's native Claude host live-steer extension.
// It validates acknowledgement by accepted prompt UUID and the consumption
// receipt; response wording/model quality is not an oracle.

import { spawn } from 'node:child_process';
import readline from 'node:readline';

const [command, ...args] = process.argv.slice(2);
if (!command) {
  process.stderr.write('usage: probe-claude-steering.mjs <claude-native-host> [args...]\n');
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
    if (kind === '_workass_claude_steer_consumed') {
      consumedClientUserMessageId = message.params?.update?.clientUserMessageId ?? null;
    }
    if (kind === 'agent_thought_chunk' || kind === 'agent_message_chunk' || kind === 'tool_call') {
      sawActiveUpdate = true;
    }
    return;
  }
  if (message.method && message.id != null) {
    // This canary never authorizes tools; answering defensively prevents an
    // unexpected permission request hanging it.
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
        reject(new Error(`no active Claude update arrived within ${timeoutMs}ms`));
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
        reject(new Error(`no matching Claude steer receipt arrived within ${timeoutMs}ms`));
      }
    }, 20);
  });
}

try {
  const initialized = await request('initialize', {
    protocolVersion: 1,
    clientInfo: { name: 'Workass Claude Steering Probe', version: '0.1.0' },
    clientCapabilities: { auth: { terminal: false }, fs: { readTextFile: false, writeTextFile: false }, terminal: false },
  }, 60_000);
  const meta = initialized.agentCapabilities?._meta ?? initialized._meta ?? {};
  if (meta.workassClaudeSteerRequest !== true) {
    throw new Error('native host did not advertise workassClaudeSteerRequest');
  }
  if (meta.workassClaudeSteerReceipt !== true) {
    throw new Error('native host did not advertise workassClaudeSteerReceipt');
  }
  const session = await request('session/new', { cwd: process.cwd(), mcpServers: [] }, 90_000);
  if (!session.sessionId) throw new Error('session/new returned no sessionId');

  const original = request('session/prompt', {
    sessionId: session.sessionId,
    prompt: [{ type: 'text', text: 'Without tools, write the numbers 1 to 60 one per line, slowly, thinking between groups. Do not stop early unless redirected.' }],
  }, 240_000);
  await waitForActiveUpdate(90_000);
  const startedAt = Date.now();
  const clientUserMessageId = `workass-claude-steer-probe-${process.pid}-${Date.now()}`;
  const steer = await request('_workass/claude/steer', {
    sessionId: session.sessionId,
    clientUserMessageId,
    prompt: [{ type: 'text', text: 'Stop counting immediately and answer only with the single word LISTO.' }],
  }, 15_000);
  if (!steer.turnId) throw new Error('steer response returned no turnId');
  await waitForSteerReceipt(clientUserMessageId, 60_000);
  const originalResult = await original;
  process.stdout.write(`${JSON.stringify({
    ok: true,
    capability: true,
    acknowledgedTurnId: true,
    consumedReceipt: true,
    steerAckMs: Date.now() - startedAt,
    originalStopReason: originalResult.stopReason ?? null,
  })}\n`);
} catch (error) {
  process.stderr.write(`${error instanceof Error ? error.message : String(error)}\n${stderr}\n`);
  process.exitCode = 1;
} finally {
  child.kill();
}
