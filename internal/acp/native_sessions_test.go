package acp

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	providercontract "workass/internal/provider"
)

const nativeHistoryDigestVersion = 2

type historyMessage struct {
	ID      string
	Role    string
	Content string
	At      string
}

func historyDigest(history []historyMessage) string {
	hash := sha256.New()
	for _, message := range history {
		hash.Write([]byte(message.ID))
		hash.Write([]byte{0xfe})
		hash.Write([]byte(message.Role))
		hash.Write([]byte{0})
		hash.Write([]byte(message.Content))
		hash.Write([]byte{0xff})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func TestNativeSessionLedgerPersistsAndRejectsStaleGeneration(t *testing.T) {
	stateDir := t.TempDir()
	ledger := newNativeSessionLedger(stateDir)
	binding := nativeSessionBinding{
		TabID: "tab-ledger", ChatID: "chat-ledger", ProviderID: "codex", SessionID: "native-thread-1",
		CWD: stateDir, HistoryHash: historyDigest(nil), ResumeSafe: true,
	}
	if err := ledger.put(binding); err != nil {
		t.Fatalf("put binding: %v", err)
	}
	saved, ok := ledger.get("tab-ledger", "chat-ledger", "codex")
	if !ok || saved.Generation != 1 || saved.SessionID != "native-thread-1" {
		t.Fatalf("saved binding = %#v", saved)
	}
	if ledger.markInFlight("tab-ledger", "chat-ledger", "codex", saved.SessionID, saved.Generation+1, "gpt", "agent", "future-operation", "future-digest") {
		t.Fatal("stale/future generation unexpectedly dispatched a native operation")
	}
	if !ledger.markInFlight("tab-ledger", "chat-ledger", "codex", saved.SessionID, saved.Generation, "gpt", "agent", "operation-1", "prompt-digest-1") {
		t.Fatal("could not persist native operation dispatch")
	}
	crashReload := newNativeSessionLedger(stateDir)
	dirty, ok := crashReload.get("tab-ledger", "chat-ledger", "codex")
	if !ok || dirty.PendingOperation == nil || dirty.PendingOperation.State != nativeOperationDispatched || dirty.PendingOperation.OperationID != "operation-1" {
		t.Fatalf("crash-reloaded binding lost its dispatched operation: %#v", dirty)
	}
	if !ledger.markOperationConsumed("tab-ledger", "chat-ledger", "codex", saved.SessionID, "operation-1", "native-turn-1") {
		t.Fatal("input-consumed receipt was not durable")
	}
	if !ledger.settleOperation("tab-ledger", "chat-ledger", "codex", saved.SessionID, "operation-1", "end_turn", "result-digest-1", "gpt", "agent") {
		t.Fatal("terminal operation receipt was not durable")
	}

	reloaded := newNativeSessionLedger(stateDir)
	got, ok := reloaded.get("tab-ledger", "chat-ledger", "codex")
	if !ok || got.PendingOperation != nil || got.LastOperation == nil || got.LastOperation.State != nativeOperationTerminal || got.LastOperation.NativeTurnID != "native-turn-1" || got.ModelID != "gpt" || got.ModeID != "agent" {
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

func TestNativeOperationReadbackTransitionsAreDurableAndContradictionsFailClosed(t *testing.T) {
	stateDir := t.TempDir()
	ledger := newNativeSessionLedger(stateDir)
	if err := ledger.put(nativeSessionBinding{
		TabID: "operation-tab", ChatID: "operation-chat", ProviderID: "codex",
		SessionID: "operation-thread", CWD: stateDir,
	}); err != nil {
		t.Fatal(err)
	}
	binding, _ := ledger.get("operation-tab", "operation-chat", "codex")
	if !ledger.markInFlight(
		binding.TabID, binding.ChatID, binding.ProviderID, binding.SessionID,
		binding.Generation, "", "", "operation-absent", "digest-absent",
	) {
		t.Fatal("could not persist absent-operation fixture")
	}
	if !ledger.recordOperationReadback(
		binding.TabID, binding.ChatID, binding.ProviderID, binding.SessionID,
		"operation-absent", "", "absent", false, false, false,
	) {
		t.Fatal("authoritative absent readback was rejected")
	}
	afterAbsent, _ := ledger.get(binding.TabID, binding.ChatID, binding.ProviderID)
	if afterAbsent.PendingOperation != nil || afterAbsent.LastOperation == nil || afterAbsent.LastOperation.State != nativeOperationAbsent {
		t.Fatalf("absent operation was not settled safely: %#v", afterAbsent)
	}

	if !ledger.markInFlight(
		binding.TabID, binding.ChatID, binding.ProviderID, binding.SessionID,
		binding.Generation, "", "", "operation-consumed", "digest-consumed",
	) || !ledger.markOperationConsumed(
		binding.TabID, binding.ChatID, binding.ProviderID, binding.SessionID,
		"operation-consumed", "native-turn-2",
	) {
		t.Fatal("could not persist consumed-operation fixture")
	}
	if ledger.recordOperationReadback(
		binding.TabID, binding.ChatID, binding.ProviderID, binding.SessionID,
		"operation-consumed", "", "absent", false, false, false,
	) {
		t.Fatal("readback contradicted a durable consumption receipt")
	}
	stillPending, _ := ledger.get(binding.TabID, binding.ChatID, binding.ProviderID)
	if stillPending.PendingOperation == nil || stillPending.PendingOperation.State != nativeOperationConsumed {
		t.Fatalf("contradictory readback released the operation: %#v", stillPending)
	}
	if !ledger.recordOperationReadback(
		binding.TabID, binding.ChatID, binding.ProviderID, binding.SessionID,
		"operation-consumed", "native-turn-2", "completed", true, true, true,
	) {
		t.Fatal("matching terminal readback was rejected")
	}
	reloaded := newNativeSessionLedger(stateDir)
	terminal, _ := reloaded.get(binding.TabID, binding.ChatID, binding.ProviderID)
	if terminal.PendingOperation != nil || terminal.LastOperation == nil || terminal.LastOperation.State != nativeOperationTerminal || terminal.LastOperation.NativeTurnID != "native-turn-2" {
		t.Fatalf("terminal readback was not durable: %#v", terminal)
	}
}

func TestNativeOperationWriteFailureDoesNotPublishDispatchInMemory(t *testing.T) {
	stateDir := t.TempDir()
	ledger := newNativeSessionLedger(stateDir)
	if err := ledger.put(nativeSessionBinding{
		TabID: "write-failure-tab", ChatID: "write-failure-chat", ProviderID: "codex",
		SessionID: "write-failure-thread", CWD: stateDir,
	}); err != nil {
		t.Fatal(err)
	}
	binding, _ := ledger.get("write-failure-tab", "write-failure-chat", "codex")
	ledger.path = stateDir // rename onto an existing directory must fail.
	if ledger.markInFlight(
		binding.TabID, binding.ChatID, binding.ProviderID, binding.SessionID,
		binding.Generation, "", "", "must-not-publish", "digest",
	) {
		t.Fatal("dispatch succeeded despite a failed durable write")
	}
	got, _ := ledger.get(binding.TabID, binding.ChatID, binding.ProviderID)
	if got.PendingOperation != nil {
		t.Fatalf("failed durable write leaked an executable operation into memory: %#v", got.PendingOperation)
	}
}

func TestCurrentLaneStoreSchemaMigratesTransactionallyToV6(t *testing.T) {
	stateDir := t.TempDir()
	path := filepath.Join(stateDir, nativeSessionLedgerFilename)
	legacy := nativeSessionLedgerFile{Version: 4, Bindings: []nativeSessionBinding{{
		TabID: "v4-tab", ChatID: "v4-chat", ProviderID: "codex", SessionID: "v4-thread", CWD: stateDir,
	}}}
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	ledger := newNativeSessionLedger(stateDir)
	if ledger.loadErr != nil {
		t.Fatalf("migrate v4 lane store: %v", ledger.loadErr)
	}
	migratedRaw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var migrated nativeSessionLedgerFile
	if err := json.Unmarshal(migratedRaw, &migrated); err != nil || migrated.Version != currentNativeLaneStoreVersion || len(migrated.Bindings) != 1 {
		t.Fatalf("v6 migration receipt = %#v err=%v", migrated, err)
	}
}

func TestProviderHeadAdvanceRequiresAttestedMonotonicLineage(t *testing.T) {
	stateDir := t.TempDir()
	ledger := newNativeSessionLedger(stateDir, "machine")
	binding := nativeSessionBinding{
		TabID: "tab", ChatID: "chat", ProviderID: "claude", SessionID: "root", CWD: stateDir,
		MachineID: "machine", AccountScope: "account", InstallScope: "install", RealmVerified: true,
		ThreadLineage: 1, HistoryHash: historyDigest(nil), ResumeSafe: true,
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
	if !ok || !got.Quarantined || got.QuarantineReason != "unverified-provider-lineage-transition" {
		t.Fatalf("conflicting lineage event did not quarantine the lane: %#v", got)
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
	stateDir := t.TempDir()
	ledger := newNativeSessionLedger(stateDir)
	for _, binding := range []nativeSessionBinding{
		{TabID: "tab-delete", ChatID: "chat-delete", ProviderID: "codex", SessionID: "codex-thread", CWD: stateDir, HistoryHash: historyDigest(nil), ResumeSafe: true},
		{TabID: "tab-delete", ChatID: "chat-delete", ProviderID: "claude", SessionID: "claude-thread", CWD: stateDir, HistoryHash: historyDigest(nil), ResumeSafe: true},
		{TabID: "tab-keep", ChatID: "chat-keep", ProviderID: "codex", SessionID: "keep-thread", CWD: stateDir, HistoryHash: historyDigest(nil), ResumeSafe: true},
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
	stateDir := t.TempDir()
	ledger := newNativeSessionLedger(stateDir)
	first := nativeSessionBinding{
		TabID: "tab-a", ChatID: "chat-a", ProviderID: "codex", SessionID: "native-shared",
		CWD: stateDir, HistoryHash: historyDigest(nil), ResumeSafe: true,
	}
	if err := ledger.put(first); err != nil {
		t.Fatal(err)
	}
	if _, ok := ledger.get("tab-a", "chat-b", "codex"); ok {
		t.Fatal("a tab binding was returned for a different conversation id")
	}
	if err := ledger.put(nativeSessionBinding{
		TabID: "tab-b", ChatID: "chat-b", ProviderID: "codex", SessionID: "native-shared",
		CWD: stateDir, HistoryHash: historyDigest(nil), ResumeSafe: true,
	}); err == nil {
		t.Fatal("one provider-native session was accepted for two chat owners")
	}
}

func TestNativeSessionLedgerNeverPrunesBindingsFromRendererMirror(t *testing.T) {
	stateDir := t.TempDir()
	ledger := newNativeSessionLedger(stateDir)
	for _, binding := range []nativeSessionBinding{
		{TabID: "real-tab", ChatID: "real-chat", ProviderID: "codex", SessionID: "native-real", CWD: stateDir, HistoryHash: historyDigest(nil), ResumeSafe: true},
		{TabID: "test-tab", ChatID: "test-chat", ProviderID: "mock", SessionID: "native-test", CWD: stateDir, HistoryHash: historyDigest(nil), ResumeSafe: true},
		{TabID: "changed-tab", ChatID: "old-chat", ProviderID: "claude", SessionID: "native-old", CWD: stateDir, HistoryHash: historyDigest(nil), ResumeSafe: true},
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

func TestLegacyNativeSessionLedgerMigratesToVerifiedLaneStoreWithoutDeletingSource(t *testing.T) {
	stateDir := t.TempDir()
	legacy := nativeSessionLedgerFile{Version: 3, Bindings: []nativeSessionBinding{{
		TabID: "legacy-tab", ChatID: "legacy-chat", ProviderID: "codex", SessionID: "legacy-thread",
		CWD: stateDir, HistoryHash: historyDigest(nil), ResumeSafe: true,
	}}}
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	legacyPath := filepath.Join(stateDir, legacyNativeSessionLedgerFilename)
	if err := os.WriteFile(legacyPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	ledger := newNativeSessionLedger(stateDir, "machine-migration")
	if ledger.loadErr != nil {
		t.Fatalf("migrate legacy ledger: %v", ledger.loadErr)
	}
	binding, ok := ledger.get("legacy-tab", "legacy-chat", "codex")
	if !ok || binding.Quarantined || binding.LaneID == "" || binding.WorkspaceEpoch == "" || binding.MachineID != "machine-migration" {
		t.Fatalf("migrated lane = %#v", binding)
	}
	if binding.RealmVerified {
		t.Fatal("legacy account realm was guessed as verified")
	}
	if preserved, err := os.ReadFile(legacyPath); err != nil || string(preserved) != string(raw) {
		t.Fatalf("legacy source changed during migration: err=%v", err)
	}
	newRaw, err := os.ReadFile(filepath.Join(stateDir, nativeSessionLedgerFilename))
	if err != nil {
		t.Fatal(err)
	}
	var migrated nativeSessionLedgerFile
	if err := json.Unmarshal(newRaw, &migrated); err != nil || migrated.Version != currentNativeLaneStoreVersion || len(migrated.Bindings) != 1 {
		t.Fatalf("migrated disk receipt = %#v err=%v", migrated, err)
	}
}

func TestNativeLaneOwnershipSurvivesDisposableTabChange(t *testing.T) {
	stateDir := t.TempDir()
	ledger := newNativeSessionLedger(stateDir, "machine-tab-independent")
	if err := ledger.put(nativeSessionBinding{
		TabID: "old-tab", ChatID: "immutable-chat", ProviderID: "codex",
		SessionID: "native-thread", CWD: stateDir, ResumeSafe: true,
	}); err != nil {
		t.Fatal(err)
	}

	fromNewTab, ok := ledger.get("new-tab", "immutable-chat", "codex")
	if !ok || fromNewTab.SessionID != "native-thread" || fromNewTab.Quarantined {
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
	stateDir := t.TempDir()
	ledger := newNativeSessionLedger(stateDir, "machine-lanes")
	cwdA, cwdB := filepath.Join(stateDir, "a"), filepath.Join(stateDir, "b")
	for _, binding := range []nativeSessionBinding{
		{TabID: "tab", ChatID: "chat", ProviderID: "codex", SessionID: "thread-a", CWD: cwdA, HistoryHash: historyDigest(nil), ResumeSafe: true},
		{TabID: "tab", ChatID: "chat", ProviderID: "codex", SessionID: "thread-b", CWD: cwdB, HistoryHash: historyDigest(nil), ResumeSafe: true},
	} {
		if err := ledger.put(binding); err != nil {
			t.Fatal(err)
		}
	}
	ambiguous, ok := ledger.get("tab", "chat", "codex")
	if !ok || !ambiguous.Quarantined || ambiguous.SessionID != "" {
		t.Fatalf("ambiguous lane lookup = %#v ok=%v", ambiguous, ok)
	}
	for cwd, sessionID := range map[string]string{cwdA: "thread-a", cwdB: "thread-b"} {
		binding, ok := ledger.getForWorkspace("another-tab", "chat", "codex", cwd)
		if !ok || binding.Quarantined || binding.SessionID != sessionID {
			t.Fatalf("workspace lane %q = %#v ok=%v, want %q", cwd, binding, ok, sessionID)
		}
	}
}

func TestProviderNativeThreadIDsAreScopedByProviderRealm(t *testing.T) {
	stateDir := t.TempDir()
	ledger := newNativeSessionLedger(stateDir, "machine-provider-scope")
	for _, providerID := range []string{"codex", "claude"} {
		if err := ledger.put(nativeSessionBinding{
			TabID: "tab-" + providerID, ChatID: "chat-" + providerID, ProviderID: providerID,
			SessionID: "same-opaque-thread-id", CWD: stateDir, HistoryHash: historyDigest(nil), ResumeSafe: true,
		}); err != nil {
			t.Fatalf("provider-scoped thread %s: %v", providerID, err)
		}
	}
}

func TestProviderLaneMovedToAnotherMachineIsQuarantined(t *testing.T) {
	stateDir := t.TempDir()
	first := newNativeSessionLedger(stateDir, "machine-a")
	if err := first.put(nativeSessionBinding{
		TabID: "tab", ChatID: "chat", ProviderID: "codex", SessionID: "thread",
		CWD: stateDir, HistoryHash: historyDigest(nil), ResumeSafe: true,
	}); err != nil {
		t.Fatal(err)
	}
	reloaded := newNativeSessionLedger(stateDir, "machine-b")
	binding, ok := reloaded.get("tab", "chat", "codex")
	if !ok || !binding.Quarantined || !strings.Contains(binding.QuarantineReason, "another machine") {
		t.Fatalf("cross-machine lane = %#v ok=%v", binding, ok)
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

func TestMockNativeSessionResumesExactThreadAcrossManagerRestart(t *testing.T) {
	fixture := newPersistentMockFixture(t, "resume")
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
	if !traceContains(trace, "[mock:lifecycle] session/resume") {
		t.Fatalf("trace did not use exact session/resume: %#v", trace)
	}
	if traceContains(trace, "WORKASS HISTORY DELTA SYNC") || strings.Contains(result, "Previous conversation") {
		t.Fatalf("exact resume replayed or delta-seeded history: result=%q trace=%#v", result, trace)
	}
}

func TestMockNativeSessionLoadCapabilityCannotReplaceExactResume(t *testing.T) {
	fixture := newPersistentMockFixture(t, "load")
	firstManager, firstEvents := fixture.newManager()
	first := fixture.newSession(t, firstManager)
	end := fixture.runTurn(t, firstManager, firstEvents, first.SessionID, "durable native turn", nil)
	archiveHistoryForEndedJob(t, fixture.stateDir, end, "durable native turn")
	firstManager.Reset()

	secondManager, _ := fixture.newManager()
	t.Cleanup(func() { secondManager.Reset() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := secondManager.NewSession(ctx, SessionOptions{TabID: "native-tab", ChatID: "native-chat", ProviderID: "mock"})
	if !providercontract.ErrorIs(err, providercontract.ErrorUnsupportedCapability) {
		t.Fatalf("load-only established lane error = %v, want unsupported exact resume", err)
	}
	binding, ok := secondManager.nativeSessions.get("native-tab", "native-chat", "mock")
	if !ok || binding.SessionID != first.SessionID {
		t.Fatalf("load-only failure replaced the durable binding: %#v ok=%v", binding, ok)
	}
	trace := readNativeMockTrace(t, fixture.traceFile)
	if persistentMockSessionCount(t, fixture.sessionFile) != 1 || traceContains(trace, "[mock:lifecycle] session/load") {
		t.Fatalf("load-only failure created/loaded a replacement thread: %#v", trace)
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

func TestMockNativeSessionUnseenWorkassHistoryDoesNotGovernExactResume(t *testing.T) {
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
		t.Fatalf("history mismatch replaced the native session: first=%s resumed=%s", firstSession.SessionID, resumed.SessionID)
	}
	secondEnd := fixture.runTurn(t, secondManager, secondEvents, resumed.SessionID, "after provider switch", history)
	if job := jobFromEnd(secondEnd); asString(job["status"]) != "done" {
		t.Fatalf("renderer history incorrectly governed exact native resume: %#v", job)
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
}

func TestMockNativeSessionDivergenceDoesNotGovernExactResume(t *testing.T) {
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
	resumed := fixture.newSession(t, secondManager)
	if resumed.SessionID != firstSession.SessionID {
		t.Fatalf("divergence replaced the exact provider session: first=%s resumed=%s", firstSession.SessionID, resumed.SessionID)
	}
	end := fixture.runTurn(t, secondManager, secondEvents, resumed.SessionID, "continue after rewrite", history)
	job := jobFromEnd(end)
	if asString(job["status"]) != "done" {
		t.Fatalf("renderer archive divergence incorrectly blocked native resume: %#v", job)
	}
	trace := readNativeMockTrace(t, fixture.traceFile)
	if traceContains(trace, "rewritten canonical turn") || !traceContains(trace, "continue after rewrite") || persistentMockSessionCount(t, fixture.sessionFile) != 1 {
		t.Fatalf("divergence replayed history or created a replacement: %#v", trace)
	}
}

func TestMockNativeSessionInFlightGuardResumesExactThreadAndBlocksAdmission(t *testing.T) {
	fixture := newPersistentMockFixture(t, "resume")
	firstManager, firstEvents := fixture.newManager()
	firstSession := fixture.newSession(t, firstManager)
	firstEnd := fixture.runTurn(t, firstManager, firstEvents, firstSession.SessionID, "durable turn", nil)
	history := archiveHistoryForEndedJob(t, fixture.stateDir, firstEnd, "durable turn")
	binding, ok := firstManager.nativeSessions.get("native-tab", "native-chat", "mock")
	if !ok || !firstManager.nativeSessions.markInFlight("native-tab", "native-chat", "mock", firstSession.SessionID, binding.Generation, "", "", "operation-crash", "prompt-digest-crash") {
		t.Fatal("could not simulate daemon death after provider prompt dispatch")
	}
	firstManager.Reset()

	secondManager, secondEvents := fixture.newManager()
	t.Cleanup(func() { secondManager.Reset() })
	resumed := fixture.newSession(t, secondManager)
	if resumed.SessionID != firstSession.SessionID {
		t.Fatalf("in-flight ambiguity replaced the native thread: first=%s resumed=%s", firstSession.SessionID, resumed.SessionID)
	}
	end := fixture.runTurn(t, secondManager, secondEvents, resumed.SessionID, "recover after daemon crash", history)
	job := jobFromEnd(end)
	if asString(job["status"]) != "failed" || !strings.Contains(asString(job["result"]), "unresolved delivery operation") {
		t.Fatalf("ambiguous in-flight lane did not block admission: %#v", job)
	}
	trace := readNativeMockTrace(t, fixture.traceFile)
	if traceContains(trace, "recover after daemon crash") || persistentMockSessionCount(t, fixture.sessionFile) != 1 {
		t.Fatalf("crash ambiguity replayed/replaced the native thread: %#v", trace)
	}
}

func TestMockNativeSessionTerminalOperationReadbackClearsOnlyExactPendingID(t *testing.T) {
	fixture := newPersistentMockFixture(t, "resume")
	fixture.operationReadback = true
	firstManager, firstEvents := fixture.newManager()
	firstSession := fixture.newSession(t, firstManager)
	fixture.runTurn(t, firstManager, firstEvents, firstSession.SessionID, "provider completed this operation", nil)
	binding, ok := firstManager.nativeSessions.get("native-tab", "native-chat", "mock")
	if !ok || binding.LastOperation == nil || binding.LastOperation.OperationID == "" {
		t.Fatalf("completed operation receipt missing: %#v", binding)
	}
	completedOperationID := binding.LastOperation.OperationID
	if !firstManager.nativeSessions.markInFlight(
		binding.TabID, binding.ChatID, binding.ProviderID, binding.SessionID,
		binding.Generation, "", "", completedOperationID, "simulated-pre-terminal-commit",
	) {
		t.Fatal("could not simulate crash before terminal journal commit")
	}
	firstManager.Reset()

	secondManager, secondEvents := fixture.newManager()
	t.Cleanup(func() { secondManager.Reset() })
	resumed := fixture.newSession(t, secondManager)
	if resumed.SessionID != firstSession.SessionID {
		t.Fatalf("operation reconciliation replaced native thread: first=%s resumed=%s", firstSession.SessionID, resumed.SessionID)
	}
	reconciled, ok := secondManager.nativeSessions.get("native-tab", "native-chat", "mock")
	if !ok || reconciled.PendingOperation != nil || reconciled.LastOperation == nil || reconciled.LastOperation.State != nativeOperationTerminal || reconciled.LastOperation.OperationID != completedOperationID {
		t.Fatalf("terminal provider readback did not settle exact operation: %#v", reconciled)
	}
	end := fixture.runTurn(t, secondManager, secondEvents, resumed.SessionID, "turn after terminal reconciliation", nil)
	if job := jobFromEnd(end); asString(job["status"]) != "done" {
		t.Fatalf("reconciled lane did not admit the next turn: %#v", job)
	}
}

func TestMockNativeSessionAuthoritativeAbsentReadbackClearsUnsentOperation(t *testing.T) {
	fixture := newPersistentMockFixture(t, "resume")
	fixture.operationReadback = true
	firstManager, _ := fixture.newManager()
	firstSession := fixture.newSession(t, firstManager)
	binding, ok := firstManager.nativeSessions.get("native-tab", "native-chat", "mock")
	if !ok || !firstManager.nativeSessions.markInFlight(
		binding.TabID, binding.ChatID, binding.ProviderID, binding.SessionID,
		binding.Generation, "", "", "operation-never-sent", "unsent-prompt-digest",
	) {
		t.Fatal("could not persist unsent operation fixture")
	}
	firstManager.Reset()

	secondManager, secondEvents := fixture.newManager()
	t.Cleanup(func() { secondManager.Reset() })
	resumed := fixture.newSession(t, secondManager)
	if resumed.SessionID != firstSession.SessionID {
		t.Fatalf("absent readback replaced native thread: first=%s resumed=%s", firstSession.SessionID, resumed.SessionID)
	}
	reconciled, ok := secondManager.nativeSessions.get("native-tab", "native-chat", "mock")
	if !ok || reconciled.PendingOperation != nil || reconciled.LastOperation == nil || reconciled.LastOperation.State != nativeOperationAbsent || reconciled.LastOperation.OperationID != "operation-never-sent" {
		t.Fatalf("authoritative absence did not clear the unsent operation: %#v", reconciled)
	}
	end := fixture.runTurn(t, secondManager, secondEvents, resumed.SessionID, "turn after absent reconciliation", nil)
	if job := jobFromEnd(end); asString(job["status"]) != "done" {
		t.Fatalf("absent-reconciled lane did not admit the next turn: %#v", job)
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
// must fail closed without disturbing the real owner or rewriting the corrupt
// binding to a newly created thread.
func TestMockNativeSessionForeignLiveCollisionFailsClosed(t *testing.T) {
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
	t                 *testing.T
	root              string
	stateDir          string
	sessionFile       string
	traceFile         string
	capability        string
	operationReadback bool
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
				"WORKASS_MOCK_ACP_OPERATION_READBACK": map[bool]string{true: "1", false: "0"}[f.operationReadback],
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
