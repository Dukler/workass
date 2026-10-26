package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"workass/internal/acp"
)

// Clone sources. When a stored image is byte-for-byte a file that is still on
// disk (the file an agent's "View image" call read, an attachment's absolute
// source, an agent attachment's path), the raw form is created as an APFS
// clone of it: the two share blocks, and the clone survives the original being
// changed or deleted. A source is only a candidate: it is cloned only after
// its bytes are proven identical, and the clone is verified again before it
// is used.
//
// Reading a candidate must never make macOS ask the user for access. A path
// in a TCC-protected location (Desktop, Documents, Downloads, Pictures,
// Movies, Music, the Trash, anything under ~/Library, another user's home), on
// an external or network volume, or reached through a symlink into one of
// those, is refused from its path alone, before any file system call touches
// it. A refused image is stored by writing the bytes already in hand.

// sessionImageSourceField carries clone-source hints from capture to the
// store. It never reaches the durable record or the wire: staging removes it.
const sessionImageSourceField = acp.ImageCloneSourceField

// sessionImageCloneOutcome is why a candidate was or was not cloned.
type sessionImageCloneOutcome int

const (
	cloneOK sessionImageCloneOutcome = iota
	// cloneUnsupported: this platform or volume cannot clone; no read happened.
	cloneUnsupported
	// cloneProtected: refused by the classifier; no read happened.
	cloneProtected
	// cloneUnavailable: missing, not a regular file, or unreadable.
	cloneUnavailable
	// cloneDifferent: the file is not this image (size or bytes differ).
	cloneDifferent
	// cloneOtherVolume: the file is on another volume, so it cannot be cloned.
	cloneOtherVolume
	// cloneFailed: clonefile or the post-clone verification failed.
	cloneFailed
)

var errSessionImageSourceProtected = errors.New("clone source is in a protected location")

// sessionImageSourceGuard classifies clone-source paths. It is pure: home and
// the extra mount points are inputs, so it is tested on any platform.
type sessionImageSourceGuard struct {
	home string
	// caseInsensitive: APFS and HFS+ compare names without case by default,
	// so ~/desktop is ~/Desktop.
	caseInsensitive bool
	// mounts are the mount points of network and other non-local volumes.
	mounts []string
}

// sessionImageProtectedHomeDirs are refused below the user's home. ~/Library
// as a whole is refused: Mobile Documents (iCloud), Containers and Group
// Containers are what prompt today, and the rest holds other apps' data that a
// later macOS may protect too. Losing a clone there costs bytes, never a prompt.
var sessionImageProtectedHomeDirs = []string{
	"Desktop", "Documents", "Downloads", "Pictures", "Movies", "Music", "Library", ".Trash",
}

// sessionImageProtectedRoots are refused anywhere on the machine: external and
// network volumes and the automounter's roots.
var sessionImageProtectedRoots = []string{"/Volumes", "/net", "/Network", "/automount"}

// darwinDataVolumePrefix is the firmlinked path of the data volume:
// /System/Volumes/Data/Users/me/Desktop is ~/Desktop.
const darwinDataVolumePrefix = "/System/Volumes/Data"

// fold maps a path to the form a case-insensitive volume compares: every
// rune to one member of its Unicode simple case-folding orbit, the letters
// strings.EqualFold treats as one (APFS opens ~/Deſktop, long s, as
// ~/Desktop; the Kelvin sign is a k). strings.ToLower is not enough.
func (g sessionImageSourceGuard) fold(path string) string {
	if !g.caseInsensitive {
		return path
	}
	var out strings.Builder
	out.Grow(len(path))
	for _, r := range path {
		out.WriteRune(foldSessionImageRune(r))
	}
	return out.String()
}

func foldSessionImageRune(r rune) rune {
	smallest := r
	for other := unicode.SimpleFold(r); other != r; other = unicode.SimpleFold(other) {
		if other < smallest {
			smallest = other
		}
	}
	if smallest < utf8.RuneSelf {
		return unicode.ToLower(smallest)
	}
	return smallest
}

// asciiAliasRune reports a non-ASCII rune that a case-insensitive or
// normalization-insensitive volume (APFS is both) can treat as an ASCII
// character: a case-fold partner of one (long s, Kelvin sign) or a
// canonical singleton decomposition to one (Greek question mark, varia).
// Every protected name is ASCII, so a path holding one is refused rather
// than trusted to compare correctly.
func asciiAliasRune(r rune) bool {
	if r < utf8.RuneSelf {
		return false
	}
	switch r {
	case 0x037E, 0x1FEF, 0x212A:
		return true
	}
	return foldSessionImageRune(r) < utf8.RuneSelf
}

func pathWithin(path, root string) bool {
	if root == "" {
		return false
	}
	if root == "/" {
		return true
	}
	return path == root || strings.HasPrefix(path, root+"/")
}

// check reports whether path may be read as a clone source, from the string
// alone. A nil error means allowed; it never touches the file system.
func (g sessionImageSourceGuard) check(path string) error {
	if path == "" || strings.ContainsRune(path, 0) {
		return fmt.Errorf("%w: empty or invalid path", errSessionImageSourceProtected)
	}
	path = filepath.ToSlash(path)
	if !strings.HasPrefix(path, "/") {
		return fmt.Errorf("%w: not an absolute path", errSessionImageSourceProtected)
	}
	if !utf8.ValidString(path) || strings.IndexFunc(path, asciiAliasRune) >= 0 {
		return fmt.Errorf("%w: a character the volume may read as another", errSessionImageSourceProtected)
	}
	clean := g.fold(filepath.ToSlash(filepath.Clean(path)))
	candidates := []string{clean}
	if prefix := g.fold(darwinDataVolumePrefix); pathWithin(clean, prefix) {
		rest := strings.TrimPrefix(clean, prefix)
		if rest == "" {
			rest = "/"
		}
		candidates = append(candidates, rest)
	}
	home := g.fold(filepath.ToSlash(filepath.Clean(g.home)))
	if g.home == "" || home == "/" || home == "." {
		// Without a known home nothing below it can be classified.
		return fmt.Errorf("%w: home directory unknown", errSessionImageSourceProtected)
	}
	for _, candidate := range candidates {
		for _, dir := range sessionImageProtectedHomeDirs {
			if pathWithin(candidate, home+"/"+g.fold(dir)) {
				return fmt.Errorf("%w: under ~/%s", errSessionImageSourceProtected, dir)
			}
		}
		for _, root := range sessionImageProtectedRoots {
			if pathWithin(candidate, g.fold(root)) {
				return fmt.Errorf("%w: under %s", errSessionImageSourceProtected, root)
			}
		}
		// Another user's home is theirs, whatever its folders are called.
		if users := g.fold("/Users"); pathWithin(candidate, users) && candidate != users && !pathWithin(candidate, home) {
			return fmt.Errorf("%w: another user's home", errSessionImageSourceProtected)
		}
		for _, mount := range g.mounts {
			mount = g.fold(filepath.ToSlash(filepath.Clean(mount)))
			if mount != "/" && pathWithin(candidate, mount) {
				return fmt.Errorf("%w: on a non-local volume (%s)", errSessionImageSourceProtected, mount)
			}
		}
	}
	return nil
}

// resolve follows path to the file it names one component at a time,
// classifying every intermediate path before it is looked at, so a symlink
// into a protected location is refused before anything inside it is touched.
func (g sessionImageSourceGuard) resolve(path string, lstat func(string) (os.FileInfo, error), readlink func(string) (string, error)) (string, error) {
	if err := g.check(path); err != nil {
		return "", err
	}
	remaining := splitSessionImagePath(filepath.Clean(path))
	resolved := "/"
	for hops := 0; len(remaining) > 0; {
		next := filepath.Join(resolved, remaining[0])
		remaining = remaining[1:]
		if err := g.check(next); err != nil {
			return "", err
		}
		info, err := lstat(next)
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			resolved = next
			continue
		}
		if hops++; hops > 16 {
			return "", errors.New("clone source has too many symlinks")
		}
		target, err := readlink(next)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(resolved, target)
		}
		target = filepath.Clean(target)
		if err := g.check(target); err != nil {
			return "", err
		}
		remaining = append(splitSessionImagePath(target), remaining...)
		resolved = "/"
	}
	if err := g.check(resolved); err != nil {
		return "", err
	}
	return resolved, nil
}

func splitSessionImagePath(path string) []string {
	parts := strings.Split(filepath.ToSlash(path), "/")
	out := parts[:0]
	for _, part := range parts {
		if part != "" && part != "." {
			out = append(out, part)
		}
	}
	return out
}

// sessionImageSources returns the guard for this machine. Tests replace it.
var sessionImageSources = currentSessionImageSourceGuard

var sessionImageSourceGuardCache struct {
	mu    sync.Mutex
	guard sessionImageSourceGuard
	at    time.Time
}

// currentSessionImageSourceGuard builds the guard from the user's home and
// the current non-local mounts, refreshed at most once a minute.
func currentSessionImageSourceGuard() sessionImageSourceGuard {
	cache := &sessionImageSourceGuardCache
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if !cache.at.IsZero() && time.Since(cache.at) < time.Minute {
		return cache.guard
	}
	home, _ := os.UserHomeDir()
	cache.guard = sessionImageSourceGuard{
		home:            home,
		caseInsensitive: runtime.GOOS == "darwin" || runtime.GOOS == "windows",
		mounts:          nonLocalMountPoints(),
	}
	cache.at = time.Now()
	return cache.guard
}

// sessionImageCloneSources collects the candidate paths a staged image item
// carries: its absolute "source" and any capture-time hints. The hint field is
// removed from the item.
func sessionImageCloneSources(item map[string]any) []string {
	var sources []string
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" || !filepath.IsAbs(value) {
			return
		}
		for _, existing := range sources {
			if existing == value {
				return
			}
		}
		if len(sources) < 8 {
			sources = append(sources, value)
		}
	}
	switch hint := item[sessionImageSourceField].(type) {
	case string:
		add(hint)
	case []any:
		for _, value := range hint {
			if text, ok := value.(string); ok {
				add(text)
			}
		}
	case []string:
		for _, value := range hint {
			add(value)
		}
	}
	delete(item, sessionImageSourceField)
	if source, ok := item["source"].(string); ok {
		add(source)
	}
	return sources
}
