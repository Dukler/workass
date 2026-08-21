import assert from 'node:assert/strict';
import test from 'node:test';
import { projectSteeringPresentation } from '../src/chat/steering-presentation.ts';
import type { Msg } from '../src/store/types.ts';

function user(id: string, content: string, extra: Partial<Msg> = {}): Msg {
  return { id, role: 'user', content, status: 'done', at: null, events: [], ...extra };
}

function assistant(id: string, content: string, extra: Partial<Msg> = {}): Msg {
  return { id, role: 'assistant', content, status: 'done', at: null, events: [], ...extra };
}

test('an applied live steer leaves the Steering count and owns its canonical transcript row immediately', () => {
  const rows = [
    user('prompt', 'start'),
    assistant('head', 'answer before direction', { turnRootId: 'head', turnTerminal: false }),
    user('steer', 'change direction', { turnRootId: 'head', steerState: 'applied' }),
    assistant('tail', 'answer after direction', { turnRootId: 'head', turnTerminal: true, status: 'running' }),
  ];

  const projection = projectSteeringPresentation(rows);
  assert.deepEqual(projection.transcriptMessages.map((row) => row.id), ['prompt', 'head', 'steer', 'tail']);
  assert.deepEqual(projection.steeringTrayMessages, []);
  assert.equal(rows[2].id, 'steer', 'presentation never mutates canonical chronology');
});

test('a completed turn preserves the same applied steer identity and canonical order', () => {
  const rows = [
    user('prompt', 'start'),
    assistant('head', 'answer before direction', { turnRootId: 'head', turnTerminal: false }),
    user('steer', 'change direction', { turnRootId: 'head', steerState: 'applied' }),
    assistant('tail', 'answer after direction', { turnRootId: 'head', turnTerminal: true }),
    user('next', 'next ordinary turn'),
  ];

  const projection = projectSteeringPresentation(rows);
  assert.deepEqual(projection.transcriptMessages.map((row) => row.id), ['prompt', 'head', 'steer', 'tail', 'next']);
  assert.deepEqual(projection.steeringTrayMessages, []);
  assert.deepEqual(rows.map((row) => row.id), ['prompt', 'head', 'steer', 'tail', 'next']);
});

test('rapid applied steers retain canonical submission order without entering Steering', () => {
  const rows = [
    assistant('head', 'before', { turnRootId: 'head', turnTerminal: false }),
    user('steer-1', 'first', { turnRootId: 'head', steerState: 'applied' }),
    user('steer-2', 'second', { turnRootId: 'head', steerState: 'applied' }),
    assistant('tail', 'after both', { turnRootId: 'head', turnTerminal: true }),
  ];

  assert.deepEqual(
    projectSteeringPresentation(rows).transcriptMessages.map((row) => row.id),
    ['head', 'steer-1', 'steer-2', 'tail'],
  );
  assert.deepEqual(projectSteeringPresentation(rows).steeringTrayMessages, []);
});

test('a staged native steer keeps its waiting continuation out of the transcript', () => {
  const rows = [
    assistant('head', 'current sentence', { status: 'running' }),
    user('steer', 'change direction', {
      status: 'pending', steerState: 'sending', turnRootId: 'head',
      steerBoundary: 'waiting', steerContinuationId: 'tail',
    }),
    assistant('tail', '', {
      status: 'pending', turnRootId: 'head', turnTerminal: true,
      steerBoundary: 'waiting', steerContinuationFor: 'steer',
    }),
  ];

  const projection = projectSteeringPresentation(rows);
  assert.deepEqual(projection.transcriptMessages.map((row) => row.id), ['head']);
  assert.deepEqual(projection.steeringTrayMessages.map((row) => row.id), ['steer']);
});

test('acknowledgement moves a staged steer into transcript while its continuation stays hidden until consumption', () => {
  const rows = [
    assistant('head', 'current sentence', { status: 'running' }),
    user('steer', 'change direction', {
      steerState: 'accepted', turnRootId: 'head',
      steerBoundary: 'waiting', steerContinuationId: 'tail',
    }),
    assistant('tail', '', {
      status: 'pending', turnRootId: 'head', turnTerminal: true,
      steerBoundary: 'waiting', steerContinuationFor: 'steer',
    }),
  ];

  const projection = projectSteeringPresentation(rows);
  assert.deepEqual(projection.transcriptMessages.map((row) => row.id), ['head', 'steer']);
  assert.deepEqual(projection.steeringTrayMessages, []);
});

test('rapid steering shows an accepted first row in transcript and only the unresolved second row in Steering', () => {
  const rows = [
    assistant('head', 'current sentence', { status: 'running' }),
    user('steer-1', 'first direction', {
      steerState: 'accepted', turnRootId: 'head', steerBoundary: 'waiting', steerContinuationId: 'tail-1',
    }),
    assistant('tail-1', '', {
      status: 'pending', turnRootId: 'head', steerBoundary: 'waiting', steerContinuationFor: 'steer-1',
    }),
    user('steer-2', 'second direction', {
      status: 'pending', steerState: 'sending', turnRootId: 'head', steerBoundary: 'waiting', steerContinuationId: 'tail-2',
    }),
    assistant('tail-2', '', {
      status: 'pending', turnRootId: 'head', steerBoundary: 'waiting', steerContinuationFor: 'steer-2',
    }),
  ];

  const projection = projectSteeringPresentation(rows);
  assert.deepEqual(projection.transcriptMessages.map((row) => row.id), ['head', 'steer-1']);
  assert.deepEqual(projection.steeringTrayMessages.map((row) => row.id), ['steer-2']);
});

test('a terminal boundary settles an unresolved steer out of the live tray without replay', () => {
  const rows = [
    assistant('head', 'finished'),
    user('steer', 'unconfirmed direction', { steerState: 'uncertain', turnRootId: 'head' }),
  ];

  const projection = projectSteeringPresentation(rows);
  assert.deepEqual(projection.transcriptMessages.map((row) => row.id), ['head', 'steer']);
  assert.deepEqual(projection.steeringTrayMessages, []);
});

test('ordinary messages without steer ownership retain their exact order', () => {
  const rows = [user('u-1', 'one'), assistant('a-1', 'answer'), user('u-2', 'two')];
  const projection = projectSteeringPresentation(rows);
  assert.deepEqual(projection.transcriptMessages, rows);
  assert.deepEqual(projection.steeringTrayMessages, []);
});
