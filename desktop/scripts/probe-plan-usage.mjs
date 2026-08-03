#!/usr/bin/env node

// Protocol-only canary for Workass's packaged subscription-limit extensions.
// It prints only utilization/window/reset fields—never auth material, account
// identity, transcript content, or raw provider responses.

import { spawn } from 'node:child_process';
import readline from 'node:readline';

const [provider, command, ...args] = process.argv.slice(2);
if (!['codex', 'claude'].includes(provider) || !command) {
  process.stderr.write('usage: probe-plan-usage.mjs <codex|claude> <patched-adapter> [args...]\n');
  process.exit(2);
}

const child = spawn(command, args, { cwd: process.cwd(), env: process.env, stdio: ['pipe', 'pipe', 'pipe'] });
const pending = new Map();
let nextID = 0;

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
  if (message.method && message.id != null) {
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

function safeCodex(result) {
  const snapshots = result?.rateLimitsByLimitId && typeof result.rateLimitsByLimitId === 'object'
    ? Object.values(result.rateLimitsByLimitId)
    : [result?.rateLimits];
  return snapshots.filter(Boolean).flatMap((snapshot) => ['primary', 'secondary'].flatMap((windowName) => {
    const window = snapshot?.[windowName];
    if (!window) return [];
    return [{
      window: windowName,
      usedPercent: window.usedPercent ?? null,
      windowDurationMins: window.windowDurationMins ?? null,
      resetsAt: window.resetsAt ?? null,
    }];
  }));
}

function safeClaude(result) {
  const rateLimits = result?.rate_limits ?? {};
  const standard = ['five_hour', 'seven_day', 'seven_day_oauth_apps', 'seven_day_opus', 'seven_day_sonnet']
    .flatMap((id) => rateLimits[id] ? [{ id, utilization: rateLimits[id].utilization ?? null, resetsAt: rateLimits[id].resets_at ?? null }] : []);
  const models = Array.isArray(rateLimits.model_scoped)
    ? rateLimits.model_scoped.map((row) => ({ id: `model:${row.display_name ?? 'unknown'}`, utilization: row.utilization ?? null, resetsAt: row.resets_at ?? null }))
    : [];
  return [...standard, ...models];
}

try {
  const initialized = await request('initialize', {
    protocolVersion: 1,
    clientInfo: { name: 'Workass Plan Usage Probe', version: '0.1.0' },
    clientCapabilities: { auth: { terminal: false }, fs: { readTextFile: false, writeTextFile: false }, terminal: false },
  }, 30_000);
  const marker = provider === 'codex' ? 'workassCodexRateLimitsRequest' : 'workassClaudeUsageRequest';
  const extensionMeta = initialized?._meta ?? initialized?.agentCapabilities?._meta ?? {};
  if (extensionMeta[marker] !== true) throw new Error(`provider host did not advertise ${marker}`);
  const session = await request('session/new', { cwd: process.cwd(), mcpServers: [] }, 60_000);
  if (!session.sessionId) throw new Error('session/new returned no sessionId');
  const result = provider === 'codex'
    ? await request('_workass/codex/rate-limits', {}, 20_000)
    : await request('_workass/claude/usage', { sessionId: session.sessionId }, 20_000);
  const windows = provider === 'codex' ? safeCodex(result) : safeClaude(result);
  process.stdout.write(`${JSON.stringify({ ok: true, provider, windows })}\n`);
  try { await request('session/close', { sessionId: session.sessionId }, 5_000); } catch { /* best effort */ }
} catch (error) {
  process.stderr.write(`${error instanceof Error ? error.message : String(error)}\n`);
  process.exitCode = 1;
} finally {
  child.kill();
}
