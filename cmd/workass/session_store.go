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
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"workass/internal/acp"
	"workass/internal/durablefs"
	providercontract "workass/internal/provider"
)

const (
	sessionStateFilename                = "session-state.json"
	sessionImageDirname                 = "images"
	sessionImageDataRefField            = "_workassImageDataRef"
	maxPersistedSessionImageBytes       = 64 * 1024 * 1024
	workspaceRevisionField              = "workspaceRevision"
	presentationRevisionField           = "presentationRevision"
	agentQueueRevisionField             = "agentQueueRevision"
	runtimeControlRevisionField         = "runtimeControlRevision"
	globalPresentationRevisionField     = "globalRevision"
	globalPresentationOperationField    = "_workassGlobalOperationId"
	globalPresentationReceiptsField     = "_workassGlobalMutationReceipts"
	globalPresentationReceiptOrderField = "_workassGlobalMutationReceiptOrder"
	agentQueueMessageField              = "agentQueueId"
	providerSessionImageRefPrefix       = "workass-session-image:"
	// Global presentation operations are retried only across a bounded transport
	// window. Retaining every completed id forever turned a tiny view-settings
	// file into a multi-megabyte append-only ledger and made every save clone,
	// hash, marshal, fsync, and garbage-collect the entire history.
	globalPresentationReceiptLimit = 512
)

var (
	sessionStoresMu sync.Mutex
	sessionStores   = map[string]*sessionStore{}
	sessionIDSeq    atomic.Uint64
)

// sessionStore owns daemon-global presentation and content-addressed
// attachments. Chat state is projected exclusively from durable actors.
type sessionStore struct {
	mu   sync.Mutex
	path string

	published  atomic.Pointer[sessionGeneration]
	snapshot   map[string]any
	loadErr    error
	persistSeq uint64

	persistMu  sync.Mutex
	writtenSeq uint64
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
		return &sessionStore{}
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
	store := &sessionStore{path: strings.TrimSpace(path)}
	if store.path == "" {
		return store
	}
	raw, err := os.ReadFile(store.path)
	if errors.Is(err, os.ErrNotExist) {
		store.snapshot = actorGlobalSessionSnapshot(nil)
		store.published.Store(newSessionGeneration(store.snapshot))
		return store
	}
	if err != nil {
		store.loadErr = err
		return store
	}
	var snapshot map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&snapshot); err != nil {
		store.loadErr = err
		return store
	}
	snapshot = mapFromAnyMain(redactSessionValue(snapshot))
	if len(anySlice(snapshot["chats"])) != 0 {
		store.loadErr = errors.New("session-state contains chat rows; this release requires canonical actor storage")
		return store
	}
	store.snapshot = actorGlobalSessionSnapshot(snapshot)
	receipts, receiptOrder, compacted := boundedGlobalPresentationReceipts(
		snapshot[globalPresentationReceiptsField], snapshot[globalPresentationReceiptOrderField],
	)
	if len(receipts) != 0 {
		store.snapshot[globalPresentationReceiptsField] = receipts
		store.snapshot[globalPresentationReceiptOrderField] = stringsToAny(receiptOrder)
	}
	// Compact legacy unbounded receipt ledgers at the first boot on this build.
	// A failed best-effort rewrite does not invalidate the already-valid global
	// snapshot: memory is bounded now, and the next real mutation retries the
	// same atomic persistence path.
	if compacted {
		if data, marshalErr := json.Marshal(store.snapshot); marshalErr == nil {
			if persistErr := store.persistSnapshot(1, data, nil); persistErr == nil {
				store.persistSeq = 1
			}
		}
	}
	store.published.Store(newSessionGeneration(store.snapshot))
	return store
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
	return out
}

var actorGlobalSessionFields = []string{
	"v", "activeId", "seq", "chatOrder", "workspaces", "collapsedWorkspaces", "removedWorkspaces",
	"theme", "themePref", "density", "panes", "mode", "notifEnabled", globalPresentationRevisionField,
}

const (
	actorGlobalChatOrderLimit = 4096
	actorGlobalTabIDLimit     = 256
)

func actorGlobalSessionSnapshot(source map[string]any) map[string]any {
	out := map[string]any{
		"v":        json.Number("1"),
		"activeId": nil,
		"seq":      json.Number("0"),
		"chats":    []any{},
	}
	for _, key := range actorGlobalSessionFields {
		if value, exists := source[key]; exists {
			if key == "chatOrder" {
				out[key] = normalizedActorChatOrder(value)
				continue
			}
			out[key] = cloneJSON(value)
		}
	}
	return mapFromAnyMain(redactSessionValue(out))
}

func normalizedActorChatOrder(raw any) []any {
	var values []any
	switch typed := raw.(type) {
	case []any:
		values = typed
	case []string:
		values = make([]any, len(typed))
		for index, value := range typed {
			values[index] = value
		}
	default:
		return []any{}
	}
	out := make([]any, 0, min(len(values), actorGlobalChatOrderLimit))
	seen := make(map[string]struct{}, min(len(values), actorGlobalChatOrderLimit))
	for _, rawValue := range values {
		value, ok := rawValue.(string)
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		if value == "" || len(value) > actorGlobalTabIDLimit {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
		if len(out) == actorGlobalChatOrderLimit {
			break
		}
	}
	return out
}

// GlobalSnapshot exposes only daemon-wide presentation preferences. It cannot
// return, merge, or recover a chat row.
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

type globalPresentationSaveResult struct {
	Revision uint64
	Changed  bool
}

// SaveActorGlobalSnapshot persists daemon-global UI preferences only.
func (s *sessionStore) SaveActorGlobalSnapshot(raw any) (globalPresentationSaveResult, error) {
	if s == nil || !s.enabled() {
		return globalPresentationSaveResult{}, errors.New("session presentation store is unavailable")
	}
	incoming := mapFromAnyMain(redactSessionValue(raw))
	if incoming == nil {
		return globalPresentationSaveResult{}, errors.New("renderer session snapshot is not an object")
	}
	operationID, operationErr := providercontract.ValidateOperationID(fieldString(incoming, globalPresentationOperationField))
	if operationErr != nil {
		return globalPresentationSaveResult{}, fmt.Errorf("global presentation save requires a stable operation id: %w", operationErr)
	}
	expectedRevision := uint64(max(0, intValue(incoming[globalPresentationRevisionField])))
	root := actorGlobalSessionSnapshot(incoming)
	digest := globalPresentationDigest(root)
	s.mu.Lock()
	defer s.mu.Unlock()
	currentRevision := uint64(max(0, intValue(s.snapshot[globalPresentationRevisionField])))
	receipts, receiptOrder, _ := boundedGlobalPresentationReceipts(
		s.snapshot[globalPresentationReceiptsField], s.snapshot[globalPresentationReceiptOrderField],
	)
	if existing := mapFromAnyMain(receipts[string(operationID)]); len(existing) > 0 {
		if fieldString(existing, "digest") != digest {
			return globalPresentationSaveResult{}, errors.New("global presentation operation id was reused for different content")
		}
		return globalPresentationSaveResult{Revision: uint64(max(0, intValue(existing["revision"])))}, nil
	}
	if expectedRevision != currentRevision {
		return globalPresentationSaveResult{}, errors.New("daemon-global presentation changed in another controller; reload before saving")
	}
	changed := digest != globalPresentationDigest(actorGlobalSessionSnapshot(s.snapshot))
	if changed {
		currentRevision++
	}
	root[globalPresentationRevisionField] = json.Number(fmt.Sprint(currentRevision))
	receipts[string(operationID)] = map[string]any{"digest": digest, "revision": json.Number(fmt.Sprint(currentRevision))}
	receiptOrder = append(receiptOrder, string(operationID))
	if len(receiptOrder) > globalPresentationReceiptLimit {
		for _, expired := range receiptOrder[:len(receiptOrder)-globalPresentationReceiptLimit] {
			delete(receipts, expired)
		}
		receiptOrder = receiptOrder[len(receiptOrder)-globalPresentationReceiptLimit:]
	}
	root[globalPresentationReceiptsField] = receipts
	root[globalPresentationReceiptOrderField] = stringsToAny(receiptOrder)
	data, err := json.Marshal(root)
	if err != nil {
		return globalPresentationSaveResult{}, err
	}
	seq := s.persistSeq + 1
	if err := s.persistSnapshot(seq, data, nil); err != nil {
		return globalPresentationSaveResult{}, err
	}
	s.persistSeq = seq
	s.snapshot = root
	s.published.Store(newSessionGeneration(root))
	return globalPresentationSaveResult{Revision: currentRevision, Changed: changed}, nil
}

type globalPresentationReceiptCandidate struct {
	id       string
	receipt  any
	revision int
	timeKey  string
}

// boundedGlobalPresentationReceipts accepts both the current ordered shape and
// the legacy map-only shape. Legacy entries are ranked by committed revision,
// then by the base-36 timestamp embedded in Workass operation ids, so the first
// bounded rewrite retains the newest retry window rather than an arbitrary map
// iteration suffix.
func boundedGlobalPresentationReceipts(rawReceipts, rawOrder any) (map[string]any, []string, bool) {
	source := mapFromAnyMain(rawReceipts)
	candidates := make(map[string]globalPresentationReceiptCandidate, min(len(source), globalPresentationReceiptLimit))
	for rawID, rawReceipt := range source {
		operationID, err := providercontract.ValidateOperationID(rawID)
		receipt := mapFromAnyMain(rawReceipt)
		digest := fieldString(receipt, "digest")
		if err != nil || len(digest) != sha256.Size*2 || !isLowerHex(digest) || receipt["revision"] == nil || intValue(receipt["revision"]) < 0 {
			continue
		}
		id := string(operationID)
		candidates[id] = globalPresentationReceiptCandidate{
			id: id, receipt: receipt, revision: intValue(receipt["revision"]), timeKey: globalPresentationOperationTimeKey(id),
		}
	}

	rawOrderValues := stringValues(rawOrder)
	explicitOrder := make([]string, 0, len(rawOrderValues))
	explicitSet := make(map[string]struct{}, len(explicitOrder))
	for _, rawID := range rawOrderValues {
		id := rawID
		id = strings.TrimSpace(id)
		if _, exists := candidates[id]; !exists {
			continue
		}
		if _, duplicate := explicitSet[id]; duplicate {
			continue
		}
		explicitSet[id] = struct{}{}
		explicitOrder = append(explicitOrder, id)
	}

	legacy := make([]globalPresentationReceiptCandidate, 0, len(candidates)-len(explicitOrder))
	for id, candidate := range candidates {
		if _, ordered := explicitSet[id]; !ordered {
			legacy = append(legacy, candidate)
		}
	}
	sort.Slice(legacy, func(i, j int) bool {
		if legacy[i].revision != legacy[j].revision {
			return legacy[i].revision < legacy[j].revision
		}
		if legacy[i].timeKey != legacy[j].timeKey {
			return legacy[i].timeKey < legacy[j].timeKey
		}
		return legacy[i].id < legacy[j].id
	})
	order := make([]string, 0, len(candidates))
	for _, candidate := range legacy {
		order = append(order, candidate.id)
	}
	order = append(order, explicitOrder...)
	if len(order) > globalPresentationReceiptLimit {
		order = order[len(order)-globalPresentationReceiptLimit:]
	}
	out := make(map[string]any, len(order))
	for _, id := range order {
		out[id] = cloneJSON(candidates[id].receipt)
	}
	compacted := len(out) != len(source) || !sameStringOrder(rawOrder, order)
	return out, order, compacted
}

func globalPresentationOperationTimeKey(operationID string) string {
	parts := strings.FieldsFunc(operationID, func(char rune) bool { return char == '-' || char == ':' })
	for _, part := range parts {
		if len(part) < 8 || part[0] != 'm' {
			continue
		}
		valid := true
		for _, char := range part {
			if (char < '0' || char > '9') && (char < 'a' || char > 'z') {
				valid = false
				break
			}
		}
		if valid {
			return part
		}
	}
	return ""
}

func isLowerHex(value string) bool {
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func stringsToAny(values []string) []any {
	out := make([]any, len(values))
	for index, value := range values {
		out[index] = value
	}
	return out
}

func stringValues(raw any) []string {
	switch values := raw.(type) {
	case []string:
		return append([]string(nil), values...)
	case []any:
		out := make([]string, 0, len(values))
		for _, value := range values {
			if text, ok := value.(string); ok {
				out = append(out, text)
			}
		}
		return out
	default:
		return nil
	}
}

func sameStringOrder(raw any, expected []string) bool {
	values := stringValues(raw)
	if len(values) != len(expected) {
		return false
	}
	for index, value := range values {
		if value != expected[index] {
			return false
		}
	}
	return true
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

// SaveGlobalActiveTab is the single writer for the daemon-wide focus field. It
// uses the same stable operation/CAS receipt as every other
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
	result, err := s.SaveActorGlobalSnapshot(snapshot)
	return result.Revision, err
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
	for _, attachment := range p.Attachments {
		ref := strings.TrimPrefix(strings.TrimSpace(attachment.Ref), providerSessionImageRefPrefix)
		if _, _, _, err := externalSessionImageInfo(ref, p.stateDir); err != nil {
			return err
		}
	}
	return nil
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
