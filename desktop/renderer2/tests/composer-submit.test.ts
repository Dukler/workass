import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import { composerButtonAction, composerKeyAction, composerSubmitIntent, restoreRejectedSteerDraft } from '../src/composer-submit.ts';

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

test('repeated steering shortcuts never become Stop; only a distinct empty-button click does', () => {
  const shortcut = { key: 'Enter', shiftKey: false, ctrlKey: false, metaKey: true };
  assert.equal(composerKeyAction(true, shortcut, false), 'steer');
  assert.equal(composerKeyAction(true, shortcut, false), 'steer');
  assert.equal(composerButtonAction(true, true, 1), 'steer');
  assert.equal(composerButtonAction(true, false, 2), null, 'the second click after a steer must not hit Stop');
  assert.equal(composerButtonAction(true, false, 1), 'stop', 'a distinct empty-button click remains explicit Stop');
  assert.equal(composerButtonAction(false, true, 1), 'send');
});

test('the live composer keeps Stop out of keyboard submission', () => {
  const composer = readFileSync(new URL('../src/components/Composer.tsx', import.meta.url), 'utf8');
  const submit = composer.slice(composer.indexOf('async function submit('), composer.indexOf('function keydown('));
  assert.doesNotMatch(submit, /cancelChatTurn/, 'an empty steering shortcut must never call Stop');
  assert.match(composer, /composerButtonAction\(running, hasText, event\.detail\)/,
    'the live button must distinguish a separate Stop click from repeated steering clicks');
});

test('a rejected steer never puts submitted text back into the composer', () => {
  assert.equal(restoreRejectedSteerDraft('rejected direction', ''), '');
  assert.equal(
    restoreRejectedSteerDraft('rejected direction', 'new draft typed during admission'),
    'new draft typed during admission',
  );
  assert.equal(restoreRejectedSteerDraft('already restored', 'already restored'), 'already restored');
});
