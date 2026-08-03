import assert from 'node:assert/strict';
import test from 'node:test';

import { settleTerminalToolEvents } from '../src/terminal-tool-reconciliation.ts';
import type { TimelineEvent, ToolEvent } from '../src/store/types.ts';

const tool = (status: string): ToolEvent => ({
  key: `tool-${status}`,
  at: 0,
  kind: 'tool',
  id: `tool-${status}`,
  toolKind: 'other',
  title: 'View image',
  status,
  command: null,
  terminalId: null,
  input: null,
  output: null,
  location: null,
  startedAt: 10,
});

test('terminal turn reconciliation cannot leave foreground tools running', () => {
  for (const [turnStatus, want] of [
    ['done', 'completed'],
    ['failed', 'failed'],
    ['cancelled', 'cancelled'],
  ] as const) {
    const events: TimelineEvent[] = [
      tool('in_progress'),
      tool('pending'),
      tool('running'),
      tool('completed'),
      { key: 'plan', at: 0, kind: 'plan', entries: [{ status: 'in_progress', content: 'Unrelated plan' }] },
    ];
    assert.equal(settleTerminalToolEvents(events, turnStatus, 25), 3);
    assert.deepEqual(events.slice(0, 4).map((event) => (event as ToolEvent).status), [want, want, want, 'completed']);
    assert.deepEqual(events.slice(0, 3).map((event) => (event as ToolEvent).endedAt), [25, 25, 25]);
    assert.equal(events[4]?.kind, 'plan');
  }
});
