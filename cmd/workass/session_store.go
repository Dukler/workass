package main

import (
	"bufio"
	"bytes"
	"container/list"
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
)

const (
	sessionStateFilename          = "session-state.json"
	sessionStreamPersistInterval  = 100 * time.Millisecond
	sessionGetArchiveFenceTimeout = 100 * time.Millisecond
	sessionSaveMaxAttempts        = 4
	sessionJournalDirname         = ".session-stream"
	sessionJournalQuarantineDir   = "quarantine"
	droppedChatDirname            = "dropped-chats"
	sessionImageDirname           = "images"
	sessionImageDataRefField      = "_workassImageDataRef"
	sessionMessageImageRefField   = "_workassMessageImageRef"
	maxPersistedSessionImageBytes = 64 * 1024 * 1024
	sessionImageGCGrace           = 24 * time.Hour
	sessionJournalVersion         = 1
	workspaceRevisionField        = "workspaceRevision"
	agentQueueRevisionField       = "agentQueueRevision"
	runtimeControlRevisionField   = "runtimeControlRevision"
	agentQueueMessageField        = "agentQueueId"
	hostQueueOriginsField         = "hostQueueOrigins"
	hostQueueOriginUserField      = "originUserId"
	hostQueueOriginAssistantField = "originAssistantId"
	adoptedQueueLedgerField       = "adoptedQueueIds"
	rendererQueueObservedField    = "observedAt"
	queueParkedField              = "parked"
	retainedQueueLedgerEntries    = 256
)

var (
	sessionStoresMu     sync.Mutex
	sessionStores       = map[string]*sessionStore{}
	sessionIDSeq        atomic.Uint64
	sessionStoreLogLine = func(line string) { fmt.Fprintln(os.Stderr, line) }
	errQueueRowAdopted  = errors.New("originating queue row was already adopted")
	errInternalJournal  = errors.New("ignored legacy internal subagent journal")
)

// sessionStore owns the renderer Mirror v1 snapshot. The renderer can still
// submit UI state through session:save, but live turns are merged from daemon
// job events so a disconnected renderer cannot lose their output.
type sessionStore struct {
	mu   sync.Mutex
	path string
	// published is the linearizable renderer mirror. A generation root and every
	// map/slice reachable from it are immutable after Store. snapshot and
	// generation remain package-local compatibility aliases for the existing
	// tests and helpers; while idle they point at the exact published root, and a
	// mutation swaps snapshot to its private path-copy until commit.
	published             atomic.Pointer[sessionGeneration]
	snapshot              map[string]any
	generation            *sessionGeneration
	mutation              *sessionMutation
	generationSeq         uint64
	stateRevision         uint64
	jobs                  map[string]*sessionJob
	pending               []*sessionJob
	jobOrder              []string
	loadErr               error
	streamPersistInterval time.Duration
	persistTimer          *time.Timer
	persistDirty          bool
	persistSeq            uint64
	journals              map[string]*sessionJournal
	archivePending        map[string]chan struct{}
	archivePendingTabs    map[string]string
	droppedChatIDs        map[string]struct{}
	writeJournalRecord    func(*os.File, []byte) error
	queueWake             func(tabID, chatID string)
	// beforePersistImages is a store-local test barrier. Production leaves it
	// nil; keeping it per store avoids cross-test/global interference.
	beforePersistImages func()
	// beforeGetRehydrate is the equivalent session:get barrier: the ref-native
	// snapshot has been detached and mu has been released, but image files have
	// not been read yet. Production leaves it nil.
	beforeGetRehydrate func()
	// beforeGenerationMarshal proves that immutable-generation serialization is
	// outside mu. Production leaves it nil.
	beforeGenerationMarshal func()
	// afterPersistTombstones is the deletion-CAS test seam between durable
	// tombstone IO and generation revalidation. Production leaves it nil.
	afterPersistTombstones func()
	// beforeSaveCAS is the ordinary-save equivalent: candidate construction is
	// complete and no store mutex is held. Production leaves it nil.
	beforeSaveCAS func()
	// saveStageObserver receives one complete, additive lock-stage receipt.
	// Production leaves it nil; the real-state cost harness uses it to prove its
	// breakdown sums to the measured mutex hold.
	saveStageObserver func(sessionSaveStageReceipt)

	persistMu  sync.Mutex
	journalMu  sync.Mutex
	writtenSeq uint64

	// streamLock measures how long an incoming provider chunk waited for mu, and
	// saveLock how long a renderer save held it. Invoke latency alone could not
	// distinguish "the daemon is slow to hand text over" from "the agent is slow
	// to produce it"; these two do.
	streamLock lockStat
	saveLock   lockStat
	getLock    lockStat

	snapshotByteEstimate atomic.Int64
	wireByteEstimate     atomic.Int64
}

type sessionSaveStageReceipt struct {
	Stages map[string]time.Duration
	Held   time.Duration
}

func (s *sessionStore) RefreshGeneration() uint64 {
	if s == nil || !s.enabled() {
		return 0
	}
	if generation := s.publishedGeneration(); generation != nil {
		return generation.stateRevision
	}
	return 0
}

// sessionGeneration is one immutable, ref-native renderer mirror publication.
// Indexes belong to the same root and make Save validation/merge linear rather
// than repeatedly scanning every chat for every incoming row.
type sessionGeneration struct {
	root             map[string]any
	chatPositions    map[string]int
	messagePositions map[string]map[string]int
	chatsByTab       map[string]map[string]any
	messagesByTab    map[string]map[string]map[string]any
	number           uint64
	stateRevision    uint64
	persistenceSeq   uint64
	saveRebaseEpoch  uint64
	pendingArchives  []sessionArchiveFence
	ownedTurns       []sessionOwnedTurn
	rendererIntent   *rendererIntentIndex
}

type sessionArchiveFence struct {
	tabID string
	done  chan struct{}
}

type sessionOwnedTurn struct {
	id              string
	tabID           string
	userID          string
	assistantID     string
	rootAssistantID string
}

// sessionMutation owns one private path-copy rooted at base. root and the
// chats slice are copied eagerly because every renderer mutation reaches a
// chat or a root field. Chat/message containers are copied only when addressed.
type sessionMutation struct {
	store            *sessionStore
	base             *sessionGeneration
	root             map[string]any
	chatPositions    map[string]int
	writableChats    map[int]map[string]any
	writableMessages map[string]map[string]any
	saveRebaseSafe   bool
	changed          bool
}

func newSessionGeneration(root map[string]any, number, stateRevision, persistenceSeq uint64) *sessionGeneration {
	generation := &sessionGeneration{
		root:           root,
		number:         number,
		stateRevision:  stateRevision,
		persistenceSeq: persistenceSeq,
	}
	generation.chatPositions, generation.messagePositions = buildSessionPositionIndexes(root)
	return generation
}

func buildSessionPositionIndexes(root map[string]any) (map[string]int, map[string]map[string]int) {
	chats := make(map[string]int)
	messages := make(map[string]map[string]int)
	if root == nil {
		return chats, messages
	}
	for chatIndex, rawChat := range anySlice(root["chats"]) {
		chat := mapFromAnyMain(rawChat)
		tabID := fieldString(chat, "id")
		if tabID == "" {
			continue
		}
		chats[tabID] = chatIndex
		byID := make(map[string]int)
		for messageIndex, rawMessage := range messageSlice(chat) {
			if id := fieldString(mapFromAnyMain(rawMessage), "id"); id != "" {
				byID[id] = messageIndex
			}
		}
		messages[tabID] = byID
	}
	return chats, messages
}

// newIndexedSessionGeneration is Save-local. Published generations deliberately
// omit these indexes until the read path consumes them; building them at boot
// and every publication only duplicated work and retained more containers.
func newIndexedSessionGeneration(root map[string]any, number, persistenceSeq uint64) *sessionGeneration {
	generation := newSessionGeneration(root, number, number, persistenceSeq)
	generation.chatsByTab = make(map[string]map[string]any)
	generation.messagesByTab = make(map[string]map[string]map[string]any)
	for _, rawChat := range anySlice(root["chats"]) {
		chat := mapFromAnyMain(rawChat)
		tabID := fieldString(chat, "id")
		if tabID == "" {
			continue
		}
		generation.chatsByTab[tabID] = chat
		messages := make(map[string]map[string]any)
		for _, rawMessage := range messageSlice(chat) {
			message := mapFromAnyMain(rawMessage)
			if id := fieldString(message, "id"); id != "" {
				messages[id] = message
			}
		}
		generation.messagesByTab[tabID] = messages
	}
	return generation
}

// cloneSessionContainers detaches every mutable JSON container while sharing
// scalar strings/numbers/bools. Unlike cloneJSON it never marshals or copies
// multi-megabyte payload bytes. It is used once per published renderer save to
// keep s.snapshot mutable without aliasing the immutable generation.
func cloneSessionContainers(value any) any {
	switch item := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(item))
		for key, child := range item {
			// These subtrees are daemon-owned immutable values. Mutation paths
			// replace the owning field/array; they never edit an image or tool
			// payload in place. Preserve them across the compatibility working
			// root so the next generation can share them without reconstructing
			// multi-megabyte payloads.
			if key == "images" || (fieldString(item, "kind") == "tool" && (key == "input" || key == "output")) {
				out[key] = child
				continue
			}
			out[key] = cloneSessionContainers(child)
		}
		return out
	case []any:
		out := make([]any, len(item))
		for index, child := range item {
			out[index] = cloneSessionContainers(child)
		}
		return out
	default:
		return value
	}
}

func (s *sessionStore) publishedGeneration() *sessionGeneration {
	if s == nil {
		return nil
	}
	return s.published.Load()
}

func (s *sessionStore) beginSessionMutationLocked() *sessionMutation {
	if s == nil {
		return nil
	}
	if s.mutation != nil {
		return s.mutation
	}
	base := s.publishedGeneration()
	root := s.snapshot
	if base != nil {
		root = base.root
	}
	if root == nil {
		root = map[string]any{
			"v": json.Number("1"), "activeId": nil,
			"seq": json.Number("0"), "chats": []any{},
		}
	}
	privateRoot := shallowCopyStringMap(root)
	privateRoot["chats"] = append([]any(nil), anySlice(root["chats"])...)
	positions := make(map[string]int)
	if base != nil {
		for tabID, index := range base.chatPositions {
			positions[tabID] = index
		}
	} else {
		for index, raw := range anySlice(privateRoot["chats"]) {
			if tabID := fieldString(mapFromAnyMain(raw), "id"); tabID != "" {
				positions[tabID] = index
			}
		}
	}
	tx := &sessionMutation{
		store: s, base: base, root: privateRoot,
		chatPositions: positions, writableChats: make(map[int]map[string]any),
		writableMessages: make(map[string]map[string]any),
	}
	s.mutation = tx
	s.snapshot = tx.root
	return tx
}

func (tx *sessionMutation) rootForWrite() map[string]any {
	if tx == nil {
		return nil
	}
	tx.changed = true
	return tx.root
}

func (tx *sessionMutation) chatForWrite(tabID string) map[string]any {
	if tx == nil {
		return nil
	}
	index, ok := tx.chatPositions[strings.TrimSpace(tabID)]
	chats := anySlice(tx.root["chats"])
	if !ok || index < 0 || index >= len(chats) {
		return nil
	}
	if chat := tx.writableChats[index]; chat != nil {
		tx.changed = true
		return chat
	}
	source := mapFromAnyMain(chats[index])
	chat := make(map[string]any, len(source))
	for key, value := range source {
		if key != "messages" {
			chat[key] = cloneSessionContainers(value)
			continue
		}
		messages := messageSlice(source)
		copied := make([]any, len(messages))
		for messageIndex, rawMessage := range messages {
			copied[messageIndex] = shallowCopyStringMap(mapFromAnyMain(rawMessage))
		}
		chat[key] = copied
	}
	chats[index] = chat
	tx.root["chats"] = chats
	tx.writableChats[index] = chat
	tx.changed = true
	return chat
}

func (tx *sessionMutation) messageForWrite(tabID, messageID string) map[string]any {
	key := strings.TrimSpace(tabID) + "\x00" + strings.TrimSpace(messageID)
	if message := tx.writableMessages[key]; message != nil {
		tx.changed = true
		return message
	}
	chat := tx.chatForWrite(tabID)
	if chat == nil {
		return nil
	}
	messages := messageSlice(chat)
	index := -1
	if tx.base != nil {
		if byID := tx.base.messagePositions[strings.TrimSpace(tabID)]; byID != nil {
			var exists bool
			index, exists = byID[strings.TrimSpace(messageID)]
			if !exists || index < 0 || index >= len(messages) ||
				fieldString(mapFromAnyMain(messages[index]), "id") != strings.TrimSpace(messageID) {
				index = -1
			}
		}
	}
	if index < 0 {
		for candidate, raw := range messages {
			if fieldString(mapFromAnyMain(raw), "id") == strings.TrimSpace(messageID) {
				index = candidate
				break
			}
		}
	}
	if index < 0 {
		return nil
	}
	message, _ := cloneSessionContainers(mapFromAnyMain(messages[index])).(map[string]any)
	messages[index] = message
	chat["messages"] = messages
	tx.writableMessages[key] = message
	tx.changed = true
	return message
}

func (s *sessionStore) capturedArchiveFencesLocked() []sessionArchiveFence {
	pending := make([]sessionArchiveFence, 0, len(s.archivePending))
	for key, done := range s.archivePending {
		pending = append(pending, sessionArchiveFence{tabID: s.archivePendingTabs[key], done: done})
	}
	return pending
}

func (s *sessionStore) capturedOwnedTurnsLocked() []sessionOwnedTurn {
	traces := append([]*sessionJob(nil), s.pending...)
	for _, id := range s.jobOrder {
		if job := s.jobs[id]; job != nil {
			traces = append(traces, job)
		}
	}
	owned := make([]sessionOwnedTurn, 0, len(traces))
	for _, job := range traces {
		if job == nil || isInternalTabID(job.TabID) {
			continue
		}
		owned = append(owned, sessionOwnedTurn{
			id: job.ID, tabID: job.TabID, userID: job.UserID,
			assistantID: job.AssistantID, rootAssistantID: job.RootAssistantID,
		})
	}
	return owned
}

func (s *sessionStore) commitSessionMutationLocked(tx *sessionMutation) *sessionGeneration {
	if s == nil || tx == nil || s.mutation != tx {
		return s.publishedGeneration()
	}
	base := tx.base
	number, revision := uint64(1), uint64(1)
	if base != nil {
		number = base.number + 1
		revision = base.stateRevision + 1
	}
	s.generationSeq = number
	s.stateRevision = revision
	generation := newSessionGeneration(tx.root, number, revision, s.persistSeq)
	if base != nil {
		generation.saveRebaseEpoch = base.saveRebaseEpoch
		if !tx.saveRebaseSafe {
			generation.saveRebaseEpoch++
		}
	}
	generation.pendingArchives = s.capturedArchiveFencesLocked()
	generation.ownedTurns = s.capturedOwnedTurnsLocked()
	if base != nil {
		generation.rendererIntent = base.rendererIntent
	}
	s.published.Store(generation)
	s.generation = generation
	s.snapshot = generation.root
	s.mutation = nil
	return generation
}

func (s *sessionStore) abortSessionMutationLocked(tx *sessionMutation) {
	if s == nil || tx == nil || s.mutation != tx {
		return
	}
	s.mutation = nil
	if tx.base != nil {
		s.snapshot = tx.base.root
	} else {
		s.snapshot = nil
	}
}

func (s *sessionStore) republishArchiveFencesLocked() *sessionGeneration {
	base := s.publishedGeneration()
	if base == nil {
		return nil
	}
	generation := &sessionGeneration{
		root: base.root, chatPositions: base.chatPositions,
		messagePositions: base.messagePositions,
		number:           base.number + 1, stateRevision: base.stateRevision + 1,
		persistenceSeq: base.persistenceSeq, saveRebaseEpoch: base.saveRebaseEpoch,
		pendingArchives: s.capturedArchiveFencesLocked(),
		ownedTurns:      s.capturedOwnedTurnsLocked(),
		rendererIntent:  base.rendererIntent,
	}
	s.generationSeq, s.stateRevision = generation.number, generation.stateRevision
	s.published.Store(generation)
	s.generation, s.snapshot = generation, generation.root
	return generation
}

// lockStat accumulates mutex wait/hold timings without taking a lock of its own.
type lockStat struct {
	count     atomic.Int64
	waitNanos atomic.Int64
	waitMax   atomic.Int64
	heldNanos atomic.Int64
	heldMax   atomic.Int64
	over50    atomic.Int64
}

func (l *lockStat) observe(wait, held time.Duration) {
	l.count.Add(1)
	l.waitNanos.Add(int64(wait))
	l.heldNanos.Add(int64(held))
	updateAtomicMax(&l.waitMax, int64(wait))
	updateAtomicMax(&l.heldMax, int64(held))
	if wait+held >= 50*time.Millisecond {
		l.over50.Add(1)
	}
}

func (l *lockStat) snapshot() map[string]any {
	count := l.count.Load()
	avg := func(total int64) float64 {
		if count == 0 {
			return 0
		}
		return float64(total) / float64(count) / float64(time.Millisecond)
	}
	ms := func(value int64) float64 { return float64(value) / float64(time.Millisecond) }
	return map[string]any{
		"count":     count,
		"waitAvgMs": avg(l.waitNanos.Load()),
		"waitMaxMs": ms(l.waitMax.Load()),
		"heldAvgMs": avg(l.heldNanos.Load()),
		"heldMaxMs": ms(l.heldMax.Load()),
		"over50ms":  l.over50.Load(),
	}
}

func (l *lockStat) reset() {
	if l == nil {
		return
	}
	l.count.Store(0)
	l.waitNanos.Store(0)
	l.waitMax.Store(0)
	l.heldNanos.Store(0)
	l.heldMax.Store(0)
	l.over50.Store(0)
}

func updateAtomicMax(target *atomic.Int64, value int64) {
	for {
		current := target.Load()
		if value <= current || target.CompareAndSwap(current, value) {
			return
		}
	}
}

type sessionJob struct {
	ID          string
	TabID       string
	ChatID      string
	Prompt      string
	UserID      string
	AssistantID string
	// RootAssistantID identifies the provider-native turn while AssistantID
	// follows the current chronological continuation segment after steering.
	RootAssistantID string
	StartedAt       string
	Finished        bool
	JournalID       string
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

// sessionJournal is an incremental, private recovery sidecar for one live
// turn. The canonical session-state.json and chat archive formats remain
// unchanged. Records are appended in the same order as the in-memory mutation;
// only the small sidecar is synced on the streaming cadence.
type sessionJournal struct {
	turnID string
	path   string
	file   *os.File
	seq    uint64
	dirty  bool
}

type recoveredSessionJournal struct {
	path     string
	turnID   string
	job      *sessionJob
	terminal bool
}

type sessionArchiveWork struct {
	tabID     string
	messages  []any
	job       *sessionJob
	journalOK bool
	done      chan struct{}
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
		path:                  strings.TrimSpace(path),
		jobs:                  map[string]*sessionJob{},
		journals:              map[string]*sessionJournal{},
		droppedChatIDs:        map[string]struct{}{},
		streamPersistInterval: sessionStreamPersistInterval,
		writeJournalRecord:    writeSessionJournalRecord,
	}
	if store.path == "" {
		return store
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
	if normalizeErr == nil {
		store.snapshotByteEstimate.Store(int64(len(normalized)))
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
		store.generationSeq++
		store.stateRevision++
		store.generation = newSessionGeneration(
			store.snapshot, store.generationSeq, store.stateRevision, store.persistSeq,
		)
		store.generation.pendingArchives = store.capturedArchiveFencesLocked()
		store.generation.rendererIntent = buildRendererIntentIndex(store.snapshot)
		store.published.Store(store.generation)
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

func (s *sessionStore) SetQueueWakeFunc(wake func(tabID, chatID string)) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.queueWake = wake
	s.mu.Unlock()
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
	// The store invariant is redacted-at-ingress: load, renderer saves, runtime
	// controls, prepared turns, and job events are scrubbed before insertion.
	// The atomic Load is the read linearization point. The root and captured
	// archive fence slice are immutable, so every expensive operation happens
	// without the streaming mutex.
	data, err := json.Marshal(generation.root)
	activeTabID := fieldString(generation.root, "activeId")
	pendingArchives := make([]chan struct{}, 0, len(generation.pendingArchives))
	for _, fence := range generation.pendingArchives {
		tabID, done := fence.tabID, fence.done
		// session:get paints the active transcript. Inactive chat archives are
		// loaded through chat:archive-load when selected; unknown legacy entries
		// remain conservative and participate in the bounded fence.
		if tabID == "" || (activeTabID != "" && tabID == activeTabID) {
			pendingArchives = append(pendingArchives, done)
		}
	}
	s.getLock.observe(0, 0)
	if err != nil {
		s.recordLoadError(err)
		return nil
	}
	// The canonical mirror was fsynced before its terminal event could be
	// delivered. This bounded, outside-mu fence only gives the active chat's
	// secondary inline archive a chance to catch up.
	if len(pendingArchives) > 0 {
		timer := time.NewTimer(sessionGetArchiveFenceTimeout)
	archiveFence:
		for _, done := range pendingArchives {
			select {
			case <-done:
			case <-timer.C:
				break archiveFence
			}
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}
	if s.beforeGetRehydrate != nil {
		s.beforeGetRehydrate()
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

func (s *sessionStore) recordLoadError(err error) {
	if s == nil || err == nil {
		return
	}
	s.mu.Lock()
	s.loadErr = errors.Join(s.loadErr, err)
	s.mu.Unlock()
}

// GetWithLiveSessions overlays runtime-only ACP bindings on the persisted
// mirror. The overlay is returned to the renderer but never written to disk, so
// a daemon restart cannot resurrect stale session ids.
func (s *sessionStore) GetWithLiveSessions(manager *acp.Manager) any {
	snapshot := s.Get()
	root, ok := snapshot.(map[string]any)
	if !ok || manager == nil {
		return snapshot
	}
	byTab := make(map[string]acp.LiveSession)
	for _, binding := range manager.LiveSessions() {
		if binding.TabID != "" {
			byTab[binding.TabID] = binding
		}
	}
	for _, raw := range anySlice(root["chats"]) {
		chat := mapFromAnyMain(raw)
		if binding, exists := byTab[fieldString(chat, "id")]; exists {
			if binding.ChatID == "" || fieldString(chat, "chatId") == "" || binding.ChatID != fieldString(chat, "chatId") {
				continue
			}
			chat["liveSession"] = binding.Info
		}
	}
	return root
}

// Inventory reports the shape of the in-memory mirror without cloning it. The
// metrics endpoint used store.Get(), which meant asking "why is the daemon busy"
// cost a full multi-megabyte clone of the answer.
func (s *sessionStore) Inventory() map[string]any {
	out := map[string]any{
		"chats": 0, "messages": 0, "events": 0, "messageImages": 0,
		"messageBytes": 0, "inlineImageBytes": 0, "heaviestChat": map[string]any{},
		"streamLock": s.streamLock.snapshot(), "saveLock": s.saveLock.snapshot(),
		"getLock": s.getLock.snapshot(),
	}
	if !s.enabled() {
		return out
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	chats := anySlice(s.snapshot["chats"])
	messages, events, images, textBytes, imageBytes := 0, 0, 0, 0, 0
	heaviest := map[string]any{}
	heaviestEvents := -1
	for _, raw := range chats {
		chat := mapFromAnyMain(raw)
		chatEvents := 0
		for _, rawMessage := range messageSlice(chat) {
			message := mapFromAnyMain(rawMessage)
			messages++
			textBytes += len(toString(message["content"]))
			messageEvents := anySlice(message["events"])
			chatEvents += len(messageEvents)
			images += len(anySlice(message["images"]))
			imageBytes += inlineImageBytes(message["images"])
			for _, rawEvent := range messageEvents {
				imageBytes += inlineImageBytes(mapFromAnyMain(rawEvent)["images"])
			}
		}
		events += chatEvents
		if chatEvents > heaviestEvents {
			heaviestEvents = chatEvents
			heaviest = map[string]any{"id": toString(chat["id"]), "events": chatEvents, "messages": len(messageSlice(chat))}
		}
	}
	out["chats"], out["messages"], out["events"] = len(chats), messages, events
	out["messageImages"], out["messageBytes"] = images, textBytes
	// inlineImageBytes is the multiplier behind every clone and marshal of the
	// mirror: rehydrated screenshots make the live tree far larger than its
	// on-disk form, and each one is paid for on the streaming path.
	out["inlineImageBytes"] = imageBytes
	out["heaviestChat"] = heaviest
	return out
}

func inlineImageBytes(raw any) int {
	total := 0
	for _, item := range anySlice(raw) {
		total += len(fieldString(mapFromAnyMain(item), "data"))
	}
	return total
}

// ChatIdentitySnapshot returns only the tab/chat identity pairs, in the same
// {"chats":[{"id","chatId"}]} shape consumers of the full mirror expect. It
// exists so owner reconciliation never has to clone the entire snapshot.
func (s *sessionStore) ChatIdentitySnapshot() map[string]any {
	if !s.enabled() {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	chats := make([]any, 0, len(anySlice(s.snapshot["chats"])))
	for _, raw := range anySlice(s.snapshot["chats"]) {
		chat := mapFromAnyMain(raw)
		chats = append(chats, map[string]any{
			"id":     fieldString(chat, "id"),
			"chatId": fieldString(chat, "chatId"),
		})
	}
	return map[string]any{"chats": chats}
}

func (s *sessionStore) StateDigest(manager *acp.Manager, catalogHashes map[string]string, settingsRevision, procHash string) map[string]any {
	runtime := map[string]acp.ChatRuntimeDigest{}
	if manager != nil {
		runtime = manager.ChatRuntimeDigests()
	}
	s.mu.Lock()
	chats := make([]any, 0, len(anySlice(s.snapshot["chats"])))
	for _, raw := range anySlice(s.snapshot["chats"]) {
		chat := mapFromAnyMain(raw)
		tabID, chatID := fieldString(chat, "id"), fieldString(chat, "chatId")
		messages := messageSlice(chat)
		lastMessageID := any(nil)
		if len(messages) > 0 {
			if id := fieldString(mapFromAnyMain(messages[len(messages)-1]), "id"); id != "" {
				lastMessageID = id
			}
		}
		queue := anySlice(chat["queue"])
		queueHeadID := any(nil)
		if len(queue) > 0 {
			if id := fieldString(mapFromAnyMain(queue[0]), "id"); id != "" {
				queueHeadID = id
			}
		}
		live := runtime[tabID+"\x00"+chatID]
		runningJobID := any(nil)
		if live.RunningJobID != "" {
			runningJobID = live.RunningJobID
		}
		pendingPermissionIDs := live.PendingPermissionIDs
		if pendingPermissionIDs == nil {
			pendingPermissionIDs = []string{}
		}
		chats = append(chats, map[string]any{
			"tabId": tabID, "chatId": chatID,
			"runningJobId": runningJobID, "lastMessageId": lastMessageID,
			"messageCount": len(messages), "queueLen": len(queue), "queueHeadId": queueHeadID,
			agentQueueRevisionField:     intValue(chat[agentQueueRevisionField]),
			runtimeControlRevisionField: intValue(chat[runtimeControlRevisionField]),
			"providerId":                nullableDigestString(fieldString(chat, "providerId")),
			"currentModelId":            nullableDigestString(fieldString(chat, "currentModelId")),
			"currentModeId":             nullableDigestString(fieldString(chat, "currentModeId")),
			"pendingPermissionIds":      pendingPermissionIDs,
		})
	}
	s.mu.Unlock()
	if catalogHashes == nil {
		catalogHashes = map[string]string{}
	}
	return map[string]any{
		"chats": chats, "catalogHash": catalogHashes,
		"settingsRevision": settingsRevision, "procHash": procHash,
	}
}

func nullableDigestString(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

type rendererIntentFingerprint struct {
	digest    [sha256.Size]byte
	canonical []byte
}

type rendererChatIntent struct {
	chat     rendererIntentFingerprint
	messages map[string]rendererIntentFingerprint
	queue    map[string]rendererIntentFingerprint
}

type rendererIntentIndex struct {
	chats map[string]rendererChatIntent
}

func rendererIntentFingerprintFor(value any) rendererIntentFingerprint {
	canonical, err := json.Marshal(value)
	if err != nil {
		canonical = []byte("null")
	}
	return rendererIntentFingerprint{digest: sha256.Sum256(canonical), canonical: canonical}
}

func rendererIntentEqual(left, right rendererIntentFingerprint) bool {
	return left.digest == right.digest && bytes.Equal(left.canonical, right.canonical)
}

func buildRendererIntentIndex(root map[string]any) *rendererIntentIndex {
	index := &rendererIntentIndex{chats: make(map[string]rendererChatIntent)}
	if root == nil {
		return index
	}
	for _, rawChat := range anySlice(root["chats"]) {
		chat := mapFromAnyMain(rawChat)
		tabID := fieldString(chat, "id")
		if tabID == "" {
			continue
		}
		metadata := make(map[string]any, len(chat))
		for key, value := range chat {
			if key != "messages" && key != "queue" {
				metadata[key] = value
			}
		}
		intent := rendererChatIntent{
			chat:     rendererIntentFingerprintFor(metadata),
			messages: make(map[string]rendererIntentFingerprint),
			queue:    make(map[string]rendererIntentFingerprint),
		}
		for _, rawMessage := range messageSlice(chat) {
			if id := fieldString(mapFromAnyMain(rawMessage), "id"); id != "" {
				intent.messages[id] = rendererIntentFingerprintFor(rawMessage)
			}
		}
		for _, rawQueue := range anySlice(chat["queue"]) {
			if id := fieldString(mapFromAnyMain(rawQueue), "id"); id != "" {
				intent.queue[id] = rendererIntentFingerprintFor(rawQueue)
			}
		}
		index.chats[tabID] = intent
	}
	return index
}

func rendererChatIntentEqual(left, right rendererChatIntent) bool {
	if !rendererIntentEqual(left.chat, right.chat) ||
		len(left.messages) != len(right.messages) ||
		len(left.queue) != len(right.queue) {
		return false
	}
	for id, fingerprint := range left.messages {
		other, exists := right.messages[id]
		if !exists || !rendererIntentEqual(fingerprint, other) {
			return false
		}
	}
	for id, fingerprint := range left.queue {
		other, exists := right.queue[id]
		if !exists || !rendererIntentEqual(fingerprint, other) {
			return false
		}
	}
	return true
}

func classifyRendererChanges(base, candidate *rendererIntentIndex) map[string]bool {
	changed := make(map[string]bool)
	if candidate == nil {
		return changed
	}
	for tabID, intent := range candidate.chats {
		baseIntent, exists := rendererChatIntent{}, false
		if base != nil {
			baseIntent, exists = base.chats[tabID]
		}
		changed[tabID] = !exists || !rendererChatIntentEqual(baseIntent, intent)
	}
	return changed
}

func committedWorkspacesFromRoot(root map[string]any) []committedWorkspace {
	out := make([]committedWorkspace, 0)
	for _, raw := range anySlice(root["chats"]) {
		chat := mapFromAnyMain(raw)
		revision, ok := intFieldPresent(chat, workspaceRevisionField)
		if !ok || revision <= 0 {
			continue
		}
		binding := committedWorkspace{
			tabID: fieldString(chat, "id"), chatID: fieldString(chat, "chatId"),
			cwd: fieldString(chat, "cwd"), revision: revision,
		}
		if binding.tabID != "" && binding.chatID != "" && binding.cwd != "" {
			out = append(out, binding)
		}
	}
	return out
}

func pruneInternalChats(snapshot map[string]any) {
	chats := anySlice(snapshot["chats"])
	kept := make([]any, 0, len(chats))
	for _, raw := range chats {
		if !isInternalTabID(fieldString(mapFromAnyMain(raw), "id")) {
			kept = append(kept, raw)
		}
	}
	snapshot["chats"] = kept
}

func buildSaveCandidate(
	base *sessionGeneration,
	incoming map[string]any,
	baseIntent *rendererIntentIndex,
	saveMode string,
	explicitDeletes map[string]struct{},
) (map[string]any, *rendererIntentIndex, [][2]string, error) {
	candidate, _ := cloneSessionContainers(incoming).(map[string]any)
	if candidate == nil {
		candidate = map[string]any{"chats": []any{}}
	}
	if saveMode == "lean-payload-v2" && len(explicitDeletes) > 0 {
		kept := make([]any, 0, len(anySlice(candidate["chats"])))
		for _, rawChat := range anySlice(candidate["chats"]) {
			if _, deleted := explicitDeletes[fieldString(mapFromAnyMain(rawChat), "id")]; !deleted {
				kept = append(kept, rawChat)
			}
		}
		candidate["chats"] = kept
	}
	pruneInternalChats(candidate)
	candidateIntent := buildRendererIntentIndex(candidate)

	if base == nil {
		base = newSessionGeneration(map[string]any{"chats": []any{}}, 0, 0, 0)
	}
	indexed := newIndexedSessionGeneration(base.root, base.number, base.persistenceSeq)
	indexed.ownedTurns = base.ownedTurns
	if !validateChatIdentities(candidate, indexed.chatsByTab) {
		return nil, nil, nil, errors.New("session save changed a chat identity")
	}
	workspaces := committedWorkspacesFromRoot(base.root)
	for _, rawChat := range anySlice(candidate["chats"]) {
		delete(mapFromAnyMain(rawChat), workspaceRevisionField)
	}
	if saveMode == "lean-payload-v1" || saveMode == "lean-payload-v2" {
		mergeLeanPayloads(candidate, indexed)
	}
	var wakeTargets [][2]string
	for _, rawChat := range anySlice(candidate["chats"]) {
		incomingChat := mapFromAnyMain(rawChat)
		existingChat := indexed.chatsByTab[fieldString(incomingChat, "id")]
		observedChanged := stampRendererQueueObservedAt(incomingChat, existingChat, time.Now().UTC())
		if existingChat != nil {
			queueChanged := reconcileAgentQueueRevision(incomingChat, existingChat)
			reconcileRuntimeControlRevision(incomingChat, existingChat)
			observedChanged = observedChanged || queueChanged
		}
		if observedChanged {
			wakeTargets = append(wakeTargets, [2]string{
				fieldString(incomingChat, "id"), fieldString(incomingChat, "chatId"),
			})
		}
	}
	merged, ok := mergeAuthoritativeTurns(
		candidate, indexed, saveMode == "lean-payload-v2", explicitDeletes,
	)
	if !ok {
		return nil, nil, nil, errors.New("session save could not reconcile daemon turns")
	}
	applyCommittedWorkspaces(merged, workspaces)
	if err := validateSessionEventImageRefs(merged); err != nil {
		return nil, nil, nil, err
	}

	changed := classifyRendererChanges(baseIntent, candidateIntent)
	chats := anySlice(merged["chats"])
	for index, rawChat := range chats {
		tabID := fieldString(mapFromAnyMain(rawChat), "id")
		position, exists := base.chatPositions[tabID]
		baseChats := anySlice(base.root["chats"])
		if !exists || position < 0 || position >= len(baseChats) {
			continue
		}
		baseChat := mapFromAnyMain(baseChats[position])
		// Renderer equality permits pointer reuse only when reconciliation made
		// no daemon-side semantic change. Adoption, revision stamping, and other
		// authoritative rewrites must publish their merged chat.
		if !changed[tabID] && reflect.DeepEqual(mapFromAnyMain(rawChat), baseChat) {
			chats[index] = baseChat
			continue
		}
		intent := candidateIntent.chats[tabID]
		oldIntent, hadOldIntent := rendererChatIntent{}, false
		if baseIntent != nil {
			oldIntent, hadOldIntent = baseIntent.chats[tabID]
		}
		if !hadOldIntent {
			continue
		}
		chat := mapFromAnyMain(rawChat)
		baseMessages := indexed.messagesByTab[tabID]
		messages := messageSlice(chat)
		for messageIndex, rawMessage := range messages {
			messageID := fieldString(mapFromAnyMain(rawMessage), "id")
			currentFingerprint, currentExists := intent.messages[messageID]
			oldFingerprint, oldExists := oldIntent.messages[messageID]
			if currentExists && oldExists && rendererIntentEqual(currentFingerprint, oldFingerprint) {
				if existing := baseMessages[messageID]; existing != nil {
					messages[messageIndex] = existing
				}
			}
		}
		chat["messages"] = messages
		baseQueue := make(map[string]any)
		for _, rawQueue := range anySlice(baseChat["queue"]) {
			if queueID := fieldString(mapFromAnyMain(rawQueue), "id"); queueID != "" {
				baseQueue[queueID] = rawQueue
			}
		}
		queue := anySlice(chat["queue"])
		for queueIndex, rawQueue := range queue {
			queueID := fieldString(mapFromAnyMain(rawQueue), "id")
			currentFingerprint, currentExists := intent.queue[queueID]
			oldFingerprint, oldExists := oldIntent.queue[queueID]
			if currentExists && oldExists && rendererIntentEqual(currentFingerprint, oldFingerprint) {
				if existing := baseQueue[queueID]; existing != nil {
					queue[queueIndex] = existing
				}
			}
		}
		chat["queue"] = queue
	}
	merged["chats"] = chats
	return merged, candidateIntent, wakeTargets, nil
}

// rebaseSaveCandidateForJobEvents is the bounded deletion-convergence path.
// The expensive renderer merge has already completed outside mu. When every
// intervening generation is a job event, immutable ownership metadata can
// overlay only the newer daemon rows/revisions onto that candidate without
// rescanning lean payloads or rebuilding fingerprints.
func rebaseSaveCandidateForJobEvents(
	candidate map[string]any,
	current *sessionGeneration,
	saveMode string,
	explicitDeletes map[string]struct{},
) (map[string]any, [][2]string, error) {
	if candidate == nil || current == nil {
		return nil, nil, errors.New("session save has no deletion rebase candidate")
	}
	currentChats := make(map[string]map[string]any, len(current.chatPositions))
	for _, rawChat := range anySlice(current.root["chats"]) {
		chat := mapFromAnyMain(rawChat)
		if tabID := fieldString(chat, "id"); tabID != "" {
			currentChats[tabID] = chat
		}
	}
	var wakeTargets [][2]string
	for _, rawChat := range anySlice(candidate["chats"]) {
		chat := mapFromAnyMain(rawChat)
		existing := currentChats[fieldString(chat, "id")]
		observedChanged := stampRendererQueueObservedAt(chat, existing, time.Now().UTC())
		if existing != nil {
			queueChanged := reconcileAgentQueueRevision(chat, existing)
			reconcileRuntimeControlRevision(chat, existing)
			observedChanged = observedChanged || queueChanged
		}
		if observedChanged {
			wakeTargets = append(wakeTargets, [2]string{
				fieldString(chat, "id"), fieldString(chat, "chatId"),
			})
		}
	}
	merged, ok := mergeAuthoritativeTurns(
		candidate, current, saveMode == "lean-payload-v2", explicitDeletes,
	)
	if !ok {
		return nil, nil, errors.New("session save could not rebase daemon turns")
	}
	applyCommittedWorkspaces(merged, committedWorkspacesFromRoot(current.root))
	if err := validateSessionEventImageRefs(merged); err != nil {
		return nil, nil, err
	}
	return merged, wakeTargets, nil
}

func newSessionGenerationIncremental(
	base *sessionGeneration,
	root map[string]any,
	intent *rendererIntentIndex,
	number, stateRevision, persistenceSeq uint64,
) *sessionGeneration {
	generation := newSessionGeneration(root, number, stateRevision, persistenceSeq)
	generation.rendererIntent = intent
	if base != nil {
		generation.pendingArchives = base.pendingArchives
		generation.ownedTurns = base.ownedTurns
		generation.saveRebaseEpoch = base.saveRebaseEpoch + 1
	}
	return generation
}

func (s *sessionStore) Save(raw any) bool {
	if !s.enabled() {
		return false
	}
	incoming := mapFromAnyMain(redactSessionValue(raw))
	// Offset-anchored steer rows came from the abandoned presentation-only
	// implementation. Materialize them before any merge so every in-memory and
	// on-disk owner uses ordinary transcript array order.
	migrateLegacySteerChronology(incoming)
	migrateModelControlKeys(incoming)
	saveMode := fieldString(incoming, "_workassSave")
	deleteRaw, hasDeleteMarker := incoming["_workassDeletedChatIds"]
	explicitDeletes := map[string]struct{}{}
	if saveMode == "lean-payload-v2" {
		var ok bool
		explicitDeletes, ok = sessionDeletedChatIDs(deleteRaw, hasDeleteMarker)
		if !ok {
			return false
		}
	}
	// Redaction rebuilt the complete caller tree, so it is safe to rewrite that
	// detached copy. New image bytes are content-addressed and made durable before
	// the tree can enter the live mirror or any journal. Existing refs are left
	// untouched.
	if err := s.makeIncomingSnapshotRefNative(incoming); err != nil {
		s.recordLoadError(err)
		return false
	}
	delete(incoming, "_workassSave")
	delete(incoming, "_workassDeletedChatIds")
	preparedIncoming := incoming
	durableDroppedChatIDs := map[string]struct{}{}
	var totalWait, totalHeld time.Duration
	stages := make(map[string]time.Duration)

	for attempt := 1; ; attempt++ {
		// Tombstone saves have a bounded convergence contract because their
		// durability phase has already made an externally recoverable decision.
		// Ordinary saves may keep rebuilding until a hot stream yields; rejecting
		// one solely because four outside-lock candidates raced would turn normal
		// renderer persistence into data loss.
		if attempt > sessionSaveMaxAttempts && len(durableDroppedChatIDs) > 0 {
			err := fmt.Errorf("session save abandoned after %d deletion CAS attempts", sessionSaveMaxAttempts)
			s.saveLock.observe(totalWait, totalHeld)
			s.recordLoadError(err)
			return false
		}
		basePublished := s.publishedGeneration()
		base := basePublished
		if base == nil {
			base = newSessionGeneration(map[string]any{"chats": []any{}}, 0, 0, 0)
		}
		baseIntent := base.rendererIntent
		if baseIntent == nil {
			baseIntent = buildRendererIntentIndex(base.root)
		}
		merged, candidateIntent, wakeTargets, err := buildSaveCandidate(
			base, preparedIncoming, baseIntent, saveMode, explicitDeletes,
		)
		if err != nil {
			s.saveLock.observe(totalWait, totalHeld)
			s.recordLoadError(err)
			return false
		}
		tombstones, err := stageDroppedChatTombstones(base.root, merged)
		if err != nil {
			s.saveLock.observe(totalWait, totalHeld)
			s.recordLoadError(err)
			return false
		}
		tombstonesToPersist := make([]stagedDroppedChatTombstone, 0, len(tombstones))
		for _, tombstone := range tombstones {
			if _, durable := durableDroppedChatIDs[tombstone.tabID]; !durable {
				tombstonesToPersist = append(tombstonesToPersist, tombstone)
			}
		}
		if err := s.persistDroppedChatTombstones(tombstonesToPersist); err != nil {
			s.saveLock.observe(totalWait, totalHeld)
			s.recordLoadError(err)
			return false
		}
		for _, tombstone := range tombstonesToPersist {
			durableDroppedChatIDs[tombstone.tabID] = struct{}{}
		}
		if len(tombstonesToPersist) > 0 && s.afterPersistTombstones != nil {
			s.afterPersistTombstones()
		}
		if s.beforeSaveCAS != nil {
			s.beforeSaveCAS()
		}

		waitStart := time.Now()
		s.mu.Lock()
		totalWait += time.Since(waitStart)
		heldStart := time.Now()
		current := s.publishedGeneration()
		if current != basePublished {
			s.recordDroppedChatTombstonesLocked(tombstonesToPersist)
			if len(durableDroppedChatIDs) > 0 && current != nil &&
				current.saveRebaseEpoch == base.saveRebaseEpoch {
				var rebaseWake [][2]string
				merged, rebaseWake, err = rebaseSaveCandidateForJobEvents(
					merged, current, saveMode, explicitDeletes,
				)
				if err != nil {
					held := time.Since(heldStart)
					totalHeld += held
					stages["deletion-rebase-failed"] += held
					s.mu.Unlock()
					s.saveLock.observe(totalWait, totalHeld)
					s.recordLoadError(err)
					return false
				}
				wakeTargets = append(wakeTargets, rebaseWake...)
				base = current
			} else {
				held := time.Since(heldStart)
				totalHeld += held
				stages["cas-retry"] += held
				s.mu.Unlock()
				continue
			}
		}
		s.recordDroppedChatTombstonesLocked(tombstonesToPersist)
		if s.pruneTombstonedChatsLocked(merged) && candidateIntent != nil {
			for tabID := range s.droppedChatIDs {
				delete(candidateIntent.chats, tabID)
			}
		}
		s.persistSeq++
		number, revision := base.number+1, base.stateRevision+1
		generation := newSessionGenerationIncremental(
			base, merged, candidateIntent, number, revision, s.persistSeq,
		)
		generation.pendingArchives = s.capturedArchiveFencesLocked()
		generation.ownedTurns = s.capturedOwnedTurnsLocked()
		s.generationSeq, s.stateRevision = number, revision
		s.published.Store(generation)
		s.generation = generation
		s.snapshot = merged
		wake := s.queueWake
		held := time.Since(heldStart)
		totalHeld += held
		stages["cas-publication"] += held
		s.mu.Unlock()
		s.saveLock.observe(totalWait, totalHeld)
		if s.saveStageObserver != nil {
			s.saveStageObserver(sessionSaveStageReceipt{Stages: stages, Held: totalHeld})
		}

		if s.beforeGenerationMarshal != nil {
			s.beforeGenerationMarshal()
		}
		data, err := json.Marshal(generation.root)
		if err != nil {
			s.recordLoadError(err)
			return false
		}
		if err := s.persistSnapshot(generation.persistenceSeq, data, nil); err != nil {
			s.recordLoadError(err)
			return false
		}
		if wake != nil {
			for _, target := range wakeTargets {
				wake(target[0], target[1])
			}
		}
		return true
	}
}

func (s *sessionStore) makeIncomingSnapshotRefNative(snapshot map[string]any) error {
	if snapshot == nil {
		return nil
	}
	stateDir := filepath.Dir(s.path)
	dedupeSessionEventImages(snapshot)
	plans, err := stageExternalSessionImageData(snapshot, stateDir)
	if err != nil {
		return err
	}
	if len(plans) > 0 && s.beforePersistImages != nil {
		s.beforePersistImages()
	}
	if err := persistSessionImagePlans(plans); err != nil {
		return err
	}
	return validateRefNativeSessionImages(snapshot, stateDir)
}

func sessionDeletedChatIDs(raw any, present bool) (map[string]struct{}, bool) {
	out := make(map[string]struct{})
	if !present || raw == nil {
		return out, true
	}
	items, ok := raw.([]any)
	if !ok || len(items) > 4096 {
		return nil, false
	}
	for _, item := range items {
		id, ok := item.(string)
		id = strings.TrimSpace(id)
		if !ok || id == "" || len(id) > 512 || strings.ContainsAny(id, `/\`) {
			return nil, false
		}
		out[id] = struct{}{}
	}
	return out, true
}

func bumpAgentQueueRevision(chat map[string]any) int {
	if chat == nil {
		return 0
	}
	next := intValue(chat[agentQueueRevisionField]) + 1
	if next < 1 {
		next = 1
	}
	chat[agentQueueRevisionField] = next
	return next
}

func bumpRuntimeControlRevision(chat map[string]any) int {
	if chat == nil {
		return 0
	}
	next := intValue(chat[runtimeControlRevisionField]) + 1
	if next < 1 {
		next = 1
	}
	chat[runtimeControlRevisionField] = next
	return next
}

func daemonOwnedQueueItem(item map[string]any) bool {
	switch fieldString(item, "source") {
	case "agent", "host":
		return true
	default:
		return false
	}
}

func daemonOwnedQueueRows(queue []any) []any {
	rows := make([]any, 0)
	for _, raw := range queue {
		if daemonOwnedQueueItem(mapFromAnyMain(raw)) {
			rows = append(rows, raw)
		}
	}
	return rows
}

func sameDaemonQueueRows(left, right []any) bool {
	leftJSON, leftErr := json.Marshal(daemonOwnedQueueRows(left))
	rightJSON, rightErr := json.Marshal(daemonOwnedQueueRows(right))
	return leftErr == nil && rightErr == nil && bytes.Equal(leftJSON, rightJSON)
}

func sameQueueRows(left, right []any) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftJSON, rightJSON)
}

func adoptedQueueIDs(chat map[string]any) map[string]struct{} {
	out := make(map[string]struct{})
	for _, raw := range anySlice(chat[adoptedQueueLedgerField]) {
		if id := strings.TrimSpace(stringValue(raw)); id != "" {
			out[id] = struct{}{}
		}
	}
	return out
}

func queueIDInAdoptedLedger(chat map[string]any, queueID string) bool {
	queueID = strings.TrimSpace(queueID)
	if queueID == "" {
		return false
	}
	_, ok := adoptedQueueIDs(chat)[queueID]
	return ok
}

func filterAdoptedRendererQueueRows(queue []any, adopted map[string]struct{}) []any {
	if len(queue) == 0 || len(adopted) == 0 {
		return queue
	}
	out := make([]any, 0, len(queue))
	for _, raw := range queue {
		item := mapFromAnyMain(raw)
		if !daemonOwnedQueueItem(item) {
			if _, consumed := adopted[fieldString(item, "id")]; consumed {
				continue
			}
		}
		out = append(out, raw)
	}
	return out
}

func stampRendererQueueObservedAt(incomingChat, daemonChat map[string]any, now time.Time) bool {
	if incomingChat == nil {
		return false
	}
	existing := make(map[string]string)
	if daemonChat != nil {
		for _, raw := range anySlice(daemonChat["queue"]) {
			item := mapFromAnyMain(raw)
			if daemonOwnedQueueItem(item) {
				continue
			}
			if id, observedAt := fieldString(item, "id"), fieldString(item, rendererQueueObservedField); id != "" && observedAt != "" {
				existing[id] = observedAt
			}
		}
	}
	changed := false
	queue := anySlice(incomingChat["queue"])
	for _, raw := range queue {
		item := mapFromAnyMain(raw)
		if fieldString(item, "source") != "" {
			continue
		}
		observedAt := existing[fieldString(item, "id")]
		if observedAt == "" {
			observedAt = now.Format(time.RFC3339Nano)
		}
		if fieldString(item, rendererQueueObservedField) != observedAt {
			item[rendererQueueObservedField] = observedAt
			changed = true
		}
	}
	return changed
}

// mergeQueueWithAuthoritativeDaemonRows keeps renderer-owned rows editable even
// when the renderer's queue revision is stale. Exact renderer rows replace
// their prior local copies; daemon-owned agent/host rows keep the authoritative
// order, and stale daemon echoes are discarded.
func mergeQueueWithAuthoritativeDaemonRows(rendererQueue, daemonQueue []any, adopted map[string]struct{}) []any {
	rendererRows := make(map[string]any)
	for _, raw := range rendererQueue {
		item := mapFromAnyMain(raw)
		if daemonOwnedQueueItem(item) {
			continue
		}
		if id := fieldString(item, "id"); id != "" {
			if _, consumed := adopted[id]; consumed {
				continue
			}
			rendererRows[id] = raw
		}
	}
	used := make(map[string]struct{})
	out := make([]any, 0, len(rendererQueue)+len(daemonQueue))
	for _, raw := range daemonQueue {
		item := mapFromAnyMain(raw)
		if daemonOwnedQueueItem(item) {
			out = append(out, cloneJSON(raw))
			continue
		}
		id := fieldString(item, "id")
		if replacement, ok := rendererRows[id]; ok && id != "" {
			out = append(out, replacement)
			used[id] = struct{}{}
		}
	}
	for _, raw := range rendererQueue {
		item := mapFromAnyMain(raw)
		if daemonOwnedQueueItem(item) {
			continue
		}
		id := fieldString(item, "id")
		if _, consumed := adopted[id]; consumed && id != "" {
			continue
		}
		if _, ok := used[id]; ok && id != "" {
			continue
		}
		out = append(out, raw)
	}
	return out
}

// A capability-aware busy start is converted into a daemon-owned host queue
// row. A renderer save already in flight may still contain the optimistic
// transcript pair that was withdrawn during that conversion. When its queue
// revision is stale, remove only those exact origin ids before the ordinary
// authoritative-turn merge; unrelated renderer messages remain editable.
func stripStaleHostQueueOrigins(incomingChat, daemonChat map[string]any) {
	origins := make(map[string]struct{})
	for _, raw := range anySlice(daemonChat[hostQueueOriginsField]) {
		origin := mapFromAnyMain(raw)
		for _, key := range []string{hostQueueOriginUserField, hostQueueOriginAssistantField} {
			if id := fieldString(origin, key); id != "" {
				origins[id] = struct{}{}
			}
		}
	}
	for _, raw := range anySlice(daemonChat["queue"]) {
		item := mapFromAnyMain(raw)
		if fieldString(item, "source") != "host" {
			continue
		}
		for _, key := range []string{hostQueueOriginUserField, hostQueueOriginAssistantField} {
			if id := fieldString(item, key); id != "" {
				origins[id] = struct{}{}
			}
		}
	}
	if len(origins) == 0 {
		return
	}
	messages := messageSlice(incomingChat)
	out := make([]any, 0, len(messages))
	for _, raw := range messages {
		message := mapFromAnyMain(raw)
		_, exact := origins[fieldString(message, "id")]
		_, continuation := origins[fieldString(message, "turnRootId")]
		if exact || continuation {
			continue
		}
		out = append(out, raw)
	}
	incomingChat["messages"] = out
}

func reconcileAgentQueueRevision(incomingChat, daemonChat map[string]any) bool {
	if incomingChat == nil || daemonChat == nil {
		return false
	}
	daemonRevision := intValue(daemonChat[agentQueueRevisionField])
	incomingRevision := intValue(incomingChat[agentQueueRevisionField])
	daemonQueue := anySlice(daemonChat["queue"])
	adopted := adoptedQueueIDs(daemonChat)
	incomingQueue := filterAdoptedRendererQueueRows(anySlice(incomingChat["queue"]), adopted)
	incomingChat["queue"] = incomingQueue
	if origins, exists := daemonChat[hostQueueOriginsField]; exists {
		incomingChat[hostQueueOriginsField] = cloneJSON(origins)
	}
	if ledger, exists := daemonChat[adoptedQueueLedgerField]; exists {
		incomingChat[adoptedQueueLedgerField] = cloneJSON(ledger)
	}
	if incomingRevision != daemonRevision {
		stripStaleHostQueueOrigins(incomingChat, daemonChat)
		incomingChat["queue"] = mergeQueueWithAuthoritativeDaemonRows(incomingQueue, daemonQueue, adopted)
		incomingChat[agentQueueRevisionField] = daemonRevision
		return !sameQueueRows(daemonQueue, anySlice(incomingChat["queue"]))
	}
	if !sameDaemonQueueRows(incomingQueue, daemonQueue) {
		incomingChat[agentQueueRevisionField] = daemonRevision + 1
		return !sameQueueRows(daemonQueue, incomingQueue)
	}
	incomingChat[agentQueueRevisionField] = daemonRevision
	return !sameQueueRows(daemonQueue, incomingQueue)
}

func reconcileRuntimeControlRevision(incomingChat, daemonChat map[string]any) {
	if incomingChat == nil || daemonChat == nil {
		return
	}
	daemonRevision := intValue(daemonChat[runtimeControlRevisionField])
	incomingRevision := intValue(incomingChat[runtimeControlRevisionField])
	if incomingRevision != daemonRevision {
		for _, key := range []string{"providerId", "currentModelId", "currentModeId", "modelControls"} {
			if value, exists := daemonChat[key]; exists {
				incomingChat[key] = cloneJSON(value)
			} else {
				delete(incomingChat, key)
			}
		}
	}
	incomingChat[runtimeControlRevisionField] = daemonRevision
}

type committedWorkspace struct {
	tabID    string
	chatID   string
	cwd      string
	revision int
}

func (s *sessionStore) committedWorkspacesLocked() []committedWorkspace {
	out := make([]committedWorkspace, 0)
	for _, raw := range anySlice(s.snapshot["chats"]) {
		chat := mapFromAnyMain(raw)
		revision, ok := intFieldPresent(chat, workspaceRevisionField)
		if !ok || revision <= 0 {
			continue
		}
		binding := committedWorkspace{
			tabID: fieldString(chat, "id"), chatID: fieldString(chat, "chatId"),
			cwd: fieldString(chat, "cwd"), revision: revision,
		}
		if binding.tabID != "" && binding.chatID != "" && binding.cwd != "" {
			out = append(out, binding)
		}
	}
	return out
}

func (s *sessionStore) applyCommittedWorkspacesLocked(bindings []committedWorkspace) {
	applyCommittedWorkspaces(s.snapshot, bindings)
}

func applyCommittedWorkspaces(snapshot map[string]any, bindings []committedWorkspace) {
	chatsByTab := make(map[string]map[string]any, len(anySlice(snapshot["chats"])))
	for _, raw := range anySlice(snapshot["chats"]) {
		chat := mapFromAnyMain(raw)
		if tabID := fieldString(chat, "id"); tabID != "" {
			chatsByTab[tabID] = chat
		}
	}
	for _, binding := range bindings {
		chat := chatsByTab[binding.tabID]
		if chat == nil || fieldString(chat, "chatId") != binding.chatID {
			continue
		}
		chat["cwd"] = binding.cwd
		chat[workspaceRevisionField] = binding.revision
	}
}

// validateChatIdentitiesLocked makes (tabId, chatId) immutable. A renderer
// reload may submit an older snapshot, but it may never retarget an existing
// tab to a different logical conversation or duplicate one conversation under
// multiple tabs. Missing legacy chat ids are deterministically backfilled.
func (s *sessionStore) validateChatIdentitiesLocked(incoming map[string]any) bool {
	chatsByTab := make(map[string]map[string]any, len(anySlice(s.snapshot["chats"])))
	for _, raw := range anySlice(s.snapshot["chats"]) {
		chat := mapFromAnyMain(raw)
		if tabID := fieldString(chat, "id"); tabID != "" {
			chatsByTab[tabID] = chat
		}
	}
	return validateChatIdentities(incoming, chatsByTab)
}

func validateChatIdentities(incoming map[string]any, existingChats map[string]map[string]any) bool {
	seenTabs := make(map[string]struct{})
	seenChats := make(map[string]string)
	for _, raw := range anySlice(incoming["chats"]) {
		chat := mapFromAnyMain(raw)
		tabID := fieldString(chat, "id")
		if tabID == "" {
			return false
		}
		if _, duplicate := seenTabs[tabID]; duplicate {
			return false
		}
		seenTabs[tabID] = struct{}{}

		chatID := fieldString(chat, "chatId")
		if existing := existingChats[tabID]; existing != nil {
			existingChatID := fieldString(existing, "chatId")
			if chatID == "" {
				chatID = existingChatID
				chat["chatId"] = chatID
			} else if existingChatID != "" && existingChatID != chatID {
				return false
			}
		}
		if chatID == "" {
			chatID = "legacy-" + tabID
			chat["chatId"] = chatID
		}
		if ownerTab, duplicate := seenChats[chatID]; duplicate && ownerTab != tabID {
			return false
		}
		seenChats[chatID] = tabID
	}
	return true
}

// UpdateChatControls durably patches the daemon-owned mirror for one chat.
// These fields define the ACP runtime selected for that conversation, so they
// cannot depend solely on a later renderer-wide session:save flush.
func (s *sessionStore) UpdateChatControls(tabID, chatID, providerID, modelID, modeID string) bool {
	if !s.enabled() {
		return false
	}
	tabID = redactedSessionString(tabID)
	if tabID == "" {
		return false
	}
	chatID = redactedSessionString(chatID)
	providerID = redactedSessionString(providerID)
	modelID = redactedSessionString(modelID)
	modeID = redactedSessionString(modeID)

	s.mu.Lock()
	tx := s.beginSessionMutationLocked()
	defer func() {
		if s.mutation == tx {
			s.abortSessionMutationLocked(tx)
		}
		s.mu.Unlock()
	}()
	s.ensureSnapshotLocked()
	chat := s.ensureChatLocked(tabID, chatID, "")
	if chat == nil {
		return false // internal subagent session: nothing user-visible to bind
	}
	if chatID != "" {
		chat["chatId"] = chatID
	}
	if providerID != "" {
		chat["providerId"] = providerID
	}
	if modelID != "" {
		chat["currentModelId"] = modelID
	}
	if modeID != "" {
		chat["currentModeId"] = modeID
	}
	if providerID != "" || modelID != "" || modeID != "" {
		// Every accepted daemon-side control commit fences renderer snapshots that
		// were already in flight, even when the selected value itself is unchanged.
		bumpRuntimeControlRevision(chat)
	}
	return s.writeLocked() == nil
}

func (s *sessionStore) ChatRuntimeControls(tabID, chatID string) (providerID, modelID, modeID string, ok bool) {
	if !s.enabled() {
		return "", "", "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	chat, err := s.exactChatLocked(tabID, chatID)
	if err != nil {
		return "", "", "", false
	}
	return fieldString(chat, "providerId"), fieldString(chat, "currentModelId"), fieldString(chat, "currentModeId"), true
}

// UpdateChatWorkspace is the durable half of an initialized sidebar move. It
// addresses the immutable tab+conversation pair exactly and lands before ACP
// bindings are invalidated, so a daemon restart can only recreate the chat in
// the target directory.
func (s *sessionStore) UpdateChatWorkspace(tabID, chatID, cwd string) bool {
	_, ok := s.moveChatWorkspace(tabID, chatID, cwd, -1)
	return ok
}

// MoveChatWorkspace is the compare-and-swap boundary used by the renderer.
// Revision 0 is the initial cwd; every accepted move increments it. A second
// controller holding stale sidebar state cannot overwrite a newer move.
func (s *sessionStore) MoveChatWorkspace(tabID, chatID, cwd string, expectedRevision int) (int, bool) {
	return s.moveChatWorkspace(tabID, chatID, cwd, expectedRevision)
}

func (s *sessionStore) moveChatWorkspace(tabID, chatID, cwd string, expectedRevision int) (int, bool) {
	if !s.enabled() {
		return 0, false
	}
	tabID = redactedSessionString(tabID)
	chatID = redactedSessionString(chatID)
	cwd = redactedSessionString(cwd)
	if tabID == "" || chatID == "" || cwd == "" {
		return 0, false
	}
	s.mu.Lock()
	tx := s.beginSessionMutationLocked()
	defer func() {
		if s.mutation == tx {
			s.abortSessionMutationLocked(tx)
		}
		s.mu.Unlock()
	}()
	chat, err := s.exactChatLocked(tabID, chatID)
	if err != nil {
		return 0, false
	}
	revision, _ := intFieldPresent(chat, workspaceRevisionField)
	if expectedRevision >= 0 && revision != expectedRevision {
		return revision, false
	}
	next := revision + 1
	chat["cwd"] = cwd
	chat[workspaceRevisionField] = next
	if err := s.writeLocked(); err != nil {
		return revision, false
	}
	return next, true
}

func (s *sessionStore) ChatWorkspace(tabID, chatID string) (string, bool) {
	if !s.enabled() {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	chat, err := s.exactChatLocked(strings.TrimSpace(tabID), strings.TrimSpace(chatID))
	if err != nil {
		return "", false
	}
	cwd := fieldString(chat, "cwd")
	return cwd, cwd != ""
}

// MostRecentVisibleAssistantJobID returns the newest daemon job anchor already
// present on an assistant row for one exact chat. It is used only as the
// optional visible root for a subagent spawned while that chat has no running
// turn; transcript content never leaves the session mirror through this path.
func (s *sessionStore) MostRecentVisibleAssistantJobID(tabID, chatID string) string {
	if !s.enabled() {
		return ""
	}
	tabID, chatID = strings.TrimSpace(tabID), strings.TrimSpace(chatID)
	s.mu.Lock()
	defer s.mu.Unlock()
	chat, err := s.exactChatLocked(tabID, chatID)
	if err != nil {
		return ""
	}
	messages := messageSlice(chat)
	for index := len(messages) - 1; index >= 0; index-- {
		message := mapFromAnyMain(messages[index])
		if fieldString(message, "role") != "assistant" {
			continue
		}
		if jobID := fieldString(message, "jobId"); jobID != "" {
			return jobID
		}
	}
	return ""
}

// AgentChatList is the daemon-authoritative, transcript-free discovery surface
// for first-party agent tools. Stable tabId+chatId pairs are always returned
// together so a later mutation cannot accidentally address "the active tab".
func (s *sessionStore) AgentChatList() []map[string]any {
	if !s.enabled() {
		return []map[string]any{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	activeID := fieldString(s.snapshot, "activeId")
	out := make([]map[string]any, 0, len(anySlice(s.snapshot["chats"])))
	for _, raw := range anySlice(s.snapshot["chats"]) {
		chat := mapFromAnyMain(raw)
		tabID, chatID := fieldString(chat, "id"), fieldString(chat, "chatId")
		if tabID == "" || chatID == "" {
			continue
		}
		messages := messageSlice(chat)
		lastRole, lastAt := "", ""
		if len(messages) > 0 {
			last := mapFromAnyMain(messages[len(messages)-1])
			lastRole, lastAt = fieldString(last, "role"), fieldString(last, "at")
		}
		out = append(out, map[string]any{
			"tabId": tabID, "chatId": chatID, "title": fieldString(chat, "title"),
			"cwd": fieldString(chat, "cwd"), "providerId": fieldString(chat, "providerId"),
			"modelId": fieldString(chat, "currentModelId"), "modeId": fieldString(chat, "currentModeId"),
			"messageCount": len(messages), "queueCount": len(anySlice(chat["queue"])),
			"lastRole": lastRole, "lastAt": lastAt, "active": tabID == activeID,
		})
	}
	return out
}

func (s *sessionStore) AgentReadChat(tabID, chatID string, limit int, includeEvents bool) (map[string]any, error) {
	if !s.enabled() {
		return nil, errors.New("session store is unavailable")
	}
	tabID, chatID = strings.TrimSpace(tabID), strings.TrimSpace(chatID)
	s.mu.Lock()
	defer s.mu.Unlock()
	chat, err := s.exactChatLocked(tabID, chatID)
	if err != nil {
		return nil, err
	}
	messages := messageSlice(chat)
	if limit <= 0 {
		limit = 40
	}
	if limit > 200 {
		limit = 200
	}
	if len(messages) > limit {
		messages = messages[len(messages)-limit:]
	}
	copyMessagesReversed := make([]any, 0, len(messages))
	encodedBytes := 0
	for i := len(messages) - 1; i >= 0; i-- {
		raw := messages[i]
		message, _ := cloneJSON(mapFromAnyMain(raw)).(map[string]any)
		if content := stringValue(message["content"]); len(content) > 32*1024 {
			message["content"] = content[:32*1024] + "\n[message truncated by Workass chat control]"
			message["contentTruncated"] = true
		}
		if images := anySlice(message["images"]); len(images) > 0 {
			attachments := make([]any, 0, len(images))
			for _, imageRaw := range images {
				image := mapFromAnyMain(imageRaw)
				attachments = append(attachments, map[string]any{"mimeType": fieldString(image, "mimeType"), "name": fieldString(image, "name")})
			}
			message["attachments"] = attachments
			delete(message, "images")
		}
		// Tool-result images are visible transcript media, but chat-control reads
		// expose metadata only. Returning multi-megabyte base64 through another
		// agent tool would duplicate the payload and can crowd useful context.
		for _, eventRaw := range anySlice(message["events"]) {
			event := mapFromAnyMain(eventRaw)
			images := anySlice(event["images"])
			if len(images) == 0 {
				continue
			}
			attachments := make([]any, 0, len(images))
			for _, imageRaw := range images {
				image := mapFromAnyMain(imageRaw)
				attachments = append(attachments, map[string]any{"mimeType": fieldString(image, "mimeType"), "name": fieldString(image, "name")})
			}
			event["attachments"] = attachments
			delete(event, "images")
		}
		if !includeEvents {
			message["events"] = []any{}
		} else if events := anySlice(message["events"]); len(events) > 24 {
			message["events"] = events[len(events)-24:]
			message["eventsTruncated"] = true
		}
		encoded, _ := json.Marshal(message)
		if len(encoded) > 64*1024 {
			message["events"] = []any{}
			message["eventsTruncated"] = true
			encoded, _ = json.Marshal(message)
		}
		if encodedBytes+len(encoded) > 4*1024*1024 && len(copyMessagesReversed) > 0 {
			break
		}
		encodedBytes += len(encoded)
		copyMessagesReversed = append(copyMessagesReversed, message)
	}
	copyMessages := make([]any, len(copyMessagesReversed))
	for i := range copyMessagesReversed {
		copyMessages[len(copyMessagesReversed)-1-i] = copyMessagesReversed[i]
	}
	return map[string]any{
		"tabId": tabID, "chatId": chatID, "title": fieldString(chat, "title"),
		"cwd": fieldString(chat, "cwd"), "providerId": fieldString(chat, "providerId"),
		"modelId": fieldString(chat, "currentModelId"), "modeId": fieldString(chat, "currentModeId"),
		"messages": copyMessages, "truncated": len(messageSlice(chat)) > len(copyMessages),
	}, nil
}

func (s *sessionStore) AgentCreateChat(title, cwd, providerID, modelID, modeID string, focus bool) (map[string]any, error) {
	if !s.enabled() {
		return nil, errors.New("session store is unavailable")
	}
	title = redactedSessionString(title)
	if title == "" {
		title = "Nuevo chat"
	}
	if len(title) > 200 {
		title = title[:200]
	}
	tabID, chatID := nextSessionID("tab"), nextSessionID("chat")
	s.mu.Lock()
	tx := s.beginSessionMutationLocked()
	defer func() {
		if s.mutation == tx {
			s.abortSessionMutationLocked(tx)
		}
		s.mu.Unlock()
	}()
	s.ensureSnapshotLocked()
	seq := intValue(s.snapshot["seq"]) + 1
	s.snapshot["seq"] = seq
	chat := map[string]any{
		"id": tabID, "chatId": chatID, "title": title, "titleLocked": true,
		"group": nil, "cwd": redactedSessionString(cwd), "currentModelId": redactedSessionString(modelID),
		"currentModeId": redactedSessionString(modeID), "draft": "", "providerId": redactedSessionString(providerID),
		"messages": []any{}, "serverAuthored": true,
	}
	s.snapshot["chats"] = append(anySlice(s.snapshot["chats"]), chat)
	if focus || fieldString(s.snapshot, "activeId") == "" {
		s.snapshot["activeId"] = tabID
	}
	if err := s.writeLocked(); err != nil {
		return nil, err
	}
	return map[string]any{"tabId": tabID, "chatId": chatID, "title": title, "active": fieldString(s.snapshot, "activeId") == tabID}, nil
}

func (s *sessionStore) AgentRenameChat(tabID, chatID, title string) error {
	title = redactedSessionString(title)
	if title == "" {
		return errors.New("title is required")
	}
	if len(title) > 200 {
		title = title[:200]
	}
	s.mu.Lock()
	tx := s.beginSessionMutationLocked()
	defer func() {
		if s.mutation == tx {
			s.abortSessionMutationLocked(tx)
		}
		s.mu.Unlock()
	}()
	chat, err := s.exactChatLocked(tabID, chatID)
	if err != nil {
		return err
	}
	chat["title"], chat["titleLocked"] = title, true
	return s.writeLocked()
}

func (s *sessionStore) AgentConfigureChat(tabID, chatID, cwd, providerID, modelID, baseModelID, effort, modeID string) error {
	s.mu.Lock()
	tx := s.beginSessionMutationLocked()
	defer func() {
		if s.mutation == tx {
			s.abortSessionMutationLocked(tx)
		}
		s.mu.Unlock()
	}()
	chat, err := s.exactChatLocked(tabID, chatID)
	if err != nil {
		return err
	}
	if cwd != "" {
		chat["cwd"] = redactedSessionString(cwd)
	}
	if providerID != "" {
		chat["providerId"] = redactedSessionString(providerID)
	}
	if modelID != "" {
		chat["currentModelId"] = redactedSessionString(modelID)
	}
	if modeID != "" {
		chat["currentModeId"] = redactedSessionString(modeID)
	}
	if providerID != "" && baseModelID != "" {
		baseModelID = modelControlBaseKey(baseModelID)
		memory, _ := cloneSessionContainers(mapFromAnyMain(chat["modelControls"])).(map[string]any)
		providerMemory := mapFromAnyMain(memory[providerID])
		controls := map[string]any{}
		if effort != "" {
			controls["effort"] = redactedSessionString(effort)
		}
		if modeID != "" {
			controls["modeId"] = redactedSessionString(modeID)
		}
		providerMemory[baseModelID] = controls
		memory[providerID] = providerMemory
		chat["modelControls"] = memory
	}
	// Agent configuration is an exact-chat daemon commit. Advance the fence even
	// when a repeated request selects the same values: an older renderer may
	// still be holding the pre-configuration provider/model snapshot.
	bumpRuntimeControlRevision(chat)
	return s.writeLocked()
}

func (s *sessionStore) AgentFocusChat(tabID, chatID string) error {
	s.mu.Lock()
	tx := s.beginSessionMutationLocked()
	defer func() {
		if s.mutation == tx {
			s.abortSessionMutationLocked(tx)
		}
		s.mu.Unlock()
	}()
	if _, err := s.exactChatLocked(tabID, chatID); err != nil {
		return err
	}
	s.snapshot["activeId"] = strings.TrimSpace(tabID)
	return s.writeLocked()
}

func (s *sessionStore) AgentDeleteChat(tabID, chatID string) error {
	s.mu.Lock()
	tx := s.beginSessionMutationLocked()
	defer func() {
		if s.mutation == tx {
			s.abortSessionMutationLocked(tx)
		}
		s.mu.Unlock()
	}()
	chat, err := s.exactChatLocked(tabID, chatID)
	if err != nil {
		return err
	}
	if err := s.writeDroppedChatLocked(strings.TrimSpace(tabID), cloneJSON(chat).(map[string]any)); err != nil {
		return err
	}
	chats := anySlice(s.snapshot["chats"])
	out := make([]any, 0, len(chats)-1)
	for _, raw := range chats {
		if fieldString(mapFromAnyMain(raw), "id") != strings.TrimSpace(tabID) {
			out = append(out, raw)
		}
	}
	s.snapshot["chats"] = out
	if fieldString(s.snapshot, "activeId") == strings.TrimSpace(tabID) {
		s.snapshot["activeId"] = nil
		if len(out) > 0 {
			s.snapshot["activeId"] = fieldString(mapFromAnyMain(out[0]), "id")
		}
	}
	return s.writeLocked()
}

func (s *sessionStore) AgentEnqueueChat(tabID, chatID, prompt, delivery string) (map[string]any, error) {
	prompt = redactedSessionString(prompt)
	if prompt == "" {
		return nil, errors.New("message is required")
	}
	s.mu.Lock()
	tx := s.beginSessionMutationLocked()
	defer func() {
		if s.mutation == tx {
			s.abortSessionMutationLocked(tx)
		}
		s.mu.Unlock()
	}()
	chat, err := s.exactChatLocked(tabID, chatID)
	if err != nil {
		return nil, err
	}
	id := nextSessionID("q")
	item := map[string]any{"id": id, "text": prompt, "source": "agent", "delivery": delivery, "queuedAt": time.Now().UTC().Format(time.RFC3339Nano)}
	chat["queue"] = append(anySlice(chat["queue"]), item)
	bumpAgentQueueRevision(chat)
	if err := s.writeLocked(); err != nil {
		return nil, err
	}
	return map[string]any{"queueId": id, "position": len(anySlice(chat["queue"])), "delivery": delivery}, nil
}

// QueueRendererStartCollision transfers a capability-aware ordinary send into
// the daemon-owned FIFO after StartJob atomically reports that the chat is busy.
// The optimistic renderer pair is withdrawn by exact id, and those origin ids
// stay on the queue row so a stale save cannot resurrect the false failed turn.
func (s *sessionStore) QueueRendererStartCollision(opts map[string]any) (map[string]any, error) {
	opts = mapFromAnyMain(redactSessionValue(opts))
	if err := makeSessionValueRefNative(opts, filepath.Dir(s.path)); err != nil {
		return nil, err
	}
	tabID, chatID := fieldString(opts, "tabId"), fieldString(opts, "chatId")
	prompt := firstNonEmptyString(fieldString(opts, "prompt"), fieldString(opts, "message"))
	userID, assistantID := fieldString(opts, "userMessageId"), fieldString(opts, "assistantMessageId")
	if prompt == "" {
		return nil, errors.New("busy queue requires a message")
	}
	if userID == "" || assistantID == "" {
		return nil, errors.New("busy queue-v1 requires stable user and assistant message ids")
	}

	s.mu.Lock()
	tx := s.beginSessionMutationLocked()
	defer func() {
		if s.mutation == tx {
			s.abortSessionMutationLocked(tx)
		}
		s.mu.Unlock()
	}()
	chat, err := s.exactChatLocked(tabID, chatID)
	if err != nil {
		return nil, err
	}
	originQueueID := firstNonEmptyString(fieldString(opts, "queueId"), fieldString(opts, agentQueueMessageField))
	if queueIDInAdoptedLedger(chat, originQueueID) {
		before := cloneJSON(s.snapshot).(map[string]any)
		messages := messageSlice(chat)
		kept := make([]any, 0, len(messages))
		for _, raw := range messages {
			message := mapFromAnyMain(raw)
			if id := fieldString(message, "id"); id == userID || id == assistantID || fieldString(message, "turnRootId") == assistantID {
				continue
			}
			kept = append(kept, raw)
		}
		chat["messages"] = kept
		if err := s.writeLocked(); err != nil {
			s.snapshot = before
			return nil, err
		}
		return map[string]any{
			"queued": true, "queueId": originQueueID, "position": 0,
			"delivery": "queue", "adopted": true, "agentQueueRevision": intValue(chat[agentQueueRevisionField]),
		}, nil
	}
	queue := anySlice(chat["queue"])
	for index, raw := range queue {
		item := mapFromAnyMain(raw)
		if fieldString(item, "source") == "host" && fieldString(item, hostQueueOriginAssistantField) == assistantID {
			return map[string]any{
				"queued": true, "queueId": fieldString(item, "id"), "position": index + 1,
				"delivery": "queue", "agentQueueRevision": intValue(chat[agentQueueRevisionField]),
			}, nil
		}
	}

	before := cloneJSON(s.snapshot).(map[string]any)
	if applyChatControls(chat, opts) {
		bumpRuntimeControlRevision(chat)
	}
	messages := messageSlice(chat)
	kept := make([]any, 0, len(messages))
	for _, raw := range messages {
		message := mapFromAnyMain(raw)
		id, rootID := fieldString(message, "id"), fieldString(message, "turnRootId")
		if id == userID || id == assistantID || rootID == assistantID {
			continue
		}
		kept = append(kept, raw)
	}
	chat["messages"] = kept

	id := nextSessionID("q")
	queuedAt := time.Now().UTC().Format(time.RFC3339Nano)
	item := map[string]any{
		"id": id, "text": prompt, "source": "host", "delivery": "queue", "queuedAt": queuedAt,
		hostQueueOriginUserField: userID, hostQueueOriginAssistantField: assistantID,
	}
	if images := anySlice(opts["images"]); len(images) > 0 {
		item["images"] = cloneJSON(images)
	}
	chat["queue"] = append(queue, item)
	origins := append(anySlice(chat[hostQueueOriginsField]), map[string]any{
		"queueId": id, hostQueueOriginUserField: userID, hostQueueOriginAssistantField: assistantID,
	})
	const retainedHostQueueOrigins = 256
	if len(origins) > retainedHostQueueOrigins {
		origins = origins[len(origins)-retainedHostQueueOrigins:]
	}
	chat[hostQueueOriginsField] = origins
	revision := bumpAgentQueueRevision(chat)
	if err := s.writeLocked(); err != nil {
		s.snapshot = before
		return nil, err
	}
	return map[string]any{
		"queued": true, "queueId": id, "position": len(queue) + 1,
		"delivery": "queue", "queuedAt": queuedAt, "agentQueueRevision": revision,
	}, nil
}

func (s *sessionStore) AgentQueueHead(tabID, chatID string) (map[string]any, bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	chat, err := s.exactChatLocked(tabID, chatID)
	if err != nil {
		return nil, false, false
	}
	queue := anySlice(chat["queue"])
	if len(queue) == 0 {
		return nil, false, false
	}
	item := mapFromAnyMain(queue[0])
	if !daemonOwnedQueueItem(item) {
		return nil, false, true
	}
	copyItem, _ := cloneJSON(item).(map[string]any)
	return copyItem, true, true
}

func rendererQueueAttachmentsPending(item map[string]any) bool {
	switch fieldString(item, "attachmentState") {
	case "", "ready":
	default:
		return true
	}
	return len(anySlice(item["draftImages"])) > 0 && len(anySlice(item["images"])) == 0
}

func (s *sessionStore) AgentAdoptRendererQueueHead(
	tabID, chatID string,
	now time.Time,
	minAge time.Duration,
) (map[string]any, bool, bool, error) {
	s.mu.Lock()
	tx := s.beginSessionMutationLocked()
	defer func() {
		if s.mutation == tx {
			s.abortSessionMutationLocked(tx)
		}
		s.mu.Unlock()
	}()
	chat, err := s.exactChatLocked(tabID, chatID)
	if err != nil {
		return nil, false, false, err
	}
	queue := anySlice(chat["queue"])
	if len(queue) == 0 {
		return nil, false, false, nil
	}
	item := mapFromAnyMain(queue[0])
	copyHead := func() map[string]any {
		copyItem, _ := cloneJSON(item).(map[string]any)
		return copyItem
	}
	if daemonOwnedQueueItem(item) || item[queueParkedField] == true || rendererQueueAttachmentsPending(item) {
		return copyHead(), false, true, nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	observedAt, parseErr := time.Parse(time.RFC3339Nano, fieldString(item, rendererQueueObservedField))
	if parseErr != nil {
		before := cloneJSON(s.snapshot).(map[string]any)
		item[rendererQueueObservedField] = now.Format(time.RFC3339Nano)
		bumpAgentQueueRevision(chat)
		if err := s.writeLocked(); err != nil {
			s.snapshot = before
			return nil, false, true, err
		}
		return copyHead(), false, true, nil
	}
	if minAge > 0 && now.Sub(observedAt) < minAge {
		return copyHead(), false, true, nil
	}

	before := cloneJSON(s.snapshot).(map[string]any)
	queueID := fieldString(item, "id")
	item["source"] = "agent"
	item["adoptedFrom"] = "renderer"
	ledger := anySlice(chat[adoptedQueueLedgerField])
	if !queueIDInAdoptedLedger(chat, queueID) {
		ledger = append(ledger, queueID)
		if len(ledger) > retainedQueueLedgerEntries {
			ledger = ledger[len(ledger)-retainedQueueLedgerEntries:]
		}
		chat[adoptedQueueLedgerField] = ledger
	}
	bumpAgentQueueRevision(chat)
	if err := s.writeLocked(); err != nil {
		s.snapshot = before
		return nil, false, true, err
	}
	return copyHead(), true, true, nil
}

func (s *sessionStore) AgentParkQueuedTurn(tabID, chatID, queueID, detail string) error {
	s.mu.Lock()
	tx := s.beginSessionMutationLocked()
	defer func() {
		if s.mutation == tx {
			s.abortSessionMutationLocked(tx)
		}
		s.mu.Unlock()
	}()
	chat, err := s.exactChatLocked(tabID, chatID)
	if err != nil {
		return err
	}
	queue := anySlice(chat["queue"])
	if len(queue) == 0 || fieldString(mapFromAnyMain(queue[0]), "id") != strings.TrimSpace(queueID) {
		return errors.New("queued message is no longer the head")
	}
	before := cloneJSON(s.snapshot).(map[string]any)
	item := mapFromAnyMain(queue[0])
	now := time.Now().UTC().Format(time.RFC3339Nano)
	safeDetail := strings.TrimSpace(acp.RedactSensitiveText(detail))
	if len(safeDetail) > 1024 {
		safeDetail = safeDetail[:1024]
	}
	item[queueParkedField] = true
	item["parkedAt"] = now
	item["parkReason"] = safeDetail
	bumpAgentQueueRevision(chat)
	if err := s.writeLocked(); err != nil {
		s.snapshot = before
		return err
	}
	return nil
}

func (s *sessionStore) AdoptedQueueReceipt(tabID, chatID, queueID string) (map[string]any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	chat, err := s.exactChatLocked(tabID, chatID)
	if err != nil || !queueIDInAdoptedLedger(chat, queueID) {
		return nil, false
	}
	return map[string]any{
		"queued": true, "queueId": strings.TrimSpace(queueID), "position": 0,
		"delivery": "queue", "adopted": true, "agentQueueRevision": intValue(chat[agentQueueRevisionField]),
	}, true
}

func (s *sessionStore) AgentQueueTargets() [][2]string {
	if !s.enabled() {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out [][2]string
	for _, raw := range anySlice(s.snapshot["chats"]) {
		chat := mapFromAnyMain(raw)
		queue := anySlice(chat["queue"])
		if len(queue) == 0 || mapFromAnyMain(queue[0])[queueParkedField] == true {
			continue
		}
		out = append(out, [2]string{fieldString(chat, "id"), fieldString(chat, "chatId")})
	}
	return out
}

// AgentPrepareQueuedTurn atomically consumes the first daemon-owned FIFO item
// and creates the canonical user/assistant rows plus pending daemon job anchor.
// A crash can therefore produce an honest failed visible turn, never a lost or
// silently duplicated queue item.
func (s *sessionStore) AgentPrepareQueuedTurn(tabID, chatID, queueID, sessionID string) (map[string]any, error) {
	s.mu.Lock()
	tx := s.beginSessionMutationLocked()
	opts, err := func() (map[string]any, error) {
		chat, err := s.exactChatLocked(tabID, chatID)
		if err != nil {
			return nil, err
		}
		queue := anySlice(chat["queue"])
		if len(queue) == 0 {
			return nil, errors.New("queued message no longer exists")
		}
		item := mapFromAnyMain(queue[0])
		if fieldString(item, "id") != strings.TrimSpace(queueID) || !daemonOwnedQueueItem(item) {
			return nil, errors.New("queued message is not the next daemon-owned item")
		}
		prompt := fieldString(item, "text")
		if prompt == "" {
			return nil, errors.New("queued message is empty")
		}
		chat["queue"] = queue[1:]
		bumpAgentQueueRevision(chat)
		startedAt := time.Now().UTC().Format(time.RFC3339Nano)
		user := newSessionMessage("user", prompt, "done", startedAt)
		if id := fieldString(item, hostQueueOriginUserField); fieldString(item, "source") == "host" && id != "" {
			user["id"] = id
		}
		if images := anySlice(item["images"]); len(images) > 0 {
			user["images"] = cloneJSON(images)
		}
		user[agentQueueMessageField] = fieldString(item, "id")
		assistant := newSessionMessage("assistant", "", "running", nil)
		if id := fieldString(item, hostQueueOriginAssistantField); fieldString(item, "source") == "host" && id != "" {
			assistant["id"] = id
		}
		chat["messages"] = append(messageSlice(chat), user, assistant)
		job := &sessionJob{
			TabID: tabID, ChatID: chatID, Prompt: prompt, UserID: fieldString(user, "id"),
			AssistantID: fieldString(assistant, "id"), StartedAt: startedAt,
		}
		seedSessionJobOutput(job, assistant)
		s.pending = append(s.pending, job)
		out := map[string]any{
			"kind": "app-chat", "title": fieldString(chat, "title"), "tabId": tabID, "chatId": chatID,
			"sessionId": sessionID, "cwd": fieldString(chat, "cwd"), "providerId": fieldString(chat, "providerId"),
			"modelId": fieldString(chat, "currentModelId"), "modeId": fieldString(chat, "currentModeId"),
			"permissionMode": fieldString(chat, "currentModeId"), "prompt": prompt,
			"userMessageId": fieldString(user, "id"), "assistantMessageId": fieldString(assistant, "id"),
			"queueId": fieldString(item, "id"), "promptText": prompt,
		}
		if images := anySlice(item["images"]); len(images) > 0 {
			out["images"] = cloneJSON(images)
		}
		if err := s.beginJournalLocked(job, chat, user, assistant, fieldString(item, "id")); err != nil {
			return nil, err
		}
		s.commitSessionMutationLocked(tx)
		return out, nil
	}()
	if s.mutation == tx {
		s.abortSessionMutationLocked(tx)
	}
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	// ACP prompt input retains the historical inline image contract. Only this
	// detached turn-options tree is hydrated, after releasing the store mutex.
	if err := rehydrateExternalSessionImages(opts, filepath.Dir(s.path)); err != nil {
		return nil, err
	}
	return opts, nil
}

func (s *sessionStore) AgentCommitLiveSteer(tabID, chatID, queueID string) error {
	var archived any
	s.mu.Lock()
	tx := s.beginSessionMutationLocked()
	chat, err := s.exactChatLocked(tabID, chatID)
	if err == nil {
		queue := anySlice(chat["queue"])
		index := -1
		var prompt string
		for i, raw := range queue {
			item := mapFromAnyMain(raw)
			if fieldString(item, "id") == strings.TrimSpace(queueID) && fieldString(item, "source") == "agent" {
				index, prompt = i, fieldString(item, "text")
				break
			}
		}
		if index < 0 {
			err = errors.New("queued steer no longer exists")
		} else {
			chat["queue"] = append(queue[:index], queue[index+1:]...)
			bumpAgentQueueRevision(chat)
			message := newSessionMessage("user", prompt, "done", time.Now().UTC().Format(time.RFC3339Nano))
			message[agentQueueMessageField] = strings.TrimSpace(queueID)
			chat["messages"] = append(messageSlice(chat), message)
			archived = cloneJSON(message)
			err = s.writeLocked()
		}
	}
	if s.mutation == tx {
		s.abortSessionMutationLocked(tx)
	}
	s.mu.Unlock()
	if err == nil && archived != nil {
		err = appendChatArchive(filepath.Dir(s.path), tabID, []any{archived})
	}
	return err
}

// BeginLiveSteer durably owns the submitted user row before turn/steer is
// written to the provider. Native Codex requests set deferUntilConsumed: their
// hidden user/continuation pair is committed between sampling steps by the
// canonical userMessage receipt. Other ACP providers retain the immediate
// chronological split used by their capability contract.
func (s *sessionStore) BeginLiveSteer(tabID, chatID, clientUserMessageID, prompt, continuationAssistantMessageID string, images []any, boundary map[string]any) error {
	if !s.enabled() {
		return errors.New("session store is unavailable")
	}
	tabID = redactedSessionString(tabID)
	chatID = redactedSessionString(chatID)
	clientUserMessageID = redactedSessionString(clientUserMessageID)
	prompt = redactedSessionString(prompt)
	continuationAssistantMessageID = redactedSessionString(continuationAssistantMessageID)
	images = anySlice(redactSessionValue(images))
	imageOwner := map[string]any{"images": images}
	if err := makeSessionValueRefNative(imageOwner, filepath.Dir(s.path)); err != nil {
		return err
	}
	images = anySlice(imageOwner["images"])
	if clientUserMessageID == "" {
		return errors.New("client user message id is required")
	}

	s.mu.Lock()
	tx := s.beginSessionMutationLocked()
	defer func() {
		if s.mutation == tx {
			s.abortSessionMutationLocked(tx)
		}
		s.mu.Unlock()
	}()
	chat, err := s.exactChatLocked(tabID, chatID)
	if err != nil {
		return err
	}
	job := s.liveJobForChatLocked(tabID, chatID)
	if job == nil {
		return errors.New("no active turn owns the steer")
	}
	if continuationAssistantMessageID == "" {
		continuationAssistantMessageID = nextSessionID("a")
	}
	originalMessages := anySlice(cloneJSON(messageSlice(chat)))
	originalAssistantID, originalRootID := job.AssistantID, job.RootAssistantID
	originalContentStart, originalFinalStart := job.contentSegmentStart, job.finalSegmentStart
	if err := s.beginLiveSteerLocked(chat, job, clientUserMessageID, prompt, continuationAssistantMessageID, images, boundary); err != nil {
		chat["messages"] = originalMessages
		job.AssistantID, job.RootAssistantID = originalAssistantID, originalRootID
		job.contentSegmentStart, job.finalSegmentStart = originalContentStart, originalFinalStart
		return err
	}
	if err := s.appendJournalRecordLocked(job, map[string]any{
		"kind": "steer", "jobId": job.ID, "clientUserMessageId": clientUserMessageID,
		"prompt": prompt, "continuationAssistantMessageId": continuationAssistantMessageID,
		"images": images, "boundary": boundary,
	}, true); err != nil {
		chat["messages"] = originalMessages
		job.AssistantID, job.RootAssistantID = originalAssistantID, originalRootID
		job.contentSegmentStart, job.finalSegmentStart = originalContentStart, originalFinalStart
		return err
	}
	if err := s.writeLocked(); err != nil {
		// The fsynced per-turn journal is sufficient to recover the exact split.
		// Keep delivery moving and surface the canonical snapshot failure later.
		s.loadErr = errors.Join(s.loadErr, err)
	}
	return nil
}

func (s *sessionStore) liveJobForChatLocked(tabID, chatID string) *sessionJob {
	for index := len(s.jobOrder) - 1; index >= 0; index-- {
		job := s.jobs[s.jobOrder[index]]
		if job != nil && !job.Finished && job.TabID == tabID && (chatID == "" || job.ChatID == "" || job.ChatID == chatID) {
			return job
		}
	}
	for index := len(s.pending) - 1; index >= 0; index-- {
		job := s.pending[index]
		if job != nil && !job.Finished && job.TabID == tabID && (chatID == "" || job.ChatID == "" || job.ChatID == chatID) {
			return job
		}
	}
	return nil
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
func (s *sessionStore) CommitLiveSteerBoundary(tabID, chatID, clientUserMessageID string) error {
	if !s.enabled() {
		return errors.New("session store is unavailable")
	}
	s.mu.Lock()
	tx := s.beginSessionMutationLocked()
	defer func() {
		if s.mutation == tx {
			s.abortSessionMutationLocked(tx)
		}
		s.mu.Unlock()
	}()
	chat, err := s.exactChatLocked(redactedSessionString(tabID), redactedSessionString(chatID))
	if err != nil {
		return err
	}
	job := s.liveJobForChatLocked(fieldString(chat, "id"), fieldString(chat, "chatId"))
	if job == nil || !s.commitStagedSteerLocked(chat, job, strings.TrimSpace(clientUserMessageID)) {
		return errors.New("staged steer boundary is unavailable")
	}
	if err := s.appendJournalRecordLocked(job, map[string]any{
		"kind": "steer-boundary", "jobId": job.ID, "clientUserMessageId": clientUserMessageID,
	}, true); err != nil {
		return err
	}
	return s.writeLocked()
}

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

func (s *sessionStore) AcknowledgeLiveSteer(tabID, chatID, clientUserMessageID, prompt, outcome string) error {
	if !s.enabled() {
		return errors.New("session store is unavailable")
	}
	s.mu.Lock()
	tx := s.beginSessionMutationLocked()
	defer func() {
		if s.mutation == tx {
			s.abortSessionMutationLocked(tx)
		}
		s.mu.Unlock()
	}()
	chat, err := s.exactChatLocked(redactedSessionString(tabID), redactedSessionString(chatID))
	if err != nil {
		return err
	}
	var steer map[string]any
	for _, raw := range messageSlice(chat) {
		message := mapFromAnyMain(raw)
		if fieldString(message, "id") == strings.TrimSpace(clientUserMessageID) && fieldString(message, "role") == "user" {
			steer = message
			break
		}
	}
	if steer == nil {
		return errors.New("chronological steer row is unavailable")
	}
	state := fieldString(steer, "steerState")
	steer["status"] = "done"
	if strings.TrimSpace(outcome) == "uncertain" {
		if state != "accepted" && state != "applied" {
			steer["steerState"] = "uncertain"
		}
	} else if state != "applied" {
		steer["steerState"] = "accepted"
	}
	job := s.liveJobForChatLocked(fieldString(chat, "id"), fieldString(chat, "chatId"))
	if job != nil {
		if err := s.appendJournalRecordLocked(job, map[string]any{
			"kind": "steer-state", "jobId": job.ID, "clientUserMessageId": clientUserMessageID,
			"outcome": outcome,
		}, true); err != nil {
			return err
		}
	}
	return s.writeLocked()
}

func (s *sessionStore) RejectLiveSteer(tabID, chatID, clientUserMessageID string) error {
	if !s.enabled() {
		return errors.New("session store is unavailable")
	}
	s.mu.Lock()
	tx := s.beginSessionMutationLocked()
	defer func() {
		if s.mutation == tx {
			s.abortSessionMutationLocked(tx)
		}
		s.mu.Unlock()
	}()
	chat, err := s.exactChatLocked(redactedSessionString(tabID), redactedSessionString(chatID))
	if err != nil {
		return err
	}
	job := s.liveJobForChatLocked(fieldString(chat, "id"), fieldString(chat, "chatId"))
	originalMessages := anySlice(cloneJSON(messageSlice(chat)))
	originalAssistantID, originalRootID := "", ""
	originalContentStart, originalFinalStart := 0, 0
	if job != nil {
		originalAssistantID, originalRootID = job.AssistantID, job.RootAssistantID
		originalContentStart, originalFinalStart = job.contentSegmentStart, job.finalSegmentStart
	}
	rollback := func() {
		chat["messages"] = originalMessages
		if job != nil {
			job.AssistantID, job.RootAssistantID = originalAssistantID, originalRootID
			job.contentSegmentStart, job.finalSegmentStart = originalContentStart, originalFinalStart
		}
	}
	if err := s.rejectLiveSteerLocked(chat, job, strings.TrimSpace(clientUserMessageID)); err != nil {
		rollback()
		return err
	}
	if job != nil {
		if err := s.appendJournalRecordLocked(job, map[string]any{
			"kind": "steer-reject", "jobId": job.ID, "clientUserMessageId": clientUserMessageID,
		}, true); err != nil {
			rollback()
			return err
		}
	}
	if err := s.writeLocked(); err != nil {
		if job == nil {
			rollback()
			return err
		}
		// The fsynced journal owns the rejection and will reproduce it after a
		// crash; a canonical snapshot failure must not mislabel an explicit
		// provider rejection as transport-uncertain.
		s.loadErr = errors.Join(s.loadErr, err)
	}
	return nil
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

func (s *sessionStore) exactChatLocked(tabID, chatID string) (map[string]any, error) {
	tabID, chatID = strings.TrimSpace(tabID), strings.TrimSpace(chatID)
	if tabID == "" || chatID == "" {
		return nil, errors.New("exact tab_id and chat_id are required")
	}
	chat := s.chatLocked(tabID)
	if chat == nil || fieldString(chat, "chatId") != chatID {
		return nil, errors.New("chat identity not found")
	}
	return chat, nil
}

func (s *sessionStore) PrepareTurn(opts map[string]any) bool {
	if !s.enabled() {
		return false
	}
	opts = mapFromAnyMain(redactSessionValue(opts))
	if err := makeSessionValueRefNative(opts, filepath.Dir(s.path)); err != nil {
		s.recordLoadError(err)
		return false
	}
	tabID := fieldString(opts, "tabId")
	if tabID == "" {
		return false
	}
	prompt := firstNonEmptyString(fieldString(opts, "prompt"), fieldString(opts, "message"))
	if prompt == "" {
		return false
	}
	startedAt := time.Now().UTC().Format(time.RFC3339Nano)
	s.mu.Lock()
	tx := s.beginSessionMutationLocked()
	defer func() {
		if s.mutation == tx {
			s.abortSessionMutationLocked(tx)
		}
		s.mu.Unlock()
	}()
	s.ensureSnapshotLocked()
	chat := s.ensureChatLocked(tabID, fieldString(opts, "chatId"), fieldString(opts, "title"))
	if chat == nil {
		return false // internal subagent session: not a queueable user chat
	}
	queueID := firstNonEmptyString(fieldString(opts, "queueId"), fieldString(opts, agentQueueMessageField))
	if queueIDInAdoptedLedger(chat, queueID) {
		return false
	}
	if applyChatControls(chat, opts) {
		bumpRuntimeControlRevision(chat)
	}
	user, assistant := findTurnMessages(chat, prompt, "")
	if user == nil || !adoptableAssistantPlaceholder(chat, assistant) {
		user = newSessionMessage("user", prompt, "done", startedAt)
		assistant = newSessionMessage("assistant", "", "running", nil)
		messages := messageSlice(chat)
		chat["messages"] = append(messages, user, assistant)
	}
	// The renderer already created the optimistic rows. Adopt those exact ids so
	// session:get and the archive describe the same turn identity after a tab
	// switch/reconnect instead of manufacturing a second copy of its history.
	if id := fieldString(opts, "userMessageId"); id != "" {
		user["id"] = id
	}
	if id := fieldString(opts, "assistantMessageId"); id != "" {
		assistant["id"] = id
	}
	if planLatestAllCompleted(chat["planLatest"]) {
		chat["planLatest"] = []any{}
		chat["planLatestMessageId"] = fieldString(assistant, "id")
	}
	if images := anySlice(opts["images"]); len(images) > 0 {
		user["images"] = cloneJSON(images)
	}
	assistant["status"] = "running"
	assistant["at"] = nil
	job := &sessionJob{
		TabID:       tabID,
		ChatID:      fieldString(opts, "chatId"),
		Prompt:      prompt,
		UserID:      stringValue(user["id"]),
		AssistantID: stringValue(assistant["id"]),
		StartedAt:   startedAt,
	}
	seedSessionJobOutput(job, assistant)
	s.pending = append(s.pending, job)
	if err := s.beginJournalLocked(job, chat, user, assistant, ""); err != nil {
		// PrepareTurn cannot return an error through the frozen handler surface.
		// Retain the previous full-snapshot fallback rather than dispatching with
		// no crash-recovery record at all.
		s.loadErr = errors.Join(s.loadErr, err)
		_ = s.writeLocked()
	} else {
		s.commitSessionMutationLocked(tx)
	}
	return true
}

func (s *sessionStore) PreparedTurnPublicFields(tabID string) (map[string]string, bool) {
	if s == nil {
		return nil, false
	}
	tabID = strings.TrimSpace(tabID)
	s.mu.Lock()
	defer s.mu.Unlock()
	for index := len(s.pending) - 1; index >= 0; index-- {
		job := s.pending[index]
		if job == nil || job.TabID != tabID || job.Finished {
			continue
		}
		return map[string]string{
			"promptText": job.Prompt, "userMessageId": job.UserID, "assistantMessageId": job.AssistantID,
		}, true
	}
	return nil, false
}

func applyChatControls(chat, controls map[string]any) bool {
	if chat == nil || controls == nil {
		return false
	}
	changed := false
	if chatID := fieldString(controls, "chatId"); chatID != "" {
		chat["chatId"] = chatID
	}
	if providerID := fieldString(controls, "providerId"); providerID != "" {
		changed = changed || fieldString(chat, "providerId") != providerID
		chat["providerId"] = providerID
	}
	if modelID := firstNonEmptyString(fieldString(controls, "modelId"), fieldString(controls, "model")); modelID != "" {
		changed = changed || fieldString(chat, "currentModelId") != modelID
		chat["currentModelId"] = modelID
	}
	if modeID := firstNonEmptyString(fieldString(controls, "modeId"), fieldString(controls, "mode")); modeID != "" {
		changed = changed || fieldString(chat, "currentModeId") != modeID
		chat["currentModeId"] = modeID
	}
	return changed
}

func (s *sessionStore) FailPreparedTurn(tabID, message string) {
	if !s.enabled() {
		return
	}
	s.mu.Lock()
	tx := s.beginSessionMutationLocked()
	job := s.takePendingLocked(strings.TrimSpace(tabID))
	if job == nil {
		s.abortSessionMutationLocked(tx)
		s.mu.Unlock()
		return
	}
	finishedAt := time.Now().UTC().Format(time.RFC3339Nano)
	if assistant := s.messageForJobLocked(job); assistant != nil {
		assistant["status"] = "failed"
		assistant["content"] = stringValue(redactSessionValue(strings.TrimSpace(message)))
		delete(assistant, "result")
		assistant["at"] = finishedAt
		resetSessionJobOutputs(job, stringValue(assistant["content"]), "")
	}
	journalOK := s.appendJournalRecordLocked(job, map[string]any{
		"kind": "fail", "status": "failed", "message": message, "finishedAt": finishedAt,
	}, true) == nil
	writeErr := s.writeLocked()
	if writeErr != nil {
		s.loadErr = errors.Join(s.loadErr, writeErr)
		if s.mutation == tx {
			s.abortSessionMutationLocked(tx)
		}
		s.mu.Unlock()
		return
	}
	job.Finished = true
	messages := s.archiveJobMessagesLocked(job)
	archiveDone := s.beginArchiveLocked(job)
	s.mu.Unlock()
	archiveWarning, archiveErr := s.archiveSessionMessages(job.TabID, messages)
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.finishArchiveLocked(job, archiveDone)
	s.loadErr = errors.Join(s.loadErr, archiveWarning)
	if archiveErr != nil {
		s.loadErr = errors.Join(s.loadErr, archiveErr)
		return
	}
	if journalOK {
		if err := s.removeJobJournalLocked(job); err != nil {
			s.loadErr = errors.Join(s.loadErr, err)
		}
	}
}

func (s *sessionStore) RecordJobEvent(channel string, raw any) {
	s.recordJobEvent(channel, raw, nil)
}

func (s *sessionStore) recordJobEvent(channel string, raw any, published func()) {
	publishedCalled := false
	notifyPublished := func() {
		if publishedCalled {
			return
		}
		publishedCalled = true
		if published != nil {
			published()
		}
	}
	defer notifyPublished()
	if !s.enabled() || channel != "job:event" {
		return
	}
	payload := mapFromAnyMain(redactSessionValue(raw))
	// Provider media must never enter the live mirror or crash journal as
	// base64. Ordinary text chunks traverse no image nodes and pay only the
	// bounded payload walk.
	if err := makeSessionValueRefNative(payload, filepath.Dir(s.path)); err != nil {
		s.recordLoadError(err)
		return
	}
	typeName := fieldString(payload, "type")
	var archiveWork *sessionArchiveWork
	var journalJob *sessionJob
	var journalRecord map[string]any
	var journalSync bool
	var persistGeneration bool

	waitStart := time.Now()
	s.mu.Lock()
	waited := time.Since(waitStart)
	heldStart := time.Now()
	tx := s.beginSessionMutationLocked()
	// Job events are the one mutation family a durable deletion candidate can
	// rebase from immutable generation ownership metadata without accepting a
	// stale renderer-authored chat/control edit.
	tx.saveRebaseSafe = true
	s.ensureSnapshotLocked()
	switch typeName {
	case "start":
		jobMap := mapFromAnyMain(payload["job"])
		id := fieldString(jobMap, "id")
		tabID := fieldString(jobMap, "tabId")
		job := s.takePendingLocked(tabID)
		if job == nil {
			job = &sessionJob{TabID: tabID, ChatID: fieldString(jobMap, "chatId"), StartedAt: fieldString(jobMap, "startedAt")}
			chat := s.ensureChatLocked(tabID, job.ChatID, fieldString(jobMap, "title"))
			if chat == nil {
				// Internal subagent session: no visible chat, no turn row and no
				// journal entry. Its own registry owns the receipts.
				break
			}
			user, assistant := findTurnMessages(chat, "", "")
			if assistant == nil {
				assistant = newSessionMessage("assistant", "", "running", nil)
				chat["messages"] = append(messageSlice(chat), assistant)
			}
			job.AssistantID = stringValue(assistant["id"])
			if user != nil {
				job.UserID = stringValue(user["id"])
				job.Prompt = stringValue(user["content"])
			}
			seedSessionJobOutput(job, assistant)
			if err := s.beginJournalLocked(job, chat, user, assistant, ""); err != nil {
				s.loadErr = errors.Join(s.loadErr, err)
			}
		}
		job.ID = id
		if job.StartedAt == "" {
			job.StartedAt = fieldString(jobMap, "startedAt")
		}
		s.jobs[id] = job
		s.jobOrder = append(s.jobOrder, id)
		s.trimJobsLocked()
		if assistant := s.messageForJobLocked(job); assistant != nil {
			assistant["jobId"] = id
			assistant["status"] = "running"
			seedSessionJobOutput(job, assistant)
		}
		journalJob, journalRecord = job, map[string]any{"kind": "start", "job": jobMap}
	case "data":
		if fieldString(payload, "stream") != "stdout" {
			break
		}
		job := s.jobs[fieldString(payload, "id")]
		if job == nil || job.Finished {
			break
		}
		if assistant := s.messageForJobLocked(job); assistant != nil {
			seedSessionJobOutput(job, assistant)
			offset := job.output.Len()
			chunk := stringValue(payload["chunk"])
			phase := normalizedAssistantPhase(fieldString(payload, "phase"))
			appendSessionJobChunk(job, chunk, phase)
			materializeSessionJobOutput(job, assistant)
			assistant["status"] = "running"
			journalJob = job
			journalRecord = map[string]any{
				"kind": "data", "jobId": job.ID, "offset": offset, "chunk": chunk,
			}
			if phase != "" {
				journalRecord["phase"] = phase
			}
		}
	case "assistant-media":
		job := s.jobs[fieldString(payload, "id")]
		if job == nil || job.Finished {
			break
		}
		images := anySlice(payload["images"])
		if len(images) == 0 {
			break
		}
		if assistant := s.messageForJobLocked(job); assistant != nil {
			assistant["images"] = mergeAssistantImages(anySlice(assistant["images"]), images)
			assistant["status"] = "running"
			journalJob, journalRecord = job, map[string]any{
				"kind": "assistant-media", "jobId": job.ID, "images": images,
			}
		}
	case "acp":
		job := s.jobs[fieldString(payload, "id")]
		if job == nil || job.Finished {
			break
		}
		if assistant := s.messageForJobLocked(job); assistant != nil {
			seedSessionJobOutput(job, assistant)
			event := mapFromAnyMain(payload["event"])
			if fieldString(event, "kind") == "steer-consumed" {
				s.markSteerConsumedLocked(job, fieldString(event, "clientUserMessageId"))
			} else {
				target := s.assistantForSessionAcpEventLocked(job, event)
				applySessionAcpEventAt(target, event, utf16CodeUnits(stringValue(target["content"])))
				applyChatPlanLatest(s.chatLocked(job.TabID), target, event)
			}
			journalJob, journalRecord = job, map[string]any{
				"kind": "acp", "jobId": job.ID, "event": event,
			}
		}
	case "usage":
		if tabID, changed := s.recordContextUsageLocked(payload); changed && !s.hasUnfinishedJobForTabLocked(tabID) {
			// Usage outside a foreground turn has no later terminal boundary.
			s.persistSeq++
			persistGeneration = true
		}
	case "end":
		jobMap := mapFromAnyMain(payload["job"])
		job := s.jobs[fieldString(jobMap, "id")]
		if job == nil {
			break
		}
		s.applyJournalEndLocked(job, jobMap)
		journalJob = job
		journalRecord = map[string]any{"kind": "end", "job": jobMap}
		journalSync = true
		s.persistSeq++
		persistGeneration = true
		archiveWork = &sessionArchiveWork{
			tabID: job.TabID, messages: s.archiveJobMessagesLocked(job), job: job,
		}
	}

	generation := s.commitSessionMutationLocked(tx)
	if archiveWork != nil {
		archiveWork.done = s.beginArchiveLocked(archiveWork.job)
	}
	held := time.Since(heldStart)
	s.mu.Unlock()
	s.streamLock.observe(waited, held)

	journalOK := true
	if journalRecord != nil {
		if err := s.appendJournalRecordLocked(journalJob, journalRecord, journalSync); err != nil {
			journalOK = false
			s.recordLoadError(err)
		}
	}
	// Journal admission is ordered before the matching visible event, while its
	// write/sync and every canonical snapshot/archive operation remain outside
	// the session mutex.
	notifyPublished()
	if persistGeneration && generation != nil && generation.root != nil {
		if s.beforeGenerationMarshal != nil {
			s.beforeGenerationMarshal()
		}
		data, err := json.Marshal(generation.root)
		if err == nil {
			err = s.persistSnapshot(generation.persistenceSeq, data, nil)
		}
		if err != nil {
			s.recordLoadError(err)
		}
	}
	if archiveWork == nil {
		return
	}
	archiveWork.journalOK = journalOK
	archiveWarning, archiveErr := s.archiveSessionMessages(archiveWork.tabID, archiveWork.messages)
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.finishArchiveLocked(archiveWork.job, archiveWork.done)
	s.loadErr = errors.Join(s.loadErr, archiveWarning)
	if archiveErr != nil {
		s.loadErr = errors.Join(s.loadErr, archiveErr)
		return
	}
	if archiveWork.journalOK {
		if err := s.removeJobJournalLocked(archiveWork.job); err != nil {
			s.loadErr = errors.Join(s.loadErr, err)
		}
	}
}

// Streaming events are appended to per-turn journals immediately, but their
// file Sync is coalesced to this bounded cadence. The canonical multi-megabyte
// mirror is folded only at terminal boundaries or an explicit session save.
func (s *sessionStore) scheduleWriteLocked() {
	s.persistDirty = true
	if s.persistTimer != nil {
		return
	}
	interval := s.streamPersistInterval
	if interval <= 0 {
		interval = sessionStreamPersistInterval
	}
	s.persistTimer = time.AfterFunc(interval, s.flushScheduledWrite)
}

func (s *sessionStore) flushScheduledWrite() {
	s.journalMu.Lock()
	if s.persistTimer != nil {
		s.persistTimer.Stop()
		s.persistTimer = nil
	}
	if !s.persistDirty {
		s.journalMu.Unlock()
		return
	}
	if err := s.syncDirtyJournalsLocked(); err != nil {
		s.scheduleWriteLocked()
		s.journalMu.Unlock()
		s.recordLoadError(err)
		return
	}
	s.journalMu.Unlock()
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

func (s *sessionStore) beginJournalLocked(job *sessionJob, chat, user, assistant map[string]any, queueID string) error {
	if job == nil || assistant == nil {
		return errors.New("session journal requires an assistant turn")
	}
	if job.JournalID == "" {
		job.JournalID = fieldString(assistant, "id")
	}
	if job.JournalID == "" {
		job.JournalID = nextSessionID("turn")
	}
	chatFields := map[string]any{}
	for _, key := range []string{"id", "chatId", "title", "titleLocked", "group", "cwd", "currentModelId", "currentModeId", "providerId"} {
		if value, ok := chat[key]; ok {
			chatFields[key] = cloneJSON(value)
		}
	}
	record := map[string]any{
		"kind": "prepare", "tabId": job.TabID, "chatId": job.ChatID,
		"prompt": job.Prompt, "userId": job.UserID, "assistantId": job.AssistantID,
		"startedAt": job.StartedAt, "chat": chatFields,
		"user": cloneJSON(user), "assistant": cloneJSON(assistant),
	}
	if queueID != "" {
		record["queueId"] = queueID
	}
	return s.appendJournalRecordLocked(job, record, true)
}

func (s *sessionStore) appendJournalRecordLocked(job *sessionJob, record map[string]any, syncNow bool) error {
	s.journalMu.Lock()
	defer s.journalMu.Unlock()
	if job == nil || strings.TrimSpace(job.JournalID) == "" {
		return errors.New("session journal turn id is missing")
	}
	if s.journals == nil {
		s.journals = map[string]*sessionJournal{}
	}
	journal := s.journals[job.JournalID]
	created := false
	if journal == nil {
		if fieldString(record, "kind") != "prepare" {
			return fmt.Errorf("session journal %q cannot start with %q", job.JournalID, fieldString(record, "kind"))
		}
		path := s.journalPath(job.JournalID)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		if info, statErr := file.Stat(); statErr != nil {
			file.Close()
			_ = os.Remove(path)
			return statErr
		} else if info.Size() != 0 {
			file.Close()
			return fmt.Errorf("session journal already exists for turn %q", job.JournalID)
		}
		journal = &sessionJournal{turnID: job.JournalID, path: path, file: file}
		s.journals[job.JournalID] = journal
		created = true
	}
	rollbackFirstAppend := func(cause error) error {
		if !created {
			return cause
		}
		if journal.file != nil {
			_ = journal.file.Close()
		}
		delete(s.journals, job.JournalID)
		_ = os.Remove(journal.path)
		s.refreshJournalDirtyLocked()
		_ = os.Remove(filepath.Dir(journal.path))
		return cause
	}

	safe := mapFromAnyMain(redactSessionValue(record))
	safe["v"] = json.Number(fmt.Sprint(sessionJournalVersion))
	safe["seq"] = json.Number(fmt.Sprint(journal.seq + 1))
	safe["turnId"] = job.JournalID
	data, err := json.Marshal(safe)
	if err != nil {
		return rollbackFirstAppend(err)
	}
	data = append(data, '\n')
	writeRecord := s.writeJournalRecord
	if writeRecord == nil {
		writeRecord = writeSessionJournalRecord
	}
	if err := writeRecord(journal.file, data); err != nil {
		return rollbackFirstAppend(err)
	}
	journal.seq++
	journal.dirty = true
	if syncNow {
		if err := journal.file.Sync(); err != nil {
			return rollbackFirstAppend(err)
		}
		journal.dirty = false
		s.refreshJournalDirtyLocked()
		return nil
	}
	s.scheduleWriteLocked()
	return nil
}

func writeSessionJournalRecord(file *os.File, data []byte) error {
	for len(data) > 0 {
		n, err := file.Write(data)
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func (s *sessionStore) syncDirtyJournalsLocked() error {
	var joined error
	for _, journal := range s.journals {
		if journal == nil || !journal.dirty || journal.file == nil {
			continue
		}
		if err := journal.file.Sync(); err != nil {
			joined = errors.Join(joined, err)
			continue
		}
		journal.dirty = false
	}
	s.refreshJournalDirtyLocked()
	return joined
}

func (s *sessionStore) refreshJournalDirtyLocked() {
	s.persistDirty = false
	for _, journal := range s.journals {
		if journal != nil && journal.dirty {
			s.persistDirty = true
			break
		}
	}
	if !s.persistDirty && s.persistTimer != nil {
		s.persistTimer.Stop()
		s.persistTimer = nil
	}
}

func (s *sessionStore) journalPath(turnID string) string {
	digest := sha256.Sum256([]byte(turnID))
	return filepath.Join(filepath.Dir(s.path), sessionJournalDirname, fmt.Sprintf("%x.jsonl", digest[:]))
}

func (s *sessionStore) removeJobJournalLocked(job *sessionJob) error {
	if job == nil || job.JournalID == "" {
		return nil
	}
	return s.removeJournalLocked(job.JournalID, s.journalPath(job.JournalID))
}

func (s *sessionStore) removeJournalLocked(turnID, path string) error {
	s.journalMu.Lock()
	defer s.journalMu.Unlock()
	if journal := s.journals[turnID]; journal != nil {
		if journal.dirty && journal.file != nil {
			if err := journal.file.Sync(); err != nil {
				return err
			}
		}
		if journal.file != nil {
			if err := journal.file.Close(); err != nil {
				return err
			}
		}
		delete(s.journals, turnID)
	}
	if path == "" {
		path = s.journalPath(turnID)
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	s.refreshJournalDirtyLocked()
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

func (s *sessionStore) beginArchiveLocked(job *sessionJob) chan struct{} {
	if job == nil {
		return nil
	}
	key := firstNonEmptyString(job.ID, job.JournalID, job.AssistantID)
	if key == "" {
		return nil
	}
	if s.archivePending == nil {
		s.archivePending = map[string]chan struct{}{}
	}
	if s.archivePendingTabs == nil {
		s.archivePendingTabs = map[string]string{}
	}
	if s.archivePending[key] != nil {
		return nil
	}
	done := make(chan struct{})
	s.archivePending[key] = done
	s.archivePendingTabs[key] = job.TabID
	s.republishArchiveFencesLocked()
	return done
}

func (s *sessionStore) finishArchiveLocked(job *sessionJob, done chan struct{}) {
	if done == nil {
		return
	}
	if job != nil {
		key := firstNonEmptyString(job.ID, job.JournalID, job.AssistantID)
		if s.archivePending[key] == done {
			delete(s.archivePending, key)
			delete(s.archivePendingTabs, key)
		}
	}
	close(done)
	s.republishArchiveFencesLocked()
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
		path:                  s.path,
		jobs:                  make(map[string]*sessionJob, len(s.jobs)),
		jobOrder:              append([]string(nil), s.jobOrder...),
		journals:              map[string]*sessionJournal{},
		droppedChatIDs:        s.droppedChatIDs,
		streamPersistInterval: s.streamPersistInterval,
		writeJournalRecord:    s.writeJournalRecord,
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
				Prompt: fieldString(record, "prompt"), UserID: fieldString(record, "userId"),
				AssistantID: assistantID, StartedAt: fieldString(record, "startedAt"),
				JournalID: turnID, Finished: true,
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
			bumpAgentQueueRevision(chat)
		}
	}
	user := mapFromAnyMain(record["user"])
	assistant := mapFromAnyMain(record["assistant"])
	if fieldString(assistant, "id") != assistantID {
		return nil, false, errors.New("session journal assistant id does not match prepare identity")
	}
	resetJournalTurnMessages(chat, user, assistant)
	job := &sessionJob{
		TabID: tabID, ChatID: chatID, Prompt: fieldString(record, "prompt"),
		UserID: fieldString(record, "userId"), AssistantID: assistantID,
		StartedAt: fieldString(record, "startedAt"), JournalID: turnID,
	}
	seedSessionJobOutput(job, assistant)
	s.pending = append(s.pending, job)
	return job, false, nil
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
				if s.mutation != nil {
					message = s.mutation.messageForWrite(job.TabID, fieldString(message, "id"))
				}
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
	if s.mutation != nil {
		s.snapshot = s.mutation.rootForWrite()
	}
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
	if s.mutation != nil {
		if chat := s.mutation.chatForWrite(tabID); chat != nil {
			if fieldString(chat, "chatId") == "" && chatID != "" {
				chat["chatId"] = chatID
			}
			return chat
		}
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
	if s.mutation != nil {
		s.mutation.chatPositions[tabID] = len(anySlice(s.snapshot["chats"])) - 1
		s.mutation.changed = true
	}
	if s.snapshot["activeId"] == nil {
		s.snapshot["activeId"] = tabID
	}
	return chat
}

func (s *sessionStore) assistantForEventLocked(id string) map[string]any {
	job := s.jobs[id]
	if job == nil {
		return nil
	}
	return s.messageForJobLocked(job)
}

func (s *sessionStore) messageForJobLocked(job *sessionJob) map[string]any {
	chat := s.chatLocked(job.TabID)
	if chat == nil {
		return nil
	}
	for _, raw := range messageSlice(chat) {
		message := mapFromAnyMain(raw)
		if fieldString(message, "role") == "assistant" && stringValue(message["id"]) == job.AssistantID {
			if s.mutation != nil {
				return s.mutation.messageForWrite(job.TabID, job.AssistantID)
			}
			return message
		}
	}
	if job.ID != "" {
		messages := messageSlice(chat)
		for index := len(messages) - 1; index >= 0; index-- {
			message := mapFromAnyMain(messages[index])
			if fieldString(message, "role") == "assistant" && stringValue(message["jobId"]) == job.ID {
				job.AssistantID = fieldString(message, "id")
				if s.mutation != nil {
					return s.mutation.messageForWrite(job.TabID, job.AssistantID)
				}
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
				if s.mutation != nil {
					return s.mutation.messageForWrite(job.TabID, fieldString(message, "id"))
				}
				return message
			}
		}
	}
	return current
}

func (s *sessionStore) userForJobLocked(job *sessionJob) map[string]any {
	chat := s.chatLocked(job.TabID)
	if chat == nil {
		return nil
	}
	for _, raw := range messageSlice(chat) {
		message := mapFromAnyMain(raw)
		if stringValue(message["id"]) == job.UserID {
			if s.mutation != nil {
				return s.mutation.messageForWrite(job.TabID, job.UserID)
			}
			return message
		}
	}
	return nil
}

func (s *sessionStore) chatLocked(tabID string) map[string]any {
	if s.mutation != nil {
		return s.mutation.chatForWrite(tabID)
	}
	for _, raw := range anySlice(s.snapshot["chats"]) {
		chat := mapFromAnyMain(raw)
		if fieldString(chat, "id") == tabID {
			return chat
		}
	}
	return nil
}

func (s *sessionStore) recordContextUsageLocked(payload map[string]any) (string, bool) {
	tabID := fieldString(payload, "tabId")
	chatID := fieldString(payload, "chatId")
	providerID := fieldString(payload, "providerId")
	if tabID == "" || providerID == "" || len(providerID) > 256 {
		return tabID, false
	}
	chat := s.chatLocked(tabID)
	if chat == nil || (chatID != "" && fieldString(chat, "chatId") != chatID) {
		return tabID, false
	}
	used, usedOK := intFieldOK(payload, "used")
	size, sizeOK := intFieldOK(payload, "size")
	if !usedOK || !sizeOK || used < 0 || size <= 0 {
		return tabID, false
	}
	updatedAt := fieldString(payload, "updatedAt")
	if len(updatedAt) > 128 {
		updatedAt = ""
	}
	if _, err := time.Parse(time.RFC3339Nano, updatedAt); err != nil {
		updatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	byProvider := mapFromAnyMain(cloneJSON(mapFromAnyMain(chat["contextUsageByProvider"])))
	byProvider[providerID] = map[string]any{
		"used": used, "size": size, "updatedAt": updatedAt,
	}
	chat["contextUsageByProvider"] = byProvider
	return tabID, true
}

func (s *sessionStore) hasUnfinishedJobForTabLocked(tabID string) bool {
	if tabID == "" {
		return false
	}
	for _, job := range s.pending {
		if job != nil && !job.Finished && job.TabID == tabID {
			return true
		}
	}
	for _, job := range s.jobs {
		if job != nil && !job.Finished && job.TabID == tabID {
			return true
		}
	}
	return false
}

func (s *sessionStore) takePendingLocked(tabID string) *sessionJob {
	for i := len(s.pending) - 1; i >= 0; i-- {
		if s.pending[i].TabID == tabID {
			job := s.pending[i]
			s.pending = append(s.pending[:i], s.pending[i+1:]...)
			return job
		}
	}
	return nil
}

func mergeAuthoritativeTurns(incoming map[string]any, base *sessionGeneration, preserveOmissions bool, explicitDeletes map[string]struct{}) (map[string]any, bool) {
	if base == nil || base.root == nil {
		return incoming, true
	}
	out := incoming
	if out == nil {
		out = map[string]any{}
	}
	if _, ok := out["chats"].([]any); !ok {
		out["chats"] = []any{}
	}
	serverChats := make(map[string]map[string]any, len(anySlice(base.root["chats"])))
	for _, raw := range anySlice(base.root["chats"]) {
		chat := mapFromAnyMain(raw)
		if tabID := fieldString(chat, "id"); tabID != "" {
			serverChats[tabID] = chat
		}
	}
	incomingChats := make(map[string]map[string]any)
	submittedChats := make(map[string]struct{})
	for _, raw := range anySlice(out["chats"]) {
		chat := mapFromAnyMain(raw)
		if tabID := fieldString(chat, "id"); tabID != "" {
			incomingChats[tabID] = chat
			submittedChats[tabID] = struct{}{}
		}
	}
	for tabID, clientChat := range incomingChats {
		if serverChat := serverChats[tabID]; serverChat != nil {
			clientChat["messages"] = mergeAgentQueueMessageRows(messageSlice(clientChat), messageSlice(serverChat))
			if planLatest, exists := serverChat["planLatest"]; exists {
				clientChat["planLatest"] = cloneJSON(planLatest)
				if ownerID, ownerExists := serverChat["planLatestMessageId"]; ownerExists {
					clientChat["planLatestMessageId"] = cloneJSON(ownerID)
				} else {
					delete(clientChat, "planLatestMessageId")
				}
			} else {
				delete(clientChat, "planLatest")
				delete(clientChat, "planLatestMessageId")
			}
			mergedUsage := mergeContextUsageByProvider(
				mapFromAnyMain(clientChat["contextUsageByProvider"]),
				mapFromAnyMain(serverChat["contextUsageByProvider"]),
			)
			if len(mergedUsage) > 0 {
				clientChat["contextUsageByProvider"] = mergedUsage
			} else {
				delete(clientChat, "contextUsageByProvider")
			}
		}
	}
	for _, job := range base.ownedTurns {
		// An explicitly closed chat stays closed. This loop restores any chat a
		// job trace still points at, and without this guard it silently undid
		// every close of a chat that still had a live or pending job: the row
		// came back on the same save, the renderer's post-save verification saw
		// it present, and the pending delete retried forever (user 2026-07-24).
		// The serverAuthored loop below already honours the same marker.
		if _, deleted := explicitDeletes[job.tabID]; deleted {
			continue
		}
		if isInternalTabID(job.tabID) {
			continue
		}
		serverChat := serverChats[job.tabID]
		if serverChat == nil {
			continue
		}
		clientChat := incomingChats[job.tabID]
		if clientChat == nil {
			out["chats"] = append(anySlice(out["chats"]), serverChat)
			incomingChats[job.tabID] = serverChat
			continue
		}
		if !ownedTurnPresent(serverChat, job) {
			continue
		}
		clientChat["messages"] = mergeAuthoritativeJobRows(messageSlice(clientChat), serverChat, job)
	}
	for _, raw := range anySlice(base.root["chats"]) {
		serverChat := mapFromAnyMain(raw)
		serverAuthored, _ := boolField(serverChat, "serverAuthored")
		if !serverAuthored {
			continue
		}
		tabID := fieldString(serverChat, "id")
		if tabID == "" {
			continue
		}
		if _, deleted := explicitDeletes[tabID]; deleted {
			continue
		}
		if incomingChat := incomingChats[tabID]; incomingChat != nil {
			if _, submitted := submittedChats[tabID]; submitted {
				delete(incomingChat, "serverAuthored")
			}
			continue
		}
		if incomingChats[tabID] == nil {
			out["chats"] = append(anySlice(out["chats"]), serverChat)
			incomingChats[tabID] = serverChat
		}
	}
	if preserveOmissions {
		for _, raw := range anySlice(base.root["chats"]) {
			serverChat := mapFromAnyMain(raw)
			tabID := fieldString(serverChat, "id")
			if tabID == "" {
				continue
			}
			if _, deleted := explicitDeletes[tabID]; deleted {
				continue
			}
			if incomingChats[tabID] == nil {
				out["chats"] = append(anySlice(out["chats"]), serverChat)
				incomingChats[tabID] = serverChat
			}
		}
	}
	return out, true
}

func mergeContextUsageByProvider(client, server map[string]any) map[string]any {
	out := map[string]any{}
	for providerID, raw := range client {
		if providerID != "" && len(providerID) <= 256 {
			out[providerID] = cloneJSON(raw)
		}
	}
	// Daemon-observed provider usage is authoritative over any stale renderer
	// save racing the same chat. Client-only provider snapshots are retained for
	// rolling upgrades and older session mirrors.
	for providerID, raw := range server {
		if providerID != "" && len(providerID) <= 256 {
			out[providerID] = cloneJSON(raw)
		}
	}
	return out
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

type stagedDroppedChatTombstone struct {
	tabID string
	data  []byte
}

func (s *sessionStore) stageDroppedChatTombstonesLocked(out map[string]any) ([]stagedDroppedChatTombstone, error) {
	return stageDroppedChatTombstones(s.snapshot, out)
}

func stageDroppedChatTombstones(baseRoot, out map[string]any) ([]stagedDroppedChatTombstone, error) {
	preserved := make(map[string]struct{})
	for _, raw := range anySlice(out["chats"]) {
		if tabID := fieldString(mapFromAnyMain(raw), "id"); tabID != "" {
			preserved[tabID] = struct{}{}
		}
	}
	dropped := make([]stagedDroppedChatTombstone, 0)
	for _, raw := range anySlice(baseRoot["chats"]) {
		chat := mapFromAnyMain(raw)
		tabID := fieldString(chat, "id")
		if tabID == "" {
			continue
		}
		if _, ok := preserved[tabID]; ok {
			continue
		}
		if strings.ContainsAny(tabID, `/\`) {
			return nil, fmt.Errorf("dropped chat tab id %q cannot be used as a tombstone filename", tabID)
		}
		data, err := json.Marshal(chat)
		if err != nil {
			return nil, err
		}
		dropped = append(dropped, stagedDroppedChatTombstone{tabID: tabID, data: data})
	}
	sort.Slice(dropped, func(i, j int) bool { return dropped[i].tabID < dropped[j].tabID })
	return dropped, nil
}

func (s *sessionStore) persistDroppedChatTombstones(tombstones []stagedDroppedChatTombstone) error {
	if len(tombstones) == 0 {
		return nil
	}
	dir := filepath.Join(filepath.Dir(s.path), droppedChatDirname)
	dirExisted := true
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		dirExisted = false
	} else if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if !dirExisted {
		if err := syncDirectory(filepath.Dir(dir)); err != nil {
			return err
		}
	}
	for _, tombstone := range tombstones {
		tmp, err := os.CreateTemp(dir, ".dropped-chat-*.tmp")
		if err != nil {
			return err
		}
		tmpName := tmp.Name()
		if err := tmp.Chmod(0o600); err != nil {
			tmp.Close()
			_ = os.Remove(tmpName)
			return err
		}
		if _, err := tmp.Write(tombstone.data); err != nil {
			tmp.Close()
			_ = os.Remove(tmpName)
			return err
		}
		if err := tmp.Sync(); err != nil {
			tmp.Close()
			_ = os.Remove(tmpName)
			return err
		}
		if err := tmp.Close(); err != nil {
			_ = os.Remove(tmpName)
			return err
		}
		target := filepath.Join(dir, tombstone.tabID+".json")
		if err := os.Rename(tmpName, target); err != nil {
			_ = os.Remove(tmpName)
			return err
		}
	}
	if err := syncDirectory(dir); err != nil {
		return err
	}
	tabIDs := make([]string, 0, len(tombstones))
	for _, tombstone := range tombstones {
		tabIDs = append(tabIDs, tombstone.tabID)
	}
	sessionStoreLogLine(fmt.Sprintf("session save dropped chats count=%d tabIds=%s", len(tabIDs), strings.Join(tabIDs, ",")))
	return nil
}

func (s *sessionStore) recordDroppedChatTombstonesLocked(tombstones []stagedDroppedChatTombstone) {
	if s.droppedChatIDs == nil {
		s.droppedChatIDs = map[string]struct{}{}
	}
	for _, tombstone := range tombstones {
		s.droppedChatIDs[tombstone.tabID] = struct{}{}
	}
}

func (s *sessionStore) writeDroppedChatLocked(tabID string, chat map[string]any) error {
	if strings.ContainsAny(tabID, `/\`) {
		return fmt.Errorf("dropped chat tab id %q cannot be used as a tombstone filename", tabID)
	}
	dir := filepath.Join(filepath.Dir(s.path), droppedChatDirname)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(chat)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, tabID+".json"), data, 0o600); err != nil {
		return err
	}
	if s.droppedChatIDs == nil {
		s.droppedChatIDs = map[string]struct{}{}
	}
	s.droppedChatIDs[tabID] = struct{}{}
	return nil
}

func ownedTurnPresent(serverChat map[string]any, job sessionOwnedTurn) bool {
	for _, raw := range messageSlice(serverChat) {
		message := mapFromAnyMain(raw)
		if fieldString(message, "id") == job.assistantID ||
			(job.id != "" && fieldString(message, "jobId") == job.id) {
			return true
		}
	}
	return false
}

func mergeAuthoritativeJobRows(clientMessages []any, serverChat map[string]any, job sessionOwnedTurn) []any {
	if serverChat == nil {
		return clientMessages
	}
	rootID := firstNonEmptyString(job.rootAssistantID, job.assistantID)
	var serverRows []any
	for _, raw := range messageSlice(serverChat) {
		message := mapFromAnyMain(raw)
		if fieldString(message, "id") == job.userID || fieldString(message, "turnRootId") == rootID || fieldString(message, "id") == job.assistantID {
			serverRows = append(serverRows, message)
		}
	}
	if len(serverRows) == 0 {
		return clientMessages
	}
	belongs := func(message map[string]any) bool {
		return fieldString(message, "id") == job.userID || fieldString(message, "id") == job.assistantID || fieldString(message, "turnRootId") == rootID || (job.id != "" && fieldString(message, "jobId") == job.id)
	}
	insertAt := len(clientMessages)
	out := make([]any, 0, len(clientMessages)+len(serverRows))
	for _, raw := range clientMessages {
		if belongs(mapFromAnyMain(raw)) {
			if insertAt == len(clientMessages) {
				insertAt = len(out)
			}
			continue
		}
		out = append(out, raw)
	}
	if insertAt > len(out) {
		insertAt = len(out)
	}
	merged := make([]any, 0, len(out)+len(serverRows))
	merged = append(merged, out[:insertAt]...)
	merged = append(merged, serverRows...)
	merged = append(merged, out[insertAt:]...)
	return merged
}

func steerStateRank(state string) int {
	switch strings.TrimSpace(state) {
	case "sending":
		return 0
	case "uncertain":
		return 1
	case "accepted":
		return 2
	case "applied":
		return 3
	default:
		return -1
	}
}

// mergeLeanPayloads restores daemon-owned heavy fields into a
// capability-gated renderer save. Missing rows remain deleted; only exact
// chat/message/queue ids can inherit payloads from the current snapshot.
func mergeLeanPayloads(incoming map[string]any, base *sessionGeneration) {
	if base == nil || base.root == nil || incoming == nil {
		return
	}
	for _, chatRaw := range anySlice(incoming["chats"]) {
		incomingChat := mapFromAnyMain(chatRaw)
		tabID := fieldString(incomingChat, "id")
		existingChat := base.chatsByTab[tabID]
		if existingChat == nil {
			continue
		}

		existingMessages := base.messagesByTab[tabID]
		for _, messageRaw := range messageSlice(incomingChat) {
			message := mapFromAnyMain(messageRaw)
			existing := existingMessages[fieldString(message, "id")]
			if existing == nil {
				continue
			}
			mergeLeanMessagePayloads(message, existing)
		}

		existingQueue := make(map[string]map[string]any)
		for _, itemRaw := range anySlice(existingChat["queue"]) {
			item := mapFromAnyMain(itemRaw)
			if id := fieldString(item, "id"); id != "" {
				existingQueue[id] = item
			}
		}
		for _, itemRaw := range anySlice(incomingChat["queue"]) {
			item := mapFromAnyMain(itemRaw)
			existing := existingQueue[fieldString(item, "id")]
			if existing == nil {
				continue
			}
			if _, present := item["images"]; !present {
				if images, exists := existing["images"]; exists {
					item["images"] = images
				}
			}
		}
	}
}

func mergeLeanMessagePayloads(message, existing map[string]any) {
	if message == nil || existing == nil {
		return
	}
	if _, present := message["images"]; !present {
		if images, exists := existing["images"]; exists {
			message["images"] = images
		}
	}
	if overlays, present := message["events"]; present {
		message["events"] = mergeLeanSessionEvents(existing["events"], overlays)
	} else if events, exists := existing["events"]; exists {
		message["events"] = events
	}
}

func mergeLeanSessionEvents(existingRaw, overlayRaw any) []any {
	existing := anySlice(existingRaw)
	overlays := anySlice(overlayRaw)
	existingTools := make([]map[string]any, 0)
	toolByIdentity := make(map[string]int)
	for _, raw := range existing {
		event := mapFromAnyMain(raw)
		if fieldString(event, "kind") != "tool" {
			continue
		}
		index := len(existingTools)
		existingTools = append(existingTools, event)
		for _, identity := range leanToolIdentities(event) {
			toolByIdentity[identity] = index
		}
	}
	usedTools := make(map[int]struct{}, len(existingTools))
	out := make([]any, 0, len(existing)+len(overlays))
	for _, raw := range overlays {
		event := mapFromAnyMain(raw)
		if fieldString(event, "kind") != "tool" {
			out = append(out, event)
			continue
		}
		index := -1
		for _, identity := range leanToolIdentities(event) {
			if candidate, ok := toolByIdentity[identity]; ok {
				index = candidate
				break
			}
		}
		if index < 0 {
			continue
		}
		usedTools[index] = struct{}{}
		merged := existingTools[index]
		var copied bool
		for _, key := range []string{"startedAt", "endedAt", "subagentModel"} {
			if value, present := event[key]; present {
				if !copied {
					merged = shallowCopyStringMap(existingTools[index])
					copied = true
				}
				merged[key] = value
			}
		}
		out = append(out, merged)
	}
	for index, event := range existingTools {
		if _, used := usedTools[index]; !used {
			out = append(out, event)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return intValue(mapFromAnyMain(out[i])["at"]) < intValue(mapFromAnyMain(out[j])["at"])
	})
	return out
}

func leanToolIdentities(event map[string]any) []string {
	out := make([]string, 0, 3)
	for _, key := range []string{"id", "terminalId", "key"} {
		if value := fieldString(event, key); value != "" {
			out = append(out, key+":"+value)
		}
	}
	return out
}

func (s *sessionStore) trimJobsLocked() {
	const retainedJobs = 256
	for len(s.jobOrder) > retainedJobs {
		id := s.jobOrder[0]
		s.jobOrder = s.jobOrder[1:]
		delete(s.jobs, id)
	}
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
	if s.mutation != nil {
		s.commitSessionMutationLocked(s.mutation)
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

func prepareSessionSnapshotForPersistence(snapshot map[string]any, stateDir string) (map[string]any, error) {
	out, images, err := stageSessionSnapshotForPersistence(snapshot, stateDir)
	if err != nil {
		return nil, err
	}
	if err := persistSessionImagePlans(images); err != nil {
		return nil, err
	}
	return out, nil
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

func externalizeSessionImageData(value any, stateDir string) error {
	plans, err := stageExternalSessionImageData(value, stateDir)
	if err != nil {
		return err
	}
	return persistSessionImagePlans(plans)
}

func persistSessionImagePlans(plans []sessionImageWritePlan) error {
	for _, plan := range plans {
		if err := persistSessionImagePlan(plan); err != nil {
			return err
		}
	}
	return nil
}

func persistSessionImageData(stateDir, data string) (string, error) {
	name := sessionImageName(data)
	ref := sessionImageDirname + "/" + name
	dir := filepath.Join(stateDir, sessionImageDirname)
	path := filepath.Join(dir, name)
	plan := sessionImageWritePlan{ref: ref, dir: dir, path: path, data: data}
	if err := persistSessionImagePlan(plan); err != nil {
		return "", err
	}
	return ref, nil
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
		if s.beforePersistImages != nil {
			s.beforePersistImages()
		}
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
	s.snapshotByteEstimate.Store(int64(len(data)))
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

func findTurnMessages(chat map[string]any, prompt, jobID string) (map[string]any, map[string]any) {
	messages := messageSlice(chat)
	if jobID != "" {
		for i := len(messages) - 1; i >= 0; i-- {
			assistant := mapFromAnyMain(messages[i])
			if fieldString(assistant, "jobId") != jobID {
				continue
			}
			for j := i - 1; j >= 0; j-- {
				user := mapFromAnyMain(messages[j])
				if fieldString(user, "role") == "user" {
					return user, assistant
				}
			}
			return nil, assistant
		}
	}
	for i := len(messages) - 1; i >= 0; i-- {
		user := mapFromAnyMain(messages[i])
		if fieldString(user, "role") != "user" || (prompt != "" && stringValue(user["content"]) != prompt) {
			continue
		}
		if i+1 < len(messages) {
			assistant := mapFromAnyMain(messages[i+1])
			if fieldString(assistant, "role") == "assistant" {
				return user, assistant
			}
		}
		return user, nil
	}
	if prompt == "" {
		for i := len(messages) - 1; i >= 0; i-- {
			assistant := mapFromAnyMain(messages[i])
			if fieldString(assistant, "role") == "assistant" {
				return nil, assistant
			}
		}
	}
	return nil, nil
}

func adoptableAssistantPlaceholder(chat map[string]any, assistant map[string]any) bool {
	if assistant == nil {
		return false
	}
	messages := messageSlice(chat)
	if len(messages) == 0 || stringValue(mapFromAnyMain(messages[len(messages)-1])["id"]) != stringValue(assistant["id"]) {
		return false
	}
	status := fieldString(assistant, "status")
	if status == "running" || status == "pending" {
		return true
	}
	return stringValue(assistant["content"]) == "" && stringValue(assistant["result"]) == "" && assistant["at"] == nil && fieldString(assistant, "jobId") == ""
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

func chatFromSnapshot(snapshot map[string]any, tabID string) map[string]any {
	for _, raw := range anySlice(snapshot["chats"]) {
		chat := mapFromAnyMain(raw)
		if fieldString(chat, "id") == tabID {
			return chat
		}
	}
	return nil
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
