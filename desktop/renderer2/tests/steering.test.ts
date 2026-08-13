import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { withoutDraftImages } from '../src/image-drafts.ts';
import {
  acceptPendingSteer,
  commitChronologicalSteer,
  hasSteerConsumptionReceipt,
  beginPendingSteer,
  insertChronologicalSteer,
  markPendingSteerUncertain,
  settleStagedSteersAtTurnEnd,
  settlePendingSteer,
  settleSendingSteersAtTurnEnd,
  stageChronologicalSteer,
  SteeringDispatchLane,
  steeringBehavior,
  steeringDestination,
  steeringStagesBoundary,
  steerStatusLabel,
} from '../src/steering.ts';

test('both frontier providers steer the live turn and stage their own boundary', () => {
  assert.equal(steeringBehavior('codex'), 'codex-live');
  assert.equal(steeringBehavior('codex-acp'), 'codex-live');
  // The packaged Claude host answers _workass/claude/steer from the Agent SDK's
  // live streaming input and echoes a consumption receipt, so a Claude steer
  // belongs in the transcript exactly like Codex — never bounced through FIFO.
  assert.equal(steeringBehavior('claude'), 'claude-live');
  assert.equal(steeringBehavior('claude-agent-acp'), 'claude-live');
  assert.equal(steeringStagesBoundary(steeringBehavior('codex')), true);
  assert.equal(steeringStagesBoundary(steeringBehavior('claude')), true);
});

test('generic ACP providers remain capability driven and split immediately', () => {
  assert.equal(steeringBehavior('mock'), 'capability');
  assert.equal(steeringBehavior('qwen'), 'capability');
  assert.equal(steeringBehavior('custom'), 'capability');
  assert.equal(steeringBehavior(null), 'capability');
  assert.equal(steeringStagesBoundary('capability'), false);
});

test('a live Claude acknowledgement owns the transcript row and never enters FIFO', () => {
  assert.equal(steeringDestination({ ok: true, strategy: 'claude-live' }), 'transcript');
  assert.equal(hasSteerConsumptionReceipt({ ok: true, strategy: 'claude-live', receipt: true }), true);
  // Only the adapter's own rejection may move a submitted steer into the queue.
  assert.equal(steeringDestination({ ok: false, strategy: 'claude-live' }), 'queue');
  assert.equal(steeringDestination({ ok: false, strategy: 'interrupt-queue' }), 'queue');
});

test('the provider-neutral actor receipt is a real consumption boundary', () => {
  assert.equal(hasSteerConsumptionReceipt({ ok: true, strategy: 'receipt-live', receipt: true }), true);
});

test('steer acknowledgement selects one visible owner without optimistic bouncing', () => {
  assert.equal(steeringDestination({ ok: true, strategy: 'codex-live' }), 'transcript');
  assert.equal(steeringDestination({ ok: true, strategy: 'capability-live' }), 'transcript');
  assert.equal(steeringDestination({ ok: false, strategy: 'queue' }), 'queue');
  assert.equal(steeringDestination({ ok: false, strategy: 'interrupt-queue' }), 'queue');
  assert.equal(steeringDestination({ ok: false, strategy: 'uncertain' }), 'transcript');
  assert.equal(steeringDestination(undefined), 'queue');
});

test('steer delivery feedback keeps one stable owner through acknowledgement and receipt', () => {
  const messages = [{ id: 'assistant-running', status: 'running' }];
  const pending = { id: 'steer-user', status: 'done' };
  const started = beginPendingSteer(messages, pending);
  assert.equal(started, pending);
  assert.equal(messages.at(-1), pending);
  assert.equal(pending.status, 'pending');
  assert.equal(pending.steerState, 'sending');

  assert.equal(hasSteerConsumptionReceipt({ ok: true, strategy: 'codex-live', receipt: true }), true);
  assert.equal(acceptPendingSteer(messages, pending.id), pending);
  assert.equal(pending.status, 'done', 'official turn-id acknowledgement is the delivery boundary');
  assert.equal(pending.steerState, 'accepted');

  const settled = settlePendingSteer(messages, pending.id, 'transcript');
  assert.equal(settled, pending, 'consumption settles the same object');
  assert.equal(messages.at(-1), pending, 'the transcript bubble keeps its position');
  assert.equal(pending.status, 'done');
  assert.equal(pending.steerState, 'applied');
});

test('receipt can beat acknowledgement without regressing applied feedback', () => {
  const pending = { id: 'steer-user', status: 'done' };
  const messages = [pending];
  beginPendingSteer(messages.slice(0, 0), pending);
  const owned = [pending];
  assert.equal(settlePendingSteer(owned, pending.id, 'transcript'), pending);
  assert.equal(pending.steerState, 'applied');
  assert.equal(acceptPendingSteer(owned, pending.id), undefined);
  assert.equal(pending.steerState, 'applied');
});

test('transport-uncertain delivery settles in place and can be upgraded by a late receipt', () => {
  assert.equal(hasSteerConsumptionReceipt({ ok: false, strategy: 'uncertain', receipt: true }), true);
  assert.equal(hasSteerConsumptionReceipt({ ok: true, strategy: 'codex-live' }), false, 'older adapters retain acknowledgement behavior');
  const pending = { id: 'steer-user', status: 'pending', steerState: 'sending' as const };
  const messages = [pending];
  assert.equal(markPendingSteerUncertain(messages, pending.id), pending);
  assert.equal(pending.status, 'done');
  assert.equal(pending.steerState, 'uncertain');
  assert.equal(settlePendingSteer(messages, pending.id, 'transcript'), pending);
  assert.equal(pending.steerState, 'applied');
});

test('a rejected live steer transfers its pending owner out before FIFO creation', () => {
  const pending = { id: 'steer-user', status: 'pending' };
  const messages = [{ id: 'assistant-running', status: 'running' }, pending];
  assert.equal(settlePendingSteer(messages, pending.id, 'queue'), pending);
  assert.deepEqual(messages, [{ id: 'assistant-running', status: 'running' }]);
});

test('a late steer rejection cannot steal a bubble already promoted to a normal turn', () => {
  const promoted = { id: 'steer-user', status: 'done' };
  const messages = [{ id: 'assistant-finished', status: 'done' }, promoted, { id: 'assistant-next', status: 'running' }];
  assert.equal(settlePendingSteer(messages, promoted.id, 'queue'), undefined);
  assert.equal(messages[1], promoted);
});

test('a late rejection can transfer a transport-uncertain owner exactly once', () => {
  const uncertain = { id: 'steer-user', status: 'done', steerState: 'uncertain' as const };
  const messages = [{ id: 'assistant-finished', status: 'done' }, uncertain];
  assert.equal(settlePendingSteer(messages, uncertain.id, 'queue'), uncertain);
  assert.deepEqual(messages, [{ id: 'assistant-finished', status: 'done' }]);
  assert.equal(settlePendingSteer(messages, uncertain.id, 'queue'), undefined);
});

test('turn end resolves only unacknowledged spinners and never replays accepted input', () => {
  const sending = { id: 'sending', status: 'pending', steerState: 'sending' as const };
  const accepted = { id: 'accepted', status: 'done', steerState: 'accepted' as const };
  const applied = { id: 'applied', status: 'done', steerState: 'applied' as const };
  const ordinary = { id: 'ordinary', status: 'done' };
  const messages = [sending, accepted, applied, ordinary];
  assert.deepEqual(settleSendingSteersAtTurnEnd(messages), [sending]);
  assert.equal(sending.status, 'done');
  assert.equal(sending.steerState, 'uncertain');
  assert.equal(accepted.steerState, 'accepted');
  assert.equal(applied.steerState, 'applied');
  assert.equal(ordinary.status, 'done');
  assert.deepEqual(settleSendingSteersAtTurnEnd(messages), [], 'terminal reconciliation is idempotent');
});

test('multiple rapid steers settle independently in submission order', () => {
  const messages: Array<{ id: string; status: string; steerState?: 'sending' | 'accepted' | 'applied' | 'uncertain' }> = [];
  const first = beginPendingSteer(messages, { id: 'first', status: 'done' });
  const second = beginPendingSteer(messages, { id: 'second', status: 'done' });
  assert.deepEqual(messages.map((message) => message.id), ['first', 'second']);
  assert.equal(acceptPendingSteer(messages, second.id), second);
  assert.equal(settlePendingSteer(messages, first.id, 'transcript'), first);
  assert.deepEqual(messages.map((message) => [message.id, message.steerState]), [
    ['first', 'applied'],
    ['second', 'accepted'],
  ]);
});

test('rapid native steer dispatch is FIFO per chat and independent across chats', async () => {
  const lane = new SteeringDispatchLane();
  const events: string[] = [];
  let releaseFirst!: () => void;
  const firstGate = new Promise<void>((resolve) => { releaseFirst = resolve; });

  const first = lane.run('chat-a', async () => {
    events.push('a1:start');
    await firstGate;
    events.push('a1:end');
    return 1;
  });
  const second = lane.run('chat-a', async () => {
    events.push('a2:start');
    return 2;
  });
  const other = lane.run('chat-b', async () => {
    events.push('b1:start');
    return 3;
  });

  await Promise.resolve();
  await Promise.resolve();
  assert.deepEqual(events, ['a1:start', 'b1:start']);
  releaseFirst();
  assert.deepEqual(await Promise.all([first, second, other]), [1, 2, 3]);
  assert.deepEqual(events, ['a1:start', 'b1:start', 'a1:end', 'a2:start']);
});

test('a failed steer does not poison later dispatch in the same chat', async () => {
  const lane = new SteeringDispatchLane();
  const first = lane.run('chat-a', async () => { throw new Error('explicit rejection'); });
  const second = lane.run('chat-a', async () => 'next');
  await assert.rejects(first, /explicit rejection/);
  assert.equal(await second, 'next');
});

test('Codex waits for the semantic receipt boundary instead of splitting streamed prose', () => {
  const assistant = {
    id: 'assistant-running', role: 'assistant' as const, content: 'before', result: undefined,
    status: 'running', at: null, events: [{ key: 'before-tool', at: 6 }], jobId: 'job-1',
  };
  const steer = {
    id: 'steer-user', role: 'user' as const, content: 'change direction', status: 'done', at: '2026-07-14T10:00:01Z', events: [],
  };
  const continuation = {
    id: 'assistant-after-steer', role: 'assistant' as const, content: '', result: undefined,
    status: 'running', at: null, events: [], jobId: 'job-1',
  };
  const messages = [assistant];

  assert.ok(stageChronologicalSteer(messages, steer, continuation));
  assert.deepEqual(messages.map((message) => message.id), ['assistant-running', 'steer-user', 'assistant-after-steer']);
  assert.equal(assistant.turnTerminal, undefined);
  assert.equal(assistant.status, 'running');
  assert.equal(steer.steerState, 'sending');
  assert.equal(steer.steerBoundary, 'waiting');
  assert.equal(continuation.steerBoundary, 'waiting');
  assert.equal(continuation.status, 'pending');
  assert.equal(continuation.turnRootId, assistant.id);
  assert.equal(continuation.turnTerminal, true);

  assistant.content += ' and finish this sentence.';
  assistant.events.push({ key: 'semantic-step-finished', at: assistant.content.length });
  assert.deepEqual(messages.filter((message) => message.steerBoundary !== 'waiting').map((message) => message.id), [
    'assistant-running',
  ], 'the submitted direction is a pending preview until Codex commits userMessage.clientId');

  assert.ok(commitChronologicalSteer(messages, steer.id));
  assert.deepEqual(messages.map((message) => message.id), ['assistant-running', 'steer-user', 'assistant-after-steer']);
  assert.equal(assistant.content, 'before and finish this sentence.');
  assert.equal(assistant.status, 'done');
  assert.equal(assistant.turnTerminal, false);
  assert.equal(steer.steerBoundary, undefined);
  assert.equal(continuation.steerBoundary, undefined);
  assert.equal(continuation.status, 'running');

  continuation.content = 'after';
  continuation.result = 'final answer';
  continuation.status = 'done';
  assert.deepEqual(messages.map((message) => [message.role, message.content, message.result ?? '']), [
    ['assistant', 'before and finish this sentence.', ''],
    ['user', 'change direction', ''],
    ['assistant', 'after', 'final answer'],
  ], 'all output after the semantic receipt boundary belongs below the committed user row');
});

test('rapid Codex receipts commit staged steers in FIFO order without empty assistant rows', () => {
  const root = { id: 'assistant-root', role: 'assistant' as const, content: 'before', status: 'running', at: null, events: [], jobId: 'job-1' };
  const first = { id: 'first', role: 'user' as const, content: 'first', status: 'done', at: '1', events: [] };
  const tail1 = { id: 'tail-1', role: 'assistant' as const, content: '', status: 'running', at: null, events: [], jobId: 'job-1' };
  const second = { id: 'second', role: 'user' as const, content: 'second', status: 'done', at: '2', events: [] };
  const tail2 = { id: 'tail-2', role: 'assistant' as const, content: '', status: 'running', at: null, events: [], jobId: 'job-1' };
  const messages = [root];

  stageChronologicalSteer(messages, first, tail1);
  stageChronologicalSteer(messages, second, tail2);
  assert.deepEqual(messages.filter((message) => message.steerBoundary !== 'waiting').map((message) => message.id), ['assistant-root']);

  commitChronologicalSteer(messages, first.id);
  commitChronologicalSteer(messages, second.id);
  assert.deepEqual(messages.map((message) => message.id), ['assistant-root', 'first', 'second', 'tail-2']);
  tail2.result = 'answer after both';
  assert.equal(messages.at(-1)?.result, 'answer after both');
});

test('turn end reveals an acknowledged staged steer after the completed assistant without inventing a tail', () => {
  const root = { id: 'assistant-root', role: 'assistant' as const, content: 'complete thought', status: 'running', at: null, events: [], jobId: 'job-1' };
  const steer = { id: 'steer', role: 'user' as const, content: 'late direction', status: 'done', at: '1', events: [], steerState: 'accepted' as const };
  const tail = { id: 'tail', role: 'assistant' as const, content: '', status: 'running', at: null, events: [], jobId: 'job-1' };
  const messages = [root];
  stageChronologicalSteer(messages, steer, tail);
  acceptPendingSteer(messages, steer.id);
  root.status = 'done';

  assert.deepEqual(settleStagedSteersAtTurnEnd(messages), [steer]);
  assert.deepEqual(messages.map((message) => message.id), ['assistant-root', 'steer']);
  assert.equal(steer.steerBoundary, undefined);
  assert.equal(steer.steerState, 'accepted');
});

test('generic capability steering retains immediate chronological insertion', () => {
  const root = { id: 'assistant-root', role: 'assistant' as const, content: 'partial', status: 'running', at: null, events: [], jobId: 'job-1' };
  const steer = { id: 'steer', role: 'user' as const, content: 'redirect', status: 'done', at: '1', events: [] };
  const tail = { id: 'tail', role: 'assistant' as const, content: '', status: 'running', at: null, events: [], jobId: 'job-1' };
  const messages = [root];
  assert.ok(insertChronologicalSteer(messages, steer, tail));
  assert.deepEqual(messages.map((message) => message.id), ['assistant-root', 'steer', 'tail']);
  assert.equal(steer.steerBoundary, undefined);
});

test('acknowledged steering stays pending until Codex consumes it', () => {
  assert.equal(steerStatusLabel('sending'), 'Steering…');
  assert.equal(steerStatusLabel('accepted'), 'Steered');
  assert.equal(steerStatusLabel('accepted', true), 'Steering…');
  assert.equal(steerStatusLabel('applied'), 'Steered');
  assert.equal(steerStatusLabel('uncertain'), 'Steer unconfirmed');

  const css = readFileSync(new URL('../src/styles/app.css', import.meta.url), 'utf8');
  const accepted = css.match(/\.steerstate\.accepted\s*\{([^}]*)\}/)?.[1] ?? '';
  const applied = css.match(/\.steerstate\.applied\s*\{([^}]*)\}/)?.[1] ?? '';
  assert.match(accepted, /color:\s*var\(--muted\)/);
  assert.match(applied, /color:\s*var\(--muted\)/);
  assert.doesNotMatch(`${accepted}\n${applied}`, /var\(--acc\)|#53b982/);
});

test('a staged steer keeps one attachment owner across draft, preview, and transcript', () => {
  const draft = { id: 'draft-image', name: 'steer.png', mimeType: 'image/png', data: 'c3RlZXI=', url: 'data:image/png;base64,c3RlZXI=' };
  const attached = { mimeType: draft.mimeType, data: draft.data, name: draft.name };
  const root = { id: 'assistant-root', role: 'assistant' as const, content: 'before', status: 'running', at: null, events: [], images: undefined };
  const steer = { id: 'steer', role: 'user' as const, content: 'use this', status: 'done', at: '1', events: [], images: [attached] };
  const tail = { id: 'tail', role: 'assistant' as const, content: '', status: 'running', at: null, events: [], images: undefined };
  const messages = [root];

  assert.ok(stageChronologicalSteer(messages, steer, tail));
  assert.deepEqual(steer.images, [attached], 'the waiting preview owns the encoded attachment');
  assert.deepEqual(withoutDraftImages([draft], [draft.id]), [], 'the transferred composer preview is hidden, not duplicated');
  assert.ok(commitChronologicalSteer(messages, steer.id));
  assert.deepEqual(messages.find((message) => message.id === steer.id)?.images, [attached], 'the same attachment survives transcript commit');
});
