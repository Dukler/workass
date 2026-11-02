package acp

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// The kernel scan against processes whose holdings the test knows, not against lsof,
// which can't be relied on to answer on a busy machine.
func TestDarwinScanFindsWhoHoldsAFile(t *testing.T) {
	t.Parallel()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	heldPath := filepath.Join(dir, "held.output")
	idlePath := filepath.Join(dir, "idle.output")
	linkPath := filepath.Join(dir, "link.output")
	missingPath := filepath.Join(dir, "missing.output")
	held, err := os.Create(heldPath)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if err := os.WriteFile(idlePath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(heldPath, linkPath); err != nil {
		t.Fatal(err)
	}
	child := exec.Command("/bin/sh", "-c", "echo up; read _")
	child.Stdout = held
	stdin, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = child.Process.Kill()
		_ = child.Wait()
	})
	// "up" in the file means the child's stdout is the file. Its own handle in this
	// process is what the parent side of the test holds, and it is held too.
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if info, err := os.Stat(heldPath); err == nil && info.Size() > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the child never wrote to the file")
		}
	}

	result, ok := darwinPIDsHoldingFiles([]string{heldPath, idlePath, linkPath, missingPath})
	if !ok {
		t.Fatal("the scan could not list processes")
	}
	for _, path := range []string{heldPath, linkPath} {
		if !slices.Contains(result[path], child.Process.Pid) || !slices.Contains(result[path], os.Getpid()) {
			t.Fatalf("holders of %s = %v, want child %d and this test %d", path, result[path], child.Process.Pid, os.Getpid())
		}
	}
	if !slices.IsSorted(result[heldPath]) {
		t.Fatalf("holders are not sorted: %v", result[heldPath])
	}
	for _, path := range []string{idlePath, missingPath} {
		holders, present := result[path]
		if !present || len(holders) != 0 {
			t.Fatalf("holders of %s = %v present=%v, want an empty answer", path, holders, present)
		}
	}

	// A renamed file keeps its holder through its new name.
	renamed := filepath.Join(dir, "renamed.output")
	if err := os.Rename(heldPath, renamed); err != nil {
		t.Fatal(err)
	}
	if after, ok := darwinPIDsHoldingFiles([]string{renamed}); !ok || !slices.Contains(after[renamed], child.Process.Pid) {
		t.Fatalf("holders after a rename = %v ok=%v", after[renamed], ok)
	}

	// Once it has gone, so has its claim on the file.
	_ = child.Process.Kill()
	_ = child.Wait()
	if after, ok := darwinPIDsHoldingFiles([]string{renamed}); !ok || slices.Contains(after[renamed], child.Process.Pid) {
		t.Fatalf("a dead process still holds the file: %v ok=%v", after[renamed], ok)
	}
}

func TestDarwinScanWithNoPathsAndNoFilesLooksAtNothing(t *testing.T) {
	t.Parallel()
	if result, ok := darwinPIDsHoldingFiles(nil); !ok || len(result) != 0 {
		t.Fatalf("no paths = %v ok=%v", result, ok)
	}
	missing := filepath.Join(t.TempDir(), "missing")
	if result, ok := darwinPIDsHoldingFiles([]string{missing}); !ok || len(result[missing]) != 0 {
		t.Fatalf("missing file = %v ok=%v", result, ok)
	}
}
