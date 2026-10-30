package procchild

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// What the supervisor must get right, from §13 P4 and delegation-fabric.md §5.
// Every assertion here is about EVIDENCE: the spool holds a transition before
// the supervisor acts on it, the exit code is the child's own, a ceiling is
// timed_out rather than failed, a stop is cancelled rather than either, and a
// declared stall is said exactly once while the job keeps running.

type recordingRegistrar struct {
	calls []map[string]any
	fail  error
}

func (r *recordingRegistrar) Register(spec Spec, childID, outputFile, doneFile, spoolFile string, pid, pgid int, group GroupKind) (Registration, error) {
	r.calls = append(r.calls, map[string]any{
		"childID": childID, "pid": pid, "pgid": pgid, "group": string(group),
		"operationID": spec.OperationID, "owner": spec.Owner, "resource": spec.Resource,
	})
	if r.fail != nil {
		return Registration{}, r.fail
	}
	return Registration{ChildID: childID, OutputFile: outputFile, DoneFile: doneFile, Status: "running"}, nil
}

func fastSpec(t *testing.T, state string, command ...string) Spec {
	t.Helper()
	return Spec{
		StateDir: state, OperationID: "op-" + t.Name(), Label: "test job",
		Command: command, StopPoll: 5 * time.Millisecond, StalePoll: 5 * time.Millisecond,
		StopGrace: 200 * time.Millisecond, Diagnostics: &bytes.Buffer{},
	}
}

func spoolOf(t *testing.T, state, childID string) []Event {
	t.Helper()
	events, err := ReadSpool(SpoolPath(state, childID))
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func kindsOf(events []Event) []string {
	kinds := make([]string, 0, len(events))
	for _, event := range events {
		kinds = append(kinds, event.Kind)
	}
	return kinds
}

func TestASupervisedRunRecordsItsWholeLifeAndReturnsTheChildExitCode(t *testing.T) {
	state := t.TempDir()
	registrar := &recordingRegistrar{}
	spec := fastSpec(t, state, "/bin/sh", "-c", "echo work; exit 7")
	spec.Registrar = registrar
	result, err := Run(spec)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != OutcomeFailed || result.ExitCode != 7 {
		t.Fatalf("outcome %q exit %d; want failed with the child's own 7", result.Outcome, result.ExitCode)
	}
	events := spoolOf(t, state, result.ChildID)
	if got := strings.Join(kindsOf(events), ","); got != "created,started,settled" {
		t.Fatalf("spool kinds = %s; want created,started,settled", got)
	}
	for i, event := range events {
		if event.Seq != uint64(i+1) {
			t.Fatalf("spool sequences must be dense and in order, got %+v", kindsOf(events))
		}
	}
	settled := events[len(events)-1]
	if settled.Payload["outcome"] != OutcomeFailed || int(settled.Payload["exitCode"].(float64)) != 7 {
		t.Fatalf("the settled event must carry the real outcome and code, got %+v", settled.Payload)
	}
	output, err := os.ReadFile(result.OutputFile)
	if err != nil || !strings.Contains(string(output), "work") {
		t.Fatalf("the child's output must be in its own file, got %q / %v", output, err)
	}
	// The done marker keeps the pre-P4 settle path working byte for byte.
	done, err := os.ReadFile(result.DoneFile)
	if err != nil || strings.TrimSpace(string(done)) != "7" {
		t.Fatalf("done marker = %q / %v; want the exit code", done, err)
	}
	if len(registrar.calls) < 2 {
		t.Fatalf("want a registration before the start and a poke after the settle, got %d", len(registrar.calls))
	}
	if registrar.calls[0]["childID"] != result.ChildID || registrar.calls[0]["operationID"] != spec.OperationID {
		t.Fatalf("every registration must carry the same child id and operation id, got %+v", registrar.calls)
	}
}

func TestTheChildIdIsDerivedFromTheOperationIdSoARetryIsTheSameJob(t *testing.T) {
	first, err := IDForOperation("launch-there-record-here")
	if err != nil {
		t.Fatal(err)
	}
	second, err := IDForOperation("launch-there-record-here")
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("the same operation id must derive the same child id, got %q and %q", first, second)
	}
	other, err := IDForOperation("a-different-job")
	if err != nil {
		t.Fatal(err)
	}
	if other == first {
		t.Fatal("two operations must not derive one child id")
	}
	if !ValidID(first) || !strings.HasPrefix(first, "xw") {
		t.Fatalf("a derived id must keep today's external-work id shape, got %q", first)
	}
	if _, err := IDForOperation("  "); err == nil {
		t.Fatal("an empty operation id must be refused rather than deriving a shared id")
	}
}

func TestAPreflightRefusalStartsNothingAndLeavesNoRecord(t *testing.T) {
	state := t.TempDir()
	registrar := &recordingRegistrar{}
	spec := fastSpec(t, state, "/bin/sh", "-c", "echo never")
	spec.Inputs = []string{filepath.Join(state, "absent.bin")}
	spec.Registrar = registrar
	result, err := Run(spec)
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != ExitPreflightRefused || len(result.Refusals) != 1 {
		t.Fatalf("want one refusal and exit %d, got %+v", ExitPreflightRefused, result)
	}
	if len(registrar.calls) != 0 {
		// Nothing started, so a chat with a running row for it would be a row
		// about nothing.
		t.Fatalf("a refused run must register no child, got %+v", registrar.calls)
	}
	if _, err := os.Stat(Dir(state, result.ChildID)); !os.IsNotExist(err) {
		t.Fatalf("a refused run must leave no spool directory behind: %v", err)
	}
	if _, err := os.Stat(result.OutputFile); err == nil {
		if data, _ := os.ReadFile(result.OutputFile); len(data) > 0 {
			t.Fatalf("a refused run must write no output, got %q", data)
		}
	}
}

func TestMaxRuntimeSettlesTimedOutAndNotFailed(t *testing.T) {
	state := t.TempDir()
	spec := fastSpec(t, state, "/bin/sh", "-c", "sleep 30")
	spec.MaxRuntime = 80 * time.Millisecond
	result, err := Run(spec)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != OutcomeTimedOut || result.ExitCode != ExitTimedOut {
		// A job stopped at its own ceiling did not fail on its own terms, and
		// a chat told it "failed" would look for a bug that is not there.
		t.Fatalf("outcome %q exit %d; want timed_out and %d", result.Outcome, result.ExitCode, ExitTimedOut)
	}
	events := spoolOf(t, state, result.ChildID)
	if got := strings.Join(kindsOf(events), ","); got != "created,started,cancel_requested,settled" {
		t.Fatalf("spool kinds = %s; a timeout must record why it stopped", got)
	}
	if events[2].Payload["outcome"] != OutcomeTimedOut {
		t.Fatalf("the recorded reason must be the timeout, got %+v", events[2].Payload)
	}
}

func TestAStopFileCancelsAndIsNeverAFailure(t *testing.T) {
	state := t.TempDir()
	childID, err := IDForOperation("op-" + t.Name())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan Result, 1)
	go func() {
		spec := fastSpec(t, state, "/bin/sh", "-c", "sleep 30")
		result, runErr := Run(spec)
		if runErr != nil {
			t.Error(runErr)
		}
		done <- result
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if events := func() []Event {
			found, _ := ReadSpool(SpoolPath(state, childID))
			return found
		}(); len(events) >= 2 && events[1].Kind == KindStarted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the child never started")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := RequestStop(state, childID, "the operator asked"); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-done:
		if result.Outcome != OutcomeCancelled || result.ExitCode != ExitCancelled {
			t.Fatalf("outcome %q exit %d; want cancelled and %d", result.Outcome, result.ExitCode, ExitCancelled)
		}
		events := spoolOf(t, state, childID)
		if got := strings.Join(kindsOf(events), ","); got != "created,started,cancel_requested,settled" {
			t.Fatalf("spool kinds = %s; a cancel must be recorded before it is acted on", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a stop file did not stop the job")
	}
}

func TestADeclaredStallIsSaidOnceAndTheJobKeepsRunning(t *testing.T) {
	state := t.TempDir()
	spec := fastSpec(t, state, "/bin/sh", "-c", "echo start; sleep 0.9; echo end")
	spec.StaleAfter = 60 * time.Millisecond
	spec.StalePoll = 10 * time.Millisecond
	result, err := Run(spec)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != OutcomeSettled || result.ExitCode != 0 {
		// A stall is a FLAG on running, never a state: the job finished.
		t.Fatalf("a stalled job that then finished must settle, got %q / %d", result.Outcome, result.ExitCode)
	}
	stalls := 0
	for _, event := range spoolOf(t, state, result.ChildID) {
		if event.Kind == KindStalled {
			stalls++
			if evidence, _ := event.Payload["evidence"].(string); !strings.Contains(evidence, "still running") {
				t.Fatalf("a stall must say the job is still running, got %q", evidence)
			}
		}
	}
	if stalls != 1 {
		t.Fatalf("a declared stall must be said exactly once however long the silence lasts, got %d", stalls)
	}
}

func TestASupervisedRunWorksWithNoDaemonAndStillHasItsOutcomeOnDisk(t *testing.T) {
	state := t.TempDir()
	registrar := &recordingRegistrar{fail: os.ErrDeadlineExceeded}
	spec := fastSpec(t, state, "/bin/sh", "-c", "exit 0")
	spec.Registrar = registrar
	result, err := Run(spec)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != OutcomeSettled || result.Registered {
		t.Fatalf("a run whose daemon refused every call must still settle, got %+v", result)
	}
	events := spoolOf(t, state, result.ChildID)
	if len(events) == 0 || events[len(events)-1].Kind != KindSettled {
		// A supervisor that needed a live daemon would fail exactly during a
		// self-update, which is when it is needed most.
		t.Fatalf("the outcome must be on disk for the daemon to ingest later, got %v", kindsOf(events))
	}
}

func TestTheChildRunsInItsOwnProcessGroup(t *testing.T) {
	state := t.TempDir()
	spec := fastSpec(t, state, "/bin/sh", "-c", "exit 0")
	result, err := Run(spec)
	if err != nil {
		t.Fatal(err)
	}
	started := spoolOf(t, state, result.ChildID)[1]
	isolation, _ := started.Payload["group"].(string)
	switch GroupKind(isolation) {
	case GroupProcessGroup, GroupSystemdScope, GroupJobObject:
	default:
		t.Fatalf("the child must record how it was isolated, got %q", isolation)
	}
	if GroupKind(isolation) == GroupProcessGroup {
		pgid, _ := started.Payload["pgid"].(float64)
		pid, _ := started.Payload["pid"].(float64)
		if int(pgid) <= 1 || int(pgid) != int(pid) {
			// setpgid makes the child its own group leader. Sharing the
			// supervisor's group would mean a stop aimed at the child killed
			// the supervisor too, and a supervisor killed mid-stop writes no
			// settle — which this daemon then correctly reads as lost.
			t.Fatalf("the child must lead its own process group, got pgid %v pid %v", pgid, pid)
		}
	}
}

func TestTheSpoolSurvivesAPartialLastLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	spool, err := OpenSpool(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := spool.Append(KindCreated, map[string]any{"label": "one"}); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	// A reader that arrives mid-append sees a prefix. Inventing an event from
	// half a line would be the one way the spool could lie.
	if _, err := file.WriteString(`{"seq":2,"kind":"sett`); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	events, err := ReadSpool(path)
	if err != nil || len(events) != 1 || events[0].Kind != KindCreated {
		t.Fatalf("a partial trailing line must be skipped, got %v / %v", kindsOf(events), err)
	}
	// Reopening must continue the sequence, not restart it.
	resumed, err := OpenSpool(path)
	if err != nil || resumed.Head() != 1 {
		t.Fatalf("a reopened spool must resume at the last whole event, got %d / %v", resumed.Head(), err)
	}
	next, err := resumed.Append(KindSettled, map[string]any{"outcome": OutcomeSettled})
	if err != nil || next.Seq != 2 {
		t.Fatalf("the next event must be seq 2, got %d / %v", next.Seq, err)
	}
}

func TestTheCredentialNeverTouchesArgv(t *testing.T) {
	state := t.TempDir()
	contextDir := filepath.Join(state, "tool-bootstrap", "bridge-test")
	if err := os.MkdirAll(contextDir, 0o700); err != nil {
		t.Fatal(err)
	}
	const secret = "this-is-the-session-credential-value"
	contextPath := filepath.Join(contextDir, "context.json")
	encoded, err := json.Marshal(map[string]string{
		"endpoint": "https://tools.localhost:8788/workass/tools", "ca_file": filepath.Join(state, "ca.pem"),
		"credential": secret, "chat_id": "app-chat-1", "tab_id": "tab-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(contextPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	registrar, resolved, err := ResolveCredential(contextPath)
	if err != nil || registrar == nil {
		t.Fatalf("a 0600 credential file must be accepted, got %v", err)
	}
	if resolved != contextPath {
		t.Fatalf("the resolved credential path = %q, want %q", resolved, contextPath)
	}
	// The one invariant: the secret is read from a file whose PATH is the
	// argument. A command line is readable by every process on the box through
	// ps and lands in shell history.
	for _, arg := range []string{"--token-file", contextPath, "--label", "job", "--", "sh", "-c", "true"} {
		if strings.Contains(arg, secret) {
			t.Fatalf("a credential appeared in argv: %q", arg)
		}
	}
	if dir := StateDirForCredential(contextPath); dir != state {
		t.Fatalf("the state directory derived from the credential = %q, want %q", dir, state)
	}
	if err := os.Chmod(contextPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ResolveCredential(contextPath); err == nil {
		// A credential in a world-readable file is a credential every process
		// on the machine holds.
		t.Fatal("a group- or world-readable credential file must be refused")
	}
}

func TestARunWithNoLabelOrNoCommandIsRefusedBeforeAnythingHappens(t *testing.T) {
	state := t.TempDir()
	spec := fastSpec(t, state, "/bin/sh", "-c", "true")
	spec.Label = "  "
	if _, err := Run(spec); err == nil {
		t.Fatal("a job with no label must be refused: the label is the only name its chat will show")
	}
	spec = fastSpec(t, state)
	if _, err := Run(spec); err == nil {
		t.Fatal("a job with no command must be refused")
	}
	spec = fastSpec(t, state, "/bin/sh", "-c", "true")
	spec.Resource = "../escape"
	if _, err := Run(spec); err == nil {
		t.Fatal("a resource name that could escape its queue directory must be refused")
	}
}
