package chat

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"workass/internal/provider"
)

func testLane(chatID, providerID string) provider.LaneIdentity {
	return provider.LaneIdentity{
		ChatID: chatID,
		Realm: provider.Realm{
			ProviderID: provider.ID(providerID), MachineID: "machine", AccountScope: "default", InstallScope: "official",
		},
		WorkspaceEpoch: "workspace",
	}.Normalize()
}

func exactContext(importMode provider.ContextImportMode) provider.ContextCapabilities {
	capabilities := provider.ContextCapabilities{ExactResume: true, ImportMode: importMode, NativeCompaction: true, VerifiedLineage: true}
	if importMode == provider.ContextImportNonSampling {
		capabilities.ImportReadback = true
		capabilities.IdempotentImport = true
		capabilities.MaxImportEvents = 64
		capabilities.MaxImportBytes = 1 << 20
	}
	return capabilities
}

func apply(t *testing.T, state State, command Command) (State, []Effect) {
	t.Helper()
	next, effects, err := Reduce(state, command)
	if err != nil {
		t.Fatalf("reduce %T: %v", command, err)
	}
	return next, effects
}

func TestAdmissionRejectionPersistsVisibleFailedTurnWithoutClaimingNativeConsumption(t *testing.T) {
	state, _ := NewState("chat")
	laneID := testLane("chat", "codex")
	state, _ = apply(t, state, SelectLane{Identity: laneID})
	state, _ = apply(t, state, LaneOpened{
		LaneID:               laneID.ID,
		Thread:               provider.ThreadRef{ProviderID: "codex", RootID: "thread", HeadID: "thread", Lineage: 1},
		ConnectionGeneration: 1,
		Context:              exactContext(provider.ContextImportUnsupported),
	})
	state, _ = apply(t, state, Submit{
		OperationID: "rejected", Text: "keep this prompt",
		Presentation: provider.TurnPresentation{
			UserMessageID: "public-user", AssistantMessageID: "public-assistant", StartedAt: "2026-08-11T12:00:00Z",
		},
	})
	state, _ = apply(t, state, TurnAdmissionFailed{OperationID: "rejected", Kind: provider.ErrorProviderUnavailable})

	if state.Foreground != nil || len(state.Ledger) != 2 {
		t.Fatalf("rejected admission state = foreground:%#v ledger:%#v", state.Foreground, state.Ledger)
	}
	user, assistant := state.Ledger[0], state.Ledger[1]
	if user.MessageID != "public-user" || user.Text != "keep this prompt" || user.Status != "done" {
		t.Fatalf("rejected user row = %#v", user)
	}
	if assistant.MessageID != "public-assistant" || assistant.Status != "failed" || !assistant.Interrupted || assistant.RetryPrompt != "keep this prompt" {
		t.Fatalf("rejected assistant row = %#v", assistant)
	}
	if assistant.Terminal == nil || assistant.Terminal.Status != "failed" || assistant.Terminal.Error != string(provider.ErrorProviderUnavailable) {
		t.Fatalf("rejected terminal receipt = %#v", assistant.Terminal)
	}
	lane := state.Lanes[laneID.ID]
	if lane.Phase != LaneBlocked || lane.CoveredThrough != 2 || lane.Coverage[1].Status != CoverageExcluded || lane.Coverage[2].Status != CoverageExcluded {
		t.Fatalf("rejected lane coverage = %#v", lane)
	}
	if err := state.Validate(); err != nil {
		t.Fatalf("rejected state did not survive durable validation: %v", err)
	}
}

func TestBackgroundActionJournalPreservesPendingAndNeverReplaysDispatched(t *testing.T) {
	state, _ := NewState("chat")
	laneID := testLane("chat", "codex")
	state, _ = apply(t, state, InitializeChat{Presentation: PresentationState{TabID: "tab"}, OperationID: "create:test", Digest: "create-test"})
	state, _ = apply(t, state, SelectLane{Identity: laneID, Owner: provider.AttachmentOwner{TabID: "tab"}})
	state, _ = apply(t, state, LaneOpened{
		LaneID:               laneID.ID,
		Thread:               provider.ThreadRef{ProviderID: "codex", RootID: "thread", HeadID: "thread", Lineage: 1},
		ConnectionGeneration: 1,
		Context:              exactContext(provider.ContextImportUnsupported),
	})
	state, _ = apply(t, state, Submit{OperationID: "turn", Text: "coordinate"})
	state, _ = apply(t, state, TurnAdmitted{
		OperationID: "turn", Accepted: true, Turn: provider.TurnRef{OperationID: "turn", NativeID: "native-turn"},
	})
	action := BackgroundAction{
		Kind: BackgroundSpawnAgent, OperationID: "spawn-op", TabID: "tab", ChatID: "chat",
		Owner: ProviderActivityOwner{LaneID: laneID.ID, OperationID: "turn", TurnID: "native-turn", ConnectionGeneration: 1},
		Spawn: &SpawnAgentAction{Prompt: "review"},
	}
	state, _ = apply(t, state, RequestBackgroundAction{Action: action})
	if len(state.Outbox) == 0 || state.Outbox[len(state.Outbox)-1].Status != OutboxPending {
		t.Fatalf("background command was not durably pending: %#v", state.Outbox)
	}
	state, _ = apply(t, state, RecoverOutbox{})
	entry := OutboxEntry{}
	for _, candidate := range state.Outbox {
		if candidate.Kind == EffectBackground && candidate.OperationID == "spawn-op" {
			entry = candidate
			break
		}
	}
	if entry.Status != OutboxPending {
		t.Fatalf("never-dispatched background action was lost on restart: %#v", entry)
	}
	state, effects := apply(t, state, ClaimEffect{EffectID: entry.ID})
	if len(effects) != 1 {
		t.Fatalf("pending background action was not claimed exactly once: %#v", effects)
	}
	if effect, ok := effects[0].(BackgroundActionEffect); !ok || effect.Action.OperationID != "spawn-op" {
		t.Fatalf("claimed background action = %#v", effects[0])
	}
	state, _ = apply(t, state, RecoverOutbox{})
	for _, candidate := range state.Outbox {
		if candidate.Kind == EffectBackground && candidate.OperationID == "spawn-op" {
			entry = candidate
			break
		}
	}
	if entry.Status != OutboxAmbiguous || entry.LastError != provider.ErrorAcceptanceAmbiguous {
		t.Fatalf("dispatched background action became replayable: %#v", entry)
	}
}

func TestBackgroundReconciliationPreservesTerminalRowsAndOrphansMissingLiveWork(t *testing.T) {
	state, _ := NewState("chat")
	lane := testLane("chat", "codex")
	state, _ = apply(t, state, InitializeChat{Presentation: PresentationState{TabID: "tab"}, OperationID: "create", Digest: "digest"})
	state, _ = apply(t, state, SelectLane{Identity: lane, Owner: provider.AttachmentOwner{TabID: "tab"}})
	state, _ = apply(t, state, LaneOpened{
		LaneID: lane.ID, Thread: provider.ThreadRef{ProviderID: "codex", RootID: "thread", HeadID: "thread", Lineage: 1},
		ConnectionGeneration: 1, Context: exactContext(provider.ContextImportUnsupported),
	})
	owner := ProviderActivityOwner{LaneID: lane.ID, OperationID: "turn", TurnID: "native-turn", ConnectionGeneration: 1}
	state.Background["running"] = BackgroundState{Owner: owner, Event: provider.BackgroundEvent{WorkID: "running", Status: "running"}}
	state.Background["done"] = BackgroundState{Owner: owner, Event: provider.BackgroundEvent{WorkID: "done", Status: "exited", FinishedAt: "2026-08-11T11:00:00Z"}}

	state, _ = apply(t, state, ReconcileBackgroundSnapshot{
		AuthoritativeAbsence: true, ObservedAt: "2026-08-11T12:00:00Z",
	})
	if got := state.Background["running"].Event; got.Status != "orphaned" || got.FinishedAt != "2026-08-11T12:00:00Z" {
		t.Fatalf("missing live work was not made explicitly orphaned: %#v", got)
	}
	if got := state.Background["done"].Event; got.Status != "exited" || got.FinishedAt != "2026-08-11T11:00:00Z" {
		t.Fatalf("missing terminal history was erased or rewritten: %#v", got)
	}

	state, _ = apply(t, state, ReconcileBackgroundSnapshot{Items: []BackgroundState{{
		Owner: owner, Event: provider.BackgroundEvent{WorkID: "new", Status: "running"},
	}}})
	if len(state.Background) != 3 || state.Background["new"].Event.Status != "running" {
		t.Fatalf("runtime evidence replaced actor-owned background history: %#v", state.Background)
	}
}

func TestLegacyObligationAndBackgroundUpgradeRemainSeparateFromV2TranscriptDigest(t *testing.T) {
	state, _ := NewState("chat")
	state, _ = apply(t, state, MigrateLegacyChat{
		Version: 2, Digest: "v2-transcript-digest", Presentation: PresentationState{TabID: "tab"},
	})
	if state.Migration.LegacyObligationMigrated || state.Migration.LegacyBackgroundMigrated {
		t.Fatalf("v2 transcript migration prematurely claimed later stores: %#v", state.Migration)
	}
	obligation := &ObligationState{
		State: "needs_input", Source: "declared", OpenedAt: "2026-08-11T12:00:00Z", UpdatedAt: "2026-08-11T12:00:00Z",
	}
	state, _ = apply(t, state, MigrateLegacyObligation{Obligation: obligation})
	state, _ = apply(t, state, MigrateLegacyBackground{Items: nil})
	if !state.Migration.LegacyObligationMigrated || !state.Migration.LegacyBackgroundMigrated || state.Obligation == nil {
		t.Fatalf("post-v2 migration acknowledgements = %#v obligation=%#v", state.Migration, state.Obligation)
	}
	// A daemon crash before the global cutover receipt replays each exact actor
	// migration. The transcript digest and the two later inputs stay idempotent.
	state, _ = apply(t, state, MigrateLegacyChat{
		Version: 2, Digest: "v2-transcript-digest", Presentation: PresentationState{TabID: "tab"},
	})
	state, _ = apply(t, state, MigrateLegacyObligation{Obligation: obligation})
	state, _ = apply(t, state, MigrateLegacyBackground{Items: nil})
	if _, _, err := Reduce(state, MigrateLegacyObligation{Obligation: nil}); err == nil {
		t.Fatal("changed legacy obligation replay did not fail closed")
	}
}

func TestNewLaneCreatesOnceAndEstablishedLaneOnlyResumes(t *testing.T) {
	state, _ := NewState("chat")
	lane := testLane("chat", "codex")
	state, effects := apply(t, state, SelectLane{Identity: lane})
	if len(effects) != 1 {
		t.Fatalf("create effects = %#v", effects)
	}
	if _, ok := effects[0].(CreateLaneEffect); !ok {
		t.Fatalf("first lane effect = %T, want CreateLaneEffect", effects[0])
	}
	thread := provider.ThreadRef{ProviderID: "codex", RootID: "thread-1", HeadID: "thread-1", Lineage: 1}
	state, _ = apply(t, state, LaneOpened{LaneID: lane.ID, Thread: thread, ConnectionGeneration: 1, Context: exactContext(provider.ContextImportNonSampling)})
	state, effects = apply(t, state, HostLost{LaneID: lane.ID, ConnectionGeneration: 1})
	if len(effects) != 0 || state.Lanes[lane.ID].Phase != LaneDetached {
		t.Fatalf("idle host loss = phase %q effects %#v", state.Lanes[lane.ID].Phase, effects)
	}
	state, effects = apply(t, state, SelectLane{Identity: lane})
	if len(effects) != 1 {
		t.Fatalf("resume effects = %#v", effects)
	}
	resume, ok := effects[0].(ResumeLaneEffect)
	if !ok || !resume.Thread.Equal(thread) {
		t.Fatalf("established lane effect = %#v, want exact resume", effects[0])
	}
}

func TestDeleteChatPersistsIdempotentCleanupBeforeNativeDeletion(t *testing.T) {
	state, _ := NewState("delete-chat")
	state, _ = apply(t, state, InitializeChat{Presentation: PresentationState{TabID: "delete-tab"}, OperationID: "create:delete", Digest: "create-delete"})
	state, effects := apply(t, state, DeleteChat{OperationID: "delete-op", Force: true})
	if !state.Deleted || len(effects) != 1 {
		t.Fatalf("delete transition = deleted:%v effects:%#v", state.Deleted, effects)
	}
	cleanup, ok := effects[0].(DeleteChatEffect)
	if !ok || cleanup.ChatID != "delete-chat" || cleanup.TabID != "delete-tab" {
		t.Fatalf("delete effect = %#v", effects[0])
	}
	state, effects = apply(t, state, ClaimEffect{EffectID: deleteChatEffectID("delete-chat")})
	if len(effects) != 1 || state.Outbox[0].Status != OutboxDispatched {
		t.Fatalf("claimed delete = state:%#v effects:%#v", state.Outbox, effects)
	}
	state, _ = apply(t, state, RecoverOutbox{})
	if state.Outbox[0].Status != OutboxPending {
		t.Fatalf("idempotent delete was not retryable after crash: %#v", state.Outbox[0])
	}
	state, _ = apply(t, state, ClaimEffect{EffectID: deleteChatEffectID("delete-chat")})
	state, _ = apply(t, state, ChatDeletionCompleted{ChatID: "delete-chat"})
	if state.Outbox[0].Status != OutboxCompleted {
		t.Fatalf("delete cleanup receipt = %#v", state.Outbox[0])
	}
	if _, _, err := Reduce(state, Submit{OperationID: "resurrect", Text: "no"}); err == nil {
		t.Fatal("tombstoned chat accepted semantic work")
	}
}

func TestAdoptExactLegacyBindingOnlySchedulesResume(t *testing.T) {
	state, _ := NewState("chat")
	lane := testLane("chat", "codex")
	thread := provider.ThreadRef{ProviderID: "codex", RootID: "thread-existing", HeadID: "thread-existing", Lineage: 1}
	state, effects := apply(t, state, AdoptLaneBinding{
		Identity: lane, Thread: thread, Owner: provider.AttachmentOwner{TabID: "tab"},
		CWD: "/workspace", Context: exactContext(provider.ContextImportUnsupported),
	})
	if len(effects) != 0 || state.Lanes[lane.ID].Phase != LaneDetached {
		t.Fatalf("adopted binding created provider work: phase=%q effects=%#v", state.Lanes[lane.ID].Phase, effects)
	}
	state, effects = apply(t, state, SelectLane{Identity: lane, Owner: provider.AttachmentOwner{TabID: "tab"}, CWD: "/workspace"})
	if len(effects) != 1 {
		t.Fatalf("selected adopted binding effects = %#v", effects)
	}
	resume, ok := effects[0].(ResumeLaneEffect)
	if !ok || !resume.Thread.Equal(thread) {
		t.Fatalf("selected adopted binding effect = %#v, want exact resume", effects[0])
	}

	conflict := thread
	conflict.RootID = "replacement"
	conflict.HeadID = "replacement"
	if _, _, err := Reduce(state, AdoptLaneBinding{Identity: lane, Thread: conflict, Context: exactContext(provider.ContextImportUnsupported)}); err == nil {
		t.Fatal("conflicting migration replaced an existing native thread")
	}
}

func TestLegacyNonemptyChatMigratesBeforeProviderSelection(t *testing.T) {
	state, _ := NewState("legacy-chat")
	state, _ = apply(t, state, MigrateLegacyChat{
		Version: 1,
		Digest:  "legacy-digest",
		Presentation: PresentationState{
			TabID: "legacy-tab", Title: "Existing conversation", Draft: "unsent draft",
		},
		Messages: []LegacyMessage{
			{MessageID: "user-1", OperationID: "legacy-user-1", Role: "user", Text: "old question", Status: "done"},
			{MessageID: "assistant-1", OperationID: "legacy-user-1", Role: "assistant", Text: "old answer", Status: "done"},
		},
	})
	if state.LedgerHead() != 2 || !state.Migration.Complete || state.Presentation.Title != "Existing conversation" {
		t.Fatalf("legacy migration did not become authoritative: %#v", state)
	}

	target := testLane("legacy-chat", "codex")
	state, effects := apply(t, state, SelectLane{Identity: target})
	if len(effects) != 1 {
		t.Fatalf("new target create effects = %#v", effects)
	}
	state, effects = apply(t, state, LaneOpened{
		LaneID:               target.ID,
		Thread:               provider.ThreadRef{ProviderID: "codex", RootID: "thread", HeadID: "thread", Lineage: 1},
		ConnectionGeneration: 1,
		Context:              exactContext(provider.ContextImportUnsupported),
	})
	if len(effects) != 0 || state.ActiveLaneID != "" || state.Lanes[target.ID].Phase != LaneBlocked {
		t.Fatalf("migrated nonempty chat was treated as empty: lane=%#v effects=%#v", state.Lanes[target.ID], effects)
	}
	if state.Ledger[0].Legacy == false || state.Ledger[1].Legacy == false {
		t.Fatal("legacy-visible events lost their explicit unknown-attribution marker")
	}

	// Replaying the exact migration is idempotent; a different source digest is
	// an ownership conflict and cannot replace the already-authoritative ledger.
	same, _, err := Reduce(state, MigrateLegacyChat{Version: 1, Digest: "legacy-digest"})
	if err != nil || same.LedgerHead() != state.LedgerHead() {
		t.Fatalf("idempotent migration replay failed: err=%v state=%#v", err, same)
	}
	if _, _, err := Reduce(state, MigrateLegacyChat{Version: 1, Digest: "different"}); err == nil {
		t.Fatal("conflicting legacy migration replaced authoritative actor state")
	}
}

func TestRendererPresentationCannotOverwriteActorRuntimeState(t *testing.T) {
	state, _ := NewState("chat")
	group, cwd, pane := "original-group", "/authoritative", "rail"
	state, _ = apply(t, state, InitializeChat{Presentation: PresentationState{
		TabID: "tab", Title: "Original", Group: &group, CWD: &cwd, Pane: &pane,
		ProviderID: "codex", CurrentModelID: "gpt-authoritative", CurrentModeID: "ask",
		WorkspaceRevision: 4, AgentQueueRevision: 7, RuntimeControlRevision: 9,
		ContextUsageByProvider: json.RawMessage(`{"codex":{"used":3}}`),
		LegacyUsage:            json.RawMessage(`{"used":3}`),
		PlanLatest:             []provider.PlanEntry{{ID: "p1", Text: "authoritative", Status: "in_progress"}},
		PlanLatestMessageID:    "assistant-authoritative",
	}, OperationID: "create:provider-switch", Digest: "create-provider-switch"})
	attackerGroup, attackerCWD, attackerPane := "renamed-group", "/forged", "browser"
	forged := PresentationState{
		TabID: "tab", Title: "Renamed", TitleLocked: true, Group: &attackerGroup,
		CWD: &attackerCWD, Draft: "draft", Unread: true, Settled: "settled", Pane: &attackerPane,
		ProviderID: "claude", CurrentModelID: "forged-model", CurrentModeID: "forged-mode",
		WorkspaceRevision: 4, AgentQueueRevision: 7, RuntimeControlRevision: 9,
		ModelControls:          json.RawMessage(`{"ui":"allowed"}`),
		ContextUsageByProvider: json.RawMessage(`{"claude":{"used":999}}`),
		LegacyUsage:            json.RawMessage(`{"used":999}`),
		PlanLatest:             []provider.PlanEntry{{ID: "forged", Text: "forged", Status: "done"}},
		PlanLatestMessageID:    "forged-assistant",
	}
	state, _ = apply(t, state, UpdatePresentation{Presentation: forged})
	got := state.Presentation
	if got.TabID != "tab" || got.Title != "Renamed" || got.Draft != "draft" || got.Group == nil || *got.Group != attackerGroup || got.Pane == nil || *got.Pane != attackerPane {
		t.Fatalf("renderer-owned presentation did not update: %#v", got)
	}
	if got.CWD == nil || *got.CWD != cwd || got.ProviderID != "codex" || got.CurrentModelID != "gpt-authoritative" || got.CurrentModeID != "ask" ||
		string(got.ContextUsageByProvider) != `{"codex":{"used":3}}` || string(got.LegacyUsage) != `{"used":3}` ||
		len(got.PlanLatest) != 1 || got.PlanLatest[0].ID != "p1" || got.PlanLatestMessageID != "assistant-authoritative" {
		t.Fatalf("renderer snapshot overwrote actor runtime state: %#v", got)
	}
}

func TestLegacyMigrationReconcilesOnlyExactPreCutoverActorRows(t *testing.T) {
	state, _ := NewState("chat")
	lane := testLane("chat", "codex")
	state.Lanes[lane.ID] = LaneState{
		Identity: lane, Phase: LaneReady, Coverage: map[uint64]CoverageRecord{
			1: {Sequence: 1, EventID: "event:op:user", Status: CoverageNativeSeen, DeliveryID: "op"},
		},
		CoveredThrough: 1, Context: exactContext(provider.ContextImportUnsupported),
	}
	state.Operations["op"] = struct{}{}
	state.Ledger = []LedgerEvent{{
		EventID: "event:op:user", MessageID: "user", Sequence: 1, Role: "user", Text: "hello", Status: "done",
		LaneID: lane.ID, ProviderID: "codex", OperationID: "op",
	}}
	next, _, err := Reduce(state, MigrateLegacyChat{
		Version: 1, Digest: "digest", Presentation: PresentationState{Title: "Existing"},
		Messages: []LegacyMessage{{MessageID: "user", OperationID: "op", Role: "user", Text: "hello", Status: "done", At: "2026-08-11T00:00:00Z"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !next.Migration.Complete || next.Ledger[0].Legacy || next.Ledger[0].At != "2026-08-11T00:00:00Z" {
		t.Fatalf("exact actor/mirror reconciliation lost ownership: %#v", next)
	}
	if _, _, err := Reduce(state, MigrateLegacyChat{
		Version: 1, Digest: "different", Presentation: PresentationState{Title: "Existing"},
		Messages: []LegacyMessage{{MessageID: "other", OperationID: "op", Role: "user", Text: "hello", Status: "done"}},
	}); err == nil {
		t.Fatal("identity-conflicting mirror was merged into pre-cutover actor state")
	}
}

func TestRendererQueueReplacementUsesActorRevisionAndCannotTouchProviderOutbox(t *testing.T) {
	state, _ := NewState("chat")
	state, _ = apply(t, state, MigrateLegacyChat{
		Version: 1, Digest: "digest", Presentation: PresentationState{AgentQueueRevision: 4},
	})
	state, _ = apply(t, state, ReplaceStagedQueue{OperationID: "queue-op-1", Digest: "queue-digest-1", ExpectedRevision: 4, Entries: []StagedQueueEntry{{
		ID: "queued", Text: "later", Delivery: "queue", TargetProviderID: "codex",
	}}})
	if state.Presentation.AgentQueueRevision != 5 || len(state.StagedQueue) != 1 || len(state.Outbox) != 0 {
		t.Fatalf("actor queue replacement mutated provider runtime: %#v", state)
	}
	if _, _, err := Reduce(state, ReplaceStagedQueue{OperationID: "queue-op-2", Digest: "queue-digest-2", ExpectedRevision: 4}); err == nil {
		t.Fatal("stale renderer queue revision overwrote actor queue")
	}
}

func TestResumeCannotReplaceNativeThread(t *testing.T) {
	state, _ := NewState("chat")
	lane := testLane("chat", "codex")
	state, _ = apply(t, state, SelectLane{Identity: lane})
	state, _ = apply(t, state, LaneOpened{
		LaneID: lane.ID, Thread: provider.ThreadRef{ProviderID: "codex", RootID: "thread-1", HeadID: "thread-1", Lineage: 1},
		ConnectionGeneration: 1, Context: exactContext(provider.ContextImportNonSampling),
	})
	laneState := state.Lanes[lane.ID]
	laneState.Phase = LaneResuming
	state.Lanes[lane.ID] = laneState
	_, _, err := Reduce(state, LaneOpened{
		LaneID: lane.ID, Thread: provider.ThreadRef{ProviderID: "codex", RootID: "thread-2", HeadID: "thread-2", Lineage: 1},
		ConnectionGeneration: 2, Context: exactContext(provider.ContextImportNonSampling),
	})
	if err == nil {
		t.Fatal("replacement native thread was accepted during resume")
	}
}

func TestProviderSwitchImportsOnlyUnseenLedgerAndSwitchesBack(t *testing.T) {
	state, _ := NewState("chat")
	codex := testLane("chat", "codex")
	claude := testLane("chat", "claude")
	state, _ = apply(t, state, SelectLane{Identity: codex})
	state, _ = apply(t, state, LaneOpened{
		LaneID: codex.ID, Thread: provider.ThreadRef{ProviderID: "codex", RootID: "codex-thread", HeadID: "codex-thread", Lineage: 1},
		ConnectionGeneration: 1, Context: exactContext(provider.ContextImportNonSampling),
	})
	state, effects := apply(t, state, Submit{OperationID: "op-codex", Text: "first"})
	if _, ok := effects[0].(StartTurnEffect); !ok {
		t.Fatalf("submit effect = %T", effects[0])
	}
	state, _ = apply(t, state, TurnAdmitted{OperationID: "op-codex", Accepted: true, Turn: provider.TurnRef{OperationID: "op-codex", NativeID: "turn-c"}})
	state, _ = apply(t, state, InputConsumed{OperationID: "op-codex"})
	state, _ = apply(t, state, TurnCompleted{OperationID: "op-codex", Assistant: "answer"})

	state, effects = apply(t, state, SelectLane{Identity: claude})
	if _, ok := effects[0].(CreateLaneEffect); !ok {
		t.Fatalf("new provider effect = %T", effects[0])
	}
	state, effects = apply(t, state, LaneOpened{
		LaneID: claude.ID, Thread: provider.ThreadRef{ProviderID: "claude", RootID: "claude-thread", HeadID: "claude-thread", Lineage: 1},
		ConnectionGeneration: 1, Context: exactContext(provider.ContextImportNonSampling),
	})
	if len(effects) != 1 {
		t.Fatalf("context import effects = %#v", effects)
	}
	imp, ok := effects[0].(ImportContextEffect)
	if !ok || imp.From != 0 || imp.To != 2 {
		t.Fatalf("context import effect = %#v", effects[0])
	}
	if len(imp.Batch.Messages) != 2 || imp.Batch.Messages[0].LedgerSequence != 1 || imp.Batch.Messages[1].LedgerSequence != 2 {
		t.Fatalf("context import projection = %#v ledger=%#v", imp.Batch, state.Ledger)
	}
	state, _ = apply(t, state, ContextImported{
		LaneID: claude.ID, OperationID: imp.OperationID, From: imp.From, To: imp.To, Digest: imp.Batch.Digest, Found: true, Confirmed: true,
	})
	if state.ActiveLaneID != claude.ID {
		t.Fatalf("active lane = %q, want Claude", state.ActiveLaneID)
	}
	state, effects = apply(t, state, SelectLane{Identity: codex})
	if len(effects) != 0 || state.ActiveLaneID != codex.ID {
		t.Fatalf("switch back should reuse covered Codex lane: active=%q effects=%#v", state.ActiveLaneID, effects)
	}
}

func TestContextProjectionPreservesAssistantResultBoundary(t *testing.T) {
	state, _ := NewState("chat")
	state, _ = apply(t, state, MigrateLegacyChat{
		Version: 1, Digest: "legacy-digest", Presentation: PresentationState{TabID: "tab"},
		Messages: []LegacyMessage{
			{MessageID: "user", OperationID: "operation", Role: "user", Text: "question", Status: "done"},
			{MessageID: "assistant", OperationID: "operation", Role: "assistant", Text: "commentary", Result: "final answer", Status: "done", TerminalState: "done"},
		},
	})
	batch, from, to, err := buildContextBatch(state, 0, exactContext(provider.ContextImportNonSampling))
	if err != nil {
		t.Fatal(err)
	}
	if from != 0 || to != 2 || len(batch.Messages) != 2 {
		t.Fatalf("context range = %d..%d messages=%#v", from, to, batch.Messages)
	}
	assistant := batch.Messages[1]
	if assistant.Text != "commentary" || assistant.Result != "final answer" || assistant.TerminalStatus != "done" {
		t.Fatalf("assistant result boundary was lost: %#v", assistant)
	}
}

func TestSwitchDuringTurnDoesNotRetargetForegroundOrSteerOwner(t *testing.T) {
	state, _ := NewState("chat")
	codex := testLane("chat", "codex")
	claude := testLane("chat", "claude")
	state, _ = apply(t, state, SelectLane{Identity: codex})
	state, _ = apply(t, state, LaneOpened{
		LaneID: codex.ID, Thread: provider.ThreadRef{ProviderID: "codex", RootID: "codex-thread", HeadID: "codex-thread", Lineage: 1},
		ConnectionGeneration: 1, Context: exactContext(provider.ContextImportNonSampling),
	})
	state, _ = apply(t, state, Submit{OperationID: "running", Text: "work"})
	state, effects := apply(t, state, SelectLane{Identity: claude})
	if len(effects) != 0 {
		t.Fatalf("switch during turn emitted effects: %#v", effects)
	}
	if state.Foreground == nil || state.Foreground.LaneID != codex.ID || state.DesiredLaneID != claude.ID {
		t.Fatalf("foreground/desired ownership changed incorrectly: %#v", state)
	}
	state, _ = apply(t, state, Submit{OperationID: "queued-for-claude", Text: "next"})
	if len(state.Queue) != 1 || state.Queue[0].LaneID != claude.ID {
		t.Fatalf("queued target was not snapshotted: %#v", state.Queue)
	}
}

func TestSteerCancelAndPermissionKeepOriginLaneAcrossProviderSelection(t *testing.T) {
	state, _ := NewState("chat")
	alpha := testLane("chat", "alpha")
	beta := testLane("chat", "beta")
	state, _ = apply(t, state, SelectLane{Identity: alpha})
	state, _ = apply(t, state, LaneOpened{
		LaneID: alpha.ID, Thread: provider.ThreadRef{ProviderID: "alpha", RootID: "thread-a", HeadID: "thread-a", Lineage: 1},
		ConnectionGeneration: 1, Context: exactContext(provider.ContextImportNonSampling),
	})
	state, _ = apply(t, state, Submit{OperationID: "turn", Text: "work"})
	state, _ = apply(t, state, TurnAdmitted{
		OperationID: "turn", Accepted: true, Turn: provider.TurnRef{OperationID: "turn", NativeID: "native-turn"},
	})
	state, _ = apply(t, state, SelectLane{Identity: beta})

	state, effects := apply(t, state, Steer{OperationID: "steer", Text: "redirect"})
	steer, ok := effects[0].(SteerTurnEffect)
	if !ok || steer.LaneID != alpha.ID || steer.Turn.NativeID != "native-turn" {
		t.Fatalf("steer was retargeted by provider selection: %#v", effects)
	}
	state, _ = apply(t, state, SteerFailed{
		OperationID: "steer", Kind: provider.ErrorUnsupportedCapability, Unsupported: true,
	})
	if len(state.Queue) != 1 || state.Queue[0].LaneID != alpha.ID || state.Queue[0].OperationID != "steer" {
		t.Fatalf("unsupported steer fallback lost origin lane: %#v", state.Queue)
	}

	permission := provider.Event{
		Kind: provider.EventPermissionRequested,
		Identity: provider.EventIdentity{
			ChatID: "chat", LaneID: alpha.ID, OperationID: "turn", TurnID: "native-turn", Sequence: 1,
		},
		Permission: &provider.PermissionEvent{RequestID: "permission", Status: "pending", Options: []string{"allow", "deny"}},
	}
	state, _ = apply(t, state, ProviderEventReceived{ConnectionGeneration: 1, Event: permission})
	state, effects = apply(t, state, ResolvePermission{OperationID: "permission-decision", RequestID: "permission", OptionID: "allow"})
	decision, ok := effects[0].(ResolvePermissionEffect)
	if !ok || decision.LaneID != alpha.ID {
		t.Fatalf("permission decision was retargeted by provider selection: %#v", effects)
	}
	state, _ = apply(t, state, PermissionDecided{
		OperationID: "permission-decision", RequestID: "permission", OptionID: "allow", Accepted: true,
	})
	if state.Permissions["permission"].Event.Status != "resolved" {
		t.Fatalf("permission did not settle: %#v", state.Permissions["permission"])
	}

	state, effects = apply(t, state, CancelTurn{OperationID: "cancel"})
	cancel, ok := effects[0].(CancelTurnEffect)
	if !ok || cancel.LaneID != alpha.ID || cancel.Turn.NativeID != "native-turn" {
		t.Fatalf("cancel was retargeted by provider selection: %#v", effects)
	}
	state, _ = apply(t, state, TurnTerminated{OperationID: "turn", Status: "cancelled"})
	state, _ = apply(t, state, CancelAcknowledged{OperationID: "cancel"})
	if state.PendingCancel != nil || !outboxHas(&state, cancelEffectID("cancel"), OutboxCompleted) {
		t.Fatalf("terminal cancel receipt did not settle idempotently: %#v", state)
	}
	if state.Foreground == nil || state.Foreground.OperationID != "steer" || state.Foreground.LaneID != alpha.ID {
		t.Fatalf("queued unsupported steer did not retain priority/origin after terminal: %#v", state.Foreground)
	}
}

func TestConsumedSteerGetsOneSemanticOwner(t *testing.T) {
	state, _ := NewState("chat")
	lane := testLane("chat", "alpha")
	state, _ = apply(t, state, SelectLane{Identity: lane})
	state, _ = apply(t, state, LaneOpened{
		LaneID: lane.ID, Thread: provider.ThreadRef{ProviderID: "alpha", RootID: "thread", HeadID: "thread", Lineage: 1},
		ConnectionGeneration: 1, Context: exactContext(provider.ContextImportNonSampling),
	})
	state, _ = apply(t, state, Submit{OperationID: "turn", Text: "work"})
	state, _ = apply(t, state, TurnAdmitted{
		OperationID: "turn", Accepted: true, Turn: provider.TurnRef{OperationID: "turn", NativeID: "native-turn"},
	})
	state, _ = apply(t, state, Steer{OperationID: "steer", Text: "one direction"})
	state, _ = apply(t, state, SteerAdmitted{OperationID: "steer", Accepted: true})
	state, _ = apply(t, state, InputConsumed{OperationID: "steer"})
	if state.PendingSteer != nil || len(state.Ledger) != 3 {
		t.Fatalf("consumed steer did not become chronological actor history: %#v", state)
	}
	if state.Ledger[0].OperationID != "turn" || state.Ledger[0].Role != "user" ||
		state.Ledger[1].Role != "assistant" || state.Ledger[1].TurnTerminal == nil || *state.Ledger[1].TurnTerminal ||
		state.Ledger[2].OperationID != "steer" || state.Ledger[2].SteerState != "applied" {
		t.Fatalf("steer chronology/ownership = %#v", state.Ledger)
	}
	state, _ = apply(t, state, InputConsumed{OperationID: "turn"})
	state, _ = apply(t, state, TurnCompleted{OperationID: "turn", Assistant: "done"})
	seen := 0
	for _, event := range state.Ledger {
		if event.OperationID == "steer" {
			seen++
		}
	}
	if seen != 1 || !outboxHas(&state, steerEffectID("steer"), OutboxCompleted) {
		t.Fatalf("terminal boundary duplicated or stranded consumed steer: seen=%d state=%#v", seen, state)
	}
	if len(state.Ledger) != 4 || state.Ledger[3].Role != "assistant" || state.Ledger[3].TurnTerminal == nil || !*state.Ledger[3].TurnTerminal || state.Ledger[3].TurnRootID != state.Ledger[1].TurnRootID {
		t.Fatalf("steered turn continuation was not terminal under one root: %#v", state.Ledger)
	}
}

func TestRichProviderTimelineSurvivesActorRestartLosslessly(t *testing.T) {
	state, _ := NewState("chat")
	lane := testLane("chat", "codex")
	state, _ = apply(t, state, SelectLane{Identity: lane})
	state, _ = apply(t, state, LaneOpened{
		LaneID: lane.ID, Thread: provider.ThreadRef{ProviderID: "codex", RootID: "thread", HeadID: "thread", Lineage: 1},
		ConnectionGeneration: 1, Context: exactContext(provider.ContextImportNonSampling),
	})
	state, _ = apply(t, state, Submit{
		OperationID: "turn", Text: "question", ModelID: "model",
		Presentation: provider.TurnPresentation{
			UserMessageID: "user", AssistantMessageID: "assistant", StartedAt: "2026-08-11T12:00:00Z",
		},
	})
	state, _ = apply(t, state, TurnAdmitted{
		OperationID: "turn", Accepted: true, Turn: provider.TurnRef{OperationID: "turn", NativeID: "native-turn"},
	})
	sequence := uint64(0)
	emit := func(event provider.Event) {
		t.Helper()
		sequence++
		event.Identity = provider.EventIdentity{
			ChatID: "chat", LaneID: lane.ID, OperationID: "turn", TurnID: "native-turn",
			Sequence: sequence, ObservedAtUnixMS: 1_700_000_000_000 + int64(sequence),
		}
		state, _ = apply(t, state, ProviderEventReceived{ConnectionGeneration: 1, Event: event})
	}
	emit(provider.Event{Kind: provider.EventInputConsumed, Input: &provider.InputEvent{OperationID: "turn"}})
	emit(provider.Event{Kind: provider.EventAssistantChunk, Assistant: &provider.AssistantEvent{Phase: provider.AssistantPhaseContent, Text: "A😀"}})
	emit(provider.Event{Kind: provider.EventThinkingUpdate, Thinking: &provider.ThinkingEvent{Text: "reasoning window"}})
	emit(provider.Event{Kind: provider.EventToolUpdate, Tool: &provider.ToolEvent{
		ToolCallID: "tool-1", ToolKind: "terminal", Title: "Run tests", Status: "running",
		Command: "go test", TerminalID: "terminal-1", Input: "input", Location: "/workspace",
		SubagentID: "sub-1", SubagentLabel: "review", SubagentProvider: "codex", SubagentModel: "gpt", SubagentHeader: true,
	}})
	emit(provider.Event{Kind: provider.EventToolUpdate, Tool: &provider.ToolEvent{
		ToolCallID: "tool-1", Status: "completed", Output: "ok",
		Attachments: []provider.Attachment{{ID: "tool-image", MIMEType: "image/png", Ref: "workass-session-image:tool-image"}},
	}})
	emit(provider.Event{Kind: provider.EventPlanUpdate, Plan: &provider.PlanEvent{Entries: []provider.PlanEntry{{ID: "one", Text: "Verify", Status: "completed"}}}})
	emit(provider.Event{Kind: provider.EventPermissionRequested, Permission: &provider.PermissionEvent{
		RequestID: "permission-1", Title: "Choose", Kind: "question", Status: "pending", Options: []string{"yes"},
		OptionDetails: []provider.PermissionOption{{ID: "yes", Name: "Yes", Kind: "allow"}},
		Question: &provider.PermissionQuestion{Question: "Proceed?", Header: "Decision", MultiSelect: false,
			Options: []provider.PermissionQuestionOption{{Label: "Yes", Description: "Continue"}}},
	}})
	emit(provider.Event{Kind: provider.EventPermissionResolved, Permission: &provider.PermissionEvent{
		RequestID: "permission-1", Title: "Choose", Kind: "question", Status: "resolved", ResolvedOptionID: "yes",
	}})
	emit(provider.Event{Kind: provider.EventAssistantMedia, Media: &provider.AssistantMediaEvent{Attachments: []provider.Attachment{{
		ID: "assistant-image", MIMEType: "image/png", Ref: "workass-session-image:assistant-image",
	}}}})
	emit(provider.Event{Kind: provider.EventAssistantChunk, Assistant: &provider.AssistantEvent{Phase: provider.AssistantPhaseFinal, Text: "final answer"}})
	emit(provider.Event{Kind: provider.EventCompactionCheckpoint, Compaction: &provider.CompactionEvent{CheckpointID: "checkpoint", Coverage: 1, Digest: "digest"}})
	emit(provider.Event{Kind: provider.EventTurnTerminal, Terminal: &provider.TerminalEvent{Status: "completed"}})

	if len(state.Ledger) != 2 {
		t.Fatalf("rich turn ledger = %#v", state.Ledger)
	}
	assistant := state.Ledger[1]
	if assistant.Text != "A😀" || assistant.Result != "final answer" || len(assistant.Attachments) != 1 || len(assistant.Timeline) != 4 || assistant.Permission == nil {
		t.Fatalf("rich assistant state was collapsed: %#v", assistant)
	}
	if assistant.Timeline[0].At != 3 || assistant.Timeline[0].Thinking == nil || assistant.Timeline[0].Thinking.Text != "reasoning window" {
		t.Fatalf("thinking UTF-16 position = %#v", assistant.Timeline[0])
	}
	tool := assistant.Timeline[1].Tool
	if tool == nil || tool.Title != "Run tests" || tool.Output != "ok" || tool.StartedAtUnixMS == 0 || tool.EndedAtUnixMS == 0 || tool.EndedAtUnixMS <= tool.StartedAtUnixMS || len(tool.Attachments) != 1 || !tool.SubagentHeader {
		t.Fatalf("tool lifecycle was not merged losslessly: %#v", tool)
	}

	store := FileStore{Path: filepath.Join(t.TempDir(), "actor.json")}
	if err := store.Save(state); err != nil {
		t.Fatal(err)
	}
	restored, ok, err := store.Load("chat")
	if err != nil || !ok {
		t.Fatalf("reload rich actor state: ok=%v err=%v", ok, err)
	}
	restoredAssistant := restored.Ledger[1]
	if restoredAssistant.Result != assistant.Result || len(restoredAssistant.Timeline) != len(assistant.Timeline) || restoredAssistant.Permission == nil || restoredAssistant.Timeline[1].Tool == nil || restoredAssistant.Timeline[1].Tool.Output != "ok" {
		t.Fatalf("restart lost rich actor state: %#v", restoredAssistant)
	}
}

func TestTerminalEventPreservesCompleteSemanticsAndSettlesOpenActivity(t *testing.T) {
	state, _ := NewState("chat")
	lane := testLane("chat", "codex")
	state, _ = apply(t, state, SelectLane{Identity: lane})
	state, _ = apply(t, state, LaneOpened{
		LaneID: lane.ID, Thread: provider.ThreadRef{ProviderID: "codex", RootID: "thread", HeadID: "thread", Lineage: 1},
		ConnectionGeneration: 1, Context: exactContext(provider.ContextImportNonSampling),
	})
	state, _ = apply(t, state, Submit{
		OperationID: "turn", Text: "question", ModelID: "model",
		Presentation: provider.TurnPresentation{UserMessageID: "user", AssistantMessageID: "assistant", StartedAt: "2026-08-11T12:00:00Z"},
	})
	state, _ = apply(t, state, TurnAdmitted{OperationID: "turn", Accepted: true, Turn: provider.TurnRef{OperationID: "turn", NativeID: "native-turn"}})
	sequence := uint64(0)
	emit := func(event provider.Event) {
		t.Helper()
		sequence++
		event.Identity = provider.EventIdentity{
			ChatID: "chat", LaneID: lane.ID, OperationID: "turn", TurnID: "native-turn",
			Sequence: sequence, ObservedAtUnixMS: 1_786_446_010_000 + int64(sequence),
		}
		state, _ = apply(t, state, ProviderEventReceived{ConnectionGeneration: 1, Event: event})
	}
	emit(provider.Event{Kind: provider.EventInputConsumed, Input: &provider.InputEvent{OperationID: "turn"}})
	emit(provider.Event{Kind: provider.EventAssistantChunk, Assistant: &provider.AssistantEvent{Phase: provider.AssistantPhaseContent, Text: "partial"}})
	emit(provider.Event{Kind: provider.EventToolUpdate, Tool: &provider.ToolEvent{ToolCallID: "tool", Status: "running", Title: "Run"}})
	emit(provider.Event{Kind: provider.EventPermissionRequested, Permission: &provider.PermissionEvent{RequestID: "permission", Status: "pending"}})
	state, _ = apply(t, state, Steer{
		OperationID: "steer", Text: "late direction",
		Presentation: provider.TurnPresentation{UserMessageID: "steer-user", StartedAt: "2026-08-11T12:00:05Z"},
	})
	state, _ = apply(t, state, SteerAdmitted{OperationID: "steer", Accepted: true, AwaitConsumption: true})
	code := 1
	emit(provider.Event{Kind: provider.EventTurnTerminal, Terminal: &provider.TerminalEvent{
		Status: "failed", StopReason: "engine-crash", Result: "canonical whole turn", Error: "boom",
		FinishedAt: "2026-08-11T12:00:10Z", Code: &code, Interrupted: true, CrashInterrupted: true,
		DispositionState: "needs_input", DispositionSource: "native",
		Attachments: []provider.Attachment{{ID: "terminal-image", MIMEType: "image/png", Ref: "workass-session-image:terminal-image"}},
	}})
	if state.Foreground != nil || state.PendingSteer != nil {
		t.Fatalf("terminal boundary left transient owners: foreground=%#v steer=%#v", state.Foreground, state.PendingSteer)
	}
	if len(state.Ledger) != 3 {
		t.Fatalf("terminal ledger = %#v", state.Ledger)
	}
	assistant := state.Ledger[1]
	if assistant.Text != "canonical whole turn" || assistant.Status != "failed" || assistant.At != "2026-08-11T12:00:10Z" || !assistant.Interrupted || assistant.RetryPrompt != "question" || len(assistant.Attachments) != 1 {
		t.Fatalf("terminal assistant projection state = %#v", assistant)
	}
	if assistant.Terminal == nil || assistant.Terminal.Error != "boom" || !assistant.Terminal.CrashInterrupted || assistant.Terminal.DispositionState != "needs_input" || assistant.Terminal.Code == nil || *assistant.Terminal.Code != 1 {
		t.Fatalf("terminal receipt was collapsed: %#v", assistant.Terminal)
	}
	if assistant.Permission != nil || len(state.Permissions) != 0 {
		t.Fatalf("unresolved permission survived terminal: message=%#v state=%#v", assistant.Permission, state.Permissions)
	}
	if len(assistant.Timeline) != 1 || assistant.Timeline[0].Tool == nil || assistant.Timeline[0].Tool.Status != "failed" || assistant.Timeline[0].Tool.EndedAtUnixMS == 0 || state.Tools["tool"].Event.Status != "failed" {
		t.Fatalf("running tool did not settle at terminal: %#v tools=%#v", assistant.Timeline, state.Tools)
	}
	steer := state.Ledger[2]
	if steer.MessageID != "steer-user" || steer.SteerState != "accepted" || steer.Status != "done" || steer.TerminalState != "unconsumed" {
		t.Fatalf("unconsumed steer was dropped or rewritten: %#v", steer)
	}
	if !outboxHas(&state, steerEffectID("steer"), OutboxAmbiguous) {
		t.Fatalf("unconsumed steer outbox is not ambiguity-fenced: %#v", state.Outbox)
	}
}

func TestTerminalConsumedSteerReceiptCommitsChronologyExactlyOnce(t *testing.T) {
	state, _ := NewState("chat")
	lane := testLane("chat", "codex")
	state, _ = apply(t, state, SelectLane{Identity: lane})
	state, _ = apply(t, state, LaneOpened{
		LaneID: lane.ID, Thread: provider.ThreadRef{ProviderID: "codex", RootID: "thread", HeadID: "thread", Lineage: 1},
		ConnectionGeneration: 1, Context: exactContext(provider.ContextImportNonSampling),
	})
	state, _ = apply(t, state, Submit{OperationID: "turn", Text: "question", Presentation: provider.TurnPresentation{UserMessageID: "user", AssistantMessageID: "assistant"}})
	state, _ = apply(t, state, TurnAdmitted{OperationID: "turn", Accepted: true, Turn: provider.TurnRef{OperationID: "turn", NativeID: "native-turn"}})
	state, _ = apply(t, state, Steer{OperationID: "steer", Text: "direction", Presentation: provider.TurnPresentation{UserMessageID: "steer-user", AssistantMessageID: "continuation"}})
	state, _ = apply(t, state, SteerAdmitted{OperationID: "steer", Accepted: true, AwaitConsumption: true})
	event := provider.Event{
		Kind:     provider.EventTurnTerminal,
		Identity: provider.EventIdentity{ChatID: "chat", LaneID: lane.ID, OperationID: "turn", TurnID: "native-turn", Sequence: 1},
		Terminal: &provider.TerminalEvent{Status: "completed", ConsumedSteerIDs: []provider.OperationID{"steer"}},
	}
	state, _ = apply(t, state, ProviderEventReceived{ConnectionGeneration: 1, Event: event})
	if len(state.Ledger) != 4 || state.Ledger[2].OperationID != "steer" || state.Ledger[2].SteerState != "applied" || state.Ledger[3].MessageID != "continuation" {
		t.Fatalf("terminal consumption receipt did not create exact chronology: %#v", state.Ledger)
	}
	seen := 0
	for _, row := range state.Ledger {
		if row.OperationID == "steer" {
			seen++
		}
	}
	if seen != 1 || !outboxHas(&state, steerEffectID("steer"), OutboxCompleted) {
		t.Fatalf("terminal steer receipt duplicated or stranded input: seen=%d outbox=%#v", seen, state.Outbox)
	}
}

func TestProviderWithoutContextImportCannotJoinNonemptyChat(t *testing.T) {
	state, _ := NewState("chat")
	codex := testLane("chat", "codex")
	limited := testLane("chat", "limited")
	state, _ = apply(t, state, SelectLane{Identity: codex})
	state, _ = apply(t, state, LaneOpened{
		LaneID: codex.ID, Thread: provider.ThreadRef{ProviderID: "codex", RootID: "thread", HeadID: "thread", Lineage: 1},
		ConnectionGeneration: 1, Context: exactContext(provider.ContextImportNonSampling),
	})
	state, _ = apply(t, state, Submit{OperationID: "op", Text: "hello"})
	state, _ = apply(t, state, TurnCompleted{OperationID: "op", Assistant: "world"})
	state, _ = apply(t, state, SelectLane{Identity: limited})
	state, effects := apply(t, state, LaneOpened{
		LaneID: limited.ID, Thread: provider.ThreadRef{ProviderID: "limited", RootID: "thread", HeadID: "thread", Lineage: 1},
		ConnectionGeneration: 1, Context: exactContext(provider.ContextImportUnsupported),
	})
	if len(effects) != 0 || state.Lanes[limited.ID].Phase != LaneBlocked || state.ActiveLaneID != codex.ID {
		t.Fatalf("unsupported import did not fail closed: lane=%#v active=%q effects=%#v", state.Lanes[limited.ID], state.ActiveLaneID, effects)
	}
}

func TestDuplicateOperationCannotCreateTwoVisibleOwners(t *testing.T) {
	state, _ := NewState("chat")
	lane := testLane("chat", "codex")
	state, _ = apply(t, state, SelectLane{Identity: lane})
	state, _ = apply(t, state, LaneOpened{
		LaneID: lane.ID, Thread: provider.ThreadRef{ProviderID: "codex", RootID: "thread", HeadID: "thread", Lineage: 1},
		ConnectionGeneration: 1, Context: exactContext(provider.ContextImportNonSampling),
	})
	state, _ = apply(t, state, Submit{OperationID: "same", Text: "once"})
	if _, _, err := Reduce(state, Submit{OperationID: "same", Text: "twice"}); err == nil {
		t.Fatal("duplicate operation id was accepted")
	}
}

func TestStaleHostLossCannotDetachNewConnection(t *testing.T) {
	state, _ := NewState("chat")
	lane := testLane("chat", "codex")
	state, _ = apply(t, state, SelectLane{Identity: lane})
	state, _ = apply(t, state, LaneOpened{
		LaneID: lane.ID, Thread: provider.ThreadRef{ProviderID: "codex", RootID: "thread", HeadID: "thread", Lineage: 1},
		ConnectionGeneration: 5, Context: exactContext(provider.ContextImportNonSampling),
	})
	state, effects := apply(t, state, HostLost{LaneID: lane.ID, ConnectionGeneration: 4})
	if len(effects) != 0 || state.Lanes[lane.ID].Phase != LaneReady {
		t.Fatalf("stale host loss changed current connection: %#v %#v", state.Lanes[lane.ID], effects)
	}
}

func TestProviderEventSequenceGapFailsClosed(t *testing.T) {
	state, _ := NewState("chat")
	lane := testLane("chat", "codex")
	state, _ = apply(t, state, SelectLane{Identity: lane})
	state, _ = apply(t, state, LaneOpened{
		LaneID: lane.ID, Thread: provider.ThreadRef{ProviderID: "codex", RootID: "thread", HeadID: "thread", Lineage: 1},
		ConnectionGeneration: 1, Context: exactContext(provider.ContextImportNonSampling),
	})
	first := provider.Event{
		Kind: provider.EventUsageUpdated,
		Identity: provider.EventIdentity{
			ChatID: "chat", LaneID: lane.ID, Sequence: 1,
		},
		Usage: &provider.UsageEvent{Used: 1, Size: 10},
	}
	state, _ = apply(t, state, ProviderEventReceived{ConnectionGeneration: 1, Event: first})

	gapped := first
	gapped.Identity.Sequence = 3
	gapped.Usage = &provider.UsageEvent{Used: 3, Size: 10}
	next, _, err := Reduce(state, ProviderEventReceived{ConnectionGeneration: 1, Event: gapped})
	if err == nil {
		t.Fatal("provider event sequence gap was silently accepted")
	}
	if next.Lanes[lane.ID].LastEventSequence != 1 || next.Usage[lane.ID].Used != 1 {
		t.Fatalf("rejected sequence gap mutated actor state: lane=%#v usage=%#v", next.Lanes[lane.ID], next.Usage[lane.ID])
	}
}

func TestVerifiedLineageEventMayAdvanceHeadWithoutReplacingLane(t *testing.T) {
	state, _ := NewState("chat")
	lane := testLane("chat", "claude")
	state, _ = apply(t, state, SelectLane{Identity: lane})
	current := provider.ThreadRef{ProviderID: "claude", RootID: "root", HeadID: "head-1", Lineage: 1}
	state, _ = apply(t, state, LaneOpened{
		LaneID: lane.ID, Thread: current, ConnectionGeneration: 1,
		Context: exactContext(provider.ContextImportNonSampling),
	})
	next := provider.ThreadRef{ProviderID: "claude", RootID: "root", HeadID: "head-2", Lineage: 2, Proof: "native-adapter-event"}
	state, _ = apply(t, state, LineageAdvanced{
		LaneID: lane.ID, ConnectionGeneration: 1, From: current, To: next,
	})
	if !state.Lanes[lane.ID].Thread.Equal(next) {
		t.Fatalf("lane head did not advance: %#v", state.Lanes[lane.ID].Thread)
	}
	bad := provider.ThreadRef{ProviderID: "claude", RootID: "other", HeadID: "head-x", Lineage: 3, Proof: "native-adapter-event"}
	if _, _, err := Reduce(state, LineageAdvanced{LaneID: lane.ID, ConnectionGeneration: 1, From: next, To: bad}); err == nil {
		t.Fatal("cross-lineage native id replacement was accepted")
	}
}

func TestContextImportIsBoundedAndCoverageCannotSkipAGap(t *testing.T) {
	state, _ := NewState("chat")
	first := testLane("chat", "first")
	second := testLane("chat", "second")
	state, _ = apply(t, state, SelectLane{Identity: first})
	state, _ = apply(t, state, LaneOpened{
		LaneID:               first.ID,
		Thread:               provider.ThreadRef{ProviderID: "first", RootID: "first-thread", HeadID: "first-thread", Lineage: 1},
		ConnectionGeneration: 1, Context: exactContext(provider.ContextImportNonSampling),
	})
	state, _ = apply(t, state, Submit{OperationID: "source", Text: "question"})
	state, _ = apply(t, state, TurnCompleted{OperationID: "source", Assistant: "answer"})

	state, _ = apply(t, state, SelectLane{Identity: second})
	limited := exactContext(provider.ContextImportNonSampling)
	limited.MaxImportEvents = 1
	state, effects := apply(t, state, LaneOpened{
		LaneID:               second.ID,
		Thread:               provider.ThreadRef{ProviderID: "second", RootID: "second-thread", HeadID: "second-thread", Lineage: 1},
		ConnectionGeneration: 1, Context: limited,
	})
	firstBatch := effects[0].(ImportContextEffect)
	if firstBatch.From != 0 || firstBatch.To != 1 || len(firstBatch.Batch.EventIDs) != 1 {
		t.Fatalf("first bounded batch = %#v", firstBatch)
	}
	state, effects = apply(t, state, ContextImported{
		LaneID: second.ID, OperationID: firstBatch.OperationID, From: firstBatch.From, To: firstBatch.To,
		Digest: firstBatch.Batch.Digest, Found: true, Confirmed: true,
	})
	if state.Lanes[second.ID].CoveredThrough != 1 {
		t.Fatalf("covered through = %d after first batch", state.Lanes[second.ID].CoveredThrough)
	}
	secondBatch := effects[0].(ImportContextEffect)
	if secondBatch.From != 1 || secondBatch.To != 2 || firstBatch.Batch.Digest == secondBatch.Batch.Digest {
		t.Fatalf("second bounded batch = %#v", secondBatch)
	}
	state, _ = apply(t, state, ContextImported{
		LaneID: second.ID, OperationID: secondBatch.OperationID, From: secondBatch.From, To: secondBatch.To,
		Digest: secondBatch.Batch.Digest, Found: true, Confirmed: true,
	})
	if state.Lanes[second.ID].CoveredThrough != 2 || state.ActiveLaneID != second.ID {
		t.Fatalf("bounded import did not commit the switch: %#v", state.Lanes[second.ID])
	}

	corrupt := state.Clone()
	laneState := corrupt.Lanes[second.ID]
	delete(laneState.Coverage, 1)
	corrupt.Lanes[second.ID] = laneState
	if err := corrupt.Validate(); err == nil {
		t.Fatal("coverage frontier concealed a missing earlier event")
	}
}

func TestRecoveredContextImportResendsOnlyAfterAuthoritativeNotFound(t *testing.T) {
	state, _ := NewState("chat")
	source, target := testLane("chat", "source"), testLane("chat", "target")
	state, _ = apply(t, state, SelectLane{Identity: source})
	state, _ = apply(t, state, LaneOpened{
		LaneID: source.ID, Thread: provider.ThreadRef{ProviderID: "source", RootID: "source-thread", HeadID: "source-thread", Lineage: 1},
		ConnectionGeneration: 1, Context: exactContext(provider.ContextImportNonSampling),
	})
	state, _ = apply(t, state, Submit{OperationID: "source-turn", Text: "question"})
	state, _ = apply(t, state, TurnCompleted{OperationID: "source-turn", Assistant: "answer"})
	state, _ = apply(t, state, SelectLane{Identity: target})
	state, effects := apply(t, state, LaneOpened{
		LaneID: target.ID, Thread: provider.ThreadRef{ProviderID: "target", RootID: "target-thread", HeadID: "target-thread", Lineage: 1},
		ConnectionGeneration: 1, Context: exactContext(provider.ContextImportNonSampling),
	})
	initial := effects[0].(ImportContextEffect)
	state, effects = apply(t, state, ClaimEffect{EffectID: string(initial.OperationID)})
	if claimed := effects[0].(ImportContextEffect); claimed.Reconcile {
		t.Fatalf("initial import unexpectedly became readback: %#v", claimed)
	}

	// Crash before the provider accepted the dispatched import. Recovery cannot
	// know that locally, so it first resumes the same thread and performs
	// operation readback with the original immutable identity.
	state, _ = apply(t, state, RecoverOutbox{})
	if state.Outbox[len(state.Outbox)-1].Status != OutboxPending || !state.Outbox[len(state.Outbox)-1].Reconcile {
		t.Fatalf("recovered import was not readback-only: %#v", state.Outbox[len(state.Outbox)-1])
	}
	state, effects = apply(t, state, SelectLane{Identity: target})
	resume := effects[0].(ResumeLaneEffect)
	state, _ = apply(t, state, LaneOpened{
		LaneID: target.ID, Thread: resume.Thread, ConnectionGeneration: resume.Generation,
		Context: exactContext(provider.ContextImportNonSampling),
	})
	state, effects = apply(t, state, ClaimEffect{EffectID: string(initial.OperationID)})
	readback := effects[0].(ImportContextEffect)
	if !readback.Reconcile || readback.OperationID != initial.OperationID || readback.Batch.Digest != initial.Batch.Digest {
		t.Fatalf("readback changed import identity: initial=%#v readback=%#v", initial, readback)
	}
	state, _ = apply(t, state, ContextImported{
		LaneID: target.ID, OperationID: initial.OperationID, From: initial.From, To: initial.To,
		Digest: initial.Batch.Digest, Found: false, Confirmed: false, Reconciled: true,
	})
	var entry OutboxEntry
	for _, candidate := range state.Outbox {
		if candidate.ID == string(initial.OperationID) {
			entry = candidate
			break
		}
	}
	if entry.Status != OutboxPending || entry.Reconcile {
		t.Fatalf("authoritative not-found did not make the original import sendable: %#v", entry)
	}
	_, effects = apply(t, state, ClaimEffect{EffectID: string(initial.OperationID)})
	resend := effects[0].(ImportContextEffect)
	if resend.Reconcile || resend.OperationID != initial.OperationID || resend.Batch.Digest != initial.Batch.Digest {
		t.Fatalf("safe resend changed immutable import identity: initial=%#v resend=%#v", initial, resend)
	}
}

func TestOneOversizedContextEventFailsClosed(t *testing.T) {
	state, _ := NewState("chat")
	first := testLane("chat", "first")
	second := testLane("chat", "second")
	state, _ = apply(t, state, SelectLane{Identity: first})
	state, _ = apply(t, state, LaneOpened{
		LaneID:               first.ID,
		Thread:               provider.ThreadRef{ProviderID: "first", RootID: "first-thread", HeadID: "first-thread", Lineage: 1},
		ConnectionGeneration: 1, Context: exactContext(provider.ContextImportNonSampling),
	})
	state, _ = apply(t, state, Submit{OperationID: "large", Text: strings.Repeat("x", 2048)})
	state, _ = apply(t, state, TurnCompleted{OperationID: "large", Assistant: "answer"})
	state, _ = apply(t, state, SelectLane{Identity: second})
	limited := exactContext(provider.ContextImportNonSampling)
	limited.MaxImportBytes = 64
	state, effects := apply(t, state, LaneOpened{
		LaneID:               second.ID,
		Thread:               provider.ThreadRef{ProviderID: "second", RootID: "second-thread", HeadID: "second-thread", Lineage: 1},
		ConnectionGeneration: 1, Context: limited,
	})
	if len(effects) != 0 || state.Lanes[second.ID].Phase != LaneBlocked || state.Lanes[second.ID].LastError != provider.ErrorContextLimitReached {
		t.Fatalf("oversized import did not fail closed: lane=%#v effects=%#v", state.Lanes[second.ID], effects)
	}
}
