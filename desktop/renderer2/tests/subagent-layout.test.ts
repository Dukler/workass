import assert from 'node:assert/strict';
import test from 'node:test';
import type { ToolEvent } from '../src/store/types.ts';
import { extractSubagents, summarizeSettledSubagents, isSubagentHeader, isSubagentChild, subagentActivity } from '../src/subagent-layout.ts';

function tool(partial: Partial<ToolEvent> & Pick<ToolEvent, 'key' | 'title'>): ToolEvent {
  return {
    kind: 'tool', toolKind: 'execute', id: null, status: 'completed',
    command: null, location: null, input: null, output: null, terminalId: null,
    ...partial,
  };
}

test('groups Workass and native Task children while preserving main-thread calls', () => {
  const events = [
    tool({ key: 'main', id: 'main-1', title: 'Main call' }),
    tool({ key: 'wa-head', id: 'wa-1', title: 'Sol audit', status: 'in_progress', subagentId: 'wa-1', subagentHeader: true, subagentProvider: 'gpt' }),
    tool({ key: 'wa-leaf', id: 'wa-1:leaf', title: 'Read Go', subagentId: 'wa-1', subagentLabel: 'Sol audit', subagentProvider: 'gpt' }),
    tool({ key: 'task-head', id: 'task-1', title: 'Claude audit', status: 'in_progress' }),
    tool({ key: 'task-leaf', id: 'leaf-2', title: 'Read TS', subagentId: 'task-1', subagentLabel: 'Claude audit', subagentProvider: 'claude' }),
  ];
  const grouped = extractSubagents(events);
  assert.equal(grouped.hasSubagents, true);
  assert.deepEqual(grouped.mainTools.map((event) => event.key), ['main']);
  assert.deepEqual(grouped.nodes.map((node) => ({ id: node.id, label: node.label, provider: node.provider, calls: node.calls.length })), [
    { id: 'wa-1', label: 'Sol audit', provider: 'gpt', calls: 1 },
    { id: 'task-1', label: 'Claude audit', provider: 'claude', calls: 1 },
  ]);
});

test('the model+effort combo rides the node from header or child (Turnos chip)', () => {
  const fromHeader = extractSubagents([
    tool({ key: 'h', id: 'wa-1', title: 'Sol audit', subagentId: 'wa-1', subagentHeader: true, subagentProvider: 'gpt', subagentModel: 'GPT-5.5-xhigh' }),
    tool({ key: 'l', id: 'wa-1:leaf', title: 'Read', subagentId: 'wa-1' }),
  ]);
  assert.equal(fromHeader.nodes[0].model, 'GPT-5.5-xhigh');
  // Backfills from a child when the header hasn't carried it yet.
  const fromChild = extractSubagents([
    tool({ key: 'l', id: 'wa-2:leaf', title: 'Read', subagentId: 'wa-2', subagentProvider: 'claude', subagentModel: 'Opus4.8-xhigh' }),
  ]);
  assert.equal(fromChild.nodes[0].model, 'Opus4.8-xhigh');
  // Absent (older daemon) → null, so the chip is simply hidden.
  const none = extractSubagents([
    tool({ key: 'h', id: 'wa-3', title: 'x', subagentId: 'wa-3', subagentHeader: true }),
  ]);
  assert.equal(none.nodes[0].model, null);
});

test('header renders inline as a task; children are dropped from the transcript', () => {
  const header = tool({ key: 'h', id: 'wa-1', title: 'Sol audit', subagentId: 'wa-1', subagentHeader: true, subagentProvider: 'gpt' });
  const child = tool({ key: 'l', id: 'wa-1:leaf', title: 'Read', subagentId: 'wa-1' });
  const main = tool({ key: 'main', id: 'main-1', title: 'Main' });
  // The header shows in the chat (it's not a child); the child is rail-only.
  assert.equal(isSubagentHeader(header), true);
  assert.equal(isSubagentChild(header), false);
  assert.equal(isSubagentChild(child), true);
  assert.equal(isSubagentHeader(child), false);
  // A plain main-thread call is neither.
  assert.equal(isSubagentHeader(main), false);
  assert.equal(isSubagentChild(main), false);
  // A native-style header discovered via toolKind 'agent' also renders inline.
  assert.equal(isSubagentHeader(tool({ key: 'a', id: 'x', title: 'T', toolKind: 'agent' })), true);
});

test('failed and cancelled children never produce a success summary', () => {
  assert.deepEqual(summarizeSettledSubagents(['done', 'failed', 'cancelled']), {
    failedCount: 1, cancelledCount: 1, status: 'failed',
  });
  assert.deepEqual(summarizeSettledSubagents(['done', 'cancelled']), {
    failedCount: 0, cancelledCount: 1, status: 'cancelled',
  });
});

// The rail names a subagent's live call with the SAME vocabulary the transcript
// uses (tool-names.ts) and carries the action's glyph — no second classifier and
// no invented gerunds (approved mock rail-actions, 2026-07-27).
test('compact running summaries describe the latest real tool kind without exposing commands', () => {
  const node = extractSubagents([
    tool({ key: 'h', id: 'wa-1', title: 'Audit UI', status: 'in_progress', subagentId: 'wa-1', subagentHeader: true }),
    tool({ key: 'old', id: 'wa-1:old', title: 'Read File', status: 'completed', toolKind: 'read', subagentId: 'wa-1' }),
    tool({ key: 'live', id: 'wa-1:live', title: 'Terminal', status: 'in_progress', toolKind: 'execute', command: 'git status --short', subagentId: 'wa-1' }),
  ]).nodes[0];
  assert.equal(subagentActivity(node).label, 'Ejecutar un comando');
  assert.equal(subagentActivity(node).icon, 'run');
  // the exact command belongs to the expanded body, never to the collapsed row
  assert.doesNotMatch(subagentActivity(node).label, /git status/);
});

test('compact activity falls back through old titles and idle gaps safely', () => {
  const reading = extractSubagents([
    tool({ key: 'l', id: 'wa-2:leaf', title: 'Read File', status: 'pending', subagentId: 'wa-2' }),
  ]).nodes[0];
  assert.equal(subagentActivity(reading).label, 'Leer un archivo');
  assert.equal(subagentActivity(reading).icon, 'read');

  const betweenCalls = extractSubagents([
    tool({ key: 'h', id: 'wa-3', title: 'Audit', status: 'in_progress', subagentId: 'wa-3', subagentHeader: true }),
    tool({ key: 'done', id: 'wa-3:done', title: 'Search', status: 'completed', subagentId: 'wa-3' }),
  ]).nodes[0];
  assert.equal(subagentActivity(betweenCalls).label, 'Trabajando');
});
