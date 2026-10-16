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
	if bound.CoveredThrough != 2 || len(bound.Coverage) != 2 || bound.Coverage[1].Status != CoverageExcluded {
		t.Fatalf("forked lane coverage = through %d, %#v", bound.CoveredThrough, bound.Coverage)
	}
	if batch, err := buildContextDelta(state, bound); err != nil || len(batch.Messages) != 0 {
		t.Fatalf("forked lane would resend the inherited prefix: %#v err=%v", batch.Messages, err)
	}
}
