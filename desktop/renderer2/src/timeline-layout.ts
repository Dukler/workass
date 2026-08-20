import type { Msg, TimelineEvent, ToolEvent } from './store/types';
import { isSubagentChild } from './subagent-layout.ts';

export type TimelineSegment =
  | { prose: string; start: number; end: number }
  | { event: TimelineEvent };

export type TranscriptTimelineSegment = TimelineSegment | { tools: ToolEvent[]; key: string; revision: unknown[] };

interface BoundaryCandidate {
  start: number;
  end: number;
}

interface ParagraphBoundary {
  first: number;
  last: number;
  end: number;
}

interface BoundaryIndex {
  fenceToggleEnds: number[];
  punctuation: BoundaryCandidate[];
  paragraphs: ParagraphBoundary[];
}

const boundaryCache = new WeakMap<Msg, { content: string; index: BoundaryIndex }>();
const markdownKeyCache = new WeakMap<Msg, { next: number; entries: Array<{ signature: string; key: string }> }>();
const transcriptTimelineCache = new WeakMap<Msg, {
  snapshot: readonly unknown[];
  segments: TranscriptTimelineSegment[];
}>();
const turnBlockTimelineCache = new WeakMap<Msg, {
  messages: readonly Msg[];
  segments: TranscriptTimelineSegment[][];
}>();
const CAPTURE_PUNCTUATION = '.!?:';
const CLOSERS = `"')]`;

function isWhitespace(ch: string): boolean {
  // Keep the parser's JavaScript `\s` semantics for non-ASCII model output,
  // while avoiding a RegExp call for the overwhelmingly common ASCII path.
  const code = ch.charCodeAt(0);
  return code === 32 || (code >= 9 && code <= 13) || (code === 160) || /\s/u.test(ch);
}

function isWhitespaceAt(content: string, index: number): boolean {
  const code = content.charCodeAt(index);
  if (code === 32 || (code >= 9 && code <= 13) || code === 160) return true;
  return code >= 128 && /\s/u.test(content[index]);
}

function isExtendableWhitespaceAt(content: string, index: number): boolean {
  const code = content.charCodeAt(index);
  return code === 32 || code === 9 || code === 10 || code === 13;
}

function fenceToggleEnds(content: string): number[] {
  const ends: number[] = [];
  let lineStart = 0;
  while (lineStart < content.length) {
    let marker = lineStart;
    while (marker < content.length && content.charCodeAt(marker) !== 10 && isWhitespaceAt(content, marker)) marker++;
    const markerCode = content.charCodeAt(marker);
    if (markerCode === 96 || markerCode === 126) {
      let end = marker + 1;
      while (end < content.length && content.charCodeAt(end) === markerCode) end++;
      if (end - marker >= 3) ends.push(end);
    }
    const newline = content.indexOf('\n', marker);
    if (newline < 0) break;
    lineStart = newline + 1;
  }
  return ends;
}

function upperBound(values: number[], value: number): number {
  let lo = 0;
  let hi = values.length;
  while (lo < hi) {
    const mid = (lo + hi) >>> 1;
    if (values[mid] <= value) lo = mid + 1;
    else hi = mid;
  }
  return lo;
}

function isInsideFence(toggleEnds: number[], offset: number): boolean {
  return upperBound(toggleEnds, offset) % 2 === 1;
}

// Build all future safe boundaries in one linear pass. Paragraph whitespace
// runs store a compact first/last eligible newline range instead of one object
// per newline; lookup uses String#indexOf to recover the exact first candidate
// at/after a tool capture. This preserves overlapping blank-line semantics
// without either O(content × tools) rescans or pathological all-newline regexes.
function buildBoundaryIndex(content: string): BoundaryIndex {
  const toggleEnds = fenceToggleEnds(content);
  const punctuation: BoundaryCandidate[] = [];
  const paragraphs: ParagraphBoundary[] = [];
  let cursor = 0;
  while (cursor < content.length) {
    if (isWhitespaceAt(content, cursor)) {
      let firstNewline = -1;
      let previousNewline = -1;
      let lastNewline = -1;
      while (cursor < content.length && isWhitespaceAt(content, cursor)) {
        if (content.charCodeAt(cursor) === 10) {
          if (firstNewline < 0) firstNewline = cursor;
          previousNewline = lastNewline;
          lastNewline = cursor;
        }
        cursor++;
      }
      if (previousNewline >= 0) {
        let end = lastNewline + 1;
        while (end < content.length && isExtendableWhitespaceAt(content, end)) end++;
        if (!isInsideFence(toggleEnds, end)) {
          paragraphs.push({ first: firstNewline, last: previousNewline, end });
        }
      }
      continue;
    }

    const code = content.charCodeAt(cursor);
    if (code === 46 || code === 33 || code === 63) {
      let matchEnd = cursor + 1;
      if (matchEnd < content.length && CLOSERS.includes(content[matchEnd])) matchEnd++;
      const nextCode = content.charCodeAt(matchEnd);
      if (matchEnd === content.length || isWhitespaceAt(content, matchEnd) || (nextCode >= 65 && nextCode <= 90)) {
        let end = matchEnd;
        while (end < content.length && isExtendableWhitespaceAt(content, end)) end++;
        if (!isInsideFence(toggleEnds, end)) punctuation.push({ start: cursor, end });
      }
    }
    cursor++;
  }
  return { fenceToggleEnds: toggleEnds, punctuation, paragraphs };
}

function boundaryIndexFor(msg: Msg): BoundaryIndex {
  const cached = boundaryCache.get(msg);
  if (cached?.content === msg.content) return cached.index;
  const index = buildBoundaryIndex(msg.content);
  boundaryCache.set(msg, { content: msg.content, index });
  return index;
}

function firstPunctuationAtOrAfter(candidates: BoundaryCandidate[], at: number): BoundaryCandidate | undefined {
  let lo = 0;
  let hi = candidates.length;
  while (lo < hi) {
    const mid = (lo + hi) >>> 1;
    if (candidates[mid].start < at) lo = mid + 1;
    else hi = mid;
  }
  return candidates[lo];
}

function firstParagraphAtOrAfter(content: string, ranges: ParagraphBoundary[], at: number): BoundaryCandidate | undefined {
  let lo = 0;
  let hi = ranges.length;
  while (lo < hi) {
    const mid = (lo + hi) >>> 1;
    if (ranges[mid].last < at) lo = mid + 1;
    else hi = mid;
  }
  const range = ranges[lo];
  if (!range) return undefined;
  const start = content.indexOf('\n', Math.max(at, range.first));
  return start >= 0 && start <= range.last ? { start, end: range.end } : undefined;
}

function firstCandidateAtOrAfter(content: string, index: BoundaryIndex, at: number): BoundaryCandidate | undefined {
  const punctuation = firstPunctuationAtOrAfter(index.punctuation, at);
  const paragraph = firstParagraphAtOrAfter(content, index.paragraphs, at);
  if (!punctuation) return paragraph;
  if (!paragraph) return punctuation;
  return punctuation.start <= paragraph.start ? punctuation : paragraph;
}

function captureBoundaryAt(content: string, index: BoundaryIndex, at: number): boolean {
  if (at === 0) return true;
  if (isInsideFence(index.fenceToggleEnds, at)) return false;
  let cursor = at - 1;
  let trailingNewlines = 0;
  while (cursor >= 0 && isWhitespace(content[cursor])) {
    if (content[cursor] === '\n') trailingNewlines++;
    cursor--;
  }
  if (trailingNewlines >= 2) return true;
  if (cursor < 0) return false;
  const ch = content[cursor];
  return CAPTURE_PUNCTUATION.includes(ch)
    || (CLOSERS.includes(ch) && cursor > 0 && CAPTURE_PUNCTUATION.includes(content[cursor - 1]));
}

function eventBoundary(content: string, index: BoundaryIndex, rawOffset: number, running: boolean): number | null {
  const at = Math.min(Math.max(rawOffset, 0), content.length);
  if (captureBoundaryAt(content, index, at)) return at;
  const next = firstCandidateAtOrAfter(content, index, at);
  if (next) return next.end;

  // A completed response has no more text to supply a boundary. Put the tool
  // after all prose; while streaming, defer it instead of flashing mid-sentence.
  return running ? null : content.length;
}

// Interleave prose with timeline events. Capture offsets remain untouched in
// state; only tool-card presentation is shifted forward to a safe prose
// boundary. Once a tool is deferred, later events follow it so event ordering
// remains stable.
export function buildTimelineSegments(msg: Msg, sourceEvents: readonly TimelineEvent[] = msg.events): TimelineSegment[] {
  const events = sourceEvents
    .map((event, order) => ({ event, order }))
    .sort((a, b) => a.event.at - b.event.at || a.order - b.order);
  const hasTools = events.some(({ event }) => event.kind === 'tool');
  const index = hasTools ? boundaryIndexFor(msg) : null;
  const placed: Array<{ event: TimelineEvent; at: number }> = [];
  let lastAt = 0;
  let blocked = false;

  for (const { event } of events) {
    if (blocked) continue;
    const raw = Math.min(Math.max(event.at, 0), msg.content.length);
    const safe = event.kind === 'tool'
      ? eventBoundary(msg.content, index!, raw, msg.status === 'running')
      : raw;
    if (safe == null) { blocked = true; continue; }
    const at = Math.max(lastAt, safe);
    placed.push({ event, at });
    lastAt = at;
  }

  const segments: TimelineSegment[] = [];
  let cursor = 0;
  for (const item of placed) {
    if (item.at > cursor) {
      segments.push({ prose: msg.content.slice(cursor, item.at), start: cursor, end: item.at });
      cursor = item.at;
    }
    segments.push({ event: item.event });
  }
  if (cursor < msg.content.length || segments.length === 0) {
    segments.push({ prose: msg.content.slice(cursor), start: cursor, end: msg.content.length });
  }
  return segments;
}

function foldToolGroups(segments: TimelineSegment[]): TranscriptTimelineSegment[] {
  const out: TranscriptTimelineSegment[] = [];
  for (const segment of segments) {
    if ('event' in segment && segment.event.kind === 'tool') {
      const last = out[out.length - 1];
      if (last && 'tools' in last) {
        last.tools.push(segment.event);
        last.revision.push(...toolRenderSnapshot(segment.event));
      } else {
        out.push({ tools: [segment.event], key: segment.event.key, revision: toolRenderSnapshot(segment.event) });
      }
    } else {
      out.push(segment);
    }
  }
  return out;
}

// ToolEvent objects are updated in place by ACP, so React.memo cannot compare
// old/new object fields after a mutation. Snapshot only primitives that affect
// ToolGroup/ToolDetail paint; unchanged collapsed groups then skip prose-frame
// renders while live status/output changes still invalidate immediately.
function toolRenderSnapshot(tool: ToolEvent): unknown[] {
  const imageSnapshot = (tool.images ?? []).flatMap((image) => [image.mimeType, image.data, image.name]);
  return [
    tool.key, tool.status, tool.title, tool.toolKind, tool.command, tool.location, tool.output,
    tool.subagentHeader, tool.subagentProvider, tool.subagentModel, tool.startedAt, tool.endedAt,
    tool.images?.length ?? 0, ...imageSnapshot,
  ];
}

function sameSnapshot(previous: readonly unknown[], next: readonly unknown[]): boolean {
  if (previous.length !== next.length) return false;
  for (let index = 0; index < previous.length; index++) {
    if (previous[index] !== next[index]) return false;
  }
  return true;
}

function transcriptTimelineSnapshot(
  msg: Msg,
  visibleEvents: readonly TimelineEvent[],
): unknown[] {
  const snapshot: unknown[] = [msg.content, msg.status, visibleEvents.length];
  for (const event of visibleEvents) {
    snapshot.push(event, event.key, event.at, event.kind);
    if (event.kind === 'tool') {
      snapshot.push(event.id, event.subagentId, event.subagentHeader, event.subagentProvider, event.subagentModel);
      snapshot.push(...toolRenderSnapshot(event));
    }
  }
  return snapshot;
}

// Transcript law: subagent child calls are rail-only. Filter them BEFORE
// sorting/boundary placement so hundreds of hidden calls never scan prose or
// block later visible events; subagent headers remain normal grouped tool rows.
export function buildTranscriptTimelineSegments(msg: Msg): TranscriptTimelineSegment[] {
  const visibleEvents = msg.events.filter((event) => event.kind !== 'tool'
    || !isSubagentChild(event));
  const snapshot = transcriptTimelineSnapshot(msg, visibleEvents);
  const cached = transcriptTimelineCache.get(msg);
  if (cached && sameSnapshot(cached.snapshot, snapshot)) return cached.segments;
  const segments = foldToolGroups(buildTimelineSegments(msg, visibleEvents));
  transcriptTimelineCache.set(msg, { snapshot, segments });
  return segments;
}

// Some timeline events are rendered outside the prose flow. The live/settled
// thinking row is the important case: AssistantMessage moves it to the turn
// tail, but its capture boundary still splits the surrounding sentence into
// two Markdown paragraphs unless the adjacent prose is joined here. Preserve
// every other event/tool boundary and never mutate the cached segment list.
export function detachTimelineEvent(
  segments: readonly TranscriptTimelineSegment[],
  eventKey: string | undefined,
): TranscriptTimelineSegment[] {
  if (!eventKey) return segments as TranscriptTimelineSegment[];
  const eventIndex = segments.findIndex((segment) => 'event' in segment && segment.event.key === eventKey);
  if (eventIndex < 0) return segments as TranscriptTimelineSegment[];

  const previous = segments[eventIndex - 1];
  const next = segments[eventIndex + 1];
  if (previous && next && 'prose' in previous && 'prose' in next) {
    return [
      ...segments.slice(0, eventIndex - 1),
      {
        prose: previous.prose + next.prose,
        start: previous.start,
        end: next.end,
      },
      ...segments.slice(eventIndex + 2),
    ];
  }
  return [...segments.slice(0, eventIndex), ...segments.slice(eventIndex + 1)];
}

type ToolGroupSegment = Extract<TranscriptTimelineSegment, { tools: ToolEvent[] }>;

function isToolGroupSegment(segment: TranscriptTimelineSegment | undefined): segment is ToolGroupSegment {
  return !!segment && 'tools' in segment;
}

function isTerminalTool(tool: ToolEvent): boolean {
  return tool.status === 'completed'
    || tool.status === 'success'
    || tool.status === 'failed'
    || tool.status === 'error'
    || tool.status === 'cancelled'
    || tool.status === 'canceled';
}

function isFullyTerminalGroup(segment: TranscriptTimelineSegment | undefined): segment is ToolGroupSegment {
  return isToolGroupSegment(segment) && segment.tools.length > 0 && segment.tools.every(isTerminalTool);
}

// Merge only the tool folds that touch an assistant-row boundary. Inputs are
// never mutated: rows without a merge retain their original array reference,
// while the first row owns the concatenated fold and later rows lose the
// leading group that moved into it.
export function coalesceTurnBlockToolFolds(
  perMsgSegments: readonly (readonly TranscriptTimelineSegment[])[],
): TranscriptTimelineSegment[][] {
  if (perMsgSegments.length === 0) return [];
  const output = perMsgSegments.map((segments) => segments as TranscriptTimelineSegment[]);
  const first = output[0];
  let anchor = first.length > 0 && isFullyTerminalGroup(first[first.length - 1])
    ? { row: 0, index: first.length - 1 }
    : null;

  for (let row = 1; row < output.length; row++) {
    const before = output[row];
    const leading = before[0];
    let removedLeading = false;
    if (anchor && isFullyTerminalGroup(leading)) {
      const anchorSegments = output[anchor.row];
      const anchorGroup = anchorSegments[anchor.index];
      if (isFullyTerminalGroup(anchorGroup)) {
        const nextAnchorSegments = anchorSegments.slice();
        nextAnchorSegments[anchor.index] = {
          tools: [...anchorGroup.tools, ...leading.tools],
          key: anchorGroup.key,
          revision: [...anchorGroup.revision, ...leading.revision],
        };
        output[anchor.row] = nextAnchorSegments;
        output[row] = before.slice(1);
        removedLeading = true;
      }
    }

    const current = output[row];
    if (current.length === 0) {
      // A row emptied by the merge is transparent so three or more adjacent
      // tool-only continuation rows coalesce into the original first fold.
      if (!removedLeading) anchor = null;
      continue;
    }
    const trailingIndex = current.length - 1;
    anchor = isFullyTerminalGroup(current[trailingIndex]) ? { row, index: trailingIndex } : null;
  }
  return output;
}

function sameTimelineSegmentList(
  previous: readonly TranscriptTimelineSegment[],
  next: readonly TranscriptTimelineSegment[],
): boolean {
  if (previous === next) return true;
  if (previous.length !== next.length) return false;
  for (let index = 0; index < previous.length; index++) {
    const left = previous[index];
    const right = next[index];
    if (left === right) continue;
    if ('prose' in left && 'prose' in right) {
      if (left.prose === right.prose && left.start === right.start && left.end === right.end) continue;
      return false;
    }
    if ('event' in left && 'event' in right) {
      if (left.event === right.event) continue;
      return false;
    }
    if (!isToolGroupSegment(left) || !isToolGroupSegment(right)) return false;
    if (left.key !== right.key || left.tools.length !== right.tools.length) return false;
    for (let toolIndex = 0; toolIndex < left.tools.length; toolIndex++) {
      if (left.tools[toolIndex] !== right.tools[toolIndex]) return false;
    }
    if (!sameSnapshot(left.revision, right.revision)) return false;
  }
  return true;
}

// Cache only the structural result around the pure coalescer. Per-message
// layout snapshots keep untouched rows stable, and this reconciliation also
// preserves rows whose merged fold is semantically unchanged when another row
// in the same continuation block updates.
export function buildCoalescedTurnBlockTimelineSegments(
  messages: readonly Msg[],
): TranscriptTimelineSegment[][] {
  if (messages.length === 0) return [];
  const perMsgSegments = messages.map((message) => buildTranscriptTimelineSegments(message));
  const candidate = coalesceTurnBlockToolFolds(perMsgSegments);
  const first = messages[0];
  const cached = turnBlockTimelineCache.get(first);
  const segments = candidate.map((row, index) => (
    cached?.messages[index] === messages[index]
      && cached.segments[index]
      && sameTimelineSegmentList(cached.segments[index], row)
      ? cached.segments[index]
      : row
  ));
  turnBlockTimelineCache.set(first, { messages: [...messages], segments });
  return segments;
}

export interface AssistantTurnBlockRange { start: number; end: number }

// Return only multi-row blocks because singleton assistant rows keep the
// existing isolated AssistantMessage path. `end` is exclusive.
export function assistantTurnBlockRanges(messages: readonly Msg[]): AssistantTurnBlockRange[] {
  const blocks: AssistantTurnBlockRange[] = [];
  let start = 0;
  while (start < messages.length) {
    const first = messages[start];
    const root = first.role === 'assistant' ? first.turnRootId?.trim() : '';
    if (!root) { start++; continue; }
    let end = start + 1;
    while (end < messages.length) {
      const next = messages[end];
      if (next.role !== 'assistant' || next.turnRootId?.trim() !== root) break;
      end++;
    }
    if (end - start > 1) blocks.push({ start, end });
    start = end;
  }
  return blocks;
}

// Stable React keys for Markdown blocks in one mutable message. Exact signatures
// keep their prior keys even if a deferred tool splits/reorders prose segments.
// The one unmatched block at the tail inherits the previous tail key as tokens
// grow, so the receiving block updates in place instead of remounting per chunk.
export function stableMarkdownBlockKeys(msg: Msg, signatures: readonly string[]): string[] {
  let state = markdownKeyCache.get(msg);
  if (!state) state = { next: 0, entries: [] };

  const keys = new Array<string | undefined>(signatures.length);
  const previousTail = state.entries[state.entries.length - 1];
  const available = new Map<string, { keys: string[]; cursor: number }>();
  for (const entry of state.entries) {
    const queue = available.get(entry.signature);
    if (queue) queue.keys.push(entry.key);
    else available.set(entry.signature, { keys: [entry.key], cursor: 0 });
  }

  const used = new Set<string>();
  for (let index = 0; index < signatures.length; index++) {
    const queue = available.get(signatures[index]);
    if (queue && queue.cursor < queue.keys.length) {
      const key = queue.keys[queue.cursor++];
      keys[index] = key;
      used.add(key);
    }
  }
  // Exact sealed-block matches win first. Only an unmatched streaming tail may
  // inherit the previous tail's key, and only when that key was not already
  // claimed by an exact match (the common paragraph-split case).
  const tailIndex = signatures.length - 1;
  if (tailIndex >= 0 && keys[tailIndex] == null && previousTail && !used.has(previousTail.key)) {
    keys[tailIndex] = previousTail.key;
  }
  for (let index = 0; index < keys.length; index++) {
    if (keys[index] == null) keys[index] = `md:${state.next++}`;
  }

  const resolved = keys as string[];
  state.entries = signatures.map((signature, index) => ({ signature, key: resolved[index] }));
  markdownKeyCache.set(msg, state);
  return resolved;
}
