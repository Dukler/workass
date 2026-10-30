package procchild

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Preflight: every refusal this lane can make in a second instead of in hour
// three.
//
// delegation-fabric.md §9 states the principle — "a missing input is a refusal
// in seconds instead of a crash in hour three" — and the failure history is
// exactly that: lanes that queued for a GPU, waited, started, and died on a
// path that was never there, or on a tool that was not installed on the machine
// that happened to be chosen.
//
// Five checks, each the cheapest honest form of itself:
//
//   - A declared input exists, resolves, and is NOT EMPTY. Non-empty is the
//     one that catches the real case: a staging step that created the file and
//     then failed leaves a zero-byte input that every `test -e` accepts.
//   - A symlinked input's target resolves. A dangling symlink passes Lstat and
//     fails on open, which is the kind of refusal that arrives too late.
//   - The command is on PATH, resolved by the same rule the exec will use.
//   - The output directory is writable, proven by writing, not by reading a
//     mode bit — a read-only mount and a wrong owner both report modes that
//     look fine.
//   - Free disk is at or above the declared floor.
//
// Every refusal names the thing and the reason, because a refusal an agent
// cannot act on is a dead end.

// Refusal is one failed precondition.
type Refusal struct {
	// Check is the precondition's name: input | symlink | tool | output | disk.
	Check string `json:"check"`
	// Subject is what failed — a path, a tool name, a directory.
	Subject string `json:"subject"`
	// Reason is what is wrong with it, in words an agent can act on.
	Reason string `json:"reason"`
}

func (r Refusal) String() string {
	return fmt.Sprintf("%s %s: %s", r.Check, r.Subject, r.Reason)
}

// PreflightRequest is everything preflight needs, and nothing about the run.
type PreflightRequest struct {
	// Command is argv. Command[0] is resolved on PATH.
	Command []string
	// Inputs are the declared inputs: each must exist and be non-empty.
	Inputs []string
	// OutputFile is where the child's output will be written; its directory
	// must be writable.
	OutputFile string
	// MinFreeDiskBytes is the declared floor, 0 for no floor.
	MinFreeDiskBytes uint64
	// Dir is the child's working directory, checked for existence when set.
	Dir string
}

// Preflight returns every refusal it finds, in a stable order, rather than the
// first one. A lane with three missing inputs is one fix, not three attempts.
func Preflight(req PreflightRequest) []Refusal {
	refusals := make([]Refusal, 0, 4)
	if len(req.Command) == 0 || strings.TrimSpace(req.Command[0]) == "" {
		refusals = append(refusals, Refusal{Check: "tool", Subject: "", Reason: "no command was given after --"})
	} else if _, err := exec.LookPath(req.Command[0]); err != nil {
		refusals = append(refusals, Refusal{
			Check: "tool", Subject: req.Command[0],
			Reason: "not found on PATH on this machine",
		})
	}
	if dir := strings.TrimSpace(req.Dir); dir != "" {
		if info, err := os.Stat(dir); err != nil {
			refusals = append(refusals, Refusal{Check: "input", Subject: dir, Reason: "working directory does not exist"})
		} else if !info.IsDir() {
			refusals = append(refusals, Refusal{Check: "input", Subject: dir, Reason: "working directory is not a directory"})
		}
	}
	inputs := append([]string(nil), req.Inputs...)
	sort.Strings(inputs)
	for _, raw := range inputs {
		input := strings.TrimSpace(raw)
		if input == "" {
			continue
		}
		link, err := os.Lstat(input)
		if err != nil {
			refusals = append(refusals, Refusal{Check: "input", Subject: input, Reason: "does not exist"})
			continue
		}
		if link.Mode()&os.ModeSymlink != 0 {
			// Resolve the chain, and say which link broke. A dangling symlink is
			// the case Lstat accepts and the open rejects, so checking the link
			// alone would postpone the refusal to the moment it costs the most.
			target, err := filepath.EvalSymlinks(input)
			if err != nil {
				refusals = append(refusals, Refusal{
					Check: "symlink", Subject: input,
					Reason: "symlink target does not resolve",
				})
				continue
			}
			if _, err := os.Stat(target); err != nil {
				refusals = append(refusals, Refusal{
					Check: "symlink", Subject: input,
					Reason: "symlink resolves to " + target + ", which does not exist",
				})
				continue
			}
		}
		info, err := os.Stat(input)
		if err != nil {
			refusals = append(refusals, Refusal{Check: "input", Subject: input, Reason: "cannot be read"})
			continue
		}
		if info.IsDir() {
			entries, err := os.ReadDir(input)
			if err != nil {
				refusals = append(refusals, Refusal{Check: "input", Subject: input, Reason: "directory cannot be read"})
			} else if len(entries) == 0 {
				refusals = append(refusals, Refusal{Check: "input", Subject: input, Reason: "directory is empty"})
			}
			continue
		}
		if info.Size() == 0 {
			// Zero bytes is the staging failure that every existence check
			// passes: a step created the file, then died before writing it.
			refusals = append(refusals, Refusal{Check: "input", Subject: input, Reason: "exists but is empty"})
		}
	}
	if output := strings.TrimSpace(req.OutputFile); output != "" {
		dir := filepath.Dir(output)
		if refusal, ok := checkWritable(dir); !ok {
			refusals = append(refusals, refusal)
		} else if req.MinFreeDiskBytes > 0 {
			free, err := freeDiskBytes(dir)
			switch {
			case err != nil:
				refusals = append(refusals, Refusal{
					Check: "disk", Subject: dir,
					Reason: "free space cannot be measured on this machine: " + err.Error(),
				})
			case free < req.MinFreeDiskBytes:
				refusals = append(refusals, Refusal{
					Check: "disk", Subject: dir,
					Reason: fmt.Sprintf("%s free, below the declared %s",
						humanBytes(free), humanBytes(req.MinFreeDiskBytes)),
				})
			}
		}
	}
	return refusals
}

// checkWritable proves the directory is writable by writing to it. A mode bit
// says what the owner may do; a read-only mount, a full filesystem and a
// directory owned by somebody else all report modes that look fine.
func checkWritable(dir string) (Refusal, bool) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return Refusal{Check: "output", Subject: dir, Reason: "no output directory"}, false
	}
	info, err := os.Stat(dir)
	if err != nil {
		return Refusal{Check: "output", Subject: dir, Reason: "output directory does not exist"}, false
	}
	if !info.IsDir() {
		return Refusal{Check: "output", Subject: dir, Reason: "output directory is not a directory"}, false
	}
	probe, err := os.CreateTemp(dir, ".workass-run-probe-*")
	if err != nil {
		return Refusal{Check: "output", Subject: dir, Reason: "output directory is not writable"}, false
	}
	name := probe.Name()
	_ = probe.Close()
	_ = os.Remove(name)
	return Refusal{}, true
}

func humanBytes(value uint64) string {
	const unit = 1024
	if value < unit {
		return fmt.Sprintf("%d B", value)
	}
	suffixes := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	size, index := float64(value)/unit, 0
	for size >= unit && index < len(suffixes)-1 {
		size /= unit
		index++
	}
	return fmt.Sprintf("%.1f %s", size, suffixes[index])
}

// ParseBytes reads a human disk floor: 50G, 500M, 1T, or plain bytes. It is
// here rather than in the CLI because the supervisor records what it parsed.
func ParseBytes(value string) (uint64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	// A trailing B or i is noise that every operator writes sometimes: 50G,
	// 50GB and 50GiB are one floor, and refusing two of the three spellings
	// would be a refusal about typing rather than about disk.
	digits := strings.TrimRight(value, "bBiI")
	if digits == "" {
		return 0, fmt.Errorf("%q is not a byte size (use 50G, 500M or a plain byte count)", value)
	}
	multiplier := uint64(1)
	switch digits[len(digits)-1] {
	case 'K', 'k':
		multiplier = 1 << 10
	case 'M', 'm':
		multiplier = 1 << 20
	case 'G', 'g':
		multiplier = 1 << 30
	case 'T', 't':
		multiplier = 1 << 40
	}
	if multiplier > 1 {
		digits = digits[:len(digits)-1]
	}
	digits = strings.TrimSpace(digits)
	if digits == "" || strings.IndexFunc(digits, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return 0, fmt.Errorf("%q is not a byte size (use 50G, 500M or a plain byte count)", value)
	}
	var parsed uint64
	if _, err := fmt.Sscanf(digits, "%d", &parsed); err != nil {
		return 0, fmt.Errorf("%q is not a byte size (use 50G, 500M or a plain byte count)", value)
	}
	return parsed * multiplier, nil
}
