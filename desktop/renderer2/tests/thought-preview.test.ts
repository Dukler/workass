import assert from 'node:assert/strict';
import { test } from 'node:test';
import { fullThoughtPreview, thoughtPreview } from '../src/thought-preview.ts';

// The collapsed reasoning line keeps per-thought state so a streaming thought
// costs only its new chunk (2026-11-12, energy lane). At every length, chunked
// any way, it must equal the whole-text definition.
const samples = [
  'Locating the review tooling. The handoff points at tools/silver_review.py, so I will read it first.',
  '**Inspecting the mask**\n\nThe mask in frame 3812 fills the gap.',
  '**Bold that is not alone** on its line. Then more.',
  '  \n\n  leading blank lines then a sentence. And another one.',
  '- list mark first\n- second item. done\n1. numbered\n## heading here. after',
  'see [the docs](https://example.com/a.b) for details. then [an open link\nthat closes](x.y) later. end',
  'a `code.x` tick and *emphasis.* and _under_score. **bold.** ~~gone~~ done',
  'no sentence end at all\njust lines\nand lines',
  'quote ends "like this." next',
  'question? yes! (paren.) next',
  'array[i] and x[0] then a [bracket\nwithout end and a ]( weird\n(paren) here. tail',
  'line one\r\nline two. three\r\n',
  'unicode space. next and line',
  'The user is\nsaying "\nping —\npost -cr ash\n-fix recovery test ." I\nshould confirm\nthe recovery',
  '*star at line start* text.\n*another emphasis* here. x',
  'trailing dot.',
  'trailing dot.\n',
  'trailing dot.   \n\n',
  '**Title**',
  '**Title** not alone',
  '',
  '\n\n\n',
  '[a](b) [c](d\n) e. f',
  'word\n**x** y. z\n_a_ b.',
];

function lcg(seed: number) {
  let s = seed;
  return () => { s = (s * 1103515245 + 12345) % 2147483648; return s / 2147483648; };
}

test('the streaming preview equals the whole-text preview at every length', () => {
  samples.forEach((sample, n) => {
    for (let length = 0; length <= sample.length; length++) {
      const text = sample.slice(0, length);
      assert.equal(thoughtPreview(text, `sample-${n}`), fullThoughtPreview(text), `sample ${n} at length ${length}`);
    }
  });
});

test('random markdown streamed in random chunks matches at every step', () => {
  const pieces = ['word', ' ', '  ', '\n', '\n\n', '. ', '.', '? ', '!', '"', '”', ')', '(', '[', ']', '](', 'http://x.y', '*', '**', '_', '`', '~~',
    '# ', '- ', '1. ', '\t', '\r\n', 'é', ' ', 'x', 'y', ' and ', '[link](u)'];
  for (let seed = 1; seed <= 1500; seed++) {
    const rand = lcg(seed);
    let text = '';
    const parts = 20 + Math.floor(rand() * 120);
    for (let i = 0; i < parts; i++) text += pieces[Math.floor(rand() * pieces.length)];
    let at = 0;
    const key = `random-${seed}`;
    while (at < text.length) {
      at = Math.min(text.length, at + 1 + Math.floor(rand() * 9));
      const prefix = text.slice(0, at);
      assert.equal(thoughtPreview(prefix, key), fullThoughtPreview(prefix), `seed ${seed} at ${at}: ${JSON.stringify(prefix)}`);
    }
  }
});

test('long thoughts, read whole or streamed in big chunks, match too (the piece-by-piece path)', () => {
  // Rarely a sentence end: mostly words, newlines, links spanning lines, emphasis.
  const pieces = ['word ', 'word ', 'word ', 'and ', '\n', '\n', '[a link\nacross](u) ', '[open ', '](', ') ', '*em* ', '`x` ', '- ', '# ',
    'e.g ', '3.5 ', ' ', '\t', '\r\n', '**b** '];
  for (let seed = 1; seed <= 60; seed++) {
    const rand = lcg(seed * 7919);
    let text = '';
    const size = 6_000 + Math.floor(rand() * 40_000);
    while (text.length < size) text += rand() < 0.0015 ? 'end. ' : pieces[Math.floor(rand() * pieces.length)];
    if (seed % 3 === 0) text = text.replace(/\. /g, ' ');   // no sentence end at all
    assert.equal(thoughtPreview(text, `whole-${seed}`), fullThoughtPreview(text), `whole seed ${seed}`);
    let at = 0;
    while (at < text.length) {
      at = Math.min(text.length, at + 1 + Math.floor(rand() * 5_000));
      const prefix = text.slice(0, at);
      assert.equal(thoughtPreview(prefix, `big-${seed}`), fullThoughtPreview(prefix), `seed ${seed} at ${at}`);
      assert.equal(thoughtPreview(prefix, `big-${seed}`), fullThoughtPreview(prefix), `seed ${seed} at ${at}, again`);
    }
  }
});

test('a key reused for a different thought never changes the answer', () => {
  thoughtPreview('First thought. It ends here.\n', 'shared');
  assert.equal(thoughtPreview('Another thought entirely! yes', 'shared'), fullThoughtPreview('Another thought entirely! yes'));
  assert.equal(thoughtPreview('First thought', 'shared'), 'First thought');
});

test('a long thought costs its new chunk, not its whole length', () => {
  const sentence = 'The floor seam between the legs is the only region where the two masks disagree, so I compare';
  let body = '';
  while (body.length < 98_304) body += `${sentence}\nnext line `;
  const text = `Opening words without a period\n${body}`;
  const steps = Array.from({ length: 24 }, (_, i) => text.slice(0, Math.round(((i + 1) * text.length) / 24)));
  for (const step of steps) assert.equal(thoughtPreview(step, 'long'), fullThoughtPreview(step));
  // Same answer, and the streamed path does not rescan: 24 more growing steps
  // on the same key must be far cheaper than 24 whole-text previews.
  const t0 = performance.now();
  for (let r = 0; r < 20; r++) for (const step of steps) fullThoughtPreview(step);
  const whole = performance.now() - t0;
  const t1 = performance.now();
  for (let r = 0; r < 20; r++) { for (const step of steps) thoughtPreview(step, `long-${r}`); }
  const streamed = performance.now() - t1;
  assert.ok(streamed < whole, `streamed ${streamed.toFixed(1)} ms, whole ${whole.toFixed(1)} ms`);
});
