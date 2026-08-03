// Package workflows reads Claude Code workflow journals without modifying them.
package workflows

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	pollInterval          = 100 * time.Millisecond
	maxResultSummaryRunes = 200
)

var (
	secretTextRE = regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/=-]+|((?:api[_-]?key|token|secret|password|credential)\s*[:=]\s*)("[^"]*"|'[^']*'|[^\s,;}]+)`)
	secretKeyRE  = regexp.MustCompile(`(?i)(api[_-]?key|token|secret|password|credential|bearer)`)
)

// AgentState is the journal-derived state of one workflow sub-agent.
type AgentState struct {
	AgentID       string
	Started       bool
	Done          bool
	ResultSummary string
}

// RunProgress is a point-in-time snapshot of one workflow run.
type RunProgress struct {
	RunID     string
	Agents    map[string]AgentState
	Started   int
	Done      int
	UpdatedAt time.Time
}

type journalEvent struct {
	Type    string          `json:"type"`
	AgentID string          `json:"agentId"`
	Result  json.RawMessage `json:"result"`
}

type runState struct {
	offset    int64
	remainder []byte
	agents    map[string]AgentState
}

// ProjectRoot returns Claude Code's encoded project directory for cwd.
func ProjectRoot(home, cwd string) string {
	encoded := strings.ReplaceAll(cwd, "/", "-")
	return filepath.Join(home, ".claude", "projects", encoded)
}

// WorkflowRoot returns the workflow directory for a Claude Code session.
func WorkflowRoot(home, cwd, sessionID string) string {
	return filepath.Join(ProjectRoot(home, cwd), sessionID, "subagents", "workflows")
}

// DiscoverRuns returns wf_* directories ordered by modification time, newest first.
func DiscoverRuns(workflowRoot string) ([]string, error) {
	entries, err := os.ReadDir(workflowRoot)
	if err != nil {
		return nil, err
	}

	type run struct {
		path    string
		modTime time.Time
	}
	runs := make([]run, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "wf_") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		runs = append(runs, run{
			path:    filepath.Join(workflowRoot, entry.Name()),
			modTime: info.ModTime(),
		})
	}
	sort.Slice(runs, func(i, j int) bool {
		if runs[i].modTime.Equal(runs[j].modTime) {
			return runs[i].path < runs[j].path
		}
		return runs[i].modTime.After(runs[j].modTime)
	})

	paths := make([]string, len(runs))
	for i, run := range runs {
		paths[i] = run.path
	}
	return paths, nil
}

// Watch polls workflow journals and emits a new immutable snapshot after each
// append containing at least one complete, valid journal event. Errors after
// startup are treated as transient and retried on the next poll.
func Watch(ctx context.Context, workflowRoot string) (<-chan RunProgress, error) {
	if _, err := DiscoverRuns(workflowRoot); err != nil {
		return nil, err
	}

	updates := make(chan RunProgress, 16)
	go watch(ctx, workflowRoot, updates)
	return updates, nil
}

func watch(ctx context.Context, workflowRoot string, updates chan<- RunProgress) {
	defer close(updates)
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	runs := make(map[string]*runState)
	for {
		if !poll(ctx, workflowRoot, runs, updates) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func poll(ctx context.Context, workflowRoot string, states map[string]*runState, updates chan<- RunProgress) bool {
	runPaths, err := DiscoverRuns(workflowRoot)
	if err != nil {
		return true
	}
	for _, runPath := range runPaths {
		journalPath := filepath.Join(runPath, "journal.jsonl")
		info, err := os.Stat(journalPath)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}

		state := states[runPath]
		if state == nil {
			state = &runState{agents: make(map[string]AgentState)}
			states[runPath] = state
		}
		if info.Size() < state.offset {
			state.offset = 0
			state.remainder = nil
			state.agents = make(map[string]AgentState)
		}
		if info.Size() == state.offset {
			continue
		}

		advanced, err := readAppendedJournal(journalPath, state)
		if err != nil || !advanced {
			continue
		}
		snapshot := snapshot(filepath.Base(runPath), state.agents, info.ModTime())
		select {
		case updates <- snapshot:
		case <-ctx.Done():
			return false
		}
	}
	return true
}

func readAppendedJournal(path string, state *runState) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	if _, err := f.Seek(state.offset, io.SeekStart); err != nil {
		return false, err
	}

	appended, err := io.ReadAll(f)
	if err != nil {
		return false, err
	}
	state.offset += int64(len(appended))
	combined := append(append([]byte(nil), state.remainder...), appended...)
	lastNewline := bytes.LastIndexByte(combined, '\n')
	if lastNewline < 0 {
		state.remainder = combined
		return false, nil
	}
	complete := combined[:lastNewline]
	state.remainder = append(state.remainder[:0], combined[lastNewline+1:]...)

	advanced := false
	for _, line := range bytes.Split(complete, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var event journalEvent
		if err := json.Unmarshal(line, &event); err != nil || event.AgentID == "" {
			continue
		}
		agent := state.agents[event.AgentID]
		agent.AgentID = event.AgentID
		switch event.Type {
		case "started":
			agent.Started = true
		case "result":
			agent.Done = true
			agent.ResultSummary = summarizeResult(event.Result)
		default:
			continue
		}
		state.agents[event.AgentID] = agent
		advanced = true
	}
	return advanced, nil
}

func snapshot(runID string, agents map[string]AgentState, updatedAt time.Time) RunProgress {
	copyAgents := make(map[string]AgentState, len(agents))
	started := 0
	done := 0
	for id, agent := range agents {
		copyAgents[id] = agent
		if agent.Started {
			started++
		}
		if agent.Done {
			done++
		}
	}
	return RunProgress{
		RunID:     runID,
		Agents:    copyAgents,
		Started:   started,
		Done:      done,
		UpdatedAt: updatedAt,
	}
}

func summarizeResult(raw json.RawMessage) string {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return "<invalid result>"
	}
	encoded, err := json.Marshal(redactValue(value))
	if err != nil {
		return "<invalid result>"
	}
	summary := redactSensitiveText(string(encoded))
	if utf8.RuneCountInString(summary) <= maxResultSummaryRunes {
		return summary
	}
	runes := []rune(summary)
	return string(runes[:maxResultSummaryRunes-1]) + "…"
}

func redactValue(value any) any {
	switch value := value.(type) {
	case map[string]any:
		redacted := make(map[string]any, len(value))
		for key, item := range value {
			if secretKeyRE.MatchString(key) {
				redacted[key] = "[redacted]"
				continue
			}
			redacted[key] = redactValue(item)
		}
		return redacted
	case []any:
		redacted := make([]any, len(value))
		for i, item := range value {
			redacted[i] = redactValue(item)
		}
		return redacted
	case string:
		return redactSensitiveText(value)
	default:
		return value
	}
}

func redactSensitiveText(value string) string {
	return secretTextRE.ReplaceAllStringFunc(value, func(match string) string {
		lower := strings.ToLower(match)
		if strings.HasPrefix(lower, "bearer ") {
			return match[:strings.Index(lower, "bearer ")+7] + "[redacted]"
		}
		if i := strings.IndexAny(match, ":="); i >= 0 {
			return match[:i+1] + "[redacted]"
		}
		return "[redacted]"
	})
}
