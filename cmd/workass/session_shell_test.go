package main

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"testing"

	"workass/internal/acp"
	"workass/internal/chat"
	providercontract "workass/internal/provider"
	"workass/internal/wire"
)

func sessionShellFixture(t *testing.T) (*providerChatRuntime, *wire.Hub) {
	t.Helper()
	stateDir := t.TempDir()
	actors := map[string]*providerChatActor{}
	known := map[string]struct{}{}
	for _, name := range []string{"alpha", "beta"} {
		engine, err := chat.NewEngine("chat-" + name)
		if err != nil {
			t.Fatal(err)
		}
		messages := make([]chat.LedgerEvent, 0, 6)
		for index := 0; index < 3; index++ {
			messages = append(messages,
				chat.LedgerEvent{EventID: fmt.Sprintf("%s-user-%d", name, index), MessageID: fmt.Sprintf("%s-user-%d", name, index), OperationID: "fixture", Role: "user", Text: "question", Status: "done"},
				chat.LedgerEvent{EventID: fmt.Sprintf("%s-reply-%d", name, index), MessageID: fmt.Sprintf("%s-reply-%d", name, index), OperationID: "fixture", Role: "assistant", Text: "answer", Status: "done"},
			)
		}
		if err := engine.Apply(chat.InitializeFork{Presentation: chat.PresentationState{TabID: "tab-" + name, Title: name},
			SourceChatID: "source", OperationID: providercontract.OperationID("create-" + name), Digest: "fixture", Messages: messages}); err != nil {
			t.Fatal(err)
		}
		actors["chat-"+name] = &providerChatActor{engine: engine}
		known["chat-"+name] = struct{}{}
	}
	runtime := &providerChatRuntime{manager: &acp.Manager{}, sessions: sharedSessionStore(stateDir), stateDir: stateDir, actors: actors, known: known}
	if _, err := runtime.sessions.SaveGlobalActiveTab("tab-alpha", "shell-test-select-alpha"); err != nil {
		t.Fatal(err)
	}
	hub := wire.NewHub()
	registerSessionHandlersWithActor(hub, runtime.sessions, nil, runtime)
	return runtime, hub
}

func invokeSessionGet(t *testing.T, hub *wire.Hub, options map[string]any) ([]byte, map[string]any) {
	t.Helper()
	args := []any{}
	if options != nil {
		args = append(args, options)
	}
	reply, err := hub.Invoke("session:get", args)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(reply)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	return data, decoded
}

func sessionChatsByTab(root map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, raw := range root["chats"].([]any) {
		row := raw.(map[string]any)
		out[row["id"].(string)] = row
	}
	return out
}

// session:get {shell} is the cold-connect read: the chat list and every chat's
// metadata, with no history tail for the selected chat (the renderer opens it
// with its own bounded archive read). Everything else must be exactly what the
// ordinary read sends, so the sidebar and composer are unchanged.
func TestSessionShellReadOmitsTheSelectedTailAndKeepsEveryOtherField(t *testing.T) {
	runtime, hub := sessionShellFixture(t)
	lean := map[string]any{"leanTools": true, "deferImages": true, "leanTitles": true}

	fullBytes, full := invokeSessionGet(t, hub, lean)
	if _, present := full["etag"]; present {
		t.Fatal("an ordinary session:get grew an etag; older renderers must get identical bytes")
	}
	want, err := runtime.ProjectSessionRaw(archiveReadOptionsFrom(lean))
	if err != nil {
		t.Fatal(err)
	}
	if string(fullBytes) != string(want) {
		t.Fatal("an ordinary session:get changed bytes")
	}
	if got := len(sessionChatsByTab(full)["tab-alpha"]["messages"].([]any)); got != 6 {
		t.Fatalf("ordinary read selected tail = %d rows, want 6", got)
	}

	shellOptions := map[string]any{"leanTools": true, "deferImages": true, "leanTitles": true, "shell": true}
	_, shell := invokeSessionGet(t, hub, shellOptions)
	etag, _ := shell["etag"].(string)
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(etag) {
		t.Fatalf("shell etag = %q, want a sha256 hex digest", etag)
	}
	if shell["activeId"] != "tab-alpha" {
		t.Fatalf("shell activeId = %#v, want the selected chat", shell["activeId"])
	}
	for key, value := range full {
		if key == "chats" {
			continue
		}
		if !reflect.DeepEqual(shell[key], value) {
			t.Fatalf("shell root field %q = %#v, want %#v", key, shell[key], value)
		}
	}
	fullChats, shellChats := sessionChatsByTab(full), sessionChatsByTab(shell)
	if len(shellChats) != len(fullChats) {
		t.Fatalf("shell lists %d chats, want %d", len(shellChats), len(fullChats))
	}
	for tab, fullChat := range fullChats {
		shellChat := shellChats[tab]
		if rows := shellChat["messages"].([]any); len(rows) != 0 {
			t.Fatalf("shell chat %s carries %d rows, want none", tab, len(rows))
		}
		if shellChat["messageCount"] != float64(6) || shellChat["historyComplete"] != false {
			t.Fatalf("shell chat %s count/complete = %#v/%#v", tab, shellChat["messageCount"], shellChat["historyComplete"])
		}
		for key, value := range fullChat {
			if key == "messages" || key == "historyComplete" {
				continue
			}
			if !reflect.DeepEqual(shellChat[key], value) {
				t.Fatalf("shell chat %s field %q = %#v, want %#v", tab, key, shellChat[key], value)
			}
		}
	}

	// Nothing changed: the client's etag is answered with a tiny receipt.
	unchangedOptions := map[string]any{"leanTools": true, "deferImages": true, "leanTitles": true, "shell": true, "ifNoneMatch": etag}
	unchangedBytes, unchanged := invokeSessionGet(t, hub, unchangedOptions)
	if unchanged["unchanged"] != true || unchanged["etag"] != etag || len(unchanged) != 2 || len(unchangedBytes) > 100 {
		t.Fatalf("unchanged reply = %s", unchangedBytes)
	}
	// A stale etag gets the whole shell.
	if _, err := runtime.sessions.SaveGlobalActiveTab("tab-beta", "shell-test-select-beta"); err != nil {
		t.Fatal(err)
	}
	_, changed := invokeSessionGet(t, hub, unchangedOptions)
	if changed["unchanged"] == true || changed["etag"] == etag || changed["activeId"] != "tab-beta" {
		t.Fatalf("a changed session answered %v/%v/%v", changed["unchanged"], changed["etag"], changed["activeId"])
	}
}

// A running chat keeps its tail in the shell: the rows are what shows it as
// running in the sidebar and lets its live turn resume on screen.
func TestSessionShellKeepsARunningChatsTail(t *testing.T) {
	idle := chat.State{Presentation: chat.PresentationState{TabID: "tab-idle"}}
	running := chat.State{Presentation: chat.PresentationState{TabID: "tab-running"}, Foreground: &chat.ForegroundTurn{}}
	if got := sessionHistoryProjection(idle, ""); got != actorHistoryMetadataOnly {
		t.Fatalf("idle chat in a shell read = %v, want metadata only", got)
	}
	if got := sessionHistoryProjection(running, ""); got != actorHistoryTail {
		t.Fatalf("running chat in a shell read = %v, want its tail", got)
	}
}
