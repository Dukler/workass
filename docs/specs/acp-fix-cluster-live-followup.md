# ACP lifecycle/delivery cluster — live follow-up

Status: USER-REPORTED LIVE FAILURE; APPROVED FOR DEV IMPLEMENTATION BY THE
2026-07-18 CONTINUATION OF THE ORIGINAL “do it” REQUEST.

## Verified gaps after the first production daemon promotion

1. The production daemon was promoted after commit `d71abfa`, but the installed
   Electron renderer was not rebuilt or repackaged. The running shell still
   serves `/Applications/Workass.app/Contents/Resources/renderer`, whose asset
   hashes predate the renderer projection fix.
2. The installed Claude adapter's real catalog canary still advertises
   `claude-fable-5[1m]` with its model-specific effort axis. A resumed live
   session can subsequently replace that authoritative provider catalog with a
   partial/stale `claude-fable-5` view.
3. Existing chat state already contains degraded selections such as
   `claude-fable-5[high]`. The remembered per-model controls still contain the
   original literal `claude-fable-5[1m]` key and effort, but the first fix does
   not use that evidence to repair legacy state.
4. The new relative-time clock ticks every 30 seconds while the visible label
   remains `hace unos segundos` for the first minute. The user requires a
   visibly fresh timestamp.

## Binding behavior

- A successful provider catalog probe is authoritative for Claude model IDs.
  Later live-session catalog updates are partial capability updates: they may
  update efforts/modes and add genuinely new IDs, but may not replace a probed
  literal context model with its degraded root alias. Missing probed Claude
  models remain until the next explicit provider detection rebuilds the
  authoritative catalog.
- Legacy selection repair is evidence-based, never guessed. If the current
  selection no longer resolves, Workass may switch it to a literal bracketed
  catalog variant only when:
  1. the degraded selection's canonical effort suffix peels to that variant's
     exact root;
  2. the bracket is not a Workass effort;
  3. the chat's per-model memory contains that exact literal ID; and
  4. exactly one catalog model satisfies all three conditions.
  The remembered compatible effort and mode are retained and the repaired
  controls are persisted immediately.
- The compact model selector visibly identifies provider and any literal
  context qualifier (for example `Fable 5 · Claude · 1M`). Its title contains
  the exact provider/model selection for unambiguous inspection. Effort remains
  the existing adjacent control.
- Settled-turn timestamps under one minute display elapsed seconds and share
  one one-second clock. Only the leaf stamp re-renders.
- Production activation requires packaging/installing the renderer, not merely
  relaunching the already-installed app. The daemon PID must remain unchanged
  during renderer packaging/activation.

## Regression gates

- Go: partial Claude live catalog containing the degraded root retains the
  probed literal context ID, transfers the live effort axis to it, preserves
  other missing probed IDs, and does not emit the degraded alias.
- Renderer: a degraded selection plus one exact remembered literal variant
  repairs to that variant; ambiguity or missing memory performs no repair.
- Renderer: model selector identity exposes provider/context and never treats a
  canonical effort suffix as a context qualifier.
- Renderer: second/minute/hour/day timestamp boundaries are deterministic.
- Existing affected package tests, renderer typecheck/build, isolated dev
  activation, then production package/install and post-activation health.
