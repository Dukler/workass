import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';

test('closing a chat flushes its daemon delete capability immediately', () => {
  const source = readFileSync(new URL('../src/store/store.ts', import.meta.url), 'utf8');
  const closeChat = source.match(/closeChat\(id: string\) \{([\s\S]*?)\n  \}\n  setDraft/)?.[1] ?? '';

  assert.match(closeChat, /this\.pendingChatDeletes\.add\(id\)/);
  assert.match(closeChat, /void this\.flushSession\(true\)/);
});
