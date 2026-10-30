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
`shell-settings.json` selects either; the shell starts on the native-view host
and switches to the webview host only when the app renderer asks for it, so an
older renderer keeps working, and pages made before that stay native views
until they are replaced) the page is the guest of a `<webview>` in the app window and captures
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
`format:"compact"` renders one line per element with short refs (`c12`, frames
`f1`) aliased in the same observation, so they work with that `snapshot_id`
for click, type, scroll and read; they are kept apart from outline refs, and a
compact ref without its `snapshot_id` is refused instead of reaching an
outline element. Compact output folds an image into its link,
drops rows that only repeat text (list items, paragraphs, static text), and
budgets interactive lines first, then page text, then the outline.
`since_snapshot_id` diffs against an earlier compact snapshot of the same
document (`+` added, `~` changed, `-` removed, with unchanged refs still valid)
and falls back to a full compact snapshot with `incremental.reason`
(`document_changed`, `base_snapshot_unknown`, `too_many_changes`).

`format:"outline"` is the page as nested lines built from Chromium's
accessibility tree of every frame the tab owns, cross-site frames included and
placed under their `<iframe>`: role, name, current value (every password,
one-time-code, card or secret-named field `[redacted]` by the json snapshot's
rule — type, autocomplete, name and id, read with `DOM.describeNode`; a field
that cannot be described counts as secret; diffs inherit it), state (checked, expanded, selected, disabled,
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
`download_finished`, `download_blocked`, `tab_opened`, `popup_blocked`,
`beforeunload_accepted`, `file_chooser_opened`)
and `changes`: a bounded outline diff, or after a navigation the new page's
outline (first part, with `next_cursor`). The page is settled first, for as
long as something is happening and no longer: a navigation reaches a usable
document; then, if nothing happened, two frames (32 ms) of quiet; once
something happened (DOM mutations, input/change/focus/toggle events, requests
the action started, frames attaching or loading), 80 ms of quiet after the
last of it; at most 1.5 s. Requests are watched only while an action settles
(WebSockets, event streams and document loads excepted). The page is always
read again after an action, so `changes.none` comes only from comparing two
reads; the read before is reused only when it is under a second old and
nothing has happened since, and an explicit snapshot always reads the page as
it is now. A batch reports once, for all of its actions; a dialog stops it
where it opened (`batch.completed_indexes`, `dialog_index`,
`unexecuted_indexes`) and nothing after it runs. Each tab operation has a
deadline (25 s; navigation and batch 28.5 s): one that does not finish is
reported and the tab's queue moves on, and `workass_browser_list` shows
`stuck_since_ms` for it. An action's waits and reads fit in what is left of its
deadline: a navigation that runs out reports `changes.loading` (with
`loadingNote`) instead of failing. A navigation whose page has not committed
within 20 s fails and says so; back and forward with nowhere to go do
nothing. Commands on the top frame are bounded like a frame's.

Nothing Workass runs in a page runs in the page's own JavaScript world: every
script, observer, listener and timer of the browser tools lives in an isolated
world of its own (CDP `Page.createIsolatedWorld`, one per frame and document),
so a page can neither see that an agent is reading it nor change what the agent
reads. The one exception is a code editor (Monaco, CodeMirror): one fixed
function, called in the page's world on the element the agent world found,
reads its text for snapshots and, for `workass_browser_type`, focuses it and
selects or deletes its text through the editor's own API. Input is
bounded the same way: an input event the page does not take within 5 s fails
the call; a press an action left down is released when the action ends, at
its deadline, or when the app loses focus; a drag that did not finish is
cancelled (`Input.cancelDragging`); while an operation is past its deadline the
tab is no longer held focused and painting for it. A click or a menu key on a
one-line `<select>` is refused with `native_menu` (its native menu would hold
the window's input; choose with `workass_browser_select`), and while an agent
action runs a file chooser the page opens is intercepted
(`Page.setInterceptFileChooserDialog`) and reported as `file_chooser_opened`
instead of opening a native panel.

`workass_browser_select` chooses options of a native `<select>` by values,
labels or indexes (input and change events); a custom dropdown is clicked
instead. `workass_browser_hover` moves the pointer with trusted input.
`workass_browser_drag` drags to an element or by `delta_x`/`delta_y` with a
trusted press, moves and release; an HTML drag and drop goes through
Chromium's drag events (`Input.setInterceptDrags`), never an OS drag session;
both ends must be in one frame (`unsupported` otherwise); a target that cannot
be hovered or dragged is reported by status like a click's. A scroll target
with nothing to scroll fails with `not_scrollable`. `workass_browser_upload`
sets a file input's files (`DOM.setFileInputFiles`) from absolute paths the
agent passes: at most 10 files and 10 MB, never a symbolic link, a path reached
through a linked folder (the real path must be the path given, apart from
macOS's `/tmp`, `/var` and `/etc`) or a file with other hard links, and never a
credential-like name or folder, checked on the real path (`.ssh`, `.aws`,
cloud and git CLI sign-ins, browser profiles and cookies, Claude, Codex and
Workass data, private keys, tokens, `.env` and the like).
`workass_browser_dialog` answers the open alert, confirm or beforeunload
(`Page.handleJavaScriptDialog`); while one is open other page tools fail with
its message, and with none open it fails with `no_dialog`. Electron answers `prompt()` itself with null. An agent's
navigation leaves a page whose beforeunload asks to stay. `workass_browser_evaluate`
runs page JavaScript in the page's own world, where the page can see it (8 KiB
source; script that runs without pausing is stopped after 10 s and the call
stops waiting for a promise after 12 s, though work it started can go on; JSON
result up to 16 KiB as `page_data`, redacted); a frame with no script context
yet fails with `unavailable` rather than running in its parent. It can change
the page, so it is a mutation with an `operation_id`, never labelled
read-only. `workass_browser_close` closes a tab
one of the chat's pages opened, or resets the chat's own page to a fresh blank
page in the same profile (a new tab id), shown in place when the person was
looking at it; a page's popup tabs close with it. It fails for a tab id that
is not the chat's.

Links (`target=_blank`) and `window.open` popups of a page an agent is driving
(not in front of the person — a popped-out page is in front — or acted on by an
agent call in the last 15 s; reads do not count) open as background tabs of the
same chat, listed with `opened_by_tab_id` (at most 8). Chromium creates the
popup itself (`createWindow`) and its own webContents becomes the tab, so
`window.opener`, `postMessage` and POST targets keep working (sign-in, payment
and 3-D Secure windows). A page's popup tabs close with it and when it is
reset. Their downloads save into a folder of the chat's own under the
profile's data directory (`browser-downloads/chat-<hash>/`) and are listed per
tab, at most 10 a minute, 200 files and 2 GB per chat folder (beyond that a
download is cancelled and reported as `download_blocked`); a download the
person starts in a page they are looking at keeps Electron's save dialog.

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

## Delegating to another machine

`workass_spawn_subagent` takes a `machine_id`: the machine that RUNS the child,
while the calling chat keeps owning it. The child is listed under the calling
chat with its host on the row, its progress and its outcome arrive there, and
nothing appears under any other conversation. Pass `cwd` as an absolute path on
that machine and any explicit `provider_id` / `model_id` from THAT machine's
`workass_agent_catalog`; `fork` is unavailable, because a native session fork
only runs where its session file is.

A machine-tagged `tab_id` + `chat_id` is the other thing, and still works: it
makes the child a subagent of that other chat, owned there. Choose by who
should be told, not by where it runs.

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

## Long jobs that outlive everything: `workass run`

A subagent is an agent. A **process child** is an ordinary OS process — a
training run, a build, a long sync — supervised by Workass's own binary and
owned by a chat. Use it when the work is a command rather than a conversation,
and especially when it must survive things an in-process child cannot: a daemon
restart, a Workass self-update, and your own session ending.

```sh
workass run --label "retrain ranker" --resource gpu0   --max-runtime 6h --stale-after 40m   --inputs /data/shards,/data/labels.jsonl --min-free-disk 50G   --operation-id retrain-ranker-2026-10-30   -- python train.py --epochs 8
```

Nothing about that call polls, and nothing about it needs a watcher:

- **Preflight refuses in seconds.** Declared inputs must exist and be
  non-empty, symlink targets must resolve, the command must be on PATH, the
  output directory must be writable and the disk floor met. A missing input is
  a refusal before the job queues, not a crash in hour three. A refused run
  creates no row.
- **`--resource` is a FIFO queue, visible to anyone.** Waiters report their
  position; the hold is tied to the supervisor process, so however the holder
  dies the resource frees. `--max-hold` is reported, never enforced by a kill.
- **The job runs in its own process group** (a `systemd --user` scope on Linux,
  a Job object on Windows), so it is not a child of the daemon.
- **Every transition is written to disk before it is reported**, so a daemon
  that is restarting or updating itself misses nothing.
- **`--operation-id` is one key for both sides.** The same id is the same job:
  a repeat cannot create a second one.
- **The chat is told only what is actionable**: settled, failed, `timed_out`,
  `lost`, a declared stall once, and a decision. A queue position and a start
  are visible by reading, which is when they matter. A job you cancelled never
  notifies you.
- **`--owner <global id>`** sends the outcome to a chat on another machine, so a
  job on the box with the GPU reports into the chat that asked for it.

To START one on another machine, where you have no shell, ask that machine:

```sh
"$WORKASS_TOOLS_COMMAND" tools call workass_start_process_child <<'JSON'
{"machine_id":"m-91be04","label":"retrain ranker",
 "host_tab_id":"M~m-91be04~tab-1","host_chat_id":"M~m-91be04~chat-1",
 "cwd":"/srv/work","command":["python","train.py","--epochs","8"],
 "env":{"CUDA_VISIBLE_DEVICES":"0"},"resource":"gpu0",
 "max_runtime_ms":21600000,"operation_id":"retrain-ranker-2026-10-30"}
JSON
```

That machine runs its own `workass run` — same preflight, same queue, same
outcomes — with a credential minted for this one job and an environment built
from nothing but PATH, HOME and the `env` you named, so no daemon's environment
is lent to the job. `host_tab_id` + `host_chat_id` name a chat THERE that holds
the job's local row; the chat that called owns the outcome and gets the notice.
There is no `command` shell: the program and each argument are separate
strings. On your own machine the tool refuses and points at `workass run`,
which is the right tool when you already have a shell.

Read and control it from any machine in the fleet:

```sh
"$WORKASS_TOOLS_COMMAND" tools call workass_list_process_children <<'JSON'
{"machine_id":"m-91be04"}
JSON

"$WORKASS_TOOLS_COMMAND" tools call workass_read_process_child <<'JSON'
{"work_id":"xw6f12aa...","tail_chars":4000,"after_seq":3}
JSON
```

Reading never notifies anybody and never grows a log: the output is a file and
you asked for a bounded tail of it. A finished job leaves the live list and its
outcome lives in `workass_list_spawned_work_receipts`.
`workass_cancel_process_child` writes the stop the supervisor acts on — a stop
file, then SIGTERM to the whole group, then a kill after the grace — and the
stop is applied even if it was issued while the machine was unreachable.

A pid that is gone with no recorded outcome is **`lost`**, which means UNKNOWN,
not dead: the work may still be running and nobody is left who could say. It is
never reported as finished, and never as failed.

`workass_register_external_work` is unchanged and still tracks a detached lane
with no supervisor. It gets no preflight, no resource queue, no notice and no
`lost` verdict, because nothing is watching it — which is the reason to prefer
`workass run`.

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

## Driving chats on another fleet machine

Every Workass tool reaches exactly one machine, and the ids say which. A chat
mounted from another fleet machine is returned by `workass_list_chats` with a
machine-tagged `tabId`/`chatId` pair (`M~<machineId>~<localId>`) and its
`machineId`. Pass that pair back and the tool runs on the machine that owns the
chat; pass an untagged pair and it runs here, byte-identically to before.

`cmd/workass/agent_remote_parity.go` is the single authority for this. Each
control method is classified once:

- **chat-scoped** — addressed by the exact pair, so a tagged pair selects the
  owning machine: read, diagnostics, configure, rename, settle, focus, delete,
  send, cancel, subsession, `spawned_work.*`, `fork.create`, `fork.list`, and
  the whole subagent family (spawn, list, receipts, wait, wait_many, message,
  retry, cancel, permission decisions).
- **machine-scoped** — no chat to address, so they take `machine_id`:
  `workass_create_chat` and `workass_agent_catalog`. The catalog is what *that*
  machine can actually run: its providers, models, efforts and native
  permission modes. Read it before you configure or create a chat there.
  `machine_id` naming this machine is simply local.
- **fork-scoped** — addressed by the `fork_point_id` / `run_id` / `child_id`
  the owning machine minted: `fork.run`, `fork.wait`, `fork.cancel`,
  `fork.retry`.
- **local-only** — they act on the calling chat, on this machine's filesystem,
  or on the person at this controller, and each says so in its own words:
  `workass_ask_user_question`, `workass_render_html`, `workass_host_artifact`,
  the scheduled-message tools, external-work registration/settling, and the
  obligation read.

The subagent and subsession tools take an optional `tab_id` + `chat_id`. Absent,
they mean the calling chat, as they always did. Tagged, they name the owning
chat on another machine — which is how one agent delegates there. Locally an
explicit pair may only be the calling chat, so no agent gains reach into a
sibling chat.

How it travels: the local daemon resolves the machine and hands the call to the
renderer's machine router, which strips the ids back to what that daemon minted
and invokes `agent:control` on it. That channel is additive to the frozen
`invoke/reply/event` protocol and is authorized exactly like `chat:delete` or
`job:start` — the paired controller link, then the durable actor fence, then the
same short-lived actor owner key the local path mints. Remote validation is
therefore the same Go code as local validation: an unknown model, a `cwd` that
is not a directory *there*, a running turn that forbids a provider change, and
operation-id idempotency all behave the same.

Four boundaries worth knowing:

- **Remote waits poll.** Invokes share one ordered lane per client, so a
  blocking cross-machine wait would stall every other operation against that
  machine. `workass_wait_subagent`, `workass_wait_subagents` and
  `workass_wait_fork_run` are bounded to a short window on a remote chat and
  return the same shape a local wait returns when its window elapses. Re-ask,
  or use `workass_schedule_message` to come back later.
- **No remote steer.** `workass_send_chat_message` accepts `auto` and `queue`
  on a remote chat; `steer` is refused so a lost route reply cannot duplicate
  live input.
- **No remote attachments.** Attachment paths are read on the machine that runs
  the chat. Host the file with `workass_host_artifact` on the machine that has
  it and send the link.
- **Remote create does not inherit.** There is no calling chat on the target to
  inherit from, so selection is explicit or that machine's first ready provider,
  and `cwd` must be an absolute directory on that machine (`inherit` is
  rejected).

An unreachable machine fails visibly. A tagged destination is binding: it never
degrades into the same operation against this machine.
