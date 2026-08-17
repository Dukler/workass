export interface ChatFindShortcutEvent {
  key: string;
  metaKey: boolean;
  ctrlKey: boolean;
  altKey: boolean;
  shiftKey: boolean;
}

export interface TextMatchOffset {
  start: number;
  end: number;
}

export interface SearchableChatMessage {
  id: string;
  content: string;
  result?: string;
}

export interface ChatFindMatch {
  messageId: string;
  messageIndex: number;
  occurrence: number;
}

export function isChatFindShortcut(event: ChatFindShortcutEvent): boolean {
  return (event.metaKey || event.ctrlKey)
    && !event.altKey
    && !event.shiftKey
    && event.key.toLowerCase() === 'f';
}

export function findMatchOffsets(text: string, query: string): TextMatchOffset[] {
  if (!query) return [];
  let haystack = '';
  const starts: number[] = [];
  const ends: number[] = [];
  let sourceOffset = 0;
  for (const character of text) {
    const start = sourceOffset;
    sourceOffset += character.length;
    const folded = character.toLowerCase();
    haystack += folded;
    for (let index = 0; index < folded.length; index++) {
      starts.push(start);
      ends.push(sourceOffset);
    }
  }
  const needle = [...query].map((character) => character.toLowerCase()).join('');
  if (!needle) return [];
  const matches: TextMatchOffset[] = [];
  let from = 0;
  while (from <= haystack.length - needle.length) {
    const start = haystack.indexOf(needle, from);
    if (start < 0) break;
    const original = { start: starts[start], end: ends[start + needle.length - 1] };
    const previous = matches[matches.length - 1];
    if (!previous || previous.start !== original.start || previous.end !== original.end) matches.push(original);
    from = start + Math.max(1, needle.length);
  }
  return matches;
}

export function findChatMessageMatches(messages: readonly SearchableChatMessage[], query: string): ChatFindMatch[] {
  if (!query) return [];
  const matches: ChatFindMatch[] = [];
  messages.forEach((message, messageIndex) => {
    let occurrence = 0;
    for (const part of [message.content, message.result ?? '']) {
      for (const _match of findMatchOffsets(part, query)) {
        matches.push({ messageId: message.id, messageIndex, occurrence });
        occurrence += 1;
      }
    }
  });
  return matches;
}

export function nextFindIndex(current: number, count: number, direction: 1 | -1): number {
  if (count <= 0) return 0;
  return (Math.max(0, current) + direction + count) % count;
}
