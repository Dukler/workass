import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import { findChatMessageMatches, findMatchOffsets, isChatFindShortcut, nextFindIndex } from '../src/chat-find.ts';

test('Cmd+F and Ctrl+F are the exact chat-find shortcuts', () => {
  const base = { key: 'f', metaKey: false, ctrlKey: false, altKey: false, shiftKey: false };
  assert.equal(isChatFindShortcut({ ...base, metaKey: true }), true);
  assert.equal(isChatFindShortcut({ ...base, ctrlKey: true }), true);
  assert.equal(isChatFindShortcut({ ...base, key: 'F', metaKey: true }), true);
  assert.equal(isChatFindShortcut({ ...base, metaKey: true, shiftKey: true }), false);
  assert.equal(isChatFindShortcut({ ...base, metaKey: true, altKey: true }), false);
  assert.equal(isChatFindShortcut({ ...base, key: 'k', metaKey: true }), false);
});

test('chat find is case-insensitive and returns non-overlapping offsets', () => {
  assert.deepEqual(findMatchOffsets('One one ONE', 'one'), [
    { start: 0, end: 3 },
    { start: 4, end: 7 },
    { start: 8, end: 11 },
  ]);
  assert.deepEqual(findMatchOffsets('aaaa', 'aa'), [
    { start: 0, end: 2 },
    { start: 2, end: 4 },
  ]);
  assert.deepEqual(findMatchOffsets('history', ''), []);
  assert.deepEqual(findMatchOffsets('İstanbul', 'i'), [{ start: 0, end: 1 }],
    'Unicode case folding must map matches back onto safe original offsets');
});

test('the full chat is searched semantically without indexing renderer controls', () => {
  assert.deepEqual(findChatMessageMatches([
    { id: 'user', content: 'first needle' },
    { id: 'assistant', content: 'needle twice: needle', result: 'final needle' },
  ], 'needle'), [
    { messageId: 'user', messageIndex: 0, occurrence: 0 },
    { messageId: 'assistant', messageIndex: 1, occurrence: 0 },
    { messageId: 'assistant', messageIndex: 1, occurrence: 1 },
    { messageId: 'assistant', messageIndex: 1, occurrence: 2 },
  ]);
});

test('previous and next navigation wrap around the match list', () => {
  assert.equal(nextFindIndex(0, 3, 1), 1);
  assert.equal(nextFindIndex(2, 3, 1), 0);
  assert.equal(nextFindIndex(0, 3, -1), 2);
  assert.equal(nextFindIndex(4, 0, 1), 0);
});

test('the transcript shortcut searches semantic history in a bounded render window and owns Escape', () => {
  const transcript = readFileSync(new URL('../src/components/Transcript.tsx', import.meta.url), 'utf8');
  const styles = readFileSync(new URL('../src/styles/app.css', import.meta.url), 'utf8');
  assert.match(transcript, /window\.addEventListener\('keydown', onKey, true\)/);
  assert.match(transcript, /event\.stopImmediatePropagation\(\)/);
  assert.match(transcript, /findChatMessageMatches\(visibleMessages, findQuery\)/);
  assert.match(transcript, /searchStart \+ WINDOW/);
  assert.doesNotMatch(transcript, /setReveal\(total\)/);
  assert.match(transcript, /data-chat-find-message=\{m\.id\}/);
  assert.match(styles, /::highlight\(workass-chat-find-current\)/);
});
