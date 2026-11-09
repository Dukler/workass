package acp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// countingProviderUpdater installs latest into versionFile (when ok) and leaves
// one line per run in a count file.
func countingProviderUpdater(t *testing.T, versionFile, latest string, ok bool) (string, func() int) {
	t.Helper()
	dir := t.TempDir()
	countFile := filepath.Join(dir, "runs")
	script := filepath.Join(dir, "qwen-update")
	body := "printf 'run\\n' >> " + shellQuote(countFile) + "\n"
	if ok {
		body += "printf '" + latest + "\\n' > " + shellQuote(versionFile) + "\n"
	} else {
		body += "printf 'boom\\n' >&2\nexit 3\n"
	}
	writeFixtureExecutable(t, script, body)
	return script, func() int {
		raw, err := os.ReadFile(countFile)
		if err != nil {
			return 0
		}
		return strings.Count(string(raw), "run\n")
	}
}

func TestProviderAutoUpdateWaitsForTheSwitchAndAQuietAgent(t *testing.T) {
	t.Parallel()
	manager, events, versionFile := newProviderUpdateTestManager(t, "0.58.1", "0.58.2", "")
	script, runs := countingProviderUpdater(t, versionFile, "0.58.2", true)
	manager.opts.ProviderUpdateCommands = map[string]ProviderUpdateCommand{"qwen": {Command: script}}

	// Off: an offer is only offered.
	if update := manager.CheckProviderUpdates(context.Background()); len(update.Updates) != 1 || !update.Updates[0].UpdateAvailable {
		t.Fatalf("check = %#v, want one pending qwen update", update)
	}
	time.Sleep(150 * time.Millisecond)
	if n := runs(); n != 0 {
		t.Fatalf("updater ran %d times with the switch off", n)
	}

	// On, but a qwen turn is running: it waits.
	manager.mu.Lock()
	manager.jobs["busy-turn"] = &Job{ID: "busy-turn", Kind: "app-chat", Status: "running", ProviderID: "qwen"}
	manager.mu.Unlock()
	manager.SetAutoUpdateProviders(true)
	manager.autoInstallProviderUpdates()
	time.Sleep(150 * time.Millisecond)
	if n := runs(); n != 0 {
		t.Fatalf("updater ran %d times while a qwen turn was running", n)
	}

	// The turn ends: the next look installs it.
	manager.mu.Lock()
	manager.jobs["busy-turn"].Status = "done"
	manager.mu.Unlock()
	manager.kickProviderAutoUpdate()
	waitProviderUpdate(t, events, "qwen", func(update ProviderUpdate) bool {
		return update.Installed == "0.58.2" && !update.UpdateAvailable
	}, 3*time.Second)
	if n := runs(); n != 1 {
		t.Fatalf("updater ran %d times, want 1", n)
	}
}

func TestProviderAutoUpdateIgnoresOtherAgentsTurns(t *testing.T) {
	t.Parallel()
	manager, _, versionFile := newProviderUpdateTestManager(t, "0.58.1", "0.58.2", "")
	script, _ := countingProviderUpdater(t, versionFile, "0.58.2", true)
	manager.opts.ProviderUpdateCommands = map[string]ProviderUpdateCommand{"qwen": {Command: script}}
	manager.mu.Lock()
	manager.jobs["other"] = &Job{ID: "other", Kind: "app-chat", Status: "running", ProviderID: "claude"}
	manager.mu.Unlock()
	if manager.providerHasRunningTurn("qwen") {
		t.Fatal("a claude turn kept qwen from updating")
	}
	if !manager.providerHasRunningTurn("claude") {
		t.Fatal("the claude turn was not seen")
	}
}

func TestProviderAutoUpdateDoesNotRetryAFailedVersion(t *testing.T) {
	t.Parallel()
	manager, events, versionFile := newProviderUpdateTestManager(t, "0.58.1", "0.58.2", "")
	script, runs := countingProviderUpdater(t, versionFile, "0.58.2", false)
	manager.opts.ProviderUpdateCommands = map[string]ProviderUpdateCommand{"qwen": {Command: script}}
	manager.CheckProviderUpdates(context.Background())
	manager.SetAutoUpdateProviders(true)
	waitProviderUpdateProgress(t, events, "qwen", func(progress ProviderUpdateProgress) bool {
		return progress.Status == "failed"
	}, 3*time.Second)
	// The failed run re-checks, which looks again; so do later polls.
	for range 3 {
		manager.autoInstallProviderUpdates()
	}
	time.Sleep(200 * time.Millisecond)
	if n := runs(); n != 1 {
		t.Fatalf("updater ran %d times, want the one failed run only", n)
	}
	if pending := manager.pendingProviderAutoUpdates(); len(pending) != 0 {
		t.Fatalf("a failed version is still pending: %#v", pending)
	}
}
