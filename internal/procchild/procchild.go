// Package procchild is the process child: a supervised OS process that belongs
// to a chat but is not a child of the daemon.
//
// docs/specs/fleet-runtime.md §13 P4, with the supervisor itself specified in
// docs/specs/delegation-fabric.md §5. It exists because every earlier attempt
// to run long work from a chat failed in one of two ways, and both are failures
// of EVIDENCE rather than of plumbing:
//
//   - A detached `nohup` plus a marker file plus an agent-side polling watcher.
//     The watcher died with the agent session or at the harness cap; one job
//     finished and never wrote its marker; a remote `pgrep -f` matched its own
//     ssh command and reported "running" for five hours.
//   - In-process work, which cannot survive the one event it most needs to
//     survive: the daemon restarting or updating itself.
//
// So a process child is started by `workass run`, which is the product's own
// binary acting as the supervisor. The daemon is never its parent (§9: daemon-
// side spawning stays a non-goal, and a daemon that spawned it would lend it an
// environment and die with it). What the daemon holds instead is the record,
// the log and the verdict:
//
//   - The supervisor appends every state transition to a SPOOL on disk before
//     it tells anybody, so a daemon that is restarting or mid-self-update
//     misses nothing — it ingests the spool when it comes back. Facts on disk,
//     because a supervisor that needed a live daemon would fail exactly during
//     an update, which is when it is needed most.
//   - A pid that is gone with no terminal spool event is `lost`. Absence is
//     evidence: this is the one rule that makes "running" unable to be a lie,
//     and it is the opposite of the marker-file design, where absence meant
//     "still running".
//
// Nothing here starts a goroutine the caller did not ask for, and nothing here
// talks to the daemon except register.go, which is explicitly the one place
// that does.
package procchild

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"workass/internal/durablefs"
)

// Root is the directory every process child's durable state lives under. It is
// a sibling of `external-work` rather than a reuse of it, because the two have
// different lifetimes: an output file is the caller's and may be anywhere the
// path validator allows, while a spool is the runtime's and is only ever
// derived from a child id.
func Root(stateDir string) string {
	stateDir = strings.TrimSpace(stateDir)
	if stateDir == "" {
		return ""
	}
	root := filepath.Join(stateDir, "process-children")
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	return filepath.Clean(root)
}

// idPattern is the shape of a child id. It is today's external-work id shape
// (`xw` plus base64url) because a process child IS today's external work, now
// with a supervisor: the identifier law forbids renaming what already exists,
// and every stored snapshot, receipt and rail row already matches this.
var idPattern = regexp.MustCompile(`^xw[A-Za-z0-9_-]{6,40}$`)

// ValidID reports whether value is a well-formed child id.
func ValidID(value string) bool { return idPattern.MatchString(strings.TrimSpace(value)) }

// IDForOperation derives a child id from the caller's operation id.
//
// This is §6's "one operation_id for both sides", made concrete. The
// supervisor mints the operation id once and derives the child id from it, so
// "launch there" and "record here" cannot disagree: a registration that is
// retried after an uncertain reply resolves to the same id, the same row and
// the same spool file, and a second child is impossible. The old failure this
// removes is a job that ran once and was recorded twice, or recorded once and
// then orphaned under a second name.
func IDForOperation(operationID string) (string, error) {
	operationID = strings.TrimSpace(operationID)
	if operationID == "" {
		return "", errors.New("a process child needs an operation id to derive its own id from")
	}
	sum := sha256.Sum256([]byte("workass-process-child-v1\x00" + operationID))
	id := "xw" + base64.RawURLEncoding.EncodeToString(sum[:9])
	if !ValidID(id) {
		return "", fmt.Errorf("derived process child id %q is malformed", id)
	}
	return id, nil
}

// Dir is one child's own directory.
func Dir(stateDir, childID string) string {
	root := Root(stateDir)
	if root == "" || !ValidID(childID) {
		return ""
	}
	return filepath.Join(root, strings.TrimSpace(childID))
}

// SpoolPath is the child's append-only event spool.
func SpoolPath(stateDir, childID string) string {
	dir := Dir(stateDir, childID)
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "events.jsonl")
}

// StopPath is the stop-file: a cancel written by whoever wants the child to
// stop, read by the supervisor.
//
// A file rather than a signal because a cancel is a WISH (§4), and a wish has
// to survive the three cases a signal cannot: the daemon and the supervisor
// running as different users, a Windows host with no SIGTERM at all, and a
// cancel issued while the supervisor is between states. The supervisor reads
// it, performs the graceful sequence itself, and records what it did — so the
// same mechanism is the one path on every platform, local or remote.
func StopPath(stateDir, childID string) string {
	dir := Dir(stateDir, childID)
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "stop")
}

// ResourceQueueDir is the visible FIFO queue for one named resource.
//
// Visible is literal: it is a directory a human can list, with one file per
// waiter naming its child, its label, its supervisor pid and when it asked.
// §10 is explicit that a queue only the daemon honours is not a lock, so this
// is the queue AND the lock, held by the process that is actually using the
// resource.
func ResourceQueueDir(stateDir, resource string) string {
	root := Root(stateDir)
	name := NormalizeResource(resource)
	if root == "" || name == "" {
		return ""
	}
	return filepath.Join(root, "resources", name)
}

var resourcePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$`)

// NormalizeResource accepts a resource name that can be a directory entry and
// nothing else. A name with a separator in it would put the queue somewhere
// other than the queue.
func NormalizeResource(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || !resourcePattern.MatchString(value) {
		return ""
	}
	return value
}

// RemoveDir deletes one child's directory once its outcome is durable
// elsewhere.
//
// What is left in there is small — the stop file, if anybody cancelled — but
// the directory itself is per job and forever, and a busy week of jobs leaving
// thousands of empty directories behind is exactly the litter the spool's own
// removal used to prevent. Called by the daemon, never by a supervisor: the
// supervisor does not know whether anybody has read what it reported.
func RemoveDir(stateDir, childID string) error {
	dir := Dir(stateDir, childID)
	if dir == "" {
		return nil
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return durablefs.SyncDirectory(Root(stateDir))
}

// EnsureDir creates one child's directory with the owner-only mode every
// Workass state directory uses.
func EnsureDir(stateDir, childID string) (string, error) {
	dir := Dir(stateDir, childID)
	if dir == "" {
		return "", errors.New("this machine has no process-child state directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}
