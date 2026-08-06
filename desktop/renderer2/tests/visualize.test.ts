import assert from 'node:assert/strict';
import test from 'node:test';
import { parseBlocks } from '../src/markdown/blocks.ts';
import { parseVisualizeReference } from '../src/visualize.ts';

const token = (value: string) => `visualize${value}`;

test('visualize references become standalone blocks with validated metadata', () => {
  const parsed = parseVisualizeReference(token('{"path":"/tmp/visual.html","mode":"wide","title":"Signals"}'));
  assert.deepEqual(parsed, { spec: { path: '/tmp/visual.html', mode: 'wide', title: 'Signals' } });

  const blocks = parseBlocks(`before\n\n${token('{"path":"/tmp/visual.html"}')}\n\nafter`);
  assert.deepEqual(blocks.map(({ block }) => block.kind), ['p', 'visualize', 'p']);
  assert.equal(blocks[1].block.kind, 'visualize');
  if (blocks[1].block.kind === 'visualize') assert.equal(blocks[1].block.spec?.path, '/tmp/visual.html');

  assert.deepEqual(parseBlocks(`structured record.\n${token('{"path":"/tmp/visual.html"}')}`).map(({ block }) => block.kind), ['p', 'visualize']);
});

test('invalid visualize metadata becomes a bounded failure block', () => {
  const result = parseVisualizeReference(token('{"path":"/tmp/visual.html","mode":"full"}'));
  assert.equal(result?.spec, undefined);
  assert.match(result?.error ?? '', /mode/);
  const block = parseBlocks(token('{"path":"/tmp/visual.html","mode":"full"}'))[0].block;
  assert.equal(block.kind, 'visualize');
  if (block.kind === 'visualize') assert.match(block.error ?? '', /mode/);
});
