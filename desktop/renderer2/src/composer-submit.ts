export type ComposerSubmitIntent = 'send' | 'queue' | 'steer';

// The user-facing law while a turn is running: ordinary Enter is a durable FIFO
// follow-up; Command+Enter is an explicit attempt to change the active turn.
// Idle chats always send normally. Shift+Enter is handled by the textarea and
// never reaches this resolver.
export function composerSubmitIntent(running: boolean, metaKey: boolean): ComposerSubmitIntent {
  if (!running) return 'send';
  return metaKey ? 'steer' : 'queue';
}

// A definite steer rejection returns ownership of that exact input to the
// composer. The rejected text predates anything typed while provider admission
// was pending, so keep that chronological order without overwriting either
// draft. An exact duplicate is already restored and must not be inserted twice.
export function restoreRejectedSteerDraft(rejectedDraft: string, currentDraft: string): string {
  if (!rejectedDraft) return currentDraft;
  if (!currentDraft || currentDraft === rejectedDraft) return rejectedDraft;
  return `${rejectedDraft}\n\n${currentDraft}`;
}
