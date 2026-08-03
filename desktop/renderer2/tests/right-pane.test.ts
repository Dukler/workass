import assert from 'node:assert/strict';
import test from 'node:test';
import type { Chat, RightPane } from '../src/store/types.ts';
import { chatPane, nextPane } from '../src/store/right-pane.ts';

function chat(pane?: RightPane | null): Pick<Chat, 'pane'> {
  return pane === undefined ? {} : { pane };
}

test('a chat that never chose defaults to the info rail; null means explicitly closed', () => {
  assert.equal(chatPane(chat()), 'rail');        // undefined → historical default
  assert.equal(chatPane(chat(null)), null);      // closed
  assert.equal(chatPane(chat('rail')), 'rail');
  assert.equal(chatPane(chat('browser')), 'browser');
  assert.equal(chatPane(null), null);            // no active chat → no pane
});

test('toggling the shown occupant closes the column; a different one switches to it', () => {
  // Default (rail) → click rail closes it.
  assert.equal(nextPane(chatPane(chat()), 'rail'), null);
  // Rail shown → click browser switches.
  assert.equal(nextPane('rail', 'browser'), 'browser');
  // Browser shown → click browser closes.
  assert.equal(nextPane('browser', 'browser'), null);
  // Closed → click rail opens it.
  assert.equal(nextPane(null, 'rail'), 'rail');
});

test('right-column occupancy is per-chat: one chat never disturbs another', () => {
  // The whole point of the redesign (user law 2026-07-12): an agent opening a
  // browser in ITS chat must not touch the chat you are viewing. Each chat owns
  // its own `pane`, so writing one leaves the others untouched.
  const viewing: Pick<Chat, 'pane'> = {};              // you: defaults to rail
  const agentChat: Pick<Chat, 'pane'> = {};            // another chat
  agentChat.pane = 'browser';                          // agent opens a browser there
  assert.equal(chatPane(agentChat), 'browser');
  assert.equal(chatPane(viewing), 'rail');             // your view is unchanged
});
