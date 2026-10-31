package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"workass/internal/acp"
	"workass/internal/headlessupdate"
	"workass/internal/wire"
)

// Live bug (2026-10-31): a packaged machine whose app is closed answered every
// status read with an error naming a router. The daemon knows its version, its
// jobs and the last receipt, and says so, along with what it cannot know.
func TestAPackagedMachineWithNoWindowStillAnswersUpdateStatus(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	if err := os.MkdirAll(filepath.Join(root, "updates"), 0o700); err != nil {
		t.Fatal(err)
	}
	receipt := `{"phase":"failed","targetVersion":"9.9.9","error":"boom","workerPID":7,"installTarget":"C:/secret/path"}`
	if err := os.WriteFile(filepath.Join(root, "updates", "receipt.json"), []byte(receipt), 0o600); err != nil {
		t.Fatal(err)
	}
	router := &stubAgentChatRemoteRouter{err: errNoLocalRenderer}
	check := func(t *testing.T, result any, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("status refused: %v", err)
		}
		fields := mapFromAnyMain(result)
		if fields["updaterAvailable"] != false || fields["daemonOnly"] != true || fields["machineId"] != "m-host" {
			t.Fatalf("status did not say what it is: %#v", fields)
		}
		if !strings.Contains(fieldString(fields, "reason"), "no Workass window") {
			t.Fatalf("no reason a person can act on: %#v", fields)
		}
		kept := mapFromAnyMain(mapFromAnyMain(fields["diagnostics"])["receipt"])
		if kept["phase"] != "failed" || kept["error"] != "boom" {
			t.Fatalf("the last receipt is missing: %#v", kept)
		}
		if _, leaked := kept["installTarget"]; leaked {
			t.Fatalf("a field outside the list crossed machines: %#v", kept)
		}
	}
	t.Run("tool and mesh", func(t *testing.T) {
		handler := &agentControlHandler{remoteChats: router, selfMachineID: "m-host", stateDir: stateDir}
		result, err := handler.answerUpdateLocally(context.Background(), "update.status", map[string]any{})
		check(t, result, err)
	})
	t.Run("wire", func(t *testing.T) {
		hub := wire.NewHub()
		registerAgentUpdateWireHandlers(hub, router, nil, "m-host", stateDir, func() []headlessupdate.RunningJob { return nil })
		result, err := hub.Invoke(agentUpdateStatusChannel, []any{map[string]any{"machine_id": "m-host"}})
		check(t, result, err)
	})
	t.Run("apply is still refused", func(t *testing.T) {
		handler := &agentControlHandler{remoteChats: router, selfMachineID: "m-host", stateDir: stateDir}
		if _, err := handler.answerUpdateLocally(context.Background(), "update.apply", map[string]any{}); err == nil {
			t.Fatal("an apply with no window answered as if it could run")
		}
	})
}

func TestAnOldPeerWithNoWindowIsNamedByTheAskingMachine(t *testing.T) {
	err := updateStatusPeerRefusal("m-laptop", errNoLocalRenderer)
	if err == nil || !strings.Contains(err.Error(), "m-laptop") || !strings.Contains(err.Error(), "no Workass window") {
		t.Fatalf("refusal = %v", err)
	}
}

// A cancelled row leaves the rail whichever way it was cancelled; the words the
// tools read stay on the receipt, and an ended row of any other kind stays.
func TestCancelledWorkIsNotARailRow(t *testing.T) {
	for _, item := range []struct {
		kind, outcome, phase string
		cancelled            bool
	}{
		{"subagent", "cancelled", "cancelled", true},
		{"subagent", "", "cancelled", true},
		{"external", "cancelled", "", true},
		{"subagent", "failed", "failed", false},
		{"subagent", "settled", "done", false},
		{"external", "settled", "", false},
		{"bash", "", "cancelled", false},
	} {
		if got := acp.SpawnedWorkCancelled(item.kind, item.outcome, item.phase); got != item.cancelled {
			t.Fatalf("%#v: cancelled = %v", item, got)
		}
	}
}
