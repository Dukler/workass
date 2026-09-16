import assert from 'node:assert/strict';
import test from 'node:test';
import { localBrowserOwnsChat } from '../src/browser.ts';
import { tagId } from '../src/wire/machineIds.ts';

test('native browser views stay controller-local while remote ownership requires the full machine-tagged pair', () => {
  assert.equal(localBrowserOwnsChat('local-tab'), true);
  assert.equal(localBrowserOwnsChat('local-tab', ''), true);
  assert.equal(localBrowserOwnsChat(tagId('machine-71', 'remote-tab'), 'machine-71'), true);
  assert.equal(localBrowserOwnsChat(tagId('machine-71', 'remote-tab')), false);
  assert.equal(localBrowserOwnsChat('remote-tab', 'machine-71'), false);
  assert.equal(localBrowserOwnsChat(''), false);
});

test('machine-tagged remote and untagged local ids with the same raw value never share native ownership', () => {
  const remoteTab = tagId('san-laptop', 'same-tab');
  const remoteChat = tagId('san-laptop', 'same-chat');

  assert.equal(remoteTab, 'M~san-laptop~same-tab');
  assert.equal(remoteChat, 'M~san-laptop~same-chat');
  assert.equal(localBrowserOwnsChat('same-tab'), true);
  assert.equal(localBrowserOwnsChat(remoteTab, 'san-laptop'), true);
  assert.equal(localBrowserOwnsChat('same-chat'), true);
  assert.equal(localBrowserOwnsChat(remoteChat, 'san-laptop'), true);
});

test('remote hydration with a machine tag cannot become local when machine metadata is missing', () => {
  assert.equal(localBrowserOwnsChat(tagId('san-laptop', 'hydrated-tab')), false);
  assert.equal(localBrowserOwnsChat('hydrated-tab', undefined), true);
});
