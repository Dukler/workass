package main

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"workass/internal/acp"
	"workass/internal/chat"
	providercontract "workass/internal/provider"
	"workass/internal/wire"
)

const (
	pageNow           = int64(1_800_000_000_000)
	pageSettledBefore = pageNow - 6*24*60*60*1000
	pageLongAgo       = pageNow - 30*24*60*60*1000
)

type pageFixtureChat struct {
	name     string
	settled  string
	at       int64
	unread   bool
	cwd      string
	parent   string
	messages int
}

// sessionPageFixture is a daemon with chats in every sidebar state: live
// rows, a recently settled one (the shelf), filed ones (the archive), a
// filed subsession of a live chat and a filed parent of a live subsession.
func sessionPageFixture(t *testing.T) (*providerChatRuntime, *wire.Hub) {
	t.Helper()
	chats := []pageFixtureChat{
		{name: "live", cwd: "/work/alpha", messages: 2},
		{name: "shelf", settled: "settled", at: pageNow - 24*60*60*1000, cwd: "/work/alpha"},
		{name: "filed", settled: "settled", at: pageLongAgo, cwd: "/work/beta"},
		{name: "unread", settled: "settled", at: pageLongAgo, unread: true, cwd: "/work/beta"},
		{name: "reactivated", settled: "active", cwd: "/work/beta"},
		{name: "legacy", settled: "settled", cwd: "/work/gamma"},
		{name: "kid", settled: "settled", at: pageLongAgo, cwd: "/work/alpha", parent: "live"},
		{name: "oldparent", settled: "settled", at: pageLongAgo, cwd: "/work/delta"},
		{name: "livekid", cwd: "/work/delta", parent: "oldparent"},
		{name: "search target", settled: "settled", at: pageLongAgo, cwd: "/work/projects/needle-project"},
	}
	stateDir := t.TempDir()
	actors := map[string]*providerChatActor{}
	known := map[string]struct{}{}
	for _, item := range chats {
		id := strings.ReplaceAll(item.name, " ", "-")
		engine, err := chat.NewEngine("chat-" + id)
		if err != nil {
			t.Fatal(err)
		}
		cwd := item.cwd
		presentation := chat.PresentationState{TabID: "tab-" + id, Title: item.name, CWD: &cwd, Settled: item.settled, SettledAt: item.at, Unread: item.unread}
		var parent *chat.ParentLink
		if item.parent != "" {
			parent = &chat.ParentLink{ChatID: "chat-" + item.parent, TabID: "tab-" + item.parent}
		}
		messages := make([]chat.LedgerEvent, 0, item.messages)
		for index := 0; index < item.messages; index++ {
			messages = append(messages, chat.LedgerEvent{EventID: id + "-m" + string(rune('a'+index)), MessageID: id + "-m" + string(rune('a'+index)), OperationID: "fixture", Role: "user", Text: "hello", Status: "done"})
		}
		var command chat.Command = chat.InitializeFork{Presentation: presentation, SourceChatID: "source", OperationID: providercontract.OperationID("create-" + id), Digest: "fixture", Messages: messages}
		if parent != nil {
			command = chat.InitializeChat{Presentation: presentation, OperationID: providercontract.OperationID("create-" + id), Digest: "fixture", Parent: parent}
		}
		if err := engine.Apply(command); err != nil {
			t.Fatal(err)
		}
		actors["chat-"+id] = &providerChatActor{engine: engine}
		known["chat-"+id] = struct{}{}
	}
	runtime := &providerChatRuntime{manager: &acp.Manager{}, sessions: sharedSessionStore(stateDir), stateDir: stateDir, actors: actors, known: known}
	if _, err := runtime.sessions.SaveGlobalActiveTab("tab-live", "page-test-select-live"); err != nil {
		t.Fatal(err)
	}
	hub := wire.NewHub()
	registerSessionHandlersWithActor(hub, runtime.sessions, nil, runtime)
	return runtime, hub
}

func pageOptions(extra map[string]any) map[string]any {
	page := map[string]any{"settledBefore": float64(pageSettledBefore), "include": []any{}}
	for key, value := range extra {
		page[key] = value
	}
	return map[string]any{"leanTools": true, "deferImages": true, "leanTitles": true, "shell": true, "page": page}
}

func sortedTabs(root map[string]any) []string {
	out := []string{}
	for tab := range sessionChatsByTab(root) {
		out = append(out, tab)
	}
	sort.Strings(out)
	return out
}

func TestPagedShellListsWhatTheSidebarShowsAndCountsTheRest(t *testing.T) {
	_, hub := sessionPageFixture(t)
	_, full := invokeSessionGet(t, hub, map[string]any{"leanTools": true, "deferImages": true, "leanTitles": true, "shell": true})
	if _, paged := full["deferredChats"]; paged {
		t.Fatal("a shell read without {page} grew deferredChats")
	}
	if got := len(sessionChatsByTab(full)); got != 10 {
		t.Fatalf("unpaged shell lists %d chats, want all 10", got)
	}

	_, paged := invokeSessionGet(t, hub, pageOptions(nil))
	want := []string{"tab-kid", "tab-live", "tab-livekid", "tab-oldparent", "tab-reactivated", "tab-shelf", "tab-unread"}
	if got := sortedTabs(paged); !reflect.DeepEqual(got, want) {
		t.Fatalf("paged shell lists %v, want %v", got, want)
	}
	deferred, _ := paged["deferredChats"].(map[string]any)
	if deferred["count"] != float64(3) {
		t.Fatalf("deferredChats = %#v, want 3 filed chats", deferred)
	}
	wantCWDs := []any{[]any{"/work/beta", float64(1)}, []any{"/work/gamma", float64(1)}, []any{"/work/projects/needle-project", float64(1)}}
	if !reflect.DeepEqual(deferred["cwds"], wantCWDs) {
		t.Fatalf("deferredChats.cwds = %#v, want %#v", deferred["cwds"], wantCWDs)
	}
	if paged["chatOrderPartial"] != true {
		t.Fatal("a paged shell must say its chat order is partial")
	}

	// A named chat and a search match are listed as well.
	_, named := invokeSessionGet(t, hub, pageOptions(map[string]any{"include": []any{"tab-filed"}}))
	if _, ok := sessionChatsByTab(named)["tab-filed"]; !ok {
		t.Fatal("a chat named in include was left out")
	}
	_, searched := invokeSessionGet(t, hub, pageOptions(map[string]any{"search": "needle"}))
	if _, ok := sessionChatsByTab(searched)["tab-search-target"]; !ok {
		t.Fatal("the project basename of a filed chat did not match its search")
	}
	_, labelled := invokeSessionGet(t, hub, pageOptions(map[string]any{"search": "target custom", "labels": []any{[]any{"/work/projects/needle-project", "Custom"}}}))
	if _, ok := sessionChatsByTab(labelled)["tab-search-target"]; !ok {
		t.Fatal("a search across `${title} ${project}` with the renderer's label did not match")
	}
	_, missed := invokeSessionGet(t, hub, pageOptions(map[string]any{"search": "nothing like it"}))
	if _, ok := sessionChatsByTab(missed)["tab-search-target"]; ok {
		t.Fatal("a search that does not match listed a filed chat")
	}
}

func TestPagedReadsAgreeOnTheChatsTheyList(t *testing.T) {
	runtime, hub := sessionPageFixture(t)
	_, paged := invokeSessionGet(t, hub, pageOptions(nil))
	page := sessionPageFrom(pageOptions(nil))
	digest, err := runtime.StateDigestPage(nil, "", "", page)
	if err != nil {
		t.Fatal(err)
	}
	digestTabs := []string{}
	for _, raw := range digest["chats"].([]any) {
		digestTabs = append(digestTabs, raw.(map[string]any)["tabId"].(string))
	}
	sort.Strings(digestTabs)
	if !reflect.DeepEqual(digestTabs, sortedTabs(paged)) {
		t.Fatalf("paged digest rows %v differ from the paged shell %v", digestTabs, sortedTabs(paged))
	}
	if digest["deferredChats"] != 3 {
		t.Fatalf("paged digest deferredChats = %#v, want 3", digest["deferredChats"])
	}
	whole, err := runtime.StateDigest(nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(whole["chats"].([]any)) != 10 {
		t.Fatal("a digest without {page} must cover every chat")
	}
	if _, present := whole["deferredChats"]; present {
		t.Fatal("a digest without {page} grew deferredChats")
	}
}

func TestPageListsEveryChatThatIsNotFiled(t *testing.T) {
	page := &sessionPage{settledBefore: pageSettledBefore, include: map[string]struct{}{}}
	filed := chat.PageFacts{TabID: "tab", Settled: "settled", SettledAt: pageLongAgo}
	if page.lists(filed) {
		t.Fatal("a filed chat was listed")
	}
	for name, change := range map[string]func(*chat.PageFacts){
		"unread":      func(f *chat.PageFacts) { f.Unread = true },
		"foreground":  func(f *chat.PageFacts) { f.Foreground = true },
		"permission":  func(f *chat.PageFacts) { f.PendingPermissions = true },
		"queued":      func(f *chat.PageFacts) { f.Queued = true },
		"background":  func(f *chat.PageFacts) { f.RunningBackground = true },
		"parked":      func(f *chat.PageFacts) { f.ObligationState = "parked" },
		"not settled": func(f *chat.PageFacts) { f.Settled = "" },
		"recent":      func(f *chat.PageFacts) { f.SettledAt = pageSettledBefore + 1 },
	} {
		facts := filed
		change(&facts)
		if !page.lists(facts) {
			t.Fatalf("a %s chat was left out", name)
		}
	}
	// Without its settle instant the renderer dates a filed chat by its last
	// activity, and one with neither is filed since forever.
	legacy := chat.PageFacts{TabID: "tab", Settled: "settled", LastActivityAt: pageSettledBefore + 1}
	if !page.lists(legacy) {
		t.Fatal("a settled chat active after the boundary was left out")
	}
	legacy.LastActivityAt = 0
	if page.lists(legacy) {
		t.Fatal("a settled chat with no dates at all was listed")
	}
	// needs_input does not keep a settled chat out of the archive: the
	// sidebar only shows Atención on an unsettled row.
	attention := filed
	attention.ObligationState = "needs_input"
	if page.lists(attention) {
		t.Fatal("a filed chat that owes input was listed; the sidebar files it")
	}
}

func TestPartialChatOrderFoldsBackIntoTheStoredOne(t *testing.T) {
	stored := []any{"tab-a", "tab-x", "M~m1~tab-r", "tab-b", "tab-y", "tab-c", "tab-gone"}
	listed := map[string]struct{}{"tab-a": {}, "tab-b": {}, "tab-c": {}}
	partial := partialChatOrder(stored, listed)
	if !reflect.DeepEqual(partial, []any{"tab-a", "M~m1~tab-r", "tab-b", "tab-c"}) {
		t.Fatalf("partial order = %#v", partial)
	}
	live := map[string]struct{}{"tab-a": {}, "tab-b": {}, "tab-c": {}, "tab-x": {}, "tab-y": {}, "tab-new": {}}
	// A new chat first, c dragged above a, the remote row kept where it was.
	incoming := []any{"tab-new", "tab-c", "tab-a", "M~m1~tab-r", "tab-b"}
	merged := mergePartialChatOrder(stored, incoming, live)
	want := []any{"tab-new", "tab-c", "tab-x", "tab-a", "M~m1~tab-r", "tab-y", "tab-b"}
	if !reflect.DeepEqual(merged, want) {
		t.Fatalf("merged order = %#v, want %#v", merged, want)
	}
	if again := mergePartialChatOrder(merged, incoming, live); !reflect.DeepEqual(again, merged) {
		t.Fatalf("folding the same order twice changed it: %#v", again)
	}
}

func TestPartialSaveKeepsTheChatsTheRendererWasNotSent(t *testing.T) {
	runtime, hub := sessionPageFixture(t)
	if _, err := runtime.applyRendererSnapshot(map[string]any{
		globalPresentationOperationField: "order-whole", globalPresentationRevisionField: float64(1),
		"chatOrder": []any{"tab-live", "tab-filed", "tab-shelf", "tab-legacy", "tab-unread"},
	}); err != nil {
		t.Fatal(err)
	}
	_, paged := invokeSessionGet(t, hub, pageOptions(nil))
	order, _ := paged["chatOrder"].([]any)
	if !reflect.DeepEqual(order, []any{"tab-live", "tab-shelf", "tab-unread"}) {
		t.Fatalf("paged chatOrder = %#v", order)
	}
	revision := paged[globalPresentationRevisionField]
	if _, err := runtime.applyRendererSnapshot(map[string]any{
		globalPresentationOperationField: "order-partial", globalPresentationRevisionField: revision,
		"chatOrder": []any{"tab-unread", "tab-live", "tab-shelf"}, "chatOrderPartial": true,
	}); err != nil {
		t.Fatal(err)
	}
	stored := normalizedActorChatOrder(runtime.sessions.GlobalSnapshot()["chatOrder"])
	if want := []any{"tab-unread", "tab-filed", "tab-live", "tab-legacy", "tab-shelf"}; !reflect.DeepEqual(stored, want) {
		t.Fatalf("stored order = %#v, want %#v", stored, want)
	}
	// The same operation again is answered by its receipt.
	if _, err := runtime.applyRendererSnapshot(map[string]any{
		globalPresentationOperationField: "order-partial", globalPresentationRevisionField: revision,
		"chatOrder": []any{"tab-unread", "tab-live", "tab-shelf"}, "chatOrderPartial": true,
	}); err != nil {
		t.Fatalf("a retried partial save failed: %v", err)
	}
	if _, kept := runtime.sessions.GlobalSnapshot()["chatOrderPartial"]; kept {
		t.Fatal("the partial flag was persisted")
	}
}

func TestShellDeltaRebuildsTheShellItNames(t *testing.T) {
	_, hub := sessionPageFixture(t)
	options := pageOptions(nil)
	options["delta"] = true
	_, base := invokeSessionGet(t, hub, options)
	baseEtag := base["etag"].(string)

	searchOptions := pageOptions(map[string]any{"search": "needle", "include": []any{"tab-filed"}})
	searchOptions["delta"] = true
	searchOptions["ifNoneMatch"] = baseEtag
	_, reply := invokeSessionGet(t, hub, searchOptions)
	delta, ok := reply["delta"].(map[string]any)
	if !ok {
		t.Fatalf("a changed page against a known shell was not a delta: %#v", reply)
	}
	if _, whole := delta["set"].(map[string]any)["chatOrder"]; whole {
		t.Fatal("the chat order was resent whole instead of as an edit")
	}
	delete(searchOptions, "ifNoneMatch")
	delete(searchOptions, "delta")
	_, want := invokeSessionGet(t, hub, searchOptions)
	rebuilt := applyShellDeltaForTest(t, base, reply)
	if !reflect.DeepEqual(rebuilt, want) {
		left, _ := json.Marshal(rebuilt)
		right, _ := json.Marshal(want)
		t.Fatalf("rebuilt shell differs from the shell it names:\n%s\n%s", left, right)
	}

	// An unchanged shell is a receipt; a shell the daemon never sent is whole.
	again := pageOptions(map[string]any{"search": "needle", "include": []any{"tab-filed"}})
	again["delta"] = true
	again["ifNoneMatch"] = want["etag"]
	if _, receipt := invokeSessionGet(t, hub, again); receipt["unchanged"] != true {
		t.Fatalf("an unchanged paged shell = %#v, want a receipt", receipt)
	}
	again["ifNoneMatch"] = strings.Repeat("0", 64)
	if _, whole := invokeSessionGet(t, hub, again); whole["chats"] == nil || whole["delta"] != nil {
		t.Fatal("a delta against a shell the daemon does not remember must be the whole shell")
	}
}

// applyShellDeltaForTest is desktop/renderer2/src/store/session-page.ts
// applyShellDelta.
func applyShellDeltaForTest(t *testing.T, base, reply map[string]any) map[string]any {
	t.Helper()
	delta := reply["delta"].(map[string]any)
	next := map[string]any{}
	for key, value := range base {
		next[key] = value
	}
	if set, ok := delta["set"].(map[string]any); ok {
		for key, value := range set {
			next[key] = value
		}
	}
	if unset, ok := delta["unset"].([]any); ok {
		for _, key := range unset {
			delete(next, key.(string))
		}
	}
	if lists, ok := delta["lists"].(map[string]any); ok {
		for key, rawOps := range lists {
			kept := base[key].([]any)
			list := []any{}
			for _, op := range rawOps.([]any) {
				if id, ok := op.(string); ok {
					list = append(list, id)
					continue
				}
				pair := op.([]any)
				start, count := int(pair[0].(float64)), int(pair[1].(float64))
				list = append(list, kept[start:start+count]...)
			}
			next[key] = list
		}
	}
	byID := map[string]any{}
	ids := []string{}
	for _, raw := range base["chats"].([]any) {
		id := raw.(map[string]any)["id"].(string)
		byID[id] = raw
		ids = append(ids, id)
	}
	if patches, ok := delta["patch"].([]any); ok {
		for _, raw := range patches {
			patch := raw.(map[string]any)
			id := patch["id"].(string)
			row := map[string]any{}
			for key, value := range byID[id].(map[string]any) {
				row[key] = value
			}
			if set, ok := patch["set"].(map[string]any); ok {
				for key, value := range set {
					row[key] = value
				}
			}
			if unset, ok := patch["unset"].([]any); ok {
				for _, key := range unset {
					delete(row, key.(string))
				}
			}
			byID[id] = row
		}
	}
	if added, ok := delta["chats"].([]any); ok {
		for _, raw := range added {
			byID[raw.(map[string]any)["id"].(string)] = raw
		}
	}
	if rawIDs, ok := delta["ids"].([]any); ok {
		ids = ids[:0]
		for _, id := range rawIDs {
			ids = append(ids, id.(string))
		}
	}
	chats := []any{}
	for _, id := range ids {
		row, ok := byID[id]
		if !ok {
			t.Fatalf("delta names row %q it never sent", id)
		}
		chats = append(chats, row)
	}
	next["chats"] = chats
	next["etag"] = reply["etag"]
	return next
}
