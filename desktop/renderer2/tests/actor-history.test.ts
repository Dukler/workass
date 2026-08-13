import assert from 'node:assert/strict';
import test from 'node:test';
import { actorMessages } from '../src/chat/history.ts';
import type { MirrorMsg } from '../src/store/persistence.ts';

function row(id: string, role: 'user' | 'assistant', content: string): MirrorMsg {
  return { id, role, content, status: 'done', at: null, events: [] };
}

test('actor history preserves canonical rows and order without merging', () => {
  const projected = actorMessages([
    row('u-1', 'user', 'first'),
    row('a-1', 'assistant', 'answer'),
    row('u-2', 'user', 'second'),
  ]);
  assert.deepEqual(projected.map((message) => [message.id, message.content]), [
    ['u-1', 'first'], ['a-1', 'answer'], ['u-2', 'second'],
  ]);
});

test('actor history rejects rows without stable identity', () => {
  assert.throws(() => actorMessages([row('', 'assistant', 'invalid')]), /stable message id/);
});

test('an in-flight steer from a replaced renderer becomes uncertain without replay', () => {
  const pending = row('steer-1', 'user', 'redirect');
  pending.status = 'pending';
  pending.steerState = 'sending';
  const [restored] = actorMessages([pending]);
  assert.equal(restored.status, 'done');
  assert.equal(restored.steerState, 'uncertain');
});
