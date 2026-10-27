//go:build darwin

package main

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// A Finder-locked (uchg) source still clones, and the stored raw file keeps
// none of its lock: the store can always remove its own files.
func TestSessionImageCloneOfALockedSourceIsAnOrdinaryStoreFile(t *testing.T) {
	home := t.TempDir()
	withSourceGuard(t, sessionImageSourceGuard{home: home, caseInsensitive: true})
	dir := filepath.Join(t.TempDir(), sessionImageDirname)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw := testImageBytes(t, 20)
	name := testImageDigest(raw)
	source := filepath.Join(home, "locked.png")
	if err := os.WriteFile(source, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := unix.Chflags(source, unix.UF_IMMUTABLE); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Chflags(source, 0) })
	result, err := persistRawSessionImage(dir, name, raw, []string{source}, sessionImageDurable)
	if err != nil || result.method != "cloned" {
		t.Fatalf("%+v %v", result, err)
	}
	final := filepath.Join(dir, name+sessionImageRawSuffix)
	var stat unix.Stat_t
	if err := unix.Lstat(final, &stat); err != nil {
		t.Fatal(err)
	}
	if stat.Flags&sessionImageLockFlags != 0 {
		t.Fatalf("stored raw file kept the source's lock flags %#x", stat.Flags)
	}
	if err := os.Remove(final); err != nil {
		t.Fatalf("store file not removable: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("left behind: %v", entries)
	}
}
