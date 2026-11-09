# Dream subagent experience

This document is the implementation map for Workass's agent-facing
orchestration experience. The behavior is described in `PORT-SPEC.md`; this file
keeps the product sequence and acceptance receipts readable.

## Experience contract

The coordinator should be able to delegate without memorizing opaque provider
ids or babysitting one child at a time:

1. Call `workass_agent_catalog` and receive one normalized schema.
2. Spawn with only a task to inherit the current setup, or with explicit
   catalog ids.
3. Inspect meaningful progress (`phase`, `latestActivity`, sequence, elapsed
   time), not merely `running`.
4. Send corrections between tool turns without losing them to an unsupported
   steering implementation.
5. Wait for first or all children and retain snapshots on timeout.
6. Retry a settled child with the exact prior setup plus new guidance.
7. Recover bounded, redacted receipts after renderer or daemon restarts.

## Delivery and recovery

Coordinator messages are appended to the child's durable FIFO before any
adapter operation. Confirmed live steer removes the queued copy. Unsupported
or definitely rejected steering leaves that same copy as the immediate next
prompt without interrupting the child. An uncertain acknowledgement is
surfaced and never blindly duplicated.

Settled receipt records contain identity, parent/root lineage, resolved
selection, start/finish times, stop reason, retry linkage, and bounded result
or error text. Secret-shaped content is redacted before disk. Files are
per-chat JSONL with bounded record/file counts; the active in-memory list is
also bounded.

## Background work a child started

A child that starts a background shell, Monitor or native agent and ends its turn
is waiting, not done (`internal/acp/subagent_background.go`). Its row reads
`waiting_background` with the work named, and a parent's wait keeps waiting. The rail
shows it as Trabajando, since no label of its own has an approved mock. Claude
Code wakes it when the work ends; real Codex never starts a turn by itself, so the
daemon sends one prompt after a 10-second grace. Codex reports a command left
running only by its item staying in progress past `turn/completed`, which the host
publishes as `_workass_codex_spawned_work`. A receipt's `backgroundStopped` names
what a closing session stopped, and the notice carries it as
`<background-stopped>`. A service alone leaves the run `done`; anything it owed
makes it `failed`. Lanes started with `workass run` belong to the parent chat, keep
running, and report to it themselves, so they never hold a child open.

## Gates

- Catalog has one schema version and no duplicate legacy top-level model list.
- A task-only spawn inherits provider/model/effort/mode/cwd correctly.
- Provider-neutral permissions resolve only to advertised native mode ids.
- Eight children can fan out, wait-first, receive feedback, then wait-all.
- Parent cancellation cascades and cannot leave an orphan engine.
- Retry preserves resolved selection and records lineage.
- Restarted daemon can list prior receipts without exposing another chat.
- A child's background work finishes and is read before the parent is told;
  any close over live work reads as lost, never as plain `completed`.
- Results/errors/notes are bounded and secret-redacted.
- MCP stdout remains JSON-RPC only.

## Attachment responsiveness (same release gate)

Attachment selection is not orchestration, but it shares the coordinator's
critical interaction path. Selecting or pasting several large images must:

- create zero-copy object-URL previews in the same UI commit;
- never wait for cold ACP session startup before showing those previews;
- never base64-encode or serialize the chat during attachment add;
- keep the draft owned by its stable chat across tab switches;
- encode sequentially only at send time and release object URLs on removal,
  acceptance, or chat close.

The deterministic benchmark uses six synthetic 24 MiB images and requires
preview preparation under 25 ms with zero file reads before send.
