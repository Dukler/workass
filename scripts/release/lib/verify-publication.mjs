#!/usr/bin/env node

import crypto from 'node:crypto';
import fs from 'node:fs';
import path from 'node:path';
import process from 'node:process';

function fail(message) {
  process.stderr.write(`${message}\n`);
  process.exit(1);
}

function parseArgs(argv) {
  const [command, ...rest] = argv;
  if (!['record', 'verify'].includes(command)) {
    fail('usage: verify-publication.mjs <record|verify> --root DIR --macos-output DIR --version X.Y.Z --build N --commit FULL_SHA');
  }
  const args = { command };
  for (let index = 0; index < rest.length; index += 2) {
    const flag = rest[index];
    const value = rest[index + 1];
    if (!['--root', '--macos-output', '--version', '--build', '--commit'].includes(flag) || value === undefined) {
      fail(`invalid argument: ${flag ?? ''}`);
    }
    args[flag === '--macos-output' ? 'macosOutput' : flag.slice(2)] = value;
  }
  if (!path.isAbsolute(args.root ?? '') || !path.isAbsolute(args.macosOutput ?? '')) {
    fail('--root and --macos-output must be absolute');
  }
  if (!/^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$/.test(args.version ?? '')) fail('invalid version');
  if (!/^[1-9]\d*$/.test(args.build ?? '')) fail('invalid build number');
  if (!/^[0-9a-f]{40}$/.test(args.commit ?? '')) fail('invalid commit');
  return args;
}

function readJson(file) {
  try {
    return JSON.parse(fs.readFileSync(file, 'utf8'));
  } catch (error) {
    fail(`cannot read JSON ${file}: ${error.message}`);
  }
}

function requireFile(file) {
  if (!fs.statSync(file, { throwIfNoEntry: false })?.isFile()) fail(`publication file is missing: ${file}`);
}

function sha256(file) {
  requireFile(file);
  const hash = crypto.createHash('sha256');
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
  return hash.digest('hex');
}

function artifact(file) {
  requireFile(file);
  return { name: path.basename(file), sha256: sha256(file), size: fs.statSync(file).size };
}

function sameArtifact(left, right) {
  return left?.name === right?.name && left?.sha256 === right?.sha256 && left?.size === right?.size;
}

function expectedReceipt(args) {
  const candidateReceiptFile = path.join(args.root, 'receipt.json');
  const candidate = readJson(candidateReceiptFile);
  if (candidate.schemaVersion !== 1 || candidate.product !== 'Workass' || candidate.version !== args.version ||
      candidate.build !== Number(args.build) || candidate.commit !== args.commit) {
    fail('candidate receipt identity does not match the publication');
  }

  const windowsReceiptFile = path.join(args.root, 'windows-publication.json');
  const windows = readJson(windowsReceiptFile);
  const windowsChecksums = artifact(path.join(args.root, 'windows', args.version, 'SHA256SUMS'));
  if (windows.schemaVersion !== 1 || windows.product !== 'Workass' || windows.kind !== 'windows-publication' ||
      windows.status !== 'verified' || windows.version !== args.version || windows.commit !== args.commit ||
      windows.repository !== 'Dukler/workass' || windows.tag !== `v${args.version}` ||
      windows.releaseUrl !== `https://github.com/Dukler/workass/releases/tag/v${args.version}` ||
      windows.latestManifestUrl !== 'https://github.com/Dukler/workass/releases/latest/download/workass-windows-amd64-release.json' ||
      !sameArtifact(windows.assets?.archive, candidate.artifacts?.windows) ||
      !sameArtifact(windows.assets?.manifest, candidate.artifacts?.windowsFeed) ||
      !sameArtifact(windows.assets?.checksums, windowsChecksums)) {
    fail('Windows publication receipt does not match the verified candidate');
  }

  const archiveName = `Workass-${args.version}-darwin-arm64.zip`;
  const candidateArchive = artifact(path.join(args.root, 'macos-feed', archiveName));
  const candidateFeed = artifact(path.join(args.root, 'macos-feed', 'workass-darwin-arm64-release.json'));
  const publishedArchive = artifact(path.join(args.macosOutput, archiveName));
  const publishedFeedFile = path.join(args.macosOutput, 'workass-darwin-arm64-release.json');
  const publishedFeed = artifact(publishedFeedFile);
  if (!sameArtifact(candidateArchive, candidate.artifacts?.macos) ||
      !sameArtifact(candidateFeed, candidate.artifacts?.macosFeed) ||
      !sameArtifact(publishedArchive, candidateArchive) || !sameArtifact(publishedFeed, candidateFeed)) {
    fail('published macOS feed differs from the verified candidate');
  }
  const feed = readJson(publishedFeedFile);
  if (feed.version !== args.version || feed.build !== Number(args.build) ||
      feed.artifacts?.update?.name !== archiveName || feed.artifacts.update.sha256 !== publishedArchive.sha256 ||
      feed.artifacts.update.size !== publishedArchive.size) {
    fail('published macOS manifest does not bind the published archive');
  }
  const checksumFields = fs.readFileSync(path.join(args.macosOutput, 'SHA256SUMS'), 'utf8').trim().split(/\s+/);
  if (checksumFields.length !== 2 || checksumFields[0] !== publishedArchive.sha256 || checksumFields[1] !== archiveName) {
    fail('published macOS checksum does not bind the published archive');
  }

  return {
    schemaVersion: 1,
    product: 'Workass',
    kind: 'paired-publication',
    status: 'verified',
    version: args.version,
    build: Number(args.build),
    commit: args.commit,
    candidateReceipt: artifact(candidateReceiptFile),
    macos: { feed: publishedFeed, archive: publishedArchive },
    windows,
  };
}

const args = parseArgs(process.argv.slice(2));
const expected = expectedReceipt(args);
const receiptFile = path.join(args.root, 'publication.json');
if (args.command === 'record') {
  if (fs.existsSync(receiptFile)) {
    if (JSON.stringify(readJson(receiptFile)) !== JSON.stringify(expected)) fail('published release differs from its immutable receipt');
  } else {
    const incoming = `${receiptFile}.incoming.${process.pid}`;
    try {
      fs.writeFileSync(incoming, `${JSON.stringify(expected, null, 2)}\n`, { mode: 0o600, flag: 'wx' });
      fs.renameSync(incoming, receiptFile);
    } finally {
      fs.rmSync(incoming, { force: true });
    }
  }
} else if (JSON.stringify(readJson(receiptFile)) !== JSON.stringify(expected)) {
  fail('paired publication receipt does not match the published release');
}

process.stdout.write('WORKASS_PAIRED_PUBLICATION_VERIFIED\n');
process.stdout.write(`publication=${receiptFile}\n`);
process.stdout.write(`release=${expected.windows.releaseUrl}\n`);
