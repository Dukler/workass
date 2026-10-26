# Workass tools

Workass's own chat, browser, artifact, update and tracked-work actions use the
packaged `workass tools` CLI. The provider invokes it through its native shell
tool. There are no Workass MCP servers, MCP discovery sessions, protocol
handshakes, stdio proxies, or compatibility endpoints. Vendor/user MCP
integrations remain supported by their provider hosts.

Each provider process inherits the exact executable and a private context path
through `WORKASS_TOOLS_COMMAND` and `WORKASS_TOOL_CONTEXT`. Use the current
environment, never a context reference from earlier conversation turns: a tool
error names what happened to the owning chat without echoing the private path,
and a stale path is both wrong and a private detail that must not be repeated.

On production Windows, providers invoke the signed bundled Node runtime through
the `workass-tools.cmd` shim and its packaged `workass-tools.mjs` client. The
client makes the same authenticated request and preserves the tool catalog,
argument validation, context ownership, redaction, and security checks. Do not
launch the unsigned Workass Go PE as a transient shell CLI. This signed Node
shim is the only new Windows-layout exception. Older Windows layouts without
the shim continue using the in-process daemon tools entrypoint; do not search
for or retry a sibling executable. Other production layouts use the running
daemon's early `tools` dispatch. Development may supply an explicit absolute
`--tools-command` override; production ignores that override. Retain the legacy
standalone helper where package compatibility requires it, but never select it
as a runtime fallback.

On the San-laptop Windows development machine, do not build or run unsigned Go
executables or tests; use the Mac development machine or Windows CI. Windows
RSS sampling and the raw-MCP guard must not use tasklist or PowerShell
process-query loops. Shortcut discovery and icon repair must not use a
PowerShell or WScript sweep, or launch ie4uinit. These constraints do not
guarantee CrowdStrike approval and do not establish that every alert is fixed.
They do not change LAN peer discovery or updater behavior; related design
options remain outstanding.

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
actor. Delegated agents receive the control catalog without browser actions,
questions, or subagent orchestration, and every call acts for the exact chat
that spawned them (as a Claude Code subagent works inside its parent's
session and cannot delegate further).
The context file contains a private capability; never print it or paste it into
a prompt. Its path stays stable for that process; its exact owner binding is
refreshed before use, and a missing context file is recreated. A new attachment
gets a new process path. Context is removed on session close/reset. Old
authority is rejected after detachment even if a caller retained the file.
An explicit `--context` remains available for controlled CLI callers; an
invalid explicit path fails instead of silently switching to another owner.
Context errors never echo the private context file path. When
`WORKASS_TOOL_CONTEXT` is unset the process was not launched by a Workass chat
engine. A missing context file means the chat that launched the process was
deleted, or the daemon cleaned up a context left over from before a restart:
long-running helpers should not poll, ask the chat to use
`workass_schedule_message` instead. A rejected credential (`401`) means either
the chat that launched the process was deleted, or the context was issued by an
engine that has since stopped (contexts written before chat-lifetime tokens
carry an engine owner key that dies with its engine); for delayed work use
`workass_schedule_message` instead of a detached helper.

`workass_ask_user_question` is available to a foreground ACP turn even when
its native manifest has no ask-question capability. It opens the existing
question card in the exact owning chat and waits for a structured answer. This
asks for a decision only; it never approves a tool or executes an action.
Keep `operation_id` and every question argument unchanged when retrying: the
actor returns the same durable answer or terminal status and never opens a
second card. Caller cancellation settles the card as `cancelled`; a same-id
retry reads that durable result. A controller reconnect can recover the same
pending card while its owning turn remains live. Turn/session cancellation also
settles the card as `cancelled`. Omit `timeout_ms` to wait for the owning turn
or user; an explicit 1000..3600000 deadline returns `timed_out`.

```sh
"$WORKASS_TOOLS_COMMAND" tools call workass_ask_user_question <<'JSON'
{"operation_id":"choose-deploy-target-1","question_id":"deploy-target","header":"Deploy target","question":"Which target should I prepare?","options":[{"id":"canary","label":"Canary","description":"Lower-risk test"},{"id":"production","label":"Production"}],"multi_select":false,"allow_free_text":true}
JSON
```

The result contains `status` (`answered`, `dismissed`, `cancelled`, or
`timed_out`), `selected_options` with original ids and labels, `free_text`, and
the question and operation ids. An `answered` result needs a selection or
nonblank free text. Selections and free text may be returned together. Child
and subagent catalogs omit this tool because their owner model does not allow
them to open a human question; they can report the blocker to the parent.

## Owned browser observations

`workass_browser_open` creates the page for the exact owning chat. Omit
`visible` to keep the legacy pane request, or pass `visible:false` to navigate
in the background. A new page starts at a logical 1440×900 CSS viewport at DPR
1. When shown, the browser pane sets the visible page viewport from its bounds;
human resizing updates it again. `workass_browser_list` and
`workass_browser_snapshot` report requested/effective metrics and generations.
Agents use `workass_browser_set_viewport` for an intentional responsive size and
`workass_browser_reset_viewport` to return to the desktop default. Opening or
resizing the human pane may then set its visible size.

`workass_browser_screenshot` supports `viewport` (default), `full_page`, and a
document-CSS `clip`. Its image is accompanied by `screenshot_id`, exact tab and
generation identity, CSS capture rectangle, scroll origin, image dimensions,
and pixel-to-CSS scale. Pass screenshot pixel `x`/`y` with that `screenshot_id`
to click a visual target. In the webview host (the default on macOS; Windows
keeps the native-view host by default until the webview host has run on real
Windows hardware; `WORKASS_BROWSER_HOST=webview|view` or
`{"browserHost":"webview"|"view"}` in the data directory's
`shell-settings.json` selects either) the page is the guest of a `<webview>` in the app window and captures
come from the guest's own surface (`capture_surface:"webview"`): sharp at any
shown size and while the app covers or hides it; a full page or a clip beyond
the viewport is taken one viewport at a time, scrolling the page there and
back. One capture runs at a time per page, bounded to 12 s. After an app
reload the tab keeps its id and comes back at its URL in the same profile.
Selector clicks remain supported; snapshot
`element_ref` values must be paired with their `snapshot_id`. Refresh the
snapshot after navigation or a stale-reference result. Ambiguous, disabled,
covered, blocked, stale, and missing targets are not reported as successful
clicks.

Snapshots retain editor information and add bounded semantic nodes, open
shadow roots, owned same-origin frame boundaries, scroll and loading state, and
explicit truncation counts. `workass_browser_wait` observes DOM/load/element
conditions for at most 15 seconds and returns safe current state on timeout.
`workass_browser_batch` prevalidates 1–20 sequential actions and can return one
final snapshot with `observe_after:true`. `workass_browser_diagnostics` reads a
redacted in-memory warning/error cursor; page diagnostics are not persisted.

Snapshot element rows also carry `href` (absolute, resolved against the
document base), `src`/`alt`, `title`, and the current `value` of inputs,
textareas and selects (a select reports its selected option labels). Password,
one-time-code, card and secret-named fields report `valueRedacted:true` and
never a value; every other value and URL passes the page redaction plus
credential-shape redaction (provider keys, JWTs, private keys, URL userinfo).
In the default `json` format these facts are added only after the legacy 64
KiB budget has run, so every legacy field is byte-identical to before; facts
that do not fit are counted in `truncation.element_facts_omitted`.
`format:"compact"` renders one line per element with short refs (`e12`, frames
`f1`) aliased in the same observation, so they work with that `snapshot_id`
for click, type, scroll and read. Compact output folds an image into its link,
drops rows that only repeat text (list items, paragraphs, static text), and
budgets interactive lines first, then page text, then the outline.
`since_snapshot_id` diffs against an earlier compact snapshot of the same
document (`+` added, `~` changed, `-` removed, with unchanged refs still valid)
and falls back to a full compact snapshot with `incremental.reason`
(`document_changed`, `base_snapshot_unknown`, `too_many_changes`).

`format:"outline"` is the page as nested lines built from Chromium's
accessibility tree of every frame the tab owns, cross-site frames included and
placed under their `<iframe>`: role, name, current value (passwords and other
secret fields `[redacted]`), state (checked, expanded, selected, disabled,
required, focused, level), link and frame URLs, and a short ref (`e12`) on
everything an action can target. A frame that cannot be read says why in
place; a canvas is reported as pixels only. Refs stay the same for an element
while its document lives (a navigation that never commits, such as a download,
keeps them) and work on their own as `element_ref` in click, type, scroll,
select, hover, drag, upload and evaluate; a `snapshot_id` with them is
optional and, when given, is the `outline_id`. Output is bounded (`max_bytes`
2048–60000, default 20000) on whole lines, with `next_cursor` to continue the
same outline (`cursor`); `since_snapshot_id` with an `outline_id` returns only
the lines added (`+`), changed (`~`) and removed (`-`). Four or more sibling
lines that differ only in counting numbers are folded into one line with
`{first..last}` (or `{first..last..step}`) ranges: `- link "Item {1..2000}"
[e{2..2001}]` is 2000 links, and every ref in the range targets its element. A
plain list item around a single thing is shown as that thing.

Through the agent tools every action (open with a URL, navigate, history,
click, type, key, scroll, select, hover, drag, upload, evaluate, batch) returns
`page` (url, title, loading), `events` that happened during it (`navigated`,
`url_changed`, `dialog_opened`/`dialog_closed`, `download_started`/
`download_finished`, `tab_opened`, `popup_blocked`, `beforeunload_accepted`)
and `changes`: a bounded outline diff, or after a navigation the new page's
outline (first part, with `next_cursor`). The page is settled first, for as
long as something is happening and no longer: a navigation reaches a usable
document; then, if nothing happened, two frames (32 ms) of quiet; once
something happened (DOM mutations, input/change/focus/toggle events, requests
the action started, frames attaching or loading), 80 ms of quiet after the
last of it; at most 1.5 s. Requests are watched only while an action settles
(WebSockets, event streams and document loads excepted). On a page without
frames where nothing happened, `changes` says `none` without reading the page
again. A batch reports once, for all of its actions. Each tab
operation has a deadline (25 s; navigation and batch 28.5 s): one that does not
finish is reported and the tab's queue moves on, and `workass_browser_list`
shows `stuck_since_ms` for it.

`workass_browser_select` chooses options of a native `<select>` by values,
labels or indexes (input and change events); a custom dropdown is clicked
instead. `workass_browser_hover` moves the pointer with trusted input.
`workass_browser_drag` drags to an element or by `delta_x`/`delta_y` with a
trusted press, moves and release; an HTML drag and drop goes through
Chromium's drag events (`Input.setInterceptDrags`), never an OS drag session;
both ends must be in one frame. `workass_browser_upload` sets a file input's
files (`DOM.setFileInputFiles`) from absolute paths the agent passes: at most
10 files and 10 MB, never symbolic or hard links, never credential-like names
or folders (`.ssh`, `.aws`, private keys, tokens, `.env` and the like).
`workass_browser_dialog` answers the open alert, confirm or beforeunload
(`Page.handleJavaScriptDialog`); while one is open other page tools fail with
its message. Electron answers `prompt()` itself with null. An agent's
navigation leaves a page whose beforeunload asks to stay. `workass_browser_evaluate`
runs page JavaScript (8 KiB source, 10 s, JSON result up to 16 KiB as
`page_data`, redacted); it can change the page, so it is a mutation with an
`operation_id`, never labelled read-only. `workass_browser_close` closes a tab
one of the chat's pages opened, or resets the chat's own page to a fresh blank
page in the same profile (a new tab id).

Links (`target=_blank`) and `window.open` popups of a page an agent is driving
(not shown to the person, or touched by an agent call in the last 15 s) open as
background tabs of the same chat, listed with `opened_by_tab_id` (at most 8).
Their downloads save into a folder of the chat's own under the profile's data
directory (`browser-downloads/chat-<hash>/`) and are listed per tab; a download
the person starts in a page they are looking at keeps Electron's save dialog.

`workass_browser_read` renders the page, a frame (`frame_ref`), an element
(`element_ref`) or CSS selector matches as markdown: links and images with
absolute URLs, markdown tables, summarized form fields, and JSON-LD, microdata
and meta tags first (or after the content, with an offset pointer, when larger
than 8 KiB). A page's `<main>` region comes before its chrome. Child frames,
including cross-site owned frames, are read in their own documents and placed
where their iframe is; frame reads share an 8 s budget and at most 40 frames.
Output is paged by UTF-8 byte offset (`max_bytes` 2048–65536, default 32768,
pages end on a line when one is near); continuations reuse the same extraction
until the document or the agent's interaction generation changes. The read is
non-mutating and needs no `operation_id`.

Arguments are validated against the tool's input schema before any handler
runs: unknown arguments, wrong types, out-of-range numbers and values outside an
enum are rejected with an error naming the argument.

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

## Forkable subsessions

For many short, independent jobs that share one long prefix (a brief plus
example images, then one frame per job), fork a chat instead of spawning a
fresh subagent per job. `workass_create_fork_point` marks an exact idle chat;
`workass_fork_run` starts children from that point, each with its own message
and up to 20 local image paths in ONE user message; `workass_wait_fork_run`
(`timeout_ms:-1`) returns every child's final text, turns, wall time and usage
(`input`, `output`, `cacheRead`, `cacheCreation` tokens). `workass_list_forks`,
`workass_cancel_fork_child` and `workass_retry_fork_child` complete the set.
Mutations take a caller-stable `operation_id`; the fork store keeps its own
receipts, so a replay returns the original fork point, run or retry.

```sh
"$WORKASS_TOOLS_COMMAND" tools call workass_fork_run <<'JSON'
{"fork_point_id":"FP_ID","operation_id":"frames-batch-1",
 "children":[{"label":"frame_00","message":"Review frame 00; write /tmp/out/f00.json",
              "attachments":["/abs/frame_00_r01.png","/abs/frame_00_r02.png"]}]}
JSON
```

Only providers with a native session fork can do this; each provider's
`fork` entry in `workass_agent_catalog` says whether it can and why. Claude
uses `claude --resume <seal> --fork-session`. These rules keep the prefix in
the provider's prompt cache (all measured 2026-10-15):

- The fork point is sealed with two short resumed turns. A copy of the live
  parent re-renders some content differently once resumed; the second turn
  is itself resumed, so its cache entry sits exactly where children share.
- Permission mode, effort and extra directories (`add_dirs`) belong to the
  fork point and apply to the seal and every child. A child launched with
  different flags sends a different prefix.
- Children run with no MCP servers, so every request has the same tool list.
- The API looks back only ~20 content blocks from its cache breakpoint.
  Image labels ride in the message text, not as one block per image, and
  throwaway children skip session persistence (a persisted session adds one
  note block per image). Keep to 16 images per throwaway child, 8 per kept
  child; runs above that carry a `warnings` entry.

Images are validated before any child starts: regular PNG/JPEG/WebP/GIF
files, at most 5 MiB each and 20 MiB per message, longest side at most
`max_image_edge_px` (default 1000). Oversized images are rejected, never
resized. Claude Code itself re-encodes PNG inputs above ~500 KB to JPEG at the
same pixel size. `workass_send_chat_message` accepts the same `attachments`
for ordinary turns (not with `steer`).

Children are throw-away by default: no native session is kept, and Workass
keeps a compact transcript (text, tool calls, per-call cache usage; no image
bytes) for `retention_days` (default 7) plus the receipt. `retention:"keep"`
keeps everything. Live children are not restart-durable: a daemon restart
settles them as failed with that reason.
