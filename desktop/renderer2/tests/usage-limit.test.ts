import assert from 'node:assert/strict';
import { test } from 'node:test';
import { currentLimitedReply, limitLineState, limitWhenText } from '../src/chat/usage-limit.ts';

// Continuar under a reply a usage limit stopped (owner 2026-11-18): it
// belongs to the chat's latest reply only, and is gone once anything follows.
test('the line belongs to the latest reply a limit stopped, until anything follows it', () => {
  const limited = { id: 'a1', role: 'assistant' as const, usageLimit: { label: 'Claude Max' } };
  assert.equal(currentLimitedReply([{ id: 'u1', role: 'user' as const }, limited])?.id, 'a1');
  // A Workass wake-up notice is not the person writing.
  assert.equal(currentLimitedReply([limited, { id: 'n1', role: 'user' as const, notice: { kind: 'subagent' } as never }])?.id, 'a1');
  // "continue" itself, or anything else, removes it completely.
  assert.equal(currentLimitedReply([limited, { id: 'u2', role: 'user' as const }]), null);
  assert.equal(currentLimitedReply([limited, { id: 'a2', role: 'assistant' as const }]), null);
  assert.equal(currentLimitedReply([{ id: 'a3', role: 'assistant' as const }]), null);
});

test('the line says when the subscription is back, and what is scheduled', () => {
  const now = new Date(2026, 10, 18, 15, 0);
  const later = new Date(2026, 10, 18, 18, 40);
  assert.deepEqual(limitLineState(later.toISOString(), null, now), { kind: 'stopped', back: later });
  assert.deepEqual(limitLineState(new Date(2026, 10, 18, 14, 0).toISOString(), null, now), { kind: 'stopped', back: 'back' });
  assert.deepEqual(limitLineState(undefined, null, now), { kind: 'stopped', back: 'unknown' });
  assert.deepEqual(limitLineState(later.toISOString(), later.toISOString(), now), { kind: 'scheduled', at: later });
  assert.equal(limitWhenText(later, now), 'a las 18:40');
  assert.equal(limitWhenText(new Date(2026, 10, 19, 9, 5), now), 'mañana a las 09:05');
});
