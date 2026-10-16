import assert from 'node:assert/strict';
import test from 'node:test';
import { latestOrdinaryUserMessageId } from '../src/transcript-scroll.ts';
import { chatToMarkdown } from '../src/transcriptExport.ts';
import { transcriptWindowStart } from '../src/chat/residency.ts';
import type { Chat } from '../src/store/types.ts';

// Review wa-subagent-1792184734224-6, suggestion 2: a wake-up notice is not
// the user asking to follow a turn.
test('a wake-up notice never counts as the latest ordinary user message', () => {
  const rows = [
    { id: 'u1', role: 'user' },
    { id: 'a1', role: 'assistant' },
    { id: 'n1', role: 'user', notice: { kind: 'subagent' } },
    { id: 'a2', role: 'assistant' },
  ];
  assert.equal(latestOrdinaryUserMessageId(rows), 'u1');
});

// Same review, correction 5: the export never attributes a notice to the user.
test('the Markdown export writes a notice as a quiet line, not as the user', () => {
  const md = chatToMarkdown({
    id: 't', title: 'Chat', messages: [
      { id: 'u', role: 'user', content: 'hola', status: 'done', at: null, events: [] },
      { id: 'n', role: 'user', content: '<task-notification>…', status: 'done', at: null, events: [], notice: { kind: 'subagent', title: 'Revisión', status: 'failed' } },
    ],
  } as unknown as Chat);
  assert.match(md, /### Tú\n\nhola/);
  assert.match(md, /_Revisión falló_/);
  assert.doesNotMatch(md, /task-notification/);
  assert.equal((md.match(/### Tú/g) || []).length, 1);
});

// Review wa-subagent-1792186068644-7, suggestion 2: wake-up notices never push
// the user's own message behind "Ver anteriores".
test('the history window anchors on the user message, not on a notice', () => {
  const rows = [
    { role: 'user' },
    ...Array.from({ length: 6 }, (_, i) => (i % 2 ? { role: 'assistant' } : { role: 'user', notice: { kind: 'subagent' } })),
  ];
  assert.equal(transcriptWindowStart(rows, 2), 0, 'the window reaches back to the real user message');
});
