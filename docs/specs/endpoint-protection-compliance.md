# Windows endpoint-protection constraints

Authority: the user's 2026-09-25 instruction for the San-laptop Workass
endpoint-protection work. This is a binding constraint on Windows provider
tool launch and related process/shortcut handling; it does not authorize
changes to LAN peer discovery or updater behavior.

Production Windows provider tools use the signed bundled Node runtime and the
packaged `workass-tools.mjs` client, invoked by `workass-tools.cmd`. Do not
launch the unsigned Workass Go PE as a transient shell CLI. This signed Node
shim is the sole new Windows-layout exception. A legacy layout without it uses
the in-process daemon tools entrypoint. Preserve the existing tool catalog,
abilities, authenticated request, context ownership, redaction, security
checks, and package compatibility. The shim changes the launcher, not the
Workass tools contract or frozen LAN `invoke/reply/event` protocol.

RSS sampling and the raw-MCP Docker guard must not run recurring `tasklist` or
PowerShell process-query loops. Shortcut discovery and icon repair must not
sweep through PowerShell or WScript, and must not launch `ie4uinit`. Do not
replace these prohibited mechanisms with another recurring external process
query or shortcut sweep.

On the San-laptop Windows development machine, do not build or run unsigned Go
executables or tests. Use the Mac development machine or Windows CI for Go
builds and tests.

These constraints do not guarantee CrowdStrike approval and do not establish
that all endpoint-protection alerts are resolved. LAN peer-discovery and
updater behavior are unchanged by this document; related design options remain
outstanding. Do not infer a peer-discovery or updater design from this rule.
