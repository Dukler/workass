package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"workass/internal/store"
)

// Images of deleted chats. Deleting a chat leaves a tombstone: the actor is
// permanent, never shown again, and its images are refused to every reader
// (authorizeSessionImageByTab). An image becomes garbage only when ALL of
// these hold, and otherwise stays:
//
//   - a tombstoned chat references it, and that chat was deleted (its durable
//     state last changed) more than imageStoreDeletedChatGrace ago;
//   - no other chat references it: every other chat's head, ledger rows,
//     journal and tool owners are read, each as one committed state;
//   - no other file in the state directory names its digest: legacy JSON
//     stores, archives, forks, checkpoints, receipts, the session mirror and
//     any SQLite backup (read row by row), except files that belong to the
//     collected chats themselves;
//   - this process has not written or re-used it in the last
//     imageStoreTouchHold, checked again under the store's write lock right
//     before the files go.
//
// Any read error, a chat that keeps changing while it is read, or anything
// else unexpected stops the collection with nothing removed.

// collectDeletedChatImages removes images only long-deleted chats reference.
func (m *imageStoreMaintenance) collectDeletedChatImages(ctx context.Context, openReceipts func() (*imageStoreReceipts, error), throttle *imageStoreThrottle, report *imageStoreReport) error {
	if m.refs == nil {
		return errors.New("no chat store to count references in")
	}
	chats, err := m.refs.chats(ctx)
	if err != nil {
		return err
	}
	now := m.now()
	eligible := make(map[string]imageRefChat)
	for _, entry := range chats {
		if entry.deleted && !entry.updatedAt.IsZero() && now.Sub(entry.updatedAt) >= m.grace {
			eligible[entry.chatID] = entry
		}
	}
	report.DeletedChatsEligible = len(eligible)
	if len(eligible) == 0 {
		return nil
	}
	entries, err := m.listImageStore()
	if err != nil {
		return err
	}
	present := make(map[string]imageStoreEntry, len(entries))
	for _, entry := range entries {
		present[entry.name] = entry
	}
	pause := func() error { return m.sleep(ctx, time.Millisecond) }
	// The collected chats' own files: skipped by the live scan, and their
	// digests are candidates too.
	owned := make(map[string]bool)
	for chatID, entry := range eligible {
		for _, path := range m.refs.ownedFiles(chatID) {
			owned[path] = true
		}
		if tab := strings.TrimSpace(entry.tabID); tab != "" && !strings.ContainsAny(tab, `/\`) {
			owned[filepath.ToSlash(filepath.Join("chat-archive", tab+".jsonl"))] = true
			owned[filepath.ToSlash(filepath.Join("dropped-chats", tab+".json"))] = true
		}
	}

	candidates := make(map[string]bool)
	addCandidate := func(digest string) {
		if _, ok := present[digest]; ok {
			candidates[digest] = true
		}
	}
	chatIDs := sortedImageRefChatIDs(eligible)
	for _, chatID := range chatIDs {
		if err := m.refs.eachRow(ctx, chatID, nil, true, pause, nil, func(_ string, data []byte) error {
			scanDigests(data, addCandidate)
			return throttle.add(ctx, int64(len(data)))
		}); err != nil {
			return fmt.Errorf("read deleted chat %s: %w", chatID, err)
		}
	}
	for path := range owned {
		if err := scanFileDigests(filepath.Join(m.stateDir, filepath.FromSlash(path)), addCandidate); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if len(candidates) == 0 {
		return nil
	}

	// Every other reference keeps an image.
	keep := func(digest string) { delete(candidates, digest) }
	covered := make(map[string]bool)
	for _, path := range m.refs.skipped() {
		covered[path] = true
	}
	for _, entry := range chats {
		for _, path := range m.refs.coveredFiles(entry.chatID) {
			covered[path] = true
		}
		if _, collected := eligible[entry.chatID]; collected {
			continue
		}
		if len(candidates) == 0 {
			return nil
		}
		// A chat that commits while it is read is read again from the start;
		// what an earlier attempt kept stays kept, which is only ever more
		// conservative.
		err := m.refs.eachRow(ctx, entry.chatID, nil, true, pause, nil, func(_ string, data []byte) error {
			scanDigests(data, keep)
			return throttle.add(ctx, int64(len(data)))
		})
		if err != nil {
			return fmt.Errorf("read chat %s: %w", entry.chatID, err)
		}
	}
	if err := m.scanStateFiles(ctx, covered, owned, keep, throttle); err != nil {
		return err
	}
	if len(candidates) == 0 {
		return nil
	}

	receipts, err := openReceipts()
	if err != nil {
		return err
	}
	cutoff := m.now().Add(-m.touchHold)
	digests := make([]string, 0, len(candidates))
	for digest := range candidates {
		digests = append(digests, digest)
	}
	sort.Strings(digests)
	removedAny := false
	for _, digest := range digests {
		if err := ctx.Err(); err != nil {
			return err
		}
		sessionImageStoreMu.Lock()
		if sessionImageTouchedSince(digest, cutoff) {
			sessionImageStoreMu.Unlock()
			continue
		}
		var freed int64
		var removeErr error
		for _, path := range []string{sessionImageRawPath(m.stateDir, digest), sessionImageLegacyPath(m.stateDir, digest)} {
			if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() {
				if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
					removeErr = err
					continue
				}
				verifiedSessionImages.Delete(path)
				freed += info.Size()
			}
		}
		sessionImageStoreMu.Unlock()
		if removeErr != nil {
			return removeErr
		}
		m.removeThumbs(digest)
		removedAny = true
		report.Collected++
		report.CollectedBytes += freed
		if err := receipts.record(imageStoreReceipt{Digest: digest, Outcome: imageReceiptCollected, Detail: "only deleted chats referenced it"}); err != nil {
			return err
		}
		if err := m.step("collect", digest); err != nil {
			return err
		}
	}
	if removedAny {
		return syncDirectory(m.imagesDir)
	}
	return nil
}

func sortedImageRefChatIDs(chats map[string]imageRefChat) []string {
	ids := make([]string, 0, len(chats))
	for id := range chats {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// scanStateFiles reports every digest named by any file in the state
// directory outside the image store itself, the files a reference source
// reads per chat (covered), and the collected chats' own files (owned).
func (m *imageStoreMaintenance) scanStateFiles(ctx context.Context, covered, owned map[string]bool, found func(string), throttle *imageStoreThrottle) error {
	skipDirs := map[string]bool{sessionImageDirname: true, chatImageThumbDirname: true, imageStoreDirname: true}
	return filepath.WalkDir(m.stateDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, os.ErrNotExist) {
				return nil
			}
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(m.stateDir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if entry.IsDir() {
			if rel != "." && !strings.Contains(rel, "/") && skipDirs[rel] {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() || covered[rel] || owned[rel] {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if err := throttle.add(ctx, info.Size()); err != nil {
			return err
		}
		if isSQLiteFile(path) {
			return store.ScanTextReadOnly(ctx, path, func(_ string, value []byte) error {
				scanDigests(value, found)
				return nil
			})
		}
		if err := scanFileDigests(path, found); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	})
}

var sqliteFileHeader = []byte("SQLite format 3\x00")

func isSQLiteFile(path string) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	head := make([]byte, len(sqliteFileHeader))
	if _, err := io.ReadFull(file, head); err != nil {
		return false
	}
	return bytes.Equal(head, sqliteFileHeader)
}

// removeThumbs drops the display renditions of a removed image.
func (m *imageStoreMaintenance) removeThumbs(digest string) {
	for _, width := range chatImageThumbWidths {
		_ = os.Remove(filepath.Join(m.thumbsDir, fmt.Sprintf("%s-w%d", digest, width)))
	}
}
