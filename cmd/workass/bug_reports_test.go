package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workass/internal/acp"
)

func bugCall(t *testing.T, harness statelessMCPTestHarness, method string, params map[string]any) (map[string]any, error) {
	t.Helper()
	params = copyAnyMap(params)
	params["owner_key"], params["parent_chat_id"], params["parent_tab_id"] = "mcp-owner", "mcp-chat", "mcp-tab"
	result, err := harness.handler.agentControl.call(httptest.NewRequest(http.MethodPost, toolsPath, nil), agentControlRequest{Method: method, Params: params})
	return mapFromAnyMain(result), err
}

// The owner removed GitHub issues (2026-11-10): an agent that hits a Workass bug
// reports it to Workass, which refuses an incomplete report, keeps a complete
// one in its own ledger with redacted logs and diagnostics, and opens a NEW chat
// in the Workass checkout whose brief is the report.
func TestABugReportIsRefusedIncompleteThenLedgeredWithAFixingChat(t *testing.T) {
	harness := newStatelessMCPTestHarness(t)
	harness.handler.agentControl.selfMachineID = "m-home"
	ledger := filepath.Join(harness.manager.StateDir(), bugReportDirName)

	_, err := bugCall(t, harness, "bug.report", map[string]any{
		"operation_id": "bug-incomplete", "title": "workass run deletes its token file",
		"what_happened": "the file was gone",
	})
	if err == nil {
		t.Fatal("an incomplete report was accepted")
	}
	for _, missing := range []string{"expected", "repro_steps", "severity", "workaround", "log_paths or log_excerpts"} {
		if !strings.Contains(err.Error(), missing) {
			t.Errorf("the refusal does not name %s: %v", missing, err)
		}
	}
	if entries, _ := os.ReadDir(ledger); len(entries) != 0 {
		t.Fatalf("an incomplete report left %d entries in the ledger", len(entries))
	}

	logFile := filepath.Join(t.TempDir(), "job.log")
	if err := os.WriteFile(logFile, []byte("start\nAuthorization: Bearer sekritsekritsekrit1234567890\nreading wct_0123456789abcdef0123456789abcdef\nfile gone\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	complete := map[string]any{
		"operation_id": "bug-token-file", "title": "workass run deletes its token file",
		"what_happened": "workass run --token-file F deleted F, a chat's context file",
		"expected":      "the file is only read",
		"repro_steps":   []any{"take a chat context file F", "run workass run --token-file F -- true", "F is gone"},
		"severity":      "high", "workaround": "copy the file first",
		"log_paths":    []any{logFile},
		"log_excerpts": []any{"rm: token=abcdef0123456789 removed"},
	}
	first, err := bugCall(t, harness, "bug.report", complete)
	if err != nil {
		t.Fatal(err)
	}
	bugID := fieldString(first, "bug_id")
	thread := mapFromAnyMain(first["thread"])
	if !strings.HasPrefix(bugID, "bug-") || fieldString(first, "status") != "fixing" || fieldString(thread, "chat_id") == "" {
		t.Fatalf("report = %#v", first)
	}
	if fieldString(thread, "cwd") != repoRoot(t) {
		t.Fatalf("the fixing chat is not in the Workass checkout: %#v", thread)
	}
	if fieldString(thread, "chat_id") == "mcp-chat" {
		t.Fatal("the fixing thread is the reporting chat, not a new one")
	}

	raw, err := os.ReadFile(filepath.Join(ledger, bugID, "report.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"sekritsekritsekrit", "wct_0123456789abcdef", "abcdef0123456789"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("the saved report holds a secret %q", secret)
		}
	}
	var saved bugReport
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Thread.ChatID != fieldString(thread, "chat_id") || saved.Reporter.ChatID != "mcp-chat" || saved.Version == "" || saved.Machine == "" || len(saved.Logs) != 2 {
		t.Fatalf("saved = %#v", saved)
	}
	diagnostics, err := os.ReadFile(filepath.Join(ledger, bugID, "diagnostics.json"))
	if err != nil || !strings.Contains(string(diagnostics), "updateStatus") || !strings.Contains(string(diagnostics), "chatDiagnostics") {
		t.Fatalf("diagnostics = %s %v", diagnostics, err)
	}

	// The brief reached the new chat as its first message.
	deadline := time.Now().Add(15 * time.Second)
	for {
		read, err := harness.handler.agentControl.chats.read(map[string]any{"tab_id": fieldString(thread, "tab_id"), "chat_id": fieldString(thread, "chat_id"), "limit": 5})
		if err == nil && strings.Contains(bugJSON(read), bugID) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the fixing chat never got its brief: %v %s", err, bugJSON(read))
		}
		time.Sleep(50 * time.Millisecond)
	}

	again, err := bugCall(t, harness, "bug.report", complete)
	if err != nil || fieldString(again, "bug_id") != bugID || again["replayed"] != true || fieldString(mapFromAnyMain(again["thread"]), "chat_id") != fieldString(thread, "chat_id") {
		t.Fatalf("a retried report = %#v %v", again, err)
	}

	listed, err := bugCall(t, harness, "bug.list", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if rows, _ := listed["bugs"].([]map[string]any); len(rows) != 1 || rows[0]["bug_id"] != bugID {
		t.Fatalf("ledger = %#v", listed)
	}
	if _, err := bugCall(t, harness, "bug.update", map[string]any{"bug_id": bugID, "status": "fixed", "operation_id": "fix-1"}); err == nil {
		t.Fatal("a status change without a note was accepted")
	}
	if _, err := bugCall(t, harness, "bug.update", map[string]any{"bug_id": bugID, "status": "fixed", "note": "fixed in abc123, ships in 0.1.284", "operation_id": "fix-1"}); err != nil {
		t.Fatal(err)
	}
	one, err := bugCall(t, harness, "bug.list", map[string]any{"bug_id": bugID})
	if err != nil {
		t.Fatal(err)
	}
	if report, _ := one["bug"].(bugReport); report.Status != "fixed" || len(report.History) != 3 {
		t.Fatalf("after update = %#v", one["bug"])
	}
}

func bugJSON(value any) string {
	data, _ := json.Marshal(value)
	return string(data)
}

// The fixing chat starts on Workass's own default (Ajustes · General), the way
// a chat the owner starts by hand does. It took the first ready provider's first
// model instead, which on the owner's Mac was not the default they had chosen.
func TestTheFixingChatStartsOnWorkassNewChatDefault(t *testing.T) {
	harness := newStatelessMCPTestHarness(t)
	harness.handler.agentControl.selfMachineID = "m-home"
	// The mock has one model, whose own default effort is its last; the
	// default names its other one, which applies only with its own model.
	groups, _ := harness.manager.Catalog(context.Background())["groups"].([]acp.CatalogGroup)
	var model acp.Model
	for _, group := range groups {
		if group.ProviderID == "mock" && len(group.Models) > 0 {
			model = group.Models[0]
		}
	}
	if len(model.Efforts) < 2 {
		t.Fatalf("the mock catalog needs a model with two efforts for this test, has %#v", model)
	}
	chosen, effort := model.ModelID, model.Efforts[0]
	harness.handler.agentControl.settings = func() map[string]any {
		return map[string]any{"newChat": map[string]any{
			"providerId": "mock", "model": chosen, "effort": effort, "modeId": "bypass", "folder": "last",
		}}
	}
	logFile := filepath.Join(t.TempDir(), "job.log")
	if err := os.WriteFile(logFile, []byte("boom\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := bugCall(t, harness, "bug.report", map[string]any{
		"operation_id": "bug-default-model", "title": "the fixing chat ignores the default",
		"what_happened": "it opened on another model", "expected": "the default model",
		"repro_steps": []any{"report a bug"}, "severity": "low", "workaround": "none",
		"log_paths": []any{logFile},
	})
	if err != nil {
		t.Fatal(err)
	}
	chatID := fieldString(mapFromAnyMain(report["thread"]), "chat_id")
	state, ok := harness.runtime.Snapshot(chatID)
	if !ok {
		t.Fatalf("no fixing chat: %#v", report)
	}
	if want := chosen + "[" + effort + "]"; state.Presentation.ProviderID != "mock" || state.Presentation.CurrentModelID != want {
		t.Fatalf("the fixing chat runs %s/%s, want mock/%s", state.Presentation.ProviderID, state.Presentation.CurrentModelID, want)
	}
	// The reporting chat runs at `ask`, so the default's bypass would have asked
	// a human; it is left to the agent's own default instead.
	if state.Presentation.CurrentModeID == "bypass" {
		t.Fatal("the default permission mode raised the fixing chat above the reporter")
	}
	// A reporter at full access, the owner's own case, gets the default's mode.
	setChatModeAsHuman(t, harness, "mcp-tab", "mcp-chat", "bypass", "reporter-full-access")
	report, err = bugCall(t, harness, "bug.report", map[string]any{
		"operation_id": "bug-default-mode", "title": "the fixing chat ignores the default mode",
		"what_happened": "it opened on another mode", "expected": "the default mode",
		"repro_steps": []any{"report a bug"}, "severity": "low", "workaround": "none",
		"log_paths": []any{logFile},
	})
	if err != nil {
		t.Fatal(err)
	}
	state, _ = harness.runtime.Snapshot(fieldString(mapFromAnyMain(report["thread"]), "chat_id"))
	if state.Presentation.CurrentModeID != "bypass" {
		t.Fatalf("a full-access reporter's fixing chat runs mode %q, want the default bypass", state.Presentation.CurrentModeID)
	}
	// With no default naming a mode, the fixing chat still starts at full access
	// when its reporter holds it.
	harness.handler.agentControl.settings = func() map[string]any {
		return map[string]any{"newChat": map[string]any{"providerId": "mock", "model": chosen, "effort": effort}}
	}
	report, err = bugCall(t, harness, "bug.report", map[string]any{
		"operation_id": "bug-full-by-default", "title": "the fixing chat asks for permissions",
		"what_happened": "it opened at a lower mode", "expected": "allow everything",
		"repro_steps": []any{"report a bug"}, "severity": "low", "workaround": "none",
		"log_paths": []any{logFile},
	})
	if err != nil {
		t.Fatal(err)
	}
	state, _ = harness.runtime.Snapshot(fieldString(mapFromAnyMain(report["thread"]), "chat_id"))
	if state.Presentation.CurrentModeID != "bypass" {
		t.Fatalf("a full-access reporter's fixing chat runs mode %q, want full access (bypass)", state.Presentation.CurrentModeID)
	}
}
