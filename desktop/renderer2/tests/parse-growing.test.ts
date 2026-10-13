import assert from 'node:assert/strict';
import { test } from 'node:test';
import { parseBlocks, parseBlocksGrowing } from '../src/markdown/blocks.ts';

// A streaming reply reparses only its open tail (parseBlocksGrowing). At every
// length it must equal a whole parse, and settled blocks must be the very same
// objects so their memoized rows never rerender (2026-10-13, after t3code).
const samples = [
  '## Gold vs Silver\n\nThe two labels agree everywhere except the seam.\n\nWhat I checked:\n\n- the raw floor\n- the Silver mask\n  - nested detail\n- the Gold mask\n\n1. first\n2. second\n\n```python\ndiff = a ^ b\n\nprint(diff)\n```\n\n| frame | gold | silver |\n|---|---|---|\n| 3812 | filled | cleared |\n\n> quoted line\n> another\n\n---\n\nSo the only frame to fix is **3812**.',
  'One paragraph\nwrapped on two lines.\n\n~~~\nfence with tilde\n\n```\nstill inside\n~~~\nafter',
  '- a\n\n- b\n\n* c\n\nText right after\n```\nunclosed fence\n\nwith a blank line',
  '',
];

test('a growing reply parses to exactly what a whole parse gives, at every length', () => {
  samples.forEach((sample, n) => {
    for (let length = 0; length <= sample.length; length++) {
      const text = sample.slice(0, length);
      const growing = parseBlocksGrowing(`sample-${n}`, text).map((block) => block.sig);
      assert.deepEqual(growing, parseBlocks(text).map((block) => block.sig), `sample ${n} at length ${length}`);
    }
  });
});

test('settled blocks come back as the same objects while the reply grows', () => {
  const first = parseBlocksGrowing('reuse', 'Done paragraph.\n\nOpen tail');
  const second = parseBlocksGrowing('reuse', 'Done paragraph.\n\nOpen tail grows');
  assert.equal(second[0], first[0], 'the settled paragraph is reused, not reparsed');
  assert.notEqual(second[1].sig, first[1].sig);
  // A text that is not an extension (a segment split) starts over correctly.
  assert.deepEqual(parseBlocksGrowing('reuse', 'Other text').map((block) => block.sig), parseBlocks('Other text').map((block) => block.sig));
});
