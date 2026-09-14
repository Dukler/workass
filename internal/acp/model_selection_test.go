package acp

import (
	"context"
	"errors"
	"testing"
	"time"

	providercontract "workass/internal/provider"
)

func TestClosedModelSelectValidation(t *testing.T) {
	for _, tc := range []struct {
		name, provider, model string
		raw                   any
		missing               bool
	}{
		{"removed", "devin", "removed", []any{map[string]any{"value": "offered"}}, true},
		{"offered", "devin", "offered", []any{map[string]any{"value": "offered"}}, false},
		{"empty authoritative", "devin", "removed", []any{}, true},
		{"unknown", "devin", "custom", nil, false},
		{"malformed", "devin", "custom", []any{map[string]any{"name": "incomplete"}}, false},
		{"open provider", "custom", "custom", []any{map[string]any{"value": "offered"}}, false},
		{"no explicit selection", "devin", "", []any{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &Bridge{providerID: tc.provider, modelSelectValues: completeModelSelectValues(tc.raw)}
			if got := providercontract.ErrorIs(b.validateModelSelection(tc.model), providercontract.ErrorModelUnavailable); got != tc.missing {
				t.Fatalf("unavailable=%v, want %v", got, tc.missing)
			}
		})
	}
}

func TestModelSelectionRPCDisappearance(t *testing.T) {
	missing := &acpError{Code: -32002, Msg: "Resource not found", Data: map[string]any{"uri": "Model not found: removed. Available models: private"}}
	classified := classifyModelSelectionError(missing)
	if !providercontract.ErrorIs(classified, providercontract.ErrorModelUnavailable) || !errors.Is(classified, missing) {
		t.Fatalf("lost model rejection: %v", classified)
	}
	unrelated := &acpError{Code: -32002, Msg: "Resource not found", Data: map[string]any{"uri": "Session not found: private"}}
	if classifyModelSelectionError(unrelated) != unrelated {
		t.Fatal("unrelated resource was reclassified")
	}
}

func TestRemovedModelOnResumedDevinLaneNeverDispatchesPrompt(t *testing.T) {
	f := newPersistentMockFixture(t, "resume")
	m, events := f.newManagerTuned(func(opts *Options) { opts.Provider.ID = "devin" })
	t.Cleanup(func() { m.Reset() })
	session, err := m.NewSession(context.Background(), SessionOptions{TabID: "native-tab", ChatID: "native-chat", ProviderID: "devin"})
	if err != nil {
		t.Fatal(err)
	}
	job, err := m.StartJob(context.Background(), JobStartOptions{Kind: "app-chat", SessionID: session.SessionID, TabID: "native-tab", ChatID: "native-chat", ProviderID: "devin", Prompt: "establish native history"})
	if err != nil {
		t.Fatal(err)
	}
	events.waitJobEnd(t, jobID(job), 5*time.Second)
	// This transport fixture bypasses the actor; materialize its observed first-input receipt.
	if _, ok := m.nativeSessions.commitThread("native-tab", "native-chat", "devin", session.SessionID); !ok {
		t.Fatal("commit fixture native history")
	}
	binding, ok := m.nativeSessions.get("native-tab", "native-chat", "devin")
	if !ok {
		t.Fatal("missing binding")
	}
	lane, err := (managerLaneFactory{manager: m, providerID: "devin"}).Resume(context.Background(), providercontract.ResumeLaneRequest{
		Identity: bindingLaneIdentity(binding), Thread: bindingThreadRef(binding), Owner: providercontract.AttachmentOwner{TabID: "native-tab"}, CWD: binding.CWD,
	})
	if err != nil {
		t.Fatal(err)
	}
	managed := lane.(*managerLane)
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for event := range lane.Events() {
			managed.AcknowledgeDurableEvent(event.Identity.Sequence, nil)
		}
	}()
	t.Cleanup(func() { _ = lane.Detach(context.Background()); <-drained })
	// The selected id equals the resumed currentValue but is absent from the
	// complete model list: checking only when SetModel runs would miss this.
	selected := stringPointer(session.CurrentModelID)
	_, err = lane.Delivery().StartTurn(context.Background(), providercontract.TurnInput{OperationID: "removed-model", Text: "MUST_NOT_DISPATCH_REMOVED_MODEL", ModelID: selected})
	if !providercontract.ErrorIs(err, providercontract.ErrorModelUnavailable) {
		t.Fatalf("removed model admission: %v", err)
	}
	if traceContains(readNativeMockTrace(t, f.traceFile), "MUST_NOT_DISPATCH_REMOVED_MODEL") {
		t.Fatal("removed model dispatched a prompt")
	}
	if persistentMockSessionCount(t, f.sessionFile) != 1 {
		t.Fatal("model rejection replaced native thread")
	}
	// A valid explicit selection remains usable on the same connection/thread.
	if _, err = m.SetModel(context.Background(), session.SessionID, "mock-deterministic[high]"); err != nil {
		t.Fatalf("valid selection failed: %v", err)
	}
	if persistentMockSessionCount(t, f.sessionFile) != 1 {
		t.Fatal("model change replaced native thread")
	}
}
