export type ComposerSubmitIntent = 'send' | 'queue' | 'steer';
export type ComposerSubmitModifiers = { metaKey: boolean; ctrlKey: boolean };
type ComposerKey = ComposerSubmitModifiers & { key: string; shiftKey: boolean };
export type ComposerKeyAction = ComposerSubmitIntent | 'next' | 'previous' | 'pick' | 'dismiss';

// The empty composer button is the explicit Stop control. Keyboard steering
// never becomes Stop, and a second click in the same pointer sequence must not
// hit Stop after the first click clears a submitted direction.
export function composerButtonAction(running: boolean, hasText: boolean, clickCount: number): ComposerSubmitIntent | 'stop' | null {
  if (clickCount > 1) return null;
  return running ? (hasText ? 'steer' : 'stop') : 'send';
}

// Explicit submission wins over autocomplete. A catalog suggestion must never
// consume the first steering shortcut or make the user submit twice.
export function composerKeyAction(running: boolean, event: ComposerKey, popupOpen: boolean): ComposerKeyAction | null {
  if (event.key === 'Enter' && !event.shiftKey && (event.metaKey || event.ctrlKey)) {
    return composerSubmitIntent(running, event);
  }
  if (popupOpen) {
    if (event.key === 'ArrowDown') return 'next';
    if (event.key === 'ArrowUp') return 'previous';
    if (event.key === 'Tab' || (event.key === 'Enter' && !event.shiftKey)) return 'pick';
    if (event.key === 'Escape') return 'dismiss';
  }
  return event.key === 'Enter' && !event.shiftKey ? composerSubmitIntent(running, event) : null;
}

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
