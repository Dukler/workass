import assert from 'node:assert/strict';
import test from 'node:test';
import { composerSubmitIntent } from '../src/composer-submit.ts';

test('Enter queues and Command+Enter steers only while a turn is running', () => {
  assert.equal(composerSubmitIntent(true, false), 'queue');
  assert.equal(composerSubmitIntent(true, true), 'steer');
  assert.equal(composerSubmitIntent(false, false), 'send');
  assert.equal(composerSubmitIntent(false, true), 'send');
});
