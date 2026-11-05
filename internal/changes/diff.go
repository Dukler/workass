package changes

import (
	"bytes"
	"context"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// Bounds on one file's diff: the pane shows it in a small scroll area.
const (
	diffByteLimit = 256 << 10
	diffLineLimit = 4000
)

// Diff is one file's change against HEAD as unified diff text.
type Diff struct {
	Root      string `json:"root"`
	Path      string `json:"path"`
	Text      string `json:"text"`
	Truncated bool   `json:"truncated,omitempty"`
	New       bool   `json:"new,omitempty"`
	Binary    bool   `json:"binary,omitempty"`
}

// Diff returns one changed file's diff. It answers only while a client
// watches, and only for a file the last check reported in a repo the service
// knows, so it cannot be used to read arbitrary files. An untracked file is
// read directly; a tracked one costs one git process, under the same slots
// and timeout as a check.
func (s *Service) Diff(ctx context.Context, root, rel string) (Diff, error) {
	root = filepath.Clean(strings.TrimSpace(root))
	rel = strings.TrimSpace(rel)
	if rel == "" || path.IsAbs(rel) || strings.HasPrefix(path.Clean(rel), "../") || path.Clean(rel) == ".." {
		return Diff{}, ErrUnknownFile
	}
	s.mu.Lock()
	if len(s.watchers) == 0 {
		s.mu.Unlock()
		return Diff{}, ErrNotWatching
	}
	repo := s.repos[root]
	var file *fileStatus
	initial, slow := false, false
	if repo != nil && repo.status != nil {
		initial, slow = repo.status.initial, repo.slow
		for index := range repo.status.files {
			if repo.status.files[index].path == rel {
				copied := repo.status.files[index]
				file = &copied
				break
			}
		}
	}
	runCtx := s.runCtx
	s.mu.Unlock()
	if file == nil || runCtx == nil {
		return Diff{}, ErrUnknownFile
	}
	result := Diff{Root: root, Path: rel, New: file.untracked || strings.HasPrefix(file.code, "A"), Binary: file.binary}
	if file.untracked {
		text, truncated, binary := untrackedDiff(filepath.Join(root, filepath.FromSlash(rel)), rel)
		result.Text, result.Truncated, result.Binary = s.opts.Redact(text), truncated, binary
		return result, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	merged, cancel := context.WithCancel(runCtx)
	defer cancel()
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	release, err := s.acquire(merged, slow)
	if err != nil {
		return Diff{}, err
	}
	args := []string{"diff", "--no-color", "--no-ext-diff", "--no-renames"}
	if initial {
		args = append(args, "--cached")
	} else {
		args = append(args, "HEAD")
	}
	args = append(args, "--", rel)
	out, err := s.git(merged, root, args...)
	release()
	if err != nil {
		return Diff{}, err
	}
	text, truncated := boundDiff(out)
	result.Text, result.Truncated = s.opts.Redact(text), truncated
	return result, nil
}

func boundDiff(out []byte) (string, bool) {
	truncated := false
	if len(out) > diffByteLimit {
		out = out[:diffByteLimit]
		if cut := bytes.LastIndexByte(out, '\n'); cut > 0 {
			out = out[:cut+1]
		}
		truncated = true
	}
	if lines := bytes.Count(out, []byte{'\n'}); lines > diffLineLimit {
		index := 0
		for seen := 0; seen < diffLineLimit; seen++ {
			next := bytes.IndexByte(out[index:], '\n')
			if next < 0 {
				break
			}
			index += next + 1
		}
		out = out[:index]
		truncated = true
	}
	return string(out), truncated
}

// untrackedDiff renders a new file as an all-added diff without git.
func untrackedDiff(abs, rel string) (string, bool, bool) {
	handle, err := os.Open(abs)
	if err != nil {
		return "", false, false
	}
	defer handle.Close()
	data, err := io.ReadAll(io.LimitReader(handle, diffByteLimit+1))
	if err != nil {
		return "", false, false
	}
	truncated := len(data) > diffByteLimit
	if truncated {
		data = data[:diffByteLimit]
	}
	probe := data
	if len(probe) > 8000 {
		probe = probe[:8000]
	}
	if bytes.IndexByte(probe, 0) >= 0 {
		return "", false, true
	}
	text := strings.TrimSuffix(string(data), "\n")
	lines := []string{}
	if text != "" {
		lines = strings.Split(text, "\n")
	}
	if len(lines) > diffLineLimit {
		lines = lines[:diffLineLimit]
		truncated = true
	}
	var b strings.Builder
	b.WriteString("--- /dev/null\n+++ b/" + rel + "\n")
	b.WriteString("@@ -0,0 +1," + strconv.Itoa(len(lines)) + " @@\n")
	for _, line := range lines {
		b.WriteString("+" + line + "\n")
	}
	return b.String(), truncated, false
}
