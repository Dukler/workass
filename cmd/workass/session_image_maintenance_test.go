package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"workass/internal/chat"
	"workass/internal/store"
)

// fakeImageRefs is an in-memory reference source.
type fakeImageRefs struct {
	mu    sync.Mutex
	list  []imageRefChat
	rows  map[string][][]byte
	owned map[string][]string
}

func (f *fakeImageRefs) chats(context.Context) ([]imageRefChat, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]imageRefChat(nil), f.list...), nil
}

func (f *fakeImageRefs) eachRow(_ context.Context, chatID string, _ map[string]bool, _ bool, _ func() error, _ func(), visit func(string, []byte) error) error {
	f.mu.Lock()
	rows := append([][]byte(nil), f.rows[chatID]...)
	f.mu.Unlock()
	for _, row := range rows {
		if err := visit(chat.ImageReferenceMessage, row); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeImageRefs) coveredFiles(string) []string      { return nil }
func (f *fakeImageRefs) ownedFiles(chatID string) []string { return f.owned[chatID] }
func (f *fakeImageRefs) skipped() []string                 { return nil }

// toolRow is a ledger row whose tool viewed path and returned the images.
func toolRow(path string, digests ...string) []byte {
	type attachment struct{ Digest, Ref string }
	var attachments []attachment
	for _, digest := range digests {
		attachments = append(attachments, attachment{Digest: digest, Ref: providerSessionImageRefPrefix + sessionImageDirname + "/" + digest})
	}
	input, _ := json.Marshal(map[string]string{"path": path})
	row, _ := json.Marshal(map[string]any{"Timeline": []any{map[string]any{"Tool": map[string]any{
		"Title": "View image " + path, "Input": string(input), "Attachments": attachments,
	}}}})
	return row
}

func userRow(digests ...string) []byte {
	type attachment struct{ Digest, Ref, Source string }
	var attachments []attachment
	for _, digest := range digests {
		attachments = append(attachments, attachment{Digest: digest, Ref: providerSessionImageRefPrefix + sessionImageDirname + "/" + digest})
	}
	row, _ := json.Marshal(map[string]any{"Role": "user", "Attachments": attachments})
	return row
}

type maintenanceFixture struct {
	stateDir string
	refs     *fakeImageRefs
	clock    time.Time
	m        *imageStoreMaintenance
	images   map[string][]byte
	sources  map[string]string
}

// newMaintenanceFixture builds a legacy store: count images, the even ones
// viewed from a file that still holds them (clone candidates), the rest with
// no source.
func newMaintenanceFixture(t *testing.T, count int) *maintenanceFixture {
	t.Helper()
	withLegacyShadow(t, true)
	fixture := &maintenanceFixture{
		stateDir: t.TempDir(),
		refs:     &fakeImageRefs{rows: map[string][][]byte{}, owned: map[string][]string{}},
		clock:    time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC),
		images:   map[string][]byte{},
		sources:  map[string]string{},
	}
	home := t.TempDir()
	withSourceGuard(t, sessionImageSourceGuard{home: home, caseInsensitive: runtime.GOOS == "darwin"})
	viewed := filepath.Join(home, "Workspace", "labels")
	if err := os.MkdirAll(viewed, 0o755); err != nil {
		t.Fatal(err)
	}
	var rows [][]byte
	for index := 0; index < count; index++ {
		raw := testImageBytes(t, 100+index)
		name := writeLegacyTestImage(t, fixture.stateDir, raw)
		fixture.images[name] = raw
		legacy := sessionImageLegacyPath(fixture.stateDir, name)
		past := fixture.clock.Add(-30 * 24 * time.Hour)
		if err := os.Chtimes(legacy, past, past); err != nil {
			t.Fatal(err)
		}
		if index%2 == 0 {
			path := filepath.Join(viewed, fmt.Sprintf("%03d.png", index))
			if err := os.WriteFile(path, raw, 0o644); err != nil {
				t.Fatal(err)
			}
			fixture.sources[name] = path
			rows = append(rows, toolRow(path, name))
		} else {
			rows = append(rows, userRow(name))
		}
	}
	fixture.refs.list = []imageRefChat{{chatID: "chat-live", tabID: "tab-live", updatedAt: fixture.clock}}
	fixture.refs.rows["chat-live"] = rows
	fixture.m = fixture.newMaintenance(t)
	return fixture
}

func (f *maintenanceFixture) newMaintenance(t *testing.T) *imageStoreMaintenance {
	m := newImageStoreMaintenance(f.stateDir, "test-build", f.refs, t.Logf)
	m.rate = 0
	m.now = func() time.Time { return f.clock }
	m.sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	return m
}

// assertImagesIntact checks every image reads back byte-identical.
func (f *maintenanceFixture) assertImagesIntact(t *testing.T) {
	t.Helper()
	for name, raw := range f.images {
		ref := sessionImageDirname + "/" + name
		got, err := readChatImageBytes(ref, f.stateDir)
		if err != nil || !bytes.Equal(got, raw) {
			t.Fatalf("image %s: %v", name, err)
		}
		text, err := readExternalSessionImage(ref, f.stateDir)
		if err != nil || text != base64.StdEncoding.EncodeToString(raw) {
			t.Fatalf("image %s base64: %v", name, err)
		}
	}
}

func (f *maintenanceFixture) count(t *testing.T) (legacy, raw int) {
	t.Helper()
	for name := range f.images {
		if _, err := os.Stat(sessionImageLegacyPath(f.stateDir, name)); err == nil {
			legacy++
		}
		if _, err := os.Stat(sessionImageRawPath(f.stateDir, name)); err == nil {
			raw++
		}
	}
	return legacy, raw
}

func TestImageStoreMigrationKeepsLegacyUntilTheWindowPasses(t *testing.T) {
	f := newMaintenanceFixture(t, 12)
	report, err := f.m.runOnce(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	f.assertImagesIntact(t)
	if legacy, raw := f.count(t); legacy != 12 || raw != 12 {
		t.Fatalf("window open: legacy %d raw %d, want every legacy file kept beside its raw twin", legacy, raw)
	}
	if runtime.GOOS == "darwin" {
		if report.Cloned != 6 || report.Written != 6 {
			t.Fatalf("cloned %d written %d, want the 6 viewed files cloned and 6 written", report.Cloned, report.Written)
		}
	} else if report.Written != 12 {
		t.Fatalf("written %d, want all 12 without clones", report.Written)
	}
	if report.VerifyFailures != 0 || report.WriteErrors != 0 || report.ReclaimAllowed || report.Retired != 0 {
		t.Fatalf("unexpected report %+v", report)
	}
	if !sessionImageLegacyShadow.Load() {
		t.Fatal("new images must keep a legacy file while the window is open")
	}
	marker, _ := f.m.loadMarker()
	if !marker.WindowStartedAt.Equal(f.clock) {
		t.Fatalf("window should start at the first complete pass, got %v", marker.WindowStartedAt)
	}

	// Six days later: still open. A second pass trusts its receipts.
	f.clock = f.clock.Add(6 * 24 * time.Hour)
	report, err = f.m.runOnce(t.Context())
	if err != nil || report.Retired != 0 || report.AlreadyRaw != 12 || report.ReadBytes != 0 {
		t.Fatalf("day 6: %+v %v (want nothing retired and nothing re-read)", report, err)
	}

	// Seven days: every legacy file goes, after its raw twin verifies again.
	f.clock = f.clock.Add(24 * time.Hour)
	report, err = f.m.runOnce(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !report.ReclaimAllowed || report.Retired != 12 || report.VerifyFailures != 0 {
		t.Fatalf("day 7: %+v", report)
	}
	if legacy, raw := f.count(t); legacy != 0 || raw != 12 {
		t.Fatalf("after reclaim: legacy %d raw %d", legacy, raw)
	}
	f.assertImagesIntact(t)
	if sessionImageLegacyShadow.Load() {
		t.Fatal("after the window new images are raw only")
	}
	// The sources can go: the clones are the store's own.
	for _, path := range f.sources {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	f.assertImagesIntact(t)
}

func TestImageStoreOlderBuildWriteRestartsTheWindow(t *testing.T) {
	f := newMaintenanceFixture(t, 4)
	if _, err := f.m.runOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Two days in, the previous release runs and writes an image the only way
	// it can: legacy text alone.
	f.clock = f.clock.Add(2 * 24 * time.Hour)
	raw := testImageBytes(t, 999)
	name := writeLegacyTestImage(t, f.stateDir, raw)
	f.images[name] = raw
	if err := os.Chtimes(sessionImageLegacyPath(f.stateDir, name), f.clock, f.clock); err != nil {
		t.Fatal(err)
	}
	f.clock = f.clock.Add(time.Hour)
	report, err := f.m.runOnce(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if report.OlderBuildWrites != 1 || report.ReclaimAllowed {
		t.Fatalf("an older build's write must be seen: %+v", report)
	}
	marker, _ := f.m.loadMarker()
	if !marker.WindowStartedAt.Equal(f.clock) || marker.WindowResets != 1 {
		t.Fatalf("window should restart now: %+v", marker)
	}
	// Seven days after the original start is not enough any more.
	f.clock = f.clock.Add(5 * 24 * time.Hour)
	if report, err := f.m.runOnce(t.Context()); err != nil || report.Retired != 0 {
		t.Fatalf("reclaimed inside the restarted window: %+v %v", report, err)
	}
	f.clock = f.clock.Add(2 * 24 * time.Hour)
	if report, err := f.m.runOnce(t.Context()); err != nil || report.Retired != 5 {
		t.Fatalf("restarted window elapsed: %+v %v", report, err)
	}
	f.assertImagesIntact(t)
}

func TestImageStoreLeavesUnverifiableAndNonCanonicalLegacyAlone(t *testing.T) {
	f := newMaintenanceFixture(t, 2)
	f.m.window = 0
	// A legacy file whose content is not its digest, and a data URL.
	corrupt := strings.Repeat("ab", 32)
	if err := os.WriteFile(sessionImageLegacyPath(f.stateDir, corrupt), []byte("not this"), 0o600); err != nil {
		t.Fatal(err)
	}
	dataURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(testImageBytes(t, 50))
	dataName := sessionImageTextDigest([]byte(dataURL))
	if err := os.WriteFile(sessionImageLegacyPath(f.stateDir, dataName), []byte(dataURL), 0o600); err != nil {
		t.Fatal(err)
	}
	for pass := 0; pass < 2; pass++ {
		report, err := f.m.runOnce(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if report.LegacyUnverified != 1 || report.KeptLegacy != 1 || report.VerifyFailures != 0 {
			t.Fatalf("pass %d: %+v", pass, report)
		}
	}
	for _, name := range []string{corrupt, dataName} {
		if _, err := os.Stat(sessionImageLegacyPath(f.stateDir, name)); err != nil {
			t.Fatalf("%s must keep its legacy file: %v", name, err)
		}
		if _, err := os.Stat(sessionImageRawPath(f.stateDir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s must not get a raw twin", name)
		}
	}
	f.assertImagesIntact(t)
}

func TestImageStoreRepairsADamagedRawTwin(t *testing.T) {
	f := newMaintenanceFixture(t, 3)
	if _, err := f.m.runOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	var victim string
	for name := range f.images {
		victim = name
		break
	}
	rawPath := sessionImageRawPath(f.stateDir, victim)
	_ = os.Remove(rawPath) // break any clone sharing before damaging it
	if err := os.WriteFile(rawPath, []byte("damaged"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.m.window = 0
	report, err := f.m.runOnce(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if report.RawRepaired != 1 || report.Retired != 3 {
		t.Fatalf("%+v", report)
	}
	f.assertImagesIntact(t)
}

func TestImageStoreResumesAfterAStopAtEveryStep(t *testing.T) {
	steps := []string{"raw", "retire"}
	for _, stepName := range steps {
		for _, at := range []int{1, 3, 6} {
			t.Run(fmt.Sprintf("%s-%d", stepName, at), func(t *testing.T) {
				// With no window, one pass migrates and then retires.
				f := newMaintenanceFixture(t, 8)
				f.m.window = 0
				seen := 0
				crash := errors.New("crash")
				f.m.hook = func(step, _ string) error {
					if step == stepName {
						if seen++; seen == at {
							return crash
						}
					}
					return nil
				}
				if _, err := f.m.runOnce(t.Context()); !errors.Is(err, crash) {
					t.Fatalf("want the injected stop, got %v", err)
				}
				f.assertImagesIntact(t)
				// A fresh process resumes.
				resumed := f.newMaintenance(t)
				resumed.window = 0
				report, err := resumed.runOnce(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if report.VerifyFailures != 0 || report.WriteErrors != 0 {
					t.Fatalf("%+v", report)
				}
				if legacy, raw := f.count(t); legacy != 0 || raw != 8 {
					t.Fatalf("after resume: legacy %d raw %d", legacy, raw)
				}
				f.assertImagesIntact(t)
			})
		}
	}
}

const (
	imageKillStateEnv = "WORKASS_TEST_IMAGE_STORE_KILL_STATE"
	imageKillAtEnv    = "WORKASS_TEST_IMAGE_STORE_KILL_AT"
	imageKillStepEnv  = "WORKASS_TEST_IMAGE_STORE_KILL_STEP"
)

// TestImageStoreKillHelper is the child of TestImageStoreSurvivesAKill: it
// runs the pass over a prepared store and SIGKILLs itself at the Nth step,
// with receipts still buffered and no deferred code run.
func TestImageStoreKillHelper(t *testing.T) {
	stateDir := os.Getenv(imageKillStateEnv)
	if stateDir == "" {
		t.Skip("child process of the kill test")
	}
	killAt, _ := strconv.Atoi(os.Getenv(imageKillAtEnv))
	stepName := os.Getenv(imageKillStepEnv)
	m := newImageStoreMaintenance(stateDir, "kill-child", &fakeImageRefs{}, nil)
	m.rate, m.window = 0, 0
	seen := 0
	m.hook = func(step, _ string) error {
		if step == stepName {
			if seen++; seen == killAt {
				self, _ := os.FindProcess(os.Getpid())
				_ = self.Kill()
				time.Sleep(time.Minute)
			}
		}
		return nil
	}
	// With no window one pass migrates every image, then retires.
	_, err := m.runOnce(context.Background())
	t.Fatalf("child pass finished without being killed: %v", err)
}

func TestImageStoreSurvivesAKill(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a child process")
	}
	for _, stepName := range []string{"raw", "retire"} {
		t.Run(stepName, func(t *testing.T) {
			f := newMaintenanceFixture(t, 10)
			child := exec.Command(os.Args[0], "-test.run=^TestImageStoreKillHelper$", "-test.count=1")
			child.Env = append(os.Environ(), imageKillStateEnv+"="+f.stateDir, imageKillAtEnv+"=5", imageKillStepEnv+"="+stepName)
			output, err := child.CombinedOutput()
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.Success() {
				t.Fatalf("child was not killed: %v\n%s", err, output)
			}
			f.assertImagesIntact(t)
			resumed := f.newMaintenance(t)
			resumed.window = 0
			resumed.now = time.Now // the child kept the real clock
			report, err := resumed.runOnce(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if report.VerifyFailures != 0 || report.WriteErrors != 0 {
				t.Fatalf("%+v", report)
			}
			if legacy, raw := f.count(t); legacy != 0 || raw != 10 {
				t.Fatalf("after the kill and a resume: legacy %d raw %d", legacy, raw)
			}
			f.assertImagesIntact(t)
		})
	}
}

// collectFixture is a migrated store with chats that share images.
func collectFixture(t *testing.T) (*maintenanceFixture, map[string]string) {
	t.Helper()
	f := newMaintenanceFixture(t, 0)
	names := map[string]string{}
	for index, label := range []string{"shared", "onlyDeleted", "onlyLive", "recentDeleted", "inFork", "touched"} {
		raw := testImageBytes(t, 300+index)
		names[label] = writeLegacyTestImage(t, f.stateDir, raw)
		f.images[names[label]] = raw
	}
	old := f.clock.Add(-30 * 24 * time.Hour)
	f.refs.list = []imageRefChat{
		{chatID: "chat-deleted", tabID: "tab-deleted", deleted: true, updatedAt: old},
		{chatID: "chat-live", tabID: "tab-live", updatedAt: f.clock},
		{chatID: "chat-recent", tabID: "tab-recent", deleted: true, updatedAt: f.clock.Add(-time.Hour)},
	}
	f.refs.rows = map[string][][]byte{
		"chat-deleted": {userRow(names["shared"], names["onlyDeleted"], names["inFork"], names["touched"]), toolRow("/nowhere/x.png", names["recentDeleted"])},
		"chat-live":    {userRow(names["shared"], names["onlyLive"])},
		"chat-recent":  {userRow(names["recentDeleted"])},
	}
	// Another store (a fork record) still names one of the deleted chat's
	// images, and the deleted chat's own legacy archive names them all.
	if err := os.MkdirAll(filepath.Join(f.stateDir, "forks"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.stateDir, "forks", "chat-x.json"), []byte(`{"ref":"images/`+names["inFork"]+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(f.stateDir, "chat-archive"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.stateDir, "chat-archive", "tab-deleted.jsonl"), userRow(names["shared"], names["onlyDeleted"]), 0o600); err != nil {
		t.Fatal(err)
	}
	return f, names
}

func TestImageStoreCollectsOnlyImagesNoOtherReferenceUses(t *testing.T) {
	f, names := collectFixture(t)
	f.m.window = 0
	touchSessionImage(names["touched"])
	t.Cleanup(func() { sessionImageTouches.Delete(names["touched"]) })
	report, err := f.m.runOnce(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if report.CollectSkipped != "" || report.DeletedChatsEligible != 1 {
		t.Fatalf("%+v", report)
	}
	gone := func(label string) bool {
		_, legacyErr := os.Stat(sessionImageLegacyPath(f.stateDir, names[label]))
		_, rawErr := os.Stat(sessionImageRawPath(f.stateDir, names[label]))
		return errors.Is(legacyErr, os.ErrNotExist) && errors.Is(rawErr, os.ErrNotExist)
	}
	if !gone("onlyDeleted") || report.Collected != 1 {
		t.Fatalf("the image only the long-deleted chat used must go (collected %d)", report.Collected)
	}
	for _, label := range []string{"shared", "onlyLive", "recentDeleted", "inFork", "touched"} {
		if gone(label) {
			t.Fatalf("%s must stay", label)
		}
	}
	delete(f.images, names["onlyDeleted"])
	f.assertImagesIntact(t)
	// Idempotent: nothing else goes on the next pass.
	if report, err := f.m.runOnce(t.Context()); err != nil || report.Collected != 0 {
		t.Fatalf("second pass: %+v %v", report, err)
	}
}

func TestImageStoreCollectsInARawOnlyStore(t *testing.T) {
	// After reclaim no legacy file is left, so the pass opens its receipts
	// for the first time in the collection phase.
	f := newMaintenanceFixture(t, 0)
	gone := writeRawTestImage(t, f.stateDir, testImageBytes(t, 500))
	kept := writeRawTestImage(t, f.stateDir, testImageBytes(t, 501))
	f.refs.list = []imageRefChat{
		{chatID: "chat-deleted", deleted: true, updatedAt: f.clock.Add(-30 * 24 * time.Hour)},
		{chatID: "chat-live", updatedAt: f.clock},
	}
	f.refs.rows = map[string][][]byte{"chat-deleted": {userRow(gone, kept)}, "chat-live": {userRow(kept)}}
	report, err := f.m.runOnce(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if report.Collected != 1 || report.CollectSkipped != "" {
		t.Fatalf("%+v", report)
	}
	if _, err := os.Stat(sessionImageRawPath(f.stateDir, gone)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("collected image still there")
	}
	if _, err := readChatImageBytes(sessionImageDirname+"/"+kept, f.stateDir); err != nil {
		t.Fatal(err)
	}
}

func TestImageStoreThrottleYieldsToForegroundBounded(t *testing.T) {
	var slept time.Duration
	busyCalls := 0
	throttle := &imageStoreThrottle{
		sleep: func(_ context.Context, d time.Duration) error { slept += d; return nil },
		busy:  func() bool { busyCalls++; return busyCalls <= 3 },
	}
	if err := throttle.add(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	if slept != 150*time.Millisecond {
		t.Fatalf("waited %s for a burst of three busy checks", slept)
	}
	slept = 0
	throttle.busy = func() bool { return true }
	if err := throttle.add(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	if slept != imageStoreYieldLimit {
		t.Fatalf("a client that never pauses must not stop the pass: waited %s", slept)
	}
}

func TestImageStoreCollectionStopsOnDoubt(t *testing.T) {
	f, names := collectFixture(t)
	// An unreadable file in the state directory might name anything.
	unreadable := filepath.Join(f.stateDir, "spawned-work", "receipt.json")
	if err := os.MkdirAll(filepath.Dir(unreadable), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unreadable, []byte("{}"), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o600) })
	report, err := f.m.runOnce(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if report.CollectSkipped == "" || report.Collected != 0 {
		t.Fatalf("collection must stop with nothing removed: %+v", report)
	}
	if _, err := os.Stat(sessionImageLegacyPath(f.stateDir, names["onlyDeleted"])); err != nil {
		t.Fatal("removed on doubt")
	}
}

func TestImageStoreCollectsThroughSQLiteReferences(t *testing.T) {
	withLegacyShadow(t, true)
	stateDir := t.TempDir()
	db, err := store.Open(store.Options{Path: filepath.Join(stateDir, store.FileName), BinaryVersion: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	shared := writeLegacyTestImage(t, stateDir, testImageBytes(t, 400))
	mine := writeLegacyTestImage(t, stateDir, testImageBytes(t, 401))
	theirs := writeLegacyTestImage(t, stateDir, testImageBytes(t, 402))
	old := time.Now().Add(-30 * 24 * time.Hour).UTC().Format(time.RFC3339Nano)
	insert := func(chatID, tabID string, deleted bool, updated string, rows ...[]byte) {
		state, _ := json.Marshal(map[string]any{"ChatID": chatID, "Deleted": deleted, "Presentation": map[string]any{"TabID": tabID}})
		if err := db.Write(t.Context(), func(tx *store.Tx) error {
			if _, err := tx.Exec(`INSERT INTO chats(chat_id, envelope_version, revision, state_json, ledger_len, updated_at, ledger_gen) VALUES (?, 1, 1, ?, ?, ?, 1)`,
				chatID, string(state), len(rows), updated); err != nil {
				return err
			}
			for index, row := range rows {
				if _, err := tx.Exec(`INSERT INTO messages(chat_id, seq, message_id, role, event_json) VALUES (?, ?, ?, 'user', ?)`,
					chatID, index+1, fmt.Sprintf("m%d", index), string(row)); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	insert("chat-gone", "tab-gone", true, old, userRow(shared, mine))
	insert("chat-here", "tab-here", false, old, userRow(shared), toolRow("/x.png", theirs))
	m := newImageStoreMaintenance(stateDir, "test", sqlImageReferences{actors: chat.NewSQLActors(db)}, t.Logf)
	m.rate, m.window = 0, 0
	m.sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	report, err := m.runOnce(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if report.Collected != 1 || report.CollectSkipped != "" {
		t.Fatalf("%+v", report)
	}
	if _, err := os.Stat(sessionImageRawPath(stateDir, mine)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the deleted chat's own image must go")
	}
	for _, name := range []string{shared, theirs} {
		if _, err := readChatImageBytes(sessionImageDirname+"/"+name, stateDir); err != nil {
			t.Fatalf("%s must stay: %v", name, err)
		}
	}
}

func TestImageStoreScanTextReadOnlyFindsBackupReferences(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(store.Options{Path: filepath.Join(dir, store.FileName), BinaryVersion: "test"})
	if err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("c", 64)
	long := strings.Repeat("x", 9000) + "images/" + digest + strings.Repeat("y", 9000) // spans overflow pages
	if err := db.Write(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO chats(chat_id, envelope_version, revision, state_json, ledger_len, updated_at) VALUES ('c', 1, 1, ?, 0, 'now')`, long)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(dir, "backups", "state-v5-copy.db")
	if err := os.MkdirAll(filepath.Dir(backup), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := db.Backup(backup); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if !isSQLiteFile(backup) {
		t.Fatal("backup not recognized as SQLite")
	}
	found := false
	if err := store.ScanTextReadOnly(t.Context(), backup, func(_ string, value []byte) error {
		scanDigests(value, func(d string) { found = found || d == digest })
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("a reference inside a backup must be found")
	}
}

func TestFileBackendReferencesReadEnvelopeAndJournal(t *testing.T) {
	// The JSON actor store (Windows production): one envelope file plus its
	// .events journal per chat, both read, both named as covered.
	stateDir := t.TempDir()
	digest := strings.Repeat("e", 64)
	envelope, _ := json.Marshal(map[string]any{"v": 1, "state": map[string]any{"ChatID": "c1", "Ledger": []json.RawMessage{toolRow("/tmp/a.png", digest)}}})
	base := filepath.Join(stateDir, filepath.FromSlash(providerChatFileName("c1")))
	if err := os.MkdirAll(filepath.Dir(base), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(base, envelope, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(base+".events", []byte(`{"ref":"images/`+strings.Repeat("f", 64)+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	refs := fileImageReferences{stateDir: stateDir}
	var kinds []string
	found := map[string]bool{}
	hints := imageCloneHints{}
	if err := refs.eachRow(t.Context(), "c1", nil, true, nil, nil, func(kind string, data []byte) error {
		kinds = append(kinds, kind)
		scanDigests(data, func(d string) { found[d] = true })
		hints.addRow(kind, data)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(kinds, ",") != imageRefEnvelope+","+chat.ImageReferenceJournal {
		t.Fatalf("kinds %v", kinds)
	}
	if !found[digest] || !found[strings.Repeat("f", 64)] {
		t.Fatalf("both files must be scanned: %v", found)
	}
	if got := hints[digest]; len(got) != 1 || got[0] != "/tmp/a.png" {
		t.Fatalf("envelope hints %v", got)
	}
	if covered := refs.coveredFiles("c1"); len(covered) != 1 || covered[0] != providerChatFileName("c1") {
		t.Fatalf("covered %v", covered)
	}
}

func TestDigestScannerFindsExactRunsAcrossWrites(t *testing.T) {
	digest := strings.Repeat("0123456789abcdef", 4)
	var got []string
	scanner := &digestScanner{found: func(d string) { got = append(got, d) }}
	input := `{"Ref":"workass-session-image:images/` + digest + `","x":"` + strings.Repeat("a", 65) + `","y":"` + strings.Repeat("b", 63) + `"}`
	for index := 0; index < len(input); index += 7 {
		end := min(index+7, len(input))
		_, _ = scanner.Write([]byte(input[index:end]))
	}
	scanner.flush()
	if len(got) != 1 || got[0] != digest {
		t.Fatalf("got %v", got)
	}
}

func TestToolInputHintsBecomeCloneCandidates(t *testing.T) {
	hints := imageCloneHints{}
	digest := strings.Repeat("d", 64)
	hints.addRow(chat.ImageReferenceMessage, toolRow("/Users/me/Workspace/a.png", digest))
	if got := hints[digest]; len(got) != 1 || got[0] != "/Users/me/Workspace/a.png" {
		t.Fatalf("hints = %v", got)
	}
	envelope, _ := json.Marshal(map[string]any{"v": 1, "state": map[string]any{"Ledger": []json.RawMessage{toolRow("/tmp/b.png", digest)}}})
	hints.addRow(imageRefEnvelope, envelope)
	if got := hints[digest]; len(got) != 2 {
		t.Fatalf("file-backend envelope hints = %v", got)
	}
}
