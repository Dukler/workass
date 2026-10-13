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
		t.Fatalf("in-flight admission blocked an explicitly authorized update: %#v", result)
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
		t.Fatalf("active work must never block an explicitly authorized update: %#v", result)
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

func TestUpdateWorkCountsWhatAnUpdateStopsWithoutDraining(t *testing.T) {
	m := updateTestManager()
	m.jobs["turn"] = &Job{ID: "turn", Kind: "app-chat", TabID: "tab-a", Status: "running"}
	m.jobs["done"] = &Job{ID: "done", Kind: "app-chat", TabID: "tab-a", Status: "completed"}
	m.jobs["subagent-job"] = &Job{ID: "subagent-job", Kind: "subagent", TabID: "tab-b", Status: "running"}
	work := func(id, tab, kind, status, role string) {
		m.spawnedWork[id] = &spawnedWorkRecord{Item: SpawnedWorkItem{ID: id, TabID: tab, Kind: kind, Status: status, Role: role}}
	}
	work("agent", "tab-b", "agent", "running", "")
	work("shell", "tab-b", "background", "running", "")
	work("tracked", "tab-b", trackedSubagentSpawnedWorkKind, "running", "")
	work("lane", "tab-a", "external", "running", "")
	work("server", "tab-a", "background", "running", spawnedWorkRoleService)
	work("finished", "tab-a", "agent", "completed", "")

	got := m.UpdateWorkInProgress()
	if got.ForegroundTurns != 1 || got.BackgroundWork != 3 {
		t.Fatalf("counts = %d turns / %d background, want 1 / 3 (external lanes, services and the subagent's own job excluded): %#v", got.ForegroundTurns, got.BackgroundWork, got)
	}
	if len(got.Chats) != 2 || got.Chats[0] != (UpdateWorkChat{TabID: "tab-a", ForegroundTurns: 1}) || got.Chats[1] != (UpdateWorkChat{TabID: "tab-b", BackgroundWork: 3}) {
		t.Fatalf("per-chat = %#v", got.Chats)
	}
	if m.updateDraining {
		t.Fatal("a read-only count must never close admission")
	}
}
