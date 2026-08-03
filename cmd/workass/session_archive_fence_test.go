package main

import (
	"path/filepath"
	"testing"
	"time"
)

func TestSessionGetSkipsInactiveArchiveAndBoundsActiveFence(t *testing.T) {
	const expectedFence = 100 * time.Millisecond
	store := newSessionStore(filepath.Join(t.TempDir(), sessionStateFilename))
	if !store.Save(sessionMirrorFixture("active-fence-tab", "active-fence-chat", "active")) {
		t.Fatal("seed save")
	}

	inactiveJob := &sessionJob{ID: "inactive-archive-job", TabID: "inactive-fence-tab"}
	store.mu.Lock()
	inactiveDone := store.beginArchiveLocked(inactiveJob)
	store.mu.Unlock()
	inactiveGet := make(chan any, 1)
	inactiveStarted := time.Now()
	go func() { inactiveGet <- store.Get() }()
	select {
	case got := <-inactiveGet:
		if got == nil {
			t.Fatal("inactive archive fence blanked session:get")
		}
		if elapsed := time.Since(inactiveStarted); elapsed >= expectedFence {
			t.Fatalf("inactive archive delayed session:get for %s", elapsed)
		}
	case <-time.After(300 * time.Millisecond):
		store.mu.Lock()
		store.finishArchiveLocked(inactiveJob, inactiveDone)
		store.mu.Unlock()
		t.Fatal("session:get waited on an inactive chat archive")
	}
	store.mu.Lock()
	store.finishArchiveLocked(inactiveJob, inactiveDone)
	store.mu.Unlock()

	activeJob := &sessionJob{ID: "active-archive-job", TabID: "active-fence-tab"}
	store.mu.Lock()
	activeDone := store.beginArchiveLocked(activeJob)
	store.mu.Unlock()
	activeStarted := time.Now()
	got := store.Get()
	elapsed := time.Since(activeStarted)
	store.mu.Lock()
	store.finishArchiveLocked(activeJob, activeDone)
	store.mu.Unlock()
	if got == nil {
		t.Fatal("bounded active archive fence blanked session:get")
	}
	if elapsed < expectedFence/2 || elapsed >= 300*time.Millisecond {
		t.Fatalf("active archive fence elapsed=%s, want one bounded %s budget", elapsed, expectedFence)
	}
}
