# bug-20261110-12916e73: a timer's job-start credential on another machine

Rig: `RIG_SDK=<claude-agent-sdk sdk.mjs> node harness/rig.mjs <daemon binary>`. It runs two real daemons
on one Mac (the owner on loopback, the host on the LAN address, reached only through the fleet mesh) and
a real Claude Haiku chat at full access on the owner. The chat issues a job-start credential. Every
check after that calls the real CLI with nothing but `WORKASS_TOOL_CONTEXT=<token_file>`, which is how
the reporter's launchd timer calls it.

Before the fix (v0.1.284): `workass_issue_job_credential` answered `token_file: "[redacted]"` and
`credential_id: "[redacted]"`, because the tool-output redactor masks any key named like
`token` or `credential`. With the file found by hand, `workass_start_process_child` and
`workass_list_process_children` were both refused with "authorizes only workass_register_process_child".

After the fix, 2026-11-10, all 11 checks pass:

| check | result |
|---|---|
| issue answers the real token_file and credential_id | PASS |
| remote start with the job-start credential | PASS |
| list on the host shows it running (own jobs only) | PASS |
| unaddressed list includes it, with host_machine | PASS |
| read | PASS |
| cancel a second job | PASS |
| workass_list_chats / send_chat_message / spawn_subagent refused | PASS |
| a local start points at `workass run --token-file` | PASS |
| the job's notice reached the issuing chat (`<status>done</status>`, exit 0) | PASS |

The unit test `TestATimersCredentialStartsListsReadsAndStopsItsChatsJobsOnAnotherMachine` also covers
what the rig cannot easily stage: another chat's job on the same host is neither listed, read nor
stopped, a resource queue shows only the chat's own entries, and a revoked credential starts nothing.
