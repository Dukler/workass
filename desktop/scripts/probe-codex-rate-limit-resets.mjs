#!/usr/bin/env node

// Read-only protocol canary for Codex earned rate-limit resets. It reports only
// bounded summary facts; opaque credit ids and account data never reach stdout.

import { spawn } from 'node:child_process';
import readline from 'node:readline';

const [command, ...args] = process.argv.slice(2);
if (!command) {
  process.stderr.write('usage: probe-codex-rate-limit-resets.mjs <codex-native-host> [args...]\n');
  process.exit(2);
}

const child = spawn(command, args, { cwd: process.cwd(), env: process.env, stdio: ['pipe', 'pipe', 'pipe'] });
const pending = new Map();
let nextID = 0;
let stderr = '';
child.stderr.on('data', (chunk) => { stderr = (stderr + chunk.toString()).slice(-4000); });

function request(method, params, timeoutMs) {
  const id = ++nextID;
  child.stdin.write(`${JSON.stringify({ jsonrpc: '2.0', id, method, params })}\n`);
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      pending.delete(id);
      reject(new Error(`${method} timed out after ${timeoutMs}ms`));
    }, timeoutMs);
    pending.set(id, { method, resolve, reject, timer });
  });
}

const lines = readline.createInterface({ input: child.stdout });
lines.on('line', (line) => {
  let message;
  try { message = JSON.parse(line); } catch { return; }
  if (message.method && message.id != null) {
    child.stdin.write(`${JSON.stringify({ jsonrpc: '2.0', id: message.id, result: { outcome: { outcome: 'cancelled' } } })}\n`);
    return;
  }
  const rec = pending.get(message.id);
  if (!rec) return;
  pending.delete(message.id);
  clearTimeout(rec.timer);
  if (message.error) rec.reject(new Error(`${rec.method}: ${message.error.message || message.error.code}`));
  else rec.resolve(message.result ?? {});
});

try {
  const initialized = await request('initialize', {
    protocolVersion: 1,
    clientInfo: { name: 'Workass Codex Earned Reset Probe', version: '0.1.0' },
    clientCapabilities: { auth: { terminal: false }, fs: { readTextFile: false, writeTextFile: false }, terminal: false },
  }, 30_000);
  const meta = initialized.agentCapabilities?._meta ?? {};
  if (meta.workassCodexRateLimitsRequest !== true || meta.workassCodexRateLimitResetRequest !== true) {
    throw new Error('native host did not advertise earned-reset read + consume capabilities');
  }
  const snapshot = await request('_workass/codex/rate-limits', {}, 30_000);
  const summary = snapshot.rateLimitResetCredits;
  const count = Number(summary?.availableCount);
  const credits = Array.isArray(summary?.credits) ? summary.credits : null;
  process.stdout.write(`${JSON.stringify({
    ok: true,
    capability: true,
    availableCount: Number.isFinite(count) && count > 0 ? Math.floor(count) : 0,
    detailsAvailable: credits !== null,
    detailCount: credits?.length ?? null,
    hasExpiry: !!credits?.some((credit) => credit && credit.expiresAt != null),
  })}\n`);
} catch (error) {
  process.stderr.write(`${error instanceof Error ? error.message : String(error)}\n${stderr}\n`);
  process.exitCode = 1;
} finally {
  child.kill();
}
