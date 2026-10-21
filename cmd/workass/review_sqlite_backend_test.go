//go:build review

// Independent-review reproduction (b8b18322..fe7003e5). Run with:
//   go test -tags review -count=1 -run TestReview ./cmd/workass/

package main

// Independent-review reproduction (fe7003e5): a byte-identical provider-chats
// JSON whose mtime moved after the cutover makes the daemon refuse to start.

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"workass/internal/chat"
)

// The macOS updater snapshots the state dir with fs.cpSync(preserveTimestamps)
// and, on rollback, renames that copy into place. The copy's mtimes differ in
// the sub-microsecond digits, so the restored (SQLite-live) state is refused.
// Set WORKASS_REVIEW_NODE to the updater's node binary.
func TestReviewUpdaterSnapshotCopyIsRefusedOnRollback(t *testing.T) {
	node := os.Getenv("WORKASS_REVIEW_NODE")
	if node == "" {
		t.Skip("set WORKASS_REVIEW_NODE")
	}
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	writeBackendTestActor(t, stateDir, "rollback-chat")
	backend, err := openChatActorBackend(stateDir, true, "test", t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.close(); err != nil {
		t.Fatal(err)
	}
	// Sanity: the original state dir boots again.
	backend, err = openChatActorBackend(stateDir, true, "test", t.Logf)
	if err != nil {
		t.Fatalf("original state dir no longer boots: %v", err)
	}
	if err := backend.close(); err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(root, "snapshot")
	script := `const fs=require("fs");fs.cpSync(process.argv[1],process.argv[2],{recursive:true,errorOnExist:true,force:false,dereference:false,preserveTimestamps:true,verbatimSymlinks:true,mode:fs.constants.COPYFILE_FICLONE});`
	if out, err := exec.Command(node, "-e", script, stateDir, snapshot).CombinedOutput(); err != nil {
		t.Fatalf("cpSync: %v %s", err, out)
	}
	// Rollback: the failed state moves aside and the snapshot copy takes its place.
	if err := os.Rename(stateDir, filepath.Join(root, "failed")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(snapshot, stateDir); err != nil {
		t.Fatal(err)
	}
	_, err = openChatActorBackend(stateDir, true, "test", t.Logf)
	if !errors.Is(err, chat.ErrFileActorsChangedAfterCutover) {
		t.Fatalf("expected the restored snapshot to be refused, got %v", err)
	}
	t.Logf("REPRODUCED: updater snapshot restored by rollback is refused: %v", err)
}

func TestReviewDaemonRefusesStartAfterJSONTouch(t *testing.T) {
	stateDir := t.TempDir()
	writeBackendTestActor(t, stateDir, "touch-chat")
	backend, err := openChatActorBackend(stateDir, true, "test", t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.close(); err != nil {
		t.Fatal(err)
	}
	path := providerChatStatePath(stateDir, "touch-chat")
	before, _ := os.ReadFile(path)
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("bytes changed")
	}
	_, err = openChatActorBackend(stateDir, true, "test", t.Logf)
	if !errors.Is(err, chat.ErrFileActorsChangedAfterCutover) {
		t.Fatalf("expected refusal, got %v", err)
	}
	t.Logf("REPRODUCED: main.go os.Exit(1) path, openChatActorBackend: %v", err)
}
