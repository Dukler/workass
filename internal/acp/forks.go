package acp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Forkable subsessions. A fork point freezes a parent chat's native provider
// conversation at one moment; children start from that exact history, so the
// provider's prompt cache serves the shared prefix, then each child receives
// its own message, runs once, returns its final text and ends. Children never
// see one another. Only providers with a real native fork can do this; the
// catalog says so per provider and Workass never replays a transcript instead.
//
// The fork point is SEALED with one short resumed turn: the provider
// re-serializes a resumed conversation slightly differently from the live one
// it was recorded from, so children forked straight from the parent each paid
// to re-cache part of the prefix. The seal turn pays that once and writes a
// cache entry at exactly the shared boundary every child then reads.

const (
	ForkStatusSealing   = "sealing"
	ForkStatusReady     = "ready"
	ForkStatusQueued    = "queued"
	ForkStatusRunning   = "running"
	ForkStatusCompleted = "completed"
	ForkStatusFailed    = "failed"
	ForkStatusCancelled = "cancelled"

	ForkRetentionThrowaway = "throwaway"
	ForkRetentionKeep      = "keep"

	DefaultForkConcurrency = 6
	MaxForkConcurrency     = 12
	maxForkGlobalRunning   = 12
	MaxForkChildrenPerRun  = 64
	// ForkCacheLookbackImages is the most images one child can carry while
	// the provider still finds the fork point within its cache lookback.
	ForkCacheLookbackImages = 16
	// A kept child persists its native session, which adds a note block per
	// image, halving the images that fit in the lookback.
	ForkCacheLookbackImagesKept = 8
	DefaultForkRetentionDays    = 7
	maxForkRetentionDays        = 90
	DefaultForkChildTimeout     = 15 * time.Minute
	maxForkChildTimeout         = 2 * time.Hour
	maxForkPointsPerChat        = 64
	maxForkRunsPerChat          = 32
	maxForkResultChars          = 12000
	maxForkErrorChars           = 2000
	maxForkMessageChars         = 200000
	forkStoreVersion            = 1
)

// ForkUsage is the provider-reported token usage summed over every model call
// of one turn (a child that calls a tool makes more than one).
type ForkUsage struct {
	InputTokens         int64 `json:"inputTokens"`
	OutputTokens        int64 `json:"outputTokens"`
	CacheReadTokens     int64 `json:"cacheReadTokens"`
	CacheCreationTokens int64 `json:"cacheCreationTokens"`
}

// CacheHitShare is cache-read tokens over all input-side tokens.
func (u ForkUsage) CacheHitShare() float64 {
	total := u.InputTokens + u.CacheReadTokens + u.CacheCreationTokens
	if total <= 0 {
		return 0
	}
	return float64(u.CacheReadTokens) / float64(total)
}

type ForkPoint struct {
	ID              string      `json:"id"`
	Name            string      `json:"name"`
	ChatID          string      `json:"chatId"`
	TabID           string      `json:"tabId"`
	ProviderID      string      `json:"providerId"`
	ModelID         string      `json:"modelId,omitempty"`
	Effort          string      `json:"effort,omitempty"`
	ModeID          string      `json:"modeId,omitempty"`
	AddDirs         []string    `json:"addDirs,omitempty"`
	CWD             string      `json:"cwd"`
	ParentSessionID string      `json:"parentSessionId"`
	SealSessionID   string      `json:"sealSessionId,omitempty"`
	AfterMessageID  string      `json:"afterMessageId,omitempty"`
	Status          string      `json:"status"`
	Error           string      `json:"error,omitempty"`
	SealUsage       ForkUsage   `json:"sealUsage"`
	SealHops        []ForkUsage `json:"sealHops,omitempty"`
	SealWallMS      int64       `json:"sealWallMs,omitempty"`
	PrefixTokens    int64       `json:"prefixTokens,omitempty"`
	CreatedAt       string      `json:"createdAt"`
	ReadyAt         string      `json:"readyAt,omitempty"`
	OperationID     string      `json:"operationId,omitempty"`
}

type ForkChild struct {
	ID          string            `json:"id"`
	Index       int               `json:"index"`
	Label       string            `json:"label,omitempty"`
	Message     string            `json:"message"`
	Attachments []ImageAttachment `json:"attachments,omitempty"`
	Status      string            `json:"status"`
	SessionID   string            `json:"sessionId,omitempty"`
	Result      string            `json:"result,omitempty"`
	Error       string            `json:"error,omitempty"`
	StopReason  string            `json:"stopReason,omitempty"`
	Usage       ForkUsage         `json:"usage"`
	Turns       int               `json:"turns,omitempty"`
	WallMS      int64             `json:"wallMs,omitempty"`
	Activity    string            `json:"activity,omitempty"`
	StartedAt   string            `json:"startedAt,omitempty"`
	FinishedAt  string            `json:"finishedAt,omitempty"`
	RetryOf     string            `json:"retryOf,omitempty"`
	Attempt     int               `json:"attempt,omitempty"`
	Transcript  bool              `json:"transcript,omitempty"`
	NativeKept  bool              `json:"nativeKept,omitempty"`
}

type ForkRun struct {
	ID             string      `json:"id"`
	ForkPointID    string      `json:"forkPointId"`
	ChatID         string      `json:"chatId"`
	TabID          string      `json:"tabId"`
	Concurrency    int         `json:"concurrency"`
	Retention      string      `json:"retention"`
	RetentionDays  int         `json:"retentionDays"`
	ChildTimeoutMS int64       `json:"childTimeoutMs"`
	LabelImages    bool        `json:"labelImages"`
	MaxImageEdge   int         `json:"maxImageEdge"`
	Children       []ForkChild `json:"children"`
	CreatedAt      string      `json:"createdAt"`
	OperationID    string      `json:"operationId,omitempty"`
}

// Status is derived: running while any child is queued or running.
func (r ForkRun) Status() string {
	cancelled := 0
	for _, child := range r.Children {
		switch child.Status {
		case ForkStatusQueued, ForkStatusRunning:
			return ForkStatusRunning
		case ForkStatusCancelled:
			cancelled++
		}
	}
	if cancelled == len(r.Children) && cancelled > 0 {
		return ForkStatusCancelled
	}
	return ForkStatusCompleted
}

type forkOperation struct {
	Kind   string `json:"kind"`
	Digest string `json:"digest"`
	Ref    string `json:"ref"`
}

type forkChatFile struct {
	Version    int                      `json:"v"`
	ChatID     string                   `json:"chatId"`
	TabID      string                   `json:"tabId"`
	Points     []ForkPoint              `json:"points"`
	Runs       []ForkRun                `json:"runs"`
	Operations map[string]forkOperation `json:"operations"`
}

// ForkLaunch is everything needed to start the provider CLI exactly as the
// chat's own provider process would find it.
type ForkLaunch struct {
	ProviderID  string
	Command     string
	Env         []string
	NativeStore string
}

// ForkParent is the parent chat's state at the moment a fork point is marked.
type ForkParent struct {
	ChatID          string
	TabID           string
	ProviderID      string
	ModelID         string
	Effort          string
	ModeID          string
	CWD             string
	NativeSessionID string
	AfterMessageID  string
}

type ForkChildSpec struct {
	Label       string
	Message     string
	Attachments []string
}

type ForkRunOptions struct {
	ForkPointID    string
	Children       []ForkChildSpec
	Concurrency    int
	Retention      string
	RetentionDays  int
	ChildTimeout   time.Duration
	LabelImages    *bool
	MaxImageEdge   int
	OperationID    string
	RequesterChat  string
	RequesterTabID string
}

// forkTurnRunner executes one provider turn. Tests replace it; production
// dispatches to the provider's registered fork strategy.
type forkTurnRunner func(ctx context.Context, spec forkTurnSpec) (forkTurnResult, error)

type ForkService struct {
	stateDir string
	launch   func(providerID string) (ForkLaunch, error)
	notify   func(tabID, chatID string)
	runTurn  forkTurnRunner
	now      func() time.Time

	mu      sync.Mutex
	chats   map[string]*forkChatFile
	cancels map[string]context.CancelFunc
	changed chan struct{}
	global  chan struct{}
	seq     uint64
}

func NewForkService(stateDir string, launch func(providerID string) (ForkLaunch, error), notify func(tabID, chatID string)) *ForkService {
	if notify == nil {
		notify = func(string, string) {}
	}
	return &ForkService{
		stateDir: strings.TrimSpace(stateDir), launch: launch, notify: notify, runTurn: runProviderForkTurn,
		now: time.Now, chats: make(map[string]*forkChatFile), cancels: make(map[string]context.CancelFunc),
		changed: make(chan struct{}), global: make(chan struct{}, maxForkGlobalRunning),
	}
}

func (s *ForkService) signalLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}

func (s *ForkService) storePath(chatID string) string {
	if s.stateDir == "" {
		return ""
	}
	return filepath.Join(s.stateDir, "forks", safeArchiveName(strings.TrimSpace(chatID))+".json")
}

func (s *ForkService) transcriptPath(chatID, runID, childID string) string {
	if s.stateDir == "" {
		return ""
	}
	return filepath.Join(s.stateDir, "forks", "transcripts", safeArchiveName(chatID), safeArchiveName(runID), safeArchiveName(childID)+".jsonl")
}

// chatLocked loads one chat's fork state. Work that was live when an earlier
// daemon stopped cannot be resumed: it settles as failed with that reason.
func (s *ForkService) chatLocked(chatID string) (*forkChatFile, error) {
	chatID = strings.TrimSpace(chatID)
	if chatID == "" {
		return nil, errors.New("exact chat id is required")
	}
	if file := s.chats[chatID]; file != nil {
		return file, nil
	}
	file := &forkChatFile{Version: forkStoreVersion, ChatID: chatID, Operations: map[string]forkOperation{}}
	if path := s.storePath(chatID); path != "" {
		data, err := os.ReadFile(path)
		switch {
		case err == nil:
			if err := json.Unmarshal(data, file); err != nil {
				return nil, fmt.Errorf("fork state for chat is unreadable: %w", err)
			}
		case !os.IsNotExist(err):
			return nil, err
		}
	}
	if file.Operations == nil {
		file.Operations = map[string]forkOperation{}
	}
	stamp := s.now().UTC().Format(time.RFC3339Nano)
	recovered := false
	for i := range file.Points {
		if file.Points[i].Status == ForkStatusSealing {
			file.Points[i].Status, file.Points[i].Error = ForkStatusFailed, "Workass restarted before the fork point was sealed"
			recovered = true
		}
	}
	for r := range file.Runs {
		for c := range file.Runs[r].Children {
			child := &file.Runs[r].Children[c]
			if child.Status == ForkStatusQueued || child.Status == ForkStatusRunning {
				child.Status, child.Error, child.FinishedAt, child.Activity = ForkStatusFailed, "Workass restarted before this child finished", stamp, ""
				recovered = true
			}
		}
	}
	s.chats[chatID] = file
	s.pruneLocked(file)
	if recovered {
		_ = s.persistLocked(file)
	}
	return file, nil
}

func (s *ForkService) persistLocked(file *forkChatFile) error {
	path := s.storePath(file.ChatID)
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(file, "", " ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func (s *ForkService) commitLocked(file *forkChatFile) {
	_ = s.persistLocked(file)
	s.signalLocked()
	tabID, chatID := file.TabID, file.ChatID
	go s.notify(tabID, chatID)
}

// pruneLocked bounds the per-chat record and expires child transcripts whose
// retention window has passed. Receipts outlive transcripts until the run
// itself ages out of the bounded list.
func (s *ForkService) pruneLocked(file *forkChatFile) {
	if len(file.Points) > maxForkPointsPerChat {
		file.Points = file.Points[len(file.Points)-maxForkPointsPerChat:]
	}
	settledRuns := 0
	for _, run := range file.Runs {
		if run.Status() != ForkStatusRunning {
			settledRuns++
		}
	}
	for len(file.Runs) > maxForkRunsPerChat && settledRuns > 0 {
		dropped := -1
		for i, run := range file.Runs {
			if run.Status() != ForkStatusRunning {
				dropped = i
				break
			}
		}
		if dropped < 0 {
			break
		}
		s.removeRunTranscriptsLocked(file.ChatID, file.Runs[dropped])
		file.Runs = append(file.Runs[:dropped], file.Runs[dropped+1:]...)
		settledRuns--
	}
	now := s.now()
	for r := range file.Runs {
		run := &file.Runs[r]
		if run.Retention == ForkRetentionKeep {
			continue
		}
		days := run.RetentionDays
		if days <= 0 {
			days = DefaultForkRetentionDays
		}
		for c := range run.Children {
			child := &run.Children[c]
			if !child.Transcript || child.FinishedAt == "" {
				continue
			}
			finished, err := time.Parse(time.RFC3339Nano, child.FinishedAt)
			if err != nil || now.Sub(finished) < time.Duration(days)*24*time.Hour {
				continue
			}
			if path := s.transcriptPath(file.ChatID, run.ID, child.ID); path != "" {
				_ = os.Remove(path)
			}
			child.Transcript = false
		}
	}
}

func (s *ForkService) removeRunTranscriptsLocked(chatID string, run ForkRun) {
	if s.stateDir == "" {
		return
	}
	_ = os.RemoveAll(filepath.Join(s.stateDir, "forks", "transcripts", safeArchiveName(chatID), safeArchiveName(run.ID)))
}

func (s *ForkService) nextIDLocked(prefix string) string {
	s.seq++
	return fmt.Sprintf("%s-%d-%d", prefix, s.now().UnixMilli(), s.seq)
}

func forkDigest(parts ...any) string {
	data, _ := json.Marshal(parts)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// operationLocked implements the durable operation receipt: the same id with
// the same input returns the original ref; a different input is rejected.
func operationLocked(file *forkChatFile, operationID, kind, digest string) (string, bool, error) {
	operationID = strings.TrimSpace(operationID)
	if operationID == "" {
		return "", false, fmt.Errorf("%s requires a stable operation_id", kind)
	}
	existing, ok := file.Operations[operationID]
	if !ok {
		return "", false, nil
	}
	if existing.Kind != kind || existing.Digest != digest {
		return "", true, fmt.Errorf("operation_id %q was already used for a different fork request", operationID)
	}
	return existing.Ref, true, nil
}

// ForkPointOptions fixes everything that shapes the provider request for the
// seal AND every child. A child launched with different flags (permission
// mode, extra directories) sends a different request prefix and misses the
// cache, so these belong to the fork point, never to a run.
type ForkPointOptions struct {
	Name        string
	OperationID string
	ModeID      string
	Effort      string
	AddDirs     []string
}

// CreateForkPoint marks and seals a fork point. It blocks for the sealing
// turns (a few seconds) so the caller gets a ready fork point or a definite
// failure. Repeating the same operation returns the same fork point.
func (s *ForkService) CreateForkPoint(ctx context.Context, parent ForkParent, opts ForkPointOptions) (ForkPoint, error) {
	name := SanitizeDisplayText(strings.TrimSpace(opts.Name), 80)
	operationID := strings.TrimSpace(opts.OperationID)
	if name == "" {
		return ForkPoint{}, errors.New("fork point name is required")
	}
	if !forkSupported(parent.ProviderID) {
		return ForkPoint{}, fmt.Errorf("provider %q cannot fork: %v", parent.ProviderID, ForkCapability(parent.ProviderID)["reason"])
	}
	if strings.TrimSpace(parent.NativeSessionID) == "" {
		return ForkPoint{}, errors.New("the chat has no native provider conversation yet; send it a message before marking a fork point")
	}
	if strings.TrimSpace(parent.CWD) == "" {
		return ForkPoint{}, errors.New("the chat has no working directory")
	}
	addDirs := make([]string, 0, len(opts.AddDirs))
	for _, dir := range opts.AddDirs {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			continue
		}
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(parent.CWD, dir)
		}
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			return ForkPoint{}, fmt.Errorf("add_dirs entry %s is not an existing directory", displayAttachmentPath(dir))
		}
		addDirs = append(addDirs, filepath.Clean(dir))
	}
	mode := firstNonEmpty(strings.TrimSpace(opts.ModeID), parent.ModeID)
	effort := parent.Effort
	if requested := strings.TrimSpace(opts.Effort); requested != "" {
		key, ok := canonicalEffortKey(requested)
		if !ok {
			return ForkPoint{}, fmt.Errorf("effort %q is not a known effort level", requested)
		}
		effort = key
	}
	digest := forkDigest("fork-point", parent.ChatID, parent.TabID, name, mode, effort, addDirs)
	s.mu.Lock()
	file, err := s.chatLocked(parent.ChatID)
	if err != nil {
		s.mu.Unlock()
		return ForkPoint{}, err
	}
	if ref, replay, err := operationLocked(file, operationID, "fork_point", digest); err != nil || replay {
		defer s.mu.Unlock()
		if err != nil {
			return ForkPoint{}, err
		}
		for _, point := range file.Points {
			if point.ID == ref {
				return point, nil
			}
		}
		return ForkPoint{}, errors.New("fork point receipt is missing its fork point")
	}
	point := ForkPoint{
		ID: s.nextIDLocked("fp"), Name: name, ChatID: parent.ChatID, TabID: parent.TabID,
		ProviderID: normalizeProviderID(parent.ProviderID), ModelID: parent.ModelID, Effort: effort, ModeID: mode,
		AddDirs: addDirs, CWD: parent.CWD, ParentSessionID: parent.NativeSessionID, AfterMessageID: parent.AfterMessageID,
		Status: ForkStatusSealing, CreatedAt: s.now().UTC().Format(time.RFC3339Nano), OperationID: operationID,
	}
	file.TabID = parent.TabID
	file.Points = append(file.Points, point)
	file.Operations[point.OperationID] = forkOperation{Kind: "fork_point", Digest: digest, Ref: point.ID}
	s.commitLocked(file)
	s.mu.Unlock()

	sealed, sealErr := s.seal(ctx, point)
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range file.Points {
		if file.Points[i].ID != point.ID {
			continue
		}
		if sealErr != nil {
			file.Points[i].Status, file.Points[i].Error = ForkStatusFailed, compactText(RedactSensitiveText(sealErr.Error()), maxForkErrorChars)
			file.Points[i].SealHops = sealed.SealHops
		} else {
			file.Points[i] = sealed
		}
		point = file.Points[i]
	}
	s.commitLocked(file)
	if sealErr != nil {
		return point, sealErr
	}
	return point, nil
}

func forkSealMessage(name string, hop int) string {
	if hop > 1 {
		return fmt.Sprintf("Fork point «%s» confirmed. Reply with exactly: OK", name)
	}
	return fmt.Sprintf("Workass fork point «%s». The conversation above is the shared starting context for independent tasks; each task arrives in its own forked conversation. Reply with exactly: OK", name)
}

// A seal hop whose own cache write is at most this many tokens has proved
// that a resumed request reproduces the whole prefix byte for byte.
const (
	forkSealStableCreation = 2048
	maxForkSealHops        = 3
)

// seal runs short forked turns until the last one was itself a resumed
// request that re-cached almost nothing. The first hop is a resumed copy of
// the LIVE parent: its request carries whatever the live process rendered and
// any tool-environment delta between the parent's launch and the fork launch,
// and a resumed child re-renders that slightly differently. The second hop is
// resumed from the first, so its prefix is rendered exactly as every child
// will render it, and its cache write sits at the boundary children share.
// Intermediate seal sessions are discarded; only the final one is kept.
func (s *ForkService) seal(ctx context.Context, point ForkPoint) (ForkPoint, error) {
	launch, err := s.launch(point.ProviderID)
	if err != nil {
		return point, err
	}
	started := s.now()
	resume := point.ParentSessionID
	var total ForkUsage
	var last forkTurnResult
	for hop := 1; hop <= maxForkSealHops; hop++ {
		result, err := s.runTurn(ctx, forkTurnSpec{
			Launch: launch, CWD: point.CWD, ResumeSessionID: resume,
			ModelID: point.ModelID, Effort: point.Effort, ModeID: point.ModeID, AddDirs: point.AddDirs, Persist: true,
			Content: []any{map[string]any{"type": "text", "text": forkSealMessage(point.Name, hop)}},
			Timeout: 5 * time.Minute,
		})
		if err == nil && (result.IsError || strings.TrimSpace(result.SessionID) == "") {
			err = errors.New(firstNonEmpty(result.Result, result.Subtype, "no session"))
		}
		if err != nil {
			if hop > 1 {
				discardForkSession(launch, resume)
			}
			return point, fmt.Errorf("sealing turn %d failed: %w", hop, err)
		}
		point.SealHops = append(point.SealHops, result.Usage)
		total.InputTokens += result.Usage.InputTokens
		total.OutputTokens += result.Usage.OutputTokens
		total.CacheReadTokens += result.Usage.CacheReadTokens
		total.CacheCreationTokens += result.Usage.CacheCreationTokens
		if hop > 1 {
			discardForkSession(launch, resume)
		}
		resume, last = result.SessionID, result
		if hop > 1 && result.Usage.CacheCreationTokens <= forkSealStableCreation {
			break
		}
	}
	point.SealSessionID = last.SessionID
	point.SealUsage = total
	point.SealWallMS = s.now().Sub(started).Milliseconds()
	point.PrefixTokens = last.LastCallContext
	point.Status = ForkStatusReady
	point.ReadyAt = s.now().UTC().Format(time.RFC3339Nano)
	return point, nil
}

func (s *ForkService) findPointLocked(forkPointID string) (*forkChatFile, *ForkPoint) {
	for _, file := range s.chats {
		for i := range file.Points {
			if file.Points[i].ID == forkPointID {
				return file, &file.Points[i]
			}
		}
	}
	return nil, nil
}

func (s *ForkService) findRunLocked(runID string) (*forkChatFile, *ForkRun) {
	for _, file := range s.chats {
		for i := range file.Runs {
			if file.Runs[i].ID == runID {
				return file, &file.Runs[i]
			}
		}
	}
	return nil, nil
}

// loadAllLocked brings every persisted chat into memory so an id lookup can
// find fork points and runs created before this daemon started.
func (s *ForkService) loadAllLocked() {
	if s.stateDir == "" {
		return
	}
	entries, err := os.ReadDir(filepath.Join(s.stateDir, "forks"))
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.stateDir, "forks", entry.Name()))
		if err != nil {
			continue
		}
		var header struct {
			ChatID string `json:"chatId"`
		}
		if json.Unmarshal(data, &header) == nil && header.ChatID != "" {
			_, _ = s.chatLocked(header.ChatID)
		}
	}
}

func (s *ForkService) lookupPointLocked(forkPointID string) (*forkChatFile, *ForkPoint) {
	if file, point := s.findPointLocked(forkPointID); point != nil {
		return file, point
	}
	s.loadAllLocked()
	return s.findPointLocked(forkPointID)
}

func (s *ForkService) lookupRunLocked(runID string) (*forkChatFile, *ForkRun) {
	if file, run := s.findRunLocked(runID); run != nil {
		return file, run
	}
	s.loadAllLocked()
	return s.findRunLocked(runID)
}

// StartRun validates every child (including every attachment) before any
// child starts, records the run durably, and returns immediately.
func (s *ForkService) StartRun(opts ForkRunOptions) (ForkRun, error) {
	if len(opts.Children) == 0 {
		return ForkRun{}, errors.New("children is required")
	}
	if len(opts.Children) > MaxForkChildrenPerRun {
		return ForkRun{}, fmt.Errorf("too many children: got %d, maximum is %d per run", len(opts.Children), MaxForkChildrenPerRun)
	}
	concurrency := opts.Concurrency
	if concurrency == 0 {
		concurrency = DefaultForkConcurrency
	}
	if concurrency < 1 || concurrency > MaxForkConcurrency {
		return ForkRun{}, fmt.Errorf("concurrency must be between 1 and %d", MaxForkConcurrency)
	}
	retention := strings.TrimSpace(opts.Retention)
	if retention == "" {
		retention = ForkRetentionThrowaway
	}
	if retention != ForkRetentionThrowaway && retention != ForkRetentionKeep {
		return ForkRun{}, errors.New("retention must be throwaway or keep")
	}
	days := opts.RetentionDays
	if days == 0 {
		days = DefaultForkRetentionDays
	}
	if days < 1 || days > maxForkRetentionDays {
		return ForkRun{}, fmt.Errorf("retention_days must be between 1 and %d", maxForkRetentionDays)
	}
	timeout := opts.ChildTimeout
	if timeout == 0 {
		timeout = DefaultForkChildTimeout
	}
	if timeout < 10*time.Second || timeout > maxForkChildTimeout {
		return ForkRun{}, fmt.Errorf("child_timeout_ms must be between 10000 and %d", maxForkChildTimeout.Milliseconds())
	}
	labelImages := true
	if opts.LabelImages != nil {
		labelImages = *opts.LabelImages
	}
	maxEdge := opts.MaxImageEdge
	if maxEdge == 0 {
		maxEdge = DefaultMaxImageEdge
	}

	s.mu.Lock()
	file, point := s.lookupPointLocked(strings.TrimSpace(opts.ForkPointID))
	if point == nil {
		s.mu.Unlock()
		return ForkRun{}, fmt.Errorf("unknown fork point %q", opts.ForkPointID)
	}
	if point.Status != ForkStatusReady {
		s.mu.Unlock()
		return ForkRun{}, fmt.Errorf("fork point %q is %s, not ready", point.ID, point.Status)
	}
	pointCopy := *point
	digest := forkDigest("fork-run", opts.ForkPointID, opts.Children, concurrency, retention, days, timeout.Milliseconds(), labelImages, maxEdge)
	if ref, replay, err := operationLocked(file, opts.OperationID, "fork_run", digest); err != nil || replay {
		defer s.mu.Unlock()
		if err != nil {
			return ForkRun{}, err
		}
		for _, run := range file.Runs {
			if run.ID == ref {
				return run, nil
			}
		}
		return ForkRun{}, errors.New("fork run receipt is missing its run")
	}
	s.mu.Unlock()

	children := make([]ForkChild, 0, len(opts.Children))
	for index, spec := range opts.Children {
		message := RedactSensitiveText(strings.TrimSpace(spec.Message))
		if message == "" {
			return ForkRun{}, fmt.Errorf("child %d: message is required", index+1)
		}
		if len(message) > maxForkMessageChars {
			return ForkRun{}, fmt.Errorf("child %d: message is longer than %d characters", index+1, maxForkMessageChars)
		}
		attachments, err := ValidateImageAttachmentPaths(spec.Attachments, pointCopy.CWD, maxEdge)
		if err != nil {
			return ForkRun{}, fmt.Errorf("child %d: %w", index+1, err)
		}
		children = append(children, ForkChild{
			Index: index + 1, Label: SanitizeDisplayText(strings.TrimSpace(spec.Label), 80), Message: message,
			Attachments: attachments, Status: ForkStatusQueued, Attempt: 1,
		})
	}

	s.mu.Lock()
	// Re-check the receipt: a concurrent identical call may have won while
	// attachments were being validated outside the lock.
	if ref, replay, err := operationLocked(file, opts.OperationID, "fork_run", digest); err != nil || replay {
		defer s.mu.Unlock()
		if err != nil {
			return ForkRun{}, err
		}
		for _, run := range file.Runs {
			if run.ID == ref {
				return run, nil
			}
		}
		return ForkRun{}, errors.New("fork run receipt is missing its run")
	}
	run := ForkRun{
		ID: s.nextIDLocked("fr"), ForkPointID: pointCopy.ID, ChatID: pointCopy.ChatID, TabID: pointCopy.TabID,
		Concurrency: concurrency, Retention: retention, RetentionDays: days, ChildTimeoutMS: timeout.Milliseconds(), LabelImages: labelImages, MaxImageEdge: maxEdge,
		CreatedAt: s.now().UTC().Format(time.RFC3339Nano), OperationID: strings.TrimSpace(opts.OperationID),
	}
	for i := range children {
		children[i].ID = fmt.Sprintf("%s-c%d", run.ID, children[i].Index)
	}
	run.Children = children
	file.Runs = append(file.Runs, run)
	file.Operations[run.OperationID] = forkOperation{Kind: "fork_run", Digest: digest, Ref: run.ID}
	s.pruneLocked(file)
	s.commitLocked(file)
	s.mu.Unlock()

	go s.dispatch(run.ID)
	return run, nil
}

// dispatch starts queued children in index order under the run's own cap and
// the daemon-wide cap. Retries append queued children and call it again.
func (s *ForkService) dispatch(runID string) {
	for {
		s.mu.Lock()
		_, run := s.findRunLocked(runID)
		if run == nil {
			s.mu.Unlock()
			return
		}
		running := 0
		next := -1
		for i, child := range run.Children {
			switch child.Status {
			case ForkStatusRunning:
				running++
			case ForkStatusQueued:
				if next < 0 {
					next = i
				}
			}
		}
		if next < 0 || running >= run.Concurrency {
			s.mu.Unlock()
			return
		}
		select {
		case s.global <- struct{}{}:
		default:
			s.mu.Unlock()
			// The daemon-wide cap is full; a finishing child of any run
			// re-dispatches every run with queued work.
			return
		}
		child := &run.Children[next]
		ctx, cancel := context.WithCancel(context.Background())
		s.cancels[child.ID] = cancel
		child.Status = ForkStatusRunning
		child.StartedAt = s.now().UTC().Format(time.RFC3339Nano)
		child.Activity = "Iniciando"
		file, _ := s.findRunLocked(runID)
		s.commitLocked(file)
		childID := child.ID
		s.mu.Unlock()
		go s.runChild(ctx, runID, childID)
	}
}

func (s *ForkService) redispatchAll() {
	s.mu.Lock()
	ids := make([]string, 0)
	for _, file := range s.chats {
		for _, run := range file.Runs {
			for _, child := range run.Children {
				if child.Status == ForkStatusQueued {
					ids = append(ids, run.ID)
					break
				}
			}
		}
	}
	s.mu.Unlock()
	sort.Strings(ids)
	for _, id := range ids {
		s.dispatch(id)
	}
}

func (s *ForkService) childLocked(runID, childID string) (*forkChatFile, *ForkRun, *ForkChild) {
	file, run := s.findRunLocked(runID)
	if run == nil {
		return nil, nil, nil
	}
	for i := range run.Children {
		if run.Children[i].ID == childID {
			return file, run, &run.Children[i]
		}
	}
	return file, run, nil
}

func (s *ForkService) runChild(ctx context.Context, runID, childID string) {
	defer func() {
		<-s.global
		s.redispatchAll()
	}()
	s.mu.Lock()
	file, run, child := s.childLocked(runID, childID)
	if child == nil {
		s.mu.Unlock()
		return
	}
	_, point := s.findPointLocked(run.ForkPointID)
	if point == nil {
		child.Status, child.Error, child.FinishedAt = ForkStatusFailed, "fork point disappeared", s.now().UTC().Format(time.RFC3339Nano)
		s.commitLocked(file)
		s.mu.Unlock()
		return
	}
	pointCopy, runCopy, childCopy := *point, *run, *child
	transcript := s.transcriptPath(file.ChatID, runID, childID)
	s.mu.Unlock()

	started := s.now()
	result, runErr := s.executeChild(ctx, pointCopy, runCopy, childCopy, transcript, func(activity string) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if file, _, live := s.childLocked(runID, childID); live != nil && live.Status == ForkStatusRunning && live.Activity != activity {
			live.Activity = activity
			s.signalLocked()
			go s.notify(file.TabID, file.ChatID)
		}
	}, func(sessionID string) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if file, _, live := s.childLocked(runID, childID); live != nil && live.SessionID == "" {
			live.SessionID = sessionID
			_ = s.persistLocked(file)
		}
	})
	wall := s.now().Sub(started).Milliseconds()

	launch, launchErr := s.launch(pointCopy.ProviderID)
	nativeKept := runCopy.Retention == ForkRetentionKeep
	sessionID := firstNonEmpty(result.SessionID, childCopy.SessionID)
	if !nativeKept && sessionID != "" && launchErr == nil {
		discardForkSession(launch, sessionID)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.cancels, childID)
	file, _, child = s.childLocked(runID, childID)
	if child == nil {
		return
	}
	child.WallMS = wall
	child.FinishedAt = s.now().UTC().Format(time.RFC3339Nano)
	child.Activity = ""
	child.SessionID = sessionID
	child.NativeKept = nativeKept && sessionID != ""
	child.Usage = result.Usage
	child.Turns = result.Turns
	child.StopReason = result.Subtype
	child.Transcript = transcript != "" && fileExists(transcript)
	child.Result = compactText(RedactSensitiveText(result.Result), maxForkResultChars)
	switch {
	case ctx.Err() != nil:
		child.Status = ForkStatusCancelled
		child.Error = "cancelled"
	case runErr != nil:
		child.Status = ForkStatusFailed
		child.Error = compactText(RedactSensitiveText(runErr.Error()), maxForkErrorChars)
	case result.IsError:
		child.Status = ForkStatusFailed
		child.Error = compactText(RedactSensitiveText(firstNonEmpty(result.Result, result.Subtype, "the provider reported an error")), maxForkErrorChars)
	default:
		child.Status = ForkStatusCompleted
	}
	s.commitLocked(file)
}

func (s *ForkService) executeChild(ctx context.Context, point ForkPoint, run ForkRun, child ForkChild, transcript string, onActivity func(string), onSession func(string)) (forkTurnResult, error) {
	launch, err := s.launch(point.ProviderID)
	if err != nil {
		return forkTurnResult{}, err
	}
	// Image names ride in the message text, never as one text block per image:
	// the provider looks back only ~20 content blocks from its breakpoint for a
	// cached prefix, and label blocks pushed the seal out of reach (measured
	// 2026-10-15: 12 images + 12 label blocks read 14,641 cached tokens; the
	// same images with labels in the text read the whole 44,786 prefix).
	text := child.Message
	if run.LabelImages && len(child.Attachments) > 0 {
		var labels strings.Builder
		labels.WriteString("\n\nAttached images, in order:")
		for index, attachment := range child.Attachments {
			fmt.Fprintf(&labels, "\n%d. %s (%dx%d)", index+1, attachment.Name, attachment.Width, attachment.Height)
		}
		text += labels.String()
	}
	content := []any{map[string]any{"type": "text", "text": text}}
	for _, attachment := range child.Attachments {
		data, err := LoadImageAttachment(attachment)
		if err != nil {
			return forkTurnResult{}, err
		}
		content = append(content, map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": attachment.MIME, "data": data}})
	}
	return s.runTurn(ctx, forkTurnSpec{
		Launch: launch, CWD: point.CWD, ResumeSessionID: point.SealSessionID,
		ModelID: point.ModelID, Effort: point.Effort, ModeID: point.ModeID, AddDirs: point.AddDirs,
		Persist: run.Retention == ForkRetentionKeep, Content: content, Timeout: time.Duration(run.ChildTimeoutMS) * time.Millisecond,
		TranscriptPath: transcript, UserText: child.Message, UserAttachments: child.Attachments,
		OnActivity: onActivity, OnSession: onSession,
	})
}

// CancelChild cancels one queued or running child. It is idempotent per
// operation id and harmless on a settled child.
func (s *ForkService) CancelChild(runID, childID, operationID string) (ForkChild, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	file, run := s.lookupRunLocked(strings.TrimSpace(runID))
	if run == nil {
		return ForkChild{}, fmt.Errorf("unknown fork run %q", runID)
	}
	digest := forkDigest("fork-cancel", runID, childID)
	if _, replay, err := operationLocked(file, operationID, "fork_cancel", digest); err != nil {
		return ForkChild{}, err
	} else if !replay {
		file.Operations[strings.TrimSpace(operationID)] = forkOperation{Kind: "fork_cancel", Digest: digest, Ref: childID}
	}
	_, _, child := s.childLocked(run.ID, strings.TrimSpace(childID))
	if child == nil {
		return ForkChild{}, fmt.Errorf("unknown child %q in run %q", childID, runID)
	}
	switch child.Status {
	case ForkStatusQueued:
		child.Status, child.Error, child.FinishedAt = ForkStatusCancelled, "cancelled before it started", s.now().UTC().Format(time.RFC3339Nano)
	case ForkStatusRunning:
		if cancel := s.cancels[child.ID]; cancel != nil {
			cancel()
		}
		child.Activity = "Deteniendo"
	}
	s.commitLocked(file)
	return *child, nil
}

// RetryChild appends a new attempt of a settled child, from the same fork
// point, with the same message plus optional guidance. Attachments are
// re-validated against the files as they are now.
func (s *ForkService) RetryChild(runID, childID, guidance, operationID string) (ForkChild, error) {
	s.mu.Lock()
	file, run := s.lookupRunLocked(strings.TrimSpace(runID))
	if run == nil {
		s.mu.Unlock()
		return ForkChild{}, fmt.Errorf("unknown fork run %q", runID)
	}
	guidance = RedactSensitiveText(strings.TrimSpace(guidance))
	digest := forkDigest("fork-retry", runID, childID, guidance)
	if ref, replay, err := operationLocked(file, operationID, "fork_retry", digest); err != nil || replay {
		defer s.mu.Unlock()
		if err != nil {
			return ForkChild{}, err
		}
		if _, _, child := s.childLocked(run.ID, ref); child != nil {
			return *child, nil
		}
		return ForkChild{}, errors.New("fork retry receipt is missing its child")
	}
	_, _, prior := s.childLocked(run.ID, strings.TrimSpace(childID))
	if prior == nil {
		s.mu.Unlock()
		return ForkChild{}, fmt.Errorf("unknown child %q in run %q", childID, runID)
	}
	if prior.Status == ForkStatusQueued || prior.Status == ForkStatusRunning {
		s.mu.Unlock()
		return ForkChild{}, errors.New("child is still running; cancel it or wait before retrying")
	}
	_, point := s.findPointLocked(run.ForkPointID)
	if point == nil {
		s.mu.Unlock()
		return ForkChild{}, errors.New("fork point disappeared")
	}
	cwd, maxEdge := point.CWD, run.MaxImageEdge
	paths := make([]string, 0, len(prior.Attachments))
	for _, attachment := range prior.Attachments {
		paths = append(paths, attachment.Path)
	}
	priorCopy := *prior
	s.mu.Unlock()

	attachments, err := ValidateImageAttachmentPaths(paths, cwd, maxEdge)
	if err != nil {
		return ForkChild{}, err
	}
	message := priorCopy.Message
	if guidance != "" {
		message += "\n\nAdditional guidance for this retry: " + guidance
	}

	s.mu.Lock()
	defer func() {
		s.mu.Unlock()
		go s.dispatch(run.ID)
	}()
	file, run = s.findRunLocked(run.ID)
	if run == nil {
		return ForkChild{}, errors.New("fork run disappeared")
	}
	if ref, replay, err := operationLocked(file, operationID, "fork_retry", digest); err != nil || replay {
		if err != nil {
			return ForkChild{}, err
		}
		if _, _, child := s.childLocked(run.ID, ref); child != nil {
			return *child, nil
		}
		return ForkChild{}, errors.New("fork retry receipt is missing its child")
	}
	attempt := priorCopy.Attempt + 1
	retry := ForkChild{
		ID: fmt.Sprintf("%s-r%d", firstNonEmpty(priorCopy.RetryOf, priorCopy.ID), attempt), Index: priorCopy.Index,
		Label: priorCopy.Label, Message: message, Attachments: attachments, Status: ForkStatusQueued,
		RetryOf: firstNonEmpty(priorCopy.RetryOf, priorCopy.ID), Attempt: attempt,
	}
	for _, existing := range run.Children {
		if existing.ID == retry.ID {
			retry.ID = s.nextIDLocked(retry.ID)
			break
		}
	}
	run.Children = append(run.Children, retry)
	file.Operations[strings.TrimSpace(operationID)] = forkOperation{Kind: "fork_retry", Digest: digest, Ref: retry.ID}
	s.commitLocked(file)
	return retry, nil
}

// Wait blocks until the selected children of a run have settled (all, or the
// first newly settled one), the timeout passes, or ctx ends. timeout < 0 waits
// without a Workass deadline. It always returns the current snapshot.
func (s *ForkService) Wait(ctx context.Context, runID string, childIDs []string, returnWhen string, timeout time.Duration) (ForkRun, bool, error) {
	if returnWhen == "" {
		returnWhen = "all"
	}
	if returnWhen != "all" && returnWhen != "first" {
		return ForkRun{}, false, errors.New("return_when must be first or all")
	}
	var deadline <-chan time.Time
	if timeout >= 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		deadline = timer.C
	}
	selected := map[string]bool{}
	for _, id := range childIDs {
		if id = strings.TrimSpace(id); id != "" {
			selected[id] = true
		}
	}
	initialSettled := -1
	for {
		s.mu.Lock()
		_, run := s.lookupRunLocked(strings.TrimSpace(runID))
		if run == nil {
			s.mu.Unlock()
			return ForkRun{}, false, fmt.Errorf("unknown fork run %q", runID)
		}
		snapshot := cloneForkRun(*run)
		changed := s.changed
		s.mu.Unlock()
		settled, total := 0, 0
		for _, child := range snapshot.Children {
			if len(selected) > 0 && !selected[child.ID] {
				continue
			}
			total++
			if child.Status != ForkStatusQueued && child.Status != ForkStatusRunning {
				settled++
			}
		}
		if len(selected) > 0 && total != len(selected) {
			return snapshot, false, errors.New("child_ids contains an id that is not in this run")
		}
		if initialSettled < 0 {
			initialSettled = settled
		}
		if settled == total || (returnWhen == "first" && settled > initialSettled) {
			return snapshot, true, nil
		}
		select {
		case <-changed:
		case <-deadline:
			return snapshot, false, nil
		case <-ctx.Done():
			return snapshot, false, ctx.Err()
		}
	}
}

func cloneForkRun(run ForkRun) ForkRun {
	out := run
	out.Children = append([]ForkChild(nil), run.Children...)
	return out
}

// List returns one chat's fork points and runs, newest last.
func (s *ForkService) List(chatID string) ([]ForkPoint, []ForkRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	file, err := s.chatLocked(chatID)
	if err != nil {
		return nil, nil, err
	}
	points := append([]ForkPoint(nil), file.Points...)
	runs := make([]ForkRun, 0, len(file.Runs))
	for _, run := range file.Runs {
		runs = append(runs, cloneForkRun(run))
	}
	return points, runs, nil
}

// Summary returns one compact entry per chat that has fork points, so a
// sidebar can show nested subsession counts and live status without loading
// every chat's full fork list.
func (s *ForkService) Summary() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadAllLocked()
	chatIDs := make([]string, 0, len(s.chats))
	for chatID, file := range s.chats {
		if len(file.Points) > 0 {
			chatIDs = append(chatIDs, chatID)
		}
	}
	sort.Strings(chatIDs)
	out := make([]map[string]any, 0, len(chatIDs))
	for _, chatID := range chatIDs {
		file := s.chats[chatID]
		counts := map[string]int{}
		children := 0
		latest := ""
		for _, point := range file.Points {
			latest = max(latest, point.CreatedAt)
		}
		for _, run := range file.Runs {
			latest = max(latest, run.CreatedAt)
			// A retried child counts once, as its latest attempt.
			latestAttempt := map[string]ForkChild{}
			for _, child := range run.Children {
				key := firstNonEmpty(child.RetryOf, child.ID)
				if prior, ok := latestAttempt[key]; !ok || child.Attempt >= prior.Attempt {
					latestAttempt[key] = child
				}
				latest = max(latest, child.StartedAt, child.FinishedAt)
			}
			for _, child := range latestAttempt {
				children++
				counts[child.Status]++
			}
		}
		out = append(out, map[string]any{
			"chatId": chatID, "tabId": file.TabID, "forkPoints": len(file.Points), "children": children,
			"counts": counts, "updatedAt": latest,
		})
	}
	return out
}

// RunOwner returns the chat that owns a run, for authorization.
func (s *ForkService) RunOwner(runID string) (chatID, tabID string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	file, run := s.lookupRunLocked(strings.TrimSpace(runID))
	if run == nil {
		return "", "", false
	}
	return file.ChatID, firstNonEmpty(run.TabID, file.TabID), true
}

// PointOwner returns the chat that owns a fork point, for authorization.
func (s *ForkService) PointOwner(forkPointID string) (chatID, tabID string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	file, point := s.lookupPointLocked(strings.TrimSpace(forkPointID))
	if point == nil {
		return "", "", false
	}
	return file.ChatID, firstNonEmpty(point.TabID, file.TabID), true
}

// ChildImage returns one of a child's attachments for display. Only files the
// child was validated with are served, and only while their bytes match.
func (s *ForkService) ChildImage(runID, childID string, index int) (ImageAttachment, string, error) {
	s.mu.Lock()
	_, _, child := s.childLocked(strings.TrimSpace(runID), strings.TrimSpace(childID))
	if child == nil {
		s.loadAllLocked()
		_, _, child = s.childLocked(strings.TrimSpace(runID), strings.TrimSpace(childID))
	}
	if child == nil {
		s.mu.Unlock()
		return ImageAttachment{}, "", fmt.Errorf("unknown child %q in run %q", childID, runID)
	}
	if index < 0 || index >= len(child.Attachments) {
		s.mu.Unlock()
		return ImageAttachment{}, "", fmt.Errorf("child %q has no attachment %d", childID, index)
	}
	attachment := child.Attachments[index]
	s.mu.Unlock()
	data, err := LoadImageAttachment(attachment)
	return attachment, data, err
}

// SetRunRetention switches a run between throwaway and keep. Keeping stops
// its child transcripts from expiring; a throwaway child's native session was
// never persisted, so only Workass's transcript and receipt are kept.
func (s *ForkService) SetRunRetention(runID, retention string) (ForkRun, error) {
	if retention != ForkRetentionThrowaway && retention != ForkRetentionKeep {
		return ForkRun{}, errors.New("retention must be throwaway or keep")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	file, run := s.lookupRunLocked(strings.TrimSpace(runID))
	if run == nil {
		return ForkRun{}, fmt.Errorf("unknown fork run %q", runID)
	}
	run.Retention = retention
	s.commitLocked(file)
	return cloneForkRun(*run), nil
}

// Transcript returns a child's retained transcript entries.
func (s *ForkService) Transcript(runID, childID string) ([]map[string]any, error) {
	s.mu.Lock()
	file, _, child := s.childLocked(strings.TrimSpace(runID), strings.TrimSpace(childID))
	if child == nil {
		s.loadAllLocked()
		file, _, child = s.childLocked(strings.TrimSpace(runID), strings.TrimSpace(childID))
	}
	if child == nil {
		s.mu.Unlock()
		return nil, fmt.Errorf("unknown child %q in run %q", childID, runID)
	}
	path := s.transcriptPath(file.ChatID, runID, child.ID)
	s.mu.Unlock()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errors.New("this child's transcript is no longer retained")
		}
		return nil, err
	}
	entries := make([]map[string]any, 0)
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var entry map[string]any
		if json.Unmarshal([]byte(line), &entry) == nil {
			entries = append(entries, entry)
		}
	}
	return entries, nil
}

// CancelAll stops every live child; used on daemon shutdown.
func (s *ForkService) CancelAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, cancel := range s.cancels {
		cancel()
	}
}

// ForkChildView is the public projection of one child.
func ForkChildView(child ForkChild) map[string]any {
	view := map[string]any{
		"childId": child.ID, "index": child.Index, "status": child.Status,
		"usage": map[string]any{
			"input": child.Usage.InputTokens, "output": child.Usage.OutputTokens,
			"cacheRead": child.Usage.CacheReadTokens, "cacheCreation": child.Usage.CacheCreationTokens,
		},
		"cacheHitShare": roundShare(child.Usage.CacheHitShare()), "turns": child.Turns, "wallMs": child.WallMS,
		"attempt": child.Attempt, "transcriptRetained": child.Transcript,
	}
	attachments := make([]any, 0, len(child.Attachments))
	for _, attachment := range child.Attachments {
		attachments = append(attachments, map[string]any{
			"name": attachment.Name, "path": attachment.Path, "width": attachment.Width, "height": attachment.Height, "bytes": attachment.Bytes,
		})
	}
	view["attachments"] = attachments
	for key, value := range map[string]string{
		"label": child.Label, "result": child.Result, "error": child.Error, "stopReason": child.StopReason,
		"activity": child.Activity, "startedAt": child.StartedAt, "finishedAt": child.FinishedAt,
		"retryOf": child.RetryOf, "sessionId": child.SessionID,
	} {
		if value != "" {
			view[key] = value
		}
	}
	if child.NativeKept {
		view["nativeSessionKept"] = true
	}
	return view
}

// ForkRunView is the public projection of one run.
func ForkRunView(run ForkRun) map[string]any {
	children := make([]any, 0, len(run.Children))
	var total ForkUsage
	counts := map[string]int{}
	for _, child := range run.Children {
		children = append(children, ForkChildView(child))
		counts[child.Status]++
		total.InputTokens += child.Usage.InputTokens
		total.OutputTokens += child.Usage.OutputTokens
		total.CacheReadTokens += child.Usage.CacheReadTokens
		total.CacheCreationTokens += child.Usage.CacheCreationTokens
	}
	view := map[string]any{
		"runId": run.ID, "forkPointId": run.ForkPointID, "chatId": run.ChatID, "tabId": run.TabID,
		"status": run.Status(), "concurrency": run.Concurrency, "retention": run.Retention, "retentionDays": run.RetentionDays,
		"createdAt": run.CreatedAt, "children": children, "counts": counts,
		"totals": map[string]any{
			"input": total.InputTokens, "output": total.OutputTokens,
			"cacheRead": total.CacheReadTokens, "cacheCreation": total.CacheCreationTokens,
			"cacheHitShare": roundShare(total.CacheHitShare()),
		},
	}
	warnings := make([]any, 0)
	for _, child := range run.Children {
		limit := ForkCacheLookbackImages
		if run.Retention == ForkRetentionKeep {
			limit = ForkCacheLookbackImagesKept
		}
		if len(child.Attachments) > limit {
			warnings = append(warnings, fmt.Sprintf("%s carries %d images; above %d per %s child the provider cannot reach the fork point in its cache lookback and re-caches the shared prefix", child.ID, len(child.Attachments), limit, run.Retention))
		}
	}
	if len(warnings) > 0 {
		view["warnings"] = warnings
	}
	return view
}

// ForkPointView is the public projection of one fork point.
func ForkPointView(point ForkPoint) map[string]any {
	view := map[string]any{
		"forkPointId": point.ID, "name": point.Name, "chatId": point.ChatID, "tabId": point.TabID,
		"providerId": point.ProviderID, "status": point.Status, "cwd": point.CWD, "createdAt": point.CreatedAt,
		"sealUsage": map[string]any{
			"input": point.SealUsage.InputTokens, "output": point.SealUsage.OutputTokens,
			"cacheRead": point.SealUsage.CacheReadTokens, "cacheCreation": point.SealUsage.CacheCreationTokens,
		},
		"sealWallMs": point.SealWallMS, "prefixSize": point.PrefixTokens, "sealTurns": len(point.SealHops),
	}
	if len(point.AddDirs) > 0 {
		view["addDirs"] = append([]string(nil), point.AddDirs...)
	}
	for key, value := range map[string]string{
		"modelId": point.ModelID, "effort": point.Effort, "modeId": point.ModeID, "error": point.Error,
		"readyAt": point.ReadyAt, "afterMessageId": point.AfterMessageID,
	} {
		if value != "" {
			view[key] = value
		}
	}
	return view
}

func roundShare(value float64) float64 {
	return float64(int(value*1000+0.5)) / 1000
}
