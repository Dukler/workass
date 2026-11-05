package changes

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// rootFinder maps a directory to the git worktree that contains it by looking
// for a ".git" entry (a directory, or the file a linked worktree has) in the
// directory and its parents. It only stats; it never starts git. A folder
// that is not in a repo maps to "" and is skipped.
type rootFinder struct {
	mu    sync.Mutex
	roots map[string]string
	canon map[string]string
}

const rootWalkLimit = 64

func newRootFinder() *rootFinder {
	return &rootFinder{roots: map[string]string{}, canon: map[string]string{}}
}

// canonicalDir resolves symlinks in dir (on macOS /tmp is /private/tmp) so a
// chat folder and an agent's edit path name the same repo. A directory that no
// longer exists resolves through its nearest existing parent.
func (f *rootFinder) canonicalDir(dir string) string {
	dir = filepath.Clean(dir)
	f.mu.Lock()
	if cached, ok := f.canon[dir]; ok {
		f.mu.Unlock()
		return cached
	}
	f.mu.Unlock()
	resolved := dir
	if eval, err := filepath.EvalSymlinks(dir); err == nil {
		resolved = eval
	} else if parent := filepath.Dir(dir); parent != dir {
		resolved = filepath.Join(f.canonicalDir(parent), filepath.Base(dir))
	}
	f.mu.Lock()
	f.canon[dir] = resolved
	f.mu.Unlock()
	return resolved
}

// CanonicalPath is canonicalDir for a file path.
func (f *rootFinder) canonicalPath(path string) string {
	path = filepath.Clean(path)
	return filepath.Join(f.canonicalDir(filepath.Dir(path)), filepath.Base(path))
}

// rootOf returns the canonical worktree root containing dir, or "".
func (f *rootFinder) rootOf(dir string) string {
	if strings.TrimSpace(dir) == "" {
		return ""
	}
	dir = f.canonicalDir(dir)
	f.mu.Lock()
	if cached, ok := f.roots[dir]; ok {
		f.mu.Unlock()
		return cached
	}
	f.mu.Unlock()
	root := ""
	walked := []string{}
	current := dir
	for step := 0; step < rootWalkLimit; step++ {
		f.mu.Lock()
		cached, known := f.roots[current]
		f.mu.Unlock()
		if known {
			root = cached
			break
		}
		walked = append(walked, current)
		if _, err := os.Lstat(filepath.Join(current, ".git")); err == nil {
			root = current
			break
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	f.mu.Lock()
	for _, path := range walked {
		f.roots[path] = root
	}
	f.mu.Unlock()
	return root
}

// forget drops what the finder learned, so a repo created or removed since is
// seen on the next walk.
func (f *rootFinder) forget() {
	f.mu.Lock()
	f.roots = map[string]string{}
	f.canon = map[string]string{}
	f.mu.Unlock()
}

// relInside returns path relative to root with forward slashes, or false when
// path is not inside root.
func relInside(root, path string) (string, bool) {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// pathKey is how a repo-relative path is compared: case-insensitively on
// Windows, exactly elsewhere.
func pathKey(rel string) string {
	if runtime.GOOS == "windows" {
		return strings.ToLower(rel)
	}
	return rel
}
