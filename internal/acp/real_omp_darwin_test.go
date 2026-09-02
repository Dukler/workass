//go:build darwin

package acp

import (
	"context"
	"os"
	"os/exec"
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
