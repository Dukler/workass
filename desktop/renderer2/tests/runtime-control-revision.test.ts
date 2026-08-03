import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';

test('daemon runtime-control revision crosses the renderer mirror unchanged', () => {
  const persistence = readFileSync(new URL('../src/store/persistence.ts', import.meta.url), 'utf8');
  const types = readFileSync(new URL('../src/store/types.ts', import.meta.url), 'utf8');
  const store = readFileSync(new URL('../src/store/store.ts', import.meta.url), 'utf8');

  assert.match(persistence, /runtimeControlRevision\?: number/);
  assert.match(types, /runtimeControlRevision\?: number/);
  assert.match(store, /runtimeControlRevision: c\.runtimeControlRevision/);
  assert.match(store, /runtimeControlRevision: Number\.isInteger\(c\.runtimeControlRevision\)/);
});
