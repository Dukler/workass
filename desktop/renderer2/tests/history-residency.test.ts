import assert from 'node:assert/strict';
import test from 'node:test';
import {
  CHAT_MESSAGE_TAIL,
  preserveUnchangedFullHistories,
  releaseInactiveHistories,
} from '../src/chat/residency.ts';
import type { Chat, Msg } from '../src/store/types.ts';

function message(index: number, status: Msg['status'] = 'done'): Msg {
  return { id: `message-${index}`, role: 'assistant', content: `row ${index}`, status, at: null, events: [] };
}

function chat(id: string, count: number, revision = 1): Chat {
  return {
    id, chatId: `chat-${id}`, actorRevision: revision, title: id, titleLocked: false,
    messages: Array.from({ length: count }, (_, index) => message(index)),
    messageCount: count, historyComplete: true, draft: '', unread: false,
  } as Chat;
}

test('inactive history residency keeps only the bounded actor tail', () => {
  const active = chat('active', 90);
  const inactive = chat('inactive', 90);
  assert.deepEqual(releaseInactiveHistories([active, inactive], active.id), [inactive.id]);
  assert.equal(active.messages.length, 90);
  assert.equal(inactive.messages.length, CHAT_MESSAGE_TAIL);
  assert.equal(inactive.messages[0].id, 'message-30');
  assert.equal(inactive.messageCount, 90);
  assert.equal(inactive.historyComplete, false);
});

test('an unchanged actor revision preserves the already-loaded full history', () => {
  const previous = chat('chat', 90, 7);
  const restored = chat('chat', CHAT_MESSAGE_TAIL, 7);
  restored.messageCount = 90;
  restored.historyComplete = false;
  assert.deepEqual(preserveUnchangedFullHistories([previous], [restored]), [restored.id]);
  assert.equal(restored.messages, previous.messages);
  assert.equal(restored.historyComplete, true);
});

test('a newer actor revision merges its authoritative tail into the loaded history', () => {
  const previous = chat('chat', 90, 7);
  const restored = chat('chat', 60, 8);
  restored.messages = [
    ...previous.messages.slice(31).map((row) => ({ ...row })),
    message(90, 'running'),
  ];
  restored.messages.at(-2)!.content = 'updated by the actor';
  restored.messageCount = 91;
  restored.historyComplete = false;

  assert.deepEqual(preserveUnchangedFullHistories([previous], [restored]), [restored.id]);
  assert.equal(restored.messages.length, 91);
  assert.equal(restored.messages[89].content, 'updated by the actor');
  assert.equal(restored.messages[90].status, 'running');
  assert.equal(restored.historyComplete, true);
});

test('an unrelated partial tail is reloaded instead of being guessed into history', () => {
  const previous = chat('chat', 90, 7);
  const restored = chat('chat', 60, 8);
  restored.messages = Array.from({ length: 60 }, (_, index) => ({
    ...message(index + 100), id: `replacement-${index}`,
  }));
  restored.messageCount = 160;
  restored.historyComplete = false;

  assert.deepEqual(preserveUnchangedFullHistories([previous], [restored]), []);
  assert.equal(restored.historyComplete, false);
});
