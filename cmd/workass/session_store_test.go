package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestSessionStoreRejectsConversationIdentityRetargeting(t *testing.T) {
	store := newSessionStore(filepath.Join(t.TempDir(), sessionStateFilename))
	if !store.Save(sessionMirrorFixture("tab-1", "chat-1", "original")) {
		t.Fatal("initial save failed")
	}
	if store.Save(sessionMirrorFixture("tab-1", "chat-2", "foreign history")) {
		t.Fatal("same tab was allowed to retarget a different conversation id")
	}
	duplicate := sessionMirrorFixture("tab-1", "chat-1", "original")
	duplicate["chats"] = append(anySlice(duplicate["chats"]), map[string]any{
		"id": "tab-2", "chatId": "chat-1", "messages": []any{},
	})
	if store.Save(duplicate) {
		t.Fatal("one conversation id was accepted under multiple tabs")
	}
	chat := chatFromSnapshot(store.Get().(map[string]any), "tab-1")
	if fieldString(chat, "chatId") != "chat-1" {
		t.Fatalf("authoritative identity changed after rejected saves: %#v", chat)
	}
}

func TestSessionStorePersistsProviderScopedContextUsageAcrossReloadAndStaleSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), sessionStateFilename)
	store := newSessionStore(path)
	initial := sessionMirrorFixture("usage-tab", "usage-chat", "measure context")
	if !store.Save(initial) {
		t.Fatal("initial save failed")
	}

	store.RecordJobEvent("job:event", map[string]any{
		"type": "usage", "tabId": "usage-tab", "chatId": "usage-chat", "providerId": "codex",
		"used": 128800, "size": 258400, "updatedAt": "2026-07-22T19:37:00Z",
	})
	assertContextUsage := func(t *testing.T, snapshot map[string]any) {
		t.Helper()
		chat := chatFromSnapshot(snapshot, "usage-tab")
		byProvider := mapFromAnyMain(chat["contextUsageByProvider"])
		usage := mapFromAnyMain(byProvider["codex"])
		if intValue(usage["used"]) != 128800 || intValue(usage["size"]) != 258400 || fieldString(usage, "updatedAt") != "2026-07-22T19:37:00Z" {
			t.Fatalf("durable context usage = %#v", usage)
		}
	}
	assertContextUsage(t, store.Get().(map[string]any))

	stale := cloneJSON(initial).(map[string]any)
	stale["_workassSave"] = "lean-payload-v2"
	if !store.Save(stale) {
		t.Fatal("stale lean save failed")
	}
	assertContextUsage(t, store.Get().(map[string]any))
	staleFull := cloneJSON(initial).(map[string]any)
	if !store.Save(staleFull) {
		t.Fatal("stale full save failed")
	}
	assertContextUsage(t, store.Get().(map[string]any))

	reloaded := newSessionStore(path)
	if err := reloaded.LoadError(); err != nil {
		t.Fatalf("reload session store: %v", err)
	}
	assertContextUsage(t, reloaded.Get().(map[string]any))

	reloaded.RecordJobEvent("job:event", map[string]any{
		"type": "usage", "tabId": "usage-tab", "chatId": "another-chat", "providerId": "codex",
		"used": 1, "size": 2, "updatedAt": "2026-07-22T19:38:00Z",
	})
	assertContextUsage(t, reloaded.Get().(map[string]any))
}

func TestSessionStoreTerminalSettlesLingeringToolEvents(t *testing.T) {
	for _, tc := range []struct {
		name       string
		jobStatus  string
		stopReason string
		want       string
	}{
		{name: "done", jobStatus: "done", want: "completed"},
		{name: "failed", jobStatus: "failed", want: "failed"},
		{name: "cancelled", jobStatus: "failed", stopReason: "cancelled", want: "cancelled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stateDir := t.TempDir()
			store := newSessionStore(filepath.Join(stateDir, sessionStateFilename))
			if !store.Save(sessionMirrorFixture("tool-tab", "tool-chat", "inspect")) {
				t.Fatal("initial save failed")
			}
			store.PrepareTurn(map[string]any{"tabId": "tool-tab", "chatId": "tool-chat", "prompt": "inspect"})
			store.RecordJobEvent("job:event", map[string]any{"type": "start", "job": map[string]any{
				"id": "tool-job", "tabId": "tool-tab", "chatId": "tool-chat", "startedAt": "2026-07-22T20:00:00Z",
			}})
			store.RecordJobEvent("job:event", map[string]any{
				"type": "data", "id": "tool-job", "stream": "stdout", "chunk": "settled",
			})
			for _, status := range []string{"in_progress", "pending", "running"} {
				store.RecordJobEvent("job:event", map[string]any{"type": "acp", "id": "tool-job", "event": map[string]any{
					"kind": "tool", "id": "tool-" + status, "title": "View image", "status": status,
				}})
			}
			store.RecordJobEvent("job:event", map[string]any{"type": "end", "job": map[string]any{
				"id": "tool-job", "tabId": "tool-tab", "chatId": "tool-chat", "status": tc.jobStatus,
				"stopReason": tc.stopReason, "finishedAt": "2026-07-22T20:00:01Z",
			}})

			assertSettled := func(t *testing.T, assistant map[string]any) {
				t.Helper()
				for _, raw := range anySlice(assistant["events"]) {
					tool := mapFromAnyMain(raw)
					if got := fieldString(tool, "status"); got != tc.want {
						t.Fatalf("terminal %s tool status = %q, want %q: %#v", tc.name, got, tc.want, tool)
					}
				}
			}
			assertSettled(t, sessionAssistant(t, store.Get().(map[string]any), "tool-tab"))

			archive := loadChatArchive(stateDir, "tool-tab")
			if len(archive) != 2 {
				t.Fatalf("terminal archive = %#v", archive)
			}
			assertSettled(t, mapFromAnyMain(archive[1]))
		})
	}
}

func TestSessionStoreReloadSettlesPreviouslyTerminalToolEvents(t *testing.T) {
	stateDir := t.TempDir()
	path := filepath.Join(stateDir, sessionStateFilename)
	snapshot := sessionMirrorFixture("stale-tool-tab", "stale-tool-chat", "inspect")
	assistant := mapFromAnyMain(messageSlice(chatFromSnapshot(snapshot, "stale-tool-tab"))[1])
	assistant["status"] = "done"
	assistant["events"] = []any{
		map[string]any{"kind": "tool", "id": "stale-view", "title": "View image", "status": "in_progress"},
		map[string]any{"kind": "tool", "id": "already-done", "title": "Read", "status": "completed"},
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal stale terminal fixture: %v", err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatalf("write stale terminal fixture: %v", err)
	}

	reloaded := newSessionStore(path)
	if err := reloaded.LoadError(); err != nil {
		t.Fatalf("reload stale terminal fixture: %v", err)
	}
	events := anySlice(sessionAssistant(t, reloaded.Get().(map[string]any), "stale-tool-tab")["events"])
	if got := fieldString(mapFromAnyMain(events[0]), "status"); got != "completed" {
		t.Fatalf("stale terminal tool status = %q, want completed", got)
	}
	if got := fieldString(mapFromAnyMain(events[1]), "status"); got != "completed" {
		t.Fatalf("already terminal tool status changed to %q", got)
	}
}

func TestSessionStoreRestartSettlesInterruptedToolEvents(t *testing.T) {
	stateDir := t.TempDir()
	path := filepath.Join(stateDir, sessionStateFilename)
	snapshot := sessionMirrorFixture("orphan-tool-tab", "orphan-tool-chat", "inspect")
	assistant := mapFromAnyMain(messageSlice(chatFromSnapshot(snapshot, "orphan-tool-tab"))[1])
	assistant["events"] = []any{
		map[string]any{"kind": "tool", "id": "orphan-view", "title": "View image", "status": "in_progress"},
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal orphan terminal fixture: %v", err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatalf("write orphan terminal fixture: %v", err)
	}

	reloaded := newSessionStore(path)
	if err := reloaded.LoadError(); err != nil {
		t.Fatalf("reload orphan terminal fixture: %v", err)
	}
	assistant = sessionAssistant(t, reloaded.Get().(map[string]any), "orphan-tool-tab")
	if got := fieldString(assistant, "status"); got != "failed" {
		t.Fatalf("interrupted turn status = %q, want failed", got)
	}
	// The sidebar tells a restart apart from an agent error by this flag alone;
	// without it every restart repaints the owning chat as "Falló".
	if assistant["interrupted"] != true {
		t.Fatalf("interrupted turn flag = %v, want true", assistant["interrupted"])
	}
	events := anySlice(assistant["events"])
	if got := fieldString(mapFromAnyMain(events[0]), "status"); got != "failed" {
		t.Fatalf("interrupted tool status = %q, want failed", got)
	}
}

func TestSessionStoreCarriesDaemonInterruptionFromJobEnd(t *testing.T) {
	stateDir := t.TempDir()
	path := filepath.Join(stateDir, sessionStateFilename)
	store := newSessionStore(path)
	if !store.Save(sessionMirrorFixture("shutdown-tab", "shutdown-chat", "shutdown prompt")) {
		t.Fatal("save shutdown fixture")
	}
	store.PrepareTurn(map[string]any{"tabId": "shutdown-tab", "chatId": "shutdown-chat", "prompt": "shutdown prompt"})
	store.RecordJobEvent("job:event", map[string]any{
		"type": "start", "job": map[string]any{"id": "shutdown-job", "tabId": "shutdown-tab", "chatId": "shutdown-chat", "status": "running"},
	})
	// A daemon restart closes every bridge, so the in-flight prompt fails and the
	// job is finalized before the process exits — the next boot's orphan sweep
	// finds nothing running to stamp. The interruption has to ride the job here.
	store.RecordJobEvent("job:event", map[string]any{
		"type": "end", "job": map[string]any{
			"id": "shutdown-job", "tabId": "shutdown-tab", "chatId": "shutdown-chat",
			"status": "failed", "interrupted": true, "stopReason": "daemon-restart",
			"finishedAt": "2026-07-25T16:13:37Z", "error": "ACP session reset",
		},
	})
	assistant := sessionAssistant(t, store.Get().(map[string]any), "shutdown-tab")
	if got := fieldString(assistant, "status"); got != "failed" {
		t.Fatalf("interrupted turn status = %q, want failed", got)
	}
	if assistant["interrupted"] != true {
		t.Fatalf("interrupted turn flag = %v, want true", assistant["interrupted"])
	}
}

func TestSessionStoreAgentFailureIsNotMarkedInterrupted(t *testing.T) {
	stateDir := t.TempDir()
	path := filepath.Join(stateDir, sessionStateFilename)
	store := newSessionStore(path)
	if !store.Save(sessionMirrorFixture("broke-tab", "broke-chat", "broke prompt")) {
		t.Fatal("save failure fixture")
	}
	store.PrepareTurn(map[string]any{"tabId": "broke-tab", "chatId": "broke-chat", "prompt": "broke prompt"})
	store.RecordJobEvent("job:event", map[string]any{
		"type": "start", "job": map[string]any{"id": "broke-job", "tabId": "broke-tab", "chatId": "broke-chat", "status": "running"},
	})
	store.RecordJobEvent("job:event", map[string]any{
		"type": "end", "job": map[string]any{
			"id": "broke-job", "tabId": "broke-tab", "chatId": "broke-chat",
			"status": "failed", "finishedAt": "2026-07-25T16:13:37Z", "error": "agent exploded",
		},
	})
	assistant := sessionAssistant(t, store.Get().(map[string]any), "broke-tab")
	if got := fieldString(assistant, "status"); got != "failed" {
		t.Fatalf("failed turn status = %q, want failed", got)
	}
	// A real agent error must still reach the sidebar as "Falló".
	if _, ok := assistant["interrupted"]; ok {
		t.Fatalf("agent failure carried interrupted = %v", assistant["interrupted"])
	}
}

func TestSessionStoreDaemonCreatedChatSurvivesStaleFullSave(t *testing.T) {
	store := newSessionStore(filepath.Join(t.TempDir(), sessionStateFilename))
	created, err := store.AgentCreateChat("Daemon durable", "/tmp", "mock", "mock-deterministic", "ask", false)
	if err != nil {
		t.Fatalf("create daemon chat: %v", err)
	}
	tabID, chatID := fieldString(created, "tabId"), fieldString(created, "chatId")
	stale := map[string]any{"v": json.Number("1"), "activeId": nil, "seq": json.Number("1"), "chats": []any{}}
	if !store.Save(stale) {
		t.Fatal("stale renderer save failed")
	}
	if !agentChatListed(store.AgentChatList(), tabID, chatID) {
		t.Fatalf("daemon-created chat missing after stale save: tab=%s chat=%s list=%#v", tabID, chatID, store.AgentChatList())
	}
	if _, err := store.AgentReadChat(tabID, chatID, 1, false); err != nil {
		t.Fatalf("exact read after stale save: %v", err)
	}
}

func TestSessionStoreServerAuthoredChatAdoptionClearsMarkerAndRendererDeleteWins(t *testing.T) {
	store := newSessionStore(filepath.Join(t.TempDir(), sessionStateFilename))
	created, err := store.AgentCreateChat("Adopt me", "/tmp", "mock", "mock-deterministic", "ask", false)
	if err != nil {
		t.Fatalf("create daemon chat: %v", err)
	}
	tabID, chatID := fieldString(created, "tabId"), fieldString(created, "chatId")
	adoption := cloneJSON(store.Get()).(map[string]any)
	if !store.Save(adoption) {
		t.Fatal("adoption save failed")
	}
	adopted := chatFromSnapshot(store.Get().(map[string]any), tabID)
	if _, exists := adopted["serverAuthored"]; exists {
		t.Fatalf("serverAuthored marker survived adoption: %#v", adopted)
	}
	deleteSnapshot := cloneJSON(store.Get()).(map[string]any)
	deleteSnapshot["chats"] = []any{}
	if !store.Save(deleteSnapshot) {
		t.Fatal("renderer delete save failed")
	}
	if agentChatListed(store.AgentChatList(), tabID, chatID) {
		t.Fatalf("adopted chat resurrected after renderer delete: %#v", store.AgentChatList())
	}
	if _, err := store.AgentReadChat(tabID, chatID, 1, false); err == nil {
		t.Fatal("exact read succeeded after renderer delete")
	}
}

func TestSessionStoreServerAuthoredChatMarkerSurvivesRestartBeforeStaleSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), sessionStateFilename)
	store := newSessionStore(path)
	created, err := store.AgentCreateChat("Restart durable", "/tmp", "mock", "mock-deterministic", "ask", false)
	if err != nil {
		t.Fatalf("create daemon chat: %v", err)
	}
	tabID, chatID := fieldString(created, "tabId"), fieldString(created, "chatId")
	reloaded := newSessionStore(path)
	if err := reloaded.LoadError(); err != nil {
		t.Fatalf("reload session store: %v", err)
	}
	chat := chatFromSnapshot(reloaded.Get().(map[string]any), tabID)
	if marked, _ := boolField(chat, "serverAuthored"); !marked {
		t.Fatalf("serverAuthored marker did not survive restart: %#v", chat)
	}
	stale := map[string]any{"v": json.Number("1"), "activeId": nil, "seq": json.Number("1"), "chats": []any{}}
	if !reloaded.Save(stale) {
		t.Fatal("stale renderer save after restart failed")
	}
	if !agentChatListed(reloaded.AgentChatList(), tabID, chatID) {
		t.Fatalf("daemon-created chat missing after restart stale save: tab=%s chat=%s list=%#v", tabID, chatID, reloaded.AgentChatList())
	}
	if _, err := reloaded.AgentReadChat(tabID, chatID, 1, false); err != nil {
		t.Fatalf("exact read after restart stale save: %v", err)
	}
}

func TestSessionStoreExplicitDeleteSavePreservesOmissionsAndDeletesOnlyNamedChat(t *testing.T) {
	store := newSessionStore(filepath.Join(t.TempDir(), sessionStateFilename))
	initial := map[string]any{
		"v": json.Number("1"), "activeId": "keep-tab", "seq": json.Number("2"),
		"theme": "dark", "panes": map[string]any{}, "mode": "chats",
		"chats": []any{
			map[string]any{"id": "keep-tab", "chatId": "keep-chat", "title": "Keep", "messages": []any{}},
			map[string]any{"id": "delete-tab", "chatId": "delete-chat", "title": "Delete", "messages": []any{}},
		},
	}
	if !store.Save(initial) {
		t.Fatal("initial save failed")
	}

	partial := map[string]any{
		"v": json.Number("1"), "activeId": "keep-tab", "seq": json.Number("3"),
		"_workassSave": "lean-payload-v2",
		"theme":        "dark", "panes": map[string]any{}, "mode": "chats",
		"chats": []any{
			map[string]any{"id": "keep-tab", "chatId": "keep-chat", "title": "Keep updated", "messages": []any{}},
		},
	}
	if !store.Save(partial) {
		t.Fatal("partial v2 save failed")
	}
	if chatFromSnapshot(store.Get().(map[string]any), "delete-tab") == nil {
		t.Fatal("chat omitted from a v2 save was deleted without an explicit delete capability")
	}

	explicit := cloneJSON(partial).(map[string]any)
	explicit["_workassDeletedChatIds"] = []any{"delete-tab"}
	if !store.Save(explicit) {
		t.Fatal("explicit v2 delete save failed")
	}
	got := store.Get().(map[string]any)
	if chatFromSnapshot(got, "delete-tab") != nil {
		t.Fatalf("explicitly deleted chat survived: %#v", got)
	}
	if chatFromSnapshot(got, "keep-tab") == nil {
		t.Fatalf("unrelated chat disappeared during explicit delete: %#v", got)
	}
	if _, exists := got["_workassDeletedChatIds"]; exists {
		t.Fatalf("transient delete marker leaked into durable state: %#v", got)
	}

	malformed := cloneJSON(partial).(map[string]any)
	malformed["_workassDeletedChatIds"] = []any{json.Number("7")}
	if store.Save(malformed) {
		t.Fatal("v2 save accepted a non-string explicit delete id")
	}
	if chatFromSnapshot(store.Get().(map[string]any), "keep-tab") == nil {
		t.Fatal("rejected delete marker mutated the durable snapshot")
	}
}

func TestSessionStoreAgentDeleteTombstoneRejectsStaleRendererResurrection(t *testing.T) {
	stateDir := t.TempDir()
	path := filepath.Join(stateDir, sessionStateFilename)
	initial := map[string]any{
		"v": json.Number("1"), "activeId": "keep-tab", "seq": json.Number("2"),
		"theme": "dark", "panes": map[string]any{}, "mode": "chats",
		"chats": []any{
			map[string]any{"id": "keep-tab", "chatId": "keep-chat", "title": "Keep", "messages": []any{}},
			map[string]any{"id": "delete-tab", "chatId": "delete-chat", "title": "Delete", "messages": []any{
				map[string]any{"id": "delete-u", "role": "user", "content": "recoverable history", "status": "done"},
			}},
		},
	}
	store := newSessionStore(path)
	if !store.Save(initial) {
		t.Fatal("initial save failed")
	}
	stale := cloneJSON(store.Get()).(map[string]any)
	stale["_workassSave"] = "lean-payload-v2"

	if err := store.AgentDeleteChat("delete-tab", "delete-chat"); err != nil {
		t.Fatalf("agent delete: %v", err)
	}
	if !store.Save(stale) {
		t.Fatal("stale renderer save failed")
	}
	if chatFromSnapshot(store.Get().(map[string]any), "delete-tab") != nil {
		t.Fatal("stale renderer save resurrected agent-deleted chat")
	}
	if chatFromSnapshot(store.Get().(map[string]any), "keep-tab") == nil {
		t.Fatal("stale renderer save removed unrelated chat")
	}

	tombstonePath := filepath.Join(stateDir, "dropped-chats", "delete-tab.json")
	raw, err := os.ReadFile(tombstonePath)
	if err != nil {
		t.Fatalf("read delete tombstone: %v", err)
	}
	var tombstone map[string]any
	if err := json.Unmarshal(raw, &tombstone); err != nil {
		t.Fatalf("decode delete tombstone: %v", err)
	}
	if tombstone["chatId"] != "delete-chat" || len(messageSlice(tombstone)) != 1 {
		t.Fatalf("delete tombstone = %#v", tombstone)
	}

	reloaded := newSessionStore(path)
	if err := reloaded.LoadError(); err != nil {
		t.Fatalf("reload store: %v", err)
	}
	if !reloaded.Save(stale) {
		t.Fatal("post-restart stale renderer save failed")
	}
	if chatFromSnapshot(reloaded.Get().(map[string]any), "delete-tab") != nil {
		t.Fatal("post-restart stale renderer save resurrected agent-deleted chat")
	}
}

func TestSessionStoreDroppedChatTombstoneAndPreservesProtectedChats(t *testing.T) {
	stateDir := t.TempDir()
	store := newSessionStore(filepath.Join(stateDir, sessionStateFilename))
	snapshot := map[string]any{
		"v": json.Number("1"), "activeId": "drop-tab", "seq": json.Number("3"),
		"theme": "dark", "panes": map[string]any{}, "mode": "chats",
		"chats": []any{
			map[string]any{"id": "drop-tab", "chatId": "drop-chat", "title": "Drop", "messages": []any{
				map[string]any{"id": "drop-u", "role": "user", "content": "drop me", "status": "done"},
				map[string]any{"id": "drop-a", "role": "assistant", "content": "dropped answer", "status": "done"},
			}},
			map[string]any{"id": "live-tab", "chatId": "live-chat", "title": "Live", "messages": []any{}},
			map[string]any{"id": "server-tab", "chatId": "server-chat", "title": "Server", "messages": []any{}, "serverAuthored": true},
		},
	}
	if !store.Save(snapshot) {
		t.Fatal("initial save failed")
	}
	store.PrepareTurn(map[string]any{"tabId": "live-tab", "chatId": "live-chat", "title": "Live", "prompt": "live prompt"})

	var logLines []string
	prevLog := sessionStoreLogLine
	sessionStoreLogLine = func(line string) { logLines = append(logLines, line) }
	t.Cleanup(func() { sessionStoreLogLine = prevLog })

	stale := map[string]any{"v": json.Number("1"), "activeId": nil, "seq": json.Number("4"), "chats": []any{}}
	if !store.Save(stale) {
		t.Fatal("stale save failed")
	}
	got := store.Get().(map[string]any)
	if chatFromSnapshot(got, "drop-tab") != nil {
		t.Fatalf("unprotected chat survived stale save: %#v", got)
	}
	if chatFromSnapshot(got, "live-tab") == nil {
		t.Fatalf("live-job chat was not preserved: %#v", got)
	}
	serverChat := chatFromSnapshot(got, "server-tab")
	if serverChat == nil {
		t.Fatalf("serverAuthored chat was not preserved: %#v", got)
	}
	if marked, _ := boolField(serverChat, "serverAuthored"); !marked {
		t.Fatalf("serverAuthored marker missing after stale save: %#v", serverChat)
	}

	raw, err := os.ReadFile(filepath.Join(stateDir, "dropped-chats", "drop-tab.json"))
	if err != nil {
		t.Fatalf("read dropped chat tombstone: %v", err)
	}
	var tombstone map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&tombstone); err != nil {
		t.Fatalf("decode dropped chat tombstone: %v", err)
	}
	if tombstone["id"] != "drop-tab" || tombstone["chatId"] != "drop-chat" || len(messageSlice(tombstone)) != 2 {
		t.Fatalf("dropped chat tombstone = %#v", tombstone)
	}
	if len(logLines) != 1 || !strings.Contains(logLines[0], "session save dropped chats") || !strings.Contains(logLines[0], "count=1") || !strings.Contains(logLines[0], "drop-tab") {
		t.Fatalf("drop log lines = %#v", logLines)
	}
}

func TestSessionStoreWorkspaceMoveIsExactAndSurvivesHydration(t *testing.T) {
	path := filepath.Join(t.TempDir(), sessionStateFilename)
	snapshot := sessionMirrorFixture("move-tab", "move-chat", "history stays canonical")
	chat := chatFromSnapshot(snapshot, "move-tab")
	chat["cwd"] = "/workspace/old"
	snapshot["chats"] = append(anySlice(snapshot["chats"]), map[string]any{
		"id": "other-tab", "chatId": "other-chat", "cwd": "/workspace/other", "messages": []any{},
	})
	store := newSessionStore(path)
	if !store.Save(snapshot) {
		t.Fatal("initial save failed")
	}
	if store.UpdateChatWorkspace("move-tab", "wrong-chat", "/workspace/target") {
		t.Fatal("workspace update accepted the wrong immutable conversation id")
	}
	if !store.UpdateChatWorkspace("move-tab", "move-chat", "/workspace/target") {
		t.Fatal("exact workspace update failed")
	}
	if cwd, ok := store.ChatWorkspace("move-tab", "move-chat"); !ok || cwd != "/workspace/target" {
		t.Fatalf("authoritative workspace cwd=%q ok=%v", cwd, ok)
	}
	// Both old full snapshots and capability-gated lean snapshots may arrive
	// after the move acknowledgment. Neither may roll daemon cwd backward.
	if !store.Save(snapshot) {
		t.Fatal("stale full save failed")
	}
	staleLean := cloneJSON(snapshot).(map[string]any)
	staleLean["_workassSave"] = "lean-payload-v1"
	if !store.Save(staleLean) {
		t.Fatal("stale lean save failed")
	}
	if cwd, _ := store.ChatWorkspace("move-tab", "move-chat"); cwd != "/workspace/target" {
		t.Fatalf("stale save rolled workspace back to %q", cwd)
	}
	if revision, moved := store.MoveChatWorkspace("move-tab", "move-chat", "/workspace/second", 1); !moved || revision != 2 {
		t.Fatalf("second legitimate move revision=%d moved=%v", revision, moved)
	}
	if revision, moved := store.MoveChatWorkspace("move-tab", "move-chat", "/workspace/stale", 1); moved || revision != 2 {
		t.Fatalf("stale competing move revision=%d moved=%v", revision, moved)
	}
	if cwd, _ := store.ChatWorkspace("other-tab", "other-chat"); cwd != "/workspace/other" {
		t.Fatalf("exact move changed other chat cwd to %q", cwd)
	}
	// A failed durable write is not a commit: the daemon's in-memory authority
	// must roll back with the disk failure so the old live session and cwd cannot
	// diverge. Pointing the snapshot path at a directory forces rename failure.
	goodPath := store.path
	store.path = t.TempDir()
	if revision, moved := store.MoveChatWorkspace("move-tab", "move-chat", "/workspace/unwritten", 2); moved || revision != 2 {
		t.Fatalf("failed durable move revision=%d moved=%v", revision, moved)
	}
	if cwd, _ := store.ChatWorkspace("move-tab", "move-chat"); cwd != "/workspace/second" {
		t.Fatalf("failed durable move changed in-memory cwd to %q", cwd)
	}
	store.path = goodPath
	reloaded := newSessionStore(path)
	if !reloaded.Save(snapshot) {
		t.Fatal("stale full save after restart failed")
	}
	staleLeanAfterRestart := cloneJSON(snapshot).(map[string]any)
	staleLeanAfterRestart["_workassSave"] = "lean-payload-v1"
	if !reloaded.Save(staleLeanAfterRestart) {
		t.Fatal("stale lean save after restart failed")
	}
	if cwd, _ := reloaded.ChatWorkspace("move-tab", "move-chat"); cwd != "/workspace/second" {
		t.Fatalf("post-restart stale save rolled workspace back to %q", cwd)
	}
	got := chatFromSnapshot(reloaded.Get().(map[string]any), "move-tab")
	if fieldString(got, "cwd") != "/workspace/second" {
		t.Fatalf("hydrated workspace reverted: %#v", got)
	}
	if len(messageSlice(got)) != len(messageSlice(chat)) {
		t.Fatalf("workspace move changed canonical messages: %#v", got)
	}
}

func TestSessionStoreLeanSavePreservesExactHeavyPayloadsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), sessionStateFilename)
	full := sessionMirrorFixture("lean-tab", "lean-chat", "look")
	chat := chatFromSnapshot(full, "lean-tab")
	messages := messageSlice(chat)
	user := mapFromAnyMain(messages[0])
	assistant := mapFromAnyMain(messages[1])
	assistant["status"] = "done"
	assistant["at"] = "2026-07-13T10:00:01Z"
	user["images"] = []any{map[string]any{"mimeType": "image/png", "data": "image-bytes"}}
	assistant["events"] = []any{
		map[string]any{"key": "tool-key", "at": 2, "kind": "tool", "id": "tool-id", "title": "Read", "status": "completed", "output": "large-output"},
		map[string]any{"key": "old-compact", "at": 3, "kind": "compaction"},
	}
	chat["queue"] = []any{map[string]any{
		"id": "queue-id", "text": "later", "images": []any{map[string]any{"mimeType": "image/png", "data": "queue-bytes"}},
	}}
	store := newSessionStore(path)
	if !store.Save(full) {
		t.Fatal("full save failed")
	}
	store = newSessionStore(path) // jobs are intentionally empty after restart

	lean := cloneJSON(full).(map[string]any)
	lean["_workassSave"] = "lean-payload-v1"
	leanChat := chatFromSnapshot(lean, "lean-tab")
	leanMessages := messageSlice(leanChat)
	delete(mapFromAnyMain(leanMessages[0]), "images")
	mapFromAnyMain(leanMessages[1])["events"] = []any{
		map[string]any{"key": "tool-key", "at": 2, "kind": "tool", "id": "tool-id", "startedAt": 10, "endedAt": 20, "subagentModel": "gpt-test"},
		map[string]any{"key": "restored", "at": 4, "kind": "restored", "turnSeq": 1},
	}
	delete(mapFromAnyMain(anySlice(leanChat["queue"])[0]), "images")
	if !store.Save(lean) {
		t.Fatal("lean save failed")
	}

	reloaded := newSessionStore(path)
	got := reloaded.Get().(map[string]any)
	if _, leaked := got["_workassSave"]; leaked {
		t.Fatal("transient lean-save marker reached canonical state")
	}
	gotChat := chatFromSnapshot(got, "lean-tab")
	gotMessages := messageSlice(gotChat)
	if image := mapFromAnyMain(anySlice(mapFromAnyMain(gotMessages[0])["images"])[0]); image["data"] != "image-bytes" {
		t.Fatalf("message image was not preserved: %#v", image)
	}
	events := anySlice(mapFromAnyMain(gotMessages[1])["events"])
	if len(events) != 2 {
		t.Fatalf("merged events = %#v", events)
	}
	tool := mapFromAnyMain(events[0])
	if tool["output"] != "large-output" || tool["subagentModel"] != "gpt-test" || intValue(tool["startedAt"]) != 10 || intValue(tool["endedAt"]) != 20 {
		t.Fatalf("tool payload/overlay = %#v", tool)
	}
	if event := mapFromAnyMain(events[1]); event["kind"] != "restored" {
		t.Fatalf("renderer-owned event was not replaced: %#v", event)
	}
	queueImage := mapFromAnyMain(anySlice(mapFromAnyMain(anySlice(gotChat["queue"])[0])["images"])[0])
	if queueImage["data"] != "queue-bytes" {
		t.Fatalf("queue image was not preserved: %#v", queueImage)
	}

	deleteRows := cloneJSON(got).(map[string]any)
	deleteRows["_workassSave"] = "lean-payload-v1"
	deleteChat := chatFromSnapshot(deleteRows, "lean-tab")
	deleteChat["messages"] = []any{messageSlice(deleteChat)[1]}
	deleteChat["queue"] = []any{}
	if !reloaded.Save(deleteRows) {
		t.Fatal("lean deletion save failed")
	}
	afterDelete := chatFromSnapshot(reloaded.Get().(map[string]any), "lean-tab")
	if len(messageSlice(afterDelete)) != 1 || len(anySlice(afterDelete["queue"])) != 0 {
		t.Fatalf("lean merge resurrected deleted rows: %#v", afterDelete)
	}
}

func TestSessionStoreUnmarkedSaveKeepsLegacyReplacementSemantics(t *testing.T) {
	store := newSessionStore(filepath.Join(t.TempDir(), sessionStateFilename))
	full := sessionMirrorFixture("legacy-tab", "legacy-chat", "look")
	chat := chatFromSnapshot(full, "legacy-tab")
	assistant := mapFromAnyMain(messageSlice(chat)[1])
	assistant["status"] = "done"
	assistant["events"] = []any{map[string]any{"kind": "tool", "id": "tool", "output": "old"}}
	mapFromAnyMain(messageSlice(chat)[0])["images"] = []any{map[string]any{"data": "old-image"}}
	if !store.Save(full) {
		t.Fatal("full save failed")
	}
	replacement := cloneJSON(full).(map[string]any)
	replacementChat := chatFromSnapshot(replacement, "legacy-tab")
	delete(mapFromAnyMain(messageSlice(replacementChat)[0]), "images")
	mapFromAnyMain(messageSlice(replacementChat)[1])["events"] = []any{}
	if !store.Save(replacement) {
		t.Fatal("replacement save failed")
	}
	got := chatFromSnapshot(store.Get().(map[string]any), "legacy-tab")
	if _, exists := mapFromAnyMain(messageSlice(got)[0])["images"]; exists {
		t.Fatal("unmarked save unexpectedly preserved omitted images")
	}
	if len(anySlice(mapFromAnyMain(messageSlice(got)[1])["events"])) != 0 {
		t.Fatal("unmarked save unexpectedly preserved events")
	}
}

func TestSessionStorePrepareTurnOwnsImagesAndSubagentModel(t *testing.T) {
	stateDir := t.TempDir()
	store := newSessionStore(filepath.Join(stateDir, sessionStateFilename))
	if !store.Save(sessionMirrorFixture("media-tab", "media-chat", "inspect")) {
		t.Fatal("initial save failed")
	}
	store.PrepareTurn(map[string]any{
		"tabId": "media-tab", "chatId": "media-chat", "prompt": "inspect",
		"images": []any{map[string]any{"mimeType": "image/png", "data": "turn-image"}},
	})
	store.RecordJobEvent("job:event", map[string]any{"type": "start", "job": map[string]any{
		"id": "media-job", "tabId": "media-tab", "chatId": "media-chat", "startedAt": "2026-07-13T10:00:00Z",
	}})
	store.RecordJobEvent("job:event", map[string]any{"type": "acp", "id": "media-job", "event": map[string]any{
		"kind": "tool", "id": "subagent", "title": "Agent", "status": "pending", "subagentId": "subagent", "subagentModel": "gpt-subagent",
	}})
	store.RecordJobEvent("job:event", map[string]any{"type": "acp", "id": "media-job", "event": map[string]any{
		"kind": "tool", "id": "subagent", "title": "Agent", "status": "completed", "subagentId": "subagent", "subagentModel": "gpt-subagent",
		"images": []any{map[string]any{"mimeType": "image/png", "data": "tool-image", "name": "mock.png"}},
	}})
	store.RecordJobEvent("job:event", map[string]any{"type": "end", "job": map[string]any{
		"id": "media-job", "tabId": "media-tab", "chatId": "media-chat", "status": "done", "finishedAt": "2026-07-13T10:00:01Z",
	}})

	gotChat := chatFromSnapshot(store.Get().(map[string]any), "media-tab")
	image := mapFromAnyMain(anySlice(mapFromAnyMain(messageSlice(gotChat)[0])["images"])[0])
	if image["data"] != "turn-image" {
		t.Fatalf("prepared image = %#v", image)
	}
	tool := mapFromAnyMain(anySlice(sessionAssistant(t, store.Get().(map[string]any), "media-tab")["events"])[0])
	if tool["subagentModel"] != "gpt-subagent" {
		t.Fatalf("subagent model = %#v", tool)
	}
	toolImage := mapFromAnyMain(anySlice(tool["images"])[0])
	if toolImage["data"] != "tool-image" || toolImage["name"] != "mock.png" {
		t.Fatalf("tool image = %#v", toolImage)
	}
	reloaded := newSessionStore(filepath.Join(stateDir, sessionStateFilename))
	reloadedChat := chatFromSnapshot(reloaded.Get().(map[string]any), "media-tab")
	reloadedImage := mapFromAnyMain(anySlice(mapFromAnyMain(messageSlice(reloadedChat)[0])["images"])[0])
	if reloadedImage["data"] != "turn-image" {
		t.Fatalf("reloaded prepared image = %#v", reloadedImage)
	}
	reloadedTool := mapFromAnyMain(anySlice(sessionAssistant(t, reloaded.Get().(map[string]any), "media-tab")["events"])[0])
	reloadedToolImage := mapFromAnyMain(anySlice(reloadedTool["images"])[0])
	if reloadedToolImage["data"] != "tool-image" || reloadedToolImage["name"] != "mock.png" {
		t.Fatalf("reloaded tool image = %#v", reloadedToolImage)
	}
	read, err := reloaded.AgentReadChat("media-tab", "media-chat", 10, true)
	if err != nil {
		t.Fatalf("agent read media chat: %v", err)
	}
	readAssistant := mapFromAnyMain(anySlice(read["messages"])[1])
	readTool := mapFromAnyMain(anySlice(readAssistant["events"])[0])
	if _, exposed := readTool["images"]; exposed {
		t.Fatalf("agent chat read exposed tool image bytes: %#v", readTool)
	}
	attachments := anySlice(readTool["attachments"])
	if len(attachments) != 1 || mapFromAnyMain(attachments[0])["name"] != "mock.png" {
		t.Fatalf("agent chat read tool attachment metadata = %#v", readTool)
	}
}

func TestSessionStoreResolvesMostRecentVisibleAssistantJobID(t *testing.T) {
	store := newSessionStore(filepath.Join(t.TempDir(), sessionStateFilename))
	if !store.Save(sessionMirrorFixture("anchor-tab", "anchor-chat", "first")) {
		t.Fatal("initial save failed")
	}
	store.PrepareTurn(map[string]any{"tabId": "anchor-tab", "chatId": "anchor-chat", "prompt": "first"})
	store.RecordJobEvent("job:event", map[string]any{"type": "start", "job": map[string]any{
		"id": "visible-job-one", "tabId": "anchor-tab", "chatId": "anchor-chat", "startedAt": "2026-07-16T10:00:00Z",
	}})
	store.RecordJobEvent("job:event", map[string]any{"type": "end", "job": map[string]any{
		"id": "visible-job-one", "tabId": "anchor-tab", "chatId": "anchor-chat", "status": "done", "finishedAt": "2026-07-16T10:00:01Z",
	}})
	store.PrepareTurn(map[string]any{"tabId": "anchor-tab", "chatId": "anchor-chat", "prompt": "second"})
	store.RecordJobEvent("job:event", map[string]any{"type": "start", "job": map[string]any{
		"id": "visible-job-two", "tabId": "anchor-tab", "chatId": "anchor-chat", "startedAt": "2026-07-16T10:00:02Z",
	}})
	if got := store.MostRecentVisibleAssistantJobID("anchor-tab", "anchor-chat"); got != "visible-job-two" {
		t.Fatalf("most recent visible assistant job id = %q", got)
	}
	if got := store.MostRecentVisibleAssistantJobID("anchor-tab", "wrong-chat"); got != "" {
		t.Fatalf("mismatched chat resolved visible job id %q", got)
	}
}

func TestSessionStorePersistsLiveSteerAckReceiptBoundaryAndArchive(t *testing.T) {
	stateDir := t.TempDir()
	path := filepath.Join(stateDir, sessionStateFilename)
	store := newSessionStore(path)
	if !store.Save(sessionMirrorFixture("steer-tab", "steer-chat", "base turn")) {
		t.Fatal("initial steer fixture save failed")
	}
	store.PrepareTurn(map[string]any{
		"tabId": "steer-tab", "chatId": "steer-chat", "prompt": "base turn",
		"userMessageId": "client-u", "assistantMessageId": "client-a",
	})
	store.RecordJobEvent("job:event", map[string]any{"type": "start", "job": map[string]any{
		"id": "steer-job", "tabId": "steer-tab", "chatId": "steer-chat", "startedAt": "2026-07-14T10:00:00Z",
	}})
	store.RecordJobEvent("job:event", map[string]any{
		"type": "data", "id": "steer-job", "stream": "stdout", "chunk": "before ",
	})

	if err := store.BeginLiveSteer("steer-tab", "steer-chat", "steer-user", "change direction", "client-a-after-steer", []any{map[string]any{"mimeType": "image/png", "data": "c3RlZXI="}}, map[string]any{
		"assistantMessageId": "client-a", "contentOffset": 7, "resultOffset": 0, "eventCount": 0, "deferUntilConsumed": true,
	}); err != nil {
		t.Fatalf("begin chronological steer: %v", err)
	}
	if err := store.AcknowledgeLiveSteer("steer-tab", "steer-chat", "steer-user", "change direction", "accepted"); err != nil {
		t.Fatalf("acknowledge live steer: %v", err)
	}
	ackSnapshot := store.Get().(map[string]any)
	steer := mapFromAnyMain(messageSlice(chatFromSnapshot(ackSnapshot, "steer-tab"))[2])
	ackRoot := mapFromAnyMain(messageSlice(chatFromSnapshot(ackSnapshot, "steer-tab"))[1])
	ackTail := mapFromAnyMain(messageSlice(chatFromSnapshot(ackSnapshot, "steer-tab"))[3])
	if fieldString(steer, "status") != "done" || fieldString(steer, "steerState") != "accepted" || fieldString(steer, "steerBoundary") != "waiting" || fieldString(ackRoot, "status") != "running" || fieldString(ackTail, "status") != "pending" {
		t.Fatalf("acknowledged steer = %#v", steer)
	}
	store.RecordJobEvent("job:event", map[string]any{
		"type": "data", "id": "steer-job", "stream": "stdout", "chunk": "finish sentence. ",
	})

	store.RecordJobEvent("job:event", map[string]any{
		"type": "acp", "id": "steer-job", "event": map[string]any{
			"kind": "steer-consumed", "clientUserMessageId": "steer-user",
		},
	})
	// A controller that captured the acknowledgement just before the receipt
	// cannot regress the daemon's stronger applied state with a late full save.
	if !store.Save(ackSnapshot) {
		t.Fatal("stale accepted steer save failed")
	}
	afterStaleSave := mapFromAnyMain(messageSlice(chatFromSnapshot(store.Get().(map[string]any), "steer-tab"))[2])
	if fieldString(afterStaleSave, "steerState") != "applied" {
		t.Fatalf("stale accepted save regressed receipt = %#v", afterStaleSave)
	}
	store.RecordJobEvent("job:event", map[string]any{
		"type": "data", "id": "steer-job", "stream": "stdout", "chunk": "after",
	})
	store.RecordJobEvent("job:event", map[string]any{"type": "end", "job": map[string]any{
		"id": "steer-job", "tabId": "steer-tab", "chatId": "steer-chat", "status": "done", "finishedAt": "2026-07-14T10:00:02Z",
	}})

	reloaded := newSessionStore(path)
	if err := reloaded.LoadError(); err != nil {
		t.Fatalf("reload acknowledged steer: %v", err)
	}
	reloadedMessages := messageSlice(chatFromSnapshot(reloaded.Get().(map[string]any), "steer-tab"))
	if len(reloadedMessages) != 4 {
		t.Fatalf("reloaded messages = %#v", reloadedMessages)
	}
	reloadedSteer := mapFromAnyMain(reloadedMessages[2])
	if fieldString(reloadedSteer, "steerState") != "applied" || reloadedSteer["steerAnchor"] != nil || fieldString(reloadedSteer, "turnRootId") != "client-a" || len(anySlice(reloadedSteer["images"])) != 1 {
		t.Fatalf("reloaded canonical steer = %#v", reloadedSteer)
	}
	if prefix, tail := mapFromAnyMain(reloadedMessages[1]), mapFromAnyMain(reloadedMessages[3]); stringValue(prefix["content"]) != "before finish sentence. " || fieldString(prefix, "turnTerminal") != "false" || stringValue(tail["content"]) != "after" || fieldString(tail, "id") != "client-a-after-steer" {
		t.Fatalf("reloaded chronological assistant split = %#v", reloadedMessages)
	}
	archive := loadChatArchive(stateDir, "steer-tab")
	if len(archive) != 4 {
		t.Fatalf("steered turn archive = %#v", archive)
	}
	archivedSteer := mapFromAnyMain(archive[2])
	if fieldString(archivedSteer, "steerState") != "applied" || archivedSteer["steerAnchor"] != nil || len(anySlice(archivedSteer["images"])) != 1 {
		t.Fatalf("archived steer metadata = %#v", archivedSteer)
	}
	if stringValue(mapFromAnyMain(archive[1])["content"]) != "before finish sentence. " || stringValue(mapFromAnyMain(archive[3])["content"]) != "after" {
		t.Fatalf("archive must preserve the semantic receipt boundary: %#v", archive)
	}
}

func TestSessionStoreTurnEndRevealsUnconsumedStagedSteerAfterCompleteAssistant(t *testing.T) {
	path := filepath.Join(t.TempDir(), sessionStateFilename)
	store := newSessionStore(path)
	if !store.Save(sessionMirrorFixture("late-steer-tab", "late-steer-chat", "base")) {
		t.Fatal("save late steer fixture")
	}
	store.PrepareTurn(map[string]any{
		"tabId": "late-steer-tab", "chatId": "late-steer-chat", "prompt": "base",
		"userMessageId": "late-user", "assistantMessageId": "late-root",
	})
	store.RecordJobEvent("job:event", map[string]any{"type": "start", "job": map[string]any{
		"id": "late-job", "tabId": "late-steer-tab", "chatId": "late-steer-chat",
	}})
	store.RecordJobEvent("job:event", map[string]any{
		"type": "data", "id": "late-job", "stream": "stdout", "chunk": "complete sentence",
	})
	if err := store.BeginLiveSteer("late-steer-tab", "late-steer-chat", "late-steer", "next direction", "late-tail", nil, map[string]any{
		"assistantMessageId": "late-root", "deferUntilConsumed": true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.AcknowledgeLiveSteer("late-steer-tab", "late-steer-chat", "late-steer", "next direction", "accepted"); err != nil {
		t.Fatal(err)
	}
	store.RecordJobEvent("job:event", map[string]any{"type": "end", "job": map[string]any{
		"id": "late-job", "tabId": "late-steer-tab", "chatId": "late-steer-chat", "status": "done",
		"finishedAt": "2026-07-14T10:00:02Z",
	}})

	messages := messageSlice(chatFromSnapshot(store.Get().(map[string]any), "late-steer-tab"))
	if len(messages) != 3 {
		t.Fatalf("terminal staged steer rows = %#v", messages)
	}
	assistant, steer := mapFromAnyMain(messages[1]), mapFromAnyMain(messages[2])
	if fieldString(assistant, "id") != "late-root" || stringValue(assistant["content"]) != "complete sentence" || fieldString(assistant, "status") != "done" {
		t.Fatalf("completed assistant moved at turn end: %#v", messages)
	}
	if fieldString(steer, "id") != "late-steer" || fieldString(steer, "steerState") != "accepted" || fieldString(steer, "steerBoundary") != "" || fieldString(steer, "status") != "done" {
		t.Fatalf("unconsumed steer did not settle after assistant: %#v", messages)
	}
	for _, raw := range messages {
		if fieldString(mapFromAnyMain(raw), "id") == "late-tail" {
			t.Fatalf("never-activated continuation survived turn end: %#v", messages)
		}
	}
}

func TestSessionStoreMigratesBottomAnchoredSteerIntoPermanentChronologicalRows(t *testing.T) {
	stateDir := t.TempDir()
	path := filepath.Join(stateDir, sessionStateFilename)
	snapshot := sessionMirrorFixture("legacy-steer-tab", "legacy-steer-chat", "start")
	chat := chatFromSnapshot(snapshot, "legacy-steer-tab")
	chat["messages"] = []any{
		map[string]any{"id": "client-u", "role": "user", "content": "start", "status": "done", "at": "0", "events": []any{}},
		map[string]any{
			"id": "legacy-assistant", "role": "assistant", "content": "beforeafter", "result": "final", "status": "done", "at": "2", "jobId": "job-1",
			"events": []any{map[string]any{"key": "before", "at": 1}, map[string]any{"key": "after", "at": 7}},
		},
		// This is the broken production order: the response owner precedes the
		// steer in the array, so the UI could only pin the steer at the bottom.
		map[string]any{
			"id": "legacy-steer", "role": "user", "content": "redirect", "status": "done", "at": "1", "events": []any{}, "steerState": "applied",
			"steerAnchor": map[string]any{"assistantMessageId": "legacy-assistant", "contentOffset": 6, "resultOffset": 0, "eventCount": 1},
		},
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := appendChatArchive(stateDir, "legacy-steer-tab", messageSlice(chat)); err != nil {
		t.Fatalf("write legacy steer archive: %v", err)
	}

	store := newSessionStore(path)
	if err := store.LoadError(); err != nil {
		t.Fatalf("load legacy steer: %v", err)
	}
	messages := messageSlice(chatFromSnapshot(store.Get().(map[string]any), "legacy-steer-tab"))
	if len(messages) != 4 {
		t.Fatalf("migrated message count/order = %#v", messages)
	}
	want := []struct{ role, content, result string }{
		{"user", "start", ""},
		{"assistant", "before", ""},
		{"user", "redirect", ""},
		{"assistant", "after", "final"},
	}
	for index, expected := range want {
		message := mapFromAnyMain(messages[index])
		if fieldString(message, "role") != expected.role || stringValue(message["content"]) != expected.content || stringValue(message["result"]) != expected.result {
			t.Fatalf("migrated row %d = %#v, want %#v", index, message, expected)
		}
		if message["steerAnchor"] != nil {
			t.Fatalf("legacy anchor survived row %d: %#v", index, message)
		}
	}
	if fieldString(mapFromAnyMain(messages[1]), "turnTerminal") != "false" || fieldString(mapFromAnyMain(messages[3]), "turnTerminal") != "true" {
		t.Fatalf("assistant terminal ownership = %#v", messages)
	}

	// The repair is durable and idempotent, not a renderer-only presentation.
	reloaded := newSessionStore(path)
	if err := reloaded.LoadError(); err != nil {
		t.Fatalf("reload migrated steer: %v", err)
	}
	reloadedMessages := messageSlice(chatFromSnapshot(reloaded.Get().(map[string]any), "legacy-steer-tab"))
	if len(reloadedMessages) != 4 || fieldString(mapFromAnyMain(reloadedMessages[2]), "id") != "legacy-steer" {
		t.Fatalf("durable migrated order = %#v", reloadedMessages)
	}
	archive := loadChatArchive(stateDir, "legacy-steer-tab")
	if len(archive) != 4 || fieldString(mapFromAnyMain(archive[1]), "content") != "before" || fieldString(mapFromAnyMain(archive[2]), "id") != "legacy-steer" || fieldString(mapFromAnyMain(archive[3]), "content") != "after" {
		t.Fatalf("migrated archive order = %#v", archive)
	}
	for index, raw := range archive {
		if mapFromAnyMain(raw)["steerAnchor"] != nil {
			t.Fatalf("archive row %d retained a legacy anchor: %#v", index, raw)
		}
	}
}

func TestSessionStoreRapidSteersKeepPhysicalFIFOWithCrossBoundaryToolAndTypedFinal(t *testing.T) {
	stateDir := t.TempDir()
	path := filepath.Join(stateDir, sessionStateFilename)
	store := newSessionStore(path)
	if !store.Save(sessionMirrorFixture("rapid-steer-tab", "rapid-steer-chat", "base")) {
		t.Fatal("save rapid steer fixture")
	}
	store.PrepareTurn(map[string]any{
		"tabId": "rapid-steer-tab", "chatId": "rapid-steer-chat", "prompt": "base",
		"userMessageId": "rapid-user", "assistantMessageId": "rapid-root",
	})
	store.RecordJobEvent("job:event", map[string]any{"type": "start", "job": map[string]any{
		"id": "rapid-job", "tabId": "rapid-steer-tab", "chatId": "rapid-steer-chat", "startedAt": "2026-07-14T10:00:00Z",
	}})
	store.RecordJobEvent("job:event", map[string]any{
		"type": "data", "id": "rapid-job", "stream": "stdout", "phase": "commentary", "chunk": "before",
	})
	store.RecordJobEvent("job:event", map[string]any{
		"type": "acp", "id": "rapid-job", "event": map[string]any{
			"kind": "tool", "id": "cross-tool", "title": "Read", "status": "pending",
		},
	})
	if err := store.BeginLiveSteer("rapid-steer-tab", "rapid-steer-chat", "steer-one", "first steer", "rapid-tail-one", nil, map[string]any{
		"assistantMessageId": "rapid-root", "contentOffset": 6, "resultOffset": 0, "eventCount": 1, "deferUntilConsumed": true,
	}); err != nil {
		t.Fatalf("begin first steer: %v", err)
	}
	if err := store.AcknowledgeLiveSteer("rapid-steer-tab", "rapid-steer-chat", "steer-one", "first steer", "accepted"); err != nil {
		t.Fatal(err)
	}
	store.RecordJobEvent("job:event", map[string]any{
		"type": "data", "id": "rapid-job", "stream": "stdout", "phase": "commentary", "chunk": " sentence.",
	})
	store.RecordJobEvent("job:event", map[string]any{
		"type": "acp", "id": "rapid-job", "event": map[string]any{
			"kind": "steer-consumed", "clientUserMessageId": "steer-one",
		},
	})
	// The completion belongs to the tool row that began before the steer. It
	// must update that frozen prefix without moving new prose above the user.
	store.RecordJobEvent("job:event", map[string]any{
		"type": "acp", "id": "rapid-job", "event": map[string]any{
			"kind": "tool", "id": "cross-tool", "title": "Read", "status": "completed", "output": "ok",
		},
	})
	store.RecordJobEvent("job:event", map[string]any{
		"type": "data", "id": "rapid-job", "stream": "stdout", "phase": "commentary", "chunk": "between",
	})
	if err := store.BeginLiveSteer("rapid-steer-tab", "rapid-steer-chat", "steer-two", "second steer", "rapid-tail-two", nil, map[string]any{
		"assistantMessageId": "rapid-tail-one", "contentOffset": 7, "resultOffset": 0, "eventCount": 0, "deferUntilConsumed": true,
	}); err != nil {
		t.Fatalf("begin second steer: %v", err)
	}
	if err := store.AcknowledgeLiveSteer("rapid-steer-tab", "rapid-steer-chat", "steer-two", "second steer", "accepted"); err != nil {
		t.Fatal(err)
	}
	store.RecordJobEvent("job:event", map[string]any{
		"type": "data", "id": "rapid-job", "stream": "stdout", "phase": "commentary", "chunk": " sentence.",
	})
	store.RecordJobEvent("job:event", map[string]any{
		"type": "acp", "id": "rapid-job", "event": map[string]any{
			"kind": "steer-consumed", "clientUserMessageId": "steer-two",
		},
	})
	store.RecordJobEvent("job:event", map[string]any{
		"type": "data", "id": "rapid-job", "stream": "stdout", "phase": "final_answer", "chunk": "final",
	})
	store.RecordJobEvent("job:event", map[string]any{"type": "end", "job": map[string]any{
		"id": "rapid-job", "tabId": "rapid-steer-tab", "chatId": "rapid-steer-chat", "status": "done",
		"result": "beforebetweenfinal", "finishedAt": "2026-07-14T10:00:02Z",
	}})

	messages := messageSlice(chatFromSnapshot(store.Get().(map[string]any), "rapid-steer-tab"))
	if got := len(messages); got != 6 {
		t.Fatalf("rapid steer rows=%d messages=%#v", got, messages)
	}
	wantIDs := []string{"rapid-user", "rapid-root", "steer-one", "rapid-tail-one", "steer-two", "rapid-tail-two"}
	for index, wantID := range wantIDs {
		if got := fieldString(mapFromAnyMain(messages[index]), "id"); got != wantID {
			t.Fatalf("rapid steer row %d id=%q want=%q messages=%#v", index, got, wantID, messages)
		}
	}
	prefix := mapFromAnyMain(messages[1])
	firstTail := mapFromAnyMain(messages[3])
	finalTail := mapFromAnyMain(messages[5])
	if stringValue(prefix["content"]) != "before sentence." || fieldString(prefix, "turnTerminal") != "false" || stringValue(firstTail["content"]) != "between sentence." || fieldString(firstTail, "turnTerminal") != "false" || stringValue(finalTail["result"]) != "final" || fieldString(finalTail, "turnTerminal") != "true" {
		t.Fatalf("rapid steer physical boundaries = %#v", messages)
	}
	tools := anySlice(prefix["events"])
	if len(tools) != 1 || fieldString(mapFromAnyMain(tools[0]), "status") != "completed" || fieldString(mapFromAnyMain(tools[0]), "output") != "ok" {
		t.Fatalf("cross-boundary tool owner = %#v", messages)
	}
	if len(anySlice(firstTail["events"])) != 0 || len(anySlice(finalTail["events"])) != 0 {
		t.Fatalf("cross-boundary tool duplicated after steer = %#v", messages)
	}

	reloaded := newSessionStore(path)
	if err := reloaded.LoadError(); err != nil {
		t.Fatalf("reload rapid steers: %v", err)
	}
	reloadedMessages := messageSlice(chatFromSnapshot(reloaded.Get().(map[string]any), "rapid-steer-tab"))
	if len(reloadedMessages) != 6 || fieldString(mapFromAnyMain(reloadedMessages[2]), "id") != "steer-one" || fieldString(mapFromAnyMain(reloadedMessages[4]), "id") != "steer-two" || stringValue(mapFromAnyMain(reloadedMessages[5])["result"]) != "final" {
		t.Fatalf("reloaded rapid steer order = %#v", reloadedMessages)
	}
	archive := loadChatArchive(stateDir, "rapid-steer-tab")
	if len(archive) != 6 || fieldString(mapFromAnyMain(archive[2]), "id") != "steer-one" || fieldString(mapFromAnyMain(archive[4]), "id") != "steer-two" {
		t.Fatalf("archived rapid steer order = %#v", archive)
	}
}

func TestSessionStoreRestartSettlesOnlyUnacknowledgedSteerAsUncertain(t *testing.T) {
	path := filepath.Join(t.TempDir(), sessionStateFilename)
	store := newSessionStore(path)
	snapshot := sessionMirrorFixture("restart-steer-tab", "restart-steer-chat", "base")
	chat := chatFromSnapshot(snapshot, "restart-steer-tab")
	chat["messages"] = append(messageSlice(chat), map[string]any{
		"id": "orphan-steer", "role": "user", "content": "do not replay",
		"status": "pending", "at": nil, "events": []any{}, "steerState": "sending",
		"steerAnchor": map[string]any{"assistantMessageId": "client-a", "contentOffset": 0, "resultOffset": 0, "eventCount": 0},
	})
	if !store.Save(snapshot) {
		t.Fatal("save orphan steer fixture failed")
	}

	reloaded := newSessionStore(path)
	messages := messageSlice(chatFromSnapshot(reloaded.Get().(map[string]any), "restart-steer-tab"))
	var steer map[string]any
	for _, raw := range messages {
		message := mapFromAnyMain(raw)
		if fieldString(message, "id") == "orphan-steer" {
			steer = message
			break
		}
	}
	if fieldString(steer, "status") != "done" || fieldString(steer, "steerState") != "uncertain" || fieldString(steer, "content") != "do not replay" {
		t.Fatalf("orphaned steer = %#v", steer)
	}
}

func TestSessionStoreSteerReceiptJournalSurvivesCrashBeforeTurnEnd(t *testing.T) {
	stateDir := t.TempDir()
	path := filepath.Join(stateDir, sessionStateFilename)
	store := newSessionStore(path)
	if !store.Save(sessionMirrorFixture("receipt-crash-tab", "receipt-crash-chat", "base")) {
		t.Fatal("save receipt crash fixture failed")
	}
	store.PrepareTurn(map[string]any{
		"tabId": "receipt-crash-tab", "chatId": "receipt-crash-chat", "prompt": "base",
		"userMessageId": "client-u", "assistantMessageId": "client-a",
	})
	store.RecordJobEvent("job:event", map[string]any{"type": "start", "job": map[string]any{
		"id": "receipt-crash-job", "tabId": "receipt-crash-tab", "chatId": "receipt-crash-chat",
	}})
	if err := store.BeginLiveSteer("receipt-crash-tab", "receipt-crash-chat", "receipt-crash-steer", "persist me", "receipt-crash-tail", nil, map[string]any{
		"assistantMessageId": "client-a", "contentOffset": 0, "resultOffset": 0, "eventCount": 0, "deferUntilConsumed": true,
	}); err != nil {
		t.Fatalf("begin receipt crash steer: %v", err)
	}
	if err := store.AcknowledgeLiveSteer("receipt-crash-tab", "receipt-crash-chat", "receipt-crash-steer", "persist me", "accepted"); err != nil {
		t.Fatal(err)
	}
	store.RecordJobEvent("job:event", map[string]any{
		"type": "acp", "id": "receipt-crash-job", "event": map[string]any{
			"kind": "steer-consumed", "clientUserMessageId": "receipt-crash-steer",
		},
	})
	store.flushScheduledWrite()
	closeSessionStoreJournals(t, store)

	reloaded := newSessionStore(path)
	if err := reloaded.LoadError(); err != nil {
		t.Fatalf("reload receipt journal: %v", err)
	}
	messages := messageSlice(chatFromSnapshot(reloaded.Get().(map[string]any), "receipt-crash-tab"))
	var steer map[string]any
	for _, raw := range messages {
		message := mapFromAnyMain(raw)
		if fieldString(message, "id") == "receipt-crash-steer" {
			steer = message
			break
		}
	}
	if fieldString(steer, "steerState") != "applied" || fieldString(steer, "status") != "done" {
		t.Fatalf("recovered receipt steer = %#v", steer)
	}
	if tail := mapFromAnyMain(messages[len(messages)-1]); fieldString(tail, "status") != "failed" || fieldString(tail, "id") != "receipt-crash-tail" {
		t.Fatalf("orphaned assistant continuation was not settled honestly: %#v", messages)
	}
	if archive := loadChatArchive(stateDir, "receipt-crash-tab"); len(archive) < 3 || fieldString(mapFromAnyMain(archive[len(archive)-2]), "id") != "receipt-crash-steer" {
		t.Fatalf("recovered receipt archive = %#v", archive)
	}
}

func TestArchiveTurnPrefixesDoNotCountSteersAsNewTurns(t *testing.T) {
	records := []map[string]any{
		{"id": "u1", "role": "user", "content": "first"},
		{"id": "s1", "role": "user", "content": "steer", "steerAnchor": map[string]any{"assistantMessageId": "a1"}},
		{"id": "a1", "role": "assistant", "content": "answer"},
		{"id": "u2", "role": "user", "content": "second"},
		{"id": "a2", "role": "assistant", "content": "answer two"},
	}
	if got := countArchiveTurns(records); got != 2 {
		t.Fatalf("archive turn count = %d, want 2", got)
	}
	prefix, effective := archivePrefixThroughTurn(records, 1, true)
	if effective != 1 || len(prefix) != 3 || fieldString(prefix[1], "id") != "s1" || fieldString(prefix[2], "id") != "a1" {
		t.Fatalf("first turn prefix effective=%d records=%#v", effective, prefix)
	}
}

func TestSessionStorePersistsAndMergesDaemonOwnedTurn(t *testing.T) {
	stateDir := t.TempDir()
	path := filepath.Join(stateDir, sessionStateFilename)
	store := newSessionStore(path)
	stale := sessionMirrorFixture("tab-1", "chat-1", "offline result")
	if !store.Save(stale) {
		t.Fatal("initial session save failed")
	}

	store.PrepareTurn(map[string]any{
		"tabId": "tab-1", "chatId": "chat-1", "title": "Devin · Offline",
		"prompt": "offline result",
	})
	store.RecordJobEvent("job:event", map[string]any{
		"type": "start",
		"job": map[string]any{
			"id": "job-1", "tabId": "tab-1", "chatId": "chat-1",
			"title": "Devin · Offline", "status": "running", "startedAt": "2026-07-11T10:00:00Z",
		},
	})
	store.RecordJobEvent("job:event", map[string]any{
		"type": "data", "id": "job-1", "stream": "stdout", "chunk": "captured while ",
	})
	store.RecordJobEvent("job:event", map[string]any{
		"type": "acp", "id": "job-1",
		"event": map[string]any{
			"kind": "tool", "id": "tool-1", "title": "Read", "status": "pending", "input": "README.md",
		},
	})
	store.RecordJobEvent("job:event", map[string]any{
		"type": "acp", "id": "job-1",
		"event": map[string]any{
			"kind": "tool", "id": "tool-1", "title": "Read", "status": "completed", "output": "ok",
		},
	})
	store.RecordJobEvent("job:event", map[string]any{
		"type": "data", "id": "job-1", "stream": "stdout", "chunk": "closed",
	})
	store.RecordJobEvent("job:event", map[string]any{
		"type": "end",
		"job": map[string]any{
			"id": "job-1", "tabId": "tab-1", "chatId": "chat-1", "status": "done",
			"result": "captured while closed", "finishedAt": "2026-07-11T10:00:01Z", "stopReason": "end_turn",
		},
	})

	// A renderer that was already closing can flush its older blank snapshot
	// after the daemon captured output. The merge must keep the daemon copy.
	if !store.Save(stale) {
		t.Fatal("stale session merge failed")
	}
	snapshot := store.Get().(map[string]any)
	assistant := sessionAssistant(t, snapshot, "tab-1")
	if assistant["content"] != "captured while closed" || assistant["status"] != "done" || assistant["jobId"] != "job-1" {
		t.Fatalf("assistant after stale merge = %#v", assistant)
	}
	events := anySlice(assistant["events"])
	if len(events) != 1 {
		t.Fatalf("assistant events = %#v", events)
	}
	tool := mapFromAnyMain(events[0])
	if tool["kind"] != "tool" || tool["status"] != "completed" || tool["output"] != "ok" {
		t.Fatalf("merged tool event = %#v", tool)
	}

	reloaded := newSessionStore(path)
	if reloaded.LoadError() != nil {
		t.Fatalf("reload session: %v", reloaded.LoadError())
	}
	reloadedAssistant := sessionAssistant(t, reloaded.Get().(map[string]any), "tab-1")
	if reloadedAssistant["content"] != "captured while closed" || reloadedAssistant["status"] != "done" {
		t.Fatalf("reloaded assistant = %#v", reloadedAssistant)
	}

	archive := loadChatArchive(stateDir, "tab-1")
	if len(archive) != 2 {
		t.Fatalf("daemon archive records = %#v", archive)
	}
	if err := appendChatArchive(stateDir, "tab-1", archive); err != nil {
		t.Fatalf("idempotent archive append: %v", err)
	}
	if got := len(loadChatArchive(stateDir, "tab-1")); got != 2 {
		t.Fatalf("idempotent archive count = %d, want 2", got)
	}

	if matches, err := filepath.Glob(filepath.Join(stateDir, ".session-state-*.tmp")); err != nil || len(matches) != 0 {
		t.Fatalf("atomic temp files left behind: matches=%v err=%v", matches, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat session state: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("session state mode = %o, want 600", info.Mode().Perm())
	}
}

func TestSessionStorePersistsQueuedFollowUpAcrossReloadAndOrphanRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), sessionStateFilename)
	store := newSessionStore(path)
	snapshot := sessionMirrorFixture("tab-queue", "chat-queue", "active turn")
	chat := chatFromSnapshot(snapshot, "tab-queue")
	chat["queued"] = "first queued follow-up\n\nsecond queued follow-up"
	assistant := mapFromAnyMain(messageSlice(chat)[1])
	assistant["status"] = "running"
	if !store.Save(snapshot) {
		t.Fatal("save queued snapshot failed")
	}

	reloaded := newSessionStore(path)
	if reloaded.LoadError() != nil {
		t.Fatalf("reload queued snapshot: %v", reloaded.LoadError())
	}
	got := reloaded.Get().(map[string]any)
	reloadedChat := chatFromSnapshot(got, "tab-queue")
	if queued := fieldString(reloadedChat, "queued"); queued != "first queued follow-up\n\nsecond queued follow-up" {
		t.Fatalf("queued follow-up after reload = %q", queued)
	}
	reloadedAssistant := sessionAssistant(t, got, "tab-queue")
	if reloadedAssistant["status"] != "failed" {
		t.Fatalf("orphaned assistant status = %#v, want failed", reloadedAssistant["status"])
	}
}

func TestSessionStorePersistsRuntimeControlsPerChatWithoutMixing(t *testing.T) {
	path := filepath.Join(t.TempDir(), sessionStateFilename)
	store := newSessionStore(path)
	snapshot := sessionMirrorFixture("codex-tab", "codex-chat", "codex prompt")
	claude := sessionMirrorFixture("claude-tab", "claude-chat", "claude prompt")
	snapshot["chats"] = append(anySlice(snapshot["chats"]), anySlice(claude["chats"])[0])
	if !store.Save(snapshot) {
		t.Fatal("initial two-chat save failed")
	}

	if !store.UpdateChatControls("codex-tab", "codex-chat", "codex", "gpt-5.6-sol[xhigh]", "agent-full-access") {
		t.Fatal("persist codex controls failed")
	}
	if !store.UpdateChatControls("claude-tab", "claude-chat", "claude", "opus[1m][high]", "bypassPermissions") {
		t.Fatal("persist claude controls failed")
	}
	// job:start is the last authority before prompting and must patch only its
	// own chat, including effort carried inside the composite model id.
	store.PrepareTurn(map[string]any{
		"tabId": "codex-tab", "chatId": "codex-chat", "providerId": "codex",
		"modelId": "gpt-5.6-sol[max]", "modeId": "guardian", "prompt": "next codex turn",
	})

	reloaded := newSessionStore(path)
	if err := reloaded.LoadError(); err != nil {
		t.Fatalf("reload controls: %v", err)
	}
	got := reloaded.Get().(map[string]any)
	codex := chatFromSnapshot(got, "codex-tab")
	claudeChat := chatFromSnapshot(got, "claude-tab")
	if fieldString(codex, "providerId") != "codex" || fieldString(codex, "currentModelId") != "gpt-5.6-sol[max]" || fieldString(codex, "currentModeId") != "guardian" {
		t.Fatalf("codex controls after reload = provider=%q model=%q mode=%q", fieldString(codex, "providerId"), fieldString(codex, "currentModelId"), fieldString(codex, "currentModeId"))
	}
	if fieldString(claudeChat, "providerId") != "claude" || fieldString(claudeChat, "currentModelId") != "opus[1m][high]" || fieldString(claudeChat, "currentModeId") != "bypassPermissions" {
		t.Fatalf("claude controls were mixed = provider=%q model=%q mode=%q", fieldString(claudeChat, "providerId"), fieldString(claudeChat, "currentModelId"), fieldString(claudeChat, "currentModeId"))
	}
	t.Logf("trace per-chat controls codex=%s/%s/%s claude=%s/%s/%s",
		fieldString(codex, "providerId"), fieldString(codex, "currentModelId"), fieldString(codex, "currentModeId"),
		fieldString(claudeChat, "providerId"), fieldString(claudeChat, "currentModelId"), fieldString(claudeChat, "currentModeId"))
}

func TestSessionStoreDaemonControlRevisionRejectsStaleRendererSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), sessionStateFilename)
	store := newSessionStore(path)
	initial := sessionMirrorFixture("control-tab", "control-chat", "recover this chat")
	initialChat := chatFromSnapshot(initial, "control-tab")
	initialChat["providerId"] = "claude"
	initialChat["currentModelId"] = "claude-fable-5[1m][xhigh]"
	initialChat["currentModeId"] = "bypassPermissions"
	if !store.Save(initial) {
		t.Fatal("initial renderer save failed")
	}
	staleFull := cloneJSON(initial).(map[string]any)
	staleLean := cloneJSON(initial).(map[string]any)
	staleLean["_workassSave"] = "lean-payload-v2"

	if err := store.AgentConfigureChat(
		"control-tab", "control-chat", "/tmp", "codex",
		"gpt-5.6-sol[max]", "gpt-5.6-sol", "max", "agent-full-access",
	); err != nil {
		t.Fatalf("agent configure chat: %v", err)
	}
	assertControls := func(t *testing.T, providerID, modelID, modeID string, minRevision int) {
		t.Helper()
		chat := chatFromSnapshot(store.Get().(map[string]any), "control-tab")
		if fieldString(chat, "providerId") != providerID || fieldString(chat, "currentModelId") != modelID || fieldString(chat, "currentModeId") != modeID {
			t.Fatalf("controls = %q/%q/%q, want %q/%q/%q", fieldString(chat, "providerId"), fieldString(chat, "currentModelId"), fieldString(chat, "currentModeId"), providerID, modelID, modeID)
		}
		if revision := intValue(chat["runtimeControlRevision"]); revision < minRevision {
			t.Fatalf("runtime control revision = %d, want >= %d", revision, minRevision)
		}
	}
	assertControls(t, "codex", "gpt-5.6-sol[max]", "agent-full-access", 1)

	if !store.Save(staleFull) {
		t.Fatal("stale full renderer save failed")
	}
	assertControls(t, "codex", "gpt-5.6-sol[max]", "agent-full-access", 1)
	if !store.Save(staleLean) {
		t.Fatal("stale lean renderer save failed")
	}
	assertControls(t, "codex", "gpt-5.6-sol[max]", "agent-full-access", 1)

	// A renderer that has hydrated the daemon-issued revision remains allowed to
	// make the ordinary local selection that precedes a new session.
	fresh := cloneJSON(store.Get()).(map[string]any)
	freshChat := chatFromSnapshot(fresh, "control-tab")
	freshChat["providerId"] = "mock"
	freshChat["currentModelId"] = "mock-deterministic"
	freshChat["currentModeId"] = "ask"
	if !store.Save(fresh) {
		t.Fatal("current renderer control save failed")
	}
	assertControls(t, "mock", "mock-deterministic", "ask", 1)

	// A later daemon-side control commit advances the revision and makes that
	// once-current renderer snapshot stale as well.
	if !store.UpdateChatControls("control-tab", "control-chat", "codex", "gpt-5.6-sol[ultra]", "guardian") {
		t.Fatal("daemon runtime control update failed")
	}
	if !store.Save(fresh) {
		t.Fatal("second stale renderer save failed")
	}
	assertControls(t, "codex", "gpt-5.6-sol[ultra]", "guardian", 2)
}

func TestSessionStoreRedactsAndInterruptsOrphanedTurn(t *testing.T) {
	path := filepath.Join(t.TempDir(), sessionStateFilename)
	store := newSessionStore(path)
	snapshot := sessionMirrorFixture("tab-secret", "chat-secret", "token=top-secret")
	chat := chatFromSnapshot(snapshot, "tab-secret")
	assistant := mapFromAnyMain(messageSlice(chat)[1])
	assistant["status"] = "running"
	if !store.Save(snapshot) {
		t.Fatal("save running snapshot failed")
	}

	reloaded := newSessionStore(path)
	if reloaded.LoadError() != nil {
		t.Fatalf("reload orphan snapshot: %v", reloaded.LoadError())
	}
	got := sessionAssistant(t, reloaded.Get().(map[string]any), "tab-secret")
	if got["status"] != "failed" {
		t.Fatalf("orphan status = %#v", got)
	}
	if !strings.Contains(stringValue(got["content"]), "interrumpido") {
		t.Fatalf("orphan interruption message = %q", got["content"])
	}
	encoded, err := json.Marshal(reloaded.Get())
	if err != nil {
		t.Fatalf("marshal redacted snapshot: %v", err)
	}
	if strings.Contains(string(encoded), "top-secret") || !strings.Contains(string(encoded), "[redacted]") {
		t.Fatalf("secret was not redacted from snapshot: %s", encoded)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read session state: %v", err)
	}
	if strings.Contains(string(raw), "top-secret") {
		t.Fatalf("secret reached session-state.json: %s", raw)
	}
}

func TestSessionStoreRedactsLegacySnapshotAtLoadBoundary(t *testing.T) {
	path := filepath.Join(t.TempDir(), sessionStateFilename)
	raw := `{"v":1,"activeId":"legacy-tab","chats":[{"id":"legacy-tab","chatId":"legacy-chat","messages":[{"id":"legacy-user","role":"user","content":"token=legacy-secret","status":"done","events":[]}]}]}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatalf("write legacy snapshot: %v", err)
	}

	store := newSessionStore(path)
	if err := store.LoadError(); err != nil {
		t.Fatalf("load legacy snapshot: %v", err)
	}
	persisted, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read normalized snapshot: %v", err)
	}
	if strings.Contains(string(persisted), "legacy-secret") || !strings.Contains(string(persisted), "token=[redacted]") {
		t.Fatalf("legacy snapshot was not redacted and rewritten: %s", persisted)
	}

	if !store.UpdateChatControls("legacy-tab", "legacy-chat", "mock", "api_key=model-secret", "password=mode-secret") {
		t.Fatal("update secret-shaped controls failed")
	}
	persisted, err = os.ReadFile(path)
	if err != nil {
		t.Fatalf("read control snapshot: %v", err)
	}
	if strings.Contains(string(persisted), "model-secret") || strings.Contains(string(persisted), "mode-secret") {
		t.Fatalf("secret-shaped controls reached disk: %s", persisted)
	}
}

func TestSessionStoreLargeSnapshotLockStaysWithinFrameBudget(t *testing.T) {
	path := filepath.Join(t.TempDir(), sessionStateFilename)
	store := newSessionStore(path)
	snapshot := sessionMirrorFixture("large-tab", "large-chat", "large prompt")
	snapshot["streamFixturePadding"] = strings.Repeat("safe streaming state ", 100_000)
	snapshot["probe"] = "api_key=large-state-secret"
	if !store.Save(snapshot) {
		t.Fatal("save large snapshot failed")
	}

	const runs = 20
	samples := make([]time.Duration, 0, runs)
	var encoded []byte
	for range runs {
		started := time.Now()
		store.mu.Lock()
		data, _, err := store.snapshotBytesLocked()
		store.mu.Unlock()
		if err != nil {
			t.Fatalf("snapshot large state: %v", err)
		}
		encoded = data
		samples = append(samples, time.Since(started))
	}
	if len(encoded) < 1_800_000 {
		t.Fatalf("large fixture bytes = %d, want at least 1.8MB", len(encoded))
	}
	if strings.Contains(string(encoded), "large-state-secret") || !strings.Contains(string(encoded), "api_key=[redacted]") {
		t.Fatal("large snapshot bypassed ingress redaction")
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	var total time.Duration
	for _, sample := range samples {
		total += sample
	}
	p95 := samples[runs*95/100]
	t.Logf("large snapshot bytes=%d runs=%d mean=%s p50=%s p95=%s max=%s", len(encoded), runs, (total / runs).Round(time.Microsecond), samples[runs/2].Round(time.Microsecond), p95.Round(time.Microsecond), samples[runs-1].Round(time.Microsecond))
	if p95 > 25*time.Millisecond {
		t.Fatalf("large snapshot held the streaming mutex for %s at p95, want <=25ms", p95)
	}
}

func TestSessionStoreRepeatedPromptCreatesNewTurn(t *testing.T) {
	store := newSessionStore(filepath.Join(t.TempDir(), sessionStateFilename))
	prompt := "repeat me"
	if !store.Save(sessionMirrorFixture("repeat-tab", "repeat-chat", prompt)) {
		t.Fatal("save repeated-prompt fixture failed")
	}
	store.PrepareTurn(map[string]any{"tabId": "repeat-tab", "chatId": "repeat-chat", "prompt": prompt})
	store.RecordJobEvent("job:event", map[string]any{
		"type": "start", "job": map[string]any{"id": "repeat-1", "tabId": "repeat-tab", "chatId": "repeat-chat", "status": "running"},
	})
	store.RecordJobEvent("job:event", map[string]any{
		"type": "end", "job": map[string]any{"id": "repeat-1", "status": "done", "result": "first answer", "finishedAt": "2026-07-11T10:00:01Z"},
	})

	// No intervening client save: the daemon must not mistake the completed
	// first pair for an optimistic placeholder for the repeated request.
	store.PrepareTurn(map[string]any{"tabId": "repeat-tab", "chatId": "repeat-chat", "prompt": prompt})
	snapshot := store.Get().(map[string]any)
	chat := chatFromSnapshot(snapshot, "repeat-tab")
	messages := messageSlice(chat)
	if len(messages) != 4 {
		t.Fatalf("repeated prompt messages = %#v", messages)
	}
	firstAssistant := mapFromAnyMain(messages[1])
	secondAssistant := mapFromAnyMain(messages[3])
	if firstAssistant["content"] != "first answer" || firstAssistant["status"] != "done" || secondAssistant["status"] != "running" {
		t.Fatalf("repeated prompt turn states first=%#v second=%#v", firstAssistant, secondAssistant)
	}
}

func TestSessionStoreAdoptsRendererTurnIDsWithoutCrossChatDuplication(t *testing.T) {
	store := newSessionStore(filepath.Join(t.TempDir(), sessionStateFilename))
	store.PrepareTurn(map[string]any{
		"tabId": "tab-a", "chatId": "chat-a", "prompt": "first chat",
		"userMessageId": "renderer-u-a", "assistantMessageId": "renderer-a-a",
	})
	store.PrepareTurn(map[string]any{
		"tabId": "tab-b", "chatId": "chat-b", "prompt": "second chat",
		"userMessageId": "renderer-u-b", "assistantMessageId": "renderer-a-b",
	})

	snapshot := store.Get().(map[string]any)
	chatA := chatFromSnapshot(snapshot, "tab-a")
	chatB := chatFromSnapshot(snapshot, "tab-b")
	if chatA == nil || chatB == nil {
		t.Fatalf("isolated chats missing: %#v", snapshot["chats"])
	}
	ids := func(chat map[string]any) []string {
		messages := messageSlice(chat)
		out := make([]string, 0, len(messages))
		for _, raw := range messages {
			out = append(out, fieldString(mapFromAnyMain(raw), "id"))
		}
		return out
	}
	if got := strings.Join(ids(chatA), ","); got != "renderer-u-a,renderer-a-a" {
		t.Fatalf("chat A message ids = %q", got)
	}
	if got := strings.Join(ids(chatB), ","); got != "renderer-u-b,renderer-a-b" {
		t.Fatalf("chat B message ids = %q", got)
	}
}

func TestSessionStoreStreamingPersistenceDoesNotBlockBroadcast(t *testing.T) {
	path := filepath.Join(t.TempDir(), sessionStateFilename)
	store := newSessionStore(path)
	// Keep the scheduled write deterministic: the test triggers it explicitly.
	store.streamPersistInterval = time.Hour
	if !store.Save(sessionMirrorFixture("stream-tab", "stream-chat", "stream prompt")) {
		t.Fatal("initial session save failed")
	}
	store.PrepareTurn(map[string]any{"tabId": "stream-tab", "chatId": "stream-chat", "prompt": "stream prompt"})
	store.RecordJobEvent("job:event", map[string]any{
		"type": "start",
		"job":  map[string]any{"id": "stream-job", "tabId": "stream-tab", "chatId": "stream-chat", "status": "running"},
	})

	store.RecordJobEvent("job:event", map[string]any{
		"type": "data", "id": "stream-job", "stream": "stdout", "chunk": "arrives without fsync",
	})
	if got := stringValue(sessionAssistant(t, store.Get().(map[string]any), "stream-tab")["content"]); got != "arrives without fsync" {
		t.Fatalf("in-memory streaming content = %q", got)
	}
	rawBefore, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read pre-flush state: %v", err)
	}
	if strings.Contains(string(rawBefore), "arrives without fsync") {
		t.Fatal("streaming event synchronously rewrote session-state.json")
	}

	store.flushScheduledWrite()
	rawAfter, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state after journal flush: %v", err)
	}
	if strings.Contains(string(rawAfter), "arrives without fsync") {
		t.Fatal("streaming journal flush rewrote the canonical session snapshot")
	}
	journals := sessionJournalFiles(t, store)
	if len(journals) != 1 {
		t.Fatalf("streaming journal files = %v, want one", journals)
	}
	journalRaw, err := os.ReadFile(journals[0])
	if err != nil {
		t.Fatalf("read streaming journal: %v", err)
	}
	if !strings.Contains(string(journalRaw), "arrives without fsync") {
		t.Fatal("scheduled journal sync did not preserve the partial output")
	}

	store.RecordJobEvent("job:event", map[string]any{
		"type": "data", "id": "stream-job", "stream": "stdout", "chunk": " and final",
	})
	store.RecordJobEvent("job:event", map[string]any{
		"type": "end",
		"job": map[string]any{
			"id": "stream-job", "status": "done", "result": "arrives without fsync and final",
			"finishedAt": "2026-07-12T02:00:00Z", "stopReason": "end_turn",
		},
	})
	rawEnd, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read end state: %v", err)
	}
	if !strings.Contains(string(rawEnd), "arrives without fsync and final") {
		t.Fatal("turn end did not synchronously persist final output")
	}
	if journals := sessionJournalFiles(t, store); len(journals) != 0 {
		t.Fatalf("turn end left recovery journals behind: %v", journals)
	}
	store.mu.Lock()
	timer := store.persistTimer
	dirty := store.persistDirty
	store.mu.Unlock()
	if timer != nil || dirty {
		t.Fatalf("turn end left scheduled persistence behind: timer=%v dirty=%v", timer != nil, dirty)
	}
}

func TestSessionStoreScheduledJournalSyncCannotRecreateCommittedTurn(t *testing.T) {
	path := filepath.Join(t.TempDir(), sessionStateFilename)
	store := newSessionStore(path)
	store.streamPersistInterval = time.Hour
	if !store.Save(sessionMirrorFixture("ordered-tab", "ordered-chat", "ordered prompt")) {
		t.Fatal("initial session save failed")
	}
	store.PrepareTurn(map[string]any{"tabId": "ordered-tab", "chatId": "ordered-chat", "prompt": "ordered prompt"})
	store.RecordJobEvent("job:event", map[string]any{
		"type": "start",
		"job":  map[string]any{"id": "ordered-job", "tabId": "ordered-tab", "chatId": "ordered-chat", "status": "running"},
	})
	store.RecordJobEvent("job:event", map[string]any{
		"type": "data", "id": "ordered-job", "stream": "stdout", "chunk": "older partial",
	})

	store.flushScheduledWrite()
	store.RecordJobEvent("job:event", map[string]any{
		"type": "end",
		"job": map[string]any{
			"id": "ordered-job", "status": "done", "result": "newer final",
			"finishedAt": "2026-07-12T02:00:00Z", "stopReason": "end_turn",
		},
	})
	// A timer callback that was already queued before terminal cleanup must be a
	// no-op; it may not recreate the removed journal or rewrite an older mirror.
	store.flushScheduledWrite()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read final state: %v", err)
	}
	if !strings.Contains(string(raw), "newer final") || strings.Contains(string(raw), "older partial") {
		t.Fatalf("stale journal work overwrote final state: %s", raw)
	}
	if journals := sessionJournalFiles(t, store); len(journals) != 0 {
		t.Fatalf("stale journal work recreated committed sidecar: %v", journals)
	}
}

func TestSessionStorePersistsTypedFinalAnswerAcrossMirrorArchiveAndRestart(t *testing.T) {
	stateDir := t.TempDir()
	path := filepath.Join(stateDir, sessionStateFilename)
	store := newSessionStore(path)
	if !store.Save(sessionMirrorFixture("typed-tab", "typed-chat", "typed result")) {
		t.Fatal("save typed fixture")
	}
	store.PrepareTurn(map[string]any{"tabId": "typed-tab", "chatId": "typed-chat", "prompt": "typed result"})
	store.RecordJobEvent("job:event", map[string]any{
		"type": "start", "job": map[string]any{"id": "typed-job", "tabId": "typed-tab", "chatId": "typed-chat", "status": "running"},
	})
	store.RecordJobEvent("job:event", map[string]any{
		"type": "data", "id": "typed-job", "stream": "stdout", "phase": "commentary", "chunk": "working notes",
	})
	store.RecordJobEvent("job:event", map[string]any{
		"type": "data", "id": "typed-job", "stream": "stdout", "phase": "final_answer", "chunk": "final report",
	})
	live := sessionAssistant(t, store.Get().(map[string]any), "typed-tab")
	if fieldString(live, "content") != "working notes" || fieldString(live, "result") != "final report" {
		t.Fatalf("live typed assistant = %#v", live)
	}
	store.RecordJobEvent("job:event", map[string]any{
		"type": "end", "job": map[string]any{
			"id": "typed-job", "tabId": "typed-tab", "chatId": "typed-chat", "status": "done",
			"result": "working notesfinal report", "finishedAt": "2026-07-14T18:00:00Z", "stopReason": "end_turn",
		},
	})

	reloaded := newSessionStore(path)
	if err := reloaded.LoadError(); err != nil {
		t.Fatalf("reload typed session: %v", err)
	}
	got := sessionAssistant(t, reloaded.Get().(map[string]any), "typed-tab")
	if fieldString(got, "content") != "working notes" || fieldString(got, "result") != "final report" || fieldString(got, "status") != "done" {
		t.Fatalf("reloaded typed assistant = %#v", got)
	}
	archive := loadChatArchive(stateDir, "typed-tab")
	if len(archive) != 2 {
		t.Fatalf("typed archive records = %#v", archive)
	}
	archivedAssistant := mapFromAnyMain(archive[1])
	if fieldString(archivedAssistant, "content") != "working notes" || fieldString(archivedAssistant, "result") != "final report" {
		t.Fatalf("archived typed assistant = %#v", archivedAssistant)
	}
}

func TestSessionStoreKeepsPhaseLessProviderOutputInOrdinaryAssistantContent(t *testing.T) {
	stateDir := t.TempDir()
	path := filepath.Join(stateDir, sessionStateFilename)
	store := newSessionStore(path)
	if !store.Save(sessionMirrorFixture("plain-tab", "plain-chat", "plain response")) {
		t.Fatal("save phase-less fixture")
	}
	store.PrepareTurn(map[string]any{"tabId": "plain-tab", "chatId": "plain-chat", "prompt": "plain response"})
	store.RecordJobEvent("job:event", map[string]any{
		"type": "start", "job": map[string]any{"id": "plain-job", "tabId": "plain-tab", "chatId": "plain-chat", "status": "running"},
	})
	store.RecordJobEvent("job:event", map[string]any{
		"type": "data", "id": "plain-job", "stream": "stdout", "chunk": "provider-authored response",
	})
	live := sessionAssistant(t, store.Get().(map[string]any), "plain-tab")
	if fieldString(live, "content") != "provider-authored response" || fieldString(live, "result") != "" {
		t.Fatalf("live phase-less assistant = %#v", live)
	}
	store.RecordJobEvent("job:event", map[string]any{
		"type": "end", "job": map[string]any{
			"id": "plain-job", "tabId": "plain-tab", "chatId": "plain-chat", "status": "done",
			"result": "provider-authored response", "finishedAt": "2026-07-14T18:30:00Z", "stopReason": "end_turn",
		},
	})

	reloaded := newSessionStore(path)
	if err := reloaded.LoadError(); err != nil {
		t.Fatalf("reload phase-less session: %v", err)
	}
	got := sessionAssistant(t, reloaded.Get().(map[string]any), "plain-tab")
	if fieldString(got, "content") != "provider-authored response" || fieldString(got, "result") != "" || fieldString(got, "status") != "done" {
		t.Fatalf("reloaded phase-less assistant = %#v", got)
	}
	archive := loadChatArchive(stateDir, "plain-tab")
	if len(archive) != 2 {
		t.Fatalf("phase-less archive records = %#v", archive)
	}
	archivedAssistant := mapFromAnyMain(archive[1])
	if fieldString(archivedAssistant, "content") != "provider-authored response" || fieldString(archivedAssistant, "result") != "" {
		t.Fatalf("archived phase-less assistant = %#v", archivedAssistant)
	}
}

func TestSessionStoreJournalRecoversTypedFinalAnswerWithoutFlattening(t *testing.T) {
	stateDir := t.TempDir()
	path := filepath.Join(stateDir, sessionStateFilename)
	store := newSessionStore(path)
	if !store.Save(sessionMirrorFixture("typed-journal-tab", "typed-journal-chat", "recover typed")) {
		t.Fatal("save typed journal fixture")
	}
	store.PrepareTurn(map[string]any{"tabId": "typed-journal-tab", "chatId": "typed-journal-chat", "prompt": "recover typed"})
	store.RecordJobEvent("job:event", map[string]any{
		"type": "start", "job": map[string]any{"id": "typed-journal-job", "tabId": "typed-journal-tab", "chatId": "typed-journal-chat", "status": "running"},
	})
	store.RecordJobEvent("job:event", map[string]any{
		"type": "data", "id": "typed-journal-job", "stream": "stdout", "phase": "commentary", "chunk": "journal notes",
	})
	store.RecordJobEvent("job:event", map[string]any{
		"type": "data", "id": "typed-journal-job", "stream": "stdout", "phase": "final_answer", "chunk": "journal final",
	})
	store.flushScheduledWrite()
	closeSessionStoreJournals(t, store)

	reloaded := newSessionStore(path)
	if err := reloaded.LoadError(); err != nil {
		t.Fatalf("reload typed journal: %v", err)
	}
	got := sessionAssistant(t, reloaded.Get().(map[string]any), "typed-journal-tab")
	if fieldString(got, "content") != "journal notes" || fieldString(got, "result") != "journal final" || fieldString(got, "status") != "failed" {
		t.Fatalf("recovered typed journal assistant = %#v", got)
	}
	if journals := sessionJournalFiles(t, reloaded); len(journals) != 0 {
		t.Fatalf("typed recovered journal was not retired: %v", journals)
	}
}

func TestSessionStoreEmptyCancellationUsesOnlyQuietStatusStamp(t *testing.T) {
	stateDir := t.TempDir()
	path := filepath.Join(stateDir, sessionStateFilename)
	store := newSessionStore(path)
	if !store.Save(sessionMirrorFixture("cancel-tab", "cancel-chat", "stop cleanly")) {
		t.Fatal("save cancellation fixture")
	}
	store.PrepareTurn(map[string]any{"tabId": "cancel-tab", "chatId": "cancel-chat", "prompt": "stop cleanly"})
	store.RecordJobEvent("job:event", map[string]any{
		"type": "start", "job": map[string]any{"id": "cancel-job", "tabId": "cancel-tab", "chatId": "cancel-chat", "status": "running"},
	})
	store.RecordJobEvent("job:event", map[string]any{
		"type": "end", "job": map[string]any{
			"id": "cancel-job", "tabId": "cancel-tab", "chatId": "cancel-chat", "status": "failed",
			"code": 130, "stopReason": "cancelled", "finishedAt": "2026-07-14T18:05:00Z",
		},
	})

	assistant := sessionAssistant(t, store.Get().(map[string]any), "cancel-tab")
	if fieldString(assistant, "status") != "cancelled" || fieldString(assistant, "content") != "" || fieldString(assistant, "result") != "" {
		t.Fatalf("empty cancellation should use only status chrome: %#v", assistant)
	}
	archive := loadChatArchive(stateDir, "cancel-tab")
	if len(archive) != 2 {
		t.Fatalf("cancelled turn archive records = %#v", archive)
	}
	archivedAssistant := mapFromAnyMain(archive[1])
	if fieldString(archivedAssistant, "status") != "cancelled" || fieldString(archivedAssistant, "content") != "" {
		t.Fatalf("archived cancellation should remain text-free: %#v", archivedAssistant)
	}
}

func TestSessionStoreJournalReplaysOrderedDataAndACPThenInterruptsOrphan(t *testing.T) {
	stateDir := t.TempDir()
	path := filepath.Join(stateDir, sessionStateFilename)
	store := newSessionStore(path)
	if !store.Save(sessionMirrorFixture("journal-tab", "journal-chat", "recover this")) {
		t.Fatal("save journal fixture")
	}
	store.PrepareTurn(map[string]any{"tabId": "journal-tab", "chatId": "journal-chat", "prompt": "recover this"})
	store.RecordJobEvent("job:event", map[string]any{
		"type": "start", "job": map[string]any{"id": "journal-job", "tabId": "journal-tab", "chatId": "journal-chat", "status": "running"},
	})
	store.RecordJobEvent("job:event", map[string]any{
		"type": "data", "id": "journal-job", "stream": "stdout", "chunk": "alpha",
	})
	store.RecordJobEvent("job:event", map[string]any{
		"type": "acp", "id": "journal-job", "event": map[string]any{
			"kind": "tool", "id": "journal-tool", "title": "Read", "status": "pending", "input": "README.md",
		},
	})
	store.RecordJobEvent("job:event", map[string]any{
		"type": "data", "id": "journal-job", "stream": "stdout", "chunk": "omega",
	})
	store.RecordJobEvent("job:event", map[string]any{
		"type": "acp", "id": "journal-job", "event": map[string]any{
			"kind": "tool", "id": "journal-tool", "title": "Read", "status": "completed", "output": "ok",
		},
	})
	store.flushScheduledWrite()
	closeSessionStoreJournals(t, store)

	reloaded := newSessionStore(path)
	if err := reloaded.LoadError(); err != nil {
		t.Fatalf("reload ordered journal: %v", err)
	}
	assistant := sessionAssistant(t, reloaded.Get().(map[string]any), "journal-tab")
	if got := fieldString(assistant, "content"); got != "alphaomega" {
		t.Fatalf("recovered journal content = %q", got)
	}
	if got := fieldString(assistant, "status"); got != "failed" {
		t.Fatalf("recovered orphan status = %q", got)
	}
	events := anySlice(assistant["events"])
	if len(events) != 1 {
		t.Fatalf("recovered journal events = %#v", events)
	}
	tool := mapFromAnyMain(events[0])
	if fieldString(tool, "status") != "completed" || fieldString(tool, "output") != "ok" || intValue(tool["at"]) != len("alpha") {
		t.Fatalf("recovered tool event = %#v", tool)
	}
	if journals := sessionJournalFiles(t, reloaded); len(journals) != 0 {
		t.Fatalf("recovered journal was not retired: %v", journals)
	}
	if archive := loadChatArchive(stateDir, "journal-tab"); len(archive) != 2 {
		t.Fatalf("recovered journal archive = %#v", archive)
	}
}

func TestSessionStorePreparedTurnJournalSurvivesCrashBeforeStart(t *testing.T) {
	stateDir := t.TempDir()
	path := filepath.Join(stateDir, sessionStateFilename)
	store := newSessionStore(path)
	store.PrepareTurn(map[string]any{
		"tabId": "prepared-tab", "chatId": "prepared-chat", "title": "Prepared",
		"providerId": "mock", "modelId": "mock-model", "modeId": "ask",
		"prompt": "prepared prompt", "userMessageId": "renderer-user", "assistantMessageId": "renderer-assistant",
		"images": []any{map[string]any{"mimeType": "image/png", "data": "prepared-image"}},
	})
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("prepare rewrote canonical session state: err=%v", err)
	}
	if journals := sessionJournalFiles(t, store); len(journals) != 1 {
		t.Fatalf("prepared journals = %v, want one", journals)
	}
	closeSessionStoreJournals(t, store)

	reloaded := newSessionStore(path)
	if err := reloaded.LoadError(); err != nil {
		t.Fatalf("reload prepared journal: %v", err)
	}
	snapshot := reloaded.Get().(map[string]any)
	chat := chatFromSnapshot(snapshot, "prepared-tab")
	if chat == nil || fieldString(chat, "chatId") != "prepared-chat" || fieldString(chat, "providerId") != "mock" || fieldString(chat, "currentModelId") != "mock-model" || fieldString(chat, "currentModeId") != "ask" {
		t.Fatalf("prepared chat recovery = %#v", chat)
	}
	messages := messageSlice(chat)
	if len(messages) != 2 || fieldString(mapFromAnyMain(messages[0]), "id") != "renderer-user" || fieldString(mapFromAnyMain(messages[1]), "id") != "renderer-assistant" {
		t.Fatalf("prepared message identities = %#v", messages)
	}
	image := mapFromAnyMain(anySlice(mapFromAnyMain(messages[0])["images"])[0])
	if image["data"] != "prepared-image" {
		t.Fatalf("prepared image recovery = %#v", image)
	}
	assistant := mapFromAnyMain(messages[1])
	if fieldString(assistant, "status") != "failed" || !strings.Contains(fieldString(assistant, "content"), "interrumpido") {
		t.Fatalf("prepared orphan recovery = %#v", assistant)
	}
}

func TestSessionStoreRedactsBeforeJournalDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), sessionStateFilename)
	store := newSessionStore(path)
	if !store.Save(sessionMirrorFixture("redact-tab", "redact-chat", "safe prompt")) {
		t.Fatal("save redaction fixture")
	}
	store.PrepareTurn(map[string]any{"tabId": "redact-tab", "chatId": "redact-chat", "prompt": "safe prompt"})
	store.RecordJobEvent("job:event", map[string]any{
		"type": "start", "job": map[string]any{"id": "redact-job", "tabId": "redact-tab", "chatId": "redact-chat", "status": "running"},
	})
	store.RecordJobEvent("job:event", map[string]any{
		"type": "data", "id": "redact-job", "stream": "stdout", "chunk": "token=journal-secret",
	})
	store.RecordJobEvent("job:event", map[string]any{
		"type": "acp", "id": "redact-job", "event": map[string]any{
			"kind": "tool", "id": "redact-tool", "title": "Read", "status": "completed", "output": "api_key=tool-secret",
		},
	})
	store.flushScheduledWrite()
	journals := sessionJournalFiles(t, store)
	if len(journals) != 1 {
		t.Fatalf("redaction journals = %v", journals)
	}
	raw, err := os.ReadFile(journals[0])
	if err != nil {
		t.Fatalf("read redaction journal: %v", err)
	}
	if strings.Contains(string(raw), "journal-secret") || strings.Contains(string(raw), "tool-secret") || !strings.Contains(string(raw), "[redacted]") {
		t.Fatalf("secret reached journal: %s", raw)
	}
	closeSessionStoreJournals(t, store)
	reloaded := newSessionStore(path)
	encoded, err := json.Marshal(reloaded.Get())
	if err != nil {
		t.Fatalf("marshal recovered redacted state: %v", err)
	}
	if strings.Contains(string(encoded), "journal-secret") || strings.Contains(string(encoded), "tool-secret") {
		t.Fatalf("secret survived journal replay: %s", encoded)
	}
}

func TestSessionStoreJournalIgnoresOnlyTornFinalRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), sessionStateFilename)
	store := newSessionStore(path)
	if !store.Save(sessionMirrorFixture("torn-tab", "torn-chat", "torn prompt")) {
		t.Fatal("save torn fixture")
	}
	store.PrepareTurn(map[string]any{"tabId": "torn-tab", "chatId": "torn-chat", "prompt": "torn prompt"})
	store.RecordJobEvent("job:event", map[string]any{
		"type": "start", "job": map[string]any{"id": "torn-job", "tabId": "torn-tab", "chatId": "torn-chat", "status": "running"},
	})
	store.RecordJobEvent("job:event", map[string]any{
		"type": "data", "id": "torn-job", "stream": "stdout", "chunk": "complete prefix",
	})
	store.flushScheduledWrite()
	journals := sessionJournalFiles(t, store)
	closeSessionStoreJournals(t, store)
	file, err := os.OpenFile(journals[0], os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open torn journal: %v", err)
	}
	if _, err := file.WriteString(`{"v":1,"seq":4,"kind":"data"`); err != nil {
		file.Close()
		t.Fatalf("append torn record: %v", err)
	}
	_ = file.Close()

	reloaded := newSessionStore(path)
	if err := reloaded.LoadError(); err != nil {
		t.Fatalf("reload torn journal: %v", err)
	}
	assistant := sessionAssistant(t, reloaded.Get().(map[string]any), "torn-tab")
	if fieldString(assistant, "content") != "complete prefix" || fieldString(assistant, "status") != "failed" {
		t.Fatalf("torn journal recovery = %#v", assistant)
	}
}

func TestSessionStoreJournalRecoversSyncedTerminalBeforeCanonicalFold(t *testing.T) {
	stateDir := t.TempDir()
	path := filepath.Join(stateDir, sessionStateFilename)
	store := newSessionStore(path)
	if !store.Save(sessionMirrorFixture("terminal-tab", "terminal-chat", "terminal prompt")) {
		t.Fatal("save terminal fixture")
	}
	store.PrepareTurn(map[string]any{"tabId": "terminal-tab", "chatId": "terminal-chat", "prompt": "terminal prompt"})
	store.RecordJobEvent("job:event", map[string]any{
		"type": "start", "job": map[string]any{"id": "terminal-job", "tabId": "terminal-tab", "chatId": "terminal-chat", "status": "running"},
	})
	store.RecordJobEvent("job:event", map[string]any{
		"type": "data", "id": "terminal-job", "stream": "stdout", "chunk": "streamed result",
	})
	end := map[string]any{
		"id": "terminal-job", "status": "done", "result": "",
		"finishedAt": "2026-07-13T10:00:00Z", "stopReason": "end_turn",
	}
	store.mu.Lock()
	job := store.jobs["terminal-job"]
	if err := store.appendJournalRecordLocked(job, map[string]any{"kind": "end", "job": end}, true); err != nil {
		store.mu.Unlock()
		t.Fatalf("sync terminal journal record: %v", err)
	}
	store.mu.Unlock()
	// Simulate a crash after the terminal journal fsync but before the canonical
	// snapshot/archive fold and journal removal.
	closeSessionStoreJournals(t, store)

	reloaded := newSessionStore(path)
	if err := reloaded.LoadError(); err != nil {
		t.Fatalf("recover synced terminal journal: %v", err)
	}
	assistant := sessionAssistant(t, reloaded.Get().(map[string]any), "terminal-tab")
	if fieldString(assistant, "content") != "streamed result" || fieldString(assistant, "status") != "done" {
		t.Fatalf("recovered terminal = %#v", assistant)
	}
	if archive := loadChatArchive(stateDir, "terminal-tab"); len(archive) != 2 {
		t.Fatalf("recovered terminal archive = %#v", archive)
	}
	if journals := sessionJournalFiles(t, reloaded); len(journals) != 0 {
		t.Fatalf("terminal recovery left journal: %v", journals)
	}
}

func TestSessionStoreQuarantinesAcpFirstJournalAndContinuesRecovery(t *testing.T) {
	stateDir := t.TempDir()
	path := filepath.Join(stateDir, sessionStateFilename)
	store := newSessionStore(path)
	snapshot := sessionMirrorFixture("good-tab", "good-chat", "recover good")
	snapshot["chats"] = append(anySlice(snapshot["chats"]), map[string]any{
		"id": "orphan-tab", "chatId": "orphan-chat", "title": "Orphan", "titleLocked": true,
		"group": nil, "cwd": nil, "currentModelId": nil, "currentModeId": nil,
		"draft": "", "providerId": "mock",
		"messages": []any{
			map[string]any{"id": "orphan-user", "role": "user", "content": "orphan", "status": "done", "at": "2026-07-24T10:00:00Z", "events": []any{}},
			map[string]any{"id": "orphan-assistant", "role": "assistant", "content": "", "status": "running", "at": nil, "events": []any{}},
		},
	})
	if !store.Save(snapshot) {
		t.Fatal("save recovery fixture")
	}
	store.PrepareTurn(map[string]any{"tabId": "good-tab", "chatId": "good-chat", "prompt": "recover good"})
	store.RecordJobEvent("job:event", map[string]any{
		"type": "start", "job": map[string]any{"id": "good-job", "tabId": "good-tab", "chatId": "good-chat", "status": "running"},
	})
	store.RecordJobEvent("job:event", map[string]any{
		"type": "data", "id": "good-job", "stream": "stdout", "chunk": "good recovered",
	})
	store.flushScheduledWrite()
	goodJournals := sessionJournalFiles(t, store)
	if len(goodJournals) != 1 {
		t.Fatalf("good journals = %v", goodJournals)
	}
	closeSessionStoreJournals(t, store)

	goodName := filepath.Base(goodJournals[0])
	badTurnID := ""
	badPath := ""
	for index := 0; index < 100000; index++ {
		candidate := "acp-first-" + fmt.Sprint(index)
		candidatePath := store.journalPath(candidate)
		if filepath.Base(candidatePath) > goodName {
			badTurnID, badPath = candidate, candidatePath
			break
		}
	}
	if badTurnID == "" {
		t.Fatal("could not choose an acp-first journal that sorts after the good journal")
	}
	badRecord := map[string]any{
		"v": json.Number("1"), "seq": json.Number("1"), "turnId": badTurnID,
		"kind": "acp", "jobId": "poisoned-job", "event": map[string]any{"kind": "thinking", "text": "poisoned"},
	}
	badData, err := json.Marshal(badRecord)
	if err != nil {
		t.Fatalf("marshal acp-first journal: %v", err)
	}
	if err := os.WriteFile(badPath, append(badData, '\n'), 0o600); err != nil {
		t.Fatalf("write acp-first journal: %v", err)
	}

	reloaded := newSessionStore(path)
	var quarantineErr *sessionJournalQuarantineError
	if err := reloaded.LoadError(); !errors.As(err, &quarantineErr) || quarantineErr.Count != 1 {
		t.Fatalf("load error = %v, want one quarantined journal", err)
	}
	goodAssistant := sessionAssistant(t, reloaded.Get().(map[string]any), "good-tab")
	if fieldString(goodAssistant, "content") != "good recovered" || fieldString(goodAssistant, "status") != "failed" {
		t.Fatalf("good journal did not fully recover before later poison: %#v", goodAssistant)
	}
	orphanAssistant := sessionAssistant(t, reloaded.Get().(map[string]any), "orphan-tab")
	if fieldString(orphanAssistant, "status") != "failed" || !strings.Contains(fieldString(orphanAssistant, "content"), "interrumpido") {
		t.Fatalf("post-load orphan interruption did not run: %#v", orphanAssistant)
	}
	quarantinedPath := filepath.Join(stateDir, sessionJournalDirname, sessionJournalQuarantineDir, filepath.Base(badPath))
	if _, err := os.Stat(quarantinedPath); err != nil {
		t.Fatalf("quarantined journal missing: %v", err)
	}
	reason, err := os.ReadFile(quarantinedPath + ".reason")
	if err != nil || !strings.Contains(string(reason), "does not start with prepare") {
		t.Fatalf("quarantine reason = %q, err=%v", reason, err)
	}
}

func TestSessionStoreDropsLateEventsAfterJournalRemoval(t *testing.T) {
	path := filepath.Join(t.TempDir(), sessionStateFilename)
	store := newSessionStore(path)
	if !store.Save(sessionMirrorFixture("late-tab", "late-chat", "finish")) {
		t.Fatal("save late-event fixture")
	}
	store.PrepareTurn(map[string]any{"tabId": "late-tab", "chatId": "late-chat", "prompt": "finish"})
	store.RecordJobEvent("job:event", map[string]any{
		"type": "start", "job": map[string]any{"id": "late-job", "tabId": "late-tab", "chatId": "late-chat", "status": "running"},
	})
	store.RecordJobEvent("job:event", map[string]any{
		"type": "end", "job": map[string]any{
			"id": "late-job", "tabId": "late-tab", "chatId": "late-chat", "status": "done",
			"result": "finished", "finishedAt": "2026-07-24T10:01:00Z", "stopReason": "end_turn",
		},
	})
	store.RecordJobEvent("job:event", map[string]any{"type": "data", "id": "late-job", "stream": "stdout", "chunk": " late"})
	store.RecordJobEvent("job:event", map[string]any{
		"type": "assistant-media", "id": "late-job",
		"images": []any{map[string]any{"mimeType": "image/png", "data": "late-image"}},
	})
	store.RecordJobEvent("job:event", map[string]any{
		"type": "acp", "id": "late-job",
		"event": map[string]any{"kind": "tool", "id": "late-tool", "title": "late", "status": "completed"},
	})
	if journals := sessionJournalFiles(t, store); len(journals) != 0 {
		t.Fatalf("late event recreated terminal journal: %v", journals)
	}
	assistant := sessionAssistant(t, store.Get().(map[string]any), "late-tab")
	if fieldString(assistant, "content") != "finished" || len(anySlice(assistant["images"])) != 0 || len(anySlice(assistant["events"])) != 0 {
		t.Fatalf("late event mutated terminal row: %#v", assistant)
	}
}

func TestSessionStorePrepareAppendFailureLeavesNoJournalResidue(t *testing.T) {
	path := filepath.Join(t.TempDir(), sessionStateFilename)
	store := newSessionStore(path)
	if !store.Save(sessionMirrorFixture("append-tab", "append-chat", "prepare")) {
		t.Fatal("save append-failure fixture")
	}
	store.writeJournalRecord = func(*os.File, []byte) error {
		return errors.New("injected first append failure")
	}
	store.PrepareTurn(map[string]any{"tabId": "append-tab", "chatId": "append-chat", "prompt": "prepare"})
	if journals := sessionJournalFiles(t, store); len(journals) != 0 {
		t.Fatalf("failed first append left journal file: %v", journals)
	}
	store.mu.Lock()
	openJournals := len(store.journals)
	store.mu.Unlock()
	if openJournals != 0 {
		t.Fatalf("failed first append left %d registered journals", openJournals)
	}
}

func TestSessionStoreStreamingJournalScalesWithTurnNotCanonicalMirror(t *testing.T) {
	path := filepath.Join(t.TempDir(), sessionStateFilename)
	store := newSessionStore(path)
	store.streamPersistInterval = time.Hour
	snapshot := sessionMirrorFixture("scale-tab", "scale-chat", "scale prompt")
	snapshot["streamFixturePadding"] = strings.Repeat("safe streaming state ", 400_000)
	if !store.Save(snapshot) {
		t.Fatal("save production-scale mirror")
	}
	canonicalBefore, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat canonical before stream: %v", err)
	}
	store.PrepareTurn(map[string]any{"tabId": "scale-tab", "chatId": "scale-chat", "prompt": "scale prompt"})
	store.RecordJobEvent("job:event", map[string]any{
		"type": "start", "job": map[string]any{"id": "scale-job", "tabId": "scale-tab", "chatId": "scale-chat", "status": "running"},
	})
	const chunks = 4096
	chunk := strings.Repeat("x", 128)
	started := time.Now()
	for range chunks {
		store.RecordJobEvent("job:event", map[string]any{
			"type": "data", "id": "scale-job", "stream": "stdout", "chunk": chunk,
		})
	}
	store.flushScheduledWrite()
	elapsed := time.Since(started)
	canonicalAfter, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat canonical after stream: %v", err)
	}
	if canonicalAfter.Size() != canonicalBefore.Size() || !canonicalAfter.ModTime().Equal(canonicalBefore.ModTime()) {
		t.Fatalf("stream rewrote %d-byte canonical mirror: before=%d/%s after=%d/%s",
			canonicalBefore.Size(), canonicalBefore.Size(), canonicalBefore.ModTime(), canonicalAfter.Size(), canonicalAfter.ModTime())
	}
	journals := sessionJournalFiles(t, store)
	if len(journals) != 1 {
		t.Fatalf("scale journals = %v", journals)
	}
	journalInfo, err := os.Stat(journals[0])
	if err != nil {
		t.Fatalf("stat scale journal: %v", err)
	}
	if canonicalAfter.Size() < 8_000_000 {
		t.Fatalf("canonical scale fixture = %d bytes, want at least 8MB", canonicalAfter.Size())
	}
	if journalInfo.Size() > 2_000_000 {
		t.Fatalf("incremental journal = %d bytes for %d bytes of output", journalInfo.Size(), chunks*len(chunk))
	}
	t.Logf("stream scale canonical=%d journal=%d output=%d chunks=%d elapsed=%s",
		canonicalAfter.Size(), journalInfo.Size(), chunks*len(chunk), chunks, elapsed.Round(time.Millisecond))
}

func sessionJournalFiles(t *testing.T, store *sessionStore) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(store.path), sessionJournalDirname, "*.jsonl"))
	if err != nil {
		t.Fatalf("glob session journals: %v", err)
	}
	return matches
}

func closeSessionStoreJournals(t *testing.T, store *sessionStore) {
	t.Helper()
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.persistTimer != nil {
		store.persistTimer.Stop()
		store.persistTimer = nil
	}
	for _, journal := range store.journals {
		if journal != nil && journal.file != nil {
			if err := journal.file.Close(); err != nil {
				t.Fatalf("close session journal: %v", err)
			}
		}
	}
	store.journals = map[string]*sessionJournal{}
	store.persistDirty = false
}

func agentChatListed(chats []map[string]any, tabID, chatID string) bool {
	for _, chat := range chats {
		if fieldString(chat, "tabId") == tabID && fieldString(chat, "chatId") == chatID {
			return true
		}
	}
	return false
}

func sessionMirrorFixture(tabID, chatID, prompt string) map[string]any {
	return map[string]any{
		"v": json.Number("1"), "activeId": tabID, "seq": json.Number("2"),
		"theme": "dark", "panes": map[string]any{}, "mode": "chats",
		"chats": []any{map[string]any{
			"id": tabID, "chatId": chatID, "title": "Offline", "titleLocked": true,
			"group": nil, "cwd": nil, "currentModelId": nil, "currentModeId": nil,
			"draft": "", "providerId": "mock",
			"messages": []any{
				map[string]any{"id": "client-u", "role": "user", "content": prompt, "status": "done", "at": "2026-07-11T10:00:00Z", "events": []any{}},
				map[string]any{"id": "client-a", "role": "assistant", "content": "", "status": "running", "at": nil, "events": []any{}},
			},
		}},
	}
}

func sessionAssistant(t *testing.T, snapshot map[string]any, tabID string) map[string]any {
	t.Helper()
	chat := chatFromSnapshot(snapshot, tabID)
	if chat == nil {
		t.Fatalf("chat %q missing from %#v", tabID, snapshot)
	}
	for i := len(messageSlice(chat)) - 1; i >= 0; i-- {
		message := mapFromAnyMain(messageSlice(chat)[i])
		if message["role"] == "assistant" {
			return message
		}
	}
	t.Fatalf("assistant missing from chat %#v", chat)
	return nil
}
