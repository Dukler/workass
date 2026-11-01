#!/usr/bin/env node
// Signs and checks release manifests in the publish lane. The private key is read
// here at publish time only, from outside the repository, and is never printed,
// copied or staged. The verifying half and the pinned keys are the ones the
// shipped updater uses (desktop/shell/release-manifest-signature.js).
//
//   manifest-signature.mjs check-key                 fail now, not after a long build
//   manifest-signature.mjs sign --manifest FILE      add the signature in place
//   manifest-signature.mjs verify --manifest FILE    the check a client will make

import crypto from 'node:crypto';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import process from 'node:process';
import { createRequire } from 'node:module';
import { fileURLToPath } from 'node:url';

const require = createRequire(import.meta.url);
const {
  PINNED_RELEASE_MANIFEST_KEYS,
  canonicalReleaseManifest,
  releaseManifestKeyId,
  verifyReleaseManifestSignature,
} = require('../../../desktop/shell/release-manifest-signature.js');

// Offline keys never live in the tree. Tests point this at a generated key.
export const KEY_PATH_ENV = 'WORKASS_RELEASE_MANIFEST_KEY';
// Honoured by these scripts only, for tests, and refused by a real publication
// (stage-updates.sh --publish). A shipped client has no such override.
export const TEST_TRUSTED_KEY_ENV = 'WORKASS_RELEASE_MANIFEST_TEST_PUBLIC_KEY';

const PKCS8_ED25519_PREFIX = Buffer.from('302e020100300506032b657004220420', 'hex');
const PRIVATE_KEY_BYTES = 64;

export function trustedPublicKeys(env = process.env) {
  const testKey = String(env[TEST_TRUSTED_KEY_ENV] || '').trim();
  return testKey ? [testKey] : [...PINNED_RELEASE_MANIFEST_KEYS];
}

export function signingKeyPath(env = process.env) {
  const override = String(env[KEY_PATH_ENV] || '').trim();
  if (override) return override;
  return path.join(env.HOME || os.homedir(), 'Library', 'Application Support', 'Workass', 'signing', 'release-manifest-ed25519.key');
}

// Every failure names what to do and nothing about the bytes: the key's contents
// never reach a message, a log or a release receipt.
export function loadSigningKey(env = process.env) {
  const file = signingKeyPath(env);
  let info;
  try {
    info = fs.statSync(file);
  } catch {
    throw new Error(`the release signing key is not at ${file}; a release cannot be published unsigned (docs/DISTRIBUTION.md)`);
  }
  if (!info.isFile()) throw new Error(`the release signing key at ${file} is not a regular file`);
  if (process.platform !== 'win32' && (info.mode & 0o077) !== 0) {
    throw new Error(`the release signing key at ${file} is readable by others; it must be mode 0600`);
  }
  const raw = Buffer.from(fs.readFileSync(file, 'utf8').trim(), 'base64');
  if (raw.length !== PRIVATE_KEY_BYTES) {
    throw new Error(`the release signing key at ${file} is not 64 bytes of base64`);
  }
  const privateKey = crypto.createPrivateKey({
    key: Buffer.concat([PKCS8_ED25519_PREFIX, raw.subarray(0, 32)]), format: 'der', type: 'pkcs8',
  });
  const derived = crypto.createPublicKey(privateKey).export({ type: 'spki', format: 'der' }).subarray(-32);
  if (!derived.equals(raw.subarray(32))) {
    throw new Error(`the release signing key at ${file} is damaged: its two halves do not match`);
  }
  const publicKey = derived.toString('base64');
  if (!trustedPublicKeys(env).includes(publicKey)) {
    throw new Error(`the release signing key at ${file} is not one the shipped updaters pin, so every client would refuse this release`);
  }
  return { privateKey, publicKey };
}

export function signManifest(manifest, key) {
  const signature = crypto.sign(null, Buffer.from(canonicalReleaseManifest(manifest), 'utf8'), key.privateKey);
  const { signature: _previous, ...unsigned } = manifest;
  return {
    ...unsigned,
    signature: { algorithm: 'ed25519', keyId: releaseManifestKeyId(key.publicKey), value: signature.toString('base64') },
  };
}

export function verifyManifestFile(file, env = process.env) {
  let manifest;
  try {
    manifest = JSON.parse(fs.readFileSync(file, 'utf8'));
  } catch (error) {
    throw new Error(`cannot read release manifest ${file}: ${error.message}`);
  }
  try {
    return verifyReleaseManifestSignature(manifest, { publicKeys: trustedPublicKeys(env) });
  } catch (error) {
    throw new Error(`${path.basename(file)}: ${error.message}`);
  }
}

function signManifestFile(file, env = process.env) {
  const key = loadSigningKey(env);
  const manifest = JSON.parse(fs.readFileSync(file, 'utf8'));
  const signed = signManifest(manifest, key);
  // Prove the result the way a client will before it replaces the unsigned file.
  verifyReleaseManifestSignature(signed, { publicKeys: trustedPublicKeys(env) });
  const incoming = `${file}.signing.${process.pid}`;
  try {
    fs.writeFileSync(incoming, `${JSON.stringify(signed, null, 2)}\n`, { mode: fs.statSync(file).mode & 0o777, flag: 'wx' });
    fs.renameSync(incoming, file);
  } finally {
    fs.rmSync(incoming, { force: true });
  }
  return signed.signature;
}

function main(argv) {
  const [command, flag, value] = argv;
  if (command === 'check-key' && argv.length === 1) {
    loadSigningKey();
    return;
  }
  if (!['sign', 'verify'].includes(command) || flag !== '--manifest' || !path.isAbsolute(value ?? '') || argv.length !== 3) {
    throw new Error('usage: manifest-signature.mjs <check-key | sign --manifest FILE | verify --manifest FILE> (FILE absolute)');
  }
  if (command === 'sign') signManifestFile(value);
  else verifyManifestFile(value);
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try {
    main(process.argv.slice(2));
  } catch (error) {
    process.stderr.write(`${error.message}\n`);
    process.exit(1);
  }
}
