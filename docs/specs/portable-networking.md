# Portable networking: one app, any network

Status: DESIGN v3, after two contrarian reviews. Nothing here is built.
Owner decisions are listed in §9.

## 1. Goal

Install one artifact, open it, and it works on any machine and any network, with
nothing else to run: no reverse proxy, VPN, domain, SSH tunnel, port forward or
account.

- **G1** One machine: works offline from the first launch.
- **G2** Same network: machines on default ports find each other with no
  configuration. A custom port is typed once at first pairing.
- **G3** Different networks: machines connect after one pairing, through NAT,
  with no third-party product.
- **G4** The certificate never needs a human, including at expiry.
- **G5** Hardened hosts: works with TCP only, one port, no admin rights, no UDP,
  and on a side that can accept no inbound connection.
- **G6** No security regression: end-to-end pinned TLS, nothing in the middle can
  read or forge traffic, no central account.

Binding constraints: frozen `invoke/reply/event` wire; Go stdlib-first with no new
third-party dependency unless granted; no npm on Windows; PORT-SPEC network and
discovery laws (2026-07-10, 2026-09-09, 2026-09-25); no feature may be lost.

## 2. What exists today

Already inside the binary or bundle: HTTPS/WSS server, certificate generation, UI
hosting, SQLite (pure Go), Node and agent adapters (checksum-pinned), fleet
authentication and ordered routes. Workass has never used Caddy, and nothing here
needs it.

The gaps:

- **F1 Discovery sees only port 80.** `machinebook.Scanner` probes TCP 80 on the
  local /24. macOS and Linux default to 8788, so those daemons stay invisible until
  someone types an address. `machinebook.Beacon` runs only in tests, although
  `NON-BROWSER-CLIENT.md` §1 and `fleet-runtime.md` §4 describe it as live.
- **F2 The desktop client refuses most addresses.** `privateLANHost` allows only
  RFC1918 IPv4 and loopback names, so overlay, public and global IPv6 addresses
  all fail. The Go mesh has no such limit.
- **F3 Pins are one per host.** Electron's verify callback gets a hostname and no
  port, so two daemons behind one host overwrite each other.
- **F4 The certificate has no renewal.** It lasts 10 years. After expiry the daemon
  fails its readiness probe and exits, and the shell forgives only authority and
  name errors.
- **F5 Replacing the certificate unpairs every client.** The pin covers the whole
  certificate.
- **F6 Nothing crosses NAT.**
- **F7 Trust is weaker than the design assumed.** See §3.
- **F8 No listener hardening.** The HTTP server sets no timeouts, pending approvals
  are uncapped, lockout is per connection, and `0.0.0.0` is dual-stack in Go, so a
  LAN bind can also be reachable on global IPv6.

## 3. P0: trust before reach

Every later layer widens who can reach a daemon, so this comes first and ships on
its own.

- Every client verifies the certificate proof **before** sending any enrolment
  proof, on the renderer, the Go mesh and the phone. This is the fix. A missing
  fingerprint or a missing proof for the key in use means no proof is sent. On a
  daemon holding several keys, a proof missing for one key skips that key and is
  never treated as a wrong identity, so stripping cannot permanently refuse a route.
- A client that cannot see the presented certificate (a plain browser) sends no
  fleet proof and uses human pairing, because a replayed proof would pass there.
- Enrolment proofs also bind to the certificate fingerprint, as defence in depth
  only: daemons keep accepting old clients, so old clients stay exposed until they
  update.
- The machine book ignores a changed fingerprint until it has been proved. An
  unproved card cannot change a pin or clear stored credentials.
- Listener hardening: only `ReadHeaderTimeout` and `IdleTimeout`, because read and
  write timeouts would cut slow media responses and long tool calls. Caps count only
  unauthenticated connections and pending approvals. Per-source limits on enrolment
  never key on a shared relay marker.
- One locality helper replaces the scattered `isLocalIP` checks. An empty or
  unparseable address is **not** local, and the connection context records whether
  a connection came through a relay. Every current gate moves to it.
- From a non-private source, the identity card shrinks to what a key holder needs,
  and human-approval pairing is refused.

## 4. Certificate lifecycle (G4)

Keep the private key, reissue the certificate around it, and move pins from the
whole certificate to its public key.

- At startup, if under one year remains, reissue with the same key and the original
  raw subject, and write atomically. Startup-only is deliberate: a one-year window
  covers any realistic uptime, every update restarts the daemon, and it avoids
  swapping identity under live handshakes.
- Additive fields: `certKeyFingerprint` on the card, and `certKeyProofs` alongside
  `certProofs`.
- Clients pin the key whenever a whole-certificate pin verifies. Old clients keep
  working until the first reissue, roughly nine years after this ships.
- A verifier holding a matching key pin ignores expiry. A missed window therefore
  cannot lock anyone out, and the readiness probe never blocks startup on it.
- Key replacement after a compromise stays a deliberate, visible ceremony. Key
  reuse follows the SSH host-key model, and rotation without revocation adds little.

## 5. Address trust (F2, F3)

Two verifier policies replace one:

- **Default session (the Workass UI):** accept exactly when the presented key
  matches a pin for that host, for any address class. Proof arrives inside TLS, so a
  new pin starts provisional, and no credential is released until the proof verifies.
- **In-app browser partition:** keeps today's private-only gate, because it holds
  an agent's site sign-ins. A book entry must never relax TLS for a public site
  there.

This alone makes overlay and VPN addresses work, with no embedded VPN. Public
addresses stay behind §9.3. The renderer also gains the route ladder that only the
Go mesh has today, so it falls back instead of stopping at the first address.
Refused routes expire or scope per network, because RFC1918 addresses collide
across networks.

## 6. Discovery by network type (F1)

A network is **associated** only while a stored peer completes a pinned handshake
on it right now. Subnets collide and cards can be forged, so neither counts.

| Network | Behaviour |
|---|---|
| Associated | Today's automatic sweep and cadence, plus multicast announce/listen, which also carries custom ports |
| Unassociated | No automatic sweep or multicast. Sweep only while the Machines pane is open |
| Windows production profile | Unchanged, under the 2026-09-25 law |

Sweeps probe the default ports `{80, 8788}`. Connected peers also gossip their
current addresses over authenticated links, which needs no scanning and handles
roaming. A headless daemon that has just joined a fleet by key runs one bounded
sweep, since nobody will ever open its Machines pane. UDP is an opportunistic addition, never required (G5). This amends the
2026-07-10 and 2026-09-09 laws, so it waits for §9.2. The stale docs get corrected
regardless.

## 7. Built-in relay (G3, G5, G6)

Any reachable daemon can relay, as can the same binary on a small server
(`workass relay`). It is a pure splice.

```
target -> relay   upgrade /workass/relay/listen    (control, heartbeat)
client -> relay   upgrade /workass/relay/dial?target=<rendezvous id>
relay  -> target  "incoming <stream>" on the control channel
target -> relay   upgrade /workass/relay/accept?stream=<id>
relay splices; inner TLS 1.3 runs client <-> target, pinned
```

- **One outbound TCP per stream.** TCP supplies isolation and flow control, so no
  multiplexer and no dependency.
- **Reaches restrictive networks.** A WebSocket-shaped upgrade on a port list held
  as data, with 443 first. Honours system proxies. Outer TLS is not the trust
  anchor, so an inspecting proxy cannot read the inner session.
- **The daemon is unchanged above the listener.** A `relayListener` feeds streams
  into the same `tls.NewListener` + `http.Server`, so a relayed connection is an
  ordinary wire session.
- **The relay sees only ciphertext.** Device tokens stay inside inner TLS.
- **Admission for an untrusted relay.** A rendezvous token derived from the fleet
  key under its own domain separator, with no fleet key and no stored account on
  the relay, and per-token caps.
- **Locality law.** A relayed connection is never local, on every gate, through the
  §3 helper. Human pairing is refused over a relay, and no route is learned from a
  relayed peer.

**Clients:**

- Go mesh: a custom dial function.
- Electron: one loopback forwarder owned by the local daemon. The renderer connects
  to `wss://<machineId>.localhost:<port>`, and the forwarder routes on SNI to the
  right peer, reading the name with a stdlib `GetConfigForClient` peek that writes
  nothing. It is a separate listener, and an unknown name closes the connection and
  never reaches the daemon, which auto-pairs loopback peers. Each peer gets its own
  hostname, so each has exactly one pin; a shared loopback host would let one peer's
  key satisfy another's pin. Chromium's handling of these names needs rig proof
  before this is final.
- Plain browsers: LAN only, because they cannot hold a pin.
- Phone: a native transport, in its own spec in its own repo.

**Why nested TLS rather than SNI passthrough:** browsers send no SNI for IP
literals, and DNS names would need central infrastructure.

## 8. Rejected

| Option | Why |
|---|---|
| Embed Caddy | Its unique value here is publicly trusted certificates, which reach only a public node, not NAT |
| Let's Encrypt IP certificates | Available since January 2026, but a public-IP node only. Deferred, not rejected, for a future public relay serving plain browsers |
| Embed `tsnet` | Still needs a control server, and Tailscale's module carries several hundred requirements |
| iroh, Syncthing's relay pool, Cloudflare quick tunnels | Public relays are not offered for this: not for production, built for another app, or testing-only |
| WebRTC / QUIC hole punching | Needs UDP, a large stack, and still a signalling server |
| UPnP / NAT-PMP / PCP | Unreliable, adds exposure, and the relay already covers the case |
| `workass://join` as an OS link handler | A web page could add a key to the daemon. Invites are pasted, or confirmed naming the new fleet |

## 9. Owner decisions

1. **Who runs the default relay?** Two machines that are both outbound-only cannot
   meet without a third party. "Bring your own" is not "just runs". The options:
   ship with a project-run relay, which amends fleet-runtime §15 and adds cost and
   abuse handling, or require one reachable machine you own. Recommendation: build
   bring-your-own first with admission designed for an open relay, so a project
   relay later is a data change, and decide before P4 ships.
2. **Discovery law amendment** (§6).
3. **Public addresses.** Direct public routes require public-key enrolment (the
   replacement `fleet.go` already promises), because a symmetric key means one
   stolen machine opens every reachable daemon. P4 itself requires real rotation,
   and public-key enrolment is a separate spec. Rotation revokes every device
   enrolled under the old key id, distributes the new key only over surviving links
   (never under the old key, which would hand it to the thief), and rotates the
   rendezvous token.

## 10. Phases

| Phase | Work | Gate |
|---|---|---|
| P0 | §3 trust and hardening | An on-path rig with a forged card and a stripped proof gets no enrolment, on every client class; locality denial tests on every gate |
| P1 | §5 address trust and renderer ladder | Client reaches an overlay-range address; browser partition still refuses a public forged certificate |
| P2 | §4 certificate lifecycle | Clock-forward rig: reissue at restart, every client class reconnects with no re-approval, including past expiry |
| P3 | §6 discovery and gossip, after §9.2 | Address change reconnects with no human; no sweep on an unassociated network |
| P4 | §7 relay, Go mesh then Electron, plus key rotation | Two daemons with all inbound firewalled exchange chats; capture at the relay shows no plaintext; works through an HTTP proxy on 443; a stolen key and stolen tokens are refused over relay and LAN after rotation |
| Later | Public routes after public-key enrolment | Own spec |

Separate specs: phone transport, and install (signing, `workass install --user`,
first-run reachability check, which is UI and therefore mock-first). Fleet features
need the live rig before release: unit tests alone pass no phase.
