package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
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
	stateDir := flags.String("state-dir", "", "daemon state directory override; resolved from the credential or WORKASS_DATA_ROOT by default")
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

The job runs in its own process group, so it is not a child of the daemon and
survives a daemon restart or a self-update. Its transitions are written to a
spool on disk before anything is reported, so a daemon that is down misses
nothing. A supervisor pid that is gone with no settled event means lost, not
running.

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

	resolvedStateDir, registrar, err := resolveRunContext(*tokenFile, *stateDir, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "workass run: %s\n", acp.RedactSensitiveText(err.Error()))
		return procchild.ExitSupervisorFailed
	}

	floor, err := procchild.ParseBytes(*minFreeDisk)
	if err != nil {
		fmt.Fprintf(stderr, "workass run: %v\n", err)
		return 2
	}

	id := strings.TrimSpace(*operationID)
	if id == "" {
		if id, err = randomOperationID(); err != nil {
			fmt.Fprintf(stderr, "workass run: %v\n", err)
			return procchild.ExitSupervisorFailed
		}
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
		Diagnostics: diagnostics, Registrar: registrar,
	}
	result, err := procchild.Run(spec)
	if err != nil {
		fmt.Fprintf(stderr, "workass run: %s\n", acp.RedactSensitiveText(err.Error()))
		return procchild.ExitSupervisorFailed
	}
	receipt := map[string]any{
		"workId": result.ChildID, "operationId": id, "outcome": result.Outcome,
		"exitCode": result.ExitCode, "elapsedMs": result.Elapsed.Milliseconds(),
		"outputFile": result.OutputFile, "registered": result.Registered,
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

// resolveRunContext finds this machine's state directory and, when there is a
// credential, the client that will register the child.
//
// A run with no credential is not an error: it supervises, spools and settles
// exactly the same, and the only thing it lacks is a chat to tell. Saying so
// once on stderr is better than refusing work a human asked for from a shell.
func resolveRunContext(tokenFile, stateDirOverride string, stderr io.Writer) (string, procchild.Registrar, error) {
	registrar, credentialPath, err := procchild.ResolveCredential(tokenFile)
	if err != nil {
		return "", nil, err
	}
	stateDir := strings.TrimSpace(stateDirOverride)
	if stateDir == "" && credentialPath != "" {
		stateDir = procchild.StateDirForCredential(credentialPath)
	}
	if stateDir == "" {
		// defaultFleetStateDir is the same resolution `workass fleet` uses: the
		// data root a unit or a profile set, then this machine's own install.
		// One answer to "where does this machine keep its state", not two.
		stateDir = defaultFleetStateDir()
	}
	if !filepath.IsAbs(stateDir) {
		absolute, err := filepath.Abs(stateDir)
		if err != nil {
			return "", nil, err
		}
		stateDir = absolute
	}
	if registrar == nil {
		fmt.Fprintf(stderr, "workass run: no Workass credential in this environment; the job is supervised and spooled at %s, but no chat is told\n", stateDir)
	}
	return filepath.Clean(stateDir), registrar, nil
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
