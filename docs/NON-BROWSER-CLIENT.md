# Connecting a client that has no DOM

Written 2026-07-26 for the `workass-mobile` React Native client, but it applies
to any client that is not the injected browser shim. It answers six questions
asked from that repo and corrects two premises they rested on.

The authoritative sources stay what they were: `docs/PORT-SPEC.md` for the
binding laws, `internal/httpserve/lan_bridge.go` for the browser bridge, and
`internal/wire/wire.go` for the daemon. Where this file disagrees with those,
they win.

Vendor from commit `b5945cba` or later. Anything earlier has no fleet enrolment
and no machine book.

---

## 1 · LAN bind — exists, opt-in, off by default

`--bind lan` (default `localhost`), plus `--port`. Nothing binds `0.0.0.0`
unless a human says so, and `--prod` on Windows is the one place that defaults
it, because that machine is a reach-into machine by design.

Two things ride with it:

- `--beacon` (default on under `--bind lan`, `WORKASS_DAEMON_BEACON=0` to
  disable) announces the machine on `239.87.87.88:48788` so other daemons can
  find it. It never sweeps a subnet; it announces, and probes only the source
  address of a packet it received.
- Binding LAN now logs a warning naming exactly what is readable on that
  network. `secure: false` in the health card is truthful and stays false until
  E5 puts a certificate on the port. Badge it.

## 2 · Enrolment with no desktop UI — this is what E2 built

**A phone can complete enrolment alone. No desktop chrome exists for it, so no
mock is owed on my side.** The ceremony is: the human reads one fleet key off
whichever machine has it and types it into the phone, once, ever.

The key is shown by `workass fleet key` (mints on first ask; a daemon never
mints on start — a fresh one would otherwise invent a fleet of one behind your
back). Other daemons take it with `workass fleet join`, reading stdin, never
argv.

### The channels

Two additive channels, both callable by an **unapproved** client:

| channel | args | reply |
| --- | --- | --- |
| `fleet:challenge` | none | `{enabled, machineId, serverNonce, keyIds[], nonceTtlMs}` |
| `fleet:enroll` | `{clientNonce, proof, name}` | `{ok, deviceId, name, keyId, owner, machineId}` |

`enabled: false` means this daemon holds no fleet key — a different problem from
a wrong key, and the client should say so differently.

### The derivation, exactly

The key's canonical form is `wf-` + 26 lowercase base32 characters (RFC 4648
alphabet, **no padding**) encoding 16 random bytes. Normalisation accepts any
case, with spaces, dashes or underscores anywhere, so a human retyping it off a
screen is not punished.

```
raw          = base32_decode(uppercase(key without "wf-"))     # 16 bytes
message(ctx) = ctx ‖ 0x00 ‖ serverNonce ‖ 0x00 ‖ clientNonce ‖ 0x00 ‖ machineId ‖ 0x00
proof        = hex( HMAC-SHA256(raw, message("workass-fleet-proof-v1")) )
deviceToken  = hex( HMAC-SHA256(raw, message("workass-fleet-token-v1")) )
```

Note the trailing separator after the last field — every field gets one, so
moving a character across a boundary cannot produce the same input twice.
`clientNonce` is yours: 16 random bytes, hex. Both nonces travel as the strings
they are; do not re-encode them before hashing.

**The token is never sent, in either direction.** You compute it; the daemon
computes the same value and stores only its hash. That is what makes this safe
on a plaintext network, and it is why the enrol reply deliberately contains no
credential. Persist it under your per-machine key and connect with
`?deviceToken=<derived>` from then on.

Two implementation traps, both hit for real while proving this:

- Go writes base32 **unpadded**; most decoders require padding. Pad to a
  multiple of 8 with `=` before decoding.
- **Frames interleave.** The approval event arrives on the same socket *before*
  the reply to `fleet:enroll`. Demultiplex by `t`; a client that reads the next
  frame expecting its reply will mis-parse the event and hang.

### Failure behaviour

A wrong key is refused with `fleet:rejected` — "that key does not belong to this
fleet" — which says nothing about whether the key or the machine was wrong.
Each failure burns its challenge, so one nonce is one attempt; five failures on
one socket lock it until the client reconnects. Enrolment is broadcast on
`fleet:enrolled` and logged, so a key used by someone else announces itself.

Retiring a key (`workass fleet forget <keyId>`) closes future enrolments and
strands nobody: a device's token does not depend on the key that admitted it.

### Recognising your own machines

The public health card and each machine-book entry carry `fleetIds` — one-way
hashes of the keys that machine accepts. Matching one against a key you hold is
how a client decides to enrol silently rather than asking a human again.

**Match against `fleetIds`, never against the key id your own enrolment
returned, and treat any overlap as a match.** The distinction is not stylistic:

- `fleetIds` is CURRENT — it is `store.KeyIDs()`, refreshed on every health
  probe, and a machine mid-rotation legitimately advertises two.
- The `keyId` from `fleet:enroll`, and the `keyId` on a device record, are
  HISTORICAL. They record which key admitted that device, deliberately, so that
  retiring a key can be told apart from revoking a device (`lease.go:26-29`).
  Neither is ever rewritten.

They agree until the day someone rotates. `fleet rotate` then `fleet forget`
strands nobody — an enrolled device keeps working, because its token does not
depend on the key that admitted it — so the client reconnects on its stored
token, never enrols again, and never learns the new id. A client filtering the
book by its remembered key id then matches nothing and silently stops seeing
every machine in the fleet. Filtering by the daemon's advertised `fleetIds`
survives the rotation with no client change at all.

This is also why `lan:access-state` does not carry `keyId` and should not: the
only value the daemon could put there is the historical one, so the field would
look authoritative while being exactly the value that goes stale.

## 3 · The reconnect generation — the premise to correct

**The generation is not daemon state and never was.** `socketGen` is a counter
inside the client, incremented in `connect()` before the socket even opens
(`lan_bridge.go:76`). The daemon never sees it and has nothing to send.

The DOM event exists only because the browser bridge lives in an injected script
context and the store lives in another; `window.dispatchEvent` is how it crosses
that boundary. React Native has no such boundary — your transport and your store
are the same JS context, so `onSocketOpen` as already written in your
`docs/m0/transport.ts` is the whole answer. Nothing is missing.

**But there is a real gap underneath the question, and it is daemon knowledge.**
A client-side counter cannot tell "reconnected to the same daemon" from
"reconnected to a daemon that restarted underneath me" — and those need
different recoveries, because the second one lost every engine and every piece
of in-memory session state. So:

Every socket now receives, on the `lan:access-state` event it already gets the
moment it opens — in **all three** outcomes, approved, waiting and rejected:

```jsonc
{
  "state": "approved",
  "machineId": "m-…",     // which machine answered; matches the health card
  "instanceId": "i-…"     // this daemon RUN. Changes only across a restart.
}
```

No new frame type, no new channel, additive fields on an existing payload. The
rule to implement: bump your generation on every open as you already planned,
and additionally treat *"`instanceId` differs from the one I saw last connect"*
as a full resync rather than a reconciliation.

## 4 · The two-phase gate

Your reading is right, with one correction about where the deadlock lives.

- `open` — the socket handshook. Client-side fact.
- `ready` — the daemon approved this device, signalled by `lan:access-state`
  with `state: "approved"`.

Invokes issued while merely `open` are queued client-side. The exceptions are
the channels that *produce* an approval, which must go out before one exists:

```
lan:pairing-info   fleet:challenge   fleet:enroll
```

That set is now `PRE_READY_CHANNELS` in `lan_bridge.go`; mirror it. Queue
`fleet:challenge` and you have built the deadlock you were worried about — the
client waits for approval that only enrolment can produce, and enrolment is
sitting in the queue waiting for approval.

The correction: **the daemon never deadlocks.** It answers every invoke from an
unapproved client with `lan:access-pending` or `lan:access-rejected`. The queue
is entirely the client's own policy, so the failure is yours to avoid and yours
to test.

Localhost is auto-approved server-side when `--trust-localhost` is on (the
default); a LAN client never is.

## 5 · Error taxonomy

Two families that must not be confused, because the store's connection monitor
keys off the difference.

**Transport-lifecycle errors — minted by the client, never by the daemon.**
`name: 'WorkassInvokeError'`, with `code` one of:

```
socket-replaced   a newer connect() superseded this socket
socket-closed     the socket closed with invokes in flight
invoke-timeout    no reply inside the channel's budget (30s; 120s for session:*)
```

The store discriminates on the **name alone** (`store.ts:3564`), not the code.
The codes are yours; keep them for your own logs and tests.

**Daemon errors — a string in `reply.error`, surfaced as a plain `Error`.**
Never as a `WorkassInvokeError`, or a rolling old daemon that lacks an additive
channel will flap the client offline. Most are a JSON document:
`{"code":…,"message":…,…extra}`.

| code | when | extra fields |
| --- | --- | --- |
| `lan:access-pending` | invoke before approval | `requestId`, `state` |
| `lan:access-rejected` | invoke after refusal | `state`, `reason` |
| `lan:not-controller` | a mutating channel from a non-controller | `deviceId`, `controllerDeviceId`, `controllerName` |
| `fleet:no-key` | this daemon holds no fleet key | — |
| `fleet:rejected` | proof did not match any key here | — |
| `fleet:challenge-expired` | no live challenge; ask again | — |
| `fleet:malformed` | `clientNonce`/`proof` missing | — |
| `fleet:locked` | five failed attempts on this socket | — |

One error is **not** JSON: an unknown channel returns the bare string
`unknown channel: <name>` (`wire.go:295`). Parse defensively — a non-JSON
`reply.error` is an ordinary channel failure, not a protocol violation.

## 6 · Push — nothing forecloses it

The shape described from `workass-mobile` works and nothing here blocks it:

- Connect query params are read in `newClient` (`wire.go:651`) and unknown ones
  are ignored, so `pushToken=…` lands additively beside `deviceName`, exactly
  as described.
- Nothing logs the request URI, so a token in the query string does not reach
  the daemon log today. That is a property worth a test on my side before the
  token is real — say the word when M3 starts and I will add one.
- `go.mod` has an empty require block and every phase so far has kept it that
  way. APNs from stdlib is consistent with that.

One caveat to design around rather than discover: a query string is the wrong
place for a long-lived secret in general — it is the existing debt that
`deviceToken` already carries, recorded in the plan and paid off by E5. If push
tokens follow the same path, they inherit the same debt. Fine for now, worth a
line in your SPEC.

---

## What is owed, and by whom

- **Me:** TLS (E5) — the badge stays `secure: false` until then. A test that
  proves connect-query secrets never reach a log. E3's per-machine client
  transport on the desktop side.
- **You:** the `PRE_READY_CHANNELS` mirror, the `instanceId` resync rule, and
  the base32 padding. All three are cheap and all three fail silently if missed.
- **The user:** whether a daemon binds LAN at all. It is his network.
