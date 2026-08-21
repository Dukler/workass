import assert from 'node:assert/strict';
import test from 'node:test';
import { parseBlocks } from '../src/markdown/blocks.ts';
import type { Msg, ToolEvent } from '../src/store/types.ts';
import { buildTranscriptTimelineSegments, stableMarkdownBlockKeys } from '../src/timeline-layout.ts';

const CHUNK_BYTES = 128;
const CHUNK_COUNT = 4096;
const CHUNKS_PER_FRAME = 15;

function burstChunk(index: number): string {
  const prefix = `Burst ${String(index + 1).padStart(5, '0')} `;
  const suffix = index % 16 === 15 ? '\n\n' : ' ';
  return (prefix + 'render cadence '.repeat(Math.ceil(CHUNK_BYTES / 15))).slice(0, CHUNK_BYTES - suffix.length) + suffix;
}

function percentile(sorted: number[], ratio: number): number {
  return sorted[Math.min(sorted.length - 1, Math.floor(sorted.length * ratio))];
}

function measuredCPUms(run: () => void): number {
  const started = process.cpuUsage();
  run();
  const elapsed = process.cpuUsage(started);
  return (elapsed.user + elapsed.system) / 1_000;
}

test('benchmark: 512 KiB burst stays inside the renderer preparation budget at display cadence', () => {
  const msg: Msg = {
    id: 'burst-message', role: 'assistant', content: '', status: 'running', at: null, events: [],
  };
  const samples: number[] = [];
  let renderedBlocks = 0;

  for (let offset = 0; offset < CHUNK_COUNT; offset += CHUNKS_PER_FRAME) {
    for (let index = offset; index < Math.min(CHUNK_COUNT, offset + CHUNKS_PER_FRAME); index++) {
      msg.content += burstChunk(index);
    }
    const signatures: string[] = [];
    samples.push(measuredCPUms(() => {
      for (const segment of buildTranscriptTimelineSegments(msg)) {
        if (!('prose' in segment)) continue;
        for (const block of parseBlocks(segment.prose)) signatures.push(block.sig);
      }
      void stableMarkdownBlockKeys(msg, signatures);
    }));
    renderedBlocks = signatures.length;
  }

  assert.equal(msg.content.length, CHUNK_COUNT * CHUNK_BYTES);
  assert.equal(renderedBlocks, CHUNK_COUNT / 16);
  samples.sort((a, b) => a - b);
  const p95 = percentile(samples, 0.95);
  const max = samples[samples.length - 1];
  assert.ok(p95 < 8, `p95 renderer preparation ${p95.toFixed(3)}ms exceeded 8ms`);
  assert.ok(max < 20, `max renderer preparation ${max.toFixed(3)}ms exceeded 20ms`);
});

test('benchmark: 384 KiB event-rich stream with 200 tools prepares inside one display frame at p95', () => {
  const totalBytes = 384 * 1024;
  const initialBytes = 256 * 1024;
  const frameCount = 72;
  const unit = 'Streaming sentence with enough prose to exercise tool boundaries.\n\n';
  const full = unit.repeat(Math.ceil(totalBytes / unit.length)).slice(0, totalBytes);
  const events: ToolEvent[] = Array.from({ length: 200 }, (_, index) => ({
    key: `event-rich-${index}`,
    at: Math.floor(((index + 1) * 192 * 1024) / 201),
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
    id: 'event-rich-message', role: 'assistant', content: '', status: 'running', at: null, events,
  };
  const samples: number[] = [];
  let renderedBlocks = 0;
  let renderedTools = 0;

  for (let frame = 0; frame < frameCount; frame++) {
    const bytes = initialBytes + Math.floor(((frame + 1) * (totalBytes - initialBytes)) / frameCount);
    msg.content = full.slice(0, bytes);
    const signatures: string[] = [];
    let blocks = 0;
    let tools = 0;
    samples.push(measuredCPUms(() => {
      for (const segment of buildTranscriptTimelineSegments(msg)) {
        if ('tools' in segment) {
          tools += segment.tools.length;
          continue;
        }
        if (!('prose' in segment)) continue;
        for (const block of parseBlocks(segment.prose)) {
          signatures.push(block.sig);
          blocks++;
        }
      }
      void stableMarkdownBlockKeys(msg, signatures);
    }));
    renderedBlocks = blocks;
    renderedTools = tools;
  }

  assert.equal(msg.content.length, totalBytes);
  assert.equal(renderedTools, events.length);
  assert.ok(renderedBlocks > 4_000, `expected an event-rich block workload, got ${renderedBlocks}`);
  samples.sort((a, b) => a - b);
  const p95 = percentile(samples, 0.95);
  assert.ok(p95 < 16.7, `p95 event-rich preparation ${p95.toFixed(3)}ms exceeded one 16.7ms frame`);
});
