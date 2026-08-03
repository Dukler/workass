import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { adoptsTerminalJobResult, appendAssistantChunk, fullAssistantText, normalizeAssistantMessagePhase, restoredAssistantContent, restoredAssistantResult } from '../src/assistant-output.ts';

test('provider-typed final answer has one result owner while commentary remains normal prose', () => {
  const msg: { content: string; result?: string } = { content: '' };
  appendAssistantChunk(msg, 'working ', 'commentary');
  appendAssistantChunk(msg, 'notes', 'commentary');
  appendAssistantChunk(msg, 'final ', 'final_answer');
  appendAssistantChunk(msg, 'report', 'final_answer');
  assert.deepEqual(msg, { content: 'working notes', result: 'final report' });
  assert.equal(fullAssistantText(msg), 'working notes\n\nfinal report');
});

test('phase-less and unknown providers retain the legacy transcript path', () => {
  const msg: { content: string; result?: string } = { content: '' };
  appendAssistantChunk(msg, 'legacy', undefined);
  appendAssistantChunk(msg, ' future', 'future_phase');
  assert.deepEqual(msg, { content: 'legacy future' });
  assert.equal(normalizeAssistantMessagePhase('final_answer'), 'final_answer');
  assert.equal(normalizeAssistantMessagePhase('future_phase'), null);
});

test('legacy empty cancellation prose migrates to status-only chrome', () => {
  assert.equal(restoredAssistantContent('assistant', 'cancelled', 'Detenido.'), '');
  assert.equal(restoredAssistantContent('assistant', 'cancelled', '  Detenido.  '), '');
  assert.equal(restoredAssistantContent('assistant', 'cancelled', 'partial work'), 'partial work');
  assert.equal(restoredAssistantContent('assistant', 'done', 'Detenido.'), 'Detenido.');
  assert.equal(restoredAssistantContent('user', 'cancelled', 'Detenido.'), 'Detenido.');
});

test('the combined terminal result is adopted only by a turn this renderer never streamed', () => {
  assert.equal(adoptsTerminalJobResult([{ content: '', result: undefined }]), true);
  assert.equal(adoptsTerminalJobResult([{ content: 'streamed prose', result: undefined }]), false);
  assert.equal(adoptsTerminalJobResult([{ content: '', result: 'typed answer' }]), false);
  // A steer split leaves the text on the head segment and an empty continuation.
  assert.equal(adoptsTerminalJobResult([{ content: 'pre-steer', result: undefined }, { content: '', result: undefined }]), false);
  assert.equal(adoptsTerminalJobResult([]), true);
});

test('an exact content/result twin is healed on restore', () => {
  assert.equal(restoredAssistantResult('same text', 'same text'), undefined);
  assert.equal(restoredAssistantResult('commentary', 'final answer'), 'final answer');
  assert.equal(restoredAssistantResult('', ''), '');   // empty stays empty; only a real twin is dropped
  assert.equal(restoredAssistantResult('anything', null), undefined);
  assert.equal(restoredAssistantResult('', 'answer with no commentary'), 'answer with no commentary');
});

test('typed result renders as ordinary assistant Markdown without dedicated presentation', () => {
  const component = readFileSync(new URL('../src/components/AssistantMessage.tsx', import.meta.url), 'utf8');
  const css = readFileSync(new URL('../src/styles/app.css', import.meta.url), 'utf8');
  assert.match(component, /resultBlocks\.map\(\(block, index\) => <MarkdownBlock/);
  assert.doesNotMatch(component, /FinalResult|finalresult|Resultado/);
  assert.doesNotMatch(css, /finalresult/);
});
