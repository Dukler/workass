#!/usr/bin/env node

// Run focused development checks under one wall-clock budget. The release
// matrix remains a separate gate; this command makes a handoff's narrow checks
// explicit and stops a mistaken broad suite before it burns a full minute.
import { spawn } from 'node:child_process';
import process from 'node:process';

const argv = process.argv.slice(2);
let budgetMs = 10_000;
if (argv[0] === '--timeout-ms') {
  budgetMs = Number(argv[1]);
  argv.splice(0, 2);
}
if (!Number.isInteger(budgetMs) || budgetMs < 100 || budgetMs > 10_000 || argv.shift() !== '--') {
  process.stderr.write('usage: test-focused.mjs [--timeout-ms 100..10000] -- COMMAND [ARGS...] [::: COMMAND [ARGS...]]\n');
  process.exit(2);
}

const commands = [[]];
for (const arg of argv) {
  if (arg === ':::') commands.push([]);
  else commands.at(-1).push(arg);
}
if (commands.some((command) => command.length === 0)) {
  process.stderr.write('each focused check needs a command\n');
  process.exit(2);
}

const startedAt = Date.now();
const children = new Set();
let timedOut = false;
function stop(child) {
  if (child.exitCode !== null || child.signalCode !== null || !child.pid) return;
  try {
    if (process.platform === 'win32') child.kill('SIGKILL');
    else process.kill(-child.pid, 'SIGKILL');
  } catch (error) {
    if (error.code !== 'ESRCH') throw error;
  }
}
const timer = setTimeout(() => {
  timedOut = true;
  for (const child of children) stop(child);
}, budgetMs);

const results = await Promise.all(commands.map((command) => new Promise((resolve) => {
  const child = spawn(command[0], command.slice(1), { stdio: 'inherit', detached: process.platform !== 'win32' });
  children.add(child);
  let spawnError;
  child.on('error', (error) => { spawnError = error; });
  child.on('close', (code, signal) => {
    children.delete(child);
    resolve({ command: command[0], code, signal, spawnError });
  });
})));
clearTimeout(timer);
const failed = timedOut || results.some((result) => result.spawnError || result.code !== 0);
process.stdout.write(`WORKASS_FOCUSED_CHECK status=${timedOut ? 'timeout' : failed ? 'failed' : 'passed'} seconds=${((Date.now() - startedAt) / 1000).toFixed(2)} commands=${commands.length}\n`);
if (timedOut) process.exit(124);
if (failed) process.exit(1);
