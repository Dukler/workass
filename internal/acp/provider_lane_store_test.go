package acp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	providercontract "workass/internal/provider"
)

type historyMessage struct {
	ID      string
	Role    string
	Content string
	At      string
}

func TestNativeSessionLedgerPersistsExactSessionOnly(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	ledger := newNativeSessionLedger(stateDir)
	binding := nativeSessionBinding{
		TabID: "tab-ledger", ChatID: "chat-ledger", ProviderID: "codex", SessionID: "native-thread-1",
		CWD: stateDir, ThreadCommitted: true,
	}
	if err := ledger.put(binding); err != nil {
		t.Fatalf("put binding: %v", err)
	}
	saved, ok := ledger.get("tab-ledger", "chat-ledger", "codex")
	if !ok || saved.Generation != 1 || saved.SessionID != "native-thread-1" {
		t.Fatalf("saved binding = %#v", saved)
	}
	reloaded := newNativeSessionLedger(stateDir)
	got, ok := reloaded.get("tab-ledger", "chat-ledger", "codex")
	if !ok || got.SessionID != saved.SessionID || !got.ThreadCommitted {
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

func TestNativeSessionLedgerDropsLegacyTurnGateOnLoad(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	path := filepath.Join(stateDir, nativeSessionLedgerFilename)
	disk := nativeSessionLedgerFile{Version: currentNativeLaneStoreVersion, Bindings: []nativeSessionBinding{{
		TabID: "legacy-tab", ChatID: "legacy-chat", ProviderID: "codex", SessionID: "saved-session",
		CWD: stateDir, ThreadCommitted: true, Generation: 1,
		LegacyPendingOperation: json.RawMessage(`{"operationId":"stale","state":"consumed"}`),
		LegacyLastOperation:    json.RawMessage(`{"operationId":"older","state":"terminal"}`),
	}}}
	raw, err := json.Marshal(disk)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	ledger := newNativeSessionLedger(stateDir)
	if ledger.loadErr != nil {
		t.Fatalf("load legacy turn fields: %v", ledger.loadErr)
	}
	binding, ok := ledger.get("legacy-tab", "legacy-chat", "codex")
	if !ok || binding.SessionID != "saved-session" || len(binding.LegacyPendingOperation) != 0 || len(binding.LegacyLastOperation) != 0 {
		t.Fatalf("legacy migration changed session or retained turn gate: %#v ok=%v", binding, ok)
	}
	migrated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(migrated, []byte("pendingOperation")) || bytes.Contains(migrated, []byte("lastOperation")) {
		t.Fatalf("legacy provider-turn fields remained on disk: %s", migrated)
	}
}

func TestProviderLaneStoreRejectsUnsupportedVersionWithoutWriting(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	path := filepath.Join(stateDir, nativeSessionLedgerFilename)
	raw := []byte(`{"v":7,"bindings":[]}`)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	ledger := newNativeSessionLedger(stateDir)
	if ledger.loadErr == nil || !strings.Contains(ledger.loadErr.Error(), "schema v7 is unsupported") {
		t.Fatalf("unsupported provider lane schema was accepted: %v", ledger.loadErr)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatalf("unsupported provider lane store was rewritten: got=%q want=%q", got, raw)
	}
}

func TestDeferredCandidateRoundTripAndThreadCommitAreAtomic(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	ledger := newNativeSessionLedger(stateDir)
	if err := ledger.put(nativeSessionBinding{
		TabID: "candidate-tab", ChatID: "candidate-chat", ProviderID: "codex",
		SessionID: "candidate-thread", CWD: stateDir,
	}); err != nil {
		t.Fatal(err)
	}
	reloaded := newNativeSessionLedger(stateDir)
	candidate, ok := reloaded.get("candidate-tab", "candidate-chat", "codex")
	if !ok || candidate.ThreadCommitted {
		t.Fatalf("unconsumed candidate did not round-trip provisionally: ok=%v binding=%#v", ok, candidate)
	}
	committed, ok := reloaded.commitThread(candidate.TabID, candidate.ChatID, candidate.ProviderID, candidate.SessionID)
	if !ok || !committed.ThreadCommitted {
		t.Fatalf("candidate input receipt did not commit the thread: ok=%v binding=%#v", ok, committed)
	}
	crashReload := newNativeSessionLedger(stateDir)
	durable, ok := crashReload.get(candidate.TabID, candidate.ChatID, candidate.ProviderID)
	if !ok || !durable.ThreadCommitted {
		t.Fatalf("thread commit did not survive reload: ok=%v binding=%#v", ok, durable)
	}
}

func TestProviderHeadAdvanceRequiresAttestedMonotonicLineage(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	ledger := newNativeSessionLedger(stateDir, "machine")
	binding := nativeSessionBinding{
		TabID: "tab", ChatID: "chat", ProviderID: "claude", SessionID: "root", CWD: stateDir,
		MachineID: "machine", AccountScope: "account", InstallScope: "install", RealmVerified: true,
		ThreadLineage: 1, ThreadCommitted: true,
	}
	if err := ledger.put(binding); err != nil {
		t.Fatal(err)
	}
	if !ledger.adoptProviderSession("tab", "chat", "claude", "root", "root", "head-2", 2, "attestation") {
		t.Fatal("valid provider lineage transition was rejected")
	}
	got, ok := ledger.get("tab", "chat", "claude")
	if !ok || got.SessionID != "root" || got.ProviderSessionID != "head-2" || got.ThreadLineage != 2 || got.LineageProof != "attestation" {
		t.Fatalf("advanced binding = %#v", got)
	}
	reloaded := newNativeSessionLedger(stateDir, "machine")
	reloadedHead, ok := reloaded.get("tab", "chat", "claude")
	if !ok || reloadedHead.SessionID != "root" || bindingCurrentThreadID(reloadedHead) != "head-2" || !reloaded.ownsSession("tab", "chat", "head-2") {
		t.Fatalf("restart lost immutable root/current-head identity: %#v", reloadedHead)
	}
	if ledger.adoptProviderSession("tab", "chat", "claude", "root", "root", "attacker-head", 3, "attestation") {
		t.Fatal("lineage event with a stale previous head was accepted")
	}
	got, ok = ledger.get("tab", "chat", "claude")
	if !ok || got.ProviderSessionID != "head-2" || got.ThreadLineage != 2 || got.LineageProof != "attestation" {
		t.Fatalf("conflicting lineage event mutated the valid lane: %#v", got)
	}
}

func TestCorruptNativeSessionLedgerDisablesResumeWithoutOverwriting(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	stateDir := t.TempDir()
	ledger := newNativeSessionLedger(stateDir)
	for _, binding := range []nativeSessionBinding{
		{TabID: "tab-delete", ChatID: "chat-delete", ProviderID: "codex", SessionID: "codex-thread", CWD: stateDir, ThreadCommitted: true},
		{TabID: "tab-delete", ChatID: "chat-delete", ProviderID: "claude", SessionID: "claude-thread", CWD: stateDir, ThreadCommitted: true},
		{TabID: "tab-keep", ChatID: "chat-keep", ProviderID: "codex", SessionID: "keep-thread", CWD: stateDir, ThreadCommitted: true},
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
	t.Parallel()
	stateDir := t.TempDir()
	ledger := newNativeSessionLedger(stateDir)
	first := nativeSessionBinding{
		TabID: "tab-a", ChatID: "chat-a", ProviderID: "codex", SessionID: "native-shared",
		CWD: stateDir, ThreadCommitted: true,
	}
	if err := ledger.put(first); err != nil {
		t.Fatal(err)
	}
	if _, ok := ledger.get("tab-a", "chat-b", "codex"); ok {
		t.Fatal("a tab binding was returned for a different conversation id")
	}
	if err := ledger.put(nativeSessionBinding{
		TabID: "tab-b", ChatID: "chat-b", ProviderID: "codex", SessionID: "native-shared",
		CWD: stateDir, ThreadCommitted: true,
	}); err == nil {
		t.Fatal("one provider-native session was accepted for two chat owners")
	}
}

func TestNativeSessionLedgerNeverPrunesBindingsFromRendererMirror(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	ledger := newNativeSessionLedger(stateDir)
	for _, binding := range []nativeSessionBinding{
		{TabID: "real-tab", ChatID: "real-chat", ProviderID: "codex", SessionID: "native-real", CWD: stateDir, ThreadCommitted: true},
		{TabID: "test-tab", ChatID: "test-chat", ProviderID: "mock", SessionID: "native-test", CWD: stateDir, ThreadCommitted: true},
		{TabID: "changed-tab", ChatID: "old-chat", ProviderID: "claude", SessionID: "native-old", CWD: stateDir, ThreadCommitted: true},
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
		t.Fatal("renderer mirror removed a durable binding")
	}
	if _, ok := reloaded.get("test-tab", "test-chat", "mock"); !ok {
		t.Fatal("mirror omission removed a durable binding")
	}
	if _, ok := reloaded.get("changed-tab", "old-chat", "claude"); !ok {
		t.Fatal("renderer chat replacement removed a durable binding")
	}
	if len(reloaded.bindings) != 3 {
		t.Fatalf("bindings after mirror reload = %d, want 3", len(reloaded.bindings))
	}
}

func TestNativeLaneOwnershipSurvivesDisposableTabChange(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	ledger := newNativeSessionLedger(stateDir, "machine-tab-independent")
	if err := ledger.put(nativeSessionBinding{
		TabID: "old-tab", ChatID: "immutable-chat", ProviderID: "codex",
		SessionID: "native-thread", CWD: stateDir, ThreadCommitted: true,
	}); err != nil {
		t.Fatal(err)
	}

	fromNewTab, ok := ledger.get("new-tab", "immutable-chat", "codex")
	if !ok || fromNewTab.SessionID != "native-thread" {
		t.Fatalf("same chat did not retain its lane across tab change: %#v ok=%v", fromNewTab, ok)
	}
	if !ledger.updateAttachment("new-tab", "immutable-chat", "codex", "native-thread") {
		t.Fatal("could not persist the new disposable attachment")
	}
	reloaded := newNativeSessionLedger(stateDir, "machine-tab-independent")
	got, ok := reloaded.get("third-tab", "immutable-chat", "codex")
	if !ok || got.TabID != "new-tab" || got.SessionID != "native-thread" {
		t.Fatalf("tab-independent lane did not survive reload: %#v ok=%v", got, ok)
	}
}

func TestMultipleWorkspaceEpochsFailClosedInsteadOfSelectingOrCreating(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	ledger := newNativeSessionLedger(stateDir, "machine-lanes")
	cwdA, cwdB := filepath.Join(stateDir, "a"), filepath.Join(stateDir, "b")
	for _, binding := range []nativeSessionBinding{
		{TabID: "tab", ChatID: "chat", ProviderID: "codex", SessionID: "thread-a", CWD: cwdA, ThreadCommitted: true},
		{TabID: "tab", ChatID: "chat", ProviderID: "codex", SessionID: "thread-b", CWD: cwdB, ThreadCommitted: true},
	} {
		if err := ledger.put(binding); err != nil {
			t.Fatal(err)
		}
	}
	if ambiguous, ok := ledger.get("tab", "chat", "codex"); ok {
		t.Fatalf("workspace-free lookup selected an ambiguous lane: %#v", ambiguous)
	}
	for cwd, sessionID := range map[string]string{cwdA: "thread-a", cwdB: "thread-b"} {
		binding, ok := ledger.getForWorkspace("another-tab", "chat", "codex", cwd)
		if !ok || binding.SessionID != sessionID {
			t.Fatalf("workspace lane %q = %#v ok=%v, want %q", cwd, binding, ok, sessionID)
		}
	}
}

func TestProviderNativeThreadIDsAreScopedByProviderRealm(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	ledger := newNativeSessionLedger(stateDir, "machine-provider-scope")
	for _, providerID := range []string{"codex", "claude"} {
		if err := ledger.put(nativeSessionBinding{
			TabID: "tab-" + providerID, ChatID: "chat-" + providerID, ProviderID: providerID,
			SessionID: "same-opaque-thread-id", CWD: stateDir, ThreadCommitted: true,
		}); err != nil {
			t.Fatalf("provider-scoped thread %s: %v", providerID, err)
		}
	}
}

func TestProviderLaneMovedToAnotherMachineIsRejectedAtLoad(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	first := newNativeSessionLedger(stateDir, "machine-a")
	if err := first.put(nativeSessionBinding{
		TabID: "tab", ChatID: "chat", ProviderID: "codex", SessionID: "thread",
		CWD: stateDir, ThreadCommitted: true,
	}); err != nil {
		t.Fatal(err)
	}
	reloaded := newNativeSessionLedger(stateDir, "machine-b")
	if reloaded.loadErr == nil || !strings.Contains(reloaded.loadErr.Error(), "another machine") {
		t.Fatalf("cross-machine lane load error = %v", reloaded.loadErr)
	}
	if binding, ok := reloaded.get("tab", "chat", "codex"); ok {
		t.Fatalf("invalid cross-machine lane entered runtime state: %#v", binding)
	}
}

func TestManagerWithoutExplicitStateDirCannotWriteRepositoryLedger(t *testing.T) {
	t.Parallel()
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

func TestMockNativeSessionResumesExactThreadAcrossManagerRestart(t *testing.T) {
	t.Parallel()
	fixture := newPersistentMockFixture(t, "resume")
	firstManager, firstEvents := fixture.newManager()
	firstSession := fixture.newSession(t, firstManager)
	if _, err := firstManager.SetModel(context.Background(), firstSession.SessionID, "mock-deterministic[high]"); err != nil {
		t.Fatalf("set native model: %v", err)
	}
	if _, err := firstManager.SetMode(context.Background(), firstSession.SessionID, "bypass"); err != nil {
		t.Fatalf("set native mode: %v", err)
	}
	firstEnd := fixture.runTurn(t, firstManager, firstEvents, firstSession.SessionID, "first native turn")
	archiveHistoryForEndedJob(t, fixture.stateDir, firstEnd, "first native turn")
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
	secondEnd := fixture.runTurn(t, secondManager, secondEvents, resumed.SessionID, "second native turn")
	result := asString(jobFromEnd(secondEnd)["result"])
	if !strings.Contains(result, "Mock ACP turn 2:") {
		t.Fatalf("provider turn counter did not resume: %q", result)
	}
	trace := readNativeMockTrace(t, fixture.traceFile)
	if !traceContains(trace, "[mock:lifecycle] session/resume") {
		t.Fatalf("trace did not use exact session/resume: %#v", trace)
	}
	if traceContains(trace, "WORKASS HISTORY DELTA SYNC") || strings.Contains(result, "Previous conversation") {
		t.Fatalf("exact resume replayed or delta-seeded history: result=%q trace=%#v", result, trace)
	}
}

func TestMockNativeSessionLoadAttachesTheExactThreadWithoutPublishingReplay(t *testing.T) {
	t.Parallel()
	fixture := newPersistentMockFixture(t, "load")
	firstManager, firstEvents := fixture.newManager()
	first := fixture.newSession(t, firstManager)
	end := fixture.runTurn(t, firstManager, firstEvents, first.SessionID, "durable native turn")
	archiveHistoryForEndedJob(t, fixture.stateDir, end, "durable native turn")
	firstManager.Reset()

	secondManager, secondEvents := fixture.newManager()
	t.Cleanup(func() { secondManager.Reset() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	loaded, err := secondManager.NewSession(ctx, SessionOptions{TabID: "native-tab", ChatID: "native-chat", ProviderID: "mock"})
	if err != nil {
		t.Fatalf("load exact native thread: %v", err)
	}
	if loaded.SessionID != first.SessionID {
		t.Fatalf("same-id load changed native thread: first=%q loaded=%q", first.SessionID, loaded.SessionID)
	}
	binding, ok := secondManager.nativeSessions.get("native-tab", "native-chat", "mock")
	if !ok || binding.SessionID != first.SessionID {
		t.Fatalf("same-id load changed the durable binding: %#v ok=%v", binding, ok)
	}
	secondEnd := fixture.runTurn(t, secondManager, secondEvents, loaded.SessionID, "after exact load")
	if result := asString(jobFromEnd(secondEnd)["result"]); !strings.Contains(result, "Mock ACP turn 2:") {
		t.Fatalf("same-id load did not preserve provider context: %q", result)
	}
	for _, event := range secondEvents.snapshot() {
		raw, _ := json.Marshal(event.payload)
		if strings.Contains(string(raw), "[mock:loaded-history]") {
			t.Fatalf("session/load replay entered the Workass event stream: %s", raw)
		}
	}
	trace := readNativeMockTrace(t, fixture.traceFile)
	if persistentMockSessionCount(t, fixture.sessionFile) != 1 || !traceContains(trace, "[mock:lifecycle] session/load") || traceContains(trace, "[mock:lifecycle] session/resume") {
		t.Fatalf("load-only attachment did not use exactly one same-id load: %#v", trace)
	}

	// The same durable binding must also fail closed if a later load reports a
	// different native identity. Reuse the established provider thread instead
	// of paying for a second create/turn/restart scenario.
	secondManager.Reset()
	conflictManager, _ := fixture.newManagerTuned(func(opts *Options) {
		opts.Provider.Env["WORKASS_MOCK_ACP_MISMATCHED_ATTACHMENT_ID"] = "different-native-thread"
	})
	t.Cleanup(func() { conflictManager.Reset() })
	_, err = conflictManager.NewSession(context.Background(), SessionOptions{TabID: "native-tab", ChatID: "native-chat", ProviderID: "mock"})
	if !providercontract.ErrorIs(err, providercontract.ErrorNativeIdentityConflict) {
		t.Fatalf("changed load identity error = %v, want native identity conflict", err)
	}
	if binding, ok := conflictManager.nativeSessions.get("native-tab", "native-chat", "mock"); !ok || binding.SessionID != first.SessionID {
		t.Fatalf("identity conflict changed the durable binding: %#v ok=%v", binding, ok)
	}
}

func TestExactAttachmentDoesNotTryLoadAfterSelectedResumeFails(t *testing.T) {
	t.Parallel()
	fixture := newPersistentMockFixture(t, "both")
	firstManager, firstEvents := fixture.newManager()
	first := fixture.newSession(t, firstManager)
	fixture.runTurn(t, firstManager, firstEvents, first.SessionID, "durable native turn")
	firstManager.Reset()

	secondManager, _ := fixture.newManagerTuned(func(opts *Options) {
		opts.Provider.Env["WORKASS_MOCK_ACP_FAIL_RESUME"] = "1"
	})
	t.Cleanup(func() { secondManager.Reset() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := secondManager.NewSession(ctx, SessionOptions{TabID: "native-tab", ChatID: "native-chat", ProviderID: "mock"})
	if !providercontract.ErrorIs(err, providercontract.ErrorTransientTransport) {
		t.Fatalf("selected resume failure = %v, want transient transport", err)
	}
	trace := readNativeMockTrace(t, fixture.traceFile)
	if !traceContains(trace, "[mock:lifecycle] session/resume") || traceContains(trace, "[mock:lifecycle] session/load") {
		t.Fatalf("exact attachment tried another wire method after resume failed: %#v", trace)
	}
	if binding, ok := secondManager.nativeSessions.get("native-tab", "native-chat", "mock"); !ok || binding.SessionID != first.SessionID {
		t.Fatalf("failed exact attachment changed the durable binding: %#v ok=%v", binding, ok)
	}
}

func TestMockNativeSessionNeverResumesAfterConversationIdentityChanges(t *testing.T) {
	t.Parallel()
	fixture := newPersistentMockFixture(t, "resume")
	firstManager, firstEvents := fixture.newManager()
	firstSession := fixture.newSession(t, firstManager)
	fixture.runTurn(t, firstManager, firstEvents, firstSession.SessionID, "original owner turn")
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

func TestMockNativeSessionUnseenWorkassHistoryDoesNotGovernExactResume(t *testing.T) {
	t.Parallel()
	fixture := newPersistentMockFixture(t, "resume")
	firstManager, firstEvents := fixture.newManager()
	firstSession := fixture.newSession(t, firstManager)
	firstEnd := fixture.runTurn(t, firstManager, firstEvents, firstSession.SessionID, "first native turn")
	history := archiveHistoryForEndedJob(t, fixture.stateDir, firstEnd, "first native turn")
	extra := []historyMessage{
		{Role: "user", Content: "turn handled by another provider", At: "2026-07-12T01:00:00Z"},
		{Role: "assistant", Content: "other provider answer", At: "2026-07-12T01:00:01Z"},
	}
	appendNativeTestArchive(t, fixture.stateDir, extra)
	firstManager.Reset()

	secondManager, secondEvents := fixture.newManager()
	t.Cleanup(func() { secondManager.Reset() })
	resumed := fixture.newSession(t, secondManager)
	if resumed.SessionID != firstSession.SessionID {
		t.Fatalf("history mismatch replaced the native session: first=%s resumed=%s", firstSession.SessionID, resumed.SessionID)
	}
	secondEnd := fixture.runTurn(t, secondManager, secondEvents, resumed.SessionID, "after provider switch")
	if job := jobFromEnd(secondEnd); asString(job["status"]) != "done" {
		t.Fatalf("Workass archive incorrectly governed exact native resume: %#v", job)
	}
	trace := readNativeMockTrace(t, fixture.traceFile)
	if traceContains(trace, "WORKASS HISTORY DELTA SYNC") || traceContains(trace, "turn handled by another provider") || traceContains(trace, "other provider answer") || !traceContains(trace, "after provider switch") {
		t.Fatalf("unimported Workass history reached the provider: %#v", trace)
	}
	for _, line := range trace {
		if strings.Contains(line, "WORKASS HISTORY DELTA SYNC") && strings.Contains(line, "first native turn") {
			t.Fatalf("delta seed resent history already seen by the native session: %q", line)
		}
	}

	// Simulate rewind/edit of canonical Workass history. The saved provider cursor
	// no longer names a prefix and must never be guessed-merged. This is the same
	// exact-resume invariant as the unseen-history case above, so exercise it on
	// the already-established native thread instead of creating another one.
	secondManager.Reset()
	history[0].Content = "rewritten canonical turn"
	writeNativeTestArchive(t, fixture.stateDir, history)
	thirdManager, thirdEvents := fixture.newManager()
	t.Cleanup(func() { thirdManager.Reset() })
	resumedAfterRewrite := fixture.newSession(t, thirdManager)
	if resumedAfterRewrite.SessionID != firstSession.SessionID {
		t.Fatalf("divergence replaced the exact provider session: first=%s resumed=%s", firstSession.SessionID, resumedAfterRewrite.SessionID)
	}
	end := fixture.runTurn(t, thirdManager, thirdEvents, resumedAfterRewrite.SessionID, "continue after rewrite")
	job := jobFromEnd(end)
	if asString(job["status"]) != "done" {
		t.Fatalf("renderer archive divergence incorrectly blocked native resume: %#v", job)
	}
	trace = readNativeMockTrace(t, fixture.traceFile)
	if traceContains(trace, "rewritten canonical turn") || !traceContains(trace, "continue after rewrite") || persistentMockSessionCount(t, fixture.sessionFile) != 1 {
		t.Fatalf("divergence replayed history or created a replacement: %#v", trace)
	}
}

// Regression: a hibernated engine keeps its session→bridge mapping so the
// chat stays resolvable, but the replacement bridge that session/resumes the
// same provider-native session must take ownership instead of failing with
// "ACP session id collision" (which bricked the chat until a daemon restart).
func TestMockNativeSessionResumeAfterHibernationDoesNotCollide(t *testing.T) {
	t.Parallel()
	fixture := newPersistentMockFixture(t, "resume")
	manager, events := fixture.newManagerTuned(func(opts *Options) {
		opts.HibernateTTL = 80 * time.Millisecond
		opts.LifecycleCheckInterval = 10 * time.Millisecond
	})
	t.Cleanup(func() { manager.Reset() })

	first := fixture.newSession(t, manager)
	end := fixture.runTurn(t, manager, events, first.SessionID, "turn before hibernation")
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
// must fail closed without disturbing the real owner or rewriting the corrupt
// binding to a newly created thread.
func TestMockNativeSessionForeignLiveCollisionFailsClosed(t *testing.T) {
	t.Parallel()
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
		SessionID: owner.SessionID, CWD: fixture.root, ThreadCommitted: true,
	}); err != nil {
		t.Fatalf("seed foreign binding: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := manager.NewSession(ctx, SessionOptions{TabID: "other-tab", ChatID: "other-chat", ProviderID: "mock"})
	if !providercontract.ErrorIs(err, providercontract.ErrorNativeIdentityConflict) {
		t.Fatalf("live collision error = %v, want native identity conflict", err)
	}
	if live, ok := manager.LiveSession(owner.SessionID); !ok || live.TabID != "native-tab" {
		t.Fatalf("original owner lost its live session: %+v ok=%v", live, ok)
	}
	if binding, ok := manager.nativeSessions.get("other-tab", "other-chat", "mock"); !ok || binding.SessionID != owner.SessionID {
		t.Fatalf("foreign binding was silently rewritten: %#v ok=%v", binding, ok)
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

func (f *persistentMockFixture) runTurn(t *testing.T, manager *Manager, events *eventCollector, sessionID, prompt string) map[string]any {
	job, err := manager.StartJob(context.Background(), JobStartOptions{
		Kind: "app-chat", SessionID: sessionID, TabID: "native-tab", ChatID: "native-chat",
		ProviderID: "mock", Prompt: prompt,
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

func persistentMockSessionCount(t *testing.T, path string) int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read persistent mock sessions: %v", err)
	}
	var disk struct {
		Sessions []json.RawMessage `json:"sessions"`
	}
	if err := json.Unmarshal(raw, &disk); err != nil {
		t.Fatalf("decode persistent mock sessions: %v", err)
	}
	return len(disk.Sessions)
}
