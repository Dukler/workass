package fleetlog

import (
	"fmt"
	"testing"
	"time"
)

const (
	testSubagent = "wa:m-home/subagent/wa-subagent-1792187130561-8"
	testWatcher  = "wa:m-away/chat/app-chat-1792187130561"
)

func fixedClock(start time.Time) (func() time.Time, func(time.Duration)) {
	now := start
	return func() time.Time { return now }, func(step time.Duration) { now = now.Add(step) }
}

func openTestStore(t *testing.T, dir string, clock func() time.Time, owed func(string) (uint64, bool)) *Store {
	t.Helper()
	store, err := Open(dir, Options{Now: clock, Logf: t.Logf, Owed: owed})
	if err != nil {
		t.Fatalf("open the log store: %v", err)
	}
	return store
}

// The ordinary case, and the two watermarks a subscriber reads to tell
// "current" from "behind" without a poll of its own.
func TestALogServesACursorInOrderWithItsWatermarks(t *testing.T) {
	clock, advance := fixedClock(time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC))
	store := openTestStore(t, t.TempDir(), clock, nil)
	for i := 1; i <= 3; i++ {
		advance(time.Second)
		event, err := store.Append(testSubagent, KindProgress, map[string]any{"n": i})
		if err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		if event.Seq != uint64(i) {
			t.Fatalf("append %d got seq %d", i, event.Seq)
		}
	}
	slice, err := store.Since(testSubagent, 1, 0)
	if err != nil {
		t.Fatalf("since: %v", err)
	}
	if len(slice.Events) != 2 || slice.Events[0].Seq != 2 || slice.Events[1].Seq != 3 {
		t.Fatalf("a cursor at 1 was served %#v", slice.Events)
	}
	if slice.Head != 3 || slice.FirstSeq != 1 || slice.Truncated {
		t.Fatalf("watermarks = head %d first %d truncated %v", slice.Head, slice.FirstSeq, slice.Truncated)
	}
	// A cursor at the head is current, and says so with an empty slice rather
	// than with an error or a re-read.
	current, err := store.Since(testSubagent, 3, 0)
	if err != nil || len(current.Events) != 0 || current.Head != 3 {
		t.Fatalf("a current cursor = %#v, %v", current, err)
	}
	// An entity with no log is not an error either: head zero, nothing owed.
	empty, err := store.Since("wa:m-home/subagent/never", 0, 0)
	if err != nil || len(empty.Events) != 0 || empty.Head != 0 {
		t.Fatalf("an entity with no log = %#v, %v", empty, err)
	}
}

// §13 P3: "a home restarted between append and delivery still owes exactly
// those events". The log is the authoritative history, not a cache of a live
// process, so a restart is invisible to the cursor.
func TestAHomeRestartedBetweenAppendAndDeliveryStillOwesExactlyThoseEvents(t *testing.T) {
	dir := t.TempDir()
	clock, advance := fixedClock(time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC))
	first := openTestStore(t, dir, clock, nil)
	for i := 1; i <= 4; i++ {
		advance(time.Second)
		if _, err := first.Append(testSubagent, KindProgress, map[string]any{"n": i}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	advance(time.Second)
	if _, err := first.Append(testSubagent, KindExit, map[string]any{"outcome": OutcomeSettled}); err != nil {
		t.Fatalf("append the exit: %v", err)
	}

	// The daemon dies here, with the watcher's cursor at 2 and nothing acked.
	restarted := openTestStore(t, dir, clock, nil)
	slice, err := restarted.Since(testSubagent, 2, 0)
	if err != nil {
		t.Fatalf("since after restart: %v", err)
	}
	if len(slice.Events) != 3 {
		t.Fatalf("after a restart the home owed %d event(s), want exactly 3", len(slice.Events))
	}
	for index, want := range []uint64{3, 4, 5} {
		if slice.Events[index].Seq != want {
			t.Fatalf("event %d has seq %d, want %d", index, slice.Events[index].Seq, want)
		}
	}
	if slice.Events[2].Kind != KindExit {
		t.Fatalf("the terminal event survived the restart as %q", slice.Events[2].Kind)
	}
	if to := slice.Events[2].Payload["outcome"]; to != OutcomeSettled {
		t.Fatalf("the restored exit payload = %#v", slice.Events[2].Payload)
	}
	// The next append continues the same sequence rather than restarting it,
	// which is what makes dedup by seq safe across a restart.
	advance(time.Second)
	next, err := restarted.Append(testSubagent, KindProgress, nil)
	if err != nil || next.Seq != 6 {
		t.Fatalf("the first append after a restart = seq %d, %v", next.Seq, err)
	}
}

// Compaction and resync: a subscriber below the watermark is told, resyncs from
// the head, and is never silently skipped past a gap (§5).
func TestACursorBelowTheWatermarkIsToldToResyncRatherThanSilentlySkipped(t *testing.T) {
	clock, advance := fixedClock(time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC))
	store, err := Open(t.TempDir(), Options{Now: clock, Logf: t.Logf, Retention: Retention{MaxAge: 7 * 24 * time.Hour, MaxEvents: 5}})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for i := 1; i <= 9; i++ {
		advance(time.Second)
		if _, err := store.Append(testSubagent, KindProgress, map[string]any{"n": i}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	slice, err := store.Since(testSubagent, 1, 0)
	if err != nil {
		t.Fatalf("since: %v", err)
	}
	if !slice.Truncated {
		t.Fatalf("a cursor at 1 against a compacted log was served as if nothing were missing: %#v", slice)
	}
	if slice.FirstSeq != 5 {
		t.Fatalf("the watermark after compaction = %d, want 5 (the five kept of nine)", slice.FirstSeq)
	}
	if slice.TruncatedTo != 4 {
		t.Fatalf("truncated to_seq = %d, want 4", slice.TruncatedTo)
	}
	// Resync from the head means the kept events, in order — not an error and
	// not an empty answer.
	if len(slice.Events) != 5 || slice.Events[0].Seq != 5 || slice.Events[4].Seq != 9 {
		t.Fatalf("the resync slice = %#v", slice.Events)
	}
	// A cursor inside the surviving window is served normally.
	inside, err := store.Since(testSubagent, 7, 0)
	if err != nil || inside.Truncated || len(inside.Events) != 2 {
		t.Fatalf("a cursor inside the window = %#v, %v", inside, err)
	}
}

// Retention by age, and the one case that outranks it: a terminal event still
// owed to a monitor is kept, and leaves only once that monitor has acked.
func TestATerminalEventSurvivesRetentionUntilItIsAcknowledged(t *testing.T) {
	clock, advance := fixedClock(time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
	acked, monitored := uint64(0), true
	store, err := Open(t.TempDir(), Options{
		Now: clock, Logf: t.Logf,
		Retention: Retention{MaxAge: 24 * time.Hour, MaxEvents: 1000},
		Owed:      func(string) (uint64, bool) { return acked, monitored },
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := store.Append(testSubagent, KindProgress, map[string]any{"n": 1}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if _, err := store.Append(testSubagent, KindExit, map[string]any{"outcome": OutcomeFailed}); err != nil {
		t.Fatalf("append the exit: %v", err)
	}

	// Four days later the window has long passed. The progress event goes; the
	// unacknowledged outcome does not.
	advance(4 * 24 * time.Hour)
	store.Prune()
	slice, err := store.Since(testSubagent, 0, 0)
	if err != nil {
		t.Fatalf("since: %v", err)
	}
	kinds := map[string]int{}
	for _, event := range slice.Events {
		kinds[event.Kind]++
	}
	if kinds[KindExit] != 1 {
		t.Fatalf("an unacknowledged outcome was evicted by retention: %#v", slice.Events)
	}
	if kinds[KindProgress] != 0 {
		t.Fatalf("an expired progress event survived: %#v", slice.Events)
	}

	// Acknowledged, the same sweep lets it go — and the truncation marker says
	// so, so eviction is never silent.
	acked = 2
	store.Prune()
	after, err := store.Since(testSubagent, 0, 0)
	if err != nil {
		t.Fatalf("since after the ack: %v", err)
	}
	for _, event := range after.Events {
		if event.Kind == KindExit {
			t.Fatalf("an acknowledged outcome was kept forever: %#v", after.Events)
		}
	}
	if after.Head != 2 {
		t.Fatalf("compaction lost the head: %d", after.Head)
	}
	// An entity nobody monitors has nothing owed, so its terminal event is
	// ordinary and expires with everything else.
	monitored = false
	if _, err := store.Append("wa:m-home/subagent/unwatched", KindExit, nil); err != nil {
		t.Fatalf("append: %v", err)
	}
	advance(4 * 24 * time.Hour)
	store.Prune()
	unwatched, err := store.Since("wa:m-home/subagent/unwatched", 0, 0)
	if err != nil {
		t.Fatalf("since: %v", err)
	}
	for _, event := range unwatched.Events {
		if event.Kind == KindExit {
			t.Fatalf("an unmonitored outcome was held back: %#v", unwatched.Events)
		}
	}
}

// Two entities whose ids sanitize to the same characters must not share a file.
// The id is the identity; the filename is only a place to put it.
func TestEntityFilesAreReadableAndNeverCollide(t *testing.T) {
	left := entityFileName("wa:m-home/subagent/one")
	right := entityFileName("wa:m-home/subagent:one")
	if left == right {
		t.Fatalf("two ids share one log file: %s", left)
	}
	clock, _ := fixedClock(time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC))
	store := openTestStore(t, t.TempDir(), clock, nil)
	for _, entity := range []string{"wa:m-home/subagent/one", "wa:m-home/subagent:one"} {
		if _, err := store.Append(entity, KindExit, nil); err != nil {
			t.Fatalf("append %s: %v", entity, err)
		}
	}
	for _, entity := range []string{"wa:m-home/subagent/one", "wa:m-home/subagent:one"} {
		slice, err := store.Since(entity, 0, 0)
		if err != nil || len(slice.Events) != 1 || slice.Events[0].Entity != entity {
			t.Fatalf("%s reads back as %#v (%v)", entity, slice.Events, err)
		}
	}
	if entities := store.Entities(); len(entities) != 2 {
		t.Fatalf("the store lists %#v", entities)
	}
}

// A slice is bounded, so a four-day backlog is delivered across several of them
// rather than in one oversized frame.
func TestASliceIsBoundedSoABacklogArrivesInOrderedBatches(t *testing.T) {
	clock, advance := fixedClock(time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC))
	store, err := Open(t.TempDir(), Options{Now: clock, Logf: t.Logf, Retention: Retention{MaxAge: time.Hour * 24 * 30, MaxEvents: 5000}})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for i := 1; i <= maxSliceEvents+10; i++ {
		advance(time.Millisecond)
		if _, err := store.Append(testSubagent, KindProgress, map[string]any{"n": i}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	cursor := uint64(0)
	batches, seen := 0, 0
	for {
		slice, err := store.Since(testSubagent, cursor, 0)
		if err != nil {
			t.Fatalf("since: %v", err)
		}
		if len(slice.Events) == 0 {
			break
		}
		batches++
		for _, event := range slice.Events {
			if event.Seq != cursor+1 {
				t.Fatalf("batch %d broke the order at seq %d (cursor %d)", batches, event.Seq, cursor)
			}
			cursor = event.Seq
			seen++
		}
		if batches > 10 {
			t.Fatal("the backlog never drained")
		}
	}
	if seen != maxSliceEvents+10 || batches != 2 {
		t.Fatalf("%d event(s) in %d batch(es)", seen, batches)
	}
}

func TestAnAppendNeedsAnEntityAndAKind(t *testing.T) {
	clock, _ := fixedClock(time.Now())
	store := openTestStore(t, t.TempDir(), clock, nil)
	for _, bad := range [][2]string{{"", KindExit}, {testSubagent, ""}} {
		if _, err := store.Append(bad[0], bad[1], nil); err == nil {
			t.Fatalf("append(%q, %q) was accepted", bad[0], bad[1])
		}
	}
	var absent *Store
	if _, err := absent.Append(testSubagent, KindExit, nil); err == nil {
		t.Fatal("a nil store accepted an append")
	}
	if head := absent.Head(testSubagent); head != 0 {
		t.Fatalf("a nil store reported head %d", head)
	}
}

func TestProgressIsOptInOnEveryMonitor(t *testing.T) {
	plain := Registration{WatcherID: testWatcher, TargetID: testSubagent}
	if plain.Allows(KindProgress) {
		t.Fatal("a monitor with no stated kinds carried progress")
	}
	for _, kind := range []string{KindExit, KindAttention, KindWaiting, KindResumed} {
		if !plain.Allows(kind) {
			t.Fatalf("a monitor with no stated kinds refused %s", kind)
		}
	}
	opted := Registration{WatcherID: testWatcher, TargetID: testSubagent, Kinds: []string{KindProgress}}
	if !opted.Allows(KindProgress) || opted.Allows(KindExit) {
		t.Fatalf("an explicit kind list is not honoured: %#v", opted)
	}
	if plain.Allows("") {
		t.Fatal("an empty kind was allowed")
	}
}

func TestTerminalNamesOnlyTheEndOfALife(t *testing.T) {
	if !Terminal(KindExit) {
		t.Fatal("exit is not terminal")
	}
	for _, kind := range []string{KindAttention, KindWaiting, KindResumed, KindProgress, KindSpawned, KindTruncated} {
		if Terminal(kind) {
			t.Fatalf("%s was treated as terminal, which would hold it back from retention forever", kind)
		}
	}
}

func TestUint64FieldReadsEveryShapeAJSONNumberArrivesIn(t *testing.T) {
	for _, value := range []any{uint64(7), int64(7), 7, float64(7), "7"} {
		if got := uint64Field(map[string]any{"to_seq": value}, "to_seq"); got != 7 {
			t.Fatalf("%T %v read back as %d", value, value, got)
		}
	}
	if got := uint64Field(nil, "to_seq"); got != 0 {
		t.Fatalf("a nil payload read back as %d", got)
	}
}

func BenchmarkAppend(b *testing.B) {
	store, err := Open(b.TempDir(), Options{})
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < b.N; i++ {
		if _, err := store.Append(fmt.Sprintf("wa:m-home/subagent/bench-%d", i%8), KindProgress, nil); err != nil {
			b.Fatal(err)
		}
	}
}

// A subagent is an entity, so a busy week is thousands of entities. A log that
// only ever compacted would leave one empty file per subagent forever.
func TestASpentLogIsRemovedButAMonitoredOneIsKept(t *testing.T) {
	dir := t.TempDir()
	clock, advance := fixedClock(time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
	monitored := map[string]bool{"wa:m-home/subagent/watched": true}
	store, err := Open(dir, Options{
		Now: clock, Logf: t.Logf, Retention: Retention{MaxAge: time.Hour, MaxEvents: 1000},
		Owed: func(entity string) (uint64, bool) { return 99, monitored[entity] },
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for _, entity := range []string{"wa:m-home/subagent/spent", "wa:m-home/subagent/watched"} {
		if _, err := store.Append(entity, KindExit, map[string]any{"outcome": OutcomeSettled}); err != nil {
			t.Fatalf("append %s: %v", entity, err)
		}
	}
	advance(2 * time.Hour)
	store.Prune()
	entities := store.Entities()
	for _, entity := range entities {
		if entity == "wa:m-home/subagent/spent" {
			t.Fatalf("a spent log was kept: %#v", entities)
		}
	}
	// The monitored one is kept, however empty: the monitor is a standing claim,
	// and an absent file would answer head 0 where the truth is "delivered".
	if head := store.Head("wa:m-home/subagent/watched"); head != 1 {
		t.Fatalf("the monitored log reports head %d", head)
	}
	// The removed one answers as an entity with no log — not as an error.
	slice, err := store.Since("wa:m-home/subagent/spent", 0, 0)
	if err != nil || slice.Head != 0 || len(slice.Events) != 0 {
		t.Fatalf("a removed log reads back as %#v (%v)", slice, err)
	}
	// And it can be written again, starting a fresh sequence.
	if event, err := store.Append("wa:m-home/subagent/spent", KindExit, nil); err != nil || event.Seq != 1 {
		t.Fatalf("appending to a removed log = seq %d, %v", event.Seq, err)
	}
}
