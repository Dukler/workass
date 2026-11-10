package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workass/internal/acp"
	"workass/internal/procchild"
	"workass/internal/toolcli"
)

func jobCredentialHarness(t *testing.T) statelessMCPTestHarness {
	t.Helper()
	harness := newStatelessMCPTestHarnessWithOptions(t, nil, jobCredentialOptions(t))
	harness.handler.agentControl.selfMachineID = "m-home"
	return harness
}

// jobCredentialOptions gives a test daemon the tool endpoint a credential file
// has to name.
func jobCredentialOptions(t *testing.T) func(*acp.Options) {
	t.Helper()
	return func(options *acp.Options) {
		options.WorkassToolsOrigin = "https://tools.localhost:18799"
		options.WorkassToolsCAFile = filepath.Join(t.TempDir(), "ca.pem")
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		options.WorkassToolsCommand = executable
	}
}

// A timer has no chat turn, so it had no supported way to start a job owned by
// a chat (former issue #4, 2026-11-10). A job-start credential is that way: the
// chat issues it, a timer runs `workass run --token-file` with it, and the job
// is the chat's — and the credential can do nothing else.
func TestATimersJobStartCredentialStartsJobsOwnedByItsChatAndNothingElse(t *testing.T) {
	harness := jobCredentialHarness(t)
	const tabID, chatID, ownerKey = "mcp-tab", "mcp-chat", "mcp-owner"
	issued := callProcessControl(t, harness.handler.agentControl, "job_credential.issue", map[string]any{
		"owner_key": ownerKey, "parent_chat_id": chatID, "parent_tab_id": tabID,
		"label": "launchd nightly build", "operation_id": "issue-nightly",
	})
	file := fieldString(issued, "token_file")
	if fieldString(issued, "owner") != "wa:m-home/chat/mcp-chat" || file == "" {
		t.Fatalf("issued %#v", issued)
	}
	info, err := os.Stat(file)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the credential file must exist with mode 0600: %v %v", info, err)
	}
	config, err := toolcli.ReadConfig(file)
	if err != nil {
		t.Fatal(err)
	}
	if config.ChatID != "" || config.TabID != "" || config.Owner != "wa:m-home/chat/mcp-chat" || config.StateDir != harness.manager.StateDir() {
		t.Fatalf("credential file = %#v", config)
	}
	again := callProcessControl(t, harness.handler.agentControl, "job_credential.issue", map[string]any{
		"owner_key": ownerKey, "parent_chat_id": chatID, "parent_tab_id": tabID,
		"label": "launchd nightly build", "operation_id": "issue-nightly",
	})
	if fieldString(again, "credential_id") != fieldString(issued, "credential_id") || again["replayed"] != true {
		t.Fatalf("a retried issue minted another credential: %#v", again)
	}

	// `workass run` reads it and does not delete it: the timer needs it tomorrow.
	reporter, _, err := procchild.ResolveCredential(file)
	if err != nil || reporter == nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatal("a job-start credential was deleted when read")
	}
	if owner := procchild.CredentialOwner(reporter); owner != "wa:m-home/chat/mcp-chat" {
		t.Fatalf("the run's owner = %q", owner)
	}

	const operation = "nightly-build-2026-11-10"
	workID, err := procchild.IDForOperation(operation)
	if err != nil {
		t.Fatal(err)
	}
	report := func(owner string) map[string]any {
		return map[string]any{"name": "workass_register_process_child", "arguments": procchild.ReportArguments(procchild.Report{
			Spec:    procchild.Spec{OperationID: operation, Label: "nightly build", Owner: owner, Command: []string{"make"}},
			ChildID: workID, OutputFile: filepath.Join(harness.manager.StateDir(), "external-work", workID+".output"),
			SupervisorPID: os.Getpid(), Heartbeat: procchild.HeartbeatInterval,
			Events: []procchild.Event{{Seq: 1, At: time.Now().UTC(), Kind: procchild.KindCreated, Payload: map[string]any{"label": "nightly build"}}},
		})}
	}
	status, response := timerCredentialRequest(t, harness, http.MethodPost, config.Credential, "", "", report(config.Owner))
	if status != http.StatusOK || response["error"] != nil {
		t.Fatalf("the timer's job report: status=%d %#v", status, response)
	}
	owned := false
	for _, item := range harness.manager.ListSpawnedWork(tabID, chatID) {
		if item.ID == workID {
			owned = true
		}
	}
	if !owned {
		t.Fatalf("the timer's job is not the chat's: %#v", harness.manager.ListSpawnedWork(tabID, chatID))
	}

	if _, response := timerCredentialRequest(t, harness, http.MethodPost, config.Credential, "", "", report("wa:m-home/chat/other-chat")); response["error"] == nil {
		t.Fatal("a job-start credential started a job for another chat")
	}
	if status, _ := timerCredentialRequest(t, harness, http.MethodPost, config.Credential, chatID, tabID, report(config.Owner)); status != http.StatusUnauthorized {
		t.Fatalf("it acted as a chat: %d", status)
	}
	if status, _ := timerCredentialRequest(t, harness, http.MethodGet, config.Credential, "", "", nil); status != http.StatusUnauthorized {
		t.Fatalf("it read the catalog: %d", status)
	}
	for _, name := range []string{"workass_list_chats", "workass_issue_job_credential"} {
		if status, _ := timerCredentialRequest(t, harness, http.MethodPost, config.Credential, "", "", map[string]any{
			"name": name, "arguments": map[string]any{},
		}); status != http.StatusForbidden {
			t.Fatalf("it reached %s: %d", name, status)
		}
	}

	listed := callProcessControl(t, harness.handler.agentControl, "job_credential.list", map[string]any{
		"owner_key": ownerKey, "parent_chat_id": chatID, "parent_tab_id": tabID,
	})
	if rows, _ := listed["credentials"].([]map[string]any); len(rows) != 1 || rows[0]["credential"] != nil || rows[0]["last_used_at"] == nil {
		t.Fatalf("list = %#v", listed)
	}
	revoked := callProcessControl(t, harness.handler.agentControl, "job_credential.revoke", map[string]any{
		"owner_key": ownerKey, "parent_chat_id": chatID, "parent_tab_id": tabID,
		"credential_id": fieldString(issued, "credential_id"), "operation_id": "revoke-nightly",
	})
	if revoked["revoked"] != true {
		t.Fatalf("revoke = %#v", revoked)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatal("a revoked credential's file is still there")
	}
	if status, _ := timerCredentialRequest(t, harness, http.MethodPost, config.Credential, "", "", report(config.Owner)); status != http.StatusUnauthorized {
		t.Fatalf("a revoked credential still reports: %d", status)
	}
}

// timerCredentialRequest is one tool call over the real endpoint, carrying the
// timer's credential and the chat headers given.
func timerCredentialRequest(t *testing.T, harness statelessMCPTestHarness, method, token, chatID, tabID string, body map[string]any) (int, map[string]any) {
	t.Helper()
	encoded, _ := json.Marshal(body)
	request, err := http.NewRequest(method, harness.server.URL+toolsPath, bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	if chatID != "" {
		request.Header.Set("X-Workass-Chat-ID", chatID)
		request.Header.Set("X-Workass-Tab-ID", tabID)
	}
	request.Header.Set("Content-Type", "application/json")
	reply, err := harness.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer reply.Body.Close()
	var response map[string]any
	_ = json.NewDecoder(reply.Body).Decode(&response)
	return reply.StatusCode, response
}

// Through the tool endpoint an agent actually calls, the answer names the file
// to hand the timer and the id to revoke it by. Both were masked by the key-name
// redactor, so a chat was told token_file "[redacted]".
func TestTheIssuedCredentialFileAndIDReachTheAgentUnmasked(t *testing.T) {
	harness := jobCredentialHarness(t)
	status, response := harness.request(t, http.MethodPost, map[string]any{
		"name": "workass_issue_job_credential", "arguments": map[string]any{"label": "launchd x", "operation_id": "issue-unmasked"},
	})
	issued := mapFromAnyMain(response["result"])
	file, id := fieldString(issued, "token_file"), fieldString(issued, "credential_id")
	if status != http.StatusOK || !strings.HasPrefix(id, "jsc-") || !filepath.IsAbs(file) {
		t.Fatalf("issue answered %d %#v", status, response)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("token_file does not name the file: %v", err)
	}
	config, err := toolcli.ReadConfig(file)
	if err != nil || strings.Contains(bugJSON(response), config.Credential) {
		t.Fatalf("the token itself must never be in the answer: %v", err)
	}
	_, response = harness.request(t, http.MethodPost, map[string]any{"name": "workass_list_job_credentials", "arguments": map[string]any{}})
	rows := anySlice(mapFromAnyMain(response["result"])["credentials"])
	if len(rows) != 1 || fieldString(mapFromAnyMain(rows[0]), "credential_id") != id || fieldString(mapFromAnyMain(rows[0]), "token_file") != file {
		t.Fatalf("list answered %#v", response)
	}
	if strings.Contains(bugJSON(response), config.Credential) {
		t.Fatal("the list carried the token")
	}
}
