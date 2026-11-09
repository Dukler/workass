package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workass/internal/chat"
	"workass/internal/wire"
)

// ---- chatLifecycle settings ------------------------------------------------

func TestChatLifecycleSettingsDefaultToNoAgeFilingAndTheFiveDayArchive(t *testing.T) {
	got := normalizeSettings(nil)["chatLifecycle"]
	want := map[string]any{"settleAfterDays": 0, "archiveAfterDays": 5}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("default chatLifecycle = %#v, want %#v", got, want)
	}
	if got := normalizeSettings(map[string]any{"chatTitles": map[string]any{}})["chatLifecycle"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("a blob without chatLifecycle normalizes to %#v, want the defaults", got)
	}
}

func TestChatLifecycleSettingsKeepWholeDaysAndRefuseTheRest(t *testing.T) {
	cases := []struct {
		in   map[string]any
		want map[string]any
	}{
		{map[string]any{"settleAfterDays": 7, "archiveAfterDays": 30}, map[string]any{"settleAfterDays": 7, "archiveAfterDays": 30}},
		{map[string]any{"settleAfterDays": json.Number("3"), "archiveAfterDays": json.Number("0")}, map[string]any{"settleAfterDays": 3, "archiveAfterDays": 0}},
		{map[string]any{"settleAfterDays": float64(14), "archiveAfterDays": float64(365)}, map[string]any{"settleAfterDays": 14, "archiveAfterDays": 365}},
		{map[string]any{"settleAfterDays": -1, "archiveAfterDays": 366}, map[string]any{"settleAfterDays": 0, "archiveAfterDays": 5}},
		{map[string]any{"settleAfterDays": 1.5, "archiveAfterDays": json.Number("2.5")}, map[string]any{"settleAfterDays": 0, "archiveAfterDays": 5}},
		{map[string]any{"settleAfterDays": "x", "archiveAfterDays": nil}, map[string]any{"settleAfterDays": 0, "archiveAfterDays": 5}},
	}
	for _, tc := range cases {
		if got := normalizeChatLifecycle(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("normalizeChatLifecycle(%#v) = %#v, want %#v", tc.in, got, tc.want)
		}
	}
}

// settings:set replaces the whole blob: the key must survive a write and a
// read back from disk, or the Settings control would snap back.
func TestChatLifecycleSurvivesTheSettingsFile(t *testing.T) {
	store := newAppSettingsStore(filepath.Join(t.TempDir(), "app-settings.json"))
	saved, err := store.set(map[string]any{"chatLifecycle": map[string]any{"settleAfterDays": 7, "archiveAfterDays": 30}})
	if err != nil {
		t.Fatal(err)
	}
	read := store.get()
	want := map[string]any{"settleAfterDays": 7, "archiveAfterDays": 30}
	if !reflect.DeepEqual(read["chatLifecycle"], want) {
		t.Fatalf("read back chatLifecycle = %#v, want %#v", read["chatLifecycle"], want)
	}
	if settingsRevision(saved) != settingsRevision(read) {
		t.Fatal("the revision of the saved blob differs from the revision read back, so a compare-and-swap write would always conflict")
	}
}

// ---- paged shell: chats filed by age ---------------------------------------

func TestPagedShellLeavesOutChatsFiledByAgeOnlyWhenTheRendererAsks(t *testing.T) {
	const day = int64(24 * 60 * 60 * 1000)
	now := int64(1_800_000_000_000)
	idleBefore := now - 40*day
	quiet := chat.PageFacts{TabID: "tab", ChatID: "chat", Initialized: true, LastActivityAt: now - 60*day}
	page := &sessionPage{settledBefore: now - 6*day, idleBefore: idleBefore, include: map[string]struct{}{}}
	if page.lists(quiet) {
		t.Fatal("a chat idle since before idleBefore was listed; the sidebar files it in the archive")
	}
	if (&sessionPage{settledBefore: now - 6*day, include: map[string]struct{}{}}).lists(quiet) == false {
		t.Fatal("without idleBefore a never-settled chat must be listed, as before")
	}
	listed := func(name string, facts chat.PageFacts) {
		t.Helper()
		if !page.lists(facts) {
			t.Fatalf("%s was left out; the sidebar would not file it", name)
		}
	}
	recent := quiet
	recent.LastActivityAt = now - 10*day
	listed("a chat active after idleBefore", recent)
	never := quiet
	never.LastActivityAt = 0
	listed("a chat with no activity at all", never)
	reactivated := quiet
	reactivated.Settled = "active"
	listed("a chat reactivated by hand", reactivated)
	for name, mutate := range map[string]func(*chat.PageFacts){
		"unread":             func(f *chat.PageFacts) { f.Unread = true },
		"running":            func(f *chat.PageFacts) { f.Foreground = true },
		"asking permission":  func(f *chat.PageFacts) { f.PendingPermissions = true },
		"queued":             func(f *chat.PageFacts) { f.Queued = true },
		"running background": func(f *chat.PageFacts) { f.RunningBackground = true },
		"parked":             func(f *chat.PageFacts) { f.ObligationState = "parked" },
		"needs input":        func(f *chat.PageFacts) { f.ObligationState = "needs_input" },
		"stalled":            func(f *chat.PageFacts) { f.ObligationState = "stalled" },
	} {
		facts := quiet
		mutate(&facts)
		listed("a "+name+" chat", facts)
	}
	named := &sessionPage{settledBefore: page.settledBefore, idleBefore: idleBefore, include: map[string]struct{}{"tab": {}}}
	if !named.lists(quiet) {
		t.Fatal("a chat the renderer names is always listed")
	}
	search := &sessionPage{settledBefore: page.settledBefore, idleBefore: idleBefore, include: map[string]struct{}{}, needle: "needle"}
	match := quiet
	match.Title = "find the needle"
	if !search.lists(match) {
		t.Fatal("a search match is listed even when filed by age")
	}
	// The explicit shelf keeps its own rule.
	settled := quiet
	settled.Settled, settled.SettledAt = "settled", now-day
	listed("a chat settled yesterday", settled)
	settled.SettledAt = now - 30*day
	if page.lists(settled) {
		t.Fatal("a chat settled long ago was listed")
	}
}

func TestSessionPageReadsIdleBefore(t *testing.T) {
	page := sessionPageFrom(map[string]any{"page": map[string]any{"settledBefore": float64(10), "idleBefore": float64(7)}})
	if page == nil || page.idleBefore != 7 || page.settledBefore != 10 {
		t.Fatalf("page = %#v", page)
	}
	if page := sessionPageFrom(map[string]any{"page": map[string]any{"settledBefore": float64(10)}}); page.idleBefore != 0 {
		t.Fatalf("a page without idleBefore read %d", page.idleBefore)
	}
}

// ---- storage:usage ---------------------------------------------------------

func writeSized(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestStorageUsageSplitsTheStateDirectoryIntoWhatItHolds(t *testing.T) {
	dir := t.TempDir()
	writeSized(t, filepath.Join(dir, "state.db"), 1000)
	writeSized(t, filepath.Join(dir, "state.db-wal"), 200)
	writeSized(t, filepath.Join(dir, "backups", "state.db.bak"), 30)
	writeSized(t, filepath.Join(dir, "session-state.json"), 4)
	writeSized(t, filepath.Join(dir, "chat-archive", "a.jsonl"), 6)
	writeSized(t, filepath.Join(dir, "images", "ab", "cd.png"), 500)
	writeSized(t, filepath.Join(dir, "image-thumbs", "x.webp"), 50)
	writeSized(t, filepath.Join(dir, "spawned-work", "t.json"), 70)
	writeSized(t, filepath.Join(dir, "fleet-logs", "j", "out.log"), 7)
	writeSized(t, filepath.Join(dir, "process-outbox.db-shm"), 3)
	writeSized(t, filepath.Join(dir, "machine-id.json"), 9)
	writeSized(t, filepath.Join(dir, "tool-contexts", "chat-tokens.json"), 1)
	if err := os.Symlink(filepath.Join(dir, "images"), filepath.Join(dir, "linked-images")); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	got, err := measureStorage(dir, now)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int64{"chats": 1240, "images": 550, "jobs": 80, "other": 10}
	if !reflect.DeepEqual(got.Parts, want) {
		t.Fatalf("parts = %#v, want %#v", got.Parts, want)
	}
	if got.Total != 1880 {
		t.Fatalf("total = %d, want 1880 (a symlink is not followed)", got.Total)
	}
	reply := storageUsageReply(got, true)
	if reply["totalBytes"] != int64(1880) || reply["computedAt"] != "2027-01-15T08:00:00Z" {
		t.Fatalf("reply = %#v", reply)
	}
	if _, err := measureStorage(filepath.Join(dir, "missing"), now); err == nil {
		t.Fatal("a missing state directory measured as empty")
	}
}

func TestStorageUsageWalksOnceAndKeepsTheResult(t *testing.T) {
	clock := time.Unix(1_800_000_000, 0)
	var walks atomic.Int32
	release := make(chan struct{})
	meter := &storageUsageMeter{dir: "/state", ttl: time.Minute, now: func() time.Time { return clock }}
	meter.measure = func(string, time.Time) (storageUsageSnapshot, error) {
		walks.Add(1)
		<-release
		return storageUsageSnapshot{ComputedAt: clock, Total: 42, Parts: map[string]int64{"chats": 42}}, nil
	}
	// The first walk outlasts the wait: nothing to show yet, and a second read
	// does not start a second walk.
	if _, ok, err := meter.read(10 * time.Millisecond); ok || err != nil {
		t.Fatalf("read during the first walk: ok=%v err=%v", ok, err)
	}
	if _, ok, _ := meter.read(0); ok {
		t.Fatal("read during the first walk returned a result")
	}
	close(release)
	snapshot, ok, err := meter.read(time.Second)
	if err != nil || !ok || snapshot.Total != 42 {
		t.Fatalf("after the walk: %#v ok=%v err=%v", snapshot, ok, err)
	}
	if got := walks.Load(); got != 1 {
		t.Fatalf("%d walks, want 1", got)
	}
	// Fresh: answered from memory.
	if _, ok, _ := meter.read(time.Second); !ok || walks.Load() != 1 {
		t.Fatalf("a fresh result walked again (%d walks)", walks.Load())
	}
	// Stale: walked again.
	clock = clock.Add(2 * time.Minute)
	if _, ok, _ := meter.read(time.Second); !ok || walks.Load() != 2 {
		t.Fatalf("a stale result was not refreshed (%d walks)", walks.Load())
	}
}

func TestStorageUsageReportsAWalkThatFailed(t *testing.T) {
	meter := &storageUsageMeter{dir: "/state", ttl: time.Minute, now: time.Now}
	meter.measure = func(string, time.Time) (storageUsageSnapshot, error) { return storageUsageSnapshot{}, errors.New("gone") }
	if _, ok, err := meter.read(time.Second); ok || err == nil {
		t.Fatalf("a failed walk: ok=%v err=%v", ok, err)
	}
	if _, _, err := (&storageUsageMeter{}).read(time.Second); err == nil {
		t.Fatal("a daemon with no state directory measured something")
	}
}

// ---- diagnostics:export ----------------------------------------------------

func TestDiagnosticsExportRedactsSecretsAndKeepsTheEndOfEachLog(t *testing.T) {
	logs := t.TempDir()
	secret := "sk-ant-api03-" + strings.Repeat("A", 40)
	long := strings.Repeat("old line that falls off the tail\n", 20_000) + "Authorization: Bearer " + secret + "\nlast line\n"
	writeLog := func(name, text string, at time.Time) {
		path := filepath.Join(logs, name)
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	base := time.Unix(1_800_000_000, 0)
	writeLog("daemon.err.log", long, base)
	writeLog("daemon.out.log", "token="+secret+"\n", base.Add(-time.Hour))
	writeLog("notes.txt", "not a log", base)

	stateDir := t.TempDir()
	writeSized(t, filepath.Join(stateDir, "state.db"), 10)
	state := &daemonState{config: newAppConfigStore("", engineConfig{}, nil), stateDir: stateDir}
	report := diagnosticsReport{
		state: state, storage: newStorageUsageMeter(stateDir), logRoot: logs,
		startedAt: base.Add(-time.Minute), now: func() time.Time { return base },
	}.build()

	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), secret) {
		t.Fatal("the export carries a secret")
	}
	if report["schema"] != diagnosticsSchema {
		t.Fatalf("schema = %v", report["schema"])
	}
	if daemon, _ := report["daemon"].(map[string]any); daemon["uptimeSeconds"] != int64(60) || daemon["version"] != daemonVersion {
		t.Fatalf("daemon = %#v", daemon)
	}
	if engine, _ := report["engine"].(map[string]any); engine["hibernateTtlMs"] != int64(20*time.Minute/time.Millisecond) {
		t.Fatalf("engine = %#v", engine)
	}
	if storage, _ := report["storage"].(map[string]any); storage["totalBytes"] != int64(10) {
		t.Fatalf("storage = %#v", storage)
	}
	files, _ := report["logs"].([]any)
	if len(files) != 2 {
		t.Fatalf("logs = %#v, want the two .log files", files)
	}
	first, _ := files[0].(map[string]any)
	if first["name"] != "daemon.err.log" || first["bytes"] != int64(len(long)) {
		t.Fatalf("newest log first: %#v", first["name"])
	}
	tail, _ := first["tail"].(string)
	if len(tail) > diagnosticsLogTailBytes || !strings.HasSuffix(tail, "last line\n") || !strings.HasPrefix(tail, "old line") {
		t.Fatalf("tail is %d bytes, starts %q", len(tail), tail[:min(len(tail), 20)])
	}
	if !strings.Contains(tail, "Bearer [redacted]") {
		t.Fatal("the bearer token was not redacted in place")
	}
	if _, note := report["logsNote"]; note {
		t.Fatalf("a readable log folder carries a note: %v", report["logsNote"])
	}
}

func TestDiagnosticsExportSaysWhenItHasNoLogs(t *testing.T) {
	report := diagnosticsReport{now: time.Now, startedAt: time.Now()}.build()
	if files, _ := report["logs"].([]any); len(files) != 0 {
		t.Fatalf("logs = %#v", files)
	}
	if report["logsNote"] == nil {
		t.Fatal("no note for a daemon that does not know its log folder")
	}
}

func TestRedactDiagnosticsRedactsByNameAndByShape(t *testing.T) {
	got := redactDiagnostics(map[string]any{
		"apiKey": "plain",
		"rows":   []map[string]any{{"command": "tool --token=abcdefghijklmnopqrstuvwxyz0123456789"}},
		"n":      3,
	}).(map[string]any)
	if got["apiKey"] != "[redacted]" || got["n"] != 3 {
		t.Fatalf("got %#v", got)
	}
	rows, _ := got["rows"].([]any)
	command, _ := rows[0].(map[string]any)["command"].(string)
	if strings.Contains(command, "abcdefghijklmnop") {
		t.Fatalf("command kept its token: %q", command)
	}
}

func TestMaintenanceChannelsAnswerOverTheWire(t *testing.T) {
	stateDir := t.TempDir()
	writeSized(t, filepath.Join(stateDir, "images", "a.png"), 25)
	hub := wire.NewHub()
	state := &daemonState{config: newAppConfigStore("", engineConfig{}, nil), stateDir: stateDir}
	if got := registerMaintenanceHandlers(hub, state, nil); got != 2 {
		t.Fatalf("registered %d channels", got)
	}
	raw, err := hub.Invoke("storage:usage", nil)
	if err != nil {
		t.Fatal(err)
	}
	usage, _ := raw.(map[string]any)
	parts, _ := usage["parts"].(map[string]any)
	if usage["totalBytes"] != int64(25) || parts["images"] != int64(25) {
		t.Fatalf("storage:usage = %#v", usage)
	}
	raw, err = hub.Invoke("diagnostics:export", nil)
	if err != nil {
		t.Fatal(err)
	}
	if report, _ := raw.(map[string]any); report["schema"] != diagnosticsSchema {
		t.Fatalf("diagnostics:export = %#v", raw)
	}
}
