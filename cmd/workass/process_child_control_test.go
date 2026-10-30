package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workass/internal/acp"
	"workass/internal/fleetid"
	"workass/internal/fleetlog"
	"workass/internal/procchild"
)

// The Agent API surface for process children, driven end to end through the
// real control handler, a real chat actor and a real ACP engine.
//
// The unit tests prove the supervisor and the verdicts; this proves the wiring
// nobody else touches: a registration that is fenced by the calling chat and
// authorized by its owner key, an origin stamp resolved from the running turn
// so the row is actor-owned rather than attached to a convenient one, a monitor
// registered at the home at registration time, and list/read/cancel reaching
// the machine that holds the pids.

func processControlRuntime(t *testing.T) (*acp.Manager, *agentControlHandler, string, string, string) {
	t.Helper()
	root := repoRoot(t)
	const tabID, chatID, ownerKey = "proc-child-tab", "proc-child-chat", "proc-child-owner"
	stateDir := filepath.Join(t.TempDir(), "state")
	store := sharedSessionStore(stateDir)
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: stateDir, RuntimeProfile: "dev",
		Provider: acp.ProviderConfig{
			ID: "codex", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")},
			CWD: root, Enabled: true, Label: "Codex ACP fixture",
			Env: map[string]string{
				"WORKASS_MOCK_ACP_SESSION_STORE":     filepath.Join(stateDir, "proc-mock-native.json"),
				"WORKASS_MOCK_ACP_STABLE_TURN_INPUT": "1",
			},
		},
		DefaultProviderID: "codex", RSSSampleInterval: time.Hour,
		SpawnedWorkReconcileInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	t.Cleanup(cancel)
	runtime := newTestProviderChatRuntime(t, manager, store, stateDir)
	if _, err := runtime.CreateRendererChat(map[string]any{
		"tabId": tabID, "chatId": chatID, "operationId": "proc-child-chat-create", "focus": false,
		"title": "Process children", "titleLocked": true, "cwd": root,
		"providerId": "codex", "currentModelId": "mock-deterministic", "currentModeId": "ask",
	}); err != nil {
		t.Fatalf("create the owning actor: %v", err)
	}
	info, err := runtime.Select(ctx, acp.SessionOptions{
		TabID: tabID, ChatID: chatID, ProviderID: "codex", CWD: root, AgentOwnerKey: ownerKey,
	})
	if err != nil {
		t.Fatalf("new owner session: %v", err)
	}
	if _, err := runtime.Start(ctx, map[string]any{
		"kind": "app-chat", "tabId": tabID, "chatId": chatID, "sessionId": info.SessionID,
		"providerId": "codex", "cwd": root, "operationId": "proc-owner-turn",
		"userMessageId": "proc-owner-user", "assistantMessageId": "proc-owner-assistant",
		"prompt": "establish the durable owner for a supervised job",
	}, "human"); err != nil {
		t.Fatalf("start the owner turn: %v", err)
	}
	waitProviderChatIdle(t, runtime, chatID, 5*time.Second)
	handler := &agentControlHandler{
		manager: manager, chats: newChatControlCoordinator(manager, nil, runtime),
		selfMachineID: "m-local",
	}
	return manager, handler, tabID, chatID, ownerKey
}

func callProcessControl(t *testing.T, handler *agentControlHandler, method string, params map[string]any) map[string]any {
	t.Helper()
	result, err := handler.call(httptest.NewRequest(http.MethodPost, toolsPath, nil), agentControlRequest{
		Method: method, Params: params,
	})
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	return mapFromAnyMain(result)
}

func TestProcessChildRegistrationIsActorOwnedAndMonitoredFromTheInstantItExists(t *testing.T) {
	manager, handler, tabID, chatID, ownerKey := processControlRuntime(t)
	stateDir := manager.StateDir()
	childID, err := procchild.IDForOperation("proc-control-op-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := procchild.EnsureDir(stateDir, childID); err != nil {
		t.Fatal(err)
	}
	outputDir := filepath.Join(stateDir, "external-work")
	if err := os.MkdirAll(outputDir, 0o700); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(outputDir, childID+".output")
	if err := os.WriteFile(output, []byte("line one\nline two\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A home with real logs and a real monitor registry, so the monitor the
	// registration creates is the one a watcher would actually read.
	held, err := fleetlog.OpenMonitors(t.TempDir(), fleetlog.MonitorsHeld)
	if err != nil {
		t.Fatal(err)
	}
	logs, err := fleetlog.Open(t.TempDir(), fleetlog.Options{Logf: t.Logf, Owed: held.MinAcked})
	if err != nil {
		t.Fatal(err)
	}
	handler.fleetHome = newFleetHome("m-local", logs, held, nil, t.Logf)

	registered := callProcessControl(t, handler, "process.register", map[string]any{
		"owner_key": ownerKey, "parent_chat_id": chatID, "parent_tab_id": tabID,
		"operation_id": "proc-control-op-1", "work_id": childID,
		"label": "retrain ranker", "command": "python train.py --epochs 8",
		"output_file": output, "pid": os.Getpid(), "process_group": os.Getpid(),
		"isolation": string(procchild.GroupProcessGroup), "resource": "gpu0",
		"max_runtime_ms": (6 * time.Hour).Milliseconds(),
		"stale_after_ms": (40 * time.Minute).Milliseconds(),
	})
	if fieldString(registered, "workId") != childID || registered["created"] != true {
		t.Fatalf("process.register = %#v", registered)
	}
	monitor := mapFromAnyMain(registered["monitor"])
	target := fieldString(monitor, "targetId")
	if target == "" {
		t.Fatalf("a registration must create the monitor at once, got %#v", registered)
	}
	id, err := fleetid.Parse(target)
	if err != nil || id.Type != fleetid.Process || id.Local != childID || id.Home != "m-local" {
		// The identifier law: the global id CONTAINS today's work id.
		t.Fatalf("monitor target = %q (%v)", target, err)
	}
	rows := held.ByTarget(target)
	if len(rows) != 1 || rows[0].WatcherChatID != chatID || rows[0].OriginOperationID == "" {
		// At registration, not at completion: a job that finishes while its
		// reply is in flight has somewhere to report, and the origin stamp is
		// what lets the stopped-turn suppression be evaluated at the chat.
		t.Fatalf("the home must hold one monitor stamped with the asking turn, got %#v", rows)
	}

	// The row is actor-owned: it survives the projection, which excludes any
	// row whose immutable origin the exact actor did not accept.
	items := manager.ListSpawnedWork(tabID, chatID)
	if len(items) != 1 || !items[0].Supervised() || items[0].Status != "running" {
		t.Fatalf("spawned work = %#v", items)
	}
	if items[0].Resource != "gpu0" || items[0].MaxRuntimeMs == 0 || items[0].Isolation == "" {
		t.Fatalf("the declared facts must reach the row, got %#v", items[0])
	}

	listed := callProcessControl(t, handler, "process.list", map[string]any{
		"owner_key": ownerKey, "parent_chat_id": chatID, "parent_tab_id": tabID,
	})
	children, _ := listed["children"].([]acp.ProcessChildView)
	if len(children) != 1 || children[0].WorkID != childID || children[0].Resource != "gpu0" {
		t.Fatalf("process.list = %#v", listed)
	}
	if fieldString(listed, "machine_id") != "m-local" {
		t.Fatalf("a machine-scoped read must name the machine it answered for, got %#v", listed)
	}

	read := callProcessControl(t, handler, "process.read", map[string]any{
		"owner_key": ownerKey, "parent_chat_id": chatID, "parent_tab_id": tabID,
		"work_id": childID, "tail_chars": 2048,
	})
	if tail := fieldString(read, "output_tail"); !strings.Contains(tail, "line two") {
		t.Fatalf("process.read must return the output tail, got %q", tail)
	}

	cancelled := callProcessControl(t, handler, "process.cancel", map[string]any{
		"owner_key": ownerKey, "parent_chat_id": chatID, "parent_tab_id": tabID,
		"operation_id": "proc-control-cancel-1", "work_id": childID, "reason": "the operator asked",
	})
	if cancelled["already"] != false {
		t.Fatalf("process.cancel = %#v", cancelled)
	}
	stop, err := os.ReadFile(procchild.StopPath(stateDir, childID))
	if err != nil || !strings.Contains(string(stop), "the operator asked") {
		t.Fatalf("the cancel must be written where the supervisor will see it, got %q / %v", stop, err)
	}
}

func TestProcessChildRegistrationRefusesWhatItCannotStandBehind(t *testing.T) {
	manager, handler, tabID, chatID, ownerKey := processControlRuntime(t)
	stateDir := manager.StateDir()
	childID, err := procchild.IDForOperation("proc-control-refusals")
	if err != nil {
		t.Fatal(err)
	}
	outputDir := filepath.Join(stateDir, "external-work")
	if err := os.MkdirAll(outputDir, 0o700); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(outputDir, childID+".output")
	if err := os.WriteFile(output, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	base := map[string]any{
		"owner_key": ownerKey, "parent_chat_id": chatID, "parent_tab_id": tabID,
		"operation_id": "proc-control-refusals", "work_id": childID,
		"label": "a job", "output_file": output, "pid": os.Getpid(),
	}
	with := func(mutate func(map[string]any)) map[string]any {
		copied := map[string]any{}
		for key, value := range base {
			copied[key] = value
		}
		mutate(copied)
		return copied
	}
	request := httptest.NewRequest(http.MethodPost, toolsPath, nil)
	for name, params := range map[string]map[string]any{
		"no supervisor pid":    with(func(p map[string]any) { delete(p, "pid") }),
		"a borrowed owner key": with(func(p map[string]any) { p["owner_key"] = "someone-elses-key" }),
		"another chat":         with(func(p map[string]any) { p["chat_id"] = "a-different-chat" }),
		// The path validator admits the temp root and this machine's own
		// external-work directory, and nothing else: a registration cannot
		// point the daemon at a file somewhere it has no business writing.
		"an output path outside the allowed roots": with(func(p map[string]any) {
			p["output_file"] = "/etc/workass-process-child-escape.output"
		}),
		"a relative output path": with(func(p map[string]any) {
			p["output_file"] = "escape.output"
		}),
		"an owner that is not a global id": with(func(p map[string]any) { p["owner"] = "just-a-chat-id" }),
	} {
		if _, err := handler.call(request, agentControlRequest{Method: "process.register", Params: params}); err == nil {
			t.Fatalf("%s must be refused", name)
		}
	}
	if items := manager.ListSpawnedWork(tabID, chatID); len(items) != 0 {
		t.Fatalf("a refused registration must leave no row, got %#v", items)
	}
}

func TestProcessChildMethodsAreMachineScopedAndRegistrationIsNot(t *testing.T) {
	// The combination this table had no class for, and the reason supervised
	// jobs could not be expressed before the fleet runtime: machine-scoped in
	// execution, chat-scoped in ownership.
	for _, method := range []string{"process.list", "process.read", "process.cancel"} {
		machine, remote, err := remoteAgentMethodTarget(method, map[string]any{"machine_id": "m-91be04"})
		if err != nil || !remote || machine != "m-91be04" {
			t.Fatalf("%s must be addressable on another machine, got %q/%v/%v", method, machine, remote, err)
		}
		if _, remote, err := remoteAgentMethodTarget(method, map[string]any{}); err != nil || remote {
			t.Fatalf("%s with no machine_id must stay local, got %v/%v", method, remote, err)
		}
	}
	// A registration carries a pid only its own machine can probe, so a
	// forwarded one would be the evidence-less row P4 forbids. The refusal
	// names what to do instead.
	_, _, err := remoteAgentMethodTarget("process.register", map[string]any{"machine_id": "m-91be04"})
	if err == nil {
		t.Fatal("process.register must refuse another machine")
	}
	if !strings.Contains(err.Error(), "workass run") {
		t.Fatalf("the refusal must name what to do instead, got %q", err)
	}
}
