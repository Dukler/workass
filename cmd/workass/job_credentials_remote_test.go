//go:build !windows

package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"workass/internal/toolcli"
)

// bug-20261110-12916e73: a timer on one machine must start supervised jobs on
// another fleet machine, owned by the chat that issued its credential. The
// job-start credential was fenced to workass_register_process_child, so a remote
// start, the "is it already running?" list and a stop were all refused, and the
// only way left was a saved copy of the chat's whole context: every tool, dying
// with the session. The credential now does exactly that work for its own chat
// and nothing more.
func TestATimersCredentialStartsListsReadsAndStopsItsChatsJobsOnAnotherMachine(t *testing.T) {
	fleet := newHostedJobFleetAsking(t, jobCredentialOptions(t))
	asking := fleet.asking
	issued := callProcessControl(t, asking.handler.agentControl, "job_credential.issue", map[string]any{
		"owner_key": "mcp-owner", "parent_chat_id": "mcp-chat", "parent_tab_id": "mcp-tab",
		"label": "launchd remote nightly", "operation_id": "issue-remote-nightly",
	})
	config, err := toolcli.ReadConfig(fieldString(issued, "token_file"))
	if err != nil {
		t.Fatal(err)
	}
	if config.Owner != hostedJobOwner {
		t.Fatalf("credential owner = %q", config.Owner)
	}
	call := func(name string, arguments map[string]any) (int, map[string]any) {
		t.Helper()
		return timerCredentialRequest(t, asking, http.MethodPost, config.Credential, "", "", map[string]any{
			"name": name, "arguments": arguments,
		})
	}
	succeed := func(name string, arguments map[string]any) map[string]any {
		t.Helper()
		status, response := call(name, arguments)
		if status != http.StatusOK || response["error"] != nil {
			t.Fatalf("%s: status=%d %#v", name, status, response)
		}
		return mapFromAnyMain(response["result"])
	}
	refused := func(name string, arguments map[string]any, want string) {
		t.Helper()
		status, response := call(name, arguments)
		message, _ := response["error"].(string)
		if status == http.StatusOK && message == "" {
			t.Fatalf("%s was allowed: %#v", name, response)
		}
		if want != "" && !strings.Contains(message, want) {
			t.Fatalf("%s refused with %q, want it to say %q", name, message, want)
		}
	}

	// Another chat's job on the same machine: the credential must not see or stop it.
	if _, err := fleet.hosting.handler.agentControl.registerStartedProcessChild(startedProcessChild{
		ChildID: "xwotherchat0001", Label: "someone else's job", Owner: "wa:m-third/chat/theirs",
		SupervisorPID: os.Getpid(), Command: []string{"sleep", "60"},
		OutputFile:    filepath.Join(fleet.hosting.manager.StateDir(), "external-work", "xwotherchat0001.output"),
		SupervisorLog: filepath.Join(fleet.hosting.manager.StateDir(), "external-work", "xwotherchat0001.log"),
		Params:        map[string]any{},
	}); err != nil {
		t.Fatal(err)
	}

	cwd := t.TempDir()
	release := filepath.Join(cwd, "release")
	started := succeed("workass_start_process_child", map[string]any{
		"machine_id": "m-home", "label": "remote nightly", "cwd": cwd, "operation_id": "remote-nightly-1",
		"command": []any{"/bin/sh", "-c", holdUntilReleased(release) + "; echo built"},
	})
	workID := fieldString(started, "workId")
	if workID == "" || fieldString(started, "owner") != hostedJobOwner {
		t.Fatalf("start = %#v", started)
	}
	awaitHostedJob(t, "the job to run on its host", func() bool {
		row, found := fleet.hostsRow(workID)
		return found && row.Status == "running"
	})

	// "Is it already running?" on that machine: its own job, and only its own.
	listed := succeed("workass_list_process_children", map[string]any{"machine_id": "m-home"})
	children := anySlice(listed["children"])
	if len(children) != 1 || fieldString(mapFromAnyMain(children[0]), "work_id") != workID {
		t.Fatalf("the credential's list on m-home = %#v", listed)
	}
	for _, raw := range anySlice(listed["resource_queues"]) {
		if fieldString(mapFromAnyMain(raw), "owner") != hostedJobOwner {
			t.Fatalf("the credential saw another chat's queue entry: %#v", raw)
		}
	}
	// And addressed to nobody, the chat's own jobs here and elsewhere.
	listed = succeed("workass_list_process_children", map[string]any{})
	found := false
	for _, raw := range anySlice(listed["children"]) {
		row := mapFromAnyMain(raw)
		if fieldString(row, "work_id") == "xwotherchat0001" {
			t.Fatalf("another chat's job was listed: %#v", row)
		}
		found = found || fieldString(row, "work_id") == workID
	}
	if !found {
		t.Fatalf("the unaddressed list lacks the chat's remote job: %#v", listed)
	}

	read := succeed("workass_read_process_child", map[string]any{"machine_id": "m-home", "work_id": workID})
	if fieldString(mapFromAnyMain(read["child"]), "work_id") != workID {
		t.Fatalf("read = %#v", read)
	}
	refused("workass_read_process_child", map[string]any{"machine_id": "m-home", "work_id": "xwotherchat0001"}, "another chat")
	refused("workass_cancel_process_child", map[string]any{
		"machine_id": "m-home", "work_id": "xwotherchat0001", "operation_id": "cancel-other",
	}, "another chat")
	if row, ok := fleet.hostsRow("xwotherchat0001"); !ok || row.StopRequested {
		t.Fatalf("another chat's job was stopped: %#v", row)
	}

	cancelled := succeed("workass_cancel_process_child", map[string]any{
		"machine_id": "m-home", "work_id": workID, "operation_id": "cancel-remote-nightly-1",
	})
	if cancelled["ok"] != true {
		t.Fatalf("cancel = %#v", cancelled)
	}
	awaitHostedJob(t, "the cancelled job to end", func() bool {
		row, found := fleet.hostsRow(workID)
		return !found || row.Status != "running"
	})

	// A job on THIS machine is still `workass run --token-file`, and the rest of
	// the catalog stays closed.
	refused("workass_start_process_child", map[string]any{
		"machine_id": "m-away", "label": "local", "cwd": cwd, "operation_id": "local-1", "command": []any{"/bin/true"},
	}, "workass run")
	for _, name := range []string{"workass_list_chats", "workass_send_chat_message", "workass_issue_job_credential", "workass_spawn_subagent"} {
		if status, _ := call(name, map[string]any{}); status != http.StatusForbidden {
			t.Fatalf("the credential reached %s: %d", name, status)
		}
	}
	if status, _ := timerCredentialRequest(t, asking, http.MethodGet, config.Credential, "", "", nil); status != http.StatusUnauthorized {
		t.Fatalf("it read the catalog: %d", status)
	}

	// Revoked, it starts nothing anywhere.
	callProcessControl(t, asking.handler.agentControl, "job_credential.revoke", map[string]any{
		"owner_key": "mcp-owner", "parent_chat_id": "mcp-chat", "parent_tab_id": "mcp-tab",
		"credential_id": fieldString(issued, "credential_id"), "operation_id": "revoke-remote-nightly",
	})
	if status, _ := call("workass_start_process_child", map[string]any{
		"machine_id": "m-home", "label": "after revoke", "cwd": cwd, "operation_id": "remote-nightly-2", "command": []any{"/bin/true"},
	}); status != http.StatusUnauthorized {
		t.Fatalf("a revoked credential started a remote job: %d", status)
	}
}
