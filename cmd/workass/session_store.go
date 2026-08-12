package main

import (
	"bufio"
	"bytes"
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"hash/maphash"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf16"

	"workass/internal/acp"
	providercontract "workass/internal/provider"
)

const (
	sessionStateFilename             = "session-state.json"
	sessionJournalDirname            = ".session-stream"
	sessionJournalQuarantineDir      = "quarantine"
	droppedChatDirname               = "dropped-chats"
	sessionImageDirname              = "images"
	sessionImageDataRefField         = "_workassImageDataRef"
	sessionMessageImageRefField      = "_workassMessageImageRef"
	maxPersistedSessionImageBytes    = 64 * 1024 * 1024
	sessionImageGCGrace              = 24 * time.Hour
	sessionJournalVersion            = 1
	workspaceRevisionField           = "workspaceRevision"
	presentationRevisionField        = "presentationRevision"
	agentQueueRevisionField          = "agentQueueRevision"
	runtimeControlRevisionField      = "runtimeControlRevision"
	globalPresentationRevisionField  = "globalRevision"
	globalPresentationOperationField = "_workassGlobalOperationId"
	globalPresentationReceiptsField  = "_workassGlobalMutationReceipts"
	agentQueueMessageField           = "agentQueueId"
	providerSessionImageRefPrefix    = "workass-session-image:"
)

var (
	sessionStoresMu     sync.Mutex
	sessionStores       = map[string]*sessionStore{}
	sessionIDSeq        atomic.Uint64
	sessionStoreLogLine = func(line string) { fmt.Fprintln(os.Stderr, line) }
	errInternalJournal  = errors.New("ignored legacy internal subagent journal")
)

// sessionStore is the bounded pre-actor migration reader plus the daemon-global
// presentation store. Once actor cutover commits, chat rows are neither read
// nor written here; providerChatRuntime projects every chat from its actor.
type sessionStore struct {
	mu   sync.Mutex
	path string
	// published is a lock-free immutable read view of snapshot. Before cutover it
	// is migration input; after cutover it contains daemon-global presentation
	// state only.
	published      atomic.Pointer[sessionGeneration]
	snapshot       map[string]any
	jobs           map[string]*sessionJob
	pending        []*sessionJob
	jobOrder       []string
	loadErr        error
	persistSeq     uint64
	droppedChatIDs map[string]struct{}

	persistMu  sync.Mutex
	writtenSeq uint64

	// actorCutover is set only after providerChatRuntime verifies the durable
	// global cutover receipt and every referenced actor file. In this mode the
	// store owns daemon-global UI preferences only; chats are projected from
	// actors and no legacy chat merge/journal path is legal.
	actorCutover          bool
	cutoverReceiptPresent bool
}

type sessionGeneration struct {
	root map[string]any
}

func newSessionGeneration(root map[string]any) *sessionGeneration {
	return &sessionGeneration{root: root}
}

func (s *sessionStore) publishedGeneration() *sessionGeneration {
	if s == nil {
		return nil
	}
	return s.published.Load()
}

type sessionJob struct {
	ID          string
	TabID       string
	ChatID      string
	UserID      string
	AssistantID string
	// RootAssistantID identifies the provider-native turn while AssistantID
	// follows the current chronological continuation segment after steering.
	RootAssistantID string
	Finished        bool
	// output preserves the provider's complete ordered assistant text for
	// journal offsets/native history. contentOutput and finalOutput own the two
	// visible surfaces when Codex supplies typed message phases.
	output              strings.Builder
	contentOutput       strings.Builder
	finalOutput         strings.Builder
	outputReady         bool
	typedOutput         bool
	contentSegmentStart int
	finalSegmentStart   int
}

type recoveredSessionJournal struct {
	path     string
	turnID   string
	job      *sessionJob
	terminal bool
}

type sessionJournalQuarantineError struct {
	Count int
}

func (e *sessionJournalQuarantineError) Error() string {
	if e == nil || e.Count <= 0 {
		return ""
	}
	if e.Count == 1 {
		return "1 session journal quarantined"
	}
	return fmt.Sprintf("%d session journals quarantined", e.Count)
}

func sharedSessionStore(stateDir string) *sessionStore {
	stateDir = strings.TrimSpace(stateDir)
	if stateDir == "" {
		return &sessionStore{jobs: map[string]*sessionJob{}, droppedChatIDs: map[string]struct{}{}}
	}
	path := filepath.Join(stateDir, sessionStateFilename)
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	sessionStoresMu.Lock()
	defer sessionStoresMu.Unlock()
	if store := sessionStores[path]; store != nil {
		return store
	}
	store := newSessionStore(path)
	sessionStores[path] = store
	return store
}

func newSessionStore(path string) *sessionStore {
	store := &sessionStore{
		path:           strings.TrimSpace(path),
		jobs:           map[string]*sessionJob{},
		droppedChatIDs: map[string]struct{}{},
	}
	if store.path == "" {
		return store
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(store.path), legacyChatCutoverReceiptFilename)); err == nil {
		store.cutoverReceiptPresent = true
	}
	droppedChatIDs, err := loadDroppedChatIDs(filepath.Dir(store.path))
	if err != nil {
		store.loadErr = err
		return store
	}
	store.droppedChatIDs = droppedChatIDs
	raw, err := os.ReadFile(store.path)
	canonicalExists := err == nil
	internalChatsPruned := false
	if err != nil && !os.IsNotExist(err) {
		store.loadErr = err
		return store
	}
	if canonicalExists {
		var snapshot map[string]any
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&snapshot); err != nil {
			// Never replay a journal over an unreadable canonical history. Keeping
			// the sidecar intact lets an operator repair the snapshot and retry.
			store.loadErr = err
			return store
		}
		// Establish the store invariant once at the disk boundary: every value
		// held in the authoritative in-memory mirror is already redacted.
		store.snapshot = mapFromAnyMain(redactSessionValue(snapshot))
		migrateLegacySteerChronology(store.snapshot)
		migrateModelControlKeys(store.snapshot)
		internalChatsPruned = store.pruneInternalChatsLocked(store.snapshot)
	}
	if store.cutoverReceiptPresent {
		// The receipt is verified by providerChatRuntime after it discovers actor
		// files. Until then this store is a read-only migration-era cache. Do not
		// replay journals, rewrite chat rows, or garbage-collect actor-owned image
		// sidecars based on this obsolete projection.
		if store.snapshot == nil {
			store.snapshot = map[string]any{"v": json.Number("1"), "activeId": nil, "seq": json.Number("0"), "chats": []any{}}
		}
		store.published.Store(newSessionGeneration(store.snapshot))
		return store
	}
	tombstonesPruned := store.pruneTombstonedChatsLocked(store.snapshot)
	assistantImagesMigrated := migrateAssistantMarkdownImages(store.snapshot)

	recovered, quarantined, replayErr := store.replaySessionJournalsLocked()
	if replayErr != nil {
		store.loadErr = errors.Join(store.loadErr, replayErr)
	}
	if quarantined > 0 {
		store.loadErr = errors.Join(store.loadErr, &sessionJournalQuarantineError{Count: quarantined})
	}
	planLatestMigrated := migratePlanLatest(store.snapshot, filepath.Dir(store.path))
	agentQueueEchoesMigrated := migrateAgentQueueEchoes(store.snapshot)
	store.materializeLiveOutputsLocked()
	orphaned := store.interruptOrphanedTurnsLocked()
	store.finalizeRecoveredOrphansLocked(recovered)
	terminalToolsMigrated := migrateTerminalToolEvents(store.snapshot)
	// Missing image sidecars are recoverable transcript damage, not a reason to
	// discard the entire mirror. Drop only the unreadable payload refs before
	// strict normalization and retain their diagnostics in loadErr.
	imageRefWarning := dropUnreadableExternalSessionImageRefs(store.snapshot, filepath.Dir(store.path))
	eventRefWarning := dropOutOfRangeSessionEventImageRefs(store.snapshot)
	store.loadErr = errors.Join(store.loadErr, imageRefWarning, eventRefWarning)
	// Disk snapshots from current builds are already ref-native. Older inline
	// snapshots, recovered journals, and assistant-image migrations can still
	// introduce bytes at boot, so externalize those once before the mirror is
	// published. Image files become durable before any canonical rewrite that
	// references them.
	normalizeErr := makeSessionSnapshotRefNative(store.snapshot, filepath.Dir(store.path))
	var normalized []byte
	if normalizeErr == nil {
		normalizeErr = validateRefNativeSessionImages(store.snapshot, filepath.Dir(store.path))
	}
	if normalizeErr == nil {
		normalized, normalizeErr = json.Marshal(store.snapshot)
	}
	needsRewrite := canonicalExists && normalizeErr == nil && !bytes.Equal(bytes.TrimSpace(raw), normalized)
	if normalizeErr != nil {
		store.loadErr = normalizeErr
		return store
	}
	if len(recovered) > 0 || orphaned || needsRewrite || tombstonesPruned || internalChatsPruned || assistantImagesMigrated || terminalToolsMigrated || planLatestMigrated || agentQueueEchoesMigrated {
		store.ensureSnapshotLocked()
		if err := store.writeLocked(); err != nil {
			store.loadErr = err
			return store
		}
	}
	for _, journal := range recovered {
		if err := store.archiveRecoveredJournalLocked(journal); err != nil {
			store.loadErr = errors.Join(store.loadErr, err)
			continue
		}
		if err := store.removeJournalLocked(journal.turnID, journal.path); err != nil {
			store.loadErr = errors.Join(store.loadErr, err)
		}
	}
	// No provider process survives a daemon restart. Journal state has now been
	// folded into canonical failed/done messages, so runtime-only job anchors are
	// discarded even when cleanup must be retried on the next startup.
	store.jobs = map[string]*sessionJob{}
	store.pending = nil
	store.jobOrder = nil
	if store.snapshot != nil {
		store.published.Store(newSessionGeneration(store.snapshot))
	}
	if removed, err := sweepOrphanedSessionImages(
		filepath.Dir(store.path), store.snapshot, time.Now(), sessionImageGCGrace,
	); err != nil {
		store.loadErr = errors.Join(store.loadErr, err)
	} else if removed > 0 {
		sessionStoreLogLine(fmt.Sprintf("session image gc removed=%d grace=%s", removed, sessionImageGCGrace))
	}
	return store
}

func loadDroppedChatIDs(stateDir string) (map[string]struct{}, error) {
	out := map[string]struct{}{}
	entries, err := os.ReadDir(filepath.Join(stateDir, droppedChatDirname))
	if os.IsNotExist(err) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != ".json" {
			continue
		}
		tabID := strings.TrimSuffix(name, ".json")
		if tabID == "" || strings.ContainsAny(tabID, `/\`) {
			continue
		}
		out[tabID] = struct{}{}
	}
	return out, nil
}

func (s *sessionStore) pruneTombstonedChatsLocked(snapshot map[string]any) bool {
	if snapshot == nil || len(s.droppedChatIDs) == 0 {
		return false
	}
	chats := anySlice(snapshot["chats"])
	kept := make([]any, 0, len(chats))
	changed := false
	for _, raw := range chats {
		tabID := fieldString(mapFromAnyMain(raw), "id")
		if _, deleted := s.droppedChatIDs[tabID]; deleted {
			changed = true
			continue
		}
		kept = append(kept, raw)
	}
	if !changed {
		return false
	}
	snapshot["chats"] = kept
	if activeID := fieldString(snapshot, "activeId"); activeID != "" {
		if _, deleted := s.droppedChatIDs[activeID]; deleted {
			snapshot["activeId"] = nil
			if len(kept) > 0 {
				snapshot["activeId"] = fieldString(mapFromAnyMain(kept[0]), "id")
			}
		}
	}
	return true
}

// Older Workass builds archived ordinary ACP-authored image Markdown as prose
// only. Recover the most recent safe workspace-local raster references once at
// load so the fix also repairs currently visible broken rows. The global byte
// cap keeps startup bounded even if a pathological archive contains many links.
func migrateAssistantMarkdownImages(snapshot map[string]any) bool {
	if snapshot == nil {
		return false
	}
	const migrationByteCap = 32 * 1024 * 1024
	remaining := migrationByteCap
	changed := false
	for _, rawChat := range anySlice(snapshot["chats"]) {
		chat := mapFromAnyMain(rawChat)
		cwd := fieldString(chat, "cwd")
		if cwd == "" {
			continue
		}
		messages := messageSlice(chat)
		for index := len(messages) - 1; index >= 0 && remaining > 0; index-- {
			message := mapFromAnyMain(messages[index])
			if fieldString(message, "role") != "assistant" || len(anySlice(message["images"])) > 0 {
				continue
			}
			markdown := strings.TrimSpace(stringValue(message["content"]) + "\n\n" + stringValue(message["result"]))
			if !strings.Contains(markdown, "![") {
				continue
			}
			images := acp.ResolveAssistantMarkdownImages(markdown, cwd)
			encodedBytes := 0
			for _, rawImage := range images {
				encodedBytes += len(fieldString(mapFromAnyMain(rawImage), "data"))
			}
			if len(images) == 0 || encodedBytes <= 0 || encodedBytes > remaining {
				continue
			}
			message["images"] = cloneJSON(images)
			remaining -= encodedBytes
			changed = true
		}
	}
	return changed
}

func terminalToolStatus(turnStatus string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(turnStatus)) {
	case "done", "completed", "success":
		return "completed", true
	case "failed", "error":
		return "failed", true
	case "cancelled", "canceled":
		return "cancelled", true
	default:
		return "", false
	}
}

func settleTerminalToolEvents(message map[string]any, turnStatus string) bool {
	status, terminal := terminalToolStatus(turnStatus)
	if message == nil || !terminal {
		return false
	}
	changed := false
	for _, raw := range anySlice(message["events"]) {
		event := mapFromAnyMain(raw)
		if fieldString(event, "kind") != "tool" {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(fieldString(event, "status"))) {
		case "", "in_progress", "pending", "running":
			event["status"] = status
			changed = true
		}
	}
	return changed
}

// A terminal foreground turn is authoritative evidence that none of its tool
// calls can still be running. Older builds persisted the assistant's terminal
// status without settling an omitted tool_call_update, leaving a fake live row
// after reload. Normalize those rows once at the canonical state boundary.
func migrateTerminalToolEvents(snapshot map[string]any) bool {
	if snapshot == nil {
		return false
	}
	changed := false
	for _, rawChat := range anySlice(snapshot["chats"]) {
		for _, rawMessage := range messageSlice(mapFromAnyMain(rawChat)) {
			message := mapFromAnyMain(rawMessage)
			if fieldString(message, "role") == "assistant" && settleTerminalToolEvents(message, fieldString(message, "status")) {
				changed = true
			}
		}
	}
	return changed
}

func normalizedPlanLatestEntries(raw any) []any {
	entries, _ := raw.([]any)
	out := make([]any, 0, len(entries))
	for _, rawEntry := range entries {
		entry := mapFromAnyMain(rawEntry)
		out = append(out, map[string]any{
			"status":  fieldString(entry, "status"),
			"content": fieldString(entry, "content"),
		})
	}
	return out
}

func planLatestAllCompleted(raw any) bool {
	entries, ok := raw.([]any)
	if !ok || len(entries) == 0 {
		return false
	}
	for _, rawEntry := range entries {
		if fieldString(mapFromAnyMain(rawEntry), "status") != "completed" {
			return false
		}
	}
	return true
}

func applyChatPlanLatest(chat, assistant, event map[string]any) {
	if chat == nil || assistant == nil || fieldString(event, "kind") != "plan" {
		return
	}
	chat["planLatest"] = normalizedPlanLatestEntries(event["entries"])
	chat["planLatestMessageId"] = fieldString(assistant, "id")
}

func latestPlanFromMessages(messages []any) (entries []any, ownerID, laterAssistantID string, found bool) {
	latestAssistantID := ""
	for index := len(messages) - 1; index >= 0; index-- {
		message := mapFromAnyMain(messages[index])
		if fieldString(message, "role") != "assistant" {
			continue
		}
		messageID := fieldString(message, "id")
		if latestAssistantID == "" {
			latestAssistantID = messageID
		}
		events := anySlice(message["events"])
		for eventIndex := len(events) - 1; eventIndex >= 0; eventIndex-- {
			event := mapFromAnyMain(events[eventIndex])
			if fieldString(event, "kind") != "plan" {
				continue
			}
			laterID := latestAssistantID
			if laterID == messageID {
				laterID = ""
			}
			return normalizedPlanLatestEntries(event["entries"]), messageID, laterID, true
		}
	}
	return nil, "", "", false
}

func latestAssistantAfterArchiveOwner(messages []any, ownerID string) string {
	ownerIndex := -1
	latestID := ""
	for index, raw := range messages {
		message := mapFromAnyMain(raw)
		if fieldString(message, "role") != "assistant" {
			continue
		}
		messageID := fieldString(message, "id")
		if messageID == ownerID {
			ownerIndex = index
			latestID = ""
			continue
		}
		if ownerIndex < 0 || index > ownerIndex {
			latestID = messageID
		}
	}
	return latestID
}

// Older snapshots only retained plans on assistant events. Reconstruct the
// daemon-owned chat summary after journal replay so session:get can hydrate it
// even when that assistant has aged out of the renderer's retained window.
func migratePlanLatest(snapshot map[string]any, stateDir string) bool {
	if snapshot == nil {
		return false
	}
	changed := false
	for _, rawChat := range anySlice(snapshot["chats"]) {
		chat := mapFromAnyMain(rawChat)
		if _, ok := chat["planLatest"].([]any); ok {
			continue
		}
		messages := messageSlice(chat)
		entries, ownerID, laterID, found := latestPlanFromMessages(messages)
		if !found {
			archive := loadChatArchive(stateDir, fieldString(chat, "id"))
			entries, ownerID, laterID, found = latestPlanFromMessages(archive)
			if found && laterID == "" {
				laterID = latestAssistantAfterArchiveOwner(messages, ownerID)
			}
		}
		if !found {
			continue
		}
		if planLatestAllCompleted(entries) && laterID != "" {
			entries = []any{}
			ownerID = laterID
		}
		chat["planLatest"] = entries
		chat["planLatestMessageId"] = ownerID
		changed = true
	}
	return changed
}

func (s *sessionStore) enabled() bool { return s != nil && s.path != "" }

func (s *sessionStore) LoadError() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadErr
}

func migrateModelControlKeys(snapshot map[string]any) bool {
	if snapshot == nil {
		return false
	}
	changed := false
	for _, rawChat := range anySlice(snapshot["chats"]) {
		chat := mapFromAnyMain(rawChat)
		memory, ok := chat["modelControls"].(map[string]any)
		if !ok || len(memory) == 0 {
			continue
		}
		providerIDs := make([]string, 0, len(memory))
		for providerID := range memory {
			providerIDs = append(providerIDs, providerID)
		}
		sort.Strings(providerIDs)
		for _, providerID := range providerIDs {
			providerMemory, ok := memory[providerID].(map[string]any)
			if !ok || len(providerMemory) == 0 {
				continue
			}
			modelIDs := make([]string, 0, len(providerMemory))
			for modelID := range providerMemory {
				modelIDs = append(modelIDs, modelID)
			}
			sort.Strings(modelIDs)
			for _, modelID := range modelIDs {
				baseModelID, _, split := acp.SplitCanonicalEffortSuffix(modelID)
				if !split || baseModelID == "" || baseModelID == modelID {
					continue
				}
				if _, exists := providerMemory[baseModelID]; !exists {
					providerMemory[baseModelID] = providerMemory[modelID]
				}
				delete(providerMemory, modelID)
				sessionStoreLogLine(fmt.Sprintf(
					"session store migrated model control key provider=%s from=%s to=%s",
					redactedSessionString(providerID), redactedSessionString(modelID), redactedSessionString(baseModelID),
				))
				changed = true
			}
		}
	}
	return changed
}

func modelControlBaseKey(modelID string) string {
	baseModelID, _, split := acp.SplitCanonicalEffortSuffix(modelID)
	if split && baseModelID != "" {
		return baseModelID
	}
	return strings.TrimSpace(modelID)
}

func (s *sessionStore) Get() any {
	if !s.enabled() {
		return nil
	}
	generation := s.publishedGeneration()
	if generation == nil || generation.root == nil {
		return nil
	}
	// The store invariant is redacted at the disk/input boundary. The atomic load
	// is the read linearization point and the immutable root is cloned outside
	// the store mutex.
	data, err := json.Marshal(generation.root)
	if err != nil {
		s.recordLoadError(err)
		return nil
	}
	var out map[string]any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		s.recordLoadError(err)
		return nil
	}
	if err := materializeSessionSnapshotForWire(out, filepath.Dir(s.path)); err != nil {
		s.recordLoadError(err)
	}
	return out
}

var actorGlobalSessionFields = []string{
	"v", "activeId", "seq", "workspaces", "collapsedWorkspaces", "removedWorkspaces",
	"theme", "themePref", "density", "panes", "mode", "notifEnabled", globalPresentationRevisionField,
}

func actorGlobalSessionSnapshot(source map[string]any) map[string]any {
	out := map[string]any{
		"v":        json.Number("1"),
		"activeId": nil,
		"seq":      json.Number("0"),
		"chats":    []any{},
	}
	for _, key := range actorGlobalSessionFields {
		if value, exists := source[key]; exists {
			out[key] = cloneJSON(value)
		}
	}
	return mapFromAnyMain(redactSessionValue(out))
}

// GlobalSnapshot exposes only daemon-wide presentation preferences after
// actor cutover. It cannot return, merge, or recover a chat row.
func (s *sessionStore) GlobalSnapshot() map[string]any {
	if s == nil || !s.enabled() {
		return actorGlobalSessionSnapshot(nil)
	}
	generation := s.publishedGeneration()
	if generation == nil || generation.root == nil {
		return actorGlobalSessionSnapshot(nil)
	}
	return actorGlobalSessionSnapshot(generation.root)
}

// ActivateActorCutover is called only after the global receipt and every
// referenced actor have been verified. It durably removes legacy chat rows
// from session-state.json; from this point the store persists daemon-global UI
// preferences only and cannot act as a transcript/session fallback.
func (s *sessionStore) ActivateActorCutover() error {
	if s == nil || !s.enabled() {
		return errors.New("session presentation store is unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.actorCutover {
		return nil
	}
	root := actorGlobalSessionSnapshot(s.snapshot)
	data, err := json.Marshal(root)
	if err != nil {
		return err
	}
	seq := s.persistSeq + 1
	if err := s.persistSnapshot(seq, data, nil); err != nil {
		return err
	}
	s.persistSeq = seq
	s.snapshot = root
	s.published.Store(newSessionGeneration(root))
	s.jobs = map[string]*sessionJob{}
	s.pending = nil
	s.jobOrder = nil
	s.droppedChatIDs = map[string]struct{}{}
	s.loadErr = nil
	s.actorCutover = true
	return nil
}

// SaveActorGlobalSnapshot persists only the root-level renderer preferences.
// Chat rows in the submitted frozen mirror are command inputs already applied
// to actors by providerChatRuntime and are deliberately discarded here.
func (s *sessionStore) SaveActorGlobalSnapshot(raw any) (uint64, error) {
	if s == nil || !s.enabled() {
		return 0, errors.New("session presentation store is unavailable")
	}
	incoming := mapFromAnyMain(redactSessionValue(raw))
	if incoming == nil {
		return 0, errors.New("renderer session snapshot is not an object")
	}
	operationID := providercontract.NormalizeOperationID(fieldString(incoming, globalPresentationOperationField))
	if operationID == "" {
		return 0, errors.New("global presentation save requires a stable operation id")
	}
	expectedRevision := uint64(max(0, intValue(incoming[globalPresentationRevisionField])))
	root := actorGlobalSessionSnapshot(incoming)
	digest := globalPresentationDigest(root)
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.actorCutover {
		return 0, errors.New("actor cutover is not committed")
	}
	currentRevision := uint64(max(0, intValue(s.snapshot[globalPresentationRevisionField])))
	receipts := mapFromAnyMain(cloneJSON(s.snapshot[globalPresentationReceiptsField]))
	if existing := mapFromAnyMain(receipts[string(operationID)]); len(existing) > 0 {
		if fieldString(existing, "digest") != digest {
			return 0, errors.New("global presentation operation id was reused for different content")
		}
		return uint64(max(0, intValue(existing["revision"]))), nil
	}
	if expectedRevision != currentRevision {
		return 0, errors.New("daemon-global presentation changed in another controller; reload before saving")
	}
	if digest != globalPresentationDigest(actorGlobalSessionSnapshot(s.snapshot)) {
		currentRevision++
	}
	root[globalPresentationRevisionField] = json.Number(fmt.Sprint(currentRevision))
	receipts[string(operationID)] = map[string]any{"digest": digest, "revision": json.Number(fmt.Sprint(currentRevision))}
	root[globalPresentationReceiptsField] = receipts
	data, err := json.Marshal(root)
	if err != nil {
		return 0, err
	}
	seq := s.persistSeq + 1
	if err := s.persistSnapshot(seq, data, nil); err != nil {
		return 0, err
	}
	s.persistSeq = seq
	s.snapshot = root
	s.published.Store(newSessionGeneration(root))
	return currentRevision, nil
}

func globalPresentationDigest(root map[string]any) string {
	value := actorGlobalSessionSnapshot(root)
	delete(value, globalPresentationRevisionField)
	raw, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}

func (s *sessionStore) recordLoadError(err error) {
	if s == nil || err == nil {
		return
	}
	s.mu.Lock()
	s.loadErr = errors.Join(s.loadErr, err)
	s.mu.Unlock()
}

func nullableDigestString(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

// SaveGlobalActiveTab is the single actor-cutover writer for the daemon-wide
// focus field. It uses the same stable operation/CAS receipt as every other
// global presentation mutation; there is no second last-write-wins path.
func (s *sessionStore) SaveGlobalActiveTab(tabID string, operationID providercontract.OperationID) (uint64, error) {
	operationID = providercontract.NormalizeOperationID(string(operationID))
	if operationID == "" {
		return 0, errors.New("global focus change requires a stable operation id")
	}
	snapshot := s.GlobalSnapshot()
	if strings.TrimSpace(tabID) == "" {
		snapshot["activeId"] = nil
	} else {
		snapshot["activeId"] = strings.TrimSpace(tabID)
	}
	snapshot[globalPresentationOperationField] = string(operationID)
	return s.SaveActorGlobalSnapshot(snapshot)
}

func (s *sessionStore) beginLiveSteerLocked(chat map[string]any, job *sessionJob, clientUserMessageID, prompt, continuationAssistantMessageID string, images []any, boundary map[string]any) error {
	if chat == nil || job == nil {
		return errors.New("steer chronology requires an active chat turn")
	}
	for _, raw := range messageSlice(chat) {
		message := mapFromAnyMain(raw)
		if fieldString(message, "id") != clientUserMessageID {
			continue
		}
		if fieldString(message, "role") != "user" {
			return errors.New("client user message id belongs to a non-user row")
		}
		return nil
	}
	if deferred, _ := boundary["deferUntilConsumed"].(bool); deferred {
		return s.stageLiveSteerLocked(chat, job, clientUserMessageID, prompt, continuationAssistantMessageID, images, boundary)
	}
	assistant := s.messageForJobLocked(job)
	if assistant == nil || fieldString(assistant, "role") != "assistant" {
		return errors.New("active assistant continuation is unavailable")
	}
	materializeSessionJobOutput(job, assistant)
	if target := fieldString(boundary, "assistantMessageId"); target != "" && target != fieldString(assistant, "id") {
		return errors.New("steer boundary no longer matches the active continuation")
	}
	content := stringValue(assistant["content"])
	result := stringValue(assistant["result"])
	contentOffset := utf16CodeUnits(content)
	if _, ok := boundary["contentOffset"]; ok {
		contentOffset = intValue(boundary["contentOffset"])
	}
	resultOffset := utf16CodeUnits(result)
	if _, ok := boundary["resultOffset"]; ok {
		resultOffset = intValue(boundary["resultOffset"])
	}
	contentHead, contentTail := splitUTF16Units(content, contentOffset)
	resultHead, resultTail := splitUTF16Units(result, resultOffset)
	events := anySlice(assistant["events"])
	eventCount := len(events)
	if _, ok := boundary["eventCount"]; ok {
		eventCount = min(max(intValue(boundary["eventCount"]), 0), len(events))
	}
	eventHead := cloneJSON(events[:eventCount])
	eventTail := shiftSessionEvents(cloneJSON(events[eventCount:]), -utf16CodeUnits(contentHead))

	rootID := firstNonEmptyString(job.RootAssistantID, fieldString(assistant, "turnRootId"), fieldString(assistant, "id"))
	job.RootAssistantID = rootID
	steer := newSessionMessage("user", prompt, "pending", time.Now().UTC().Format(time.RFC3339Nano))
	steer["id"] = clientUserMessageID
	steer["steerState"] = "sending"
	steer["turnRootId"] = rootID
	if len(images) > 0 {
		steer["images"] = cloneJSON(images)
	}
	continuation := newSessionMessage("assistant", contentTail, "running", nil)
	continuation["id"] = continuationAssistantMessageID
	continuation["jobId"] = job.ID
	continuation["turnRootId"] = rootID
	continuation["turnTerminal"] = true
	continuation["events"] = eventTail
	if resultTail != "" {
		continuation["result"] = resultTail
	}
	for _, key := range []string{"permission", "interrupted", "retryPrompt", "turnStartedAt"} {
		if value, ok := assistant[key]; ok {
			continuation[key] = cloneJSON(value)
		}
	}

	messages := messageSlice(chat)
	assistantIndex := -1
	for index, raw := range messages {
		if fieldString(mapFromAnyMain(raw), "id") == fieldString(assistant, "id") {
			assistantIndex = index
			break
		}
	}
	if assistantIndex < 0 {
		return errors.New("active assistant is missing from transcript order")
	}
	keepHead := contentHead != "" || resultHead != "" || len(anySlice(eventHead)) > 0 || fieldString(assistant, "id") == rootID
	if keepHead {
		assistant["content"] = contentHead
		assistant["events"] = eventHead
		assistant["status"] = "done"
		assistant["at"] = nil
		assistant["turnRootId"] = rootID
		assistant["turnTerminal"] = false
		if resultHead != "" {
			assistant["result"] = resultHead
		} else {
			delete(assistant, "result")
		}
		for _, key := range []string{"permission", "interrupted", "retryPrompt"} {
			delete(assistant, key)
		}
		messages = insertSessionMessages(messages, assistantIndex+1, 0, steer, continuation)
	} else {
		messages = insertSessionMessages(messages, assistantIndex, 1, steer, continuation)
	}
	chat["messages"] = messages
	job.contentSegmentStart += len(contentHead)
	job.finalSegmentStart += len(resultHead)
	job.AssistantID = continuationAssistantMessageID
	return nil
}

func (s *sessionStore) stageLiveSteerLocked(chat map[string]any, job *sessionJob, clientUserMessageID, prompt, continuationAssistantMessageID string, images []any, boundary map[string]any) error {
	assistant := s.messageForJobLocked(job)
	if assistant == nil || fieldString(assistant, "role") != "assistant" {
		return errors.New("active assistant continuation is unavailable")
	}
	materializeSessionJobOutput(job, assistant)
	if target := fieldString(boundary, "assistantMessageId"); target != "" && target != fieldString(assistant, "id") {
		return errors.New("steer boundary no longer matches the active continuation")
	}
	rootID := firstNonEmptyString(job.RootAssistantID, fieldString(assistant, "turnRootId"), fieldString(assistant, "id"))
	job.RootAssistantID = rootID
	steer := newSessionMessage("user", prompt, "pending", time.Now().UTC().Format(time.RFC3339Nano))
	steer["id"] = clientUserMessageID
	steer["steerState"] = "sending"
	steer["turnRootId"] = rootID
	steer["steerBoundary"] = "waiting"
	steer["steerContinuationId"] = continuationAssistantMessageID
	if len(images) > 0 {
		steer["images"] = cloneJSON(images)
	}
	continuation := newSessionMessage("assistant", "", "pending", nil)
	continuation["id"] = continuationAssistantMessageID
	continuation["jobId"] = job.ID
	continuation["turnRootId"] = rootID
	continuation["turnTerminal"] = true
	continuation["steerBoundary"] = "waiting"
	continuation["steerContinuationFor"] = clientUserMessageID
	if value, ok := assistant["turnStartedAt"]; ok {
		continuation["turnStartedAt"] = cloneJSON(value)
	}

	messages := messageSlice(chat)
	insertAt := -1
	for index, raw := range messages {
		if fieldString(mapFromAnyMain(raw), "id") == fieldString(assistant, "id") {
			insertAt = index + 1
			break
		}
	}
	if insertAt < 0 {
		return errors.New("active assistant is missing from transcript order")
	}
	for insertAt < len(messages) {
		candidate := mapFromAnyMain(messages[insertAt])
		if fieldString(candidate, "steerBoundary") != "waiting" || fieldString(candidate, "turnRootId") != rootID {
			break
		}
		insertAt++
	}
	chat["messages"] = insertSessionMessages(messages, insertAt, 0, steer, continuation)
	return nil
}

func sessionAssistantEmpty(message map[string]any) bool {
	return fieldString(message, "role") == "assistant" &&
		stringValue(message["content"]) == "" && stringValue(message["result"]) == "" &&
		len(anySlice(message["events"])) == 0 && message["permission"] == nil
}

func (s *sessionStore) commitOneStagedSteerLocked(chat map[string]any, job *sessionJob, clientUserMessageID string) bool {
	if chat == nil || job == nil {
		return false
	}
	messages := messageSlice(chat)
	steerIndex := -1
	var steer map[string]any
	for index, raw := range messages {
		candidate := mapFromAnyMain(raw)
		if fieldString(candidate, "id") == clientUserMessageID && fieldString(candidate, "role") == "user" && fieldString(candidate, "steerBoundary") == "waiting" {
			steerIndex, steer = index, candidate
			break
		}
	}
	if steerIndex < 0 {
		return false
	}
	continuationID := fieldString(steer, "steerContinuationId")
	continuationIndex := -1
	var continuation map[string]any
	for index, raw := range messages {
		candidate := mapFromAnyMain(raw)
		if fieldString(candidate, "id") == continuationID && fieldString(candidate, "role") == "assistant" && fieldString(candidate, "steerBoundary") == "waiting" && fieldString(candidate, "steerContinuationFor") == clientUserMessageID {
			continuationIndex, continuation = index, candidate
			break
		}
	}
	if continuationIndex < 0 {
		return false
	}
	active := s.messageForJobLocked(job)
	if active == nil || fieldString(active, "role") != "assistant" {
		return false
	}
	materializeSessionJobOutput(job, active)
	rootID := firstNonEmptyString(fieldString(steer, "turnRootId"), job.RootAssistantID, fieldString(active, "turnRootId"), fieldString(active, "id"))
	activeID := fieldString(active, "id")
	activeIndex := -1
	for index, raw := range messages {
		if fieldString(mapFromAnyMain(raw), "id") == activeID {
			activeIndex = index
			break
		}
	}
	if activeIndex < 0 || activeIndex >= steerIndex {
		return false
	}
	if activeID != rootID && fieldString(active, "turnRootId") == rootID && sessionAssistantEmpty(active) {
		messages = insertSessionMessages(messages, activeIndex, 1)
	} else {
		active["status"] = "done"
		active["at"] = nil
		active["turnRootId"] = rootID
		active["turnTerminal"] = false
		for _, key := range []string{"permission", "interrupted", "retryPrompt"} {
			delete(active, key)
		}
	}
	delete(steer, "steerBoundary")
	delete(steer, "steerContinuationId")
	continuation["status"] = "running"
	continuation["jobId"] = job.ID
	continuation["turnRootId"] = rootID
	continuation["turnTerminal"] = true
	delete(continuation, "steerBoundary")
	delete(continuation, "steerContinuationFor")
	chat["messages"] = messages
	job.RootAssistantID = rootID
	job.AssistantID = continuationID
	job.contentSegmentStart = job.contentOutput.Len()
	job.finalSegmentStart = job.finalOutput.Len()
	return true
}

func (s *sessionStore) commitStagedSteerLocked(chat map[string]any, job *sessionJob, clientUserMessageID string) bool {
	if chat == nil || job == nil || strings.TrimSpace(clientUserMessageID) == "" {
		return false
	}
	var target map[string]any
	for _, raw := range messageSlice(chat) {
		message := mapFromAnyMain(raw)
		if fieldString(message, "id") == clientUserMessageID && fieldString(message, "role") == "user" && fieldString(message, "steerBoundary") == "waiting" {
			target = message
			break
		}
	}
	if target == nil {
		return false
	}
	rootID := fieldString(target, "turnRootId")
	waitingIDs := make([]string, 0, 2)
	for _, raw := range messageSlice(chat) {
		message := mapFromAnyMain(raw)
		if fieldString(message, "role") == "user" && fieldString(message, "steerBoundary") == "waiting" && fieldString(message, "turnRootId") == rootID {
			waitingIDs = append(waitingIDs, fieldString(message, "id"))
		}
		if fieldString(message, "id") == clientUserMessageID {
			break
		}
	}
	committed := false
	for _, id := range waitingIDs {
		if !s.commitOneStagedSteerLocked(chat, job, id) {
			return false
		}
		if id == clientUserMessageID {
			committed = true
		}
	}
	return committed
}

// CommitLiveSteerBoundary is used only for adapters that acknowledge native
// delivery but cannot emit a canonical client-id receipt.
func insertSessionMessages(messages []any, index, remove int, items ...any) []any {
	index = min(max(index, 0), len(messages))
	remove = min(max(remove, 0), len(messages)-index)
	out := make([]any, 0, len(messages)-remove+len(items))
	out = append(out, messages[:index]...)
	out = append(out, items...)
	out = append(out, messages[index+remove:]...)
	return out
}

func shiftSessionEvents(raw any, delta int) any {
	events := anySlice(raw)
	for _, item := range events {
		event := mapFromAnyMain(item)
		event["at"] = max(0, intValue(event["at"])+delta)
	}
	return events
}

func utf16CodeUnits(value string) int { return len(utf16.Encode([]rune(value))) }

func splitUTF16Units(value string, units int) (string, string) {
	if units <= 0 {
		return "", value
	}
	limit := utf16CodeUnits(value)
	if units >= limit {
		return value, ""
	}
	seen := 0
	for byteIndex, r := range value {
		next := seen + len(utf16.Encode([]rune{r}))
		if next > units {
			return value[:byteIndex], value[byteIndex:]
		}
		seen = next
		if seen == units {
			end := byteIndex + len(string(r))
			return value[:end], value[end:]
		}
	}
	return value, ""
}

func sliceUTF16Units(value string, start, end int) string {
	limit := utf16CodeUnits(value)
	start = min(max(start, 0), limit)
	end = min(max(end, start), limit)
	_, tail := splitUTF16Units(value, start)
	head, _ := splitUTF16Units(tail, end-start)
	return head
}

func legacySteerSegment(owner map[string]any, id, rootID string, contentStart, contentEnd, resultStart, resultEnd, eventStart, eventEnd int, terminal bool) map[string]any {
	segment := mapFromAnyMain(cloneJSON(owner))
	content := stringValue(owner["content"])
	result := stringValue(owner["result"])
	events := anySlice(owner["events"])
	contentStart = min(max(contentStart, 0), utf16CodeUnits(content))
	contentEnd = min(max(contentEnd, contentStart), utf16CodeUnits(content))
	resultStart = min(max(resultStart, 0), utf16CodeUnits(result))
	resultEnd = min(max(resultEnd, resultStart), utf16CodeUnits(result))
	eventStart = min(max(eventStart, 0), len(events))
	eventEnd = min(max(eventEnd, eventStart), len(events))

	segment["id"] = id
	segment["content"] = sliceUTF16Units(content, contentStart, contentEnd)
	resultSlice := sliceUTF16Units(result, resultStart, resultEnd)
	if resultSlice != "" {
		segment["result"] = resultSlice
	} else {
		delete(segment, "result")
	}
	segment["events"] = shiftSessionEvents(cloneJSON(events[eventStart:eventEnd]), -contentStart)
	segment["turnRootId"] = rootID
	segment["turnTerminal"] = terminal
	delete(segment, "steerAnchor")
	if !terminal {
		segment["status"] = "done"
		segment["at"] = nil
		delete(segment, "permission")
		delete(segment, "interrupted")
		delete(segment, "retryPrompt")
	}
	return segment
}

func emptyAssistantSegment(message map[string]any) bool {
	return fieldString(message, "role") == "assistant" &&
		stringValue(message["content"]) == "" &&
		stringValue(message["result"]) == "" &&
		len(anySlice(message["events"])) == 0 && message["permission"] == nil
}

// migrateLegacySteerChronology permanently repairs snapshots written by the
// offset-anchor implementation. The returned array contains real chronological
// assistant-prefix/user/assistant-tail rows, and no steerAnchor survives.
func migrateLegacySteerChronology(snapshot map[string]any) bool {
	changed := false
	for _, rawChat := range anySlice(snapshot["chats"]) {
		chat := mapFromAnyMain(rawChat)
		messages := messageSlice(chat)
		assistants := make(map[string]bool)
		for _, raw := range messages {
			message := mapFromAnyMain(raw)
			if fieldString(message, "role") == "assistant" && fieldString(message, "id") != "" {
				assistants[fieldString(message, "id")] = true
			}
		}
		steersByAssistant := make(map[string][]map[string]any)
		legacySteerIDs := make(map[string]bool)
		for _, raw := range messages {
			message := mapFromAnyMain(raw)
			target := fieldString(mapFromAnyMain(message["steerAnchor"]), "assistantMessageId")
			if fieldString(message, "role") != "user" || target == "" || !assistants[target] {
				continue
			}
			steersByAssistant[target] = append(steersByAssistant[target], message)
			legacySteerIDs[fieldString(message, "id")] = true
		}
		if len(legacySteerIDs) == 0 {
			continue
		}

		out := make([]any, 0, len(messages)+len(legacySteerIDs))
		for _, raw := range messages {
			message := mapFromAnyMain(raw)
			if legacySteerIDs[fieldString(message, "id")] {
				continue
			}
			steers := steersByAssistant[fieldString(message, "id")]
			if fieldString(message, "role") != "assistant" || len(steers) == 0 {
				out = append(out, message)
				continue
			}

			rootID := firstNonEmptyString(fieldString(message, "turnRootId"), fieldString(message, "id"))
			content := stringValue(message["content"])
			result := stringValue(message["result"])
			events := anySlice(message["events"])
			contentStart, resultStart, eventStart := 0, 0, 0
			priorSteerID := ""
			for _, steer := range steers {
				anchor := mapFromAnyMain(steer["steerAnchor"])
				contentEnd := min(max(intValue(anchor["contentOffset"]), contentStart), utf16CodeUnits(content))
				resultEnd := min(max(intValue(anchor["resultOffset"]), resultStart), utf16CodeUnits(result))
				eventEnd := min(max(intValue(anchor["eventCount"]), eventStart), len(events))
				segmentID := fieldString(message, "id")
				if priorSteerID != "" {
					segmentID = rootID + "~after~" + priorSteerID
				}
				segment := legacySteerSegment(message, segmentID, rootID, contentStart, contentEnd, resultStart, resultEnd, eventStart, eventEnd, false)
				if !emptyAssistantSegment(segment) {
					out = append(out, segment)
				}
				migratedSteer := mapFromAnyMain(cloneJSON(steer))
				migratedSteer["turnRootId"] = rootID
				delete(migratedSteer, "steerAnchor")
				out = append(out, migratedSteer)
				contentStart, resultStart, eventStart = contentEnd, resultEnd, eventEnd
				priorSteerID = fieldString(steer, "id")
			}
			terminal := true
			if value, ok := message["turnTerminal"].(bool); ok {
				terminal = value
			}
			out = append(out, legacySteerSegment(
				message, rootID+"~after~"+priorSteerID, rootID,
				contentStart, utf16CodeUnits(content), resultStart, utf16CodeUnits(result),
				eventStart, len(events), terminal,
			))
		}
		chat["messages"] = out
		changed = true
	}
	return changed
}

func (s *sessionStore) rejectLiveSteerLocked(chat map[string]any, job *sessionJob, clientUserMessageID string) error {
	messages := messageSlice(chat)
	index := -1
	for i, raw := range messages {
		if fieldString(mapFromAnyMain(raw), "id") == strings.TrimSpace(clientUserMessageID) {
			index = i
			break
		}
	}
	if index < 0 {
		return nil
	}
	steer := mapFromAnyMain(messages[index])
	state := fieldString(steer, "steerState")
	if state == "accepted" || state == "applied" {
		return nil
	}
	if fieldString(steer, "steerBoundary") == "waiting" {
		continuationID := fieldString(steer, "steerContinuationId")
		messages = append(messages[:index], messages[index+1:]...)
		for continuationIndex, raw := range messages {
			continuation := mapFromAnyMain(raw)
			if fieldString(continuation, "id") == continuationID && fieldString(continuation, "steerBoundary") == "waiting" && fieldString(continuation, "steerContinuationFor") == clientUserMessageID {
				messages = append(messages[:continuationIndex], messages[continuationIndex+1:]...)
				break
			}
		}
		chat["messages"] = messages
		return nil
	}
	rootID := fieldString(steer, "turnRootId")
	messages = append(messages[:index], messages[index+1:]...)
	if index > 0 && index < len(messages) {
		left := mapFromAnyMain(messages[index-1])
		right := mapFromAnyMain(messages[index])
		if fieldString(left, "role") == "assistant" && fieldString(right, "role") == "assistant" && rootID != "" && fieldString(left, "turnRootId") == rootID && fieldString(right, "turnRootId") == rootID {
			leftContent := stringValue(left["content"])
			leftResult := stringValue(left["result"])
			left["content"] = leftContent + stringValue(right["content"])
			if mergedResult := leftResult + stringValue(right["result"]); mergedResult != "" {
				left["result"] = mergedResult
			} else {
				delete(left, "result")
			}
			shifted := anySlice(shiftSessionEvents(cloneJSON(anySlice(right["events"])), utf16CodeUnits(leftContent)))
			left["events"] = append(anySlice(left["events"]), shifted...)
			for _, key := range []string{"status", "at", "jobId", "turnTerminal", "permission", "interrupted", "retryPrompt", "turnStartedAt"} {
				if value, ok := right[key]; ok {
					left[key] = cloneJSON(value)
				} else {
					delete(left, key)
				}
			}
			if job != nil && job.AssistantID == fieldString(right, "id") {
				job.AssistantID = fieldString(left, "id")
				job.contentSegmentStart = max(0, job.contentSegmentStart-len(leftContent))
				job.finalSegmentStart = max(0, job.finalSegmentStart-len(leftResult))
			}
			messages = append(messages[:index], messages[index+1:]...)
		}
	}
	chat["messages"] = messages
	return nil
}

func (s *sessionStore) markSteerConsumedLocked(job *sessionJob, clientUserMessageID string) bool {
	if job == nil || strings.TrimSpace(clientUserMessageID) == "" {
		return false
	}
	chat := s.chatLocked(job.TabID)
	if chat == nil {
		return false
	}
	committed := s.commitStagedSteerLocked(chat, job, strings.TrimSpace(clientUserMessageID))
	for _, raw := range messageSlice(chat) {
		message := mapFromAnyMain(raw)
		if fieldString(message, "id") != strings.TrimSpace(clientUserMessageID) || fieldString(message, "role") != "user" || fieldString(message, "steerState") == "" {
			continue
		}
		message["status"] = "done"
		message["steerState"] = "applied"
		return true
	}
	return committed
}

func (s *sessionStore) settleJobSteersLocked(job *sessionJob) {
	if job == nil {
		return
	}
	chat := s.chatLocked(job.TabID)
	if chat == nil {
		return
	}
	rootID := firstNonEmptyString(job.RootAssistantID, job.AssistantID)
	s.settleStagedSteersLocked(chat, rootID)
	for _, raw := range messageSlice(chat) {
		message := mapFromAnyMain(raw)
		if fieldString(message, "role") != "user" || fieldString(message, "turnRootId") != rootID || fieldString(message, "steerState") == "" {
			continue
		}
		state := fieldString(message, "steerState")
		if state == "sending" {
			message["steerState"] = "uncertain"
		}
		if state != "" {
			message["status"] = "done"
		}
	}
}

func (s *sessionStore) settleStagedSteersLocked(chat map[string]any, rootID string) bool {
	if chat == nil {
		return false
	}
	messages := messageSlice(chat)
	waitingContinuations := make(map[string]struct{})
	for _, raw := range messages {
		message := mapFromAnyMain(raw)
		if fieldString(message, "role") != "user" || fieldString(message, "steerBoundary") != "waiting" || (rootID != "" && fieldString(message, "turnRootId") != rootID) {
			continue
		}
		if id := fieldString(message, "steerContinuationId"); id != "" {
			waitingContinuations[id] = struct{}{}
		}
	}
	if len(waitingContinuations) == 0 {
		return false
	}
	out := make([]any, 0, len(messages))
	changed := false
	for _, raw := range messages {
		message := mapFromAnyMain(raw)
		if _, waiting := waitingContinuations[fieldString(message, "id")]; waiting && fieldString(message, "role") == "assistant" && fieldString(message, "steerBoundary") == "waiting" {
			changed = true
			continue
		}
		if fieldString(message, "role") == "user" && fieldString(message, "steerBoundary") == "waiting" && (rootID == "" || fieldString(message, "turnRootId") == rootID) {
			delete(message, "steerBoundary")
			delete(message, "steerContinuationId")
			if fieldString(message, "steerState") == "sending" {
				message["steerState"] = "uncertain"
			}
			message["status"] = "done"
			changed = true
		}
		out = append(out, raw)
	}
	if changed {
		chat["messages"] = out
	}
	return changed
}

func (s *sessionStore) PersistProviderAttachments(images []any) ([]providercontract.Attachment, error) {
	if len(images) == 0 {
		return nil, nil
	}
	if s == nil || !s.enabled() {
		return nil, errors.New("durable session image storage is unavailable")
	}
	copyValue := cloneJSON(images)
	copyImages, ok := copyValue.([]any)
	if !ok {
		return nil, errors.New("provider attachments are not an image list")
	}
	stateDir := filepath.Dir(s.path)
	owner := map[string]any{"images": copyImages}
	if err := makeSessionValueRefNative(owner, stateDir); err != nil {
		return nil, err
	}
	copyImages = anySlice(owner["images"])
	attachments := make([]providercontract.Attachment, 0, len(copyImages))
	for index, raw := range copyImages {
		image := mapFromAnyMain(raw)
		ref := fieldString(image, sessionImageDataRefField)
		mimeType := strings.ToLower(strings.TrimSpace(fieldString(image, "mimeType")))
		if ref == "" || !strings.HasPrefix(mimeType, "image/") {
			return nil, fmt.Errorf("provider image %d is missing durable content or MIME identity", index)
		}
		name, _, info, err := externalSessionImageInfo(ref, stateDir)
		if err != nil {
			return nil, err
		}
		attachmentName := firstNonEmptyString(fieldString(image, "name"), fmt.Sprintf("image-%d", index+1))
		attachments = append(attachments, providercontract.Attachment{
			ID: "image-" + name[:16], Name: attachmentName, MIMEType: mimeType,
			Digest: name, Size: info.Size(), Ref: providerSessionImageRefPrefix + ref,
		})
	}
	return attachments, nil
}

func (s *sessionStore) ResolveProviderAttachment(_ context.Context, attachment providercontract.Attachment) (any, error) {
	if s == nil || !s.enabled() {
		return nil, errors.New("durable session image storage is unavailable")
	}
	ref := strings.TrimPrefix(strings.TrimSpace(attachment.Ref), providerSessionImageRefPrefix)
	if ref == strings.TrimSpace(attachment.Ref) || ref == "" {
		return nil, errors.New("provider attachment reference does not belong to the session image store")
	}
	data, err := readExternalSessionImage(ref, filepath.Dir(s.path))
	if err != nil {
		return nil, err
	}
	if attachment.Digest != "" && sessionImageName(data) != strings.TrimSpace(attachment.Digest) {
		return nil, errors.New("provider attachment digest changed after persistence")
	}
	return map[string]any{
		"mimeType": strings.TrimSpace(attachment.MIMEType), "data": data, "name": strings.TrimSpace(attachment.Name),
	}, nil
}

func seedSessionJobOutput(job *sessionJob, assistant map[string]any) {
	if job == nil || job.outputReady {
		return
	}
	resetSessionJobOutputs(job, stringValue(assistant["content"]), stringValue(assistant["result"]))
}

func normalizedAssistantPhase(phase string) string {
	switch strings.TrimSpace(phase) {
	case "commentary":
		return "commentary"
	case "final_answer":
		return "final_answer"
	default:
		return ""
	}
}

func resetSessionJobOutputs(job *sessionJob, content, result string) {
	if job == nil {
		return
	}
	job.output.Reset()
	job.contentOutput.Reset()
	job.finalOutput.Reset()
	job.contentOutput.WriteString(content)
	job.finalOutput.WriteString(result)
	job.output.WriteString(content)
	job.output.WriteString(result)
	job.outputReady = true
	job.typedOutput = result != ""
	job.contentSegmentStart = 0
	job.finalSegmentStart = 0
}

func appendSessionJobChunk(job *sessionJob, chunk, phase string) {
	if job == nil || chunk == "" {
		return
	}
	normalizedPhase := normalizedAssistantPhase(phase)
	job.output.WriteString(chunk)
	if normalizedPhase != "" {
		job.typedOutput = true
	}
	if normalizedPhase == "final_answer" {
		job.finalOutput.WriteString(chunk)
	} else {
		job.contentOutput.WriteString(chunk)
	}
}

func materializeSessionJobOutput(job *sessionJob, assistant map[string]any) {
	if job == nil || assistant == nil {
		return
	}
	content := job.contentOutput.String()
	contentStart := min(max(job.contentSegmentStart, 0), len(content))
	assistant["content"] = content[contentStart:]
	result := job.finalOutput.String()
	resultStart := min(max(job.finalSegmentStart, 0), len(result))
	if resultStart < len(result) {
		assistant["result"] = result[resultStart:]
	} else {
		delete(assistant, "result")
	}
}

func (s *sessionStore) materializeLiveOutputsLocked() {
	seen := make(map[*sessionJob]struct{}, len(s.pending)+len(s.jobs))
	materialize := func(job *sessionJob) {
		if job == nil || job.Finished {
			return
		}
		if _, ok := seen[job]; ok {
			return
		}
		seen[job] = struct{}{}
		assistant := s.messageForJobLocked(job)
		if assistant == nil {
			return
		}
		seedSessionJobOutput(job, assistant)
		materializeSessionJobOutput(job, assistant)
	}
	for _, job := range s.pending {
		materialize(job)
	}
	for _, job := range s.jobs {
		materialize(job)
	}
}

func (s *sessionStore) journalPath(turnID string) string {
	digest := sha256.Sum256([]byte(turnID))
	return filepath.Join(filepath.Dir(s.path), sessionJournalDirname, fmt.Sprintf("%x.jsonl", digest[:]))
}

func (s *sessionStore) removeJournalLocked(turnID, path string) error {
	if path == "" {
		path = s.journalPath(turnID)
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	_ = os.Remove(filepath.Dir(path))
	return nil
}

func (s *sessionStore) archiveJobMessagesLocked(job *sessionJob) []any {
	if job == nil || job.TabID == "" {
		return nil
	}
	if assistant := s.messageForJobLocked(job); assistant != nil {
		if job.outputReady && !job.Finished {
			materializeSessionJobOutput(job, assistant)
		}
	}
	var messages []any
	rootID := firstNonEmptyString(job.RootAssistantID, job.AssistantID)
	if chat := s.chatLocked(job.TabID); chat != nil {
		for _, raw := range messageSlice(chat) {
			message := mapFromAnyMain(raw)
			id := fieldString(message, "id")
			belongs := id == job.UserID || id == job.AssistantID
			if rowRoot := fieldString(message, "turnRootId"); rowRoot != "" && rowRoot == rootID {
				belongs = true
			}
			if belongs {
				messages = append(messages, cloneJSON(message))
			}
		}
	}
	if len(messages) == 0 {
		return nil
	}
	return messages
}

func (s *sessionStore) archiveSessionMessages(tabID string, messages []any) (warning error, fatal error) {
	if strings.TrimSpace(tabID) == "" || len(messages) == 0 {
		return nil, nil
	}
	stateDir := filepath.Dir(s.path)
	// The canonical mirror is ref-native, while archives intentionally retain
	// their established inline JSONL format for backward compatibility. Hydrate
	// only this detached turn copy; never put the bytes back into s.snapshot.
	warning = materializeSessionMessagesForArchive(messages, stateDir)
	if err := appendChatArchive(stateDir, tabID, messages); err != nil {
		return warning, err
	}
	path := chatArchivePath(stateDir, tabID)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return warning, err
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	return warning, errors.Join(syncErr, closeErr)
}

func (s *sessionStore) archiveJobLocked(job *sessionJob) error {
	if job == nil {
		return nil
	}
	warning, err := s.archiveSessionMessages(job.TabID, s.archiveJobMessagesLocked(job))
	s.loadErr = errors.Join(s.loadErr, warning)
	return err
}

func (s *sessionStore) archiveRecoveredJournalLocked(journal recoveredSessionJournal) error {
	if journal.job == nil {
		return nil
	}
	if assistant := s.messageForJobLocked(journal.job); assistant != nil && journal.job.outputReady {
		materializeSessionJobOutput(journal.job, assistant)
	}
	return s.archiveJobLocked(journal.job)
}

func (s *sessionStore) replaySessionJournalsLocked() ([]recoveredSessionJournal, int, error) {
	dir := filepath.Join(filepath.Dir(s.path), sessionJournalDirname)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, nil
		}
		return nil, 0, err
	}
	var recovered []recoveredSessionJournal
	quarantined := 0
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".jsonl" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		records, err := readSessionJournal(path)
		if err != nil {
			if quarantineErr := quarantineSessionJournal(path, err); quarantineErr != nil {
				return recovered, quarantined, quarantineErr
			}
			quarantined++
			continue
		}
		journal, err := s.validateThenApplySessionJournalLocked(path, records)
		if err != nil {
			if quarantineErr := quarantineSessionJournal(path, err); quarantineErr != nil {
				return recovered, quarantined, quarantineErr
			}
			quarantined++
			continue
		}
		if journal.job == nil {
			// Builds before internal subagent sessions were separated from the
			// visible mirror may leave a synced prepare/start sidecar behind.
			// There is deliberately no user chat to recover it into. Keep the
			// startup path non-panicking and move the stale sidecar through the
			// same quarantine path used for every non-replayable journal.
			if quarantineErr := quarantineSessionJournal(path, errInternalJournal); quarantineErr != nil {
				return recovered, quarantined, quarantineErr
			}
			quarantined++
			continue
		}
		recovered = append(recovered, journal)
	}
	return recovered, quarantined, nil
}

func quarantineSessionJournal(path string, validationErr error) error {
	if strings.TrimSpace(path) == "" || validationErr == nil {
		return errors.New("session journal quarantine requires a path and reason")
	}
	dir := filepath.Join(filepath.Dir(path), sessionJournalQuarantineDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create session journal quarantine: %w", err)
	}
	name := filepath.Base(path)
	target := filepath.Join(dir, name)
	if _, err := os.Stat(target); err == nil {
		ext := filepath.Ext(name)
		stem := strings.TrimSuffix(name, ext)
		for index := 1; ; index++ {
			candidate := filepath.Join(dir, fmt.Sprintf("%s-%d%s", stem, index, ext))
			if _, candidateErr := os.Stat(candidate); os.IsNotExist(candidateErr) {
				target = candidate
				break
			} else if candidateErr != nil {
				return fmt.Errorf("inspect session journal quarantine: %w", candidateErr)
			}
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect session journal quarantine: %w", err)
	}
	if err := os.Rename(path, target); err != nil {
		return fmt.Errorf("quarantine session journal %s: %w", path, err)
	}
	reason := strings.TrimSpace(acp.RedactSensitiveText(validationErr.Error()))
	if len(reason) > 4096 {
		reason = reason[:4096]
	}
	reasonPath := target + ".reason"
	if err := os.WriteFile(reasonPath, []byte(reason+"\n"), 0o600); err != nil {
		if rollbackErr := os.Rename(target, path); rollbackErr != nil {
			return errors.Join(
				fmt.Errorf("write session journal quarantine reason: %w", err),
				fmt.Errorf("restore journal after quarantine failure: %w", rollbackErr),
			)
		}
		return fmt.Errorf("write session journal quarantine reason: %w", err)
	}
	return nil
}

func (s *sessionStore) validateThenApplySessionJournalLocked(path string, records []map[string]any) (recoveredSessionJournal, error) {
	staged := s.cloneForJournalReplayLocked()
	recovered, err := staged.applySessionJournalLocked(path, records)
	if err != nil {
		return recoveredSessionJournal{}, err
	}
	s.snapshot = staged.snapshot
	s.jobs = staged.jobs
	s.pending = staged.pending
	s.jobOrder = staged.jobOrder
	return recovered, nil
}

func (s *sessionStore) cloneForJournalReplayLocked() *sessionStore {
	staged := &sessionStore{
		path:           s.path,
		jobs:           make(map[string]*sessionJob, len(s.jobs)),
		jobOrder:       append([]string(nil), s.jobOrder...),
		droppedChatIDs: s.droppedChatIDs,
	}
	if s.snapshot != nil {
		staged.snapshot, _ = cloneJSON(s.snapshot).(map[string]any)
	}
	clones := make(map[*sessionJob]*sessionJob, len(s.jobs)+len(s.pending))
	cloneJob := func(job *sessionJob) *sessionJob {
		if job == nil {
			return nil
		}
		if existing := clones[job]; existing != nil {
			return existing
		}
		copyJob := *job
		copyJob.output = strings.Builder{}
		copyJob.output.WriteString(job.output.String())
		copyJob.contentOutput = strings.Builder{}
		copyJob.contentOutput.WriteString(job.contentOutput.String())
		copyJob.finalOutput = strings.Builder{}
		copyJob.finalOutput.WriteString(job.finalOutput.String())
		clones[job] = &copyJob
		return &copyJob
	}
	for id, job := range s.jobs {
		staged.jobs[id] = cloneJob(job)
	}
	staged.pending = make([]*sessionJob, 0, len(s.pending))
	for _, job := range s.pending {
		staged.pending = append(staged.pending, cloneJob(job))
	}
	return staged
}

func readSessionJournal(path string) ([]map[string]any, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	reader := bufio.NewReader(file)
	var records []map[string]any
	for {
		line, readErr := reader.ReadBytes('\n')
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) > 0 {
			var record map[string]any
			dec := json.NewDecoder(bytes.NewReader(trimmed))
			dec.UseNumber()
			decodeErr := dec.Decode(&record)
			if decodeErr != nil {
				if readErr == io.EOF {
					// A process can die in the middle of the final append. The last
					// complete newline-delimited prefix remains authoritative.
					break
				}
				return nil, fmt.Errorf("decode session journal %s: %w", path, decodeErr)
			}
			var extra any
			if err := dec.Decode(&extra); err != io.EOF {
				return nil, fmt.Errorf("decode session journal %s: trailing data", path)
			}
			records = append(records, mapFromAnyMain(redactSessionValue(record)))
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, readErr
		}
	}
	if len(records) == 0 {
		return nil, fmt.Errorf("session journal %s has no complete records", path)
	}
	return records, nil
}

func (s *sessionStore) applySessionJournalLocked(path string, records []map[string]any) (recoveredSessionJournal, error) {
	var recovered recoveredSessionJournal
	if len(records) == 0 {
		return recovered, errors.New("session journal is empty")
	}
	turnID := fieldString(records[0], "turnId")
	if turnID == "" || filepath.Base(path) != filepath.Base(s.journalPath(turnID)) {
		return recovered, fmt.Errorf("session journal %s has invalid turn identity", path)
	}
	for i, record := range records {
		if intValue(record["v"]) != sessionJournalVersion {
			return recovered, fmt.Errorf("session journal %s version = %d", path, intValue(record["v"]))
		}
		if fieldString(record, "turnId") != turnID {
			return recovered, fmt.Errorf("session journal %s changed turn identity", path)
		}
		if got, want := intValue(record["seq"]), i+1; got != want {
			return recovered, fmt.Errorf("session journal %s sequence = %d, want %d", path, got, want)
		}
	}
	if fieldString(records[0], "kind") != "prepare" {
		return recovered, fmt.Errorf("session journal %s does not start with prepare", path)
	}
	job, canonicalTerminal, err := s.applyJournalPrepareLocked(turnID, records[0])
	if err != nil {
		return recovered, err
	}
	recovered = recoveredSessionJournal{path: path, turnID: turnID, job: job, terminal: canonicalTerminal}
	if canonicalTerminal {
		return recovered, nil
	}
	if job == nil {
		// The replay caller owns quarantine/removal. Do not inspect later records:
		// every record in this journal belongs to internal subagent plumbing and
		// none has a visible sessionJob owner after the namespace migration.
		return recovered, nil
	}

	terminal := false
	for _, record := range records[1:] {
		kind := fieldString(record, "kind")
		if terminal {
			return recovered, fmt.Errorf("session journal %s contains %s after terminal record", path, kind)
		}
		switch kind {
		case "start":
			if job.ID != "" {
				return recovered, fmt.Errorf("session journal %s contains two starts", path)
			}
			jobMap := mapFromAnyMain(record["job"])
			job.ID = fieldString(jobMap, "id")
			if job.ID == "" {
				return recovered, fmt.Errorf("session journal %s start has no job id", path)
			}
			if tabID := fieldString(jobMap, "tabId"); tabID != "" && tabID != job.TabID {
				return recovered, fmt.Errorf("session journal %s start changed tab id", path)
			}
			s.removePendingJobLocked(job)
			s.jobs[job.ID] = job
			s.jobOrder = append(s.jobOrder, job.ID)
			if assistant := s.messageForJobLocked(job); assistant != nil {
				assistant["jobId"] = job.ID
				assistant["status"] = "running"
			}
		case "data":
			if err := validateJournalJobID(path, job, record); err != nil {
				return recovered, err
			}
			offset := intValue(record["offset"])
			if offset != job.output.Len() {
				return recovered, fmt.Errorf("session journal %s data offset = %d, want %d", path, offset, job.output.Len())
			}
			appendSessionJobChunk(job, stringValue(record["chunk"]), fieldString(record, "phase"))
			if assistant := s.messageForJobLocked(job); assistant != nil {
				materializeSessionJobOutput(job, assistant)
				assistant["status"] = "running"
			}
		case "assistant-media":
			if err := validateJournalJobID(path, job, record); err != nil {
				return recovered, err
			}
			if assistant := s.messageForJobLocked(job); assistant != nil {
				assistant["images"] = mergeAssistantImages(anySlice(assistant["images"]), anySlice(record["images"]))
				assistant["status"] = "running"
			}
		case "acp":
			if err := validateJournalJobID(path, job, record); err != nil {
				return recovered, err
			}
			if assistant := s.messageForJobLocked(job); assistant != nil {
				event := mapFromAnyMain(record["event"])
				if fieldString(event, "kind") == "steer-consumed" {
					s.markSteerConsumedLocked(job, fieldString(event, "clientUserMessageId"))
				} else {
					target := s.assistantForSessionAcpEventLocked(job, event)
					applySessionAcpEventAt(target, event, utf16CodeUnits(stringValue(target["content"])))
					applyChatPlanLatest(s.chatLocked(job.TabID), target, event)
				}
			}
		case "steer":
			if err := validateJournalJobID(path, job, record); err != nil {
				return recovered, err
			}
			chat := s.chatLocked(job.TabID)
			if chat == nil {
				return recovered, fmt.Errorf("session journal %s steer has no chat", path)
			}
			if err := s.beginLiveSteerLocked(
				chat,
				job,
				fieldString(record, "clientUserMessageId"),
				fieldString(record, "prompt"),
				fieldString(record, "continuationAssistantMessageId"),
				anySlice(record["images"]),
				mapFromAnyMain(record["boundary"]),
			); err != nil {
				return recovered, fmt.Errorf("session journal %s steer: %w", path, err)
			}
		case "steer-boundary":
			if err := validateJournalJobID(path, job, record); err != nil {
				return recovered, err
			}
			chat := s.chatLocked(job.TabID)
			if chat == nil || !s.commitStagedSteerLocked(chat, job, fieldString(record, "clientUserMessageId")) {
				return recovered, fmt.Errorf("session journal %s staged steer boundary is unavailable", path)
			}
		case "steer-state":
			if err := validateJournalJobID(path, job, record); err != nil {
				return recovered, err
			}
			if chat := s.chatLocked(job.TabID); chat != nil {
				for _, raw := range messageSlice(chat) {
					message := mapFromAnyMain(raw)
					if fieldString(message, "id") != fieldString(record, "clientUserMessageId") {
						continue
					}
					message["status"] = "done"
					if fieldString(record, "outcome") == "uncertain" && fieldString(message, "steerState") != "applied" {
						message["steerState"] = "uncertain"
					} else if fieldString(message, "steerState") != "applied" {
						message["steerState"] = "accepted"
					}
					break
				}
			}
		case "steer-reject":
			if err := validateJournalJobID(path, job, record); err != nil {
				return recovered, err
			}
			if chat := s.chatLocked(job.TabID); chat != nil {
				if err := s.rejectLiveSteerLocked(chat, job, fieldString(record, "clientUserMessageId")); err != nil {
					return recovered, fmt.Errorf("session journal %s steer rejection: %w", path, err)
				}
			}
		case "end":
			if err := validateJournalJobID(path, job, mapFromAnyMain(record["job"])); err != nil {
				return recovered, err
			}
			s.applyJournalEndLocked(job, mapFromAnyMain(record["job"]))
			terminal = true
		case "fail":
			assistant := s.messageForJobLocked(job)
			if assistant != nil {
				message := fieldString(record, "message")
				assistant["status"] = "failed"
				assistant["content"] = message
				assistant["at"] = record["finishedAt"]
				delete(assistant, "result")
				resetSessionJobOutputs(job, message, "")
			}
			job.Finished = true
			terminal = true
		default:
			return recovered, fmt.Errorf("session journal %s has unknown record kind %q", path, kind)
		}
	}
	recovered.terminal = terminal
	if job.outputReady {
		if assistant := s.messageForJobLocked(job); assistant != nil {
			materializeSessionJobOutput(job, assistant)
		}
	}
	return recovered, nil
}

func (s *sessionStore) applyJournalPrepareLocked(turnID string, record map[string]any) (*sessionJob, bool, error) {
	tabID, chatID := fieldString(record, "tabId"), fieldString(record, "chatId")
	assistantID := fieldString(record, "assistantId")
	if tabID == "" || assistantID == "" || assistantID != turnID {
		return nil, false, errors.New("session journal prepare identity is incomplete")
	}
	if existing := s.chatLocked(tabID); existing != nil {
		if existingChatID := fieldString(existing, "chatId"); chatID != "" && existingChatID != "" && existingChatID != chatID {
			return nil, false, errors.New("session journal conflicts with canonical chat identity")
		}
		if assistant := messageByID(existing, assistantID); assistant != nil && terminalSessionStatus(fieldString(assistant, "status")) && assistant["turnTerminal"] != false {
			job := &sessionJob{
				ID: fieldString(assistant, "jobId"), TabID: tabID, ChatID: chatID,
				UserID: fieldString(record, "userId"), AssistantID: assistantID, Finished: true,
			}
			seedSessionJobOutput(job, assistant)
			return job, true, nil
		}
	}

	s.ensureSnapshotLocked()
	chatFields := mapFromAnyMain(record["chat"])
	chat := s.ensureChatLocked(tabID, chatID, fieldString(chatFields, "title"))
	if chat == nil {
		return nil, false, nil // internal subagent session: nothing to recover into
	}
	for _, key := range []string{"chatId", "title", "titleLocked", "group", "cwd", "currentModelId", "currentModeId", "providerId"} {
		if value, ok := chatFields[key]; ok {
			chat[key] = cloneJSON(value)
		}
	}
	if queueID := fieldString(record, "queueId"); queueID != "" {
		queue := anySlice(chat["queue"])
		out := make([]any, 0, len(queue))
		removed := false
		for _, raw := range queue {
			if fieldString(mapFromAnyMain(raw), "id") == queueID {
				removed = true
				continue
			}
			out = append(out, raw)
		}
		chat["queue"] = out
		if removed {
			bumpLegacyQueueRevision(chat)
		}
	}
	user := mapFromAnyMain(record["user"])
	assistant := mapFromAnyMain(record["assistant"])
	if fieldString(assistant, "id") != assistantID {
		return nil, false, errors.New("session journal assistant id does not match prepare identity")
	}
	resetJournalTurnMessages(chat, user, assistant)
	job := &sessionJob{
		TabID: tabID, ChatID: chatID, UserID: fieldString(record, "userId"), AssistantID: assistantID,
	}
	seedSessionJobOutput(job, assistant)
	s.pending = append(s.pending, job)
	return job, false, nil
}

// bumpLegacyQueueRevision records the one-time consumption of a queued row
// while importing a pre-actor crash journal. Runtime queue revisions belong to
// the chat actor and never call this migration helper.
func bumpLegacyQueueRevision(chat map[string]any) {
	if chat == nil {
		return
	}
	chat[agentQueueRevisionField] = max(0, intValue(chat[agentQueueRevisionField])) + 1
}

func resetJournalTurnMessages(chat, user, assistant map[string]any) {
	userID, assistantID := fieldString(user, "id"), fieldString(assistant, "id")
	messages := messageSlice(chat)
	insertAt := len(messages)
	out := make([]any, 0, len(messages)+2)
	for i, raw := range messages {
		message := mapFromAnyMain(raw)
		id := fieldString(message, "id")
		if id == userID || id == assistantID || fieldString(message, "turnRootId") == assistantID {
			if i < insertAt {
				insertAt = len(out)
			}
			continue
		}
		out = append(out, raw)
	}
	if insertAt > len(out) {
		insertAt = len(out)
	}
	turn := make([]any, 0, 2)
	if userID != "" {
		turn = append(turn, cloneJSON(user))
	}
	turn = append(turn, cloneJSON(assistant))
	out = append(out, make([]any, len(turn))...)
	copy(out[insertAt+len(turn):], out[insertAt:])
	copy(out[insertAt:], turn)
	chat["messages"] = out
}

func messageByID(chat map[string]any, id string) map[string]any {
	for _, raw := range messageSlice(chat) {
		message := mapFromAnyMain(raw)
		if fieldString(message, "id") == id {
			return message
		}
	}
	return nil
}

func terminalSessionStatus(status string) bool {
	switch status {
	case "done", "failed", "cancelled":
		return true
	default:
		return false
	}
}

func validateJournalJobID(path string, job *sessionJob, record map[string]any) error {
	if job == nil || job.ID == "" {
		return fmt.Errorf("session journal %s event arrived before start", path)
	}
	if id := fieldString(record, "jobId"); id != "" && id != job.ID {
		return fmt.Errorf("session journal %s changed job id", path)
	}
	if id := fieldString(record, "id"); id != "" && id != job.ID {
		return fmt.Errorf("session journal %s changed job id", path)
	}
	return nil
}

func (s *sessionStore) removePendingJobLocked(target *sessionJob) {
	for i, job := range s.pending {
		if job == target {
			s.pending = append(s.pending[:i], s.pending[i+1:]...)
			return
		}
	}
}

func mergeAssistantImages(existing, incoming []any) []any {
	out := make([]any, 0, len(existing)+len(incoming))
	seen := make(map[string]struct{}, len(existing)+len(incoming))
	appendImage := func(raw any) {
		image := mapFromAnyMain(raw)
		mimeType := strings.ToLower(strings.TrimSpace(fieldString(image, "mimeType")))
		data := strings.TrimSpace(fieldString(image, "data"))
		dataRef := strings.TrimSpace(fieldString(image, sessionImageDataRefField))
		source := strings.TrimSpace(fieldString(image, "source"))
		if mimeType == "" || (data == "" && dataRef == "") {
			return
		}
		key := source
		if key == "" {
			key = mimeType + "\x00" + firstNonEmptyString(dataRef, data)
		}
		if _, duplicate := seen[key]; duplicate {
			return
		}
		seen[key] = struct{}{}
		out = append(out, cloneJSON(image))
	}
	for _, raw := range existing {
		appendImage(raw)
	}
	for _, raw := range incoming {
		appendImage(raw)
	}
	return out
}

func (s *sessionStore) applyJournalEndLocked(job *sessionJob, jobMap map[string]any) {
	if job == nil {
		return
	}
	if chat := s.chatLocked(job.TabID); chat != nil {
		for _, rawID := range anySlice(jobMap["consumedSteerIds"]) {
			s.commitStagedSteerLocked(chat, job, stringValue(rawID))
			for _, raw := range messageSlice(chat) {
				message := mapFromAnyMain(raw)
				if fieldString(message, "id") == stringValue(rawID) && fieldString(message, "role") == "user" {
					message["status"] = "done"
					message["steerState"] = "applied"
					break
				}
			}
		}
	}
	assistant := s.messageForJobLocked(job)
	if assistant == nil {
		job.Finished = true
		return
	}
	seedSessionJobOutput(job, assistant)
	// Phase-less providers retain the existing terminal-result authority: it
	// may replace a stale/partial stream. Once typed chunks exist, their exact
	// structural split is authoritative and Job.Result is only the combined
	// provider-history representation.
	if result := stringValue(jobMap["result"]); result != "" && !job.typedOutput {
		resetSessionJobOutputs(job, result, "")
	}
	materializeSessionJobOutput(job, assistant)
	if images := anySlice(jobMap["images"]); len(images) > 0 {
		assistant["images"] = mergeAssistantImages(anySlice(assistant["images"]), images)
	}
	status := fieldString(jobMap, "status")
	if fieldString(jobMap, "stopReason") == "cancelled" || intValue(jobMap["code"]) == 130 {
		status = "cancelled"
	} else if status == "done" {
		status = "done"
	} else {
		status = "failed"
	}
	if chat := s.chatLocked(job.TabID); chat != nil {
		assistantID := fieldString(assistant, "id")
		for _, raw := range messageSlice(chat) {
			message := mapFromAnyMain(raw)
			if fieldString(message, "role") == "assistant" && ((assistantID != "" && fieldString(message, "id") == assistantID) || (job.ID != "" && fieldString(message, "jobId") == job.ID)) {
				settleTerminalToolEvents(message, status)
			}
		}
	} else {
		settleTerminalToolEvents(assistant, status)
	}
	assistant["status"] = status
	assistant["at"] = jobMap["finishedAt"]
	// Carry the daemon's own interruption onto the transcript. This runs for the
	// live job:end and for its journal replay, so a turn the daemon ended keeps
	// saying so across the very restart that ended it.
	if jobMap["interrupted"] == true {
		assistant["interrupted"] = true
	} else {
		delete(assistant, "interrupted")
	}
	if stringValue(assistant["content"]) == "" && stringValue(assistant["result"]) == "" {
		switch status {
		case "failed":
			if detail := fieldString(jobMap, "error"); detail != "" {
				assistant["content"] = "Error: " + detail
			} else {
				assistant["content"] = "La tarea falló."
			}
		}
		resetSessionJobOutputs(job, stringValue(assistant["content"]), "")
	}
	job.Finished = true
	s.settleJobSteersLocked(job)
}

func (s *sessionStore) finalizeRecoveredOrphansLocked(recovered []recoveredSessionJournal) {
	for i := range recovered {
		journal := &recovered[i]
		if journal.job == nil || journal.terminal {
			continue
		}
		assistant := s.messageForJobLocked(journal.job)
		if assistant != nil {
			resetSessionJobOutputs(journal.job, stringValue(assistant["content"]), stringValue(assistant["result"]))
		}
		journal.job.Finished = true
		journal.terminal = true
	}
}

func (s *sessionStore) ensureSnapshotLocked() {
	if s.snapshot == nil {
		s.snapshot = map[string]any{"v": json.Number("1"), "activeId": nil, "seq": json.Number("0"), "chats": []any{}}
	}
	if _, ok := s.snapshot["chats"].([]any); !ok {
		s.snapshot["chats"] = []any{}
	}
	s.pruneInternalChatsLocked(s.snapshot)
}

// The subagent: namespace is reserved for internal ACP sessions. A subagent
// runs on its own session whose chat/tab id is
// "subagent:<runID>" (internal/acp/subagents.go). That session is internal
// plumbing: its receipts live in the subagent registry and its visible events
// route to the owning turn's job through VisibleJobID. It must never
// materialize a user-visible chat — every spawned lane was surfacing in the
// sidebar as an untitled "Nuevo chat" row and persisting across restarts
// (user 2026-07-24).
const subagentTabPrefix = "subagent:"

func isInternalTabID(tabID string) bool {
	return strings.HasPrefix(tabID, subagentTabPrefix)
}

// Drop internal rows that older builds already persisted, so the ghosts do not
// come back on the next load.
func (s *sessionStore) pruneInternalChatsLocked(snapshot map[string]any) bool {
	chats := anySlice(snapshot["chats"])
	kept := make([]any, 0, len(chats))
	for _, raw := range chats {
		if isInternalTabID(fieldString(mapFromAnyMain(raw), "id")) {
			continue
		}
		kept = append(kept, raw)
	}
	if len(kept) != len(chats) {
		snapshot["chats"] = kept
		return true
	}
	return false
}

// Returns nil for an internal tab; every caller must tolerate that.
func (s *sessionStore) ensureChatLocked(tabID, chatID, title string) map[string]any {
	if isInternalTabID(tabID) {
		return nil
	}
	for _, raw := range anySlice(s.snapshot["chats"]) {
		chat := mapFromAnyMain(raw)
		if fieldString(chat, "id") == tabID {
			if fieldString(chat, "chatId") == "" && chatID != "" {
				chat["chatId"] = chatID
			}
			return chat
		}
	}
	cleanTitle := strings.TrimSpace(strings.TrimPrefix(title, "Devin ·"))
	if cleanTitle == "" {
		cleanTitle = "Nuevo chat"
	}
	chat := map[string]any{
		"id": tabID, "chatId": chatID, "title": cleanTitle, "titleLocked": true,
		"group": nil, "cwd": nil, "currentModelId": nil, "currentModeId": nil,
		"draft": "", "providerId": nil, "messages": []any{},
	}
	s.snapshot["chats"] = append(anySlice(s.snapshot["chats"]), chat)
	if s.snapshot["activeId"] == nil {
		s.snapshot["activeId"] = tabID
	}
	return chat
}

func (s *sessionStore) messageForJobLocked(job *sessionJob) map[string]any {
	chat := s.chatLocked(job.TabID)
	if chat == nil {
		return nil
	}
	for _, raw := range messageSlice(chat) {
		message := mapFromAnyMain(raw)
		if fieldString(message, "role") == "assistant" && stringValue(message["id"]) == job.AssistantID {
			return message
		}
	}
	if job.ID != "" {
		messages := messageSlice(chat)
		for index := len(messages) - 1; index >= 0; index-- {
			message := mapFromAnyMain(messages[index])
			if fieldString(message, "role") == "assistant" && stringValue(message["jobId"]) == job.ID {
				job.AssistantID = fieldString(message, "id")
				return message
			}
		}
	}
	return nil
}

func (s *sessionStore) assistantForSessionAcpEventLocked(job *sessionJob, event map[string]any) map[string]any {
	current := s.messageForJobLocked(job)
	if current == nil || fieldString(event, "kind") != "tool" {
		return current
	}
	eventID, terminalID := fieldString(event, "id"), fieldString(event, "terminalId")
	if eventID == "" && terminalID == "" {
		return current
	}
	chat := s.chatLocked(job.TabID)
	if chat == nil {
		return current
	}
	for _, raw := range messageSlice(chat) {
		message := mapFromAnyMain(raw)
		if fieldString(message, "role") != "assistant" || (job.ID != "" && fieldString(message, "jobId") != job.ID) {
			continue
		}
		for _, eventRaw := range anySlice(message["events"]) {
			candidate := mapFromAnyMain(eventRaw)
			if fieldString(candidate, "kind") != "tool" {
				continue
			}
			if (eventID != "" && fieldString(candidate, "id") == eventID) || (terminalID != "" && fieldString(candidate, "terminalId") == terminalID) {
				return message
			}
		}
	}
	return current
}

func (s *sessionStore) chatLocked(tabID string) map[string]any {
	for _, raw := range anySlice(s.snapshot["chats"]) {
		chat := mapFromAnyMain(raw)
		if fieldString(chat, "id") == tabID {
			return chat
		}
	}
	return nil
}

func mergeAgentQueueMessageRows(clientMessages, serverMessages []any) []any {
	out := append([]any(nil), clientMessages...)
	position := func(id string) int {
		for index, raw := range out {
			if fieldString(mapFromAnyMain(raw), "id") == id {
				return index
			}
		}
		return -1
	}
	for serverIndex, raw := range serverMessages {
		serverMessage := mapFromAnyMain(raw)
		if fieldString(serverMessage, agentQueueMessageField) == "" {
			continue
		}
		// A renderer can optimistically paint the human row just before the
		// daemon adopts that same submission from its durable FIFO. The promoted
		// row has a new server id plus agentQueueId; a stale renderer save can
		// therefore leave its unowned local echo beside the canonical turn. Match
		// only a near-simultaneous orphan user row. A deliberate repeated prompt
		// followed by its own assistant remains a separate turn.
		if echo := agentQueueEchoPosition(out, serverMessage); echo >= 0 {
			out = append(out[:echo], out[echo+1:]...)
		}
		id := fieldString(serverMessage, "id")
		if id == "" {
			continue
		}
		if index := position(id); index >= 0 {
			out[index] = raw
			continue
		}
		insertAt := len(out)
		for next := serverIndex + 1; next < len(serverMessages); next++ {
			nextID := fieldString(mapFromAnyMain(serverMessages[next]), "id")
			if index := position(nextID); index >= 0 {
				insertAt = index
				break
			}
		}
		if insertAt == len(out) {
			for previous := serverIndex - 1; previous >= 0; previous-- {
				previousID := fieldString(mapFromAnyMain(serverMessages[previous]), "id")
				if index := position(previousID); index >= 0 {
					insertAt = index + 1
					break
				}
			}
		}
		out = append(out, nil)
		copy(out[insertAt+1:], out[insertAt:])
		out[insertAt] = raw
	}
	return out
}

func agentQueueEchoPosition(messages []any, canonical map[string]any) int {
	if fieldString(canonical, "role") != "user" || fieldString(canonical, agentQueueMessageField) == "" {
		return -1
	}
	canonicalAt, err := time.Parse(time.RFC3339Nano, fieldString(canonical, "at"))
	if err != nil {
		return -1
	}
	best, bestGap := -1, time.Duration(1<<63-1)
	for index, raw := range messages {
		message := mapFromAnyMain(raw)
		if fieldString(message, "id") == fieldString(canonical, "id") ||
			fieldString(message, "role") != "user" ||
			fieldString(message, "status") != "done" ||
			fieldString(message, agentQueueMessageField) != "" ||
			fieldString(message, "steerState") != "" ||
			fieldString(message, "turnRootId") != "" ||
			fieldString(message, "content") != fieldString(canonical, "content") ||
			!reflect.DeepEqual(anySlice(message["images"]), anySlice(canonical["images"])) {
			continue
		}
		// An ordinary user row followed by an assistant is a complete turn, even
		// when its text repeats. Only a row stranded without its own assistant is
		// an optimistic echo eligible for replacement.
		if index+1 < len(messages) && fieldString(mapFromAnyMain(messages[index+1]), "role") == "assistant" {
			continue
		}
		messageAt, parseErr := time.Parse(time.RFC3339Nano, fieldString(message, "at"))
		if parseErr != nil {
			continue
		}
		gap := canonicalAt.Sub(messageAt)
		if gap < 0 {
			gap = -gap
		}
		if gap <= time.Second && gap < bestGap {
			best, bestGap = index, gap
		}
	}
	return best
}

// Older snapshots may already contain both the daemon-promoted FIFO row and
// the renderer echo it superseded. Repair those once at the disk boundary so a
// corrected build does not need a later renderer save before the duplicate
// disappears from the transcript.
func migrateAgentQueueEchoes(snapshot map[string]any) bool {
	if snapshot == nil {
		return false
	}
	changed := false
	for _, rawChat := range anySlice(snapshot["chats"]) {
		chat := mapFromAnyMain(rawChat)
		messages := messageSlice(chat)
		merged := mergeAgentQueueMessageRows(messages, messages)
		if len(merged) == len(messages) {
			continue
		}
		chat["messages"] = merged
		changed = true
	}
	return changed
}

func (s *sessionStore) interruptOrphanedTurnsLocked() bool {
	changed := false
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, chatRaw := range anySlice(s.snapshot["chats"]) {
		chat := mapFromAnyMain(chatRaw)
		if s.settleStagedSteersLocked(chat, "") {
			changed = true
		}
		for _, messageRaw := range messageSlice(chat) {
			message := mapFromAnyMain(messageRaw)
			status := fieldString(message, "status")
			if status != "running" && status != "pending" {
				continue
			}
			if fieldString(message, "role") == "user" && fieldString(message, "steerState") != "" {
				if fieldString(message, "steerState") == "sending" {
					message["steerState"] = "uncertain"
				}
				message["status"] = "done"
				if message["at"] == nil {
					message["at"] = now
				}
				changed = true
				continue
			}
			message["status"] = "failed"
			message["at"] = now
			// A turn killed by a daemon restart is not an agent error, and the
			// renderer already distinguishes the two (its own disconnect sweep
			// stamps this flag). Without it the snapshot that overwrites the
			// renderer's local state on rehydration is indistinguishable from a
			// real failure, so every restart left the sidebar shouting "Falló".
			message["interrupted"] = true
			if stringValue(message["content"]) == "" {
				message["content"] = "El turno fue interrumpido al reiniciar el daemon."
			}
			changed = true
		}
	}
	return changed
}

func (s *sessionStore) writeLocked() error {
	if !s.enabled() {
		return os.ErrInvalid
	}
	data, seq, err := s.snapshotBytesLocked()
	if err != nil {
		return err
	}
	if err := s.persistSnapshot(seq, data, nil); err != nil {
		return err
	}
	return nil
}

func (s *sessionStore) snapshotBytesLocked() ([]byte, uint64, error) {
	data, seq, images, err := s.stageSnapshotBytesLocked()
	if err != nil {
		return nil, 0, err
	}
	// Most daemon mutations use writeLocked at infrequent turn/control
	// boundaries and already hold s.mu. session:save uses stageSnapshotBytesLocked
	// directly so this filesystem phase runs only after releasing s.mu.
	if err := persistSessionImagePlans(images); err != nil {
		return nil, 0, err
	}
	return data, seq, nil
}

func (s *sessionStore) stageSnapshotBytesLocked() ([]byte, uint64, []sessionImageWritePlan, error) {
	// Do not recursively redact here. The snapshot is redacted once when loaded
	// and every mutation path (Save, PrepareTurn, RecordJobEvent, controls, and
	// failures) redacts its input before insertion. Re-redacting the complete
	// multi-megabyte mirror here held s.mu for ~157 ms on the user's real state,
	// blocking the pre-broadcast chunk path on every 100 ms streaming flush.
	snapshot, images, err := stageSessionSnapshotForPersistence(s.snapshot, filepath.Dir(s.path))
	if err != nil {
		return nil, 0, nil, err
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return nil, 0, nil, err
	}
	s.persistSeq++
	return data, s.persistSeq, images, nil
}

func stageSessionSnapshotForPersistence(snapshot map[string]any, stateDir string) (map[string]any, []sessionImageWritePlan, error) {
	if snapshot == nil {
		return nil, nil, nil
	}
	// Persistence rewrites exactly one kind of node: an image map carrying inline
	// base64 (and, above it, the slice/map that holds it). cloneJSON copied the
	// entire mirror instead — 275 ms per write on the user's real state, with
	// rehydrated screenshots making the in-memory snapshot an order of magnitude
	// larger than its on-disk form, all of it inside s.mu and therefore in front
	// of every streaming chunk. Copy only the paths that get rewritten.
	out, _ := cloneSessionImagePaths(snapshot).(map[string]any)
	dedupeSessionEventImages(out)
	images, err := stageExternalSessionImageData(out, stateDir)
	if err != nil {
		return nil, nil, err
	}
	return out, images, nil
}

// inlineSessionImage reports whether a map is an image node still holding inline
// base64 — the only node persistence mutates.
func inlineSessionImage(item map[string]any) bool {
	return strings.HasPrefix(strings.ToLower(fieldString(item, "mimeType")), "image/") && fieldString(item, "data") != ""
}

// cloneSessionImagePaths returns a copy-on-write view of value in which every
// inline image node, and every map and slice on the path to one, is a fresh
// object; everything else is shared with the caller's tree. dedupe and
// externalize only ever write to an inline image node or to its ancestors, so
// the shared remainder is never touched.
func cloneSessionImagePaths(value any) any {
	switch item := value.(type) {
	case map[string]any:
		var out map[string]any
		if inlineSessionImage(item) {
			out = shallowCopyStringMap(item)
		}
		for key, child := range item {
			replacement := cloneSessionImagePaths(child)
			if unchangedNode(child, replacement) {
				continue
			}
			if out == nil {
				out = shallowCopyStringMap(item)
			}
			out[key] = replacement
		}
		if out == nil {
			return item
		}
		return out
	case []any:
		var out []any
		for i, child := range item {
			replacement := cloneSessionImagePaths(child)
			if unchangedNode(child, replacement) {
				continue
			}
			if out == nil {
				out = make([]any, len(item))
				copy(out, item)
			}
			out[i] = replacement
		}
		if out == nil {
			return item
		}
		return out
	default:
		return value
	}
}

func dedupeSessionEventImages(snapshot map[string]any) {
	for _, rawChat := range anySlice(snapshot["chats"]) {
		for _, rawMessage := range messageSlice(mapFromAnyMain(rawChat)) {
			message := mapFromAnyMain(rawMessage)
			messageImages := anySlice(message["images"])
			if len(messageImages) == 0 {
				continue
			}
			indices := make(map[string]int, len(messageImages))
			for index, rawImage := range messageImages {
				if key, ok := exactSessionImageKey(rawImage); ok {
					if _, exists := indices[key]; !exists {
						indices[key] = index
					}
				}
			}
			if len(indices) == 0 {
				continue
			}
			for _, rawEvent := range anySlice(message["events"]) {
				event := mapFromAnyMain(rawEvent)
				images := anySlice(event["images"])
				replaced := false
				for index, rawImage := range images {
					key, ok := exactSessionImageKey(rawImage)
					if !ok {
						continue
					}
					if messageIndex, duplicate := indices[key]; duplicate {
						images[index] = map[string]any{sessionMessageImageRefField: messageIndex}
						replaced = true
					}
				}
				// Only write back when a ref actually replaced inline data. The
				// snapshot handed here shares every untouched node with live state,
				// so an unconditional assignment would poke a shared map for nothing.
				if replaced {
					event["images"] = images
				}
			}
		}
	}
}

func exactSessionImageKey(raw any) (string, bool) {
	image := mapFromAnyMain(raw)
	data := fieldString(image, "data")
	if !strings.HasPrefix(strings.ToLower(fieldString(image, "mimeType")), "image/") || data == "" {
		return "", false
	}
	// Identify the payload by its content address rather than marshaling the
	// base64 itself. Marshaling every image on every persist cost ~26 ms inside
	// s.mu on the user's real state; the address is memoized and just as exact.
	surrogate := shallowCopyStringMap(image)
	surrogate["data"] = sessionImageName(data)
	encoded, err := json.Marshal(surrogate)
	if err != nil {
		return "", false
	}
	return string(encoded), true
}

// The user's observed session carried about 35 MiB of rehydrated screenshots.
// A 64 MiB retained-string budget covers that working set (and about six
// maximum-size base64 screenshots) while putting a strict 64x ceiling below
// the former 512-entry/~4 GiB worst case.
const sessionImageNameMemoByteBudget = 64 << 20

var (
	sessionImageNameMu   sync.Mutex
	sessionImageNameSeed = maphash.MakeSeed()
	sessionImageNameMemo = newSessionImageMemo(sessionImageNameMemoByteBudget)
)

type sessionImageMemoEntry struct {
	fingerprint uint64
	data        string
	name        string
	element     *list.Element
}

type sessionImageMemo struct {
	byteBudget    int
	retainedBytes int
	byFingerprint map[uint64][]*sessionImageMemoEntry
	lru           list.List
}

func newSessionImageMemo(byteBudget int) *sessionImageMemo {
	return &sessionImageMemo{
		byteBudget:    byteBudget,
		byFingerprint: make(map[uint64][]*sessionImageMemoEntry),
	}
}

func sessionImageFingerprint(data string) uint64 {
	var hash maphash.Hash
	hash.SetSeed(sessionImageNameSeed)
	_, _ = hash.WriteString(data)
	return hash.Sum64()
}

func (memo *sessionImageMemo) lookup(fingerprint uint64, data string) (string, bool) {
	for _, entry := range memo.byFingerprint[fingerprint] {
		if entry.data != data {
			continue
		}
		memo.lru.MoveToFront(entry.element)
		return entry.name, true
	}
	return "", false
}

func (memo *sessionImageMemo) insert(fingerprint uint64, data, name string) {
	cost := len(data) + len(name)
	if memo.byteBudget <= 0 || cost > memo.byteBudget {
		return
	}
	for memo.retainedBytes+cost > memo.byteBudget {
		oldestElement := memo.lru.Back()
		if oldestElement == nil {
			break
		}
		oldest := oldestElement.Value.(*sessionImageMemoEntry)
		bucket := memo.byFingerprint[oldest.fingerprint]
		for index, candidate := range bucket {
			if candidate != oldest {
				continue
			}
			bucket = append(bucket[:index], bucket[index+1:]...)
			break
		}
		if len(bucket) == 0 {
			delete(memo.byFingerprint, oldest.fingerprint)
		} else {
			memo.byFingerprint[oldest.fingerprint] = bucket
		}
		memo.lru.Remove(oldestElement)
		memo.retainedBytes -= len(oldest.data) + len(oldest.name)
	}
	entry := &sessionImageMemoEntry{fingerprint: fingerprint, data: data, name: name}
	entry.element = memo.lru.PushFront(entry)
	memo.byFingerprint[fingerprint] = append(memo.byFingerprint[fingerprint], entry)
	memo.retainedBytes += cost
}

// sessionImageName returns the content address of an inline base64 payload.
// Rehydration deliberately drops the ref it loaded from, so persistence had to
// re-derive it — sha256 over every screenshot in the mirror, ~22 ms per save
// while s.mu was held and therefore in front of every streaming chunk. A fast
// fingerprint selects a bounded collision bucket; the retained payload value
// is compared for exactness, but never serves as a long-lived map key.
func sessionImageName(data string) string {
	fingerprint := sessionImageFingerprint(data)
	sessionImageNameMu.Lock()
	name, ok := sessionImageNameMemo.lookup(fingerprint, data)
	sessionImageNameMu.Unlock()
	if ok {
		return name
	}
	sum := sha256.Sum256([]byte(data))
	name = fmt.Sprintf("%x", sum[:])
	sessionImageNameMu.Lock()
	if existing, found := sessionImageNameMemo.lookup(fingerprint, data); found {
		sessionImageNameMu.Unlock()
		return existing
	}
	sessionImageNameMemo.insert(fingerprint, data, name)
	sessionImageNameMu.Unlock()
	return name
}

type sessionImageWritePlan struct {
	ref  string
	dir  string
	path string
	data string
}

func stageExternalSessionImageData(value any, stateDir string) ([]sessionImageWritePlan, error) {
	plans := make([]sessionImageWritePlan, 0)
	byPath := make(map[string]int)
	if err := stageExternalSessionImageDataInto(value, stateDir, &plans, byPath); err != nil {
		return nil, err
	}
	return plans, nil
}

// makeSessionSnapshotRefNative converts a detached session-shaped tree in
// place. It is the only ingress boundary for renderer snapshots and recovered
// state: duplicate event images become owning-message refs, remaining inline
// payloads become content refs, and every referenced file is durable before the
// caller can publish the tree.
func makeSessionSnapshotRefNative(snapshot map[string]any, stateDir string) error {
	if snapshot == nil {
		return nil
	}
	dedupeSessionEventImages(snapshot)
	plans, err := stageExternalSessionImageData(snapshot, stateDir)
	if err != nil {
		return err
	}
	if err := persistSessionImagePlans(plans); err != nil {
		return err
	}
	return validateRefNativeSessionImages(snapshot, stateDir)
}

// makeSessionValueRefNative is the corresponding boundary for detached
// non-snapshot payloads (turn options and job events). Message-relative event
// dedupe does not apply, but all inline image maps are externalized recursively.
func makeSessionValueRefNative(value any, stateDir string) error {
	plans, err := stageExternalSessionImageData(value, stateDir)
	if err != nil {
		return err
	}
	if err := persistSessionImagePlans(plans); err != nil {
		return err
	}
	return validateExternalSessionImageRefs(value, stateDir)
}

func stageExternalSessionImageDataInto(value any, stateDir string, plans *[]sessionImageWritePlan, byPath map[string]int) error {
	switch item := value.(type) {
	case map[string]any:
		if strings.HasPrefix(strings.ToLower(fieldString(item, "mimeType")), "image/") {
			if data := fieldString(item, "data"); data != "" {
				name := sessionImageName(data)
				ref := sessionImageDirname + "/" + name
				dir := filepath.Join(stateDir, sessionImageDirname)
				path := filepath.Join(dir, name)
				if existingIndex, exists := byPath[path]; exists {
					if (*plans)[existingIndex].data != data {
						return fmt.Errorf("session image ref %s has conflicting staged content", ref)
					}
				} else {
					byPath[path] = len(*plans)
					*plans = append(*plans, sessionImageWritePlan{ref: ref, dir: dir, path: path, data: data})
				}
				delete(item, "data")
				item[sessionImageDataRefField] = ref
			}
		}
		for _, child := range item {
			if err := stageExternalSessionImageDataInto(child, stateDir, plans, byPath); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range item {
			if err := stageExternalSessionImageDataInto(child, stateDir, plans, byPath); err != nil {
				return err
			}
		}
	}
	return nil
}

func persistSessionImagePlans(plans []sessionImageWritePlan) error {
	for _, plan := range plans {
		if err := persistSessionImagePlan(plan); err != nil {
			return err
		}
	}
	return nil
}

func persistSessionImagePlan(plan sessionImageWritePlan) error {
	if verified, ok := sessionImageVerified(plan.path); ok {
		info, err := os.Lstat(plan.path)
		switch {
		case err == nil && !info.Mode().IsRegular():
			return fmt.Errorf("session image ref %s is not a regular file", plan.ref)
		case err == nil && info.Size() == verified.size && info.ModTime().UnixNano() == verified.modTimeUnixNano:
			return nil
		case err != nil && !os.IsNotExist(err):
			return err
		}
	}
	dirExisted := true
	if _, err := os.Stat(plan.dir); os.IsNotExist(err) {
		dirExisted = false
	} else if err != nil {
		return err
	}
	if err := os.MkdirAll(plan.dir, 0o700); err != nil {
		return err
	}
	if !dirExisted {
		if err := syncDirectory(filepath.Dir(plan.dir)); err != nil {
			return err
		}
	}
	if info, err := os.Lstat(plan.path); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("session image ref %s is not a regular file", plan.ref)
		}
		existing, readErr := os.ReadFile(plan.path)
		if readErr != nil {
			return readErr
		}
		if bytes.Equal(existing, []byte(plan.data)) {
			markSessionImageVerified(plan.path, info)
			return nil
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	tmp, err := os.CreateTemp(plan.dir, ".image-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(plan.data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, plan.path); err != nil {
		return err
	}
	if err := syncDirectory(plan.dir); err != nil {
		return err
	}
	info, err := os.Lstat(plan.path)
	if err != nil {
		return err
	}
	markSessionImageVerified(plan.path, info)
	return nil
}

// verifiedSessionImages remembers content-addressed image paths this process has
// already written or byte-verified. Cache hits still Lstat and compare the
// durable file identity, so a deleted/replaced image can never leave a new
// canonical snapshot pointing at absent bytes.
var verifiedSessionImages sync.Map

type verifiedSessionImage struct {
	size            int64
	modTimeUnixNano int64
}

func sessionImageVerified(path string) (verifiedSessionImage, bool) {
	value, ok := verifiedSessionImages.Load(path)
	if !ok {
		return verifiedSessionImage{}, false
	}
	verified, ok := value.(verifiedSessionImage)
	return verified, ok
}

func markSessionImageVerified(path string, info os.FileInfo) {
	verifiedSessionImages.Store(path, verifiedSessionImage{
		size: info.Size(), modTimeUnixNano: info.ModTime().UnixNano(),
	})
}

func syncDirectory(path string) error {
	// Windows does not support Sync on an os.File opened as a directory; its
	// rename durability boundary is provided by the filesystem/MoveFileEx path.
	if runtime.GOOS == "windows" {
		return nil
	}
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

func rehydrateExternalSessionImages(value any, stateDir string) error {
	var warnings []error
	var visit func(any)
	visit = func(value any) {
		switch item := value.(type) {
		case map[string]any:
			if ref := fieldString(item, sessionImageDataRefField); ref != "" {
				data, err := readExternalSessionImage(ref, stateDir)
				if err != nil {
					// Keep the surrounding message/event and its metadata, but
					// make this image inert so downstream validation/rendering
					// cannot mistake a broken sidecar for usable payload data.
					delete(item, sessionImageDataRefField)
					delete(item, "data")
					warnings = append(warnings, err)
				} else {
					item["data"] = data
					delete(item, sessionImageDataRefField)
				}
			}
			for _, child := range item {
				visit(child)
			}
		case []any:
			for _, child := range item {
				visit(child)
			}
		}
	}
	visit(value)
	return errors.Join(warnings...)
}

func readExternalSessionImage(ref, stateDir string) (string, error) {
	name, path, _, err := externalSessionImageInfo(ref, stateDir)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read session image ref %s: %w", ref, err)
	}
	sum := sha256.Sum256(data)
	if fmt.Sprintf("%x", sum[:]) != name {
		return "", fmt.Errorf("session image ref %s failed content verification", ref)
	}
	return string(data), nil
}

func externalSessionImageInfo(ref, stateDir string) (name, path string, info os.FileInfo, err error) {
	name, ok := validSessionImageRef(ref)
	if !ok {
		return "", "", nil, fmt.Errorf("invalid session image ref %q", ref)
	}
	path = filepath.Join(stateDir, sessionImageDirname, name)
	info, err = os.Lstat(path)
	if err != nil {
		return "", "", nil, fmt.Errorf("read session image ref %s: %w", ref, err)
	}
	if !info.Mode().IsRegular() || info.Size() > maxPersistedSessionImageBytes {
		return "", "", nil, fmt.Errorf("session image ref %s is not a bounded regular file", ref)
	}
	return name, path, info, nil
}

func dropUnreadableExternalSessionImageRefs(value any, stateDir string) error {
	var warnings []error
	var visit func(any)
	visit = func(value any) {
		switch item := value.(type) {
		case map[string]any:
			if ref := fieldString(item, sessionImageDataRefField); ref != "" {
				if _, _, _, err := externalSessionImageInfo(ref, stateDir); err != nil {
					delete(item, sessionImageDataRefField)
					delete(item, "data")
					warnings = append(warnings, err)
				}
			}
			for _, child := range item {
				visit(child)
			}
		case []any:
			for _, child := range item {
				visit(child)
			}
		}
	}
	visit(value)
	return errors.Join(warnings...)
}

// validateExternalSessionImageRefs checks the ref graph without reading image
// payloads. Content-addressed writes verify bytes before publication; boot only
// needs to prove that every ref still names a bounded regular file. The
// expensive byte read and digest verification stay on the wire/archive
// materialization paths, outside the streaming mutex.
func validateExternalSessionImageRefs(value any, stateDir string) error {
	switch item := value.(type) {
	case map[string]any:
		if ref := fieldString(item, sessionImageDataRefField); ref != "" {
			if _, _, _, err := externalSessionImageInfo(ref, stateDir); err != nil {
				return err
			}
		}
		if inlineSessionImage(item) {
			return errors.New("ref-native session mirror retained inline image data")
		}
		for _, child := range item {
			if err := validateExternalSessionImageRefs(child, stateDir); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range item {
			if err := validateExternalSessionImageRefs(child, stateDir); err != nil {
				return err
			}
		}
	}
	return nil
}

func validSessionImageRef(ref string) (string, bool) {
	const prefix = sessionImageDirname + "/"
	if !strings.HasPrefix(ref, prefix) {
		return "", false
	}
	name := strings.TrimPrefix(ref, prefix)
	if len(name) != 64 {
		return "", false
	}
	for _, char := range name {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return "", false
		}
	}
	return name, true
}

func sweepOrphanedSessionImages(stateDir string, snapshot map[string]any, now time.Time, grace time.Duration) (int, error) {
	live := map[string]struct{}{}
	collectExternalSessionImageRefs(snapshot, live)
	// Archives intentionally own inline JSONL image payloads and therefore make
	// no claim on state/images. Dropped-chat recovery and stream journals remain
	// ref-native, so conservatively retain every ref-shaped token they contain,
	// even in a quarantined or partially malformed journal.
	for _, dirname := range []string{droppedChatDirname, sessionJournalDirname} {
		root := filepath.Join(stateDir, dirname)
		err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				return nil
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("refusing session image claim symlink %s", path)
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("refusing non-regular session image claim source %s", path)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			collectExternalSessionImageRefsFromBytes(data, live)
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			// A claim source that cannot be read makes deletion unsafe.
			return 0, fmt.Errorf("scan session image claims in %s: %w", dirname, err)
		}
	}

	imageDir := filepath.Join(stateDir, sessionImageDirname)
	entries, err := os.ReadDir(imageDir)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, entry := range entries {
		name := entry.Name()
		if _, valid := validSessionImageRef(sessionImageDirname + "/" + name); !valid {
			continue
		}
		if _, claimed := live[name]; claimed {
			continue
		}
		path := filepath.Join(imageDir, name)
		info, err := os.Lstat(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return removed, err
		}
		if !info.Mode().IsRegular() || now.Before(info.ModTime().Add(grace)) {
			continue
		}
		if err := os.Remove(path); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return removed, err
		}
		verifiedSessionImages.Delete(path)
		removed++
	}
	return removed, nil
}

func collectExternalSessionImageRefs(value any, live map[string]struct{}) {
	switch item := value.(type) {
	case map[string]any:
		if name, ok := validSessionImageRef(fieldString(item, sessionImageDataRefField)); ok {
			live[name] = struct{}{}
		}
		for _, child := range item {
			collectExternalSessionImageRefs(child, live)
		}
	case []any:
		for _, child := range item {
			collectExternalSessionImageRefs(child, live)
		}
	}
}

func collectExternalSessionImageRefsFromBytes(data []byte, live map[string]struct{}) {
	prefix := []byte(sessionImageDirname + "/")
	for offset := 0; offset < len(data); {
		index := bytes.Index(data[offset:], prefix)
		if index < 0 {
			return
		}
		start := offset + index
		end := start + len(prefix) + 64
		if end <= len(data) {
			ref := string(data[start:end])
			if name, ok := validSessionImageRef(ref); ok {
				live[name] = struct{}{}
			}
		}
		offset = start + len(prefix)
	}
}

func rehydrateSessionEventImageRefs(snapshot map[string]any) error {
	var warnings []error
	for _, rawChat := range anySlice(snapshot["chats"]) {
		if err := rehydrateSessionEventImageRefsInMessages(messageSlice(mapFromAnyMain(rawChat))); err != nil {
			warnings = append(warnings, err)
		}
	}
	return errors.Join(warnings...)
}

func rehydrateSessionEventImageRefsInMessages(messages []any) error {
	var warnings []error
	for _, rawMessage := range messages {
		message := mapFromAnyMain(rawMessage)
		messageImages := anySlice(message["images"])
		for _, rawEvent := range anySlice(message["events"]) {
			event := mapFromAnyMain(rawEvent)
			images := anySlice(event["images"])
			for index, rawImage := range images {
				ref := mapFromAnyMain(rawImage)
				messageIndex, present := intFieldPresent(ref, sessionMessageImageRefField)
				if !present {
					continue
				}
				if messageIndex < 0 || messageIndex >= len(messageImages) {
					delete(ref, sessionMessageImageRefField)
					delete(ref, "data")
					warnings = append(warnings, fmt.Errorf("session event image ref %d is outside its owning message", messageIndex))
					continue
				}
				images[index] = cloneJSON(messageImages[messageIndex])
			}
			if len(images) > 0 {
				event["images"] = images
			}
		}
	}
	return errors.Join(warnings...)
}

func dropOutOfRangeSessionEventImageRefs(snapshot map[string]any) error {
	if snapshot == nil {
		return nil
	}
	var warnings []error
	for _, rawChat := range anySlice(snapshot["chats"]) {
		for _, rawMessage := range messageSlice(mapFromAnyMain(rawChat)) {
			message := mapFromAnyMain(rawMessage)
			messageImages := anySlice(message["images"])
			for _, rawEvent := range anySlice(message["events"]) {
				for _, rawImage := range anySlice(mapFromAnyMain(rawEvent)["images"]) {
					ref := mapFromAnyMain(rawImage)
					messageIndex, present := intFieldPresent(ref, sessionMessageImageRefField)
					if !present || (messageIndex >= 0 && messageIndex < len(messageImages)) {
						continue
					}
					delete(ref, sessionMessageImageRefField)
					delete(ref, "data")
					warnings = append(warnings, fmt.Errorf("session event image ref %d is outside its owning message", messageIndex))
				}
			}
		}
	}
	return errors.Join(warnings...)
}

func validateSessionEventImageRefs(snapshot map[string]any) error {
	for _, rawChat := range anySlice(snapshot["chats"]) {
		for _, rawMessage := range messageSlice(mapFromAnyMain(rawChat)) {
			message := mapFromAnyMain(rawMessage)
			messageImages := anySlice(message["images"])
			for _, rawEvent := range anySlice(message["events"]) {
				for _, rawImage := range anySlice(mapFromAnyMain(rawEvent)["images"]) {
					messageIndex, present := intFieldPresent(mapFromAnyMain(rawImage), sessionMessageImageRefField)
					if present && (messageIndex < 0 || messageIndex >= len(messageImages)) {
						return fmt.Errorf("session event image ref %d is outside its owning message", messageIndex)
					}
				}
			}
		}
	}
	return nil
}

func validateRefNativeSessionImages(snapshot map[string]any, stateDir string) error {
	if err := validateExternalSessionImageRefs(snapshot, stateDir); err != nil {
		return err
	}
	return validateSessionEventImageRefs(snapshot)
}

func materializeSessionSnapshotForWire(snapshot map[string]any, stateDir string) error {
	imageWarning := rehydrateExternalSessionImages(snapshot, stateDir)
	eventWarning := rehydrateSessionEventImageRefs(snapshot)
	return errors.Join(imageWarning, eventWarning)
}

func materializeSessionMessagesForArchive(messages []any, stateDir string) error {
	imageWarning := rehydrateExternalSessionImages(messages, stateDir)
	eventWarning := rehydrateSessionEventImageRefsInMessages(messages)
	return errors.Join(imageWarning, eventWarning)
}

func redactedSessionString(value string) string {
	return stringValue(redactSessionValue(strings.TrimSpace(value)))
}

func (s *sessionStore) persistSnapshot(seq uint64, data []byte, images []sessionImageWritePlan) error {
	s.persistMu.Lock()
	defer s.persistMu.Unlock()
	if seq <= s.writtenSeq {
		return nil
	}
	if len(images) > 0 {
		if err := persistSessionImagePlans(images); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".session-state-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return err
	}
	if err := syncDirectory(filepath.Dir(s.path)); err != nil {
		return err
	}
	s.writtenSeq = seq
	return nil
}

func applySessionAcpEventAt(message, event map[string]any, at int) {
	kind := fieldString(event, "kind")
	events := anySlice(message["events"])
	switch kind {
	case "thinking":
		for _, raw := range events {
			item := mapFromAnyMain(raw)
			if fieldString(item, "kind") == "thinking" {
				item["text"] = stringValue(event["text"])
				message["events"] = events
				return
			}
		}
		events = append(events, map[string]any{"key": nextSessionID("t"), "at": at, "kind": "thinking", "text": stringValue(event["text"])})
	case "plan":
		for _, raw := range events {
			item := mapFromAnyMain(raw)
			if fieldString(item, "kind") == "plan" {
				item["entries"] = event["entries"]
				message["events"] = events
				return
			}
		}
		events = append(events, map[string]any{"key": nextSessionID("p"), "at": at, "kind": "plan", "entries": event["entries"]})
	case "tool":
		for _, raw := range events {
			item := mapFromAnyMain(raw)
			if fieldString(item, "kind") != "tool" || !sameToolEvent(item, event) {
				continue
			}
			// Subagent linkage (subagentId/subagentLabel/subagentProvider) is carried
			// through so the authoritative mirror keeps nesting on reload/replay/
			// multi-device — without these the renderer would collapse to flat.
			for _, key := range []string{"status", "output", "command", "terminalId", "location", "toolKind", "images", "subagentId", "subagentLabel", "subagentProvider", "subagentModel", "subagentHeader"} {
				if key == "images" {
					if images := anySlice(event[key]); len(images) > 0 {
						item[key] = cloneJSON(images)
					}
					continue
				}
				if event[key] != nil && stringValue(event[key]) != "" {
					item[key] = event[key]
				}
			}
			message["events"] = events
			return
		}
		item := map[string]any{"key": nextSessionID("tc"), "at": at, "kind": "tool"}
		for _, key := range []string{"id", "toolKind", "title", "status", "command", "terminalId", "input", "output", "location", "images"} {
			item[key] = event[key]
		}
		// Preserve subagent linkage on the mirror only when present (main-thread
		// tool events omit it), so historical/replayed turns still nest.
		for _, key := range []string{"subagentId", "subagentLabel", "subagentProvider", "subagentModel", "subagentHeader"} {
			if event[key] != nil && stringValue(event[key]) != "" {
				item[key] = event[key]
			}
		}
		events = append(events, item)
	}
	message["events"] = events
}

func sameToolEvent(left, right map[string]any) bool {
	leftID, rightID := fieldString(left, "id"), fieldString(right, "id")
	if leftID != "" && rightID != "" {
		return leftID == rightID
	}
	leftTerminal, rightTerminal := fieldString(left, "terminalId"), fieldString(right, "terminalId")
	return leftTerminal != "" && rightTerminal != "" && leftTerminal == rightTerminal
}

func newSessionMessage(role, content, status string, at any) map[string]any {
	prefix := "a"
	if role == "user" {
		prefix = "u"
	}
	return map[string]any{
		"id": nextSessionID(prefix), "role": role, "content": content,
		"status": status, "at": at, "events": []any{},
	}
}

func nextSessionID(prefix string) string {
	return fmt.Sprintf("srv-%s-%x-%x", prefix, time.Now().UnixNano(), sessionIDSeq.Add(1))
}

func messageSlice(chat map[string]any) []any { return anySlice(chat["messages"]) }

func anySlice(raw any) []any {
	items, _ := raw.([]any)
	if items == nil {
		return []any{}
	}
	return items
}

func stringValue(raw any) string {
	if raw == nil {
		return ""
	}
	return fmt.Sprint(raw)
}

func intValue(raw any) int {
	switch value := raw.(type) {
	case json.Number:
		var out int
		_, _ = fmt.Sscan(value.String(), &out)
		return out
	case float64:
		return int(value)
	case int:
		return value
	default:
		var out int
		_, _ = fmt.Sscan(strings.TrimSpace(fmt.Sprint(raw)), &out)
		return out
	}
}

func cloneJSON(raw any) any {
	if raw == nil {
		return nil
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return raw
	}
	var out any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		return raw
	}
	return out
}

// redactSessionValue scrubs secrets out of a freshly decoded tree (a wire frame,
// an MCP argument, or a file just read from disk) and returns a fresh tree.
//
// It deliberately rebuilds every node rather than sharing untouched subtrees
// with its input: callers such as Save go on to mutate the result, and sharing
// would reach back into the caller's payload. Sharing measured 22 ms cheaper on
// the user's real mirror, but that work runs before the mutex is taken, so it
// buys nothing on the streaming path and is not worth the aliasing hazard. The
// cost that mattered — 404 ms of regex over multi-megabyte payloads — is gone
// via acp.MayContainSecret, which skips text that provably cannot match.
func redactSessionValue(raw any) any {
	switch value := raw.(type) {
	case string:
		return acp.RedactSensitiveText(value)
	case map[string]any:
		out := make(map[string]any, len(value))
		for key, item := range value {
			if acp.MayContainSecret(key) && secretKeyRE.MatchString(key) {
				out[key] = "[redacted]"
			} else {
				out[key] = redactSessionValue(item)
			}
		}
		return out
	case []any:
		out := make([]any, len(value))
		for i, item := range value {
			out[i] = redactSessionValue(item)
		}
		return out
	default:
		return value
	}
}

// unchangedNode reports whether cloneSessionImagePaths left a child untouched,
// so its parent can be shared instead of copied. Maps and slices are compared by
// identity (the rewriter returns the input itself when unchanged); strings by
// value; everything else is returned verbatim.
func unchangedNode(before, after any) bool {
	switch original := before.(type) {
	case string:
		replaced, ok := after.(string)
		return ok && replaced == original
	case map[string]any:
		replaced, ok := after.(map[string]any)
		if !ok || len(replaced) != len(original) {
			return false
		}
		return reflect.ValueOf(replaced).Pointer() == reflect.ValueOf(original).Pointer()
	case []any:
		replaced, ok := after.([]any)
		if !ok || len(replaced) != len(original) {
			return false
		}
		if len(original) == 0 {
			return true
		}
		return &replaced[0] == &original[0]
	default:
		return true
	}
}

func shallowCopyStringMap(value map[string]any) map[string]any {
	out := make(map[string]any, len(value)+4)
	for key, item := range value {
		out[key] = item
	}
	return out
}
