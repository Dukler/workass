package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"workass/internal/procchild"
)

// The live P4 failure, as the test that would have caught it.
//
// Four supervised jobs ran on duklerver for a chat on the Mac on 2026-10-30.
// All four returned ok with the right owner, all four produced perfectly
// correct events in the outbox — and `delivered_at` was NULL on every single
// row, no notice reached the owning chat, and the owner's
// `workass_list_process_children` answered with nothing.
//
// The first cause was one empty string. The supervisor stated `role: ""`,
// which is not "no role" but a value outside the work|service enum, so this
// daemon's own argument validator refused the WHOLE report — instantly, every
// time, before the registration was reached. The repo's two-daemon e2e passed
// throughout because it built the report payload by hand: it spoke a dialect
// the daemon accepts while the real `workass run` spoke one it refuses.
//
// So the assertion is specifically that the SUPERVISOR'S OWN payload, from the
// supervisor's own constructor, is accepted by the tool the supervisor calls.
func TestTheSupervisorsOwnReportPayloadIsAcceptedByTheToolItCalls(t *testing.T) {
	var tool map[string]any
	for _, candidate := range agentMCPTools() {
		if candidate["name"] == "workass_register_process_child" {
			tool = candidate
		}
	}
	if tool == nil {
		t.Fatal("workass_register_process_child is not in the catalog")
	}
	for _, role := range []string{"", "work", "service"} {
		arguments := procchild.ReportArguments(procchild.Report{
			Spec: procchild.Spec{
				OperationID: "op-1", Label: "train the model", Role: role,
				Owner: "wa:m-mac/chat/app-chat-1", Command: []string{"python", "train.py"},
				Resource: "gpu0", MaxRuntime: time.Hour, StaleAfter: 10 * time.Minute,
			},
			ChildID: "xwLiveRepro01", OutputFile: "/data/workass/state/external-work/xwLiveRepro01.output",
			DoneFile:      "/data/workass/state/external-work/xwLiveRepro01.output.done",
			SupervisorPID: 4242, ProcessGroup: 4243, Group: procchild.GroupProcessGroup,
			Heartbeat: procchild.HeartbeatInterval, ReportGap: 40 * time.Second,
			Events: []procchild.Event{
				{Seq: 1, Kind: procchild.KindCreated, At: time.Now().UTC()},
				{Seq: 2, Kind: procchild.KindSettled, At: time.Now().UTC(), Payload: map[string]any{"outcome": "settled"}},
			},
		})
		// Through JSON, because that is how the arguments actually arrive: the
		// supervisor marshals them and the daemon decodes them before the
		// validator ever sees them.
		arguments = roundTripToolArgumentsForTest(t, arguments)
		if err := validateToolArguments(tool, arguments); err != nil {
			t.Fatalf("the supervisor's report with role %q was refused by the tool it calls: %v", role, err)
		}
		if role == "" {
			if _, stated := arguments["role"]; stated {
				t.Fatalf("an undeclared role must be absent, not empty: %#v", arguments["role"])
			}
			continue
		}
		if arguments["role"] != role {
			t.Fatalf("role %q did not survive: %#v", role, arguments["role"])
		}
	}
}

// TestProcessStartCapturesTheSupervisorsOwnStreamNextToTheJobOutput is the
// third cause, which hid the first two: `process.start` gave the supervisor no
// stdout and no stderr and passed --quiet, so "NOT REPORTING" — the one
// sentence the design promises is never missing — was written to /dev/null,
// and a job nobody had been told about was indistinguishable from a healthy
// one from every side.
func TestProcessStartCapturesTheSupervisorsOwnStreamNextToTheJobOutput(t *testing.T) {
	stateDir := t.TempDir()
	output, err := procchild.OutputPath(stateDir, "xwLiveRepro02", "")
	if err != nil {
		t.Fatal(err)
	}
	log := procchild.SupervisorLogPath(output)
	if log != output+".supervisor.log" {
		t.Fatalf("the supervisor's stream must sit next to the job's output, got %q beside %q", log, output)
	}
	file, err := openSupervisorLog(log, "xwLiveRepro02", "train the model")
	if err != nil {
		t.Fatalf("open the supervisor log: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	stamped, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"xwLiveRepro02", "train the model"} {
		if !strings.Contains(string(stamped), expected) {
			t.Fatalf("the log must name whose it is, got %q", stamped)
		}
	}
}

// roundTripToolArgumentsForTest puts one argument map through the encoding the
// wire performs, so a test asserts what the validator will really be handed.
func roundTripToolArgumentsForTest(t *testing.T, arguments map[string]any) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(arguments)
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var decoded map[string]any
	if err := decoder.Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}
