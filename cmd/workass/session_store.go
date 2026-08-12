package main

import (
	"bytes"
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"hash/maphash"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf16"

	"workass/internal/acp"
	"workass/internal/durablefs"
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

func sharedSessionStore(stateDir string) *sessionStore {
	stateDir = strings.TrimSpace(stateDir)
	if stateDir == "" {
		return &sessionStore{droppedChatIDs: map[string]struct{}{}}
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
		if store.snapshot != nil {
			internalChatsPruned = pruneLegacyInternalChats(store.snapshot)
		}
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
	tombstonesPruned := pruneLegacyTombstonedChats(store.snapshot, store.droppedChatIDs)
	assistantImagesMigrated := migrateAssistantMarkdownImages(store.snapshot)

	planLatestMigrated := migratePlanLatest(store.snapshot, filepath.Dir(store.path))
	agentQueueEchoesMigrated := migrateAgentQueueEchoes(store.snapshot)
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
	if needsRewrite || tombstonesPruned || internalChatsPruned || assistantImagesMigrated || terminalToolsMigrated || planLatestMigrated || agentQueueEchoesMigrated {
		if err := persistLegacySnapshot(store); err != nil {
			store.loadErr = err
			return store
		}
	}
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

func pruneLegacyTombstonedChats(snapshot map[string]any, droppedChatIDs map[string]struct{}) bool {
	if snapshot == nil || len(droppedChatIDs) == 0 {
		return false
	}
	chats := anySlice(snapshot["chats"])
	kept := make([]any, 0, len(chats))
	changed := false
	for _, raw := range chats {
		tabID := fieldString(mapFromAnyMain(raw), "id")
		if _, deleted := droppedChatIDs[tabID]; deleted {
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
		if _, deleted := droppedChatIDs[activeID]; deleted {
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

func (s *sessionStore) PersistProviderAttachments(images []any) ([]providercontract.Attachment, error) {
	plan, err := s.PlanProviderAttachments(images)
	if err != nil {
		return nil, err
	}
	if err := plan.Materialize(); err != nil {
		return nil, err
	}
	return append([]providercontract.Attachment(nil), plan.Attachments...), nil
}

// providerAttachmentPlan separates immutable attachment identity from local
// content-addressed materialization. Actor-owned commands validate and accept
// the identity first, then materialize these writes inside the engine's
// prepared-commit boundary before the actor snapshot is published.
type providerAttachmentPlan struct {
	Attachments []providercontract.Attachment
	writes      []sessionImageWritePlan
	stateDir    string
}

func (p providerAttachmentPlan) Materialize() error {
	if err := persistSessionImagePlans(p.writes); err != nil {
		return err
	}
	owner := map[string]any{"images": make([]any, 0, len(p.Attachments))}
	for _, attachment := range p.Attachments {
		ref := strings.TrimPrefix(strings.TrimSpace(attachment.Ref), providerSessionImageRefPrefix)
		owner["images"] = append(owner["images"].([]any), map[string]any{
			"mimeType": attachment.MIMEType, sessionImageDataRefField: ref,
		})
	}
	return validateExternalSessionImageRefs(owner, p.stateDir)
}

func (s *sessionStore) PlanProviderAttachments(images []any) (providerAttachmentPlan, error) {
	if len(images) == 0 {
		return providerAttachmentPlan{}, nil
	}
	if s == nil || !s.enabled() {
		return providerAttachmentPlan{}, errors.New("durable session image storage is unavailable")
	}
	copyValue := cloneJSON(images)
	copyImages, ok := copyValue.([]any)
	if !ok {
		return providerAttachmentPlan{}, errors.New("provider attachments are not an image list")
	}
	stateDir := filepath.Dir(s.path)
	owner := map[string]any{"images": copyImages}
	writes, err := stageExternalSessionImageData(owner, stateDir)
	if err != nil {
		return providerAttachmentPlan{}, err
	}
	copyImages = anySlice(owner["images"])
	attachments := make([]providercontract.Attachment, 0, len(copyImages))
	for index, raw := range copyImages {
		image := mapFromAnyMain(raw)
		ref := fieldString(image, sessionImageDataRefField)
		mimeType := strings.ToLower(strings.TrimSpace(fieldString(image, "mimeType")))
		if ref == "" || !strings.HasPrefix(mimeType, "image/") {
			return providerAttachmentPlan{}, fmt.Errorf("provider image %d is missing durable content or MIME identity", index)
		}
		name := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(ref)), sessionImageDirname+"/")
		var size int64
		planned := false
		for _, write := range writes {
			if filepath.Clean(write.ref) == filepath.Clean(ref) {
				size = int64(len(write.data))
				planned = true
				break
			}
		}
		if !planned {
			var info os.FileInfo
			var infoErr error
			name, _, info, infoErr = externalSessionImageInfo(ref, stateDir)
			if infoErr != nil {
				return providerAttachmentPlan{}, infoErr
			}
			size = info.Size()
		}
		if len(name) < 16 || strings.Contains(name, "/") {
			return providerAttachmentPlan{}, fmt.Errorf("provider image %d has invalid durable content identity", index)
		}
		attachmentName := firstNonEmptyString(fieldString(image, "name"), fmt.Sprintf("image-%d", index+1))
		attachments = append(attachments, providercontract.Attachment{
			ID: "image-" + name[:16], Name: attachmentName, MIMEType: mimeType,
			Digest: name, Size: size, Ref: providerSessionImageRefPrefix + ref,
		})
	}
	return providerAttachmentPlan{Attachments: attachments, writes: writes, stateDir: stateDir}, nil
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
func pruneLegacyInternalChats(snapshot map[string]any) bool {
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

// persistLegacySnapshot is used only while the one-time actor cutover still
// consumes session-state.json. Runtime chat mutations never write this file.
func persistLegacySnapshot(s *sessionStore) error {
	if !s.enabled() {
		return os.ErrInvalid
	}
	data, err := json.Marshal(s.snapshot)
	if err != nil {
		return err
	}
	seq := s.persistSeq + 1
	if err := s.persistSnapshot(seq, data, nil); err != nil {
		return err
	}
	s.persistSeq = seq
	return nil
}

// inlineSessionImage reports whether a map is an image node still holding
// base64 data that must be externalized before publication.
func inlineSessionImage(item map[string]any) bool {
	return strings.HasPrefix(strings.ToLower(fieldString(item, "mimeType")), "image/") && fieldString(item, "data") != ""
}

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
	return durablefs.SyncDirectory(path)
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
