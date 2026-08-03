package acp

import "strings"

// TurnDisposition is what a finished turn says about the user's REQUEST, which
// is a different question from what it says about the turn. A turn can end
// cleanly while the request is still owed — that is exactly what an async agent
// does when it parks on a lane and waits to be woken.
type TurnDisposition string

const (
	// DispositionUnknown is the honest answer when a signal cannot tell a
	// finished request from a park. It is not a failure: it hands the question
	// to the inference tier instead of guessing.
	DispositionUnknown    TurnDisposition = ""
	DispositionDone       TurnDisposition = "done"
	DispositionNeedsInput TurnDisposition = "needs_input"
	DispositionParked     TurnDisposition = "parked"
	// DispositionCancelled closes the obligation silently. The user stopped
	// this themselves, so it is never news to come back to.
	DispositionCancelled TurnDisposition = "cancelled"
	// DispositionDeferred means write nothing at all. The turn ended in a way
	// that carries no information about the request — currently only a daemon
	// restart, where the process is already exiting and the boot reconciliation
	// is what settles the record that survives. It is a control value and is
	// never persisted.
	DispositionDeferred TurnDisposition = "deferred"
)

const (
	dispositionSourceNative   = "native"
	dispositionSourceInferred = "inferred"
	// dispositionSourceHarness is the provider harness answering for itself:
	// the work it still has in flight and the wakes it has scheduled, read at
	// the instant its turn ends. Unlike the daemon's registry it cannot be
	// stale, which is why it is allowed to demote a claimed done.
	dispositionSourceHarness = "harness"
)

const maxDispositionNote = 1000

// TurnOutcome is the raw, provider-neutral end state of one turn. It is what
// the daemon knows without asking anyone: the ACP stop reason, the exit code
// Workass assigned, whether the daemon itself ended the turn, and the error
// text if there was one.
type TurnOutcome struct {
	StopReason  string
	ExitCode    *int
	Interrupted bool
	ErrorText   string
}

// CompletionSignal is one tier's answer about a turn, and which tier produced
// it. Source is kept because a declared answer and an inferred one deserve
// different trust, and because a receipt that cannot say who claimed a request
// was finished cannot attribute it later.
type CompletionSignal struct {
	Disposition TurnDisposition
	Source      string
	Note        string
}

// CompletionAdapter maps one provider's turn-end vocabulary onto the shared
// dispositions. Implementations MUST return DispositionUnknown rather than
// guess: mapping a clean yield to "done" is the bug this whole mechanism
// exists to fix, and an adapter is the one place where that mistake would look
// reasonable.
type CompletionAdapter interface {
	ProviderID() string
	FromNative(TurnOutcome) CompletionSignal
}

// acpCompletionAdapter reads the ACP vocabulary plus the three stop reasons
// Workass synthesises itself: "cancelled" (manager.go:1124), "engine-crash"
// (manager.go:1113) and "daemon-restart" (manager.go:954).
type acpCompletionAdapter struct{ providerID string }

func newACPCompletionAdapter(providerID string) acpCompletionAdapter {
	return acpCompletionAdapter{providerID: normalizeProviderID(providerID)}
}

func (a acpCompletionAdapter) ProviderID() string { return a.providerID }

// FromNative is ordered, and the order is the specification: the first match
// wins. Every branch above the default exists because that case is provably
// NOT a finished request; everything else is Unknown, because ACP's end_turn
// is emitted for a completed request and for a park alike (canary.go:873
// asserts it on plain success) and no amount of reading it harder will
// separate the two.
func (a acpCompletionAdapter) FromNative(outcome TurnOutcome) CompletionSignal {
	reason := strings.ToLower(strings.TrimSpace(outcome.StopReason))
	code, hasCode := 0, outcome.ExitCode != nil
	if hasCode {
		code = *outcome.ExitCode
	}
	signal := CompletionSignal{Source: dispositionSourceNative}

	switch {
	case outcome.Interrupted:
		// Set only under m.resetting (manager.go:951) — the daemon ended this,
		// not the user, and the process is on its way out. Writing a verdict
		// here would race the exit; the boot rule settles the record instead.
		signal.Disposition = DispositionDeferred
	case reason == "cancelled" || (hasCode && code == 130):
		signal.Disposition = DispositionCancelled
	case hasCode && code != 0:
		// The ACP error path (manager.go:1121) ends failed turns with code 1
		// and a stop reason of "" or "engine-crash". Inferring "done" from
		// those would contradict the red failed pill the renderer already
		// shows, and would be the loudest possible version of the lie this
		// mechanism is meant to end. A crash is not silence.
		signal.Disposition = DispositionNeedsInput
		// compactText redacts before bounding (bridge.go:1062), so this is the
		// whole secrets obligation for this field.
		signal.Note = compactText(outcome.ErrorText, maxDispositionNote)
	case reason == "refusal":
		signal.Disposition = DispositionNeedsInput
	case reason == "max_tokens", reason == "max_turn_requests":
		signal.Disposition = DispositionNeedsInput
	default:
		signal.Disposition = DispositionUnknown
	}
	return signal
}

// completionAdapterFor returns the adapter for a provider. Every ACP provider
// shares one today; a provider that grows a richer turn-end vocabulary gets
// its own adapter here and changes nothing else.
func completionAdapterFor(providerID string) CompletionAdapter {
	return newACPCompletionAdapter(providerID)
}
