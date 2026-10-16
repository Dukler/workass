package chat

import (
	"testing"

	"workass/internal/provider"
)

// A subsession is a permanent child chat (user 2026-10-16). Its parent link is
// fixed at creation; a fresh one is an ordinary new chat, a forked one comes
// only from fork initialization of its own parent.
func TestSubsessionParentLinkIsFixedAtCreation(t *testing.T) {
	state, _ := NewState("child")
	state, _ = apply(t, state, InitializeChat{
		Presentation: PresentationState{TabID: "child-tab"}, OperationID: "create", Digest: "d",
		Parent: &ParentLink{ChatID: " parent ", TabID: "parent-tab"},
	})
	if state.Parent == nil || state.Parent.ChatID != "parent" || state.Parent.TabID != "parent-tab" || state.Parent.Forked {
		t.Fatalf("fresh subsession parent = %#v", state.Parent)
	}
	clone := state.Clone()
	clone.Parent.ChatID = "mutated"
	if state.Parent.ChatID != "parent" {
		t.Fatal("State.Clone shares the parent link")
	}

	for name, parent := range map[string]*ParentLink{
		"self":            {ChatID: "child", TabID: "t"},
		"no tab":          {ChatID: "parent"},
		"forked as fresh": {ChatID: "parent", TabID: "t", Forked: true, ForkSourceSessionID: "s"},
		"fresh with src":  {ChatID: "parent", TabID: "t", ForkSourceSessionID: "s"},
	} {
		fresh, _ := NewState("child")
		if _, _, err := Reduce(fresh, InitializeChat{Presentation: PresentationState{TabID: "child-tab"}, OperationID: "create", Digest: "d", Parent: parent}); err == nil {
			t.Fatalf("%s: invalid parent link accepted", name)
		}
	}
}

func TestForkedSubsessionMustForkItsOwnParentNatively(t *testing.T) {
	rows := []LedgerEvent{{EventID: "e1", MessageID: "m1", Role: "user", Text: "hi", Status: "done", OperationID: "op1"}}
	fork := func(parent *ParentLink) error {
		state, _ := NewState("child")
		_, _, err := Reduce(state, InitializeFork{
			Presentation: PresentationState{TabID: "child-tab"}, SourceChatID: "source",
			Messages: rows, OperationID: "fork", Digest: "d", Parent: parent,
		})
		return err
	}
	if err := fork(&ParentLink{ChatID: "source", TabID: "source-tab", Forked: true, ForkSourceSessionID: "native-head"}); err != nil {
		t.Fatalf("native forked subsession rejected: %v", err)
	}
	if err := fork(&ParentLink{ChatID: "source", TabID: "source-tab"}); err == nil {
		t.Fatal("a contextless copy was accepted as a subsession")
	}
	if err := fork(&ParentLink{ChatID: "other", TabID: "t", Forked: true, ForkSourceSessionID: "native-head"}); err == nil {
		t.Fatal("a forked subsession was linked to a chat it did not fork")
	}
	if err := fork(nil); err != nil {
		t.Fatalf("the original top-level fork broke: %v", err)
	}
}

// The forked lane already holds the copied prefix natively, so binding it
// covers the prefix and the first turn sends no seed and no delta.
func TestForkedSubsessionLaneNeverResendsTheInheritedPrefix(t *testing.T) {
	state, _ := NewState("child")
	state, _ = apply(t, state, InitializeFork{
		Presentation: PresentationState{TabID: "child-tab"}, SourceChatID: "source",
		OperationID: "fork", Digest: "d",
		Parent: &ParentLink{ChatID: "source", TabID: "source-tab", Forked: true, ForkSourceSessionID: "native-head"},
		Messages: []LedgerEvent{
			{EventID: "e1", MessageID: "m1", Role: "user", Text: "question", Status: "done", OperationID: "op1"},
			{EventID: "e2", MessageID: "m2", Role: "assistant", Text: "answer", Status: "done", OperationID: "op1"},
		},
	})
	lane := testLane("child", "claude")
	thread := provider.ThreadRef{ProviderID: "claude", RootID: "forked-session", HeadID: "forked-session", Lineage: 1}
	state, _ = apply(t, state, BindEstablishedLane{
		Identity: lane, Thread: thread, Owner: provider.AttachmentOwner{TabID: "child-tab"},
		CWD: "/workspace", Context: exactContext(provider.ContextImportUnsupported),
	})
	bound := state.Lanes[lane.ID]
	if bound.CoveredThrough != 2 || len(bound.Coverage) != 2 || bound.Coverage[1].Status != CoverageNativeSeen || bound.Coverage[2].Status != CoverageNativeSeen {
		t.Fatalf("forked lane coverage = through %d, %#v", bound.CoveredThrough, bound.Coverage)
	}
	if batch, err := buildContextDelta(state, bound); err != nil || len(batch.Messages) != 0 {
		t.Fatalf("forked lane would resend the inherited prefix: %#v err=%v", batch.Messages, err)
	}
}

// Review wa-subagent-1792181911547-3, correction 2: the inherited rows are
// native only to the pinned fork. Another engine the child switches to gets
// them as ordinary catch-up history, never a silent contextless lane.
func TestForkedSubsessionInheritedHistoryReachesOtherEngines(t *testing.T) {
	state, _ := NewState("child")
	state, _ = apply(t, state, InitializeFork{
		Presentation: PresentationState{TabID: "child-tab"}, SourceChatID: "source",
		OperationID: "fork", Digest: "d",
		Parent: &ParentLink{ChatID: "source", TabID: "source-tab", Forked: true, ForkSourceSessionID: "head", ForkedSessionID: "forked-session"},
		Messages: []LedgerEvent{
			{EventID: "e1", MessageID: "m1", Role: "user", Text: "remember AZUL", Status: "done", OperationID: "op1", LaneID: "src-lane", ProviderID: "claude"},
			{EventID: "e2", MessageID: "m2", Role: "assistant", Text: "ok", Status: "done", OperationID: "op1", LaneID: "src-lane", ProviderID: "claude"},
		},
	})
	if state.ContextFloor != 0 || state.NativeForkThrough != 2 || !state.Ledger[0].Inherited || state.Ledger[0].ContextExcluded || state.Ledger[0].LaneID != "" {
		t.Fatalf("inherited rows = floor %d native %d rows %#v", state.ContextFloor, state.NativeForkThrough, state.Ledger)
	}
	// Any other thread is not the pinned fork: nothing counts as already seen.
	other := testLane("child", "claude")
	state, _ = apply(t, state, BindEstablishedLane{
		Identity: other, Thread: provider.ThreadRef{ProviderID: "claude", RootID: "some-other-session", HeadID: "some-other-session", Lineage: 1},
		Owner: provider.AttachmentOwner{TabID: "child-tab"}, CWD: "/workspace", Context: exactContext(provider.ContextImportUnsupported),
	})
	if lane := state.Lanes[other.ID]; len(lane.Coverage) != 0 {
		t.Fatalf("a lane that is not the pinned fork claimed the inherited rows: %#v", lane.Coverage)
	}
	if batch, err := buildContextDelta(state, state.Lanes[other.ID]); err != nil || len(batch.Messages) != 2 {
		t.Fatalf("another engine must receive the inherited rows: %#v err=%v", batch.Messages, err)
	}
	codex := testLane("child", "codex")
	state, _ = apply(t, state, SelectLane{Identity: codex, Owner: provider.AttachmentOwner{TabID: "child-tab"}, CWD: "/workspace"})
	if lane := state.Lanes[codex.ID]; lane.CoveredThrough != 0 || !lane.InitialSeedPending {
		t.Fatalf("a new engine starts uncovered with a pending seed: through %d seed %v", lane.CoveredThrough, lane.InitialSeedPending)
	}
	if seed, err := buildInitialSeedBatch(state, 0, state.LedgerHead()); err != nil || len(seed.Messages) != 2 {
		t.Fatalf("the seed must carry the inherited rows: %#v err=%v", seed.Messages, err)
	}
}

// Review wa-subagent-1792184734224-6, correction 3: every user row a Workass
// wake-up notice turn can leave — consumed, rejected, uncertain, cancelled —
// keeps the notice, so none is ever drawn as the user speaking.
func TestNoticeTurnRowsKeepTheNoticeOnEveryOutcome(t *testing.T) {
	notice := func(id string) provider.TurnPresentation {
		return provider.TurnPresentation{
			Origin: "agent", UserMessageID: "user-" + id, AssistantMessageID: "assistant-" + id, PromptText: id,
			NoticeKind: "subagent", NoticeTitle: "Revisión", NoticeStatus: "done",
		}
	}
	ready := func() (State, provider.LaneIdentity) {
		state, _ := NewState("chat")
		lane := testLane("chat", "alpha")
		state, _ = apply(t, state, SelectLane{Identity: lane})
		state, _ = apply(t, state, LaneOpened{
			LaneID: lane.ID, Thread: provider.ThreadRef{ProviderID: "alpha", RootID: "thread", HeadID: "thread", Lineage: 1},
			ConnectionGeneration: 1, Context: exactContext(provider.ContextImportNonSampling),
			Delivery: provider.DeliveryCapabilities{StableInputIdentity: true},
		})
		return state, lane
	}
	userRow := func(state State, id provider.OperationID) LedgerEvent {
		for _, event := range state.Ledger {
			if event.OperationID == id && event.Role == "user" {
				return event
			}
		}
		t.Fatalf("no user row for %s in %#v", id, state.Ledger)
		return LedgerEvent{}
	}
	check := func(name string, row LedgerEvent, terminal string) {
		if row.TerminalState != terminal || row.NoticeKind != "subagent" || row.NoticeTitle != "Revisión" || row.NoticeStatus != "done" {
			t.Fatalf("%s: row = %#v", name, row)
		}
	}

	state, _ := ready()
	state, _ = apply(t, state, Submit{OperationID: "head", Text: "head", Presentation: provider.TurnPresentation{Origin: "human"}})
	state, _ = apply(t, state, Submit{OperationID: "queued", Text: "<task-notification/>", Presentation: notice("queued")})
	state, _ = apply(t, state, CancelPendingTurn{OperationID: "queued"})
	check("cancelled before admission", userRow(state, "queued"), "cancelled_before_admission")

	state, _ = ready()
	state, _ = apply(t, state, Submit{OperationID: "uncertain", Text: "<task-notification/>", Presentation: notice("uncertain")})
	state, _ = apply(t, state, TurnAdmitted{OperationID: "uncertain", Ambiguous: true})
	check("delivery uncertain", userRow(state, "uncertain"), "delivery_uncertain")

	state, _ = ready()
	state, _ = apply(t, state, Submit{OperationID: "rejected", Text: "<task-notification/>", Presentation: notice("rejected")})
	state, _ = apply(t, state, TurnAdmissionFailed{OperationID: "rejected", Kind: provider.ErrorProviderUnavailable, Reason: "auth expired"})
	check("admission rejected", userRow(state, "rejected"), "admission_rejected")
}
