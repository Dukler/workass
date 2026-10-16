package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workass/internal/acp"
	"workass/internal/chat"
	providercontract "workass/internal/provider"
)

func subsessionTestHandler(t *testing.T) (*agentControlHandler, *providerChatRuntime, map[string]any) {
	t.Helper()
	root := repoRoot(t)
	store := newSessionStore(filepath.Join(t.TempDir(), sessionStateFilename))
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: filepath.Join(t.TempDir(), "state"),
		Provider:          acp.ProviderConfig{ID: "mock", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")}, CWD: root, Enabled: true, Label: "Workass Mock ACP"},
		RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	runtime := newTestProviderChatRuntime(t, manager, store, manager.StateDir())
	if _, err := runtime.CreateRendererChat(map[string]any{
		"operationId": "test:create-subsession-parent",
		"tabId":       "sub-parent-tab", "chatId": "sub-parent-chat",
		"title": "Subsession parent", "cwd": root, "providerId": "mock",
		"currentModelId": "mock-deterministic", "currentModeId": "ask",
	}); err != nil {
		t.Fatalf("create parent: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if _, err := manager.NewSession(ctx, acp.SessionOptions{
		ChatID: "sub-parent-chat", TabID: "sub-parent-tab", CWD: root, AgentOwnerKey: "sub-owner",
	}); err != nil {
		t.Fatalf("new owner session: %v", err)
	}
	handler := &agentControlHandler{manager: manager, chats: newChatControlCoordinator(manager, nil, runtime)}
	owner := map[string]any{"owner_key": "sub-owner", "parent_chat_id": "sub-parent-chat", "parent_tab_id": "sub-parent-tab"}
	return handler, runtime, owner
}

func withOwner(owner map[string]any, params map[string]any) map[string]any {
	out := map[string]any{}
	for key, value := range owner {
		out[key] = value
	}
	for key, value := range params {
		out[key] = value
	}
	return out
}

// A fresh subsession (user 2026-10-16) is a permanent child chat linked to the
// caller: the link is durable, projected for the sidebar, listed for agents,
// and a lost reply replays to the same child.
func TestAgentFreshSubsessionIsLinkedListedAndReplayable(t *testing.T) {
	handler, runtime, owner := subsessionTestHandler(t)
	request := httptest.NewRequest(http.MethodPost, toolsPath, nil)
	params := withOwner(owner, map[string]any{"operation_id": "test:fresh-subsession", "title": "Revisar la API"})
	createdRaw, err := handler.call(request, agentControlRequest{Method: "chat.subsession", Params: params})
	if err != nil {
		t.Fatalf("chat.subsession: %v", err)
	}
	created := mapFromAnyMain(createdRaw)
	tabID, chatID := fieldString(created, "tabId"), fieldString(created, "chatId")
	if tabID == "" || chatID == "" || fieldString(created, "parentChatId") != "sub-parent-chat" || created["forked"] != false {
		t.Fatalf("fresh subsession receipt = %#v", created)
	}
	state, ok := runtime.Snapshot(chatID)
	if !ok || state.Parent == nil || *state.Parent != (chat.ParentLink{ChatID: "sub-parent-chat", TabID: "sub-parent-tab"}) {
		t.Fatalf("durable parent link = %#v", state.Parent)
	}
	if state.Presentation.Title != "Revisar la API" || len(state.Ledger) != 0 {
		t.Fatalf("fresh subsession must start empty under its own title: %#v ledger=%d", state.Presentation.Title, len(state.Ledger))
	}

	session, err := runtime.ProjectSession()
	if err != nil {
		t.Fatal(err)
	}
	var projected map[string]any
	for _, raw := range anySlice(session["chats"]) {
		if item := mapFromAnyMain(raw); fieldString(item, "chatId") == chatID {
			projected = item
		}
	}
	if fieldString(projected, "parentChatId") != "sub-parent-chat" || fieldString(projected, "parentTabId") != "sub-parent-tab" {
		t.Fatalf("projection lacks the parent link: %#v", projected)
	}
	if _, forked := projected["subsessionForked"]; forked {
		t.Fatalf("a fresh subsession projected as forked: %#v", projected)
	}

	listedRaw, err := handler.call(request, agentControlRequest{Method: "chat.list", Params: owner})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, raw := range anySlice(mapFromAnyMain(listedRaw)["chats"]) {
		if item := mapFromAnyMain(raw); fieldString(item, "chatId") == chatID {
			found = fieldString(item, "parentChatId") == "sub-parent-chat" && item["forked"] == false
		}
	}
	if !found {
		t.Fatalf("chat.list does not name the subsession's parent: %#v", listedRaw)
	}

	replayRaw, err := handler.call(request, agentControlRequest{Method: "chat.subsession", Params: params})
	if err != nil || fieldString(mapFromAnyMain(replayRaw), "chatId") != chatID {
		t.Fatalf("replay = %#v err=%v, want the same child", replayRaw, err)
	}
	changed := withOwner(owner, map[string]any{"operation_id": "test:fresh-subsession", "title": "Otro título"})
	if _, err := handler.call(request, agentControlRequest{Method: "chat.subsession", Params: changed}); err == nil {
		t.Fatal("the same operation with a different request was accepted")
	}
	if _, err := handler.call(request, agentControlRequest{Method: "chat.subsession", Params: withOwner(owner, map[string]any{"operation_id": "test:untitled"})}); err == nil {
		t.Fatal("a subsession without a title was accepted")
	}
}

// A forked subsession is a native fork or nothing: no provider session, a
// provider without native forks, or a request to change the provider all
// fail before any child exists — never a contextless copy.
func TestAgentForkedSubsessionNeverFallsBackToAContextlessCopy(t *testing.T) {
	handler, runtime, owner := subsessionTestHandler(t)
	request := httptest.NewRequest(http.MethodPost, toolsPath, nil)
	for name, params := range map[string]map[string]any{
		"no native fork": {"operation_id": "test:forked-subsession", "title": "Probar tokens", "fork": true},
		"other provider": {"operation_id": "test:forked-other", "title": "Probar tokens", "fork": true, "provider_id": "codex"},
	} {
		_, err := handler.call(request, agentControlRequest{Method: "chat.subsession", Params: withOwner(owner, params)})
		if err == nil {
			t.Fatalf("%s: forked subsession accepted", name)
		}
		operationID := providercontract.OperationID(fieldString(params, "operation_id"))
		if _, exists := runtime.Snapshot(stableAgentChatIdentity("chat", operationID)); exists {
			t.Fatalf("%s: a child chat was created despite the refusal: %v", name, err)
		}
	}
}

func TestSubsessionForkLinkPinsTheNativeHeadAndTheCut(t *testing.T) {
	lane := providercontract.LaneIdentity{ID: "lane-claude", ChatID: "src", Realm: providercontract.Realm{ProviderID: "claude"}}
	native := func(sequence uint64, op providercontract.OperationID) chat.CoverageRecord {
		return chat.CoverageRecord{Sequence: sequence, Status: chat.CoverageNativeSeen, DeliveryID: op}
	}
	build := func(coverage map[uint64]chat.CoverageRecord) chat.State {
		return chat.State{
			ChatID: "src", ActiveLaneID: lane.ID,
			Lanes: map[providercontract.LaneID]chat.LaneState{lane.ID: {
				Identity: lane, Coverage: coverage,
				Thread: providercontract.ThreadRef{ProviderID: "claude", RootID: "root-id", HeadID: "head-id", Lineage: 2, Proof: "p"},
			}},
			Ledger: []chat.LedgerEvent{
				{Role: "user", OperationID: "op1", LaneID: lane.ID}, {Role: "assistant", OperationID: "op1", LaneID: lane.ID},
				{Role: "user", OperationID: "op2", SteerState: "consumed", LaneID: lane.ID},
				{Role: "user", OperationID: "op3", LaneID: lane.ID}, {Role: "assistant", OperationID: "op3", LaneID: lane.ID},
			},
		}
	}
	full := map[uint64]chat.CoverageRecord{1: native(1, "op1"), 2: native(2, "op1"), 3: native(3, "op2"), 4: native(4, "op3"), 5: native(5, "op3")}
	state := build(full)
	whole, err := subsessionForkLink(state, "src-tab", state.Ledger, false)
	if err != nil || *whole != (chat.ParentLink{ChatID: "src", TabID: "src-tab", Forked: true, ForkSourceSessionID: "head-id"}) {
		t.Fatalf("whole-session fork link = %#v err=%v", whole, err)
	}
	cut, err := subsessionForkLink(state, "src-tab", state.Ledger[:2], true)
	if err != nil || cut.ForkBeforeOperationID != "op3" {
		t.Fatalf("a cut must stop before the next real prompt, skipping steers: %#v err=%v", cut, err)
	}

	// Review wa-subagent-1792181911547-3, correction 1. Row 2 was answered by
	// another engine and reached this lane inside op3's prompt as catch-up.
	caughtUp := map[uint64]chat.CoverageRecord{}
	for sequence, record := range full {
		caughtUp[sequence] = record
	}
	caughtUp[2] = chat.CoverageRecord{Sequence: 2, Status: chat.CoveragePromptSeen, DeliveryID: "op3"}
	crossed := build(caughtUp)
	crossed.Ledger[1].LaneID = "lane-codex"
	if _, err := subsessionForkLink(crossed, "src-tab", crossed.Ledger, true); err != nil {
		t.Fatalf("a fork from the latest message after a cross-engine catch-up must succeed: %v", err)
	}
	if _, err := subsessionForkLink(crossed, "src-tab", crossed.Ledger[:2], true); err == nil || !strings.Contains(err.Error(), "another engine") {
		t.Fatalf("a cut that drops the catch-up prompt = %v, want a refusal", err)
	}
	gap := map[uint64]chat.CoverageRecord{1: full[1], 2: full[2], 3: full[3], 5: full[5]}
	for name, cutHere := range map[string]bool{"whole": false, "cut": true} {
		gapped := build(gap)
		if _, err := subsessionForkLink(gapped, "src-tab", gapped.Ledger, cutHere); err == nil || !strings.Contains(err.Error(), "has not received every turn") {
			t.Fatalf("%s fork over a coverage gap = %v, want a refusal", name, err)
		}
	}
	floored := build(map[uint64]chat.CoverageRecord{1: {Sequence: 1, Status: chat.CoverageExcluded}, 2: {Sequence: 2, Status: chat.CoverageExcluded}, 3: full[3], 4: full[4], 5: full[5]})
	floored.ContextFloor = 2
	if _, err := subsessionForkLink(floored, "src-tab", floored.Ledger, false); err != nil {
		t.Fatalf("rows below the source's floor are not this lane's to deliver: %v", err)
	}
	if got := turnsBeforeOperation(state.Ledger, "op3"); got != 1 {
		t.Fatalf("turnsBeforeOperation = %d, want 1", got)
	}

	codex := state
	codexLane := lane
	codexLane.Realm.ProviderID = "codex"
	codex.Lanes = map[providercontract.LaneID]chat.LaneState{lane.ID: {Identity: codexLane, Thread: providercontract.ThreadRef{ProviderID: "codex", RootID: "r", HeadID: "r", Lineage: 1}}}
	if _, err := subsessionForkLink(codex, "src-tab", nil, false); err == nil || !strings.Contains(err.Error(), "cannot fork") {
		t.Fatalf("codex fork link err = %v", err)
	}
	empty := chat.State{ChatID: "src"}
	if _, err := subsessionForkLink(empty, "src-tab", nil, false); err == nil {
		t.Fatal("a chat with no provider session produced a fork link")
	}
}

// Review wa-subagent-1792180415563-2, correction 1: a forked subsession whose
// pinned native fork cannot be bound refuses lane selection — it never falls
// through to session/new as a silent contextless copy.
func TestForkedSubsessionNeverReachesAFreshLane(t *testing.T) {
	_, runtime, _ := subsessionTestHandler(t)
	parent, ok := runtime.Snapshot("sub-parent-chat")
	if !ok {
		t.Fatal("parent missing")
	}
	presentation := parent.Presentation.Clone()
	presentation.TabID = "orphan-fork-tab"
	actor, err := runtime.actorForFork("orphan-fork-chat", chat.InitializeFork{
		Presentation: presentation, SourceChatID: "sub-parent-chat", OperationID: "test:orphan-fork", Digest: "orphan-digest",
		Parent: &chat.ParentLink{
			ChatID: "sub-parent-chat", TabID: "sub-parent-tab", Forked: true,
			ForkSourceSessionID: "no-such-native-head", ForkedSessionID: "7a1c2b3d-4e5f-4061-8a9b-0c1d2e3f4a5b",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if _, err := runtime.attachForkChild(ctx, actor, "test:orphan-fork", "sub-parent-tab", ""); err == nil {
		t.Fatal("a forked subsession without its native fork attached a lane")
	}
	if state, _ := runtime.Snapshot("orphan-fork-chat"); len(state.Lanes) != 0 {
		t.Fatalf("a forked subsession got a lane anyway: %#v", state.Lanes)
	}
}

func TestDerivedOperationIDsStayWithinTheLimit(t *testing.T) {
	if got := derivedOperationID("focus:", "op-1", ""); got != "focus:op-1" {
		t.Fatalf("short id = %q", got)
	}
	long := providercontract.OperationID(strings.Repeat("x", 250))
	got := derivedOperationID("", long, ":first-message")
	if _, err := providercontract.ValidateOperationID(string(got)); err != nil || !strings.HasSuffix(string(got), ":first-message") {
		t.Fatalf("long derived id %q invalid: %v", got, err)
	}
	if derivedOperationID("", long, ":first-message") != got {
		t.Fatal("a derived id must be stable across retries")
	}
}

// Review wa-subagent-1792182761893-4, correction 2: a cut stops before the next
// prompt THIS lane received; a later turn another engine answered never
// entered its session.
func TestSubsessionCutSkipsTurnsAnotherEngineAnswered(t *testing.T) {
	lane := providercontract.LaneIdentity{ID: "lane-a", ChatID: "src", Realm: providercontract.Realm{ProviderID: "claude"}}
	const codex providercontract.LaneID = "lane-codex"
	ledger := []chat.LedgerEvent{
		{Role: "user", OperationID: "op1", LaneID: lane.ID}, {Role: "assistant", OperationID: "op1", LaneID: lane.ID},
		{Role: "user", OperationID: "op2", LaneID: lane.ID}, {Role: "assistant", OperationID: "op2", LaneID: lane.ID},
		{Role: "user", OperationID: "op3", LaneID: codex}, {Role: "assistant", OperationID: "op3", LaneID: codex},
		{Role: "user", OperationID: "op4", LaneID: lane.ID}, {Role: "assistant", OperationID: "op4", LaneID: lane.ID},
	}
	coverage := map[uint64]chat.CoverageRecord{}
	for sequence, event := range ledger {
		status, delivery := chat.CoverageNativeSeen, event.OperationID
		if event.LaneID == codex {
			status, delivery = chat.CoveragePromptSeen, "op4"
		}
		coverage[uint64(sequence+1)] = chat.CoverageRecord{Sequence: uint64(sequence + 1), Status: status, DeliveryID: delivery}
	}
	state := func(rows []chat.LedgerEvent, covered map[uint64]chat.CoverageRecord) chat.State {
		return chat.State{
			ChatID: "src", ActiveLaneID: lane.ID, Ledger: rows,
			Lanes: map[providercontract.LaneID]chat.LaneState{lane.ID: {
				Identity: lane, Coverage: covered,
				Thread: providercontract.ThreadRef{ProviderID: "claude", RootID: "root", HeadID: "root", Lineage: 1},
			}},
		}
	}
	link, err := subsessionForkLink(state(ledger, coverage), "src-tab", ledger[:4], true)
	if err != nil || link.ForkBeforeOperationID != "op4" {
		t.Fatalf("turns 1-2 must cut before op4 (this lane's next prompt), not op3: %#v err=%v", link, err)
	}
	pending := map[uint64]chat.CoverageRecord{1: coverage[1], 2: coverage[2], 3: coverage[3], 4: coverage[4]}
	link, err = subsessionForkLink(state(ledger[:6], pending), "src-tab", ledger[:4], true)
	if err != nil || link.ForkBeforeOperationID != "" {
		t.Fatalf("no later turn reached this lane: the whole session is the prefix: %#v err=%v", link, err)
	}
	inherited := append([]chat.LedgerEvent(nil), ledger[:4]...)
	inherited = append(inherited, chat.LedgerEvent{Role: "user", OperationID: "op-inh", Inherited: true})
	if _, err := subsessionForkLink(state(inherited, pending), "src-tab", inherited[:4], true); err == nil || !strings.Contains(err.Error(), "inherited history") {
		t.Fatalf("a cut inside inherited history must refuse, never widen: %v", err)
	}
}

// Review wa-subagent-1792183828094-5, correction 1: only a prompt that provably
// never reached the session is skipped; an uncertain one stays the boundary,
// so a fork never silently carries a prompt the child does not show.
func TestSubsessionCutNeverSkipsAnUncertainPrompt(t *testing.T) {
	lane := providercontract.LaneIdentity{ID: "lane-a", ChatID: "src", Realm: providercontract.Realm{ProviderID: "claude"}}
	row := func(role string, op providercontract.OperationID, terminal string) chat.LedgerEvent {
		return chat.LedgerEvent{Role: role, OperationID: op, LaneID: lane.ID, TerminalState: terminal}
	}
	build := func(rows []chat.LedgerEvent, foreground *chat.ForegroundTurn) chat.State {
		coverage := map[uint64]chat.CoverageRecord{}
		for index, event := range rows {
			status := chat.CoverageNativeSeen
			if event.TerminalState != "" && event.TerminalState != "consumed" {
				status = chat.CoverageExcluded
			}
			coverage[uint64(index+1)] = chat.CoverageRecord{Sequence: uint64(index + 1), Status: status, DeliveryID: event.OperationID}
		}
		return chat.State{
			ChatID: "src", ActiveLaneID: lane.ID, Ledger: rows, Foreground: foreground,
			Lanes: map[providercontract.LaneID]chat.LaneState{lane.ID: {
				Identity: lane, Coverage: coverage,
				Thread: providercontract.ThreadRef{ProviderID: "claude", RootID: "root", HeadID: "root", Lineage: 1},
			}},
		}
	}
	base := []chat.LedgerEvent{row("user", "op1", "consumed"), row("assistant", "op1", ""), row("user", "op2", "consumed"), row("assistant", "op2", "")}
	cases := []struct {
		name  string
		later []chat.LedgerEvent
		want  providercontract.OperationID
	}{
		{"uncertain prompt is the boundary", []chat.LedgerEvent{row("user", "op3", "delivery_uncertain"), row("user", "op4", "consumed")}, "op3"},
		{"failed prompt is the boundary", []chat.LedgerEvent{row("user", "op3", "failed"), row("user", "op4", "consumed")}, "op3"},
		{"rejected prompt never reached it", []chat.LedgerEvent{row("user", "op3", "admission_rejected"), row("user", "op4", "consumed")}, "op4"},
		{"cancelled before admission never reached it", []chat.LedgerEvent{row("user", "op3", "cancelled_before_admission")}, ""},
	}
	for _, tc := range cases {
		state := build(append(append([]chat.LedgerEvent(nil), base...), tc.later...), nil)
		link, err := subsessionForkLink(state, "src-tab", state.Ledger[:4], true)
		if err != nil || providercontract.OperationID(link.ForkBeforeOperationID) != tc.want {
			t.Fatalf("%s: cut = %#v err=%v, want before %q", tc.name, link, err, tc.want)
		}
	}
	// Suggestion 2: a prompt dispatched on this lane but not yet consumed has
	// no row yet; it may be in the session, so it is the boundary.
	inFlight := build(append([]chat.LedgerEvent(nil), base...), &chat.ForegroundTurn{OperationID: "op5", LaneID: lane.ID})
	if link, err := subsessionForkLink(inFlight, "src-tab", inFlight.Ledger, true); err != nil || link.ForkBeforeOperationID != "op5" {
		t.Fatalf("an unconsumed in-flight prompt must be the boundary: %#v err=%v", link, err)
	}
}
