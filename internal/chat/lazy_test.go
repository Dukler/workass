package chat

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"workass/internal/provider"
	"workass/internal/store"
)

func TestMessageIDLookupsUseTheirIndex(t *testing.T) {
	actors := openTestActors(t)
	var plan []string
	if err := actors.DB().Read(context.Background(), func(tx *store.Tx) error {
		rows, err := tx.Query(`EXPLAIN QUERY PLAN SELECT max(seq) FROM messages WHERE chat_id = ? AND message_id = ?`, "chat", "message")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				return err
			}
			plan = append(plan, detail)
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(plan, "\n"), "messages_by_message_id") {
		t.Fatalf("message-id lookup does not use its index: %v", plan)
	}
}

// quiescentScriptedActor drives a SQLite-backed actor through the store
// lifecycle script (turns, tools, media, a cancelled and a completed turn),
// then restarts it and settles its one background item, which leaves it
// quiescent with a realistic ledger, lanes, outbox and usage.
func quiescentScriptedActor(t *testing.T, actors *SQLActors, chatID string) *Engine {
	t.Helper()
	engine, err := NewDurableEngine(chatID, actors.Store(chatID))
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range actorLifecycleScript(chatID) {
		step.run(t, engine)
	}
	if ok, _ := Quiescent(engine.Snapshot()); ok {
		t.Fatal("a scripted actor with a running turn lane and background work counted as quiescent")
	}
	// Restart: recovery detaches the process-local lane.
	engine, err = NewDurableEngine(chatID, actors.Store(chatID))
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Apply(ReconcileBackgroundSnapshot{AuthoritativeAbsence: true, ObservedAt: "2026-10-21T10:03:00Z"}); err != nil {
		t.Fatal(err)
	}
	if err := engine.Apply(ReconcileObligation{ObservedAt: "2026-10-21T10:03:00Z", DaemonBoot: true, HarnessQuiet: true}); err != nil {
		t.Fatal(err)
	}
	if ok, reason := Quiescent(engine.Snapshot()); !ok {
		t.Fatalf("settled scripted actor is not quiescent: %s", reason)
	}
	return engine
}

func TestQuiescentNamesWhatRestartMustSettle(t *testing.T) {
	actors := openTestActors(t)
	base := quiescentScriptedActor(t, actors, "quiescent-chat").Snapshot()
	laneID := base.ActiveLaneID
	cases := map[string]func(*State){
		"uninitialized":   func(s *State) { s.Initialized = false },
		"foreground turn": func(s *State) { s.Foreground = &ForegroundTurn{OperationID: "op"} },
		"pending steer":   func(s *State) { s.PendingSteer = &PendingSteer{OperationID: "op"} },
		"pending cancel":  func(s *State) { s.PendingCancel = &PendingCancel{OperationID: "op"} },
		"queued input":    func(s *State) { s.Queue = append(s.Queue, QueueEntry{OperationID: "op"}) },
		"queued staged":   func(s *State) { s.StagedQueue = append(s.StagedQueue, StagedQueueEntry{ID: "q"}) },
		"outbox work": func(s *State) {
			s.Outbox = append(s.Outbox, OutboxEntry{ID: "effect", Kind: EffectStartTurn, Status: OutboxPending})
		},
		"running background work": func(s *State) {
			s.Background["running"] = BackgroundState{Event: provider.BackgroundEvent{WorkID: "running", Status: "running"}}
		},
		"pending permission": func(s *State) {
			s.Permissions["request"] = PermissionState{Event: provider.PermissionEvent{Status: "pending"}}
		},
		"open obligation": func(s *State) { s.Obligation = &ObligationState{State: "parked"} },
		"restart recovery": func(s *State) {
			lane := s.Lanes[laneID]
			lane.Phase = LaneRunning
			s.Lanes[laneID] = lane
		},
	}
	for want, mutate := range cases {
		state := base.Clone()
		mutate(&state)
		if ok, reason := Quiescent(state); ok || !strings.Contains(reason, strings.Split(want, " ")[0]) {
			t.Errorf("%s: Quiescent = %v %q", want, ok, reason)
		}
	}
	settled := base.Clone()
	settled.Obligation = &ObligationState{State: "done"}
	settled.Background["done"] = BackgroundState{Event: provider.BackgroundEvent{WorkID: "done", Status: "exited"}}
	settled.Permissions["answered"] = PermissionState{Event: provider.PermissionEvent{Status: "resolved"}}
	if ok, reason := Quiescent(settled); !ok {
		t.Fatalf("settled work kept the actor open: %s", reason)
	}
}

func TestOpeningAQuiescentActorCommitsNothing(t *testing.T) {
	actors := openTestActors(t)
	const chatID = "neutral-open"
	before := quiescentScriptedActor(t, actors, chatID).Snapshot()
	changes := totalChanges(t, actors.DB())
	reopened, err := NewDurableEngine(chatID, actors.Store(chatID))
	if err != nil {
		t.Fatal(err)
	}
	after := reopened.Snapshot()
	if after.Revision != before.Revision {
		t.Fatalf("opening a quiescent actor moved its revision %d -> %d", before.Revision, after.Revision)
	}
	if got := totalChanges(t, actors.DB()); got != changes {
		t.Fatalf("opening a quiescent actor wrote %d rows", got-changes)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatal("opening a quiescent actor changed its state")
	}

	// An actor restart recovery must settle still commits that recovery.
	busy := after.Clone()
	lane := busy.Lanes[busy.ActiveLaneID]
	lane.Phase = LaneRunning
	busy.Lanes[busy.ActiveLaneID] = lane
	if err := actors.Store(chatID).Save(busy); err != nil {
		t.Fatal(err)
	}
	recovered, err := NewDurableEngine(chatID, actors.Store(chatID))
	if err != nil {
		t.Fatal(err)
	}
	if state := recovered.Snapshot(); state.Revision != busy.Revision+1 || state.Lanes[state.ActiveLaneID].Phase != LaneDetached {
		t.Fatalf("recovery did not commit: revision %d phase %s", state.Revision, state.Lanes[state.ActiveLaneID].Phase)
	}
}

// discoveredSummary is what a daemon boot holds for a closed actor.
func discoveredSummary(t *testing.T, actors *SQLActors, chatID string) ActorSummary {
	t.Helper()
	var summary ActorSummary
	found := false
	if err := actors.DiscoverEach(func(head ActorHead) error {
		if head.State.ChatID != chatID {
			return nil
		}
		if head.JournalBytes != 0 || head.LedgerGen == 0 {
			t.Fatalf("head is not a plain checkpoint: journal %d gen %d", head.JournalBytes, head.LedgerGen)
		}
		stamps, err := actors.ReadLedgerStamps(chatID, head.LedgerLength, SessionActivityWindow)
		if err != nil {
			return err
		}
		summary, found = SummarizeHead(head.State, head.LedgerLength, stamps), true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatalf("%s was not discovered", chatID)
	}
	return summary
}

func TestActorSummaryAnswersLikeTheEngine(t *testing.T) {
	actors := openTestActors(t)
	const chatID = "summary-chat"
	engine := quiescentScriptedActor(t, actors, chatID)
	// Older rows carry timestamps, the newest do not: the session timestamp
	// is the newest parseable one within the window.
	state := engine.Snapshot()
	at := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	for index := 0; index < 70; index++ {
		row := historyRow(len(state.Ledger) + 1)
		row.MessageID = "summary-row-" + row.MessageID
		if index < 66 {
			row.At = at.Add(time.Duration(index) * time.Minute).Format(time.RFC3339Nano)
		}
		state.Ledger = append(state.Ledger, row)
	}
	state.Revision++
	if err := actors.Store(chatID).Save(state); err != nil {
		t.Fatal(err)
	}
	engine, err := NewDurableEngine(chatID, actors.Store(chatID))
	if err != nil {
		t.Fatal(err)
	}
	newest, err := actors.ReadNewestStamps()
	if err != nil {
		t.Fatal(err)
	}
	last := state.Ledger[len(state.Ledger)-1]
	if stamp := newest[chatID]; stamp.MessageID != last.MessageID || stamp.At != last.At || stamp.HasLedgerTimestamp() {
		t.Fatalf("newest stamp = %+v, want the undated %s", stamp, last.MessageID)
	}
	for name, summary := range map[string]ActorSummary{"discovered": discoveredSummary(t, actors, chatID), "resident": engine.Summary()} {
		if got, want := summary.DigestSnapshot(), engine.DigestSnapshot(); !reflect.DeepEqual(got, want) {
			t.Errorf("%s digest = %#v, engine %#v", name, got, want)
		}
		if got, want := summary.ActivitySnapshot(), engine.ReadActivitySnapshot(); !reflect.DeepEqual(got, want) {
			t.Errorf("%s activity differs from the engine's", name)
		}
		if got, want := summary.ProjectionSnapshot(nil), engine.ReadSessionProjectionSnapshot("another-tab", SessionActivityWindow); !reflect.DeepEqual(got, want) {
			t.Errorf("%s metadata projection differs:\n got %+v\nwant %+v", name, got, want)
		}
		if summary.LastActivityAt == 0 || summary.IdentitySnapshot() != engine.IdentitySnapshot() {
			t.Errorf("%s identity/timestamp = %+v %d", name, summary.IdentitySnapshot(), summary.LastActivityAt)
		}
		tail, err := engine.ReadMessages(0, 12)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := summary.ProjectionSnapshot(tail.Events), engine.ReadProjectionSnapshot(12); !reflect.DeepEqual(got, want) {
			t.Errorf("%s tail projection differs", name)
		}
	}
	if live, known := engine.Summary().LiveRows(); !known || len(live) != 0 {
		t.Fatalf("resident summary live rows = %v %v", live, known)
	}
}

func TestClosedLedgerReadsMatchTheEngine(t *testing.T) {
	actors := openTestActors(t)
	const chatID = "closed-reads"
	engine := quiescentScriptedActor(t, actors, chatID)
	state := engine.Snapshot()
	count := len(state.Ledger)
	if count < 3 {
		t.Fatalf("script left %d rows", count)
	}
	for _, limit := range []int{1, 2, count, count + 5} {
		want, err := engine.ReadMessages(0, limit)
		if err != nil {
			t.Fatal(err)
		}
		got, revision, err := actors.ReadLedgerPage(chatID, 0, "", limit)
		if err != nil || revision != state.Revision {
			t.Fatalf("page: revision %d err %v", revision, err)
		}
		want.Deleted = false
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("tail page of %d differs from the engine's", limit)
		}
	}
	for index, row := range state.Ledger {
		want, err := engine.ReadLedgerPageBefore(row.MessageID, 2)
		if err != nil {
			t.Fatal(err)
		}
		got, _, err := actors.ReadLedgerPage(chatID, 0, row.MessageID, 2)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("page before row %d differs: %v", index+1, err)
		}
		message, found, revision, err := actors.ReadLedgerMessage(chatID, row.MessageID)
		engineMessage, engineFound := engine.ReadMessage(row.MessageID)
		if err != nil || found != engineFound || revision != state.Revision || !reflect.DeepEqual(message, engineMessage) {
			t.Fatalf("message %s differs: %v", row.MessageID, err)
		}
	}
	if _, _, err := actors.ReadLedgerPage(chatID, 0, "no-such-message", 2); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("missing boundary: %v", err)
	}
	if _, _, err := actors.ReadLedgerPage(chatID, uint64(count)+2, "", 2); err == nil || !strings.Contains(err.Error(), "ahead") {
		t.Fatalf("boundary ahead of the ledger: %v", err)
	}

	// A backward walk visits the window's rows newest first and stops on
	// request.
	window, revision, err := actors.LedgerWindowBefore(chatID, 0, "", count)
	if err != nil || window != (LedgerWindow{Start: 0, End: count, Count: count}) || revision != state.Revision {
		t.Fatalf("window = %+v %d %v", window, revision, err)
	}
	var walked []LedgerEvent
	if err := actors.WalkLedgerBackward(chatID, window, revision, func(event LedgerEvent) (bool, error) {
		walked = append(walked, event)
		return len(walked) < count-1, nil
	}); err != nil {
		t.Fatal(err)
	}
	for index, event := range walked {
		if !reflect.DeepEqual(event, cloneLedgerEvents(state.Ledger[count-1-index : count-index])[0]) {
			t.Fatalf("walked row %d differs", index)
		}
	}
	if len(walked) != count-1 {
		t.Fatalf("walk visited %d rows, want %d", len(walked), count-1)
	}
	if err := actors.WalkLedgerBackward(chatID, window, revision+1, func(LedgerEvent) (bool, error) { return true, nil }); !errors.Is(err, errLedgerChangedDuringRead) {
		t.Fatalf("walk at a stale revision: %v", err)
	}
	if live, err := actors.ScanLedgerLiveRows(chatID, 0, count, count, revision); err != nil || len(live) != 0 {
		t.Fatalf("live rows = %v %v", live, err)
	}

	// Attachment ownership: a row's own image and a missing one.
	for _, ref := range []string{"workass-session-image:images/abc", "workass-session-image:images/def", "workass-session-image:images/none"} {
		owned, _, err := actors.LedgerOwnsAttachment(chatID, ref)
		if err != nil || owned != engine.AttachmentRefOwnership(ref).Owned {
			t.Fatalf("ownership of %s = %v %v", ref, owned, err)
		}
	}
	if found, err := actors.LedgerHasNativeTurn(chatID, "native-2"); err != nil || !found {
		t.Fatalf("native turn lookup = %v %v", found, err)
	}
	if found, err := actors.LedgerHasNativeTurn(chatID, "native-404"); err != nil || found {
		t.Fatalf("missing native turn lookup = %v %v", found, err)
	}
}

func TestLiveRowScanFindsOnlyRowsWithTheirOwnLiveStatus(t *testing.T) {
	actors := openTestActors(t)
	state := largeLedgerState(t, "live-rows", 6)
	state.Ledger[1].Status = "running"
	// A nested running tool status is not the row's own.
	state.Ledger[3].Timeline = []TimelineEntry{{Key: "k", Kind: provider.EventToolUpdate, Tool: &provider.ToolEvent{ToolCallID: "k", Status: "running"}}}
	if err := actors.Store("live-rows").Save(state); err != nil {
		t.Fatal(err)
	}
	live, err := actors.ScanLedgerLiveRows("live-rows", 0, 6, 6, state.Revision)
	if err != nil || !reflect.DeepEqual(live, []int{2}) {
		t.Fatalf("live rows = %v %v", live, err)
	}
	engine, err := NewDurableEngine("live-rows", actors.Store("live-rows"))
	if err != nil {
		t.Fatal(err)
	}
	if rows, known := engine.Summary().LiveRows(); !known || !reflect.DeepEqual(rows, []int{2}) {
		t.Fatalf("resident live rows = %v %v", rows, known)
	}
}

func TestRetireTakesOnlyAQuiescentActorAndThenRefusesCommits(t *testing.T) {
	actors := openTestActors(t)
	const chatID = "retire-chat"
	engine := quiescentScriptedActor(t, actors, chatID)
	if _, ok, err := engine.Retire(func(*State) bool { return false }); ok || err != nil {
		t.Fatalf("a refused retirement retired: %v %v", ok, err)
	}
	summary, ok, err := engine.Retire(nil)
	if !ok || err != nil {
		t.Fatalf("retire = %v %v", ok, err)
	}
	if summary.ChatID != chatID || summary.LedgerCount != len(engine.Snapshot().Ledger) {
		t.Fatalf("summary = %+v", summary)
	}
	next := engine.Snapshot().Presentation.Clone()
	next.Draft = "after retirement"
	if err := engine.Apply(SavePresentation{OperationID: "late", Digest: "late", Presentation: next}); !errors.Is(err, ErrActorRetired) {
		t.Fatalf("commit on a retired engine: %v", err)
	}
	if _, ok, err := engine.ClaimNext(); ok || err != nil {
		t.Fatalf("claim on a retired engine: %v %v", ok, err)
	}
	if _, _, err := engine.Retire(nil); !errors.Is(err, ErrActorRetired) {
		t.Fatalf("second retirement: %v", err)
	}
	reopened, err := NewDurableEngine(chatID, actors.Store(chatID))
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.Apply(SavePresentation{OperationID: "late", Digest: "late", Presentation: next}); err != nil {
		t.Fatal(err)
	}
}

func TestCheckpointForRetireFoldsTheJournal(t *testing.T) {
	actors := openTestActors(t)
	const chatID = "fold-journal"
	engine, err := NewDurableEngine(chatID, actors.Store(chatID))
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range actorLifecycleScript(chatID) {
		step.run(t, engine)
		if step.name == "usage" {
			break
		}
	}
	if sqlJournalCount(t, actors, chatID) == 0 {
		t.Fatal("script left no journal to fold")
	}
	state := engine.Snapshot()
	store := actors.Store(chatID)
	if _, _, err := store.Load(chatID); err != nil {
		t.Fatal(err)
	}
	if !store.needsCheckpointAfterLoad() {
		t.Fatal("a store that replayed journal records does not ask for a checkpoint")
	}
	if err := store.checkpointForRetire(state); err != nil {
		t.Fatal(err)
	}
	if rows := sqlJournalCount(t, actors, chatID); rows != 0 {
		t.Fatalf("%d journal rows survive the retirement checkpoint", rows)
	}
	loaded, _, err := actors.Store(chatID).Load(chatID)
	if err != nil {
		t.Fatal(err)
	}
	if !equalCancelDurableState(t, loaded, state) {
		got, _ := json.Marshal(loaded)
		want, _ := json.Marshal(state)
		t.Fatalf("folded checkpoint differs: %s", firstJSONDifference(got, want))
	}
}
