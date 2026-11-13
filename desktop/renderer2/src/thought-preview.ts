// The one-line preview of a reasoning thought (t3code's rule): a bold-only
// opening line (the Codex summary heading) wins, else the first sentence of the
// thought as plain text.
//
// A running thought grows by appends, and rendering it again must not cost a
// pass over everything written so far (a 96 KiB thought used to rerun five
// regex passes over all of it on every chunk). thoughtPreview therefore keeps,
// per thought, the plain text of the part that is already final and only
// converts what was appended. The output is identical to
// fullThoughtPreview(markdown) for every input (tests/thought-preview.test.ts).
//
// Why a newline cut is final: every step of thoughtPlain except the link step
// and the whitespace collapse works inside one line, so converting the text up
// to and including a newline, then the rest, gives the same characters as
// converting it whole. The link step is safe once no "[" or "](" before the
// cut is still waiting for its "]" or ")"; the whitespace run that spans the
// cut collapses to the one space both halves agree on. And a sentence end found
// before the cut never moves, because a later chunk cannot change anything
// before it.

const LINK = /!?\[([^\]]*)\]\([^)]*\)/g;
const BLOCK_MARK = /^[ \t]*(?:#{1,6}|[-*+]|\d+\.)[ \t]+/gm;
const TICKS = /`+|\*\*|~~/g;
const EMPHASIS = /(^|[^\w*])[*_]([^*_\n]+)[*_](?![\w*])/g;
const SPACE = /\s+/g;
const HEADING = /^\s*\*\*([^*\r\n]+)\*\*[ \t]*\r?(?:\n|$)/;
const SENTENCE_END = /[.?!]["'”’)]?(?=\s)/;
const SENTENCE_END_FROM = /[.?!]["'”’)]?(?=\s)/g;

// thoughtPlain before its trim: whitespace runs are already one space.
function plainSteps(markdown: string): string {
  return markdown
    .replace(LINK, '$1')
    .replace(BLOCK_MARK, '')
    .replace(TICKS, '')
    .replace(EMPHASIS, '$1$2')
    .replace(SPACE, ' ');
}

// A thought as plain text: links keep their label; headings, list marks,
// emphasis and code ticks go.
export function thoughtPlain(markdown: string): string {
  return plainSteps(markdown).trim();
}

// The reference definition, over the whole text.
export function fullThoughtPreview(markdown: string): string {
  const heading = HEADING.exec(markdown)?.[1];
  const text = thoughtPlain(heading ?? markdown);
  if (heading !== undefined) return text;
  const end = SENTENCE_END.exec(text);
  return end ? text.slice(0, end.index + end[0].length) : text;
}

// Two converted pieces, the first ending at a newline: the space run across the
// join is one space.
function join(head: string, tail: string): string {
  return head.endsWith(' ') && tail.startsWith(' ') ? head + tail.slice(1) : head + tail;
}

// True while a link could still close after the end of `text`.
function linkOpen(text: string): boolean {
  return text.lastIndexOf('[') > text.lastIndexOf(']') || text.lastIndexOf('](') > text.lastIndexOf(')');
}

interface PreviewState {
  source: string;   // the final part of the thought, ending just after a newline
  plain: string;    // plainSteps(source)
  result: string | null; // the first sentence, once one ends inside `plain`
  text: string;     // the last text asked about, and its answer
  answer: string;
}

const states = new Map<string, PreviewState>();
const STATE_LIMIT = 64;
// The first piece converted; each later one at least doubles it, so a thought
// whose first sentence ends early never converts its whole length.
const FIRST_PIECE = 4096;

function firstSentenceFrom(text: string, lead: number, from: number): RegExpExecArray | null {
  SENTENCE_END_FROM.lastIndex = Math.max(lead, from);
  return SENTENCE_END_FROM.exec(text);
}

// `key` names the thought (ThinkingEvent.key); it only decides which state is
// reused, never the answer: a state is used only when the text still starts
// with the part it converted.
export function thoughtPreview(markdown: string, key?: string): string {
  const heading = HEADING.exec(markdown)?.[1];
  if (heading !== undefined) return thoughtPlain(heading);
  if (key === undefined) return fullThoughtPreview(markdown);
  let state = states.get(key);
  if (!state || !markdown.startsWith(state.source)) state = { source: '', plain: '', result: null, text: '', answer: '' };
  states.delete(key);
  states.set(key, state);
  while (states.size > STATE_LIMIT) states.delete(states.keys().next().value as string);
  if (state.result !== null) return state.result;
  if (state.text === markdown) return state.answer;

  const finalCut = markdown.lastIndexOf('\n') + 1;
  let want = Math.max(FIRST_PIECE, state.source.length * 2);
  while (state.source.length < finalCut) {
    let cut = want >= finalCut ? finalCut : markdown.lastIndexOf('\n', want - 1) + 1;
    if (cut <= state.source.length) cut = Math.min(finalCut, markdown.indexOf('\n', want) + 1);
    const source = markdown.slice(0, cut);
    if (linkOpen(source)) {
      if (cut === finalCut) {
        states.delete(key);
        return fullThoughtPreview(markdown);
      }
      want = cut * 2;   // a link spans this cut: take a longer piece
      continue;
    }
    const searchedTo = state.plain.length;
    state.plain = join(state.plain, plainSteps(markdown.slice(state.source.length, cut)));
    state.source = source;
    // `plain` ends with the space of the cut's newline, so every match that
    // starts inside it is decided by its own characters.
    const lead = state.plain.startsWith(' ') ? 1 : 0;
    const end = firstSentenceFrom(state.plain, lead, searchedTo - 3);
    if (end) {
      state.result = state.plain.slice(lead, end.index + end[0].length);
      return state.result;
    }
    want = cut * 2;
  }
  // No sentence has ended in the final part: the open last line decides.
  const text = join(state.plain, plainSteps(markdown.slice(state.source.length))).trim();
  const lead = state.plain.startsWith(' ') ? 1 : 0;
  const end = firstSentenceFrom(text, 0, state.plain.length - lead - 3);
  state.text = markdown;
  state.answer = end ? text.slice(0, end.index + end[0].length) : text;
  return state.answer;
}
