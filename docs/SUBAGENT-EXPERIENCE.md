# Dream subagent experience

This document is the implementation map for Workass's agent-facing
orchestration experience. Binding behavior lives in `PORT-SPEC.md`; this file
keeps the product sequence and acceptance receipts readable.

## Experience contract

The coordinator should be able to delegate without memorizing opaque provider
ids or babysitting one child at a time:

1. Call `workass_agent_catalog` and receive one normalized schema.
2. Spawn with only a task to inherit the current setup, with a scored profile,
   or with explicit catalog ids.
3. Inspect meaningful progress (`phase`, `latestActivity`, sequence, elapsed
   time), not merely `running`.
4. Send corrections between tool turns without losing them to an unsupported
   steering implementation.
5. Wait for first or all children and retain snapshots on timeout.
6. Retry a settled child with the exact prior setup plus new guidance.
7. Recover bounded, redacted receipts after renderer or daemon restarts.

## Personal model knowledge

Settings owns optional per-provider/base-model fields:

- Intelligence: integer 1–10.
- Taste: integer 1–10.
- Cost: integer 1–10, where a larger value means more expensive.
- Note: freeform user guidance, capped at 500 characters.

Workass never invents these values. Recommendation profiles rank only models
the user scored; unconfigured profiles fail with an actionable message. The
catalog returns both the raw user score and the resolved recommendation so the
agent can explain its choice.

## Delivery and recovery

Coordinator messages are appended to the child's durable FIFO before any
adapter operation. Confirmed live steer removes the queued copy. Unsupported
or interrupt-based adapters consume it as the immediate next prompt. An
uncertain acknowledgement is surfaced and never blindly duplicated.

Settled receipt records contain identity, parent/root lineage, resolved
selection, start/finish times, stop reason, retry linkage, and bounded result
or error text. Secret-shaped content is redacted before disk. Files are
per-chat JSONL with bounded record/file counts; the active in-memory list is
also bounded.

## Gates

- Catalog has one schema version and no duplicate legacy top-level model list.
- A task-only spawn inherits provider/model/effort/mode/cwd correctly.
- User-scored profiles choose deterministically; high cost reduces budget rank.
- Provider-neutral permissions resolve only to advertised native mode ids.
- Eight children can fan out, wait-first, receive feedback, then wait-all.
- Parent cancellation cascades and cannot leave an orphan engine.
- Retry preserves resolved selection and records lineage.
- Restarted daemon can list prior receipts without exposing another chat.
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
