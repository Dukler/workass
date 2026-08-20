import assert from 'node:assert/strict';
import test from 'node:test';
import { reconcileQueuedJobStart } from '../src/store/busy-start.ts';
import type { Chat, Msg } from '../src/store/types.ts';
import { tagId } from '../src/wire/machineIds.ts';

function chat(messages: Msg[]): Chat {
  return {
    id: 'busy-tab', chatId: 'busy-chat', sessionId: 'native-session', title: 'Busy', titleLocked: true,
    group: null, cwd: '/tmp', currentModelId: null, currentModeId: null, pending: false,
    messages, draft: '', unread: 0,
  } as Chat;
}

test('busy start receipt withdraws the optimistic pair and inserts one host FIFO row', () => {
  const target = chat([
    { id: 'prior', role: 'assistant', content: 'still running', status: 'running', at: null, events: [], jobId: 'active-job' },
    { id: 'follow-user', role: 'user', content: 'follow once', status: 'done', at: '2026-07-21T00:00:00Z', events: [] },
    { id: 'follow-assistant', role: 'assistant', content: '', status: 'running', at: null, events: [] },
  ]);
  target.agentQueueRevision = 0;
  target.queue = [{ id: 'local-first', text: 'already queued' }];
  const receipt = {
    queued: true as const, queueId: 'host-q', position: 2, delivery: 'queue' as const,
    queuedAt: '2026-07-21T00:00:01Z', agentQueueRevision: 1,
  };

  assert.deepEqual(reconcileQueuedJobStart(target, {
    userId: 'follow-user', assistantId: 'follow-assistant',
  }, 'follow once', undefined, receipt), { alreadyStarted: false });
  assert.deepEqual(target.messages.map((message) => message.id), ['prior']);
  assert.deepEqual(target.queue?.map((item) => [item.id, item.source]), [
    ['local-first', undefined], ['host-q', 'host'],
  ]);
  assert.equal(target.agentQueueRevision, 1);

  reconcileQueuedJobStart(target, {
    userId: 'follow-user', assistantId: 'follow-assistant',
  }, 'follow once', undefined, receipt);
  assert.equal(target.queue?.filter((item) => item.id === 'host-q').length, 1);
});

test('late busy receipt never removes a host turn that has already started', () => {
  const target = chat([
    { id: 'follow-user', role: 'user', content: 'follow once', status: 'done', at: '2026-07-21T00:00:00Z', events: [] },
    { id: 'follow-assistant', role: 'assistant', content: 'working', status: 'running', at: null, events: [], jobId: 'promoted-job' },
  ]);
  target.agentQueueRevision = 2;

  assert.deepEqual(reconcileQueuedJobStart(target, {
    userId: 'follow-user', assistantId: 'follow-assistant',
  }, 'follow once', undefined, {
    queued: true, queueId: 'host-q', position: 1, delivery: 'queue', agentQueueRevision: 1,
  }), { alreadyStarted: true });
  assert.deepEqual(target.messages.map((message) => message.id), ['follow-user', 'follow-assistant']);
  assert.equal(target.queue, undefined);
  assert.equal(target.agentQueueRevision, 2);
});

test('a promoted turn rejects its stale queue receipt even before a newer queue revision hydrates', () => {
  const target = chat([
    { id: 'follow-user', role: 'user', content: 'follow once', status: 'done', at: '2026-07-21T00:00:00Z', events: [] },
    { id: 'follow-assistant', role: 'assistant', content: 'working', status: 'running', at: null, events: [], jobId: 'promoted-job' },
  ]);
  target.agentQueueRevision = 0;

  assert.deepEqual(reconcileQueuedJobStart(target, {
    userId: 'follow-user', assistantId: 'follow-assistant',
  }, 'follow once', undefined, {
    queued: true, queueId: 'host-q', position: 1, delivery: 'queue', agentQueueRevision: 1,
  }), { alreadyStarted: true });
  assert.deepEqual(target.messages.map((message) => message.id), ['follow-user', 'follow-assistant']);
  assert.equal(target.queue, undefined);
  assert.equal(target.agentQueueRevision, 0);
});

test('a tagged remote busy receipt matches its hydrated queue row instead of duplicating it', () => {
  const machineId = 'm-lagpc';
  const userId = tagId(machineId, 'follow-user');
  const assistantId = tagId(machineId, 'follow-assistant');
  const queueId = tagId(machineId, 'host-q');
  const target = chat([
    { id: userId, role: 'user', content: 'follow once', status: 'done', at: '2026-07-21T00:00:00Z', events: [] },
    { id: assistantId, role: 'assistant', content: '', status: 'running', at: null, events: [] },
  ]);
  target.id = tagId(machineId, target.id);
  target.chatId = tagId(machineId, target.chatId!);
  target.queue = [{ id: queueId, text: 'follow once', source: 'host', delivery: 'queue' }];

  reconcileQueuedJobStart(target, { userId, assistantId }, 'follow once', undefined, {
    queued: true, queueId, position: 1, delivery: 'queue', agentQueueRevision: 1,
  });

  assert.deepEqual(target.queue?.map((item) => item.id), [queueId]);
  assert.deepEqual(target.messages, []);
});
