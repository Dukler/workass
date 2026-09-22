# Devin one-action stop and send

Authority: the user's 2026-09-22 report that Devin requires queueing a message
before the steering shortcut, followed by authorization to land and publish
the proposed one-action stop-and-send fix. This is an explicit exception to
PORT-SPEC §3.13–14 for the registered Devin delivery strategy.

The official Devin 3000.11.1 isolated initialize canary advertises no live
steering capability; `_session/steer` returns JSON-RPC -32601. Do not advertise
or manufacture native live steering. Instead expose a separate `stopAndSend`
delivery capability when the Devin attachment lacks real live steering.

One Ctrl/Cmd+Enter or composer button action with text durably queues that
message (including prepared attachments) through the existing exact-chat queue
command, then stops only the captured foreground turn through the existing
cancel command. Preserve ordinary FIFO ordering and target selection. No
cancel before a matching durable queue receipt; failed/uncertain persistence
keeps the one queued owner and reports the failure without stopping. Never
cancel a replacement turn, another chat, or a removed queue entry. A failed
Stop leaves the durable input queued. Do not automatically retry Stop.

The composer labels this action “Detener y enviar”. Empty shortcut remains
Stop; ordinary Enter remains queue. Providers with live steering retain their
existing direct path, and other unsupported providers remain unsupported.
Provider names stay confined to registrations; UI consumes typed capabilities.
Wire channels and invoke/reply/event frames remain unchanged.

## Lane manifest

- `internal/provider/contract.go`
- `internal/acp/provider_delivery.go`, `internal/acp/provider_registration.go`
- `internal/acp/types.go`, `internal/acp/provider_delivery_capabilities_test.go`
- `cmd/workass/provider_chat_projection.go`
- `cmd/workass/provider_chat_runtime_test.go`, `desktop/acp/mock-server.mjs`
- `desktop/renderer2/src/wire/types.ts`, `desktop/renderer2/src/steering.ts`
- `desktop/renderer2/src/store/store.ts`, `desktop/renderer2/src/components/Composer.tsx`
- `desktop/renderer2/tests/steer-queue-lifecycle.test.ts`
- `docs/PORT-SPEC.md`, `desktop/acp/README.md`, this spec
- Generated `cmd/workass/embedded/dist/` after source preparation

Acceptance: capability projection and genuine live-steer precedence; exactly
one queue receipt before one exact cancellation; persistence failure/mismatch;
turn/chat replacement and queue removal races; attachment and newer-draft
preservation; no effect on native live-steer lanes. Mock ACP is the behavior
oracle. Validate both dev process boundaries before the canonical publication.
