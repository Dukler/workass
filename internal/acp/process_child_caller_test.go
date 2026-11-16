package acp

import (
	"os"
	"testing"
	"time"

	"workass/internal/procchild"
)

// bug-20261116-7ab17943: a `workass run` whose caller was still waiting handed
// that caller its outcome as the receipt. The settle says so, and the daemon
// suppresses the chat notice on that fact; the receipt and the history stay.
func TestASettleTheWaitingCallerReceivedIsNotNotifiedAgain(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		received   any
		outcome    string
		suppressed bool
	}{
		{name: "received", received: true, outcome: procchild.OutcomeSettled, suppressed: true},
		{name: "received a failure", received: true, outcome: procchild.OutcomeFailed, suppressed: true},
		// The wire may carry a flag as a string, as it does numbers.
		{name: "received over the wire", received: "true", outcome: procchild.OutcomeSettled, suppressed: true},
		{name: "caller gone", outcome: procchild.OutcomeSettled},
		{name: "caller gone, failed", received: false, outcome: procchild.OutcomeFailed},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			lifecycle := &recordedLifecycle{}
			manager := processChildManager(t, lifecycle)
			childID, err := procchild.IDForOperation("op-caller-" + testCase.name)
			if err != nil {
				t.Fatal(err)
			}
			pid := os.Getpid()
			settled := map[string]any{"outcome": testCase.outcome, "exitCode": 0, "summary": "Finished"}
			if testCase.received != nil {
				settled[procchild.SettledPayloadCallerReceived] = testCase.received
			}
			now := time.Now().UTC()
			if _, err := manager.RegisterProcessChild(ProcessChildRegistrationOptions{
				OwnerKey: "owner-proc", ParentChatID: "chat-proc", ParentTabID: "tab-proc",
				WorkID: childID, Label: "deploy", SupervisorPID: &pid,
				OutputFile: liveOutputPath(t, manager.opts.StateDir, childID),
				Events: []procchild.Event{
					{Seq: 1, At: now, Kind: procchild.KindCreated, Payload: map[string]any{"label": "deploy"}},
					{Seq: 2, At: now, Kind: procchild.KindStarted, Payload: map[string]any{"pid": 4242, "pgid": 4242}},
					{Seq: 3, At: now, Kind: procchild.KindSettled, Payload: settled},
				},
			}); err != nil {
				t.Fatal(err)
			}
			var exits []ProcessLifecycleEvent
			for _, event := range lifecycle.snapshot() {
				if event.Kind == ProcessLifecycleExit {
					exits = append(exits, event)
				}
			}
			if len(exits) != 1 {
				t.Fatalf("want one exit event (it is still history), got %+v", exits)
			}
			if exits[0].NoticeSuppressed != testCase.suppressed {
				t.Fatalf("notice suppressed = %v (%q), want %v", exits[0].NoticeSuppressed, exits[0].SuppressReason, testCase.suppressed)
			}
			if testCase.suppressed && exits[0].SuppressReason == "" {
				t.Fatal("a suppressed notice must say why")
			}
			if receipt := receiptFor(t, manager, childID); receipt.Outcome != testCase.outcome {
				t.Fatalf("the receipt must still carry the outcome, got %q", receipt.Outcome)
			}
		})
	}
}
