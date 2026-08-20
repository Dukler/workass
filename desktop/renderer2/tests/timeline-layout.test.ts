import assert from 'node:assert/strict';
import test from 'node:test';
import { parseBlocks } from '../src/markdown/blocks.ts';
import type { Msg, TimelineEvent, ToolEvent } from '../src/store/types.ts';
import {
  assistantTurnBlockRanges,
  buildCoalescedTurnBlockTimelineSegments,
  buildTimelineSegments,
  buildTranscriptTimelineSegments,
  detachTimelineEvent,
  stableMarkdownBlockKeys,
} from '../src/timeline-layout.ts';
import type { TranscriptTimelineSegment } from '../src/timeline-layout.ts';

function tool(key: string, at: number): ToolEvent {
  return {
    key, at, kind: 'tool', id: key, toolKind: 'read', title: key,
    status: 'completed', command: null, terminalId: null, input: null,
    output: null, location: null,
  };
}

function message(content: string, at: number, status: Msg['status'] = 'running', tools = 1): Msg {
  return {
    id: 'assistant-1', role: 'assistant', content, status, at: null,
    events: Array.from({ length: tools }, (_, index) => tool(`tool-${index + 1}`, at)),
  };
}

function shape(msg: Msg): string[] {
  return buildTimelineSegments(msg).map((segment) => 'prose' in segment ? `p:${segment.prose}` : `e:${segment.event.key}`);
}

function referenceInsideFence(content: string, offset: number): boolean {
  const prefix = content.slice(0, offset);
  const fences = prefix.match(/^\s*(`{3,}|~{3,})/gm);
  return !!fences && fences.length % 2 === 1;
}

function referenceBoundary(content: string, rawOffset: number, running: boolean): number | null {
  const at = Math.min(Math.max(rawOffset, 0), content.length);
  if (at === 0) return 0;
  const left = content.slice(0, at);
  if (!referenceInsideFence(content, at) && (/(?:\n\s*\n|[.!?:]["')\]]?)\s*$/.test(left))) return at;
  const tail = content.slice(at);
  const boundary = /\n\s*\n|[.!?]["')\]]?(?=\s|[A-Z]|$)/g;
  let match: RegExpExecArray | null;
  while ((match = boundary.exec(tail))) {
    let end = at + match.index + match[0].length;
    while (end < content.length && /[ \t\r\n]/.test(content[end])) end++;
    if (!referenceInsideFence(content, end)) return end;
  }
  return running ? null : content.length;
}

function markdownKeys(msg: Msg): string[] {
  const signatures: string[] = [];
  for (const segment of buildTranscriptTimelineSegments(msg)) {
    if (!('prose' in segment)) continue;
    for (const block of parseBlocks(segment.prose)) signatures.push(block.sig);
  }
  return stableMarkdownBlockKeys(msg, signatures);
}

function assistantRow(
  id: string,
  turnRootId: string,
  events: TimelineEvent[],
  content = '',
  status: Msg['status'] = 'done',
): Msg {
  return { id, role: 'assistant', content, status, at: null, events, turnRootId };
}

function coalescedVisibleRows(messages: Msg[]): Array<TranscriptTimelineSegment[] | null> {
  const rows = messages.map((message) => message.role === 'assistant'
    ? buildTranscriptTimelineSegments(message)
    : null);
  for (const { start, end } of assistantTurnBlockRanges(messages)) {
    const block = buildCoalescedTurnBlockTimelineSegments(messages.slice(start, end));
    for (let index = start; index < end; index++) rows[index] = block[index - start];
  }
  return rows;
}

function groups(row: TranscriptTimelineSegment[] | null): ToolEvent[][] {
  return (row ?? []).filter((segment) => 'tools' in segment).map((segment) => segment.tools);
}

test('defers a tool while its surrounding sentence is still streaming', () => {
  assert.deepEqual(shape(message('I am inspecting the', 8)), ['p:I am inspecting the']);
});

test('places a deferred tool after the completed sentence, never inside it', () => {
  assert.deepEqual(
    shape(message('I am inspecting the repository. The result follows.', 8)),
    ['p:I am inspecting the repository. ', 'e:tool-1', 'p:The result follows.'],
  );
});

test('keeps a tool at an intentional sentence or colon boundary', () => {
  assert.deepEqual(shape(message('I will inspect this now.Next step.', 24)), [
    'p:I will inspect this now.', 'e:tool-1', 'p:Next step.',
  ]);
  assert.deepEqual(shape(message('Verification:', 13)), ['p:Verification:', 'e:tool-1']);
});

test('completed turns put a boundary-less tool after all prose', () => {
  assert.deepEqual(shape(message('A sentence fragment', 4, 'done')), ['p:A sentence fragment', 'e:tool-1']);
});

test('multiple tools preserve their order at the same safe boundary', () => {
  assert.deepEqual(shape(message('Checking the repository. Done.', 5, 'running', 2)), [
    'p:Checking the repository. ', 'e:tool-1', 'e:tool-2', 'p:Done.',
  ]);
});

test('does not place a tool at punctuation inside a fenced code block', () => {
  const content = '```js\nconsole.log("done.");\n```\n\nThe result is ready.';
  assert.deepEqual(shape(message(content, 10)), [
    'p:```js\nconsole.log("done.");\n```\n\n', 'e:tool-1', 'p:The result is ready.',
  ]);
});

test('indexed placement matches the frozen boundary behavior across offsets', () => {
  const fixtures = [
    'A fragment without a boundary',
    'Done.Next sentence. Then more.',
    'Quoted result!\"  Next.',
    'Intro:\n\n\nResult follows.',
    '```js\nconsole.log("inside.");\n```\n\nOutside result.',
    '~~~txt\ninside?\n~~~\nAfter.',
  ];
  for (const content of fixtures) {
    for (const status of ['running', 'done'] as const) {
      for (let at = 0; at <= content.length; at++) {
        const expected = referenceBoundary(content, at, status === 'running');
        const segments = buildTimelineSegments(message(content, at, status));
        const placed = segments.find((segment) => 'event' in segment);
        if (expected == null) {
          assert.equal(placed, undefined, `${status} offset ${at} in ${JSON.stringify(content)}`);
        } else {
          let cursor = 0;
          let actual: number | null = null;
          for (const segment of segments) {
            if ('prose' in segment) cursor += segment.prose.length;
            else { actual = cursor; break; }
          }
          assert.equal(actual, expected, `${status} offset ${at} in ${JSON.stringify(content)}`);
        }
      }
    }
  }
});

test('filters rail-only subagent children before placement and preserves later visible events', () => {
  const child: ToolEvent = {
    ...tool('child', 2), id: 'child', subagentId: 'parent', subagentHeader: false,
  };
  const visible: TimelineEvent = { key: 'compact', at: 8, kind: 'compaction' };
  const msg = message('unfinished fragment', 0);
  msg.events = [child, visible];

  const segments = buildTranscriptTimelineSegments(msg);
  assert.deepEqual(
    segments.map((segment) => 'prose' in segment ? `p:${segment.prose}` : 'tools' in segment ? `t:${segment.key}` : `e:${segment.event.key}`),
    ['p:unfinish', 'e:compact', 'p:ed fragment'],
  );
});

test('detaching the tail thinking row rejoins prose split at a streaming token boundary', () => {
  const content = 'I did not add a torrent/source exclusion in this patch.';
  const splitAt = content.indexOf(' exclusion');
  const thinking: TimelineEvent = { key: 'thought', at: splitAt, kind: 'thinking', text: 'Checking.' };
  const msg = assistantRow('assistant-live', 'turn-live', [thinking], content, 'running');

  const split = buildTranscriptTimelineSegments(msg);
  assert.deepEqual(split.map((segment) => 'prose' in segment ? `p:${segment.prose}` : `e:${segment.event.key}`), [
    'p:I did not add a torrent/source',
    'e:thought',
    'p: exclusion in this patch.',
  ]);

  const rendered = detachTimelineEvent(split, thinking.key);
  assert.deepEqual(rendered, [{ prose: content, start: 0, end: content.length }]);
  assert.equal(parseBlocks(rendered[0].prose).length, 1, 'the sentence must remain one Markdown paragraph');
});

test('detaching a tail event preserves every surrounding tool and event boundary', () => {
  const thinking: TimelineEvent = { key: 'thought', at: 3, kind: 'thinking', text: 'Checking.' };
  const compact: TimelineEvent = { key: 'compact', at: 6, kind: 'compaction' };
  const segments: TranscriptTimelineSegment[] = [
    { tools: [tool('before', 0)], key: 'before', revision: [] },
    { prose: 'one', start: 0, end: 3 },
    { event: thinking },
    { prose: ' two', start: 3, end: 7 },
    { event: compact },
  ];

  const rendered = detachTimelineEvent(segments, thinking.key);
  assert.equal(rendered.length, 3);
  assert.ok('tools' in rendered[0] && rendered[0].key === 'before');
  assert.deepEqual(rendered[1], { prose: 'one two', start: 0, end: 7 });
  assert.ok('event' in rendered[2] && rendered[2].event.key === 'compact');
  assert.equal(segments.length, 5, 'cached segments must remain immutable');
});

test('keeps a subagent header in its ordered tool group while dropping its children', () => {
  const header: ToolEvent = {
    ...tool('header', 6), id: 'parent', toolKind: 'agent', subagentId: 'parent', subagentHeader: true,
  };
  const child: ToolEvent = {
    ...tool('child', 6), id: 'child', subagentId: 'parent', subagentHeader: false,
  };
  const main = tool('main', 6);
  const msg = message('Ready:', 0);
  msg.events = [header, child, main];

  const segments = buildTranscriptTimelineSegments(msg);
  const group = segments.find((segment) => 'tools' in segment);
  assert.ok(group && 'tools' in group);
  assert.equal(group.key, 'header');
  assert.deepEqual(group.tools.map((event) => event.key), ['header', 'main']);
});

test('tool groups keep a stable key and expose an immutable paint revision across in-place ACP updates', () => {
  const event = tool('stable-tool', 6);
  const msg = message('Ready:', 0);
  msg.events = [event];
  const before = buildTranscriptTimelineSegments(msg).find((segment) => 'tools' in segment);
  assert.ok(before && 'tools' in before);
  const previousRevision = [...before.revision];

  event.status = 'failed';
  event.output = 'failure detail';
  const after = buildTranscriptTimelineSegments(msg).find((segment) => 'tools' in segment);
  assert.ok(after && 'tools' in after);
  assert.equal(after.key, before.key);
  assert.deepEqual(before.revision, previousRevision);
  assert.notDeepEqual(after.revision, previousRevision);
});

test('sealed Markdown keys survive a deferred tool insertion and streaming-tail growth', () => {
  const first = 'First sealed paragraph.';
  const second = 'Second sealed paragraph.';
  const base = `${first}\n\n${second}\n\nTail`;
  const msg = message(base, 0);
  msg.events = [];
  const before = markdownKeys(msg);

  msg.events = [tool('between', first.length + 2)];
  msg.content += ' continues';
  const after = markdownKeys(msg);

  assert.equal(before.length, 3);
  assert.equal(after.length, 3);
  assert.deepEqual(after.slice(0, 2), before.slice(0, 2));
  assert.equal(after[2], before[2]);
});

test('a newly split paragraph keeps the sealed prefix key and gives the new tail a fresh key', () => {
  const msg = message('First paragraph', 0);
  msg.events = [];
  const before = markdownKeys(msg);

  msg.content += '\n\nSecond paragraph';
  const after = markdownKeys(msg);

  assert.equal(before.length, 1);
  assert.equal(after.length, 2);
  assert.equal(after[0], before[0], 'the exact sealed paragraph must keep its component identity');
  assert.notEqual(after[1], before[0], 'the new streaming tail must not steal the sealed paragraph key');
});

test('coalesces a terminal tail and head across adjacent rows of the same visible turn', () => {
  const firstTools = [tool('first-1', 0), tool('first-2', 0)];
  const secondTools = [tool('second-1', 0), tool('second-2', 0), tool('second-3', 0)];
  const rows = coalescedVisibleRows([
    assistantRow('row-1', 'turn-1', firstTools),
    assistantRow('row-2', 'turn-1', secondTools),
  ]);

  assert.deepEqual(groups(rows[0]).map((run) => run.map((event) => event.key)), [[
    'first-1', 'first-2', 'second-1', 'second-2', 'second-3',
  ]]);
  assert.deepEqual(groups(rows[1]), []);
  const merged = rows[0]?.find((segment) => 'tools' in segment);
  assert.ok(merged && 'tools' in merged);
  assert.equal(merged.key, 'first-1');
  assert.equal(merged.tools.length, 5);
});

test('coalesces terminal tool-only boundaries transitively across three rows', () => {
  const rows = coalescedVisibleRows([
    assistantRow('row-1', 'turn-1', [tool('first', 0)]),
    assistantRow('row-2', 'turn-1', [tool('second', 0)]),
    assistantRow('row-3', 'turn-1', [tool('third', 0)]),
  ]);

  assert.deepEqual(groups(rows[0]).map((run) => run.map((event) => event.key)), [['first', 'second', 'third']]);
  assert.deepEqual(groups(rows[1]), []);
  assert.deepEqual(groups(rows[2]), []);
});

test('does not coalesce adjacent assistant rows from different turns', () => {
  const rows = coalescedVisibleRows([
    assistantRow('row-1', 'turn-1', [tool('first', 0)]),
    assistantRow('row-2', 'turn-2', [tool('second', 0)]),
  ]);

  assert.deepEqual(groups(rows[0]).map((run) => run.map((event) => event.key)), [['first']]);
  assert.deepEqual(groups(rows[1]).map((run) => run.map((event) => event.key)), [['second']]);
});

test('prose or thinking at a row boundary blocks coalescing', () => {
  const proseRows = coalescedVisibleRows([
    assistantRow('prose-1', 'turn-prose', [tool('first', 0)], 'After the call.'),
    assistantRow('prose-2', 'turn-prose', [tool('second', 0)]),
  ]);
  assert.deepEqual(groups(proseRows[0]).map((run) => run.map((event) => event.key)), [['first']]);
  assert.deepEqual(groups(proseRows[1]).map((run) => run.map((event) => event.key)), [['second']]);

  const thinking: TimelineEvent = { key: 'thought', at: 0, kind: 'thinking', text: 'Checking.' };
  const thinkingRows = coalescedVisibleRows([
    assistantRow('thinking-1', 'turn-thinking', [tool('first', 0), thinking]),
    assistantRow('thinking-2', 'turn-thinking', [tool('second', 0)]),
  ]);
  assert.deepEqual(groups(thinkingRows[0]).map((run) => run.map((event) => event.key)), [['first']]);
  assert.deepEqual(groups(thinkingRows[1]).map((run) => run.map((event) => event.key)), [['second']]);
});

test('a running or pending boundary group never coalesces', () => {
  const running = { ...tool('running', 0), status: 'in_progress' };
  const rows = coalescedVisibleRows([
    assistantRow('row-1', 'turn-1', [tool('finished', 0)]),
    assistantRow('row-2', 'turn-1', [running], '', 'running'),
  ]);

  assert.deepEqual(groups(rows[0]).map((run) => run.map((event) => event.key)), [['finished']]);
  assert.deepEqual(groups(rows[1]).map((run) => run.map((event) => event.key)), [['running']]);
});

test('a user row between continuation rows prevents coalescing', () => {
  const rows = coalescedVisibleRows([
    assistantRow('row-1', 'turn-1', [tool('first', 0)]),
    { id: 'user-1', role: 'user', content: 'Steer', status: 'done', at: null, events: [], turnRootId: 'turn-1' },
    assistantRow('row-2', 'turn-1', [tool('second', 0)]),
  ]);

  assert.deepEqual(groups(rows[0]).map((run) => run.map((event) => event.key)), [['first']]);
  assert.equal(rows[1], null);
  assert.deepEqual(groups(rows[2]).map((run) => run.map((event) => event.key)), [['second']]);
});

test('a streaming continuation may merge an already-terminal leading group', () => {
  const rows = coalescedVisibleRows([
    assistantRow('row-1', 'turn-1', [tool('first', 0)]),
    assistantRow('row-2', 'turn-1', [tool('second', 0)], 'Streaming prose', 'running'),
  ]);

  assert.deepEqual(groups(rows[0]).map((run) => run.map((event) => event.key)), [['first', 'second']]);
  assert.deepEqual(groups(rows[1]), []);
});

test('an unrelated row edit preserves other coalesced segment-list references', () => {
  const first = assistantRow('row-1', 'turn-1', [tool('first', 0)]);
  const second = assistantRow('row-2', 'turn-1', [tool('second', 0)], 'Boundary prose.');
  const unrelated = assistantRow('row-3', 'turn-1', [], 'Unrelated prose.');
  const messages = [first, second, unrelated];
  const before = buildCoalescedTurnBlockTimelineSegments(messages);

  unrelated.content = 'Edited unrelated prose.';
  const after = buildCoalescedTurnBlockTimelineSegments(messages);

  assert.equal(after[0], before[0]);
  assert.equal(after[1], before[1]);
  assert.notEqual(after[2], before[2]);
});

test('sealing a row with terminal boundary tools keeps the coalesced layout references stable', () => {
  const first = assistantRow('row-1', 'turn-1', [tool('first', 0)]);
  const last = assistantRow('row-2', 'turn-1', [tool('second', 0)], 'Streaming prose', 'running');
  const before = buildCoalescedTurnBlockTimelineSegments([first, last]);

  last.status = 'done';
  const after = buildCoalescedTurnBlockTimelineSegments([first, last]);

  assert.equal(after[0], before[0]);
  assert.equal(after[1], before[1]);
});
