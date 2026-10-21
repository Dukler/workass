import { createContext } from 'react';

// The open chat-find query ('' when find is closed). A rendered HTML page shows
// its source while the query matches inside it, so the existing
// [data-chat-find-text] ranges can highlight the match (t3code reveals a
// Mermaid block's source the same way, MarkdownMermaidCodeBlock.tsx:28-29).
export const ChatFindQueryContext = createContext('');
