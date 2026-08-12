import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';

test('closing a chat flushes its daemon delete capability immediately', () => {
  const source = readFileSync(new URL('../src/store/store.ts', import.meta.url), 'utf8');
  const closeChat = source.match(/closeChat\(id: string\) \{([\s\S]*?)\n  \}\n  private async closeChatDurably/)?.[1] ?? '';

  // Chat deletion is now an actor command. The renderer must not encode a
  // delete in an omitted session snapshot or maintain the removed legacy set.
  assert.match(closeChat, /void this\.closeChatDurably\(id\)/);
  const durableClose = source.match(/private async closeChatDurably\(id: string\) \{([\s\S]*?)\n  \}\n  setDraft/)?.[1] ?? '';
  assert.match(durableClose, /call\('chatDelete'/);
  assert.match(durableClose, /void this\.flushSession\(true\)/);
  assert.doesNotMatch(source, /pendingChatDeletes/);
});
