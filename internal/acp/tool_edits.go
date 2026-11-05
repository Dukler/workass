package acp

import (
	"context"
	"strings"
)

// ToolEdit is one agent tool call that changes files (ACP kind edit, delete or
// move), as the provider reported it: Location and Input are the event's own
// fields, CWD the job's working folder for resolving relative paths.
type ToolEdit struct {
	TabID    string
	ChatID   string
	CWD      string
	ToolKind string
	Status   string
	Location string
	Input    string
}

// toolEditObserverBox holds the observer so the provider event path reads it
// with one atomic load.
type toolEditObserverBox struct{ fn func(ToolEdit) }

// SetToolEditObserver installs the hook told about every agent edit tool
// event, or removes it with nil. It runs on the provider event path, so it
// must return at once: the «Cambios» watcher only notes the path and does any
// work elsewhere, and does nothing while nobody watches.
func (m *Manager) SetToolEditObserver(fn func(ToolEdit)) {
	if m == nil {
		return
	}
	if fn == nil {
		m.toolEditObserver.Store(nil)
		return
	}
	m.toolEditObserver.Store(&toolEditObserverBox{fn: fn})
}

func (m *Manager) noteToolEdit(job *Job, toolKind, status, location, input string) {
	if m == nil || job == nil {
		return
	}
	box := m.toolEditObserver.Load()
	if box == nil {
		return
	}
	switch strings.ToLower(strings.TrimSpace(toolKind)) {
	case "edit", "delete", "move":
	default:
		return
	}
	box.fn(ToolEdit{
		TabID: job.TabID, ChatID: job.ChatID, CWD: job.CWD,
		ToolKind: toolKind, Status: status, Location: location, Input: input,
	})
}

// RunGitReadOnly runs one read-only git command in repo for a caller outside
// this package: GIT_OPTIONAL_LOCKS=0 so it never takes the index lock from the
// user's own git, the daemon's process-tree ownership so a timeout leaves no
// helper behind, and at most chatEnvGitTimeout however long ctx allows.
func RunGitReadOnly(ctx context.Context, repo string, args ...string) ([]byte, error) {
	return gitOutput(ctx, repo, args...)
}
