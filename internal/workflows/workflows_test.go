package workflows

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestProjectAndWorkflowRoot(t *testing.T) {
	home := filepath.Join(string(filepath.Separator), "Users", "test")
	cwd := "/Users/dev/Workspace/workass"
	wantProject := filepath.Join(home, ".claude", "projects", "-Users-dev-Workspace-workass")
	if got := ProjectRoot(home, cwd); got != wantProject {
		t.Fatalf("ProjectRoot() = %q, want %q", got, wantProject)
	}
	wantWorkflow := filepath.Join(wantProject, "session-1", "subagents", "workflows")
	if got := WorkflowRoot(home, cwd, "session-1"); got != wantWorkflow {
		t.Fatalf("WorkflowRoot() = %q, want %q", got, wantWorkflow)
	}
}

func TestDiscoverRunsNewestFirst(t *testing.T) {
	root := t.TempDir()
	older := filepath.Join(root, "wf_older")
	newer := filepath.Join(root, "wf_newer")
	ignored := filepath.Join(root, "other")
	for _, path := range []string{older, newer, ignored} {
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	base := time.Now().Add(-time.Hour)
	if err := os.Chtimes(older, base, base); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(newer, base.Add(time.Minute), base.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	runs, err := DiscoverRuns(root)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{newer, older}; !slices.Equal(runs, want) {
		t.Fatalf("DiscoverRuns() = %q, want %q", runs, want)
	}
}

func TestDiscoverRunsMissingRoot(t *testing.T) {
	if _, err := DiscoverRuns(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("DiscoverRuns() error = nil, want an error")
	}
}

func TestWatchCarriesPartialLineAndCopiesSnapshots(t *testing.T) {
	root := t.TempDir()
	runDir := filepath.Join(root, "wf_test")
	if err := os.Mkdir(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(runDir, "journal.jsonl")
	initial := "{\"type\":\"started\",\"key\":\"v2:first\",\"agentId\":\"agent-1\"}\n" +
		"{\"type\":\"result\",\"key\":\"v2:first\",\"agentId\":\"agent-1\",\"result\":{\"api_key\":\"secret"
	if err := os.WriteFile(journal, []byte(initial), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	updates, err := Watch(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	first := receiveProgress(t, updates)
	if first.RunID != "wf_test" || first.Started != 1 || first.Done != 0 {
		t.Fatalf("first snapshot = %#v, want wf_test started=1 done=0", first)
	}

	select {
	case update := <-updates:
		t.Fatalf("partial line produced a snapshot: %#v", update)
	case <-time.After(3 * pollInterval):
	}
	appendJournal(t, journal, "\"}}\n")
	second := receiveProgress(t, updates)
	if second.Started != 1 || second.Done != 1 {
		t.Fatalf("second snapshot started=%d done=%d, want 1/1", second.Started, second.Done)
	}
	if summary := second.Agents["agent-1"].ResultSummary; summary != `{"api_key":"[redacted]"}` {
		t.Fatalf("result summary = %q, want redacted JSON", summary)
	}
	if len(first.Agents) != 1 || first.Agents["agent-1"].Done {
		t.Fatalf("first snapshot mutated after append: %#v", first)
	}

	appendJournal(t, journal, "{\"type\":\"started\",\"key\":\"v2:second\",\"agentId\":\"agent-2\"}\n")
	third := receiveProgress(t, updates)
	if third.Started != 2 || third.Done != 1 || len(third.Agents) != 2 {
		t.Fatalf("third snapshot = %#v, want started=2 done=1 agents=2", third)
	}
}

func TestSummarizeResultBoundsAndRedacts(t *testing.T) {
	raw := []byte(`{"note":"token=plain-secret ` + strings.Repeat("界", 250) + `","credential":"hidden"}`)
	summary := summarizeResult(raw)
	if strings.Contains(summary, "plain-secret") || strings.Contains(summary, "hidden") {
		t.Fatalf("summary leaked a secret: %q", summary)
	}
	if got := utf8.RuneCountInString(summary); got > maxResultSummaryRunes {
		t.Fatalf("summary has %d runes, want at most %d", got, maxResultSummaryRunes)
	}
	if !utf8.ValidString(summary) {
		t.Fatalf("summary is not valid UTF-8: %q", summary)
	}
}

func TestLiveWorkflowReceipt(t *testing.T) {
	const (
		cwd       = "/Users/dev/Workspace/workass"
		sessionID = "14b4ba15-1887-4658-b7bb-8c130c4b38df"
		runID     = "wf_d33a13e0-084"
	)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("live receipt unavailable: resolve home: %v", err)
	}
	workflowRoot := WorkflowRoot(home, cwd, sessionID)
	pathSource := "session-id"
	if _, err := os.Stat(filepath.Join(workflowRoot, runID)); err != nil {
		pathSource = "project-root-fallback"
		matches, globErr := filepath.Glob(filepath.Join(ProjectRoot(home, cwd), "*", "subagents", "workflows"))
		if globErr != nil {
			t.Fatalf("fallback scan: %v", globErr)
		}
		workflowRoot = ""
		for _, candidate := range matches {
			if info, statErr := os.Stat(filepath.Join(candidate, runID)); statErr == nil && info.IsDir() {
				workflowRoot = candidate
				break
			}
		}
		if workflowRoot == "" {
			t.Skipf("live receipt unavailable: %s not found below %s", runID, ProjectRoot(home, cwd))
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	updates, err := Watch(ctx, workflowRoot)
	if err != nil {
		t.Fatalf("Watch(%q): %v", workflowRoot, err)
	}
	for update := range updates {
		if update.RunID != runID {
			continue
		}
		if update.Started != 18 || update.Done != 18 {
			t.Fatalf("live run %s has started=%d done=%d, want 18/18", runID, update.Started, update.Done)
		}
		t.Logf("LIVE_RECEIPT path_source=%s workflow_root=%s run=%s started=%d done=%d", pathSource, workflowRoot, runID, update.Started, update.Done)
		return
	}
	t.Fatalf("live run %s produced no snapshot before timeout", runID)
}

func appendJournal(t *testing.T, path, text string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(text); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func receiveProgress(t *testing.T, updates <-chan RunProgress) RunProgress {
	t.Helper()
	select {
	case update, ok := <-updates:
		if !ok {
			t.Fatal("updates channel closed")
		}
		return update
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for workflow progress")
		return RunProgress{}
	}
}
