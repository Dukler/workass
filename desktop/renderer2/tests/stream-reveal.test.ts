import assert from 'node:assert/strict';
import { test } from 'node:test';
import { chunkCanReveal, revealedText, settleMountFades } from '../src/stream-reveal.ts';

test('a paragraph shows only its finished sentences', () => {
  assert.equal(revealedText('The labels agree. Gold kept the fill, while Sil'), 'The labels agree.');
  assert.equal(revealedText('Checked frame 3.5 and file.md, then wr'), '');
  assert.equal(revealedText('Done: it matches. Next'), 'Done: it matches.');
});

test('half-written markdown never shows', () => {
  assert.equal(revealedText('Intro line\n- the raw fl'), 'Intro line\n');
  assert.equal(revealedText('## Gold vs Sil'), '');
  assert.equal(revealedText('## Gold vs Silver\nThe'), '## Gold vs Silver\n');
  assert.equal(revealedText('| Frame | Gold |\n|---|-'), '');
  assert.equal(revealedText('| Frame | Gold |\n|---|---|\n| 3812 | filled |\n| 3815 | cle'), '| Frame | Gold |\n|---|---|\n| 3812 | filled |\n');
  assert.equal(revealedText('```python\ndiff = a ^ b\nys, xs = np.non'), '```python\ndiff = a ^ b\n');
  assert.equal(revealedText('```python\ndiff = a ^ b\n``'), '```python\ndiff = a ^ b\n');
  assert.equal(revealedText('```python\ndiff = a ^ b\n```\nAfter'), '```python\ndiff = a ^ b\n```\n');
});

test('finished list items show as they complete', () => {
  assert.equal(revealedText('- first item\n- second item\n- thi'), '- first item\n- second item\n');
  assert.equal(revealedText('- a full sentence. And mo'), '- a full sentence.');
});

test('the whole text shows once nothing is pending', () => {
  const text = 'Line one.\n\nLine two.\n';
  assert.equal(revealedText(text), text);
});

// Switching chats remounts a streaming reply: the text already on screen must
// not fade in again (user 2026-10-12). Only the reveal fade is settled; any
// other animation in the reply keeps running.
test('a reply mounting with text settles its reveal fades and nothing else', () => {
  const finished: string[] = [];
  const animation = (animationName: string) => ({ animationName, finish: () => finished.push(animationName) });
  const root = { getAnimations: () => [animation('mdfresh'), animation('workorbit'), animation('mdfresh')] } as unknown as Element;
  assert.equal(settleMountFades(root), 2);
  assert.deepEqual(finished, ['mdfresh', 'mdfresh']);
  assert.equal(settleMountFades(null), 0);
});

// Only a finished line or sentence can move the reveal, so only such a chunk
// re-renders a streaming reply; the rest paint the same screen (2026-10-13).
test('a chunk can change the reveal only by finishing a line or a sentence', () => {
  assert.equal(chunkCanReveal('The floor is cle', 'an and the'), false);
  assert.equal(chunkCanReveal('The floor is clean', '. Next'), true);
  assert.equal(chunkCanReveal('The floor is clean.', ' Next'), true, 'the space after a period ends the sentence');
  assert.equal(chunkCanReveal('He said "done.', '" Then'), true, 'a closing quote after the period counts');
  assert.equal(chunkCanReveal('version 3', '.5 of the'), false, 'a decimal point is not a sentence end');
  assert.equal(chunkCanReveal('- item one', '\n- item'), true, 'a finished line');
});
