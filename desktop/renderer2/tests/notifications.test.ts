import assert from 'node:assert/strict';
import test from 'node:test';
import { isAutoTurnEndNotice } from '../src/notifications.ts';

test('drops the stale daemon per-turn notifications', () => {
  assert.equal(isAutoTurnEndNotice('Chat turn finished'), true);
  assert.equal(isAutoTurnEndNotice('Chat turn ended: failed'), true);
  assert.equal(isAutoTurnEndNotice('Chat turn ended: cancelled'), true);
  assert.equal(isAutoTurnEndNotice('  Chat turn finished  '), true);
});

test('keeps real notifications (explicit agent notify, permission alerts)', () => {
  assert.equal(isAutoTurnEndNotice('Permiso solicitado: escribir archivo'), false);
  assert.equal(isAutoTurnEndNotice('Build terminado — 0 errores'), false);
  assert.equal(isAutoTurnEndNotice('Turn finished the report'), false); // not the daemon's card
  assert.equal(isAutoTurnEndNotice(''), false);
  assert.equal(isAutoTurnEndNotice(undefined), false);
  assert.equal(isAutoTurnEndNotice(null), false);
});
