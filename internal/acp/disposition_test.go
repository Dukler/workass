package acp

import (
	"strings"
	"testing"
)

func TestCleanYieldIsNeverReadAsDone(t *testing.T) {
	adapter := completionAdapterFor("claude")
	got := adapter.FromNative(TurnOutcome{StopReason: "end_turn", ExitCode: intPtr(0)})
	if got.Disposition != DispositionUnknown {
		t.Fatalf("end_turn = %q, want Unknown: it is emitted for a finished request and a park alike", got.Disposition)
	}
	if got.Source != dispositionSourceNative {
		t.Fatalf("source = %q, want %q", got.Source, dispositionSourceNative)
	}
	// An unrecognised vocabulary is not evidence of anything either.
	for _, reason := range []string{"", "something_new"} {
		if d := adapter.FromNative(TurnOutcome{StopReason: reason, ExitCode: intPtr(0)}).Disposition; d != DispositionUnknown {
			t.Fatalf("stopReason %q = %q, want Unknown", reason, d)
		}
	}
}

func TestBlockedOnTheUserIsReadFromTheNativeSignal(t *testing.T) {
	adapter := completionAdapterFor("claude")
	for _, reason := range []string{"refusal", "max_tokens", "max_turn_requests"} {
		got := adapter.FromNative(TurnOutcome{StopReason: reason, ExitCode: intPtr(0)})
		if got.Disposition != DispositionNeedsInput {
			t.Fatalf("stopReason %q = %q, want needs_input", reason, got.Disposition)
		}
	}
}

func TestFailedTurnNeedsTheUserAndNeverReadsAsDone(t *testing.T) {
	adapter := completionAdapterFor("claude")
	// The ACP error path ends failed turns with code 1 and a stop reason of ""
	// or "engine-crash" (manager.go:1113, :1121). Both must reach the user.
	for _, reason := range []string{"", "engine-crash"} {
		got := adapter.FromNative(TurnOutcome{StopReason: reason, ExitCode: intPtr(1), ErrorText: "boom"})
		if got.Disposition != DispositionNeedsInput {
			t.Fatalf("failed turn (stopReason %q) = %q, want needs_input", reason, got.Disposition)
		}
	}
}

func TestFailureNoteIsRedactedAndBounded(t *testing.T) {
	adapter := completionAdapterFor("claude")
	got := adapter.FromNative(TurnOutcome{
		ExitCode:  intPtr(1),
		ErrorText: "engine died: api_key=do-not-expose " + strings.Repeat("x", 4000),
	})
	if strings.Contains(got.Note, "do-not-expose") {
		t.Fatalf("failure note leaked a secret: %q", got.Note)
	}
	// compactText truncates to max-1 and appends a 3-byte ellipsis, so the
	// package-wide bound is max+2. Assert that convention rather than a
	// stricter one this field does not actually promise.
	if len(got.Note) > maxDispositionNote+2 {
		t.Fatalf("failure note = %d bytes, want <= %d", len(got.Note), maxDispositionNote+2)
	}
}

func TestUserCancelIsNotAnOutcome(t *testing.T) {
	adapter := completionAdapterFor("claude")
	for _, outcome := range []TurnOutcome{
		{StopReason: "cancelled", ExitCode: intPtr(130)},
		{StopReason: "", ExitCode: intPtr(130)},
	} {
		if d := adapter.FromNative(outcome).Disposition; d != DispositionCancelled {
			t.Fatalf("user cancel = %q, want cancelled", d)
		}
	}
}

func TestDaemonRestartDefersRatherThanVerdicts(t *testing.T) {
	adapter := completionAdapterFor("claude")
	// A restart-ended turn carries status failed AND a non-zero code, so the
	// interrupted branch must be reached first — otherwise a daemon restart
	// would report itself to the user as "needs input".
	got := adapter.FromNative(TurnOutcome{
		StopReason: "daemon-restart", ExitCode: intPtr(1), Interrupted: true, ErrorText: "torn down",
	})
	if got.Disposition != DispositionDeferred {
		t.Fatalf("daemon restart = %q, want deferred", got.Disposition)
	}
}

func TestDispositionIsOmittedUntilThereIsOne(t *testing.T) {
	// Job.Public() feeds both job:start (manager.go:925) and job:end (:968).
	// A starting turn has no verdict, and emitting an empty one would invite a
	// client to read "" as an answer.
	job := &Job{ID: "j1", Status: "running"}
	if _, present := job.Public()["disposition"]; present {
		t.Fatal("a starting turn must not carry a disposition")
	}
	job.DispositionState, job.DispositionSource = string(DispositionParked), dispositionSourceInferred
	got, present := job.Public()["disposition"].(map[string]any)
	if !present || got["state"] != string(DispositionParked) || got["source"] != dispositionSourceInferred {
		t.Fatalf("disposition = %#v", job.Public()["disposition"])
	}
}
