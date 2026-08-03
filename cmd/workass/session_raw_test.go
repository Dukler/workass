package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"workass/internal/acp"
	"workass/internal/wire"
)

func TestSessionRawResultMatchesLegacyShapeAndInlineImages(t *testing.T) {
	stateDir := t.TempDir()
	store := newSessionStore(filepath.Join(stateDir, sessionStateFilename))
	snapshot := sessionMirrorFixture("raw-tab", "raw-chat", "show raw image")
	assistant := sessionAssistant(t, snapshot, "raw-tab")
	assistant["status"] = "done"
	data := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("raw-wire-image", 2048)))
	image := refNativeImage(data)
	assistant["images"] = []any{cloneJSON(image)}
	assistant["events"] = []any{map[string]any{
		"kind": "tool", "id": "raw-tool",
		"images": []any{cloneJSON(image)},
	}}
	if !store.Save(snapshot) {
		t.Fatal("save raw session fixture")
	}

	legacy, err := json.Marshal(store.GetWithLiveSessions(nil))
	if err != nil {
		t.Fatal(err)
	}
	raw := store.GetRawWithLiveSessions(nil)
	if !bytes.Equal(raw, legacy) {
		t.Fatalf("raw session result differs from legacy hydration\nlegacy=%s\nraw=%s", legacy, raw)
	}
	decoded := decodeSessionSnapshotForTest(t, raw)
	gotAssistant := sessionAssistant(t, decoded, "raw-tab")
	gotImages := anySlice(gotAssistant["images"])
	gotEventImages := anySlice(mapFromAnyMain(anySlice(gotAssistant["events"])[0])["images"])
	if len(gotImages) != 1 || len(gotEventImages) != 1 ||
		fieldString(mapFromAnyMain(gotImages[0]), "data") != data ||
		fieldString(mapFromAnyMain(gotEventImages[0]), "data") != data {
		t.Fatalf("raw inline image materialization = message:%#v event:%#v", gotImages, gotEventImages)
	}
}

func TestSessionRawResultMissingImageDegradesOneThumbnail(t *testing.T) {
	stateDir := t.TempDir()
	store := newSessionStore(filepath.Join(stateDir, sessionStateFilename))
	snapshot := sessionMirrorFixture("raw-missing-tab", "raw-missing-chat", "missing raw image")
	user := mapFromAnyMain(messageSlice(chatFromSnapshot(snapshot, "raw-missing-tab"))[0])
	user["images"] = []any{refNativeImage(base64.StdEncoding.EncodeToString([]byte("missing raw thumbnail")))}
	if !store.Save(snapshot) {
		t.Fatal("save missing-image fixture")
	}
	files, err := os.ReadDir(filepath.Join(stateDir, sessionImageDirname))
	if err != nil || len(files) != 1 {
		t.Fatalf("image files=%d err=%v", len(files), err)
	}
	if err := os.Remove(filepath.Join(stateDir, sessionImageDirname, files[0].Name())); err != nil {
		t.Fatal(err)
	}

	raw := store.GetRawWithLiveSessions(nil)
	decoded := decodeSessionSnapshotForTest(t, raw)
	gotUser := mapFromAnyMain(messageSlice(chatFromSnapshot(decoded, "raw-missing-tab"))[0])
	images := anySlice(gotUser["images"])
	if len(images) != 1 {
		t.Fatalf("missing image produced %d thumbnails, want one", len(images))
	}
	image := mapFromAnyMain(images[0])
	if fieldString(image, "data") != "" || fieldString(image, sessionImageDataRefField) != "" {
		t.Fatalf("missing image remained usable: %#v", image)
	}
	if err := store.LoadError(); err == nil || !strings.Contains(err.Error(), "session image ref") {
		t.Fatalf("missing image warning = %v", err)
	}
}

func TestSessionRawResultPreservesLiveSessionOverlay(t *testing.T) {
	root := repoRoot(t)
	stateDir := t.TempDir()
	store := newSessionStore(filepath.Join(stateDir, sessionStateFilename))
	snapshot := sessionMirrorFixture("raw-live-tab", "raw-live-chat", "live overlay")
	if !store.Save(snapshot) {
		t.Fatal("save live overlay fixture")
	}
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: filepath.Join(stateDir, "acp"), RuntimeProfile: "dev",
		Provider: acp.ProviderConfig{
			ID: "mock", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")},
			CWD: root, Enabled: true, Label: "Workass Mock ACP",
		},
		DefaultProviderID: "mock", RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session, err := manager.NewSession(ctx, acp.SessionOptions{
		TabID: "raw-live-tab", ChatID: "raw-live-chat", ProviderID: "mock", CWD: root,
	})
	if err != nil {
		t.Fatalf("new live session: %v", err)
	}

	legacy, err := json.Marshal(store.GetWithLiveSessions(manager))
	if err != nil {
		t.Fatal(err)
	}
	raw := store.GetRawWithLiveSessions(manager)
	if !bytes.Equal(raw, legacy) {
		t.Fatalf("raw liveSession overlay differs\nlegacy=%s\nraw=%s", legacy, raw)
	}
	decoded := decodeSessionSnapshotForTest(t, raw)
	live := mapFromAnyMain(chatFromSnapshot(decoded, "raw-live-tab")["liveSession"])
	if fieldString(live, "sessionId") != session.SessionID || fieldString(live, "providerId") != "mock" {
		t.Fatalf("raw liveSession overlay = %#v", live)
	}
}

func TestSessionRawResultHandlerReturnsPreSerializedWireValue(t *testing.T) {
	store := newSessionStore(filepath.Join(t.TempDir(), sessionStateFilename))
	if !store.Save(sessionMirrorFixture("raw-handler-tab", "raw-handler-chat", "handler")) {
		t.Fatal("save handler fixture")
	}
	hub := wire.NewHub()
	registerSessionHandlers(hub, store)
	result, err := hub.Invoke("session:get", nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, ok := result.(wire.RawResult)
	if !ok || !json.Valid(raw) {
		t.Fatalf("session:get result = %T valid=%v", result, ok && json.Valid(raw))
	}
}

func TestSessionRawResultKeepsIngressRedaction(t *testing.T) {
	store := newSessionStore(filepath.Join(t.TempDir(), sessionStateFilename))
	snapshot := sessionMirrorFixture("raw-secret-tab", "raw-secret-chat", "secret")
	chat := chatFromSnapshot(snapshot, "raw-secret-tab")
	chat["credential"] = "must-not-leave"
	sessionAssistant(t, snapshot, "raw-secret-tab")["content"] = "bearer must-not-leave"
	if !store.Save(snapshot) {
		t.Fatal("save redaction fixture")
	}
	raw := store.GetRawWithLiveSessions(nil)
	if bytes.Contains(raw, []byte("must-not-leave")) || !bytes.Contains(bytes.ToLower(raw), []byte("[redacted]")) {
		t.Fatalf("raw result violated ingress redaction: %s", raw)
	}
}

func TestSessionRawResultMatchesLegacyRealFixture(t *testing.T) {
	path := os.Getenv("WORKASS_COST_SNAPSHOT")
	if path == "" {
		t.Skip("set WORKASS_COST_SNAPSHOT to an isolated real session-state.json copy")
	}
	store := newSessionStore(path)
	if err := store.LoadError(); err != nil {
		t.Fatalf("load real fixture: %v", err)
	}
	raw := store.GetRawWithLiveSessions(nil)
	legacy, err := json.Marshal(store.GetWithLiveSessions(nil))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, legacy) {
		t.Fatalf("real raw result differs from legacy: rawBytes=%d legacyBytes=%d", len(raw), len(legacy))
	}
	t.Logf("real raw result byte-identical bytes=%d", len(raw))
}

func TestSessionRawResultRealCostOnly(t *testing.T) {
	path := os.Getenv("WORKASS_COST_SNAPSHOT")
	if path == "" {
		t.Skip("set WORKASS_COST_SNAPSHOT to an isolated real session-state.json copy")
	}
	rssBeforeLoad := currentRSSBytes()
	store := newSessionStore(path)
	if err := store.LoadError(); err != nil {
		t.Fatalf("load real fixture: %v", err)
	}
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	started := time.Now()
	raw := store.GetRawWithLiveSessions(nil)
	elapsed := time.Since(started)
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc
	rss := currentRSSBytes()
	t.Logf("rawBytes=%d allocatedBytes=%d rss=%d elapsedMs=%.3f lock=%#v",
		len(raw), allocated, rss, float64(elapsed.Microseconds())/1000, store.getLock.snapshot())
	if allocated > 48_411_912 {
		t.Fatalf("raw allocated %d bytes, target <= 48411912", allocated)
	}
	if rssBeforeLoad >= 0 && rssBeforeLoad < 200<<20 && rss > 256<<20 {
		t.Fatalf("raw RSS %d bytes, target <= 256 MiB", rss)
	}
	runtime.KeepAlive(raw)
}

// The production daemon died twice on this: sessionWireCapacity's fast path
// sizes the wire buffer from the previous read plus a fixed margin, so a session
// that grows past that margin between two reads made appendImageSource reslice
// beyond cap. That panic is not local — it takes the daemon down and every
// chat's ACP engine with it, mid-turn.
func TestSessionRawResultSurvivesShortCapacityHint(t *testing.T) {
	stateDir := t.TempDir()
	store := newSessionStore(filepath.Join(stateDir, sessionStateFilename))
	snapshot := sessionMirrorFixture("short-cap-tab", "short-cap-chat", "grew past the margin")
	assistant := sessionAssistant(t, snapshot, "short-cap-tab")
	assistant["status"] = "done"
	data := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("short-capacity-image", 160<<10)))
	assistant["images"] = []any{refNativeImage(data)}
	if !store.Save(snapshot) {
		t.Fatal("save short-capacity fixture")
	}

	legacy, err := json.Marshal(store.GetWithLiveSessions(nil))
	if err != nil {
		t.Fatal(err)
	}

	// Stand in for "the session grew by more than sessionWireCapacityMargin since
	// the last read" without paying a multi-megabyte fixture for it: the hint is
	// short either way, and short is the only precondition that matters.
	store.wireByteEstimate.Store(1)
	if estimate := store.sessionWireCapacity(collectSessionWireRefs(store.publishedGeneration().root), nil); estimate >= len(legacy) {
		t.Fatalf("precondition lost: hint %d already covers the %d-byte result", estimate, len(legacy))
	}

	raw := store.GetRawWithLiveSessions(nil)
	if !bytes.Equal(raw, legacy) {
		t.Fatalf("short capacity hint changed the result\nlegacy=%d bytes\nraw=%d bytes", len(legacy), len(raw))
	}
	decoded := decodeSessionSnapshotForTest(t, raw)
	images := anySlice(sessionAssistant(t, decoded, "short-cap-tab")["images"])
	if len(images) != 1 || fieldString(mapFromAnyMain(images[0]), "data") != data {
		t.Fatalf("image did not survive the grown buffer: %#v", images)
	}
	if err := store.LoadError(); err != nil {
		t.Fatalf("short capacity hint recorded a degradation: %v", err)
	}
}

// The single-allocation property is what this encoder is for: a session holding
// megabytes of image sidecars is read constantly, and copying it around is the
// slowness the ref-native path was built to remove. So the capacity has to cover
// the result, not usually cover it — an estimate that undershoots forces a
// mid-encode grow, which is exactly the multi-megabyte copy being avoided.
func TestSessionWireCapacityCoversASessionThatGainedAnImage(t *testing.T) {
	stateDir := t.TempDir()
	store := newSessionStore(filepath.Join(stateDir, sessionStateFilename))
	snapshot := sessionMirrorFixture("grow-tab", "grow-chat", "gained a screenshot")
	assistant := sessionAssistant(t, snapshot, "grow-tab")
	assistant["status"] = "done"
	assistant["images"] = []any{refNativeImage(base64.StdEncoding.EncodeToString(
		[]byte(strings.Repeat("first-read-image", 1024))))}
	if !store.Save(snapshot) {
		t.Fatal("save first-read fixture")
	}

	first := store.GetRawWithLiveSessions(nil)
	if len(first) == 0 {
		t.Fatal("first read produced nothing")
	}

	// One screenshot arrives, larger than sessionWireCapacityMargin. This is the
	// production sequence: read, image lands, read again.
	assistant = sessionAssistant(t, snapshot, "grow-tab")
	assistant["images"] = append(anySlice(assistant["images"]), refNativeImage(
		base64.StdEncoding.EncodeToString([]byte(strings.Repeat("pasted-screenshot", 160<<10)))))
	if !store.Save(snapshot) {
		t.Fatal("save grown fixture")
	}

	generation := store.publishedGeneration()
	summary := collectSessionWireRefs(generation.root)
	sources := make(map[string]*sessionWireImageSource, len(summary.counts))
	defer closeSessionWireSources(sources)
	prepareSessionWireSources(sources, summary.counts, stateDir)
	capacity := store.sessionWireCapacity(summary, sources)

	second := store.GetRawWithLiveSessions(nil)
	if len(second) <= len(first)+sessionWireCapacityMargin {
		t.Fatalf("precondition lost: session grew %d bytes, want more than the %d margin",
			len(second)-len(first), sessionWireCapacityMargin)
	}
	if capacity < len(second) {
		t.Fatalf("capacity %d does not cover the %d-byte result: the encode had to grow mid-flight",
			capacity, len(second))
	}
}
