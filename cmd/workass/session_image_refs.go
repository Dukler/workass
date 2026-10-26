package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"workass/internal/acp"
	"workass/internal/chat"
)

// imageRefChat is one durable chat as the image store sees it.
type imageRefChat struct {
	chatID, tabID string
	deleted       bool
	// updatedAt: when its durable state last changed (for a tombstone, no
	// earlier than the deletion).
	updatedAt time.Time
}

// Row kinds beyond the SQLite ones: a file-backend actor is one envelope
// file plus its journal.
const imageRefEnvelope = "envelope"

// imageReferenceSource is where chat actors keep their rows: state.db, or
// the provider-chats JSON files of the file backend.
type imageReferenceSource interface {
	chats(ctx context.Context) ([]imageRefChat, error)
	// eachRow visits one chat's serialized rows (chat.EachImageReferenceRow).
	eachRow(ctx context.Context, chatID string, kinds map[string]bool, stable bool, pause func() error, restart func(), visit func(kind string, data []byte) error) error
	// coveredFiles are the state-relative files eachRow reads for chatID.
	coveredFiles(chatID string) []string
	// ownedFiles are state-relative files that hold only chatID's history
	// (frozen JSON backups after the SQLite cutover).
	ownedFiles(chatID string) []string
	// skipped are state-relative files eachRow covers for every chat.
	skipped() []string
}

func providerChatFileName(chatID string) string {
	digest := sha256.Sum256([]byte(strings.TrimSpace(chatID)))
	return filepath.ToSlash(filepath.Join("provider-chats", hex.EncodeToString(digest[:])+".json"))
}

// sqlImageReferences reads chats from state.db.
type sqlImageReferences struct{ actors *chat.SQLActors }

func (s sqlImageReferences) chats(ctx context.Context) ([]imageRefChat, error) {
	rows, err := s.actors.ImageReferenceChats(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]imageRefChat, 0, len(rows))
	for _, row := range rows {
		updated, _ := time.Parse(time.RFC3339Nano, row.UpdatedAt)
		out = append(out, imageRefChat{chatID: row.ChatID, tabID: row.TabID, deleted: row.Deleted, updatedAt: updated})
	}
	return out, nil
}

func (s sqlImageReferences) eachRow(ctx context.Context, chatID string, kinds map[string]bool, stable bool, pause func() error, restart func(), visit func(string, []byte) error) error {
	return s.actors.EachImageReferenceRow(ctx, chatID, kinds, stable, pause, restart, visit)
}

func (sqlImageReferences) coveredFiles(string) []string { return nil }
func (sqlImageReferences) ownedFiles(chatID string) []string {
	return []string{providerChatFileName(chatID)}
}
func (sqlImageReferences) skipped() []string {
	return []string{"state.db", "state.db-wal", "state.db-shm", "state.db-journal"}
}

// fileImageReferences reads chats from provider-chats/<sha256(chatId)>.json
// and its .events journal. The chat list comes from the runtime, which
// discovered every actor at boot.
type fileImageReferences struct {
	stateDir string
	runtime  *providerChatRuntime
}

func (s fileImageReferences) chats(ctx context.Context) ([]imageRefChat, error) {
	identities, err := s.runtime.imageReferenceIdentities()
	if err != nil {
		return nil, err
	}
	out := make([]imageRefChat, 0, len(identities))
	for _, identity := range identities {
		entry := imageRefChat{chatID: identity.ChatID, tabID: identity.TabID, deleted: identity.Deleted}
		if info, err := os.Stat(filepath.Join(s.stateDir, filepath.FromSlash(providerChatFileName(identity.ChatID)))); err == nil {
			entry.updatedAt = info.ModTime()
		}
		out = append(out, entry)
	}
	return out, nil
}

func (s fileImageReferences) eachRow(ctx context.Context, chatID string, kinds map[string]bool, stable bool, pause func() error, restart func(), visit func(string, []byte) error) error {
	base := filepath.Join(s.stateDir, filepath.FromSlash(providerChatFileName(chatID)))
	paths := []string{base, base + ".events"}
	stamp := func() string {
		var out strings.Builder
		for _, path := range paths {
			if info, err := os.Stat(path); err == nil {
				fmt.Fprintf(&out, "%d:%d", info.ModTime().UnixNano(), info.Size())
			}
			out.WriteString("|")
		}
		return out.String()
	}
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 && restart != nil {
			restart()
		}
		before := stamp()
		for index, path := range paths {
			if pause != nil {
				if err := pause(); err != nil {
					return err
				}
			}
			data, err := os.ReadFile(path)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			kind := imageRefEnvelope
			if index > 0 {
				kind = chat.ImageReferenceJournal
			}
			if err := visit(kind, data); err != nil {
				return err
			}
		}
		if !stable || stamp() == before {
			return nil
		}
	}
	return chat.ErrImageReferenceScanChanged
}

func (s fileImageReferences) coveredFiles(chatID string) []string {
	return []string{providerChatFileName(chatID)}
}
func (s fileImageReferences) ownedFiles(chatID string) []string {
	return []string{providerChatFileName(chatID)}
}
func (fileImageReferences) skipped() []string { return nil }

// imageReferenceIdentities lists every known chat's identity without opening
// a closed actor.
func (r *providerChatRuntime) imageReferenceIdentities() ([]chat.IdentitySnapshot, error) {
	if r == nil {
		return nil, errors.New("provider chat runtime is unavailable")
	}
	r.mu.Lock()
	if r.bootErr != nil {
		r.mu.Unlock()
		return nil, r.bootErr
	}
	ids := make([]string, 0, len(r.known))
	for chatID := range r.known {
		ids = append(ids, chatID)
	}
	resident := make(map[string]*providerChatActor, len(r.actors))
	for chatID, actor := range r.actors {
		resident[chatID] = actor
	}
	summaries := make(map[string]chat.IdentitySnapshot, len(r.summaries))
	for chatID, summary := range r.summaries {
		summaries[chatID] = summary.IdentitySnapshot()
	}
	r.mu.Unlock()
	sort.Strings(ids)
	out := make([]chat.IdentitySnapshot, 0, len(ids))
	for _, chatID := range ids {
		if actor := resident[chatID]; actor != nil {
			out = append(out, actor.engine.IdentitySnapshot())
			continue
		}
		if identity, ok := summaries[chatID]; ok {
			out = append(out, identity)
			continue
		}
		// Known but neither resident nor summarized: nothing can vouch for
		// it, so it is reported as live.
		out = append(out, chat.IdentitySnapshot{ChatID: chatID})
	}
	return out, nil
}

// digestScanner finds every run of exactly 64 lowercase hex characters in a
// byte stream, across write boundaries. It over-reports (any sha256 counts),
// which only ever keeps an image.
type digestScanner struct {
	run   [64]byte
	n     int
	long  bool
	found func(string)
}

func isLowerHexByte(char byte) bool { return char >= '0' && char <= '9' || char >= 'a' && char <= 'f' }

func (s *digestScanner) Write(data []byte) (int, error) {
	for _, char := range data {
		if isLowerHexByte(char) {
			if s.n < len(s.run) {
				s.run[s.n] = char
				s.n++
			} else {
				s.long = true
			}
			continue
		}
		s.flush()
	}
	return len(data), nil
}

func (s *digestScanner) flush() {
	if s.n == len(s.run) && !s.long {
		s.found(string(s.run[:]))
	}
	s.n, s.long = 0, false
}

// scanDigests reports every digest-shaped token in data.
func scanDigests(data []byte, found func(string)) {
	scanner := &digestScanner{found: found}
	_, _ = scanner.Write(data)
	scanner.flush()
}

// imageCloneHints maps a digest to the local files that may hold it.
type imageCloneHints map[string][]string

func (h imageCloneHints) add(digest, path string) {
	path = strings.TrimSpace(path)
	if len(digest) != 64 || path == "" || !filepath.IsAbs(path) {
		return
	}
	existing := h[digest]
	for _, known := range existing {
		if known == path {
			return
		}
	}
	if len(existing) < 8 {
		h[digest] = append(existing, path)
	}
}

type imageHintAttachment struct {
	Digest string
	Source string
}

type imageHintEvent struct {
	Attachments []imageHintAttachment
	Timeline    []struct {
		Tool *struct {
			Input       string
			Attachments []imageHintAttachment
		}
	}
	Terminal *struct{ Attachments []imageHintAttachment }
}

// addRow records the clone hints one ledger row (or a file-backend envelope
// of rows) carries: a tool's input paths for the images it returned, and an
// attachment's absolute source. Only the fields named are decoded.
func (h imageCloneHints) addRow(kind string, data []byte) {
	if !bytes.Contains(data, []byte(`"Digest":"`)) {
		return
	}
	var events []imageHintEvent
	switch kind {
	case chat.ImageReferenceMessage:
		var event imageHintEvent
		if json.Unmarshal(data, &event) != nil {
			return
		}
		events = []imageHintEvent{event}
	case imageRefEnvelope:
		var envelope struct {
			State struct{ Ledger []imageHintEvent } `json:"state"`
		}
		if json.Unmarshal(data, &envelope) != nil {
			return
		}
		events = envelope.State.Ledger
	default:
		return
	}
	for _, event := range events {
		for _, attachment := range event.Attachments {
			h.add(attachment.Digest, attachment.Source)
		}
		if event.Terminal != nil {
			for _, attachment := range event.Terminal.Attachments {
				h.add(attachment.Digest, attachment.Source)
			}
		}
		for _, entry := range event.Timeline {
			if entry.Tool == nil || len(entry.Tool.Attachments) == 0 {
				continue
			}
			paths := acp.ToolInputImagePaths(entry.Tool.Input)
			for _, attachment := range entry.Tool.Attachments {
				h.add(attachment.Digest, attachment.Source)
				for _, path := range paths {
					h.add(attachment.Digest, path)
				}
			}
		}
	}
}

// collectImageCloneHints reads every chat's ledger once, best effort: a chat
// that cannot be read only loses its hints, and its images are written.
func collectImageCloneHints(ctx context.Context, source imageReferenceSource, pause func() error) (imageCloneHints, error) {
	hints := imageCloneHints{}
	chats, err := source.chats(ctx)
	if err != nil {
		return hints, err
	}
	kinds := map[string]bool{chat.ImageReferenceMessage: true, imageRefEnvelope: true}
	for _, entry := range chats {
		if err := ctx.Err(); err != nil {
			return hints, err
		}
		_ = source.eachRow(ctx, entry.chatID, kinds, false, pause, nil, func(kind string, data []byte) error {
			hints.addRow(kind, data)
			return nil
		})
	}
	return hints, nil
}

// scanFileDigests streams a file through the digest scanner.
func scanFileDigests(path string, found func(string)) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	scanner := &digestScanner{found: found}
	if _, err := io.Copy(scanner, file); err != nil {
		return err
	}
	scanner.flush()
	return nil
}
