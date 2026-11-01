# Portable networking: one app, any network

Status: DESIGN v4, after two contrarian reviews. Discovery (§6) is built; the rest is not.

## 1. Goal

Install one artifact, open it, and it works on any machine and any network, with
nothing else to run: no reverse proxy, VPN, domain, SSH tunnel, port forward or
account.

- **G1** One machine: works offline from the first launch.
- **G2** Same network: machines on default ports find each other with no
  configuration. A custom port is typed once at first pairing.
- **G3** Different networks: machines connect over any VPN the owner already runs,
  with no VPN embedded in Workass.
- **G4** The certificate never needs a human, including at expiry.
- **G5** Hardened hosts: works with TCP only, one port, no admin rights and no UDP.
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
- **F6 Nothing crosses NAT by itself.** A VPN does that, once F2 is fixed.
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

This alone makes overlay and VPN addresses work, with no embedded VPN. The renderer also gains the route ladder that only the
Go mesh has today, so it falls back instead of stopping at the first address.
Refused routes expire or scope per network, because RFC1918 addresses collide
across networks.

## 6. Discovery by network type (F1). BUILT

A network is **associated** only while a stored peer completes a pinned handshake
on it right now. Subnets collide and cards can be forged, so neither counts.

| Situation | Behaviour |
|---|---|
| Associated network | Automatic sweep at today's cadence, on ports 80 and 8788 |
| Holds a fleet key and knows no machine | Automatic sweep, so a headless machine can find its fleet |
| Anywhere else | No automatic sweep. A sweep runs while the Machines pane is open, or when an agent calls `workass_discover_machines` |
| Windows | Unchanged under the 2026-09-25 law: port 80, every network |

The law is in PORT-SPEC (2026-11-01). The agent tool takes no address on purpose: a
prompt-injected agent must not be able to point enrolment at an arbitrary host. A
custom port or a VPN address is typed by a person, once. Peer address gossip, which
would handle roaming over VPN, is not built.

## 7. Reaching a machine over the internet

The owner's decision (2026-11-01): always through a VPN, and Workass is not exposed
to the internet directly. So a VPN address is just another route, and §5 is what
makes it work. No relay, no public addresses and no project-run infrastructure are
planned.

**Deferred, not designed further:** a built-in relay. If that ever changes, the shape
that survived review was a pure splice with nested TLS, one outbound TCP per stream,
a WebSocket-shaped upgrade on port 443 honouring system proxies, a rendezvous token
derived from the fleet key, and a relayed connection never counting as local on any
gate.

## 8. Rejected

| Option | Why |
|---|---|
| Embed Caddy | Its unique value here is publicly trusted certificates, which only a public node can get |
| Let's Encrypt IP certificates | Available since January 2026, but for a public-IP node only |
| Embed `tsnet` | Still needs a control server, and Tailscale's module carries several hundred requirements |
| iroh, Syncthing's relay pool, Cloudflare quick tunnels | Public relays are not offered for this: not for production, built for another app, or testing-only |
| WebRTC / QUIC hole punching | Needs UDP, a large stack, and still a signalling server |
| UPnP / NAT-PMP / PCP | Unreliable, adds exposure, and the relay already covers the case |
| `workass://join` as an OS link handler | A web page could add a key to the daemon. Invites are pasted, or confirmed naming the new fleet |

## 9. Owner decisions

All settled on 2026-11-01: no relay and no direct internet exposure (§7), and the
discovery amendment (§6).

## 10. Phases

| Phase | Work | Status or gate |
|---|---|---|
| Discovery | §6 | Built. Live: the installed build found 1 of 3 machines on a real LAN, this one found all 3 in 7.4 s; idle with nothing known it knocked 0 times |
| P0 | §3 trust and hardening | Not built. An on-path rig with a forged card and a stripped proof gets no enrolment, on every client class; locality denial tests on every gate |
| P1 | §5 address trust and renderer ladder | Not built. Client reaches a VPN-range address; browser partition still refuses a forged public certificate |
| P2 | §4 certificate lifecycle | Not built. Clock-forward rig: reissue at restart, every client class reconnects with no re-approval, including past expiry |

Separate specs: phone transport, and install (signing, `workass install --user`,
first-run reachability check, which is UI and therefore mock-first). Fleet features
need the live rig before release: unit tests alone pass no phase.
