//go:build darwin

package acp

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestRealOMPProtocolCatalog is an opt-in protocol canary for a user-owned OMP
// installation. It sends no prompt and treats only ACP metadata as the oracle.
func TestRealOMPProtocolCatalog(t *testing.T) {
	if os.Getenv("WORKASS_REAL_OMP") != "1" {
		t.Skip("set WORKASS_REAL_OMP=1 to probe the installed OMP ACP server")
	}
	command, err := exec.LookPath("omp")
	if err != nil {
		t.Fatalf("resolve installed OMP: %v", err)
	}
	root := repoRoot(t)
	manager := NewManager(Options{
		RootDir: root,
		Providers: []ProviderConfig{{
			ID: "omp", Name: "Oh My Pi", Command: command, ResolvedCommand: command,
			Args: []string{"acp"}, CWD: root, Enabled: true,
		}},
		DefaultProviderID: "omp",
		InitTimeout:       30 * time.Second,
		RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	models, modes, agent, err := manager.probeProviderCatalogWithInitTimeout(ctx, ProviderConfig{
		ID: "omp", Name: "Oh My Pi", Command: command, ResolvedCommand: command,
		Args: []string{"acp"}, CWD: root, Enabled: true,
	}, 30*time.Second)
	if err != nil {
		t.Fatalf("probe OMP catalog: %v", err)
	}
	if agent != "oh-my-pi" {
		t.Fatalf("OMP agent name = %q, want oh-my-pi", agent)
	}
	if len(models) == 0 {
		t.Fatal("installed OMP returned no selectable ACP models")
	}
	if len(modes) == 0 {
		t.Fatal("installed OMP returned no ACP modes")
	}
	t.Logf("OMP ACP catalog models=%d modes=%d", len(models), len(modes))
}

// TestRealOMPTurn is a separately opt-in inference canary for the complete
// Workass Manager -> OMP ACP session path. It intentionally uses the user's
// provider-owned OMP profile and can therefore make a real model request.
func TestRealOMPTurn(t *testing.T) {
	if os.Getenv("WORKASS_REAL_OMP_TURN") != "1" {
		t.Skip("set WORKASS_REAL_OMP_TURN=1 to run one real OMP turn")
	}
	command, err := exec.LookPath("omp")
	if err != nil {
		t.Fatalf("resolve installed OMP: %v", err)
	}
	root := repoRoot(t)
	events := newEventCollector()
	manager := NewManager(Options{
		RootDir: root,
		Providers: []ProviderConfig{{
			ID: "omp", Name: "Oh My Pi", Command: command, ResolvedCommand: command,
			Args: []string{"acp"}, CWD: root, Enabled: true,
		}},
		DefaultProviderID: "omp",
		Broadcast:         events.Broadcast,
		InitTimeout:       30 * time.Second,
		RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	session, err := manager.NewSession(ctx, SessionOptions{
		TabID: "real-omp-tab", ChatID: "real-omp-chat", ProviderID: "omp", CWD: root,
	})
	if err != nil {
		t.Fatalf("new OMP session: %v", err)
	}
	t.Logf("OMP session model=%q mode=%q models=%d modes=%d", stringPointer(session.CurrentModelID), stringPointer(session.CurrentModeID), len(session.Models), len(session.Modes))
	job, err := manager.StartJob(context.Background(), JobStartOptions{
		Kind: "app-chat", SessionID: session.SessionID, TabID: "real-omp-tab", ChatID: "real-omp-chat",
		ProviderID: "omp", Prompt: "Reply with exactly OMP_OK and nothing else. Do not use tools.",
	})
	if err != nil {
		t.Fatalf("start OMP job: %v", err)
	}
	end := events.waitJobEnd(t, jobID(job), 3*time.Minute)
	assertJobStatus(t, end, "done", 0, "end_turn")
	if result := asString(jobFromEnd(end)["result"]); !strings.Contains(result, "OMP_OK") {
		t.Fatalf("OMP result did not contain the canary marker: %q", result)
	}
}
