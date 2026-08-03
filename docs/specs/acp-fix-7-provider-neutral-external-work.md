# Fix 7 — provider-neutral external work and handoff wake (closed spec)

Status: approved by the user on 2026-07-20 before the combined production
promotion. Scope: `internal/acp`, `cmd/workass`, and binding documentation/tests.
No renderer or wire-protocol changes.

## Problem

The production handoff watcher is registered through
`workass_register_external_work`. The tool is injected into every ACP, but the
daemon rejects a Codex-owned chat with:

```
external work registration is only available for Claude-provider Workass chats
```

Consequently the handoff has no durable completion record and cannot wake the
Codex conversation after the daemon replacement. The user must intervene to
resume it.

## Binding behavior

- Passive observation of provider-native Bash/Agent/Workflow events remains
  Claude-specific and unchanged.
- Explicit external-work registration is provider-neutral. Any authenticated
  ACP owner bound to a non-empty provider id may register and settle work.
- Production still rejects the `mock` fixture provider. Dev/test may use it.
- Existing owner, path, redaction, receipt, no-pin, persistence, and wake rules
  remain unchanged.
- The environment brief and MCP tool description must state the same
  provider-neutral tracking law. They must not teach a Claude-only exception.
- A registered Codex external lane must survive a daemon-manager restart,
  settle from its done marker, dispatch one durable wake, and persist
  `wake=delivered` exactly like a Claude lane.

## Regressions

1. A Codex-owned external registration fails on the old code, then succeeds,
   persists, reloads, settles from a done marker, and invokes the wake hook once.
2. Production continues rejecting a mock-owned external registration.
3. The injected environment brief and advertised MCP tool contract describe
   all ACP providers and contain no Claude-only qualification.
4. Existing external-work, wake, ownership, path, hibernation, and race suites
   remain green.

## Gates

1. Focused fail-before/pass-after tests and focused race test.
2. `go test ./internal/acp ./cmd/workass` and `go vet` for both packages.
3. Blessed dev daemon rebuild and healthy runtime receipts with production
   processes unchanged.
4. Combined production promotion only after the dev runtime is healthy.
