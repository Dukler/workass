'use strict';

// The one update check GitHub cannot pass for an attacker: an ed25519 signature
// over each release manifest, made at publish time with an offline key and
// verified here against keys pinned in the shipped code. Everything else the
// updater checks (HTTPS, size, SHA-256) comes from the same release as the bytes
// it describes, so a compromised account or asset satisfies all of it.
//
// Mirrored in Go by internal/headlessupdate/signature.go. Both read the vectors
// in internal/headlessupdate/testdata, so neither can drift alone. No third-party
// dependency, and no private key material ever reaches this file.

const crypto = require('node:crypto');

// A list so a replacement can ship before the old key stops signing. Rotation is
// in docs/DISTRIBUTION.md. There is deliberately no environment override: a key
// an update could name would be no check at all.
const PINNED_RELEASE_MANIFEST_KEYS = Object.freeze([
  'DIV37U57AIJdwl6JGN4njQdN6OCHv39K+WgZMmlo92s=',
]);

const CANONICAL_CONTEXT = 'workass-release-manifest-v1';
const SPKI_ED25519_PREFIX = Buffer.from('302a300506032b6570032100', 'hex');
const PUBLIC_KEY_BYTES = 32;
const SIGNATURE_BYTES = 64;

function strictBase64(value, expectedBytes) {
  const text = typeof value === 'string' ? value.trim() : '';
  if (!text) return null;
  const bytes = Buffer.from(text, 'base64');
  if (bytes.length !== expectedBytes || bytes.toString('base64') !== text) return null;
  return bytes;
}

function canonicalField(name, value) {
  const text = String(value);
  return `${name}=${Buffer.byteLength(text, 'utf8')}:${text}\n`;
}

// Length-prefixed lines rather than JSON: two languages and a publisher have to
// produce the same bytes, a field can hold a newline, and no value can be made to
// read as the next field. Every field a client acts on or shows is covered.
function canonicalReleaseManifest(manifest) {
  const artifact = manifest?.artifacts?.update;
  if (!artifact || typeof artifact !== 'object') throw new Error('release manifest has no update artifact to sign');
  if (manifest.build !== undefined && (!Number.isSafeInteger(manifest.build) || manifest.build < 0)) {
    throw new Error('release manifest has an invalid build number');
  }
  for (const [label, value] of [['notes', manifest.notes], ['designatedRequirement', manifest.designatedRequirement]]) {
    if (value !== undefined && typeof value !== 'string') throw new Error(`release manifest has an invalid ${label}`);
  }
  if (!Number.isSafeInteger(artifact.size) || artifact.size < 0) throw new Error('release manifest has an invalid update size');
  return [
    `${CANONICAL_CONTEXT}\n`,
    canonicalField('schemaVersion', manifest.schemaVersion),
    canonicalField('product', manifest.product),
    canonicalField('version', manifest.version),
    canonicalField('build', manifest.build === undefined ? '' : manifest.build),
    canonicalField('platform', manifest.platform),
    canonicalField('arch', manifest.arch),
    canonicalField('portable', manifest.portable === true),
    canonicalField('headless', manifest.headless === true),
    canonicalField('designatedRequirement', manifest.designatedRequirement ?? ''),
    canonicalField('notes', manifest.notes ?? ''),
    canonicalField('artifact.name', artifact.name),
    canonicalField('artifact.url', artifact.url),
    canonicalField('artifact.sha256', String(artifact.sha256).toLowerCase()),
    canonicalField('artifact.size', artifact.size),
  ].join('');
}

function releaseManifestKeyId(publicKey) {
  const raw = strictBase64(publicKey, PUBLIC_KEY_BYTES);
  if (!raw) throw new Error('release manifest public key is not 32 bytes of base64');
  return crypto.createHash('sha256').update(raw).digest('hex').slice(0, 16);
}

function publicKeyObject(publicKey) {
  const raw = strictBase64(publicKey, PUBLIC_KEY_BYTES);
  if (!raw) throw new Error('release manifest public key is not 32 bytes of base64');
  return crypto.createPublicKey({ key: Buffer.concat([SPKI_ED25519_PREFIX, raw]), format: 'der', type: 'spki' });
}

// Throws a plain sentence on a missing, malformed, wrong or tampered signature.
// The caller stops there, before anything is downloaded or staged.
function verifyReleaseManifestSignature(manifest, { publicKeys = PINNED_RELEASE_MANIFEST_KEYS } = {}) {
  const signature = manifest?.signature;
  if (!signature || typeof signature !== 'object') {
    throw new Error('this Workass release has no signature, so it was refused');
  }
  if (signature.algorithm !== 'ed25519') {
    throw new Error('this Workass release uses an unknown signature algorithm, so it was refused');
  }
  const value = strictBase64(signature.value, SIGNATURE_BYTES);
  if (!value) throw new Error('this Workass release has a malformed signature, so it was refused');
  const message = Buffer.from(canonicalReleaseManifest(manifest), 'utf8');
  for (const key of publicKeys) {
    if (crypto.verify(null, message, publicKeyObject(key), value)) return { keyId: releaseManifestKeyId(key) };
  }
  throw new Error('this Workass release is not signed by a key this build trusts, or it was changed after signing, so it was refused');
}

module.exports = {
  PINNED_RELEASE_MANIFEST_KEYS,
  canonicalReleaseManifest,
  releaseManifestKeyId,
  verifyReleaseManifestSignature,
};
