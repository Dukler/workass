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
  if (!['verify', 'record'].includes(command)) {
    fail('usage: verify-candidate.mjs <verify|record> --root DIR --input DIR --version X.Y.Z --build N --commit FULL_SHA');
  }
  const args = { command };
  for (let index = 0; index < rest.length; index += 2) {
    const flag = rest[index];
    const value = rest[index + 1];
    if (!['--root', '--input', '--version', '--build', '--commit'].includes(flag) || value === undefined) {
      fail(`invalid argument: ${flag ?? ''}`);
    }
    args[flag.slice(2)] = value;
  }
  if (!path.isAbsolute(args.root ?? '') || !path.isAbsolute(args.input ?? '')) fail('--root and --input must be absolute');
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
  if (!fs.statSync(file, { throwIfNoEntry: false })?.isFile()) fail(`candidate file is missing: ${file}`);
}

function sha256(file) {
  return crypto.createHash('sha256').update(fs.readFileSync(file)).digest('hex');
}

function artifact(file) {
  requireFile(file);
  return { name: path.basename(file), sha256: sha256(file), size: fs.statSync(file).size };
}

function checkSums(file, expected) {
  requireFile(file);
  const fields = fs.readFileSync(file, 'utf8').trim().split(/\s+/);
  if (fields.length !== 2 || fields[0] !== expected.sha256 || fields[1] !== expected.name) {
    fail(`checksum file does not bind ${expected.name}`);
  }
}

function verify(args) {
  const macFeedRoot = path.join(args.root, 'macos-feed');
  const macApp = path.join(args.root, 'macos-app', 'Workass.app');
  const macName = `Workass-${args.version}-darwin-arm64.zip`;
  const macArchive = artifact(path.join(macFeedRoot, macName));
  const macFeedFile = path.join(macFeedRoot, 'workass-darwin-arm64-release.json');
  const macFeed = readJson(macFeedFile);
  const macUpdate = macFeed.artifacts?.update;
  if (macFeed.schemaVersion !== 1 || macFeed.product !== 'Workass' || macFeed.version !== args.version ||
      macFeed.build !== Number(args.build) || macFeed.platform !== 'darwin' || macFeed.arch !== 'arm64' ||
      typeof macFeed.designatedRequirement !== 'string' || macFeed.designatedRequirement.length === 0 ||
      macUpdate?.name !== macArchive.name || macUpdate?.url !== macArchive.name ||
      macUpdate?.sha256 !== macArchive.sha256 || macUpdate?.size !== macArchive.size) {
    fail('macOS candidate manifest does not match the staged release');
  }
  checkSums(path.join(macFeedRoot, 'SHA256SUMS'), macArchive);

  const macShell = readJson(path.join(macApp, 'Contents', 'Resources', 'app', 'package.json'));
  const macRuntime = readJson(path.join(macApp, 'Contents', 'Resources', 'runtime', 'manifest.json'));
  if (macShell.version !== args.version || macRuntime.version !== args.version ||
      macRuntime.build !== args.build || macRuntime.platform !== 'darwin' || macRuntime.arch !== 'arm64') {
    fail('macOS app shell and runtime versions do not match the candidate');
  }

  const windowsRoot = path.join(args.root, 'windows', args.version);
  const windowsBundleName = `Workass-${args.version}-windows-amd64`;
  const windowsArchive = artifact(path.join(windowsRoot, `${windowsBundleName}.zip`));
  const windowsFeedFile = path.join(windowsRoot, 'workass-windows-amd64-release.json');
  const windowsFeed = readJson(windowsFeedFile);
  const windowsUpdate = windowsFeed.artifacts?.update;
  if (windowsFeed.schemaVersion !== 1 || windowsFeed.product !== 'Workass' || windowsFeed.version !== args.version ||
      windowsFeed.platform !== 'windows' || windowsFeed.arch !== 'amd64' || windowsFeed.portable !== true ||
      windowsFeed.authenticode !== false || windowsUpdate?.name !== windowsArchive.name ||
      windowsUpdate?.sha256 !== windowsArchive.sha256 || windowsUpdate?.size !== windowsArchive.size ||
      windowsUpdate?.url !== `https://github.com/Dukler/workass/releases/download/v${args.version}/${windowsArchive.name}`) {
    fail('Windows candidate manifest does not match the staged release');
  }
  checkSums(path.join(windowsRoot, 'SHA256SUMS'), windowsArchive);

  const windowsBundle = path.join(windowsRoot, windowsBundleName);
  const windowsShell = readJson(path.join(windowsBundle, 'resources', 'app', 'package.json'));
  const windowsRuntime = readJson(path.join(windowsBundle, 'manifest.json'));
  if (windowsShell.version !== args.version || windowsRuntime.version !== args.version ||
      windowsRuntime.platform !== 'windows' || windowsRuntime.arch !== 'amd64' ||
      windowsRuntime.revision !== args.commit) {
    fail('Windows shell and runtime versions do not match the candidate');
  }
  for (const relative of [
    'Workass.exe',
    'workass-daemon.exe',
    'workass-tools.exe',
    'resources/renderer/index.html',
    'node/windows-amd64/node.exe',
    'frontier-hosts/windows-amd64/claude-native-host.mjs',
    'frontier-hosts/windows-amd64/codex-native-host.mjs',
    'frontier-hosts/windows-amd64/workass-tools.mjs',
    'frontier-hosts/windows-amd64/workass-tools.cmd',
    'frontier-hosts/windows-amd64/node_modules/@anthropic-ai/claude-agent-sdk/sdk.mjs',
  ]) requireFile(path.join(windowsBundle, ...relative.split('/')));

  const linuxRoot = path.join(args.root, 'linux', args.version);
  const linuxBundleName = `Workass-${args.version}-linux-amd64`;
  const linuxArchive = artifact(path.join(linuxRoot, `${linuxBundleName}.tar.gz`));
  const linuxFeedFile = path.join(linuxRoot, 'workass-linux-amd64-release.json');
  const linuxFeed = readJson(linuxFeedFile);
  const linuxUpdate = linuxFeed.artifacts?.update;
  if (linuxFeed.schemaVersion !== 1 || linuxFeed.product !== 'Workass' || linuxFeed.version !== args.version ||
      linuxFeed.platform !== 'linux' || linuxFeed.arch !== 'amd64' || linuxFeed.headless !== true ||
      linuxFeed.service !== 'systemd-user' || linuxUpdate?.name !== linuxArchive.name ||
      linuxUpdate?.sha256 !== linuxArchive.sha256 || linuxUpdate?.size !== linuxArchive.size ||
      linuxUpdate?.url !== `https://github.com/Dukler/workass/releases/download/v${args.version}/${linuxArchive.name}`) {
    fail('Linux candidate manifest does not match the staged release');
  }
  checkSums(path.join(linuxRoot, 'SHA256SUMS-linux-amd64'), linuxArchive);

  const linuxBundle = path.join(linuxRoot, linuxBundleName);
  const linuxRuntime = readJson(path.join(linuxBundle, 'manifest.json'));
  if (linuxRuntime.version !== args.version || linuxRuntime.platform !== 'linux' ||
      linuxRuntime.arch !== 'amd64' || linuxRuntime.headless !== true ||
      linuxRuntime.revision !== args.commit) {
    fail('Linux runtime manifest does not match the candidate');
  }
  // The four pieces a headless install needs, named one by one: the record of
  // the hand-provisioned host had to gather each of them separately, and a
  // release that drops any one of them is an install that half works.
  for (const relative of [
    'workass',
    'workass-tools',
    'workass-agent',
    'node/linux-amd64/bin/node',
    'frontier-hosts/linux-amd64/claude-native-host.mjs',
    'frontier-hosts/linux-amd64/codex-native-host.mjs',
    'frontier-hosts/linux-amd64/pi-native-host.mjs',
    'frontier-hosts/linux-amd64/omp-native-host.mjs',
    'frontier-hosts/linux-amd64/node_modules/@anthropic-ai/claude-agent-sdk/sdk.mjs',
  ]) requireFile(path.join(linuxBundle, ...relative.split('/')));

  const inputManifest = artifact(path.join(args.input, 'manifest.json'));
  return {
    schemaVersion: 1,
    product: 'Workass',
    version: args.version,
    build: Number(args.build),
    commit: args.commit,
    inputManifest,
    artifacts: {
      macos: macArchive,
      macosFeed: artifact(macFeedFile),
      windows: windowsArchive,
      windowsFeed: artifact(windowsFeedFile),
      linux: linuxArchive,
      linuxFeed: artifact(linuxFeedFile),
    },
  };
}

const args = parseArgs(process.argv.slice(2));
const receipt = verify(args);
if (args.command === 'record') {
  const receiptPath = path.join(args.root, 'receipt.json');
  if (fs.existsSync(receiptPath)) {
    const recorded = readJson(receiptPath);
    if (JSON.stringify(recorded) !== JSON.stringify(receipt)) fail('release candidate differs from its immutable receipt');
  } else {
    fs.writeFileSync(receiptPath, `${JSON.stringify(receipt, null, 2)}\n`, { mode: 0o600, flag: 'wx' });
  }
}
process.stdout.write('WORKASS_RELEASE_CANDIDATE_VERIFIED\n');
process.stdout.write(`candidate=${args.root}\nversion=${args.version}\ncommit=${args.commit}\n`);
