package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"workass/internal/acp"
	"workass/internal/procchild"
)

// `workass run` — the supervisor that ships with the product.
//
// docs/specs/fleet-runtime.md §13 P4, specified in
// docs/specs/delegation-fabric.md §5. A new subcommand of the existing binary,
// so every peer has it by construction: there is nothing to install on a
// machine that already runs Workass, which is what makes "run it over there"
// a real sentence rather than a deployment project.
//
// It is invoked BY THE AGENT, with the agent's own environment, which is how
// the daemon keeps its promise never to spawn work (§9): no environment and no
// secret is ever captured inside the daemon, and the child is a child of this
// process rather than of the service.
//
// usage:
//
//	workass run --label L [--resource gpu0] [--max-runtime 3h]
//	            [--stale-after 40m] [--inputs PATH,...] [--min-free-disk 50G]
//	            [--owner wa:m-7f3ad2/chat/app-chat-1792187130561]
//	            -- cmd args...

func runProcessChildCommand(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("workass run", flag.ContinueOnError)
	flags.SetOutput(stderr)
	label := flags.String("label", "", "what this job is, in the words its chat will show")
	role := flags.String("role", "", "work (default: its end is news) or service (it is expected never to finish)")
	resource := flags.String("resource", "", "named resource to hold exclusively, queued FIFO behind other holders")
	maxRuntime := flags.Duration("max-runtime", 0, "stop the job after this long and settle it timed_out; the clock starts at exec, not in the queue")
	staleAfter := flags.Duration("stale-after", 0, "raise ONE stalled notice if the output stops growing for this long; the job keeps running")
	maxHold := flags.Duration("max-hold", 0, "how long this job expects to hold its resource; exceeding it is reported, never killed")
	inputs := flags.String("inputs", "", "comma-separated paths that must exist and be non-empty before anything starts")
	minFreeDisk := flags.String("min-free-disk", "", "refuse to start unless this much space is free where the output is written, e.g. 50G")
	owner := flags.String("owner", "", "global fleet id of the chat or turn owed this job's outcome; defaults to the calling chat")
	operationID := flags.String("operation-id", "", "stable id for this job; a repeat with the same id is the same job, never a second one")
	tokenFile := flags.String("token-file", "", "file holding this run's Workass credential; defaults to WORKASS_RUN_TOKEN_FILE, then WORKASS_TOOL_CONTEXT")
	stateDir := flags.String("state-dir", "", "daemon state directory; resolved from the credential or WORKASS_DATA_ROOT by default, and never from the working directory")
	unreported := flags.Bool("unreported", false, "run with no chat told: supervise, queue the events and print the receipt, but report to no daemon even if a credential is in the environment")
	workDir := flags.String("cwd", "", "working directory for the job")
	outputFile := flags.String("output-file", "", "absolute path for the job's combined output; defaults to this machine's external-work directory")
	quiet := flags.Bool("quiet", false, "print only the final JSON receipt")
	flags.Usage = func() {
		fmt.Fprint(stderr, `usage: workass run --label L [flags] -- cmd args...

  --label L                what this job is, in the words its chat will show
  --resource NAME          hold NAME exclusively; waiters queue FIFO and the
                           queue is visible to anyone who lists it
  --max-runtime DUR        settle timed_out after DUR; the clock starts at exec
  --stale-after DUR        one stalled notice if the output goes quiet for DUR
  --inputs A,B,C           refuse in seconds if any input is missing or empty
  --min-free-disk 50G      refuse unless that much space is free
  --owner GLOBAL-ID        the chat or turn owed the outcome, on any machine
  --operation-id ID        the same id is the same job, never a second one
  --token-file FILE        this run's credential; never passed on a command line
  --unreported             supervise with no chat told, and say so

The job runs apart from the daemon, so it survives a daemon restart or a
self-update: on Linux under a systemd service the supervisor moves itself into a
transient user scope, because a process group does not escape a unit's cgroup. A
machine that cannot do that says NOT ISOLATED here and in the job's row, and the
update status names the job. Every transition is REPORTED to
this machine's daemon and acknowledged by it; the outbox (state/process-outbox.db)
is the write-ahead copy, so a daemon that is down misses nothing and the backlog
drains in order when it answers. A supervisor that cannot report says so on
stderr and in its receipt, and keeps trying. A supervisor pid that is gone with
no settled event means lost, not running.

`)
		flags.PrintDefaults()
	}
	// Everything before `--` is flags; everything after is the command. Go's
	// flag package already stops at `--`, which is why the command needs no
	// quoting and no escaping.
	if err := flags.Parse(args); err != nil {
		return 2
	}
	command := flags.Args()
	if len(command) == 0 {
		flags.Usage()
		fmt.Fprintln(stderr, "workass run: a command is required after --")
		return 2
	}

	// Before anything else, and before the credential is consumed: a move out of
	// a service's cgroup re-execs this same pid with these same arguments. The id
	// is settled here so the scope and the job agree on it across that exec.
	id := strings.TrimSpace(*operationID)
	if id == "" {
		var err error
		if id, err = randomOperationID(); err != nil {
			fmt.Fprintf(stderr, "workass run: %v\n", err)
			return procchild.ExitSupervisorFailed
		}
		args = append([]string{"--operation-id", id}, args...)
	}
	procchild.LeaveCallerProcessGroup()
	if childID, err := procchild.IDForOperation(id); err == nil {
		if executable, err := os.Executable(); err == nil {
			procchild.EscapeServiceCgroup(childID, executable, append([]string{"run"}, args...), stderr)
		}
	}

	resolvedStateDir, reporter, err := resolveRunContext(*tokenFile, *stateDir, *unreported, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "workass run: %s\n", acp.RedactSensitiveText(err.Error()))
		return procchild.ExitSupervisorFailed
	}
	outbox, err := procchild.OpenOutbox(resolvedStateDir)
	if err != nil {
		// Loud and first. An unusable outbox is not a degraded mode: it is a
		// supervisor that cannot write down what it is about to witness.
		fmt.Fprintf(stderr, "workass run: %s\n", acp.RedactSensitiveText(err.Error()))
		return procchild.ExitSupervisorFailed
	}
	defer outbox.Close()

	floor, err := procchild.ParseBytes(*minFreeDisk)
	if err != nil {
		fmt.Fprintf(stderr, "workass run: %v\n", err)
		return 2
	}

	diagnostics := io.Writer(stderr)
	if *quiet {
		diagnostics = io.Discard
	}
	spec := procchild.Spec{
		StateDir: resolvedStateDir, OperationID: id, Label: *label, Role: *role,
		Resource: *resource, MaxRuntime: *maxRuntime, StaleAfter: *staleAfter, MaxHold: *maxHold,
		Inputs: splitInputList(*inputs), MinFreeDisk: floor, Owner: *owner,
		Dir: strings.TrimSpace(*workDir), Command: command, OutputFile: strings.TrimSpace(*outputFile),
		Diagnostics: diagnostics, Reporter: reporter, Outbox: outbox,
	}
	result, err := procchild.Run(spec)
	if err != nil {
		fmt.Fprintf(stderr, "workass run: %s\n", acp.RedactSensitiveText(err.Error()))
		return procchild.ExitSupervisorFailed
	}
	// The receipt says what was reported, not just what happened. A run that
	// finished perfectly and could not tell anybody is a different outcome from
	// a run that finished perfectly, and the receipt is where a caller standing
	// here finds out which one it got.
	receipt := map[string]any{
		"workId": result.ChildID, "operationId": id, "outcome": result.Outcome,
		"exitCode": result.ExitCode, "elapsedMs": result.Elapsed.Milliseconds(),
		"outputFile": result.OutputFile, "registered": result.Reporting.Delivered,
		"reported": result.Reporting.Delivered, "stateDir": resolvedStateDir,
		"outbox": outbox.Path(),
	}
	if result.Reporting.Unreported {
		receipt["reported"], receipt["unreported"] = false, true
	}
	if result.Reporting.Pending > 0 {
		receipt["pendingEvents"] = result.Reporting.Pending
	}
	if result.Reporting.Gap > 0 {
		receipt["reportGapMs"] = result.Reporting.Gap.Milliseconds()
	}
	if result.Reporting.LastError != "" {
		receipt["reportError"] = acp.RedactSensitiveText(result.Reporting.LastError)
	}
	if result.Reporting.Refused {
		// Named separately because the action is different: an unreachable
		// daemon is waited for, a refusing one is updated.
		receipt["daemonTooOld"] = true
	}
	if len(result.Refusals) > 0 {
		refusals := make([]map[string]string, 0, len(result.Refusals))
		for _, refusal := range result.Refusals {
			refusals = append(refusals, map[string]string{
				"check": refusal.Check, "subject": refusal.Subject, "reason": refusal.Reason,
			})
		}
		receipt["preflight"] = refusals
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(receipt)
	return result.ExitCode
}

// resolveRunContext finds this machine's state directory and the daemon this
// run reports to.
//
// The resolution order is deliberate, and every step of it is an answer
// somebody AUTHORITATIVE gave: the flag the caller passed, then the state
// directory the daemon itself wrote into the credential, then the path that
// credential sits in, then the data root a unit or a profile exported. What is
// NOT in the list any more is a bare relative default — `state`, resolved
// against the process's working directory. That default is the bug this whole
// path was rewritten for: four supervised jobs with cwd=/tmp wrote their events
// to /tmp/state/process-children, where no daemon would ever look, and every
// one of them reported ok to the agent that started them.
//
// An ambiguous answer is refused rather than guessed at.
func resolveRunContext(tokenFile, stateDirOverride string, unreported bool, stderr io.Writer) (string, procchild.Reporter, error) {
	reporter, credentialPath, err := procchild.ResolveCredential(tokenFile)
	if err != nil {
		return "", nil, err
	}
	if unreported && reporter != nil {
		// --unreported means it, even when a credential is sitting in the
		// environment. Every Workass-launched process holds WORKASS_TOOL_CONTEXT,
		// so a run started from an agent's own shell finds one whether or not
		// the operator wanted this job reported.
		reporter = nil
	}
	stateDir := strings.TrimSpace(stateDirOverride)
	if stateDir == "" && credentialPath != "" {
		// What the daemon stated, which is the one authority on its own state.
		stateDir = procchild.CredentialStateDir(credentialPath)
	}
	if stateDir == "" && credentialPath != "" {
		// Where the credential sits, for a context file written by a build that
		// predates the stated field.
		stateDir = procchild.StateDirForCredential(credentialPath)
	}
	if stateDir == "" {
		if root := strings.TrimSpace(os.Getenv(procchild.DataRootEnvVar)); root != "" {
			stateDir = filepath.Join(root, "state")
		}
	}
	if stateDir == "" {
		stateDir = installedFleetStateDir()
	}
	if stateDir == "" {
		return "", nil, errors.New(
			"this run cannot tell where this machine's Workass daemon keeps its state, so it refuses to start rather than write its events somewhere nothing will read: " +
				"pass --state-dir, or run it with a Workass credential (--token-file / WORKASS_TOOL_CONTEXT), or export WORKASS_DATA_ROOT")
	}
	if !filepath.IsAbs(stateDir) {
		// Never relative. A relative state directory resolves against the JOB's
		// working directory, which is the exact failure above.
		return "", nil, fmt.Errorf(
			"the supervised-job state directory %q is relative, so it would resolve against this job's working directory; pass an absolute --state-dir", stateDir)
	}
	stateDir = filepath.Clean(stateDir)
	if reporter == nil {
		if !unreported {
			// Refused rather than run quietly. A run that supervises and tells
			// nobody looked identical, from the outside, to one that reported.
			return "", nil, procchild.ErrUnreportedRun
		}
		fmt.Fprintf(stderr, "workass run: --unreported, so NO CHAT WILL BE TOLD about this job; it is supervised and its events are queued at %s\n",
			procchild.OutboxPath(stateDir))
	}
	return stateDir, reporter, nil
}

// installedFleetStateDir is this machine's own installed state directory, and
// only when it is really there.
//
// It is defaultFleetStateDir without the relative fallback: `fleet join` may
// reasonably create a state directory where none exists, but a supervised job
// may not, because the place it would create one is wherever the job happens
// to be running.
func installedFleetStateDir() string {
	candidate := defaultFleetStateDir()
	if candidate == "" || !filepath.IsAbs(candidate) {
		return ""
	}
	if info, err := os.Stat(candidate); err != nil || !info.IsDir() {
		return ""
	}
	return candidate
}

func splitInputList(value string) []string {
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// randomOperationID is used only when the caller did not supply one. A caller
// that wants a retry to be the same job supplies --operation-id; a caller that
// did not ask for that gets a fresh job, which is the behaviour a bare shell
// invocation expects.
func randomOperationID() (string, error) {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "run-" + base64.RawURLEncoding.EncodeToString(buf) + "-" +
		fmt.Sprintf("%d", time.Now().UTC().UnixNano()), nil
}
