import assert from 'node:assert/strict';
import test from 'node:test';
import { WorkspaceMoveGate, workspaceMoveAccepted, workspaceRebindSupported } from '../src/workspace-move.ts';

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => { resolve = done; });
  return { promise, resolve };
}

test('send boundary waits for the exact initialized-chat workspace move', async () => {
  const gate = new WorkspaceMoveGate();
  const daemon = deferred<boolean>();
  const move = gate.run('chat-a', () => daemon.promise);
  assert.equal(gate.current('chat-a'), move);

  let sendEntered = false;
  const send = (async () => {
    const pending = gate.current('chat-a');
    if (pending && !(await pending)) return false;
    sendEntered = true;
    return true;
  })();
  await Promise.resolve();
  assert.equal(sendEntered, false, 'send crossed the daemon-owned move boundary');

  daemon.resolve(true);
  assert.equal(await move, true);
  assert.equal(await send, true);
  assert.equal(sendEntered, true);
  assert.equal(gate.current('chat-a'), undefined);
});

test('duplicate drag for one chat shares one transaction and rejection gates send', async () => {
  const gate = new WorkspaceMoveGate();
  const daemon = deferred<boolean>();
  let operations = 0;
  const first = gate.run('chat-a', () => { operations += 1; return daemon.promise; });
  const duplicate = gate.run('chat-a', async () => { operations += 1; return true; });
  const unrelated = gate.run('chat-b', async () => true);
  assert.equal(first, duplicate);
  assert.equal(operations, 1);
  assert.equal(await unrelated, true, 'another chat was blocked by the move');
  daemon.resolve(false);
  assert.equal(await first, false);
  assert.equal(gate.current('chat-a'), undefined);
});

test('workspace move accepts only the complete transactional acknowledgment', () => {
  assert.equal(workspaceMoveAccepted({
    sessionId: '', workspaceCommitted: true, workspaceRebound: true, workspaceRevision: 1,
  }), true);
  for (const mixedVersion of [
    undefined,
    {},
    { sessionId: '', workspaceCommitted: true },
    { sessionId: '', workspaceRebound: true },
    { sessionId: 'still-live', workspaceCommitted: true, workspaceRebound: true, workspaceRevision: 1 },
    { sessionId: '', workspaceCommitted: true, workspaceRebound: true, workspaceRevision: 1, error: 'partial failure' },
    { sessionId: '', workspaceCommitted: true, workspaceRebound: true },
    { sessionId: '', workspaceCommitted: true, workspaceRebound: true, workspaceRevision: 2 },
  ]) {
    assert.equal(workspaceMoveAccepted(mixedVersion), false, JSON.stringify(mixedVersion));
  }
});

test('transactional move RPC is capability gated during mixed-version rollout', () => {
  assert.equal(workspaceRebindSupported(undefined), false);
  assert.equal(workspaceRebindSupported({}), false);
  assert.equal(workspaceRebindSupported({ workspaceRebindMode: 'future-v2' }), false);
  assert.equal(workspaceRebindSupported({ workspaceRebindMode: 'transactional-v1' }), true);

  let rpcCalls = 0;
  let visibleCwd = '/workspace/old';
  const legacyMeta = { daemon: true };
  if (workspaceRebindSupported(legacyMeta)) {
    rpcCalls += 1;
    visibleCwd = '/workspace/target';
  }
  assert.equal(rpcCalls, 0, 'renderer invoked an old daemon move surface');
  assert.equal(visibleCwd, '/workspace/old', 'mixed-version rejection moved the visible chat');
});
