# Workass tools

Workass's own chat, browser, artifact, update and tracked-work actions use the
packaged `workass tools` CLI. The provider invokes it through its native shell
tool. There are no Workass MCP servers, MCP discovery sessions, protocol
handshakes, stdio proxies, or compatibility endpoints. Vendor/user MCP
integrations remain supported by their provider hosts.

Each provider process inherits the exact executable and a private context path
through `WORKASS_TOOLS_COMMAND` and `WORKASS_TOOL_CONTEXT`. Use the current
environment; paths quoted in earlier conversation turns expire when their
attachment closes. Generic ACP prompts and native instructions both use this
process binding. Do not guess the machine, active chat, or context file.

In production, the executable is the running Workass daemon itself. Its `tools`
subcommand dispatches before daemon startup. No sibling helper is selected or
retried. Development may supply an explicit absolute `--tools-command` override;
production ignores that override. The standalone helper remains packaged for
compatibility with older Windows updaters, but is no longer a runtime dependency.
New Windows installer and updater validators accept its absence and check its
executable format when it is present.

```sh
"$WORKASS_TOOLS_COMMAND" tools guide
"$WORKASS_TOOLS_COMMAND" tools list
"$WORKASS_TOOLS_COMMAND" tools list workass_read_chat
"$WORKASS_TOOLS_COMMAND" tools call workass_read_chat --input arguments.json
```

The last command reads a JSON object such as:

```json
{"tab_id":"EXACT_TAB_FROM_LIST","chat_id":"EXACT_CHAT_FROM_LIST","limit":10}
```

Without `--input`, `call` reads one JSON object from stdin. Use `{}` for no
arguments. Files are UTF-8; UTF-8 BOMs from Windows PowerShell are accepted.
On PowerShell invoke the executable with `& $env:WORKASS_TOOLS_COMMAND tools`.
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
a prompt. Its path stays stable for that process; its exact owner binding is
refreshed before use, and a missing context file is recreated. A new attachment
gets a new process path. Context is removed on session close/reset. Old
authority is rejected after detachment even if a caller retained the file.
An explicit `--context` remains available for controlled CLI callers; an
invalid explicit path fails instead of silently switching to another owner.

Calls reuse the existing action handlers, remote routing, redaction, ownership,
and durable operation receipts. Mutations require a caller-stable
`operation_id`. An uncertain response is not permission to issue a fresh
mutation: reuse the same id and exact arguments for receipt readback. Updating
an application still requires the current human's explicit instruction naming
the exact machine and the observed versions. Publication does not authorize
activation.

There is no provider tool registration or MCP certificate injection. Ordinary
chat lifecycle also performs no automatic Git capture before or after turns.

## Delegation with one event-only wait

Read `workass_agent_catalog` for exact provider/model/effort/mode ids. A
coordinator can select Astra for its own chat and spawn Luna with
`workass_spawn_subagent`, supplying a bounded task and a stable `operation_id`.
Child execution provider and parent origin lane are separate identities:
cross-provider children remain listable, messageable, waitable, and represented
in the parent's durable receipts with the child's actual provider. Inherited
permission must have a known semantic mapping; otherwise specify a catalog
mode or `permission_intent` explicitly.

After spawning, make one blocking call (substitute the returned child id):

```sh
"$WORKASS_TOOLS_COMMAND" tools call workass_wait_subagent <<'JSON'
{"subagent_id":"RETURNED_CHILD_ID","timeout_ms":-1,"operation_id":"wait-bounded-task-1"}
JSON
```

`timeout_ms:-1` has no Workass deadline and no polling. It returns for child
completion/failure/cancellation or latched permission attention. Routine
progress does not return control to the coordinator. `workass_wait_subagents`
supports the same option with `subagent_ids` and `return_when:"first"|"all"`.
Omission or zero retains the ten-minute default; 1000..3600000 sets a bounded
deadline. Caller cancellation or connection/daemon loss can interrupt any wait.

The worker reports a genuine blocker or a decision requiring a changed approach
by finishing with that report; the same pending wait returns it. Permission
attention is a separate wake reason and uses the existing authorized decision
surface. There is no synthetic follow-up message or automatic coordinator turn
when no wait is active.

This suspends Workass's request, not the vendor's shell harness. The native
shell tool must remain blocked: a shell yield, timeout, or harness-imposed
deadline can still return control to the model. Workass itself does no sampling
while waiting. The coordinator's spawn/wait calls and resumed response, plus
the worker's model work, still have their normal provider costs; this is not a
guarantee of zero provider billing. No pricing is inferred by Workass.

Children survive ordinary parent-turn completion by adoption into the exact
chat. Live worker execution is not restart-durable: daemon replacement ends
its managed processes and open waits. Actor state and bounded terminal receipts
survive, but neither the worker nor a coordinator wait is automatically replayed.
