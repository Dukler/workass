import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { CHAT_INITIAL_HISTORY, CHAT_MESSAGE_TAIL, transcriptWindowStart } from '../src/chat/residency.ts';

const transcript = readFileSync(new URL('../src/components/Transcript.tsx', import.meta.url), 'utf8');
const rows = (roles: string) => [...roles].map((r) => ({ role: r === 'u' ? 'user' : 'assistant' }));

test('a burst of short agent replies never hides the user\'s own message', () => {
  // The user asked, then 15 reviewers woke the agent one at a time.
  const messages = rows('ua' + 'u' + 'a'.repeat(16));
  assert.equal(transcriptWindowStart(messages, CHAT_INITIAL_HISTORY), 2);
});

test('an ordinary window is unchanged when the user message is already in it', () => {
  const messages = rows('uauauauauauauaua');
  assert.equal(transcriptWindowStart(messages, CHAT_INITIAL_HISTORY), messages.length - CHAT_INITIAL_HISTORY);
});

test('the reach back is bounded by the resident tail', () => {
  const messages = rows('u' + 'a'.repeat(CHAT_MESSAGE_TAIL + 20));
  assert.equal(transcriptWindowStart(messages, CHAT_INITIAL_HISTORY), messages.length - CHAT_INITIAL_HISTORY);
});

test('the transcript shows more of what is loaded until the screen is full', () => {
  assert.match(transcript, /visibleMessages\.slice\(transcriptWindowStart\(visibleMessages, reveal\)\)/);
  assert.match(transcript, /if \(doc\.offsetHeight < el\.clientHeight\) \{\s*setReveal\(/);
});
