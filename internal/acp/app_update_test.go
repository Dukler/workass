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

func TestBeginUpdateDrainLatchesOnlyWhenQuiescent(t *testing.T) {
	m := updateTestManager()
	result := m.BeginUpdateDrain()
	if !result.Ready {
		t.Fatalf("quiescent readiness = %#v", result)
	}
	if err := m.beginWorkAdmission(); err != ErrUpdateDraining {
		t.Fatalf("work admitted after update drain: %v", err)
	}
}

func TestBusyUpdateCheckReopensWorkAdmission(t *testing.T) {
	m := updateTestManager()
	m.updateGateMu.Lock()
	m.updateAdmissions = 1
	m.updateGateMu.Unlock()
	result := m.BeginUpdateDrain()
	if result.Ready || result.Admissions != 1 {
		t.Fatalf("busy readiness = %#v", result)
	}
	m.updateGateMu.Lock()
	m.updateAdmissions = 0
	m.updateGateMu.Unlock()
	if err := m.beginWorkAdmission(); err != nil {
		t.Fatalf("busy readiness left admission fenced: %v", err)
	}
	m.endWorkAdmission()
}

func TestBeginUpdateDrainCountsTrackedSubagentOnlyAsBackgroundWork(t *testing.T) {
	m := updateTestManager()
	m.jobs["foreground"] = &Job{ID: "foreground", Kind: "app-chat", Status: "running"}
	m.jobs["subagent-job"] = &Job{ID: "subagent-job", Kind: "subagent", Status: "running"}
	m.subagents["subagent"] = &SubagentRun{ID: "subagent", JobID: "subagent-job", Status: "running"}

	result := m.BeginUpdateDrain()
	if result.Ready || result.ForegroundTurns != 1 || result.BackgroundWork != 1 {
		t.Fatalf("readiness double-counted tracked subagent = %#v", result)
	}
}
