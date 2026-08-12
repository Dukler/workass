package chat

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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

func TestCrashAfterTurnDispatchBlocksWithoutResend(t *testing.T) {
	store := &memoryStateStore{}
	engine, err := NewDurableEngine("chat", store)
	if err != nil {
		t.Fatal(err)
	}
	lane := testLane("chat", "codex")
	openReadyDurableLane(t, engine, lane)
	if err := engine.Apply(Submit{OperationID: "op", Text: "send exactly once"}); err != nil {
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
	if snapshot.Lanes[lane.ID].Phase != LaneBlocked || snapshot.Foreground == nil || snapshot.Foreground.Status != ForegroundUncertain {
		t.Fatalf("ambiguous dispatch did not fail closed: %#v", snapshot)
	}
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
		Messages: []LedgerEvent{{
			EventID: "source-assistant-event", MessageID: "source-assistant", Role: "assistant",
			Text: "completed turn", Status: "done", OperationID: "source-turn",
		}},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCheckpointRestorePendingSurvivesRestartButDispatchedNeverRepeats(t *testing.T) {
	store := &memoryStateStore{}
	engine, err := NewDurableEngine("checkpoint-chat", store)
	if err != nil {
		t.Fatal(err)
	}
	initializeCheckpointActor(t, engine)
	command := RestoreCheckpoint{OperationID: "restore-once", TurnSequence: 1, ObservedAtUnixMS: 1000}
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
	command := RestoreCheckpoint{OperationID: "restore-receipt", TurnSequence: 2, ObservedAtUnixMS: 2000}
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
	if err := engine.Apply(Submit{OperationID: "op-pending", Text: "not dispatched yet"}); err != nil {
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
	if err := engine.Apply(Submit{OperationID: "op", Text: "work"}); err != nil {
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
	if err := engine.Apply(Submit{OperationID: "turn", Text: "work"}); err != nil {
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
	if err := engine.Apply(Steer{OperationID: "steer", Text: "redirect"}); err != nil {
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
	if err := engine.Apply(Submit{OperationID: "turn", Text: "work"}); err != nil {
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

func TestFileStoreUpgradesVersionOneLedgerIdentitiesWithoutClaimingMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chat", "state.json")
	laneID := testLane("chat", "codex")
	thread := provider.ThreadRef{ProviderID: "codex", RootID: "thread", HeadID: "thread", Lineage: 1}
	state, err := NewState("chat")
	if err != nil {
		t.Fatal(err)
	}
	state.Lanes[laneID.ID] = LaneState{
		Identity: laneID, Thread: thread, Phase: LaneReady,
		Coverage: map[uint64]CoverageRecord{
			1: {Sequence: 1, EventID: "event:operation:user", Status: CoverageNativeSeen, DeliveryID: "operation"},
			2: {Sequence: 2, EventID: "event:operation:assistant", Status: CoverageNativeSeen, DeliveryID: "operation"},
		},
		CoveredThrough: 2, Context: exactContext(provider.ContextImportUnsupported),
	}
	state.Operations["operation"] = struct{}{}
	state.Ledger = []LedgerEvent{
		{EventID: "event:operation:user", Sequence: 1, Role: "user", Text: "question", LaneID: laneID.ID, ProviderID: "codex", OperationID: "operation"},
		{EventID: "event:operation:assistant", Sequence: 2, Role: "assistant", Text: "answer", LaneID: laneID.ID, ProviderID: "codex", OperationID: "operation", TerminalState: "completed"},
	}
	input := QueueEntry{OperationID: "operation", LaneID: laneID.ID, Presentation: provider.TurnPresentation{
		UserMessageID: "user-public", AssistantMessageID: "assistant-public",
	}}
	state.Outbox = []OutboxEntry{{
		ID: "turn:operation", Kind: EffectStartTurn, Status: OutboxCompleted,
		LaneID: laneID.ID, OperationID: "operation", Input: &input,
	}}
	raw, err := json.Marshal(stateEnvelope{Version: 1, State: state})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	loaded, ok, err := (FileStore{Path: path}).Load("chat")
	if err != nil || !ok {
		t.Fatalf("load version one actor: ok=%v err=%v", ok, err)
	}
	if got := loaded.Ledger[0].MessageID; got != "user-public" {
		t.Fatalf("user message id = %q", got)
	}
	if got := loaded.Ledger[1].MessageID; got != "assistant-public" {
		t.Fatalf("assistant message id = %q", got)
	}
	if loaded.Ledger[0].Status != "done" || loaded.Ledger[1].Status != "done" {
		t.Fatalf("version one statuses were not normalized: %#v", loaded.Ledger)
	}
	if loaded.Migration.Complete {
		t.Fatal("version one sidecar falsely claimed complete legacy-mirror migration")
	}
	if err := (FileStore{Path: path}).Save(loaded); err != nil {
		t.Fatal(err)
	}
	var saved stateEnvelope
	savedRaw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(savedRaw, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Version != currentStateEnvelopeVersion {
		t.Fatalf("saved envelope version = %d", saved.Version)
	}
}
