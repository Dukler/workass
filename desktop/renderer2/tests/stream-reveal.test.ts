import assert from 'node:assert/strict';
import { test } from 'node:test';
import { revealedText } from '../src/stream-reveal.ts';

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
