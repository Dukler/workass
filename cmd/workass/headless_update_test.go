package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"workass/internal/headlessupdate"
	"workass/internal/wire"
)

func newTestHeadlessService(t *testing.T) *headlessUpdateService {
	t.Helper()
	root := t.TempDir()
	install := filepath.Join(root, "runtime")
	if err := os.MkdirAll(install, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(install, "workass"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	service := newHeadlessUpdateService(headlessupdate.Options{
		Executable: filepath.Join(install, "workass"),
		StateDir:   filepath.Join(root, "state"),
		Version:    "0.1.232",
		GOOS:       "linux",
		GOARCH:     "amd64",
		FeedURL:    "https://example.invalid/workass-linux-amd64-release.json",
		Env:        func(string) string { return "" },
		ReadFile: func(name string) ([]byte, error) {
			if name == "/proc/self/cgroup" {
				return []byte("0::/user.slice/user-1000.slice/user@1000.service/app.slice/workass.service\n"), nil
			}
			return nil, os.ErrNotExist
		},
		Run: func(string, ...string) ([]byte, error) { return []byte("on-failure\n"), nil },
	}, "m-duklerver", "duklerver")
	return service
}

func TestHeadlessUpdateTargetsNamesThisMachineOnly(t *testing.T) {
	service := newTestHeadlessService(t)
	result, err := service.Targets(context.Background())
	if err != nil {
		t.Fatalf("targets: %v", err)
	}
	payload := mapFromAnyMain(result)
	if payload["selfMachineId"] != "m-duklerver" || payload["remoteAvailable"] != false {
		t.Fatalf("targets = %#v", payload)
	}
	targets, ok := payload["targets"].([]any)
	if !ok || len(targets) != 1 {
		t.Fatalf("targets list = %#v", payload["targets"])
	}
	target := mapFromAnyMain(targets[0])
	if target["machineId"] != "m-duklerver" || target["name"] != "duklerver" ||
		target["local"] != true || target["updaterMcpSupported"] != true ||
		target["currentVersion"] != "0.1.232" || target["strategy"] != headlessupdate.Strategy {
		t.Fatalf("target = %#v", target)
	}
}

func TestHeadlessUpdateStatusAnswersOnlyTheExactMachine(t *testing.T) {
	service := newTestHeadlessService(t)
	result, err := service.Status(context.Background(), map[string]any{"machine_id": "m-duklerver"})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	payload := mapFromAnyMain(result)
	diagnostics := mapFromAnyMain(payload["diagnostics"])
	state := mapFromAnyMain(diagnostics["state"])
	if payload["headless"] != true || diagnostics["strategy"] != headlessupdate.Strategy ||
		state["currentVersion"] != "0.1.232" || state["supported"] != true {
		t.Fatalf("status = %#v", payload)
	}
	// A status read is a pure read: it never reaches the network, which is why
	// an unreachable feed host still answers.
	if state["availableVersion"] != nil {
		t.Fatalf("status must not fetch the feed: %#v", state["availableVersion"])
	}
	for _, params := range []map[string]any{{}, {"machine_id": "m-other"}} {
		if _, err := service.Status(context.Background(), params); err == nil {
			t.Fatalf("status answered for %#v", params)
		}
	}
	if _, err := service.Apply(context.Background(), map[string]any{"machine_id": "m-other"}); err == nil {
		t.Fatal("apply answered for another machine")
	}
}

func TestHeadlessDaemonOwnsTheUpdateChannelsWithoutARenderer(t *testing.T) {
	service := newTestHeadlessService(t)
	hub := wire.NewHub()
	// The renderer router is present but has no shell behind it, exactly as on a
	// headless host. The headless updater must answer anyway.
	registerAgentUpdateWireHandlers(hub, &stubAgentChatRemoteRouter{result: map[string]any{"renderer": true}}, service, "m-duklerver", nil)

	result, err := hub.Invoke(agentUpdateStatusChannel, []any{map[string]any{"machine_id": "m-duklerver"}})
	if err != nil {
		t.Fatalf("status invoke: %v", err)
	}
	payload := mapFromAnyMain(result)
	if payload["headless"] != true || payload["renderer"] == true {
		t.Fatalf("status came from the wrong half: %#v", payload)
	}
	if _, err := hub.Invoke(agentUpdateStatusChannel, []any{map[string]any{"machine_id": "m-other"}}); err == nil {
		t.Fatal("the wire channel answered for another machine")
	}
	// Activation still needs the whole authorized sentence, on this path too.
	_, err = hub.Invoke(agentUpdateApplyChannel, []any{map[string]any{
		"machine_id": "m-duklerver", "expected_current_version": "0.1.232",
		"expected_target_version": "0.1.233", "authorization": "update m-duklerver from 0.1.232 to 0.1.233",
		"operation_id": "op-duklerver-233",
	}})
	if err == nil || !strings.Contains(err.Error(), "feed") && !strings.Contains(err.Error(), "example.invalid") && !strings.Contains(err.Error(), "lookup") {
		t.Fatalf("apply reached the network and failed there, as expected, but with: %v", err)
	}
	if _, err := hub.Invoke(agentUpdateApplyChannel, []any{map[string]any{
		"machine_id": "m-duklerver", "expected_current_version": "0.1.232",
		"expected_target_version": "0.1.233", "authorization": "please update", "operation_id": "op-duklerver-233",
	}}); err == nil {
		t.Fatal("an unauthorized activation was accepted")
	}
}

func TestPackagedInstallKeepsTheRendererUpdaterPath(t *testing.T) {
	// The packaged Mac and Windows shells launch their daemon with --headless
	// too. Without a Linux headless updater behind it, both channels must
	// still reach the renderer's transactional UpdateManager unchanged.
	router := &stubAgentChatRemoteRouter{result: map[string]any{"renderer": true}}
	hub := wire.NewHub()
	registerAgentUpdateWireHandlers(hub, router, nil, "machine-mac", nil)
	result, err := hub.Invoke(agentUpdateStatusChannel, []any{map[string]any{"machine_id": "machine-mac"}})
	if err != nil || mapFromAnyMain(result)["renderer"] != true || router.method != "update.local.status" {
		t.Fatalf("result=%#v method=%q err=%v", result, router.method, err)
	}
}
