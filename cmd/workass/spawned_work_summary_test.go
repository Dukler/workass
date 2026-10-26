package main

import (
	"encoding/json"
	"reflect"
	"testing"

	"workass/internal/acp"
	"workass/internal/chat"
	providercontract "workass/internal/provider"
)

type summaryFixtureStore struct{ state chat.State }

func (s *summaryFixtureStore) Load(string) (chat.State, bool, error) { return s.state, true, nil }
func (s *summaryFixtureStore) Save(state chat.State) error           { s.state = state; return nil }

func summaryFixtureActor(t *testing.T, chatID, tabID string, background map[string]chat.BackgroundState, obligation *chat.ObligationState) *providerChatActor {
	t.Helper()
	state, err := chat.NewState(chatID)
	if err != nil {
		t.Fatal(err)
	}
	state.Presentation.TabID = tabID
	if background != nil {
		state.Background = background
	}
	state.Obligation = obligation
	engine, err := chat.NewDurableEngine(chatID, &summaryFixtureStore{state: state})
	if err != nil {
		t.Fatal(err)
	}
	return &providerChatActor{engine: engine}
}

// The sidebar boots from one batched read instead of one spawned-work:list per
// chat. It must carry exactly what the sidebar draws from a list — the running
// rows, in the list's own shape and order — and every open obligation, so a
// late client still sees what each chat owes. Terminal rows stay behind the
// per-chat list the work rail reads when the chat is opened.
func TestSpawnedWorkSummaryCarriesRunningRowsCountsAndObligations(t *testing.T) {
	pid := 4242
	background := map[string]chat.BackgroundState{
		"work-old":  {Event: providercontract.BackgroundEvent{WorkID: "work-old", TaskID: "task-old", Title: "older agent", Kind: "agent", Role: "agent", Status: "running", StartedAt: "2026-10-25T10:00:00Z"}},
		"work-new":  {Event: providercontract.BackgroundEvent{WorkID: "work-new", TaskID: "task-new", Title: "dev server", Kind: "shell", Role: "service", Status: "running", StartedAt: "2026-10-25T11:00:00Z", PID: &pid}},
		"work-done": {Event: providercontract.BackgroundEvent{WorkID: "work-done", TaskID: "task-done", Title: "finished", Kind: "agent", Status: "done", StartedAt: "2026-10-25T09:00:00Z", Summary: "a long terminal receipt"}},
	}
	owes := &chat.ObligationState{State: "needs_input", Source: "turn", Note: "asked a question"}
	stateDir := t.TempDir()
	runtime := &providerChatRuntime{
		manager: &acp.Manager{}, sessions: sharedSessionStore(stateDir), stateDir: stateDir,
		actors: map[string]*providerChatActor{
			"chat-work":  summaryFixtureActor(t, "chat-work", "tab-work", background, owes),
			"chat-quiet": summaryFixtureActor(t, "chat-quiet", "tab-quiet", nil, nil),
			"chat-owes":  summaryFixtureActor(t, "chat-owes", "tab-owes", nil, &chat.ObligationState{State: "needs_input", Source: "permission"}),
		},
		known: map[string]struct{}{"chat-work": {}, "chat-quiet": {}, "chat-owes": {}},
	}

	summary, err := runtime.BackgroundSummary()
	if err != nil {
		t.Fatal(err)
	}
	byTab := map[string]map[string]any{}
	for _, entry := range summary {
		byTab[entry["tabId"].(string)] = roundTripJSON(t, entry)
	}
	if _, present := byTab["tab-quiet"]; present {
		t.Fatalf("a chat with no work and no obligation must be absent: %#v", byTab["tab-quiet"])
	}
	if len(byTab) != 2 {
		t.Fatalf("summary chats = %v, want tab-work and tab-owes", byTab)
	}

	work := byTab["tab-work"]
	if work["chatId"] != "chat-work" {
		t.Fatalf("summary entry identity = %#v", work)
	}
	if counts := work["counts"].(map[string]any); counts["total"] != float64(3) || counts["running"] != float64(2) {
		t.Fatalf("counts = %#v, want total 3 running 2", counts)
	}
	if obligation := work["obligation"].(map[string]any); obligation["state"] != "needs_input" || obligation["source"] != "turn" || obligation["note"] != "asked a question" {
		t.Fatalf("obligation = %#v", obligation)
	}
	// The running rows are byte-for-byte the running rows of the per-chat list,
	// in its order (newest first), so a sidebar row is the same either way.
	listed, err := runtime.ListBackground("tab-work", "chat-work")
	if err != nil {
		t.Fatal(err)
	}
	var wantRunning []any
	for _, row := range listed {
		if row["status"] == "running" {
			wantRunning = append(wantRunning, roundTripJSON(t, row))
		}
	}
	if got := work["running"].([]any); !reflect.DeepEqual(got, wantRunning) {
		t.Fatalf("running rows = %#v, want the list's running rows %#v", got, wantRunning)
	}
	if first := wantRunning[0].(map[string]any); first["id"] != "work-new" || first["role"] != "service" || first["pid"] != float64(4242) {
		t.Fatalf("first running row = %#v, want the newest, with its role and pid", first)
	}

	owesOnly := byTab["tab-owes"]
	if rows := owesOnly["running"].([]any); len(rows) != 0 {
		t.Fatalf("a chat with only an obligation lists no rows: %#v", rows)
	}
	if obligation := owesOnly["obligation"].(map[string]any); obligation["state"] != "needs_input" || obligation["source"] != "permission" {
		t.Fatalf("obligation without work = %#v", obligation)
	}
}

func roundTripJSON(t *testing.T, value any) map[string]any {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
