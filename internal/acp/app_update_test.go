package acp

import (
	"testing"
	"time"
)

func updateTestManager(t *testing.T) *Manager {
	t.Helper()
	m := NewManager(Options{
		StateDir:               t.TempDir(),
		RSSSampleInterval:      time.Hour,
		ProviderUpdateInterval: time.Hour,
	})
	t.Cleanup(func() { m.Reset() })
	return m
}

func TestBeginUpdateDrainLatchesOnlyWhenQuiescent(t *testing.T) {
	m := updateTestManager(t)
	result := m.BeginUpdateDrain()
	if !result.Ready {
		t.Fatalf("quiescent readiness = %#v", result)
	}
	if err := m.beginWorkAdmission(); err != ErrUpdateDraining {
		t.Fatalf("work admitted after update drain: %v", err)
	}
}

func TestBusyUpdateCheckReopensWorkAdmission(t *testing.T) {
	m := updateTestManager(t)
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
