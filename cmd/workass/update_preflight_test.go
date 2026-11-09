package main

import (
	"os"
	"strings"
	"testing"
)

// The in-app updater boots a staged release against a copy of this machine's
// state to prove it starts before anything closes (update-manager.js
// runUpdatePreflight). That daemon has this machine's id and a copy of its
// chats, jobs and schedules, so it must never act as this machine. Its wiring
// lives inline in main(), so the contract is held on the source.
func TestUpdatePreflightDaemonNeverActsAsThisMachine(t *testing.T) {
	raw, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	source := string(raw)
	if updatePreflightEnv != "WORKASS_UPDATE_PREFLIGHT" {
		t.Fatalf("the shell sets WORKASS_UPDATE_PREFLIGHT, the daemon reads %q", updatePreflightEnv)
	}
	expect := func(what, needle string) {
		t.Helper()
		if !strings.Contains(source, needle) {
			t.Fatalf("%s: main.go lost %q", what, needle)
		}
	}
	expect("read once at start", `updatePreflight := os.Getenv(updatePreflightEnv) == "1"`)
	expect("loopback only", `if *bind != "localhost" {`)
	expect("no beacon", "*beacon = false")
	expect("nothing scheduled fires", "if scheduledMessages != nil && !updatePreflight {")
	expect("no presence or discovery", "if !updatePreflight {\n\t\tstartMachinePresence(")
	expect("no fleet links", "if updatePreflight {\n\t\t// No peer link and no presence")

	drain := strings.Index(source, "acpManager.BeginUpdateDrain()")
	actors := strings.Index(source, "providerChats := newProviderChatRuntimeBeforeProviderStartupWithBackend(")
	resume := strings.Index(source, "return providerChats.ResumeActors()")
	if drain < 0 || actors < 0 || resume < 0 || drain > actors || drain > resume {
		t.Fatal("admission must be closed before any chat actor exists or resumes, so no turn, subagent, job or agent update is ever admitted")
	}
}
