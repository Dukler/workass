import assert from 'node:assert/strict';
import test from 'node:test';
import { localBrowserOwnsChat } from '../src/browser.ts';
import { tagId } from '../src/wire/machineIds.ts';

test('native browser ownership is local-only and fails closed on incomplete remote hydration', () => {
  assert.equal(localBrowserOwnsChat('local-tab'), true);
  assert.equal(localBrowserOwnsChat('local-tab', ''), true);
  assert.equal(localBrowserOwnsChat(tagId('machine-71', 'remote-tab'), 'machine-71'), false);
  assert.equal(localBrowserOwnsChat(tagId('machine-71', 'remote-tab')), false);
  assert.equal(localBrowserOwnsChat('remote-tab', 'machine-71'), false);
});
