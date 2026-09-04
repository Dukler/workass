import assert from 'node:assert/strict';
import test from 'node:test';
import { composerSubmitIntent, restoreRejectedSteerDraft } from '../src/composer-submit.ts';

test('Enter queues and the platform command modifier steers only while a turn is running', () => {
  assert.equal(composerSubmitIntent(true, { metaKey: false, ctrlKey: false }), 'queue');
  assert.equal(composerSubmitIntent(true, { metaKey: true, ctrlKey: false }), 'steer', 'Command+Enter steers on macOS');
  assert.equal(composerSubmitIntent(true, { metaKey: false, ctrlKey: true }), 'steer', 'Ctrl+Enter steers on Windows/Linux');
  assert.equal(composerSubmitIntent(false, { metaKey: false, ctrlKey: false }), 'send');
  assert.equal(composerSubmitIntent(false, { metaKey: true, ctrlKey: false }), 'send');
  assert.equal(composerSubmitIntent(false, { metaKey: false, ctrlKey: true }), 'send');
});

test('a rejected steer returns its exact draft without overwriting newer typing', () => {
  assert.equal(restoreRejectedSteerDraft('rejected direction', ''), 'rejected direction');
  assert.equal(
    restoreRejectedSteerDraft('rejected direction', 'new draft typed during admission'),
    'rejected direction\n\nnew draft typed during admission',
  );
  assert.equal(restoreRejectedSteerDraft('already restored', 'already restored'), 'already restored');
});
