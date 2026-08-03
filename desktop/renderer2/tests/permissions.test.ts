import assert from 'node:assert/strict';
import test from 'node:test';
import type { Chat } from '../src/store/types.ts';
import { clearPermissionById, clearPermissionsOutsideSnapshot } from '../src/permissions.ts';

function chatWithPermissions(): Chat {
  return {
    id: 'tab-1', chatId: 'chat-1', messages: [
      { id: 'm-1', role: 'assistant', content: '', result: '', events: [], status: 'running', at: null, permission: { id: 'perm-live', title: 'Live', kind: 'execute', options: [] } },
      { id: 'm-2', role: 'assistant', content: '', result: '', events: [], status: 'running', at: null, permission: { id: 'perm-stale', title: 'Stale', kind: 'execute', options: [] } },
    ],
  } as unknown as Chat;
}

test('a permission resolution removes only its exact transcript owner', () => {
  const chat = chatWithPermissions();
  assert.deepEqual(clearPermissionById([chat], 'perm-stale'), ['m-2']);
  assert.equal(chat.messages[0].permission?.id, 'perm-live');
  assert.equal(chat.messages[1].permission, undefined);
});

test('pending-permission hydration removes stale cards but retains live requests', () => {
  const chat = chatWithPermissions();
  assert.deepEqual(clearPermissionsOutsideSnapshot([chat], new Set(['perm-live'])), ['m-2']);
  assert.equal(chat.messages[0].permission?.id, 'perm-live');
  assert.equal(chat.messages[1].permission, undefined);
});
