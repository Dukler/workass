package main

import (
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workass/internal/acp"
	"workass/internal/changes"
	"workass/internal/chat"
	"workass/internal/httpserve"
	providercontract "workass/internal/provider"
	"workass/internal/wire"
)

// changesChat builds a resident chat whose rows carry the given tool calls.
func changesChat(t *testing.T, chatID, tabID, title, cwd string, rows ...chat.LedgerEvent) *providerChatActor {
	t.Helper()
	engine, err := chat.NewEngine(chatID)
	if err != nil {
		t.Fatal(err)
	}
	for index := range rows {
		rows[index].EventID = fmt.Sprintf("%s-event-%d", chatID, index)
		rows[index].MessageID = fmt.Sprintf("%s-message-%d", chatID, index)
		rows[index].OperationID = "original"
		if rows[index].Role == "" {
			rows[index].Role = "assistant"
		}
		rows[index].Status = "done"
	}
	folder := cwd
	if err := engine.Apply(chat.InitializeFork{
		Presentation: chat.PresentationState{TabID: tabID, Title: title, CWD: &folder, ProviderID: "claude"},
		SourceChatID: "source", OperationID: "create", Digest: "fixture", Messages: rows,
	}); err != nil {
		t.Fatal(err)
	}
	return &providerChatActor{engine: engine}
}

func editRow(at time.Time, tools ...*providercontract.ToolEvent) chat.LedgerEvent {
	row := chat.LedgerEvent{At: at.UTC().Format(time.RFC3339Nano), Text: "done"}
	for index, tool := range tools {
		row.Timeline = append(row.Timeline, chat.TimelineEntry{Key: fmt.Sprintf("tool-%d", index), Kind: providercontract.EventToolUpdate, Tool: tool})
	}
	return row
}

func changesRuntime(t *testing.T, actors map[string]*providerChatActor) *providerChatRuntime {
	t.Helper()
	stateDir := t.TempDir()
	known := map[string]struct{}{}
	for chatID := range actors {
		known[chatID] = struct{}{}
	}
	return &providerChatRuntime{manager: &acp.Manager{}, sessions: sharedSessionStore(stateDir), stateDir: stateDir, actors: actors, known: known}
}

func TestChangesSourceReadsStoredEditToolsWithinTheWindow(t *testing.T) {
	now := time.Now()
	ms := func(ago time.Duration) int64 { return now.Add(-ago).UnixMilli() }
	// Rows are oldest first. The walk goes newest first and stops at the row
	// older than the window, so the ancient edit is never read.
	recent := changesChat(t, "chat-recent", "tab-recent", "Panel derecho", "/work/proj",
		editRow(now.Add(-9*24*time.Hour), &providercontract.ToolEvent{ToolCallID: "e", ToolKind: "edit", Status: "completed", Location: "/work/proj/ancient.go"}),
		editRow(now.Add(-2*time.Hour),
			&providercontract.ToolEvent{ToolCallID: "a", ToolKind: "edit", Status: "completed", Location: "/work/proj/ui/a.tsx:12", EndedAtUnixMS: ms(2 * time.Hour)},
			&providercontract.ToolEvent{ToolCallID: "b", ToolKind: "read", Status: "completed", Location: "/work/proj/read-only.go"},
			&providercontract.ToolEvent{ToolCallID: "c", ToolKind: "edit", Status: "failed", Location: "/work/proj/failed.go"},
			&providercontract.ToolEvent{ToolCallID: "d", ToolKind: "edit", Status: "completed", Input: `{"file_path":"styles/app.css"}`, StartedAtUnixMS: ms(time.Hour)},
		),
		editRow(now.Add(-time.Minute)),
	)
	stale := changesChat(t, "chat-stale", "tab-stale", "Vieja", "/work/old",
		editRow(now.Add(-10*24*time.Hour), &providercontract.ToolEvent{ToolCallID: "x", ToolKind: "edit", Status: "completed", Location: "/work/old/x.go"}))
	source := newChangesSource(changesRuntime(t, map[string]*providerChatActor{"chat-recent": recent, "chat-stale": stale}))

	activity := source.activity(now.Add(-changes.DefaultWindow))
	if len(activity) != 1 {
		t.Fatalf("activity %+v", activity)
	}
	got := activity[0]
	if got.ChatID != "chat-recent" || got.TabID != "tab-recent" || got.Title != "Panel derecho" || got.ProviderID != "claude" || got.CWD != "/work/proj" {
		t.Fatalf("chat %+v", got)
	}
	var paths []string
	for _, edit := range got.Edits {
		paths = append(paths, fmt.Sprintf("%s@%d", filepath.ToSlash(edit.Path), edit.At))
	}
	want := []string{
		fmt.Sprintf("/work/proj/ui/a.tsx@%d", ms(2*time.Hour)),
		fmt.Sprintf("/work/proj/styles/app.css@%d", ms(time.Hour)),
	}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Fatalf("edits %v want %v", paths, want)
	}
	// A second read with nothing new reuses the cached scan.
	again := source.activity(now.Add(-changes.DefaultWindow))
	if len(again) != 1 || len(again[0].Edits) != 2 {
		t.Fatalf("cached read %+v", again)
	}
}

func TestChangesWatchOverTheWireRunsGitOnlyWhileTheSocketLives(t *testing.T) {
	requireGitBinary(t)
	workspace := t.TempDir()
	origin := filepath.Join(workspace, "origin.git")
	runWireFixtureCommand(t, "", "git", "init", "--bare", origin)
	repo := filepath.Join(workspace, "proj")
	initWireGitRepo(t, repo, map[string]string{"main.go": "package main\n", "ui/view.tsx": "one\ntwo\n"})
	runWireGit(t, repo, "remote", "add", "origin", origin)
	runWireGit(t, repo, "push", "-q", "-u", "origin", "main")
	if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runWireGit(t, repo, "commit", "-q", "-am", "ahead")
	if err := os.WriteFile(filepath.Join(repo, "ui", "view.tsx"), []byte("one\nTWO\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "notes.md"), []byte("a\nb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	agent := changesChat(t, "chat-agent", "tab-agent", "Panel derecho", repo,
		editRow(now.Add(-time.Minute), &providercontract.ToolEvent{ToolCallID: "a", ToolKind: "edit", Status: "completed",
			Location: filepath.Join(repo, "ui", "view.tsx"), EndedAtUnixMS: now.Add(-time.Minute).UnixMilli()}))
	runtime := changesRuntime(t, map[string]*providerChatActor{"chat-agent": agent})

	trace := filepath.Join(t.TempDir(), "git-trace.log")
	t.Setenv("GIT_TRACE", trace)
	hub := wire.NewHub()
	service, _ := registerChangesHandlers(hub, nil, runtime)
	renderer := t.TempDir()
	if err := os.WriteFile(filepath.Join(renderer, "index.html"), []byte("<!doctype html><head></head><body></body>"), 0o644); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(httpserve.New(renderer, hub, nil))
	defer server.Close()

	time.Sleep(200 * time.Millisecond)
	if data, _ := os.ReadFile(trace); len(data) != 0 {
		t.Fatalf("git ran before anyone watched: %s", data)
	}

	client := dialTestWS(t, server.URL)
	client.invoke(t, 1, "changes:watch", map[string]any{})
	reply := client.waitReply(t, 1, 5*time.Second)
	if reply.Error != nil || fieldString(mapFromAnyMain(reply.Result), "id") == "" {
		t.Fatalf("watch reply %+v", reply)
	}
	var repoView map[string]any
	client.waitFor(t, 10*time.Second, func(message wsMessage) bool {
		if message.T != "event" || message.Channel != "changes:state" {
			return false
		}
		for _, item := range mapFromAnyMain(message.Payload)["repos"].([]any) {
			view := mapFromAnyMain(item)
			if view["root"] == canonical && view["checked"] == true {
				repoView = view
				return true
			}
		}
		return false
	})
	if repoView["branch"] != "main" || fmt.Sprint(repoView["ahead"]) != "1" || repoView["behind"] != nil {
		t.Fatalf("repo row %#v", repoView)
	}
	files := map[string]map[string]any{}
	for _, item := range repoView["files"].([]any) {
		file := mapFromAnyMain(item)
		files[fmt.Sprint(file["path"])] = file
	}
	if view := files["ui/view.tsx"]; view == nil || view["chatId"] != "chat-agent" || fmt.Sprint(view["adds"]) != "2" || fmt.Sprint(view["dels"]) != "1" {
		t.Fatalf("agent-edited file %#v", files)
	}
	if notes := files["notes.md"]; notes == nil || notes["chatId"] != nil || notes["new"] != true || fmt.Sprint(notes["adds"]) != "2" {
		t.Fatalf("hand-made file %#v", files)
	}

	client.invoke(t, 2, "changes:diff", map[string]any{"root": canonical, "path": "ui/view.tsx"})
	diffReply := client.waitReply(t, 2, 5*time.Second)
	diffText := fieldString(mapFromAnyMain(diffReply.Result), "text")
	if diffReply.Error != nil || !strings.Contains(diffText, "-two") || !strings.Contains(diffText, "+TWO") || !strings.Contains(diffText, "+three") {
		t.Fatalf("diff %+v", diffReply)
	}
	data, _ := os.ReadFile(trace)
	if !strings.Contains(string(data), "status --porcelain=v2 --branch -z --untracked-files=all") {
		t.Fatalf("status was not the porcelain v2 call: %s", data)
	}

	_ = client.conn.Close()
	deadline := time.Now().Add(5 * time.Second)
	for service.Watched() {
		if time.Now().After(deadline) {
			t.Fatal("the watch outlived its socket")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := service.Diff(t.Context(), canonical, "ui/view.tsx"); err != changes.ErrNotWatching {
		t.Fatalf("diff after the socket closed: %v", err)
	}
}
