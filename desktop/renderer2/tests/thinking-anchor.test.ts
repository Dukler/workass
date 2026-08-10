import assert from 'node:assert/strict';
import test from 'node:test';
import { readFileSync } from 'node:fs';

const assistant = readFileSync(new URL('../src/components/AssistantMessage.tsx', import.meta.url), 'utf8');
const transcript = readFileSync(new URL('../src/components/Transcript.tsx', import.meta.url), 'utf8');
const styles = readFileSync(new URL('../src/styles/app.css', import.meta.url), 'utf8');

test('the running pulse is scrollport chrome after every transcript row, including steered user messages', () => {
  assert.match(assistant, /!running && thinkEv && <StepRow ev=\{thinkEv\}/);
  assert.doesNotMatch(assistant, /running \? <TurnPulse/);
  assert.match(transcript, /className=\{`transcriptviewport\$\{runningMessage \? ' has-live' : ''\}`\}/);
  assert.match(transcript, /\{runningMessage && <LiveTurnPulse msg=\{runningMessage\} \/>\}/);
  const messageRows = transcript.indexOf('{shown.map((m) => {');
  const livePulse = transcript.indexOf('{runningMessage && <LiveTurnPulse');
  assert.ok(messageRows >= 0 && livePulse > messageRows);
  assert.match(transcript.slice(messageRows, livePulse), /<UserPill/);
});

test('the live pulse has a physical bottom anchor independent of document height', () => {
  assert.match(styles, /\.transcriptviewport\s*\{[^}]*position:\s*relative;[^}]*--thinklive-h:\s*40px;/s);
  assert.match(styles, /\.thinklive\s*\{[^}]*position:\s*absolute;[^}]*bottom:\s*0;/s);
  assert.doesNotMatch(styles, /\.thinklive\s*\{[^}]*position:\s*sticky;/s);
  assert.match(styles, /\.transcriptviewport\.has-live \.doc\s*\{[^}]*padding-bottom:\s*calc\(var\(--doc-pad-b\) \+ var\(--thinklive-h\)\);/s);
});
