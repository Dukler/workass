#!/usr/bin/env node

import crypto from 'node:crypto';
import fs from 'node:fs';
import path from 'node:path';
import process from 'node:process';

const inputPaths = Object.freeze({
  renderer: 'renderer',
  macosDaemon: 'macos/runtime/workass',
  macosElectron: 'macos/electron/darwin-arm64/Electron.app',
  macosNode: 'macos/runtime/node/darwin-arm64',
  macosFrontierHosts: 'macos/runtime/frontier-hosts/darwin-arm64',
  windowsDaemon: 'windows/runtime/workass-daemon.exe',
  windowsElectron: 'windows/electron/win32-x64',
  windowsNode: 'windows/runtime/node/windows-amd64',
  windowsFrontierHosts: 'windows/runtime/frontier-hosts/windows-amd64',
});

function fail(message) {
  process.stderr.write(`${message}\n`);
  process.exit(1);
}

function parseArgs(argv) {
  const [command, ...rest] = argv;
  if (!['create', 'verify'].includes(command)) {
    fail('usage: release-input.mjs <create|verify> --root DIR --version X.Y.Z --commit FULL_SHA');
  }
  const args = { command };
  for (let index = 0; index < rest.length; index += 2) {
    const flag = rest[index];
    const value = rest[index + 1];
    if (!['--root', '--version', '--commit'].includes(flag) || value === undefined) {
      fail(`invalid argument: ${flag ?? ''}`);
    }
    args[flag.slice(2)] = value;
  }
  if (!path.isAbsolute(args.root ?? '')) fail('--root must be absolute');
  if (!/^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$/.test(args.version ?? '')) {
    fail('--version must be strict X.Y.Z');
  }
  if (!/^[0-9a-f]{40}$/.test(args.commit ?? '')) fail('--commit must be one full Git SHA');
  return args;
}

function hashField(hash, value) {
  const bytes = Buffer.from(String(value));
  hash.update(String(bytes.length));
  hash.update(':');
  hash.update(bytes);
  hash.update(';');
}

function hashFile(hash, file) {
  const descriptor = fs.openSync(file, 'r');
  const buffer = Buffer.allocUnsafe(1024 * 1024);
  try {
    let count;
    do {
      count = fs.readSync(descriptor, buffer, 0, buffer.length, null);
      if (count > 0) hash.update(buffer.subarray(0, count));
    } while (count > 0);
  } finally {
    fs.closeSync(descriptor);
  }
}

function treeDigest(root) {
  if (!fs.existsSync(root)) fail(`release input is missing: ${root}`);
  const hash = crypto.createHash('sha256');
  let files = 0;
  let bytes = 0;

  function visit(absolute, relative) {
    const stat = fs.lstatSync(absolute);
    const normalized = relative.split(path.sep).join('/');
    if (stat.isSymbolicLink()) {
      hashField(hash, 'symlink');
      hashField(hash, normalized);
      hashField(hash, fs.readlinkSync(absolute));
      files += 1;
      return;
    }
    if (stat.isDirectory()) {
      hashField(hash, 'directory');
      hashField(hash, normalized);
      const children = fs.readdirSync(absolute).sort((left, right) => left.localeCompare(right, 'en'));
      for (const child of children) visit(path.join(absolute, child), path.join(relative, child));
      return;
    }
    if (!stat.isFile()) fail(`unsupported release input entry: ${absolute}`);
    hashField(hash, 'file');
    hashField(hash, normalized);
    hashField(hash, stat.mode & 0o111 ? 'executable' : 'regular');
    hashField(hash, stat.size);
    hashFile(hash, absolute);
    files += 1;
    bytes += stat.size;
  }

  visit(root, '.');
  return { sha256: hash.digest('hex'), files, bytes };
}

function inspectInputs(root) {
  return Object.fromEntries(Object.entries(inputPaths).map(([name, relative]) => [
    name,
    { path: relative, ...treeDigest(path.join(root, relative)) },
  ]));
}

function expectedManifest(args) {
  return {
    schemaVersion: 1,
    product: 'Workass',
    version: args.version,
    commit: args.commit,
    inputs: inspectInputs(args.root),
  };
}

function assertExactManifest(actual, expected) {
  if (JSON.stringify(actual) !== JSON.stringify(expected)) {
    fail('release input does not match its version, commit, or content hashes');
  }
}

const args = parseArgs(process.argv.slice(2));
const manifestPath = path.join(args.root, 'manifest.json');
if (args.command === 'create') {
  if (fs.existsSync(manifestPath)) fail(`release input manifest already exists: ${manifestPath}`);
  const manifest = expectedManifest(args);
  fs.writeFileSync(manifestPath, `${JSON.stringify(manifest, null, 2)}\n`, { mode: 0o600, flag: 'wx' });
} else {
  let actual;
  try {
    actual = JSON.parse(fs.readFileSync(manifestPath, 'utf8'));
  } catch (error) {
    fail(`cannot read release input manifest: ${error.message}`);
  }
  assertExactManifest(actual, expectedManifest(args));
}

process.stdout.write('WORKASS_RELEASE_INPUT_VERIFIED\n');
process.stdout.write(`input=${args.root}\nversion=${args.version}\ncommit=${args.commit}\n`);
