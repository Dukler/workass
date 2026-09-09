# Workass tools

Workass's own chat, browser, artifact, update and tracked-work actions use the
packaged `workass tools` CLI. The provider invokes it through its native shell
tool. There are no Workass MCP servers, MCP discovery sessions, protocol
handshakes, stdio proxies, or compatibility endpoints. Vendor/user MCP
integrations remain supported by their provider hosts.

Each turn supplies the exact executable and current private context path. Use
those paths; don't guess the machine, executable, active chat, or context file.

```sh
'/absolute/path/workass' tools --context '/current/context.json' list
'/absolute/path/workass' tools --context '/current/context.json' list workass_read_chat
'/absolute/path/workass' tools --context '/current/context.json' call workass_read_chat --input arguments.json
```

The last command reads a JSON object such as:

```json
{"tab_id":"EXACT_TAB_FROM_LIST","chat_id":"EXACT_CHAT_FROM_LIST","limit":10}
```

Without `--input`, `call` reads one JSON object from stdin. Use `{}` for no
arguments. Files are UTF-8; UTF-8 BOMs from Windows PowerShell are accepted.
On PowerShell invoke the executable with `& 'C:\path\workass-daemon.exe'`.
`list` returns the full catalog and JSON argument schemas. `list NAME` returns
one schema. Successful results are JSON on stdout; errors are redacted JSON on
stderr with a nonzero exit status. Screenshots return local image paths for the
provider's native image reader. Copy an image into the chat workspace before
using ordinary image Markdown to deliver it to the user.

The CLI makes one ordinary JSON request to `/workass/tools` over pinned HTTPS.
It connects directly to loopback, does not use DNS or HTTP proxies, and refuses
redirects. Authentication resolves the exact live session and durable chat
actor. Delegated agents receive the control catalog without browser actions.
The context file contains a private capability; never print it or paste it into
a prompt. It is created once per live owner, reused without rewriting on later
turns, replaced on rebinding, and removed on session close/reset. Old authority
is rejected after detachment even if a caller retained the file.

Calls reuse the existing action handlers, remote routing, redaction, ownership,
and durable operation receipts. Mutations require a caller-stable
`operation_id`. An uncertain response is not permission to issue a fresh
mutation: reuse the same id and exact arguments for receipt readback. Updating
an application still requires the current human's explicit instruction naming
the exact machine and the observed versions. Publication does not authorize
activation.

There is no provider tool registration or MCP certificate injection. Ordinary
chat lifecycle also performs no automatic Git capture before or after turns.
