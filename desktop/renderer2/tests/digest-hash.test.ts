import assert from 'node:assert/strict';
import test from 'node:test';
import { catalogHashKey, goJSON, stateDigestHash } from '../src/store/digest-hash.ts';

// state:digest hashes are SHA-256 of Go's json.Marshal output. The same fixture
// and literal are asserted by TestStateDigestHashParityFixture in
// cmd/workass/provider_catalog_read_test.go.
const GO_PARITY_HASH = '099b0cc7cebd6ca886d1127a911b9e8c4e6797a64ba5f46ca8236366797798fc';

// The group as the renderer receives it: parsed from the daemon's JSON, so its
// keys keep Go's order (struct fields in order, map keys sorted).
const fixture = JSON.parse(String.raw`{"providerId":"mock","providerName":"Mock <beta> & co","models":[{"modelId":"m1","name":"Model \u2028 one","efforts":["low"]}],"modes":[],"permissionIntents":{"read":"plan","write":"default"},"status":"error","latencyMs":812,"error":"exec: a -> b && c > d é"}`);

test('a digest hash is computed over the bytes Go writes, escapes included', async () => {
  assert.equal(await stateDigestHash(fixture), GO_PARITY_HASH);
  // Escapes spelled out with a computed backslash, so no tool or editor can
  // fold them back into the characters they stand for.
  const esc = (hex: string) => `${String.fromCharCode(92)}u${hex}`;
  const command = 'npm run dev > log 2>&1 && echo <ok>';
  assert.equal(goJSON({ cmd: command }), `{"cmd":"npm run dev ${esc('003e')} log 2${esc('003e')}${esc('0026')}1 ${esc('0026')}${esc('0026')} echo ${esc('003c')}ok${esc('003e')}"}`);
  assert.deepEqual(JSON.parse(goJSON({ cmd: command })), { cmd: command }, 'still the same value');
  assert.equal(goJSON(String.fromCharCode(0x2028, 0x2029)), `"${esc('2028')}${esc('2029')}"`);
  assert.equal(goJSON(undefined), 'null');
});

test('a catalog hash map has one identity whatever its key order', () => {
  assert.equal(catalogHashKey({ b: '2', a: '1' }), catalogHashKey({ a: '1', b: '2' }));
  assert.notEqual(catalogHashKey({ a: '1' }), catalogHashKey({ a: '2' }));
  assert.equal(catalogHashKey(null), catalogHashKey({}));
});
