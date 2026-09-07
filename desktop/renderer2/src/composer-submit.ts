export type ComposerSubmitIntent = 'send' | 'queue' | 'steer';
export type ComposerSubmitModifiers = { metaKey: boolean; ctrlKey: boolean };

// The user-facing law while a turn is running: ordinary Enter is a durable FIFO
// follow-up; Command+Enter on macOS or Ctrl+Enter elsewhere is an explicit
// attempt to change the active turn.
// Idle chats always send normally. Shift+Enter is handled by the textarea and
// never reaches this resolver.
export function composerSubmitIntent(running: boolean, modifiers: ComposerSubmitModifiers): ComposerSubmitIntent {
  if (!running) return 'send';
  return modifiers.metaKey || modifiers.ctrlKey ? 'steer' : 'queue';
}

// Explicit submission releases the editor. Even a later rejection must not
// insert the old text again or concatenate it with a new draft (user 2026-09-07).
// Keep the exported name for existing callers; only explicit typing/paste may
// populate the editor after submission.
export function restoreRejectedSteerDraft(rejectedDraft: string, currentDraft: string): string {
  void rejectedDraft;
  return currentDraft;
}
