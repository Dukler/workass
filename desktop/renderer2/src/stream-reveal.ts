// While a reply streams, the transcript shows its text only up to the last
// finished piece (approved demo stream-v2 · "frases", 2026-10-12): a finished
// sentence, list item, code line, table row (once the table exists), or a
// whole block. Half-written markdown never renders, so nothing flips type or
// shrinks mid-stream, and each new piece fades in once (CSS, opacity only).

const TABLE_SEP = /^\|[-| :]+\|\s*$/;
const HEADING = /^[ \t]{0,3}#{1,6}[ \t]/;
const FENCE = /^[ \t]{0,3}(```|~~~)/;
// Sentence end: terminal punctuation (optionally closed by a quote or bracket)
// followed by whitespace. "3.5" or "file.md" never qualify.
const SENTENCE_END = /[.!?:…]["'”’)\]]*(?=\s)/g;

/** Length of the prefix of `text` that is safe to show while it streams. */
export function revealCut(text: string): number {
  const lines = text.split('\n');
  let cut = 0;
  let pos = 0;
  let inFence = false;
  let tableOpen = false;
  for (let index = 0; index < lines.length; index++) {
    const line = lines[index];
    const complete = index < lines.length - 1;
    const end = pos + line.length + 1;
    if (FENCE.test(line)) {
      inFence = !inFence;
      if (complete) cut = end;
    } else if (inFence) {
      if (complete) cut = end;
    } else if (line.trim() === '') {
      if (complete) cut = end;
      tableOpen = false;
    } else if (line.trimStart().startsWith('|')) {
      if (TABLE_SEP.test(line)) tableOpen = true;
      else if (complete && tableOpen) cut = end;
    } else if (HEADING.test(line)) {
      if (complete) cut = end;
    } else {
      // A paragraph line or a list item: every finished sentence in it, and
      // the whole line once it ends. A marker with no finished sentence yet
      // ("- the raw fl") stays hidden.
      SENTENCE_END.lastIndex = 0;
      let match: RegExpExecArray | null;
      while ((match = SENTENCE_END.exec(line))) cut = Math.max(cut, pos + match.index + match[0].length);
      if (complete) cut = end;
    }
    pos = end;
  }
  return Math.min(cut, text.length);
}

export function revealedText(text: string): string {
  return text.slice(0, revealCut(text));
}
