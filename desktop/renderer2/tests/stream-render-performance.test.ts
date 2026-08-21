import assert from 'node:assert/strict';
import test from 'node:test';
import { parseBlocks } from '../src/markdown/blocks.ts';
import type { Msg, ToolEvent } from '../src/store/types.ts';
import { buildTranscriptTimelineSegments, stableMarkdownBlockKeys } from '../src/timeline-layout.ts';

test('incremental prose preparation preserves every completed Markdown block', () => {
  const msg: Msg = {
    id: 'stream-message', role: 'assistant', content: '', status: 'running', at: null, events: [],
  };
  for (let index = 0; index < 8; index++) msg.content += `Chunk ${index}\n\n`;

  const signatures: string[] = [];
  for (const segment of buildTranscriptTimelineSegments(msg)) {
    if (!('prose' in segment)) continue;
    for (const block of parseBlocks(segment.prose)) signatures.push(block.sig);
  }
  const keys = stableMarkdownBlockKeys(msg, signatures);

  assert.equal(signatures.length, 8);
  assert.equal(keys.length, signatures.length);
  assert.equal(new Set(keys).size, keys.length);
});

test('event-rich preparation retains every tool boundary beside prose', () => {
  const prose = 'Streaming sentence around tool boundaries.\n\n'.repeat(12);
  const events: ToolEvent[] = Array.from({ length: 5 }, (_, index) => ({
    key: `event-rich-${index}`,
    at: Math.floor(((index + 1) * prose.length) / 6),
    kind: 'tool',
    id: `event-rich-${index}`,
    toolKind: 'read',
    title: `Read fixture ${index}`,
    status: 'completed',
    command: null,
    terminalId: null,
    input: null,
    output: null,
    location: null,
  }));
  const msg: Msg = {
    id: 'event-rich-message', role: 'assistant', content: prose, status: 'running', at: null, events,
  };
  let tools = 0;
  let blocks = 0;
  for (const segment of buildTranscriptTimelineSegments(msg)) {
    if ('tools' in segment) tools += segment.tools.length;
    else if ('prose' in segment) blocks += parseBlocks(segment.prose).length;
  }

  assert.equal(tools, events.length);
  assert.ok(blocks > 0);
});
