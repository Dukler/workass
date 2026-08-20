import assert from 'node:assert/strict';
import test from 'node:test';
import { workassRuntimeProfile } from '../src/model-catalog.ts';

test('runtime profile accepts only the daemon profiles that change diagnostics', () => {
  assert.equal(workassRuntimeProfile('dev'), 'dev');
  assert.equal(workassRuntimeProfile('test'), 'test');
  assert.equal(workassRuntimeProfile('prod'), 'prod');
  assert.equal(workassRuntimeProfile('unknown'), 'prod');
  assert.equal(workassRuntimeProfile(null), 'prod');
});
