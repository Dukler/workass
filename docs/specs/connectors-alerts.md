# Conectores and alerts (owner 2026-11-10)

Mocks (approved): `desktop/docs/mocks/connectors-alerts/mock.html` section 1 (Ajustes · Conectores)
and `desktop/docs/mocks/connectors-alerts/popover-v2.html` section 0 (the bell) + variant **A · Parte**.
Variants B, C, D of popover-v2 and A/B/C/2 of mock.html are rejected; do not build them.

## Owner laws

1. An alert NEVER opens a chat, writes into a chat or wakes an agent by itself. It only shows in the bell
   (and, if enabled, one system notification). An agent gets an alert only after the owner clicks
   "Abrir sin enviar" or "Abrir y enviar".
2. The hand-off uses Workass's new-chat default agent (Ajustes · General), with no agent picker. The
   machine and folder come from the connector's target, or a per-rule override; the owner may change
   them for one hand-off by clicking the folder token.
3. Few signals: Grafana repeats never re-announce, never move the bell, never notify.
4. Universal: any Grafana, any rule. Nothing DepthCut-specific in code.

## Model

A **connector** lives on one daemon (the "machine that listens"). Kind `grafana` (only kind in v1).

```
Connector {
  id: string                       // "gf-" + 8 hex, minted by the daemon
  kind: "grafana"
  name: string                     // "fiumba"
  url: string                      // base URL, e.g. http://127.0.0.1:13000
  mode: "poll" | "webhook"         // Consultar | Recibir
  hasToken: bool, tokenHint: string   // poll: service-account token, write-only; hint = last 4 chars
  webhookPath: string              // webhook: "/workass/connectors/<id>/webhook"
  hasWebhookSecret: bool
  status: { state: "ok" | "error" | "idle", lastOkAt?: RFC3339, error?: string }   // error redacted
  rules: [{ key, title, folder, severity, enabled, seenAt }]
           // key = Grafana rule UID if known, else "title:" + title. Learned from polls or webhooks.
           // enabled defaults to true for a newly seen rule.
  notify: "never" | "critical" | "all"   // default "critical"
  target: { machineId: string, cwd: string }      // where a hand-off chat opens; machineId "" = the client's local machine
  ruleTargets: { [ruleKey]: { machineId, cwd } }
  guidePattern: string             // e.g. "ops/runbooks/<regla>.md"; "" = none. <regla> = the alert's
                                   // `alert_family` label if present, else the rule title slugged
  machineLabels: string[]          // grouping label order, default ["machine","instance","host"]
}

Alert {
  id: string                       // connectorId + ":" + fingerprint
  connectorId, ruleKey, ruleTitle: string
  severity: "critical" | "warning" | "info"   // from label `severity`, default "warning"
  machine: string                  // first present of connector.machineLabels, else ""
  labels, annotations: map[string]string     // redacted
  values?: map[string]number
  state: "firing" | "resolved"
  startsAt, endsAt?, lastSeenAt: RFC3339
  repeats: int, lastRepeatAt?: RFC3339       // Grafana re-sends of the same firing; never announced
  generatorURL?: string
  silencedUntil?: RFC3339, dismissedAt?: RFC3339
  link?: { machineId, tabId, chatId, at }   // set by the client after a hand-off
  history: [{ at, state }]         // firing/resolved transitions, last 20
}
```

An alert of a disabled rule is not listed (the rule stays in `rules`). Resolved and dismissed alerts
stay listed for 7 days, then are pruned. A dismissed alert that fires again (a new firing episode)
comes back undismissed. Silence hides an alert from the bell's "undecided" count until `silencedUntil`.

Episode: an alert that goes resolved → firing again is a new episode: `startsAt` moves, `repeats`
resets, `link` and `dismissedAt` clear, `history` keeps growing, and it is **announced**.

## Wire (additive; frozen frame shapes untouched)

| Channel | Args | Reply | Mutating |
|---|---|---|---|
| `connectors:list` | — | `{connectors: Connector[]}` | no |
| `connectors:set` | `{id?, name, url, mode, token?, notify, target, ruleTargets, guidePattern, rules?: [{key, enabled}]}` (`token` write-only, omit = keep; `""` = clear) | `{connector, webhookSecret?}` (secret only when first generated) | yes |
| `connectors:remove` | `{id}` | `{ok}` | yes |
| `connectors:test` | `{id}` or `{url, token}` | `{ok, error?, rules: n, firing: n}` | no |
| `connectors:rotate-secret` | `{id}` | `{webhookSecret}` | yes |
| `connectors:handover` | `{id}` (on the current listener) | `{connector, token?, webhookSecretHash?, alerts: Alert[]}` (every alert of the connector, a switched-off rule's too; removes nothing) | yes |
| `connectors:adopt` | `{connector, token?, webhookSecretHash?, alerts, url?}` (on the new listener; `url` overrides the address) | `{connector}` | yes |
| `alerts:list` | — | `{alerts: Alert[]}` | no |
| `alerts:act` | `{ids: string[], action: "silence", minutes}` / `"unsilence"` / `"dismiss"` / `"undismiss"` / `{action:"link", ids, link:{machineId,tabId,chatId}}` | `{alerts: Alert[]}` | yes |
| event `alerts:changed` | — | `{alerts: Alert[] (changed ones), announce: string[] (ids of new episodes), removed?: string[]}` | — |
| event `connectors:changed` | — | `{connectors: Connector[]}` | — |

`connectors:set` also takes an optional `machineLabels: string[]` (empty = the default order).

Bridge methods (`internal/httpserve/lan_bridge.go`): `connectorsList()`, `connectorsSet(o)`,
`connectorsRemove({id})`, `connectorsTest(o)`, `connectorsRotateSecret({id})`, `connectorsHandover({id})`,
`connectorsAdopt(o)`, `alertsList()`, `alertsAct(o)`, `onAlertsChanged(cb)`, `onConnectorsChanged(cb)`.

`alerts:changed` cannot express an alert leaving the list, so the daemon adds `removed: [id]` when it
prunes alerts or a connector is removed. Switching a rule off sends only `connectors:changed`; the client
re-reads `alerts:list` on `connectors:changed`. `connectors:test` is a concurrent read (it never waits
behind or holds the ordered lane); with `{id}` plus explicit `url`/`token`, the explicit ones win.

### Moving the machine that listens

One click in Conectores, three calls by the client, all mutating (controller-authorized):

1. `connectors:handover {id}` on the current listener. Its reply is the only one that ever carries a
   secret (the poll token and the webhook secret's SHA-256), only to the paired controller; it is never
   logged and removes nothing.
2. `connectors:adopt` on the new listener with that reply (plus `url` if the new machine reaches Grafana
   by another address). It creates the connector under the SAME id (refused if that id already exists
   there, unless identical: a retried adopt succeeds), imports the alerts as they were (link,
   dismissedAt, silencedUntil, history, startsAt) WITHOUT announcing anything, and starts its poller. An
   alert already firing stays the same episode on the next poll, so nothing re-announces.
3. `connectors:remove {id}` on the old listener.

Webhook mode: the secret hash moves, so Grafana's bearer stays valid; only the contact point's host
changes.

## Transport

- **poll (Consultar)**: every 30 s (10 s timeout, exponential backoff to 5 min on error)
  `GET {url}/api/prometheus/grafana/api/v1/rules` with `Authorization: Bearer <token>`; a Viewer
  service-account token is enough. It gives every rule (name, uid when present, folder/group, labels,
  state) and each alerting instance (labels, annotations, state, activeAt, value). Firing = instance
  state `Alerting`/`firing`; `Pending` is not firing; an instance that disappears is resolved
  (`endsAt` = the poll time). The token is optional: without one no `Authorization` header is sent
  (a proxy in front of Grafana may authenticate). Fixture: `internal/connectors/testdata/grafana13-rules-live.json`.
- **webhook (Recibir)**: `POST /workass/connectors/<id>/webhook` on the daemon's HTTP mux, auth
  `Authorization: Bearer <secret>` (constant-time compare; 32 random bytes, base64url, stored hashed is
  fine but the daemon must show it once). Body = Grafana's webhook JSON (`alerts[]` with `status`,
  `labels`, `annotations`, `startsAt`, `endsAt`, `fingerprint`, `generatorURL`, `values`).
  Body cap 1 MiB. 401 on bad secret, 404 on unknown id or a poll-mode connector. The route answers
  before any chat is touched (it never touches a chat).
- As built (daemon, `internal/connectors`):
  - `NoData` and `Error` instance states (and `Alerting (…)`) count as firing, because Grafana notifies
    for them; `Normal (NoData)` does not.
  - `repeats` counts only webhook re-sends; a poll that sees the same firing again changes nothing and
    sends no event unless labels, annotations or values changed.
  - A firing alert whose start moved later (a resolve missed between two looks) is a new episode.
  - A firing alert is never pruned, dismissed or not: pruning it would re-announce it on the next look.
    Only resolved alerts are pruned, 7 days after `endsAt`.
  - Silence survives a new episode (it is time-bounded).
  - Poll mode: a rule that vanished from Grafana leaves `rules` unless the owner switched it off.
- Secrets: `state/connectors/secrets/<id>.json`, mode 0600, never sent on the wire, never logged.
  Config `state/connectors/connectors.json`, alerts `state/connectors/alerts.json` (atomic writes).
  Redact labels/annotations with `acp.RedactSensitiveText`.

## Client

- **Ajustes · Conectores** (group "Tus máquinas", after Trabajos), per mock.html section 1 with the
  owner's amendments: no "Agente sugerido" row; "Al dárselas a un agente": Dónde se abre el chat
  (machine ▾ + folder), Por regla (overrides list + "Añadir una regla…"), Guía por regla. The connector
  list spans every connected machine; "Máquina que escucha" is chosen when adding.
- **Bell**: per popover-v2 section 0: a foot-bar icon, no number; one swing when an alert is announced;
  filled dot in the worst undecided severity's colour; hollow ring when every firing alert has a chat or
  is silenced; nothing when none fire.
- **Parte popover**: per popover-v2 variant A: one paragraph per (connector, machine) situation, "Hoy"
  for resolved ones, Registro (labels, annotations, values, Grafana link), verbs Dársela a un agente /
  Silenciar 10 min / Descartar. Hand-off: "Abrir un chat nuevo en [machine · folder] y decirle:" +
  editable pre-written message + "Con el agente de siempre (Ajustes · General: …)" + Volver / Abrir sin
  enviar / Abrir y enviar. It creates the chat through the client's normal new-chat path (default agent,
  remote machines included) and then calls `alerts:act link` on the alert's daemon. A linked alert shows
  "La lleva el chat X" and Ir al chat.
- System notification: one per announced alert, if the connector's `notify` allows its severity and
  the app's notifications are on.
