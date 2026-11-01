// Package scheduledmessage is the daemon-owned, durable "deliver this message
// to my own chat later" store. It outlives engine sleep and daemon restarts.
package scheduledmessage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"

	"workass/internal/durablefs"
)

const (
	stateFileName    = "scheduled-messages.json"
	fileVersion      = 1
	maxPending       = 50
	maxAttempts      = 3
	maxErrorLen      = 300
	settledRetention = 7 * 24 * time.Hour
	maxSettled       = 200
	defaultTick      = 5 * time.Second
)

const (
	StatusPending   = "pending"
	StatusFired     = "fired"
	StatusCancelled = "cancelled"
	StatusExpired   = "expired"
	StatusFailed    = "failed"
)

// ErrChatGone is returned (or wrapped) by a Sender when the target chat no
// longer exists: the scheduled message settles failed at once, without retry.
var ErrChatGone = errors.New("chat no longer exists")

// ErrNotFound is returned by Cancel for an id that is not in the caller's chat.
var ErrNotFound = errors.New("no scheduled message with that id in this chat")

type Condition struct {
	Kind string    `json:"kind"` // "at" | "tcp" | "file" | "pid"
	At   time.Time `json:"at,omitzero"`
	Addr string    `json:"addr,omitempty"`
	Path string    `json:"path,omitempty"`
	PID  int       `json:"pid,omitempty"`
}

type Entry struct {
	ID          string     `json:"id"`
	TabID       string     `json:"tabId"`
	ChatID      string     `json:"chatId"`
	OperationID string     `json:"operationId"`
	Message     string     `json:"message"`
	RequestHash string     `json:"requestHash"`
	Condition   Condition  `json:"condition"`
	CreatedAt   time.Time  `json:"createdAt"`
	ExpiresAt   time.Time  `json:"expiresAt,omitzero"` // zero for "at" scheduled messages
	Status      string     `json:"status"`
	SettledAt   *time.Time `json:"settledAt,omitempty"`
	// Late marks an "at" scheduled message that came due while the daemon was
	// not running.
	Late     bool   `json:"late,omitempty"`
	Attempts int    `json:"attempts,omitempty"`
	Error    string `json:"error,omitempty"` // redacted, short
}

// Sender delivers text to the entry's own chat.
type Sender func(ctx context.Context, e Entry, text string) error

type Options struct {
	StateDir string
	Now      func() time.Time
	Probes   Probes
	Send     Sender
	Tick     time.Duration // default 5s
}

type diskFile struct {
	Version int     `json:"version"`
	Entries []Entry `json:"entries"`
}

type Scheduler struct {
	path   string
	now    func() time.Time
	probes Probes
	send   Sender
	tick   time.Duration

	mu       sync.Mutex
	entries  []Entry
	inflight map[string]bool
}

// New loads <StateDir>/scheduled-messages.json. A missing file is empty; a
// corrupt one is moved aside so the daemon always starts.
func New(opts Options) (*Scheduler, error) {
	if opts.StateDir == "" {
		return nil, errors.New("scheduled messages need a state directory")
	}
	if opts.Send == nil {
		return nil, errors.New("scheduled messages need a sender")
	}
	s := &Scheduler{
		path:     filepath.Join(opts.StateDir, stateFileName),
		now:      opts.Now,
		probes:   opts.Probes.withDefaults(),
		send:     opts.Send,
		tick:     opts.Tick,
		inflight: map[string]bool{},
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.tick <= 0 {
		s.tick = defaultTick
	}
	if err := os.MkdirAll(opts.StateDir, 0o700); err != nil {
		return nil, err
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	now := s.now()
	changed := false
	for i := range s.entries {
		e := &s.entries[i]
		if e.Status == StatusPending && e.Condition.Kind == "at" && !e.Condition.At.After(now) && !e.Late {
			e.Late = true
			changed = true
		}
	}
	if s.prune(now) {
		changed = true
	}
	if changed {
		if err := s.persist(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *Scheduler) load() error {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var disk diskFile
	if err := json.Unmarshal(data, &disk); err != nil || disk.Version != fileVersion {
		aside := fmt.Sprintf("%s.corrupt-%d", s.path, s.now().Unix())
		if renameErr := os.Rename(s.path, aside); renameErr != nil {
			return renameErr
		}
		fmt.Fprintf(os.Stderr, "scheduled messages: unreadable state moved to %s; starting empty\n", aside)
		return nil
	}
	s.entries = disk.Entries
	return nil
}

// persist writes the state durably. The caller holds s.mu (or is New).
func (s *Scheduler) persist() error {
	entries := s.entries
	if entries == nil {
		entries = []Entry{}
	}
	data, err := json.Marshal(diskFile{Version: fileVersion, Entries: entries})
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".scheduled-messages-*.tmp")
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
	return durablefs.SyncDirectory(dir)
}

// persistLogged is for state changes that cannot be reported to a caller.
func (s *Scheduler) persistLogged() {
	s.prune(s.now())
	if err := s.persist(); err != nil {
		fmt.Fprintf(os.Stderr, "scheduled messages: cannot save state: %v\n", err)
	}
}

// prune drops settled scheduled messages past retention and keeps at most
// maxSettled per chat; pending ones are never dropped. Reports a change.
func (s *Scheduler) prune(now time.Time) bool {
	type chatKey struct{ tab, chat string }
	settled := map[chatKey][]int{}
	drop := map[int]bool{}
	for i, e := range s.entries {
		if e.Status == StatusPending {
			continue
		}
		if e.SettledAt != nil && now.Sub(*e.SettledAt) > settledRetention {
			drop[i] = true
			continue
		}
		k := chatKey{e.TabID, e.ChatID}
		settled[k] = append(settled[k], i)
	}
	for _, idx := range settled {
		if len(idx) <= maxSettled {
			continue
		}
		sort.SliceStable(idx, func(a, b int) bool {
			return settledTime(s.entries[idx[a]]).After(settledTime(s.entries[idx[b]]))
		})
		for _, i := range idx[maxSettled:] {
			drop[i] = true
		}
	}
	if len(drop) == 0 {
		return false
	}
	kept := s.entries[:0:0]
	for i, e := range s.entries {
		if !drop[i] {
			kept = append(kept, e)
		}
	}
	s.entries = kept
	return true
}

func settledTime(e Entry) time.Time {
	if e.SettledAt != nil {
		return *e.SettledAt
	}
	return e.CreatedAt
}

func newID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "sched-" + hex.EncodeToString(b[:]), nil
}

func (e Entry) clone() Entry {
	if e.SettledAt != nil {
		t := *e.SettledAt
		e.SettledAt = &t
	}
	return e
}

// Schedule records a scheduled message for the chat. The same operation id
// with the same request returns the existing one unchanged.
func (s *Scheduler) Schedule(tabID, chatID string, req Request) (Entry, error) {
	if tabID == "" || chatID == "" {
		return Entry{}, errors.New("a scheduled message needs the chat it belongs to")
	}
	hash := req.hash()
	s.mu.Lock()
	defer s.mu.Unlock()
	pending := 0
	for _, e := range s.entries {
		if e.TabID != tabID || e.ChatID != chatID {
			continue
		}
		if e.OperationID == req.OperationID {
			if e.RequestHash == hash {
				return e.clone(), nil
			}
			return Entry{}, errors.New("operation_id was already used for a different scheduled message")
		}
		if e.Status == StatusPending {
			pending++
		}
	}
	if pending >= maxPending {
		return Entry{}, fmt.Errorf("this chat already has %d pending scheduled messages; cancel one first", maxPending)
	}
	now := s.now()
	entry := Entry{
		TabID:       tabID,
		ChatID:      chatID,
		OperationID: req.OperationID,
		Message:     req.Message,
		RequestHash: hash,
		Condition:   req.Condition,
		CreatedAt:   now,
		Status:      StatusPending,
	}
	if req.Field == "after_seconds" {
		entry.Condition.At = now.Add(req.After).UTC()
	}
	if entry.Condition.Kind == "at" {
		if !entry.Condition.At.After(now) {
			return Entry{}, errors.New("at must be in the future")
		}
		if entry.Condition.At.After(now.Add(maxAhead)) {
			return Entry{}, errors.New("at must be within the next 7 days")
		}
	} else {
		entry.ExpiresAt = now.Add(req.ExpiresAfter)
	}
	id, err := newID()
	if err != nil {
		return Entry{}, err
	}
	entry.ID = id
	s.entries = append(s.entries, entry)
	if err := s.persist(); err != nil {
		s.entries = s.entries[:len(s.entries)-1]
		return Entry{}, fmt.Errorf("could not save the scheduled message: %w", err)
	}
	return entry.clone(), nil
}

// List returns the chat's scheduled messages: pending first (soonest created
// first), then settled newest-first.
func (s *Scheduler) List(tabID, chatID string) []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	var pending, settled []Entry
	for _, e := range s.entries {
		if e.TabID != tabID || e.ChatID != chatID {
			continue
		}
		if e.Status == StatusPending {
			pending = append(pending, e.clone())
		} else {
			settled = append(settled, e.clone())
		}
	}
	sort.SliceStable(pending, func(a, b int) bool {
		if !pending[a].CreatedAt.Equal(pending[b].CreatedAt) {
			return pending[a].CreatedAt.Before(pending[b].CreatedAt)
		}
		return pending[a].ID < pending[b].ID
	})
	sort.SliceStable(settled, func(a, b int) bool {
		return settledTime(settled[a]).After(settledTime(settled[b]))
	})
	return append(pending, settled...)
}

// Cancel cancels one pending scheduled message of that chat. An already
// settled one is returned as it is. One whose delivery is under way cannot be
// cancelled any more.
func (s *Scheduler) Cancel(tabID, chatID, id string) (Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.entries {
		e := &s.entries[i]
		if e.ID != id || e.TabID != tabID || e.ChatID != chatID {
			continue
		}
		if e.Status != StatusPending {
			return e.clone(), nil
		}
		if s.inflight[e.ID] {
			return Entry{}, errors.New("this scheduled message is being delivered right now and can no longer be cancelled")
		}
		prev := *e
		s.settle(e, StatusCancelled, "")
		if err := s.persist(); err != nil {
			*e = prev
			return Entry{}, fmt.Errorf("could not save the cancellation: %w", err)
		}
		settled := e.clone()
		if s.prune(s.now()) {
			s.persistLogged()
		}
		return settled, nil
	}
	return Entry{}, ErrNotFound
}

// CancelChat cancels every pending scheduled message of the chat.
func (s *Scheduler) CancelChat(tabID, chatID, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for i := range s.entries {
		e := &s.entries[i]
		if e.TabID == tabID && e.ChatID == chatID && e.Status == StatusPending {
			s.settle(e, StatusCancelled, reason)
			changed = true
		}
	}
	if changed {
		s.persistLogged()
	}
}

// settle moves a pending entry to a final status. The caller persists, and
// must not hold entry pointers across a prune.
func (s *Scheduler) settle(e *Entry, status, errText string) {
	now := s.now()
	e.Status = status
	e.SettledAt = &now
	e.Error = redact(errText)
}

type candidate struct {
	entry Entry
	probe bool
}

// Tick runs one evaluation pass. Probes and Send run without the mutex held.
func (s *Scheduler) Tick(ctx context.Context) {
	now := s.now()
	var work []candidate
	s.mu.Lock()
	changed := false
	for i := range s.entries {
		e := &s.entries[i]
		if e.Status != StatusPending || s.inflight[e.ID] {
			continue
		}
		if e.Condition.Kind == "at" {
			if now.Before(e.Condition.At) {
				continue
			}
			s.inflight[e.ID] = true
			work = append(work, candidate{entry: e.clone()})
			continue
		}
		if !now.Before(e.ExpiresAt) {
			s.settle(e, StatusExpired, "")
			changed = true
			continue
		}
		s.inflight[e.ID] = true
		work = append(work, candidate{entry: e.clone(), probe: true})
	}
	if changed {
		s.persistLogged()
	}
	s.mu.Unlock()

	for _, c := range work {
		if ctx.Err() != nil {
			s.release(c.entry.ID)
			continue
		}
		if c.probe && !s.due(c.entry.Condition) {
			s.release(c.entry.ID)
			continue
		}
		err := s.send(ctx, c.entry, Text(c.entry))
		s.finish(c.entry.ID, err)
	}
}

func (s *Scheduler) due(c Condition) bool {
	switch c.Kind {
	case "tcp":
		return s.probes.TCPUp(c.Addr)
	case "file":
		return s.probes.FileExists(c.Path)
	case "pid":
		return !s.probes.PIDAlive(c.PID)
	}
	return false
}

func (s *Scheduler) release(id string) {
	s.mu.Lock()
	delete(s.inflight, id)
	s.mu.Unlock()
}

func (s *Scheduler) finish(id string, sendErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.inflight, id)
	for i := range s.entries {
		e := &s.entries[i]
		if e.ID != id || e.Status != StatusPending {
			continue
		}
		switch {
		case sendErr == nil:
			s.settle(e, StatusFired, "")
		case errors.Is(sendErr, ErrChatGone):
			s.settle(e, StatusFailed, "chat deleted")
		default:
			e.Attempts++
			e.Error = redact(sendErr.Error())
			if e.Attempts >= maxAttempts {
				s.settle(e, StatusFailed, sendErr.Error())
			}
		}
		s.persistLogged()
		return
	}
}

// Run evaluates until ctx is done.
func (s *Scheduler) Run(ctx context.Context) {
	ticker := time.NewTicker(s.tick)
	defer ticker.Stop()
	for {
		s.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Text is the message as delivered into the chat.
func Text(e Entry) string {
	var summary string
	switch e.Condition.Kind {
	case "at":
		summary = "at " + e.Condition.At.Format(time.RFC3339)
	case "tcp":
		summary = "when " + e.Condition.Addr + " accepts connections"
	case "file":
		summary = "when " + e.Condition.Path + " exists"
	case "pid":
		summary = fmt.Sprintf("when pid %d exits", e.Condition.PID)
	}
	text := "[Workass scheduled message " + e.ID + " · " + summary + "]"
	if e.Late {
		text += "\n(This was due at " + e.Condition.At.Format(time.RFC3339) + "; Workass was not running then, so it is delivered late.)"
	}
	return text + "\n\n" + e.Message
}

var secretPattern = regexp.MustCompile(`(?i)\b(api[_-]?key|token|secret|password|credential|bearer)\b(["']?\s*[:=]?\s*)\S+`)

// redact shortens an error and masks anything that looks like a credential.
func redact(text string) string {
	text = secretPattern.ReplaceAllString(text, "${1}${2}[redacted]")
	if r := []rune(text); len(r) > maxErrorLen {
		text = string(r[:maxErrorLen])
	}
	return text
}
