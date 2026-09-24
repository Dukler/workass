# Useful tests in ten seconds

User request, 2026-09-24: preserve useful regression coverage, make the full
test suite complete in at most ten seconds (faster is better), and delegate
implementation to Luna in small, closed tasks. Ten seconds is a ceiling, not
a duration to aim for or a process-killing deadline. This spec grants
test-infrastructure changes only.

## Acceptance contract

- Provide one command for the full automated suite: renderer tests, shell
  tests, `go test ./...`, and all `scripts/tests/*.test.mjs` tests, including
  release-pipeline contracts and tests added for this change. Inventory other
  automated entry points before claiming completeness. Existing release checks
  remain required. Do not describe today's narrower gate or a selected subset
  as the full suite. Benchmarks and external manual probes must be identified
  separately; do not reclassify ordinary tests to exclude them.
- Target **at most 10.00 seconds total elapsed** on the current Mac development
  host, from command invocation through child cleanup. Include orchestration,
  test-process startup, and Go's incremental compilation. Dependencies and the
  normal compiler cache may already exist; test-result reuse may not. Always
  use `-count=1` for the acceptance measurement. Disclose cold-build timing
  separately if measured; do not promise arbitrary machines meet this target.
- Typechecking, production bundle creation, standalone build/vet, packaging,
  and uploads remain required where currently required and retain separately
  labelled timings. They are not test execution. Report both suite time and
  complete gate time; never call a longer gate a ten-second gate.
- No dropped assertions, new skips, sampling, cached pass receipts, automatic
  flaky retries, shorter safety deadlines, or ten-second process termination
  as a substitute for actual speed. An over-budget run must finish and report
  the failure to meet the performance target, alongside its correctness result.
- Retain test names/cases where practical. Any consolidation needs an explicit
  old-case-to-new-assertion mapping. Preserve existing platform skips; report
  them, and do not silently omit renderer tests when dependencies are missing.

## Evidence already available — do not repeat discovery

- `/tmp/workass-gate-profile-20260924.log`: renderer tests + typecheck + bundle
  12s, shell tests 3s, build 0s, vet 2s, Go tests 90s; ACP package 79.103s.
- `/tmp/workass-acp-parallel4-20260924.json`: fresh ACP package 64.163s with
  four concurrent tests, but four failures. Increasing concurrency alone is
  not a fix. The current gate uses `-p=2 -parallel=2`.
- That ACP JSON contains 515 top-level results, roughly 106s summed test
  elapsed time (overlapping, not wall time). Provider-update tests account for
  26.7s and detection tests 10.8s. Failure times inflate these numbers.
- `TestProviderDetectionAllowsFullInitializeAndSessionBudgets` waits a real
  2.6s twice, to prove initialize and session/new have independent deadlines.
- Update fixtures launch shell processes and local HTTP servers and have
  fragile two-second event deadlines. Their retry backoffs are already stubbed
  to milliseconds: inspect the execution path rather than shorten them again.
- `TestChatLifecycleDoesNotRunAutomaticGit` takes about 5.93s across four
  semantic combinations and repeated provider turns; retain those combinations.
- `internal/acp/turn_startup_timing_test.go` already uses `testing/synctest`.
  Its apparent long sleeps are virtual, not a performance problem.
- The recently added `scripts/test-focused.mjs` only kills selected commands
  after a budget; its 2.64s Pi result does not establish whole-suite performance.

## Implementation design

1. Replace wall-clock fixture delays with explicit synchronization and virtual
   time for timing/state-machine assertions. Use the existing Go toolchain and
   stdlib `testing/synctest` where execution can stay inside its clock domain.
   Do not wrap external-process I/O in virtual time and assume it advances.
2. Keep real ACP stdio/process integration coverage for serialization,
   initialization, prompt completion, cancellation and cleanup. Separate
   repeated timing/logic cases from the real boundary only when equivalent
   assertions still exercise the production path. Do not test a copied
   implementation instead of production behavior.
3. Reuse immutable compiled fixture executables within a test invocation;
   isolate mutable state, ports, files, environment, and process lifetimes per
   case. Wait for observable readiness/completion, not arbitrary sleeps.
   Changes involving package globals or `t.Setenv` must remain serial until
   genuinely isolated. Do not add parallelism around them blindly.
4. Run independent renderer, shell, Go, and script-test processes
   concurrently using a small dependency-free runner. Preserve child exit
   codes, complete per-suite logs, failure summaries, cleanup on interruption,
   and test counts/timings. Use a monotonic clock. Avoid unbounded CPU fan-out.
5. Keep gate preparation/build ordering where outputs are dependencies. In
   particular the rebuilt renderer must still match the committed embedded
   snapshot before compiling embedded Go assets. Execute each test suite only
   once per canonical gate; do not introduce nested/duplicate release tests.
6. Replace the mandatory timeout-wrapper instruction in AGENTS with the actual
   suite command and focused commands for iterative checks. Preserve existing
   trust in scoped handoff evidence and the single acceptance-review policy.
   Keep referenced script names compatible; do not add agent time budgets.
7. On macOS, provision one disposable APFS RAM filesystem for fixture data.
   Measured filesystem contention kept a correct full run at 26.405s;
   the same chat-package assertions took 14.41s on the ordinary temporary
   filesystem and 0.375s on the disposable volume. Keep all real filesystem
   operations, sync calls, process boundaries, and recovery assertions.
   Keep compiler caches and pinned executables on the host filesystem.
   Allocate only a new RAM device, verify its physical-store ownership before
   using its mount, and clean up only that exact device. Include provisioning,
   fixture removal, and detach in the suite's total elapsed time, including on
   interruption. Fail visibly if provisioning or cleanup fails. Other platforms
   use fresh ordinary temporary directories; the ten-second acceptance claim
   remains specific to the measured Mac host. No pre-mounted volume or saved
   test results may be required to meet the target.

## Closed implementation handoffs

These are two independent first passes, not permission for a suite-wide
rewrite. Each Luna commits its bounded result and returns promptly. No
publication, activation, runtime reinstall, or application feature changes.
The coordinator reviews each diff once and merges the independent commits.
If the combined suite still exceeds ten seconds, use its measured slowest
remaining cases to issue another small prompt; never quietly expand a lane.

### Luna A — slow ACP fixture families

Allowed files:

- `internal/acp/provider_detection_test.go`
- `internal/acp/updates_test.go`
- `internal/acp/checkpoint_test.go`
- at most one new `internal/acp/test_fixture_timing_test.go` helper

Remove avoidable execution costs in these three measured families using the
design above. Start with detection's two real waits and updater fixture
startup/cleanup. Preserve redaction, update replay/idempotency, double invoke,
resolved executable, terminal receipt, separate probe budgets, and no automatic
Git assertions. Existing real-process error/cancellation assertions stay real.
Do not alter production files or provider defaults. If a correct optimization
requires a production test seam or shared helper outside this manifest, return
the exact proposed signature/path and why; that gets its own follow-up prompt.

Reuse the existing timing JSON as baseline. Run the changed tests once after
editing, then one fresh full ACP run with JSON output to demonstrate no lost
cases and expose remaining costs. A failure justifies a targeted correction,
not unrelated cleanup. Do not run the entire release/build pipeline.

### Luna B — one measured, concurrent suite entry point

Allowed files:

- `scripts/test-suite.mjs` (new)
- `scripts/tests/test-suite.test.mjs` (new)
- `scripts/gate.sh`
- `scripts/release/prepare-input.sh`
- `scripts/tests/release-pipeline.test.mjs`
- `AGENTS.md` (fast-handoff test-command paragraph only)

Implement the runner for the full suite and wire it into the gate without
duplicating the release-contract suite in prepare-input. Inspect existing
release receipt semantics and preserve exact-commit validation; do not weaken
or fabricate receipts. Include the new runner tests in the matrix. Preserve
legacy script paths and existing correctness/failure reporting contracts.
Initially retain Go's current safe concurrency settings; increasing them
requires concrete fixture isolation and passing evidence in a later prompt.

Report test correctness and measured performance independently. A complete
correct run over ten seconds must visibly fail the performance target, but do
not impose that still-unmet target as a release-blocking policy change in this
first infrastructure handoff. Leave existing release failure semantics intact
until the optimized combined suite meets acceptance. Do not claim this runner
alone achieves ten seconds. Do not run the slow ACP suite to test the runner:
use injected fixture commands to check concurrent execution, nonzero exit and
spawn-error propagation, complete log capture, over-budget reporting without
early termination, and interrupt cleanup. Avoid narrow/flaky timing thresholds
in these runner tests. Run release-contract tests once for gate integration.

## Combined verification and handoff

After merging the two commits, run the actual fresh complete suite. Only once
it first meets ten seconds, repeat twice to check stability (three consecutive
correct, uncached runs, each <=10.00s). If it misses, stop rerunning; use the
single measured result for the next bounded bottleneck fix. Compare case
inventory and skip counts against the original suites. No aggregate passing
count can substitute for preserved behavior assertions.

Each handoff supplies its commit, changed behavior, exact command and one-line
outcome, elapsed time, case/skip counts, full log path, and any remaining gap.
Do not send a long narrative or repeat broad repository discovery. The ten-
second goal remains unachieved until the combined measurements prove it.

All tests use fresh temporary test state and deterministic mock fixtures. No
production data or processes. Existing PORT-SPEC and provider-lane contracts
remain binding; no dependency additions or forbidden build-script edits.
