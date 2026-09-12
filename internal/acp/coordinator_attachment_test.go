package acp

import (
	"context"
	"testing"
	"time"

	chatengine "workass/internal/chat"
	providercontract "workass/internal/provider"
)

// A pre-prompt rejection detaches the actor without destroying the transport.
// The next exact resume can reuse that transport, so retiring the old lane only
// after constructing its replacement would close the replacement's session.
func TestCoordinatorRetiresOldAttachmentBeforeExactResume(t *testing.T) {
	fixture := newPersistentMockFixture(t, "resume")
	manager, _ := fixture.newManagerTuned(func(opts *Options) { opts.InitTimeout = 2 * time.Second })
	t.Cleanup(func() { manager.Reset() })
	engine, err := chatengine.NewEngine("native-chat")
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := chatengine.NewCoordinator(engine, manager)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = coordinator.Close(context.Background()) })
	selection, err := manager.ResolveProviderLaneSelection(context.Background(), SessionOptions{TabID: "native-tab", ChatID: "native-chat", ProviderID: "mock", CWD: manager.opts.RootDir})
	if err != nil {
		t.Fatal(err)
	}
	apply := func(command chatengine.Command) {
		t.Helper()
		if err := engine.Apply(command); err != nil {
			t.Fatal(err)
		}
	}
	drain := func() {
		t.Helper()
		if err := coordinator.Drain(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	submit := func(op string) {
		t.Helper()
		apply(chatengine.Submit{OperationID: providercontract.OperationID(op), Text: op, Presentation: providercontract.TurnPresentation{Origin: "human"}})
	}
	wait := func(op string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			snapshot := engine.Snapshot()
			if snapshot.Foreground == nil && len(snapshot.Ledger) > 0 {
				last := snapshot.Ledger[len(snapshot.Ledger)-1]
				if last.OperationID == providercontract.OperationID(op) && last.Status == "done" {
					return
				}
				t.Fatalf("turn did not complete: status=%s state=%s", last.Status, last.TerminalState)
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("turn never reached its provider terminal")
	}
	apply(chatengine.SelectLane{Identity: selection.Identity, Owner: providercontract.AttachmentOwner{TabID: "native-tab"}, CWD: manager.opts.RootDir})
	drain()
	submit("first")
	drain()
	wait("first")
	snapshot := engine.Snapshot()
	laneID := snapshot.ActiveLaneID
	thread := snapshot.Lanes[laneID].Thread
	submit("rejected-before-prompt")
	effect, claimed, err := engine.ClaimNext()
	if err != nil || !claimed {
		t.Fatal("could not claim rejected input", err)
	}
	if _, ok := effect.(chatengine.StartTurnEffect); !ok {
		t.Fatalf("unexpected effect %T", effect)
	}
	apply(chatengine.TurnAdmissionFailed{OperationID: "rejected-before-prompt", Kind: providercontract.ErrorAdmissionRejected})
	submit("after-rejection")
	drain()
	wait("after-rejection")
	if engine.Snapshot().Lanes[laneID].Thread != thread || persistentMockSessionCount(t, fixture.sessionFile) != 1 {
		t.Fatal("resume replaced the original native thread")
	}
}
