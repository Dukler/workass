import { busyEnterIntent, type WhileWorking } from './general-settings.ts';

export type ComposerSubmitIntent = 'send' | 'queue' | 'steer';
// Ajustes · General: what plain Enter does while a turn runs, and whether the
// running chat's agent can be steered at all.
export type ComposerBusyPreference = { whileWorking: WhileWorking; canSteer: boolean };
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
export function composerKeyAction(running: boolean, event: ComposerKey, popupOpen: boolean, busy?: ComposerBusyPreference): ComposerKeyAction | null {
  if (event.key === 'Enter' && !event.shiftKey && (event.metaKey || event.ctrlKey)) {
    return composerSubmitIntent(running, event, busy);
  }
  if (popupOpen) {
    if (event.key === 'ArrowDown') return 'next';
    if (event.key === 'ArrowUp') return 'previous';
    if (event.key === 'Tab' || (event.key === 'Enter' && !event.shiftKey)) return 'pick';
    if (event.key === 'Escape') return 'dismiss';
  }
  return event.key === 'Enter' && !event.shiftKey ? composerSubmitIntent(running, event, busy) : null;
}

// The user-facing law while a turn is running: ordinary Enter does what
// Ajustes · General chose — by default a durable FIFO follow-up — and
// Command+Enter on macOS or Ctrl+Enter elsewhere does the other one. With
// "Dirigir" chosen, a chat whose agent cannot be steered still queues on Enter.
// Idle chats always send normally. Shift+Enter is handled by the textarea and
// never reaches this resolver.
export function composerSubmitIntent(running: boolean, modifiers: ComposerSubmitModifiers, busy?: ComposerBusyPreference): ComposerSubmitIntent {
  if (!running) return 'send';
  return busyEnterIntent(busy?.whileWorking ?? 'queue', modifiers.metaKey || modifiers.ctrlKey, busy?.canSteer ?? true);
}

// Explicit submission releases the editor. Even a later rejection must not
// insert the old text again or concatenate it with a new draft (user 2026-09-07).
// Keep the exported name for existing callers; only explicit typing/paste may
// populate the editor after submission.
export function restoreRejectedSteerDraft(rejectedDraft: string, currentDraft: string): string {
  void rejectedDraft;
  return currentDraft;
}
