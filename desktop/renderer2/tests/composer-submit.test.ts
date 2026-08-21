import assert from 'node:assert/strict';
import test from 'node:test';
import { composerSubmitIntent, restoreRejectedSteerDraft } from '../src/composer-submit.ts';

test('Enter queues and Command+Enter steers only while a turn is running', () => {
  assert.equal(composerSubmitIntent(true, false), 'queue');
  assert.equal(composerSubmitIntent(true, true), 'steer');
  assert.equal(composerSubmitIntent(false, false), 'send');
  assert.equal(composerSubmitIntent(false, true), 'send');
});

test('a rejected steer returns its exact draft without overwriting newer typing', () => {
  assert.equal(restoreRejectedSteerDraft('rejected direction', ''), 'rejected direction');
  assert.equal(
    restoreRejectedSteerDraft('rejected direction', 'new draft typed during admission'),
    'rejected direction\n\nnew draft typed during admission',
  );
  assert.equal(restoreRejectedSteerDraft('already restored', 'already restored'), 'already restored');
});
