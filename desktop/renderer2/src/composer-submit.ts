export type ComposerSubmitIntent = 'send' | 'queue' | 'steer';

// The user-facing law while a turn is running: ordinary Enter is a durable FIFO
// follow-up; Command+Enter is an explicit attempt to change the active turn.
// Idle chats always send normally. Shift+Enter is handled by the textarea and
// never reaches this resolver.
export function composerSubmitIntent(running: boolean, metaKey: boolean): ComposerSubmitIntent {
  if (!running) return 'send';
  return metaKey ? 'steer' : 'queue';
}
