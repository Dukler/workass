package chat

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"workass/internal/provider"
)

func TestCancelOwnerLookupDoesNotWaitForActorStorage(t *testing.T) {
	engine, _ := newJournalReadyEngine(t, filepath.Join(t.TempDir(), "actor.json"))
	entered, release := make(chan struct{}), make(chan struct{})
	committed := make(chan error, 1)
	defer func() {
		close(release)
		if err := <-committed; err != nil {
			t.Errorf("fixture commit failed: %v", err)
		}
	}()
	go func() {
		committed <- engine.ApplyPrepared(Submit{OperationID: "queued-during-save", Text: "next", Presentation: provider.TurnPresentation{Origin: "human"}}, func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	select {
	case <-entered:
	case err := <-committed:
		committed <- err
		t.Fatalf("fixture rejected before persistence: %v", err)
	case <-time.After(time.Second):
		t.Fatal("fixture did not enter actor persistence")
	}
	lookedUp := make(chan bool, 1)
	go func() {
		lookedUp <- !engine.HasCancellableJob("another-chat-job") &&
			engine.HasCancellableJob("journal-native-turn") &&
			!engine.HasCancellableJob(provider.DeriveJobID("journal-chat", "queued-during-save"))
	}()
	select {
	case valid := <-lookedUp:
		if !valid {
			t.Fatal("routing did not retain exactly the last committed owners")
		}
	case <-time.After(time.Second):
		t.Fatal("Stop owner lookup waited for another actor's persistence")
	}
}

func TestCancelRoutingTracksCommittedQueueAndTerminal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "actor.json")
	engine, lane := newJournalReadyEngine(t, path)
	initial := engine.cancelRouting.Load()
	if err := engine.Apply(journalAssistantEvent(lane, 1, "chunk")); err != nil {
		t.Fatal(err)
	}
	if engine.cancelRouting.Load() != initial {
		t.Fatal("streaming rebuilt unchanged routing")
	}
	queued := Submit{OperationID: "queued-next", Text: "next", Presentation: provider.TurnPresentation{Origin: "human"}}
	queuedJob := provider.DeriveJobID("journal-chat", queued.OperationID)
	if err := engine.ApplyPrepared(queued, func() error { return errors.New("injected persistence failure") }); err == nil {
		t.Fatal("fixture persistence failure was ignored")
	}
	if engine.HasCancellableJob(queuedJob) || !engine.HasCancellableJob("journal-native-turn") {
		t.Fatal("failed save published a new routing owner")
	}
	if err := engine.Apply(queued); err != nil {
		t.Fatal(err)
	}
	if !engine.HasCancellableJob(queuedJob) || engine.HasCancellableJob("") {
		t.Fatal("committed queued input has incorrect routing")
	}
	recovered, err := NewDurableEngine("journal-chat", &memoryStateStore{state: engine.Snapshot(), ok: true})
	if err != nil {
		t.Fatal(err)
	}
	if !recovered.HasCancellableJob(queuedJob) || recovered.HasCancellableJob("journal-native-turn") {
		t.Fatal("restart failed to preserve the queued owner and remove the interrupted turn")
	}
	if err := engine.Apply(CancelPendingTurn{OperationID: queued.OperationID}); err != nil {
		t.Fatal(err)
	}
	if engine.HasCancellableJob(queuedJob) {
		t.Fatal("cancelled queued input retained routing")
	}
	terminal := ProviderEventReceived{ConnectionGeneration: 1, Event: provider.Event{
		Kind:     provider.EventTurnTerminal,
		Identity: provider.EventIdentity{ChatID: "journal-chat", LaneID: lane.ID, OperationID: "journal-turn", TurnID: "journal-native-turn", Sequence: 2},
		Terminal: &provider.TerminalEvent{Status: "cancelled", StopReason: "cancelled"},
	}}
	if err := engine.ApplyPrepared(terminal, func() error { return errors.New("injected terminal failure") }); err == nil {
		t.Fatal("fixture terminal failure was ignored")
	}
	if !engine.HasCancellableJob("journal-native-turn") {
		t.Fatal("uncommitted terminal removed the active owner")
	}
	if err := engine.Apply(terminal); err != nil {
		t.Fatal(err)
	}
	for _, jobID := range []string{"journal-native-turn", provider.DeriveJobID("journal-chat", "journal-turn"), queuedJob} {
		if engine.HasCancellableJob(jobID) {
			t.Fatal("terminal turn retained cancellation ownership")
		}
	}
	restarted, err := NewDurableEngine("journal-chat", FileStore{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if restarted.HasCancellableJob("journal-native-turn") || restarted.HasCancellableJob(queuedJob) {
		t.Fatal("restart resurrected terminal cancellation ownership")
	}
}
