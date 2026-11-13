package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionImageSizeIndexOutlivesTheProcess(t *testing.T) {
	stateDir := t.TempDir()
	name := strings.Repeat("ab", 32)
	other := strings.Repeat("cd", 32)
	first := &sessionImageSizeIndex{loaded: map[string]bool{}, sizes: map[[32]byte]sessionImageSize{}, pending: map[string][]byte{}}
	if _, ok := first.lookup(stateDir, name); ok {
		t.Fatal("an empty index knew a size")
	}
	first.remember(stateDir, name, sessionImageSize{width: 1280, height: 720})
	first.remember(stateDir, other, sessionImageSize{}) // unreadable: not kept
	first.remember(stateDir, "not-a-digest", sessionImageSize{width: 1, height: 1})
	first.flush(stateDir)
	// A cut record (a write stopped short) is ignored and realigned.
	path := filepath.Join(stateDir, sessionImageSizeIndexFile)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	file.Close()
	first.remember(stateDir, other, sessionImageSize{width: 3, height: 4})
	first.flush(stateDir)

	next := &sessionImageSizeIndex{loaded: map[string]bool{}, sizes: map[[32]byte]sessionImageSize{}, pending: map[string][]byte{}}
	if size, ok := next.lookup(stateDir, name); !ok || size.width != 1280 || size.height != 720 {
		t.Fatalf("size %+v %v", size, ok)
	}
	if size, ok := next.lookup(stateDir, other); !ok || size.width != 3 || size.height != 4 {
		t.Fatalf("size after a cut record %+v %v", size, ok)
	}
	if info, err := os.Stat(path); err != nil || info.Size() != 2*sessionImageSizeIndexRecord {
		t.Fatalf("index is %d bytes (%v)", info.Size(), err)
	}
}
