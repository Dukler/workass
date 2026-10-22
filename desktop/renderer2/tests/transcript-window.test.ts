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

// Every loaded row is part of the transcript (windowing mounts what the screen
// needs, transcript-windowing.test.ts); the band under a short window cannot
// come from rows held back in memory.
test('the transcript shows more of what is loaded until the screen is full', () => {
  assert.match(transcript, /: visibleMessages;/);
  assert.doesNotMatch(transcript, /setReveal\(/);
});

// The screen-filling step first used only rows already in memory, so the band
// stayed whenever the older rows were still on disk ("Ver 40 mensajes
// anteriores" with nothing loaded to fill from).
test('a short transcript fills the screen from disk when memory has nothing older', () => {
  assert.match(transcript, /if \(hidden <= 0 \|\| historyLoadingNow\.current \|\| chat\.historyComplete \|\| autoFillStalled\.current \|\| olderRetryPending\(\) \|\| sliceArriving\(\)\) return;/);
  // A load that brings nothing stops it; a read that failed is tried again.
  assert.match(transcript, /void revealOlder\(\)\.then\(\(outcome\) => \{\s*if \(outcome !== 'failed' && \(store\.chat\(chatID\)\?\.messages\.length \?\? 0\) <= before\) autoFillStalled\.current = true;/);
});

// A 225 MB chat's automatic upward load stopped for good on its first failed
// read (a timeout behind thousands of queued image reads) and fell back to
// "Ver anteriores": five clicks and 199 s to reach the first message.
test('a failed older read is retried with a growing pause; only an empty one stops the load', () => {
  assert.match(transcript, /void revealOlder\(\)\.then\(\(outcome\) => \{ if \(outcome === 'empty'\) olderStalledAt\.current = before; \}\);/);
  assert.match(transcript, /el\.scrollTop > TRANSCRIPT_LOAD_AHEAD_PX \|\| olderRetryPending\(\) \|\| sliceArriving\(\)\) return;/);
  assert.match(transcript, /retry\.delay = Math\.min\(OLDER_RETRY_MAX_MS, Math\.max\(OLDER_RETRY_MIN_MS, retry\.delay \* 2\)\);/);
  assert.match(transcript, /if \(outcome === 'empty'\) restoreStalled\.current = true;/);
});

// A chat opened with nothing on screen paints its last exchange while the rest
// of its recent slice arrives. Reading older history before that slice would
// load a page the slice may already fill the screen without, so every
// automatic older read (screen fill, read-ahead, position restore) waits for
// it; the store update that completes the slice renders the transcript again.
test('automatic older reads wait for an opened chat\'s recent slice', () => {
  assert.match(transcript, /const sliceArriving = \(\): boolean => !!chat && store\.showsFirstScreenOnly\(chat\.id\);/);
  assert.equal(transcript.match(/sliceArriving\(\)/g)?.length, 3);
  assert.match(transcript, /if \(step !== 'load' \|\| olderRetryPending\(\) \|\| sliceArriving\(\)\) return;/);
});

// Holding the tallest height for a whole long-running turn kept any real
// shrink as a band of nothing under the newest line.
test('the streaming height hold is released shortly after growth stops', () => {
  assert.match(transcript, /holdRelease = window\.setTimeout\(\(\) => \{\s*holdRelease = null;\s*heldHeight\.current = 0;\s*doc\?\.style\.removeProperty\('min-height'\);/);
});
