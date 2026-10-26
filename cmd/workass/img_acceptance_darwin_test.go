//go:build darwin

package main

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// cloneGuardHome builds a fake home on the store's volume whose Desktop holds
// an image, and points the source guard at it.
func cloneGuardHome(t *testing.T) (home, storeDir string, raw []byte, name string) {
	t.Helper()
	root := t.TempDir()
	home = filepath.Join(root, "home")
	storeDir = filepath.Join(root, "state", sessionImageDirname)
	must(t, os.MkdirAll(filepath.Join(home, "Desktop"), 0o700))
	must(t, os.MkdirAll(storeDir, 0o700))
	raw = testPNG(t, 50, 40)
	name = sessionImageTextDigest([]byte(base64.StdEncoding.EncodeToString(raw)))
	must(t, os.WriteFile(filepath.Join(home, "Desktop", "shot.png"), raw, 0o600))
	withSourceGuard(t, sessionImageSourceGuard{home: home, caseInsensitive: true})
	return home, storeDir, raw, name
}

// APFS folds U+017F LATIN SMALL LETTER LONG S to "s" in a case-insensitive
// lookup, so ~/Deſktop/shot.png opens ~/Desktop/shot.png. The guard's
// strings.ToLower does not fold it, so the clone reads the protected file.
func TestAcceptCloneRefusesDesktopSpelledWithLongS(t *testing.T) {
	home, storeDir, raw, name := cloneGuardHome(t)
	alias := filepath.Join(home, "Deſktop", "shot.png")
	if _, err := os.Stat(alias); err != nil {
		t.Skipf("this volume does not fold long s (%v)", err)
	}
	outcome, err := cloneRawSessionImage(storeDir, filepath.Join(storeDir, name+sessionImageRawSuffix), name, raw, alias, sessionImageDurable)
	if outcome != cloneProtected {
		t.Fatalf("clone of %q = outcome %d (%v); want cloneProtected. The file it cloned is ~/Desktop/shot.png", alias, outcome, err)
	}
}

// Between the guard's resolution and the open/clonefile, an intermediate
// directory of the resolved path can become a symlink into a protected place.
// O_NOFOLLOW and CLONE_NOFOLLOW only cover the last component.
func TestAcceptCloneTOCTOUIntermediateSymlinkSwap(t *testing.T) {
	home, storeDir, raw, name := cloneGuardHome(t)
	work := filepath.Join(home, "work")
	// A real directory whose shot.png has the right size but other bytes, so
	// it can never be cloned: a cloneOK proves the bytes came from Desktop.
	realDir := filepath.Join(work, "real")
	must(t, os.MkdirAll(realDir, 0o700))
	decoy := append([]byte(nil), raw...)
	decoy[len(decoy)-1] ^= 0xff
	must(t, os.WriteFile(filepath.Join(realDir, "shot.png"), decoy, 0o600))
	linkSpare := filepath.Join(work, "spare-link")
	must(t, os.Symlink(filepath.Join(home, "Desktop"), linkSpare))
	dirSpare := filepath.Join(work, "spare-dir")
	live := filepath.Join(work, "live")
	must(t, os.Rename(realDir, live))
	candidate := filepath.Join(live, "shot.png")

	var stop atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		for !stop.Load() {
			// live: directory -> symlink to Desktop -> directory.
			_ = os.Rename(live, dirSpare)
			_ = os.Rename(linkSpare, live)
			time.Sleep(time.Microsecond)
			_ = os.Rename(live, linkSpare)
			_ = os.Rename(dirSpare, live)
		}
	}()
	deadline := time.Now().Add(8 * time.Second)
	attempts, protected := 0, 0
	for time.Now().Before(deadline) {
		attempts++
		final := filepath.Join(storeDir, name+sessionImageRawSuffix)
		outcome, _ := cloneRawSessionImage(storeDir, final, name, raw, candidate, sessionImageBatched)
		if outcome == cloneProtected {
			protected++
		}
		if outcome == cloneOK {
			stop.Store(true)
			<-done
			t.Fatalf("attempt %d: cloned the Desktop file through %s (the guard saw a real directory; open/clonefile followed the swapped-in symlink)", attempts, candidate)
		}
	}
	stop.Store(true)
	<-done
	t.Logf("%d attempts, %d refused as protected, none read Desktop", attempts, protected)
}

// The cost of storing one new chat image durably, this build (raw + legacy
// shadow during the window, raw only after it) against 0.1.229 (legacy only).
func TestAcceptNewImageDurableWriteCost(t *testing.T) {
	if testing.Short() {
		t.Skip("timing")
	}
	const count = 40
	images := make([][]byte, count*3)
	for i := range images {
		images[i] = testImageBytes(t, 4000+i)
	}
	measure := func(label string, write func(stateDir string, raw []byte)) time.Duration {
		stateDir := t.TempDir()
		started := time.Now()
		for i := 0; i < count; i++ {
			write(stateDir, images[i])
		}
		elapsed := time.Since(started)
		t.Logf("%-34s %8s per image", label, (elapsed / count).Round(10*time.Microsecond))
		return elapsed
	}
	old := measure("0.1.229 (legacy only)", func(stateDir string, raw []byte) {
		text := base64.StdEncoding.EncodeToString(raw)
		plan := sessionImageWritePlan{ref: sessionImageDirname + "/" + sessionImageName(text), dir: filepath.Join(stateDir, sessionImageDirname),
			path: filepath.Join(stateDir, sessionImageDirname, sessionImageName(text)), data: text}
		must(t, persistLegacySessionImagePlan(plan))
	})
	images = images[count:]
	withLegacyShadow(t, true)
	window := measure("this build, window open", func(stateDir string, raw []byte) { persistThroughStaging(t, stateDir, raw) })
	images = images[count:]
	withLegacyShadow(t, false)
	after := measure("this build, after the window", func(stateDir string, raw []byte) { persistThroughStaging(t, stateDir, raw) })
	t.Logf("window/0.1.229 = %.2fx, after/0.1.229 = %.2fx", float64(window)/float64(old), float64(after)/float64(old))
}

// The same window with a FIFO: the candidate passes the Lstat as a regular
// file of the right size, then a FIFO is in its place for the open, which
// blocks until a writer appears. The writer holds sessionImageStoreMu shared
// meanwhile, so the collector's Lock waits on it and every later writer waits
// on the collector.
func TestAcceptCloneCandidateSwappedForFIFOHangsTheWriter(t *testing.T) {
	home, storeDir, raw, name := cloneGuardHome(t)
	work := filepath.Join(home, "work")
	must(t, os.MkdirAll(work, 0o700))
	live := filepath.Join(work, "shot.png")
	fileSpare := filepath.Join(work, "file-spare")
	fifoSpare := filepath.Join(work, "fifo-spare")
	must(t, os.WriteFile(fileSpare, raw, 0o600))
	must(t, unixMkfifo(fifoSpare))
	var stop atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		for !stop.Load() {
			_ = os.Rename(fileSpare, live)
			_ = os.Rename(live, fileSpare)
			_ = os.Rename(fifoSpare, live)
			time.Sleep(time.Microsecond)
			_ = os.Rename(live, fifoSpare)
		}
	}()
	release := func() {
		for _, path := range []string{live, fifoSpare} {
			if f, err := os.OpenFile(path, os.O_WRONLY|unixONonblock, 0); err == nil {
				_ = f.Close()
			}
		}
	}
	deadline := time.Now().Add(8 * time.Second)
	for attempts := 1; time.Now().Before(deadline); attempts++ {
		returned := make(chan struct{})
		go func() {
			defer close(returned)
			_, _ = cloneRawSessionImage(storeDir, filepath.Join(storeDir, name+sessionImageRawSuffix), name, raw, live, sessionImageBatched)
		}()
		select {
		case <-returned:
			continue
		case <-time.After(2 * time.Second):
			stop.Store(true)
			<-done
			for i := 0; i < 50; i++ {
				release()
				select {
				case <-returned:
					t.Fatalf("attempt %d: cloneRawSessionImage blocked on a FIFO for 2s (released by opening it for writing)", attempts)
				case <-time.After(20 * time.Millisecond):
				}
			}
			t.Fatalf("attempt %d: cloneRawSessionImage blocked on a FIFO and could not be released", attempts)
		}
	}
	stop.Store(true)
	<-done
}
