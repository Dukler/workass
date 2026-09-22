import assert from 'node:assert/strict';
import test from 'node:test';
import { composerKeyAction, composerSubmitIntent, restoreRejectedSteerDraft } from '../src/composer-submit.ts';

test('one explicit shortcut steers even with autocomplete open; ordinary Enter still picks or queues', () => {
  const enter = { key: 'Enter', shiftKey: false, ctrlKey: false, metaKey: false };
  for (const modifier of ['ctrlKey', 'metaKey']) {
    const shortcut = { ...enter, [modifier]: true };
    for (const popupOpen of [false, true]) {
      assert.equal(composerKeyAction(true, shortcut, popupOpen), 'steer');
      assert.equal(composerKeyAction(false, shortcut, popupOpen), 'send');
      assert.equal(composerKeyAction(true, { ...shortcut, shiftKey: true }, popupOpen), null);
    }
  }
  assert.equal(composerKeyAction(true, enter, true), 'pick');
  assert.equal(composerKeyAction(true, enter, false), 'queue');
  assert.equal(composerKeyAction(true, { ...enter, key: 'Tab' }, true), 'pick');
  assert.equal(composerKeyAction(true, { ...enter, key: 'ArrowDown' }, true), 'next');
  assert.equal(composerKeyAction(true, { ...enter, key: 'ArrowUp' }, true), 'previous');
  assert.equal(composerKeyAction(true, { ...enter, key: 'Escape' }, true), 'dismiss');
  assert.equal(composerKeyAction(true, { ...enter, shiftKey: true }, true), null);
});

test('Enter queues and the platform command modifier steers only while a turn is running', () => {
  assert.equal(composerSubmitIntent(true, { metaKey: false, ctrlKey: false }), 'queue');
  assert.equal(composerSubmitIntent(true, { metaKey: true, ctrlKey: false }), 'steer', 'Command+Enter steers on macOS');
  assert.equal(composerSubmitIntent(true, { metaKey: false, ctrlKey: true }), 'steer', 'Ctrl+Enter steers on Windows/Linux');
  assert.equal(composerSubmitIntent(false, { metaKey: false, ctrlKey: false }), 'send');
  assert.equal(composerSubmitIntent(false, { metaKey: true, ctrlKey: false }), 'send');
  assert.equal(composerSubmitIntent(false, { metaKey: false, ctrlKey: true }), 'send');
});

test('a rejected steer never puts submitted text back into the composer', () => {
  assert.equal(restoreRejectedSteerDraft('rejected direction', ''), '');
  assert.equal(
    restoreRejectedSteerDraft('rejected direction', 'new draft typed during admission'),
    'new draft typed during admission',
  );
  assert.equal(restoreRejectedSteerDraft('already restored', 'already restored'), 'already restored');
});
