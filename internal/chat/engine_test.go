package chat

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"workass/internal/provider"
)

type memoryStateStore struct {
	state State
	ok    bool
	fail  bool
}

func (s *memoryStateStore) Load(chatID string) (State, bool, error) {
	if !s.ok {
		return State{}, false, nil
	}
	if s.state.ChatID != chatID {
		return State{}, false, errors.New("wrong chat")
	}
	return s.state.Clone(), true, nil
}

func (s *memoryStateStore) Save(state State) error {
	if s.fail {
		return errors.New("injected state-store failure")
	}
	s.state = state.Clone()
	s.ok = true
	return nil
}

func openReadyDurableLane(t *testing.T, engine *Engine, lane provider.LaneIdentity) {
	openReadyDurableLaneWithDelivery(t, engine, lane, provider.DeliveryCapabilities{
		StableInputIdentity: true, ConsumptionReceipt: true,
	})
}

func openReadyDurableLaneWithDelivery(t *testing.T, engine *Engine, lane provider.LaneIdentity, delivery provider.DeliveryCapabilities) {
	t.Helper()
	if err := engine.Apply(SelectLane{Identity: lane}); err != nil {
		t.Fatal(err)
	}
	effect, ok, err := engine.ClaimNext()
	if err != nil || !ok {
		t.Fatalf("claim create: ok=%v err=%v", ok, err)
	}
	create, ok := effect.(CreateLaneEffect)
	if !ok {
		t.Fatalf("claimed %T, want create", effect)
	}
	if err := engine.Apply(LaneOpened{
		LaneID:               lane.ID,
		Thread:               provider.ThreadRef{ProviderID: lane.Realm.ProviderID, RootID: "native-thread", HeadID: "native-thread", Lineage: 1},
		ConnectionGeneration: 1,
		Context:              exactContext(provider.ContextImportNonSampling),
		Delivery:             delivery,
	}); err != nil {
		t.Fatal(err)
	}
	if create.Generation != 1 {
		t.Fatalf("create generation = %d", create.Generation)
	}
}

func TestDurableEnginePersistsDispatchBeforeReturningEffect(t *testing.T) {
	store := &memoryStateStore{}
	engine, err := NewDurableEngine("chat", store)
	if err != nil {
		t.Fatal(err)
	}
	lane := testLane("chat", "codex")
	if err := engine.Apply(SelectLane{Identity: lane}); err != nil {
		t.Fatal(err)
	}
	if got := store.state.Outbox[0].Status; got != OutboxPending {
		t.Fatalf("stored status before claim = %q", got)
	}
	if _, ok, err := engine.ClaimNext(); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if got := store.state.Outbox[0].Status; got != OutboxDispatched {
		t.Fatalf("effect returned before dispatch commit: %q", got)
	}
}

func TestDurableEnginePreparedCommitRunsOnlyAfterReducerAcceptance(t *testing.T) {
	store := &memoryStateStore{}
	engine, err := NewDurableEngine("prepared-chat", store)
	if err != nil {
		t.Fatal(err)
	}
	presentation := PresentationState{TabID: "prepared-tab", Title: "Prepared"}
	prepared := 0
	if err := engine.ApplyPrepared(InitializeChat{
		Presentation: presentation, OperationID: "prepared-create", Digest: "prepared-digest",
	}, func() error {
		prepared++
		return nil
	}); err != nil {
		t.Fatalf("accepted prepared commit: %v", err)
	}
	if prepared != 1 || !engine.Snapshot().Initialized {
		t.Fatalf("accepted preparation count=%d initialized=%v", prepared, engine.Snapshot().Initialized)
	}

	if err := engine.ApplyPrepared(InitializeChat{
		Presentation: presentation, OperationID: "prepared-create", Digest: "changed-digest",
	}, func() error {
		prepared++
		return nil
	}); err == nil {
		t.Fatal("conflicting command was accepted")
	}
	if prepared != 1 {
		t.Fatalf("reducer rejection ran durable preparation %d times", prepared)
	}
}

func TestDurableEnginePreparationFailureDoesNotPublishActorState(t *testing.T) {
	store := &memoryStateStore{}
	engine, err := NewDurableEngine("prepared-failure-chat", store)
	if err != nil {
		t.Fatal(err)
	}
	before := engine.Snapshot()
	storedRevision := store.state.Revision
	wantErr := errors.New("injected attachment preparation failure")
	err = engine.ApplyPrepared(InitializeChat{
		Presentation: PresentationState{TabID: "prepared-failure-tab"},
		OperationID:  "prepared-failure-create", Digest: "prepared-failure-digest",
	}, func() error { return wantErr })
	if !errors.Is(err, wantErr) {
		t.Fatalf("prepared failure = %v, want %v", err, wantErr)
	}
	if state := engine.Snapshot(); state.Initialized || state.Revision != before.Revision {
		t.Fatalf("failed preparation published actor state: %#v", state)
	}
	if !store.ok || store.state.Initialized || store.state.Revision != storedRevision {
		t.Fatalf("failed preparation changed persisted actor state: %#v", store.state)
	}
}

func TestExternalMutationJournalClaimsBeforeEffectAndFailsClosedOnRecovery(t *testing.T) {
	store := &memoryStateStore{}
	engine, err := NewDurableEngine("browser-chat", store)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Apply(InitializeChat{
		Presentation: PresentationState{TabID: "tab-a"}, OperationID: "create-browser", Digest: "create-browser-digest",
	}); err != nil {
		t.Fatal(err)
	}
	rawBrowserInput := `{"selector":"#save","text":"password=do-not-persist"}`
	rawDigest := sha256.Sum256([]byte(rawBrowserInput))
	digest := hex.EncodeToString(rawDigest[:])
	command := RecordExternalMutation{
		OperationID: "agent-mcp:browser-once", Kind: "workass_browser_click", Method: "browser.click",
		TabID: "tab-a", Digest: digest,
	}
	if err := engine.Apply(command); err != nil {
		t.Fatal(err)
	}
	state := engine.Snapshot()
	if len(state.Outbox) != 1 || state.Outbox[0].Status != OutboxPending {
		t.Fatalf("external mutation journal = %#v", state.Outbox)
	}
	if encoded, err := json.Marshal(state); err != nil {
		t.Fatal(err)
	} else if strings.Contains(string(encoded), rawBrowserInput) || strings.Contains(string(encoded), "do-not-persist") {
		t.Fatalf("raw browser input reached durable state: %s", encoded)
	}
	if _, claimed, err := engine.ClaimNext(); err != nil || claimed {
		t.Fatalf("generic coordinator claimed external mutation: claimed=%v err=%v", claimed, err)
	}
	effect, claimed, err := engine.ClaimEffect(state.Outbox[0].ID)
	if err != nil || !claimed {
		t.Fatalf("exact external claim: claimed=%v err=%v", claimed, err)
	}
	if _, ok := effect.(ExternalMutationEffect); !ok {
		t.Fatalf("exact external claim returned %T", effect)
	}
	if got := store.state.Outbox[0].Status; got != OutboxDispatched {
		t.Fatalf("stored external status before effect = %q", got)
	}
	if err := engine.Apply(RecordExternalMutation{
		OperationID: command.OperationID, Kind: command.Kind, Method: command.Method, TabID: command.TabID,
		Digest: strings.Repeat("b", 64),
	}); err == nil {
		t.Fatal("changed request was accepted for an existing operation")
	}

	restarted, err := NewDurableEngine("browser-chat", store)
	if err != nil {
		t.Fatal(err)
	}
	recovered := restarted.Snapshot()
	if recovered.Outbox[0].Status != OutboxAmbiguous || recovered.Outbox[0].LastError != provider.ErrorAcceptanceAmbiguous {
		t.Fatalf("dispatched external mutation did not become ambiguous: %#v", recovered.Outbox[0])
	}
	if _, claimed, err := restarted.ClaimEffect(recovered.Outbox[0].ID); err != nil || claimed {
		t.Fatalf("ambiguous external mutation became replayable: claimed=%v err=%v", claimed, err)
	}
	if err := restarted.Apply(command); err != nil {
		t.Fatalf("same operation retry was not idempotent: %v", err)
	}
	if err := restarted.Apply(ExternalMutationReceipt{
		OperationID: command.OperationID, Kind: command.Kind, Method: command.Method, TabID: command.TabID,
		Digest: digest,
	}); err != nil {
		t.Fatalf("readback receipt did not resolve ambiguity: %v", err)
	}
	if got := restarted.Snapshot().Outbox[0].Status; got != OutboxCompleted {
		t.Fatalf("resolved external mutation status = %q", got)
	}
}

func TestCrashAfterTurnDispatchTerminalizesWithoutResend(t *testing.T) {
	store := &memoryStateStore{}
	engine, err := NewDurableEngine("chat", store)
	if err != nil {
		t.Fatal(err)
	}
	lane := testLane("chat", "codex")
	openReadyDurableLaneWithDelivery(t, engine, lane, provider.DeliveryCapabilities{})
	if err := engine.Apply(Submit{OperationID: "op", Text: "send exactly once", Presentation: provider.TurnPresentation{Origin: "human"}}); err != nil {
		t.Fatal(err)
	}
	effect, ok, err := engine.ClaimNext()
	if err != nil || !ok {
		t.Fatalf("claim turn: ok=%v err=%v", ok, err)
	}
	start, ok := effect.(StartTurnEffect)
	if !ok || start.Input.Text != "send exactly once" {
		t.Fatalf("claimed effect = %#v", effect)
	}

	restarted, err := NewDurableEngine("chat", store)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := restarted.Snapshot()
	assertInterruptedTurnTerminalized(t, snapshot, lane.ID, "op", provider.ErrorTransientTransport)
	if got := snapshot.Outbox[len(snapshot.Outbox)-1].Status; got != OutboxAmbiguous {
		t.Fatalf("turn outbox status = %q", got)
	}
	if _, ok, err := restarted.ClaimNext(); err != nil || ok {
		t.Fatalf("ambiguous turn was made executable again: ok=%v err=%v", ok, err)
	}
}

func initializeCheckpointActor(t *testing.T, engine *Engine) {
	t.Helper()
	if err := engine.Apply(InitializeFork{
		SourceChatID: "source-chat", Presentation: PresentationState{TabID: "tab"},
		OperationID: "checkpoint-fork", Digest: "checkpoint-fork-digest",
		Messages: []LedgerEvent{{
			EventID: "source-assistant-event", MessageID: "source-assistant", Role: "assistant",
			Text: "completed turn", Status: "done", OperationID: "source-turn",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := engine.Apply(UpdateEnvironment{
		ExpectedTabID: "tab", Payload: json.RawMessage(`{"chatId":"checkpoint-chat"}`),
		Checkpoints: json.RawMessage(`[ {"turnSeq":1,"repos":[]}, {"turnSeq":2,"repos":[]} ]`),
		Reference:   json.RawMessage(`null`),
	}); err != nil {
		t.Fatal(err)
	}
}

func checkpointRestoreCommand(t *testing.T, engine *Engine, operationID provider.OperationID, turnSequence int, observedAt int64) RestoreCheckpoint {
	t.Helper()
	payload, digest, err := engine.Snapshot().CheckpointRestoreTarget(turnSequence)
	if err != nil {
		t.Fatal(err)
	}
	return RestoreCheckpoint{
		OperationID: operationID, TurnSequence: turnSequence, ObservedAtUnixMS: observedAt,
		Checkpoint: payload, CheckpointDigest: digest,
	}
}

func TestCheckpointRestorePendingSurvivesRestartButDispatchedNeverRepeats(t *testing.T) {
	store := &memoryStateStore{}
	engine, err := NewDurableEngine("checkpoint-chat", store)
	if err != nil {
		t.Fatal(err)
	}
	initializeCheckpointActor(t, engine)
	command := checkpointRestoreCommand(t, engine, "restore-once", 1, 1000)
	if err := engine.Apply(command); err != nil {
		t.Fatal(err)
	}

	restartedBeforeClaim, err := NewDurableEngine("checkpoint-chat", store)
	if err != nil {
		t.Fatal(err)
	}
	effect, ok, err := restartedBeforeClaim.ClaimNext()
	if err != nil || !ok {
		t.Fatalf("claim pending restore: ok=%v err=%v", ok, err)
	}
	restore, ok := effect.(RestoreCheckpointEffect)
	if !ok || restore.OperationID != "restore-once" || restore.TurnSequence != 1 {
		t.Fatalf("claimed restore = %#v", effect)
	}

	restartedAfterClaim, err := NewDurableEngine("checkpoint-chat", store)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := restartedAfterClaim.Snapshot()
	var receipt OutboxEntry
	for _, entry := range snapshot.Outbox {
		if entry.Kind == EffectCheckpointRestore {
			receipt = entry
		}
	}
	if receipt.Status != OutboxAmbiguous || receipt.LastError != provider.ErrorAcceptanceAmbiguous {
		t.Fatalf("dispatched restore did not fail closed: %#v", receipt)
	}
	if _, claimed, claimErr := restartedAfterClaim.ClaimNext(); claimErr != nil || claimed {
		t.Fatalf("ambiguous restore became executable: claimed=%v err=%v", claimed, claimErr)
	}
}

func TestCheckpointRestoreReceiptIsIdempotentAndOwnsTimeline(t *testing.T) {
	store := &memoryStateStore{}
	engine, err := NewDurableEngine("checkpoint-receipt-chat", store)
	if err != nil {
		t.Fatal(err)
	}
	initializeCheckpointActor(t, engine)
	command := checkpointRestoreCommand(t, engine, "restore-receipt", 2, 2000)
	if err := engine.Apply(command); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := engine.ClaimNext(); err != nil || !ok {
		t.Fatalf("claim restore: ok=%v err=%v", ok, err)
	}
	result := json.RawMessage(`{"ok":true,"chatId":"checkpoint-receipt-chat","turnSeq":2,"repos":[]}`)
	if err := engine.Apply(CheckpointRestored{OperationID: "restore-receipt", TurnSequence: 2, Result: result}); err != nil {
		t.Fatal(err)
	}
	if err := engine.Apply(command); err != nil {
		t.Fatalf("retry same restore receipt: %v", err)
	}
	snapshot := engine.Snapshot()
	if len(snapshot.Ledger) != 1 || len(snapshot.Ledger[0].Timeline) != 1 {
		t.Fatalf("checkpoint timeline = %#v", snapshot.Ledger)
	}
	timeline := snapshot.Ledger[0].Timeline[0]
	if timeline.Kind != provider.EventCheckpointRestored || timeline.Restored == nil || timeline.Restored.TurnSequence != 2 {
		t.Fatalf("checkpoint timeline receipt = %#v", timeline)
	}
	count := 0
	for _, entry := range snapshot.Outbox {
		if entry.Kind == EffectCheckpointRestore && entry.OperationID == "restore-receipt" {
			count++
			if entry.Status != OutboxCompleted || string(entry.Result) != string(result) {
				t.Fatalf("checkpoint outbox receipt = %#v", entry)
			}
		}
	}
	if count != 1 {
		t.Fatalf("checkpoint restore outbox count = %d", count)
	}
}

func TestRestartAttachesExactLaneBeforePendingTurn(t *testing.T) {
	store := &memoryStateStore{}
	engine, err := NewDurableEngine("chat", store)
	if err != nil {
		t.Fatal(err)
	}
	lane := testLane("chat", "codex")
	openReadyDurableLane(t, engine, lane)
	if err := engine.Apply(Submit{OperationID: "op-pending", Text: "not dispatched yet", Presentation: provider.TurnPresentation{Origin: "human"}}); err != nil {
		t.Fatal(err)
	}
	if got := store.state.Outbox[len(store.state.Outbox)-1].Status; got != OutboxPending {
		t.Fatalf("turn status before restart = %q", got)
	}

	restarted, err := NewDurableEngine("chat", store)
	if err != nil {
		t.Fatal(err)
	}
	effect, ok, err := restarted.ClaimNext()
	if err != nil || !ok {
		t.Fatalf("claim after restart: ok=%v err=%v", ok, err)
	}
	resume, ok := effect.(ResumeLaneEffect)
	if !ok || resume.Thread.RootID != "native-thread" {
		t.Fatalf("first recovery effect = %#v, want exact resume", effect)
	}
	if err := restarted.Apply(LaneOpened{
		LaneID: lane.ID, Identity: lane, Thread: resume.Thread,
		ConnectionGeneration: resume.Generation, Context: exactContext(provider.ContextImportNonSampling),
		Delivery: provider.DeliveryCapabilities{StableInputIdentity: true, ConsumptionReceipt: true},
	}); err != nil {
		t.Fatal(err)
	}
	effect, ok, err = restarted.ClaimNext()
	if err != nil || !ok {
		t.Fatalf("claim pending turn after resume: ok=%v err=%v", ok, err)
	}
	start, ok := effect.(StartTurnEffect)
	if !ok || start.Input.OperationID != "op-pending" {
		t.Fatalf("second recovery effect = %#v, want original pending turn", effect)
	}
}

func TestCrashAfterAdmissionTerminalizesAndNextPromptResumesSavedThread(t *testing.T) {
	store := &memoryStateStore{}
	engine, err := NewDurableEngine("chat", store)
	if err != nil {
		t.Fatal(err)
	}
	lane := testLane("chat", "codex")
	openReadyDurableLane(t, engine, lane)
	if err := engine.Apply(Submit{OperationID: "op", Text: "work", Presentation: provider.TurnPresentation{Origin: "human"}}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := engine.ClaimNext(); err != nil || !ok {
		t.Fatalf("claim turn: ok=%v err=%v", ok, err)
	}
	if err := engine.Apply(TurnAdmitted{
		OperationID: "op", Accepted: true,
		Turn: provider.TurnRef{OperationID: "op", NativeID: "native-turn"},
	}); err != nil {
		t.Fatal(err)
	}

	restarted, err := NewDurableEngine("chat", store)
	if err != nil {
		t.Fatal(err)
	}
	state := restarted.Snapshot()
	if state.Foreground != nil || len(state.Ledger) != 2 || state.Lanes[lane.ID].Phase != LaneDetached {
		t.Fatalf("restart retained provider-turn ownership: %#v", state)
	}
	if _, ok, err := restarted.ClaimNext(); err != nil || ok {
		t.Fatalf("restart manufactured provider work: ok=%v err=%v", ok, err)
	}
	if err := restarted.Apply(Submit{OperationID: "op-next", Text: "continue", Presentation: provider.TurnPresentation{Origin: "human"}}); err != nil {
		t.Fatal(err)
	}
	effect, ok, err := restarted.ClaimNext()
	if err != nil || !ok {
		t.Fatalf("claim next prompt attachment: ok=%v err=%v", ok, err)
	}
	resume, ok := effect.(ResumeLaneEffect)
	if !ok || resume.Thread.RootID != "native-thread" {
		t.Fatalf("next prompt did not resume saved thread: %#v", effect)
	}
}

func TestCrashDuringExactResumeWaitsForNewIntent(t *testing.T) {
	store := &memoryStateStore{}
	engine, err := NewDurableEngine("chat", store)
	if err != nil {
		t.Fatal(err)
	}
	lane := testLane("chat", "codex")
	openReadyDurableLane(t, engine, lane)
	if err := engine.Apply(HostLost{LaneID: lane.ID, ConnectionGeneration: 1}); err != nil {
		t.Fatal(err)
	}
	if err := engine.Apply(SelectLane{Identity: lane}); err != nil {
		t.Fatal(err)
	}
	first, ok, err := engine.ClaimNext()
	if err != nil || !ok {
		t.Fatalf("claim resume: ok=%v err=%v", ok, err)
	}
	resume := first.(ResumeLaneEffect)

	restarted, err := NewDurableEngine("chat", store)
	if err != nil {
		t.Fatal(err)
	}
	if second, ok, err := restarted.ClaimNext(); err != nil || ok {
		t.Fatalf("restart retried the stale attachment: effect=%#v ok=%v err=%v", second, ok, err)
	}
	if err := restarted.Apply(Submit{OperationID: "resume-next", Text: "new prompt", Presentation: provider.TurnPresentation{Origin: "human"}}); err != nil {
		t.Fatal(err)
	}
	second, ok, err := restarted.ClaimNext()
	if err != nil || !ok {
		t.Fatalf("new prompt did not request exact resume: ok=%v err=%v", ok, err)
	}
	retry := second.(ResumeLaneEffect)
	if !retry.Thread.Equal(resume.Thread) || retry.Generation <= resume.Generation {
		t.Fatalf("new prompt changed or reused stale attachment identity: first=%#v next=%#v", resume, retry)
	}
}

func TestPersistenceFailureDoesNotPublishTransition(t *testing.T) {
	store := &memoryStateStore{}
	engine, err := NewDurableEngine("chat", store)
	if err != nil {
		t.Fatal(err)
	}
	before := engine.Snapshot()
	store.fail = true
	if err := engine.Apply(SelectLane{Identity: testLane("chat", "codex")}); err == nil {
		t.Fatal("state-store failure was ignored")
	}
	after := engine.Snapshot()
	if len(after.Lanes) != len(before.Lanes) || len(after.Outbox) != len(before.Outbox) {
		t.Fatalf("failed persistence leaked state: before=%#v after=%#v", before, after)
	}
}

func TestCrashRecoveryNeverResendsTurnOrSteer(t *testing.T) {
	store := &memoryStateStore{}
	engine, err := NewDurableEngine("chat", store)
	if err != nil {
		t.Fatal(err)
	}
	lane := testLane("chat", "alpha")
	openReadyDurableLane(t, engine, lane)
	if err := engine.Apply(Submit{OperationID: "turn", Text: "work", Presentation: provider.TurnPresentation{Origin: "human"}}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := engine.ClaimNext(); err != nil || !ok {
		t.Fatalf("claim turn: ok=%v err=%v", ok, err)
	}
	if err := engine.Apply(TurnAdmitted{
		OperationID: "turn", Accepted: true, Turn: provider.TurnRef{OperationID: "turn", NativeID: "native-turn"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := engine.Apply(Steer{OperationID: "steer", Text: "redirect", Presentation: provider.TurnPresentation{Origin: "human"}}); err != nil {
		t.Fatal(err)
	}
	if effect, ok, err := engine.ClaimNext(); err != nil || !ok {
		t.Fatalf("claim steer: ok=%v err=%v", ok, err)
	} else if _, ok := effect.(SteerTurnEffect); !ok {
		t.Fatalf("claimed %T, want steer", effect)
	}

	restarted, err := NewDurableEngine("chat", store)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := restarted.Snapshot()
	if snapshot.Foreground != nil || snapshot.PendingSteer != nil || !outboxHas(&snapshot, steerEffectID("steer"), OutboxAmbiguous) {
		t.Fatalf("crashed turn and steer were not terminalized fail-closed: %#v", snapshot)
	}
	if len(snapshot.Ledger) < 3 || snapshot.Ledger[len(snapshot.Ledger)-1].SteerState != string(SteerUncertain) {
		t.Fatalf("crashed steer lost its visible uncertain receipt: %#v", snapshot.Ledger)
	}
	for _, entry := range snapshot.Outbox {
		if entry.Kind == EffectSteerTurn && entry.Status == OutboxPending {
			t.Fatalf("uncertain steer was made retryable: %#v", entry)
		}
	}
}

func TestCrashRecoveryNeverReplaysDispatchedCancel(t *testing.T) {
	store := &memoryStateStore{}
	engine, err := NewDurableEngine("cancel-crash-chat", store)
	if err != nil {
		t.Fatal(err)
	}
	lane := testLane("cancel-crash-chat", "alpha")
	openReadyDurableLane(t, engine, lane)
	if err := engine.Apply(Submit{OperationID: "turn", Text: "work", Presentation: provider.TurnPresentation{Origin: "human"}}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := engine.ClaimNext(); err != nil || !ok {
		t.Fatalf("claim turn: ok=%v err=%v", ok, err)
	}
	if err := engine.Apply(TurnAdmitted{
		OperationID: "turn", Accepted: true, Turn: provider.TurnRef{OperationID: "turn", NativeID: "native-turn"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := engine.Apply(CancelTurn{OperationID: "cancel"}); err != nil {
		t.Fatal(err)
	}
	if effect, ok, err := engine.ClaimNext(); err != nil || !ok {
		t.Fatalf("claim cancel: ok=%v err=%v", ok, err)
	} else if _, ok := effect.(CancelTurnEffect); !ok {
		t.Fatalf("claimed %T, want cancel", effect)
	}

	restarted, err := NewDurableEngine("cancel-crash-chat", store)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := restarted.Snapshot()
	if snapshot.Foreground != nil || snapshot.PendingCancel != nil || !outboxHas(&snapshot, cancelEffectID("cancel"), OutboxAmbiguous) {
		t.Fatalf("dispatched cancel survived restart as live turn control: %#v", snapshot)
	}
	if effect, claimed, err := restarted.ClaimNext(); err != nil || claimed {
		t.Fatalf("restart replayed provider turn control: effect=%#v claimed=%v err=%v", effect, claimed, err)
	}
}

func TestCrashAfterPermissionDecisionFailsClosedWithoutResend(t *testing.T) {
	store := &memoryStateStore{}
	engine, err := NewDurableEngine("chat", store)
	if err != nil {
		t.Fatal(err)
	}
	lane := testLane("chat", "alpha")
	openReadyDurableLane(t, engine, lane)
	if err := engine.Apply(Submit{OperationID: "turn", Text: "work", Presentation: provider.TurnPresentation{Origin: "human"}}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := engine.ClaimNext(); err != nil || !ok {
		t.Fatalf("claim turn: ok=%v err=%v", ok, err)
	}
	if err := engine.Apply(TurnAdmitted{
		OperationID: "turn", Accepted: true, Turn: provider.TurnRef{OperationID: "turn", NativeID: "native-turn"},
	}); err != nil {
		t.Fatal(err)
	}
	permission := provider.Event{
		Kind: provider.EventPermissionRequested,
		Identity: provider.EventIdentity{
			ChatID: "chat", LaneID: lane.ID, OperationID: "turn", TurnID: "native-turn", Sequence: 1,
		},
		Permission: &provider.PermissionEvent{RequestID: "permission", Status: "pending", Options: []string{"allow"}},
	}
	if err := engine.Apply(ProviderEventReceived{ConnectionGeneration: 1, Event: permission}); err != nil {
		t.Fatal(err)
	}
	if err := engine.Apply(ResolvePermission{OperationID: "decision", RequestID: "permission", OptionID: "allow"}); err != nil {
		t.Fatal(err)
	}
	if effect, ok, err := engine.ClaimNext(); err != nil || !ok {
		t.Fatalf("claim permission: ok=%v err=%v", ok, err)
	} else if _, ok := effect.(ResolvePermissionEffect); !ok {
		t.Fatalf("claimed %T, want permission", effect)
	}
	restarted, err := NewDurableEngine("chat", store)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := restarted.Snapshot()
	if snapshot.Foreground != nil || len(snapshot.Permissions) != 0 || !outboxHas(&snapshot, permissionEffectID("decision"), OutboxAmbiguous) {
		t.Fatalf("permission decision was not failed closed: %#v", snapshot)
	}
	for _, entry := range snapshot.Outbox {
		if entry.Kind == EffectPermission && entry.Status == OutboxPending {
			t.Fatalf("uncertain permission was made retryable: %#v", entry)
		}
	}
}

func TestFileStoreRoundTripIsPrivateAndValidated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chat", "state.json")
	store := FileStore{Path: path}
	engine, err := NewDurableEngine("chat", store)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Apply(SelectLane{Identity: testLane("chat", "codex")}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("chat state permissions = %o", got)
	}
	reloaded, err := NewDurableEngine("chat", store)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Snapshot().Outbox) != 1 {
		t.Fatalf("file-backed outbox was not restored: %#v", reloaded.Snapshot())
	}
}

func TestDigestSnapshotMatchesVisibleMessageAndQueueIdentityWithoutBodies(t *testing.T) {
	engine, err := NewEngine("chat-digest")
	if err != nil {
		t.Fatal(err)
	}
	engine.state.Initialized = true
	engine.state.Revision = 42
	engine.state.Presentation = PresentationState{
		TabID: "tab-digest", ProviderID: "codex", CurrentModelID: "gpt-test", CurrentModeID: "agent",
		PresentationRevision: 3, WorkspaceRevision: 4, AgentQueueRevision: 5, RuntimeControlRevision: 7,
	}
	engine.state.Ledger = []LedgerEvent{
		{MessageID: "ledger-user", Text: "body must not enter the digest"},
		{MessageID: "ledger-assistant", Text: "another private body"},
	}
	engine.state.StagedQueue = []StagedQueueEntry{{ID: "staged-head", Text: "queued private body"}}
	engine.state.Queue = []QueueEntry{{OperationID: "agent-queue", Presentation: provider.TurnPresentation{QueueID: "agent-tail", Origin: "agent"}}}
	engine.state.Foreground = &ForegroundTurn{
		OperationID:               "foreground-op",
		Input:                     QueueEntry{Presentation: provider.TurnPresentation{UserMessageID: "foreground-user", Origin: "human"}},
		Turn:                      provider.TurnRef{NativeID: "job-live"},
		RootAssistantMessageID:    "foreground-assistant",
		CurrentAssistantMessageID: "foreground-assistant",
	}
	engine.state.PendingSteer = &PendingSteer{
		OperationID:  "steer-op",
		Presentation: provider.TurnPresentation{UserMessageID: "steer-user", AssistantMessageID: "steer-assistant", Origin: "human"},
	}
	engine.state.Permissions = map[string]PermissionState{
		"permission-z":        {Event: provider.PermissionEvent{Status: "pending"}},
		"permission-a":        {Event: provider.PermissionEvent{Status: "pending"}},
		"permission-resolved": {Event: provider.PermissionEvent{Status: "resolved"}},
	}

	digest := engine.DigestSnapshot()
	if digest.ChatID != "chat-digest" || digest.TabID != "tab-digest" || digest.ActorRevision != 42 {
		t.Fatalf("digest identity = %#v", digest)
	}
	if digest.PresentationRevision != 3 {
		t.Fatalf("digest presentation revisions = %#v", digest)
	}
	if digest.MessageCount != 6 || digest.LastMessageID != "steer-assistant" || digest.RunningJobID != "job-live" {
		t.Fatalf("digest transcript = %#v", digest)
	}
	if digest.QueueLen != 2 || digest.QueueHeadID != "staged-head" {
		t.Fatalf("digest queue = %#v", digest)
	}
	if digest.AgentQueueRevision != 5 || digest.RuntimeControlRevision != 7 ||
		digest.ProviderID != "codex" || digest.CurrentModelID != "gpt-test" || digest.CurrentModeID != "agent" {
		t.Fatalf("digest controls = %#v", digest)
	}
	if got := strings.Join(digest.PendingPermissionIDs, ","); got != "permission-a,permission-z" {
		t.Fatalf("digest permissions = %q", got)
	}
}

func TestDigestSnapshotAllocationsStayBoundedByIdentityProjection(t *testing.T) {
	engine, err := NewEngine("chat-large-digest")
	if err != nil {
		t.Fatal(err)
	}
	engine.state.Initialized = true
	engine.state.Presentation.TabID = "tab-large-digest"
	const rows = 10_000
	engine.state.Ledger = make([]LedgerEvent, rows)
	for index := range engine.state.Ledger {
		engine.state.Ledger[index] = LedgerEvent{
			MessageID: fmt.Sprintf("message-%05d", index),
			Text:      strings.Repeat("private body ", 20),
			Timeline:  []TimelineEntry{{Key: "tool", Kind: provider.EventToolUpdate, Tool: &provider.ToolEvent{Title: "private tool payload"}}},
		}
	}
	var digest DigestSnapshot
	allocations := testing.AllocsPerRun(5, func() { digest = engine.DigestSnapshot() })
	if digest.MessageCount != rows || digest.LastMessageID != "message-09999" {
		t.Fatalf("large digest identity = %#v", digest)
	}
	if allocations > 8 {
		t.Fatalf("idle digest allocations = %.1f, want <= 8 independent of ledger payload volume", allocations)
	}
}

func BenchmarkDigestSnapshotLargeIdleLedger(b *testing.B) {
	engine, err := NewEngine("chat-large-idle-digest-benchmark")
	if err != nil {
		b.Fatal(err)
	}
	engine.state.Initialized = true
	engine.state.Presentation.TabID = "tab-large-idle-digest-benchmark"
	engine.state.Ledger = make([]LedgerEvent, 100_000)
	for index := range engine.state.Ledger {
		engine.state.Ledger[index] = LedgerEvent{MessageID: fmt.Sprintf("message-%06d", index), Text: strings.Repeat("body ", 20)}
	}
	b.ReportAllocs()
	for range b.N {
		_ = engine.DigestSnapshot()
	}
}

func TestReadProjectionSnapshotBoundsHistoryAndIsolatesMutableState(t *testing.T) {
	engine, err := NewEngine("chat-read-projection")
	if err != nil {
		t.Fatal(err)
	}
	engine.state.Initialized = true
	engine.state.Presentation = PresentationState{TabID: "tab-read-projection", Title: "Original"}
	engine.state.Ledger = make([]LedgerEvent, 100)
	for index := range engine.state.Ledger {
		engine.state.Ledger[index] = LedgerEvent{
			MessageID: fmt.Sprintf("message-%03d", index), Text: strings.Repeat("body ", 100),
			Attachments: []provider.Attachment{{ID: "attachment", Ref: "session-image:fixture"}},
			Timeline:    []TimelineEntry{{Key: "tool", Kind: provider.EventToolUpdate, Tool: &provider.ToolEvent{Title: "tool"}}},
		}
	}
	laneID := provider.LaneID("lane")
	engine.state.Lanes[laneID] = LaneState{
		Identity: provider.LaneIdentity{ID: laneID},
		Coverage: map[uint64]CoverageRecord{1: {Sequence: 1, EventID: "event-1"}},
	}
	engine.state.ActiveLaneID = laneID
	engine.state.Usage[laneID] = provider.UsageEvent{Used: 12, Size: 100}
	engine.state.Operations["historical-operation"] = struct{}{}

	snapshot := engine.ReadProjectionSnapshot(10)
	if snapshot.LedgerOffset != 90 || snapshot.LedgerCount != 100 || len(snapshot.State.Ledger) != 10 {
		t.Fatalf("bounded snapshot = offset:%d count:%d rows:%d", snapshot.LedgerOffset, snapshot.LedgerCount, len(snapshot.State.Ledger))
	}
	if snapshot.State.Ledger[0].MessageID != "message-090" || snapshot.State.Ledger[9].MessageID != "message-099" {
		t.Fatalf("bounded ledger range = %q..%q", snapshot.State.Ledger[0].MessageID, snapshot.State.Ledger[9].MessageID)
	}
	if snapshot.State.Lanes[laneID].Coverage != nil || snapshot.State.Operations != nil || snapshot.State.Outbox != nil {
		t.Fatalf("read projection copied actor-internal history: %#v", snapshot.State)
	}

	snapshot.State.Presentation.Title = "Changed"
	snapshot.State.Ledger[0].Attachments[0].Ref = "changed"
	readback := engine.Snapshot()
	if readback.Presentation.Title != "Original" || readback.Ledger[90].Attachments[0].Ref != "session-image:fixture" {
		t.Fatal("read projection aliases mutable actor state")
	}
	identity := engine.IdentitySnapshot()
	if identity.ChatID != "chat-read-projection" || identity.TabID != "tab-read-projection" || identity.Deleted {
		t.Fatalf("identity snapshot = %#v", identity)
	}
}

func TestReadLedgerPageBeforeUsesStableBoundaryAndCopiesOnlyThatPage(t *testing.T) {
	engine, err := NewEngine("chat-ledger-page")
	if err != nil {
		t.Fatal(err)
	}
	engine.state.Ledger = make([]LedgerEvent, 100)
	for index := range engine.state.Ledger {
		engine.state.Ledger[index] = LedgerEvent{
			MessageID: fmt.Sprintf("message-%03d", index), Text: fmt.Sprintf("row %d", index),
			Attachments: []provider.Attachment{{Ref: "session-image:fixture"}},
		}
	}

	page, err := engine.ReadLedgerPageBefore("message-060", 40)
	if err != nil {
		t.Fatal(err)
	}
	if page.Start != 20 || page.End != 60 || page.LedgerCount != 100 || len(page.Events) != 40 {
		t.Fatalf("ledger page = start:%d end:%d total:%d rows:%d", page.Start, page.End, page.LedgerCount, len(page.Events))
	}
	if page.Events[0].MessageID != "message-020" || page.Events[39].MessageID != "message-059" {
		t.Fatalf("ledger page range = %q..%q", page.Events[0].MessageID, page.Events[39].MessageID)
	}
	page.Events[0].Attachments[0].Ref = "changed"
	if engine.Snapshot().Ledger[20].Attachments[0].Ref != "session-image:fixture" {
		t.Fatal("ledger page aliases actor attachments")
	}
	if _, err := engine.ReadLedgerPageBefore("missing", 40); err == nil {
		t.Fatal("missing stable page boundary must fail closed")
	}
	if _, err := engine.ReadLedgerPageBefore("message-060", 0); err == nil {
		t.Fatal("non-positive page limit must fail closed")
	}
}

func BenchmarkReadProjectionSnapshotLargeLedger(b *testing.B) {
	engine, err := NewEngine("chat-read-projection-benchmark")
	if err != nil {
		b.Fatal(err)
	}
	engine.state.Initialized = true
	engine.state.Presentation.TabID = "tab-read-projection-benchmark"
	engine.state.Ledger = make([]LedgerEvent, 10_000)
	for index := range engine.state.Ledger {
		engine.state.Ledger[index] = LedgerEvent{
			MessageID: fmt.Sprintf("message-%05d", index), Text: strings.Repeat("large body ", 100),
			Timeline: []TimelineEntry{{Key: "tool", Kind: provider.EventToolUpdate, Tool: &provider.ToolEvent{Title: "tool payload"}}},
		}
	}
	b.Run("full", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			_ = engine.Snapshot()
		}
	})
	b.Run("tail-10", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			_ = engine.ReadProjectionSnapshot(10)
		}
	})
	b.Run("page-40-before-tail", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			if _, err := engine.ReadLedgerPageBefore("message-09940", 40); err != nil {
				b.Fatal(err)
			}
		}
	})
}
