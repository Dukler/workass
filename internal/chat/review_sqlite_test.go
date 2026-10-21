//go:build review

// Independent-review reproduction (b8b18322..fe7003e5). Run with:
//   go test -tags review -count=1 -run TestReview ./internal/chat/

package chat

// Independent-review reproductions for the SQLite chat store (fe7003e5).
// Not part of the product suite; copy or drop as the caller decides.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"workass/internal/provider"
)

// R5 (measurement): the per-handle exact ledger copy duplicates every
// struct, slice and pointer of the resident ledger (strings are shared).
func TestReviewLedgerCacheOverhead(t *testing.T) {
	if testing.Short() {
		t.Skip("measurement")
	}
	const rows, toolsPerRow = 3000, 30
	state := largeLedgerState(t, "cache-chat", rows)
	output := strings.Repeat("o", 2000)
	for index := range state.Ledger {
		timeline := make([]TimelineEntry, 0, toolsPerRow)
		for tool := 0; tool < toolsPerRow; tool++ {
			timeline = append(timeline, TimelineEntry{Key: "tool", Kind: provider.EventToolUpdate, Tool: &provider.ToolEvent{ToolCallID: "call", Title: "Read", Status: "completed", Output: output}})
		}
		state.Ledger[index].Timeline = timeline
	}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	copied := exactLedgerCopy(state.Ledger)
	runtime.GC()
	runtime.ReadMemStats(&after)
	stringBytes := rows * toolsPerRow * len(output)
	t.Logf("MEASURED: cache copy of %d rows x %d tools retains %.1f MiB beyond the state (shared tool output strings: %.1f MiB)",
		rows, toolsPerRow, float64(after.HeapAlloc-before.HeapAlloc)/(1<<20), float64(stringBytes)/(1<<20))
	runtime.KeepAlive(copied)
	runtime.KeepAlive(state)
}

// R1 (fixed, inverted): once the store is live, a JSON backup whose bytes are
// unchanged but whose mtime moved (touch, cp without -p, a restore tool) used
// to make VerifyLiveFileActors fail, and openChatActorBackend turned that into
// a refused daemon start. The live check now hashes a file whose time moved and
// accepts identical bytes; TestVerifyLiveFileActorsAcceptsSameBytesWithNewTimes
// is the regression test in the normal suite.
func TestReviewLiveVerifyRefusesTouchedButIdenticalJSON(t *testing.T) {
	stateDir := t.TempDir()
	actorDir := filepath.Join(stateDir, "provider-chats")
	if err := os.MkdirAll(actorDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(actorDir, actorFileName("touched-chat"))
	if _, err := NewDurableEngine("touched-chat", FileStore{Path: path}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	actors := openTestActors(t)
	if _, err := ImportFileActors(context.Background(), actors.db, corpusImportOptions(stateDir)); err != nil {
		t.Fatal(err)
	}
	if err := VerifyLiveFileActors(context.Background(), actors.db, corpusImportOptions(stateDir)); err != nil {
		t.Fatalf("fresh cutover: %v", err)
	}
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(before) {
		t.Fatal("fixture changed bytes")
	}
	// A second boot: import is a no-op on a live store, then verify runs.
	result, err := ImportFileActors(context.Background(), actors.db, corpusImportOptions(stateDir))
	if err != nil || !result.Live {
		t.Fatalf("import on live store: %+v %v", result, err)
	}
	if err := VerifyLiveFileActors(context.Background(), actors.db, corpusImportOptions(stateDir)); err != nil {
		t.Fatalf("touched-but-identical JSON is still refused: %v", err)
	}
	t.Logf("FIXED: byte-identical JSON with a new mtime is accepted")
}

// R2: import does not run State.Validate on an actor without a journal, so a
// file that FileStore discovery refuses at boot (fails closed) is imported,
// the store flips live, and the bad actor only fails when opened. The JSON is
// then a frozen backup and the fix-the-file recovery path is gone.
func TestReviewImportAcceptsAnActorFileDiscoveryRefuses(t *testing.T) {
	stateDir := t.TempDir()
	dir := filepath.Join(stateDir, "provider-chats")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	state, err := NewState("bad-sequence")
	if err != nil {
		t.Fatal(err)
	}
	state.Revision = 1
	state.Ledger = []LedgerEvent{historyRow(5)} // sequence must be 1
	raw, err := json.Marshal(stateEnvelope{Version: currentStateEnvelopeVersion, State: state})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, actorFileName("bad-sequence")), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := DiscoverFileStates(dir); err == nil {
		t.Fatal("file discovery accepted the fixture")
	} else {
		t.Logf("file discovery refuses: %v", err)
	}
	actors := openTestActors(t)
	result, err := ImportFileActors(context.Background(), actors.db, corpusImportOptions(stateDir))
	if err != nil {
		t.Fatalf("import refused too (no finding): %v", err)
	}
	heads, err := actors.Discover()
	if err != nil {
		t.Fatalf("sql discover: %v", err)
	}
	_, _, loadErr := actors.Store("bad-sequence").Load("bad-sequence")
	t.Logf("REPRODUCED: import result %+v, SQL discover lists %d heads, open fails later: %v", result, len(heads), loadErr)
	if !result.Live || len(heads) != 1 || loadErr == nil {
		t.Fatalf("unexpected: live=%v heads=%d loadErr=%v", result.Live, len(heads), loadErr)
	}
}

// R3 (measurement): every chat shares one connection, and Load decodes,
// validates and replays inside its read transaction. A small chat's commit
// waits for a large chat's whole open.
func TestReviewLargeLoadBlocksOtherChatsCommits(t *testing.T) {
	if testing.Short() {
		t.Skip("measurement")
	}
	actors := openTestActors(t)
	big := largeLedgerState(t, "big-chat", 1500)
	text := strings.Repeat("x", 100_000)
	for index := range big.Ledger {
		big.Ledger[index].Text = text + big.Ledger[index].Text
	}
	if err := actors.Store("big-chat").Save(big); err != nil {
		t.Fatal(err)
	}
	small := largeLedgerState(t, "small-chat", 4)
	smallStore := actors.Store("small-chat")
	if err := smallStore.Save(small); err != nil {
		t.Fatal(err)
	}
	// Warm: an uncontended small commit.
	next := small
	next.Revision++
	started := time.Now()
	if err := smallStore.Save(next); err != nil {
		t.Fatal(err)
	}
	idle := time.Since(started)

	var wg sync.WaitGroup
	var loadTook time.Duration
	wg.Add(1)
	loadStarted := make(chan struct{})
	go func() {
		defer wg.Done()
		close(loadStarted)
		begin := time.Now()
		if _, _, err := actors.Store("big-chat").Load("big-chat"); err != nil {
			t.Error(err)
		}
		loadTook = time.Since(begin)
	}()
	<-loadStarted
	time.Sleep(20 * time.Millisecond)
	next.Revision++
	started = time.Now()
	if err := smallStore.Save(next); err != nil {
		t.Fatal(err)
	}
	blocked := time.Since(started)
	wg.Wait()
	t.Logf("MEASURED: ~150 MB chat Load %v; small chat commit idle %v, during that Load %v", loadTook, idle, blocked)
}

// R4 (measurement): import throughput, which bounds first-boot time on a
// state dir (health is not served until the import finishes).
func TestReviewImportThroughput(t *testing.T) {
	if testing.Short() {
		t.Skip("measurement")
	}
	stateDir := t.TempDir()
	dir := filepath.Join(stateDir, "provider-chats")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	var total int64
	text := strings.Repeat("y", 100_000)
	for _, chatID := range []string{"import-a", "import-b"} {
		state := largeLedgerState(t, chatID, 1000)
		for index := range state.Ledger {
			state.Ledger[index].Text = text + state.Ledger[index].Text
		}
		path := filepath.Join(dir, actorFileName(chatID))
		if err := (FileStore{Path: path}).Save(state); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		total += info.Size()
	}
	actors := openTestActors(t)
	started := time.Now()
	if _, err := ImportFileActors(context.Background(), actors.db, corpusImportOptions(stateDir)); err != nil {
		t.Fatal(err)
	}
	took := time.Since(started)
	t.Logf("MEASURED: imported %.0f MiB in %v (%.0f MiB/s)", float64(total)/(1<<20), took, float64(total)/(1<<20)/took.Seconds())
}

// R6: randomized incremental checkpoints across two handles always reload the
// last saved ledger exactly.
func TestReviewRandomizedIncrementalCheckpoints(t *testing.T) {
	const chatID = "random-chat"
	actors := openTestActors(t)
	handles := []*SQLStore{actors.Store(chatID), actors.Store(chatID)}
	state := largeLedgerState(t, chatID, 50)
	if err := handles[0].Save(state); err != nil {
		t.Fatal(err)
	}
	seed := uint64(12345)
	rnd := func(n int) int {
		seed = seed*6364136223846793005 + 1442695040888963407
		return int((seed >> 33) % uint64(n))
	}
	for step := 0; step < 600; step++ {
		next := state
		next.Revision++
		switch rnd(5) {
		case 0: // append
			next.Ledger = append(exactLedgerCopy(state.Ledger), historyRow(len(state.Ledger)+1))
		case 1: // edit a random row in a fresh copy
			next.Ledger = exactLedgerCopy(state.Ledger)
			if len(next.Ledger) > 0 {
				next.Ledger[rnd(len(next.Ledger))].Text = fmt.Sprintf("edit-%d", step)
			}
		case 2: // truncate
			if len(state.Ledger) > 1 {
				next.Ledger = exactLedgerCopy(state.Ledger[:len(state.Ledger)-1-rnd(len(state.Ledger)/2)])
			}
		case 3: // clone (normalizes nil/empty)
			next = state.Clone()
			next.Revision = state.Revision + 1
		case 4: // same slice (streamed-event shape)
		}
		handle := handles[rnd(2)]
		if err := handle.Save(next); err != nil {
			t.Fatalf("step %d: %v", step, err)
		}
		state = next
		loaded, found, err := actors.Store(chatID).Load(chatID)
		if err != nil || !found {
			t.Fatalf("step %d reload: %v", step, err)
		}
		got, _ := json.Marshal(loaded.Ledger)
		want, _ := json.Marshal(state.Ledger)
		if string(got) != string(want) && !(len(loaded.Ledger) == 0 && len(state.Ledger) == 0) {
			t.Fatalf("step %d: reloaded ledger differs (%d vs %d rows)", step, len(loaded.Ledger), len(state.Ledger))
		}
	}
	t.Logf("randomized: 600 checkpoints across two handles reloaded exactly")
}

// R7 (latent contract): the O(1) "same slice header" proof only compares the
// newest row, so an in-place edit of an older row in a shared backing array is
// silently not written. No reducer path does this today (journaled events
// never edit the ledger in place; Reduce deep-clones), so this is a contract,
// not a live bug.
func TestReviewInPlaceEditOfSharedLedgerIsNotWritten(t *testing.T) {
	const chatID = "inplace-chat"
	actors := openTestActors(t)
	handle := actors.Store(chatID)
	state := largeLedgerState(t, chatID, 3)
	if err := handle.Save(state); err != nil {
		t.Fatal(err)
	}
	next := state
	next.Revision++
	next.Ledger[0].Text = "edited in the shared array"
	if err := handle.Save(next); err != nil {
		t.Fatal(err)
	}
	loaded, _, err := actors.Store(chatID).Load(chatID)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("in-place edit of row 1 after reload: %q (dropped=%v)", loaded.Ledger[0].Text, loaded.Ledger[0].Text != "edited in the shared array")
}
