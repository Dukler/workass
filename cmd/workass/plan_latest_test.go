package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"workass/internal/httpserve"
	"workass/internal/wire"
)

func TestPlanLatestRecordJobEventSetsPlanAndOwner(t *testing.T) {
	store := newPlanLatestTurnStore(t, "record-tab", "record-chat", "record plan", "record-assistant")
	store.RecordJobEvent("job:event", map[string]any{
		"type": "acp", "id": "record-job",
		"event": map[string]any{"kind": "plan", "entries": []any{
			map[string]any{"status": "in_progress", "content": "Inspect the store", "ignored": "not durable"},
			map[string]any{"status": "pending", "content": "Persist the plan"},
		}},
	})

	chat := chatFromSnapshot(store.Get().(map[string]any), "record-tab")
	assertPlanLatest(t, chat, "record-assistant", []planLatestWant{
		{status: "in_progress", content: "Inspect the store"},
		{status: "pending", content: "Persist the plan"},
	})
}

func TestPlanLatestEmptyEventPersistsExplicitEmptySlice(t *testing.T) {
	store := newPlanLatestTurnStore(t, "empty-tab", "empty-chat", "clear plan", "empty-assistant")
	store.RecordJobEvent("job:event", map[string]any{
		"type": "acp", "id": "empty-job",
		"event": map[string]any{"kind": "plan", "entries": []any{}},
	})

	chat := chatFromSnapshot(store.Get().(map[string]any), "empty-tab")
	assertPlanLatest(t, chat, "empty-assistant", nil)
}

func TestPlanLatestJournalReplayReconstructsAfterRestart(t *testing.T) {
	stateDir := t.TempDir()
	path := filepath.Join(stateDir, sessionStateFilename)
	store := newSessionStore(path)
	if !store.Save(sessionMirrorFixture("journal-plan-tab", "journal-plan-chat", "recover plan")) {
		t.Fatal("save journal plan fixture")
	}
	store.PrepareTurn(map[string]any{
		"tabId": "journal-plan-tab", "chatId": "journal-plan-chat", "prompt": "recover plan",
		"assistantMessageId": "journal-plan-assistant",
	})
	store.RecordJobEvent("job:event", map[string]any{
		"type": "start", "job": map[string]any{
			"id": "journal-plan-job", "tabId": "journal-plan-tab", "chatId": "journal-plan-chat",
		},
	})
	store.RecordJobEvent("job:event", map[string]any{
		"type": "acp", "id": "journal-plan-job",
		"event": map[string]any{"kind": "plan", "entries": []any{
			map[string]any{"status": "in_progress", "content": "Recover from the journal"},
		}},
	})
	store.flushScheduledWrite()
	closeSessionStoreJournals(t, store)

	reloaded := newSessionStore(path)
	if err := reloaded.LoadError(); err != nil {
		t.Fatalf("reload journal plan: %v", err)
	}
	chat := chatFromSnapshot(reloaded.Get().(map[string]any), "journal-plan-tab")
	assertPlanLatest(t, chat, "journal-plan-assistant", []planLatestWant{
		{status: "in_progress", content: "Recover from the journal"},
	})
}

func TestPlanLatestPrepareTurnClearsCompletedPlanForNewAssistant(t *testing.T) {
	store := newSessionStore(filepath.Join(t.TempDir(), sessionStateFilename))
	snapshot := sessionMirrorFixture("prepare-plan-tab", "prepare-plan-chat", "old turn")
	chat := chatFromSnapshot(snapshot, "prepare-plan-tab")
	oldAssistant := mapFromAnyMain(messageSlice(chat)[1])
	oldAssistant["status"] = "done"
	oldAssistant["at"] = "2026-07-23T10:00:00Z"
	chat["planLatest"] = []any{
		map[string]any{"status": "completed", "content": "Finish old work"},
	}
	chat["planLatestMessageId"] = "client-a"
	if !store.Save(snapshot) {
		t.Fatal("save completed plan fixture")
	}

	store.PrepareTurn(map[string]any{
		"tabId": "prepare-plan-tab", "chatId": "prepare-plan-chat", "prompt": "new turn",
		"assistantMessageId": "prepare-plan-new-assistant",
	})

	got := chatFromSnapshot(store.Get().(map[string]any), "prepare-plan-tab")
	assertPlanLatest(t, got, "prepare-plan-new-assistant", nil)
}

func TestPlanLatestMergeOverlaysDaemonPlanIncludingEmpty(t *testing.T) {
	store := newPlanLatestTurnStore(t, "merge-plan-tab", "merge-plan-chat", "merge plan", "merge-plan-assistant")
	stale := cloneJSON(store.Get()).(map[string]any)
	staleChat := chatFromSnapshot(stale, "merge-plan-tab")
	staleChat["planLatest"] = []any{
		map[string]any{"status": "pending", "content": "Stale renderer plan"},
	}
	staleChat["planLatestMessageId"] = "stale-assistant"

	store.RecordJobEvent("job:event", map[string]any{
		"type": "acp", "id": "merge-plan-job",
		"event": map[string]any{"kind": "plan", "entries": []any{
			map[string]any{"status": "in_progress", "content": "Daemon plan"},
		}},
	})
	if !store.Save(stale) {
		t.Fatal("save stale renderer plan")
	}
	assertPlanLatest(t, chatFromSnapshot(store.Get().(map[string]any), "merge-plan-tab"), "merge-plan-assistant", []planLatestWant{
		{status: "in_progress", content: "Daemon plan"},
	})

	store.RecordJobEvent("job:event", map[string]any{
		"type": "acp", "id": "merge-plan-job",
		"event": map[string]any{"kind": "plan", "entries": []any{}},
	})
	if !store.Save(stale) {
		t.Fatal("save stale renderer plan after daemon clear")
	}
	assertPlanLatest(t, chatFromSnapshot(store.Get().(map[string]any), "merge-plan-tab"), "merge-plan-assistant", nil)
}

func TestPlanLatestLoadMigrationDerivesFromMessagesAndArchive(t *testing.T) {
	stateDir := t.TempDir()
	path := filepath.Join(stateDir, sessionStateFilename)
	snapshot := map[string]any{
		"v": json.Number("1"), "activeId": "migrate-active-tab", "seq": json.Number("1"),
		"theme": "dark", "panes": map[string]any{}, "mode": "chats",
		"chats": []any{
			map[string]any{
				"id": "migrate-active-tab", "chatId": "migrate-active-chat", "messages": []any{
					map[string]any{"id": "active-assistant", "role": "assistant", "content": "working", "status": "running", "events": []any{
						map[string]any{"kind": "plan", "entries": []any{
							map[string]any{"status": "in_progress", "content": "Migrate active plan"},
						}},
					}},
				},
			},
			map[string]any{
				"id": "migrate-complete-tab", "chatId": "migrate-complete-chat", "messages": []any{
					map[string]any{"id": "complete-owner", "role": "assistant", "content": "done", "status": "done", "events": []any{
						map[string]any{"kind": "plan", "entries": []any{
							map[string]any{"status": "completed", "content": "Finished old plan"},
						}},
					}},
					map[string]any{"id": "later-user", "role": "user", "content": "next", "status": "done", "events": []any{}},
					map[string]any{"id": "later-assistant", "role": "assistant", "content": "next answer", "status": "done", "events": []any{}},
				},
			},
			map[string]any{
				"id": "migrate-archive-tab", "chatId": "migrate-archive-chat", "messages": []any{
					map[string]any{"id": "archive-later-assistant", "role": "assistant", "content": "current window", "status": "running", "events": []any{}},
				},
			},
		},
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal migration fixture: %v", err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatalf("write migration fixture: %v", err)
	}
	if err := appendChatArchive(stateDir, "migrate-archive-tab", []any{
		map[string]any{"id": "archive-plan-owner", "role": "assistant", "content": "archived work", "status": "done", "events": []any{
			map[string]any{"kind": "plan", "entries": []any{
				map[string]any{"status": "pending", "content": "Recover archived plan"},
			}},
		}},
	}); err != nil {
		t.Fatalf("write plan archive: %v", err)
	}

	store := newSessionStore(path)
	if err := store.LoadError(); err != nil {
		t.Fatalf("load plan migration: %v", err)
	}
	got := store.Get().(map[string]any)
	assertPlanLatest(t, chatFromSnapshot(got, "migrate-active-tab"), "active-assistant", []planLatestWant{
		{status: "in_progress", content: "Migrate active plan"},
	})
	assertPlanLatest(t, chatFromSnapshot(got, "migrate-complete-tab"), "later-assistant", nil)
	assertPlanLatest(t, chatFromSnapshot(got, "migrate-archive-tab"), "archive-plan-owner", []planLatestWant{
		{status: "pending", content: "Recover archived plan"},
	})
}

func TestWireSessionGetHydratesLatestPlan(t *testing.T) {
	stateDir := t.TempDir()
	renderer := t.TempDir()
	if err := os.WriteFile(filepath.Join(renderer, "index.html"), []byte("<!doctype html><body></body>"), 0o644); err != nil {
		t.Fatalf("write renderer fixture: %v", err)
	}
	store := newPlanLatestTurnStoreAt(t, stateDir, "wire-plan-tab", "wire-plan-chat", "wire plan", "wire-plan-assistant")
	store.RecordJobEvent("job:event", map[string]any{
		"type": "acp", "id": "wire-plan-job",
		"event": map[string]any{"kind": "plan", "entries": []any{
			map[string]any{"status": "in_progress", "content": "Hydrate first paint"},
		}},
	})

	hub := wire.NewHub()
	registerSessionHandlers(hub, store)
	server := httptest.NewServer(httpserve.New(renderer, hub, nil))
	defer server.Close()
	client := dialTestWS(t, server.URL)
	defer client.conn.Close()

	client.invoke(t, 1, "session:get")
	reply := client.waitReply(t, 1, 2*time.Second)
	if reply.Error != nil {
		t.Fatalf("session:get plan hydration: %s", *reply.Error)
	}
	chat := chatFromSnapshot(mapFromAnyMain(reply.Result), "wire-plan-tab")
	assertPlanLatest(t, chat, "wire-plan-assistant", []planLatestWant{
		{status: "in_progress", content: "Hydrate first paint"},
	})
}

type planLatestWant struct {
	status  string
	content string
}

func newPlanLatestTurnStore(t *testing.T, tabID, chatID, prompt, assistantID string) *sessionStore {
	t.Helper()
	return newPlanLatestTurnStoreAt(t, t.TempDir(), tabID, chatID, prompt, assistantID)
}

func newPlanLatestTurnStoreAt(t *testing.T, stateDir, tabID, chatID, prompt, assistantID string) *sessionStore {
	t.Helper()
	store := newSessionStore(filepath.Join(stateDir, sessionStateFilename))
	if !store.Save(sessionMirrorFixture(tabID, chatID, prompt)) {
		t.Fatal("save plan fixture")
	}
	store.PrepareTurn(map[string]any{
		"tabId": tabID, "chatId": chatID, "prompt": prompt, "assistantMessageId": assistantID,
	})
	store.RecordJobEvent("job:event", map[string]any{
		"type": "start", "job": map[string]any{
			"id": tabID[:len(tabID)-len("-tab")] + "-job", "tabId": tabID, "chatId": chatID,
		},
	})
	return store
}

func assertPlanLatest(t *testing.T, chat map[string]any, owner string, want []planLatestWant) {
	t.Helper()
	if chat == nil {
		t.Fatal("plan chat is missing")
	}
	entries, ok := chat["planLatest"].([]any)
	if !ok {
		t.Fatalf("planLatest = %#v, want an explicit slice", chat["planLatest"])
	}
	if fieldString(chat, "planLatestMessageId") != owner {
		t.Fatalf("planLatestMessageId = %q, want %q", fieldString(chat, "planLatestMessageId"), owner)
	}
	if len(entries) != len(want) {
		t.Fatalf("planLatest = %#v, want %d entries", entries, len(want))
	}
	for index, expected := range want {
		entry := mapFromAnyMain(entries[index])
		if fieldString(entry, "status") != expected.status || fieldString(entry, "content") != expected.content {
			t.Fatalf("planLatest[%d] = %#v, want status=%q content=%q", index, entry, expected.status, expected.content)
		}
		if len(entry) != 2 {
			t.Fatalf("planLatest[%d] persisted unexpected fields: %#v", index, entry)
		}
	}
}
