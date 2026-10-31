package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"workass/internal/fleetclient"
	"workass/internal/headlessupdate"
)

// Live bug (2026-10-31): workass_get_update_status with a remote machine_id
// answered "Workass remote-chat MCP router has no local renderer" for a
// headless Windows fleet peer (dukler-laptop) even though
// workass_list_update_targets showed it ready and paired, while the identical
// call for a headless Linux peer (duklerver) worked.
//
// The cause: update.status's remote path always went straight to the ASKING
// daemon's own renderer relay — never over the daemon-to-daemon mesh that
// every other Agent API method already uses to reach another machine with no
// renderer anywhere (fleet_mesh.go). A headless daemon has no renderer by
// definition, so EVERY cross-machine update.status call was unreachable
// unless the asking daemon happened to have Electron open locally; duklerver
// "working" was a self-addressed call (hosted on duklerver's own daemon), not
// evidence the remote path functioned.
//
// This proves the fix with a real second daemon and a refusing relay: the
// mesh reaches the home's own agentControlHandler, which answers for itself
// exactly as a self-addressed call does — a headless target through its own
// headless updater, runningJobs included, with the renderer relay never
// touched.
func TestUpdateStatusReachesAHeadlessPeerOverTheMeshWithNoRendererAnywhere(t *testing.T) {
	secret := newFleetTestSecret(t)
	harness := newStatelessMCPTestHarness(t)
	harness.handler.agentControl.selfMachineID = "m-home"
	executable := filepath.Join(t.TempDir(), "workass")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fixture executable: %v", err)
	}
	harness.handler.agentControl.headlessUpdates = newHeadlessUpdateService(headlessupdate.Options{
		Executable: executable, StateDir: t.TempDir(), Version: "0.1.249", GOOS: "linux",
		RunningJobs: func() []headlessupdate.RunningJob {
			return []headlessupdate.RunningJob{{WorkID: "job-1", Label: "a running job", Protected: true}}
		},
	}, "m-home", "home")
	home := serveMeshHome(t, "m-home", secret, harness.handler.agentControl)

	mesh := newMeshDialer(t, "m-away", secret)
	if err := mesh.AddRoute(home.machineID, "home", fleetclient.KindLAN, home.address, fleetclient.SourceManual); err != nil {
		t.Fatalf("add route: %v", err)
	}
	relay := &refusingRelay{t: t}
	router := newFleetMeshRouter(mesh, relay, "m-away", t.Logf)

	result, err := router.Call(context.Background(), "update.status", map[string]any{
		"machine_id": home.machineID,
	})
	if err != nil {
		t.Fatalf("read update status on another home with no renderer anywhere: %v", err)
	}
	if relay.calls.Load() != 0 {
		t.Fatalf("the renderer relay was used %d time(s)", relay.calls.Load())
	}
	status := mapFromAnyMain(result)
	if !boolFieldValue(status, "headless") || !boolFieldValue(status, "local") {
		t.Fatalf("home update status = %#v", status)
	}
	diagnostics := mapFromAnyMain(status["diagnostics"])
	state := mapFromAnyMain(diagnostics["state"])
	if toString(state["currentVersion"]) != "0.1.249" {
		t.Fatalf("home update diagnostics state = %#v", state)
	}
	jobs := mapFromAnyMain(diagnostics["runningJobs"])
	if toString(jobs["total"]) != "1" {
		t.Fatalf("home update status lost its running jobs: %#v", diagnostics)
	}
}

// The self-addressed case must keep working exactly as before: a packaged
// install with no headless updater asks its OWN renderer, never the fleet's
// callEveryPeer union-of-every-machine read that chat.list uses for the same
// empty/self machine_id shape.
func TestSelfAddressedUpdateStatusNeverBecomesAFleetWideRead(t *testing.T) {
	relay := &refusingRelay{t: t, allow: true, result: map[string]any{"ok": true, "via": "relay", "machineId": "m-self"}}
	router := newFleetMeshRouter(nil, relay, "m-self", t.Logf)

	result, err := router.Call(context.Background(), "update.status", map[string]any{"machine_id": "m-self"})
	if err != nil {
		t.Fatalf("self-addressed update status: %v", err)
	}
	if fieldString(mapFromAnyMain(result), "via") != "relay" {
		t.Fatalf("self-addressed update status did not use the renderer relay: %#v", result)
	}
	if relay.calls.Load() != 1 {
		t.Fatalf("relay calls = %d, want exactly 1", relay.calls.Load())
	}
}
