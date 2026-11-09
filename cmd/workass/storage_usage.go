package main

import (
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"workass/internal/chat"
	"workass/internal/procchild"
	"workass/internal/store"
)

// storage:usage (Ajustes · Mantenimiento) is how much disk this daemon's state
// directory takes, split into what a person can name: chats, images, jobs and
// the rest. Walking a state directory with years of images can take a while,
// so the walk runs once in the background and its result is kept for
// storageUsageTTL; a read made while it is fresh costs nothing, and a read made
// while it is stale waits a moment for the new walk and otherwise answers with
// the previous result. Sizes are the files' own lengths, as Finder's "Size"
// shows them; nothing is followed through a symlink.

const (
	storageUsageTTL  = 5 * time.Minute
	storageUsageWait = 3 * time.Second
)

const (
	storagePartChats  = "chats"
	storagePartImages = "images"
	storagePartJobs   = "jobs"
	storagePartOther  = "other"
)

var storagePartsInOrder = []string{storagePartChats, storagePartImages, storagePartJobs, storagePartOther}

// What each top-level entry of the state directory holds.
var storageTopLevelParts = map[string]string{
	// Chats: the chat store, its migration backups, the files chats kept
	// before it, transcripts, checkpoints, forks, and the provider session
	// records that resume a chat's native thread.
	"backups":                      storagePartChats,
	"provider-chats":               storagePartChats,
	"chat-archive":                 storagePartChats,
	"checkpoints":                  storagePartChats,
	"forks":                        storagePartChats,
	"subagent-receipts":            storagePartChats,
	"provider-native":              storagePartChats,
	"provider-lanes.json":          storagePartChats,
	"turn-diagnostics.json":        storagePartChats,
	"scheduled-messages.json":      storagePartChats,
	chat.ImportRefusalFileName:     storagePartChats,
	sessionImageDirname:            storagePartImages,
	chatImageThumbDirname:          storagePartImages,
	imageStoreDirname:              storagePartImages,
	"process-children":             storagePartJobs,
	"external-work":                storagePartJobs,
	"spawned-work":                 storagePartJobs,
	"spawned-work-receipts":        storagePartJobs,
	"fleet-logs":                   storagePartJobs,
	"fleet-monitors":               storagePartJobs,
	"run-credentials":              storagePartJobs,
	"hosted-process-children.json": storagePartJobs,
	"supervisor-bin":               storagePartJobs,
}

// storagePartOf names the part a top-level entry of the state directory
// belongs to. SQLite keeps -wal and -shm files beside a database, and a few
// files are written through a temporary sibling, so those go by prefix.
func storagePartOf(top string) string {
	if part, ok := storageTopLevelParts[top]; ok {
		return part
	}
	switch {
	case strings.HasPrefix(top, store.FileName), strings.HasPrefix(top, sessionStateFilename),
		strings.HasPrefix(top, "mock-acp-sessions-"):
		return storagePartChats
	case strings.HasPrefix(top, procchild.OutboxFileName):
		return storagePartJobs
	}
	return storagePartOther
}

type storageUsageSnapshot struct {
	ComputedAt time.Time
	Total      int64
	Parts      map[string]int64
}

// measureStorage walks dir once. An entry that vanishes or cannot be read
// while the walk runs is skipped; only an unreadable dir itself is an error.
func measureStorage(dir string, now time.Time) (storageUsageSnapshot, error) {
	out := storageUsageSnapshot{ComputedAt: now, Parts: map[string]int64{}}
	for _, part := range storagePartsInOrder {
		out.Parts[part] = 0
	}
	root := filepath.Clean(dir)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if path == root {
				return err
			}
			if entry != nil && entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if entry.IsDir() || !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		top := strings.SplitN(filepath.ToSlash(rel), "/", 2)[0]
		out.Parts[storagePartOf(top)] += info.Size()
		out.Total += info.Size()
		return nil
	})
	if err != nil {
		return storageUsageSnapshot{}, err
	}
	return out, nil
}

// storageUsageMeter keeps the newest measurement of one directory and makes
// sure no more than one walk of it runs at a time.
type storageUsageMeter struct {
	dir     string
	ttl     time.Duration
	now     func() time.Time
	measure func(string, time.Time) (storageUsageSnapshot, error)

	mu      sync.Mutex
	last    *storageUsageSnapshot
	lastErr error
	running chan struct{}
}

func newStorageUsageMeter(dir string) *storageUsageMeter {
	return &storageUsageMeter{dir: dir, ttl: storageUsageTTL, now: time.Now, measure: measureStorage}
}

// read returns the newest measurement. A missing or stale one starts a walk
// and waits up to wait for it; when the walk outlasts that, the previous
// measurement is returned, or ok=false when there has never been one.
func (m *storageUsageMeter) read(wait time.Duration) (snapshot storageUsageSnapshot, ok bool, err error) {
	if m == nil || strings.TrimSpace(m.dir) == "" {
		return storageUsageSnapshot{}, false, errors.New("this daemon has no state directory")
	}
	m.mu.Lock()
	fresh := m.last != nil && m.now().Sub(m.last.ComputedAt) < m.ttl
	if !fresh && m.running == nil {
		done := make(chan struct{})
		m.running = done
		go m.walk(done)
	}
	running := m.running
	m.mu.Unlock()
	if !fresh && running != nil && wait > 0 {
		timer := time.NewTimer(wait)
		select {
		case <-running:
		case <-timer.C:
		}
		timer.Stop()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.last != nil {
		return *m.last, true, nil
	}
	if m.lastErr != nil {
		return storageUsageSnapshot{}, false, m.lastErr
	}
	return storageUsageSnapshot{}, false, nil
}

func (m *storageUsageMeter) walk(done chan struct{}) {
	snapshot, err := m.measure(m.dir, m.now())
	m.mu.Lock()
	if err == nil {
		m.last, m.lastErr = &snapshot, nil
	} else {
		m.lastErr = err
	}
	m.running = nil
	m.mu.Unlock()
	close(done)
}

// storageUsageReply is the wire shape: {computedAt, totalBytes, parts:
// {chats, images, jobs, other}}, or {pending: true} while the first walk is
// still running.
func storageUsageReply(snapshot storageUsageSnapshot, ok bool) map[string]any {
	if !ok {
		return map[string]any{"pending": true}
	}
	parts := make(map[string]any, len(storagePartsInOrder))
	for _, part := range storagePartsInOrder {
		parts[part] = snapshot.Parts[part]
	}
	return map[string]any{
		"computedAt": snapshot.ComputedAt.UTC().Format(time.RFC3339),
		"totalBytes": snapshot.Total,
		"parts":      parts,
	}
}
