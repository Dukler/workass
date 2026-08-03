package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"workass/internal/acp"
	"workass/internal/httpserve"
	"workass/internal/lease"
	"workass/internal/wire"
)

func TestAppMetaAdvertisesCapabilityGatedLeanSessionSaves(t *testing.T) {
	t.Setenv("WORKASS_PROFILE", "dev")
	meta := appMeta(t.TempDir())
	if meta["daemon"] != true || meta["profile"] != "dev" || meta["sessionSaveMode"] != "lean-payload-v2" || meta["workspaceRebindMode"] != "transactional-v1" {
		t.Fatalf("app meta session-save capability = %#v", meta)
	}
}

func TestAppMetaDefaultsUnknownRuntimeProfilesToProduction(t *testing.T) {
	t.Setenv("WORKASS_PROFILE", "unexpected")
	if profile := appMeta(t.TempDir())["profile"]; profile != "prod" {
		t.Fatalf("app meta profile = %q, want prod", profile)
	}
}

func TestStoredChatRuntimeControlsHydrateSessionAndJobArgs(t *testing.T) {
	store := newSessionStore(filepath.Join(t.TempDir(), sessionStateFilename))
	created, err := store.AgentCreateChat("Hydrate controls", "/tmp", "claude", "claude-fable-5[1m][xhigh]", "bypassPermissions", true)
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}
	tabID, chatID := fieldString(created, "tabId"), fieldString(created, "chatId")

	sessionArg := map[string]any{"tabId": tabID, "chatId": chatID}
	applyStoredChatRuntimeControls(store, sessionArg)
	sessionOpts := parseSessionOptions(sessionArg)
	if sessionOpts.ProviderID != "claude" || sessionOpts.ModelID != "claude-fable-5[1m][xhigh]" || sessionOpts.ModeID != "bypassPermissions" {
		t.Fatalf("session controls = provider=%q model=%q mode=%q", sessionOpts.ProviderID, sessionOpts.ModelID, sessionOpts.ModeID)
	}

	jobArg := map[string]any{"tabId": tabID, "chatId": chatID, "prompt": "resume"}
	applyStoredChatRuntimeControls(store, jobArg)
	jobOpts := parseJobStartOptions(jobArg)
	if jobOpts.ProviderID != "claude" || jobOpts.ModelID != "claude-fable-5[1m][xhigh]" || jobOpts.ModeID != "bypassPermissions" {
		t.Fatalf("job controls = provider=%q model=%q mode=%q", jobOpts.ProviderID, jobOpts.ModelID, jobOpts.ModeID)
	}

	explicit := map[string]any{"tabId": tabID, "chatId": chatID, "providerId": "codex", "modelId": "gpt-5.6-sol[xhigh]", "modeId": "agent-full-access"}
	applyStoredChatRuntimeControls(store, explicit)
	explicitOpts := parseSessionOptions(explicit)
	if explicitOpts.ProviderID != "codex" || explicitOpts.ModelID != "gpt-5.6-sol[xhigh]" || explicitOpts.ModeID != "agent-full-access" {
		t.Fatalf("explicit controls overwritten = provider=%q model=%q mode=%q", explicitOpts.ProviderID, explicitOpts.ModelID, explicitOpts.ModeID)
	}
}

func TestResolveMocksDirPrecedenceAndExecutableDiscovery(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "repo")
	executable := filepath.Join(cwd, "dist-bin", "workass")
	discovered := filepath.Join(cwd, "desktop", "docs", "mocks")
	if err := os.MkdirAll(discovered, 0o755); err != nil {
		t.Fatal(err)
	}

	flagDir := filepath.Join(root, "flag-mocks")
	envDir := filepath.Join(root, "env-mocks")
	if got := resolveMocksDir(flagDir, envDir, cwd, executable); got != flagDir {
		t.Fatalf("flag mocks dir = %q, want %q", got, flagDir)
	}
	if got := resolveMocksDir("", envDir, cwd, executable); got != envDir {
		t.Fatalf("env mocks dir = %q, want %q", got, envDir)
	}
	if got := resolveMocksDir("", "", cwd, executable); got != discovered {
		t.Fatalf("discovered mocks dir = %q, want %q", got, discovered)
	}
	if got := resolveMocksDir("", "", cwd, filepath.Join(root, "elsewhere", "workass")); got != "" {
		t.Fatalf("missing discovery = %q, want disabled", got)
	}
}

func TestServeDaemonHTTPStopsAcceptingAndRunsCleanup(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "agent-control.json")
	if err := os.WriteFile(marker, []byte("transient"), 0o600); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	cleaned := make(chan struct{})
	cleanup := func() {
		once.Do(func() {
			_ = os.Remove(marker)
			close(cleaned)
		})
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveDaemonHTTP(ctx, server, listener, cleanup) }()

	response, err := http.Get("http://" + listener.Addr().String())
	if err != nil {
		t.Fatalf("server did not accept before shutdown: %v", err)
	}
	_ = response.Body.Close()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve shutdown: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("daemon server did not stop after context cancellation")
	}
	select {
	case <-cleaned:
	default:
		t.Fatal("daemon cleanup was not invoked")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("agent control descriptor survived shutdown: %v", err)
	}
	connection, err := net.DialTimeout("tcp", listener.Addr().String(), 100*time.Millisecond)
	if err == nil {
		_ = connection.Close()
		t.Fatal("daemon listener still accepts after shutdown")
	}
}

func TestWireListDirBrowsesDaemonFilesystem(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"beta", "Alpha"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", name, err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "not-a-folder.txt"), []byte("ignored"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	hub := wire.NewHub()
	registerDaemonHandlers(hub, root, nil)
	normalize := func(raw any) map[string]any {
		t.Helper()
		data, err := json.Marshal(raw)
		if err != nil {
			t.Fatalf("marshal fs:list-dir: %v", err)
		}
		var result map[string]any
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.UseNumber()
		if err := dec.Decode(&result); err != nil {
			t.Fatalf("decode fs:list-dir: %v", err)
		}
		return result
	}

	raw, err := hub.Invoke("fs:list-dir", []any{root})
	if err != nil {
		t.Fatalf("fs:list-dir invoke: %v", err)
	}
	listing := normalize(raw)
	if filepath.Clean(fmt.Sprint(listing["path"])) != filepath.Clean(root) || filepath.Clean(fmt.Sprint(listing["parent"])) != filepath.Dir(filepath.Clean(root)) {
		t.Fatalf("listing location = %#v", listing)
	}
	entries := anySlice(listing["entries"])
	if len(entries) != 2 || mapFromAnyMain(entries[0])["name"] != "Alpha" || mapFromAnyMain(entries[1])["name"] != "beta" {
		t.Fatalf("directory-only sorted entries = %#v", entries)
	}
	if filepath.Clean(fmt.Sprint(mapFromAnyMain(entries[0])["path"])) != filepath.Join(filepath.Clean(root), "Alpha") {
		t.Fatalf("entry path = %#v", entries[0])
	}

	raw, err = hub.Invoke("fs:list-dir", []any{nil})
	if err != nil {
		t.Fatalf("fs:list-dir roots invoke: %v", err)
	}
	roots := normalize(raw)
	foundWorkspace := false
	foundApp := false
	for _, entry := range anySlice(roots["entries"]) {
		item := mapFromAnyMain(entry)
		if item["name"] == "Workspace" && filepath.Clean(fmt.Sprint(item["path"])) == filepath.Dir(filepath.Clean(root)) {
			foundWorkspace = true
		}
		if item["name"] == "App" && filepath.Clean(fmt.Sprint(item["path"])) == filepath.Clean(root) {
			foundApp = true
		}
	}
	if roots["path"] != nil || roots["parent"] != nil || !foundWorkspace || !foundApp {
		t.Fatalf("server roots = %#v", roots)
	}

	raw, err = hub.Invoke("app:meta", nil)
	if err != nil {
		t.Fatalf("app:meta invoke: %v", err)
	}
	meta := normalize(raw)
	if filepath.Clean(fmt.Sprint(meta["rootDir"])) != filepath.Clean(root) || filepath.Clean(fmt.Sprint(meta["workspaceDir"])) != filepath.Dir(filepath.Clean(root)) {
		t.Fatalf("app metadata path split = %#v", meta)
	}

	raw, err = hub.Invoke("fs:list-dir", []any{filepath.Join(root, "not-a-folder.txt")})
	if err != nil {
		t.Fatalf("fs:list-dir file invoke: %v", err)
	}
	failure := normalize(raw)
	if strings.TrimSpace(fmt.Sprint(failure["error"])) == "" || len(anySlice(failure["entries"])) != 0 {
		t.Fatalf("non-directory failure = %#v", failure)
	}
}

func TestWireE2EAppChatAssertsJobEventChannel(t *testing.T) {
	t.Log("asserted WIRE-CONTRACT event channels: job:event")
	t.Setenv("WORKASS_MOCK_ACP_DELAY_MS", "250")
	root := repoRoot(t)
	renderer := t.TempDir()
	stateDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(renderer, "index.html"), []byte("<!doctype html><head></head><body></body>"), 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}

	hub := wire.NewHub()
	manager := acp.NewManager(acp.Options{
		RootDir:           root,
		Provider:          acp.ProviderConfig{ID: "mock", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")}, CWD: root, Env: map[string]string{"WORKASS_MOCK_ACP_DELAY_MS": "250"}, Enabled: true, Label: "Workass Mock ACP"},
		DefaultProviderID: "mock",
		Broadcast:         hub.Broadcast,
		StateDir:          stateDir,
	})
	t.Cleanup(func() { manager.Reset() })
	registerDaemonHandlers(hub, root, manager, daemonOptions{StateDir: stateDir})

	server := httptest.NewServer(httpserve.New(renderer, hub, nil))
	defer server.Close()
	client := dialTestWS(t, server.URL)
	defer client.conn.Close()
	readyRefresh := client.waitChannelEvent(t, "agent:apply", 2*time.Second)
	if fieldString(mapFromAnyMain(readyRefresh.Payload), "action") != "session-refresh" {
		t.Fatalf("client-ready refresh = %#v", readyRefresh.Payload)
	}

	client.invoke(t, 1, "app-chat:new-session", map[string]any{"tabId": "wire-tab"})
	sessionReply := client.waitReply(t, 1, 5*time.Second)
	if sessionReply.Error != nil {
		t.Fatalf("new-session error: %s", *sessionReply.Error)
	}
	session := sessionReply.Result.(map[string]any)
	sessionID := session["sessionId"].(string)
	if sessionID == "" || session["agent"] != "Workass Mock ACP" {
		t.Fatalf("session reply = %#v", session)
	}

	chatID := "chat-wire-tab"
	// Renderer fixture: desktop/renderer/app.js:4901-4918 sends chatId, tabId,
	// sessionId, prompt, and history to api.startJob; app.js:4932-4941 needs
	// the start event PublicJob to echo chatId and status=="running".
	client.invoke(t, 2, "job:start", map[string]any{
		"kind": "app-chat", "chatId": chatID, "sessionId": sessionID, "tabId": "wire-tab",
		"prompt":        "[mock:slow] wire cancel token=public-secret",
		"userMessageId": "wire-user", "assistantMessageId": "wire-assistant",
	})
	startReply := client.waitReply(t, 2, 5*time.Second)
	if startReply.Error != nil {
		t.Fatalf("job:start error: %s", *startReply.Error)
	}
	job := startReply.Result.(map[string]any)
	jobID := job["id"].(string)
	if job["kind"] != "app-chat" || job["status"] != "running" || job["chatId"] != chatID || job["sessionId"] != sessionID {
		t.Fatalf("job reply = %#v", job)
	}
	if job["userMessageId"] != "wire-user" || job["assistantMessageId"] != "wire-assistant" ||
		!strings.Contains(fieldString(job, "promptText"), "token=[redacted]") ||
		strings.Contains(fieldString(job, "promptText"), "public-secret") {
		t.Fatalf("public canonical turn fields = %#v", job)
	}

	startEvent := client.waitJobEvent(t, jobID, "start", 5*time.Second)
	startEventJob, ok := startEvent["job"].(map[string]any)
	if !ok || startEvent["type"] != "start" || startEventJob["chatId"] != chatID || startEventJob["status"] != "running" {
		t.Fatalf("start event shape = %#v", startEvent)
	}
	if startEventJob["userMessageId"] != job["userMessageId"] ||
		startEventJob["assistantMessageId"] != job["assistantMessageId"] ||
		startEventJob["promptText"] != job["promptText"] {
		t.Fatalf("start event canonical fields = %#v, reply = %#v", startEventJob, job)
	}
	assertRendererRunningFixture(t, job, startEvent, chatID)
	// Prove the ACP event channel before cancelling. Cancelling immediately after
	// start may legitimately win before session/prompt is dispatched, in which
	// case no ACP notification can exist and waiting for one is an invalid oracle.
	acpEvent := client.waitJobEvent(t, jobID, "acp", 5*time.Second)
	eventBody, ok := acpEvent["event"].(map[string]any)
	if !ok || acpEvent["id"] != jobID || eventBody["kind"] == "" {
		t.Fatalf("acp event shape = %#v", acpEvent)
	}
	client.invoke(t, 3, "job:cancel", jobID)
	cancelReply := client.waitReply(t, 3, 5*time.Second)
	cancelResult := mapFromAnyMain(cancelReply.Result)
	if cancelReply.Error != nil || cancelResult["cancelled"] != true || cancelResult["reason"] != "cancelled" {
		t.Fatalf("cancel reply = %+v", cancelReply)
	}
	endEvent := client.waitJobEvent(t, jobID, "end", 5*time.Second)
	endJob := endEvent["job"].(map[string]any)
	if endJob["id"] != jobID || endJob["status"] != "failed" || endJob["code"] != json.Number("130") || endJob["stopReason"] != "cancelled" {
		t.Fatalf("end event job = %#v", endJob)
	}
	client.invoke(t, 4, "job:cancel", jobID)
	idleReply := client.waitReply(t, 4, 5*time.Second)
	idleResult := mapFromAnyMain(idleReply.Result)
	if idleReply.Error != nil || idleResult["cancelled"] != false || idleResult["reason"] != "idle" {
		t.Fatalf("idle cancel reply = %+v", idleReply)
	}
	client.invoke(t, 5, "job:cancel", "never-issued-job")
	unknownReply := client.waitReply(t, 5, 5*time.Second)
	unknownResult := mapFromAnyMain(unknownReply.Result)
	if unknownReply.Error != nil || unknownResult["cancelled"] != false || unknownResult["reason"] != "unknown" {
		t.Fatalf("unknown cancel reply = %+v", unknownReply)
	}
}

func TestStateDigestIsBodyFreeAndUnder64KiBForTwoHundredChats(t *testing.T) {
	stateDir := t.TempDir()
	store := newSessionStore(filepath.Join(stateDir, sessionStateFilename))
	chats := make([]any, 0, 200)
	for i := 0; i < 200; i++ {
		tabID := fmt.Sprintf("t%03d", i)
		chatID := fmt.Sprintf("c%03d", i)
		fixture := sessionMirrorFixture(tabID, chatID, "body token=digest-secret")
		chat := mapFromAnyMain(anySlice(fixture["chats"])[0])
		chat["queue"] = []any{map[string]any{"id": fmt.Sprintf("q%03d", i), "prompt": "queued body"}}
		chat[agentQueueRevisionField] = i + 1
		chat[runtimeControlRevisionField] = i + 2
		chats = append(chats, chat)
	}
	snapshot := sessionMirrorFixture("unused", "unused-chat", "unused")
	snapshot["activeId"] = "t000"
	snapshot["chats"] = chats
	if !store.Save(snapshot) {
		t.Fatal("save digest fixture")
	}

	hub := wire.NewHub()
	settings := newAppSettingsStore(filepath.Join(stateDir, "app-settings.json"))
	registerStateDigestHandler(hub, store, nil, settings)
	raw, err := hub.Invoke("state:digest", nil)
	if err != nil {
		t.Fatalf("state:digest invoke: %v", err)
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal state digest: %v", err)
	}
	if len(encoded) >= 64*1024 {
		t.Fatalf("state digest encoded size = %d, want < 65536", len(encoded))
	}
	if bytes.Contains(encoded, []byte("digest-secret")) || bytes.Contains(encoded, []byte("queued body")) {
		t.Fatalf("state digest leaked message or queue bodies: %s", encoded)
	}
	digest := mapFromAnyMain(raw)
	items := anySlice(digest["chats"])
	if len(items) != 200 {
		t.Fatalf("digest chat count = %d", len(items))
	}
	first := mapFromAnyMain(items[0])
	if first["tabId"] != "t000" || first["chatId"] != "c000" ||
		first["lastMessageId"] != "client-a" || first["messageCount"] != 2 ||
		first["queueLen"] != 1 || first["queueHeadId"] != "q000" ||
		first["agentQueueRevision"] != 1 || first["runtimeControlRevision"] != 2 {
		t.Fatalf("first chat digest = %#v", first)
	}
	if fieldString(digest, "settingsRevision") == "" || fieldString(digest, "procHash") == "" {
		t.Fatalf("global digest = %#v", digest)
	}
}

func TestWireSessionRecoversTurnCompletedWithoutRenderer(t *testing.T) {
	t.Setenv("WORKASS_MOCK_ACP_DELAY_MS", "250")
	root := repoRoot(t)
	stateDir := t.TempDir()
	renderer := t.TempDir()
	if err := os.WriteFile(filepath.Join(renderer, "index.html"), []byte("<!doctype html><head></head><body></body>"), 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}

	hub := wire.NewHub()
	sessionState := sharedSessionStore(stateDir)
	manager := acp.NewManager(acp.Options{
		RootDir:  root,
		StateDir: stateDir,
		Provider: acp.ProviderConfig{
			ID: "mock", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")},
			CWD: root, Env: map[string]string{"WORKASS_MOCK_ACP_DELAY_MS": "250"}, Enabled: true, Label: "Workass Mock ACP",
		},
		DefaultProviderID: "mock",
		Broadcast:         daemonEventBroadcaster(sessionState, hub.Broadcast),
	})
	t.Cleanup(func() { manager.Reset() })
	registerDaemonHandlers(hub, root, manager, daemonOptions{StateDir: stateDir})

	server := httptest.NewServer(httpserve.New(renderer, hub, nil))
	defer server.Close()
	client := dialTestWS(t, server.URL)

	client.invoke(t, 1, "app-chat:new-session", map[string]any{"tabId": "offline-tab", "chatId": "offline-chat", "providerId": "mock"})
	sessionReply := client.waitReply(t, 1, 5*time.Second)
	if sessionReply.Error != nil {
		t.Fatalf("new-session error: %s", *sessionReply.Error)
	}
	sessionID := sessionReply.Result.(map[string]any)["sessionId"].(string)
	prompt := "[mock:slow] persist after renderer closes"
	client.invoke(t, 2, "session:save", sessionMirrorFixture("offline-tab", "offline-chat", prompt))
	if reply := client.waitReply(t, 2, 2*time.Second); reply.Error != nil || reply.Result != true {
		t.Fatalf("session:save reply = %+v", reply)
	}

	client.invoke(t, 3, "job:start", map[string]any{
		"kind": "app-chat", "title": "Devin · Offline", "chatId": "offline-chat", "tabId": "offline-tab",
		"sessionId": sessionID, "providerId": "mock", "prompt": prompt,
	})
	startReply := client.waitReply(t, 3, 5*time.Second)
	if startReply.Error != nil {
		t.Fatalf("job:start error: %s", *startReply.Error)
	}
	jobID := startReply.Result.(map[string]any)["id"].(string)
	if err := client.conn.Close(); err != nil {
		t.Fatalf("close renderer connection: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		snapshot, _ := sessionState.Get().(map[string]any)
		assistant := sessionAssistant(t, snapshot, "offline-tab")
		if assistant["status"] == "done" {
			if assistant["jobId"] != jobID || !strings.Contains(stringValue(assistant["content"]), prompt) {
				t.Fatalf("daemon-completed assistant = %#v", assistant)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon turn did not finish while renderer was closed: %#v", assistant)
		}
		time.Sleep(20 * time.Millisecond)
	}

	reconnected := dialTestWS(t, server.URL)
	defer reconnected.conn.Close()
	reconnected.invoke(t, 4, "session:get")
	restoredReply := reconnected.waitReply(t, 4, 2*time.Second)
	if restoredReply.Error != nil {
		t.Fatalf("session:get after reconnect error: %s", *restoredReply.Error)
	}
	restored := restoredReply.Result.(map[string]any)
	assistant := sessionAssistant(t, restored, "offline-tab")
	if assistant["status"] != "done" || assistant["jobId"] != jobID || !strings.Contains(stringValue(assistant["content"]), prompt) {
		t.Fatalf("restored assistant = %#v", assistant)
	}

	reconnected.invoke(t, 5, "chat:archive-load", "offline-tab")
	archiveReply := reconnected.waitReply(t, 5, 2*time.Second)
	if archiveReply.Error != nil {
		t.Fatalf("archive load after reconnect error: %s", *archiveReply.Error)
	}
	archive, _ := archiveReply.Result.([]any)
	if len(archive) != 2 || !archiveContainsText(archive, prompt) {
		t.Fatalf("restored archive = %#v", archive)
	}
	t.Logf("trace close/reopen job=%s status=%s transcript=%q archiveRecords=%d", jobID, assistant["status"], assistant["content"], len(archive))
}

func TestWireBusyStartQueuesCapabilityAwareFollowUpWithoutFailedTranscript(t *testing.T) {
	t.Setenv("WORKASS_MOCK_ACP_DELAY_MS", "750")
	root := repoRoot(t)
	stateDir := t.TempDir()
	renderer := t.TempDir()
	if err := os.WriteFile(filepath.Join(renderer, "index.html"), []byte("<!doctype html><head></head><body></body>"), 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}

	hub := wire.NewHub()
	store := sharedSessionStore(stateDir)
	created, err := store.AgentCreateChat("Busy collision", root, "mock", "mock-deterministic", "ask", true)
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}
	tabID, chatID := fieldString(created, "tabId"), fieldString(created, "chatId")
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: stateDir,
		Provider: acp.ProviderConfig{
			ID: "mock", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")},
			CWD: root, Env: map[string]string{"WORKASS_MOCK_ACP_DELAY_MS": "750"}, Enabled: true, Label: "Workass Mock ACP",
		},
		DefaultProviderID: "mock", Broadcast: daemonEventBroadcaster(store, hub.Broadcast),
	})
	t.Cleanup(func() { manager.Reset() })
	registerDaemonHandlers(hub, root, manager, daemonOptions{StateDir: stateDir})

	server := httptest.NewServer(httpserve.New(renderer, hub, nil))
	defer server.Close()
	client := dialTestWS(t, server.URL)
	defer client.conn.Close()
	ready := client.waitChannelEvent(t, "agent:apply", 2*time.Second)
	if fieldString(mapFromAnyMain(ready.Payload), "action") != "session-refresh" {
		t.Fatalf("busy client-ready refresh = %#v", ready.Payload)
	}

	client.invoke(t, 1, "app-chat:new-session", map[string]any{
		"tabId": tabID, "chatId": chatID, "providerId": "mock", "cwd": root,
	})
	sessionReply := client.waitReply(t, 1, 5*time.Second)
	if sessionReply.Error != nil {
		t.Fatalf("new-session error: %s", *sessionReply.Error)
	}
	sessionID := fieldString(mapFromAnyMain(sessionReply.Result), "sessionId")
	if sessionID == "" {
		t.Fatalf("new-session result = %#v", sessionReply.Result)
	}

	client.invoke(t, 2, "job:start", map[string]any{
		"kind": "app-chat", "title": "Busy collision", "tabId": tabID, "chatId": chatID,
		"sessionId": sessionID, "providerId": "mock", "prompt": "[mock:slow] first turn",
		"userMessageId": "busy-first-user", "assistantMessageId": "busy-first-assistant",
	})
	firstReply := client.waitReply(t, 2, 5*time.Second)
	if firstReply.Error != nil {
		t.Fatalf("first job:start error: %s", *firstReply.Error)
	}
	firstJobID := fieldString(mapFromAnyMain(firstReply.Result), "id")
	if firstJobID == "" {
		t.Fatalf("first job:start result = %#v", firstReply.Result)
	}

	client.invoke(t, 3, "job:start", map[string]any{
		"kind": "app-chat", "title": "Busy collision", "tabId": tabID, "chatId": chatID,
		"sessionId": sessionID, "providerId": "mock", "prompt": "follow up exactly once",
		"userMessageId": "busy-follow-user", "assistantMessageId": "busy-follow-assistant",
		"busyMode": "queue-v1",
	})
	queuedReply := client.waitReply(t, 3, 5*time.Second)
	if queuedReply.Error != nil {
		t.Fatalf("capability-aware busy start returned an error: %s", *queuedReply.Error)
	}
	queued := mapFromAnyMain(queuedReply.Result)
	if queued["queued"] != true || fieldString(queued, "queueId") == "" || intValue(queued["agentQueueRevision"]) != 1 {
		t.Fatalf("busy queue receipt = %#v", queued)
	}
	collisionRefresh := client.waitChannelEvent(t, "agent:apply", time.Second)
	if fieldString(mapFromAnyMain(collisionRefresh.Payload), "action") != "session-refresh" {
		t.Fatalf("busy collision refresh = %#v", collisionRefresh.Payload)
	}

	queuedChat := chatFromSnapshot(store.Get().(map[string]any), tabID)
	queue := anySlice(queuedChat["queue"])
	if len(queue) != 1 || fieldString(mapFromAnyMain(queue[0]), "source") != "host" || fieldString(mapFromAnyMain(queue[0]), "text") != "follow up exactly once" {
		t.Fatalf("durable busy queue = %#v", queue)
	}
	for _, raw := range messageSlice(queuedChat) {
		message := mapFromAnyMain(raw)
		if fieldString(message, "id") == "busy-follow-user" || fieldString(message, "id") == "busy-follow-assistant" ||
			strings.Contains(fieldString(message, "content"), "Ya hay una respuesta en curso") {
			t.Fatalf("busy collision leaked a false transcript turn: %#v", message)
		}
	}

	client.waitJobEvent(t, firstJobID, "end", 8*time.Second)
	deadline := time.Now().Add(8 * time.Second)
	for {
		read, readErr := store.AgentReadChat(tabID, chatID, 20, false)
		if readErr != nil {
			t.Fatalf("read drained chat: %v", readErr)
		}
		messages := anySlice(read["messages"])
		userCount, doneCount, failedBusyCount := 0, 0, 0
		for _, raw := range messages {
			message := mapFromAnyMain(raw)
			if fieldString(message, "role") == "user" && fieldString(message, "content") == "follow up exactly once" {
				userCount++
			}
			if fieldString(message, "id") == "busy-follow-assistant" && fieldString(message, "status") == "done" {
				doneCount++
			}
			if fieldString(message, "status") == "failed" && strings.Contains(fieldString(message, "content"), "respuesta en curso") {
				failedBusyCount++
			}
		}
		if userCount == 1 && doneCount == 1 && failedBusyCount == 0 && len(anySlice(read["queue"])) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("queued follow-up did not drain exactly once: messages=%#v queue=%#v", messages, read["queue"])
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestWireWorkspaceMoveCommitsBeforeInvalidationAndStaleReconnectUsesTargetCWD(t *testing.T) {
	root := repoRoot(t)
	stateDir := t.TempDir()
	oldCWD := t.TempDir()
	targetCWD := t.TempDir()
	secondTargetCWD := t.TempDir()
	staleTargetCWD := t.TempDir()
	tabID, chatID := "wire-workspace-tab", "wire-workspace-chat"
	renderer := t.TempDir()
	if err := os.WriteFile(filepath.Join(renderer, "index.html"), []byte("<!doctype html><head></head><body></body>"), 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}

	sessionState := sharedSessionStore(stateDir)
	snapshot := sessionMirrorFixture(tabID, chatID, "canonical before move")
	chat := chatFromSnapshot(snapshot, tabID)
	chat["cwd"] = oldCWD
	for _, raw := range messageSlice(chat) {
		message := mapFromAnyMain(raw)
		message["status"] = "done"
		if message["role"] == "assistant" {
			message["content"] = "canonical answer before move"
			message["at"] = "2026-07-13T00:00:01Z"
		}
	}
	if !sessionState.Save(snapshot) {
		t.Fatal("save initial workspace snapshot")
	}

	hub := wire.NewHub()
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: stateDir,
		Provider: acp.ProviderConfig{
			ID: "mock", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")},
			CWD: root, Enabled: true, Label: "Workass Mock ACP",
		},
		DefaultProviderID: "mock", Broadcast: daemonEventBroadcaster(sessionState, hub.Broadcast),
	})
	t.Cleanup(func() { manager.Reset() })
	registerDaemonHandlers(hub, root, manager, daemonOptions{StateDir: stateDir})

	server := httptest.NewServer(httpserve.New(renderer, hub, nil))
	defer server.Close()
	first := dialTestWS(t, server.URL)
	first.invoke(t, 1, "app-chat:new-session", map[string]any{
		"tabId": tabID, "chatId": chatID, "providerId": "mock", "cwd": oldCWD,
	})
	oldReply := first.waitReply(t, 1, 5*time.Second)
	if oldReply.Error != nil {
		t.Fatalf("old workspace new-session: %s", *oldReply.Error)
	}
	oldSession := mapFromAnyMain(oldReply.Result)
	oldSessionID := fieldString(oldSession, "sessionId")
	if oldSessionID == "" || filepath.Clean(fieldString(oldSession, "cwd")) != filepath.Clean(oldCWD) {
		t.Fatalf("old workspace session = %#v", oldSession)
	}

	first.invoke(t, 2, "app-chat:new-session", map[string]any{
		"tabId": tabID, "chatId": chatID, "providerId": "mock", "cwd": targetCWD,
		"replaceSessionId": oldSessionID, "workspaceRebind": true, "expectedWorkspaceRevision": 0,
	})
	moveReply := first.waitReply(t, 2, 5*time.Second)
	if moveReply.Error != nil {
		t.Fatalf("workspace move reply error: %s", *moveReply.Error)
	}
	move := mapFromAnyMain(moveReply.Result)
	if move["workspaceCommitted"] != true || move["workspaceRebound"] != true || fieldString(move, "sessionId") != "" || fmt.Sprint(move["workspaceRevision"]) != "1" {
		t.Fatalf("workspace move reply = %#v", move)
	}
	if _, ok := manager.LiveSession(oldSessionID); ok {
		t.Fatal("old live session survived wire workspace move")
	}
	if cwd, ok := sessionState.ChatWorkspace(tabID, chatID); !ok || filepath.Clean(cwd) != filepath.Clean(targetCWD) {
		t.Fatalf("committed workspace cwd=%q ok=%v, want %q", cwd, ok, targetCWD)
	}
	// A second drag is legitimate while the renderer is sessionless, but still
	// crosses the daemon CAS boundary. A competing controller holding revision 1
	// cannot overwrite the accepted revision-2 move.
	first.invoke(t, 3, "app-chat:new-session", map[string]any{
		"tabId": tabID, "chatId": chatID, "providerId": "mock", "cwd": secondTargetCWD,
		"workspaceRebind": true, "expectedWorkspaceRevision": 1,
	})
	secondReply := first.waitReply(t, 3, 5*time.Second)
	second := mapFromAnyMain(secondReply.Result)
	if secondReply.Error != nil || second["workspaceCommitted"] != true || fmt.Sprint(second["workspaceRevision"]) != "2" {
		t.Fatalf("second sessionless workspace move = %+v %#v", secondReply, second)
	}
	first.invoke(t, 4, "app-chat:new-session", map[string]any{
		"tabId": tabID, "chatId": chatID, "providerId": "mock", "cwd": staleTargetCWD,
		"workspaceRebind": true, "expectedWorkspaceRevision": 1,
	})
	staleMoveReply := first.waitReply(t, 4, 5*time.Second)
	staleMove := mapFromAnyMain(staleMoveReply.Result)
	if staleMoveReply.Error != nil || fieldString(staleMove, "error") == "" || staleMove["workspaceCommitted"] != false {
		t.Fatalf("stale competing move reply = %+v %#v", staleMoveReply, staleMove)
	}
	if cwd, ok := sessionState.ChatWorkspace(tabID, chatID); !ok || filepath.Clean(cwd) != filepath.Clean(secondTargetCWD) {
		t.Fatalf("stale move changed cwd=%q ok=%v, want %q", cwd, ok, secondTargetCWD)
	}
	if err := first.conn.Close(); err != nil {
		t.Fatalf("close stale controller: %v", err)
	}

	// Reconnect with the controller's stale pre-move cwd. Both session/new and
	// job:start must take cwd from the daemon-owned chat snapshot.
	reconnected := dialTestWS(t, server.URL)
	defer reconnected.conn.Close()
	reconnected.invoke(t, 5, "app-chat:new-session", map[string]any{
		"tabId": tabID, "chatId": chatID, "providerId": "mock", "cwd": oldCWD,
	})
	freshReply := reconnected.waitReply(t, 5, 5*time.Second)
	if freshReply.Error != nil {
		t.Fatalf("fresh target session after stale reconnect: %s", *freshReply.Error)
	}
	fresh := mapFromAnyMain(freshReply.Result)
	freshSessionID := fieldString(fresh, "sessionId")
	if freshSessionID == "" || freshSessionID == oldSessionID || filepath.Clean(fieldString(fresh, "cwd")) != filepath.Clean(secondTargetCWD) {
		t.Fatalf("fresh session after stale reconnect = %#v; old=%s target=%s", fresh, oldSessionID, secondTargetCWD)
	}
	reconnected.invoke(t, 6, "job:start", map[string]any{
		"kind": "app-chat", "tabId": tabID, "chatId": chatID, "sessionId": freshSessionID,
		"providerId": "mock", "cwd": oldCWD, "prompt": "next turn after move",
		"history": []any{
			map[string]any{"role": "user", "content": "canonical before move"},
			map[string]any{"role": "assistant", "content": "canonical answer before move"},
		},
	})
	startReply := reconnected.waitReply(t, 6, 5*time.Second)
	if startReply.Error != nil {
		t.Fatalf("next send after workspace move: %s", *startReply.Error)
	}
	job := mapFromAnyMain(startReply.Result)
	end := reconnected.waitJobEvent(t, fieldString(job, "id"), "end", 5*time.Second)
	result := fieldString(mapFromAnyMain(end["job"]), "result")
	if !strings.Contains(result, "User: canonical before move") || !strings.Contains(result, "User request:\nnext turn after move") {
		t.Fatalf("next target-cwd turn lost canonical replay: %q", result)
	}
	if live, ok := manager.LiveSession(freshSessionID); !ok || filepath.Clean(live.Info.CWD) != filepath.Clean(secondTargetCWD) {
		t.Fatalf("next turn live session=%+v ok=%v, want target cwd %q", live, ok, secondTargetCWD)
	}
}

func TestWireReconnectRestoresLiveSessionControlsAndPendingPermission(t *testing.T) {
	root := repoRoot(t)
	stateDir := t.TempDir()
	renderer := t.TempDir()
	if err := os.WriteFile(filepath.Join(renderer, "index.html"), []byte("<!doctype html><head></head><body></body>"), 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}

	hub := wire.NewHub()
	sessionState := sharedSessionStore(stateDir)
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: stateDir,
		Provider:          acp.ProviderConfig{ID: "mock", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")}, CWD: root, Enabled: true, Label: "Workass Mock ACP"},
		DefaultProviderID: "mock", PermissionTimeout: 5 * time.Second,
		Broadcast: daemonEventBroadcaster(sessionState, hub.Broadcast),
	})
	t.Cleanup(func() { manager.Reset() })
	registerDaemonHandlers(hub, root, manager, daemonOptions{StateDir: stateDir})

	server := httptest.NewServer(httpserve.New(renderer, hub, nil))
	defer server.Close()
	first := dialTestWS(t, server.URL)
	first.invoke(t, 1, "app-chat:new-session", map[string]any{"tabId": "reconnect-tab", "chatId": "reconnect-chat", "providerId": "mock"})
	sessionReply := first.waitReply(t, 1, 5*time.Second)
	if sessionReply.Error != nil {
		t.Fatalf("new-session error: %s", *sessionReply.Error)
	}
	sessionID := sessionReply.Result.(map[string]any)["sessionId"].(string)
	if !sessionState.Save(sessionMirrorFixture("reconnect-tab", "reconnect-chat", "[mock:permission] reconnect controls")) {
		t.Fatal("session save failed")
	}
	first.invoke(t, 2, "job:start", map[string]any{
		"kind": "app-chat", "chatId": "reconnect-chat", "tabId": "reconnect-tab", "sessionId": sessionID,
		"providerId": "mock", "prompt": "[mock:permission] reconnect controls",
	})
	startReply := first.waitReply(t, 2, 5*time.Second)
	if startReply.Error != nil {
		t.Fatalf("job:start error: %s", *startReply.Error)
	}
	jobID := startReply.Result.(map[string]any)["id"].(string)
	permission := first.waitChannelEvent(t, "chat:permission-request", 5*time.Second).Payload.(map[string]any)
	if permission["jobId"] != jobID {
		t.Fatalf("initial permission = %#v", permission)
	}
	if err := first.conn.Close(); err != nil {
		t.Fatalf("close first controller: %v", err)
	}

	reconnected := dialTestWS(t, server.URL)
	defer reconnected.conn.Close()
	replayed := reconnected.waitChannelEvent(t, "chat:permission-request", 3*time.Second).Payload.(map[string]any)
	if replayed["id"] != permission["id"] || replayed["jobId"] != jobID || replayed["sessionId"] != sessionID {
		t.Fatalf("replayed permission = %#v, want id=%v job=%s session=%s", replayed, permission["id"], jobID, sessionID)
	}

	reconnected.invoke(t, 3, "session:get")
	restoredReply := reconnected.waitReply(t, 3, 2*time.Second)
	if restoredReply.Error != nil {
		t.Fatalf("session:get error: %s", *restoredReply.Error)
	}
	restored := restoredReply.Result.(map[string]any)
	chat := chatFromSnapshot(restored, "reconnect-tab")
	live := mapFromAnyMain(chat["liveSession"])
	if live["sessionId"] != sessionID || live["providerId"] != "mock" {
		t.Fatalf("live session overlay = %#v", live)
	}
	if len(anySlice(live["models"])) == 0 || len(anySlice(live["modes"])) == 0 {
		t.Fatalf("live session controls missing models/modes: %#v", live)
	}

	reconnected.invoke(t, 4, "chat:permission-decide", map[string]any{"id": replayed["id"], "optionId": "allow-once"})
	if reply := reconnected.waitReply(t, 4, 2*time.Second); reply.Error != nil || mapFromAnyMain(reply.Result)["ok"] != true {
		t.Fatalf("permission decide after reconnect = %+v", reply)
	}
	end := reconnected.waitJobEvent(t, jobID, "end", 5*time.Second)
	if job := mapFromAnyMain(end["job"]); job["status"] != "done" || job["stopReason"] != "end_turn" {
		t.Fatalf("job end after reconnect = %#v", job)
	}
	reconnected.invoke(t, 5, "app-chat:set-model", map[string]any{"sessionId": sessionID, "modelId": "mock-deterministic[high]"})
	if reply := reconnected.waitReply(t, 5, 2*time.Second); reply.Error != nil || mapFromAnyMain(reply.Result)["currentModelId"] != "mock-deterministic[high]" {
		t.Fatalf("set-model after reconnect = %+v", reply)
	}
	reconnected.invoke(t, 6, "app-chat:set-mode", map[string]any{"sessionId": sessionID, "modeId": "bypass"})
	if reply := reconnected.waitReply(t, 6, 2*time.Second); reply.Error != nil || mapFromAnyMain(reply.Result)["currentModeId"] != "bypass" {
		t.Fatalf("set-mode after reconnect = %+v", reply)
	}
	// No renderer session:save follows either control change. The handlers must
	// patch the daemon-owned per-chat snapshot synchronously so a tab switch or
	// renderer loss cannot restore the provider default.
	reloadedControls := newSessionStore(filepath.Join(stateDir, sessionStateFilename))
	if err := reloadedControls.LoadError(); err != nil {
		t.Fatalf("reload controls after wire set: %v", err)
	}
	persistedChat := chatFromSnapshot(reloadedControls.Get().(map[string]any), "reconnect-tab")
	if fieldString(persistedChat, "providerId") != "mock" || fieldString(persistedChat, "currentModelId") != "mock-deterministic[high]" || fieldString(persistedChat, "currentModeId") != "bypass" {
		t.Fatalf("wire controls not durably isolated = provider=%q model=%q mode=%q", fieldString(persistedChat, "providerId"), fieldString(persistedChat, "currentModelId"), fieldString(persistedChat, "currentModeId"))
	}

	reconnected.invoke(t, 7, "job:start", map[string]any{
		"kind": "app-chat", "chatId": "reconnect-chat", "tabId": "reconnect-tab", "sessionId": sessionID,
		"providerId": "mock", "prompt": "[mock:slow] reconnect cancel",
	})
	slowReply := reconnected.waitReply(t, 7, 5*time.Second)
	if slowReply.Error != nil {
		t.Fatalf("slow job start error: %s", *slowReply.Error)
	}
	slowJobID := mapFromAnyMain(slowReply.Result)["id"].(string)
	_ = reconnected.waitJobEvent(t, slowJobID, "start", 3*time.Second)
	if err := reconnected.conn.Close(); err != nil {
		t.Fatalf("close second controller: %v", err)
	}
	third := dialTestWS(t, server.URL)
	defer third.conn.Close()
	third.invoke(t, 8, "job:cancel", slowJobID)
	if reply := third.waitReply(t, 8, 3*time.Second); reply.Error != nil || mapFromAnyMain(reply.Result)["cancelled"] != true {
		t.Fatalf("cancel after reconnect = %+v", reply)
	}
	cancelEnd := third.waitJobEvent(t, slowJobID, "end", 5*time.Second)
	if job := mapFromAnyMain(cancelEnd["job"]); job["status"] != "failed" || job["stopReason"] != "cancelled" || job["code"] != json.Number("130") {
		t.Fatalf("cancelled job end = %#v", job)
	}
	t.Logf("trace reconnect restored session=%s permissionJob=%s cancelJob=%s models=%d modes=%d permission=%s", sessionID, jobID, slowJobID, len(anySlice(live["models"])), len(anySlice(live["modes"])), replayed["id"])
}

func TestWireDaemonQueueDrainsWithoutControllerAndReplaysPermissionOnAttach(t *testing.T) {
	root := repoRoot(t)
	stateDir := t.TempDir()
	renderer := t.TempDir()
	if err := os.WriteFile(filepath.Join(renderer, "index.html"), []byte("<!doctype html><head></head><body></body>"), 0o644); err != nil {
		t.Fatal(err)
	}

	hub := wire.NewHub()
	sessionState := sharedSessionStore(stateDir)
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: stateDir,
		Provider:          acp.ProviderConfig{ID: "mock", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")}, CWD: root, Enabled: true, Label: "Workass Mock ACP"},
		DefaultProviderID: "mock", PermissionTimeout: 5 * time.Second,
		Broadcast: daemonEventBroadcaster(sessionState, hub.Broadcast),
	})
	t.Cleanup(func() { manager.Reset() })
	coordinator := newChatControlCoordinator(manager, sessionState, hub.Broadcast)
	registerDaemonHandlers(hub, root, manager, daemonOptions{StateDir: stateDir, ChatControl: coordinator})

	created, err := sessionState.AgentCreateChat("Disconnected drain", root, "mock", "mock-deterministic", "ask", false)
	if err != nil {
		t.Fatal(err)
	}
	tabID, chatID := fieldString(created, "tabId"), fieldString(created, "chatId")
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if _, err := manager.NewSession(ctx, acp.SessionOptions{TabID: tabID, ChatID: chatID, ProviderID: "mock", CWD: root}); err != nil {
		t.Fatalf("new disconnected queue session: %v", err)
	}

	server := httptest.NewServer(httpserve.New(renderer, hub, nil))
	defer server.Close()
	receipt, err := sessionState.AgentEnqueueChat(tabID, chatID, "[mock:permission] drain before controller attach", "auto")
	if err != nil {
		t.Fatal(err)
	}
	coordinator.scheduleDrain(tabID, chatID)
	var permission map[string]any
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		pending := manager.PendingPermissions()
		if len(pending) > 0 {
			permission = mapFromAnyMain(pending[0])
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if permission == nil {
		t.Fatal("daemon-owned queue did not reach a permission wait without a controller")
	}

	client := dialTestWS(t, server.URL)
	defer client.conn.Close()
	replayed := mapFromAnyMain(client.waitChannelEvent(t, "chat:permission-request", 3*time.Second).Payload)
	if replayed["id"] != permission["id"] || fieldString(replayed, "jobId") == "" {
		t.Fatalf("attached controller permission = %#v, pending = %#v", replayed, permission)
	}
	client.invoke(t, 1, "session:get")
	sessionReply := client.waitReply(t, 1, 2*time.Second)
	if sessionReply.Error != nil {
		t.Fatalf("session:get after attach: %s", *sessionReply.Error)
	}
	snapshot := mapFromAnyMain(sessionReply.Result)
	assistant := sessionAssistant(t, snapshot, tabID)
	if fieldString(assistant, "status") != "running" || fieldString(assistant, "jobId") != fieldString(replayed, "jobId") {
		t.Fatalf("attached controller did not see the live queued turn: %#v", assistant)
	}
	user := mapFromAnyMain(messageSlice(chatFromSnapshot(snapshot, tabID))[0])
	if fieldString(user, agentQueueMessageField) != fieldString(receipt, "queueId") {
		t.Fatalf("queued turn identity was not threaded into canonical history: %#v", user)
	}

	client.invoke(t, 2, "chat:permission-decide", map[string]any{"id": replayed["id"], "optionId": "allow-once"})
	if reply := client.waitReply(t, 2, 2*time.Second); reply.Error != nil || mapFromAnyMain(reply.Result)["ok"] != true {
		t.Fatalf("permission decision after attach = %+v", reply)
	}
	end := client.waitJobEvent(t, fieldString(replayed, "jobId"), "end", 5*time.Second)
	if job := mapFromAnyMain(end["job"]); fieldString(job, "status") != "done" {
		t.Fatalf("disconnected queued turn end = %#v", job)
	}
}

func TestApplyProductionDefaultsWindowsOnlyAndPreservesOverrides(t *testing.T) {
	port := 8788
	bind := "localhost"
	applyProductionDefaults("windows", true, map[string]bool{}, &port, &bind)
	if port != 80 || bind != "lan" {
		t.Fatalf("windows prod defaults port=%d bind=%q", port, bind)
	}

	port = 19000
	bind = "localhost"
	applyProductionDefaults("windows", true, map[string]bool{"port": true, "bind": true}, &port, &bind)
	if port != 19000 || bind != "localhost" {
		t.Fatalf("overridden defaults port=%d bind=%q", port, bind)
	}

	port = 8788
	bind = "localhost"
	applyProductionDefaults("darwin", true, map[string]bool{}, &port, &bind)
	if port != 8788 || bind != "localhost" {
		t.Fatalf("non-windows defaults port=%d bind=%q", port, bind)
	}
}

func TestWireTraceMockPermissionTurn(t *testing.T) {
	root := repoRoot(t)
	renderer := t.TempDir()
	if err := os.WriteFile(filepath.Join(renderer, "index.html"), []byte("<!doctype html><head></head><body></body>"), 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}

	hub := wire.NewHub()
	manager := acp.NewManager(acp.Options{
		RootDir:             root,
		Provider:            acp.ProviderConfig{Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")}, CWD: root, Label: "Workass Mock ACP"},
		Broadcast:           hub.Broadcast,
		StdoutFlushInterval: 30 * time.Millisecond,
	})
	t.Cleanup(func() { manager.Reset() })
	registerDaemonHandlers(hub, root, manager)

	server := httptest.NewServer(httpserve.New(renderer, hub, nil))
	defer server.Close()
	client := dialTestWS(t, server.URL)
	defer client.conn.Close()

	client.invoke(t, 1, "app-chat:new-session", map[string]any{"tabId": "wire-perm-tab"})
	sessionReply := client.waitReply(t, 1, 5*time.Second)
	if sessionReply.Error != nil {
		t.Fatalf("new-session error: %s", *sessionReply.Error)
	}
	session := sessionReply.Result.(map[string]any)
	sessionID := session["sessionId"].(string)

	chatID := "chat-wire-permission"
	client.invoke(t, 2, "job:start", map[string]any{"kind": "app-chat", "chatId": chatID, "sessionId": sessionID, "tabId": "wire-perm-tab", "prompt": "[mock:permission] approve this"})
	startReply := client.waitReply(t, 2, 5*time.Second)
	if startReply.Error != nil {
		t.Fatalf("job:start error: %s", *startReply.Error)
	}
	job := startReply.Result.(map[string]any)
	jobID := job["id"].(string)
	if job["kind"] != "app-chat" || job["status"] != "running" || job["chatId"] != chatID || job["sessionId"] != sessionID {
		t.Fatalf("job:start reply = %#v", job)
	}

	sawStart := false
	sawPermission := false
	sawOutcome := false
	sawData := false
	for {
		msg := client.waitEvent(t, 5*time.Second)
		if msg.Channel == "chat:permission-request" {
			if !sawStart {
				t.Fatalf("permission arrived before renderer-consumable start event")
			}
			req := msg.Payload.(map[string]any)
			// Renderer fixture: desktop/renderer/app.js:6048-6056 requires id,
			// jobId, options[], title, and kind; app.js:6065-6069 renders each
			// optionId into button data-opt, which app.js:6084-6087 sends back.
			options, _ := req["options"].([]any)
			if req["id"] == "" || req["jobId"] != jobID || req["sessionId"] != sessionID || req["title"] != "Mock permission gate" || req["kind"] != "execute" || len(options) != 2 {
				t.Fatalf("permission request payload = %#v", req)
			}
			allowOpt, _ := options[0].(map[string]any)
			rejectOpt, _ := options[1].(map[string]any)
			if allowOpt["optionId"] != "allow-once" || rejectOpt["optionId"] != "reject" {
				t.Fatalf("permission options = %#v", options)
			}
			t.Logf("trace event chat:permission-request id=%s jobId=%s sessionId=%s title=%s options=%d", req["id"], req["jobId"], req["sessionId"], req["title"], len(req["options"].([]any)))
			// Renderer fixture: desktop/renderer/app.js:6084-6087 calls
			// api.chatPermissionDecide(permId, b.dataset.opt); the LAN bridge
			// at internal/httpserve/lan_bridge.go:38 turns that into exactly
			// invoke('chat:permission-decide', { id, optionId }).
			client.invoke(t, 3, "chat:permission-decide", map[string]any{"id": req["id"], "optionId": "allow-once"})
			decideReply := client.waitReply(t, 3, 5*time.Second)
			t.Logf("trace reply chat:permission-decide result=%v error=%v", decideReply.Result, decideReply.Error)
			decideResult, _ := decideReply.Result.(map[string]any)
			if decideReply.Error != nil || decideResult["ok"] != true {
				t.Fatalf("permission decide reply = %+v", decideReply)
			}
			sawPermission = true
			continue
		}
		if msg.Channel != "job:event" {
			continue
		}
		payload := msg.Payload.(map[string]any)
		switch payload["type"] {
		case "start":
			evJob := payload["job"].(map[string]any)
			if evJob["id"] != jobID || evJob["status"] != "running" || evJob["chatId"] != chatID || evJob["sessionId"] != sessionID {
				t.Fatalf("start payload = %#v", payload)
			}
			assertRendererRunningFixture(t, job, payload, chatID)
			sawStart = true
			t.Logf("trace event job:event type=start id=%s status=%s chatId=%s sessionId=%s", evJob["id"], evJob["status"], evJob["chatId"], evJob["sessionId"])
		case "acp":
			event := payload["event"].(map[string]any)
			t.Logf("trace event job:event type=acp id=%s kind=%s status=%v", payload["id"], event["kind"], event["status"])
		case "data":
			if payload["id"] != jobID || payload["stream"] != "stdout" {
				t.Fatalf("data payload = %#v", payload)
			}
			sawData = true
			if strings.Contains(fmt.Sprint(payload["chunk"]), "Permission outcome: selected allow-once.") {
				sawOutcome = true
			}
			t.Logf("trace event job:event type=data id=%s stream=%s chunk=%q", payload["id"], payload["stream"], payload["chunk"])
		case "usage":
			t.Logf("trace event job:event type=usage sessionId=%s used=%v size=%v", payload["sessionId"], payload["used"], payload["size"])
		case "end":
			evJob := payload["job"].(map[string]any)
			t.Logf("trace event job:event type=end id=%s status=%s code=%v stopReason=%v", evJob["id"], evJob["status"], evJob["code"], evJob["stopReason"])
			if !sawStart || !sawPermission || !sawData || !sawOutcome || evJob["status"] != "done" || evJob["stopReason"] != "end_turn" {
				t.Fatalf("end payload = %#v sawStart=%v sawPermission=%v sawData=%v sawOutcome=%v", payload, sawStart, sawPermission, sawData, sawOutcome)
			}
			return
		}
	}
}

func TestWireLostTerminalReconciliationEmitsRendererEndEvent(t *testing.T) {
	root := repoRoot(t)
	renderer := t.TempDir()
	if err := os.WriteFile(filepath.Join(renderer, "index.html"), []byte("<!doctype html><head></head><body></body>"), 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}

	hub := wire.NewHub()
	manager := acp.NewManager(acp.Options{
		RootDir: root,
		Provider: acp.ProviderConfig{
			ID: "mock", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")},
			CWD: root, Enabled: true, Label: "Workass Mock ACP",
		},
		DefaultProviderID:       "mock",
		Broadcast:               hub.Broadcast,
		StdoutFlushInterval:     5 * time.Millisecond,
		ThoughtFlushInterval:    5 * time.Millisecond,
		PromptReconcileInterval: 25 * time.Millisecond,
		PromptReconcileTimeout:  100 * time.Millisecond,
		PromptTerminalGrace:     100 * time.Millisecond,
	})
	t.Cleanup(func() { manager.Reset() })
	registerDaemonHandlers(hub, root, manager)

	server := httptest.NewServer(httpserve.New(renderer, hub, nil))
	defer server.Close()
	client := dialTestWS(t, server.URL)
	defer client.conn.Close()

	const tabID = "wire-lost-terminal-tab"
	const chatID = "wire-lost-terminal-chat"
	client.invoke(t, 1, "app-chat:new-session", map[string]any{
		"tabId": tabID, "chatId": chatID, "providerId": "mock",
	})
	sessionReply := client.waitReply(t, 1, 5*time.Second)
	if sessionReply.Error != nil {
		t.Fatalf("new-session error: %s", *sessionReply.Error)
	}
	sessionID := fieldString(mapFromAnyMain(sessionReply.Result), "sessionId")
	client.invoke(t, 2, "job:start", map[string]any{
		"kind": "app-chat", "tabId": tabID, "chatId": chatID, "sessionId": sessionID,
		"providerId": "mock", "prompt": "[mock:lost-terminal] complete visibly over wire",
	})
	startReply := client.waitReply(t, 2, 5*time.Second)
	if startReply.Error != nil {
		t.Fatalf("job:start error: %s", *startReply.Error)
	}
	jobID := fieldString(mapFromAnyMain(startReply.Result), "id")
	end := client.waitJobEvent(t, jobID, "end", 3*time.Second)
	ended := mapFromAnyMain(end["job"])
	if ended["status"] != "done" || ended["stopReason"] != "end_turn" || ended["code"] != json.Number("0") {
		t.Fatalf("reconciled wire end = %#v", ended)
	}
	if result := fieldString(ended, "result"); !strings.Contains(result, "[mock:lost-terminal] complete visibly over wire") {
		t.Fatalf("reconciled wire result lost streamed output: %q", result)
	}
	t.Logf("trace lost terminal reconciled job=%s status=%s stopReason=%s", jobID, ended["status"], ended["stopReason"])
}

func TestWireTraceAppChatSteer(t *testing.T) {
	root := repoRoot(t)
	renderer := t.TempDir()
	stateDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(renderer, "index.html"), []byte("<!doctype html><head></head><body></body>"), 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}

	hub := wire.NewHub()
	basePrompt := "[mock:slow] [mock:steer] base turn"
	sessionState := sharedSessionStore(stateDir)
	if !sessionState.Save(sessionMirrorFixture("wire-steer-tab", "chat-wire-steer", basePrompt)) {
		t.Fatal("save wire steer session fixture")
	}
	manager := acp.NewManager(acp.Options{
		RootDir:             root,
		StateDir:            stateDir,
		Provider:            acp.ProviderConfig{Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")}, CWD: root, Label: "Workass Mock ACP"},
		Broadcast:           daemonEventBroadcaster(sessionState, hub.Broadcast),
		StdoutFlushInterval: 20 * time.Millisecond,
	})
	t.Cleanup(func() { manager.Reset() })
	registerDaemonHandlers(hub, root, manager, daemonOptions{StateDir: stateDir})

	server := httptest.NewServer(httpserve.New(renderer, hub, nil))
	defer server.Close()
	client := dialTestWS(t, server.URL)
	defer client.conn.Close()

	client.invoke(t, 1, "app-chat:new-session", map[string]any{"tabId": "wire-steer-tab", "chatId": "chat-wire-steer"})
	sessionReply := client.waitReply(t, 1, 5*time.Second)
	if sessionReply.Error != nil {
		t.Fatalf("new-session error: %s", *sessionReply.Error)
	}
	session := sessionReply.Result.(map[string]any)
	sessionID := session["sessionId"].(string)
	t.Logf("trace reply app-chat:new-session sessionId=%s agent=%s", sessionID, session["agent"])

	client.invoke(t, 2, "job:start", map[string]any{"kind": "app-chat", "chatId": "chat-wire-steer", "sessionId": sessionID, "tabId": "wire-steer-tab", "prompt": basePrompt})
	startReply := client.waitReply(t, 2, 5*time.Second)
	if startReply.Error != nil {
		t.Fatalf("job:start error: %s", *startReply.Error)
	}
	job := startReply.Result.(map[string]any)
	jobID := job["id"].(string)
	_ = client.waitJobEvent(t, jobID, "start", 5*time.Second)
	_ = client.waitJobEvent(t, jobID, "acp", 5*time.Second)
	t.Logf("trace event job:event type=acp id=%s before steer", jobID)

	client.invoke(t, 3, "app-chat:steer", map[string]any{
		"sessionId": sessionID, "prompt": "wire follow-up steer", "clientUserMessageId": "wire-steer-user",
		"continuationAssistantMessageId": "wire-steer-tail",
		"boundary":                       map[string]any{"assistantMessageId": "client-a", "deferUntilConsumed": true},
	})
	steerReply := client.waitReply(t, 3, 5*time.Second)
	if steerReply.Error != nil {
		t.Fatalf("app-chat:steer error: %s", *steerReply.Error)
	}
	steerResult := steerReply.Result.(map[string]any)
	if steerResult["ok"] != true || steerResult["live"] != true || steerResult["queued"] != false {
		t.Fatalf("steer reply = %#v", steerResult)
	}
	t.Logf("trace reply app-chat:steer ok=%v live=%v queued=%v", steerResult["ok"], steerResult["live"], steerResult["queued"])

	client.invoke(t, 4, "session:get")
	sessionStateReply := client.waitReply(t, 4, 5*time.Second)
	if sessionStateReply.Error != nil {
		t.Fatalf("session:get after steer: %s", *sessionStateReply.Error)
	}
	steerChat := chatFromSnapshot(mapFromAnyMain(sessionStateReply.Result), "wire-steer-tab")
	var persistedSteer map[string]any
	for _, raw := range messageSlice(steerChat) {
		message := mapFromAnyMain(raw)
		if fieldString(message, "id") == "wire-steer-user" {
			persistedSteer = message
			break
		}
	}
	if persistedSteer == nil || fieldString(persistedSteer, "status") != "done" || fieldString(persistedSteer, "steerState") != "accepted" || persistedSteer["steerAnchor"] != nil || fieldString(persistedSteer, "steerBoundary") != "" || fieldString(persistedSteer, "turnRootId") != "client-a" {
		t.Fatalf("daemon-owned steer acknowledgement = %#v", persistedSteer)
	}
	if messages := messageSlice(steerChat); fieldString(mapFromAnyMain(messages[len(messages)-1]), "id") != "wire-steer-tail" || fieldString(mapFromAnyMain(messages[len(messages)-1]), "status") != "running" {
		t.Fatalf("receipt-less adapter did not commit at acknowledgement: %#v", messages)
	}

	endEvent := client.waitJobEvent(t, jobID, "end", 5*time.Second)
	endJob := endEvent["job"].(map[string]any)
	if endJob["status"] != "done" || !strings.Contains(fmt.Sprint(endJob["result"]), "Steer input: wire follow-up steer.") {
		t.Fatalf("steered end job = %#v", endJob)
	}
	t.Logf("trace event job:event type=end id=%s status=%s result=%q", endJob["id"], endJob["status"], endJob["result"])
	archive := loadChatArchive(stateDir, "wire-steer-tab")
	steerIndex := -1
	for index, raw := range archive {
		if fieldString(mapFromAnyMain(raw), "id") == "wire-steer-user" {
			steerIndex = index
			break
		}
	}
	if steerIndex <= 0 || steerIndex >= len(archive)-1 || fieldString(mapFromAnyMain(archive[steerIndex+1]), "role") != "assistant" {
		t.Fatalf("wire steer archive order = %#v", archive)
	}
}

func TestWireTraceMockAutoCompaction(t *testing.T) {
	root := repoRoot(t)
	renderer := t.TempDir()
	if err := os.WriteFile(filepath.Join(renderer, "index.html"), []byte("<!doctype html><head></head><body></body>"), 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}
	traceFile := filepath.Join(t.TempDir(), "mock-prompts.jsonl")

	hub := wire.NewHub()
	manager := acp.NewManager(acp.Options{
		RootDir:                 root,
		Provider:                acp.ProviderConfig{ID: "mock", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")}, CWD: root, Env: map[string]string{"WORKASS_MOCK_ACP_TRACE_FILE": traceFile, "WORKASS_MOCK_ACP_DELAY_MS": "5"}, Enabled: true, Label: "Workass Mock ACP"},
		DefaultProviderID:       "mock",
		Broadcast:               hub.Broadcast,
		StdoutFlushInterval:     5 * time.Millisecond,
		ThoughtFlushInterval:    5 * time.Millisecond,
		RSSSampleInterval:       time.Hour,
		CompactionEnabled:       true,
		CompactionThresholdPct:  80,
		CompactionKeepLastTurns: 1,
	})
	t.Cleanup(func() { manager.Reset() })
	registerDaemonHandlers(hub, root, manager)

	server := httptest.NewServer(httpserve.New(renderer, hub, nil))
	defer server.Close()
	client := dialTestWS(t, server.URL)
	defer client.conn.Close()

	tabID := "wire-compact-tab"
	client.invoke(t, 1, "app-chat:new-session", map[string]any{"tabId": tabID, "chatId": "wire-compact-turn"})
	sessionReply := client.waitReply(t, 1, 5*time.Second)
	if sessionReply.Error != nil {
		t.Fatalf("new-session error: %s", *sessionReply.Error)
	}
	session := sessionReply.Result.(map[string]any)
	oldSessionID := session["sessionId"].(string)
	t.Logf("trace reply app-chat:new-session tab=%s session=%s", tabID, oldSessionID)

	history := []any{
		map[string]any{"role": "user", "content": "turno anterior usuario", "at": "2026-07-10T00:00:00Z"},
		map[string]any{"role": "assistant", "content": "turno anterior asistente", "at": "2026-07-10T00:00:01Z"},
	}
	client.invoke(t, 2, "job:start", map[string]any{"kind": "app-chat", "chatId": "wire-compact-turn", "sessionId": oldSessionID, "tabId": tabID, "prompt": "[mock:bigusage] compact this", "history": history})
	startReply := client.waitReply(t, 2, 5*time.Second)
	if startReply.Error != nil {
		t.Fatalf("job:start error: %s", *startReply.Error)
	}
	job := startReply.Result.(map[string]any)
	jobID := job["id"].(string)
	usage := client.waitFor(t, 5*time.Second, func(msg wsMessage) bool {
		if msg.T != "event" || msg.Channel != "job:event" {
			return false
		}
		payload, _ := msg.Payload.(map[string]any)
		return payload["type"] == "usage" && payload["sessionId"] == oldSessionID
	}).Payload.(map[string]any)
	if usage["used"] != json.Number("85") || usage["size"] != json.Number("100") || usage["inputTokens"] == nil {
		t.Fatalf("normalized usage payload = %#v", usage)
	}
	t.Logf("trace event job:event type=usage sessionId=%s used=%v size=%v usedPct=%v inputTokens=%v", usage["sessionId"], usage["used"], usage["size"], usage["usedPct"], usage["inputTokens"])

	compacted := client.waitChannelEvent(t, "chat:compacted", 10*time.Second).Payload.(map[string]any)
	newSessionID := fmt.Sprint(compacted["sessionId"])
	if newSessionID == "" || newSessionID == oldSessionID || compacted["usedPct"] != json.Number("85") || compacted["summaryChars"] == nil || compacted["keptTurns"] != json.Number("1") {
		t.Fatalf("chat:compacted payload = %#v oldSession=%s", compacted, oldSessionID)
	}
	t.Logf("trace event chat:compacted old=%s new=%s usedPct=%v summaryChars=%v keptTurns=%v", oldSessionID, newSessionID, compacted["usedPct"], compacted["summaryChars"], compacted["keptTurns"])
	end := client.waitJobEvent(t, jobID, "end", 5*time.Second)
	assertWireJobStatus(t, end, "done", "end_turn")

	client.invoke(t, 3, "job:start", map[string]any{"kind": "app-chat", "chatId": "wire-compact-turn", "sessionId": newSessionID, "tabId": tabID, "prompt": "after compact"})
	nextReply := client.waitReply(t, 3, 5*time.Second)
	if nextReply.Error != nil {
		t.Fatalf("next job:start error: %s", *nextReply.Error)
	}
	nextJob := nextReply.Result.(map[string]any)
	nextEnd := client.waitJobEvent(t, nextJob["id"].(string), "end", 5*time.Second)
	assertWireJobStatus(t, nextEnd, "done", "end_turn")
	t.Logf("trace next turn on compacted session session=%s status=%s", newSessionID, nextEnd["job"].(map[string]any)["status"])

	prompts := readMockTracePrompts(t, traceFile)
	seedFound := false
	for _, prompt := range prompts {
		if prompt["sessionId"] == newSessionID && strings.Contains(prompt["text"], "DETERMINISTIC WORKASS MOCK SUMMARY") && strings.Contains(prompt["text"], "recent_turns_verbatim") {
			seedFound = true
			break
		}
	}
	if !seedFound {
		t.Fatalf("mock did not receive compacted seed for %s; prompts=%#v", newSessionID, prompts)
	}
	t.Logf("trace mock prompt seed asserted session=%s prompts=%d", newSessionID, len(prompts))
}

func TestWireTraceMockEngineCrashRecovery(t *testing.T) {
	root := repoRoot(t)
	renderer := t.TempDir()
	if err := os.WriteFile(filepath.Join(renderer, "index.html"), []byte("<!doctype html><head></head><body></body>"), 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}

	hub := wire.NewHub()
	manager := acp.NewManager(acp.Options{
		RootDir:              root,
		Provider:             acp.ProviderConfig{ID: "mock", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")}, CWD: root, Env: map[string]string{"WORKASS_MOCK_ACP_DELAY_MS": "5"}, Enabled: true, Label: "Workass Mock ACP"},
		DefaultProviderID:    "mock",
		Broadcast:            hub.Broadcast,
		StdoutFlushInterval:  5 * time.Millisecond,
		ThoughtFlushInterval: 5 * time.Millisecond,
		RSSSampleInterval:    time.Hour,
		CrashRecoveryBackoff: 20 * time.Millisecond,
		CompactionEnabled:    false,
	})
	t.Cleanup(func() { manager.Reset() })
	registerDaemonHandlers(hub, root, manager)

	server := httptest.NewServer(httpserve.New(renderer, hub, nil))
	defer server.Close()
	client := dialTestWS(t, server.URL)
	defer client.conn.Close()

	tabID := "wire-crash-tab"
	client.invoke(t, 1, "app-chat:new-session", map[string]any{"tabId": tabID, "chatId": "wire-crash-turn"})
	session := client.waitReply(t, 1, 5*time.Second).Result.(map[string]any)
	oldSessionID := session["sessionId"].(string)
	client.invoke(t, 2, "job:start", map[string]any{"kind": "app-chat", "chatId": "wire-crash-turn", "sessionId": oldSessionID, "tabId": tabID, "prompt": "[mock:crash] first crash"})
	startReply := client.waitReply(t, 2, 5*time.Second)
	if startReply.Error != nil {
		t.Fatalf("job:start error: %s", *startReply.Error)
	}
	job := startReply.Result.(map[string]any)
	jobID := job["id"].(string)
	system := client.waitFor(t, 5*time.Second, func(msg wsMessage) bool {
		if msg.T != "event" || msg.Channel != "job:event" {
			return false
		}
		payload, _ := msg.Payload.(map[string]any)
		return payload["type"] == "data" && payload["id"] == jobID && payload["stream"] == "system" && strings.Contains(fmt.Sprint(payload["chunk"]), "[engine reiniciado")
	}).Payload.(map[string]any)
	t.Logf("trace event job:event type=data stream=system chunk=%q", system["chunk"])
	recovered := client.waitChannelEvent(t, "chat:engine-recovered", 5*time.Second).Payload.(map[string]any)
	newSessionID := fmt.Sprint(recovered["sessionId"])
	if newSessionID == "" || newSessionID == oldSessionID || recovered["oldSessionId"] != oldSessionID {
		t.Fatalf("chat:engine-recovered payload = %#v old=%s", recovered, oldSessionID)
	}
	t.Logf("trace event chat:engine-recovered old=%s new=%s tab=%v", oldSessionID, newSessionID, recovered["tabId"])
	end := client.waitJobEvent(t, jobID, "end", 5*time.Second)
	endJob := end["job"].(map[string]any)
	if endJob["status"] != "failed" || endJob["stopReason"] != "engine-crash" || endJob["crashInterrupted"] != true {
		t.Fatalf("crash end job = %#v", endJob)
	}
	t.Logf("trace event job:event type=end id=%s status=%s stopReason=%v crashInterrupted=%v", endJob["id"], endJob["status"], endJob["stopReason"], endJob["crashInterrupted"])

	client.invoke(t, 3, "job:start", map[string]any{"kind": "app-chat", "chatId": "wire-crash-turn", "sessionId": newSessionID, "tabId": tabID, "prompt": "after recovery"})
	nextReply := client.waitReply(t, 3, 5*time.Second)
	if nextReply.Error != nil {
		t.Fatalf("next job:start error: %s", *nextReply.Error)
	}
	nextJob := nextReply.Result.(map[string]any)
	nextEnd := client.waitJobEvent(t, nextJob["id"].(string), "end", 5*time.Second)
	assertWireJobStatus(t, nextEnd, "done", "end_turn")
	t.Logf("trace next turn after recovery session=%s status=%s", newSessionID, nextEnd["job"].(map[string]any)["status"])
}

func TestWireTraceMockDoubleCrashSurfacesError(t *testing.T) {
	root := repoRoot(t)
	renderer := t.TempDir()
	if err := os.WriteFile(filepath.Join(renderer, "index.html"), []byte("<!doctype html><head></head><body></body>"), 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}

	hub := wire.NewHub()
	manager := acp.NewManager(acp.Options{
		RootDir:              root,
		Provider:             acp.ProviderConfig{ID: "mock", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")}, CWD: root, Env: map[string]string{"WORKASS_MOCK_ACP_DELAY_MS": "5"}, Enabled: true, Label: "Workass Mock ACP"},
		DefaultProviderID:    "mock",
		Broadcast:            hub.Broadcast,
		StdoutFlushInterval:  5 * time.Millisecond,
		ThoughtFlushInterval: 5 * time.Millisecond,
		RSSSampleInterval:    time.Hour,
		CrashRecoveryBackoff: 20 * time.Millisecond,
		CompactionEnabled:    false,
	})
	t.Cleanup(func() { manager.Reset() })
	registerDaemonHandlers(hub, root, manager)

	server := httptest.NewServer(httpserve.New(renderer, hub, nil))
	defer server.Close()
	client := dialTestWS(t, server.URL)
	defer client.conn.Close()

	tabID := "wire-double-crash-tab"
	client.invoke(t, 1, "app-chat:new-session", map[string]any{"tabId": tabID, "chatId": tabID})
	session := client.waitReply(t, 1, 5*time.Second).Result.(map[string]any)
	firstSessionID := session["sessionId"].(string)
	client.invoke(t, 2, "job:start", map[string]any{"kind": "app-chat", "chatId": tabID, "sessionId": firstSessionID, "tabId": tabID, "prompt": "[mock:crash] first"})
	firstJob := client.waitReply(t, 2, 5*time.Second).Result.(map[string]any)
	firstRecovered := client.waitChannelEvent(t, "chat:engine-recovered", 5*time.Second).Payload.(map[string]any)
	secondSessionID := fmt.Sprint(firstRecovered["sessionId"])
	firstEnd := client.waitJobEvent(t, firstJob["id"].(string), "end", 5*time.Second)
	if firstEnd["job"].(map[string]any)["stopReason"] != "engine-crash" {
		t.Fatalf("first crash end = %#v", firstEnd)
	}
	t.Logf("trace first crash recovered old=%s new=%s", firstSessionID, secondSessionID)

	client.invoke(t, 3, "job:start", map[string]any{"kind": "app-chat", "chatId": tabID, "sessionId": secondSessionID, "tabId": tabID, "prompt": "[mock:crash] second"})
	secondJob := client.waitReply(t, 3, 5*time.Second).Result.(map[string]any)
	secondEnd := client.waitJobEvent(t, secondJob["id"].(string), "end", 5*time.Second)
	secondEndJob := secondEnd["job"].(map[string]any)
	if secondEndJob["status"] != "failed" || secondEndJob["stopReason"] != "engine-crash" || secondEndJob["crashInterrupted"] != true {
		t.Fatalf("second crash end job = %#v", secondEndJob)
	}
	client.expectNoEventChannel(t, "chat:engine-recovered", 300*time.Millisecond)
	t.Logf("trace second crash surfaced status=%s stopReason=%v no-auto-recovery=true", secondEndJob["status"], secondEndJob["stopReason"])
}

func TestConfigAndSettingsPersistInStateDir(t *testing.T) {
	root := repoRoot(t)
	stateDir := filepath.Join(t.TempDir(), "profile", "state")
	hub := wire.NewHub()
	engine := engineConfig{
		HibernateTTL:            1500 * time.Millisecond,
		MaxRSSKB:                12345,
		MaxAge:                  2500 * time.Millisecond,
		SpareSessions:           1,
		RSSSampleInterval:       750 * time.Millisecond,
		CompactionEnabled:       true,
		CompactionThresholdPct:  80,
		CompactionKeepLastTurns: 4,
	}
	registerDaemonHandlers(hub, root, nil, daemonOptions{StateDir: stateDir, Engine: engine})

	gotConfig, err := hub.Invoke("config:get", nil)
	if err != nil {
		t.Fatalf("config:get: %v", err)
	}
	initialEngine := gotConfig.(map[string]any)["engine"].(map[string]any)
	if initialEngine["hibernateTtlMs"] != int64(1500) && initialEngine["hibernateTtlMs"] != 1500 {
		t.Fatalf("initial engine = %#v", initialEngine)
	}
	configPath := filepath.Join(filepath.Dir(stateDir), "app-config.json")
	if _, err := os.Stat(configPath); err != nil {
		t.Fatalf("app-config.json not persisted beside state dir: %v", err)
	}

	patch := map[string]any{
		"chat":     map[string]any{"defaultModel": "mock-deterministic"},
		"ui":       map[string]any{"density": "comfortable"},
		"lan":      map[string]any{"port": 9090},
		"agentApi": map[string]any{"enabled": false},
		"engine":   map[string]any{"hibernateTtlMs": 2500, "maxRssKb": 54321, "maxAgeMs": 3500, "spareSessions": 2, "rssSampleIntervalMs": 1250, "compactionEnabled": false, "compactionThresholdPct": 75, "compactionKeepLastTurns": 6},
		"integrations": map[string]any{
			"teams": map[string]any{"graphToken": "super-secret-token"},
		},
	}
	setConfig, err := hub.Invoke("config:set", []any{patch})
	if err != nil {
		t.Fatalf("config:set: %v", err)
	}
	publicConfig := setConfig.(map[string]any)
	if publicConfig["chat"].(map[string]any)["defaultModel"] != "mock-deterministic" || publicConfig["agentApi"].(map[string]any)["enabled"] != false {
		t.Fatalf("config:set result = %#v", publicConfig)
	}
	if publicConfig["integrations"].(map[string]any)["teams"].(map[string]any)["graphToken"] != "[redacted]" {
		t.Fatalf("config secret not redacted: %#v", publicConfig["integrations"])
	}
	if publicConfig["engine"].(map[string]any)["maxRssKb"] != 54321 || publicConfig["engine"].(map[string]any)["compactionEnabled"] != false || publicConfig["engine"].(map[string]any)["compactionThresholdPct"] != 75 || publicConfig["engine"].(map[string]any)["compactionKeepLastTurns"] != 6 {
		t.Fatalf("config engine result = %#v", publicConfig["engine"])
	}
	rawConfig := readJSONFile(t, configPath)
	if rawConfig["integrations"].(map[string]any)["teams"].(map[string]any)["graphToken"] != "super-secret-token" {
		t.Fatalf("config file did not preserve raw secret value: %#v", rawConfig["integrations"])
	}

	settingsInput := map[string]any{
		"models":             map[string]any{"chat": "chat-model", "consulta": "consulta-model"},
		"permissionModes":    map[string]any{"consulta": "auto", "review": "dangerous", "chat": "dangerous"},
		"chatMode":           "ask",
		"autoProcessEnabled": false,
		"notifications":      "all",
		"prApprover":         " reviewer ",
		"modelScores": map[string]any{
			"codex": map[string]any{"gpt-test": map[string]any{"intelligence": 12, "taste": 7, "cost": -3, "note": "my ranking"}},
		},
		"modelFavorites": []any{
			map[string]any{"providerId": " codex ", "modelId": " gpt-5.6-sol "},
			map[string]any{"providerId": "codex", "modelId": "gpt-5.6-sol"},
			map[string]any{"providerId": "", "modelId": "ignored"},
		},
		"ignored": "not persisted",
	}
	setSettings, err := hub.Invoke("settings:set", []any{settingsInput})
	if err != nil {
		t.Fatalf("settings:set: %v", err)
	}
	settings := setSettings.(map[string]any)
	if settings["chatMode"] != "ask" || settings["notifications"] != "all" || settings["prApprover"] != "reviewer" {
		t.Fatalf("settings result = %#v", settings)
	}
	settingsPath := filepath.Join(stateDir, "app-settings.json")
	rawSettings := readJSONFile(t, settingsPath)
	if _, ok := rawSettings["ignored"]; ok {
		t.Fatalf("settings persisted unknown field: %#v", rawSettings)
	}
	if rawSettings["models"].(map[string]any)["chat"] != "chat-model" || rawSettings["permissionModes"].(map[string]any)["chat"] != nil {
		t.Fatalf("settings file subset = %#v", rawSettings)
	}
	modelScore := rawSettings["modelScores"].(map[string]any)["codex"].(map[string]any)["gpt-test"].(map[string]any)
	if fmt.Sprint(modelScore["intelligence"]) != "10" || fmt.Sprint(modelScore["taste"]) != "7" || fmt.Sprint(modelScore["cost"]) != "1" || modelScore["note"] != "my ranking" {
		t.Fatalf("normalized model score = %#v", modelScore)
	}
	favorites := rawSettings["modelFavorites"].([]any)
	if len(favorites) != 1 || fieldString(mapFromAnyMain(favorites[0]), "providerId") != "codex" || fieldString(mapFromAnyMain(favorites[0]), "modelId") != "gpt-5.6-sol" {
		t.Fatalf("normalized model favorites = %#v", favorites)
	}
	t.Logf("trace config persisted path=%s engine=%#v settingsPath=%s", configPath, publicConfig["engine"], settingsPath)
}

func TestWireTraceChatEnvNumstat(t *testing.T) {
	requireGitBinary(t)
	root := repoRoot(t)
	renderer := t.TempDir()
	if err := os.WriteFile(filepath.Join(renderer, "index.html"), []byte("<!doctype html><head></head><body></body>"), 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}
	workspace := t.TempDir()
	alpha := filepath.Join(workspace, "alpha")
	beta := filepath.Join(workspace, "beta")
	initWireGitRepo(t, alpha, map[string]string{"work.txt": "one\n"})
	initWireGitRepo(t, beta, map[string]string{"other.txt": "base\n"})
	baseHash := wireFileSHA256(t, filepath.Join(alpha, "work.txt"))

	hub := wire.NewHub()
	manager := acp.NewManager(acp.Options{
		RootDir:             root,
		StateDir:            filepath.Join(t.TempDir(), "state"),
		Provider:            acp.ProviderConfig{Command: os.Args[0], Args: []string{"-test.run=TestWireFakeACPHelper", "--"}, CWD: root, Env: map[string]string{"WORKASS_WIRE_FAKE_ACP": "1"}, Label: "Wire Fake ACP"},
		Broadcast:           hub.Broadcast,
		InitTimeout:         2 * time.Second,
		StdoutFlushInterval: 10 * time.Millisecond,
		RSSSampleInterval:   time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	registerDaemonHandlers(hub, root, manager)

	server := httptest.NewServer(httpserve.New(renderer, hub, nil))
	defer server.Close()
	client := dialTestWS(t, server.URL)
	defer client.conn.Close()

	chatID := "chat-wire-env"
	tabID := "wire-env-tab"
	client.invoke(t, 1, "app-chat:new-session", map[string]any{"tabId": tabID, "chatId": chatID, "cwd": workspace})
	sessionReply := client.waitReply(t, 1, 5*time.Second)
	if sessionReply.Error != nil {
		t.Fatalf("new-session error: %s", *sessionReply.Error)
	}
	session := sessionReply.Result.(map[string]any)
	sessionID := session["sessionId"].(string)
	created := client.waitChannelEvent(t, "chat:env", 5*time.Second).Payload.(map[string]any)
	if created["chatId"] != chatID || created["tabId"] != tabID || filepath.Clean(fmt.Sprint(created["cwd"])) != filepath.Clean(workspace) || len(created["repos"].([]any)) != 0 || strings.Join(stringSliceFromAny(created["unchanged"]), ",") != "alpha,beta" {
		t.Fatalf("session chat:env = %#v", created)
	}
	t.Logf("trace event chat:env session-create chatId=%s tabId=%s cwd=%s repos=%d unchanged=%s", created["chatId"], created["tabId"], created["cwd"], len(created["repos"].([]any)), strings.Join(stringSliceFromAny(created["unchanged"]), ","))

	client.invoke(t, 2, "job:start", map[string]any{"kind": "app-chat", "chatId": chatID, "sessionId": sessionID, "tabId": tabID, "cwd": workspace, "prompt": "wire env turn"})
	startReply := client.waitReply(t, 2, 5*time.Second)
	if startReply.Error != nil {
		t.Fatalf("job:start error: %s", *startReply.Error)
	}
	job := startReply.Result.(map[string]any)
	jobID := job["id"].(string)
	if err := os.WriteFile(filepath.Join(alpha, "work.txt"), []byte("one\ntwo\nthree\n"), 0o644); err != nil {
		t.Fatalf("edit alpha: %v", err)
	}
	endEvent := client.waitJobEvent(t, jobID, "end", 5*time.Second)
	endJob := endEvent["job"].(map[string]any)
	if endJob["status"] != "done" || endJob["stopReason"] != "end_turn" {
		t.Fatalf("end job = %#v", endJob)
	}
	envMsg := client.waitFor(t, 5*time.Second, func(msg wsMessage) bool {
		if msg.T != "event" || msg.Channel != "chat:env" {
			return false
		}
		payload, _ := msg.Payload.(map[string]any)
		repos, _ := payload["repos"].([]any)
		if len(repos) != 1 {
			return false
		}
		repo, _ := repos[0].(map[string]any)
		return repo["name"] == "alpha"
	})
	env := envMsg.Payload.(map[string]any)
	repo := env["repos"].([]any)[0].(map[string]any)
	files := repo["files"].([]any)
	file := files[0].(map[string]any)
	if repo["adds"] != json.Number("2") || repo["dels"] != json.Number("0") || file["path"] != "work.txt" || file["adds"] != json.Number("2") || file["dels"] != json.Number("0") || strings.Join(stringSliceFromAny(env["unchanged"]), ",") != "beta" {
		t.Fatalf("turn chat:env = %#v", env)
	}
	t.Logf("trace event chat:env after-turn chatId=%s repo=%s branch=%s file=%s adds=%v dels=%v unchanged=%s", env["chatId"], repo["name"], repo["branch"], file["path"], file["adds"], file["dels"], strings.Join(stringSliceFromAny(env["unchanged"]), ","))

	client.invoke(t, 3, "chat:env-get", map[string]any{"chatId": chatID})
	getReply := client.waitReply(t, 3, 5*time.Second)
	if getReply.Error != nil {
		t.Fatalf("chat:env-get error: %s", *getReply.Error)
	}
	got := getReply.Result.(map[string]any)
	gotRepo := got["repos"].([]any)[0].(map[string]any)
	if gotRepo["name"] != "alpha" || gotRepo["adds"] != json.Number("2") {
		t.Fatalf("chat:env-get result = %#v", got)
	}
	t.Logf("trace reply chat:env-get chatId=%s repos=%d firstRepo=%s adds=%v", got["chatId"], len(got["repos"].([]any)), gotRepo["name"], gotRepo["adds"])

	client.invoke(t, 4, "chat:checkpoints", map[string]any{"chatId": chatID})
	cpReply := client.waitReply(t, 4, 5*time.Second)
	if cpReply.Error != nil {
		t.Fatalf("chat:checkpoints error: %s", *cpReply.Error)
	}
	checkpoints := cpReply.Result.([]any)
	if len(checkpoints) != 1 {
		t.Fatalf("chat:checkpoints result = %#v", checkpoints)
	}
	cp := checkpoints[0].(map[string]any)
	repos := cp["repos"].([]any)
	cpRepo := repos[0].(map[string]any)
	if cp["turnSeq"] != json.Number("1") || cpRepo["name"] != "alpha" || (cpRepo["skipped"] != nil && cpRepo["skipped"] != false) || cpRepo["ref"] == "" {
		t.Fatalf("chat:checkpoints checkpoint = %#v", cp)
	}
	t.Logf("trace reply chat:checkpoints turnSeq=%v repo=%s ref=%s", cp["turnSeq"], cpRepo["name"], cpRepo["ref"])

	client.invoke(t, 5, "chat:diff", map[string]any{"chatId": chatID, "repo": "alpha", "path": "work.txt"})
	diffReply := client.waitReply(t, 5, 5*time.Second)
	if diffReply.Error != nil {
		t.Fatalf("chat:diff error: %s", *diffReply.Error)
	}
	diff := diffReply.Result.(map[string]any)
	diffText := diff["text"].(string)
	if diff["truncated"] != false || !strings.Contains(diffText, "+two") || !strings.Contains(diffText, "+three") {
		t.Fatalf("chat:diff result = %#v", diff)
	}
	t.Logf("trace reply chat:diff turnSeq=%v hunk=%t truncated=%v", diff["turnSeq"], strings.Contains(diffText, "@@"), diff["truncated"])

	client.invoke(t, 6, "chat:rewind", map[string]any{"chatId": chatID, "turnSeq": 1})
	rewindReply := client.waitReply(t, 6, 5*time.Second)
	if rewindReply.Error != nil {
		t.Fatalf("chat:rewind error: %s", *rewindReply.Error)
	}
	restored := client.waitChannelEvent(t, "chat:checkpoint-restored", 5*time.Second).Payload.(map[string]any)
	if restored["chatId"] != chatID || restored["turnSeq"] != json.Number("1") {
		t.Fatalf("chat:checkpoint-restored = %#v", restored)
	}
	if gotHash := wireFileSHA256(t, filepath.Join(alpha, "work.txt")); gotHash != baseHash {
		t.Fatalf("rewind hash=%s want=%s", gotHash, baseHash)
	}
	t.Logf("trace event chat:checkpoint-restored chatId=%s turnSeq=%v hash=%s", restored["chatId"], restored["turnSeq"], baseHash)
}

func TestWireTraceHibernatedCheckpointKeepsTurnBaseline(t *testing.T) {
	requireGitBinary(t)
	repo := repoRoot(t)
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	renderer := t.TempDir()
	if err := os.WriteFile(filepath.Join(renderer, "index.html"), []byte("<!doctype html><head></head><body></body>"), 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}
	workspace := filepath.Join(root, "workspace")
	alpha := filepath.Join(workspace, "alpha")
	initWireGitRepo(t, alpha, map[string]string{"work.txt": "one\n"})

	hub := wire.NewHub()
	manager := acp.NewManager(acp.Options{
		RootDir:  repo,
		StateDir: stateDir,
		Provider: acp.ProviderConfig{
			ID:      "mock",
			Command: "node",
			Args:    []string{filepath.Join(repo, "desktop", "acp", "mock-server.mjs")},
			CWD:     repo,
			Env:     map[string]string{"WORKASS_MOCK_ACP_DELAY_MS": "10"},
			Enabled: true,
			Label:   "Workass Mock ACP",
		},
		DefaultProviderID:      "mock",
		Broadcast:              hub.Broadcast,
		StdoutFlushInterval:    10 * time.Millisecond,
		ThoughtFlushInterval:   10 * time.Millisecond,
		HibernateTTL:           60 * time.Millisecond,
		LifecycleCheckInterval: 10 * time.Millisecond,
		RSSSampleInterval:      time.Hour,
		CompactionEnabled:      false,
	})
	t.Cleanup(func() { manager.Reset() })
	registerDaemonHandlers(hub, repo, manager, daemonOptions{StateDir: stateDir})

	server := httptest.NewServer(httpserve.New(renderer, hub, nil))
	defer server.Close()
	client := dialTestWS(t, server.URL)
	defer client.conn.Close()

	chatID := "chat-wire-hibernate-checkpoint"
	tabID := "wire-hibernate-checkpoint-tab"
	client.invoke(t, 1, "app-chat:new-session", map[string]any{"tabId": tabID, "chatId": chatID, "cwd": workspace})
	sessionReply := client.waitReply(t, 1, 5*time.Second)
	if sessionReply.Error != nil {
		t.Fatalf("new-session error: %s", *sessionReply.Error)
	}
	session := sessionReply.Result.(map[string]any)
	oldSessionID := session["sessionId"].(string)
	runWireChatTurn(t, client, 2, chatID, tabID, oldSessionID, "before hibernate checkpoint")
	hibernated := waitWireProcStateForChat(t, manager, chatID, acp.StateHibernated, 2*time.Second)
	t.Logf("trace lifecycle hibernated chat=%s pid=%v", chatID, hibernated["pid"])

	client.invoke(t, 3, "job:start", map[string]any{"kind": "app-chat", "chatId": chatID, "sessionId": oldSessionID, "tabId": tabID, "cwd": workspace, "prompt": "[mock:slow] hibernated checkpoint"})
	startReply := client.waitReply(t, 3, 5*time.Second)
	if startReply.Error != nil {
		t.Fatalf("job:start error: %s", *startReply.Error)
	}
	job := startReply.Result.(map[string]any)
	jobID := job["id"].(string)
	if err := os.WriteFile(filepath.Join(alpha, "work.txt"), []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatalf("edit alpha: %v", err)
	}
	end := client.waitJobEvent(t, jobID, "end", 5*time.Second)
	endJob := end["job"].(map[string]any)
	if endJob["status"] != "done" || endJob["sessionId"] == oldSessionID {
		t.Fatalf("hibernated checkpoint end = %#v", endJob)
	}
	envMsg := client.waitFor(t, 5*time.Second, func(msg wsMessage) bool {
		if msg.T != "event" || msg.Channel != "chat:env" {
			return false
		}
		payload, _ := msg.Payload.(map[string]any)
		return payload["chatId"] == chatID && envHasWireRepoFile(payload, "alpha", "work.txt")
	})
	env := envMsg.Payload.(map[string]any)
	t.Logf("trace event chat:env hibernated checkpoint chatId=%s repos=%d", env["chatId"], len(env["repos"].([]any)))

	client.invoke(t, 4, "chat:checkpoints", map[string]any{"chatId": chatID})
	cpReply := client.waitReply(t, 4, 5*time.Second)
	if cpReply.Error != nil {
		t.Fatalf("chat:checkpoints error: %s", *cpReply.Error)
	}
	checkpoints := cpReply.Result.([]any)
	if len(checkpoints) != 1 {
		t.Fatalf("checkpoints after hibernated turn = %#v", checkpoints)
	}
	latest := checkpoints[len(checkpoints)-1].(map[string]any)
	if latest["turnSeq"] != json.Number("2") {
		t.Fatalf("latest checkpoint = %#v", latest)
	}
	client.invoke(t, 5, "chat:diff", map[string]any{"chatId": chatID, "repo": "alpha", "path": "work.txt"})
	diffReply := client.waitReply(t, 5, 5*time.Second)
	if diffReply.Error != nil {
		t.Fatalf("chat:diff error: %s", *diffReply.Error)
	}
	diff := diffReply.Result.(map[string]any)
	if !strings.Contains(fmt.Sprint(diff["text"]), "+two") {
		t.Fatalf("hibernated diff = %#v", diff)
	}
	t.Logf("trace reply chat:diff hibernated turnSeq=%v", diff["turnSeq"])
}

func TestWireTraceGroupedCatalogAndInterleavedProviders(t *testing.T) {
	root := repoRoot(t)
	renderer := t.TempDir()
	if err := os.WriteFile(filepath.Join(renderer, "index.html"), []byte("<!doctype html><head></head><body></body>"), 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}

	hub := wire.NewHub()
	manager := acp.NewManager(acp.Options{
		RootDir: root,
		Providers: []acp.ProviderConfig{
			{ID: "mock", Name: "Mock Provider", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")}, CWD: root, Enabled: true},
			{ID: "fake-agent", Name: "Fake Provider", Command: os.Args[0], Args: []string{"-test.run=TestWireFakeACPHelper", "--"}, CWD: root, Env: map[string]string{"WORKASS_WIRE_FAKE_ACP": "1"}, Enabled: true},
		},
		DefaultProviderID:    "mock",
		Broadcast:            hub.Broadcast,
		InitTimeout:          2 * time.Second,
		StdoutFlushInterval:  10 * time.Millisecond,
		ThoughtFlushInterval: 10 * time.Millisecond,
		RSSSampleInterval:    time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	registerDaemonHandlers(hub, root, manager)
	manager.EmitCatalog(context.Background())

	server := httptest.NewServer(httpserve.New(renderer, hub, nil))
	defer server.Close()
	client := dialTestWS(t, server.URL)
	defer client.conn.Close()

	catalogMsg := client.waitChannelEvent(t, "chat:catalog", 5*time.Second)
	catalog := catalogMsg.Payload.(map[string]any)
	groups := catalogGroups(catalog)
	if len(groups) != 2 || catalog["models"] == nil || catalog["modes"] == nil {
		t.Fatalf("catalog payload = %#v", catalog)
	}
	if catalogGroup(groups, "mock") == nil || catalogGroup(groups, "fake-agent") == nil {
		t.Fatalf("catalog groups missing providers: %#v", groups)
	}
	t.Logf("trace event chat:catalog groups=%s legacyModels=%d legacyModes=%d", groupSummary(groups), len(catalog["models"].([]any)), len(catalog["modes"].([]any)))

	client.invoke(t, 1, "app-chat:new-session", map[string]any{"tabId": "wire-mock-tab", "chatId": "chat-wire-mock", "providerId": "mock"})
	client.invoke(t, 2, "app-chat:new-session", map[string]any{"tabId": "wire-fake-tab", "chatId": "chat-wire-fake", "providerId": "fake-agent"})
	mockSession := client.waitReply(t, 1, 5*time.Second).Result.(map[string]any)
	fakeSession := client.waitReply(t, 2, 5*time.Second).Result.(map[string]any)
	if mockSession["providerId"] != "mock" || fakeSession["providerId"] != "fake-agent" {
		t.Fatalf("session providers mock=%#v fake=%#v", mockSession, fakeSession)
	}
	t.Logf("trace reply app-chat:new-session chat=chat-wire-mock provider=%s session=%s", mockSession["providerId"], mockSession["sessionId"])
	t.Logf("trace reply app-chat:new-session chat=chat-wire-fake provider=%s session=%s", fakeSession["providerId"], fakeSession["sessionId"])

	client.invoke(t, 3, "job:start", map[string]any{"kind": "app-chat", "chatId": "chat-wire-mock", "sessionId": mockSession["sessionId"], "tabId": "wire-mock-tab", "prompt": "[mock:slow] interleaved mock"})
	client.invoke(t, 4, "job:start", map[string]any{"kind": "app-chat", "chatId": "chat-wire-fake", "sessionId": fakeSession["sessionId"], "tabId": "wire-fake-tab", "prompt": "interleaved fake"})
	mockStartReply := client.waitReply(t, 3, 5*time.Second)
	fakeStartReply := client.waitReply(t, 4, 5*time.Second)
	if mockStartReply.Error != nil || fakeStartReply.Error != nil {
		t.Fatalf("job start replies mock=%+v fake=%+v", mockStartReply, fakeStartReply)
	}
	mockJob := mockStartReply.Result.(map[string]any)
	fakeJob := fakeStartReply.Result.(map[string]any)
	mockJobID := mockJob["id"].(string)
	fakeJobID := fakeJob["id"].(string)
	if mockJob["providerId"] != "mock" || fakeJob["providerId"] != "fake-agent" {
		t.Fatalf("job providers mock=%#v fake=%#v", mockJob, fakeJob)
	}
	t.Logf("trace reply job:start chat=chat-wire-mock provider=%s job=%s", mockJob["providerId"], mockJobID)
	t.Logf("trace reply job:start chat=chat-wire-fake provider=%s job=%s", fakeJob["providerId"], fakeJobID)

	ended := map[string]bool{}
	results := map[string]string{}
	for len(ended) < 2 {
		msg := client.waitEvent(t, 6*time.Second)
		if msg.Channel != "job:event" {
			continue
		}
		payload := msg.Payload.(map[string]any)
		switch payload["type"] {
		case "start":
			job := payload["job"].(map[string]any)
			t.Logf("trace event job:event type=start chat=%s provider=%s job=%s", job["chatId"], job["providerId"], job["id"])
		case "data":
			t.Logf("trace event job:event type=data job=%s stream=%s chunk=%q", payload["id"], payload["stream"], payload["chunk"])
		case "acp":
			event := payload["event"].(map[string]any)
			t.Logf("trace event job:event type=acp job=%s kind=%s status=%v", payload["id"], event["kind"], event["status"])
		case "end":
			job := payload["job"].(map[string]any)
			id := job["id"].(string)
			ended[id] = true
			results[id] = fmt.Sprint(job["result"])
			t.Logf("trace event job:event type=end chat=%s provider=%s job=%s status=%s stopReason=%v", job["chatId"], job["providerId"], id, job["status"], job["stopReason"])
			if job["status"] != "done" || job["stopReason"] != "end_turn" {
				t.Fatalf("end job = %#v", job)
			}
		}
	}
	if !strings.Contains(results[mockJobID], "Mock ACP turn") ||
		!strings.Contains(results[fakeJobID], "Workass context:") ||
		!strings.Contains(results[fakeJobID], "User request:\ninterleaved fake") {
		t.Fatalf("provider-isolated results mock=%q fake=%q", results[mockJobID], results[fakeJobID])
	}
}

func TestWireProviderCatalogReplayToLateClient(t *testing.T) {
	root := repoRoot(t)
	renderer := t.TempDir()
	if err := os.WriteFile(filepath.Join(renderer, "index.html"), []byte("<!doctype html><head></head><body></body>"), 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}
	pathDir := t.TempDir()
	installWireFakeAgentWrapper(t, pathDir, "devin")
	t.Setenv("PATH", pathDir)

	hub := wire.NewHub()
	manager := acp.NewManager(acp.Options{
		RootDir: root,
		Providers: []acp.ProviderConfig{
			{ID: "devin", Name: "Devin ACP", Command: "devin", Args: []string{"acp"}, Enabled: false, Badge: "agent", CWD: root},
		},
		DefaultProviderID:   "devin",
		ProviderConfigFile:  filepath.Join(t.TempDir(), "providers.json"),
		Broadcast:           hub.Broadcast,
		InitTimeout:         800 * time.Millisecond,
		StdoutFlushInterval: 10 * time.Millisecond,
		RSSSampleInterval:   time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	registerDaemonHandlers(hub, root, manager)
	detected := manager.DetectProviders(context.Background(), acp.DetectOptions{})
	if detected["ok"] != true {
		t.Fatalf("detect providers = %#v", detected)
	}

	server := httptest.NewServer(httpserve.New(renderer, hub, nil))
	defer server.Close()
	client := dialTestWS(t, server.URL)
	defer client.conn.Close()

	providersMsg := client.waitChannelEvent(t, "providers:list", 2*time.Second)
	providers := providerItems(providersMsg.Payload)
	assertWireProvider(t, providers, "devin", "ready", true)
	t.Logf("trace replay providers:list %s", providerItemsSummary(providers))

	catalogMsg := client.waitChannelEvent(t, "chat:catalog", 2*time.Second)
	catalog := catalogMsg.Payload.(map[string]any)
	groups := catalogGroups(catalog)
	group := catalogGroup(groups, "devin")
	if group == nil || group["status"] != "ready" {
		t.Fatalf("replayed catalog missing ready devin group: %#v", catalog)
	}
	models, _ := group["models"].([]any)
	if len(models) == 0 {
		t.Fatalf("replayed devin catalog has no models: %#v", group)
	}
	t.Logf("trace replay chat:catalog %s", groupSummary(groups))
}

func TestWireClientReadyReplaysSessionRefresh(t *testing.T) {
	root := repoRoot(t)
	renderer := t.TempDir()
	if err := os.WriteFile(filepath.Join(renderer, "index.html"), []byte("<!doctype html><head></head><body></body>"), 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}
	stateDir := t.TempDir()
	hub := wire.NewHub()
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: stateDir,
		Broadcast:         hub.Broadcast,
		RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	registerDaemonHandlers(hub, root, manager, daemonOptions{StateDir: stateDir})

	client := dialTestWSHandler(t, httpserve.New(renderer, hub, nil), "/")
	defer client.conn.Close()

	msg := client.waitChannelEvent(t, "agent:apply", 2*time.Second)
	payload := mapFromAnyMain(msg.Payload)
	if payload["action"] != "session-refresh" {
		t.Fatalf("client-ready agent:apply = %#v, want session-refresh", payload)
	}
	t.Logf("trace replay agent:apply action=%s", payload["action"])
}

func TestWirePlanUsageReplayToLateClient(t *testing.T) {
	root := repoRoot(t)
	renderer := t.TempDir()
	if err := os.WriteFile(filepath.Join(renderer, "index.html"), []byte("<!doctype html><head></head><body></body>"), 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}

	hub := wire.NewHub()
	manager := acp.NewManager(acp.Options{
		RootDir: root,
		Providers: []acp.ProviderConfig{
			{ID: "claude", Name: "Claude Plan Fake", Command: os.Args[0], Args: []string{"-test.run=TestWireFakeACPHelper", "--"}, CWD: root, Env: map[string]string{"WORKASS_WIRE_FAKE_ACP": "1", "WORKASS_WIRE_FAKE_ACP_MODE": "plan-usage"}, Enabled: true},
		},
		DefaultProviderID:    "claude",
		Broadcast:            hub.Broadcast,
		InitTimeout:          2 * time.Second,
		StdoutFlushInterval:  10 * time.Millisecond,
		ThoughtFlushInterval: 10 * time.Millisecond,
		RSSSampleInterval:    time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	registerDaemonHandlers(hub, root, manager)

	server := httptest.NewServer(httpserve.New(renderer, hub, nil))
	defer server.Close()
	first := dialTestWS(t, server.URL)
	defer first.conn.Close()

	first.invoke(t, 1, "app-chat:new-session", map[string]any{"tabId": "plan-live-tab", "chatId": "chat-plan-live", "providerId": "claude"})
	session := first.waitReply(t, 1, 5*time.Second).Result.(map[string]any)
	first.invoke(t, 2, "job:start", map[string]any{"kind": "app-chat", "chatId": "chat-plan-live", "sessionId": session["sessionId"], "tabId": "plan-live-tab", "prompt": "emit plan usage"})
	startReply := first.waitReply(t, 2, 5*time.Second)
	if startReply.Error != nil {
		t.Fatalf("job:start error = %v", *startReply.Error)
	}
	livePlan := first.waitChannelEvent(t, "chat:plan-usage", 5*time.Second).Payload.(map[string]any)
	assertWirePlanUsageRateLimit(t, livePlan, "claude")
	jobID := startReply.Result.(map[string]any)["id"].(string)
	_ = first.waitJobEvent(t, jobID, "end", 5*time.Second)
	t.Logf("trace event chat:plan-usage live provider=%s entries=%d", livePlan["providerId"], len(livePlan["entries"].([]any)))

	late := dialTestWS(t, server.URL)
	defer late.conn.Close()
	replayed := late.waitChannelEvent(t, "chat:plan-usage", 2*time.Second).Payload.(map[string]any)
	assertWirePlanUsageRateLimit(t, replayed, "claude")
	t.Logf("trace replay chat:plan-usage provider=%s entries=%d", replayed["providerId"], len(replayed["entries"].([]any)))
}

func TestWireCodexEarnedRateLimitResetConsume(t *testing.T) {
	root := repoRoot(t)
	renderer := t.TempDir()
	if err := os.WriteFile(filepath.Join(renderer, "index.html"), []byte("<!doctype html><head></head><body></body>"), 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}
	hub := wire.NewHub()
	manager := acp.NewManager(acp.Options{
		RootDir: root,
		Providers: []acp.ProviderConfig{
			{ID: "codex", Name: "Codex Reset Fake", Command: os.Args[0], Args: []string{"-test.run=TestWireFakeACPHelper", "--"}, CWD: root, Env: map[string]string{"WORKASS_WIRE_FAKE_ACP": "1", "WORKASS_WIRE_FAKE_ACP_MODE": "codex-reset"}, Enabled: true},
		},
		DefaultProviderID:   "codex",
		Broadcast:           hub.Broadcast,
		InitTimeout:         2 * time.Second,
		RSSSampleInterval:   time.Hour,
		StdoutFlushInterval: 10 * time.Millisecond,
	})
	t.Cleanup(func() { manager.Reset() })
	registerDaemonHandlers(hub, root, manager)

	server := httptest.NewServer(httpserve.New(renderer, hub, nil))
	defer server.Close()
	client := dialTestWS(t, server.URL)
	defer client.conn.Close()
	client.invoke(t, 1, "app-chat:new-session", map[string]any{"tabId": "reset-tab", "chatId": "reset-chat", "providerId": "codex"})
	sessionReply := client.waitReply(t, 1, 5*time.Second)
	if sessionReply.Error != nil {
		t.Fatalf("new Codex reset session: %s", *sessionReply.Error)
	}
	sessionID := sessionReply.Result.(map[string]any)["sessionId"].(string)
	initial := client.waitChannelEvent(t, "chat:plan-usage", 5*time.Second).Payload.(map[string]any)
	initialReset := initial["rateLimitResetCredits"].(map[string]any)
	if fmt.Sprint(initialReset["availableCount"]) != "1" {
		t.Fatalf("initial earned reset = %#v", initialReset)
	}

	client.invoke(t, 2, "app-chat:use-rate-limit-reset", map[string]any{
		"providerId": "codex", "sessionId": sessionID, "idempotencyKey": "wire-reset-attempt", "creditId": "RateLimitResetCredit_wire",
	})
	consume := client.waitReply(t, 2, 5*time.Second)
	if consume.Error != nil {
		t.Fatalf("consume earned reset: %s", *consume.Error)
	}
	result := consume.Result.(map[string]any)
	if result["outcome"] != "reset" {
		t.Fatalf("consume result = %#v", result)
	}
	plan := result["planUsage"].(map[string]any)
	remaining := plan["rateLimitResetCredits"].(map[string]any)
	if fmt.Sprint(remaining["availableCount"]) != "0" {
		t.Fatalf("remaining earned resets = %#v", remaining)
	}

	client.invoke(t, 3, "app-chat:use-rate-limit-reset", map[string]any{
		"providerId": "codex", "sessionId": sessionID, "idempotencyKey": "wire-reset-attempt", "creditId": "RateLimitResetCredit_wire",
	})
	retry := client.waitReply(t, 3, 5*time.Second)
	if retry.Error != nil || retry.Result.(map[string]any)["outcome"] != "alreadyRedeemed" {
		t.Fatalf("idempotent earned reset retry = %#v", retry)
	}
}

func TestWireClientReadyAndSessionSaveDoNotCreatePlanUsageSessionOrRaceRealAttach(t *testing.T) {
	root := repoRoot(t)
	renderer := t.TempDir()
	if err := os.WriteFile(filepath.Join(renderer, "index.html"), []byte("<!doctype html><head></head><body></body>"), 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}
	stateDir := filepath.Join(t.TempDir(), "state")
	sessionState := sharedSessionStore(stateDir)
	snapshot := map[string]any{
		"v": json.Number("1"), "activeId": "warm-plan-tab", "seq": json.Number("1"),
		"theme": "dark", "panes": map[string]any{}, "mode": "chats",
		"chats": []any{map[string]any{
			"id": "warm-plan-tab", "chatId": "warm-plan-chat", "title": "Warm plan", "titleLocked": true,
			"group": nil, "cwd": nil, "currentModelId": nil, "currentModeId": nil,
			"draft": "", "providerId": "claude", "messages": []any{},
		}},
	}
	if !sessionState.Save(snapshot) {
		t.Fatal("save active plan-usage session mirror")
	}
	tracePath := filepath.Join(t.TempDir(), "wire-fake-methods.log")

	hub := wire.NewHub()
	manager := acp.NewManager(acp.Options{
		RootDir:  root,
		StateDir: stateDir,
		Providers: []acp.ProviderConfig{
			{ID: "claude", Name: "Claude Plan Fake", Command: os.Args[0], Args: []string{"-test.run=TestWireFakeACPHelper", "--"}, CWD: root, Env: map[string]string{"WORKASS_WIRE_FAKE_ACP": "1", "WORKASS_WIRE_FAKE_ACP_MODE": "plan-limits", "WORKASS_WIRE_FAKE_ACP_METHOD_LOG": tracePath}, Enabled: true},
		},
		DefaultProviderID:           "claude",
		Broadcast:                   hub.Broadcast,
		InitTimeout:                 2 * time.Second,
		StdoutFlushInterval:         10 * time.Millisecond,
		ThoughtFlushInterval:        10 * time.Millisecond,
		RSSSampleInterval:           time.Hour,
		ProviderUpdateInterval:      time.Hour,
		ProviderUpdateRetryBackoffs: []time.Duration{},
	})
	t.Cleanup(func() { manager.Reset() })
	registerDaemonHandlers(hub, root, manager, daemonOptions{StateDir: stateDir})

	server := httptest.NewServer(httpserve.New(renderer, hub, nil))
	defer server.Close()
	client := dialTestWS(t, server.URL)
	defer client.conn.Close()

	// A renderer reconnect and its initial state save are view lifecycle only.
	// They replay the latest in-memory snapshot but must never initialize an ACP
	// process, resume a provider thread, or create a new session just to fetch
	// plan usage.
	client.invoke(t, 1, "session:save", snapshot)
	if reply := client.waitReply(t, 1, 2*time.Second); reply.Error != nil || reply.Result != true {
		t.Fatalf("initial session save reply = %#v", reply)
	}
	time.Sleep(250 * time.Millisecond)
	if methods := readWireFakeMethods(t, tracePath); len(methods) != 0 {
		t.Fatalf("client-ready/session-save touched ACP before a real attach: methods=%v", methods)
	}

	// Exercise the former race boundary: another save and the real attach arrive
	// back-to-back. Exactly the real app-chat:new-session call owns session
	// creation; the save cannot launch a competing warm/resume goroutine.
	client.invoke(t, 2, "session:save", snapshot)
	client.invoke(t, 3, "app-chat:new-session", map[string]any{
		"tabId": "warm-plan-tab", "chatId": "warm-plan-chat", "providerId": "claude", "cwd": root,
	})
	if reply := client.waitReply(t, 2, 2*time.Second); reply.Error != nil || reply.Result != true {
		t.Fatalf("racing session save reply = %#v", reply)
	}
	sessionReply := client.waitReply(t, 3, 5*time.Second)
	if sessionReply.Error != nil {
		t.Fatalf("real session attach reply = %#v", sessionReply)
	}
	sessionID := fieldString(sessionReply.Result.(map[string]any), "sessionId")

	plan := client.waitChannelEvent(t, "chat:plan-usage", 5*time.Second).Payload.(map[string]any)
	if plan["providerId"] != "claude" {
		t.Fatalf("plan usage provider = %#v, want claude; payload=%#v", plan["providerId"], plan)
	}
	methods := waitWireFakeMethods(t, tracePath, 2*time.Second, func(methods []string) bool {
		return countWireMethod(methods, "_workass/claude/usage") == 1
	})
	time.Sleep(100 * time.Millisecond)
	methods = readWireFakeMethods(t, tracePath)
	if countWireMethod(methods, "session/new") != 1 || countWireMethod(methods, "session/resume") != 0 || countWireMethod(methods, "session/load") != 0 {
		t.Fatalf("real attach raced a duplicate session: methods=%v", methods)
	}
	if countWireMethod(methods, "session/prompt") != 0 {
		t.Fatalf("plan-usage attach sent a prompt: methods=%v", methods)
	}

	// The renderer's explicit active-chat refresh reuses the exact live session
	// and performs one metadata read. It must not create another provider session
	// or send model input.
	client.invoke(t, 4, "app-chat:new-session", map[string]any{
		"tabId": "warm-plan-tab", "chatId": "warm-plan-chat", "providerId": "claude",
		"sessionId": sessionID, "cwd": root, "refreshPlanUsage": true,
	})
	if reply := client.waitReply(t, 4, 5*time.Second); reply.Error != nil {
		t.Fatalf("explicit plan refresh reply = %#v", reply)
	}
	methods = waitWireFakeMethods(t, tracePath, 2*time.Second, func(methods []string) bool {
		return countWireMethod(methods, "_workass/claude/usage") == 2
	})
	if countWireMethod(methods, "session/new") != 1 || countWireMethod(methods, "session/prompt") != 0 {
		t.Fatalf("explicit plan refresh touched conversation methods: %v", methods)
	}

	// Provider-scoped refresh is independent of whichever engine currently owns
	// the visible chat. It reuses the initialized Claude account bridge and never
	// creates a provider conversation or sends model input.
	client.invoke(t, 5, "app-chat:refresh-plan-usage", map[string]any{"providerId": "claude"})
	if reply := client.waitReply(t, 5, 5*time.Second); reply.Error != nil {
		t.Fatalf("provider plan refresh reply = %#v", reply)
	}
	methods = waitWireFakeMethods(t, tracePath, 2*time.Second, func(methods []string) bool {
		return countWireMethod(methods, "_workass/claude/usage") == 3
	})
	if countWireMethod(methods, "session/new") != 1 || countWireMethod(methods, "session/prompt") != 0 {
		t.Fatalf("provider plan refresh touched conversation methods: %v", methods)
	}
	t.Logf("trace ready/save stayed ACP-silent; real attach methods=%s", strings.Join(methods, ","))
}

func TestWireProviderUpdatesAndMockAppUpdateReplayToLateClient(t *testing.T) {
	root := repoRoot(t)
	renderer := t.TempDir()
	if err := os.WriteFile(filepath.Join(renderer, "index.html"), []byte("<!doctype html><head></head><body></body>"), 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}
	pathDir := t.TempDir()
	installWireFakeAgentWrapper(t, pathDir, "qwen")
	t.Setenv("PATH", pathDir)
	t.Setenv("WORKASS_WIRE_FAKE_CLI_VERSION", "qwen-code 0.58.1")
	t.Setenv("WORKASS_MOCK_UPDATE", "0.0.2-mock")
	models := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"wire-qwen-model"}]}`))
	}))
	defer models.Close()
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"version":"0.58.10"}`))
	}))
	defer registry.Close()

	hub := wire.NewHub()
	manager := acp.NewManager(acp.Options{
		RootDir: root,
		Providers: []acp.ProviderConfig{
			{ID: "qwen", Name: "Qwen Code ACP", Command: "qwen", Args: []string{"--acp"}, Enabled: false, Badge: "agent", CWD: root},
		},
		DefaultProviderID:     "qwen",
		Broadcast:             hub.Broadcast,
		InitTimeout:           800 * time.Millisecond,
		StdoutFlushInterval:   10 * time.Millisecond,
		RSSSampleInterval:     time.Hour,
		LocalModelEndpoints:   []string{models.URL + "/v1/models"},
		ProviderUpdateSources: map[string]string{"qwen": registry.URL + "/latest"},
		ProviderUpdateTimeout: 200 * time.Millisecond,
	})
	t.Cleanup(func() { manager.Reset() })
	registerDaemonHandlers(hub, root, manager)

	detected := manager.DetectProviders(context.Background(), acp.DetectOptions{ProviderID: "qwen"})
	if detected["ok"] != true {
		t.Fatalf("detect providers = %#v", detected)
	}
	manager.CheckProviderUpdates(context.Background())

	server := httptest.NewServer(httpserve.New(renderer, hub, nil))
	defer server.Close()
	client := dialTestWS(t, server.URL)
	defer client.conn.Close()

	updateMsg := client.waitChannelEvent(t, "providers:updates", 2*time.Second)
	updatesPayload := updateMsg.Payload.(map[string]any)
	updates, _ := updatesPayload["updates"].([]any)
	if len(updates) != 1 {
		t.Fatalf("replayed providers:updates = %#v", updatesPayload)
	}
	update := updates[0].(map[string]any)
	if update["providerId"] != "qwen" || update["cli"] != "qwen" || update["installed"] != "0.58.1" ||
		update["latest"] != "0.58.10" || update["updateAvailable"] != true || update["hint"] != "npm i -g @qwen-code/qwen-code" {
		t.Fatalf("replayed qwen update = %#v", update)
	}
	t.Logf("trace replay providers:updates checkedAt=%s provider=%s installed=%s latest=%s updateAvailable=%v", updatesPayload["checkedAt"], update["providerId"], update["installed"], update["latest"], update["updateAvailable"])

	appUpdateMsg := client.waitChannelEvent(t, "app:update", 2*time.Second)
	appUpdate := appUpdateMsg.Payload.(map[string]any)
	if appUpdate["version"] != "0.0.2-mock" || appUpdate["mocked"] != true {
		t.Fatalf("replayed app:update = %#v", appUpdate)
	}
	t.Logf("trace replay app:update version=%s mocked=%v", appUpdate["version"], appUpdate["mocked"])
}

func TestWireProviderCatalogConnectBeforeDetectionGetsSingleBroadcast(t *testing.T) {
	root := repoRoot(t)
	renderer := t.TempDir()
	if err := os.WriteFile(filepath.Join(renderer, "index.html"), []byte("<!doctype html><head></head><body></body>"), 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}
	pathDir := t.TempDir()
	started := filepath.Join(t.TempDir(), "devin-started")
	release := filepath.Join(t.TempDir(), "devin-release")
	installWireBlockingFakeAgentWrapper(t, pathDir, "devin", started, release)
	t.Setenv("PATH", pathDir)

	hub := wire.NewHub()
	manager := acp.NewManager(acp.Options{
		RootDir: root,
		Providers: []acp.ProviderConfig{
			{ID: "devin", Name: "Devin ACP", Command: "devin", Args: []string{"acp"}, Enabled: false, Badge: "agent", CWD: root},
		},
		DefaultProviderID:   "devin",
		ProviderConfigFile:  filepath.Join(t.TempDir(), "providers.json"),
		Broadcast:           hub.Broadcast,
		InitTimeout:         2 * time.Second,
		StdoutFlushInterval: 10 * time.Millisecond,
		RSSSampleInterval:   time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	registerDaemonHandlers(hub, root, manager)

	server := httptest.NewServer(httpserve.New(renderer, hub, nil))
	defer server.Close()

	done := make(chan map[string]any, 1)
	go func() {
		done <- manager.DetectProviders(context.Background(), acp.DetectOptions{})
	}()
	waitForWireFile(t, started, 2*time.Second)

	client := dialTestWS(t, server.URL)
	defer client.conn.Close()
	client.expectNoProviderCatalogEvents(t, 100*time.Millisecond)

	if err := os.WriteFile(release, []byte("ok"), 0o644); err != nil {
		t.Fatalf("release fake provider: %v", err)
	}
	select {
	case result := <-done:
		if result["ok"] != true {
			t.Fatalf("detect providers = %#v", result)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for detection")
	}

	providersMsg := client.waitChannelEvent(t, "providers:list", 2*time.Second)
	providers := providerItems(providersMsg.Payload)
	assertWireProvider(t, providers, "devin", "ready", true)
	catalogMsg := client.waitChannelEvent(t, "chat:catalog", 2*time.Second)
	catalog := catalogMsg.Payload.(map[string]any)
	if group := catalogGroup(catalogGroups(catalog), "devin"); group == nil || group["status"] != "ready" {
		t.Fatalf("broadcast catalog missing ready devin group: %#v", catalog)
	}
	client.expectNoProviderCatalogEvents(t, 150*time.Millisecond)
	t.Logf("trace broadcast-once providers:list %s", providerItemsSummary(providers))
	t.Logf("trace broadcast-once chat:catalog %s", groupSummary(catalogGroups(catalog)))
}

func TestWireProvidersDetectInvokeEmitsAndEnablesStubs(t *testing.T) {
	root := repoRoot(t)
	renderer := t.TempDir()
	if err := os.WriteFile(filepath.Join(renderer, "index.html"), []byte("<!doctype html><head></head><body></body>"), 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}
	pathDir := t.TempDir()
	installWireNodeWrapper(t, pathDir)
	installWireFakeAgentWrapper(t, pathDir, "devin")
	installWireFakeAgentWrapper(t, pathDir, "qwen")
	installWireNativeFrontierFixtures(t, root, pathDir)
	t.Setenv("PATH", pathDir)
	t.Setenv("WORKASS_PROD", "1")
	t.Setenv("ASSISTANT_DEVIN", filepath.Join(pathDir, "devin"))
	models := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"wire-qwen-model"}]}`))
	}))
	defer models.Close()

	hub := wire.NewHub()
	manager := acp.NewManager(acp.Options{
		RootDir:             root,
		ProviderConfigFile:  filepath.Join(t.TempDir(), "providers.json"),
		Broadcast:           hub.Broadcast,
		InitTimeout:         800 * time.Millisecond,
		StdoutFlushInterval: 10 * time.Millisecond,
		RSSSampleInterval:   time.Hour,
		LocalModelEndpoints: []string{models.URL + "/v1/models"},
	})
	t.Cleanup(func() { manager.Reset() })
	registerDaemonHandlers(hub, root, manager)

	server := httptest.NewServer(httpserve.New(renderer, hub, nil))
	defer server.Close()
	client := dialTestWS(t, server.URL)
	defer client.conn.Close()

	client.invoke(t, 1, "providers:detect", map[string]any{})
	reply := client.waitReply(t, 1, 5*time.Second)
	if reply.Error != nil {
		t.Fatalf("providers:detect error: %s", *reply.Error)
	}
	res := reply.Result.(map[string]any)
	if res["ok"] != true {
		t.Fatalf("providers:detect reply = %#v", res)
	}
	providersMsg := client.waitChannelEvent(t, "providers:list", 5*time.Second)
	providers := providerItems(providersMsg.Payload)
	assertWireProvider(t, providers, "mock", "ready", true)
	assertWireProvider(t, providers, "devin", "ready", true)
	qwen := assertWireProvider(t, providers, "qwen", "ready", true)
	assertWireProvider(t, providers, "claude", "ready", true)
	assertWireProvider(t, providers, "codex", "ready", true)
	autoEnv, _ := qwen["autoEnv"].(map[string]any)
	if autoEnv["OPENAI_BASE_URL"] != models.URL+"/v1" || autoEnv["OPENAI_MODEL"] != "wire-qwen-model" || autoEnv["OPENAI_API_KEY"] != "[redacted]" {
		t.Fatalf("wire qwen autoEnv = %#v", autoEnv)
	}
	t.Logf("trace event providers:list %s", providerItemsSummary(providers))

	catalogMsg := client.waitFor(t, 5*time.Second, func(msg wsMessage) bool {
		if msg.T != "event" || msg.Channel != "chat:catalog" {
			return false
		}
		catalog, _ := msg.Payload.(map[string]any)
		return catalogGroup(catalogGroups(catalog), "qwen") != nil
	})
	catalog := catalogMsg.Payload.(map[string]any)
	t.Logf("trace event chat:catalog %s", groupSummary(catalogGroups(catalog)))

	client.invoke(t, 2, "app-chat:new-session", map[string]any{"tabId": "wire-detect-devin-tab", "chatId": "wire-detect-devin-chat", "providerId": "devin"})
	sessionReply := client.waitReply(t, 2, 5*time.Second)
	if sessionReply.Error != nil {
		t.Fatalf("new-session error: %s", *sessionReply.Error)
	}
	session := sessionReply.Result.(map[string]any)
	if session["providerId"] != "devin" || session["sessionId"] == "" {
		t.Fatalf("detected devin session = %#v", session)
	}
	t.Logf("trace reply app-chat:new-session provider=%s session=%s", session["providerId"], session["sessionId"])
}

func TestWireTraceNativeLocalProviderColdStartAndTurn(t *testing.T) {
	root := repoRoot(t)
	renderer := t.TempDir()
	if err := os.WriteFile(filepath.Join(renderer, "index.html"), []byte("<!doctype html><head></head><body></body>"), 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}
	fake := newWireFakeOpenAI(t, "wire-local-first", "wire-local-second")
	defer fake.Close()
	agentBin := buildWireWorkassAgentBinary(t, root)
	t.Setenv("WORKASS_AGENT_BIN", agentBin)

	hub := wire.NewHub()
	manager := acp.NewManager(acp.Options{
		RootDir: root,
		Providers: []acp.ProviderConfig{
			{ID: "local-lmstudio", Name: "LM Studio (local)", Command: "workass-agent", Args: []string{}, Enabled: false, Badge: "native", CWD: root},
		},
		ProviderConfigFile:  filepath.Join(t.TempDir(), "providers.json"),
		Broadcast:           hub.Broadcast,
		InitTimeout:         5 * time.Second,
		StdoutFlushInterval: 5 * time.Millisecond,
		RSSSampleInterval:   time.Hour,
		LocalModelEndpoints: []string{fake.URL() + "/v1/models", "http://127.0.0.1:1/v1/models"},
	})
	t.Cleanup(func() { manager.Reset() })
	registerDaemonHandlers(hub, root, manager)
	manager.StartProviderDetection(context.Background())

	server := httptest.NewServer(httpserve.New(renderer, hub, nil))
	defer server.Close()
	client := dialTestWS(t, server.URL)
	defer client.conn.Close()

	catalogMsg := client.waitFor(t, 10*time.Second, func(msg wsMessage) bool {
		if msg.T != "event" || msg.Channel != "chat:catalog" {
			return false
		}
		catalog, _ := msg.Payload.(map[string]any)
		group := catalogGroup(catalogGroups(catalog), "local-lmstudio")
		if group == nil || group["status"] != "ready" {
			return false
		}
		models, _ := group["models"].([]any)
		return len(models) == 2
	})
	catalog := catalogMsg.Payload.(map[string]any)
	t.Logf("trace event chat:catalog %s", groupSummary(catalogGroups(catalog)))

	client.invoke(t, 1, "app-chat:new-session", map[string]any{"tabId": "wire-local-tab", "chatId": "chat-wire-local", "providerId": "local-lmstudio"})
	sessionReply := client.waitReply(t, 1, 10*time.Second)
	if sessionReply.Error != nil {
		t.Fatalf("local new-session error: %s", *sessionReply.Error)
	}
	session := sessionReply.Result.(map[string]any)
	if session["providerId"] != "local-lmstudio" || session["sessionId"] == "" {
		t.Fatalf("local session = %#v", session)
	}
	t.Logf("trace reply app-chat:new-session provider=%s session=%s", session["providerId"], session["sessionId"])

	client.invoke(t, 2, "job:start", map[string]any{"kind": "app-chat", "chatId": "chat-wire-local", "sessionId": session["sessionId"], "tabId": "wire-local-tab", "prompt": "wire native local turn"})
	startReply := client.waitReply(t, 2, 10*time.Second)
	if startReply.Error != nil {
		t.Fatalf("local job:start error: %s", *startReply.Error)
	}
	job := startReply.Result.(map[string]any)
	jobID := job["id"].(string)
	if job["providerId"] != "local-lmstudio" {
		t.Fatalf("local job = %#v", job)
	}
	t.Logf("trace reply job:start provider=%s job=%s", job["providerId"], jobID)

	sawData := false
	for {
		msg := client.waitEvent(t, 10*time.Second)
		if msg.Channel != "job:event" {
			continue
		}
		payload := msg.Payload.(map[string]any)
		switch payload["type"] {
		case "data":
			if payload["id"] != jobID || payload["stream"] != "stdout" {
				t.Fatalf("local data payload = %#v", payload)
			}
			sawData = true
			t.Logf("trace event job:event type=data job=%s chunk=%q", payload["id"], payload["chunk"])
		case "usage":
			t.Logf("trace event job:event type=usage sessionId=%s used=%v input=%v output=%v", payload["sessionId"], payload["used"], payload["inputTokens"], payload["outputTokens"])
		case "end":
			endJob := payload["job"].(map[string]any)
			t.Logf("trace event job:event type=end provider=%s status=%s stopReason=%v result=%q", endJob["providerId"], endJob["status"], endJob["stopReason"], endJob["result"])
			if !sawData || endJob["providerId"] != "local-lmstudio" || endJob["status"] != "done" || endJob["stopReason"] != "end_turn" || endJob["result"] != "wire native ok" {
				t.Fatalf("local end payload = %#v sawData=%v", payload, sawData)
			}
			req := fake.LastChatRequest()
			archivePath := filepath.Join(manager.StateDir(), "chat-archive", "<chatId>.jsonl")
			if req.Model != "wire-local-first" || !req.Stream || len(req.Messages) != 1 ||
				!strings.Contains(req.Messages[0].Content, archivePath) ||
				!strings.Contains(req.Messages[0].Content, "User request:\nwire native local turn") {
				t.Fatalf("wire fake backend request = %#v", req)
			}
			return
		}
	}
}

func TestWireRealLMStudioNativeProviderTransport(t *testing.T) {
	if os.Getenv("WORKASS_WIRE_REAL_LMSTUDIO") != "1" {
		t.Skip("set WORKASS_WIRE_REAL_LMSTUDIO=1 to run daemon -> workass-agent -> LM Studio transport smoke")
	}
	root := repoRoot(t)
	renderer := t.TempDir()
	if err := os.WriteFile(filepath.Join(renderer, "index.html"), []byte("<!doctype html><head></head><body></body>"), 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}
	baseURL := getenvDefaultForWire("OPENAI_BASE_URL", "http://127.0.0.1:1234/v1")
	agentBin := buildWireWorkassAgentBinary(t, root)
	t.Setenv("WORKASS_AGENT_BIN", agentBin)

	hub := wire.NewHub()
	manager := acp.NewManager(acp.Options{
		RootDir: root,
		Providers: []acp.ProviderConfig{
			{ID: "local-lmstudio", Name: "LM Studio (local)", Command: "workass-agent", Args: []string{}, Enabled: false, Badge: "native", CWD: root},
		},
		ProviderConfigFile:  filepath.Join(t.TempDir(), "providers.json"),
		Broadcast:           hub.Broadcast,
		InitTimeout:         10 * time.Second,
		StdoutFlushInterval: 20 * time.Millisecond,
		RSSSampleInterval:   time.Hour,
		LocalModelEndpoints: []string{modelsEndpointForWire(baseURL), "http://127.0.0.1:1/v1/models"},
	})
	t.Cleanup(func() { manager.Reset() })
	registerDaemonHandlers(hub, root, manager)
	manager.StartProviderDetection(context.Background())

	server := httptest.NewServer(httpserve.New(renderer, hub, nil))
	defer server.Close()
	client := dialTestWS(t, server.URL)
	defer client.conn.Close()

	catalogMsg := client.waitFor(t, 30*time.Second, func(msg wsMessage) bool {
		if msg.T != "event" || msg.Channel != "chat:catalog" {
			return false
		}
		catalog, _ := msg.Payload.(map[string]any)
		group := catalogGroup(catalogGroups(catalog), "local-lmstudio")
		return group != nil && group["status"] == "ready"
	})
	t.Logf("trace real lmstudio chat:catalog %s", groupSummary(catalogGroups(catalogMsg.Payload.(map[string]any))))

	client.invoke(t, 1, "app-chat:new-session", map[string]any{"tabId": "wire-real-local-tab", "chatId": "chat-wire-real-local", "providerId": "local-lmstudio"})
	sessionReply := client.waitReply(t, 1, 30*time.Second)
	if sessionReply.Error != nil {
		t.Fatalf("real local new-session error: %s", *sessionReply.Error)
	}
	session := sessionReply.Result.(map[string]any)
	sessionID := fmt.Sprint(session["sessionId"])
	if session["providerId"] != "local-lmstudio" || sessionID == "" {
		t.Fatalf("real local session = %#v", session)
	}
	t.Logf("trace real lmstudio new-session provider=%s session=%s", session["providerId"], sessionID)

	client.invoke(t, 2, "job:start", map[string]any{"kind": "app-chat", "chatId": "chat-wire-real-local", "sessionId": sessionID, "tabId": "wire-real-local-tab", "prompt": "Reply with one short sentence for a transport smoke test."})
	startReply := client.waitReply(t, 2, 30*time.Second)
	if startReply.Error != nil {
		t.Fatalf("real local job:start error: %s", *startReply.Error)
	}
	job := startReply.Result.(map[string]any)
	jobID := job["id"].(string)
	var bytesSeen int
	for {
		msg := client.waitEvent(t, 90*time.Second)
		if msg.Channel != "job:event" {
			continue
		}
		payload := msg.Payload.(map[string]any)
		switch payload["type"] {
		case "data":
			if payload["id"] == jobID {
				chunk := fmt.Sprint(payload["chunk"])
				bytesSeen += len(chunk)
				t.Logf("trace real lmstudio data bytes=%d chunk=%q", len(chunk), chunk)
			}
		case "usage":
			t.Logf("trace real lmstudio usage used=%v input=%v output=%v", payload["used"], payload["inputTokens"], payload["outputTokens"])
		case "end":
			endJob := payload["job"].(map[string]any)
			t.Logf("trace real lmstudio end provider=%s status=%s stopReason=%v bytes=%d", endJob["providerId"], endJob["status"], endJob["stopReason"], bytesSeen)
			if endJob["id"] != jobID || endJob["providerId"] != "local-lmstudio" || endJob["status"] != "done" || endJob["stopReason"] != "end_turn" || bytesSeen == 0 {
				t.Fatalf("real local end = %#v bytes=%d", endJob, bytesSeen)
			}
			return
		}
	}
}

func TestWireTraceAccessApprovalRevoke(t *testing.T) {
	root := repoRoot(t)
	renderer := t.TempDir()
	if err := os.WriteFile(filepath.Join(renderer, "index.html"), []byte("<!doctype html><head></head><body></body>"), 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}

	leaseManager, err := lease.NewManager(lease.Options{
		StateDir: t.TempDir(),
		Logf: func(format string, args ...any) {
			t.Logf(format, args...)
		},
	})
	if err != nil {
		t.Fatalf("lease manager: %v", err)
	}
	_, controllerToken, err := leaseManager.ApproveDevice("trace-controller", "127.0.0.1")
	if err != nil {
		t.Fatalf("approve controller: %v", err)
	}
	hub := wire.NewHub(wire.Options{Lease: leaseManager, TrustLocalhost: false, Logf: t.Logf})
	registerDaemonHandlers(hub, root, nil)

	server := httptest.NewServer(httpserve.New(renderer, hub, nil))
	defer server.Close()
	controller := dialTestWSPath(t, server.URL, "/?deviceToken="+controllerToken+"&deviceName=trace-controller")
	defer controller.conn.Close()
	controllerState := controller.waitChannelEvent(t, "lan:access-state", 2*time.Second)
	controllerPayload := controllerState.Payload.(map[string]any)
	if controllerPayload["state"] != "approved" || controllerPayload["controller"] != true {
		t.Fatalf("controller access state = %#v", controllerPayload)
	}
	t.Logf("trace controller connected token=%s controller=true", redactTokenForLog(controllerToken))

	unknown := dialTestWSPath(t, server.URL, "/?deviceName=trace-phone")
	defer unknown.conn.Close()
	waiting := unknown.waitChannelEvent(t, "lan:access-state", 2*time.Second)
	waitingPayload := waiting.Payload.(map[string]any)
	requestID := fmt.Sprint(waitingPayload["requestId"])
	if waitingPayload["state"] != "waiting" || requestID == "" {
		t.Fatalf("unknown waiting state = %#v", waitingPayload)
	}
	t.Logf("trace unknown device waiting requestId=%s", requestID)

	request := controller.waitChannelEvent(t, "lan:access-request", 2*time.Second)
	requestPayload := request.Payload.(map[string]any)
	if requestPayload["requestId"] != requestID || requestPayload["deviceName"] != "trace-phone" {
		t.Fatalf("access-request payload = %#v", requestPayload)
	}
	unknown.expectNoEventChannel(t, "lan:access-request", 150*time.Millisecond)
	t.Logf("trace access-request delivered to controller only requestId=%s", requestID)

	controller.invoke(t, 1, "lan:access-decide", map[string]any{"requestId": requestID, "allow": true})
	decideReply := controller.waitReply(t, 1, 2*time.Second)
	if decideReply.Error != nil || decideReply.Result.(map[string]any)["allowed"] != true {
		t.Fatalf("access-decide reply = %+v", decideReply)
	}
	approved := unknown.waitChannelEvent(t, "lan:access-state", 2*time.Second)
	approvedPayload := approved.Payload.(map[string]any)
	issuedToken, _ := approvedPayload["deviceToken"].(string)
	deviceID, _ := approvedPayload["deviceId"].(string)
	if approvedPayload["state"] != "approved" || len(issuedToken) != 64 || deviceID == "" {
		t.Fatalf("approved payload state=%v tokenLen=%d deviceIDEmpty=%v", approvedPayload["state"], len(issuedToken), deviceID == "")
	}
	t.Logf("trace approved deviceId=%s token=%s", deviceID, redactTokenForLog(issuedToken))

	reconnected := dialTestWSPath(t, server.URL, "/?deviceToken="+issuedToken+"&deviceName=trace-phone")
	defer reconnected.conn.Close()
	reconnectState := reconnected.waitChannelEvent(t, "lan:access-state", 2*time.Second)
	if reconnectState.Payload.(map[string]any)["state"] != "approved" {
		t.Fatalf("reconnect access state = %#v", reconnectState.Payload)
	}
	t.Logf("trace reconnect with token OK deviceId=%s", deviceID)

	controller.invoke(t, 2, "lan:revoke", map[string]any{"deviceId": deviceID})
	revokeReply := controller.waitReply(t, 2, 2*time.Second)
	if revokeReply.Error != nil || revokeReply.Result.(map[string]any)["ok"] != true {
		t.Fatalf("revoke reply = %+v", revokeReply)
	}
	t.Logf("trace revoked deviceId=%s", deviceID)

	rejected := dialTestWSPath(t, server.URL, "/?deviceToken="+issuedToken+"&deviceName=trace-phone")
	defer rejected.conn.Close()
	rejectedState := rejected.waitChannelEvent(t, "lan:access-state", 2*time.Second)
	rejectedPayload := rejectedState.Payload.(map[string]any)
	if rejectedPayload["state"] != "rejected" || rejectedPayload["reason"] != "invalid-token" {
		t.Fatalf("reconnect after revoke payload = %#v", rejectedPayload)
	}
	t.Logf("trace reconnect rejected after revoke reason=%s", rejectedPayload["reason"])
}

func TestWireTraceForkChatSeedsPrefixAndDiverges(t *testing.T) {
	repo := repoRoot(t)
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	traceFile := filepath.Join(root, "mock-prompts.jsonl")
	renderer := t.TempDir()
	if err := os.WriteFile(filepath.Join(renderer, "index.html"), []byte("<!doctype html><head></head><body></body>"), 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}

	hub := wire.NewHub()
	manager := acp.NewManager(acp.Options{
		RootDir:  root,
		StateDir: stateDir,
		Provider: acp.ProviderConfig{
			ID:      "mock",
			Command: "node",
			Args:    []string{filepath.Join(repo, "desktop", "acp", "mock-server.mjs")},
			CWD:     repo,
			Env: map[string]string{
				"WORKASS_MOCK_ACP_TRACE_FILE": traceFile,
				"WORKASS_MOCK_ACP_DELAY_MS":   "10",
			},
			Enabled: true,
			Label:   "Workass Mock ACP",
		},
		DefaultProviderID:      "mock",
		Broadcast:              hub.Broadcast,
		StdoutFlushInterval:    10 * time.Millisecond,
		ThoughtFlushInterval:   10 * time.Millisecond,
		RSSSampleInterval:      time.Hour,
		SpareSessions:          0,
		CompactionEnabled:      false,
		CrashRecoveryBackoff:   10 * time.Millisecond,
		CrashRecoveryWindow:    time.Second,
		LifecycleCheckInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	registerDaemonHandlers(hub, root, manager, daemonOptions{StateDir: stateDir})

	server := httptest.NewServer(httpserve.New(renderer, hub, nil))
	defer server.Close()
	client := dialTestWS(t, server.URL)
	defer client.conn.Close()

	sourceTabID := "wire-fork-source"
	forkTabID := "wire-fork-child"
	sourceChatID := "chat-wire-fork-source"
	client.invoke(t, 1, "app-chat:new-session", map[string]any{"tabId": sourceTabID, "chatId": sourceChatID, "providerId": "mock"})
	sessionReply := client.waitReply(t, 1, 5*time.Second)
	if sessionReply.Error != nil {
		t.Fatalf("new-session error: %s", *sessionReply.Error)
	}
	sourceSession := sessionReply.Result.(map[string]any)
	sourceSessionID := sourceSession["sessionId"].(string)
	t.Logf("trace reply app-chat:new-session tab=%s provider=%s session=%s", sourceTabID, sourceSession["providerId"], sourceSessionID)

	firstResult := runWireChatTurn(t, client, 2, sourceChatID, sourceTabID, sourceSessionID, "source first")
	appendWireArchiveTurn(t, client, 3, sourceTabID, "source first", firstResult, "2026-07-10T00:00:01Z")
	secondResult := runWireChatTurn(t, client, 4, sourceChatID, sourceTabID, sourceSessionID, "source second")
	appendWireArchiveTurn(t, client, 5, sourceTabID, "source second", secondResult, "2026-07-10T00:00:02Z")

	client.invoke(t, 6, "app-chat:fork", map[string]any{"tabId": sourceTabID, "newTabId": forkTabID, "atTurn": 1})
	forkReply := client.waitReply(t, 6, 5*time.Second)
	if forkReply.Error != nil {
		t.Fatalf("fork reply error: %s", *forkReply.Error)
	}
	forkSession := forkReply.Result.(map[string]any)
	forkSessionID := forkSession["sessionId"].(string)
	forkedFrom := forkSession["forkedFrom"].(map[string]any)
	if forkSessionID == "" || forkSessionID == sourceSessionID || forkSession["providerId"] != "mock" || forkedFrom["tabId"] != sourceTabID || fmt.Sprint(forkedFrom["atTurn"]) != "1" {
		t.Fatalf("fork reply = %#v", forkSession)
	}
	t.Logf("trace reply app-chat:fork source=%s fork=%s atTurn=%v session=%s", sourceTabID, forkTabID, forkedFrom["atTurn"], forkSessionID)

	sourceArchive := loadChatArchive(stateDir, sourceTabID)
	forkArchive := loadChatArchive(stateDir, forkTabID)
	if len(sourceArchive) != 4 || len(forkArchive) != 2 {
		t.Fatalf("archive lengths source=%d fork=%d source=%#v fork=%#v", len(sourceArchive), len(forkArchive), sourceArchive, forkArchive)
	}
	if !archiveContainsText(forkArchive, "source first") || archiveContainsText(forkArchive, "source second") {
		t.Fatalf("fork archive prefix = %#v", forkArchive)
	}
	t.Logf("trace archive copied sourceMessages=%d forkMessages=%d forkFile=%s", len(sourceArchive), len(forkArchive), chatArchivePath(stateDir, forkTabID))

	client.invoke(t, 7, "job:start", map[string]any{"kind": "app-chat", "chatId": sourceChatID, "sessionId": sourceSessionID, "tabId": sourceTabID, "prompt": "source third unique"})
	client.invoke(t, 8, "job:start", map[string]any{"kind": "app-chat", "chatId": "chat-wire-fork-child", "sessionId": forkSessionID, "tabId": forkTabID, "prompt": "fork continuation unique"})
	sourceStartReply := client.waitReply(t, 7, 5*time.Second)
	forkStartReply := client.waitReply(t, 8, 5*time.Second)
	if sourceStartReply.Error != nil || forkStartReply.Error != nil {
		t.Fatalf("post-fork job replies source=%+v fork=%+v", sourceStartReply, forkStartReply)
	}
	sourceJob := sourceStartReply.Result.(map[string]any)
	forkJob := forkStartReply.Result.(map[string]any)
	sourceEnd := client.waitJobEvent(t, sourceJob["id"].(string), "end", 5*time.Second)
	forkEnd := client.waitJobEvent(t, forkJob["id"].(string), "end", 5*time.Second)
	sourceEndJob := sourceEnd["job"].(map[string]any)
	forkEndJob := forkEnd["job"].(map[string]any)
	sourceResult := fmt.Sprint(sourceEndJob["result"])
	forkResult := fmt.Sprint(forkEndJob["result"])
	if sourceEndJob["status"] != "done" || forkEndJob["status"] != "done" || sourceResult == forkResult {
		t.Fatalf("post-fork results source=%#v fork=%#v", sourceEndJob, forkEndJob)
	}
	if !strings.Contains(sourceResult, "Mock ACP turn 3: Active Workass runtime for this turn:") ||
		!strings.Contains(sourceResult, "User request:\nsource third unique") ||
		!strings.Contains(forkResult, "User: source first") || strings.Contains(forkResult, "source second") {
		t.Fatalf("divergent seeded results source=%q fork=%q", sourceResult, forkResult)
	}
	t.Logf("trace fork divergence sourceSession=%s forkSession=%s sourceResult=%q forkSeeded=%v", sourceSessionID, forkSessionID, sourceResult, strings.Contains(forkResult, "User: source first"))

	prompts := readMockTracePrompts(t, traceFile)
	forkPrompt := promptTraceForSession(t, prompts, forkSessionID, "fork continuation unique")
	sourceThirdPrompt := promptTraceForSession(t, prompts, sourceSessionID, "source third unique")
	if !strings.Contains(forkPrompt, "User: source first") || strings.Contains(forkPrompt, "source second") {
		t.Fatalf("fork seed prompt = %q", forkPrompt)
	}
	if strings.Contains(sourceThirdPrompt, "Previous conversation") ||
		!strings.Contains(sourceThirdPrompt, "Active Workass runtime for this turn:") ||
		!strings.Contains(sourceThirdPrompt, "User request:\nsource third unique") {
		t.Fatalf("source prompt was touched by fork seed: %q", sourceThirdPrompt)
	}
	t.Logf("trace mock prompts sourceThird=%q forkPromptHasPrefix=%v", sourceThirdPrompt, strings.Contains(forkPrompt, "User: source first"))
}

func TestWireTraceNotifyControllerOnlyRedactionAndNoTurnEndBacklog(t *testing.T) {
	repo := repoRoot(t)
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	renderer := t.TempDir()
	if err := os.WriteFile(filepath.Join(renderer, "index.html"), []byte("<!doctype html><head></head><body></body>"), 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}

	leaseManager, err := lease.NewManager(lease.Options{StateDir: stateDir, Logf: t.Logf})
	if err != nil {
		t.Fatalf("lease manager: %v", err)
	}
	_, controllerToken, err := leaseManager.ApproveDevice("notify-controller", "127.0.0.1")
	if err != nil {
		t.Fatalf("approve controller: %v", err)
	}
	_, viewerToken, err := leaseManager.ApproveDevice("notify-viewer", "127.0.0.1")
	if err != nil {
		t.Fatalf("approve viewer: %v", err)
	}
	hub := wire.NewHub(wire.Options{Lease: leaseManager, TrustLocalhost: false, Logf: t.Logf})
	manager := acp.NewManager(acp.Options{
		RootDir:  root,
		StateDir: stateDir,
		Provider: acp.ProviderConfig{
			ID:      "mock",
			Command: "node",
			Args:    []string{filepath.Join(repo, "desktop", "acp", "mock-server.mjs")},
			CWD:     repo,
			Env:     map[string]string{"WORKASS_MOCK_ACP_DELAY_MS": "10"},
			Enabled: true,
			Label:   "Workass Mock ACP",
		},
		DefaultProviderID:    "mock",
		Broadcast:            hub.Broadcast,
		StdoutFlushInterval:  10 * time.Millisecond,
		ThoughtFlushInterval: 10 * time.Millisecond,
		RSSSampleInterval:    time.Hour,
		CompactionEnabled:    false,
	})
	t.Cleanup(func() { manager.Reset() })
	registerDaemonHandlers(hub, root, manager, daemonOptions{StateDir: stateDir})

	server := httptest.NewServer(httpserve.New(renderer, hub, nil))
	defer server.Close()
	controller := dialTestWSPath(t, server.URL, "/?deviceToken="+controllerToken+"&deviceName=notify-controller")
	defer controller.conn.Close()
	viewer := dialTestWSPath(t, server.URL, "/?deviceToken="+viewerToken+"&deviceName=notify-viewer")
	defer viewer.conn.Close()
	if state := controller.waitChannelEvent(t, "lan:access-state", 2*time.Second).Payload.(map[string]any); state["controller"] != true {
		t.Fatalf("controller state = %#v", state)
	}
	if state := viewer.waitChannelEvent(t, "lan:access-state", 2*time.Second).Payload.(map[string]any); state["controller"] == true {
		t.Fatalf("viewer state = %#v", state)
	}

	controller.invoke(t, 1, "notify", map[string]any{"title": "Build token=abc123", "body": "Bearer shhh and password:open-sesame", "tabId": "notify-tab"})
	reply := controller.waitReply(t, 1, 2*time.Second)
	if reply.Error != nil || reply.Result != true {
		t.Fatalf("notify reply = %+v", reply)
	}
	live := controller.waitChannelEvent(t, "notify", 2*time.Second).Payload.(map[string]any)
	viewer.expectNoEventChannel(t, "notify", 150*time.Millisecond)
	if live["tabId"] != "notify-tab" || live["ts"] == "" || strings.Contains(fmt.Sprint(live["title"])+fmt.Sprint(live["body"]), "abc123") || strings.Contains(fmt.Sprint(live["body"]), "open-sesame") || !strings.Contains(fmt.Sprint(live["title"])+fmt.Sprint(live["body"]), "[redacted]") {
		t.Fatalf("redacted notify payload = %#v", live)
	}
	t.Logf("trace notify live controller-only title=%q body=%q tab=%v", live["title"], live["body"], live["tabId"])

	_ = controller.conn.Close()
	waitForNoController(t, hub, time.Second)
	session, err := manager.NewSession(context.Background(), acp.SessionOptions{TabID: "away-tab", ChatID: "chat-away"})
	if err != nil {
		t.Fatalf("away new session: %v", err)
	}
	job, err := manager.StartJob(context.Background(), acp.JobStartOptions{Kind: "app-chat", ChatID: "chat-away", TabID: "away-tab", SessionID: session.SessionID, Prompt: "away backlog turn"})
	if err != nil {
		t.Fatalf("away job: %v", err)
	}
	awayEnd := viewer.waitJobEvent(t, job["id"].(string), "end", 5*time.Second)
	if awayEnd["job"].(map[string]any)["status"] != "done" {
		t.Fatalf("away job end = %#v", awayEnd)
	}
	viewer.expectNoEventChannel(t, "notify:backlog", 150*time.Millisecond)
	t.Logf("trace no automatic turn-end notify queued job=%s tab=away-tab", job["id"])

	reconnected := dialTestWSPath(t, server.URL, "/?deviceToken="+controllerToken+"&deviceName=notify-controller")
	defer reconnected.conn.Close()
	if state := reconnected.waitChannelEvent(t, "lan:access-state", 2*time.Second).Payload.(map[string]any); state["controller"] != true {
		t.Fatalf("reconnected controller state = %#v", state)
	}
	reconnected.expectNoEventChannel(t, "notify:backlog", 250*time.Millisecond)
	t.Log("trace controller reconnect received no turn-end notification backlog")
}

func assertRendererRunningFixture(t *testing.T, replyJob, startPayload map[string]any, chatID string) {
	t.Helper()
	// Renderer fixture:
	// - desktop/renderer/app.js:4899-4903 marks the assistant message running and
	//   stores chatJobs by the generated chatId before invoking job:start.
	// - app.js:4921-4923 consumes the job:start reply id and binds jobRef.
	// - app.js:5206-5207 routes {type:"start", job} to onJobStart; app.js:5484-5490
	//   calls onAppChatStart for kind=="app-chat"; app.js:4932-4941 uses job.chatId
	//   to find chatJobs and keep the assistant status at "running".
	assistantMessage := map[string]string{"status": "running"}
	chatJobs := map[string]map[string]string{chatID: assistantMessage}
	if replyJob["id"] == "" || replyJob["status"] != "running" {
		t.Fatalf("renderer cannot consume job:start reply = %#v", replyJob)
	}
	assistantMessage["jobId"] = fmt.Sprint(replyJob["id"])
	startJob, _ := startPayload["job"].(map[string]any)
	ref := chatJobs[fmt.Sprint(startJob["chatId"])]
	if ref == nil || startJob["kind"] != "app-chat" || startJob["status"] != "running" {
		t.Fatalf("renderer cannot route start payload = %#v", startPayload)
	}
	ref["jobId"] = fmt.Sprint(startJob["id"])
	ref["status"] = fmt.Sprint(startJob["status"])
	if ref["status"] != "running" || ref["jobId"] == "" {
		t.Fatalf("renderer active-turn fixture did not reach running: %#v", ref)
	}
}

func assertWireJobStatus(t *testing.T, payload map[string]any, status, stopReason string) {
	t.Helper()
	job := payload["job"].(map[string]any)
	if job["status"] != status || job["stopReason"] != stopReason {
		t.Fatalf("wire job status = %#v, want %s/%s", job, status, stopReason)
	}
}

func runWireChatTurn(t *testing.T, client *testWS, invokeID int, chatID, tabID, sessionID, prompt string) string {
	t.Helper()
	client.invoke(t, invokeID, "job:start", map[string]any{"kind": "app-chat", "chatId": chatID, "sessionId": sessionID, "tabId": tabID, "prompt": prompt})
	reply := client.waitReply(t, invokeID, 5*time.Second)
	if reply.Error != nil {
		t.Fatalf("job:start %s error: %s", prompt, *reply.Error)
	}
	job := reply.Result.(map[string]any)
	end := client.waitJobEvent(t, job["id"].(string), "end", 5*time.Second)
	endJob := end["job"].(map[string]any)
	if endJob["status"] != "done" || endJob["stopReason"] != "end_turn" {
		t.Fatalf("turn %s end = %#v", prompt, endJob)
	}
	return fmt.Sprint(endJob["result"])
}

func appendWireArchiveTurn(t *testing.T, client *testWS, invokeID int, tabID, userText, assistantText, at string) {
	t.Helper()
	messages := []any{
		map[string]any{"role": "user", "content": userText, "status": "done", "at": at},
		map[string]any{"role": "assistant", "content": assistantText, "status": "done", "at": at},
	}
	client.invoke(t, invokeID, "chat:archive-append", map[string]any{"tabId": tabID, "messages": messages})
	reply := client.waitReply(t, invokeID, 2*time.Second)
	if reply.Error != nil || reply.Result != true {
		t.Fatalf("archive append reply = %+v", reply)
	}
}

func archiveContainsText(messages []any, needle string) bool {
	for _, raw := range messages {
		msg, _ := raw.(map[string]any)
		if strings.Contains(fmt.Sprint(msg["content"]), needle) {
			return true
		}
	}
	return false
}

func promptTraceForSession(t *testing.T, prompts []map[string]string, sessionID, contains string) string {
	t.Helper()
	for i := len(prompts) - 1; i >= 0; i-- {
		if prompts[i]["sessionId"] == sessionID && strings.Contains(prompts[i]["text"], contains) {
			return prompts[i]["text"]
		}
	}
	t.Fatalf("prompt trace not found session=%s contains=%q prompts=%#v", sessionID, contains, prompts)
	return ""
}

func waitForNoController(t *testing.T, hub *wire.Hub, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if !hub.HasControllerConnections() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("controller connection still active")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type wsMessage struct {
	T       string
	ID      json.Number
	Channel string
	Result  any
	Error   *string
	Payload any
}

type testWS struct {
	conn   net.Conn
	reader *bufio.Reader
	inbox  []wsMessage
}

func dialTestWS(t *testing.T, serverURL string) *testWS {
	t.Helper()
	return dialTestWSPath(t, serverURL, "/")
}

func dialTestWSPath(t *testing.T, serverURL, path string) *testWS {
	t.Helper()
	addr := strings.TrimPrefix(serverURL, "http://")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if path == "" {
		path = "/"
	}
	key := "dGVzdC13b3JrYXNzLWtleQ=="
	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n", path, addr, key)
	if _, err := io.WriteString(conn, req); err != nil {
		_ = conn.Close()
		t.Fatalf("handshake write: %v", err)
	}
	reader := bufio.NewReader(conn)
	status, err := reader.ReadString('\n')
	if err != nil {
		_ = conn.Close()
		t.Fatalf("handshake status: %v", err)
	}
	if strings.TrimSpace(status) != "HTTP/1.1 101 Switching Protocols" {
		_ = conn.Close()
		t.Fatalf("handshake status = %q", strings.TrimSpace(status))
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			_ = conn.Close()
			t.Fatalf("handshake header: %v", err)
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}
	return &testWS{conn: conn, reader: reader}
}

type testHijackResponseWriter struct {
	conn   net.Conn
	rw     *bufio.ReadWriter
	header http.Header
}

func (w *testHijackResponseWriter) Header() http.Header { return w.header }
func (w *testHijackResponseWriter) WriteHeader(int)     {}
func (w *testHijackResponseWriter) Write(b []byte) (int, error) {
	return w.rw.Write(b)
}
func (w *testHijackResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.conn, w.rw, nil
}

func dialTestWSHandler(t *testing.T, handler http.Handler, path string) *testWS {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	serverRW := bufio.NewReadWriter(bufio.NewReader(serverConn), bufio.NewWriter(serverConn))
	writer := &testHijackResponseWriter{conn: serverConn, rw: serverRW, header: http.Header{}}
	if path == "" {
		path = "/"
	}
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1"+path, nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Sec-WebSocket-Key", "dGVzdC13b3JrYXNzLWtleQ==")
	req.Header.Set("Sec-WebSocket-Version", "13")
	go handler.ServeHTTP(writer, req)

	reader := bufio.NewReader(clientConn)
	status, err := reader.ReadString('\n')
	if err != nil {
		_ = clientConn.Close()
		t.Fatalf("handshake status: %v", err)
	}
	if strings.TrimSpace(status) != "HTTP/1.1 101 Switching Protocols" {
		_ = clientConn.Close()
		t.Fatalf("handshake status = %q", strings.TrimSpace(status))
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			_ = clientConn.Close()
			t.Fatalf("handshake header: %v", err)
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}
	return &testWS{conn: clientConn, reader: reader}
}

func (c *testWS) invoke(t *testing.T, id int, channel string, args ...any) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"t": "invoke", "id": id, "channel": channel, "args": args})
	if err != nil {
		t.Fatalf("marshal invoke: %v", err)
	}
	if _, err := c.conn.Write(maskedFrame(payload)); err != nil {
		t.Fatalf("write invoke: %v", err)
	}
}

func (c *testWS) waitReply(t *testing.T, id int, timeout time.Duration) wsMessage {
	t.Helper()
	return c.waitFor(t, timeout, func(msg wsMessage) bool {
		want := fmt.Sprint(id)
		return msg.T == "reply" && msg.ID.String() == want
	})
}

func (c *testWS) waitJobEvent(t *testing.T, jobID, typ string, timeout time.Duration) map[string]any {
	t.Helper()
	msg := c.waitFor(t, timeout, func(msg wsMessage) bool {
		if msg.T != "event" || msg.Channel != "job:event" {
			return false
		}
		payload, _ := msg.Payload.(map[string]any)
		if payload["type"] != typ {
			return false
		}
		if typ == "start" || typ == "end" {
			job, _ := payload["job"].(map[string]any)
			return job["id"] == jobID
		}
		return payload["id"] == jobID
	})
	return msg.Payload.(map[string]any)
}

func (c *testWS) waitEvent(t *testing.T, timeout time.Duration) wsMessage {
	t.Helper()
	return c.waitFor(t, timeout, func(msg wsMessage) bool {
		return msg.T == "event"
	})
}

func (c *testWS) waitChannelEvent(t *testing.T, channel string, timeout time.Duration) wsMessage {
	t.Helper()
	return c.waitFor(t, timeout, func(msg wsMessage) bool {
		return msg.T == "event" && msg.Channel == channel
	})
}

func (c *testWS) expectNoEventChannel(t *testing.T, channel string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return
		}
		msg, ok := c.readMessageMaybe(t, remaining)
		if !ok {
			return
		}
		if msg.T == "event" && msg.Channel == channel {
			t.Fatalf("unexpected %s event payload=%#v", channel, msg.Payload)
		}
		c.inbox = append(c.inbox, msg)
	}
}

func (c *testWS) expectNoProviderCatalogEvents(t *testing.T, timeout time.Duration) {
	t.Helper()
	for _, msg := range c.inbox {
		if msg.T == "event" && (msg.Channel == "providers:list" || msg.Channel == "chat:catalog") {
			t.Fatalf("unexpected provider catalog event %s payload=%#v", msg.Channel, msg.Payload)
		}
	}
	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return
		}
		msg, ok := c.readMessageMaybe(t, remaining)
		if !ok {
			return
		}
		if msg.T == "event" && (msg.Channel == "providers:list" || msg.Channel == "chat:catalog") {
			t.Fatalf("unexpected provider catalog event %s payload=%#v", msg.Channel, msg.Payload)
		}
		c.inbox = append(c.inbox, msg)
	}
}

func (c *testWS) waitFor(t *testing.T, timeout time.Duration, pred func(wsMessage) bool) wsMessage {
	t.Helper()
	for i, msg := range c.inbox {
		if pred(msg) {
			c.inbox = append(c.inbox[:i], c.inbox[i+1:]...)
			return msg
		}
	}
	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatalf("timed out waiting for websocket message; inbox=%#v", c.inbox)
		}
		msg := c.readMessage(t, remaining)
		if pred(msg) {
			return msg
		}
		c.inbox = append(c.inbox, msg)
	}
}

func (c *testWS) readMessage(t *testing.T, timeout time.Duration) wsMessage {
	t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(timeout))
	payload := readServerFrame(t, c.reader)
	var msg wsMessage
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	if err := dec.Decode(&msg); err != nil {
		t.Fatalf("decode ws message: %v payload=%s", err, payload)
	}
	return msg
}

func (c *testWS) readMessageMaybe(t *testing.T, timeout time.Duration) (wsMessage, bool) {
	t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(timeout))
	payload, err := readServerFrameMaybe(c.reader)
	if err != nil {
		if isTimeout(err) {
			return wsMessage{}, false
		}
		t.Fatalf("read optional websocket message: %v", err)
	}
	var msg wsMessage
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	if err := dec.Decode(&msg); err != nil {
		t.Fatalf("decode optional ws message: %v payload=%s", err, payload)
	}
	return msg, true
}

func readServerFrame(t *testing.T, reader *bufio.Reader) []byte {
	t.Helper()
	header := make([]byte, 2)
	if _, err := io.ReadFull(reader, header); err != nil {
		t.Fatalf("read frame header: %v", err)
	}
	length := uint64(header[1] & 0x7f)
	switch length {
	case 126:
		var b [2]byte
		if _, err := io.ReadFull(reader, b[:]); err != nil {
			t.Fatalf("read 126 length: %v", err)
		}
		length = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err := io.ReadFull(reader, b[:]); err != nil {
			t.Fatalf("read 127 length: %v", err)
		}
		length = binary.BigEndian.Uint64(b[:])
	}
	payload := make([]byte, int(length))
	if _, err := io.ReadFull(reader, payload); err != nil {
		t.Fatalf("read payload: %v", err)
	}
	return payload
}

func readServerFrameMaybe(reader *bufio.Reader) ([]byte, error) {
	header := make([]byte, 2)
	if _, err := io.ReadFull(reader, header); err != nil {
		return nil, err
	}
	length := uint64(header[1] & 0x7f)
	switch length {
	case 126:
		var b [2]byte
		if _, err := io.ReadFull(reader, b[:]); err != nil {
			return nil, err
		}
		length = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err := io.ReadFull(reader, b[:]); err != nil {
			return nil, err
		}
		length = binary.BigEndian.Uint64(b[:])
	}
	payload := make([]byte, int(length))
	if _, err := io.ReadFull(reader, payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func maskedFrame(payload []byte) []byte {
	headerLen := 2
	switch {
	case len(payload) < 126:
	case len(payload) <= 0xffff:
		headerLen = 4
	default:
		headerLen = 10
	}
	out := make([]byte, headerLen+4+len(payload))
	out[0] = 0x81
	switch {
	case len(payload) < 126:
		out[1] = 0x80 | byte(len(payload))
	case len(payload) <= 0xffff:
		out[1] = 0x80 | 126
		binary.BigEndian.PutUint16(out[2:4], uint16(len(payload)))
	default:
		out[1] = 0x80 | 127
		binary.BigEndian.PutUint64(out[2:10], uint64(len(payload)))
	}
	maskStart := headerLen
	mask := out[maskStart : maskStart+4]
	if _, err := rand.Read(mask); err != nil {
		copy(mask, []byte{1, 2, 3, 4})
	}
	for i, b := range payload {
		out[maskStart+4+i] = b ^ mask[i&3]
	}
	return out
}

func redactTokenForLog(token string) string {
	if len(token) <= 8 {
		return "[redacted]"
	}
	return token[:8] + "...[redacted]"
}

func catalogGroups(catalog map[string]any) []map[string]any {
	raw, _ := catalog["groups"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if group, ok := item.(map[string]any); ok {
			out = append(out, group)
		}
	}
	return out
}

func catalogGroup(groups []map[string]any, providerID string) map[string]any {
	for _, group := range groups {
		if group["providerId"] == providerID {
			return group
		}
	}
	return nil
}

func groupSummary(groups []map[string]any) string {
	parts := make([]string, 0, len(groups))
	for _, group := range groups {
		models, _ := group["models"].([]any)
		modes, _ := group["modes"].([]any)
		parts = append(parts, fmt.Sprintf("%s:%s models=%d modes=%d", group["providerId"], group["status"], len(models), len(modes)))
	}
	return strings.Join(parts, ", ")
}

func providerItems(raw any) []map[string]any {
	rows, _ := raw.([]any)
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		if item, ok := row.(map[string]any); ok {
			out = append(out, item)
		}
	}
	return out
}

func assertWireProvider(t *testing.T, providers []map[string]any, id, status string, enabled bool) map[string]any {
	t.Helper()
	for _, provider := range providers {
		if provider["id"] == id {
			if provider["status"] != status || provider["enabled"] != enabled {
				t.Fatalf("provider %s = %#v, want status=%s enabled=%v in %#v", id, provider, status, enabled, providers)
			}
			return provider
		}
	}
	t.Fatalf("provider %s missing from %#v", id, providers)
	return nil
}

func assertWirePlanUsageRateLimit(t *testing.T, snapshot map[string]any, providerID string) {
	t.Helper()
	if snapshot["providerId"] != providerID {
		t.Fatalf("plan usage provider = %#v, want %s; snapshot=%#v", snapshot["providerId"], providerID, snapshot)
	}
	entries, _ := snapshot["entries"].([]any)
	if len(entries) != 1 {
		t.Fatalf("plan usage entries = %#v, want one", snapshot["entries"])
	}
	entry := entries[0].(map[string]any)
	if entry["kind"] != "rate-limit" || entry["id"] != "five_hour" || entry["resetsAt"] != "2026-07-10T20:00:00Z" {
		t.Fatalf("rate-limit entry = %#v", entry)
	}
}

func providerItemsSummary(providers []map[string]any) string {
	parts := make([]string, 0, len(providers))
	for _, provider := range providers {
		autoEnv := ""
		if env, ok := provider["autoEnv"].(map[string]any); ok && len(env) > 0 {
			autoEnv = fmt.Sprintf(" autoEnv=%v", env)
		}
		parts = append(parts, fmt.Sprintf("%s:%s enabled=%v%s", provider["id"], provider["status"], provider["enabled"], autoEnv))
	}
	return strings.Join(parts, ", ")
}

func installWireNodeWrapper(t *testing.T, dir string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not available for mock ACP detection: %v", err)
	}
	writeWireExecutable(t, filepath.Join(dir, "node"), fmt.Sprintf("#!/bin/sh\nexec %s \"$@\"\n", shellQuoteForWire(node)))
}

func installWireFakeAgentWrapper(t *testing.T, dir, name string) {
	t.Helper()
	writeWireExecutable(t, filepath.Join(dir, name), fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then\n  if [ -n \"$WORKASS_WIRE_FAKE_CLI_VERSION\" ]; then echo \"$WORKASS_WIRE_FAKE_CLI_VERSION\"; exit 0; fi\n  exit 1\nfi\nWORKASS_WIRE_FAKE_ACP=1 exec %s -test.run=TestWireFakeACPHelper -- \"$@\"\n", shellQuoteForWire(os.Args[0])))
}

func installWireNativeFrontierFixtures(t *testing.T, root, dir string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not available for native frontier detection: %v", err)
	}
	writeWireExecutable(t, filepath.Join(dir, "claude"), "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo '2.1.217 (Claude Code)'; fi\nexit 0\n")
	writeWireExecutable(t, filepath.Join(dir, "codex"), fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo 'codex-cli 0.145.0'; exit 0; fi\nexec %s \"$@\"\n", shellQuoteForWire(node)))
	appServerArgs, err := json.Marshal([]string{filepath.Join(root, "desktop", "acp", "mock-codex-app-server.mjs")})
	if err != nil {
		t.Fatalf("encode Codex fixture args: %v", err)
	}
	t.Setenv("WORKASS_NODE", node)
	t.Setenv("WORKASS_CLAUDE_SDK_MODULE", filepath.Join(root, "desktop", "acp", "mock-claude-agent-sdk.mjs"))
	t.Setenv("WORKASS_CODEX_APP_SERVER_ARGS", string(appServerArgs))
}

func installWireBlockingFakeAgentWrapper(t *testing.T, dir, name, startedPath, releasePath string) {
	t.Helper()
	writeWireExecutable(t, filepath.Join(dir, name), fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then\n  if [ -n \"$WORKASS_WIRE_FAKE_CLI_VERSION\" ]; then echo \"$WORKASS_WIRE_FAKE_CLI_VERSION\"; exit 0; fi\n  exit 1\nfi\n: > %s\nwhile [ ! -e %s ]; do sleep 0.02; done\nWORKASS_WIRE_FAKE_ACP=1 exec %s -test.run=TestWireFakeACPHelper -- \"$@\"\n", shellQuoteForWire(startedPath), shellQuoteForWire(releasePath), shellQuoteForWire(os.Args[0])))
}

func writeWireExecutable(t *testing.T, path, script string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write executable %s: %v", path, err)
	}
}

func waitForWireFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for file %s", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func shellQuoteForWire(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func buildWireWorkassAgentBinary(t *testing.T, root string) string {
	t.Helper()
	suffix := ""
	if filepath.Ext(os.Args[0]) == ".exe" {
		suffix = ".exe"
	}
	out := filepath.Join(t.TempDir(), "workass-agent"+suffix)
	cmd := exec.Command("go", "build", "-trimpath", "-o", out, "./cmd/workass-agent")
	cmd.Dir = root
	data, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build workass-agent: %v\n%s", err, data)
	}
	return out
}

func getenvDefaultForWire(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func modelsEndpointForWire(baseURL string) string {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if strings.HasSuffix(baseURL, "/models") {
		return baseURL
	}
	return baseURL + "/models"
}

type wireFakeOpenAI struct {
	server *httptest.Server
	mu     sync.Mutex
	models []string
	reqs   []wireFakeChatRequest
}

type wireFakeChatRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
	Stream bool `json:"stream"`
}

func newWireFakeOpenAI(t *testing.T, models ...string) *wireFakeOpenAI {
	t.Helper()
	fake := &wireFakeOpenAI{models: append([]string(nil), models...)}
	if len(fake.models) == 0 {
		fake.models = []string{"wire-local-first"}
	}
	fake.server = httptest.NewServer(http.HandlerFunc(fake.handle))
	return fake
}

func (f *wireFakeOpenAI) URL() string {
	return f.server.URL
}

func (f *wireFakeOpenAI) Close() {
	f.server.Close()
}

func (f *wireFakeOpenAI) LastChatRequest() wireFakeChatRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reqs) == 0 {
		return wireFakeChatRequest{}
	}
	return f.reqs[len(f.reqs)-1]
}

func (f *wireFakeOpenAI) handle(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/v1/models":
		w.Header().Set("Content-Type", "application/json")
		items := make([]map[string]string, 0, len(f.models))
		for _, model := range f.models {
			items = append(items, map[string]string{"id": model})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": items})
	case "/v1/chat/completions":
		var req wireFakeChatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.reqs = append(f.reqs, req)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"wire native "}}]}`+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"ok"}}]}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":4,"total_tokens":9}}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	default:
		http.NotFound(w, r)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}
	return root
}

func readJSONFile(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read json %s: %v", path, err)
	}
	var out map[string]any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		t.Fatalf("decode json %s: %v", path, err)
	}
	return out
}

func readMockTracePrompts(t *testing.T, path string) []map[string]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open mock trace: %v", err)
	}
	defer f.Close()
	var out []map[string]string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var item map[string]any
		dec := json.NewDecoder(strings.NewReader(scanner.Text()))
		dec.UseNumber()
		if err := dec.Decode(&item); err != nil {
			t.Fatalf("decode mock trace: %v", err)
		}
		out = append(out, map[string]string{"sessionId": fmt.Sprint(item["sessionId"]), "text": fmt.Sprint(item["text"])})
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan mock trace: %v", err)
	}
	return out
}

func stringSliceFromAny(raw any) []string {
	items, _ := raw.([]any)
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, fmt.Sprint(item))
	}
	return out
}

func envHasWireRepoFile(payload map[string]any, repoName, relPath string) bool {
	repos, _ := payload["repos"].([]any)
	for _, raw := range repos {
		repo, _ := raw.(map[string]any)
		if repo["name"] != repoName {
			continue
		}
		files, _ := repo["files"].([]any)
		for _, rawFile := range files {
			file, _ := rawFile.(map[string]any)
			if file["path"] == relPath {
				return true
			}
		}
	}
	return false
}

func waitWireProcStateForChat(t *testing.T, manager *acp.Manager, chatID string, state acp.EngineState, timeout time.Duration) map[string]any {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		for _, proc := range manager.Processes() {
			if proc["chatId"] == chatID && proc["state"] == string(state) {
				return proc
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for chat %s state %s; processes=%#v", chatID, state, manager.Processes())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func wireFileSHA256(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read hash file: %v", err)
	}
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum[:])
}

func requireGitBinary(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git unavailable: %v", err)
	}
}

func initWireGitRepo(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	runWireFixtureCommand(t, "", "git", "init", dir)
	runWireGit(t, dir, "checkout", "-b", "main")
	runWireGit(t, dir, "config", "user.email", "workass-test@example.com")
	runWireGit(t, dir, "config", "user.name", "Workass Test")
	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir fixture parent: %v", err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
	}
	runWireGit(t, dir, "add", ".")
	runWireGit(t, dir, "commit", "-m", "init")
}

func runWireGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	runWireFixtureCommand(t, dir, "git", append([]string{"-C", dir}, args...)...)
}

func runWireFixtureCommand(t *testing.T, dir, command string, args ...string) {
	t.Helper()
	cmd := exec.Command(command, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", command, strings.Join(args, " "), err, out)
	}
}

func TestWireFakeACPHelper(t *testing.T) {
	if os.Getenv("WORKASS_WIRE_FAKE_ACP") != "1" {
		return
	}
	runWireFakeACP()
	os.Exit(0)
}

func runWireFakeACP() {
	type rpc struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      any             `json:"id,omitempty"`
		Method  string          `json:"method,omitempty"`
		Params  json.RawMessage `json:"params,omitempty"`
		Result  any             `json:"result,omitempty"`
		Error   any             `json:"error,omitempty"`
	}
	write := func(message any) {
		data, _ := json.Marshal(message)
		_, _ = os.Stdout.Write(append(data, '\n'))
	}
	respond := func(id any, result any) {
		write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	}
	fail := func(id any, message string) {
		write(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -32601, "message": message}})
	}
	notify := func(sessionID string, update any) {
		write(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": sessionID, "update": update}})
	}
	mode := os.Getenv("WORKASS_WIRE_FAKE_ACP_MODE")
	sessionSeq := 0
	resetCredits := 1
	redeemedResetKeys := make(map[string]bool)
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var msg rpc
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.UseNumber()
		if err := dec.Decode(&msg); err != nil || msg.Method == "" {
			continue
		}
		recordWireFakeACPMethod(msg.Method)
		params := map[string]any{}
		if len(msg.Params) > 0 {
			dec := json.NewDecoder(bytes.NewReader(msg.Params))
			dec.UseNumber()
			_ = dec.Decode(&params)
		}
		switch msg.Method {
		case "initialize":
			capabilities := map[string]any{
				"promptCapabilities": map[string]any{"image": false},
			}
			if mode == "plan-limits" {
				capabilities["_meta"] = map[string]any{"workassClaudeUsageRequest": true}
			}
			if mode == "codex-reset" {
				capabilities["_meta"] = map[string]any{
					"workassCodexRateLimitsRequest":     true,
					"workassCodexRateLimitResetRequest": true,
				}
			}
			respond(msg.ID, map[string]any{
				"protocolVersion":   1,
				"agentInfo":         map[string]any{"name": "Wire Fake ACP", "version": "0.0.0"},
				"agentCapabilities": capabilities,
				"authMethods":       []any{},
			})
		case "session/new":
			sessionSeq++
			respond(msg.ID, map[string]any{"sessionId": fmt.Sprintf("wire-fake-%d-%d", os.Getpid(), sessionSeq), "configOptions": wireFakeConfigOptions()})
		case "session/set_config_option":
			respond(msg.ID, map[string]any{"configOptions": wireFakeConfigOptions()})
		case "session/close":
			respond(msg.ID, map[string]any{})
		case "session/prompt":
			sessionID := fmt.Sprint(params["sessionId"])
			if mode == "plan-usage" {
				notify(sessionID, map[string]any{
					"sessionUpdate": "usage_update",
					"used":          28320,
					"size":          200000,
					"_meta": map[string]any{
						"_claude/rateLimit": map[string]any{
							"status":          "allowed",
							"resetsAt":        1783713600,
							"rateLimitType":   "five_hour",
							"overageStatus":   "allowed",
							"overageResetsAt": 1783708800,
							"isUsingOverage":  false,
						},
					},
				})
				respond(msg.ID, map[string]any{"stopReason": "end_turn"})
				continue
			}
			time.Sleep(160 * time.Millisecond)
			notify(sessionID, map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": wireFakePromptText(params["prompt"])}})
			respond(msg.ID, map[string]any{"stopReason": "end_turn"})
		case "_workass/claude/usage":
			if mode != "plan-limits" {
				fail(msg.ID, "wire fake method not found: "+msg.Method)
				continue
			}
			respond(msg.ID, fakeWireClaudeStructuredUsage())
		case "_workass/codex/rate-limits":
			if mode != "codex-reset" {
				fail(msg.ID, "wire fake method not found: "+msg.Method)
				continue
			}
			respond(msg.ID, fakeWireCodexRateLimits(resetCredits))
		case "_workass/codex/rate-limit-reset/consume":
			if mode != "codex-reset" {
				fail(msg.ID, "wire fake method not found: "+msg.Method)
				continue
			}
			key := strings.TrimSpace(fieldString(params, "idempotencyKey"))
			if key == "" {
				fail(msg.ID, "idempotencyKey is required")
				continue
			}
			outcome := "noCredit"
			if redeemedResetKeys[key] {
				outcome = "alreadyRedeemed"
			} else if resetCredits > 0 {
				redeemedResetKeys[key] = true
				resetCredits--
				outcome = "reset"
			}
			respond(msg.ID, map[string]any{"outcome": outcome, "rateLimits": fakeWireCodexRateLimits(resetCredits)})
		default:
			fail(msg.ID, "wire fake method not found: "+msg.Method)
		}
	}
}

func fakeWireCodexRateLimits(available int) map[string]any {
	credits := []any{}
	if available > 0 {
		credits = append(credits, map[string]any{
			"id": "RateLimitResetCredit_wire", "resetType": "codexRateLimits", "status": "available",
			"expiresAt": 1784577600, "title": "Full reset", "description": "A free Codex reset",
		})
	}
	return map[string]any{
		"rateLimits": map[string]any{
			"limitId": "codex", "limitName": "Codex",
			"primary":   map[string]any{"usedPercent": 42, "windowDurationMins": 300, "resetsAt": 1783972800},
			"secondary": map[string]any{"usedPercent": 67, "windowDurationMins": 10080, "resetsAt": 1784145600},
		},
		"rateLimitResetCredits": map[string]any{"availableCount": available, "credits": credits},
	}
}

func recordWireFakeACPMethod(method string) {
	logPath := strings.TrimSpace(os.Getenv("WORKASS_WIRE_FAKE_ACP_METHOD_LOG"))
	if logPath == "" {
		return
	}
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(method + "\n")
}

func fakeWireClaudeStructuredUsage() map[string]any {
	return map[string]any{
		"rate_limits": map[string]any{
			"five_hour": map[string]any{
				"utilization": json.Number("37.5"),
				"resets_at":   "2026-07-13T20:00:00Z",
			},
			"seven_day": map[string]any{
				"utilization": json.Number("78"),
				"resets_at":   "2026-07-15T18:00:00Z",
			},
		},
	}
}

func waitWireFakeMethods(t *testing.T, path string, timeout time.Duration, pred func([]string) bool) []string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		methods := readWireFakeMethods(t, path)
		if pred(methods) {
			return methods
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for wire fake method trace; methods=%v", methods)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func readWireFakeMethods(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read wire fake method trace: %v", err)
	}
	var out []string
	for _, line := range strings.Split(string(raw), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

func countWireMethod(methods []string, method string) int {
	count := 0
	for _, got := range methods {
		if got == method {
			count++
		}
	}
	return count
}

func wireFakeConfigOptions() []any {
	return []any{
		map[string]any{"id": "model", "category": "model", "currentValue": "wire-fake-model", "options": []any{map[string]any{"value": "wire-fake-model", "name": "Wire fake model"}}},
		map[string]any{"id": "mode", "category": "mode", "currentValue": "ask", "options": []any{map[string]any{"value": "ask", "name": "Ask"}}},
	}
}

func wireFakePromptText(raw any) string {
	blocks, _ := raw.([]any)
	var parts []string
	for _, block := range blocks {
		m, _ := block.(map[string]any)
		if m["type"] == "text" {
			parts = append(parts, fmt.Sprint(m["text"]))
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

// Cross-provider handover (user law 2026-07-11): picking another agent's model
// mid-chat must switch the chat's engine on the next turn AND hand the chat
// context to the new agent (history-seeded, replay-once). One engine per chat
// still holds — the swap is serialized at a turn boundary, never multiplexed.
func TestWireProviderSwitchMidChatSharesContext(t *testing.T) {
	root := repoRoot(t)
	renderer := t.TempDir()
	if err := os.WriteFile(filepath.Join(renderer, "index.html"), []byte("<!doctype html><head></head><body></body>"), 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}

	hub := wire.NewHub()
	manager := acp.NewManager(acp.Options{
		RootDir: root,
		Providers: []acp.ProviderConfig{
			{ID: "mock", Name: "Mock Provider", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")}, CWD: root, Env: map[string]string{"WORKASS_MOCK_ACP_DELAY_MS": "5"}, Enabled: true},
			{ID: "fake-agent", Name: "Fake Provider", Command: os.Args[0], Args: []string{"-test.run=TestWireFakeACPHelper", "--"}, CWD: root, Env: map[string]string{"WORKASS_WIRE_FAKE_ACP": "1"}, Enabled: true},
		},
		DefaultProviderID:    "mock",
		Broadcast:            hub.Broadcast,
		InitTimeout:          2 * time.Second,
		StdoutFlushInterval:  10 * time.Millisecond,
		ThoughtFlushInterval: 10 * time.Millisecond,
		RSSSampleInterval:    time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	registerDaemonHandlers(hub, root, manager)

	server := httptest.NewServer(httpserve.New(renderer, hub, nil))
	defer server.Close()
	client := dialTestWS(t, server.URL)
	defer client.conn.Close()

	client.invoke(t, 1, "app-chat:new-session", map[string]any{"tabId": "switch-tab", "chatId": "chat-switch", "providerId": "mock"})
	mockSession := client.waitReply(t, 1, 5*time.Second).Result.(map[string]any)
	if mockSession["providerId"] != "mock" || mockSession["sessionId"] == "" {
		t.Fatalf("mock session = %#v", mockSession)
	}
	sessionID := fmt.Sprint(mockSession["sessionId"])

	// Turn 1 on the mock engine.
	client.invoke(t, 2, "job:start", map[string]any{"kind": "app-chat", "chatId": "chat-switch", "tabId": "switch-tab", "sessionId": sessionID, "prompt": "primer turno con el mock"})
	turn1 := client.waitReply(t, 2, 5*time.Second).Result.(map[string]any)
	turn1End := client.waitJobEvent(t, fmt.Sprint(turn1["id"]), "end", 10*time.Second)
	if turn1End["job"].(map[string]any)["status"] != "done" {
		t.Fatalf("turn1 end = %#v", turn1End)
	}
	t.Logf("trace switch turn1 provider=%v status=%v", turn1End["job"].(map[string]any)["providerId"], turn1End["job"].(map[string]any)["status"])

	// Turn 2 asks for the OTHER provider on the SAME chat/session, carrying the
	// visible history exactly like the renderer does on every send.
	client.invoke(t, 3, "job:start", map[string]any{
		"kind": "app-chat", "chatId": "chat-switch", "tabId": "switch-tab", "sessionId": sessionID,
		"providerId": "fake-agent", "prompt": "ahora seguí vos",
		"history": []any{
			map[string]any{"role": "user", "content": "primer turno con el mock", "at": "2026-07-11T00:00:00Z"},
			map[string]any{"role": "assistant", "content": "respuesta del mock al primer turno", "at": "2026-07-11T00:00:01Z"},
		},
	})
	turn2 := client.waitReply(t, 3, 5*time.Second).Result.(map[string]any)
	if turn2["providerId"] != "fake-agent" {
		t.Fatalf("turn2 providerId = %#v", turn2)
	}
	replaced := client.waitChannelEvent(t, "chat:session-replaced", 10*time.Second).Payload.(map[string]any)
	newSession, _ := replaced["session"].(map[string]any)
	if replaced["oldSessionId"] != sessionID || newSession == nil || newSession["providerId"] != "fake-agent" {
		t.Fatalf("session-replaced = %#v", replaced)
	}
	if fmt.Sprint(newSession["sessionId"]) == sessionID {
		t.Fatalf("session-replaced kept the old session id")
	}
	turn2End := client.waitJobEvent(t, fmt.Sprint(turn2["id"]), "end", 10*time.Second)
	turn2Job := turn2End["job"].(map[string]any)
	if turn2Job["status"] != "done" || turn2Job["providerId"] != "fake-agent" {
		t.Fatalf("turn2 end = %#v", turn2End)
	}
	// The fake agent echoes its prompt: the echoed text must carry the seeded
	// turn-1 context — that IS the cross-provider context handoff.
	result := fmt.Sprint(turn2Job["result"])
	if !strings.Contains(result, "primer turno con el mock") || !strings.Contains(result, "ahora seguí vos") {
		t.Fatalf("turn2 result lacks seeded context: %q", result)
	}
	t.Logf("trace switch turn2 provider=%v newSession=%v seeded=%t", turn2Job["providerId"], newSession["sessionId"], strings.Contains(result, "primer turno con el mock"))

	// Guard: switching providers while a turn is running must be rejected.
	client.invoke(t, 4, "job:start", map[string]any{"kind": "app-chat", "chatId": "chat-switch", "tabId": "switch-tab", "sessionId": fmt.Sprint(newSession["sessionId"]), "prompt": "[mock:slow] turno lento"})
	slow := client.waitReply(t, 4, 5*time.Second).Result.(map[string]any)
	client.invoke(t, 5, "job:start", map[string]any{"kind": "app-chat", "chatId": "chat-switch", "tabId": "switch-tab", "sessionId": fmt.Sprint(newSession["sessionId"]), "providerId": "mock", "prompt": "cambiá ya"})
	rejected := client.waitReply(t, 5, 5*time.Second)
	rejectedText := ""
	if rejected.Error != nil {
		rejectedText = *rejected.Error
	}
	if !strings.Contains(rejectedText, "en curso") {
		t.Fatalf("mid-turn switch not rejected: err=%q %#v", rejectedText, rejected)
	}
	slowEnd := client.waitJobEvent(t, fmt.Sprint(slow["id"]), "end", 15*time.Second)
	t.Logf("trace switch mid-turn rejected=%q slowEnd=%v", rejectedText, slowEnd["job"].(map[string]any)["status"])
}
