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
  assert.match(transcript, /doc\.offsetHeight >= el\.clientHeight\) return;\s*if \(residentHidden > 0\) \{\s*setReveal\(/);
});

// The screen-filling step first used only rows already in memory, so the band
// stayed whenever the older rows were still on disk ("Ver 40 mensajes
// anteriores" with nothing loaded to fill from).
test('a short transcript fills the screen from disk when memory has nothing older', () => {
  assert.match(transcript, /if \(hidden <= 0 \|\| historyLoading \|\| chat\.historyComplete \|\| autoFillStalled\.current\) return;/);
  assert.match(transcript, /void revealOlder\(\)\.then\(\(\) => \{\s*if \(\(store\.chat\(chatID\)\?\.messages\.length \?\? 0\) <= before\) autoFillStalled\.current = true;/);
});

// Holding the tallest height for a whole long-running turn kept any real
// shrink as a band of nothing under the newest line.
test('the streaming height hold is released shortly after growth stops', () => {
  assert.match(transcript, /holdRelease = window\.setTimeout\(\(\) => \{\s*holdRelease = null;\s*heldHeight\.current = 0;\s*doc\?\.style\.removeProperty\('min-height'\);/);
});
