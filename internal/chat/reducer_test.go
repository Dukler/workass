package chat

import (
	"encoding/json"
	"path/filepath"
	"reflect"
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

func TestSubmitRejectsMissingPresentationOriginWithoutMutation(t *testing.T) {
	state, _ := NewState("chat")
	laneID := testLane("chat", "provider")
	state, _ = apply(t, state, SelectLane{Identity: laneID})
	revision := state.Revision

	next, effects, err := Reduce(state, Submit{
		OperationID: "missing-origin", Text: "do not infer who sent this",
		Presentation: provider.TurnPresentation{UserMessageID: "user", AssistantMessageID: "assistant"},
	})
	if err == nil || !strings.Contains(err.Error(), "turn presentation origin is required") {
		t.Fatalf("missing origin was not rejected explicitly: %v", err)
	}
	if len(effects) != 0 || next.Revision != revision || !reflect.DeepEqual(next, state) {
		t.Fatalf("rejected submit mutated actor state: next=%#v effects=%#v", next, effects)
	}
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
			UserMessageID: "public-user", AssistantMessageID: "public-assistant", Origin: "human", StartedAt: "2026-08-11T12:00:00Z",
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
	if assistant.MessageID != "public-assistant" || assistant.Status != "failed" || !assistant.Interrupted || assistant.RetryPrompt != "" {
		t.Fatalf("rejected assistant row = %#v", assistant)
	}
	if assistant.Terminal == nil || assistant.Terminal.Status != "failed" || assistant.Terminal.Error != string(provider.ErrorProviderUnavailable) {
		t.Fatalf("rejected terminal receipt = %#v", assistant.Terminal)
	}
	lane := state.Lanes[laneID.ID]
	if lane.Phase != LaneDetached || lane.CoveredThrough != 2 || lane.Coverage[1].Status != CoverageExcluded || lane.Coverage[2].Status != CoverageExcluded {
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
	state, _ = apply(t, state, Submit{OperationID: "turn", Text: "coordinate", Presentation: provider.TurnPresentation{Origin: "human"}})
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
	if _, _, err := Reduce(state, RequestBackgroundAction{Action: action}); err != nil {
		t.Fatalf("same background operation retry was rejected: %v", err)
	}
	changed := action.Clone()
	changed.Spawn.Prompt = "different review"
	if _, _, err := Reduce(state, RequestBackgroundAction{Action: changed}); err == nil {
		t.Fatal("background operation id was reused for different content")
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
		ConnectionGeneration: 1, Context: exactContext(provider.ContextImportNonSampling),
	})
	state, _ = apply(t, state, Submit{OperationID: "turn", LaneID: lane.ID, Text: "background owner", Presentation: provider.TurnPresentation{Origin: "human"}})
	state, _ = apply(t, state, TurnAdmitted{
		OperationID: "turn", Accepted: true, Turn: provider.TurnRef{OperationID: "turn", NativeID: "native-turn"},
	})
	owner := ProviderActivityOwner{LaneID: lane.ID, OperationID: "turn", TurnID: "native-turn", ConnectionGeneration: state.Lanes[lane.ID].ConnectionGeneration}
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
	if !ok || cleanup.OperationID != "delete-op" || cleanup.ChatID != "delete-chat" || cleanup.TabID != "delete-tab" {
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
	state, _ = apply(t, state, ChatDeletionCompleted{OperationID: "delete-op", ChatID: "delete-chat"})
	if state.Outbox[0].Status != OutboxCompleted {
		t.Fatalf("delete cleanup receipt = %#v", state.Outbox[0])
	}
	if _, _, err := Reduce(state, Submit{OperationID: "resurrect", Text: "no", Presentation: provider.TurnPresentation{Origin: "human"}}); err == nil {
		t.Fatal("tombstoned chat accepted semantic work")
	}
}

func TestBindEstablishedLaneOnlySchedulesExactResume(t *testing.T) {
	state, _ := NewState("chat")
	state, _ = apply(t, state, InitializeChat{Presentation: PresentationState{TabID: "tab"}, OperationID: "create", Digest: "create-digest"})
	lane := testLane("chat", "codex")
	thread := provider.ThreadRef{ProviderID: "codex", RootID: "thread-existing", HeadID: "thread-existing", Lineage: 1}
	state, effects := apply(t, state, BindEstablishedLane{
		Identity: lane, Thread: thread, Owner: provider.AttachmentOwner{TabID: "tab"},
		CWD: "/workspace", Context: exactContext(provider.ContextImportUnsupported),
	})
	if len(effects) != 0 || state.Lanes[lane.ID].Phase != LaneDetached {
		t.Fatalf("binding an established lane created provider work: phase=%q effects=%#v", state.Lanes[lane.ID].Phase, effects)
	}
	state, effects = apply(t, state, SelectLane{Identity: lane, Owner: provider.AttachmentOwner{TabID: "tab"}, CWD: "/workspace"})
	if len(effects) != 1 {
		t.Fatalf("selected established binding effects = %#v", effects)
	}
	resume, ok := effects[0].(ResumeLaneEffect)
	if !ok || !resume.Thread.Equal(thread) {
		t.Fatalf("selected established binding effect = %#v, want exact resume", effects[0])
	}

	conflict := thread
	conflict.RootID = "replacement"
	conflict.HeadID = "replacement"
	if _, _, err := Reduce(state, BindEstablishedLane{Identity: lane, Thread: conflict, Context: exactContext(provider.ContextImportUnsupported)}); err == nil {
		t.Fatal("conflicting binding replaced an existing native thread")
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
		PlanLatest:             []provider.PlanEntry{{ID: "p1", Text: "authoritative", Status: "in_progress"}},
		PlanLatestMessageID:    "assistant-authoritative",
	}, OperationID: "create:provider-switch", Digest: "create-provider-switch"})
	attackerGroup, attackerCWD, attackerPane := "renamed-group", "/forged", "browser"
	forged := PresentationState{
		TabID: "tab", Title: "Renamed", TitleLocked: true, Group: &attackerGroup,
		CWD: &attackerCWD, Draft: "draft", Unread: true, Settled: "settled", SettledAt: 1_787_000_000_000, Pane: &attackerPane,
		ProviderID: "claude", CurrentModelID: "forged-model", CurrentModeID: "forged-mode",
		WorkspaceRevision: 4, AgentQueueRevision: 7, RuntimeControlRevision: 9,
		ModelControls:          json.RawMessage(`{"ui":"allowed"}`),
		ContextUsageByProvider: json.RawMessage(`{"claude":{"used":999}}`),
		PlanLatest:             []provider.PlanEntry{{ID: "forged", Text: "forged", Status: "done"}},
		PlanLatestMessageID:    "forged-assistant",
	}
	state, _ = apply(t, state, UpdatePresentation{Presentation: forged})
	got := state.Presentation
	if got.TabID != "tab" || got.Title != "Renamed" || got.Draft != "draft" || got.Settled != "settled" || got.SettledAt != 1_787_000_000_000 || got.Group == nil || *got.Group != attackerGroup || got.Pane == nil || *got.Pane != attackerPane {
		t.Fatalf("renderer-owned presentation did not update: %#v", got)
	}
	if got.CWD == nil || *got.CWD != cwd || got.ProviderID != "codex" || got.CurrentModelID != "gpt-authoritative" || got.CurrentModeID != "ask" ||
		string(got.ContextUsageByProvider) != `{"codex":{"used":3}}` ||
		len(got.PlanLatest) != 1 || got.PlanLatest[0].ID != "p1" || got.PlanLatestMessageID != "assistant-authoritative" {
		t.Fatalf("renderer snapshot overwrote actor runtime state: %#v", got)
	}
}

func TestForegroundWorkClearsSettledArchiveClock(t *testing.T) {
	for _, presentation := range []PresentationState{
		{TabID: "tab", Settled: "settled", SettledAt: 1_787_000_000_000},
		{TabID: "tab", Settled: "active"},
	} {
		state, _ := NewState("chat")
		state, _ = apply(t, state, InitializeChat{
			Presentation: presentation, OperationID: "create:settled", Digest: "create-settled",
		})
		lane := testLane("chat", "codex")
		state, _ = apply(t, state, SelectLane{Identity: lane, Owner: provider.AttachmentOwner{TabID: "tab"}})
		state, _ = apply(t, state, LaneOpened{
			LaneID: lane.ID, Thread: provider.ThreadRef{ProviderID: "codex", RootID: "thread", HeadID: "thread", Lineage: 1},
			ConnectionGeneration: 1, Context: exactContext(provider.ContextImportUnsupported),
		})
		revision := state.Presentation.PresentationRevision
		state, _ = apply(t, state, Submit{OperationID: "new-work", Text: "resume", Presentation: provider.TurnPresentation{Origin: "human"}})

		if state.Presentation.Settled != "" || state.Presentation.SettledAt != 0 {
			t.Fatalf("foreground work retained settled archive state: %#v", state.Presentation)
		}
		if state.Presentation.PresentationRevision != revision+1 {
			t.Fatalf("foreground reset revision = %d, want %d", state.Presentation.PresentationRevision, revision+1)
		}
	}
}

func TestRendererQueueReplacementUsesActorRevisionAndCannotTouchProviderOutbox(t *testing.T) {
	state, _ := NewState("chat")
	state, _ = apply(t, state, InitializeChat{
		Presentation: PresentationState{TabID: "tab", AgentQueueRevision: 4}, OperationID: "create", Digest: "create-digest",
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
		ConnectionGeneration: 1, Context: exactContext(provider.ContextImportUnsupported),
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

func TestCreateFailureRemainsAbsentUntilExplicitIntent(t *testing.T) {
	tests := []struct {
		name      string
		kind      provider.ErrorKind
		ambiguous bool
		status    OutboxStatus
		oldPhase  LanePhase
		submit    bool
	}{
		{name: "ambiguous selection retry", kind: provider.ErrorAcceptanceAmbiguous, ambiguous: true, status: OutboxAmbiguous, oldPhase: LaneBlocked},
		{name: "deterministic submit retry", kind: provider.ErrorUnsupportedCapability, status: OutboxFailed, oldPhase: LaneBroken, submit: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state, _ := NewState("chat")
			identity := testLane("chat", "devin")
			state, effects := apply(t, state, SelectLane{Identity: identity})
			if len(effects) != 1 {
				t.Fatalf("initial selection effects = %#v", effects)
			}
			firstCreate := createEffectID(identity.ID, 1)
			state, _ = apply(t, state, ClaimEffect{EffectID: firstCreate})
			state, effects = apply(t, state, LaneOpenFailed{LaneID: identity.ID, Kind: test.kind, Ambiguous: test.ambiguous})
			if len(effects) != 0 {
				t.Fatalf("create failure retried without user intent: %#v", effects)
			}
			lane := state.Lanes[identity.ID]
			wantPhase := LaneAbsent
			if test.ambiguous {
				wantPhase = LaneBlocked
			}
			if lane.Phase != wantPhase || !lane.Thread.IsZero() || lane.Provision != nil || !lane.CreationFailedBeforeEstablishment() {
				t.Fatalf("pre-establishment create failure = %#v", lane)
			}
			if len(state.Outbox) != 1 || state.Outbox[0].ID != firstCreate || state.Outbox[0].Status != test.status {
				t.Fatalf("failed create receipt = %#v", state.Outbox)
			}
			// Actor snapshots written before this invariant used blocked/broken for
			// the same zero-thread state. Ordinary commands must classify those
			// records from their identity, without a migration or repair pass.
			lane.Phase = test.oldPhase
			state.Lanes[identity.ID] = lane
			if err := state.Validate(); err != nil {
				t.Fatalf("persisted pre-invariant state is unreadable: %v", err)
			}

			if test.submit {
				state, effects = apply(t, state, Submit{OperationID: "retry-input", Text: "try again", Presentation: provider.TurnPresentation{Origin: "human"}})
			} else {
				state, effects = apply(t, state, SelectLane{Identity: identity})
			}
			if len(effects) != 1 {
				t.Fatalf("explicit retry effects = %#v", effects)
			}
			create, ok := effects[0].(CreateLaneEffect)
			if !ok || create.Identity.ID != identity.ID || create.Generation != 2 || create.Reconcile {
				t.Fatalf("fresh create generation = %#v", effects[0])
			}
			lane = state.Lanes[identity.ID]
			if lane.Phase != LaneCreating || lane.LastError != "" || lane.CreateGeneration != 2 {
				t.Fatalf("retried unestablished lane = %#v", lane)
			}
			if len(state.Outbox) != 2 || state.Outbox[0].Status != test.status || state.Outbox[1].Status != OutboxPending || state.Outbox[1].Reconcile {
				t.Fatalf("fresh create overwrote prior audit receipt: %#v", state.Outbox)
			}
		})
	}
}

func TestAmbiguousAdmissionEndsTurnAndLeavesLaneAvailable(t *testing.T) {
	state, _ := NewState("ambiguous-admission")
	laneID := testLane(state.ChatID, "provider")
	thread := provider.ThreadRef{ProviderID: "provider", RootID: "thread", HeadID: "thread", Lineage: 1}
	state, _ = apply(t, state, SelectLane{Identity: laneID})
	state, _ = apply(t, state, LaneOpened{
		LaneID: laneID.ID, Thread: thread, ConnectionGeneration: 1,
		Context: exactContext(provider.ContextImportUnsupported), Delivery: provider.DeliveryCapabilities{StableInputIdentity: true},
	})
	state, _ = apply(t, state, Submit{
		OperationID: "ambiguous", Text: "send once", Presentation: provider.TurnPresentation{Origin: "human"},
	})
	state, effects := apply(t, state, TurnAdmitted{OperationID: "ambiguous", Ambiguous: true})
	if len(effects) != 0 || state.Foreground != nil || state.Lanes[laneID.ID].Phase != LaneDetached {
		t.Fatalf("ambiguous admission retained Workass turn ownership: state=%#v effects=%#v", state, effects)
	}
	state, effects = apply(t, state, Submit{
		OperationID: "next", Text: "continue", Presentation: provider.TurnPresentation{Origin: "human"},
	})
	if len(effects) != 1 {
		t.Fatalf("next prompt effects = %#v", effects)
	}
	resume, ok := effects[0].(ResumeLaneEffect)
	if !ok || !resume.Thread.Equal(thread) {
		t.Fatalf("next prompt did not resume the saved provider session: %#v", effects[0])
	}
}

func TestSubmitCreatesOnlyItsSelectedProviderLane(t *testing.T) {
	state, _ := NewState("chat")
	alpha := testLane("chat", "alpha")
	beta := testLane("chat", "beta")
	for _, identity := range []provider.LaneIdentity{alpha, beta} {
		var effects []Effect
		state, effects = apply(t, state, SelectLane{Identity: identity})
		if len(effects) != 1 {
			t.Fatalf("select %s effects = %#v", identity.Realm.ProviderID, effects)
		}
		state, _ = apply(t, state, ClaimEffect{EffectID: createEffectID(identity.ID, 1)})
		state, _ = apply(t, state, LaneOpenFailed{
			LaneID: identity.ID, Kind: provider.ErrorProviderUnavailable,
		})
	}

	state, effects := apply(t, state, Submit{OperationID: "selected-provider-only", Text: "hello beta", Presentation: provider.TurnPresentation{Origin: "human"}})
	if len(effects) != 1 {
		t.Fatalf("selected-provider submit effects = %#v", effects)
	}
	create, ok := effects[0].(CreateLaneEffect)
	if !ok || create.Identity.ID != beta.ID || create.Generation != 2 {
		t.Fatalf("submit created the wrong provider lane: %#v", effects[0])
	}
	if state.Lanes[alpha.ID].Phase != LaneAbsent || state.Lanes[alpha.ID].CreateGeneration != 1 {
		t.Fatalf("submit fanned out into unselected provider: %#v", state.Lanes[alpha.ID])
	}
	if state.Lanes[beta.ID].Phase != LaneCreating || state.Lanes[beta.ID].CreateGeneration != 2 {
		t.Fatalf("selected provider did not start one fresh create: %#v", state.Lanes[beta.ID])
	}
}

func TestEstablishedResumeFailureNeverCreatesReplacementThread(t *testing.T) {
	for _, test := range []struct {
		name      string
		ambiguous bool
	}{
		{name: "deterministic"},
		{name: "ambiguous", ambiguous: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			state, _ := NewState("chat")
			identity := testLane("chat", "codex")
			thread := provider.ThreadRef{ProviderID: "codex", RootID: "thread-1", HeadID: "thread-1", Lineage: 1}
			state, _ = apply(t, state, SelectLane{Identity: identity})
			state, _ = apply(t, state, LaneOpened{
				LaneID: identity.ID, Thread: thread, ConnectionGeneration: 1,
				Context: exactContext(provider.ContextImportUnsupported),
			})
			state, _ = apply(t, state, HostLost{LaneID: identity.ID, ConnectionGeneration: 1})
			state, effects := apply(t, state, SelectLane{Identity: identity})
			if len(effects) != 1 {
				t.Fatalf("exact resume effects = %#v", effects)
			}
			if _, ok := effects[0].(ResumeLaneEffect); !ok {
				t.Fatalf("established lane emitted %T, want ResumeLaneEffect", effects[0])
			}
			state, _ = apply(t, state, LaneOpenFailed{
				LaneID: identity.ID, Kind: provider.ErrorNativeThreadMissing, Ambiguous: test.ambiguous,
			})
			lane := state.Lanes[identity.ID]
			if lane.Phase != LaneDetached || !lane.Thread.Equal(thread) || lane.CreationFailedBeforeEstablishment() {
				t.Fatalf("established resume failure changed identity: %#v", lane)
			}
			state, effects = apply(t, state, SelectLane{Identity: identity})
			if len(effects) != 1 {
				t.Fatalf("new selection did not reattach the saved thread: %#v", effects)
			}
			resume, ok := effects[0].(ResumeLaneEffect)
			if !ok || !resume.Thread.Equal(thread) {
				t.Fatalf("new selection changed native thread: %#v", effects[0])
			}
		})
	}
}

func TestSelectingTransientlyDisconnectedEstablishedLaneResumesSavedThread(t *testing.T) {
	for _, test := range []struct {
		name      string
		ambiguous bool
	}{
		{name: "definitive transport failure"},
		{name: "ambiguous transport failure", ambiguous: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			state, _ := NewState("chat")
			identity := testLane("chat", "codex")
			thread := provider.ThreadRef{ProviderID: "codex", RootID: "saved-session", HeadID: "saved-session", Lineage: 1}
			state, _ = apply(t, state, SelectLane{Identity: identity})
			state, _ = apply(t, state, LaneOpened{
				LaneID: identity.ID, Thread: thread, ConnectionGeneration: 1,
				Context: exactContext(provider.ContextImportUnsupported),
			})
			state, _ = apply(t, state, HostLost{LaneID: identity.ID, ConnectionGeneration: 1})
			state, effects := apply(t, state, SelectLane{Identity: identity})
			if len(effects) != 1 {
				t.Fatalf("initial exact attach effects = %#v", effects)
			}
			resume := effects[0].(ResumeLaneEffect)
			state, _ = apply(t, state, ClaimEffect{EffectID: resumeEffectID(identity.ID, resume.Generation)})
			state, _ = apply(t, state, LaneOpenFailed{
				LaneID: identity.ID, Kind: provider.ErrorTransientTransport, Ambiguous: test.ambiguous,
			})
			if state.Lanes[identity.ID].Phase != LaneDetached {
				t.Fatalf("transport failure phase = %q, want detached", state.Lanes[identity.ID].Phase)
			}

			state, effects = apply(t, state, SelectLane{Identity: identity})
			if len(effects) != 1 {
				t.Fatalf("saved-session selection effects = %#v", effects)
			}
			attached, ok := effects[0].(ResumeLaneEffect)
			if !ok || !attached.Thread.Equal(thread) || attached.Generation != resume.Generation+1 {
				t.Fatalf("selection did not attach the exact saved session: %#v", effects[0])
			}
			lane := state.Lanes[identity.ID]
			if lane.Phase != LaneResuming || lane.LastError != "" || !lane.Thread.Equal(thread) {
				t.Fatalf("saved session identity changed while attaching: %#v", lane)
			}
			for _, effect := range effects {
				if _, created := effect.(CreateLaneEffect); created {
					t.Fatalf("saved-session attach attempted replacement creation: %#v", effects)
				}
			}
		})
	}
}

func TestDeferredProviderThreadExistsOnlyAfterMatchingInputReceipt(t *testing.T) {
	state, _ := NewState("chat")
	laneID := testLane("chat", "codex")
	creation := provider.CreationCapabilities{DeferredUntilInput: true}
	delivery := provider.DeliveryCapabilities{StableInputIdentity: true, ConsumptionReceipt: true}
	candidate := provider.ThreadRef{ProviderID: "codex", RootID: "candidate", HeadID: "candidate", Lineage: 1}

	state, effects := apply(t, state, SelectLane{Identity: laneID, Creation: creation})
	if len(effects) != 0 || state.Lanes[laneID.ID].Phase != LaneAbsent {
		t.Fatalf("empty deferred selection touched the provider: effects=%#v lane=%#v", effects, state.Lanes[laneID.ID])
	}
	state, effects = apply(t, state, Submit{OperationID: "first-input", Text: "hello", Presentation: provider.TurnPresentation{Origin: "human"}})
	if len(effects) != 1 {
		t.Fatalf("first input did not request exactly one candidate: %#v", effects)
	}
	create, ok := effects[0].(CreateLaneEffect)
	if !ok || create.Reconcile || !create.CreateAfterCandidateAbsence {
		t.Fatalf("first input create effect = %#v", effects[0])
	}
	state, effects = apply(t, state, LaneProvisioned{
		LaneID: laneID.ID, Identity: laneID, Candidate: candidate, ConnectionGeneration: 1,
		Context: exactContext(provider.ContextImportUnsupported), Delivery: delivery, Creation: creation,
	})
	if len(effects) != 1 {
		t.Fatalf("candidate did not release the queued input: %#v", effects)
	}
	if _, ok := effects[0].(StartTurnEffect); !ok {
		t.Fatalf("candidate effect = %T, want StartTurnEffect", effects[0])
	}
	provisional := state.Lanes[laneID.ID]
	if !provisional.Thread.IsZero() || provisional.Provision == nil || !provisional.Provision.Equal(candidate) || len(state.Ledger) != 0 {
		t.Fatalf("candidate escaped the provisional boundary: lane=%#v ledger=%#v", provisional, state.Ledger)
	}
	state, _ = apply(t, state, TurnAdmitted{
		OperationID: "first-input", Accepted: true,
		Turn: provider.TurnRef{OperationID: "first-input", NativeID: "native-turn"},
	})
	if _, _, err := Reduce(state, InputConsumed{OperationID: "first-input"}); err == nil {
		t.Fatal("deferred input was accepted without its durable thread receipt")
	}
	other := provider.ThreadRef{ProviderID: "codex", RootID: "other", HeadID: "other", Lineage: 1}
	if _, _, err := Reduce(state, InputConsumed{OperationID: "first-input", Thread: &other}); err == nil {
		t.Fatal("deferred input promoted a different provider candidate")
	}
	state, _ = apply(t, state, InputConsumed{OperationID: "first-input", Thread: &candidate})
	established := state.Lanes[laneID.ID]
	if !established.Thread.Equal(candidate) || established.Provision != nil || established.Phase != LaneRunning ||
		established.CreateAfterCandidateAbsence {
		t.Fatalf("matching receipt did not promote the exact candidate: %#v", established)
	}
	if len(state.Ledger) != 1 || state.Ledger[0].OperationID != "first-input" || state.Ledger[0].Role != "user" {
		t.Fatalf("matching receipt did not admit exactly one user event: %#v", state.Ledger)
	}
}

func TestNegotiatedDeferredCandidateAcceptsTheLaterFirstInput(t *testing.T) {
	state, _ := NewState("chat")
	laneID := testLane("chat", "load-only-acp")
	state, effects := apply(t, state, SelectLane{Identity: laneID})
	if len(effects) != 1 {
		t.Fatalf("eager ACP selection did not request one create: %#v", effects)
	}
	creation := provider.CreationCapabilities{DeferredUntilInput: true}
	delivery := provider.DeliveryCapabilities{StableInputIdentity: true, ConsumptionReceipt: true}
	candidate := provider.ThreadRef{ProviderID: "load-only-acp", RootID: "candidate", HeadID: "candidate", Lineage: 1}
	state, effects = apply(t, state, LaneProvisioned{
		LaneID: laneID.ID, Identity: laneID, Candidate: candidate, ConnectionGeneration: 1,
		Context: exactContext(provider.ContextImportUnsupported), Delivery: delivery, Creation: creation,
	})
	if len(effects) != 0 || state.Lanes[laneID.ID].Provision == nil {
		t.Fatalf("idle negotiated candidate was not retained provisionally: effects=%#v lane=%#v", effects, state.Lanes[laneID.ID])
	}
	state, effects = apply(t, state, Submit{OperationID: "later-first-input", Text: "hello after selection", Presentation: provider.TurnPresentation{Origin: "human"}})
	if len(effects) != 1 {
		t.Fatalf("later input did not release the existing candidate: %#v", effects)
	}
	start, ok := effects[0].(StartTurnEffect)
	if !ok || start.Input.OperationID != "later-first-input" || start.LaneID != laneID.ID {
		t.Fatalf("later input effect = %#v", effects[0])
	}
	if state.Foreground == nil || state.Foreground.OperationID != "later-first-input" || !state.Lanes[laneID.ID].Thread.IsZero() {
		t.Fatalf("later input escaped the provisional boundary before receipt: %#v", state)
	}
}

func TestDeferredProviderCrashReconcilesExactCandidateBeforeAnyResend(t *testing.T) {
	state, _ := NewState("chat")
	laneID := testLane("chat", "codex")
	creation := provider.CreationCapabilities{DeferredUntilInput: true}
	delivery := provider.DeliveryCapabilities{StableInputIdentity: true, ConsumptionReceipt: true}
	first := provider.ThreadRef{ProviderID: "codex", RootID: "candidate-1", HeadID: "candidate-1", Lineage: 1}
	second := provider.ThreadRef{ProviderID: "codex", RootID: "candidate-2", HeadID: "candidate-2", Lineage: 1}

	state, _ = apply(t, state, SelectLane{Identity: laneID, Creation: creation})
	state, _ = apply(t, state, Submit{OperationID: "first-input", Text: "hello", Presentation: provider.TurnPresentation{Origin: "human"}})
	state, _ = apply(t, state, LaneProvisioned{
		LaneID: laneID.ID, Identity: laneID, Candidate: first, ConnectionGeneration: 1,
		Context: exactContext(provider.ContextImportUnsupported), Delivery: delivery, Creation: creation,
	})

	t.Run("authoritative absence permits one fresh candidate", func(t *testing.T) {
		beforeAdmission := state.Clone()
		next, effects := apply(t, beforeAdmission, HostLost{LaneID: laneID.ID, ConnectionGeneration: 1})
		if len(effects) != 1 {
			t.Fatalf("candidate crash did not request one exact reconciliation: %#v", effects)
		}
		reconcile, ok := effects[0].(CreateLaneEffect)
		if !ok || !reconcile.Reconcile || !reconcile.CreateAfterCandidateAbsence || reconcile.Generation != 2 {
			t.Fatalf("candidate crash effect = %#v", effects[0])
		}
		next, effects = apply(t, next, LaneProvisioned{
			LaneID: laneID.ID, Identity: laneID, Candidate: second, ConnectionGeneration: 2,
			Context: exactContext(provider.ContextImportUnsupported), Delivery: delivery, Creation: creation,
			Reconciled: true, PreviousCandidateAbsent: true,
		})
		if len(effects) != 0 || next.Foreground == nil || next.Foreground.OperationID != "first-input" {
			t.Fatalf("absence reconciliation changed the exact input owner: effects=%#v foreground=%#v", effects, next.Foreground)
		}
		lane := next.Lanes[laneID.ID]
		if !lane.Thread.IsZero() || lane.Provision == nil || !lane.Provision.Equal(second) {
			t.Fatalf("absence reconciliation exposed or changed the fresh candidate: %#v", lane)
		}
		if !outboxHas(&next, startTurnEffectID("first-input"), OutboxPending) {
			t.Fatalf("absence proof did not release the same input operation: %#v", next.Outbox)
		}
	})

	t.Run("admitted candidate loss ends turn and next input reattaches candidate", func(t *testing.T) {
		admitted, _ := apply(t, state.Clone(), TurnAdmitted{
			OperationID: "first-input", Accepted: true,
			Turn: provider.TurnRef{OperationID: "first-input", NativeID: "native-turn"},
		})
		lost, effects := apply(t, admitted, HostLost{LaneID: laneID.ID, ConnectionGeneration: 1})
		if len(effects) != 0 || lost.Foreground != nil || lost.Lanes[laneID.ID].Phase != LaneAbsent {
			t.Fatalf("admitted candidate crash retained turn control: state=%#v effects=%#v", lost, effects)
		}
		lost, effects = apply(t, lost, Submit{
			OperationID: "second-input", Text: "continue", Presentation: provider.TurnPresentation{Origin: "human"},
		})
		if len(effects) != 1 {
			t.Fatalf("next input did not request candidate attachment: %#v", effects)
		}
		attach, ok := effects[0].(CreateLaneEffect)
		if !ok || !attach.Reconcile || !attach.CreateAfterCandidateAbsence || attach.Generation != 2 {
			t.Fatalf("next input candidate attachment = %#v", effects[0])
		}
	})
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
	state, effects := apply(t, state, Submit{OperationID: "op-codex", Text: "first", Presentation: provider.TurnPresentation{Origin: "human"}})
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
	state, _ = apply(t, state, InitializeChat{Presentation: PresentationState{TabID: "tab"}, OperationID: "create", Digest: "create-digest"})
	lane := testLane("chat", "codex")
	state, _ = apply(t, state, BindEstablishedLane{
		Identity: lane,
		Thread:   provider.ThreadRef{ProviderID: "codex", RootID: "thread", HeadID: "thread", Lineage: 1},
		Owner:    provider.AttachmentOwner{TabID: "tab"}, Context: exactContext(provider.ContextImportUnsupported),
	})
	state.Operations["operation"] = struct{}{}
	state.Ledger = []LedgerEvent{
		{EventID: "event-user", MessageID: "user", Sequence: 1, Role: "user", Text: "question", Status: "done", LaneID: lane.ID, ProviderID: "codex", OperationID: "operation"},
		{EventID: "event-assistant", MessageID: "assistant", Sequence: 2, Role: "assistant", Text: "commentary", Result: "final answer", Status: "done", TerminalState: "done", LaneID: lane.ID, ProviderID: "codex", OperationID: "operation"},
	}
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
	state, _ = apply(t, state, Submit{OperationID: "running", Text: "work", Presentation: provider.TurnPresentation{Origin: "human"}})
	state, effects := apply(t, state, SelectLane{Identity: claude})
	if len(effects) != 0 {
		t.Fatalf("switch during turn emitted effects: %#v", effects)
	}
	if state.Foreground == nil || state.Foreground.LaneID != codex.ID || state.DesiredLaneID != claude.ID {
		t.Fatalf("foreground/desired ownership changed incorrectly: %#v", state)
	}
	state, _ = apply(t, state, Submit{OperationID: "queued-for-claude", Text: "next", Presentation: provider.TurnPresentation{Origin: "human"}})
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
	state, _ = apply(t, state, Submit{OperationID: "turn", Text: "work", Presentation: provider.TurnPresentation{Origin: "human"}})
	state, _ = apply(t, state, TurnAdmitted{
		OperationID: "turn", Accepted: true, Turn: provider.TurnRef{OperationID: "turn", NativeID: "native-turn"},
	})
	state, _ = apply(t, state, SelectLane{Identity: beta})

	state, effects := apply(t, state, Steer{OperationID: "steer", Text: "redirect", Presentation: provider.TurnPresentation{Origin: "human"}})
	steer, ok := effects[0].(SteerTurnEffect)
	if !ok || steer.LaneID != alpha.ID || steer.Turn.NativeID != "native-turn" {
		t.Fatalf("steer was retargeted by provider selection: %#v", effects)
	}
	state, _ = apply(t, state, SteerFailed{
		OperationID: "steer", Kind: provider.ErrorUnsupportedCapability, Unsupported: true,
	})
	if len(state.Queue) != 0 || state.PendingSteer != nil || !outboxHas(&state, steerEffectID("steer"), OutboxFailed) {
		t.Fatalf("rejected explicit steer entered FIFO or lost its typed receipt: queue=%#v outbox=%#v", state.Queue, state.Outbox)
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
	if state.Foreground != nil || len(state.Queue) != 0 {
		t.Fatalf("stop resurrected a rejected steer: foreground=%#v queue=%#v", state.Foreground, state.Queue)
	}
}

func TestSteerFailureTransfersOnlyHeadlessQueueOwner(t *testing.T) {
	newRunning := func(t *testing.T) State {
		t.Helper()
		state, _ := NewState("chat")
		lane := testLane("chat", "codex")
		state, _ = apply(t, state, SelectLane{Identity: lane})
		state, _ = apply(t, state, LaneOpened{
			LaneID:               lane.ID,
			Thread:               provider.ThreadRef{ProviderID: "codex", RootID: "thread", HeadID: "thread", Lineage: 1},
			ConnectionGeneration: 1,
			Context:              exactContext(provider.ContextImportNonSampling),
		})
		state, _ = apply(t, state, Submit{OperationID: "turn", Text: "work", Presentation: provider.TurnPresentation{Origin: "human"}})
		state, _ = apply(t, state, TurnAdmitted{
			OperationID: "turn", Accepted: true,
			Turn: provider.TurnRef{OperationID: "turn", NativeID: "native-turn"},
		})
		return state
	}

	t.Run("human rejection never enters FIFO", func(t *testing.T) {
		state := newRunning(t)
		state, _ = apply(t, state, Steer{
			OperationID: "human-steer", Text: "redirect",
			Presentation: provider.TurnPresentation{Origin: "human", UserMessageID: "human-user"},
		})
		state, _ = apply(t, state, SteerFailed{OperationID: "human-steer", Kind: provider.ErrorUnsupportedCapability})
		if state.PendingSteer != nil || len(state.Queue) != 0 || !outboxHas(&state, steerEffectID("human-steer"), OutboxFailed) {
			t.Fatalf("human rejection gained a FIFO owner: pending=%#v queue=%#v outbox=%#v", state.PendingSteer, state.Queue, state.Outbox)
		}
	})

	t.Run("agent rejection transfers the exact immutable owner once", func(t *testing.T) {
		state := newRunning(t)
		operationID := provider.OperationID("q:agent-send:fixture")
		presentation := provider.TurnPresentation{
			Origin: "agent", QueueID: string(operationID), PromptText: "redirect",
			UserMessageID: "agent-user", AssistantMessageID: "agent-assistant", StartedAt: "2026-08-20T12:00:00Z",
		}
		state, _ = apply(t, state, Steer{OperationID: operationID, Text: "redirect", Presentation: presentation})
		state, _ = apply(t, state, SteerFailed{OperationID: operationID, Kind: provider.ErrorUnsupportedCapability})
		if state.PendingSteer != nil || len(state.Queue) != 1 {
			t.Fatalf("agent rejection lost its FIFO owner: pending=%#v queue=%#v", state.PendingSteer, state.Queue)
		}
		queued := state.Queue[0]
		if queued.OperationID != operationID || queued.Text != "redirect" || queued.Presentation != presentation {
			t.Fatalf("agent steer owner changed during transfer: %#v", queued)
		}
		if state.Presentation.AgentQueueRevision != 1 || !outboxHas(&state, steerEffectID(operationID), OutboxCompleted) {
			t.Fatalf("agent transfer has no terminal steer receipt: revision=%d outbox=%#v", state.Presentation.AgentQueueRevision, state.Outbox)
		}
		state, _ = apply(t, state, SteerFailed{OperationID: operationID, Kind: provider.ErrorUnsupportedCapability})
		if len(state.Queue) != 1 || state.Presentation.AgentQueueRevision != 1 {
			t.Fatalf("lost-reply retry duplicated the agent owner: revision=%d queue=%#v", state.Presentation.AgentQueueRevision, state.Queue)
		}
	})

	t.Run("success and ambiguity never transfer", func(t *testing.T) {
		accepted := newRunning(t)
		accepted, _ = apply(t, accepted, Steer{
			OperationID: "agent-accepted", Text: "redirect",
			Presentation: provider.TurnPresentation{Origin: "agent", QueueID: "agent-accepted"},
		})
		accepted, _ = apply(t, accepted, SteerAdmitted{OperationID: "agent-accepted", Accepted: true, Consumed: true})
		if len(accepted.Queue) != 0 {
			t.Fatalf("accepted steer entered FIFO: %#v", accepted.Queue)
		}

		uncertain := newRunning(t)
		uncertain, _ = apply(t, uncertain, Steer{
			OperationID: "agent-uncertain", Text: "redirect",
			Presentation: provider.TurnPresentation{Origin: "agent", QueueID: "agent-uncertain"},
		})
		uncertain, _ = apply(t, uncertain, SteerFailed{OperationID: "agent-uncertain", Ambiguous: true})
		if uncertain.PendingSteer == nil || uncertain.PendingSteer.Status != SteerUncertain || len(uncertain.Queue) != 0 {
			t.Fatalf("ambiguous steer was replayed: pending=%#v queue=%#v", uncertain.PendingSteer, uncertain.Queue)
		}
	})
}

func TestLateSteerReceiptAfterUrgentCancelNeverReplaysInput(t *testing.T) {
	state, _ := NewState("chat")
	lane := testLane("chat", "alpha")
	state, _ = apply(t, state, SelectLane{Identity: lane})
	state, _ = apply(t, state, LaneOpened{
		LaneID: lane.ID, Thread: provider.ThreadRef{ProviderID: "alpha", RootID: "thread", HeadID: "thread", Lineage: 1},
		ConnectionGeneration: 1, Context: exactContext(provider.ContextImportNonSampling),
	})
	state, _ = apply(t, state, Submit{OperationID: "turn", Text: "work", Presentation: provider.TurnPresentation{Origin: "human"}})
	state, _ = apply(t, state, TurnAdmitted{
		OperationID: "turn", Accepted: true, Turn: provider.TurnRef{OperationID: "turn", NativeID: "native-turn"},
	})
	state, _ = apply(t, state, Steer{
		OperationID: "steer", Text: "redirect",
		Presentation: provider.TurnPresentation{UserMessageID: "steer-user", Origin: "human"},
	})
	state, _ = apply(t, state, ClaimEffect{EffectID: steerEffectID("steer")})
	state, _ = apply(t, state, CancelTurn{OperationID: "cancel"})
	state, _ = apply(t, state, TurnTerminated{OperationID: "turn", Status: "cancelled"})
	state, _ = apply(t, state, SteerAdmitted{OperationID: "steer", Accepted: true, AwaitConsumption: true})

	if state.Foreground != nil || len(state.Queue) != 0 {
		t.Fatalf("urgent stop replayed work: foreground=%#v queue=%#v", state.Foreground, state.Queue)
	}
	var rows int
	for _, event := range state.Ledger {
		if event.OperationID == "steer" {
			rows++
			if event.SteerState != "uncertain" || event.TerminalState != "unconsumed" {
				t.Fatalf("late steer changed safe terminal ownership: %#v", event)
			}
		}
	}
	if rows != 1 || !outboxHas(&state, steerEffectID("steer"), OutboxAmbiguous) {
		t.Fatalf("late steer ownership rows=%d outbox=%#v", rows, state.Outbox)
	}
}

func TestCancelAutomaticallyDrivesNextExplicitQueuedTurnExactlyOnce(t *testing.T) {
	state, _ := NewState("chat")
	lane := testLane("chat", "alpha")
	state, _ = apply(t, state, SelectLane{Identity: lane})
	state, _ = apply(t, state, LaneOpened{
		LaneID: lane.ID, Thread: provider.ThreadRef{ProviderID: "alpha", RootID: "thread", HeadID: "thread", Lineage: 1},
		ConnectionGeneration: 1, Context: exactContext(provider.ContextImportNonSampling),
	})
	state, _ = apply(t, state, Submit{OperationID: "active", Text: "active", Presentation: provider.TurnPresentation{Origin: "human"}})
	state, _ = apply(t, state, TurnAdmitted{
		OperationID: "active", Accepted: true, Turn: provider.TurnRef{OperationID: "active", NativeID: "native-active"},
	})
	state, _ = apply(t, state, Submit{OperationID: "next-one", Text: "one", Presentation: provider.TurnPresentation{Origin: "human"}})
	state, _ = apply(t, state, Submit{OperationID: "next-two", Text: "two", Presentation: provider.TurnPresentation{Origin: "human"}})
	state, _ = apply(t, state, CancelTurn{OperationID: "cancel-active"})
	state, effects := apply(t, state, TurnTerminated{OperationID: "active", Status: "cancelled"})
	if state.Foreground == nil || state.Foreground.OperationID != "next-one" || len(state.Queue) != 1 || state.Queue[0].OperationID != "next-two" {
		t.Fatalf("terminal cancel did not preserve and advance FIFO once: foreground=%#v queue=%#v", state.Foreground, state.Queue)
	}
	if len(effects) != 1 {
		t.Fatalf("terminal cancel emitted %d effects, want exactly one next start: %#v", len(effects), effects)
	}
	state, effects = apply(t, state, CancelAcknowledged{OperationID: "cancel-active"})
	if len(effects) != 0 || state.Foreground == nil || state.Foreground.OperationID != "next-one" || len(state.Queue) != 1 {
		t.Fatalf("late cancel acknowledgement duplicated FIFO advance: foreground=%#v queue=%#v effects=%#v", state.Foreground, state.Queue, effects)
	}
}

func TestCancelPendingTurnPreservesFIFOAndAdvancesOnlyAtItsExactBoundary(t *testing.T) {
	newReady := func(t *testing.T) State {
		t.Helper()
		state, _ := NewState("chat")
		lane := testLane("chat", "alpha")
		state, _ = apply(t, state, SelectLane{Identity: lane})
		state, _ = apply(t, state, LaneOpened{
			LaneID: lane.ID, Thread: provider.ThreadRef{ProviderID: "alpha", RootID: "thread", HeadID: "thread", Lineage: 1},
			ConnectionGeneration: 1, Context: exactContext(provider.ContextImportNonSampling),
		})
		return state
	}
	presentation := func(id string) provider.TurnPresentation {
		return provider.TurnPresentation{
			Origin: "human", UserMessageID: "user-" + id, AssistantMessageID: "assistant-" + id,
			PromptText: id, StartedAt: "2026-08-20T12:00:00Z",
		}
	}

	t.Run("queued row is removed without disturbing the dispatching head", func(t *testing.T) {
		state := newReady(t)
		state, _ = apply(t, state, Submit{OperationID: "head", Text: "head", Presentation: presentation("head")})
		state, _ = apply(t, state, Submit{OperationID: "cancel-me", Text: "cancel-me", Presentation: presentation("cancel-me")})
		state, _ = apply(t, state, Submit{OperationID: "survivor", Text: "survivor", Presentation: presentation("survivor")})
		state, effects := apply(t, state, CancelPendingTurn{OperationID: "cancel-me"})
		if len(effects) != 0 || state.Foreground == nil || state.Foreground.OperationID != "head" ||
			len(state.Queue) != 1 || state.Queue[0].OperationID != "survivor" {
			t.Fatalf("queued pre-admission cancel disturbed FIFO: foreground=%#v queue=%#v effects=%#v", state.Foreground, state.Queue, effects)
		}
		assertPreAdmissionCancellationRows(t, state, "cancel-me")
		retry, effects := apply(t, state, CancelPendingTurn{OperationID: "cancel-me"})
		if len(effects) != 0 || retry.Foreground == nil || retry.Foreground.OperationID != "head" || len(retry.Queue) != 1 {
			t.Fatalf("queued cancel retry mutated its terminal receipt: foreground=%#v queue=%#v effects=%#v", retry.Foreground, retry.Queue, effects)
		}
		assertPreAdmissionCancellationRows(t, retry, "cancel-me")
	})

	t.Run("dispatched head settles once and starts exactly one survivor", func(t *testing.T) {
		state := newReady(t)
		state, _ = apply(t, state, Submit{OperationID: "cancel-head", Text: "cancel-head", Presentation: presentation("cancel-head")})
		state, _ = apply(t, state, ClaimEffect{EffectID: startTurnEffectID("cancel-head")})
		state, _ = apply(t, state, Submit{OperationID: "next-one", Text: "next-one", Presentation: presentation("next-one")})
		state, _ = apply(t, state, Submit{OperationID: "next-two", Text: "next-two", Presentation: presentation("next-two")})
		state, effects := apply(t, state, CancelPendingTurn{OperationID: "cancel-head"})
		if state.Foreground == nil || state.Foreground.OperationID != "next-one" || len(state.Queue) != 1 || state.Queue[0].OperationID != "next-two" {
			t.Fatalf("dispatched pre-admission cancel did not preserve FIFO: foreground=%#v queue=%#v", state.Foreground, state.Queue)
		}
		if len(effects) != 1 {
			t.Fatalf("dispatched pre-admission cancel started %d survivors, want one: %#v", len(effects), effects)
		}
		start, ok := effects[0].(StartTurnEffect)
		if !ok || start.Input.OperationID != "next-one" {
			t.Fatalf("wrong survivor start after pre-admission cancel: %#v", effects)
		}
		assertPreAdmissionCancellationRows(t, state, "cancel-head")
		retry, effects := apply(t, state, CancelPendingTurn{OperationID: "cancel-head"})
		if len(effects) != 0 || retry.Foreground == nil || retry.Foreground.OperationID != "next-one" || len(retry.Queue) != 1 {
			t.Fatalf("dispatched cancel retry duplicated FIFO advancement: foreground=%#v effects=%#v", retry.Foreground, effects)
		}
		assertPreAdmissionCancellationRows(t, retry, "cancel-head")
	})
}

func assertPreAdmissionCancellationRows(t *testing.T, state State, operationID provider.OperationID) {
	t.Helper()
	rows := make([]LedgerEvent, 0, 2)
	for _, event := range state.Ledger {
		if event.OperationID == operationID {
			rows = append(rows, event)
		}
	}
	if len(rows) != 2 || rows[0].Role != "user" || rows[1].Role != "assistant" ||
		rows[0].TerminalState != "cancelled_before_admission" || rows[1].TerminalState != "cancelled_before_admission" ||
		rows[1].Status != "cancelled" || !rows[1].Interrupted || rows[1].Terminal == nil || rows[1].Terminal.StopReason != "cancelled" {
		t.Fatalf("pre-admission cancellation rows = %#v", rows)
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
	state, _ = apply(t, state, Submit{OperationID: "turn", Text: "work", Presentation: provider.TurnPresentation{Origin: "human"}})
	state, _ = apply(t, state, TurnAdmitted{
		OperationID: "turn", Accepted: true, Turn: provider.TurnRef{OperationID: "turn", NativeID: "native-turn"},
	})
	state, _ = apply(t, state, Steer{OperationID: "steer", Text: "one direction", Presentation: provider.TurnPresentation{Origin: "human"}})
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
		ConnectionGeneration: 1, Context: exactContext(provider.ContextImportUnsupported),
	})
	state, _ = apply(t, state, Submit{
		OperationID: "turn", Text: "question", ModelID: "model",
		Presentation: provider.TurnPresentation{
			UserMessageID: "user", AssistantMessageID: "assistant", Origin: "human", StartedAt: "2026-08-11T12:00:00Z",
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
		Presentation: provider.TurnPresentation{UserMessageID: "user", AssistantMessageID: "assistant", Origin: "human", StartedAt: "2026-08-11T12:00:00Z"},
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
		Presentation: provider.TurnPresentation{UserMessageID: "steer-user", Origin: "human", StartedAt: "2026-08-11T12:00:05Z"},
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
	if assistant.Text != "canonical whole turn" || assistant.Status != "failed" || assistant.At != "2026-08-11T12:00:10Z" || !assistant.Interrupted || assistant.RetryPrompt != "" || len(assistant.Attachments) != 1 {
		t.Fatalf("terminal assistant projection state = %#v", assistant)
	}
	if assistant.Terminal == nil || assistant.Terminal.Error != "boom" || !assistant.Terminal.CrashInterrupted || assistant.Terminal.DispositionState != "needs_input" || assistant.Terminal.Code == nil || *assistant.Terminal.Code != 1 {
		t.Fatalf("terminal receipt was collapsed: %#v", assistant.Terminal)
	}
	if assistant.Permission != nil || len(state.Permissions) != 0 {
		t.Fatalf("unresolved permission survived terminal: message=%#v state=%#v", assistant.Permission, state.Permissions)
	}
	if len(assistant.Timeline) != 1 || assistant.Timeline[0].Tool == nil || assistant.Timeline[0].Tool.Status != "failed" || assistant.Timeline[0].Tool.EndedAtUnixMS == 0 || state.Tools["tool"].Owner.OperationID != provider.OperationID("turn") {
		t.Fatalf("running tool did not settle at terminal: %#v tools=%#v", assistant.Timeline, state.Tools)
	}
	steer := state.Ledger[2]
	if steer.MessageID != "steer-user" || steer.SteerState != "accepted" || steer.Status != "done" || steer.TerminalState != "unconsumed" {
		t.Fatalf("unconsumed steer was dropped or rewritten: %#v", steer)
	}
	if !outboxHas(&state, steerEffectID("steer"), OutboxAmbiguous) {
		t.Fatalf("unconsumed steer outbox is not ambiguity-fenced: %#v", state.Outbox)
	}
	settledLane := state.Lanes[lane.ID]
	if settledLane.CoveredThrough != 3 || settledLane.Coverage[3].Status != CoverageExcluded {
		t.Fatalf("unconsumed steer created a provider import gap: %#v", settledLane)
	}
	state, effects := apply(t, state, Submit{OperationID: "next-turn", Text: "continue safely", Presentation: provider.TurnPresentation{Origin: "human"}})
	if len(effects) != 1 {
		t.Fatalf("next turn did not start after ambiguity-fenced steer: %#v", effects)
	}
	if start, ok := effects[0].(StartTurnEffect); !ok || start.Input.OperationID != "next-turn" {
		t.Fatalf("next turn effect = %#v", effects[0])
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
	state, _ = apply(t, state, Submit{OperationID: "turn", Text: "question", Presentation: provider.TurnPresentation{UserMessageID: "user", AssistantMessageID: "assistant", Origin: "human"}})
	state, _ = apply(t, state, TurnAdmitted{OperationID: "turn", Accepted: true, Turn: provider.TurnRef{OperationID: "turn", NativeID: "native-turn"}})
	state, _ = apply(t, state, Steer{OperationID: "steer", Text: "direction", Presentation: provider.TurnPresentation{UserMessageID: "steer-user", AssistantMessageID: "continuation", Origin: "human"}})
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

func TestFreshProviderWithoutContextImportSeedsNonemptyChatOnce(t *testing.T) {
	state, _ := NewState("chat")
	codex := testLane("chat", "codex")
	limited := testLane("chat", "limited")
	state, _ = apply(t, state, SelectLane{Identity: codex})
	state, _ = apply(t, state, LaneOpened{
		LaneID: codex.ID, Thread: provider.ThreadRef{ProviderID: "codex", RootID: "thread", HeadID: "thread", Lineage: 1},
		ConnectionGeneration: 1, Context: exactContext(provider.ContextImportNonSampling),
	})
	state, _ = apply(t, state, Submit{OperationID: "op", Text: "hello", Presentation: provider.TurnPresentation{Origin: "human"}})
	state, _ = apply(t, state, TurnCompleted{OperationID: "op", Assistant: "world"})
	state, _ = apply(t, state, SelectLane{Identity: limited})
	state, effects := apply(t, state, LaneOpened{
		LaneID: limited.ID, Thread: provider.ThreadRef{ProviderID: "limited", RootID: "thread", HeadID: "thread", Lineage: 1},
		ConnectionGeneration: 1, Context: exactContext(provider.ContextImportUnsupported),
	})
	if len(effects) != 0 || state.Lanes[limited.ID].Phase != LaneReady || state.ActiveLaneID != codex.ID {
		t.Fatalf("fresh unsupported-import lane was not left ready for its first input: lane=%#v active=%q effects=%#v", state.Lanes[limited.ID], state.ActiveLaneID, effects)
	}
	state, effects = apply(t, state, Submit{OperationID: "limited-op", Text: "continue here", Presentation: provider.TurnPresentation{Origin: "human"}})
	if len(effects) != 1 {
		t.Fatalf("fresh provider did not start with one seeded turn: %#v", effects)
	}
	start, ok := effects[0].(StartTurnEffect)
	if !ok || start.Input.Text != "continue here" || start.Input.InitialSeedThrough != 2 ||
		start.Input.InitialSeedDigest == "" || len(start.Seed.Messages) != 2 ||
		start.Seed.Messages[0].Text != "hello" || start.Seed.Messages[1].Text != "world" {
		t.Fatalf("initial Workass history seed = %#v", effects[0])
	}
	state, _ = apply(t, state, TurnAdmitted{
		OperationID: "limited-op", Accepted: true,
		Turn: provider.TurnRef{OperationID: "limited-op", NativeID: "limited-turn"},
	})
	state, _ = apply(t, state, InputConsumed{OperationID: "limited-op"})
	lane := state.Lanes[limited.ID]
	if lane.InitialSeedPending || lane.CoveredThrough != 3 ||
		lane.Coverage[1].Status != CoverageSeeded || lane.Coverage[2].Status != CoverageSeeded ||
		lane.Coverage[3].Status != CoverageNativeSeen {
		t.Fatalf("consumed initial seed did not advance exact coverage: %#v", lane)
	}
}

func TestSelectionRevivesBlockedLaneThatNeverConsumedInput(t *testing.T) {
	state, _ := NewState("chat")
	source := testLane("chat", "source")
	target := testLane("chat", "qwen")
	state, _ = apply(t, state, SelectLane{Identity: source})
	state, _ = apply(t, state, LaneOpened{
		LaneID: source.ID, Thread: provider.ThreadRef{ProviderID: "source", RootID: "source-thread", HeadID: "source-thread", Lineage: 1},
		ConnectionGeneration: 1, Context: exactContext(provider.ContextImportNonSampling),
	})
	state, _ = apply(t, state, Submit{OperationID: "source-op", Text: "existing history", Presentation: provider.TurnPresentation{Origin: "human"}})
	state, _ = apply(t, state, TurnCompleted{OperationID: "source-op", Assistant: "existing answer"})

	state, _ = apply(t, state, SelectLane{Identity: target})
	state, _ = apply(t, state, LaneOpened{
		LaneID: target.ID, Thread: provider.ThreadRef{ProviderID: "qwen", RootID: "unused-thread", HeadID: "unused-thread", Lineage: 1},
		ConnectionGeneration: 1, Context: exactContext(provider.ContextImportUnsupported),
		Attachment: &provider.LaneAttachmentSnapshot{ConnectionID: "unused-thread", ProviderID: "qwen"},
	})
	blocked := state.Lanes[target.ID]
	blocked.InitialSeedPending = false
	blocked.Phase = LaneBlocked
	blocked.LastError = provider.ErrorUnsupportedCapability
	state.Lanes[target.ID] = blocked
	if err := state.Validate(); err != nil {
		t.Fatalf("blocked unconsumed lane fixture: %v", err)
	}

	state, effects := apply(t, state, SelectLane{Identity: target})
	if len(effects) != 0 || state.Lanes[target.ID].Phase != LaneReady || !state.Lanes[target.ID].InitialSeedPending {
		t.Fatalf("never-used blocked lane was not revived in place: lane=%#v effects=%#v", state.Lanes[target.ID], effects)
	}
	state, effects = apply(t, state, Submit{OperationID: "qwen-op", Text: "use the old chat", Presentation: provider.TurnPresentation{Origin: "human"}})
	if len(effects) != 1 {
		t.Fatalf("revived lane emitted %d effects, want one seeded turn: %#v", len(effects), effects)
	}
	start, ok := effects[0].(StartTurnEffect)
	if !ok || start.Input.InitialSeedThrough != 2 || len(start.Seed.Messages) != 2 ||
		state.Lanes[target.ID].Thread.RootID != "unused-thread" {
		t.Fatalf("revived lane did not keep its exact thread and seed history: lane=%#v effects=%#v", state.Lanes[target.ID], effects)
	}
}

func TestDeferredFreshProviderSeedsBeforeItsThreadIsCommitted(t *testing.T) {
	state, _ := NewState("chat")
	source := testLane("chat", "source")
	target := testLane("chat", "codex")
	state, _ = apply(t, state, SelectLane{Identity: source})
	state, _ = apply(t, state, LaneOpened{
		LaneID: source.ID, Thread: provider.ThreadRef{ProviderID: "source", RootID: "source-thread", HeadID: "source-thread", Lineage: 1},
		ConnectionGeneration: 1, Context: exactContext(provider.ContextImportNonSampling),
	})
	state, _ = apply(t, state, Submit{OperationID: "source-op", Text: "history before codex", Presentation: provider.TurnPresentation{Origin: "human"}})
	state, _ = apply(t, state, TurnCompleted{OperationID: "source-op", Assistant: "source answer"})

	creation := provider.CreationCapabilities{DeferredUntilInput: true}
	state, _ = apply(t, state, SelectLane{Identity: target, Creation: creation})
	state, effects := apply(t, state, Submit{OperationID: "codex-op", Text: "continue with codex", Presentation: provider.TurnPresentation{Origin: "human"}})
	if len(effects) != 1 {
		t.Fatalf("deferred target did not request creation: %#v", effects)
	}
	state, effects = apply(t, state, LaneProvisioned{
		LaneID: target.ID, Identity: target,
		Candidate:            provider.ThreadRef{ProviderID: "codex", RootID: "candidate", HeadID: "candidate", Lineage: 1},
		ConnectionGeneration: 1, Context: exactContext(provider.ContextImportUnsupported),
		Delivery: provider.DeliveryCapabilities{StableInputIdentity: true, ConsumptionReceipt: true},
		Creation: creation,
	})
	if len(effects) != 1 {
		t.Fatalf("deferred target did not emit its first seeded turn: %#v", effects)
	}
	start, ok := effects[0].(StartTurnEffect)
	if !ok || len(start.Seed.Messages) != 2 || start.Input.InitialSeedThrough != 2 || state.Lanes[target.ID].Provision == nil {
		t.Fatalf("deferred first turn lost history or candidate ownership: state=%#v effect=%#v", state.Lanes[target.ID], effects[0])
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
	state, _ = apply(t, state, Submit{OperationID: "same", Text: "once", Presentation: provider.TurnPresentation{Origin: "human"}})
	if _, _, err := Reduce(state, Submit{OperationID: "same", Text: "twice", Presentation: provider.TurnPresentation{Origin: "human"}}); err == nil {
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
	state, _ = apply(t, state, Submit{OperationID: "source", Text: "question", Presentation: provider.TurnPresentation{Origin: "human"}})
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
	state, _ = apply(t, state, Submit{OperationID: "source-turn", Text: "question", Presentation: provider.TurnPresentation{Origin: "human"}})
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
	state, _ = apply(t, state, Submit{OperationID: "large", Text: strings.Repeat("x", 2048), Presentation: provider.TurnPresentation{Origin: "human"}})
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
