import assert from 'node:assert/strict';
import test from 'node:test';
import { chatHasLiveActivity, isServiceWork } from '../src/chat-activity.ts';

test('sidebar activity stays live for foreground turns or running spawned work', () => {
  assert.equal(chatHasLiveActivity({ messages: [] }, []), false);
  assert.equal(chatHasLiveActivity({ messages: [{ status: 'running' }] as never[] }, []), true);
  assert.equal(chatHasLiveActivity({ messages: [] }, [{ status: 'running' }]), true);
});

test('terminal spawned work does not keep the sidebar activity dot live', () => {
  assert.equal(chatHasLiveActivity({ messages: [] }, [{ status: 'exited' }, { status: 'failed' }]), false);
});

test('a running service is alive without the chat being busy', () => {
  assert.equal(isServiceWork({ role: 'service' }), true);
  assert.equal(isServiceWork({}), false);
  // The bug: `expo start` alone left its chat reporting work forever.
  assert.equal(chatHasLiveActivity({ messages: [] }, [{ status: 'running', role: 'service' }]), false);
  // A service never masks real work running beside it.
  assert.equal(
    chatHasLiveActivity({ messages: [] }, [{ status: 'running', role: 'service' }, { status: 'running' }]),
    true,
  );
  // Nor does it mask the foreground turn.
  assert.equal(
    chatHasLiveActivity({ messages: [{ status: 'running' }] as never[] }, [{ status: 'running', role: 'service' }]),
    true,
  );
});
