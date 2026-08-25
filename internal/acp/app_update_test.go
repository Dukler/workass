package acp

import "testing"

func updateTestManager() *Manager {
	return &Manager{
		jobs:               make(map[string]*Job),
		subagents:          make(map[string]*SubagentRun),
		spawnedWork:        make(map[string]*spawnedWorkRecord),
		providerUpdateRuns: make(map[string]*providerUpdateRun),
	}
}

func TestBeginUpdateDrainLatchesAdmissionUntilProcessExit(t *testing.T) {
	m := updateTestManager()
	result := m.BeginUpdateDrain()
	if !result.Ready {
		t.Fatalf("readiness after drain = %#v", result)
	}
	if err := m.beginWorkAdmission(); err != ErrUpdateDraining {
		t.Fatalf("work admitted during update drain: %v", err)
	}
}

func TestUpdateDrainNeverBlockedByInFlightAdmission(t *testing.T) {
	m := updateTestManager()
	m.updateGateMu.Lock()
	m.updateAdmissions = 1
	m.updateGateMu.Unlock()
	result := m.BeginUpdateDrain()
	if !result.Ready || result.Admissions != 1 {
		t.Fatalf("in-flight admission blocked a user-clicked update: %#v", result)
	}
	if err := m.beginWorkAdmission(); err != ErrUpdateDraining {
		t.Fatalf("work admitted during update drain: %v", err)
	}
}

func TestUpdateDrainRecordsActiveWorkWithoutBlocking(t *testing.T) {
	m := updateTestManager()
	m.jobs["foreground"] = &Job{ID: "foreground", Kind: "app-chat", Status: "running"}
	m.jobs["subagent-job"] = &Job{ID: "subagent-job", Kind: "subagent", Status: "running"}
	m.subagents["subagent"] = &SubagentRun{ID: "subagent", JobID: "subagent-job", Status: "running"}
	exit := 0
	m.providerUpdateRuns["codex"] = &providerUpdateRun{providerID: "codex", status: "running", exitCode: &exit}

	result := m.BeginUpdateDrain()
	if !result.Ready {
		t.Fatalf("active work must never block a user-clicked update: %#v", result)
	}
	if result.ForegroundTurns != 1 {
		t.Fatalf("foreground turns = %d, want 1", result.ForegroundTurns)
	}
	if result.BackgroundWork != 1 {
		t.Fatalf("background work = %d, want 1 (tracked subagent counted once)", result.BackgroundWork)
	}
	if result.ProviderUpdates != 1 {
		t.Fatalf("provider updates = %d, want 1", result.ProviderUpdates)
	}
}

func TestProviderUpdateRunTerminalNotCounted(t *testing.T) {
	m := updateTestManager()
	done := 0
	m.providerUpdateRuns["codex"] = &providerUpdateRun{providerID: "codex", status: "completed", exitCode: &done}
	result := m.BeginUpdateDrain()
	if result.ProviderUpdates != 0 {
		t.Fatalf("terminal provider update counted as active: %#v", result.ProviderUpdates)
	}
}
