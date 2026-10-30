package procchild

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// What the supervisor must get right, from §13 P4 and delegation-fabric.md §5.
// Every assertion here is about EVIDENCE: a transition is reported to the
// daemon and acknowledged before the supervisor acts on it, the exit code is
// the child's own, a ceiling is timed_out rather than failed, a stop is
// cancelled rather than either, and a declared stall is said exactly once
// while the job keeps running.

// recordingReporter is a daemon that answers. It acknowledges everything it
// is told, in order, which is what a healthy daemon does.
type recordingReporter struct {
	mu       sync.Mutex
	calls    []map[string]any
	events   []Event
	fail     error
	failFor  int
	ackSoFar uint64
	// ackNothing answers without an ackThrough, which is how a daemon older
	// than this contract behaves.
	ackNothing bool
}

func (r *recordingReporter) Report(request Report) (Acknowledgement, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	kinds := make([]string, 0, len(request.Events))
	for _, event := range request.Events {
		kinds = append(kinds, event.Kind)
	}
	r.calls = append(r.calls, map[string]any{
		"childID": request.ChildID, "pid": request.SupervisorPID, "pgid": request.ProcessGroup,
		"group": string(request.Group), "operationID": request.Spec.OperationID,
		"owner": request.Spec.Owner, "resource": request.Spec.Resource,
		"batch": kinds, "gapMs": request.ReportGap.Milliseconds(),
	})
	if r.failFor > 0 {
		r.failFor--
		return Acknowledgement{}, errors.New("the daemon is restarting")
	}
	if r.fail != nil {
		return Acknowledgement{}, r.fail
	}
	for _, event := range request.Events {
		if event.Seq <= r.ackSoFar {
			continue
		}
		r.ackSoFar = event.Seq
		r.events = append(r.events, event)
	}
	if r.ackNothing {
		return Acknowledgement{ChildID: request.ChildID, Status: "running"}, nil
	}
	return Acknowledgement{ChildID: request.ChildID, Status: "running", AckThrough: r.ackSoFar}, nil
}

// applied is every event the daemon accepted, in the order it accepted them.
// A seq applied twice would show up here twice, which is what makes
// exactly-once testable rather than asserted.
func (r *recordingReporter) applied() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Event(nil), r.events...)
}

func (r *recordingReporter) reportCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

// fastSpec is one run with a daemon that answers, because that is now the
// ordinary case. A run with no daemon, and a run whose daemon refuses, are
// each their own test below.
func fastSpec(t *testing.T, state string, command ...string) (Spec, *recordingReporter) {
	t.Helper()
	reporter := &recordingReporter{}
	return Spec{
		StateDir: state, OperationID: "op-" + t.Name(), Label: "test job",
		Command: command, StopPoll: 5 * time.Millisecond, StalePoll: 5 * time.Millisecond,
		StopGrace: 200 * time.Millisecond, Diagnostics: &bytes.Buffer{},
		Reporter: reporter, ReportRetry: 5 * time.Millisecond,
		ReportDeadline: 100 * time.Millisecond,
	}, reporter
}

// outboxOf is every event the supervisor queued, delivered or not.
func outboxOf(t *testing.T, state, childID string) []Event {
	t.Helper()
	outbox, err := OpenOutbox(state)
	if err != nil {
		t.Fatal(err)
	}
	defer outbox.Close()
	events, err := outbox.All(childID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	return events
}

// spoolOf keeps the name the neighbouring assertions use, and now reads what
// the daemon was actually TOLD rather than what a file happened to hold. That
// is the whole correction: the old helper could pass while no daemon had heard
// a word, which is the bug the first live two-machine test found.
func spoolOf(t *testing.T, reporter *recordingReporter) []Event {
	t.Helper()
	// Heartbeats are liveness, not lifecycle; a test that asserted on them
	// would be asserting on how long it took to run.
	events := make([]Event, 0, 8)
	for _, event := range reporter.applied() {
		if event.Kind == KindHeartbeat {
			continue
		}
		events = append(events, event)
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
	spec, reporter := fastSpec(t, state, "/bin/sh", "-c", "echo work; exit 7")
	result, err := Run(spec)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != OutcomeFailed || result.ExitCode != 7 {
		t.Fatalf("outcome %q exit %d; want failed with the child's own 7", result.Outcome, result.ExitCode)
	}
	events := spoolOf(t, reporter)
	if got := strings.Join(kindsOf(events), ","); got != "created,started,settled" {
		t.Fatalf("spool kinds = %s; want created,started,settled", got)
	}
	for i, event := range events {
		if event.Seq != uint64(i+1) {
			t.Fatalf("reported sequences must be dense and in order, got %+v", kindsOf(events))
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
	if len(reporter.calls) < 2 {
		t.Fatalf("want a registration before the start and a poke after the settle, got %d", len(reporter.calls))
	}
	if reporter.calls[0]["childID"] != result.ChildID || reporter.calls[0]["operationID"] != spec.OperationID {
		t.Fatalf("every registration must carry the same child id and operation id, got %+v", reporter.calls)
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

func TestAPreflightRefusalStartsNothingAndIsStillReportedAsAnOutcome(t *testing.T) {
	state := t.TempDir()
	spec, reporter := fastSpec(t, state, "/bin/sh", "-c", "echo never")
	spec.Inputs = []string{filepath.Join(state, "absent.bin")}
	result, err := Run(spec)
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != ExitPreflightRefused || len(result.Refusals) != 1 {
		t.Fatalf("want one refusal and exit %d, got %+v", ExitPreflightRefused, result)
	}
	// A refusal IS news. The old design dropped it — "nothing started, so
	// there is nothing to tell a chat" — which holds only when the caller is
	// standing here reading the receipt. A job started from another machine
	// has a chat waiting, and a refusal it never hears about is
	// indistinguishable from a job that vanished. This is the live test's
	// refused case.
	events := spoolOf(t, reporter)
	if got := strings.Join(kindsOf(events), ","); got != "created,settled" {
		t.Fatalf("reported kinds = %s; a refusal must be reported as a settle", got)
	}
	settled := events[len(events)-1].Payload
	if settled["outcome"] != OutcomeFailed {
		t.Fatalf("a refusal must settle failed, got %+v", settled)
	}
	if summary, _ := settled["summary"].(string); !strings.Contains(summary, "preflight refused") {
		t.Fatalf("the settle must say WHY nothing ran, got %q", summary)
	}
	if !result.Reporting.Delivered {
		t.Fatalf("a refusal must be delivered before the run exits, got %+v", result.Reporting)
	}
	if _, err := os.Stat(result.OutputFile); err == nil {
		if data, _ := os.ReadFile(result.OutputFile); len(data) > 0 {
			t.Fatalf("a refused run must write no output, got %q", data)
		}
	}
}

func TestMaxRuntimeSettlesTimedOutAndNotFailed(t *testing.T) {
	state := t.TempDir()
	spec, reporter := fastSpec(t, state, "/bin/sh", "-c", "sleep 30")
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
	events := spoolOf(t, reporter)
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
	spec, reporter := fastSpec(t, state, "/bin/sh", "-c", "sleep 30")
	done := make(chan Result, 1)
	go func() {
		result, runErr := Run(spec)
		if runErr != nil {
			t.Error(runErr)
		}
		done <- result
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if events := spoolOf(t, reporter); len(events) >= 2 && events[1].Kind == KindStarted {
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
		events := spoolOf(t, reporter)
		if got := strings.Join(kindsOf(events), ","); got != "created,started,cancel_requested,settled" {
			t.Fatalf("spool kinds = %s; a cancel must be recorded before it is acted on", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a stop file did not stop the job")
	}
}

func TestADeclaredStallIsSaidOnceAndTheJobKeepsRunning(t *testing.T) {
	state := t.TempDir()
	spec, reporter := fastSpec(t, state, "/bin/sh", "-c", "echo start; sleep 0.9; echo end")
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
	for _, event := range spoolOf(t, reporter) {
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

func TestARunWhoseDaemonRefusesEveryReportStillSettlesAndSaysSoLoudly(t *testing.T) {
	state := t.TempDir()
	spec, reporter := fastSpec(t, state, "/bin/sh", "-c", "exit 0")
	reporter.fail = os.ErrDeadlineExceeded
	diagnostics := &bytes.Buffer{}
	spec.Diagnostics = diagnostics
	result, err := Run(spec)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != OutcomeSettled {
		t.Fatalf("a run whose daemon refused every call must still settle, got %+v", result)
	}
	if result.Reporting.Delivered || result.Reporting.Pending == 0 {
		// The receipt must say it, not merely fail to claim it. A caller that
		// read `ok` here was the live test's actual failure.
		t.Fatalf("an undelivered run must report pending events, got %+v", result.Reporting)
	}
	if !strings.Contains(diagnostics.String(), "NOT REPORTED") {
		// Never silent, in the operator's words. A supervisor that could not
		// tell anybody must say so where somebody will see it.
		t.Fatalf("an undelivered outcome must be loud on stderr, got %q", diagnostics.String())
	}
	events := outboxOf(t, state, result.ChildID)
	if len(events) == 0 || events[len(events)-1].Kind != KindSettled {
		// A supervisor that needed a live daemon would fail exactly during a
		// self-update, which is when it is needed most. The outbox keeps the
		// outcome so the daemon can recover it.
		t.Fatalf("the outcome must be queued for delivery, got %v", kindsOf(events))
	}
	for _, event := range events {
		if event.Kind == KindSettled {
			continue
		}
	}
}

func TestTheChildRunsInItsOwnProcessGroup(t *testing.T) {
	state := t.TempDir()
	spec, reporter := fastSpec(t, state, "/bin/sh", "-c", "exit 0")
	if _, err := Run(spec); err != nil {
		t.Fatal(err)
	}
	started := spoolOf(t, reporter)[1]
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

func TestTheOutboxAllocatesDenseSeqsAndOnlyAnAckRetiresAnEvent(t *testing.T) {
	state := t.TempDir()
	outbox, err := OpenOutbox(state)
	if err != nil {
		t.Fatal(err)
	}
	defer outbox.Close()
	childID, err := IDForOperation("op-" + t.Name())
	if err != nil {
		t.Fatal(err)
	}
	first, err := outbox.Append(childID, KindCreated, map[string]any{"label": "one"})
	if err != nil || first.Seq != 1 {
		t.Fatalf("the first event must be seq 1, got %d / %v", first.Seq, err)
	}
	second, err := outbox.Append(childID, KindStarted, map[string]any{"pid": 4242})
	if err != nil || second.Seq != 2 {
		// The seq is allocated inside the write, from the rows already there,
		// which is what lets several supervisors share one file and what makes
		// a resumed supervisor continue a sequence rather than restart it.
		t.Fatalf("the second event must be seq 2, got %d / %v", second.Seq, err)
	}
	pending, err := outbox.Pending(childID, 0)
	if err != nil || len(pending) != 2 {
		t.Fatalf("both events must be queued until acknowledged, got %v / %v", kindsOf(pending), err)
	}
	if got := pending[1].Payload["pid"]; got != float64(4242) && got != 4242 {
		t.Fatalf("the payload must round-trip, got %#v", got)
	}
	if err := outbox.MarkDelivered(childID, 1); err != nil {
		t.Fatal(err)
	}
	pending, err = outbox.Pending(childID, 0)
	if err != nil || len(pending) != 1 || pending[0].Seq != 2 {
		// Only an ack retires an event. This is the whole of exactly-once:
		// anything unacknowledged is still in the queue and is re-sent.
		t.Fatalf("only acknowledged events leave the queue, got %v / %v", kindsOf(pending), err)
	}
	if events, err := outbox.All(childID, 0, 0); err != nil || len(events) != 2 {
		t.Fatalf("a reader sees delivered and undelivered alike, got %d / %v", len(events), err)
	}
}

func TestARelativeStateDirectoryIsRefusedRatherThanResolvedAgainstTheJob(t *testing.T) {
	// The bug this whole redesign started from: `state` resolved against the
	// JOB's working directory, so four jobs started with cwd=/tmp wrote their
	// whole lifecycle to /tmp/state and every one of them reported ok.
	if _, err := OpenOutbox("state"); err == nil {
		t.Fatal("a relative state directory must be refused, not resolved against the working directory")
	}
	if _, err := OpenOutbox(""); err == nil {
		t.Fatal("an empty state directory must be refused")
	}
}

func TestSeveralSupervisorsShareOneOutboxWithoutLosingASeq(t *testing.T) {
	state := t.TempDir()
	// One database, several writers: that is the production shape, since every
	// `workass run` on the machine writes the same file.
	const supervisors = 4
	const each = 5
	ids := make([]string, 0, supervisors)
	errs := make(chan error, supervisors)
	for index := 0; index < supervisors; index++ {
		childID, err := IDForOperation(fmt.Sprintf("op-%s-%d", t.Name(), index))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, childID)
		go func(childID string) {
			outbox, err := OpenOutbox(state)
			if err != nil {
				errs <- err
				return
			}
			defer outbox.Close()
			for seq := 0; seq < each; seq++ {
				if _, err := outbox.Append(childID, KindHeartbeat, nil); err != nil {
					errs <- err
					return
				}
			}
			errs <- nil
		}(childID)
	}
	for index := 0; index < supervisors; index++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	reader, err := OpenOutbox(state)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	for _, childID := range ids {
		events, err := reader.All(childID, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(events) != each {
			t.Fatalf("child %s has %d events, want %d", childID, len(events), each)
		}
		for index, event := range events {
			if event.Seq != uint64(index+1) {
				// Per child, dense and ordered, whoever else was writing.
				t.Fatalf("child %s seq %d at index %d; sequences are per child and dense", childID, event.Seq, index)
			}
		}
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
	reporter, resolved, err := ResolveCredential(contextPath)
	if err != nil || reporter == nil {
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
	// A --token-file is unlinked the moment it is read: the credential lives in
	// the process from then on, so leaving the file for the length of a
	// training run is a window with no purpose.
	if _, err := os.Stat(contextPath); err == nil {
		t.Fatal("a token file must be unlinked once it has been read")
	}
	loose := filepath.Join(contextDir, "loose.json")
	if err := os.WriteFile(loose, encoded, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ResolveCredential(loose); err == nil {
		// A credential in a world-readable file is a credential every process
		// on the machine holds.
		t.Fatal("a group- or world-readable credential file must be refused")
	}
}

func TestARunWithNoLabelOrNoCommandIsRefusedBeforeAnythingHappens(t *testing.T) {
	state := t.TempDir()
	spec, _ := fastSpec(t, state, "/bin/sh", "-c", "true")
	spec.Label = "  "
	if _, err := Run(spec); err == nil {
		t.Fatal("a job with no label must be refused: the label is the only name its chat will show")
	}
	spec, _ = fastSpec(t, state)
	if _, err := Run(spec); err == nil {
		t.Fatal("a job with no command must be refused")
	}
	spec, _ = fastSpec(t, state, "/bin/sh", "-c", "true")
	spec.Resource = "../escape"
	if _, err := Run(spec); err == nil {
		t.Fatal("a resource name that could escape its queue directory must be refused")
	}
}
