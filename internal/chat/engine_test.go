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
		StableInputIdentity: true, ConsumptionReceipt: true, TurnReadback: true,
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
	assertUnrecoverableTurnTerminalized(t, snapshot, lane.ID, "op", provider.ErrorAcceptanceAmbiguous)
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
		Delivery: provider.DeliveryCapabilities{StableInputIdentity: true, ConsumptionReceipt: true, TurnReadback: true},
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

func TestCrashAfterAdmissionQueuesReadbackNotResend(t *testing.T) {
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
	effect, ok, err := restarted.ClaimNext()
	if err != nil || !ok {
		t.Fatalf("claim exact resume: ok=%v err=%v", ok, err)
	}
	resume, ok := effect.(ResumeLaneEffect)
	if !ok {
		t.Fatalf("post-admission crash claimed %T, want exact resume", effect)
	}
	if err := restarted.Apply(LaneOpened{
		LaneID: lane.ID, Identity: lane, Thread: resume.Thread,
		ConnectionGeneration: resume.Generation, Context: exactContext(provider.ContextImportNonSampling),
		Delivery: provider.DeliveryCapabilities{StableInputIdentity: true, ConsumptionReceipt: true, TurnReadback: true},
	}); err != nil {
		t.Fatal(err)
	}
	effect, ok, err = restarted.ClaimNext()
	if err != nil || !ok {
		t.Fatalf("claim reconciliation after resume: ok=%v err=%v", ok, err)
	}
	if _, ok := effect.(ReconcileTurnEffect); !ok {
		t.Fatalf("post-resume recovery claimed %T, want reconciliation", effect)
	}
}

func TestCrashDuringExactResumeMayRetrySameThread(t *testing.T) {
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
	second, ok, err := restarted.ClaimNext()
	if err != nil || !ok {
		t.Fatalf("reclaim exact resume: ok=%v err=%v", ok, err)
	}
	retry := second.(ResumeLaneEffect)
	if !retry.Thread.Equal(resume.Thread) || retry.Generation != resume.Generation {
		t.Fatalf("resume retry changed identity: first=%#v retry=%#v", resume, retry)
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

func TestCrashRecoveryNeverBlindlyResendsSteerButMayRepeatCancel(t *testing.T) {
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
	if snapshot.PendingSteer == nil || snapshot.PendingSteer.Status != SteerUncertain || !outboxHas(&snapshot, steerEffectID("steer"), OutboxAmbiguous) {
		t.Fatalf("crashed steer did not fail closed: %#v", snapshot)
	}
	for _, entry := range snapshot.Outbox {
		if entry.Kind == EffectSteerTurn && entry.Status == OutboxPending {
			t.Fatalf("uncertain steer was made retryable: %#v", entry)
		}
	}

	// A cancellation is idempotent control of the exact same native turn, so it
	// is the one control operation recovery may safely reclaim.
	snapshot.PendingSteer = nil
	for index := range snapshot.Outbox {
		snapshot.Outbox[index].Status = OutboxCompleted
	}
	store.state = snapshot
	recoveredForCancel, err := NewDurableEngine("chat", store)
	if err != nil {
		t.Fatal(err)
	}
	if err := recoveredForCancel.Apply(CancelTurn{OperationID: "cancel"}); err != nil {
		t.Fatal(err)
	}
	firstRecovery, ok, err := recoveredForCancel.ClaimNext()
	if err != nil || !ok {
		t.Fatalf("claim lane recovery: ok=%v err=%v", ok, err)
	}
	resume, ok := firstRecovery.(ResumeLaneEffect)
	if !ok {
		t.Fatalf("claim before cancel = %T, want exact resume", firstRecovery)
	}
	if err := recoveredForCancel.Apply(LaneOpened{
		LaneID: lane.ID, Identity: lane, Thread: resume.Thread,
		ConnectionGeneration: resume.Generation, Context: exactContext(provider.ContextImportNonSampling),
		Delivery: provider.DeliveryCapabilities{StableInputIdentity: true, ConsumptionReceipt: true, TurnReadback: true},
	}); err != nil {
		t.Fatal(err)
	}
	if effect, ok, err := recoveredForCancel.ClaimNext(); err != nil || !ok {
		t.Fatalf("claim cancellation after resume: ok=%v err=%v", ok, err)
	} else if _, ok := effect.(CancelTurnEffect); !ok {
		t.Fatalf("claimed %T after resume, want cancel", effect)
	}
	restartedAgain, err := NewDurableEngine("chat", store)
	if err != nil {
		t.Fatal(err)
	}
	if effect, ok, err := restartedAgain.ClaimNext(); err != nil || !ok {
		t.Fatalf("reclaim idempotent cancel: ok=%v err=%v", ok, err)
	} else if _, ok := effect.(CancelTurnEffect); !ok {
		t.Fatalf("reclaimed %T, want cancel", effect)
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
	if snapshot.Permissions["permission"].Event.Status != "uncertain" || !outboxHas(&snapshot, permissionEffectID("decision"), OutboxAmbiguous) {
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
	if allocations > 64 {
		t.Fatalf("digest allocations = %.1f, want <= 64 independent of ledger payload volume", allocations)
	}
}
