package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"workass/internal/chat"
	providercontract "workass/internal/provider"
)

// lazyTestChat writes one initialized actor with rows of every shape the
// history reads care about: plain text, timestamps, large tool payloads
// (lean heads, tool detail), row and tool images (ownership).
func lazyTestChat(t *testing.T, stateDir string, backend chatActorBackend, chatID string, rows int, mutate func(*chat.State)) {
	t.Helper()
	store := backend.store(chatID)
	engine, err := chat.NewDurableEngine(chatID, store)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Apply(chat.InitializeChat{
		Presentation: chat.PresentationState{TabID: "tab-" + chatID, Title: "lazy " + chatID},
		OperationID:  providercontract.OperationID("chat-create:" + chatID), Digest: "digest-" + chatID,
	}); err != nil {
		t.Fatal(err)
	}
	state := engine.Snapshot()
	at := time.Date(2026, 10, 21, 8, 0, 0, 0, time.UTC)
	for len(state.Ledger) < rows {
		sequence := len(state.Ledger) + 1
		row := chat.LedgerEvent{
			EventID: fmt.Sprintf("%s-event-%d", chatID, sequence), MessageID: fmt.Sprintf("%s-message-%d", chatID, sequence),
			Sequence: uint64(sequence), Role: "assistant", Status: "done", ContextExcluded: true,
			Text:        fmt.Sprintf("row %d <&> %s", sequence, strings.Repeat("x", sequence*97)),
			OperationID: providercontract.OperationID(fmt.Sprintf("%s-operation-%d", chatID, sequence)),
		}
		if sequence%2 == 0 {
			row.Role = "user"
			row.Attachments = []providercontract.Attachment{{ID: "image", Name: "shot.png", MIMEType: "image/png", Ref: providerSessionImageRefPrefix + lazyImageRef(t, stateDir, fmt.Sprintf("%s-%d", chatID, sequence))}}
		} else {
			row.Timeline = []chat.TimelineEntry{{Key: fmt.Sprintf("tool-%d", sequence), Kind: providercontract.EventToolUpdate, Tool: &providercontract.ToolEvent{
				ToolCallID: fmt.Sprintf("tool-%d", sequence), Title: "Run", Status: "completed", Command: "go test ./...",
				Input: strings.Repeat("i", 100), Output: strings.Repeat("o", 3_000*sequence),
				Attachments: []providercontract.Attachment{{ID: "tool-image", MIMEType: "image/png", Ref: providerSessionImageRefPrefix + lazyImageRef(t, stateDir, fmt.Sprintf("%s-tool-%d", chatID, sequence))}},
			}}}
		}
		if sequence < rows-2 || rows%2 == 1 { // even-sized chats end with undated rows
			row.At = at.Add(time.Duration(sequence) * time.Minute).Format(time.RFC3339Nano)
		}
		state.Ledger = append(state.Ledger, row)
	}
	if mutate != nil {
		mutate(&state)
	}
	state.Revision++
	if err := store.Save(state); err != nil {
		t.Fatal(err)
	}
}

// lazyReads is every read surface a closed actor answers without opening.
type lazyReads struct {
	Session, LeanSession, Digest []byte
	Recent, Exchange             []string
	Pages, Details               []string
	Images, Permissions, Work    []string
	Close, Steer                 []string
}

func readEverything(t *testing.T, runtime *providerChatRuntime, chats map[string]int) lazyReads {
	t.Helper()
	stateDir := runtime.stateDir
	var out lazyReads
	lean := archiveReadOptions{leanTools: true, deferMessageImages: true}
	encode := func(value any, err error) string {
		raw, encodeErr := json.Marshal(value)
		if encodeErr != nil {
			t.Fatal(encodeErr)
		}
		if err != nil {
			return "error: " + err.Error()
		}
		return string(raw)
	}
	var err error
	if out.Session, err = runtime.ProjectSessionRaw(); err != nil {
		t.Fatal(err)
	}
	if out.LeanSession, err = runtime.ProjectSessionRaw(lean); err != nil {
		t.Fatal(err)
	}
	digest, err := runtime.StateDigest(nil, "settings", "proc")
	out.Digest = []byte(encode(digest, err))
	out.Permissions = append(out.Permissions, encode(runtime.PendingPermissions()))
	for _, chatID := range sortedKeys(chats) {
		tabID := "tab-" + chatID
		for _, limit := range []int{1, 7, 60} {
			messages, found, err := runtime.ProjectRecentArchiveByTab(tabID, limit, lean)
			out.Recent = append(out.Recent, fmt.Sprint(found)+encode(messages, err))
			messages, found, err = runtime.ProjectRecentArchiveByTab(tabID, limit)
			out.Recent = append(out.Recent, fmt.Sprint(found)+encode(messages, err))
			for _, opts := range [][]archiveReadOptions{{lean}, {{leanTools: true, leanTitles: true, deferMessageImages: true}}, nil} {
				messages, found, err = runtime.ProjectLastExchangeByTab(tabID, limit, opts...)
				out.Exchange = append(out.Exchange, fmt.Sprint(found)+encode(messages, err))
				raw, found, err := runtime.ProjectLastExchangeByTabRaw(tabID, limit, opts...)
				if err == nil && found {
					// The raw reply is the same rows, byte for byte.
					if encoded, _ := json.Marshal(messages); string(encoded) != string(raw) {
						t.Fatalf("%s last exchange %d: raw reply differs from its rows", tabID, limit)
					}
				}
				out.Exchange = append(out.Exchange, fmt.Sprint(found)+fmt.Sprint(err))
			}
		}
		for sequence := 1; sequence <= chats[chatID]+1; sequence++ {
			before := fmt.Sprintf("%s-message-%d", chatID, sequence)
			messages, found, err := runtime.ProjectArchivePageBeforeByTab(tabID, before, 5, lean)
			out.Pages = append(out.Pages, fmt.Sprint(found)+encode(messages, err))
			detail, found, err := runtime.ProjectToolDetailByTab(tabID, toolDetailRequest{messageID: before, key: fmt.Sprintf("tool-%d", sequence), field: "output", offset: 10})
			out.Details = append(out.Details, fmt.Sprint(found)+encode(detail, err))
			for _, ref := range []string{lazyImageRef(t, stateDir, fmt.Sprintf("%s-%d", chatID, sequence)), lazyImageRef(t, stateDir, fmt.Sprintf("%s-tool-%d", chatID, sequence))} {
				image, found, err := runtime.ProjectToolImageByTab(tabID, ref)
				out.Images = append(out.Images, fmt.Sprint(found)+encode(image, err))
			}
		}
		out.Work = append(out.Work, encode(runtime.ListBackground(tabID, chatID)))
		_, _, closeFound := runtime.findCloseJournal("connection-" + chatID)
		out.Close = append(out.Close, fmt.Sprint(closeFound))
		_, _, _, _, steerErr := runtime.actorForSteerSession("connection-"+chatID, "", "")
		out.Steer = append(out.Steer, fmt.Sprint(steerErr))
	}
	return out
}

func sortedKeys(values map[string]int) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// lazyImageRef stores one small PNG, distinct per label, as a session image
// sidecar and returns its content-addressed reference.
func lazyImageRef(t *testing.T, stateDir, label string) string {
	t.Helper()
	digest := sha256.Sum256([]byte(label))
	return writeSessionImageSidecar(t, stateDir, sizedPNGBase64(t, 1+int(digest[0]), 1+int(digest[1])))
}

// retireIdleSoon sweeps until a sweep closes something (at most a second).
// A coordinator's wake-up drain may still be running when an actor is
// reopened, and a sweep that meets it leaves the actor for the next one.
func retireIdleSoon(runtime *providerChatRuntime) int {
	for attempt := 0; attempt < 100; attempt++ {
		if retired := runtime.retireIdleActors(context.Background(), time.Hour); retired > 0 {
			return retired
		}
		time.Sleep(10 * time.Millisecond)
	}
	return 0
}

// queuedInput gives an actor queued input, which keeps it open.
func queuedInput(state *chat.State) {
	state.StagedQueue = append(state.StagedQueue, chat.StagedQueueEntry{ID: "staged-follow-up", Text: "after this", Source: "host"})
}

func startLazyTestRuntime(t *testing.T, stateDir string, backend chatActorBackend, activeTab string) *providerChatRuntime {
	t.Helper()
	manager := newTestManagerForBackend(t, stateDir)
	runtime := newProviderChatRuntimeBeforeProviderStartupWithBackend(manager, newSessionStore(filepath.Join(stateDir, sessionStateFilename)), stateDir, backend)
	if err := runtime.StartupError(); err != nil {
		t.Fatal(err)
	}
	if err := runtime.ResumeActors(); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.applyRendererSnapshot(map[string]any{"activeId": activeTab, globalPresentationOperationField: "lazy-test-select-" + activeTab}); err != nil {
		t.Fatal(err)
	}
	return runtime
}

func TestClosedActorsAnswerEveryReadLikeOpenOnes(t *testing.T) {
	stateDir := t.TempDir()
	backend, err := openChatActorBackend(stateDir, true, "test", t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.close()
	chats := map[string]int{"lazy-active": 23, "lazy-idle": 9, "lazy-empty": 0, "lazy-busy": 5, "lazy-legacy": 17, "lazy-exchange": 15, "lazy-bigtail": 12}
	for chatID, rows := range chats {
		var mutate func(*chat.State)
		switch chatID {
		case "lazy-bigtail":
			// A tail too large to be the first screen whole (a 3 MiB reply
			// in row 9) whose newest user row (12) is a wake-up notice: the
			// last exchange reaches back past it to the user's own row 10.
			mutate = func(state *chat.State) {
				state.Ledger[8].Text = strings.Repeat("b", 3<<20)
				state.Ledger[11].NoticeKind, state.Ledger[11].NoticeTitle, state.Ledger[11].NoticeStatus = "background", "Build", "completed"
			}
		case "lazy-exchange":
			// The last exchange (rows 12-15) holds a 9 MiB row (14) that
			// stops the newest-first walk and, below it, a row with its own
			// running status (13) that binds the rows after it together.
			mutate = func(state *chat.State) {
				state.Ledger[13].Role = "assistant"
				state.Ledger[13].Text = strings.Repeat("z", 9<<20)
				state.Ledger[12].Status = "running"
			}
		case "lazy-idle":
			// The newest user row is a wake-up notice: the last exchange
			// reaches back past it to the user's own message (row 6).
			mutate = func(state *chat.State) {
				state.Ledger[7].NoticeKind, state.Ledger[7].NoticeTitle, state.Ledger[7].NoticeStatus = "background", "Build", "completed"
			}
		case "lazy-busy":
			// Queued input: boot must open it.
			mutate = queuedInput
		case "lazy-legacy":
			// A committed row with its own running status (no current build
			// writes one) binds the rows after it into one page.
			// A 9 MiB row stops the newest-first walk above it, so the row
			// below is found by the scan, not the walk.
			mutate = func(state *chat.State) {
				state.Ledger[4].Status = "running"
				state.Ledger[12].Text = strings.Repeat("y", 9<<20)
			}
		}
		lazyTestChat(t, stateDir, backend, chatID, rows, mutate)
	}

	t.Setenv(eagerActorsEnv, "1")
	eager := startLazyTestRuntime(t, stateDir, backend, "tab-lazy-active")
	if open, closed := eager.lazyActorCounts(); open != len(chats) || closed != 0 {
		t.Fatalf("eager runtime: %d open, %d closed", open, closed)
	}
	want := readEverything(t, eager, chats)
	if err := eager.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	t.Setenv(eagerActorsEnv, "")
	lazy := startLazyTestRuntime(t, stateDir, backend, "tab-lazy-active")
	defer lazy.Close(context.Background())
	if open, closed := lazy.lazyActorCounts(); open != 1 || closed != len(chats)-1 {
		t.Fatalf("lazy runtime: %d open, %d closed (want only the busy chat open)", open, closed)
	}
	got := readEverything(t, lazy, chats)
	if open, _ := lazy.lazyActorCounts(); open != 1 {
		t.Fatalf("reads opened %d actors", open-1)
	}
	if !reflect.DeepEqual(got, want) {
		gotValue, wantValue := reflect.ValueOf(got), reflect.ValueOf(want)
		for index := 0; index < gotValue.NumField(); index++ {
			if !reflect.DeepEqual(gotValue.Field(index).Interface(), wantValue.Field(index).Interface()) {
				t.Errorf("%s differs between a closed and an open actor", gotValue.Type().Field(index).Name)
				if gotReads, ok := gotValue.Field(index).Interface().([]string); ok {
					wantReads := wantValue.Field(index).Interface().([]string)
					for read := range gotReads {
						if read < len(wantReads) && gotReads[read] != wantReads[read] {
							t.Logf("read %d: closed %.600s", read, gotReads[read])
							t.Logf("read %d: open   %.600s", read, wantReads[read])
							break
						}
					}
				}
			}
		}
	}
	if !strings.Contains(string(got.LeanSession), "lazy-active-message-23") {
		t.Fatal("the selected closed chat carried no tail in session:get")
	}
	// A small tail is the first screen whole; a large one's first screen is
	// its last exchange, back to the user's own newest message past a notice,
	// within the requested tail.
	for _, check := range []struct {
		chatID string
		limit  int
		want   []int
	}{
		{"lazy-active", 10, []int{14, 15, 16, 17, 18, 19, 20, 21, 22, 23}}, {"lazy-active", 1, []int{23}},
		{"lazy-idle", 10, []int{1, 2, 3, 4, 5, 6, 7, 8, 9}}, {"lazy-idle", 3, []int{7, 8, 9}},
		{"lazy-legacy", 10, []int{14, 15, 16, 17}}, {"lazy-empty", 10, nil},
		{"lazy-exchange", 10, []int{13, 14, 15}}, {"lazy-exchange", 2, []int{15}},
		{"lazy-bigtail", 10, []int{10, 11, 12}}, {"lazy-bigtail", 3, []int{10, 11, 12}},
	} {
		messages, found, err := lazy.ProjectLastExchangeByTab("tab-"+check.chatID, check.limit, archiveReadOptions{leanTools: true, deferMessageImages: true})
		if err != nil || !found {
			t.Fatalf("%s last exchange: %v %v", check.chatID, found, err)
		}
		var ids []string
		for _, message := range messages {
			ids = append(ids, fieldString(mapFromAnyMain(message), "id"))
		}
		var want []string
		for _, sequence := range check.want {
			want = append(want, fmt.Sprintf("%s-message-%d", check.chatID, sequence))
		}
		if !reflect.DeepEqual(ids, want) {
			t.Fatalf("%s last exchange of %d: %v, want %v", check.chatID, check.limit, ids, want)
		}
	}
	if open, _ := lazy.lazyActorCounts(); open != 1 {
		t.Fatalf("last-exchange reads opened %d actors", open-1)
	}

	// A command opens the actor, exactly once, and it commits.
	actor, err := lazy.actor("lazy-idle")
	if err != nil {
		t.Fatal(err)
	}
	next := actor.engine.Snapshot().Presentation.Clone()
	next.Draft = "opened on demand"
	if err := actor.engine.Apply(chat.SavePresentation{OperationID: "lazy-save", Digest: "lazy-save", Presentation: next}); err != nil {
		t.Fatal(err)
	}
	if _, closed := lazy.closedSummary("lazy-idle"); closed {
		t.Fatal("an opened actor kept its closed summary")
	}
}

func TestIdleActorsCloseAndReopenSafely(t *testing.T) {
	stateDir := t.TempDir()
	backend, err := openChatActorBackend(stateDir, true, "test", t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.close()
	chats := map[string]int{"idle-a": 11, "idle-b": 6, "idle-busy": 3}
	for chatID, rows := range chats {
		var mutate func(*chat.State)
		if chatID == "idle-busy" {
			mutate = queuedInput
		}
		lazyTestChat(t, stateDir, backend, chatID, rows, mutate)
	}
	t.Setenv(eagerActorsEnv, "")
	runtime := startLazyTestRuntime(t, stateDir, backend, "tab-idle-b")
	defer runtime.Close(context.Background())
	before := readEverything(t, runtime, chats)

	opened := map[string]*providerChatActor{}
	for chatID := range chats {
		actor, err := runtime.actor(chatID)
		if err != nil {
			t.Fatal(err)
		}
		opened[chatID] = actor
	}
	if retired := runtime.retireIdleActors(context.Background(), time.Hour); retired != 0 {
		t.Fatalf("retired %d actors used just now", retired)
	}
	for _, actor := range opened {
		actor.lastUsed.Store(time.Now().Add(-2 * time.Hour).UnixNano())
	}
	if retired := retireIdleSoon(runtime); retired != 1 {
		t.Fatalf("retired %d idle actors, want 1 (the selected chat stays)", retired)
	}
	if _, closed := runtime.closedSummary("idle-a"); !closed {
		t.Fatal("the idle chat is still open")
	}
	if _, closed := runtime.closedSummary("idle-b"); closed {
		t.Fatal("the selected chat was closed")
	}
	// A caller still holding the retired actor cannot commit through it.
	stale := opened["idle-a"]
	next := stale.engine.Snapshot().Presentation.Clone()
	next.Draft = "through a retired engine"
	if err := stale.engine.Apply(chat.SavePresentation{OperationID: "stale-save", Digest: "stale", Presentation: next}); !errors.Is(err, chat.ErrActorRetired) {
		t.Fatalf("commit through a retired actor: %v", err)
	}
	if after := readEverything(t, runtime, chats); !reflect.DeepEqual(after, before) {
		t.Fatal("closing an idle actor changed what its reads return")
	}
	reopened, err := runtime.actor("idle-a")
	if err != nil || reopened == stale {
		t.Fatalf("reopen = %p (stale %p) %v", reopened, stale, err)
	}
	if err := reopened.engine.Apply(chat.SavePresentation{OperationID: "stale-save", Digest: "stale", Presentation: next}); err != nil {
		t.Fatal(err)
	}
	// Idle again, it closes again; the busy chat never does.
	for _, chatID := range []string{"idle-a", "idle-busy"} {
		actor, err := runtime.actor(chatID)
		if err != nil {
			t.Fatal(err)
		}
		actor.lastUsed.Store(time.Now().Add(-2 * time.Hour).UnixNano())
	}
	if retired := retireIdleSoon(runtime); retired != 1 {
		t.Fatalf("retired %d after the second idle period, want 1", retired)
	}
	if _, closed := runtime.closedSummary("idle-busy"); closed {
		t.Fatal("an actor with queued input was closed")
	}
	summary, _ := runtime.closedSummary("idle-a")
	if summary.Revision != reopened.engine.Snapshot().Revision {
		t.Fatalf("closed summary revision %d, actor %d", summary.Revision, reopened.engine.Snapshot().Revision)
	}
}
