package chat

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"workass/internal/provider"
)

type admissionCountingStore struct {
	FileStore
	writes int
}

func (s *admissionCountingStore) Save(state State) error { s.writes++; return s.FileStore.Save(state) }

func admissionHistoryFixture(tb testing.TB) (*Engine, TurnAdmitted) {
	tb.Helper()
	e, err := NewEngine("admission-history")
	if err != nil {
		tb.Fatal(err)
	}
	apply := func(c Command) {
		tb.Helper()
		if err := e.Apply(c); err != nil {
			tb.Fatal(err)
		}
	}
	rows := make([]LedgerEvent, 140)
	for i := range rows {
		rows[i] = LedgerEvent{EventID: fmt.Sprintf("history-%d", i), MessageID: fmt.Sprintf("message-%d", i), OperationID: provider.OperationID(fmt.Sprintf("operation-%d", i)), Role: "assistant", Text: strings.Repeat("history body ", 2700), Status: "done"}
	}
	apply(InitializeFork{Presentation: PresentationState{TabID: "admission-tab"}, SourceChatID: "synthetic-source", OperationID: "fixture", Digest: "synthetic", Messages: rows})
	lane := testLane("admission-history", "devin")
	apply(SelectLane{Identity: lane})
	if _, ok, err := e.ClaimNext(); err != nil || !ok {
		tb.Fatalf("create: %v %v", ok, err)
	}
	apply(LaneOpened{LaneID: lane.ID, Thread: provider.ThreadRef{ProviderID: lane.Realm.ProviderID, RootID: "fixture-thread", HeadID: "fixture-thread", Lineage: 1}, ConnectionGeneration: 1, Context: exactContext(provider.ContextImportNonSampling), Delivery: provider.DeliveryCapabilities{StableInputIdentity: true, ConsumptionReceipt: true}})
	apply(Submit{OperationID: "current-turn", Text: "question", Presentation: provider.TurnPresentation{UserMessageID: "current-user", AssistantMessageID: "current-assistant", Origin: "human"}})
	if _, ok, err := e.ClaimNext(); err != nil || !ok {
		tb.Fatalf("start: %v %v", ok, err)
	}
	receipt := TurnAdmitted{OperationID: "current-turn", Turn: provider.TurnRef{OperationID: "current-turn", NativeID: "fixture-turn"}, Accepted: true}
	apply(receipt)
	return e, receipt
}

func TestDuplicateTurnAdmissionDoesNotRewriteHistory(t *testing.T) {
	e, receipt := admissionHistoryFixture(t)
	store := &admissionCountingStore{FileStore: FileStore{Path: filepath.Join(t.TempDir(), "actor.json")}}
	if err := store.Save(e.Snapshot()); err != nil {
		t.Fatal(err)
	}
	e.store = store
	revision := e.state.Revision
	if err := e.Apply(receipt); err != nil {
		t.Fatal(err)
	}
	if store.writes != 1 || e.state.Revision != revision {
		t.Fatalf("duplicate receipt rewrote history: writes=%d revision=%d, want 1/%d", store.writes, e.state.Revision, revision)
	}
	if allocations := testing.AllocsPerRun(5, func() {
		if err := e.Apply(receipt); err != nil {
			t.Fatal(err)
		}
	}); allocations > 1 {
		t.Fatalf("duplicate admission copied history: %.0f allocations", allocations)
	}
	// Local preparation is explicitly requested work and cannot be bypassed.
	prepared := false
	if err := e.ApplyPrepared(receipt, func() error { prepared = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if !prepared || store.writes != 2 {
		t.Fatal("admission fast path suppressed explicit preparation")
	}
}

func TestAdmissionFastPathRequiresExactRunningReceipt(t *testing.T) {
	for _, kind := range []string{"different-operation", "rejected", "ambiguous", "different-turn", "deleted"} {
		t.Run(kind, func(t *testing.T) {
			e, receipt := admissionHistoryFixture(t)
			switch kind {
			case "different-operation":
				receipt.OperationID = "other"
			case "rejected":
				receipt.Accepted = false
			case "ambiguous":
				receipt.Ambiguous = true
			case "different-turn":
				receipt.Turn.NativeID = "other-turn"
			case "deleted":
				e.state.Deleted = true
			}
			expected, _, expectedErr := Reduce(e.state, receipt)
			err := e.Apply(receipt)
			if (err == nil) != (expectedErr == nil) {
				t.Fatalf("fast path changed rejection: got %v, want %v", err, expectedErr)
			}
			if err == nil && e.state.Revision != expected.Revision {
				t.Fatal("non-identical admission was skipped")
			}
		})
	}
}

func BenchmarkDuplicateTurnAdmissionLargeHistory(b *testing.B) {
	e, receipt := admissionHistoryFixture(b)
	store := FileStore{Path: filepath.Join(b.TempDir(), "actor.json")}
	if err := store.Save(e.state); err != nil {
		b.Fatal(err)
	}
	e.store = store
	b.Run("previous-full-save", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			next, _, err := Reduce(e.state, receipt)
			if err != nil {
				b.Fatal(err)
			}
			if err := store.Save(next); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("committed-receipt", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			if err := e.Apply(receipt); err != nil {
				b.Fatal(err)
			}
		}
	})
}
