#!/usr/bin/env node

import process from 'node:process';
import probeModule from '../acp/probe.js';

const { probeAcpServer } = probeModule;
const [command, ...args] = process.argv.slice(2);

if (!command) {
  process.stderr.write('Usage: node desktop/scripts/probe-acp.mjs <command> [args...]\n');
  process.exitCode = 2;
} else {
  const result = await probeAcpServer({ command, args, cwd: process.cwd() });
  process.stdout.write(`${JSON.stringify(result, null, 2)}\n`);
  process.exitCode = result.ok ? 0 : 1;
}
