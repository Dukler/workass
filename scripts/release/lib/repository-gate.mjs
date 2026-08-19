#!/usr/bin/env node

import crypto from 'node:crypto';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import process from 'node:process';
import { spawnSync } from 'node:child_process';

function fail(message) {
  process.stderr.write(`${message}\n`);
  process.exit(1);
}

function parseArgs(argv) {
  const [command, ...rest] = argv;
  if (!['record', 'verify'].includes(command)) {
    fail('usage: repository-gate.mjs <record|verify> --repo DIR --receipt FILE --commit FULL_SHA');
  }
  const args = { command };
  for (let index = 0; index < rest.length; index += 2) {
    const flag = rest[index];
    const value = rest[index + 1];
    if (!['--repo', '--receipt', '--commit'].includes(flag) || value === undefined) {
      fail(`invalid argument: ${flag ?? ''}`);
    }
    args[flag.slice(2)] = value;
  }
  if (!path.isAbsolute(args.repo ?? '') || !path.isAbsolute(args.receipt ?? '')) {
    fail('--repo and --receipt must be absolute');
  }
  if (!/^[0-9a-f]{40}$/.test(args.commit ?? '')) fail('--commit must be one full Git SHA');
  return args;
}

function commandVersion(command, args) {
  const result = spawnSync(command, args, { encoding: 'utf8' });
  if (result.status !== 0) fail(`cannot read ${command} version`);
  return result.stdout.trim();
}

function sha256(file) {
  return crypto.createHash('sha256').update(fs.readFileSync(file)).digest('hex');
}

function expectedReceipt(args) {
  const gate = path.join(args.repo, 'scripts', 'gate.sh');
  if (!fs.statSync(gate, { throwIfNoEntry: false })?.isFile()) fail(`repository gate is missing: ${gate}`);
  return {
    schemaVersion: 1,
    product: 'Workass',
    kind: 'repository-gate',
    status: 'passed',
    commit: args.commit,
    gate: {
      path: 'scripts/gate.sh',
      sha256: sha256(gate),
    },
    environment: {
      platform: process.platform,
      arch: process.arch,
      osRelease: os.release(),
      go: commandVersion('go', ['version']),
      node: process.version,
      npm: commandVersion('npm', ['--version']),
    },
  };
}

function tryReadReceipt(file) {
  try {
    return JSON.parse(fs.readFileSync(file, 'utf8'));
  } catch {
    return null;
  }
}

function exactJson(left, right) {
  return JSON.stringify(left) === JSON.stringify(right);
}

const args = parseArgs(process.argv.slice(2));
const expected = expectedReceipt(args);

if (args.command === 'verify') {
  if (!exactJson(tryReadReceipt(args.receipt), expected)) {
    fail('repository gate receipt does not match this exact commit, gate, OS, or toolchain');
  }
  process.stdout.write('WORKASS_REPOSITORY_GATE_RECEIPT_VERIFIED\n');
} else {
  fs.mkdirSync(path.dirname(args.receipt), { recursive: true });
  if (fs.existsSync(args.receipt) && exactJson(tryReadReceipt(args.receipt), expected)) {
    process.stdout.write('WORKASS_REPOSITORY_GATE_RECEIPT_REUSED\n');
  } else {
    const incoming = `${args.receipt}.incoming.${process.pid}`;
    try {
      fs.writeFileSync(incoming, `${JSON.stringify(expected, null, 2)}\n`, { mode: 0o600, flag: 'wx' });
      fs.renameSync(incoming, args.receipt);
    } finally {
      fs.rmSync(incoming, { force: true });
    }
    process.stdout.write('WORKASS_REPOSITORY_GATE_RECEIPT_READY\n');
  }
}

process.stdout.write(`receipt=${args.receipt}\ncommit=${args.commit}\n`);
