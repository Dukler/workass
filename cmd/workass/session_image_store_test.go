package main

import (
	"bytes"
	"encoding/base64"
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// testImageBytes returns a distinct small PNG per seed.
func testImageBytes(t testing.TB, seed int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8+seed%5, 6+seed%3))
	for y := 0; y < img.Bounds().Dy(); y++ {
		for x := 0; x < img.Bounds().Dx(); x++ {
			img.Set(x, y, color.RGBA{uint8(seed), uint8(x * 13), uint8(y * 29), 255})
		}
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func testImageDigest(raw []byte) string {
	return sessionImageTextDigest([]byte(base64.StdEncoding.EncodeToString(raw)))
}

// writeLegacyTestImage stores raw the way 0.1.229 does: its base64 text.
func writeLegacyTestImage(t testing.TB, stateDir string, raw []byte) string {
	t.Helper()
	text := base64.StdEncoding.EncodeToString(raw)
	name := sessionImageTextDigest([]byte(text))
	if err := os.MkdirAll(filepath.Join(stateDir, sessionImageDirname), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sessionImageLegacyPath(stateDir, name), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return name
}

func writeRawTestImage(t testing.TB, stateDir string, raw []byte) string {
	t.Helper()
	name := testImageDigest(raw)
	if err := os.MkdirAll(filepath.Join(stateDir, sessionImageDirname), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sessionImageRawPath(stateDir, name), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return name
}

func withLegacyShadow(t testing.TB, on bool) {
	t.Helper()
	previous := sessionImageLegacyShadow.Load()
	sessionImageLegacyShadow.Store(on)
	t.Cleanup(func() { sessionImageLegacyShadow.Store(previous) })
}

func TestSessionImageBothFormsReadIdentically(t *testing.T) {
	raw := testImageBytes(t, 1)
	text := base64.StdEncoding.EncodeToString(raw)
	cases := map[string]func(t *testing.T, stateDir string) string{
		"legacy": func(t *testing.T, stateDir string) string { return writeLegacyTestImage(t, stateDir, raw) },
		"raw":    func(t *testing.T, stateDir string) string { return writeRawTestImage(t, stateDir, raw) },
		"both": func(t *testing.T, stateDir string) string {
			writeLegacyTestImage(t, stateDir, raw)
			return writeRawTestImage(t, stateDir, raw)
		},
	}
	for label, store := range cases {
		t.Run(label, func(t *testing.T) {
			stateDir := t.TempDir()
			name := store(t, stateDir)
			ref := sessionImageDirname + "/" + name
			gotText, err := readExternalSessionImage(ref, stateDir)
			if err != nil || gotText != text {
				t.Fatalf("base64 read = %d chars, %v; want the original text", len(gotText), err)
			}
			gotBytes, err := readChatImageBytes(ref, stateDir)
			if err != nil || !bytes.Equal(gotBytes, raw) {
				t.Fatalf("byte read = %d bytes, %v; want the original bytes", len(gotBytes), err)
			}
			file, err := locateSessionImage(ref, stateDir)
			if err != nil {
				t.Fatal(err)
			}
			if file.logicalSize() != int64(len(text)) {
				t.Fatalf("logical size = %d, want the base64 length %d (attachment Size never changes)", file.logicalSize(), len(text))
			}
			if file.raw != (label != "legacy") {
				t.Fatalf("located raw=%v for %s", file.raw, label)
			}
			if width, height := sessionImageRefSize(ref, stateDir); width != 8+1 || height != 6+1 {
				t.Fatalf("header size = %dx%d", width, height)
			}
		})
	}
}

func TestSessionImageDamagedRawFallsBackToLegacy(t *testing.T) {
	stateDir := t.TempDir()
	raw := testImageBytes(t, 2)
	name := writeLegacyTestImage(t, stateDir, raw)
	if err := os.WriteFile(sessionImageRawPath(stateDir, name), append([]byte("x"), raw[1:]...), 0o600); err != nil {
		t.Fatal(err)
	}
	ref := sessionImageDirname + "/" + name
	got, err := readChatImageBytes(ref, stateDir)
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("damaged raw must not hide the intact legacy file: %v", err)
	}
	// Without the legacy file the damage is reported, never served.
	if err := os.Remove(sessionImageLegacyPath(stateDir, name)); err != nil {
		t.Fatal(err)
	}
	if _, err := readChatImageBytes(ref, stateDir); err == nil {
		t.Fatal("a damaged raw file was served")
	}
}

func TestSessionImageNewWritesAreRawWithShadowDuringWindow(t *testing.T) {
	for _, shadow := range []bool{true, false} {
		t.Run(map[bool]string{true: "window open", false: "window closed"}[shadow], func(t *testing.T) {
			withLegacyShadow(t, shadow)
			stateDir := t.TempDir()
			store := newSessionStore(filepath.Join(stateDir, sessionStateFilename))
			raw := testImageBytes(t, 3)
			attachments, err := store.PersistProviderAttachments([]any{map[string]any{
				"mimeType": "image/png", "data": base64.StdEncoding.EncodeToString(raw), "name": "shot.png",
			}})
			if err != nil {
				t.Fatal(err)
			}
			name := attachments[0].Digest
			if name != testImageDigest(raw) {
				t.Fatalf("digest identity changed: %s", name)
			}
			if attachments[0].Size != int64(base64.StdEncoding.EncodedLen(len(raw))) {
				t.Fatalf("attachment Size = %d, want the base64 length", attachments[0].Size)
			}
			stored, err := os.ReadFile(sessionImageRawPath(stateDir, name))
			if err != nil || !bytes.Equal(stored, raw) {
				t.Fatalf("raw file holds %d bytes, %v; want the image bytes", len(stored), err)
			}
			_, legacyErr := os.Stat(sessionImageLegacyPath(stateDir, name))
			if shadow && legacyErr != nil {
				t.Fatalf("window open: the previous release needs the legacy file: %v", legacyErr)
			}
			if !shadow && !errors.Is(legacyErr, os.ErrNotExist) {
				t.Fatalf("window closed: no legacy file expected, got %v", legacyErr)
			}
			resolved, err := store.ResolveProviderAttachment(t.Context(), attachments[0])
			if err != nil || mapFromAnyMain(resolved)["data"] != base64.StdEncoding.EncodeToString(raw) {
				t.Fatalf("provider resolution must produce the original base64: %v", err)
			}
		})
	}
}

func TestSessionImageNonCanonicalTextKeepsLegacyIdentity(t *testing.T) {
	withLegacyShadow(t, false)
	stateDir := t.TempDir()
	store := newSessionStore(filepath.Join(stateDir, sessionStateFilename))
	raw := testImageBytes(t, 4)
	dataURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(raw)
	attachments, err := store.PersistProviderAttachments([]any{map[string]any{"mimeType": "image/png", "data": dataURL}})
	if err != nil {
		t.Fatal(err)
	}
	name := attachments[0].Digest
	if name != sessionImageTextDigest([]byte(dataURL)) {
		t.Fatal("a data URL's identity is its text")
	}
	if _, err := os.Stat(sessionImageRawPath(stateDir, name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a data URL cannot be stored raw: %v", err)
	}
	got, err := readChatImageBytes(sessionImageDirname+"/"+name, stateDir)
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("data URL image unreadable: %v", err)
	}
}

func TestSessionImageCloneSourceHintNeverPersists(t *testing.T) {
	withLegacyShadow(t, false)
	stateDir := t.TempDir()
	store := newSessionStore(filepath.Join(stateDir, sessionStateFilename))
	raw := testImageBytes(t, 5)
	source := filepath.Join(t.TempDir(), "viewed.png")
	if err := os.WriteFile(source, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	attachments, err := store.PersistProviderAttachments([]any{map[string]any{
		"mimeType": "image/png", "data": base64.StdEncoding.EncodeToString(raw), sessionImageSourceField: []any{source},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if attachments[0].Source != "" {
		t.Fatalf("a clone hint is not the attachment's source: %q", attachments[0].Source)
	}
	stored, err := os.ReadFile(sessionImageRawPath(stateDir, attachments[0].Digest))
	if err != nil || !bytes.Equal(stored, raw) {
		t.Fatal("hinted image not stored")
	}
}

// stubImageClones replaces the platform clone for one test.
func stubImageClones(t testing.TB, clone func(dir, final, name string, raw []byte, candidate string, durability sessionImageDurability) (sessionImageCloneOutcome, error)) {
	t.Helper()
	previous := sessionImageClone
	sessionImageClone = clone
	t.Cleanup(func() { sessionImageClone = previous })
}

func TestSessionImageCloneFromIdenticalSourceAndFallbacks(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("APFS clones")
	}
	home := t.TempDir()
	withSourceGuard(t, sessionImageSourceGuard{home: home, caseInsensitive: true})
	dir := filepath.Join(t.TempDir(), sessionImageDirname)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw := testImageBytes(t, 6)
	name := testImageDigest(raw)
	sources := filepath.Join(home, "Workspace")
	if err := os.MkdirAll(sources, 0o755); err != nil {
		t.Fatal(err)
	}
	same := filepath.Join(sources, "same.png")
	other := filepath.Join(sources, "other.png")
	if err := os.WriteFile(same, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, testImageBytes(t, 7), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := persistRawSessionImage(dir, name, raw, []string{filepath.Join(sources, "missing.png"), other, same}, sessionImageDurable)
	if err != nil {
		t.Fatal(err)
	}
	if result.method != "cloned" || result.source != same {
		t.Fatalf("want a clone of the identical file, got %+v", result)
	}
	if !result.tried(cloneUnavailable) || !result.tried(cloneDifferent) {
		t.Fatalf("the missing and different candidates must be refused first: %v", result.attempts)
	}
	final := filepath.Join(dir, name+sessionImageRawSuffix)
	info, err := os.Lstat(final)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("clone must be private to the store: %v %v", info.Mode(), err)
	}
	// The clone is independent of its source from now on.
	if err := os.WriteFile(same, []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(same); err != nil {
		t.Fatal(err)
	}
	if err := verifyRawSessionImageFile(final, name, int64(len(raw))); err != nil {
		t.Fatalf("clone did not survive its source changing and going: %v", err)
	}

	// No identical candidate: the bytes in hand are written.
	raw2 := testImageBytes(t, 8)
	name2 := testImageDigest(raw2)
	result, err = persistRawSessionImage(dir, name2, raw2, []string{other}, sessionImageBatched)
	if err != nil || result.method != "written" {
		t.Fatalf("fallback write: %+v %v", result, err)
	}
	if err := verifyRawSessionImageFile(filepath.Join(dir, name2+sessionImageRawSuffix), name2, int64(len(raw2))); err != nil {
		t.Fatal(err)
	}
}

func TestSessionImageUnsupportedClonePlatformWritesWithoutReadingSources(t *testing.T) {
	// The Windows and Linux path: cloning is unsupported, so a candidate is
	// never opened and the bytes in hand are written.
	tried := 0
	stubImageClones(t, func(dir, final, name string, raw []byte, candidate string, durability sessionImageDurability) (sessionImageCloneOutcome, error) {
		tried++
		return cloneUnsupported, errors.New("unsupported")
	})
	dir := filepath.Join(t.TempDir(), sessionImageDirname)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw := testImageBytes(t, 9)
	name := testImageDigest(raw)
	result, err := persistRawSessionImage(dir, name, raw, []string{"/a.png", "/b.png", "/c.png"}, sessionImageDurable)
	if err != nil || result.method != "written" {
		t.Fatalf("%+v %v", result, err)
	}
	if tried != 1 {
		t.Fatalf("an unsupported volume must stop after the first candidate, tried %d", tried)
	}
	if err := verifyRawSessionImageFile(filepath.Join(dir, name+sessionImageRawSuffix), name, int64(len(raw))); err != nil {
		t.Fatal(err)
	}
}

func withSourceGuard(t testing.TB, guard sessionImageSourceGuard) {
	t.Helper()
	previous := sessionImageSources
	sessionImageSources = func() sessionImageSourceGuard { return guard }
	t.Cleanup(func() { sessionImageSources = previous })
}

func TestSessionImageSourceGuardRefusesProtectedLocations(t *testing.T) {
	guard := sessionImageSourceGuard{home: "/Users/me", caseInsensitive: true, mounts: []string{"/Users/me/mnt/nas", "/private/nfs"}}
	refused := []string{
		"/Users/me/Desktop/shot.png",
		"/Users/me/desktop/shot.png", // APFS ignores case
		"/Users/me/Documents/a/b.png",
		"/Users/me/Downloads/x.png",
		"/Users/me/Pictures/Photos Library.photoslibrary/x.jpg",
		"/Users/me/Movies/x.png",
		"/Users/me/Music/x.png",
		"/Users/me/Library/Mobile Documents/com~apple~CloudDocs/x.png",
		"/Users/me/Library/Containers/com.apple.Notes/x.png",
		"/Users/me/Library/Group Containers/group.x/x.png",
		"/Users/me/Library/Application Support/Other/x.png",
		"/Users/me/.Trash/x.png",
		"/Users/me/Desktop",
		"/Volumes/External/x.png",
		"/volumes/External/x.png",
		"/System/Volumes/Data/Users/me/Desktop/x.png", // the firmlinked data volume
		"/net/host/x.png",
		"/Network/Servers/x.png",
		"/Users/other/Workspace/x.png", // another user's home
		"/Users/me/mnt/nas/x.png",      // a non-local mount
		"/private/nfs/share/x.png",
		"/Users/me/Workspace/../Desktop/x.png", // lexical escape
		"relative/x.png",
		"",
	}
	for _, path := range refused {
		if err := guard.check(path); !errors.Is(err, errSessionImageSourceProtected) {
			t.Errorf("%q must be refused, got %v", path, err)
		}
	}
	allowed := []string{
		"/Users/me/Workspace/lagspace/tmp/label/001.png",
		"/Users/me/Desktopper/x.png", // a different folder that only starts alike
		"/Users/me/.dev/shots/x.png",
		"/tmp/dlss/shot.jpg",
		"/private/tmp/wk-pip/x.png",
		"/private/var/folders/rf/abc/T/x.png",
		"/opt/data/x.png",
	}
	for _, path := range allowed {
		if err := guard.check(path); err != nil {
			t.Errorf("%q must be allowed, got %v", path, err)
		}
	}
	if err := (sessionImageSourceGuard{}).check("/tmp/x.png"); err == nil {
		t.Error("without a known home nothing is classifiable, so nothing is read")
	}
	caseSensitive := sessionImageSourceGuard{home: "/home/me"}
	if err := caseSensitive.check("/home/me/desktop/x.png"); err != nil {
		t.Errorf("on a case-sensitive volume desktop is not Desktop: %v", err)
	}
}

func TestSessionImageSourceResolveRefusesSymlinkIntoProtectedBeforeTouchingIt(t *testing.T) {
	home := t.TempDir()
	guard := sessionImageSourceGuard{home: home, caseInsensitive: runtime.GOOS == "darwin"}
	desktop := filepath.Join(home, "Desktop")
	work := filepath.Join(home, "Workspace")
	for _, dir := range []string{desktop, work} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(desktop, "x.png"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(desktop, filepath.Join(work, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "ok.png"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	var touched []string
	lstat := func(path string) (os.FileInfo, error) {
		touched = append(touched, path)
		return os.Lstat(path)
	}
	if _, err := guard.resolve(filepath.Join(work, "link", "x.png"), lstat, os.Readlink); !errors.Is(err, errSessionImageSourceProtected) {
		t.Fatalf("symlink into Desktop must be refused, got %v", err)
	}
	for _, path := range touched {
		if strings.HasPrefix(strings.ToLower(path), strings.ToLower(desktop)) {
			t.Fatalf("the resolver looked inside the protected folder: %s", path)
		}
	}
	resolved, err := guard.resolve(filepath.Join(work, "ok.png"), os.Lstat, os.Readlink)
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := filepath.EvalSymlinks(filepath.Join(work, "ok.png")); resolved != want {
		t.Fatalf("resolved %s, want %s", resolved, want)
	}
}

func TestSessionImageCloneOfProtectedCandidateNeverReadsIt(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("APFS clones")
	}
	home := t.TempDir()
	withSourceGuard(t, sessionImageSourceGuard{home: home, caseInsensitive: true})
	desktop := filepath.Join(home, "Desktop")
	if err := os.MkdirAll(desktop, 0o755); err != nil {
		t.Fatal(err)
	}
	raw := testImageBytes(t, 10)
	source := filepath.Join(desktop, "shot.png")
	if err := os.WriteFile(source, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	// Unreadable: a read attempt would report "unavailable", not "protected".
	if err := os.Chmod(desktop, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(desktop, 0o755) })
	dir := filepath.Join(t.TempDir(), sessionImageDirname)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	name := testImageDigest(raw)
	result, err := persistRawSessionImage(dir, name, raw, []string{source}, sessionImageDurable)
	if err != nil {
		t.Fatal(err)
	}
	if result.method != "written" || len(result.attempts) != 1 || result.attempts[0] != cloneProtected {
		t.Fatalf("a protected candidate must be refused from its path and the bytes written: %+v", result)
	}
}
