package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"workass/internal/acp"
	"workass/internal/artifacthost"
	"workass/internal/chat"
	"workass/internal/wire"
)

func TestVisualizeHostCapturesAllowedExecutorHTML(t *testing.T) {
	stateDir := t.TempDir()
	visualRoot := filepath.Join(filepath.Dir(stateDir), "visualizations", "turn-1")
	if err := os.MkdirAll(visualRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(visualRoot, "signals.html")
	if err := os.WriteFile(source, []byte(`<div id="chart">hello</div><script>document.title = "chart"</script>`), 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := artifacthost.New(stateDir, "http://127.0.0.1:8788")
	if err != nil {
		t.Fatal(err)
	}
	store := newSessionStore(filepath.Join(stateDir, sessionStateFilename))
	manager := acp.NewManager(acp.Options{StateDir: stateDir, RSSSampleInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })
	runtime := newProviderChatRuntime(manager, store, stateDir)
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	const tabID, chatID = "visual-tab", "visual-chat"
	if _, err := runtime.actorForNewChat(chatID, chat.PresentationState{TabID: tabID, Title: "Visualization"}); err != nil {
		t.Fatal(err)
	}
	hub := wire.NewHub()
	registerVisualizeHandler(hub, registry, runtime, stateDir)

	result, err := hub.Invoke("visualize:host", []any{map[string]any{
		"tabId": tabID, "chatId": chatID, "path": source, "mode": "wide", "title": "Signals",
	}})
	if err != nil {
		t.Fatal(err)
	}
	receipt, ok := result.(map[string]any)
	if !ok || fieldString(receipt, "urlPath") == "" || fieldString(receipt, "mode") != "wide" {
		t.Fatalf("visualization receipt = %#v", result)
	}

	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, fieldString(receipt, "urlPath")+fieldString(receipt, "entry"), nil)
	response := httptest.NewRecorder()
	registry.ServeHTTP(response, request)
	body := response.Body.String()
	if response.Code != http.StatusOK || !strings.Contains(body, "Content-Security-Policy") || !strings.Contains(body, "id=\"chart\"") {
		t.Fatalf("captured visualization status=%d body=%q", response.Code, body)
	}

	outside := filepath.Join(t.TempDir(), "outside.html")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := hub.Invoke("visualize:host", []any{map[string]any{
		"tabId": tabID, "chatId": chatID, "path": outside,
	}}); err == nil || !strings.Contains(err.Error(), "inside Workass visualizations") {
		t.Fatalf("outside visualization was accepted: %v", err)
	}
}

func TestResolveVisualizationPathAllowsOnlyExactWorkspaceSibling(t *testing.T) {
	parent := t.TempDir()
	workspace := filepath.Join(parent, "workass")
	visualRoot := workspace + "-visualizations"
	if err := os.MkdirAll(visualRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(visualRoot, "settings.html")
	if err := os.WriteFile(source, []byte("<div>quiet list</div>"), 0o600); err != nil {
		t.Fatal(err)
	}
	wantSource, err := filepath.EvalSymlinks(source)
	if err != nil {
		t.Fatal(err)
	}
	if resolved, err := resolveVisualizationPathForWorkspace(source, t.TempDir(), workspace); err != nil || resolved != wantSource {
		t.Fatalf("exact workspace sibling: resolved=%q err=%v", resolved, err)
	}

	otherRoot := filepath.Join(parent, "other-visualizations")
	if err := os.MkdirAll(otherRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(otherRoot, "settings.html")
	if err := os.WriteFile(other, []byte("<div>other</div>"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveVisualizationPathForWorkspace(other, t.TempDir(), workspace); err == nil || !strings.Contains(err.Error(), "inside Workass visualizations") {
		t.Fatalf("unrelated sibling was accepted: %v", err)
	}

	out := filepath.Join(parent, "outside.html")
	if err := os.WriteFile(out, []byte("<div>outside</div>"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(visualRoot, "escape.html")
	if err := os.Symlink(out, link); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveVisualizationPathForWorkspace(link, t.TempDir(), workspace); err == nil || !strings.Contains(err.Error(), "inside Workass visualizations") {
		t.Fatalf("workspace sibling symlink escape was accepted: %v", err)
	}
}

type visualizeTestHarness struct {
	stateDir string
	source   string
	tabID    string
	chatID   string
	registry *artifacthost.Registry
	manager  *acp.Manager
	runtime  *providerChatRuntime
	hub      *wire.Hub
}

func newVisualizeTestHarness(t *testing.T, fragment string) *visualizeTestHarness {
	t.Helper()
	stateDir := t.TempDir()
	visualRoot := filepath.Join(filepath.Dir(stateDir), "visualizations", "turn-1")
	if err := os.MkdirAll(visualRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(visualRoot, "signals.html")
	if err := os.WriteFile(source, []byte(fragment), 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := artifacthost.New(stateDir, "http://127.0.0.1:8788")
	if err != nil {
		t.Fatal(err)
	}
	store := newSessionStore(filepath.Join(stateDir, sessionStateFilename))
	manager := acp.NewManager(acp.Options{StateDir: stateDir, RSSSampleInterval: time.Hour})
	runtime := newProviderChatRuntime(manager, store, stateDir)
	t.Cleanup(func() {
		_ = runtime.Close(context.Background())
		manager.Reset()
	})
	const tabID, chatID = "visual-tab", "visual-chat"
	if _, err := runtime.actorForNewChat(chatID, chat.PresentationState{TabID: tabID, Title: "Visualization"}); err != nil {
		t.Fatal(err)
	}
	hub := wire.NewHub()
	registerVisualizeHandler(hub, registry, runtime, stateDir)
	return &visualizeTestHarness{stateDir: stateDir, source: source, tabID: tabID, chatID: chatID, registry: registry, manager: manager, runtime: runtime, hub: hub}
}

func (h *visualizeTestHarness) invoke(t *testing.T, overrides map[string]any) (any, error) {
	t.Helper()
	arg := map[string]any{"tabId": h.tabID, "chatId": h.chatID, "path": h.source, "mode": "wide", "title": "Signals"}
	for key, value := range overrides {
		arg[key] = value
	}
	return h.hub.Invoke("visualize:host", []any{arg})
}

func visualizeRegistryBytes(t *testing.T, stateDir string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(stateDir, "artifact-hosts.json"))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func visualizeRegistryCount(t *testing.T, stateDir string) int {
	t.Helper()
	var stored struct {
		Artifacts []json.RawMessage `json:"artifacts"`
		Hosts     []json.RawMessage `json:"hosts"`
	}
	if err := json.Unmarshal(visualizeRegistryBytes(t, stateDir), &stored); err != nil {
		t.Fatal(err)
	}
	if len(stored.Artifacts) > 0 {
		return len(stored.Artifacts)
	}
	return len(stored.Hosts)
}

func restartVisualizeHarness(t *testing.T, h *visualizeTestHarness) {
	t.Helper()
	if err := h.runtime.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.manager.Reset()

	registry, err := artifacthost.New(h.stateDir, "http://127.0.0.1:8788")
	if err != nil {
		t.Fatal(err)
	}
	manager := acp.NewManager(acp.Options{StateDir: h.stateDir, RSSSampleInterval: time.Hour})
	runtime := newProviderChatRuntime(manager, newSessionStore(filepath.Join(h.stateDir, sessionStateFilename)), h.stateDir)
	if err := runtime.StartupError(); err != nil {
		manager.Reset()
		t.Fatalf("restart provider chat runtime: %v", err)
	}
	hub := wire.NewHub()
	registerVisualizeHandler(hub, registry, runtime, h.stateDir)
	h.registry, h.manager, h.runtime, h.hub = registry, manager, runtime, hub
	t.Cleanup(func() {
		_ = runtime.Close(context.Background())
		manager.Reset()
	})
}

func TestVisualizeHostFencesStaleAndDeletedActorBeforeCapture(t *testing.T) {
	h := newVisualizeTestHarness(t, `<div id="chart">secret-free</div>`)
	if _, err := h.hub.Invoke("visualize:host", []any{map[string]any{
		"tabId": "stale-tab", "chatId": h.chatID, "path": h.source,
	}}); err == nil || !strings.Contains(err.Error(), "tab") {
		t.Fatalf("stale visualize request was accepted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.stateDir, "artifact-hosts.json")); !os.IsNotExist(err) {
		t.Fatalf("stale request changed the registry: err=%v", err)
	}

	actor, _, err := h.runtime.exactActor(h.tabID, h.chatID)
	if err != nil {
		t.Fatal(err)
	}
	actor.mu.Lock()
	err = actor.engine.Apply(chat.DeleteChat{OperationID: "visual-delete", Force: true})
	actor.mu.Unlock()
	if err != nil {
		t.Fatalf("tombstone actor: %v", err)
	}
	if _, err := h.invoke(t, nil); err == nil || !strings.Contains(err.Error(), "deleted") {
		t.Fatalf("deleted visualize request was accepted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.stateDir, "artifact-hosts.json")); !os.IsNotExist(err) {
		t.Fatalf("deleted request changed the registry: err=%v", err)
	}
}

func TestVisualizeHostLostReplyReadsActorReceiptWithoutSource(t *testing.T) {
	h := newVisualizeTestHarness(t, `<div id="chart">lost-reply</div>`)
	first, err := h.invoke(t, nil)
	if err != nil {
		t.Fatal(err)
	}
	firstReceipt, ok := first.(map[string]any)
	if !ok {
		t.Fatalf("first visualization receipt = %#v", first)
	}
	if err := os.Remove(h.source); err != nil {
		t.Fatal(err)
	}
	second, err := h.invoke(t, nil)
	if err != nil {
		t.Fatalf("lost-reply retry: %v", err)
	}
	secondReceipt, ok := second.(map[string]any)
	if !ok {
		t.Fatalf("second visualization receipt = %#v", second)
	}
	for _, key := range []string{"id", "urlPath", "localUrl", "markdown", "createdAt", "updatedAt", "mode", "title"} {
		if firstReceipt[key] != secondReceipt[key] {
			t.Fatalf("lost-reply field %q changed: first=%#v second=%#v", key, firstReceipt[key], secondReceipt[key])
		}
	}
	state, ok := h.runtime.Snapshot(h.chatID)
	if !ok {
		t.Fatal("visualization actor disappeared")
	}
	var external int
	for _, entry := range state.Outbox {
		if entry.Kind == chat.EffectExternalMutation {
			external++
			if entry.Status != chat.OutboxCompleted {
				t.Fatalf("lost-reply operation status = %q", entry.Status)
			}
		}
	}
	if external != 1 {
		t.Fatalf("lost-reply external operation count = %d, want 1", external)
	}
	raw, err := os.ReadFile(providerChatStatePath(h.stateDir, h.chatID))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), h.source) || strings.Contains(string(raw), "lost-reply") {
		t.Fatalf("actor state retained raw visualization request: %s", raw)
	}
}

func TestVisualizeHostCompletedRetryAfterRuntimeRestart(t *testing.T) {
	h := newVisualizeTestHarness(t, `<div>restart-receipt</div>`)
	first, err := h.invoke(t, nil)
	if err != nil {
		t.Fatal(err)
	}
	firstReceipt, ok := first.(map[string]any)
	if !ok {
		t.Fatalf("first visualization receipt = %#v", first)
	}
	if err := os.Remove(h.source); err != nil {
		t.Fatal(err)
	}
	restartVisualizeHarness(t, h)

	second, err := h.invoke(t, nil)
	if err != nil {
		t.Fatalf("completed retry after restart: %v", err)
	}
	secondReceipt, ok := second.(map[string]any)
	if !ok {
		t.Fatalf("second visualization receipt = %#v", second)
	}
	if !reflect.DeepEqual(firstReceipt, secondReceipt) {
		t.Fatalf("restart visualization receipt changed: first=%#v second=%#v", firstReceipt, secondReceipt)
	}
	for _, key := range []string{"id", "urlPath", "markdown", "createdAt", "updatedAt", "mode", "title"} {
		if firstReceipt[key] != secondReceipt[key] {
			t.Fatalf("restart receipt field %q changed: first=%#v second=%#v", key, firstReceipt[key], secondReceipt[key])
		}
	}
	if got := visualizeRegistryCount(t, h.stateDir); got != 1 {
		t.Fatalf("restart retry artifact count = %d, want 1", got)
	}
	state, ok := h.runtime.Snapshot(h.chatID)
	if !ok {
		t.Fatal("restarted visualization actor disappeared")
	}
	var external int
	for _, entry := range state.Outbox {
		if entry.Kind == chat.EffectExternalMutation {
			external++
			if entry.Status != chat.OutboxCompleted {
				t.Fatalf("restart retry operation status = %q", entry.Status)
			}
		}
	}
	if external != 1 {
		t.Fatalf("restart retry external operation count = %d, want 1", external)
	}
}

func TestVisualizeHostRejectsChangedRequestForStableOperation(t *testing.T) {
	h := newVisualizeTestHarness(t, `<div>changed-request</div>`)
	const operationID = "visualize-stable-operation"
	if _, err := h.invoke(t, map[string]any{"operationId": operationID}); err != nil {
		t.Fatal(err)
	}
	before := visualizeRegistryBytes(t, h.stateDir)
	if _, err := h.invoke(t, map[string]any{"operationId": operationID, "title": "Changed title"}); err == nil || !strings.Contains(err.Error(), "reused") {
		t.Fatalf("changed visualize request was accepted: %v", err)
	}
	if after := visualizeRegistryBytes(t, h.stateDir); !bytes.Equal(before, after) {
		t.Fatal("changed visualize request mutated the registry")
	}
}

func TestVisualizeHostReturnsFailedActorTerminalState(t *testing.T) {
	h := newVisualizeTestHarness(t, `<div>failed-registration</div>`)
	arg := map[string]any{"tabId": h.tabID, "chatId": h.chatID, "path": h.source, "mode": "wide", "title": "Signals", "operationId": "visualize-failed"}
	identity := visualizeRequestIdentityDigest(h.tabID, h.chatID, h.source, "wide", "Signals")
	capture, err := prepareVisualizeCapture(h.source, h.stateDir, "wide", "Signals", identity)
	if err != nil {
		t.Fatal(err)
	}
	operationID, err := visualizeOperationID(arg, identity)
	if err != nil {
		t.Fatal(err)
	}
	actor, _, err := h.runtime.exactActor(h.tabID, h.chatID)
	if err != nil {
		t.Fatal(err)
	}
	actor.mu.Lock()
	if err := actor.engine.Apply(chat.RecordExternalMutation{OperationID: operationID, Kind: visualizeMutationKind, Method: capture.method, TabID: h.tabID, Digest: capture.digest}); err != nil {
		actor.mu.Unlock()
		t.Fatal(err)
	}
	entry, ok := externalBrowserMutationEntry(actor.engine.Snapshot(), operationID)
	if !ok {
		actor.mu.Unlock()
		t.Fatal("visualization mutation was not journaled")
	}
	if _, ok, err := actor.engine.ClaimEffect(entry.ID); err != nil || !ok {
		actor.mu.Unlock()
		t.Fatalf("claim visualization mutation: ok=%v err=%v", ok, err)
	}
	if err := actor.engine.Apply(chat.ExternalMutationReceipt{
		OperationID: operationID, Kind: visualizeMutationKind, Method: capture.method,
		TabID: h.tabID, Digest: capture.digest, Failed: true,
	}); err != nil {
		actor.mu.Unlock()
		t.Fatal(err)
	}
	actor.mu.Unlock()
	if err := os.Remove(h.source); err != nil {
		t.Fatal(err)
	}
	if _, err := h.hub.Invoke("visualize:host", []any{arg}); err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("failed visualization did not return actor failure: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.stateDir, "artifact-hosts.json")); !os.IsNotExist(err) {
		t.Fatalf("failed visualization changed the registry: err=%v", err)
	}
}

func TestVisualizeHostSamePathChangedContentGetsNewFallbackOperation(t *testing.T) {
	h := newVisualizeTestHarness(t, `<div>content-version-one</div>`)
	first, err := h.invoke(t, nil)
	if err != nil {
		t.Fatal(err)
	}
	firstReceipt, ok := first.(map[string]any)
	if !ok {
		t.Fatalf("first visualization receipt = %#v", first)
	}
	if err := os.WriteFile(h.source, []byte(`<div>content-version-two</div>`), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := h.invoke(t, nil)
	if err != nil {
		t.Fatalf("same-path changed-content host: %v", err)
	}
	secondReceipt, ok := second.(map[string]any)
	if !ok {
		t.Fatalf("second visualization receipt = %#v", second)
	}
	if firstReceipt["id"] == secondReceipt["id"] || firstReceipt["urlPath"] == secondReceipt["urlPath"] {
		t.Fatalf("changed content reused the first registration: first=%#v second=%#v", firstReceipt, secondReceipt)
	}
	if got := visualizeRegistryCount(t, h.stateDir); got != 2 {
		t.Fatalf("same-path changed-content artifact count = %d, want 2", got)
	}
	state, ok := h.runtime.Snapshot(h.chatID)
	if !ok {
		t.Fatal("visualization actor disappeared")
	}
	var external int
	for _, entry := range state.Outbox {
		if entry.Kind == chat.EffectExternalMutation {
			external++
			if entry.Status != chat.OutboxCompleted {
				t.Fatalf("changed-content operation status = %q", entry.Status)
			}
		}
	}
	if external != 2 {
		t.Fatalf("same-path changed-content external operation count = %d, want 2", external)
	}
}

func TestVisualizeHostRecoversCrashAfterRegistryEffectWithoutDuplicate(t *testing.T) {
	h := newVisualizeTestHarness(t, `<div>crash-after-effect</div>`)
	arg := map[string]any{"tabId": h.tabID, "chatId": h.chatID, "path": h.source, "mode": "wide", "title": "Signals", "operationId": "visualize-crash"}
	identity := visualizeRequestIdentityDigest(h.tabID, h.chatID, h.source, "wide", "Signals")
	capture, err := prepareVisualizeCapture(h.source, h.stateDir, "wide", "Signals", identity)
	if err != nil {
		t.Fatal(err)
	}
	operationID, err := visualizeOperationID(arg, identity)
	if err != nil {
		t.Fatal(err)
	}
	actor, _, err := h.runtime.exactActor(h.tabID, h.chatID)
	if err != nil {
		t.Fatal(err)
	}
	actor.mu.Lock()
	if err := actor.engine.Apply(chat.RecordExternalMutation{OperationID: operationID, Kind: visualizeMutationKind, Method: capture.method, TabID: h.tabID, Digest: capture.digest}); err != nil {
		actor.mu.Unlock()
		t.Fatal(err)
	}
	entry, ok := externalBrowserMutationEntry(actor.engine.Snapshot(), operationID)
	if !ok {
		actor.mu.Unlock()
		t.Fatal("visualization mutation was not journaled")
	}
	if _, ok, err := actor.engine.ClaimEffect(entry.ID); err != nil || !ok {
		actor.mu.Unlock()
		t.Fatalf("claim visualization mutation: ok=%v err=%v", ok, err)
	}
	actor.mu.Unlock()
	if _, err := h.registry.RegisterCapturedHTMLForOperation("Signals", capture.wrapped, string(operationID), capture.digest); err != nil {
		t.Fatalf("simulate external capture: %v", err)
	}
	before := visualizeRegistryBytes(t, h.stateDir)
	if _, err := h.hub.Invoke("visualize:host", []any{arg}); err != nil {
		t.Fatalf("crash readback: %v", err)
	}
	if after := visualizeRegistryBytes(t, h.stateDir); !bytes.Equal(before, after) {
		t.Fatal("crash readback registered the same visualization again")
	}
	state, _ := h.runtime.Snapshot(h.chatID)
	entry, ok = externalBrowserMutationEntry(state, operationID)
	if !ok || entry.Status != chat.OutboxCompleted {
		t.Fatalf("crash readback did not commit completion: ok=%v entry=%#v", ok, entry)
	}
}

func TestVisualizeHostFailsClosedForAmbiguousDispatchedMutation(t *testing.T) {
	h := newVisualizeTestHarness(t, `<div>ambiguous-effect</div>`)
	arg := map[string]any{"tabId": h.tabID, "chatId": h.chatID, "path": h.source, "mode": "wide", "title": "Signals", "operationId": "visualize-ambiguous"}
	identity := visualizeRequestIdentityDigest(h.tabID, h.chatID, h.source, "wide", "Signals")
	capture, err := prepareVisualizeCapture(h.source, h.stateDir, "wide", "Signals", identity)
	if err != nil {
		t.Fatal(err)
	}
	operationID, err := visualizeOperationID(arg, identity)
	if err != nil {
		t.Fatal(err)
	}
	actor, _, err := h.runtime.exactActor(h.tabID, h.chatID)
	if err != nil {
		t.Fatal(err)
	}
	actor.mu.Lock()
	if err := actor.engine.Apply(chat.RecordExternalMutation{OperationID: operationID, Kind: visualizeMutationKind, Method: capture.method, TabID: h.tabID, Digest: capture.digest}); err != nil {
		actor.mu.Unlock()
		t.Fatal(err)
	}
	entry, ok := externalBrowserMutationEntry(actor.engine.Snapshot(), operationID)
	if !ok {
		actor.mu.Unlock()
		t.Fatal("visualization mutation was not journaled")
	}
	if _, ok, err := actor.engine.ClaimEffect(entry.ID); err != nil || !ok {
		actor.mu.Unlock()
		t.Fatalf("claim visualization mutation: ok=%v err=%v", ok, err)
	}
	actor.mu.Unlock()
	if _, err := h.hub.Invoke("visualize:host", []any{arg}); err == nil || !strings.Contains(err.Error(), "readback") {
		t.Fatalf("ambiguous visualization mutation was retried or accepted: %v", err)
	}
	state, _ := h.runtime.Snapshot(h.chatID)
	entry, ok = externalBrowserMutationEntry(state, operationID)
	if !ok || entry.Status != chat.OutboxAmbiguous {
		t.Fatalf("ambiguous visualization mutation was not fenced: ok=%v entry=%#v", ok, entry)
	}
	if _, err := os.Stat(filepath.Join(h.stateDir, "artifact-hosts.json")); !os.IsNotExist(err) {
		t.Fatalf("ambiguous visualization mutation created a registry: err=%v", err)
	}
}

func TestVisualizeHostDoesNotDuplicateContentAddressedRegistration(t *testing.T) {
	h := newVisualizeTestHarness(t, `<div>one-registration</div>`)
	if _, err := h.invoke(t, nil); err != nil {
		t.Fatal(err)
	}
	before := visualizeRegistryBytes(t, h.stateDir)
	if got := visualizeRegistryCount(t, h.stateDir); got != 1 {
		t.Fatalf("initial artifact count = %d, want 1", got)
	}
	if _, err := h.invoke(t, nil); err != nil {
		t.Fatalf("duplicate visualize request: %v", err)
	}
	after := visualizeRegistryBytes(t, h.stateDir)
	if !bytes.Equal(before, after) {
		t.Fatal("lost-reply/readback path rewrote the content-addressed registry")
	}
	if got := visualizeRegistryCount(t, h.stateDir); got != 1 {
		t.Fatalf("repeated artifact count = %d, want 1", got)
	}
}
