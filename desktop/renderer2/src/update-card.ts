// Footer update-card lifecycle. The card used to vanish the instant the daemon
// re-emitted `providers:updates` with nothing left to update, so a successful
// update had no visible resolution — it just blinked out mid-spin. This models
// the small display state machine that lets the card seal into a green "done"
// state, hold briefly, then slide out, without leaking timing rules into React.

export type UpdatePhase = 'hidden' | 'resting' | 'running' | 'done' | 'exiting';

// Observable, time-independent signals derived from the store each render.
//  - active:  a success-path update is in flight (a CLI is streaming, or the
//             sequential chain is still walking between CLIs without a failure).
//  - pending: how many CLIs still advertise an available update.
export interface UpdateSignals {
  active: boolean;
  pending: number;
}

// How long the sealed "done" card lingers before it begins sliding away, and how
// long the slide-out itself runs. The green border seal is timed to the hold so
// the ring finishes filling exactly as the card starts to leave.
export const DONE_HOLD_MS = 500;
export const EXIT_MS = 340;

// Model-family brand ('gpt' | 'claude' | '') for a provider id, so the footer
// card can show the same brand mark the subagent rows use. Mirrors the daemon's
// brandForProvider (internal/acp/bridge.go) so the two never drift; '' means no
// brand mark (e.g. qwen), and ModelIcon renders nothing.
export function brandForProvider(providerId: string | null | undefined): 'gpt' | 'claude' | '' {
  const s = String(providerId ?? '').toLowerCase();
  if (s.includes('codex') || s.includes('gpt') || s.includes('openai')) return 'gpt';
  if (s.includes('claude') || s.includes('anthropic') || s.includes('opus') ||
      s.includes('sonnet') || s.includes('haiku') || s.includes('fable')) return 'claude';
  return '';
}

// Next data-driven phase given the previous phase and the current signals. This
// is deliberately idempotent: `done` and `exiting` are terminal-until-timer, so
// re-evaluating them with unchanged signals holds them in place while the
// component's timers advance done → exiting → hidden. Failure is handled by the
// component ahead of this machine (its own card), so it never appears here.
export function nextUpdatePhase(prev: UpdatePhase, s: UpdateSignals): UpdatePhase {
  // An in-flight update always wins — including a restart after the card had
  // sealed/left, which re-enters running from any prior phase.
  if (s.active) return 'running';
  // We just left the active state without a failure: seal to done only when the
  // work is truly finished (nothing still pending); otherwise fall back to a
  // resting card for whatever remains.
  if (prev === 'running') return s.pending === 0 ? 'done' : 'resting';
  // Hold the sealed/leaving card until the component's timers move it on.
  if (prev === 'done' || prev === 'exiting') return prev;
  // Nothing in flight and nothing sealed: show the resting card iff work remains.
  return s.pending > 0 ? 'resting' : 'hidden';
}
