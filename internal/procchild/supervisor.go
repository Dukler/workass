package procchild

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// The supervisor: `workass run`, which ships with the product so every machine
// has it by construction (delegation-fabric.md §5).
//
// What it does, in order, and why that order:
//
//  1. Derive the child id from the caller's operation id, so "launch there"
//     and "record here" cannot become two half-truths (§6).
//  2. Open the outbox and record what it was asked to do, BEFORE it asks
//     anybody for anything. From this line on, a crash is legible.
//  3. REPORT that to the daemon and be acknowledged, which is what creates the
//     row. A daemon that is down is not fatal — the event is queued and the
//     backlog drains when it answers — but it is never quiet either.
//  4. Preflight. A refusal is reported like any other outcome, because the
//     caller of a job started from another machine is not standing here to
//     read a receipt: a refusal nobody is told is the same as a job that
//     vanished.
//  5. Take the resource hold, reporting queue position as it waits.
//  6. Start the child in its own process group / scope / job object, so it is
//     NOT a daemon child and survives a daemon restart and a self-update.
//  7. Watch three things it declared — a runtime ceiling, an output-silence
//     threshold, a stop-file — and act on each exactly once, reporting a
//     heartbeat on the declared cadence meanwhile.
//  8. Report the real outcome with the real exit code, release the hold, and
//     exit with the child's own code — saying plainly if the outcome could not
//     be delivered.
//
// Liveness has two witnesses, and both are needed. The supervisor's heartbeat
// answers "is it still reporting", which is the question a healthy quiet job
// and a job that has lost its daemon would otherwise answer identically. The
// pid answers "is it still there": a supervisor pid that is gone with no
// terminal event delivered and none left in its outbox is `lost`, and absence
// is still evidence.

// Result is what one supervised run amounted to.
type Result struct {
	ChildID    string
	Outcome    string
	ExitCode   int
	Elapsed    time.Duration
	OutputFile string
	DoneFile   string
	SpoolFile  string
	// Refusals is non-empty only when preflight refused, in which case nothing
	// was started — but the refusal IS reported, so the chat that asked learns
	// its job did not run.
	Refusals []Refusal
	// Reporting is what this run can honestly say about having been reported.
	Reporting ReportingHealth
}

// Spec is one run, exactly as the command line described it.
type Spec struct {
	StateDir    string
	OperationID string
	Label       string
	Role        string
	Resource    string
	MaxRuntime  time.Duration
	StaleAfter  time.Duration
	MaxHold     time.Duration
	Inputs      []string
	MinFreeDisk uint64
	// Owner is the global id of the chat or turn that is owed this child's
	// outcome. Empty means the calling chat, resolved by the daemon from the
	// credential — never guessed here.
	Owner      string
	Dir        string
	Command    []string
	OutputFile string

	// Diagnostics is where the supervisor's own lines go. The child's output
	// goes to the output file and never here, so a caller reading this stream
	// sees the supervision and not the work.
	Diagnostics io.Writer
	// Reporter is the daemon this run reports to. Nil is an unreported run,
	// which the CLI only allows when the operator asked for one by name.
	Reporter  Reporter
	Outbox    *Outbox
	Now       func() time.Time
	StopPoll  time.Duration
	StalePoll time.Duration
	StopGrace time.Duration
	// ReportRetry and ReportDeadline are how fast an unreachable daemon is
	// retried and how long a finished supervisor keeps trying before it says it
	// could not deliver. Zero means the production constants; a test moves them
	// so it does not have to wait out a real outage.
	ReportRetry    time.Duration
	ReportDeadline time.Duration
}

// Exit codes the supervisor itself produces, distinct from the child's.
const (
	// ExitPreflightRefused is a refusal before anything ran.
	ExitPreflightRefused = 2
	// ExitTimedOut follows GNU timeout's convention, so a shell wrapper that
	// already understands `timeout` understands this.
	ExitTimedOut = 124
	// ExitCancelled is the stop-file's code: 128 + SIGINT, the shell's own word
	// for "somebody asked this to stop".
	ExitCancelled = 130
	// ExitSupervisorFailed is the supervisor's own failure — it could not open
	// its spool, could not start the command. It is distinct from the child's
	// codes so a caller can tell "the work failed" from "the work never ran".
	ExitSupervisorFailed = 125
)

const (
	defaultStopPoll  = 250 * time.Millisecond
	defaultStalePoll = 5 * time.Second
	// defaultStopGrace is how long a polite stop is given before the group is
	// killed. A server closing its sockets gets its moment; anything still
	// standing after this never intended to leave. It is the same number the
	// rail's stop button already uses, scaled for a job rather than a click.
	defaultStopGrace = 10 * time.Second
)

// Run supervises one child and returns what happened. It does not call
// os.Exit: the caller owns the process.
func Run(spec Spec) (Result, error) {
	now := spec.Now
	if now == nil {
		now = time.Now
	}
	diagnostics := spec.Diagnostics
	if diagnostics == nil {
		diagnostics = io.Discard
	}
	spec.Label = strings.TrimSpace(spec.Label)
	if spec.Label == "" {
		return Result{}, errors.New("a process child needs --label: it is the only name its chat will ever show")
	}
	if len(spec.Command) == 0 {
		return Result{}, errors.New("a process child needs a command after --")
	}
	if strings.TrimSpace(spec.Resource) != "" && NormalizeResource(spec.Resource) == "" {
		return Result{}, fmt.Errorf("--resource %q is not a usable resource name", spec.Resource)
	}
	childID, err := IDForOperation(spec.OperationID)
	if err != nil {
		return Result{}, err
	}
	if _, err := EnsureDir(spec.StateDir, childID); err != nil {
		return Result{}, err
	}
	outbox := spec.Outbox
	if outbox == nil {
		// The state directory is a resolved absolute path by the time it gets
		// here (resolveRunContext refuses anything else), and the outbox refuses
		// again on its own account. Two refusals for one mistake, because the
		// mistake wrote four jobs' worth of events into /tmp/state.
		opened, err := OpenOutbox(spec.StateDir)
		if err != nil {
			return Result{}, err
		}
		defer opened.Close()
		outbox = opened
	}
	outputFile, err := resolveOutputFile(spec, childID)
	if err != nil {
		return Result{}, err
	}
	doneFile := outputFile + ".done"
	result := Result{
		ChildID: childID, OutputFile: outputFile, DoneFile: doneFile,
		SpoolFile: outbox.Path(),
	}

	// The registration facts, read fresh for every report: a replayed batch
	// must carry the pid and process group that are true NOW, not the ones that
	// were true when its first event was queued.
	live := struct {
		mu   sync.Mutex
		pgid int
		kind GroupKind
	}{kind: GroupNone}
	facts := func() Report {
		live.mu.Lock()
		defer live.mu.Unlock()
		return Report{
			Spec: spec, ChildID: childID, OutputFile: outputFile, DoneFile: doneFile,
			SupervisorPID: os.Getpid(), ProcessGroup: live.pgid, Group: live.kind,
		}
	}
	record := newJournal(childID, outbox, spec.Reporter, diagnostics, now, facts, spec.ReportRetry, spec.ReportDeadline)
	// One timer for the whole run: it reports the heartbeat while the daemon is
	// reachable and retries the backlog while it is not. The caller asked for
	// supervision, and this is what supervision reports with.
	beats := time.NewTicker(record.retry)
	defer beats.Stop()
	heartbeatDone := make(chan struct{})
	defer close(heartbeatDone)
	go func() {
		for {
			select {
			case <-heartbeatDone:
				return
			case <-beats.C:
				record.beat()
			}
		}
	}()

	// Recorded before anything is asked of the machine, and reported at once,
	// so the chat that asked has a row from the first moment there is anything
	// to have a row about.
	if _, err := record.record(KindCreated, map[string]any{
		"label": spec.Label, "role": spec.Role, "command": spec.Command,
		"resource": NormalizeResource(spec.Resource), "owner": strings.TrimSpace(spec.Owner),
		"maxRuntimeMs": spec.MaxRuntime.Milliseconds(), "staleAfterMs": spec.StaleAfter.Milliseconds(),
		"maxHoldMs": spec.MaxHold.Milliseconds(), "inputs": spec.Inputs,
		"supervisorPid": os.Getpid(), "outputFile": outputFile, "dir": spec.Dir,
	}); err != nil {
		return result, err
	}

	if refusals := Preflight(PreflightRequest{
		Command: spec.Command, Inputs: spec.Inputs, OutputFile: outputFile,
		MinFreeDiskBytes: spec.MinFreeDisk, Dir: spec.Dir,
	}); len(refusals) > 0 {
		result.Refusals = refusals
		lines := make([]string, 0, len(refusals))
		for _, refusal := range refusals {
			lines = append(lines, refusal.String())
		}
		sort.Strings(lines)
		// A refusal is terminal, and it IS news. The old design dropped it on
		// the floor — "nothing started, so there is nothing to tell a chat" —
		// which is true only when the caller is standing here reading the
		// receipt. A job started from another machine has a chat waiting, and a
		// refusal it never hears about is indistinguishable from a job that
		// vanished. This is what the live test's refused case proved.
		_, _ = record.record(KindSettled, map[string]any{
			"outcome": OutcomeFailed, "summary": "preflight refused: " + strings.Join(lines, "; "),
			"preflight": lines, "exitCode": ExitPreflightRefused,
		})
		result.Outcome, result.ExitCode = OutcomeFailed, ExitPreflightRefused
		result.Reporting = record.settle()
		return result, nil
	}

	group, command := newGroup(childID, spec.Command)
	defer group.close()
	live.mu.Lock()
	live.kind = group.Kind()
	live.mu.Unlock()

	stopPath := StopPath(spec.StateDir, childID)
	// A stop-file left by a previous run of the same operation id would stop
	// this one before it began. The id is derived from the operation, so a
	// deliberate retry reuses the directory.
	_ = os.Remove(stopPath)

	var lease *Lease
	if NormalizeResource(spec.Resource) != "" {
		lease, err = Acquire(AcquireRequest{
			StateDir: spec.StateDir, Resource: spec.Resource, ChildID: childID,
			Label: spec.Label, Owner: spec.Owner, MaxHold: spec.MaxHold,
			Cancelled: func() bool { return stopRequested(stopPath) },
			OnQueued: func(position, total int) {
				_, _ = record.record(KindQueued, map[string]any{
					"resource": NormalizeResource(spec.Resource), "position": position, "waiting": total,
				})
				fmt.Fprintf(diagnostics, "workass run: waiting for %s, position %d of %d\n",
					NormalizeResource(spec.Resource), position, total)
			},
		})
		if err != nil {
			_, _ = record.record(KindSettled, map[string]any{
				"outcome": OutcomeCancelled, "summary": err.Error(), "exitCode": ExitCancelled,
			})
			result.Outcome, result.ExitCode = OutcomeCancelled, ExitCancelled
			result.Reporting = record.settle()
			return result, nil
		}
		defer lease.Release()
	}

	output, err := os.OpenFile(outputFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return result, err
	}
	defer output.Close()

	cmd := exec.Command(command[0], command[1:]...)
	cmd.Dir = spec.Dir
	cmd.Stdout, cmd.Stderr = output, output
	// The child never inherits the supervisor's stdin: a job that read from a
	// terminal would block forever with nobody there to type.
	cmd.Stdin = nil
	group.prepare(cmd)
	started := now()
	if err := cmd.Start(); err != nil {
		_, _ = record.record(KindSettled, map[string]any{
			"outcome": OutcomeFailed, "summary": "the command could not be started: " + err.Error(),
			"exitCode": ExitSupervisorFailed,
		})
		result.Outcome, result.ExitCode = OutcomeFailed, ExitSupervisorFailed
		result.Reporting = record.settle()
		return result, nil
	}
	if err := group.adopt(cmd); err != nil {
		fmt.Fprintf(diagnostics, "workass run: %v; a stop will reach this process and not its descendants\n", err)
	}
	pid := cmd.Process.Pid
	live.mu.Lock()
	live.pgid = group.GroupID()
	live.mu.Unlock()
	// The row gains its pid here, which is the evidence the daemon's `lost`
	// verdict needs — and it rides the same report as the started event, so
	// there is no second verb and no window where one arrived without the other.
	_, _ = record.record(KindStarted, map[string]any{
		"pid": pid, "pgid": group.GroupID(), "group": string(group.Kind()),
		"unit": groupUnit(group), "supervisorPid": os.Getpid(), "resource": NormalizeResource(spec.Resource),
	})

	outcome, exitCode := supervise(spec, record, group, cmd, pid, stopPath, outputFile, started, diagnostics)
	elapsed := now().Sub(started)
	result.Outcome, result.ExitCode, result.Elapsed = outcome, exitCode, elapsed

	summary := outcomeSummary(outcome, exitCode, elapsed)
	_, _ = record.record(KindSettled, map[string]any{
		"outcome": outcome, "exitCode": exitCode, "summary": summary,
		"elapsedMs": elapsed.Milliseconds(), "pid": pid,
	})
	// The done marker keeps the pre-P4 settle path working byte for byte: a
	// child registered through the external-work adapter is settled from this
	// file by the same reconcile branch it always was, whatever happened to the
	// report.
	writeDoneFile(doneFile, exitCode)
	lease.Release()
	// The last thing it does is make sure the outcome arrived, and say so if it
	// did not. Exiting quietly with an undelivered settle is the one failure
	// this whole path exists to remove.
	result.Reporting = record.settle()
	return result, nil
}

// supervise watches the three things the caller declared and the one thing it
// did not: the child's own exit.
func supervise(
	spec Spec, record *journal, group *group, cmd *exec.Cmd, pid int,
	stopPath, outputFile string, started time.Time, diagnostics io.Writer,
) (string, int) {
	now := spec.Now
	if now == nil {
		now = time.Now
	}
	stopPoll, stalePoll := spec.StopPoll, spec.StalePoll
	if stopPoll <= 0 {
		stopPoll = defaultStopPoll
	}
	if stalePoll <= 0 {
		stalePoll = defaultStalePoll
	}
	grace := spec.StopGrace
	if grace <= 0 {
		grace = defaultStopGrace
	}

	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()

	var deadline <-chan time.Time
	if spec.MaxRuntime > 0 {
		// The clock starts at exec, not at the claim: queue time is not
		// runtime, and charging a job for the GPU queue would time out the jobs
		// that waited politely.
		timer := time.NewTimer(spec.MaxRuntime)
		defer timer.Stop()
		deadline = timer.C
	}
	ticker := time.NewTicker(stopPoll)
	defer ticker.Stop()

	stallWatch := newStallWatch(outputFile, started, spec.StaleAfter, stalePoll)
	var once sync.Once
	stopping := ""
	stop := func(reason, outcome string) {
		once.Do(func() {
			stopping = outcome
			_, _ = record.record(KindCancelRequested, map[string]any{"by": reason, "outcome": outcome})
			fmt.Fprintf(diagnostics, "workass run: stopping (%s); %s then a kill after %s\n",
				reason, gracefulMeans(group.Kind()), grace)
			// The stop-file is the graceful half on every platform, and the only
			// graceful half on Windows: a child that watches for it shuts down
			// on its own terms before anything is signalled.
			requestStop(stopPath, reason)
			if err := group.stop(pid, false); err != nil {
				fmt.Fprintf(diagnostics, "workass run: a polite stop could not be delivered: %v\n", err)
			}
			go func() {
				time.Sleep(grace)
				if PIDAlive(pid) {
					_ = group.stop(pid, true)
				}
			}()
		})
	}

	for {
		select {
		case err := <-waited:
			code := exitCodeOf(cmd, err)
			switch {
			case stopping == OutcomeTimedOut:
				return OutcomeTimedOut, ExitTimedOut
			case stopping == OutcomeCancelled:
				return OutcomeCancelled, ExitCancelled
			case code == 0:
				return OutcomeSettled, 0
			default:
				return OutcomeFailed, code
			}
		case <-deadline:
			stop(fmt.Sprintf("max_runtime %s reached", spec.MaxRuntime), OutcomeTimedOut)
		case <-ticker.C:
			if stopRequested(stopPath) && stopping == "" {
				stop("a stop was requested", OutcomeCancelled)
			}
			if evidence, stalled := stallWatch.check(now()); stalled {
				// One notice, however long the silence lasts. A stall is a flag
				// on running, never a state, and a chat told about it on every
				// poll would be a chat nobody reads.
				_, _ = record.record(KindStalled, map[string]any{
					"evidence": evidence, "staleAfterMs": spec.StaleAfter.Milliseconds(),
				})
				fmt.Fprintf(diagnostics, "workass run: %s\n", evidence)
			}
		}
	}
}

// stallWatch turns "the output file stopped growing" into one declared stall.
//
// It is the output file and not the process state because a stall is about
// PROGRESS, and the only progress a supervisor can see without reading the
// work's mind is bytes. The caller declares the threshold, so a job that is
// legitimately silent for an hour simply does not declare one — silence is
// never inferred to be a problem (§7: "a declared stall").
type stallWatch struct {
	path      string
	threshold time.Duration
	poll      time.Duration
	lastGrew  time.Time
	lastSize  int64
	lastCheck time.Time
	announced bool
}

func newStallWatch(path string, started time.Time, threshold, poll time.Duration) *stallWatch {
	return &stallWatch{path: path, threshold: threshold, poll: poll, lastGrew: started, lastCheck: started}
}

func (w *stallWatch) check(now time.Time) (string, bool) {
	if w == nil || w.threshold <= 0 || w.announced {
		return "", false
	}
	if now.Sub(w.lastCheck) < w.poll {
		return "", false
	}
	w.lastCheck = now
	size := int64(-1)
	if info, err := os.Stat(w.path); err == nil {
		size = info.Size()
	}
	if size != w.lastSize {
		w.lastSize, w.lastGrew = size, now
		return "", false
	}
	silent := now.Sub(w.lastGrew)
	if silent < w.threshold {
		return "", false
	}
	w.announced = true
	return fmt.Sprintf("no output for %s, past the declared %s (still running)",
		silent.Round(time.Second), w.threshold), true
}

func gracefulMeans(kind GroupKind) string {
	if kind == GroupJobObject {
		return "a stop file"
	}
	return "a stop file and SIGTERM"
}

func groupUnit(g *group) string {
	if g == nil || g.kind != GroupSystemdScope {
		return ""
	}
	return g.unit
}

func exitCodeOf(cmd *exec.Cmd, err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if code := exitErr.ExitCode(); code >= 0 {
			return code
		}
		// A signalled child has no exit code of its own; signalledExitCode
		// reconstructs the shell's answer for it, per platform.
		return signalledExitCode(exitErr)
	}
	if cmd != nil && cmd.ProcessState != nil {
		if code := cmd.ProcessState.ExitCode(); code >= 0 {
			return code
		}
	}
	return ExitSupervisorFailed
}

func outcomeSummary(outcome string, exitCode int, elapsed time.Duration) string {
	switch outcome {
	case OutcomeSettled:
		return fmt.Sprintf("Finished with exit 0 after %s", elapsed.Round(time.Second))
	case OutcomeTimedOut:
		return fmt.Sprintf("Stopped at its declared max_runtime after %s", elapsed.Round(time.Second))
	case OutcomeCancelled:
		return fmt.Sprintf("Stopped on request after %s", elapsed.Round(time.Second))
	default:
		return fmt.Sprintf("Failed with exit %d after %s", exitCode, elapsed.Round(time.Second))
	}
}

// requestStop writes the stop-file. The reason is in it so a child that reads
// the file can say why it is shutting down, and so a human who finds one knows
// who asked.
func requestStop(path, reason string) {
	if strings.TrimSpace(path) == "" {
		return
	}
	_ = os.WriteFile(path, []byte(strings.TrimSpace(reason)+"\n"), 0o600)
}

// RequestStop is requestStop for the daemon: a cancel is a wish written where
// the supervisor will see it, whether or not this process can signal it.
func RequestStop(stateDir, childID, reason string) error {
	if _, err := EnsureDir(stateDir, childID); err != nil {
		return err
	}
	path := StopPath(stateDir, childID)
	if path == "" {
		return errors.New("this machine has no process-child state directory")
	}
	return os.WriteFile(path, []byte(strings.TrimSpace(reason)+"\n"), 0o600)
}

// StopRequested reports whether a stop has been asked for.
func StopRequested(stateDir, childID string) bool {
	return stopRequested(StopPath(stateDir, childID))
}

func stopRequested(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

func writeDoneFile(path string, exitCode int) {
	if strings.TrimSpace(path) == "" {
		return
	}
	_ = os.WriteFile(path, []byte(fmt.Sprintf("%d\n", exitCode)), 0o600)
}

func resolveOutputFile(spec Spec, childID string) (string, error) {
	if explicit := strings.TrimSpace(spec.OutputFile); explicit != "" {
		if !filepath.IsAbs(explicit) {
			return "", errors.New("--output-file must be an absolute path")
		}
		return filepath.Clean(explicit), nil
	}
	stateDir := strings.TrimSpace(spec.StateDir)
	if stateDir == "" {
		return "", errors.New("this machine has no Workass state directory, so there is nowhere to write the output")
	}
	// The external-work root, not the process-child root: the output file is
	// the caller's to read and is already the one directory the daemon's path
	// validator admits, so reusing it keeps one rule about where output lives.
	dir := filepath.Join(stateDir, "external-work")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	abs, err := filepath.Abs(filepath.Join(dir, childID+".output"))
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}
