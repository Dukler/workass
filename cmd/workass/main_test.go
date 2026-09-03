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
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"workass/internal/acp"
	"workass/internal/chat"
	"workass/internal/httpserve"
	"workass/internal/lease"
	providercontract "workass/internal/provider"
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

func TestFrozenQueueResumeBoundaryIsInertAndActorIndependent(t *testing.T) {
	hub := wire.NewHub()
	registerSessionHandlersWithActor(hub, nil, nil, nil)
	result, err := hub.Invoke("chat:queue-resume", []any{map[string]any{
		"tabId": "old-tab", "chatId": "old-chat", "operationId": "old-resume-operation", "expectedRevision": 91,
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"ok": true, "tabId": "old-tab", "chatId": "old-chat", "operationId": "old-resume-operation",
		"queuePaused": false, "queuePauseRevision": 0, "actorRevision": 0,
	}
	if !reflect.DeepEqual(result, want) {
		t.Fatalf("frozen queue-resume receipt = %#v, want %#v", result, want)
	}
	if _, err := hub.Invoke("chat:queue-resume", []any{map[string]any{"chatId": "old-chat"}}); err == nil {
		t.Fatal("frozen queue-resume boundary accepted a missing operation identity")
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
	marker := filepath.Join(t.TempDir(), "daemon-transient.json")
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

type fakeAppUpdateGate struct {
	readiness acp.AppUpdateReadiness
	cancelled bool
}

func (gate *fakeAppUpdateGate) BeginUpdateDrain() acp.AppUpdateReadiness { return gate.readiness }
func (gate *fakeAppUpdateGate) CancelUpdateDrain()                       { gate.cancelled = true }

func updateControlRequest(t *testing.T, handler http.HandlerFunc, remoteAddr, updateID string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "https://workass.local/workass/update", strings.NewReader(`{"updateId":"`+updateID+`"}`))
	request.RemoteAddr = remoteAddr
	recorder := httptest.NewRecorder()
	handler(recorder, request)
	return recorder
}

func TestLocalUpdateControlRecordsActiveWorkAndExactCommit(t *testing.T) {
	gate := &fakeAppUpdateGate{readiness: acp.AppUpdateReadiness{Ready: true, ForegroundTurns: 1, BackgroundWork: 2}}
	stopped := make(chan struct{}, 1)
	control := newLocalUpdateControl(gate, func() { stopped <- struct{}{} })

	busy := updateControlRequest(t, control.prepare, "127.0.0.1:41000", "update-busy-1234")
	if busy.Code != http.StatusOK || !strings.Contains(busy.Body.String(), `"ready":true`) ||
		!strings.Contains(busy.Body.String(), `"foregroundTurns":1`) {
		t.Fatalf("active-work prepare must not be blocked = %d %s", busy.Code, busy.Body.String())
	}
	reset := updateControlRequest(t, control.cancel, "127.0.0.1:41000", "update-busy-1234")
	if reset.Code != http.StatusOK || !strings.Contains(reset.Body.String(), `"cancelled":true`) {
		t.Fatalf("reset after active-work prepare = %d %s", reset.Code, reset.Body.String())
	}
	gate.cancelled = false

	prepared := updateControlRequest(t, control.prepare, "127.0.0.1:41000", "update-ready-1234")
	if prepared.Code != http.StatusOK || !strings.Contains(prepared.Body.String(), `"ready":true`) {
		t.Fatalf("ready prepare = %d %s", prepared.Code, prepared.Body.String())
	}
	wrong := updateControlRequest(t, control.commit, "127.0.0.1:41000", "update-wrong-1234")
	if wrong.Code != http.StatusConflict {
		t.Fatalf("wrong commit = %d", wrong.Code)
	}
	committed := updateControlRequest(t, control.commit, "127.0.0.1:41000", "update-ready-1234")
	if committed.Code != http.StatusAccepted {
		t.Fatalf("commit = %d %s", committed.Code, committed.Body.String())
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("committed update did not request daemon shutdown")
	}
	retried := updateControlRequest(t, control.commit, "127.0.0.1:41000", "update-ready-1234")
	if retried.Code != http.StatusAccepted {
		t.Fatalf("idempotent commit retry = %d %s", retried.Code, retried.Body.String())
	}
	samePrepare := updateControlRequest(t, control.prepare, "127.0.0.1:41000", "update-ready-1234")
	if samePrepare.Code != http.StatusOK || !strings.Contains(samePrepare.Body.String(), `"committed":true`) {
		t.Fatalf("committed prepare retry = %d %s", samePrepare.Code, samePrepare.Body.String())
	}
	otherPrepare := updateControlRequest(t, control.prepare, "127.0.0.1:41000", "update-other-1234")
	if otherPrepare.Code != http.StatusConflict {
		t.Fatalf("other prepare while committed = %d", otherPrepare.Code)
	}
	wrongCancel := updateControlRequest(t, control.cancel, "127.0.0.1:41000", "update-wrong-1234")
	if wrongCancel.Code != http.StatusConflict || gate.cancelled {
		t.Fatalf("wrong committed cancel = %d gate=%v", wrongCancel.Code, gate.cancelled)
	}
	recovered := updateControlRequest(t, control.cancel, "127.0.0.1:41000", "update-ready-1234")
	if recovered.Code != http.StatusOK || !gate.cancelled {
		t.Fatalf("exact committed cancel = %d gate=%v", recovered.Code, gate.cancelled)
	}
	recoveredAgain := updateControlRequest(t, control.cancel, "127.0.0.1:41000", "update-ready-1234")
	if recoveredAgain.Code != http.StatusOK || !strings.Contains(recoveredAgain.Body.String(), `"alreadyCancelled":true`) {
		t.Fatalf("idempotent committed cancel = %d %s", recoveredAgain.Code, recoveredAgain.Body.String())
	}
}

func TestLocalUpdateControlIsLoopbackOnlyAndCanCancel(t *testing.T) {
	gate := &fakeAppUpdateGate{readiness: acp.AppUpdateReadiness{Ready: true}}
	control := newLocalUpdateControl(gate, func() {})
	remote := updateControlRequest(t, control.prepare, "192.168.1.44:41000", "update-remote-1234")
	if remote.Code != http.StatusForbidden {
		t.Fatalf("remote prepare = %d", remote.Code)
	}
	prepared := updateControlRequest(t, control.prepare, "[::1]:41000", "update-cancel-1234")
	if prepared.Code != http.StatusOK {
		t.Fatalf("prepare = %d %s", prepared.Code, prepared.Body.String())
	}
	cancelled := updateControlRequest(t, control.cancel, "[::1]:41000", "update-cancel-1234")
	if cancelled.Code != http.StatusOK || !gate.cancelled {
		t.Fatalf("cancel = %d gate=%v", cancelled.Code, gate.cancelled)
	}
}

func TestWireListDirBrowsesDaemonFilesystem(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, name := range []string{"beta", "Alpha"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", name, err)
		}
	}
	for _, name := range []string{"Downloads", "Documents"} {
		if err := os.Mkdir(filepath.Join(home, name), 0o755); err != nil {
			t.Fatalf("mkdir home %s: %v", name, err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, "not-a-folder.txt"), []byte("ignored"), 0o644); err != nil {
		t.Fatalf("write home file: %v", err)
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
		t.Fatalf("fs:list-dir home invoke: %v", err)
	}
	homeListing := normalize(raw)
	if filepath.Clean(fmt.Sprint(homeListing["path"])) != filepath.Clean(home) || filepath.Clean(fmt.Sprint(homeListing["parent"])) != filepath.Dir(filepath.Clean(home)) {
		t.Fatalf("server home location = %#v", homeListing)
	}
	homeEntries := anySlice(homeListing["entries"])
	if len(homeEntries) != 2 || mapFromAnyMain(homeEntries[0])["name"] != "Documents" || mapFromAnyMain(homeEntries[1])["name"] != "Downloads" {
		t.Fatalf("server home directories = %#v", homeEntries)
	}
	for _, entry := range homeEntries {
		name := fmt.Sprint(mapFromAnyMain(entry)["name"])
		if name == "Inicio" || name == "Workspace" || name == "App" {
			t.Fatalf("server home contains synthetic shortcut %q: %#v", name, homeEntries)
		}
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

func TestWireCreateDirCreatesOneExactChild(t *testing.T) {
	parent := t.TempDir()
	hub := wire.NewHub()
	registerDaemonHandlers(hub, t.TempDir(), nil)

	raw, err := hub.Invoke("fs:create-dir", []any{map[string]any{"parent": parent, "name": "  New Project  "}})
	if err != nil {
		t.Fatalf("fs:create-dir invoke: %v", err)
	}
	created := mapFromAnyMain(raw)
	want := filepath.Join(parent, "New Project")
	if created["error"] != nil || filepath.Clean(fmt.Sprint(created["parent"])) != filepath.Clean(parent) || filepath.Clean(fmt.Sprint(created["path"])) != want || created["name"] != "New Project" {
		t.Fatalf("created folder reply = %#v", created)
	}
	if info, err := os.Stat(want); err != nil || !info.IsDir() {
		t.Fatalf("created folder stat = %#v, %v", info, err)
	}

	listedRaw, err := hub.Invoke("fs:list-dir", []any{parent})
	if err != nil {
		t.Fatalf("fs:list-dir after create: %v", err)
	}
	encoded, err := json.Marshal(listedRaw)
	if err != nil {
		t.Fatalf("marshal listing after create: %v", err)
	}
	var listed map[string]any
	if err := json.Unmarshal(encoded, &listed); err != nil {
		t.Fatalf("decode listing after create: %v", err)
	}
	entries := anySlice(listed["entries"])
	if len(entries) != 1 || mapFromAnyMain(entries[0])["name"] != "New Project" {
		t.Fatalf("created folder listing = %#v", entries)
	}
}

func TestWireCreateDirRejectsTraversalDuplicatesAndNonDirectoryParents(t *testing.T) {
	parent := t.TempDir()
	fileParent := filepath.Join(t.TempDir(), "file.txt")
	if err := os.WriteFile(fileParent, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	hub := wire.NewHub()
	registerDaemonHandlers(hub, t.TempDir(), nil)

	for _, tc := range []struct {
		name   string
		parent string
		child  string
	}{
		{name: "missing parent", parent: "", child: "child"},
		{name: "blank", parent: parent, child: "  "},
		{name: "dot", parent: parent, child: "."},
		{name: "parent traversal", parent: parent, child: ".."},
		{name: "slash traversal", parent: parent, child: "nested/child"},
		{name: "windows traversal", parent: parent, child: `nested\child`},
		{name: "file parent", parent: fileParent, child: "child"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := hub.Invoke("fs:create-dir", []any{map[string]any{"parent": tc.parent, "name": tc.child}})
			if err != nil {
				t.Fatalf("invoke: %v", err)
			}
			result := mapFromAnyMain(raw)
			if strings.TrimSpace(fmt.Sprint(result["error"])) == "" || result["path"] != nil {
				t.Fatalf("unsafe create result = %#v", result)
			}
		})
	}

	existing := filepath.Join(parent, "Existing")
	if err := os.Mkdir(existing, 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := hub.Invoke("fs:create-dir", []any{map[string]any{"parent": parent, "name": "Existing"}})
	if err != nil {
		t.Fatalf("duplicate invoke: %v", err)
	}
	result := mapFromAnyMain(raw)
	if strings.TrimSpace(fmt.Sprint(result["error"])) == "" || result["path"] != nil {
		t.Fatalf("duplicate create result = %#v", result)
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
	sessionState := sharedSessionStore(stateDir)
	manager := acp.NewManager(acp.Options{
		RootDir:           root,
		Provider:          acp.ProviderConfig{ID: "mock", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")}, CWD: root, Env: map[string]string{"WORKASS_MOCK_ACP_DELAY_MS": "250"}, Enabled: true, Label: "Workass Mock ACP"},
		DefaultProviderID: "mock",
		Broadcast:         hub.Broadcast,
		StateDir:          stateDir,
	})
	providerChats := newProviderChatRuntime(manager, sessionState, stateDir)
	t.Cleanup(func() {
		_ = providerChats.Close(context.Background())
		manager.Reset()
	})
	registerDaemonHandlers(hub, root, manager, daemonOptions{StateDir: stateDir, ProviderChats: providerChats})

	server := httptest.NewServer(httpserve.New(renderer, hub, nil))
	defer server.Close()
	client := dialTestWS(t, server.URL)
	defer client.conn.Close()
	readyRefresh := client.waitChannelEvent(t, "agent:apply", 2*time.Second)
	if fieldString(mapFromAnyMain(readyRefresh.Payload), "action") != "session-refresh" {
		t.Fatalf("client-ready refresh = %#v", readyRefresh.Payload)
	}

	chatID := "chat-wire-tab"
	createWireActorChat(t, client, 100, "wire-tab", chatID, "mock")
	client.invokeRaw(t, 0, "app-chat:new-session", map[string]any{"tabId": "wire-tab", "chatId": chatID})
	missingOperation := client.waitReply(t, 0, 5*time.Second)
	missingResult := mapFromAnyMain(missingOperation.Result)
	if !strings.Contains(fieldString(missingResult, "error"), "stable operationId") {
		t.Fatalf("new-session without operation id = %#v", missingOperation)
	}
	client.invoke(t, 1, "app-chat:new-session", map[string]any{"tabId": "wire-tab", "chatId": chatID})
	sessionReply := client.waitReply(t, 1, 5*time.Second)
	if sessionReply.Error != nil {
		t.Fatalf("new-session error: %s", *sessionReply.Error)
	}
	session := sessionReply.Result.(map[string]any)
	sessionID := fieldString(session, "sessionId")
	if sessionID == "" || session["agent"] != "Workass Mock ACP" {
		t.Fatalf("session reply = %#v", session)
	}

	client.invoke(t, 101, "job:start", map[string]any{
		"kind": "app-chat", "chatId": chatID, "sessionId": sessionID, "tabId": "wire-tab",
		"prompt": "must not persist", "operationId": "bearer:credential",
		"userMessageId": "invalid-op-user", "assistantMessageId": "invalid-op-assistant",
	})
	invalidOperation := client.waitReply(t, 101, 5*time.Second)
	if invalidOperation.Error == nil || !strings.Contains(*invalidOperation.Error, "forbidden secret-shaped") {
		t.Fatalf("secret-shaped job:start operation was not rejected at handler boundary: %+v", invalidOperation)
	}
	client.invoke(t, 102, "job:start", map[string]any{
		"kind": "app-chat", "chatId": strings.Repeat("c", 257), "sessionId": sessionID, "tabId": "wire-tab",
		"prompt": "must not resolve actor", "operationId": "bounded-operation",
		"userMessageId": "invalid-chat-user", "assistantMessageId": "invalid-chat-assistant",
	})
	invalidChat := client.waitReply(t, 102, 5*time.Second)
	if invalidChat.Error == nil || !strings.Contains(*invalidChat.Error, "invalid chatId") || !strings.Contains(*invalidChat.Error, "too long") {
		t.Fatalf("unbounded job:start chat identity was not rejected at handler boundary: %+v", invalidChat)
	}

	// The frozen renderer contract sends the exact chat/session/message identity
	// and needs the start event PublicJob to echo chatId and status=="running".
	client.invoke(t, 2, "job:start", map[string]any{
		"kind": "app-chat", "chatId": chatID, "sessionId": sessionID, "tabId": "wire-tab",
		"prompt":        "[mock:slow] wire cancel token=public-secret",
		"operationId":   "wire-e2e-turn",
		"userMessageId": "wire-user", "assistantMessageId": "wire-assistant",
	})
	startReply := client.waitReply(t, 2, 5*time.Second)
	if startReply.Error != nil {
		t.Fatalf("job:start error: %s", *startReply.Error)
	}
	job := startReply.Result.(map[string]any)
	jobID := job["id"].(string)
	if job["kind"] != "app-chat" || job["status"] != "running" || job["chatId"] != chatID || job["sessionId"] != nil {
		t.Fatalf("job reply = %#v", job)
	}
	if job["userMessageId"] != "wire-user" || job["assistantMessageId"] != "wire-assistant" ||
		!strings.Contains(fieldString(job, "promptText"), "token=[redacted]") ||
		strings.Contains(fieldString(job, "promptText"), "public-secret") {
		t.Fatalf("public canonical turn fields = %#v", job)
	}

	startEvent := client.waitJobEvent(t, jobID, "start", 5*time.Second)
	startEventJob, ok := startEvent["job"].(map[string]any)
	if !ok || startEvent["type"] != "start" || startEventJob["chatId"] != chatID || startEventJob["status"] != "running" || startEventJob["sessionId"] != sessionID {
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
	providerChats := &providerChatRuntime{
		manager: &acp.Manager{}, sessions: sharedSessionStore(stateDir), stateDir: stateDir,
		actors: make(map[string]*providerChatActor, 200), known: make(map[string]struct{}, 200),
	}
	for i := 0; i < 200; i++ {
		tabID := fmt.Sprintf("t%03d", i)
		chatID := fmt.Sprintf("c%03d", i)
		engine, err := chat.NewEngine(chatID)
		if err != nil {
			t.Fatalf("create digest actor %s: %v", chatID, err)
		}
		if err := engine.Apply(chat.InitializeChat{
			Presentation: chat.PresentationState{
				TabID: tabID, Title: "Digest " + chatID, ProviderID: "mock",
			},
			OperationID: providercontract.OperationID("digest-create-" + chatID), Digest: "digest-create-" + chatID,
		}); err != nil {
			t.Fatalf("initialize digest actor %s: %v", chatID, err)
		}
		if i == 0 {
			if err := engine.Apply(chat.ReplaceStagedQueue{
				OperationID: "digest-queue", Digest: "digest-queue", ExpectedRevision: 0,
				Entries: []chat.StagedQueueEntry{{
					ID: "q000", Text: "queued body token=digest-secret", Source: "host", Delivery: "queue",
				}},
			}); err != nil {
				t.Fatalf("seed actor queue: %v", err)
			}
		}
		providerChats.actors[chatID] = &providerChatActor{engine: engine}
		providerChats.known[chatID] = struct{}{}
	}

	hub := wire.NewHub()
	settings := newAppSettingsStore(filepath.Join(stateDir, "app-settings.json"))
	registerStateDigestHandler(hub, providerChats, nil, settings)
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
	_, hasPresentationRevision := first[presentationRevisionField]
	_, hasIdleQueuePause := first["queuePaused"]
	_, hasIdleQueuePauseRevision := first["queuePauseRevision"]
	if first["tabId"] != "t000" || first["chatId"] != "c000" ||
		first["lastMessageId"] != nil || first["messageCount"] != 0 ||
		first["queueLen"] != 1 || first["queueHeadId"] != "q000" ||
		first["agentQueueRevision"] != 1 || first["runtimeControlRevision"] != 0 ||
		!hasPresentationRevision || hasIdleQueuePause || hasIdleQueuePauseRevision {
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
	providerChats := newProviderChatRuntime(manager, sessionState, stateDir)
	t.Cleanup(func() {
		_ = providerChats.Close(context.Background())
		manager.Reset()
	})
	registerDaemonHandlers(hub, root, manager, daemonOptions{StateDir: stateDir, ProviderChats: providerChats})

	server := httptest.NewServer(httpserve.New(renderer, hub, nil))
	defer server.Close()
	client := dialTestWS(t, server.URL)
	createWireActorChat(t, client, 100, "offline-tab", "offline-chat", "mock")

	client.invoke(t, 1, "app-chat:new-session", map[string]any{"tabId": "offline-tab", "chatId": "offline-chat", "providerId": "mock"})
	sessionReply := client.waitReply(t, 1, 5*time.Second)
	if sessionReply.Error != nil {
		t.Fatalf("new-session error: %s", *sessionReply.Error)
	}
	session := mapFromAnyMain(sessionReply.Result)
	sessionID := fieldString(session, "sessionId")
	if sessionID == "" {
		t.Fatalf("new-session omitted exact native session: %#v", session)
	}
	prompt := "[mock:slow] persist after renderer closes"
	client.invoke(t, 3, "job:start", map[string]any{
		"kind": "app-chat", "title": "Devin · Offline", "chatId": "offline-chat", "tabId": "offline-tab",
		"sessionId": sessionID, "providerId": "mock", "prompt": prompt,
		"operationId": "offline-turn", "userMessageId": "offline-user", "assistantMessageId": "offline-assistant",
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
		snapshot, projectionErr := providerChats.ProjectSession()
		if projectionErr != nil {
			t.Fatalf("project actor while renderer is closed: %v", projectionErr)
		}
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
	root := repoRoot(t)
	stateDir := t.TempDir()
	releaseFile := filepath.Join(t.TempDir(), "release-first-turn")
	renderer := t.TempDir()
	if err := os.WriteFile(filepath.Join(renderer, "index.html"), []byte("<!doctype html><head></head><body></body>"), 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}

	hub := wire.NewHub()
	store := sharedSessionStore(stateDir)
	const tabID, chatID = "busy-collision-tab", "busy-collision-chat"
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: stateDir,
		Provider: acp.ProviderConfig{
			ID: "mock", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")},
			CWD: root, Env: map[string]string{
				"WORKASS_MOCK_ACP_DELAY_MS": "0", "WORKASS_MOCK_ACP_HOLD_FILE": releaseFile,
			}, Enabled: true, Label: "Workass Mock ACP",
		},
		DefaultProviderID: "mock", Broadcast: daemonEventBroadcaster(store, hub.Broadcast),
	})
	providerChats := newProviderChatRuntime(manager, store, stateDir)
	t.Cleanup(func() {
		_ = providerChats.Close(context.Background())
		manager.Reset()
	})
	if _, err := providerChats.CreateRendererChat(map[string]any{
		"tabId": tabID, "chatId": chatID, "operationId": "create-busy-collision-chat",
		"focus": true, "title": "Busy collision", "titleLocked": true, "cwd": root,
		"providerId": "mock", "currentModelId": "mock-deterministic", "currentModeId": "ask",
	}); err != nil {
		t.Fatalf("create chat: %v", err)
	}
	registerDaemonHandlers(hub, root, manager, daemonOptions{StateDir: stateDir, ProviderChats: providerChats})

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
		"operationId": "wire-busy-first", "kind": "app-chat", "title": "Busy collision", "tabId": tabID, "chatId": chatID,
		"sessionId": sessionID, "providerId": "mock", "prompt": "[mock:hold-until-steer] first turn",
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
		"operationId": "wire-busy-follow", "kind": "app-chat", "title": "Busy collision", "tabId": tabID, "chatId": chatID,
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

	queuedSnapshot, err := providerChats.ProjectSession()
	if err != nil {
		t.Fatal(err)
	}
	queuedChat := chatFromSnapshot(queuedSnapshot, tabID)
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

	if err := os.WriteFile(releaseFile, []byte("release"), 0o600); err != nil {
		t.Fatalf("release first held turn: %v", err)
	}
	client.waitJobEvent(t, firstJobID, "end", 8*time.Second)
	deadline := time.Now().Add(8 * time.Second)
	for {
		projected, readErr := providerChats.ProjectSession()
		if readErr != nil {
			t.Fatalf("read drained actor chat: %v", readErr)
		}
		read := chatFromSnapshot(projected, tabID)
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

	hub := wire.NewHub()
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: stateDir,
		Provider: acp.ProviderConfig{
			ID: "mock", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")},
			CWD: root, Enabled: true, Label: "Workass Mock ACP",
			Env: map[string]string{"WORKASS_MOCK_ACP_SESSION_STORE": filepath.Join(stateDir, "mock-provider.json")},
		},
		DefaultProviderID: "mock", Broadcast: daemonEventBroadcaster(sessionState, hub.Broadcast),
	})
	providerChats := newProviderChatRuntime(manager, sessionState, stateDir)
	t.Cleanup(func() {
		_ = providerChats.Close(context.Background())
		manager.Reset()
	})
	if err := providerChats.StartupError(); err != nil {
		t.Fatalf("start actor runtime: %v", err)
	}
	if _, err := providerChats.CreateRendererChat(map[string]any{
		"tabId": tabID, "chatId": chatID, "operationId": "wire-workspace-create", "focus": true,
		"title": "Workspace lifecycle", "cwd": oldCWD, "providerId": "mock",
	}); err != nil {
		t.Fatalf("create workspace actor: %v", err)
	}
	registerDaemonHandlers(hub, root, manager, daemonOptions{StateDir: stateDir, ProviderChats: providerChats})

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
	beforeMove, ok := providerChats.Snapshot(chatID)
	if !ok || beforeMove.ActiveLaneID == "" {
		t.Fatalf("old workspace actor lane = %#v ok=%v", beforeMove, ok)
	}
	oldLaneID := beforeMove.ActiveLaneID

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
	detachDeadline := time.Now().Add(time.Second)
	for {
		actorState, exists := providerChats.Snapshot(chatID)
		if exists && actorState.Lanes[oldLaneID].Phase == "detached" {
			break
		}
		if time.Now().After(detachDeadline) {
			t.Fatalf("workspace move left the old actor lane attached: %#v", actorState.Lanes[oldLaneID])
		}
		time.Sleep(5 * time.Millisecond)
	}
	if cwd, _, ok, err := providerChats.ChatWorkspaceForExactPair(tabID, chatID); err != nil || !ok || filepath.Clean(cwd) != filepath.Clean(targetCWD) {
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
	if cwd, _, ok, err := providerChats.ChatWorkspaceForExactPair(tabID, chatID); err != nil || !ok || filepath.Clean(cwd) != filepath.Clean(secondTargetCWD) {
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
		"operationId": "workspace-next-turn", "userMessageId": "workspace-next-user", "assistantMessageId": "workspace-next-assistant",
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
	if strings.Contains(result, "User: canonical before move") || strings.Contains(result, "Previous conversation") || !strings.Contains(result, "User request:\nnext turn after move") {
		t.Fatalf("next target-cwd turn replayed Workass history or lost the current request: %q", result)
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
	providerChats := newProviderChatRuntime(manager, sessionState, stateDir)
	t.Cleanup(func() {
		_ = providerChats.Close(context.Background())
		manager.Reset()
	})
	registerDaemonHandlers(hub, root, manager, daemonOptions{StateDir: stateDir, ProviderChats: providerChats})

	server := httptest.NewServer(httpserve.New(renderer, hub, nil))
	defer server.Close()
	first := dialTestWS(t, server.URL)
	createWireActorChat(t, first, 101, "reconnect-tab", "reconnect-chat", "mock")
	first.invoke(t, 1, "app-chat:new-session", map[string]any{"tabId": "reconnect-tab", "chatId": "reconnect-chat", "providerId": "mock"})
	sessionReply := first.waitReply(t, 1, 5*time.Second)
	if sessionReply.Error != nil {
		t.Fatalf("new-session error: %s", *sessionReply.Error)
	}
	sessionID := sessionReply.Result.(map[string]any)["sessionId"].(string)
	first.invoke(t, 2, "job:start", map[string]any{
		"kind": "app-chat", "chatId": "reconnect-chat", "tabId": "reconnect-tab", "sessionId": sessionID,
		"providerId": "mock", "prompt": "[mock:permission] reconnect controls",
		"operationId": "reconnect-permission-turn", "userMessageId": "reconnect-permission-user", "assistantMessageId": "reconnect-permission-assistant",
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
	reconnected.invoke(t, 5, "chat:runtime-controls-save", map[string]any{
		"tabId": "reconnect-tab", "chatId": "reconnect-chat", "operationId": "reconnect-control-save",
		"expectedRevision": intValue(chat[runtimeControlRevisionField]), "providerId": "mock",
		"currentModelId": "mock-deterministic[high]", "currentModeId": "bypass", "modelControls": map[string]any{},
	})
	if reply := reconnected.waitReply(t, 5, 2*time.Second); reply.Error != nil || mapFromAnyMain(reply.Result)["currentModelId"] != "mock-deterministic[high]" || mapFromAnyMain(reply.Result)["currentModeId"] != "bypass" {
		t.Fatalf("actor control save after reconnect = %+v", reply)
	}
	// No renderer session:save follows either control change. Read the actor
	// projection—the legacy mirror is no longer a chat recovery authority.
	reconnected.invoke(t, 61, "session:get")
	controlsReply := reconnected.waitReply(t, 61, 2*time.Second)
	if controlsReply.Error != nil {
		t.Fatalf("actor controls readback: %s", *controlsReply.Error)
	}
	persistedChat := chatFromSnapshot(controlsReply.Result.(map[string]any), "reconnect-tab")
	if fieldString(persistedChat, "providerId") != "mock" || fieldString(persistedChat, "currentModelId") != "mock-deterministic[high]" || fieldString(persistedChat, "currentModeId") != "bypass" {
		t.Fatalf("wire controls not durably isolated = provider=%q model=%q mode=%q", fieldString(persistedChat, "providerId"), fieldString(persistedChat, "currentModelId"), fieldString(persistedChat, "currentModeId"))
	}

	reconnected.invoke(t, 7, "job:start", map[string]any{
		"kind": "app-chat", "chatId": "reconnect-chat", "tabId": "reconnect-tab", "sessionId": sessionID,
		"providerId": "mock", "prompt": "[mock:slow] reconnect cancel",
		"operationId": "reconnect-cancel-turn", "userMessageId": "reconnect-cancel-user", "assistantMessageId": "reconnect-cancel-assistant",
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
	providerChats := newProviderChatRuntime(manager, sessionState, stateDir)
	coordinator := newChatControlCoordinator(manager, hub.Broadcast, providerChats)
	registerDaemonHandlers(hub, root, manager, daemonOptions{
		StateDir: stateDir, ChatControl: coordinator, ProviderChats: providerChats,
	})

	created, err := providerChats.CreateChat("Disconnected drain", root, resolvedChatControls{ProviderID: "mock", ModelID: "mock-deterministic", BaseModel: "mock-deterministic", ModeID: "ask"}, false, "test:create-disconnected-drain")
	if err != nil {
		t.Fatal(err)
	}
	tabID, chatID := fieldString(created, "tabId"), fieldString(created, "chatId")

	server := httptest.NewServer(httpserve.New(renderer, hub, nil))
	defer server.Close()
	receipt, err := providerChats.QueueAgentMessage(
		context.Background(), tabID, chatID, "test-disconnected-drain", "[mock:permission] drain before controller attach", "queue",
	)
	if err != nil {
		t.Fatal(err)
	}
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
		actorState, _ := providerChats.Snapshot(chatID)
		t.Fatalf("actor-owned queue did not reach a permission wait without a controller: state=%#v processes=%#v", actorState, manager.Processes())
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
	stateDir := t.TempDir()
	renderer := t.TempDir()
	if err := os.WriteFile(filepath.Join(renderer, "index.html"), []byte("<!doctype html><head></head><body></body>"), 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}

	hub := wire.NewHub()
	sessionState := sharedSessionStore(stateDir)
	manager := acp.NewManager(acp.Options{
		RootDir:             root,
		StateDir:            stateDir,
		Provider:            acp.ProviderConfig{ID: "mock", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")}, CWD: root, Enabled: true, Label: "Workass Mock ACP"},
		DefaultProviderID:   "mock",
		Broadcast:           hub.Broadcast,
		StdoutFlushInterval: 30 * time.Millisecond,
	})
	providerChats := newProviderChatRuntime(manager, sessionState, stateDir)
	t.Cleanup(func() {
		_ = providerChats.Close(context.Background())
		manager.Reset()
	})
	registerDaemonHandlers(hub, root, manager, daemonOptions{StateDir: stateDir, ProviderChats: providerChats})

	server := httptest.NewServer(httpserve.New(renderer, hub, nil))
	defer server.Close()
	client := dialTestWS(t, server.URL)
	defer client.conn.Close()

	chatID := "chat-wire-permission"
	createWireActorChat(t, client, 100, "wire-perm-tab", chatID, "mock")
	client.invoke(t, 1, "app-chat:new-session", map[string]any{"tabId": "wire-perm-tab", "chatId": chatID, "providerId": "mock"})
	sessionReply := client.waitReply(t, 1, 5*time.Second)
	if sessionReply.Error != nil {
		t.Fatalf("new-session error: %s", *sessionReply.Error)
	}
	session := sessionReply.Result.(map[string]any)
	sessionID := session["sessionId"].(string)

	client.invoke(t, 2, "job:start", map[string]any{
		"kind": "app-chat", "chatId": chatID, "sessionId": sessionID, "tabId": "wire-perm-tab", "prompt": "[mock:permission] approve this",
		"operationId": "wire-permission-turn", "userMessageId": "wire-permission-user", "assistantMessageId": "wire-permission-assistant",
	})
	startReply := client.waitReply(t, 2, 5*time.Second)
	if startReply.Error != nil {
		t.Fatalf("job:start error: %s", *startReply.Error)
	}
	job := startReply.Result.(map[string]any)
	jobID := job["id"].(string)
	if job["kind"] != "app-chat" || job["status"] != "running" || job["chatId"] != chatID || job["sessionId"] != nil {
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
			// The frozen renderer permission card requires id, jobId, options[],
			// title, and kind, and returns the selected optionId.
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
			// The frozen LAN bridge turns the selected permission option into
			// exactly invoke('chat:permission-decide', { id, optionId }).
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

func TestWireJobStartReplyGateBlocksProviderAndProjectsFailureAfterReceipt(t *testing.T) {
	root := repoRoot(t)
	stateDir := t.TempDir()
	renderer := t.TempDir()
	if err := os.WriteFile(filepath.Join(renderer, "index.html"), []byte("<!doctype html><body></body>"), 0o644); err != nil {
		t.Fatal(err)
	}

	hub := wire.NewHub()
	sessions := sharedSessionStore(stateDir)
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: stateDir,
		Provider: acp.ProviderConfig{
			ID: "mock", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")},
			CWD: root, Enabled: true, Label: "Workass Mock ACP",
		},
		DefaultProviderID: "mock", Broadcast: hub.Broadcast,
		StdoutFlushInterval: 5 * time.Millisecond, ThoughtFlushInterval: 5 * time.Millisecond,
		RSSSampleInterval: time.Hour,
	})
	providerChats := newProviderChatRuntime(manager, sessions, stateDir, hub.Broadcast)
	t.Cleanup(func() {
		_ = providerChats.Close(context.Background())
		manager.Reset()
	})
	registerDaemonHandlers(hub, root, manager, daemonOptions{StateDir: stateDir, ProviderChats: providerChats})

	client := dialTestWSHandler(t, httpserve.New(renderer, hub, nil), "/")
	defer client.conn.Close()
	const tabID, chatID = "reply-gate-tab", "reply-gate-chat"
	createWireActorChat(t, client, 100, tabID, chatID, "mock")
	client.invoke(t, 1, "job:start", map[string]any{
		"kind": "app-chat", "tabId": tabID, "chatId": chatID, "providerId": "mock",
		"prompt": "[mock:error] fail after the durable reply", "operationId": "reply-gate-turn",
		"userMessageId": "reply-gate-user", "assistantMessageId": "reply-gate-assistant",
	})

	deadline := time.Now().Add(2 * time.Second)
	for {
		state, ok := providerChats.Snapshot(chatID)
		if ok {
			if _, durable := state.Operations["reply-gate-turn"]; durable {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("job:start input did not become durable before reply: state=%#v", state)
		}
		time.Sleep(time.Millisecond)
	}
	actor, err := providerChats.actor(chatID)
	if err != nil {
		t.Fatal(err)
	}
	actor.coordinator.Wake()
	if executed, err := actor.coordinator.ExecuteNext(context.Background()); err != nil || executed {
		t.Fatalf("provider effect crossed blocked reply: executed=%v err=%v", executed, err)
	}
	if processes := manager.Processes(); len(processes) != 0 {
		t.Fatalf("provider process started before job:start reply: %#v", processes)
	}

	first := client.readMessage(t, 5*time.Second)
	if first.T != "reply" || first.ID.String() != "1" || first.Error != nil {
		t.Fatalf("first frame after releasing blocked writer = %#v", first)
	}
	job := mapFromAnyMain(first.Result)
	jobID := fieldString(job, "id")
	if jobID == "" || fieldString(job, "status") != "running" || job["sessionId"] != nil {
		t.Fatalf("durable job receipt = %#v", job)
	}
	start := client.waitJobEvent(t, jobID, "start", 5*time.Second)
	if fieldString(mapFromAnyMain(start["job"]), "sessionId") == "" {
		t.Fatalf("provider start omitted native attachment: %#v", start)
	}
	end := client.waitJobEvent(t, jobID, "end", 5*time.Second)
	ended := mapFromAnyMain(end["job"])
	if fieldString(ended, "status") != "failed" || strings.TrimSpace(fieldString(ended, "error")) == "" {
		t.Fatalf("provider failure was not projected canonically: %#v", ended)
	}
}

func TestWireLostTerminalWaitsForHarnessOrExplicitCancel(t *testing.T) {
	root := repoRoot(t)
	stateDir := t.TempDir()
	renderer := t.TempDir()
	if err := os.WriteFile(filepath.Join(renderer, "index.html"), []byte("<!doctype html><head></head><body></body>"), 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}

	hub := wire.NewHub()
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: stateDir,
		Provider: acp.ProviderConfig{
			ID: "mock", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")},
			CWD: root, Enabled: true, Label: "Workass Mock ACP",
		},
		DefaultProviderID:    "mock",
		Broadcast:            hub.Broadcast,
		StdoutFlushInterval:  5 * time.Millisecond,
		ThoughtFlushInterval: 5 * time.Millisecond,
	})
	sessionState := sharedSessionStore(stateDir)
	providerChats := newProviderChatRuntime(manager, sessionState, stateDir)
	t.Cleanup(func() {
		_ = providerChats.Close(context.Background())
		manager.Reset()
	})
	registerDaemonHandlers(hub, root, manager, daemonOptions{StateDir: stateDir, ProviderChats: providerChats})

	server := httptest.NewServer(httpserve.New(renderer, hub, nil))
	defer server.Close()
	client := dialTestWS(t, server.URL)
	defer client.conn.Close()

	const tabID = "wire-lost-terminal-tab"
	const chatID = "wire-lost-terminal-chat"
	createWireActorChat(t, client, 100, tabID, chatID, "mock")
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
		"operationId": "lost-terminal-turn", "userMessageId": "lost-terminal-user", "assistantMessageId": "lost-terminal-assistant",
	})
	startReply := client.waitReply(t, 2, 5*time.Second)
	if startReply.Error != nil {
		t.Fatalf("job:start error: %s", *startReply.Error)
	}
	jobID := fieldString(mapFromAnyMain(startReply.Result), "id")
	time.Sleep(150 * time.Millisecond)
	if running, ok := manager.RunningJobForChat(tabID, chatID); !ok || fieldString(running, "id") != jobID {
		t.Fatalf("Workass invented completion for a pending harness prompt: running=%#v ok=%v", running, ok)
	}
	client.invoke(t, 3, "job:cancel", jobID)
	cancelReply := client.waitReply(t, 3, 5*time.Second)
	if cancelReply.Error != nil {
		t.Fatalf("job:cancel error: %s", *cancelReply.Error)
	}
	end := client.waitJobEvent(t, jobID, "end", 3*time.Second)
	ended := mapFromAnyMain(end["job"])
	if ended["status"] != "failed" || ended["stopReason"] != "cancelled" || ended["code"] != json.Number("130") {
		t.Fatalf("explicitly cancelled wire end = %#v", ended)
	}
	if result := fieldString(ended, "result"); !strings.Contains(result, "[mock:lost-terminal] complete visibly over wire") {
		t.Fatalf("cancelled wire result lost streamed output: %q", result)
	}
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
	manager := acp.NewManager(acp.Options{
		RootDir:             root,
		StateDir:            stateDir,
		Provider:            acp.ProviderConfig{ID: "mock", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")}, CWD: root, Enabled: true, Label: "Workass Mock ACP"},
		DefaultProviderID:   "mock",
		Broadcast:           daemonEventBroadcaster(sessionState, hub.Broadcast),
		StdoutFlushInterval: 20 * time.Millisecond,
	})
	providerChats := newProviderChatRuntime(manager, sessionState, stateDir)
	t.Cleanup(func() {
		_ = providerChats.Close(context.Background())
		manager.Reset()
	})
	registerDaemonHandlers(hub, root, manager, daemonOptions{StateDir: stateDir, ProviderChats: providerChats})

	server := httptest.NewServer(httpserve.New(renderer, hub, nil))
	defer server.Close()
	client := dialTestWS(t, server.URL)
	defer client.conn.Close()

	createWireActorChat(t, client, 100, "wire-steer-tab", "chat-wire-steer", "mock")
	client.invoke(t, 1, "app-chat:new-session", map[string]any{"tabId": "wire-steer-tab", "chatId": "chat-wire-steer", "providerId": "mock"})
	sessionReply := client.waitReply(t, 1, 5*time.Second)
	if sessionReply.Error != nil {
		t.Fatalf("new-session error: %s", *sessionReply.Error)
	}
	session := mapFromAnyMain(sessionReply.Result)
	sessionID := fieldString(session, "sessionId")
	if sessionID == "" {
		t.Fatalf("new-session omitted exact actor lane: %#v", session)
	}
	t.Logf("trace reply app-chat:new-session sessionId=%s agent=%s", sessionID, session["agent"])
	client.invoke(t, 2, "job:start", map[string]any{
		"kind": "app-chat", "chatId": "chat-wire-steer", "sessionId": sessionID, "tabId": "wire-steer-tab", "prompt": basePrompt,
		"operationId": "wire-steer-base-turn", "userMessageId": "client-u", "assistantMessageId": "client-a",
	})
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
	if persistedSteer == nil || fieldString(persistedSteer, "status") != "done" || fieldString(persistedSteer, "steerState") != "applied" || persistedSteer["steerAnchor"] != nil || fieldString(persistedSteer, "steerBoundary") != "" || fieldString(persistedSteer, "turnRootId") != "client-a" {
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
	client.invoke(t, 5, "chat:archive-load", "wire-steer-tab")
	archiveReply := client.waitReply(t, 5, 2*time.Second)
	if archiveReply.Error != nil {
		t.Fatalf("actor steer archive load: %s", *archiveReply.Error)
	}
	archive := anySlice(archiveReply.Result)
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

func TestWireTraceMockEngineCrashTerminalizesThenNextPromptResumesExactThread(t *testing.T) {
	root := repoRoot(t)
	stateDir := t.TempDir()
	renderer := t.TempDir()
	if err := os.WriteFile(filepath.Join(renderer, "index.html"), []byte("<!doctype html><head></head><body></body>"), 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}

	hub := wire.NewHub()
	manager := acp.NewManager(acp.Options{
		RootDir:  root,
		StateDir: stateDir,
		Provider: acp.ProviderConfig{ID: "mock", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")}, CWD: root, Env: map[string]string{
			"WORKASS_MOCK_ACP_DELAY_MS": "5", "WORKASS_MOCK_ACP_SESSION_STORE": filepath.Join(stateDir, "mock-provider.json"),
		}, Enabled: true, Label: "Workass Mock ACP"},
		DefaultProviderID:    "mock",
		Broadcast:            hub.Broadcast,
		StdoutFlushInterval:  5 * time.Millisecond,
		ThoughtFlushInterval: 5 * time.Millisecond,
		RSSSampleInterval:    time.Hour,
		CompactionEnabled:    false,
	})
	sessionState := sharedSessionStore(stateDir)
	providerChats := newProviderChatRuntime(manager, sessionState, stateDir, hub.Broadcast)
	t.Cleanup(func() {
		_ = providerChats.Close(context.Background())
		manager.Reset()
	})
	registerDaemonHandlers(hub, root, manager, daemonOptions{StateDir: stateDir, ProviderChats: providerChats})

	server := httptest.NewServer(httpserve.New(renderer, hub, nil))
	defer server.Close()
	client := dialTestWS(t, server.URL)
	defer client.conn.Close()

	tabID := "wire-crash-tab"
	createWireActorChat(t, client, 100, tabID, "wire-crash-turn", "mock")
	client.invoke(t, 1, "app-chat:new-session", map[string]any{"tabId": tabID, "chatId": "wire-crash-turn"})
	session := client.waitReply(t, 1, 5*time.Second).Result.(map[string]any)
	oldSessionID := session["sessionId"].(string)
	client.invoke(t, 2, "job:start", map[string]any{
		"kind": "app-chat", "chatId": "wire-crash-turn", "sessionId": oldSessionID, "tabId": tabID,
		"operationId": "engine-crash-turn", "userMessageId": "engine-crash-user", "assistantMessageId": "engine-crash-assistant",
		"prompt": "[mock:crash] first crash",
	})
	startReply := client.waitReply(t, 2, 5*time.Second)
	if startReply.Error != nil {
		t.Fatalf("job:start error: %s", *startReply.Error)
	}
	job := startReply.Result.(map[string]any)
	jobID := job["id"].(string)
	end := client.waitJobEvent(t, jobID, "end", 5*time.Second)
	waitProviderChatIdle(t, providerChats, "wire-crash-turn", 5*time.Second)
	endJob := end["job"].(map[string]any)
	if endJob["status"] != "failed" || endJob["stopReason"] != "engine-crash" || endJob["crashInterrupted"] != true {
		t.Fatalf("crash end job = %#v", endJob)
	}
	t.Logf("trace event job:event type=end id=%s status=%s stopReason=%v crashInterrupted=%v", endJob["id"], endJob["status"], endJob["stopReason"], endJob["crashInterrupted"])
	crashed, _ := providerChats.Snapshot("wire-crash-turn")
	crashedLane := crashed.Lanes[crashed.DesiredLaneID]
	if crashed.Foreground != nil || crashedLane.Phase != chat.LaneDetached || crashedLane.Thread.HeadID != oldSessionID {
		t.Fatalf("host crash did not terminalize and detach the exact thread: %#v", crashed)
	}

	client.invoke(t, 3, "job:start", map[string]any{
		"kind": "app-chat", "chatId": "wire-crash-turn", "sessionId": oldSessionID, "tabId": tabID,
		"operationId": "after-engine-crash", "userMessageId": "after-engine-crash-user", "assistantMessageId": "after-engine-crash-assistant",
		"prompt": "continue on the saved exact thread",
	})
	nextReply := client.waitReply(t, 3, 5*time.Second)
	if nextReply.Error != nil {
		t.Fatalf("next distinct prompt could not resume exact thread: %s", *nextReply.Error)
	}
	nextJob := nextReply.Result.(map[string]any)
	nextEnd := client.waitJobEvent(t, nextJob["id"].(string), "end", 5*time.Second)
	if finished := mapFromAnyMain(nextEnd["job"]); fieldString(finished, "status") != "done" {
		t.Fatalf("next exact-session turn = %#v", finished)
	}
	if live, ok := manager.LiveSession(oldSessionID); !ok || live.ChatID != "wire-crash-turn" {
		t.Fatalf("next prompt did not resume the saved exact provider thread: %+v ok=%v", live, ok)
	}
	client.expectNoEventChannel(t, "chat:engine-recovered", 150*time.Millisecond)
	client.expectNoEventChannel(t, "chat:session-replaced", 150*time.Millisecond)
}

func TestWireTraceMockCrashNeverReplaysAndNextDistinctPromptRuns(t *testing.T) {
	root := repoRoot(t)
	stateDir := t.TempDir()
	traceFile := filepath.Join(stateDir, "crash-prompts.jsonl")
	renderer := t.TempDir()
	if err := os.WriteFile(filepath.Join(renderer, "index.html"), []byte("<!doctype html><head></head><body></body>"), 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}

	hub := wire.NewHub()
	manager := acp.NewManager(acp.Options{
		RootDir:  root,
		StateDir: stateDir,
		Provider: acp.ProviderConfig{ID: "mock", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")}, CWD: root, Env: map[string]string{
			"WORKASS_MOCK_ACP_DELAY_MS": "5", "WORKASS_MOCK_ACP_TRACE_FILE": traceFile,
			"WORKASS_MOCK_ACP_SESSION_STORE": filepath.Join(stateDir, "mock-provider.json"),
		}, Enabled: true, Label: "Workass Mock ACP"},
		DefaultProviderID:    "mock",
		Broadcast:            hub.Broadcast,
		StdoutFlushInterval:  5 * time.Millisecond,
		ThoughtFlushInterval: 5 * time.Millisecond,
		RSSSampleInterval:    time.Hour,
		CompactionEnabled:    false,
	})
	sessionState := sharedSessionStore(stateDir)
	providerChats := newProviderChatRuntime(manager, sessionState, stateDir, hub.Broadcast)
	t.Cleanup(func() {
		_ = providerChats.Close(context.Background())
		manager.Reset()
	})
	registerDaemonHandlers(hub, root, manager, daemonOptions{StateDir: stateDir, ProviderChats: providerChats})

	server := httptest.NewServer(httpserve.New(renderer, hub, nil))
	defer server.Close()
	client := dialTestWS(t, server.URL)
	defer client.conn.Close()

	tabID := "wire-double-crash-tab"
	createWireActorChat(t, client, 100, tabID, tabID, "mock")
	client.invoke(t, 1, "app-chat:new-session", map[string]any{"tabId": tabID, "chatId": tabID})
	session := client.waitReply(t, 1, 5*time.Second).Result.(map[string]any)
	firstSessionID := session["sessionId"].(string)
	client.invoke(t, 2, "job:start", map[string]any{
		"kind": "app-chat", "chatId": tabID, "sessionId": firstSessionID, "tabId": tabID,
		"operationId": "ambiguous-crash-turn-1", "userMessageId": "ambiguous-crash-user-1", "assistantMessageId": "ambiguous-crash-assistant-1",
		"prompt": "[mock:crash] first",
	})
	firstJob := client.waitReply(t, 2, 5*time.Second).Result.(map[string]any)
	end := client.waitJobEvent(t, firstJob["id"].(string), "end", 5*time.Second)
	waitProviderChatIdle(t, providerChats, tabID, 5*time.Second)
	terminalized, _ := providerChats.Snapshot(tabID)
	if terminalized.Foreground != nil || len(terminalized.Ledger) < 2 {
		t.Fatalf("crash without readback did not seal a terminal no-resend receipt: %#v", terminalized)
	}
	assistant := terminalized.Ledger[len(terminalized.Ledger)-1]
	if assistant.OperationID != "ambiguous-crash-turn-1" || assistant.Status != "failed" || !assistant.Interrupted || assistant.RetryPrompt != "" || assistant.Terminal == nil {
		t.Fatalf("crash without readback terminal receipt = %#v", assistant)
	}
	if assistant.NativeTurnID != firstJob["id"].(string) {
		t.Fatalf("terminal crash changed turn identity: assistant=%#v job=%#v", assistant, firstJob)
	}
	endJob := mapFromAnyMain(end["job"])
	if fieldString(endJob, "status") != "failed" || endJob["interrupted"] != true {
		t.Fatalf("unrecoverable crash did not publish terminal job:end: %#v", endJob)
	}
	detachedLane := false
	for _, lane := range terminalized.Lanes {
		if lane.Phase == chat.LaneDetached && lane.Thread.HeadID == firstSessionID {
			detachedLane = true
			break
		}
	}
	if !detachedLane {
		t.Fatalf("crash did not leave the exact lane detached for the next prompt: %#v", terminalized.Lanes)
	}
	t.Logf("trace first crash terminalized session=%s without replay", firstSessionID)

	client.invoke(t, 3, "job:start", map[string]any{
		"kind": "app-chat", "chatId": tabID, "sessionId": firstSessionID, "tabId": tabID,
		"operationId": "ambiguous-crash-turn-2", "userMessageId": "ambiguous-crash-user-2", "assistantMessageId": "ambiguous-crash-assistant-2",
		"prompt": "must not be resent after crash ambiguity",
	})
	nextReply := client.waitReply(t, 3, 5*time.Second)
	if nextReply.Error != nil {
		t.Fatalf("next distinct prompt could not resume the exact thread: %s", *nextReply.Error)
	}
	nextJob := nextReply.Result.(map[string]any)
	nextEnd := client.waitJobEvent(t, nextJob["id"].(string), "end", 5*time.Second)
	if finished := mapFromAnyMain(nextEnd["job"]); fieldString(finished, "status") != "done" {
		t.Fatalf("next distinct turn = %#v", finished)
	}
	client.expectNoEventChannel(t, "chat:engine-recovered", 300*time.Millisecond)
	client.expectNoEventChannel(t, "chat:session-replaced", 150*time.Millisecond)
	if live, ok := manager.LiveSession(firstSessionID); !ok || live.Info.SessionID != firstSessionID {
		t.Fatalf("next prompt did not resume the exact lane: %+v ok=%v", live, ok)
	}
	prompts := readMockTracePrompts(t, traceFile)
	providerInputs := 0
	seenFirst, seenSecond := 0, 0
	for _, prompt := range prompts {
		if strings.HasPrefix(prompt["text"], "[mock:lifecycle]") {
			continue
		}
		providerInputs++
		if prompt["sessionId"] != firstSessionID {
			t.Fatalf("next prompt replaced the exact provider session: %#v", prompts)
		}
		if strings.Contains(prompt["text"], "[mock:crash] first") {
			seenFirst++
		}
		if strings.Contains(prompt["text"], "must not be resent") {
			seenSecond++
		}
	}
	if providerInputs != 2 || seenFirst != 1 || seenSecond != 1 {
		t.Fatalf("crash boundary replayed or lost provider input: %#v", prompts)
	}
	t.Logf("trace crash preserved session=%s and sent each distinct input exactly once", firstSessionID)
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
	stateDir := filepath.Join(t.TempDir(), "state")
	manager := acp.NewManager(acp.Options{
		RootDir:             root,
		StateDir:            stateDir,
		Provider:            acp.ProviderConfig{ID: "mock", Command: os.Args[0], Args: []string{"-test.run=TestWireFakeACPHelper", "--"}, CWD: root, Env: map[string]string{"WORKASS_WIRE_FAKE_ACP": "1"}, Enabled: true, Label: "Wire Fake ACP"},
		DefaultProviderID:   "mock",
		Broadcast:           hub.Broadcast,
		InitTimeout:         2 * time.Second,
		StdoutFlushInterval: 10 * time.Millisecond,
		RSSSampleInterval:   time.Hour,
	})
	sessionState := sharedSessionStore(stateDir)
	providerChats := newProviderChatRuntime(manager, sessionState, stateDir, hub.Broadcast)
	t.Cleanup(func() {
		_ = providerChats.Close(context.Background())
		manager.Reset()
	})
	registerDaemonHandlers(hub, root, manager, daemonOptions{StateDir: stateDir, ProviderChats: providerChats})

	server := httptest.NewServer(httpserve.New(renderer, hub, nil))
	defer server.Close()
	client := dialTestWS(t, server.URL)
	defer client.conn.Close()

	chatID := "chat-wire-env"
	tabID := "wire-env-tab"
	createWireActorChat(t, client, 100, tabID, chatID, "mock")
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

	client.invoke(t, 2, "job:start", map[string]any{
		"kind": "app-chat", "chatId": chatID, "sessionId": sessionID, "tabId": tabID, "cwd": workspace, "prompt": "wire env turn",
		"operationId": "wire-env-turn", "userMessageId": "wire-env-user", "assistantMessageId": "wire-env-assistant",
	})
	startReply := client.waitReply(t, 2, 5*time.Second)
	if startReply.Error != nil {
		t.Fatalf("job:start error: %s", *startReply.Error)
	}
	job := startReply.Result.(map[string]any)
	jobID := job["id"].(string)
	startEvent := client.waitJobEvent(t, jobID, "start", 5*time.Second)
	if startJob := mapFromAnyMain(startEvent["job"]); fieldString(startJob, "sessionId") != sessionID {
		t.Fatalf("job:start event session = %#v, want %s", startJob, sessionID)
	}
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

	client.invoke(t, 3, "chat:env-get", map[string]any{"chatId": chatID, "tabId": tabID})
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

	client.invoke(t, 4, "chat:checkpoints", map[string]any{"chatId": chatID, "tabId": tabID})
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

	client.invoke(t, 5, "chat:diff", map[string]any{"chatId": chatID, "tabId": tabID, "repo": "alpha", "path": "work.txt"})
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

	client.invoke(t, 6, "chat:rewind", map[string]any{
		"tabId": tabID, "chatId": chatID, "turnSeq": 1, "operationId": "wire-env-rewind-1",
	})
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
			Env: map[string]string{
				"WORKASS_MOCK_ACP_DELAY_MS": "10", "WORKASS_MOCK_ACP_SESSION_STORE": filepath.Join(stateDir, "mock-provider.json"),
			},
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
	sessionState := sharedSessionStore(stateDir)
	providerChats := newProviderChatRuntime(manager, sessionState, stateDir, hub.Broadcast)
	t.Cleanup(func() {
		_ = providerChats.Close(context.Background())
		manager.Reset()
	})
	registerDaemonHandlers(hub, repo, manager, daemonOptions{StateDir: stateDir, ProviderChats: providerChats})

	server := httptest.NewServer(httpserve.New(renderer, hub, nil))
	defer server.Close()
	client := dialTestWS(t, server.URL)
	defer client.conn.Close()

	chatID := "chat-wire-hibernate-checkpoint"
	tabID := "wire-hibernate-checkpoint-tab"
	createWireActorChat(t, client, 100, tabID, chatID, "mock")
	client.invoke(t, 1, "app-chat:new-session", map[string]any{"tabId": tabID, "chatId": chatID, "cwd": workspace})
	sessionReply := client.waitReply(t, 1, 5*time.Second)
	if sessionReply.Error != nil {
		t.Fatalf("new-session error: %s", *sessionReply.Error)
	}
	session := sessionReply.Result.(map[string]any)
	oldSessionID := session["sessionId"].(string)
	client.waitFor(t, 5*time.Second, func(msg wsMessage) bool {
		if msg.T != "event" || msg.Channel != "chat:env" {
			return false
		}
		payload, _ := msg.Payload.(map[string]any)
		return payload["chatId"] == chatID && strings.Join(stringSliceFromAny(payload["unchanged"]), ",") == "alpha"
	})
	runWireChatTurn(t, client, 2, chatID, tabID, oldSessionID, "before hibernate checkpoint", workspace)
	hibernated := waitWireProcStateForChat(t, manager, chatID, acp.StateHibernated, 2*time.Second)
	t.Logf("trace lifecycle hibernated chat=%s pid=%v", chatID, hibernated["pid"])

	client.invoke(t, 3, "job:start", map[string]any{
		"kind": "app-chat", "chatId": chatID, "sessionId": oldSessionID, "tabId": tabID, "cwd": workspace, "prompt": "[mock:slow] hibernated checkpoint",
		"operationId": "hibernate-checkpoint-turn", "userMessageId": "hibernate-checkpoint-user", "assistantMessageId": "hibernate-checkpoint-assistant",
	})
	startReply := client.waitReply(t, 3, 5*time.Second)
	if startReply.Error != nil {
		state, _ := providerChats.Snapshot(chatID)
		t.Fatalf("job:start error: %s actor=%#v", *startReply.Error, state)
	}
	job := startReply.Result.(map[string]any)
	jobID := job["id"].(string)
	startEvent := client.waitJobEvent(t, jobID, "start", 5*time.Second)
	if startJob := mapFromAnyMain(startEvent["job"]); fieldString(startJob, "sessionId") != oldSessionID {
		t.Fatalf("hibernated checkpoint start = %#v, want session %s", startJob, oldSessionID)
	}
	if err := os.WriteFile(filepath.Join(alpha, "work.txt"), []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatalf("edit alpha: %v", err)
	}
	end := client.waitJobEvent(t, jobID, "end", 5*time.Second)
	endJob := end["job"].(map[string]any)
	if endJob["status"] != "done" || endJob["sessionId"] != oldSessionID {
		t.Fatalf("hibernated checkpoint end = %#v", endJob)
	}
	waitWireChatCheckpointCount(t, manager, chatID, 1, 5*time.Second)
	envMsg := client.waitFor(t, 5*time.Second, func(msg wsMessage) bool {
		if msg.T != "event" || msg.Channel != "chat:env" {
			return false
		}
		payload, _ := msg.Payload.(map[string]any)
		return payload["chatId"] == chatID && envHasWireRepoFile(payload, "alpha", "work.txt")
	})
	env := envMsg.Payload.(map[string]any)
	t.Logf("trace event chat:env hibernated checkpoint chatId=%s repos=%d", env["chatId"], len(env["repos"].([]any)))

	client.invoke(t, 4, "chat:checkpoints", map[string]any{"chatId": chatID, "tabId": tabID})
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
	client.invoke(t, 5, "chat:diff", map[string]any{"chatId": chatID, "tabId": tabID, "repo": "alpha", "path": "work.txt"})
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
	stateDir := t.TempDir()
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: stateDir,
		Providers: []acp.ProviderConfig{
			{ID: "mock", Name: "Mock Provider", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")}, CWD: root, Env: map[string]string{"WORKASS_MOCK_ACP_SESSION_STORE": filepath.Join(stateDir, "mock-provider.json")}, Enabled: true},
			{ID: "fake-agent", Name: "Fake Provider", Command: os.Args[0], Args: []string{"-test.run=TestWireFakeACPHelper", "--"}, CWD: root, Env: map[string]string{"WORKASS_WIRE_FAKE_ACP": "1"}, Enabled: true},
		},
		DefaultProviderID:    "mock",
		Broadcast:            hub.Broadcast,
		InitTimeout:          2 * time.Second,
		StdoutFlushInterval:  10 * time.Millisecond,
		ThoughtFlushInterval: 10 * time.Millisecond,
		RSSSampleInterval:    time.Hour,
	})
	sessionState := sharedSessionStore(stateDir)
	providerChats := newProviderChatRuntime(manager, sessionState, stateDir, hub.Broadcast)
	t.Cleanup(func() {
		_ = providerChats.Close(context.Background())
		manager.Reset()
	})
	registerDaemonHandlers(hub, root, manager, daemonOptions{StateDir: stateDir, ProviderChats: providerChats})
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
	t.Logf("trace event chat:catalog groups=%s flatModels=%d flatModes=%d", groupSummary(groups), len(catalog["models"].([]any)), len(catalog["modes"].([]any)))

	createWireActorChat(t, client, 100, "wire-mock-tab", "chat-wire-mock", "mock")
	createWireActorChat(t, client, 101, "wire-fake-tab", "chat-wire-fake", "fake-agent")
	client.invoke(t, 1, "app-chat:new-session", map[string]any{"tabId": "wire-mock-tab", "chatId": "chat-wire-mock", "providerId": "mock"})
	client.invoke(t, 2, "app-chat:new-session", map[string]any{"tabId": "wire-fake-tab", "chatId": "chat-wire-fake", "providerId": "fake-agent"})
	mockSession := client.waitReply(t, 1, 5*time.Second).Result.(map[string]any)
	fakeSession := client.waitReply(t, 2, 5*time.Second).Result.(map[string]any)
	if mockSession["providerId"] != "mock" || fakeSession["providerId"] != "fake-agent" {
		t.Fatalf("session providers mock=%#v fake=%#v", mockSession, fakeSession)
	}
	t.Logf("trace reply app-chat:new-session chat=chat-wire-mock provider=%s session=%s", mockSession["providerId"], mockSession["sessionId"])
	t.Logf("trace reply app-chat:new-session chat=chat-wire-fake provider=%s session=%s", fakeSession["providerId"], fakeSession["sessionId"])

	client.invoke(t, 3, "job:start", map[string]any{
		"kind": "app-chat", "chatId": "chat-wire-mock", "sessionId": mockSession["sessionId"], "tabId": "wire-mock-tab", "prompt": "[mock:slow] interleaved mock",
		"operationId": "interleaved-mock-turn", "userMessageId": "interleaved-mock-user", "assistantMessageId": "interleaved-mock-assistant",
	})
	client.invoke(t, 4, "job:start", map[string]any{
		"kind": "app-chat", "chatId": "chat-wire-fake", "sessionId": fakeSession["sessionId"], "tabId": "wire-fake-tab", "prompt": "interleaved fake",
		"operationId": "interleaved-fake-turn", "userMessageId": "interleaved-fake-user", "assistantMessageId": "interleaved-fake-assistant",
	})
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
	stateDir := t.TempDir()
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: stateDir,
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
	sessionState := sharedSessionStore(stateDir)
	providerChats := newProviderChatRuntime(manager, sessionState, stateDir, hub.Broadcast)
	t.Cleanup(func() {
		_ = providerChats.Close(context.Background())
		manager.Reset()
	})
	registerDaemonHandlers(hub, root, manager, daemonOptions{StateDir: stateDir, ProviderChats: providerChats})

	server := httptest.NewServer(httpserve.New(renderer, hub, nil))
	defer server.Close()
	first := dialTestWS(t, server.URL)
	defer first.conn.Close()

	createWireActorChat(t, first, 100, "plan-live-tab", "chat-plan-live", "claude")
	first.invoke(t, 1, "app-chat:new-session", map[string]any{"tabId": "plan-live-tab", "chatId": "chat-plan-live", "providerId": "claude"})
	session := first.waitReply(t, 1, 5*time.Second).Result.(map[string]any)
	first.invoke(t, 2, "job:start", map[string]any{
		"kind": "app-chat", "chatId": "chat-plan-live", "sessionId": session["sessionId"], "tabId": "plan-live-tab", "prompt": "emit plan usage",
		"operationId": "plan-usage-turn", "userMessageId": "plan-usage-user", "assistantMessageId": "plan-usage-assistant",
	})
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
	stateDir := t.TempDir()
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: stateDir,
		Providers: []acp.ProviderConfig{
			{ID: "codex", Name: "Codex Reset Fake", Command: os.Args[0], Args: []string{"-test.run=TestWireFakeACPHelper", "--"}, CWD: root, Env: map[string]string{"WORKASS_WIRE_FAKE_ACP": "1", "WORKASS_WIRE_FAKE_ACP_MODE": "codex-reset"}, Enabled: true},
		},
		DefaultProviderID:   "codex",
		Broadcast:           hub.Broadcast,
		InitTimeout:         2 * time.Second,
		RSSSampleInterval:   time.Hour,
		StdoutFlushInterval: 10 * time.Millisecond,
	})
	sessionState := sharedSessionStore(stateDir)
	providerChats := newProviderChatRuntime(manager, sessionState, stateDir, hub.Broadcast)
	t.Cleanup(func() {
		_ = providerChats.Close(context.Background())
		manager.Reset()
	})
	registerDaemonHandlers(hub, root, manager, daemonOptions{StateDir: stateDir, ProviderChats: providerChats})

	server := httptest.NewServer(httpserve.New(renderer, hub, nil))
	defer server.Close()
	client := dialTestWS(t, server.URL)
	defer client.conn.Close()
	createWireActorChat(t, client, 100, "reset-tab", "reset-chat", "codex")
	client.invoke(t, 1, "app-chat:new-session", map[string]any{"tabId": "reset-tab", "chatId": "reset-chat", "providerId": "codex"})
	sessionReply := client.waitReply(t, 1, 5*time.Second)
	if sessionReply.Error != nil {
		t.Fatalf("new Codex reset session: %s", *sessionReply.Error)
	}
	if sessionID := fieldString(sessionReply.Result.(map[string]any), "sessionId"); sessionID != "" {
		t.Fatalf("empty Codex selection materialized a provider thread: %q", sessionID)
	}
	client.invoke(t, 2, "job:start", map[string]any{
		"kind": "app-chat", "chatId": "reset-chat", "tabId": "reset-tab", "prompt": "materialize the reset fixture",
		"operationId": "reset-first-turn", "userMessageId": "reset-first-user", "assistantMessageId": "reset-first-assistant",
	})
	startReply := client.waitReply(t, 2, 5*time.Second)
	if startReply.Error != nil {
		t.Fatalf("materialize Codex reset session: %s", *startReply.Error)
	}
	job := startReply.Result.(map[string]any)
	if sessionID := fieldString(job, "sessionId"); sessionID != "" {
		t.Fatalf("durable Codex admission prematurely exposed native session identity: %q", sessionID)
	}
	startEvent := client.waitJobEvent(t, fieldString(job, "id"), "start", 5*time.Second)
	sessionID := fieldString(mapFromAnyMain(startEvent["job"]), "sessionId")
	if sessionID == "" {
		t.Fatal("Codex start event omitted its materialized session identity")
	}
	initial := client.waitChannelEvent(t, "chat:plan-usage", 5*time.Second).Payload.(map[string]any)
	initialReset := initial["rateLimitResetCredits"].(map[string]any)
	if fmt.Sprint(initialReset["availableCount"]) != "1" {
		t.Fatalf("initial earned reset = %#v", initialReset)
	}

	_ = client.waitJobEvent(t, fieldString(job, "id"), "end", 5*time.Second)
	client.invoke(t, 3, "app-chat:use-rate-limit-reset", map[string]any{
		"providerId": "codex", "sessionId": sessionID, "idempotencyKey": "wire-reset-attempt", "creditId": "RateLimitResetCredit_wire",
	})
	consume := client.waitReply(t, 3, 5*time.Second)
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

	client.invoke(t, 4, "app-chat:use-rate-limit-reset", map[string]any{
		"providerId": "codex", "sessionId": sessionID, "idempotencyKey": "wire-reset-attempt", "creditId": "RateLimitResetCredit_wire",
	})
	retry := client.waitReply(t, 4, 5*time.Second)
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
	providerChats := newProviderChatRuntime(manager, sessionState, stateDir)
	t.Cleanup(func() {
		_ = providerChats.Close(context.Background())
		manager.Reset()
	})
	if err := providerChats.StartupError(); err != nil {
		t.Fatalf("start actor runtime: %v", err)
	}
	if _, err := providerChats.CreateRendererChat(map[string]any{
		"tabId": "warm-plan-tab", "chatId": "warm-plan-chat", "operationId": "warm-plan-create", "focus": true,
		"title": "Warm plan", "titleLocked": true, "cwd": root, "providerId": "claude",
	}); err != nil {
		t.Fatalf("create plan actor: %v", err)
	}
	registerDaemonHandlers(hub, root, manager, daemonOptions{StateDir: stateDir, ProviderChats: providerChats})

	server := httptest.NewServer(httpserve.New(renderer, hub, nil))
	defer server.Close()
	client := dialTestWS(t, server.URL)
	defer client.conn.Close()
	actorSnapshot, err := providerChats.ProjectSession()
	if err != nil {
		t.Fatalf("project migrated plan chat: %v", err)
	}

	// A renderer reconnect and its initial state save are view lifecycle only.
	// They replay the latest in-memory snapshot but must never initialize an ACP
	// process, resume a provider thread, or create a new session just to fetch
	// plan usage.
	actorSnapshot[globalPresentationOperationField] = "plan-session-save-1"
	client.invoke(t, 1, "session:save", actorSnapshot)
	if reply := client.waitReply(t, 1, 2*time.Second); reply.Error != nil || mapFromAnyMain(reply.Result)["ok"] != true {
		t.Fatalf("initial session save reply = %#v", reply)
	}
	preAttach, ok := providerChats.Snapshot("warm-plan-chat")
	if !ok || preAttach.Foreground != nil || len(preAttach.Lanes) != 0 || len(preAttach.Outbox) != 0 {
		t.Fatalf("client-ready/session-save scheduled provider work: %#v", preAttach)
	}
	if processes := manager.Processes(); len(processes) != 0 {
		t.Fatalf("client-ready/session-save started ACP before a real attach: %#v", processes)
	}
	if methods := readWireFakeMethods(t, tracePath); len(methods) != 0 {
		t.Fatalf("client-ready/session-save touched ACP before a real attach: methods=%v", methods)
	}

	// Exercise the former race boundary: another save and provider selection
	// arrive back-to-back. Both remain ACP-silent; neither may create a native
	// conversation before the first genuine user input.
	actorSnapshot, err = providerChats.ProjectSession()
	if err != nil {
		t.Fatalf("project migrated plan chat before race: %v", err)
	}
	actorSnapshot[globalPresentationOperationField] = "plan-session-save-2"
	client.invoke(t, 2, "session:save", actorSnapshot)
	client.invoke(t, 3, "app-chat:new-session", map[string]any{
		"tabId": "warm-plan-tab", "chatId": "warm-plan-chat", "providerId": "claude", "cwd": root,
	})
	if reply := client.waitReply(t, 2, 2*time.Second); reply.Error != nil || mapFromAnyMain(reply.Result)["ok"] != true {
		t.Fatalf("racing session save reply = %#v", reply)
	}
	sessionReply := client.waitReply(t, 3, 5*time.Second)
	if sessionReply.Error != nil {
		t.Fatalf("real session attach reply = %#v", sessionReply)
	}
	sessionID := fieldString(sessionReply.Result.(map[string]any), "sessionId")
	if sessionID != "" {
		t.Fatalf("empty selection materialized a provider thread: %q", sessionID)
	}
	selected, ok := providerChats.Snapshot("warm-plan-chat")
	if !ok || selected.Foreground != nil || len(selected.Outbox) != 0 || selected.DesiredLaneID == "" {
		t.Fatalf("empty selection scheduled provider work: %#v", selected)
	}
	for _, lane := range selected.Lanes {
		if !lane.Thread.IsZero() || lane.Attachment != nil {
			t.Fatalf("empty selection attached a native provider lane: %#v", lane)
		}
	}
	if processes := manager.Processes(); len(processes) != 0 {
		t.Fatalf("empty selection started ACP before real input: %#v", processes)
	}
	if methods := readWireFakeMethods(t, tracePath); len(methods) != 0 {
		t.Fatalf("selection touched ACP before a real input: methods=%v", methods)
	}

	client.invoke(t, 4, "job:start", map[string]any{
		"kind": "app-chat", "chatId": "warm-plan-chat", "tabId": "warm-plan-tab", "prompt": "materialize the plan fixture",
		"operationId": "warm-plan-first-turn", "userMessageId": "warm-plan-first-user", "assistantMessageId": "warm-plan-first-assistant",
	})
	startReply := client.waitReply(t, 4, 5*time.Second)
	if startReply.Error != nil {
		t.Fatalf("first real input attach reply = %#v", startReply)
	}
	job := startReply.Result.(map[string]any)
	if sessionID = fieldString(job, "sessionId"); sessionID != "" {
		t.Fatalf("durable Claude admission prematurely exposed native session identity: %q", sessionID)
	}
	startEvent := client.waitJobEvent(t, fieldString(job, "id"), "start", 5*time.Second)
	sessionID = fieldString(mapFromAnyMain(startEvent["job"]), "sessionId")
	if sessionID == "" {
		t.Fatal("Claude start event omitted native session identity")
	}

	plan := client.waitChannelEvent(t, "chat:plan-usage", 5*time.Second).Payload.(map[string]any)
	if plan["providerId"] != "claude" {
		t.Fatalf("plan usage provider = %#v, want claude; payload=%#v", plan["providerId"], plan)
	}
	methods := waitWireFakeMethods(t, tracePath, 2*time.Second, func(methods []string) bool {
		return countWireMethod(methods, "_workass/claude/usage") == 1
	})
	_ = client.waitJobEvent(t, fieldString(job, "id"), "end", 5*time.Second)
	methods = readWireFakeMethods(t, tracePath)
	if countWireMethod(methods, "session/new") != 1 || countWireMethod(methods, "session/resume") != 0 || countWireMethod(methods, "session/load") != 0 {
		t.Fatalf("real attach raced a duplicate session: methods=%v", methods)
	}
	if countWireMethod(methods, "session/prompt") != 1 {
		t.Fatalf("first real input did not own exactly one prompt: methods=%v", methods)
	}
	usageReads := countWireMethod(methods, "_workass/claude/usage")
	if usageReads < 1 {
		t.Fatalf("first real input did not initialize plan usage: methods=%v", methods)
	}

	// The renderer's explicit active-chat refresh reuses the exact live session
	// and performs one metadata read. It must not create another provider session
	// or send model input.
	client.invoke(t, 5, "app-chat:refresh-plan-usage", map[string]any{"providerId": "claude"})
	if reply := client.waitReply(t, 5, 5*time.Second); reply.Error != nil {
		t.Fatalf("explicit plan refresh reply = %#v", reply)
	}
	methods = waitWireFakeMethods(t, tracePath, 2*time.Second, func(methods []string) bool {
		return countWireMethod(methods, "_workass/claude/usage") == usageReads+1
	})
	usageReads++
	if countWireMethod(methods, "session/new") != 1 || countWireMethod(methods, "session/prompt") != 1 {
		t.Fatalf("explicit plan refresh touched conversation methods: %v", methods)
	}

	// Provider-scoped refresh is independent of whichever engine currently owns
	// the visible chat. It reuses the initialized Claude account bridge and never
	// creates a provider conversation or sends model input.
	client.invoke(t, 6, "app-chat:refresh-plan-usage", map[string]any{"providerId": "claude"})
	if reply := client.waitReply(t, 6, 5*time.Second); reply.Error != nil {
		t.Fatalf("provider plan refresh reply = %#v", reply)
	}
	methods = waitWireFakeMethods(t, tracePath, 2*time.Second, func(methods []string) bool {
		return countWireMethod(methods, "_workass/claude/usage") == usageReads+1
	})
	if countWireMethod(methods, "session/new") != 1 || countWireMethod(methods, "session/prompt") != 1 {
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
		update["latest"] != "0.58.10" || update["updateAvailable"] != true || update["hint"] != "qwen update" {
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
	stateDir := t.TempDir()
	manager := acp.NewManager(acp.Options{
		RootDir:             root,
		StateDir:            stateDir,
		ProviderConfigFile:  filepath.Join(stateDir, "providers.json"),
		Broadcast:           hub.Broadcast,
		InitTimeout:         800 * time.Millisecond,
		StdoutFlushInterval: 10 * time.Millisecond,
		RSSSampleInterval:   time.Hour,
		LocalModelEndpoints: []string{models.URL + "/v1/models"},
	})
	sessionState := sharedSessionStore(stateDir)
	providerChats := newProviderChatRuntime(manager, sessionState, stateDir, hub.Broadcast)
	t.Cleanup(func() {
		_ = providerChats.Close(context.Background())
		manager.Reset()
	})
	registerDaemonHandlers(hub, root, manager, daemonOptions{StateDir: stateDir, ProviderChats: providerChats})

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

	createWireActorChat(t, client, 100, "wire-detect-devin-tab", "wire-detect-devin-chat", "devin")
	client.invoke(t, 2, "app-chat:new-session", map[string]any{"tabId": "wire-detect-devin-tab", "chatId": "wire-detect-devin-chat", "providerId": "devin"})
	sessionReply := client.waitReply(t, 2, 5*time.Second)
	if sessionReply.Error != nil {
		t.Fatalf("new-session error: %s", *sessionReply.Error)
	}
	session := sessionReply.Result.(map[string]any)
	if session["providerId"] != "devin" || session["sessionId"] != "" {
		t.Fatalf("Devin selection created a native session before input: %#v", session)
	}
	t.Logf("trace reply app-chat:new-session provider=%s deferred=true", session["providerId"])

	client.invoke(t, 3, "job:start", map[string]any{
		"kind": "app-chat", "tabId": "wire-detect-devin-tab", "chatId": "wire-detect-devin-chat", "providerId": "devin",
		"prompt": "deferred Devin input", "operationId": "wire-detect-devin-turn",
		"userMessageId": "wire-detect-devin-user", "assistantMessageId": "wire-detect-devin-assistant",
	})
	startReply := client.waitReply(t, 3, 5*time.Second)
	if startReply.Error != nil {
		t.Fatalf("deferred Devin job:start error: %s", *startReply.Error)
	}
	job := mapFromAnyMain(startReply.Result)
	jobID := fieldString(job, "id")
	if jobID == "" || fieldString(job, "providerId") != "devin" {
		t.Fatalf("deferred Devin first input was not admitted: %#v", job)
	}
	end := client.waitJobEvent(t, jobID, "end", 5*time.Second)
	if ended := mapFromAnyMain(end["job"]); fieldString(ended, "status") != "done" || fieldString(ended, "providerId") != "devin" {
		t.Fatalf("deferred Devin first turn did not complete: %#v", ended)
	}
	state, ok := providerChats.Snapshot("wire-detect-devin-chat")
	if !ok {
		t.Fatal("deferred Devin actor disappeared after its first turn")
	}
	lane := state.Lanes[state.ActiveLaneID]
	if lane.Identity.Realm.ProviderID != "devin" || lane.Thread.HeadID == "" || lane.Phase != chat.LaneReady {
		t.Fatalf("deferred Devin first input did not establish its exact native thread: %#v", lane)
	}
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
	stateDir := t.TempDir()
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: stateDir,
		Providers: []acp.ProviderConfig{
			{ID: "local-lmstudio", Name: "LM Studio (local)", Command: "workass-agent", Args: []string{}, Enabled: false, Badge: "native", CWD: root},
		},
		ProviderConfigFile:  filepath.Join(stateDir, "providers.json"),
		Broadcast:           hub.Broadcast,
		InitTimeout:         5 * time.Second,
		StdoutFlushInterval: 5 * time.Millisecond,
		RSSSampleInterval:   time.Hour,
		LocalModelEndpoints: []string{fake.URL() + "/v1/models", "http://127.0.0.1:1/v1/models"},
	})
	sessionState := sharedSessionStore(stateDir)
	providerChats := newProviderChatRuntime(manager, sessionState, stateDir, hub.Broadcast)
	t.Cleanup(func() {
		_ = providerChats.Close(context.Background())
		manager.Reset()
	})
	registerDaemonHandlers(hub, root, manager, daemonOptions{StateDir: stateDir, ProviderChats: providerChats})
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

	createWireActorChat(t, client, 100, "wire-local-tab", "chat-wire-local", "local-lmstudio")
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

	client.invoke(t, 2, "job:start", map[string]any{
		"kind": "app-chat", "chatId": "chat-wire-local", "sessionId": session["sessionId"], "tabId": "wire-local-tab", "prompt": "wire native local turn",
		"operationId": "wire-local-turn", "userMessageId": "wire-local-user", "assistantMessageId": "wire-local-assistant",
	})
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
				"WORKASS_MOCK_ACP_TRACE_FILE":    traceFile,
				"WORKASS_MOCK_ACP_DELAY_MS":      "10",
				"WORKASS_MOCK_ACP_SESSION_STORE": filepath.Join(stateDir, "mock-provider.json"),
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
		LifecycleCheckInterval: time.Hour,
	})
	sessionState := sharedSessionStore(stateDir)
	providerChats := newProviderChatRuntime(manager, sessionState, stateDir, hub.Broadcast)
	t.Cleanup(func() {
		_ = providerChats.Close(context.Background())
		manager.Reset()
	})
	registerDaemonHandlers(hub, root, manager, daemonOptions{StateDir: stateDir, ProviderChats: providerChats})

	server := httptest.NewServer(httpserve.New(renderer, hub, nil))
	defer server.Close()
	client := dialTestWS(t, server.URL)
	defer client.conn.Close()

	sourceTabID := "wire-fork-source"
	forkTabID := "wire-fork-child"
	sourceChatID := "chat-wire-fork-source"
	createWireActorChat(t, client, 100, sourceTabID, sourceChatID, "mock")
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

	forkRequest := map[string]any{
		"tabId": sourceTabID, "chatId": sourceChatID, "newTabId": forkTabID,
		"newChatId": "chat-wire-fork-child", "cwd": root, "atTurn": 1, "operationId": "wire-fork-once",
	}
	client.invoke(t, 6, "app-chat:fork", forkRequest)
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
	thirdResult := runWireChatTurn(t, client, 7, sourceChatID, sourceTabID, sourceSessionID, "source after fork")
	appendWireArchiveTurn(t, client, 8, sourceTabID, "source after fork", thirdResult, "2026-07-10T00:00:03Z")
	client.invoke(t, 9, "app-chat:fork", forkRequest)
	forkRetryReply := client.waitReply(t, 9, 5*time.Second)
	if forkRetryReply.Error != nil {
		t.Fatalf("fork retry error: %s", *forkRetryReply.Error)
	}
	forkRetry := forkRetryReply.Result.(map[string]any)
	if forkRetry["sessionId"] != forkSessionID {
		t.Fatalf("fork lost-reply retry changed native attachment: first=%#v retry=%#v", forkSession, forkRetry)
	}
	forkConflict := cloneJSON(forkRequest).(map[string]any)
	forkConflict["cwd"] = t.TempDir()
	client.invoke(t, 10, "app-chat:fork", forkConflict)
	forkConflictReply := client.waitReply(t, 10, 5*time.Second)
	if forkConflictReply.Error == nil {
		t.Fatalf("fork creation operation id was reused for different content: %#v", forkConflictReply.Result)
	}
	t.Logf("trace reply app-chat:fork source=%s fork=%s atTurn=%v session=%s", sourceTabID, forkTabID, forkedFrom["atTurn"], forkSessionID)

	client.invoke(t, 20, "chat:archive-load", sourceTabID)
	sourceArchiveReply := client.waitReply(t, 20, 2*time.Second)
	client.invoke(t, 21, "chat:archive-load", forkTabID)
	forkArchiveReply := client.waitReply(t, 21, 2*time.Second)
	if sourceArchiveReply.Error != nil || forkArchiveReply.Error != nil {
		t.Fatalf("actor fork archive replies source=%+v fork=%+v", sourceArchiveReply, forkArchiveReply)
	}
	sourceArchive := anySlice(sourceArchiveReply.Result)
	forkArchive := anySlice(forkArchiveReply.Result)
	if len(sourceArchive) != 6 || len(forkArchive) != 2 {
		t.Fatalf("archive lengths source=%d fork=%d source=%#v fork=%#v", len(sourceArchive), len(forkArchive), sourceArchive, forkArchive)
	}
	if !archiveContainsText(forkArchive, "source first") || archiveContainsText(forkArchive, "source second") {
		t.Fatalf("fork archive prefix = %#v", forkArchive)
	}
	t.Logf("trace actor fork prefix sourceMessages=%d forkMessages=%d", len(sourceArchive), len(forkArchive))

	client.invoke(t, 11, "job:start", map[string]any{
		"kind": "app-chat", "chatId": sourceChatID, "sessionId": sourceSessionID, "tabId": sourceTabID,
		"operationId": "wire-fork-source-turn-4", "userMessageId": "wire-fork-source-user-4", "assistantMessageId": "wire-fork-source-assistant-4",
		"prompt": "source third unique",
	})
	client.invoke(t, 12, "job:start", map[string]any{
		"kind": "app-chat", "chatId": "chat-wire-fork-child", "sessionId": forkSessionID, "tabId": forkTabID,
		"operationId": "wire-fork-child-turn-2", "userMessageId": "wire-fork-child-user-2", "assistantMessageId": "wire-fork-child-assistant-2",
		"prompt": "fork continuation unique",
	})
	sourceStartReply := client.waitReply(t, 11, 5*time.Second)
	forkStartReply := client.waitReply(t, 12, 5*time.Second)
	if sourceStartReply.Error != nil || forkStartReply.Error != nil {
		t.Fatalf("post-fork job replies source=%s fork=%s", wireReplyError(sourceStartReply), wireReplyError(forkStartReply))
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
	if !strings.Contains(sourceResult, "Mock ACP turn 4: Active Workass runtime for this turn:") ||
		!strings.Contains(sourceResult, "User request:\nsource third unique") ||
		!strings.Contains(forkResult, "User request:\nfork continuation unique") ||
		strings.Contains(forkResult, "source first") || strings.Contains(forkResult, "source second") ||
		strings.Contains(forkResult, "Previous conversation") {
		t.Fatalf("divergent contextless fork results source=%q fork=%q", sourceResult, forkResult)
	}
	t.Logf("trace fork divergence sourceSession=%s forkSession=%s visiblePrefix=true providerReplay=false", sourceSessionID, forkSessionID)

	prompts := readMockTracePrompts(t, traceFile)
	forkPrompt := promptTraceForSession(t, prompts, forkSessionID, "fork continuation unique")
	sourceThirdPrompt := promptTraceForSession(t, prompts, sourceSessionID, "source third unique")
	if strings.Contains(forkPrompt, "source first") || strings.Contains(forkPrompt, "source second") ||
		strings.Contains(forkPrompt, "Previous conversation") || !strings.Contains(forkPrompt, "User request:\nfork continuation unique") {
		t.Fatalf("fork prompt replayed visible archive text = %q", forkPrompt)
	}
	if strings.Contains(sourceThirdPrompt, "Previous conversation") ||
		!strings.Contains(sourceThirdPrompt, "Active Workass runtime for this turn:") ||
		!strings.Contains(sourceThirdPrompt, "User request:\nsource third unique") {
		t.Fatalf("source prompt was touched by fork seed: %q", sourceThirdPrompt)
	}
	t.Logf("trace mock prompts sourceThird=%q forkProviderReplay=false", sourceThirdPrompt)
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
	if state := controller.waitChannelEvent(t, "lan:access-state", 2*time.Second).Payload.(map[string]any); state["controller"] != true {
		t.Fatalf("controller state = %#v", state)
	}
	// The lease belongs to the first approved client that finishes attaching,
	// not the first goroutine that called Dial. Under the race detector the
	// viewer handshake could overtake the controller handshake and invalidate
	// the fixture's own premise. Observe the controller lease before admitting
	// the viewer whose controller-only delivery we are testing.
	viewer := dialTestWSPath(t, server.URL, "/?deviceToken="+viewerToken+"&deviceName=notify-viewer")
	defer viewer.conn.Close()
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
	// The frozen renderer contract creates a running optimistic message before
	// job:start, binds the reply id, then routes the start event by job.chatId.
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

func runWireChatTurn(t *testing.T, client *testWS, invokeID int, chatID, tabID, sessionID, prompt string, cwd ...string) string {
	t.Helper()
	identity := fmt.Sprintf("wire-turn-%s-%d", chatID, invokeID)
	arg := map[string]any{
		"kind": "app-chat", "chatId": chatID, "sessionId": sessionID, "tabId": tabID, "prompt": prompt,
		"operationId": identity, "userMessageId": identity + "-user", "assistantMessageId": identity + "-assistant",
	}
	if len(cwd) != 0 {
		arg["cwd"] = cwd[0]
	}
	client.invoke(t, invokeID, "job:start", arg)
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

func wireReplyError(message wsMessage) string {
	if message.Error == nil {
		return "<nil>"
	}
	return *message.Error
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

func createWireActorChat(t *testing.T, client *testWS, requestID int, tabID, chatID, providerID string) {
	t.Helper()
	client.invoke(t, requestID, "chat:create", map[string]any{
		"tabId": tabID, "chatId": chatID, "operationId": "wire-create:" + chatID,
		"focus": true, "title": "Chat", "titleLocked": false, "group": "chats",
		"providerId": providerID,
	})
	reply := client.waitReply(t, requestID, 2*time.Second)
	if reply.Error != nil || !boolFieldValue(mapFromAnyMain(reply.Result), "ok") {
		t.Fatalf("create durable actor chat %s/%s: %+v", tabID, chatID, reply)
	}
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
	if channel == "app-chat:new-session" && len(args) > 0 {
		if arg, ok := args[0].(map[string]any); ok && strings.TrimSpace(fieldString(arg, "operationId")) == "" {
			// The frozen wire tests model the current renderer, which now supplies
			// one stable operation for every provider-lane selection. Keep older
			// fixtures readable without weakening the daemon's missing-id gate.
			arg["operationId"] = fmt.Sprintf("wire-lane-select:%d:%s:%s", id, fieldString(arg, "chatId"), fieldString(arg, "tabId"))
		}
	}
	payload, err := json.Marshal(map[string]any{"t": "invoke", "id": id, "channel": channel, "args": args})
	if err != nil {
		t.Fatalf("marshal invoke: %v", err)
	}
	if _, err := c.conn.Write(maskedFrame(payload)); err != nil {
		t.Fatalf("write invoke: %v", err)
	}
}

func (c *testWS) invokeRaw(t *testing.T, id int, channel string, args ...any) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"t": "invoke", "id": id, "channel": channel, "args": args})
	if err != nil {
		t.Fatalf("marshal raw invoke: %v", err)
	}
	if _, err := c.conn.Write(maskedFrame(payload)); err != nil {
		t.Fatalf("write raw invoke: %v", err)
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

func waitWireChatCheckpointCount(t *testing.T, manager *acp.Manager, chatID string, count int, timeout time.Duration) []acp.ChatCheckpoint {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		checkpoints := manager.ChatCheckpoints(chatID, "")
		if len(checkpoints) >= count {
			return checkpoints
		}
		if time.Now().After(deadline) {
			t.Fatalf("chat %s checkpoints = %#v, want at least %d", chatID, checkpoints, count)
		}
		time.Sleep(10 * time.Millisecond)
	}
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
	failCode := func(id any, code int, message string) {
		write(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}})
	}
	notify := func(sessionID string, update any) {
		write(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": sessionID, "update": update}})
	}
	mode := os.Getenv("WORKASS_WIRE_FAKE_ACP_MODE")
	sessionSeq := 0
	sessions := make(map[string]bool)
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
				"promptCapabilities":  map[string]any{"image": false},
				"sessionCapabilities": map[string]any{"resume": map[string]any{}, "close": map[string]any{}},
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
			if mode == "plan-usage" || mode == "plan-limits" || mode == "codex-reset" {
				meta := mapFromAnyMain(capabilities["_meta"])
				meta["workassStableTurnInputV1"] = true
				capabilities["_meta"] = meta
			}
			respond(msg.ID, map[string]any{
				"protocolVersion":   1,
				"agentInfo":         map[string]any{"name": "Wire Fake ACP", "version": "0.0.0"},
				"agentCapabilities": capabilities,
				"authMethods":       []any{},
			})
		case "session/new":
			sessionSeq++
			sessionID := fmt.Sprintf("wire-fake-%d-%d", os.Getpid(), sessionSeq)
			sessions[sessionID] = true
			respond(msg.ID, map[string]any{"sessionId": sessionID, "configOptions": wireFakeConfigOptions()})
		case "session/resume":
			sessionID := strings.TrimSpace(fmt.Sprint(params["sessionId"]))
			if !sessions[sessionID] {
				failCode(msg.ID, -32000, "wire fake session not found")
				continue
			}
			respond(msg.ID, map[string]any{"sessionId": sessionID, "configOptions": wireFakeConfigOptions()})
		case "session/set_config_option":
			respond(msg.ID, map[string]any{"configOptions": wireFakeConfigOptions()})
		case "session/close":
			respond(msg.ID, map[string]any{})
		case "session/prompt":
			sessionID := fmt.Sprint(params["sessionId"])
			operationID := strings.TrimSpace(fmt.Sprint(params["clientUserMessageId"]))
			if operationID != "" && (mode == "plan-usage" || mode == "plan-limits" || mode == "codex-reset") {
				notify(sessionID, map[string]any{
					"sessionUpdate": "_workass_input_consumed", "clientUserMessageId": operationID,
					"turnId": "wire-native-turn-" + operationID,
				})
			}
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

// A provider lane that has never consumed input may join a nonempty Workass
// chat by receiving one bounded semantic seed with its first real prompt. Once
// a lane has consumed input, later cross-provider gaps still require the
// receipt-bearing non-sampling import contract.
func TestWireFreshProviderGetsHistorySeedAndEstablishedLaneUsesSafeImport(t *testing.T) {
	root := repoRoot(t)
	stateDir := t.TempDir()
	renderer := t.TempDir()
	if err := os.WriteFile(filepath.Join(renderer, "index.html"), []byte("<!doctype html><head></head><body></body>"), 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}

	hub := wire.NewHub()
	store := sharedSessionStore(stateDir)
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: stateDir,
		Providers: []acp.ProviderConfig{
			{ID: "mock", Name: "Mock Provider", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")}, CWD: root, Env: map[string]string{
				"WORKASS_MOCK_ACP_DELAY_MS": "5", "WORKASS_MOCK_ACP_SESSION_STORE": filepath.Join(stateDir, "mock-provider.json"),
				"WORKASS_MOCK_ACP_CONTEXT_IMPORT": "1",
			}, Enabled: true},
			{ID: "fake-agent", Name: "Fake Provider", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")}, CWD: root, Env: map[string]string{
				"WORKASS_MOCK_ACP_DELAY_MS": "5", "WORKASS_MOCK_ACP_SESSION_STORE": filepath.Join(stateDir, "fake-provider.json"),
			}, Enabled: true},
		},
		DefaultProviderID:    "mock",
		Broadcast:            daemonEventBroadcaster(store, hub.Broadcast),
		InitTimeout:          2 * time.Second,
		StdoutFlushInterval:  10 * time.Millisecond,
		ThoughtFlushInterval: 10 * time.Millisecond,
		RSSSampleInterval:    time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	providerChats := newProviderChatRuntime(manager, store, stateDir)
	registerDaemonHandlers(hub, root, manager, daemonOptions{StateDir: stateDir, ProviderChats: providerChats})

	server := httptest.NewServer(httpserve.New(renderer, hub, nil))
	defer server.Close()
	client := dialTestWS(t, server.URL)
	defer client.conn.Close()

	if _, err := providerChats.CreateRendererChat(map[string]any{
		"operationId": "test:create-provider-switch-chat",
		"tabId":       "switch-tab", "chatId": "chat-switch", "title": "Provider switch",
		"cwd": root, "providerId": "mock", "currentModelId": "mock-deterministic",
	}); err != nil {
		t.Fatalf("create actor-native provider switch chat: %v", err)
	}
	client.invoke(t, 1, "app-chat:new-session", map[string]any{"tabId": "switch-tab", "chatId": "chat-switch", "providerId": "mock"})
	mockSession := client.waitReply(t, 1, 5*time.Second).Result.(map[string]any)
	if mockSession["providerId"] != "mock" || mockSession["sessionId"] == "" {
		t.Fatalf("mock session = %#v", mockSession)
	}
	sessionID := fmt.Sprint(mockSession["sessionId"])

	// Turn 1 on the mock engine.
	client.invoke(t, 2, "job:start", map[string]any{
		"operationId": "wire-provider-switch-mock", "kind": "app-chat", "chatId": "chat-switch", "tabId": "switch-tab", "sessionId": sessionID,
		"providerId": "mock", "prompt": "primer turno con el mock",
		"userMessageId": "switch-user-1", "assistantMessageId": "switch-assistant-1",
	})
	turn1 := client.waitReply(t, 2, 5*time.Second).Result.(map[string]any)
	turn1End := client.waitJobEvent(t, fmt.Sprint(turn1["id"]), "end", 10*time.Second)
	if turn1End["job"].(map[string]any)["status"] != "done" {
		t.Fatalf("turn1 end = %#v", turn1End)
	}
	t.Logf("trace switch turn1 provider=%v status=%v", turn1End["job"].(map[string]any)["providerId"], turn1End["job"].(map[string]any)["status"])

	deadline := time.Now().Add(3 * time.Second)
	for {
		state, ok := providerChats.Snapshot("chat-switch")
		if ok && state.Foreground == nil && state.LedgerHead() >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("provider actor did not commit the first terminal turn")
		}
		time.Sleep(10 * time.Millisecond)
	}

	client.invoke(t, 3, "app-chat:new-session", map[string]any{
		"tabId": "switch-tab", "chatId": "chat-switch", "providerId": "fake-agent", "cwd": root,
	})
	fakeSession := mapFromAnyMain(client.waitReply(t, 3, 5*time.Second).Result)
	if fieldString(fakeSession, "providerId") != "fake-agent" || fieldString(fakeSession, "sessionId") == "" {
		t.Fatalf("fresh target provider lane did not attach: %#v", fakeSession)
	}
	client.invoke(t, 4, "job:start", map[string]any{
		"operationId": "wire-provider-switch-fake", "kind": "app-chat", "chatId": "chat-switch", "tabId": "switch-tab", "sessionId": fieldString(fakeSession, "sessionId"),
		"providerId": "fake-agent", "prompt": "continuar con el agente nuevo",
		"userMessageId": "switch-user-fresh", "assistantMessageId": "switch-assistant-fresh",
	})
	freshTurnReply := client.waitReply(t, 4, 5*time.Second)
	if freshTurnReply.Error != nil {
		t.Fatalf("fresh provider seeded turn failed: %s", *freshTurnReply.Error)
	}
	freshTurn := mapFromAnyMain(freshTurnReply.Result)
	freshEnd := client.waitJobEvent(t, fieldString(freshTurn, "id"), "end", 10*time.Second)
	freshResult := fieldString(mapFromAnyMain(freshEnd["job"]), "result")
	if !strings.Contains(freshResult, "Previous Workass conversation for this newly created provider thread") ||
		!strings.Contains(freshResult, "primer turno con el mock") ||
		!strings.Contains(freshResult, "User request:\ncontinuar con el agente nuevo") {
		t.Fatalf("fresh provider did not receive the one-time semantic seed: %q", freshResult)
	}

	// Returning to the original provider selects and uses the exact same native
	// thread. Its later gap is filled through the negotiated non-sampling import,
	// never by another prompt seed or a replacement thread.
	client.invoke(t, 5, "app-chat:new-session", map[string]any{
		"tabId": "switch-tab", "chatId": "chat-switch", "providerId": "mock", "cwd": root,
	})
	resumed := mapFromAnyMain(client.waitReply(t, 5, 5*time.Second).Result)
	if fieldString(resumed, "sessionId") != sessionID || fieldString(resumed, "providerId") != "mock" {
		t.Fatalf("original lane was not resumed exactly: %#v", resumed)
	}
	client.invoke(t, 6, "job:start", map[string]any{
		"operationId": "wire-provider-switch-mock-second", "kind": "app-chat", "chatId": "chat-switch", "tabId": "switch-tab", "sessionId": sessionID,
		"providerId": "mock", "prompt": "segundo turno exacto",
		"userMessageId": "switch-user-2", "assistantMessageId": "switch-assistant-2",
	})
	turn2Reply := client.waitReply(t, 6, 5*time.Second)
	if turn2Reply.Error != nil {
		t.Fatalf("second exact-lane turn failed: %s", *turn2Reply.Error)
	}
	turn2 := mapFromAnyMain(turn2Reply.Result)
	turn2End := client.waitJobEvent(t, fieldString(turn2, "id"), "end", 10*time.Second)
	result := fieldString(mapFromAnyMain(turn2End["job"]), "result")
	if !strings.Contains(result, "Mock ACP turn 2:") || strings.Contains(result, "Previous conversation") {
		t.Fatalf("exact original lane was replaced or replay-seeded: %q", result)
	}
}
