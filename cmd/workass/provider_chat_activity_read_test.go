package main

import (
	"fmt"
	"testing"

	"workass/internal/acp"
	"workass/internal/chat"
	providercontract "workass/internal/provider"
)

func activityReadFixture(tb testing.TB) *providerChatRuntime {
	tb.Helper()
	stateDir := tb.TempDir()
	engine, err := chat.NewEngine("activity-chat")
	if err != nil {
		tb.Fatal(err)
	}
	rows := make([]chat.LedgerEvent, 1000)
	for i := range rows {
		rows[i] = chat.LedgerEvent{EventID: fmt.Sprintf("event-%d", i), MessageID: fmt.Sprintf("message-%d", i), OperationID: "original", Role: "assistant", Text: "history body", Status: "done", Timeline: make([]chat.TimelineEntry, 10)}
		for j := range rows[i].Timeline {
			rows[i].Timeline[j] = chat.TimelineEntry{Key: fmt.Sprintf("tool-%d", j), Kind: providercontract.EventToolUpdate, Tool: &providercontract.ToolEvent{ToolCallID: fmt.Sprintf("call-%d-%d", i, j), Title: "historic tool"}}
		}
	}
	if err := engine.Apply(chat.InitializeFork{Presentation: chat.PresentationState{TabID: "activity-tab"}, SourceChatID: "source", OperationID: "create", Digest: "fixture", Messages: rows}); err != nil {
		tb.Fatal(err)
	}
	return &providerChatRuntime{manager: &acp.Manager{}, sessions: sharedSessionStore(stateDir), stateDir: stateDir, actors: map[string]*providerChatActor{"activity-chat": {engine: engine}}, known: map[string]struct{}{"activity-chat": {}}}
}

func BenchmarkActivityReadsWithLargeHistory(b *testing.B) {
	r := activityReadFixture(b)
	b.Run("previous-full-snapshot", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			_ = r.actors["activity-chat"].engine.Snapshot()
		}
	})

	b.Run("background", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			if _, err := r.ListBackground("activity-tab", "activity-chat"); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("obligation", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			if _, err := r.Obligation("activity-tab", "activity-chat"); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("permissions", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			if _, err := r.PendingPermissions(); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func TestActivityReadsDoNotCopyHistoryAndStillRejectWrongOwner(t *testing.T) {
	r := activityReadFixture(t)
	allocs := testing.AllocsPerRun(5, func() {
		items, err := r.ListBackground("activity-tab", "activity-chat")
		if err != nil || len(items) != 0 {
			t.Fatal("background read failed")
		}
		obligation, err := r.Obligation("activity-tab", "activity-chat")
		if err != nil || obligation != nil {
			t.Fatal("obligation read failed")
		}
		permissions, err := r.PendingPermissions()
		if err != nil || len(permissions) != 0 {
			t.Fatal("permission read failed")
		}
	})
	if allocs > 40 {
		t.Fatalf("quiet activity reads allocated %.0f objects for unrelated transcript history", allocs)
	}
	if _, err := r.ListBackground("wrong-tab", "activity-chat"); err == nil {
		t.Fatal("wrong tab was accepted")
	}
	if _, err := r.Obligation("", "activity-chat"); err == nil {
		t.Fatal("missing tab was accepted")
	}
	if got := len(r.actors["activity-chat"].engine.Snapshot().Ledger); got != 1000 {
		t.Fatal("activity read changed canonical history")
	}
	if _, err := r.ReadBackground("wrong-tab", "activity-chat", "missing", 100); err == nil {
		t.Fatal("output read accepted wrong tab")
	}
	if err := r.actors["activity-chat"].engine.Apply(chat.DeleteChat{OperationID: "delete"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ListBackground("activity-tab", "activity-chat"); err == nil {
		t.Fatal("deleted chat was accepted")
	}
	if _, err := r.Obligation("activity-tab", "activity-chat"); err == nil {
		t.Fatal("deleted chat obligation was accepted")
	}

}
