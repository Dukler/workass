package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNativeSessionLedgerPersistsAndRejectsStaleGeneration(t *testing.T) {
	stateDir := t.TempDir()
	ledger := newNativeSessionLedger(stateDir)
	binding := nativeSessionBinding{
		TabID: "tab-ledger", ChatID: "chat-ledger", ProviderID: "codex", SessionID: "native-thread-1",
		HistoryHash: historyDigest(nil), ResumeSafe: true,
	}
	if err := ledger.put(binding); err != nil {
		t.Fatalf("put binding: %v", err)
	}
	saved, ok := ledger.get("tab-ledger", "chat-ledger", "codex")
	if !ok || saved.Generation != 1 || saved.SessionID != "native-thread-1" {
		t.Fatalf("saved binding = %#v", saved)
	}
	history := []historyMessage{{Role: "user", Content: "one", At: "2026-07-12T00:00:00Z"}}
	if ledger.updateCursor("tab-ledger", "chat-ledger", "codex", saved.SessionID, saved.Generation+1, history, "gpt", "agent", true) {
		t.Fatal("stale/future generation unexpectedly updated the native cursor")
	}
	if !ledger.updateCursor("tab-ledger", "chat-ledger", "codex", saved.SessionID, saved.Generation, history, "gpt", "agent", true) {
		t.Fatal("current generation did not update the native cursor")
	}
	if !ledger.markInFlight("tab-ledger", "chat-ledger", "codex", saved.SessionID, saved.Generation, "gpt", "agent") {
		t.Fatal("could not persist native in-flight guard")
	}
	crashReload := newNativeSessionLedger(stateDir)
	dirty, ok := crashReload.get("tab-ledger", "chat-ledger", "codex")
	if !ok || dirty.ResumeSafe {
		t.Fatalf("crash-reloaded binding must be resume-unsafe: %#v", dirty)
	}
	if !ledger.updateCursor("tab-ledger", "chat-ledger", "codex", saved.SessionID, saved.Generation, history, "gpt", "agent", true) {
		t.Fatal("terminal cursor commit did not clear the in-flight guard")
	}

	reloaded := newNativeSessionLedger(stateDir)
	got, ok := reloaded.get("tab-ledger", "chat-ledger", "codex")
	if !ok || got.SyncedMessages != 1 || got.HistoryHash != historyDigest(history) || got.ModelID != "gpt" || got.ModeID != "agent" {
		t.Fatalf("reloaded binding = %#v", got)
	}
	info, err := os.Stat(filepath.Join(stateDir, nativeSessionLedgerFilename))
	if err != nil {
		t.Fatalf("stat native ledger: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("native ledger mode = %o, want 600", info.Mode().Perm())
	}
	reloaded.delete("tab-ledger", "chat-ledger", "codex", got.SessionID)
	if _, ok := reloaded.get("tab-ledger", "chat-ledger", "codex"); ok {
		t.Fatal("explicit delete left the native binding behind")
	}
}

func TestCorruptNativeSessionLedgerDisablesResumeWithoutOverwriting(t *testing.T) {
	stateDir := t.TempDir()
	path := filepath.Join(stateDir, nativeSessionLedgerFilename)
	const corrupt = "{not-json"
	if err := os.WriteFile(path, []byte(corrupt), 0o600); err != nil {
		t.Fatal(err)
	}
	ledger := newNativeSessionLedger(stateDir)
	if ledger.loadErr == nil || ledger.enabledFor(SessionOptions{TabID: "tab", ProviderID: "codex"}) {
		t.Fatalf("corrupt ledger remained enabled: err=%v", ledger.loadErr)
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != corrupt {
		t.Fatalf("corrupt ledger was overwritten: raw=%q err=%v", raw, err)
	}
}

func TestNativeSessionLedgerDeleteChatRemovesEveryProviderBinding(t *testing.T) {
	ledger := newNativeSessionLedger(t.TempDir())
	for _, binding := range []nativeSessionBinding{
		{TabID: "tab-delete", ChatID: "chat-delete", ProviderID: "codex", SessionID: "codex-thread", HistoryHash: historyDigest(nil), ResumeSafe: true},
		{TabID: "tab-delete", ChatID: "chat-delete", ProviderID: "claude", SessionID: "claude-thread", HistoryHash: historyDigest(nil), ResumeSafe: true},
		{TabID: "tab-keep", ChatID: "chat-keep", ProviderID: "codex", SessionID: "keep-thread", HistoryHash: historyDigest(nil), ResumeSafe: true},
	} {
		if err := ledger.put(binding); err != nil {
			t.Fatal(err)
		}
	}
	ledger.deleteChat("tab-delete", "chat-delete")
	if _, ok := ledger.get("tab-delete", "chat-delete", "codex"); ok {
		t.Fatal("Codex binding survived exact chat deletion")
	}
	if _, ok := ledger.get("tab-delete", "chat-delete", "claude"); ok {
		t.Fatal("Claude binding survived exact chat deletion")
	}
	if _, ok := ledger.get("tab-keep", "chat-keep", "codex"); !ok {
		t.Fatal("another chat's native binding was deleted")
	}
}

func TestNativeSessionBindingRequiresExactConversationOwner(t *testing.T) {
	ledger := newNativeSessionLedger(t.TempDir())
	first := nativeSessionBinding{
		TabID: "tab-a", ChatID: "chat-a", ProviderID: "codex", SessionID: "native-shared",
		HistoryHash: historyDigest(nil), ResumeSafe: true,
	}
	if err := ledger.put(first); err != nil {
		t.Fatal(err)
	}
	if _, ok := ledger.get("tab-a", "chat-b", "codex"); ok {
		t.Fatal("a tab binding was returned for a different conversation id")
	}
	if err := ledger.put(nativeSessionBinding{
		TabID: "tab-b", ChatID: "chat-b", ProviderID: "codex", SessionID: "native-shared",
		HistoryHash: historyDigest(nil), ResumeSafe: true,
	}); err == nil {
		t.Fatal("one provider-native session was accepted for two chat owners")
	}
}

func TestNativeSessionLedgerPrunesNonAuthoritativeTabs(t *testing.T) {
	stateDir := t.TempDir()
	ledger := newNativeSessionLedger(stateDir)
	for _, binding := range []nativeSessionBinding{
		{TabID: "real-tab", ChatID: "real-chat", ProviderID: "codex", SessionID: "native-real", HistoryHash: historyDigest(nil), ResumeSafe: true},
		{TabID: "test-tab", ChatID: "test-chat", ProviderID: "mock", SessionID: "native-test", HistoryHash: historyDigest(nil), ResumeSafe: true},
		{TabID: "changed-tab", ChatID: "old-chat", ProviderID: "claude", SessionID: "native-old", HistoryHash: historyDigest(nil), ResumeSafe: true},
	} {
		if err := ledger.put(binding); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := map[string]any{"v": 1, "chats": []any{
		map[string]any{"id": "real-tab", "chatId": "real-chat"},
		map[string]any{"id": "changed-tab", "chatId": "new-chat"},
	}}
	raw, _ := json.Marshal(snapshot)
	if err := os.WriteFile(filepath.Join(stateDir, "session-state.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	reloaded := newNativeSessionLedger(stateDir)
	if _, ok := reloaded.get("real-tab", "real-chat", "codex"); !ok {
		t.Fatal("authoritative binding was pruned")
	}
	if len(reloaded.bindings) != 1 {
		t.Fatalf("bindings after authoritative prune = %d, want 1", len(reloaded.bindings))
	}
}

func TestManagerWithoutExplicitStateDirCannotWriteRepositoryLedger(t *testing.T) {
	opts := (Options{RootDir: t.TempDir()}).withDefaults()
	if opts.StateDir != "" {
		t.Fatalf("implicit state dir = %q, want ephemeral empty state", opts.StateDir)
	}
	manager := NewManager(opts)
	t.Cleanup(func() { manager.Reset() })
	if manager.nativeSessions.path != "" {
		t.Fatalf("implicit native ledger path = %q", manager.nativeSessions.path)
	}
}

func TestMockNativeSessionResumeAndLoadAcrossManagerRestart(t *testing.T) {
	for _, capability := range []string{"resume", "load"} {
		t.Run(capability, func(t *testing.T) {
			fixture := newPersistentMockFixture(t, capability)
			firstManager, firstEvents := fixture.newManager()
			firstSession := fixture.newSession(t, firstManager)
			if _, err := firstManager.SetModel(context.Background(), firstSession.SessionID, "mock-deterministic[high]"); err != nil {
				t.Fatalf("set native model: %v", err)
			}
			if _, err := firstManager.SetMode(context.Background(), firstSession.SessionID, "bypass"); err != nil {
				t.Fatalf("set native mode: %v", err)
			}
			firstEnd := fixture.runTurn(t, firstManager, firstEvents, firstSession.SessionID, "first native turn", nil)
			firstHistory := archiveHistoryForEndedJob(t, fixture.stateDir, firstEnd, "first native turn")
			firstManager.Reset()

			secondManager, secondEvents := fixture.newManager()
			t.Cleanup(func() { secondManager.Reset() })
			resumed := fixture.newSession(t, secondManager)
			if resumed.SessionID != firstSession.SessionID {
				t.Fatalf("native session id changed across restart: first=%s resumed=%s", firstSession.SessionID, resumed.SessionID)
			}
			if stringPointer(resumed.CurrentModelID) != "mock-deterministic[high]" || stringPointer(resumed.CurrentModeID) != "bypass" {
				t.Fatalf("resumed controls = model %q mode %q", stringPointer(resumed.CurrentModelID), stringPointer(resumed.CurrentModeID))
			}
			secondEnd := fixture.runTurn(t, secondManager, secondEvents, resumed.SessionID, "second native turn", firstHistory)
			result := asString(jobFromEnd(secondEnd)["result"])
			if !strings.Contains(result, "Mock ACP turn 2:") {
				t.Fatalf("provider turn counter did not resume: %q", result)
			}
			trace := readNativeMockTrace(t, fixture.traceFile)
			if !traceContains(trace, "[mock:lifecycle] session/"+capability) {
				t.Fatalf("trace did not use session/%s: %#v", capability, trace)
			}
			if traceContains(trace, "WORKASS HISTORY DELTA SYNC") || strings.Contains(result, "Previous conversation") {
				t.Fatalf("exact resume replayed or delta-seeded already-synced history: result=%q trace=%#v", result, trace)
			}
		})
	}
}

func TestMockNativeSessionNeverResumesAfterConversationIdentityChanges(t *testing.T) {
	fixture := newPersistentMockFixture(t, "resume")
	firstManager, firstEvents := fixture.newManager()
	firstSession := fixture.newSession(t, firstManager)
	fixture.runTurn(t, firstManager, firstEvents, firstSession.SessionID, "original owner turn", nil)
	firstManager.Reset()

	secondManager, _ := fixture.newManager()
	t.Cleanup(func() { secondManager.Reset() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	replacement, err := secondManager.NewSession(ctx, SessionOptions{
		TabID: "native-tab", ChatID: "replacement-chat", ProviderID: "mock",
	})
	if err != nil {
		t.Fatalf("fresh replacement session: %v", err)
	}
	if replacement.SessionID == firstSession.SessionID {
		t.Fatalf("conversation identity change resumed old native session %s", replacement.SessionID)
	}
	binding, ok := secondManager.nativeSessions.get("native-tab", "replacement-chat", "mock")
	if !ok || binding.SessionID != replacement.SessionID {
		t.Fatalf("replacement owner was not committed: %#v ok=%v", binding, ok)
	}
}

func TestMockNativeSessionDeltaSyncsOnlyUnseenWorkassHistory(t *testing.T) {
	fixture := newPersistentMockFixture(t, "resume")
	firstManager, firstEvents := fixture.newManager()
	firstSession := fixture.newSession(t, firstManager)
	firstEnd := fixture.runTurn(t, firstManager, firstEvents, firstSession.SessionID, "first native turn", nil)
	history := archiveHistoryForEndedJob(t, fixture.stateDir, firstEnd, "first native turn")
	extra := []historyMessage{
		{Role: "user", Content: "turn handled by another provider", At: "2026-07-12T01:00:00Z"},
		{Role: "assistant", Content: "other provider answer", At: "2026-07-12T01:00:01Z"},
	}
	appendNativeTestArchive(t, fixture.stateDir, extra)
	history = append(history, extra...)
	firstManager.Reset()

	secondManager, secondEvents := fixture.newManager()
	t.Cleanup(func() { secondManager.Reset() })
	resumed := fixture.newSession(t, secondManager)
	if resumed.SessionID != firstSession.SessionID {
		t.Fatalf("delta sync replaced the native session: first=%s resumed=%s", firstSession.SessionID, resumed.SessionID)
	}
	secondEnd := fixture.runTurn(t, secondManager, secondEvents, resumed.SessionID, "after provider switch", history)
	if result := asString(jobFromEnd(secondEnd)["result"]); !strings.Contains(result, "Mock ACP turn 3:") {
		t.Fatalf("want one internal delta turn plus the visible turn, result=%q", result)
	}
	trace := readNativeMockTrace(t, fixture.traceFile)
	if !traceContains(trace, "WORKASS HISTORY DELTA SYNC") || !traceContains(trace, "turn handled by another provider") || !traceContains(trace, "other provider answer") {
		t.Fatalf("missing canonical delta seed: %#v", trace)
	}
	for _, line := range trace {
		if strings.Contains(line, "WORKASS HISTORY DELTA SYNC") && strings.Contains(line, "first native turn") {
			t.Fatalf("delta seed resent history already seen by the native session: %q", line)
		}
	}
}

func TestMockNativeSessionDivergenceFallsBackToFreshReplay(t *testing.T) {
	fixture := newPersistentMockFixture(t, "resume")
	firstManager, firstEvents := fixture.newManager()
	firstSession := fixture.newSession(t, firstManager)
	firstEnd := fixture.runTurn(t, firstManager, firstEvents, firstSession.SessionID, "original turn", nil)
	history := archiveHistoryForEndedJob(t, fixture.stateDir, firstEnd, "original turn")
	firstManager.Reset()

	// Simulate rewind/edit of canonical Workass history. The saved provider cursor
	// no longer names a prefix and must never be guessed-merged.
	history[0].Content = "rewritten canonical turn"
	writeNativeTestArchive(t, fixture.stateDir, history)
	secondManager, secondEvents := fixture.newManager()
	t.Cleanup(func() { secondManager.Reset() })
	fresh := fixture.newSession(t, secondManager)
	if fresh.SessionID == firstSession.SessionID {
		t.Fatalf("divergent provider session was resumed instead of replaced: %s", fresh.SessionID)
	}
	end := fixture.runTurn(t, secondManager, secondEvents, fresh.SessionID, "continue after rewrite", history)
	result := asString(jobFromEnd(end)["result"])
	if !strings.Contains(result, "Previous conversation") || !strings.Contains(result, "rewritten canonical turn") {
		t.Fatalf("fresh fallback did not replay canonical Workass history: %q", result)
	}
}

func TestMockNativeSessionInFlightGuardFallsBackAfterDaemonCrash(t *testing.T) {
	fixture := newPersistentMockFixture(t, "resume")
	firstManager, firstEvents := fixture.newManager()
	firstSession := fixture.newSession(t, firstManager)
	firstEnd := fixture.runTurn(t, firstManager, firstEvents, firstSession.SessionID, "durable turn", nil)
	history := archiveHistoryForEndedJob(t, fixture.stateDir, firstEnd, "durable turn")
	binding, ok := firstManager.nativeSessions.get("native-tab", "native-chat", "mock")
	if !ok || !firstManager.nativeSessions.markInFlight("native-tab", "native-chat", "mock", firstSession.SessionID, binding.Generation, "", "") {
		t.Fatal("could not simulate daemon death after provider prompt dispatch")
	}
	firstManager.Reset()

	secondManager, secondEvents := fixture.newManager()
	t.Cleanup(func() { secondManager.Reset() })
	fresh := fixture.newSession(t, secondManager)
	if fresh.SessionID == firstSession.SessionID {
		t.Fatalf("resume-unsafe in-flight session was resumed: %s", fresh.SessionID)
	}
	end := fixture.runTurn(t, secondManager, secondEvents, fresh.SessionID, "recover after daemon crash", history)
	if result := asString(jobFromEnd(end)["result"]); !strings.Contains(result, "Previous conversation") || !strings.Contains(result, "durable turn") {
		t.Fatalf("crash fallback did not replay durable Workass history: %q", result)
	}
}

// Regression: a hibernated engine keeps its session→bridge mapping so the
// chat stays resolvable, but the replacement bridge that session/resumes the
// same provider-native session must take ownership instead of failing with
// "ACP session id collision" (which bricked the chat until a daemon restart).
func TestMockNativeSessionResumeAfterHibernationDoesNotCollide(t *testing.T) {
	fixture := newPersistentMockFixture(t, "resume")
	manager, events := fixture.newManagerTuned(func(opts *Options) {
		opts.HibernateTTL = 80 * time.Millisecond
		opts.LifecycleCheckInterval = 10 * time.Millisecond
	})
	t.Cleanup(func() { manager.Reset() })

	first := fixture.newSession(t, manager)
	end := fixture.runTurn(t, manager, events, first.SessionID, "turn before hibernation", nil)
	archiveHistoryForEndedJob(t, fixture.stateDir, end, "turn before hibernation")
	_ = waitProcState(t, manager, StateHibernated, 2*time.Second)

	resumed := fixture.newSession(t, manager)
	if resumed.SessionID != first.SessionID {
		t.Fatalf("hibernated engine must resume the same native session: first=%s resumed=%s", first.SessionID, resumed.SessionID)
	}
	live, ok := manager.LiveSession(resumed.SessionID)
	if !ok || live.TabID != "native-tab" {
		t.Fatalf("resumed session is not live on its chat: %+v ok=%v", live, ok)
	}
}

// Regression: a binding that claims a session id already live on another chat
// must degrade to the canonical replay fallback (fresh session) instead of
// hard-failing the send, and must not disturb the real owner.
func TestMockNativeSessionForeignLiveCollisionFallsBackToFresh(t *testing.T) {
	fixture := newPersistentMockFixture(t, "resume")
	manager, _ := fixture.newManager()
	t.Cleanup(func() { manager.Reset() })

	owner := fixture.newSession(t, manager)
	// The ledger's ownership guard rejects a binding for a session another
	// binding owns, so drop the owner's binding (its session stays live on its
	// bridge) to model a stale/corrupt ledger claiming a live session.
	manager.nativeSessions.deleteChat("native-tab", "native-chat")
	if err := manager.nativeSessions.put(nativeSessionBinding{
		TabID: "other-tab", ChatID: "other-chat", ProviderID: "mock",
		SessionID: owner.SessionID, CWD: fixture.root,
		HistoryHash: historyDigest(nil), HistoryVersion: nativeHistoryDigestVersion,
		ResumeSafe: true,
	}); err != nil {
		t.Fatalf("seed foreign binding: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	fresh, err := manager.NewSession(ctx, SessionOptions{TabID: "other-tab", ChatID: "other-chat", ProviderID: "mock"})
	if err != nil {
		t.Fatalf("live collision must degrade to a fresh session: %v", err)
	}
	if fresh.SessionID == owner.SessionID {
		t.Fatalf("foreign chat stole a live session: %s", fresh.SessionID)
	}
	if live, ok := manager.LiveSession(owner.SessionID); !ok || live.TabID != "native-tab" {
		t.Fatalf("original owner lost its live session: %+v ok=%v", live, ok)
	}
	if binding, ok := manager.nativeSessions.get("other-tab", "other-chat", "mock"); !ok || binding.SessionID != fresh.SessionID {
		t.Fatalf("foreign binding was not healed to the fresh session: %#v ok=%v", binding, ok)
	}
}

type persistentMockFixture struct {
	t           *testing.T
	root        string
	stateDir    string
	sessionFile string
	traceFile   string
	capability  string
}

func newPersistentMockFixture(t *testing.T, capability string) *persistentMockFixture {
	root := repoRoot(t)
	temp := t.TempDir()
	return &persistentMockFixture{
		t: t, root: root, stateDir: filepath.Join(temp, "state"),
		sessionFile: filepath.Join(temp, "provider-sessions.json"),
		traceFile:   filepath.Join(temp, "mock-trace.jsonl"), capability: capability,
	}
}

func (f *persistentMockFixture) newManager() (*Manager, *eventCollector) {
	return f.newManagerTuned(nil)
}

func (f *persistentMockFixture) newManagerTuned(tune func(*Options)) (*Manager, *eventCollector) {
	events := newEventCollector()
	opts := Options{
		RootDir: f.root, StateDir: f.stateDir,
		Provider: ProviderConfig{
			ID: "mock", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")},
			CWD: f.root, Enabled: true, Label: "Workass Mock ACP",
			Env: map[string]string{
				"WORKASS_MOCK_ACP_DELAY_MS": "0", "WORKASS_MOCK_ACP_SESSION_STORE": f.sessionFile,
				"WORKASS_MOCK_ACP_SESSION_CAPABILITY": f.capability, "WORKASS_MOCK_ACP_TRACE_FILE": f.traceFile,
			},
		},
		Broadcast: events.Broadcast, StdoutFlushInterval: 5 * time.Millisecond,
		ThoughtFlushInterval: 5 * time.Millisecond, RSSSampleInterval: time.Hour,
	}
	if tune != nil {
		tune(&opts)
	}
	return NewManager(opts), events
}

func (f *persistentMockFixture) newSession(t *testing.T, manager *Manager) SessionInfo {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session, err := manager.NewSession(ctx, SessionOptions{TabID: "native-tab", ChatID: "native-chat", ProviderID: "mock"})
	if err != nil {
		t.Fatalf("new/resume native mock session: %v", err)
	}
	return session
}

func (f *persistentMockFixture) runTurn(t *testing.T, manager *Manager, events *eventCollector, sessionID, prompt string, history []historyMessage) map[string]any {
	rawHistory := make([]any, 0, len(history))
	for _, message := range history {
		rawHistory = append(rawHistory, map[string]any{"role": message.Role, "content": message.Content, "at": message.At})
	}
	job, err := manager.StartJob(context.Background(), JobStartOptions{
		Kind: "app-chat", SessionID: sessionID, TabID: "native-tab", ChatID: "native-chat",
		ProviderID: "mock", Prompt: prompt, History: rawHistory,
	})
	if err != nil {
		t.Fatalf("start native mock turn: %v", err)
	}
	return events.waitJobEnd(t, jobID(job), 5*time.Second)
}

func archiveHistoryForEndedJob(t *testing.T, stateDir string, end map[string]any, prompt string) []historyMessage {
	job := jobFromEnd(end)
	history := []historyMessage{
		{Role: "user", Content: prompt, At: asString(job["startedAt"])},
		{Role: "assistant", Content: asString(job["result"]), At: asString(job["finishedAt"])},
	}
	writeNativeTestArchive(t, stateDir, history)
	return history
}

func appendNativeTestArchive(t *testing.T, stateDir string, history []historyMessage) {
	path := filepath.Join(stateDir, "chat-archive", "native-tab.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir native archive: %v", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open native archive: %v", err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, message := range history {
		if err := enc.Encode(map[string]any{"role": message.Role, "content": message.Content, "status": "done", "at": message.At}); err != nil {
			t.Fatalf("encode native archive: %v", err)
		}
	}
}

func writeNativeTestArchive(t *testing.T, stateDir string, history []historyMessage) {
	path := filepath.Join(stateDir, "chat-archive", "native-tab.jsonl")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		t.Fatalf("reset native archive: %v", err)
	}
	appendNativeTestArchive(t, stateDir, history)
}

func readNativeMockTrace(t *testing.T, path string) []string {
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open mock trace: %v", err)
	}
	defer f.Close()
	var out []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var row map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &row); err == nil {
			out = append(out, asString(row["text"]))
		}
	}
	return out
}

func traceContains(trace []string, needle string) bool {
	for _, line := range trace {
		if strings.Contains(line, needle) {
			return true
		}
	}
	return false
}
