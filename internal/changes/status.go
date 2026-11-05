package changes

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Limits on what one repo check keeps. A worktree with thousands of untracked
// files (an unignored build directory) still answers in one bounded payload.
const (
	repoFileLimit       = 300
	statusEntryLimit    = 4000
	untrackedCountLimit = 200
	untrackedReadLimit  = 512 << 10
)

// repoStatus is one repo's state from `git status --porcelain=v2 --branch -z`
// and `git diff --numstat HEAD`.
type repoStatus struct {
	branch   string
	detached bool
	upstream string
	// abKnown is false when the branch has no upstream or git could not
	// compare against it; ahead and behind are then unknown, not zero.
	abKnown        bool
	ahead          int
	behind         int
	initial        bool
	files          []fileStatus
	filesTruncated bool
}

type fileStatus struct {
	path      string
	orig      string
	code      string
	untracked bool
	adds      int
	dels      int
	binary    bool
}

func (s repoStatus) totals() (adds, dels int) {
	for _, file := range s.files {
		adds += file.adds
		dels += file.dels
	}
	return adds, dels
}

// parseStatusV2 reads `git status --porcelain=v2 --branch -z` output. Paths
// are repo-relative with forward slashes, exactly as git prints them under -z
// (no quoting).
func parseStatusV2(out []byte) repoStatus {
	var status repoStatus
	tokens := bytes.Split(bytes.TrimRight(out, "\x00"), []byte{0})
	entries := 0
	for index := 0; index < len(tokens); index++ {
		record := string(tokens[index])
		if record == "" {
			continue
		}
		switch record[0] {
		case '#':
			status.parseHeader(record)
			continue
		case '!':
			continue
		}
		entries++
		if entries > statusEntryLimit {
			status.filesTruncated = true
			break
		}
		switch record[0] {
		case '1':
			// 1 XY sub mH mI mW hH hI path
			fields := strings.SplitN(record, " ", 9)
			if len(fields) == 9 {
				status.files = append(status.files, fileStatus{code: fields[1], path: fields[8]})
			}
		case '2':
			// 2 XY sub mH mI mW hH hI Xscore path NUL origPath
			fields := strings.SplitN(record, " ", 10)
			file := fileStatus{}
			if len(fields) == 10 {
				file = fileStatus{code: fields[1], path: fields[9]}
			}
			if index+1 < len(tokens) {
				index++
				file.orig = string(tokens[index])
			}
			if file.path != "" {
				status.files = append(status.files, file)
			}
		case 'u':
			// u XY sub m1 m2 m3 mW h1 h2 h3 path
			fields := strings.SplitN(record, " ", 11)
			if len(fields) == 11 {
				status.files = append(status.files, fileStatus{code: fields[1], path: fields[10]})
			}
		case '?':
			if path := strings.TrimPrefix(record, "? "); path != "" && path != record {
				status.files = append(status.files, fileStatus{code: "??", path: path, untracked: true})
			}
		}
	}
	return status
}

func (s *repoStatus) parseHeader(record string) {
	switch {
	case strings.HasPrefix(record, "# branch.oid "):
		s.initial = strings.TrimSpace(strings.TrimPrefix(record, "# branch.oid ")) == "(initial)"
	case strings.HasPrefix(record, "# branch.head "):
		head := strings.TrimSpace(strings.TrimPrefix(record, "# branch.head "))
		if head == "(detached)" {
			s.detached = true
			s.branch = ""
		} else {
			s.branch = head
		}
	case strings.HasPrefix(record, "# branch.upstream "):
		s.upstream = strings.TrimSpace(strings.TrimPrefix(record, "# branch.upstream "))
	case strings.HasPrefix(record, "# branch.ab "):
		ahead, behind, ok := parseAheadBehind(strings.TrimPrefix(record, "# branch.ab "))
		if ok {
			s.abKnown, s.ahead, s.behind = true, ahead, behind
		}
	}
}

// parseAheadBehind reads the "+A -B" pair of a `# branch.ab` header.
func parseAheadBehind(raw string) (ahead, behind int, ok bool) {
	fields := strings.Fields(raw)
	if len(fields) != 2 || !strings.HasPrefix(fields[0], "+") || !strings.HasPrefix(fields[1], "-") {
		return 0, 0, false
	}
	ahead, errA := strconv.Atoi(fields[0][1:])
	behind, errB := strconv.Atoi(fields[1][1:])
	if errA != nil || errB != nil || ahead < 0 || behind < 0 {
		return 0, 0, false
	}
	return ahead, behind, true
}

type numstat struct {
	adds, dels int
	binary     bool
}

// parseNumstatZ reads `git diff --numstat -z --no-renames` output:
// "adds<TAB>dels<TAB>path<NUL>" per file, "-" counts for a binary file. A
// rename record ("adds<TAB>dels<TAB><NUL>old<NUL>new<NUL>") is accepted too in
// case a configuration turns renames back on; it is keyed by the new path.
func parseNumstatZ(out []byte) map[string]numstat {
	stats := map[string]numstat{}
	tokens := bytes.Split(bytes.TrimRight(out, "\x00"), []byte{0})
	for index := 0; index < len(tokens); index++ {
		parts := strings.SplitN(string(tokens[index]), "\t", 3)
		if len(parts) != 3 {
			continue
		}
		path := parts[2]
		if path == "" && index+2 < len(tokens) {
			path = string(tokens[index+2])
			index += 2
		}
		if path == "" {
			continue
		}
		stat := numstat{binary: parts[0] == "-" && parts[1] == "-"}
		stat.adds, _ = strconv.Atoi(parts[0])
		stat.dels, _ = strconv.Atoi(parts[1])
		stats[path] = stat
	}
	return stats
}

// applyNumstat gives each changed file its line counts. A renamed file takes
// the counts of its new path; untracked files are counted by reading them
// (countUntracked), never by another git process.
func (s *repoStatus) applyNumstat(stats map[string]numstat) {
	for index := range s.files {
		file := &s.files[index]
		if stat, ok := stats[file.path]; ok {
			file.adds, file.dels, file.binary = stat.adds, stat.dels, stat.binary
		}
	}
}

// countUntracked counts the lines of untracked files, at most
// untrackedCountLimit of them and untrackedReadLimit bytes each. A file with a
// NUL byte near its start is binary and counts no lines.
func (s *repoStatus) countUntracked(root string) {
	counted := 0
	for index := range s.files {
		file := &s.files[index]
		if !file.untracked {
			continue
		}
		if counted >= untrackedCountLimit {
			return
		}
		counted++
		file.adds, file.binary = countFileLines(filepath.Join(root, filepath.FromSlash(file.path)))
	}
}

func countFileLines(path string) (int, bool) {
	handle, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer handle.Close()
	info, err := handle.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return 0, false
	}
	data, err := io.ReadAll(io.LimitReader(handle, untrackedReadLimit))
	if err != nil {
		return 0, false
	}
	probe := data
	if len(probe) > 8000 {
		probe = probe[:8000]
	}
	if bytes.IndexByte(probe, 0) >= 0 {
		return 0, true
	}
	if len(data) == 0 {
		return 0, false
	}
	lines := bytes.Count(data, []byte{'\n'})
	if data[len(data)-1] != '\n' {
		lines++
	}
	return lines, false
}

// capFiles keeps the payload bounded: the first repoFileLimit files in git's
// path order, and says the list was cut.
func (s *repoStatus) capFiles() {
	if len(s.files) <= repoFileLimit {
		return
	}
	s.files = s.files[:repoFileLimit]
	s.filesTruncated = true
}
