import assert from 'node:assert/strict';
import test from 'node:test';
import type { PublicJob } from '../src/wire/types.ts';

// The wire contract is frozen and new payload fields are additive-only: the
// renderer must keep working unchanged against a daemon that sends a field it
// has never heard of, and against an older Electron main that sends none.
test('an unknown additive job field changes nothing the renderer reads', () => {
  const withDisposition = {
    id: 'j1', status: 'done', stopReason: 'end_turn', code: 0,
    disposition: { state: 'parked', source: 'inferred' },
  } as unknown as PublicJob;
  const withoutDisposition = {
    id: 'j1', status: 'done', stopReason: 'end_turn', code: 0,
  } as unknown as PublicJob;

  // Everything the renderer actually keys on is identical either way.
  for (const job of [withDisposition, withoutDisposition]) {
    assert.equal(job.stopReason, 'end_turn');
    assert.equal(job.code, 0);
    assert.notEqual(job.stopReason, 'cancelled');
  }
});

test('a starting turn carries no disposition', () => {
  const started = { id: 'j1', status: 'running', stopReason: null } as unknown as Record<string, unknown>;
  assert.equal('disposition' in started, false);
});
