# Update permission rule moved to AGENTS.md, programmed update on a rig

Owner order 2026-10-31. Rig: a packaged app at ~/Applications/WorkassUpdateRig2.app, the real
0.1.255 bundle with this tree's daemon built at version 0.1.255 and a rig profile sealed in, feed
holding the real 0.1.256 re-sealed. Model: Haiku 5.5. No real machine was updated.

- Human message on the rig: permission for a programmed update of this machine in 45 seconds.
- 23:58 the agent called workass_schedule_message and did not apply in that turn.
- The scheduled message came due; the agent read targets and status and called
  workass_apply_update for the rig's machine id only, operation
  scheduled-update-this-machine-2026-10-31-apply, accepted 23:59:16Z.
- Receipt: {'phase': 'healthy', 'previousVersion': '0.1.255', 'targetVersion': '0.1.256', 'installedVersion': '0.1.256', 'activated': True, 'rolledBack': None, 'error': None}

Live injected text, this tree's daemon, one Haiku turn, counts of the old rule's phrases:
instructions.md 0, guide.md 0, tools guide 0, tools list 0, daemon binary 0.

Tool description served live:
Apply the exact machine's already-discovered Workass update through its transactional updater. When it may be called is the owner's rule in the AGENTS.md at the root of the Workass project. Pass the exact machine id and versions returned by workass_list_update_targets and workass_get_update_status, the matching authorization text, and one caller-stable operation_id. A transport replay with the same operation_id is read back and cannot start the update twice.

Trap: the rig's renderer found work-laptop by beacon even with the daemon bound to localhost, so
the agent saw it in workass_list_update_targets. It was forgotten on the rig afterwards; forget it
before the first turn next time. Trap: the tools guide reads WORKASS_TOOLS_GUIDE from the caller's
environment, so running it from a real chat's shell reads the real installed guide.
