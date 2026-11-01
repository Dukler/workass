'use strict';

const assert = require('node:assert/strict');
const crypto = require('node:crypto');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const {
  PINNED_RELEASE_MANIFEST_KEYS,
  canonicalReleaseManifest,
  releaseManifestKeyId,
  verifyReleaseManifestSignature,
} = require('./release-manifest-signature');

const repoRoot = path.resolve(__dirname, '..', '..');

test('the pinned list is the owner\'s key and matches the Go updater exactly', () => {
  assert.equal(PINNED_RELEASE_MANIFEST_KEYS[0], 'DIV37U57AIJdwl6JGN4njQdN6OCHv39K+WgZMmlo92s=');
  const go = fs.readFileSync(path.join(repoRoot, 'internal', 'headlessupdate', 'signature.go'), 'utf8');
  const block = go.match(/pinnedReleaseManifestKeys = \[\]string\{([\s\S]*?)\n\}/);
  assert.ok(block, 'signature.go no longer declares the pinned list');
  const goKeys = [...block[1].matchAll(/"([^"]+)"/g)].map((match) => match[1]);
  assert.deepEqual(goKeys, [...PINNED_RELEASE_MANIFEST_KEYS]);
  assert.equal(releaseManifestKeyId(PINNED_RELEASE_MANIFEST_KEYS[0]).length, 16);
  assert.ok(Object.isFrozen(PINNED_RELEASE_MANIFEST_KEYS));
});

// The other half of this agreement is TestSignedFormMatchesTheElectronUpdaterAndThePublisher.
test('the signed form matches the Go updater on the shared vectors', () => {
  const vectors = JSON.parse(fs.readFileSync(path.join(repoRoot, 'internal', 'headlessupdate', 'testdata', 'release-manifest-vectors.json'), 'utf8'));
  assert.equal(vectors.cases.length, 3);
  for (const { name, canonical, manifest } of vectors.cases) {
    assert.equal(canonicalReleaseManifest(manifest), canonical, name);
    assert.doesNotThrow(() => verifyReleaseManifestSignature(manifest, { publicKeys: [vectors.publicKey] }), name);
    assert.throws(() => verifyReleaseManifestSignature(manifest), /refused/, `${name} verified against the pinned keys`);
  }
});

test('a value cannot read as the next field', () => {
  const base = {
    schemaVersion: 1, product: 'Workass', version: '1.0.0', platform: 'linux', arch: 'amd64',
    artifacts: { update: { name: 'a', url: 'https://example.test/a', sha256: 'a'.repeat(64), size: 1 } },
  };
  const shifted = canonicalReleaseManifest({ ...base, notes: 'x\nartifact.name=1:b' });
  const plain = canonicalReleaseManifest({ ...base, notes: 'x' });
  assert.notEqual(shifted, plain);
  assert.match(shifted, /notes=19:x\nartifact\.name=1:b\nartifact\.name=1:a\n/);
});

test('a rotated list accepts the old and the new key, and refuses the retired one', () => {
  const release = {
    schemaVersion: 1, product: 'Workass', version: '1.0.0', platform: 'linux', arch: 'amd64',
    artifacts: { update: { name: 'a', url: 'https://example.test/a', sha256: 'a'.repeat(64), size: 1 } },
  };
  const key = () => {
    const pair = crypto.generateKeyPairSync('ed25519');
    return { ...pair, raw: pair.publicKey.export({ type: 'spki', format: 'der' }).subarray(-32).toString('base64') };
  };
  const [oldKey, newKey] = [key(), key()];
  const signedBy = (signer) => ({
    ...release,
    signature: { algorithm: 'ed25519', value: crypto.sign(null, Buffer.from(canonicalReleaseManifest(release)), signer.privateKey).toString('base64') },
  });
  const both = { publicKeys: [oldKey.raw, newKey.raw] };
  assert.equal(verifyReleaseManifestSignature(signedBy(oldKey), both).keyId, releaseManifestKeyId(oldKey.raw));
  assert.equal(verifyReleaseManifestSignature(signedBy(newKey), both).keyId, releaseManifestKeyId(newKey.raw));
  assert.throws(() => verifyReleaseManifestSignature(signedBy(oldKey), { publicKeys: [newKey.raw] }), /refused/);
});

test('this module can only verify: it holds no signing path and no way to name a key from outside', () => {
  const source = fs.readFileSync(path.join(__dirname, 'release-manifest-signature.js'), 'utf8');
  assert.doesNotMatch(source, /crypto\.sign\(|createPrivateKey|process\.env/);
});
