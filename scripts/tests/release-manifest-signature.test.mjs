import assert from 'node:assert/strict';
import crypto from 'node:crypto';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import test from 'node:test';
import { KEY_PATH_ENV, signingKeyPath, verifyManifestFile } from '../release/lib/manifest-signature.mjs';
import { commit, releaseRoot, repoRoot, run, testSigningKey, write } from './release-pipeline-helpers.mjs';

const signer = path.join(releaseRoot, 'lib', 'manifest-signature.mjs');

function tempRoot(t) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-manifest-signature-'));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  return root;
}

// The on-disk format of the real key, for a key generated here: base64 of the
// 64-byte ed25519 private key (seed then public half).
function writeKeyFile(file, pair = crypto.generateKeyPairSync('ed25519'), mode = 0o600) {
  const seed = pair.privateKey.export({ type: 'pkcs8', format: 'der' }).subarray(-32);
  const publicHalf = pair.publicKey.export({ type: 'spki', format: 'der' }).subarray(-32);
  fs.writeFileSync(file, `${Buffer.concat([seed, publicHalf]).toString('base64')}\n`, { mode });
  fs.chmodSync(file, mode);
  return { contents: fs.readFileSync(file, 'utf8').trim(), publicKey: publicHalf.toString('base64') };
}

function feedFile(root, overrides = {}) {
  const file = path.join(root, 'workass-windows-amd64-release.json');
  write(file, `${JSON.stringify({
    schemaVersion: 1, product: 'Workass', version: '1.2.3', platform: 'windows', arch: 'amd64', portable: true,
    artifacts: { update: { name: 'a.zip', url: 'https://github.example/a.zip', sha256: 'a'.repeat(64), size: 7 } },
    ...overrides,
  }, null, 2)}\n`);
  return file;
}

function signEnv(root, extra = {}) {
  const key = path.join(root, 'release.key');
  const made = writeKeyFile(key);
  return {
    made,
    env: { ...process.env, [KEY_PATH_ENV]: key, WORKASS_RELEASE_MANIFEST_TEST_PUBLIC_KEY: made.publicKey, ...extra },
  };
}

test('the real key path is outside the repository and only an environment override moves it', () => {
  const real = signingKeyPath({ HOME: '/home/someone' });
  assert.equal(real, '/home/someone/Library/Application Support/Workass/signing/release-manifest-ed25519.key');
  assert.equal(signingKeyPath({ HOME: '/home/someone', [KEY_PATH_ENV]: '/tmp/k' }), '/tmp/k');
  assert.equal(path.relative(repoRoot, real).startsWith('..'), true);
});

test('signing adds one field, verifies, and a client would read the rest unchanged', (t) => {
  const root = tempRoot(t);
  const { env, made } = signEnv(root);
  const feed = feedFile(root);
  const before = JSON.parse(fs.readFileSync(feed, 'utf8'));
  const signed = run(process.execPath, [signer, 'sign', '--manifest', feed], { env });
  assert.equal(signed.status, 0, signed.stderr);
  const after = JSON.parse(fs.readFileSync(feed, 'utf8'));
  const { signature, ...rest } = after;
  assert.deepEqual(rest, before);
  assert.equal(signature.algorithm, 'ed25519');
  assert.equal(Buffer.from(signature.value, 'base64').length, 64);
  // Not a byte of the key reaches the tree the release ships from, or any output.
  assert.doesNotMatch(fs.readFileSync(feed, 'utf8'), new RegExp(made.contents.slice(0, 40).replace(/[+/=]/g, '.')));
  assert.equal(`${signed.stdout}${signed.stderr}`.includes(made.contents), false);
  assert.equal(run(process.execPath, [signer, 'verify', '--manifest', feed], { env }).status, 0);
  // Retry-stable: signing twice is the same bytes, so an immutable receipt holds.
  const once = fs.readFileSync(feed);
  assert.equal(run(process.execPath, [signer, 'sign', '--manifest', feed], { env }).status, 0);
  assert.deepEqual(fs.readFileSync(feed), once);
});

test('signing fails closed, in plain words, with nothing written', async (t) => {
  await t.test('no key file', (st) => {
    const root = tempRoot(st);
    const feed = feedFile(root);
    const before = fs.readFileSync(feed);
    const result = run(process.execPath, [signer, 'sign', '--manifest', feed],
      { env: { ...process.env, [KEY_PATH_ENV]: path.join(root, 'absent.key') } });
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /release signing key is not at/);
    assert.match(result.stderr, /cannot be published unsigned/);
    assert.deepEqual(fs.readFileSync(feed), before);
  });
  await t.test('a key others can read', { skip: process.platform === 'win32' }, (st) => {
    const root = tempRoot(st);
    const key = path.join(root, 'open.key');
    const made = writeKeyFile(key, undefined, 0o644);
    const feed = feedFile(root);
    const result = run(process.execPath, [signer, 'sign', '--manifest', feed],
      { env: { ...process.env, [KEY_PATH_ENV]: key, WORKASS_RELEASE_MANIFEST_TEST_PUBLIC_KEY: made.publicKey } });
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /must be mode 0600/);
  });
  await t.test('a key the shipped updaters do not pin', (st) => {
    const root = tempRoot(st);
    const key = path.join(root, 'stranger.key');
    writeKeyFile(key);
    const feed = feedFile(root);
    const before = fs.readFileSync(feed);
    const env = { ...process.env, [KEY_PATH_ENV]: key };
    delete env.WORKASS_RELEASE_MANIFEST_TEST_PUBLIC_KEY;
    const result = run(process.execPath, [signer, 'sign', '--manifest', feed], { env });
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /every client would refuse this release/);
    assert.deepEqual(fs.readFileSync(feed), before);
    assert.notEqual(run(process.execPath, [signer, 'check-key'], { env }).status, 0);
  });
  await t.test('a damaged key', (st) => {
    const root = tempRoot(st);
    const key = path.join(root, 'damaged.key');
    fs.writeFileSync(key, `${crypto.randomBytes(64).toString('base64')}\n`, { mode: 0o600 });
    const result = run(process.execPath, [signer, 'check-key'], { env: { ...process.env, [KEY_PATH_ENV]: key } });
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /damaged/);
  });
});

test('every publish path signs before it publishes and checks the key before it builds', () => {
  const read = (relative) => fs.readFileSync(path.join(repoRoot, relative), 'utf8');
  for (const [relative, manifest, publishMark] of [
    ['scripts/stage-macos-local-update.sh', '"$feed_incoming"', 'mv -f "$feed_incoming" "$feed"'],
    ['scripts/stage-windows-portable.sh', '"$release_dir/workass-windows-amd64-release.json"', 'WORKASS_WINDOWS_PORTABLE_READY'],
    ['scripts/stage-linux-headless.sh', '"$release_dir/$feed_name"', 'WORKASS_LINUX_HEADLESS_READY'],
    ['scripts/release-workass-macos.sh', '"$stage_root/$feed_name"', 'cp "$stage_root/$feed_name" "$stage_root/release.json"'],
  ]) {
    const script = read(relative);
    assert.match(script, /^set -eu/m, relative);
    const check = script.indexOf('manifest-signature.mjs" check-key');
    const sign = script.indexOf(`manifest-signature.mjs" sign --manifest ${manifest}`);
    const publish = script.indexOf(publishMark);
    assert.ok(check >= 0, `${relative} does not check the key first`);
    assert.ok(sign > check, `${relative} does not sign its manifest`);
    assert.ok(publish > sign, `${relative} publishes before signing`);
  }
  const lane = read('scripts/release/stage-updates.sh');
  assert.ok(lane.indexOf('manifest-signature.mjs" check-key') < lane.indexOf('prepare_input()'));
  assert.match(lane, /refusing to publish with WORKASS_RELEASE_MANIFEST_TEST_PUBLIC_KEY set/);
  const publisher = read('scripts/release/publish-windows.sh');
  assert.ok(publisher.indexOf('manifest-signature.mjs" verify --manifest "$release_dir/$manifest"') < publisher.indexOf('gh auth status'));
  assert.ok(publisher.indexOf('manifest-signature.mjs" verify --manifest "$linux_release_dir/$linux_manifest"') < publisher.indexOf('gh auth status'));
  for (const packager of ['scripts/package-workass-macos.sh', 'scripts/stage-windows-portable.sh']) {
    assert.match(read(packager), /for shell_file in[^\n]*release-manifest-signature\.js/, packager);
  }
});

test('a real publication refuses a test key before doing anything', { skip: process.platform !== 'darwin' }, () => {
  const result = run('sh', [path.join(releaseRoot, 'stage-updates.sh'), '--version', '1.2.3', '--publish'],
    { env: { ...process.env, WORKASS_RELEASE_MANIFEST_TEST_PUBLIC_KEY: testSigningKey.publicKey } });
  assert.equal(result.status, 2);
  assert.match(result.stderr, /refusing to publish/);
});

test('the Windows publisher refuses an unsigned manifest before it can reach GitHub', (t) => {
  const root = tempRoot(t);
  fs.mkdirSync(path.join(repoRoot, '.dev'), { recursive: true });
  const releaseDir = path.join(root, 'release');
  const version = '1.2.3';
  const archiveName = `Workass-${version}-windows-amd64.zip`;
  write(path.join(releaseDir, archiveName), 'archive');
  feedFile(releaseDir, { version });
  const digest = crypto.createHash('sha256').update('archive').digest('hex');
  write(path.join(releaseDir, 'SHA256SUMS'), `${digest}  ${archiveName}\n`);
  const bin = path.join(root, 'bin');
  const calls = path.join(root, 'gh-calls');
  write(path.join(bin, 'gh'), `#!/bin/sh\nprintf 'called\\n' >> "${calls}"\nexit 1\n`);
  fs.chmodSync(path.join(bin, 'gh'), 0o755);
  const result = run('sh', [path.join(releaseRoot, 'publish-windows.sh'), '--version', version, '--commit', commit,
    '--release-dir', releaseDir], { env: { ...process.env, PATH: `${bin}:${process.env.PATH}` } });
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /no signature/);
  assert.equal(fs.existsSync(calls), false);
});

test('the release lane reads a manifest the way a client does', (t) => {
  const root = tempRoot(t);
  const feed = feedFile(root);
  assert.throws(() => verifyManifestFile(feed), /no signature/);
  write(feed, '{');
  assert.throws(() => verifyManifestFile(feed), /cannot read release manifest/);
});
