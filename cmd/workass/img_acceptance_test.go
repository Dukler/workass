package main

// Independent acceptance tests for the raw chat image store (7865c697).
// They exercise the store through its production entry points and judge the
// result with readers written here, independently of the code under test:
// oldBinaryRead is what 0.1.229 does (legacy text only, sha256-verified).

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"io/fs"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"workass/internal/chat"
	"workass/internal/httpserve"
	"workass/internal/store"
)

// oldBinaryRead is 0.1.229's read: images/<digest> only, a bounded regular
// file whose sha256 is the digest, decoded from base64 or a base64 data URL.
func oldBinaryRead(stateDir, name string) ([]byte, error) {
	path := filepath.Join(stateDir, "images", name)
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("not regular")
	}
	text, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(text)
	if hex.EncodeToString(sum[:]) != name {
		return nil, errors.New("verification failed")
	}
	body := text
	if bytes.HasPrefix(body, []byte("data:")) {
		comma := bytes.IndexByte(body, ',')
		if comma < 0 {
			return nil, errors.New("bad data url")
		}
		body = body[comma+1:]
	}
	return base64.StdEncoding.DecodeString(string(body))
}

func acceptDigest(raw []byte) string {
	sum := sha256.Sum256([]byte(base64.StdEncoding.EncodeToString(raw)))
	return hex.EncodeToString(sum[:])
}

func acceptGIF(t testing.TB, w, h int) []byte {
	t.Helper()
	img := image.NewPaletted(image.Rect(0, 0, w, h), []color.Color{color.Black, color.White})
	img.SetColorIndex(1, 1, 1)
	var out bytes.Buffer
	if err := gif.Encode(&out, img, nil); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func acceptJPEG(t testing.TB, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 7, 255})
		}
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, nil); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// persistThroughStaging stores an image the way a chat commit does: staged
// from an image item (with an optional clone-source hint), then persisted.
func persistThroughStaging(t testing.TB, stateDir string, raw []byte, sources ...string) string {
	t.Helper()
	item := map[string]any{"mimeType": "image/png", "data": base64.StdEncoding.EncodeToString(raw)}
	if len(sources) > 0 {
		hint := make([]any, len(sources))
		for i, s := range sources {
			hint[i] = s
		}
		item[sessionImageSourceField] = hint
	}
	owner := map[string]any{"images": []any{item}}
	plans, err := stageExternalSessionImageData(owner, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := persistSessionImagePlans(plans); err != nil {
		t.Fatal(err)
	}
	if _, leaked := item[sessionImageSourceField]; leaked {
		t.Fatal("clone hint survived staging")
	}
	return acceptDigest(raw)
}

func resetImageCachesFor(names ...string) {
	for _, name := range names {
		sessionImageSizes.mu.Lock()
		delete(sessionImageSizes.byName, name)
		sessionImageSizes.mu.Unlock()
	}
}

// ---------------------------------------------------------------------------
// 1. No image lost or altered: every read path, every stored form.

func TestAcceptEveryReadPathAgreesAcrossStoredForms(t *testing.T) {
	withLegacyShadow(t, true)
	type form int
	const (
		legacyOnly form = iota
		rawOnly
		both
		damagedRawWithLegacy
		zeroedRawWithLegacy
	)
	forms := map[form]string{legacyOnly: "legacy", rawOnly: "raw", both: "both", damagedRawWithLegacy: "truncated-raw+legacy", zeroedRawWithLegacy: "zeroed-raw+legacy"}
	images := map[string]func(t testing.TB, seed int) ([]byte, int, int){
		"png": func(t testing.TB, seed int) ([]byte, int, int) {
			return testPNG(t.(*testing.T), 40+seed, 30+seed), 40 + seed, 30 + seed
		},
		"gif": func(t testing.TB, seed int) ([]byte, int, int) {
			return acceptGIF(t, 17+seed, 9+seed), 17 + seed, 9 + seed
		},
		"jpeg": func(t testing.TB, seed int) ([]byte, int, int) {
			return acceptJPEG(t, 33+seed, 21+seed), 33 + seed, 21 + seed
		},
	}
	seed := 0
	for kind, build := range images {
		for f, label := range forms {
			seed++
			t.Run(kind+"/"+label, func(t *testing.T) {
				stateDir := t.TempDir()
				raw, width, height := build(t, seed)
				text := base64.StdEncoding.EncodeToString(raw)
				name := acceptDigest(raw)
				dir := filepath.Join(stateDir, sessionImageDirname)
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				legacyPath, rawPath := filepath.Join(dir, name), filepath.Join(dir, name+".raw")
				switch f {
				case legacyOnly:
					must(t, os.WriteFile(legacyPath, []byte(text), 0o600))
				case rawOnly:
					must(t, os.WriteFile(rawPath, raw, 0o600))
				case both:
					must(t, os.WriteFile(legacyPath, []byte(text), 0o600))
					must(t, os.WriteFile(rawPath, raw, 0o600))
				case damagedRawWithLegacy:
					must(t, os.WriteFile(legacyPath, []byte(text), 0o600))
					must(t, os.WriteFile(rawPath, raw[:len(raw)/2], 0o600))
				case zeroedRawWithLegacy:
					// What a power loss can leave: the rename made durable,
					// the data not.
					must(t, os.WriteFile(legacyPath, []byte(text), 0o600))
					must(t, os.WriteFile(rawPath, make0(len(raw)), 0o600))
				}
				resetImageCachesFor(name)
				ref := sessionImageDirname + "/" + name

				got, err := readChatImageBytes(ref, stateDir)
				if err != nil || !bytes.Equal(got, raw) {
					t.Fatalf("serve bytes: %v (equal=%v)", err, bytes.Equal(got, raw))
				}
				gotText, err := readExternalSessionImage(ref, stateDir)
				if err != nil || gotText != text {
					t.Fatalf("base64 read (provider replay, socket read, archive rehydrate): %v", err)
				}
				// Archive rehydration of a stored row.
				row := map[string]any{"images": []any{map[string]any{"mimeType": "image/png", sessionImageDataRefField: ref}}}
				if err := rehydrateExternalSessionImages(row, stateDir); err != nil {
					t.Fatalf("rehydrate: %v", err)
				}
				if data := fieldString(mapFromAnyMain(anySlice(row["images"])[0]), "data"); data != text {
					t.Fatalf("rehydrated data differs (len %d vs %d)", len(data), len(text))
				}
				// Attachment planning by reference (resend, queue, fork input) and
				// provider attachment resolution (the bytes a provider is sent).
				sessions := sharedSessionStore(stateDir)
				plan, err := sessions.PlanProviderAttachments([]any{map[string]any{"mimeType": "image/png", sessionImageDataRefField: ref}})
				if err != nil {
					t.Fatalf("plan by ref: %v", err)
				}
				attachment := plan.Attachments[0]
				damaged := f == damagedRawWithLegacy || f == zeroedRawWithLegacy
				if attachment.Digest != name || (!damaged && attachment.Size != int64(len(text))) {
					t.Fatalf("planned identity digest=%s size=%d, want %s/%d", attachment.Digest, attachment.Size, name, len(text))
				}
				if !damaged && (attachment.Width != width || attachment.Height != height) {
					t.Fatalf("planned size %dx%d, want %dx%d", attachment.Width, attachment.Height, width, height)
				}
				resolved, err := sessions.ResolveProviderAttachment(context.Background(), attachment)
				if err != nil {
					t.Fatalf("resolve for provider: %v", err)
				}
				if !strings.Contains(fmt.Sprint(resolved), text) {
					t.Fatal("provider attachment does not carry the image's base64")
				}
				// HTTP serving and thumbnails through the real handler.
				runtime := screenshotHistory(t, stateDir, ref, 1, 1, true)
				handler := newChatImageHTTPHandler(runtime)
				for _, query := range []string{"", "?w=160"} {
					request := httptest.NewRequest(http.MethodGet, "http://daemon.test"+httpserve.ChatImagePathPrefix+"screenshot-tab/"+name+query, nil)
					recorder := httptest.NewRecorder()
					handler.ServeHTTP(recorder, request)
					if recorder.Code != http.StatusOK {
						t.Fatalf("GET %q = %d", query, recorder.Code)
					}
					if query == "" && !bytes.Equal(recorder.Body.Bytes(), raw) {
						t.Fatal("HTTP body differs from the image")
					}
				}
				// Socket image read (archive-load imageRef).
				socket, found, err := runtime.ProjectToolImageByTab("screenshot-tab", ref)
				if err != nil || !found || fieldString(mapFromAnyMain(socket[0]), "data") != text {
					t.Fatalf("socket image read: found=%v err=%v", found, err)
				}
				// Archive load with images inline.
				page, found, err := runtime.ProjectRecentArchiveByTab("screenshot-tab", 1)
				if err != nil || !found {
					t.Fatalf("archive load: %v", err)
				}
				encoded, _ := json.Marshal(page)
				if !bytes.Contains(encoded, []byte(text)) {
					t.Fatal("archive page does not carry the image")
				}
				// read_chat exposes metadata only, never base64.
				read, err := runtime.ReadChat("screenshot-tab", "screenshot-chat", 5, true)
				if err != nil {
					t.Fatalf("read_chat: %v", err)
				}
				encoded, _ = json.Marshal(read)
				if bytes.Contains(encoded, []byte(text[:64])) {
					t.Fatal("read_chat leaked image base64")
				}
				// Nothing on the read paths may alter the store.
				if f == legacyOnly {
					if _, err := os.Stat(rawPath); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("a read created a raw file")
					}
				}
			})
		}
	}
}

func make0(n int) []byte { return make([]byte, n) }

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// A damaged raw file whose legacy twin is intact must not change what the
// store reports about the image: the bytes fall back to the legacy file, so
// must its size (recorded as the attachment's Size) and its dimensions
// (recorded as Width/Height, and cached for the process's life).
func TestAcceptDamagedRawDoesNotChangeReportedMetadata(t *testing.T) {
	stateDir := t.TempDir()
	sessions := sharedSessionStore(stateDir)
	plan := func(name string) (int64, int, int) {
		t.Helper()
		resetImageCachesFor(name)
		p, err := sessions.PlanProviderAttachments([]any{map[string]any{"mimeType": "image/png", sessionImageDataRefField: sessionImageDirname + "/" + name}})
		if err != nil {
			t.Fatal(err)
		}
		return p.Attachments[0].Size, p.Attachments[0].Width, p.Attachments[0].Height
	}
	var problems []string
	zeroed := testPNG(t, 64, 48)
	name := writeLegacyTestImage(t, stateDir, zeroed)
	must(t, os.WriteFile(sessionImageRawPath(stateDir, name), make0(len(zeroed)), 0o600))
	if size, w, h := plan(name); size != int64(base64.StdEncoding.EncodedLen(len(zeroed))) || w != 64 || h != 48 {
		problems = append(problems, fmt.Sprintf("zeroed raw twin (power loss after a batched write): planned size %d %dx%d, want %d 64x48",
			size, w, h, base64.StdEncoding.EncodedLen(len(zeroed))))
	}
	truncated := testPNG(t, 30, 20)
	name = writeLegacyTestImage(t, stateDir, truncated)
	must(t, os.WriteFile(sessionImageRawPath(stateDir, name), truncated[:len(truncated)/2], 0o600))
	if size, w, h := plan(name); size != int64(base64.StdEncoding.EncodedLen(len(truncated))) || w != 30 || h != 20 {
		problems = append(problems, fmt.Sprintf("truncated raw twin: planned size %d %dx%d, want %d 30x20",
			size, w, h, base64.StdEncoding.EncodedLen(len(truncated))))
	}
	if len(problems) > 0 {
		t.Fatalf("metadata is read from an unverified raw file although the image's bytes come from its legacy file:\n  %s", strings.Join(problems, "\n  "))
	}
}

// The identity: a raw file stands for exactly one text, and only canonical
// base64 ever becomes raw.
func TestAcceptCanonicalDecodeNeverChangesIdentity(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	alphabet := "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/=\n\r -_"
	check := func(text string) {
		raw, ok := decodeCanonicalSessionImage(text)
		if !ok {
			return
		}
		if base64.StdEncoding.EncodeToString(raw) != text {
			t.Fatalf("decodeCanonical accepted %q whose re-encoding differs", text)
		}
	}
	for i := 0; i < 20000; i++ {
		raw := make([]byte, rng.Intn(64))
		rng.Read(raw)
		text := base64.StdEncoding.EncodeToString(raw)
		if got, ok := decodeCanonicalSessionImage(text); len(raw) > 0 && (!ok || !bytes.Equal(got, raw)) {
			t.Fatalf("canonical text rejected or altered: %q", text)
		}
		check(text)
		// Mutations: a byte replaced from a hostile alphabet, newline inserted,
		// padding stripped, URL alphabet.
		if len(text) > 0 {
			b := []byte(text)
			b[rng.Intn(len(b))] = alphabet[rng.Intn(len(alphabet))]
			check(string(b))
			at := rng.Intn(len(text))
			check(text[:at] + "\n" + text[at:])
			check(strings.TrimRight(text, "="))
			check(base64.URLEncoding.EncodeToString(raw))
			check("data:image/png;base64," + text)
		}
		n := 4 * (1 + rng.Intn(8))
		b := make([]byte, n)
		for j := range b {
			b[j] = alphabet[rng.Intn(len(alphabet))]
		}
		check(string(b))
	}
}

func TestAcceptRawWriteKeepsIdentityAndLeavesNoPartialFile(t *testing.T) {
	withLegacyShadow(t, false)
	stateDir := t.TempDir()
	for seed := 0; seed < 20; seed++ {
		raw := testImageBytes(t, 700+seed)
		name := persistThroughStaging(t, stateDir, raw)
		data, err := os.ReadFile(sessionImageRawPath(stateDir, name))
		if err != nil || !bytes.Equal(data, raw) {
			t.Fatalf("raw file is not the image bytes: %v", err)
		}
		if acceptDigest(data) != name {
			t.Fatal("sha256(base64(raw file)) is not its name")
		}
		info, _ := os.Stat(sessionImageRawPath(stateDir, name))
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
			t.Fatalf("raw file mode %v", info.Mode().Perm())
		}
	}
	entries, _ := os.ReadDir(filepath.Join(stateDir, sessionImageDirname))
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tmp") {
			t.Fatalf("temporary file left behind: %s", entry.Name())
		}
	}
}

// ---------------------------------------------------------------------------
// Crash simulation: on-disk states a crash can leave, at each step.

func TestAcceptCrashLeftoversNeverLoseOrAlterAnImage(t *testing.T) {
	f := newMaintenanceFixture(t, 8)
	f.m.now = func() time.Time { return f.clock }
	dir := filepath.Join(f.stateDir, sessionImageDirname)
	names := make([]string, 0, len(f.images))
	for name := range f.images {
		names = append(names, name)
	}
	sort.Strings(names)
	// Crash between clone/write and rename: temporaries of every writer kind,
	// old ones and one that might still be live.
	old := f.clock.Add(-2 * time.Hour)
	for _, temp := range []string{".image-raw-1.tmp", ".image-clone-ab.tmp", ".image-9.tmp"} {
		must(t, os.WriteFile(filepath.Join(dir, temp), []byte("partial"), 0o600))
		must(t, os.Chtimes(filepath.Join(dir, temp), old, old))
	}
	must(t, os.WriteFile(filepath.Join(dir, ".image-raw-live.tmp"), []byte("in flight"), 0o600))
	must(t, os.Chtimes(filepath.Join(dir, ".image-raw-live.tmp"), f.clock.Add(-time.Minute), f.clock.Add(-time.Minute)))
	// Crash after a rename whose data never reached the disk: two raw twins
	// zeroed, one truncated.
	must(t, os.WriteFile(sessionImageRawPath(f.stateDir, names[0]), make0(len(f.images[names[0]])), 0o600))
	must(t, os.WriteFile(sessionImageRawPath(f.stateDir, names[1]), f.images[names[1]][:3], 0o600))
	// Crash mid-append of the receipts: a torn last line.
	must(t, os.MkdirAll(filepath.Join(f.stateDir, imageStoreDirname), 0o700))
	must(t, os.WriteFile(filepath.Join(f.stateDir, imageStoreDirname, imageStoreReceiptsFilename), []byte(`{"digest":"`+names[2]+`","outcome":"writ`), 0o600))
	// Crash mid-save of the marker is a rename, but a garbled marker must still
	// only ever keep legacy files.
	must(t, os.WriteFile(filepath.Join(f.stateDir, imageStoreDirname, imageStoreMarkerFilename), []byte(`{"schema":1,"windowStartedAt":"2026-`), 0o600))

	f.assertImagesIntact(t)
	for name, raw := range f.images {
		if got, err := oldBinaryRead(f.stateDir, name); err != nil || !bytes.Equal(got, raw) {
			t.Fatalf("0.1.229 cannot read %s after the crash: %v", name, err)
		}
	}
	report, err := f.m.runOnce(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if report.RawRepaired != 2 || report.Retired != 0 {
		t.Fatalf("%+v", report)
	}
	f.assertImagesIntact(t)
	for name := range f.images {
		if err := verifyRawSessionImageFile(sessionImageRawPath(f.stateDir, name), name, int64(len(f.images[name]))); err != nil {
			t.Fatalf("raw twin of %s not repaired: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, ".image-raw-live.tmp")); err != nil {
		t.Fatal("a temporary file younger than an hour may belong to a live write and must stay")
	}
	for _, temp := range []string{".image-raw-1.tmp", ".image-clone-ab.tmp", ".image-9.tmp"} {
		if _, err := os.Stat(filepath.Join(dir, temp)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stale temporary %s not cleaned", temp)
		}
	}
	marker, err := f.m.loadMarker()
	if err != nil || !marker.WindowStartedAt.Equal(f.clock) {
		t.Fatalf("a garbled marker must restart the window now: %+v %v", marker, err)
	}
}

// Stop the pass after every durable step it takes, in every phase, and prove
// that at that instant every image reads back exactly, and that 0.1.229 reads
// every image whose legacy file reclaim has not yet proven redundant.
func TestAcceptStopAtEveryStepOfEveryPhase(t *testing.T) {
	for stopAt := 1; ; stopAt++ {
		f := newMaintenanceFixture(t, 6)
		f.m.window = 24 * time.Hour
		// Pass 1: migrate (window opens). Pass 2 (a day later): reclaim.
		steps := 0
		stopped := false
		f.m.hook = func(step, digest string) error {
			steps++
			if steps == stopAt {
				stopped = true
				return fmt.Errorf("crash at %s %s", step, digest[:8])
			}
			return nil
		}
		_, _ = f.m.runOnce(t.Context())
		f.assertImagesIntact(t)
		for name := range f.images {
			if _, err := oldBinaryRead(f.stateDir, name); err != nil {
				t.Fatalf("stop %d (migration): 0.1.229 lost %s: %v", stopAt, name, err)
			}
		}
		if stopped {
			// Resume, then let the window pass.
			f.m.hook = nil
			if _, err := f.m.runOnce(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		f.clock = f.clock.Add(25 * time.Hour)
		stoppedReclaim := false
		if !stopped {
			f.m.hook = func(step, digest string) error {
				steps++
				if steps == stopAt {
					stoppedReclaim = true
					return fmt.Errorf("crash at %s %s", step, digest[:8])
				}
				return nil
			}
		}
		_, _ = f.m.runOnce(t.Context())
		f.assertImagesIntact(t)
		f.m.hook = nil
		if _, err := f.m.runOnce(t.Context()); err != nil {
			t.Fatal(err)
		}
		f.assertImagesIntact(t)
		if legacy, raw := f.count(t); legacy != 0 || raw != len(f.images) {
			t.Fatalf("stop %d: converged to legacy %d raw %d", stopAt, legacy, raw)
		}
		if !stopped && !stoppedReclaim {
			if stopAt < 6 {
				t.Fatalf("only %d steps taken", stopAt)
			}
			return
		}
	}
}

// A receipt that vouches for a raw file by size and mtime must never be enough
// to retire its legacy file.
func TestAcceptLegacyRemovedOnlyAfterRawVerifiedNow(t *testing.T) {
	f := newMaintenanceFixture(t, 5)
	f.m.window = time.Hour
	if _, err := f.m.runOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(f.images))
	for name := range f.images {
		names = append(names, name)
	}
	sort.Strings(names)
	// [0] corrupted in place with its size and mtime restored, so its receipt
	// still matches; [1] removed; [2] replaced by a symlink to a good copy;
	// [3] truncated; [4] untouched.
	corrupt := sessionImageRawPath(f.stateDir, names[0])
	info, _ := os.Stat(corrupt)
	bad := append([]byte(nil), f.images[names[0]]...)
	bad[len(bad)/2] ^= 0xff
	must(t, os.WriteFile(corrupt, bad, 0o600))
	must(t, os.Chtimes(corrupt, info.ModTime(), info.ModTime()))
	must(t, os.Remove(sessionImageRawPath(f.stateDir, names[1])))
	elsewhere := filepath.Join(t.TempDir(), "good.raw")
	must(t, os.WriteFile(elsewhere, f.images[names[2]], 0o600))
	must(t, os.Remove(sessionImageRawPath(f.stateDir, names[2])))
	must(t, os.Symlink(elsewhere, sessionImageRawPath(f.stateDir, names[2])))
	must(t, os.WriteFile(sessionImageRawPath(f.stateDir, names[3]), f.images[names[3]][:10], 0o600))

	f.clock = f.clock.Add(2 * time.Hour)
	invariant := func(pass string) {
		for _, name := range names {
			if _, err := os.Lstat(sessionImageLegacyPath(f.stateDir, name)); errors.Is(err, os.ErrNotExist) {
				rawPath := sessionImageRawPath(f.stateDir, name)
				info, statErr := os.Lstat(rawPath)
				if statErr != nil || !info.Mode().IsRegular() || verifyRawSessionImageFile(rawPath, name, int64(len(f.images[name]))) != nil {
					t.Fatalf("%s: legacy file of %s removed but its raw twin does not verify", pass, name[:8])
				}
			}
		}
		f.assertImagesIntact(t)
	}
	report, err := f.m.runOnce(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	invariant("reclaim")
	if _, err := oldBinaryRead(f.stateDir, names[0]); err != nil {
		t.Fatalf("legacy file of the corrupted twin removed (report %+v): %v", report, err)
	}
	if report.VerifyFailures != 1 {
		t.Fatalf("verify failures %d", report.VerifyFailures)
	}
	// The next pass repairs the corrupted twin, then retires its legacy file.
	if _, err := f.m.runOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	invariant("repair")
	if legacy, raw := f.count(t); legacy != 0 || raw != 5 {
		t.Fatalf("after repair: legacy %d raw %d", legacy, raw)
	}
}

// ---------------------------------------------------------------------------
// Concurrency: writers of the same digests and readers during every phase.

func TestAcceptConcurrentWritersAndReadersDuringMigrationAndReclaim(t *testing.T) {
	f := newMaintenanceFixture(t, 24)
	f.m.window = time.Hour
	names := make([]string, 0, len(f.images))
	for name := range f.images {
		names = append(names, name)
	}
	sort.Strings(names)
	run := func(label string) {
		ctx, cancel := context.WithCancel(t.Context())
		var wg sync.WaitGroup
		var failures atomic.Int64
		var firstFailure atomic.Value
		fail := func(format string, args ...any) {
			if failures.Add(1) == 1 {
				firstFailure.Store(fmt.Sprintf(format, args...))
			}
		}
		for w := 0; w < 4; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				for i := 0; ctx.Err() == nil; i++ {
					name := names[(i*7+w)%len(names)]
					raw := f.images[name]
					item := map[string]any{"mimeType": "image/png", "data": base64.StdEncoding.EncodeToString(raw)}
					if source := f.sources[name]; source != "" {
						item[sessionImageSourceField] = source
					}
					plans, err := stageExternalSessionImageData(map[string]any{"images": []any{item}}, f.stateDir)
					if err == nil {
						err = persistSessionImagePlans(plans)
					}
					if err != nil {
						fail("%s writer: %v", label, err)
					}
				}
			}(w)
		}
		for r := 0; r < 4; r++ {
			wg.Add(1)
			go func(r int) {
				defer wg.Done()
				for i := 0; ctx.Err() == nil; i++ {
					name := names[(i*5+r)%len(names)]
					ref := sessionImageDirname + "/" + name
					got, err := readChatImageBytes(ref, f.stateDir)
					if err != nil || !bytes.Equal(got, f.images[name]) {
						fail("%s reader %s: %v", label, name[:8], err)
					}
				}
			}(r)
		}
		report, err := f.m.runOnce(ctx)
		cancel()
		wg.Wait()
		if err != nil {
			t.Fatalf("%s pass: %v", label, err)
		}
		if failures.Load() > 0 {
			t.Fatalf("%s: %d failures, first: %v (report %+v)", label, failures.Load(), firstFailure.Load(), report)
		}
		f.assertImagesIntact(t)
	}
	run("migration")
	for name := range f.images {
		if _, err := oldBinaryRead(f.stateDir, name); err != nil {
			t.Fatalf("0.1.229 lost %s during the window: %v", name[:8], err)
		}
	}
	f.clock = f.clock.Add(2 * time.Hour)
	run("reclaim")
	entries, _ := os.ReadDir(filepath.Join(f.stateDir, sessionImageDirname))
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tmp") {
			t.Fatalf("temporary left behind: %s", entry.Name())
		}
	}
}

// ---------------------------------------------------------------------------
// 2. Rollback.

func TestAcceptRollbackWindowIsAConstantProductionCannotOverride(t *testing.T) {
	if imageStoreRollbackWindow != 7*24*time.Hour || imageStoreDeletedChatGrace != 7*24*time.Hour {
		t.Fatalf("window %s grace %s", imageStoreRollbackWindow, imageStoreDeletedChatGrace)
	}
	t.Setenv("WORKASS_PROFILE", "")
	t.Setenv(imageStoreWindowEnv, "1s")
	t.Setenv(imageStoreGraceEnv, "1s")
	t.Setenv(imageStoreRateEnv, "0")
	m := imageStoreMaintenanceFor(t.TempDir(), "test", nil, &providerChatRuntime{}, nil)
	if m.window != imageStoreRollbackWindow || m.grace != imageStoreDeletedChatGrace || m.rate != imageStoreBytesPerSecond {
		t.Fatalf("production honoured an override: window %s grace %s rate %d", m.window, m.grace, m.rate)
	}
	t.Setenv("WORKASS_PROFILE", "dev")
	m = imageStoreMaintenanceFor(t.TempDir(), "test", nil, &providerChatRuntime{}, nil)
	if m.window != time.Second {
		t.Fatalf("dev override ignored: %s", m.window)
	}
}

// During the window, 0.1.229 must read every image this build ever wrote,
// whichever way it was stored.
func TestAcceptOlderBinaryReadsEverythingDuringTheWindow(t *testing.T) {
	f := newMaintenanceFixture(t, 6)
	if _, err := f.m.runOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	viewed := t.TempDir()
	add := func(raw []byte, sources ...string) {
		name := persistThroughStaging(t, f.stateDir, raw, sources...)
		f.images[name] = raw
	}
	// New images on each day of the window: plain, with a clone source, a
	// re-send of an existing image, and one whose raw twin already exists.
	for day := 0; day < 7; day++ {
		f.clock = f.clock.Add(24*time.Hour - time.Minute)
		plain := testImageBytes(t, 900+day)
		add(plain)
		cloned := testImageBytes(t, 950+day)
		source := filepath.Join(viewed, fmt.Sprintf("v%d.png", day))
		must(t, os.WriteFile(source, cloned, 0o644))
		add(cloned, source)
		for name, raw := range f.images {
			add(raw)
			_ = name
			break
		}
		rawFirst := testImageBytes(t, 980+day)
		writeRawTestImage(t, f.stateDir, rawFirst)
		add(rawFirst)
		report, err := f.m.runOnce(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if report.Retired != 0 {
			t.Fatalf("day %d: retired inside the window: %+v", day, report)
		}
		for name, raw := range f.images {
			got, err := oldBinaryRead(f.stateDir, name)
			if err != nil || !bytes.Equal(got, raw) {
				t.Fatalf("day %d: 0.1.229 cannot read %s: %v", day, name[:8], err)
			}
		}
	}
}

// After the window, an older build that ran in the meantime restarts it. The
// restart must hold for every image written from that moment on, not just
// from the end of the next pass's migration.
func TestAcceptRestartedWindowShadowsImagesWrittenBeforeThePassFinishes(t *testing.T) {
	withLegacyShadow(t, true)
	stateDir := t.TempDir()
	now := time.Now()
	m := newImageStoreMaintenance(stateDir, "test", &fakeImageRefs{rows: map[string][][]byte{}}, t.Logf)
	m.rate = 0
	m.sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	// The window started and closed long ago, reclaim ran.
	must(t, m.saveMarker(imageStoreMarker{WindowStartedAt: now.Add(-30 * 24 * time.Hour)}))
	// Then the user went back to 0.1.229 for a day; it wrote an image.
	olderRaw := testImageBytes(t, 1)
	olderName := writeLegacyTestImage(t, stateDir, olderRaw)
	must(t, os.Chtimes(sessionImageLegacyPath(stateDir, olderName), now.Add(-time.Hour), now.Add(-time.Hour)))
	// Now this build starts again.
	m.applyMarker()
	early := testImageBytes(t, 2)
	earlyName := persistThroughStaging(t, stateDir, early)
	// And one more image arrives while the pass is migrating.
	var midName string
	m.hook = func(step, digest string) error {
		if step == "raw" && midName == "" {
			midName = persistThroughStaging(t, stateDir, testImageBytes(t, 3))
		}
		return nil
	}
	report, err := m.runOnce(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if report.OlderBuildWrites != 1 || report.ReclaimAllowed {
		t.Fatalf("older build not seen: %+v", report)
	}
	var missing []string
	if _, err := oldBinaryRead(stateDir, earlyName); err != nil {
		missing = append(missing, "written after start, before the pass: "+err.Error())
	}
	if midName != "" {
		if _, err := oldBinaryRead(stateDir, midName); err != nil {
			missing = append(missing, "written during the pass's migration: "+err.Error())
		}
	}
	if len(missing) > 0 {
		t.Fatalf("an older build has been seen running, so a rollback is live, yet 0.1.229 cannot read images this build wrote:\n  %s", strings.Join(missing, "\n  "))
	}
}

// state.db is only ever read: a full pass (migration, reclaim, collection)
// leaves every table's content exactly as it was.
func TestAcceptStateDBUnchangedByAFullPass(t *testing.T) {
	withLegacyShadow(t, true)
	stateDir := t.TempDir()
	db, err := store.Open(store.Options{Path: filepath.Join(stateDir, store.FileName), BinaryVersion: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	gone := writeLegacyTestImage(t, stateDir, testImageBytes(t, 11))
	kept := writeLegacyTestImage(t, stateDir, testImageBytes(t, 12))
	old := time.Now().Add(-30 * 24 * time.Hour).UTC().Format(time.RFC3339Nano)
	sqlInsertChat(t, db, "chat-gone", "tab-gone", true, old, nil, userRow(gone, kept))
	sqlInsertChat(t, db, "chat-here", "tab-here", false, old, map[string]any{"Queue": []any{map[string]any{"Attachments": []any{map[string]any{"Digest": kept}}}}}, toolRow("/x.png", kept))
	dump := func() string {
		var out strings.Builder
		must(t, db.Read(t.Context(), func(tx *store.Tx) error {
			rows, err := tx.Query(`SELECT name FROM sqlite_schema WHERE type='table' ORDER BY name`)
			if err != nil {
				return err
			}
			var tables []string
			for rows.Next() {
				var name string
				_ = rows.Scan(&name)
				tables = append(tables, name)
			}
			rows.Close()
			for _, table := range tables {
				rows, err := tx.Query(`SELECT * FROM "` + table + `"`)
				if err != nil {
					return err
				}
				cols, _ := rows.Columns()
				for rows.Next() {
					values := make([]any, len(cols))
					ptrs := make([]any, len(cols))
					for i := range values {
						ptrs[i] = &values[i]
					}
					_ = rows.Scan(ptrs...)
					fmt.Fprintf(&out, "%s %v\n", table, values)
				}
				rows.Close()
			}
			return nil
		}))
		return out.String()
	}
	before := dump()
	m := newImageStoreMaintenance(stateDir, "test", sqlImageReferences{actors: chat.NewSQLActors(db)}, t.Logf)
	m.rate, m.window = 0, 0
	m.sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	report, err := m.runOnce(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if report.Collected != 1 || report.Retired == 0 {
		t.Fatalf("the pass did not exercise every phase: %+v", report)
	}
	if after := dump(); after != before {
		t.Fatalf("state.db changed:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func sqlInsertChat(t testing.TB, db *store.DB, chatID, tabID string, deleted bool, updated string, extra map[string]any, rows ...[]byte) {
	t.Helper()
	state := map[string]any{"ChatID": chatID, "Deleted": deleted, "Presentation": map[string]any{"TabID": tabID}}
	for k, v := range extra {
		state[k] = v
	}
	encoded, _ := json.Marshal(state)
	must(t, db.Write(context.Background(), func(tx *store.Tx) error {
		if _, err := tx.Exec(`INSERT INTO chats(chat_id, envelope_version, revision, state_json, ledger_len, updated_at, ledger_gen) VALUES (?, 1, 1, ?, ?, ?, 1)`,
			chatID, string(encoded), len(rows), updated); err != nil {
			return err
		}
		for index, row := range rows {
			if _, err := tx.Exec(`INSERT INTO messages(chat_id, seq, message_id, role, event_json) VALUES (?, ?, ?, 'user', ?)`,
				chatID, index+1, fmt.Sprintf("m%d", index), string(row)); err != nil {
				return err
			}
		}
		return nil
	}))
}

// ---------------------------------------------------------------------------
// 3. Delete with chat.

// Every kind of reference other than the long-deleted chat's own keeps its
// image, through the real SQLite reference reader.
func TestAcceptCollectionKeepsEveryOtherReference(t *testing.T) {
	withLegacyShadow(t, true)
	stateDir := t.TempDir()
	db, err := store.Open(store.Options{Path: filepath.Join(stateDir, store.FileName), BinaryVersion: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	labels := []string{"only", "live", "fork", "recentDeleted", "archive", "checkpoint", "receipt", "queued", "tool", "journal", "backup", "mirror", "graceEdge"}
	names := map[string]string{}
	images := map[string][]byte{}
	for index, label := range labels {
		raw := testImageBytes(t, 1200+index)
		names[label] = writeLegacyTestImage(t, stateDir, raw)
		images[names[label]] = raw
	}
	all := make([]string, 0, len(labels))
	for _, label := range labels {
		if label != "graceEdge" {
			all = append(all, names[label])
		}
	}
	now := time.Now()
	old := now.Add(-30 * 24 * time.Hour).UTC().Format(time.RFC3339Nano)
	sqlInsertChat(t, db, "chat-gone", "tab-gone", true, old, nil, userRow(all...))
	sqlInsertChat(t, db, "chat-edge", "tab-edge", true, now.Add(-imageStoreDeletedChatGrace+time.Minute).UTC().Format(time.RFC3339Nano), nil, userRow(names["graceEdge"]))
	sqlInsertChat(t, db, "chat-live", "tab-live", false, old, nil, userRow(names["live"]))
	sqlInsertChat(t, db, "chat-fork", "tab-fork", false, old, map[string]any{"ForkSource": "chat-gone"}, toolRow("/w/a.png", names["fork"]))
	sqlInsertChat(t, db, "chat-recent", "tab-recent", true, now.Add(-time.Hour).UTC().Format(time.RFC3339Nano), nil, userRow(names["recentDeleted"]))
	sqlInsertChat(t, db, "chat-queue", "tab-queue", false, old, map[string]any{"Queue": []any{map[string]any{"Attachments": []any{map[string]any{"Digest": names["queued"], "Ref": "workass-session-image:images/" + names["queued"]}}}}})
	sqlInsertChat(t, db, "chat-tools", "tab-tools", false, old, nil)
	must(t, db.Write(t.Context(), func(tx *store.Tx) error {
		if _, err := tx.Exec(`INSERT INTO chat_tools(chat_id, tool_id, owner_json) VALUES ('chat-tools', 't1', ?)`, `{"Attachments":[{"Digest":"`+names["tool"]+`"}]}`); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO chat_events(chat_id, revision, base_revision, record) VALUES ('chat-tools', 2, 1, ?)`, `{"images":[{"ref":"images/`+names["journal"]+`"}]}`)
		return err
	}))
	write := func(rel, content string) {
		path := filepath.Join(stateDir, filepath.FromSlash(rel))
		must(t, os.MkdirAll(filepath.Dir(path), 0o700))
		must(t, os.WriteFile(path, []byte(content), 0o600))
	}
	write("chat-archive/tab-other.jsonl", `{"ref":"images/`+names["archive"]+`"}`)
	write("checkpoints/tab-live/cp-1.json", `{"Attachments":[{"Digest":"`+names["checkpoint"]+`"}]}`)
	write("spawned-work/receipts.jsonl", `{"image":"`+names["receipt"]+`"}`+"\n")
	write("session-state.json", `{"tabs":{"x":{"images":[{"__sessionImageRef":"images/`+names["mirror"]+`"}]}}}`)
	// A SQLite backup that names one image, inside an overflow page.
	backupDB, err := store.Open(store.Options{Path: filepath.Join(t.TempDir(), store.FileName), BinaryVersion: "test"})
	if err != nil {
		t.Fatal(err)
	}
	sqlInsertChat(t, backupDB, "chat-b", "tab-b", false, old, map[string]any{"Pad": strings.Repeat("p", 9000) + names["backup"] + strings.Repeat("q", 9000)})
	must(t, os.MkdirAll(filepath.Join(stateDir, "backups"), 0o700))
	must(t, backupDB.Backup(filepath.Join(stateDir, "backups", "state-v4-x.db")))
	_ = backupDB.Close()

	m := newImageStoreMaintenance(stateDir, "test", sqlImageReferences{actors: chat.NewSQLActors(db)}, t.Logf)
	m.rate = 0
	m.sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	report, err := m.runOnce(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if report.CollectSkipped != "" {
		t.Fatalf("collection skipped: %s", report.CollectSkipped)
	}
	for _, label := range labels {
		_, err := readChatImageBytes(sessionImageDirname+"/"+names[label], stateDir)
		switch {
		case label == "only" && err == nil:
			t.Fatal("the image only the long-deleted chat referenced must be collected")
		case label != "only" && err != nil:
			t.Fatalf("%s image collected although referenced: %v", label, err)
		}
	}
	if report.Collected != 1 {
		t.Fatalf("collected %d", report.Collected)
	}
}

// A live chat that commits a reference to the deleted chat's image while the
// collector is reading it (after its ledger chunk was read) keeps the image.
func TestAcceptCollectionSeesACommitDuringItsOwnScan(t *testing.T) {
	stateDir := t.TempDir()
	db, err := store.Open(store.Options{Path: filepath.Join(stateDir, store.FileName), BinaryVersion: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	shared := writeRawTestImage(t, stateDir, testImageBytes(t, 1400))
	old := time.Now().Add(-30 * 24 * time.Hour).UTC().Format(time.RFC3339Nano)
	sqlInsertChat(t, db, "chat-gone", "tab-gone", true, old, nil, userRow(shared))
	sqlInsertChat(t, db, "chat-live", "tab-live", false, old, nil, userRow())
	m := newImageStoreMaintenance(stateDir, "test", sqlImageReferences{actors: chat.NewSQLActors(db)}, t.Logf)
	m.rate = 0
	pauses := 0
	committed := false
	m.sleep = func(ctx context.Context, _ time.Duration) error {
		pauses++
		// Pauses 1-4 read chat-gone (candidates); 5 precedes chat-live's
		// ledger chunk, 6 its journal chunk: commit between them.
		if pauses == 6 {
			committed = true
			must(t, db.Write(ctx, func(tx *store.Tx) error {
				if _, err := tx.Exec(`INSERT INTO messages(chat_id, seq, message_id, role, event_json) VALUES ('chat-live', 2, 'm-new', 'user', ?)`, string(userRow(shared))); err != nil {
					return err
				}
				_, err := tx.Exec(`UPDATE chats SET revision = revision + 1, ledger_len = 2 WHERE chat_id = 'chat-live'`)
				return err
			}))
		}
		return ctx.Err()
	}
	report, err := m.runOnce(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// 4 for chat-gone, 2 for chat-live's interrupted read, 4 for its re-read.
	if !committed || pauses != 10 {
		t.Fatalf("the commit was not injected mid-scan or the scan did not restart (%d pauses)", pauses)
	}
	if _, err := readChatImageBytes(sessionImageDirname+"/"+shared, stateDir); err != nil {
		t.Fatalf("collected an image a live chat committed a reference to during the scan (report %+v)", report)
	}
}

// A reference source or state file that cannot be read means nothing is
// removed.
func TestAcceptCollectionRemovesNothingWhenAnyScanFails(t *testing.T) {
	cases := map[string]func(t *testing.T, f *maintenanceFixture, refs *failingRefs){
		"chat list fails":         func(_ *testing.T, _ *maintenanceFixture, refs *failingRefs) { refs.failChats = true },
		"live chat rows fail":     func(_ *testing.T, _ *maintenanceFixture, refs *failingRefs) { refs.failRows = "chat-live" },
		"deleted chat rows fail":  func(_ *testing.T, _ *maintenanceFixture, refs *failingRefs) { refs.failRows = "chat-deleted" },
		"corrupt sqlite in state": func(_ *testing.T, f *maintenanceFixture, _ *failingRefs) { writeCorruptSQLite(f) },
		"unreadable dir in state": func(t *testing.T, f *maintenanceFixture, _ *failingRefs) {
			dir := filepath.Join(f.stateDir, "locked")
			must(t, os.MkdirAll(dir, 0o700))
			must(t, os.WriteFile(filepath.Join(dir, "x.json"), []byte("{}"), 0o600))
			must(t, os.Chmod(dir, 0))
		},
	}
	for label, setup := range cases {
		t.Run(label, func(t *testing.T) {
			if runtime.GOOS == "windows" && strings.HasPrefix(label, "unreadable") {
				t.Skip("chmod 0 does not lock a directory on Windows")
			}
			f, names := collectFixture(t)
			refs := &failingRefs{fakeImageRefs: f.refs}
			f.m.refs = refs
			t.Cleanup(func() { _ = os.Chmod(filepath.Join(f.stateDir, "locked"), 0o700) })
			setup(t, f, refs)
			t.Cleanup(func() { _ = os.Chmod(filepath.Join(f.stateDir, "locked"), 0o700) })
			report, err := f.m.runOnce(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if report.Collected != 0 {
				t.Fatalf("collected %d despite %s (%+v)", report.Collected, label, report)
			}
			if _, err := readChatImageBytes(sessionImageDirname+"/"+names["onlyDeleted"], f.stateDir); err != nil {
				t.Fatal("removed on doubt")
			}
		})
	}
}

type failingRefs struct {
	*fakeImageRefs
	failChats bool
	failRows  string
}

func (r *failingRefs) chats(ctx context.Context) ([]imageRefChat, error) {
	if r.failChats {
		return nil, errors.New("chat list unreadable")
	}
	return r.fakeImageRefs.chats(ctx)
}

func (r *failingRefs) eachRow(ctx context.Context, chatID string, kinds map[string]bool, stable bool, pause func() error, restart func(), visit func(string, []byte) error) error {
	if chatID == r.failRows && kinds == nil {
		return errors.New("rows unreadable")
	}
	return r.fakeImageRefs.eachRow(ctx, chatID, kinds, stable, pause, restart, visit)
}

func writeCorruptSQLite(f *maintenanceFixture) {
	path := filepath.Join(f.stateDir, "backups", "broken.db")
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	_ = os.WriteFile(path, append([]byte("SQLite format 3\x00"), bytes.Repeat([]byte{0xee}, 8192)...), 0o600)
}

func TestAcceptCollectionGracePeriodBoundary(t *testing.T) {
	for _, tc := range []struct {
		age     time.Duration
		collect bool
	}{{imageStoreDeletedChatGrace - time.Minute, false}, {imageStoreDeletedChatGrace + time.Minute, true}} {
		f := newMaintenanceFixture(t, 0)
		raw := testImageBytes(t, 1300)
		name := writeLegacyTestImage(t, f.stateDir, raw)
		f.refs.list = []imageRefChat{{chatID: "gone", deleted: true, updatedAt: f.clock.Add(-tc.age)}}
		f.refs.rows = map[string][][]byte{"gone": {userRow(name)}}
		report, err := f.m.runOnce(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if (report.Collected == 1) != tc.collect {
			t.Fatalf("deleted %s ago: collected %d, want collect=%v", tc.age, report.Collected, tc.collect)
		}
	}
}

// ---------------------------------------------------------------------------
// 4. Path classifier.

type virtualFS struct {
	links   map[string]string // path -> symlink target
	dirs    map[string]bool
	touched []string
}

func (v *virtualFS) lstat(path string) (os.FileInfo, error) {
	v.touched = append(v.touched, path)
	if _, ok := v.links[path]; ok {
		return fakeInfo{mode: os.ModeSymlink}, nil
	}
	return fakeInfo{mode: 0}, nil
}

func (v *virtualFS) readlink(path string) (string, error) {
	v.touched = append(v.touched, "readlink:"+path)
	return v.links[path], nil
}

type fakeInfo struct{ mode fs.FileMode }

func (f fakeInfo) Name() string       { return "x" }
func (f fakeInfo) Size() int64        { return 0 }
func (f fakeInfo) Mode() fs.FileMode  { return f.mode }
func (f fakeInfo) ModTime() time.Time { return time.Time{} }
func (f fakeInfo) IsDir() bool        { return false }
func (f fakeInfo) Sys() any           { return nil }

func TestAcceptGuardRefusesEveryAliasOfAProtectedPlace(t *testing.T) {
	guard := sessionImageSourceGuard{home: "/Users/me", caseInsensitive: true, mounts: []string{"/Users/me/Work/smb"}}
	refused := []string{
		"/Users/me/Desktop/a.png", "/USERS/ME/DESKTOP/a.png", "/users/me/desktop", "/Users/me/Documents/../Downloads/a.png",
		"/Users/me/Work/../Pictures/a.png", "/Users/me/./Movies/a.png", "/Users/me//Music/a.png", "/Users/me/Library/Mobile Documents/x.png",
		"/Users/me/LIBRARY/Containers/x", "/Users/me/.Trash/x.png", "/Users/me/.trash/x.png", "/System/Volumes/Data/Users/me/Desktop/a.png",
		"/system/volumes/data/users/me/documents/a.png", "/Volumes/USB/a.png", "/volumes/x", "/System/Volumes/Data/Volumes/x.png",
		"/net/host/a.png", "/Network/x", "/automount/x", "/Users/other/a.png", "/Users/Shared/a.png", "/Users/me/Work/smb/a.png",
		"/Users/me/work/SMB/a.png", "relative/a.png", "", "/Users/me/Desktop\x00/a.png",
		// Unicode case folding: APFS treats U+017F (long s) as "s".
		"/Users/me/Deſktop/a.png", "/Users/me/Downloadſ/a.png", "/Users/me/Muſic/a.png", "/Volumeſ/USB/a.png",
	}
	for _, path := range refused {
		if err := guard.check(path); err == nil {
			t.Errorf("allowed %q", path)
		}
	}
	allowed := []string{"/Users/me/Workspace/a.png", "/Users/me/DesktopX/a.png", "/private/tmp/a.png", "/tmp/a.png", "/private/var/folders/x/a.png", "/Users/me"}
	for _, path := range allowed {
		if err := guard.check(path); err != nil {
			t.Errorf("refused %q: %v", path, err)
		}
	}
}

func TestAcceptResolveNeverTouchesAProtectedPathThroughLinks(t *testing.T) {
	guard := sessionImageSourceGuard{home: "/Users/me", caseInsensitive: true}
	cases := map[string]struct {
		path  string
		links map[string]string
	}{
		"absolute link":         {"/Users/me/w/l/a.png", map[string]string{"/Users/me/w/l": "/Users/me/Desktop"}},
		"relative link with ..": {"/Users/me/w/l", map[string]string{"/Users/me/w/l": "../Desktop/a.png"}},
		"chain":                 {"/Users/me/w/a/x.png", map[string]string{"/Users/me/w/a": "/Users/me/w/b", "/Users/me/w/b": "/Users/me/w/c", "/Users/me/w/c": "/Users/me/Documents"}},
		"data volume alias":     {"/Users/me/w/l/a.png", map[string]string{"/Users/me/w/l": "/System/Volumes/Data/Users/me/Pictures"}},
		"case alias":            {"/Users/me/w/l/a.png", map[string]string{"/Users/me/w/l": "/users/ME/library"}},
		"via /private":          {"/Users/me/w/l/a.png", map[string]string{"/Users/me/w/l": "/private/tmp/../../Users/me/Downloads"}},
		"link to /Volumes":      {"/Users/me/w/l/a.png", map[string]string{"/Users/me/w/l": "/Volumes/Ext"}},
		"link to other home":    {"/Users/me/w/l/a.png", map[string]string{"/Users/me/w/l": "/Users/you"}},
		"tmp link into Desktop": {"/tmp/l/a.png", map[string]string{"/tmp": "private/tmp", "/private/tmp/l": "/Users/me/Desktop"}},
		"loop":                  {"/Users/me/w/a/x", map[string]string{"/Users/me/w/a": "/Users/me/w/b", "/Users/me/w/b": "/Users/me/w/a"}},
		"link .. after link":    {"/Users/me/w/l/../x.png", map[string]string{"/Users/me/w/l": "/Users/me/Desktop/sub"}},
		"long-s link":           {"/Users/me/w/l/a.png", map[string]string{"/Users/me/w/l": "/Users/me/Deſktop"}},
	}
	for label, tc := range cases {
		v := &virtualFS{links: tc.links}
		resolved, err := guard.resolve(tc.path, v.lstat, v.readlink)
		for _, touched := range v.touched {
			path := strings.TrimPrefix(touched, "readlink:")
			if guard.check(path) != nil {
				t.Errorf("%s: touched protected %q", label, touched)
			}
		}
		if label == "link .. after link" {
			// Lexical cleaning: resolves to /Users/me/w/x.png, never into Desktop.
			if err != nil || resolved != "/Users/me/w/x.png" {
				t.Errorf("%s: resolved %q %v", label, resolved, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: resolved %q, want refused", label, resolved)
		}
	}
}

// ---------------------------------------------------------------------------
// 6. Performance.

func TestAcceptThrottleHoldsTheRate(t *testing.T) {
	var slept time.Duration
	throttle := &imageStoreThrottle{rate: 1 << 20, start: time.Now()}
	// The fake sleep moves the throttle's start back, as time passing would.
	throttle.sleep = func(_ context.Context, d time.Duration) error {
		slept += d
		throttle.start = throttle.start.Add(-d)
		return nil
	}
	for i := 0; i < 10; i++ {
		must(t, throttle.add(t.Context(), 1<<20))
	}
	if slept < 9*time.Second || slept > 10*time.Second+500*time.Millisecond {
		t.Fatalf("10 MiB at 1 MiB/s slept %s", slept)
	}
}

// Receipts must not reach the file before the barrier that makes the raw
// writes they vouch for durable.
func TestAcceptReceiptsNeverWrittenBeforeTheirBarrier(t *testing.T) {
	dir := t.TempDir()
	receipts, err := openImageStoreReceipts(dir, "test")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, imageStoreReceiptsFilename)
	var sizeAtFirstBarrier int64 = -1
	receipts.barrier = func() error {
		if sizeAtFirstBarrier < 0 {
			info, _ := os.Stat(path)
			sizeAtFirstBarrier = info.Size()
		}
		return nil
	}
	source := "/Users/me/Workspace/labelling/session-2026-10-26/screenshots/" + strings.Repeat("frame-", 8) + ".png"
	for i := 0; i < 256; i++ {
		must(t, receipts.record(imageStoreReceipt{Digest: fmt.Sprintf("%064x", i), Outcome: imageReceiptCloned, Source: source,
			RawSize: 123456, RawMTimeNS: time.Now().UnixNano(), LegacySize: 164608}))
	}
	_ = receipts.close()
	if sizeAtFirstBarrier != 0 {
		t.Fatalf("%d bytes of receipts reached the file before the first barrier ran (bufio auto-flush), so a receipt can outlive the raw write it vouches for", sizeAtFirstBarrier)
	}
}

// ---------------------------------------------------------------------------
// 5. The JSON chat backend (Windows production) and a platform without clones.

func writeFileBackendChat(t testing.TB, stateDir, chatID string, mtime time.Time, journal string, ledger ...[]byte) {
	t.Helper()
	rows := make([]json.RawMessage, len(ledger))
	for i, row := range ledger {
		rows[i] = row
	}
	envelope, _ := json.Marshal(map[string]any{"v": 1, "state": map[string]any{"ChatID": chatID, "Ledger": rows}})
	base := filepath.Join(stateDir, filepath.FromSlash(providerChatFileName(chatID)))
	must(t, os.MkdirAll(filepath.Dir(base), 0o700))
	must(t, os.WriteFile(base, envelope, 0o600))
	must(t, os.Chtimes(base, mtime, mtime))
	if journal != "" {
		must(t, os.WriteFile(base+".events", []byte(journal), 0o600))
	}
}

func TestAcceptFileBackendCollectionEndToEnd(t *testing.T) {
	withLegacyShadow(t, true)
	stateDir := t.TempDir()
	labels := []string{"only", "shared", "liveJournal", "unknownChatFile", "recent"}
	names := map[string]string{}
	for i, label := range labels {
		names[label] = writeLegacyTestImage(t, stateDir, testImageBytes(t, 1500+i))
	}
	now := time.Now()
	old := now.Add(-30 * 24 * time.Hour)
	writeFileBackendChat(t, stateDir, "chat-gone", old, "", userRow(names["only"], names["shared"], names["liveJournal"], names["unknownChatFile"], names["recent"]))
	writeFileBackendChat(t, stateDir, "chat-live", now, `{"images":[{"ref":"images/`+names["liveJournal"]+`"}]}`, userRow(names["shared"]))
	writeFileBackendChat(t, stateDir, "chat-recent", now.Add(-time.Hour), "", userRow(names["recent"]))
	// An actor file the runtime does not know (quarantined, unreadable at
	// boot) still names an image: it must keep it.
	writeFileBackendChat(t, stateDir, "chat-unknown", old, "", userRow(names["unknownChatFile"]))
	runtime := &providerChatRuntime{
		known: map[string]struct{}{"chat-gone": {}, "chat-live": {}, "chat-recent": {}},
		summaries: map[string]chat.ActorSummary{
			"chat-gone":   {ChatID: "chat-gone", TabID: "tab-gone", Deleted: true},
			"chat-live":   {ChatID: "chat-live", TabID: "tab-live"},
			"chat-recent": {ChatID: "chat-recent", TabID: "tab-recent", Deleted: true},
		},
	}
	m := newImageStoreMaintenance(stateDir, "test", fileImageReferences{stateDir: stateDir, runtime: runtime}, t.Logf)
	m.rate, m.window = 0, 0
	m.sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	report, err := m.runOnce(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if report.CollectSkipped != "" || report.DeletedChatsEligible != 1 {
		t.Fatalf("%+v", report)
	}
	for _, label := range labels {
		_, err := readChatImageBytes(sessionImageDirname+"/"+names[label], stateDir)
		if (label == "only") != (err != nil) {
			t.Fatalf("%s: read err %v (collected %d)", label, err, report.Collected)
		}
	}
	if _, err := os.Stat(sessionImageLegacyPath(stateDir, names["shared"])); err == nil {
		t.Fatal("window 0: the legacy file should be retired")
	}
}

// A volume or platform that cannot clone: everything is written from the bytes
// in hand, no candidate is ever opened, and the store converges the same way.
func TestAcceptNoClonePlatformLifecycle(t *testing.T) {
	f := newMaintenanceFixture(t, 10)
	tried := 0
	stubImageClones(t, func(dir, final, name string, raw []byte, candidate string, durability sessionImageDurability) (sessionImageCloneOutcome, error) {
		tried++
		return cloneUnsupported, errors.New("not APFS")
	})
	report, err := f.m.runOnce(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if report.Written != 10 || report.Cloned != 0 || report.WriteErrors != 0 {
		t.Fatalf("%+v", report)
	}
	if tried > 10 {
		t.Fatalf("tried %d candidates for 10 images; an unsupported volume must stop at the first", tried)
	}
	for name := range f.images {
		if _, err := oldBinaryRead(f.stateDir, name); err != nil {
			t.Fatal(err)
		}
	}
	f.clock = f.clock.Add(imageStoreRollbackWindow)
	report, err = f.m.runOnce(t.Context())
	if err != nil || report.Retired != 10 {
		t.Fatalf("%+v %v", report, err)
	}
	f.assertImagesIntact(t)
}
