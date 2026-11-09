package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"workass/internal/acp"
)

// Ajustes · Permisos: the owner's rules live in the settings blob and hold at the
// tool boundary. The mock provider's modes are `ask` (asks) and `bypass` (full).

func TestAgentPermissionsSurviveTheSettingsBlob(t *testing.T) {
	defaults := map[string]any{"subagentRaise": true, "browserNavigate": "allow", "browserDownload": "allow", "browserUpload": "allow"}
	if got := normalizeSettings(nil)[agentPermissionsSettingsKey]; !reflect.DeepEqual(got, defaults) {
		t.Fatalf("default agent permissions = %#v", got)
	}
	saved := normalizeSettings(map[string]any{agentPermissionsSettingsKey: map[string]any{
		"subagentRaise": false, "browserNavigate": "ASK", "browserDownload": "block", "browserUpload": "ask", "extra": 1,
	}})[agentPermissionsSettingsKey]
	want := map[string]any{"subagentRaise": false, "browserNavigate": "ask", "browserDownload": "block", "browserUpload": "ask"}
	if !reflect.DeepEqual(saved, want) {
		t.Fatalf("normalized agent permissions = %#v", saved)
	}
	// Downloads have no "ask": a download starts after the call that caused it.
	odd := normalizeSettings(map[string]any{agentPermissionsSettingsKey: map[string]any{
		"subagentRaise": "no", "browserNavigate": 3, "browserDownload": "ask", "browserUpload": "maybe",
	}})[agentPermissionsSettingsKey]
	if !reflect.DeepEqual(odd, defaults) {
		t.Fatalf("malformed agent permissions = %#v", odd)
	}

	store := newAppSettingsStore(filepath.Join(t.TempDir(), "app-settings.json"))
	if _, err := store.set(map[string]any{agentPermissionsSettingsKey: want}); err != nil {
		t.Fatal(err)
	}
	if got := agentPermissionsFromSettings(store.get()); got.SubagentRaise || got.BrowserNavigate != "ask" || got.BrowserDownload != "block" || got.BrowserUpload != "ask" {
		t.Fatalf("read back = %#v", got)
	}
	var handler *agentControlHandler
	if got := handler.agentPermissions(); got != defaultAgentPermissionSettings() {
		t.Fatalf("no handler = %#v", got)
	}
}

func ownerOnlyRules(rules agentPermissionSettings) func() map[string]any {
	return func() map[string]any {
		return map[string]any{agentPermissionsSettingsKey: rules.settingsValue()}
	}
}

// With the switch off, a raise of another agent is the owner's at any caller level,
// and the owner's finer ladder applies: asking sits below editing on its own.
func TestOwnerOnlyRaiseRule(t *testing.T) {
	caller := func(provider, mode string) permissionCaller {
		return permissionCaller{providerID: provider, modeID: mode, level: acp.PermissionLevel(provider, mode), ownerOnly: true}
	}
	for _, test := range []struct {
		name           string
		from           permissionCaller
		provider, mode string
		current        string
		delegated      bool
		raises         bool
	}{
		{"full parent raising a child", caller("claude", "bypassPermissions"), "claude", "bypassPermissions", "default", true, true},
		{"full parent starting a child at its own level", caller("claude", "bypassPermissions"), "claude", "bypassPermissions", "", true, false},
		{"full parent starting a narrower child", caller("claude", "bypassPermissions"), "claude", "plan", "", true, false},
		{"asking parent starting an editing child", caller("claude", "default"), "claude", "acceptEdits", "", true, true},
		{"asking parent starting an asking child elsewhere", caller("claude", "default"), "codex", "read-only", "", true, false},
		{"lowering a child", caller("claude", "default"), "claude", "plan", "bypassPermissions", true, false},
		{"keeping a child", caller("claude", "plan"), "claude", "acceptEdits", "acceptEdits", true, false},
		{"a mode nobody places", caller("claude", "bypassPermissions"), "omp", "default", "", true, true},
		{"a provider with no modes", caller("claude", "default"), "workass-agent", "", "", true, false},
		// The caller's own chat keeps the ordinary ceiling.
		{"a full chat lowering itself", caller("claude", "bypassPermissions"), "claude", "plan", "bypassPermissions", false, false},
		{"an asking chat at an editing level of its own", caller("claude", "default"), "claude", "acceptEdits", "default", false, false},
	} {
		change := permissionChange{providerID: test.provider, modeID: test.mode, delegated: test.delegated}
		if test.current != "" {
			change.reconfigures, change.currentProviderID, change.currentModeID = true, test.provider, test.current
		}
		if got := permissionChangeRaises(test.from, change); got != test.raises {
			t.Errorf("%s: raises = %v, want %v", test.name, got, test.raises)
		}
	}
	forwarded := permissionCaller{kind: permissionCallerForwarded, level: fleetLimitEdit, ownerOnly: true}
	if permissionChangeRaises(forwarded, permissionChange{providerID: "claude", modeID: "acceptEdits", delegated: true}) {
		t.Error("a forwarded edit-limited start at edit raised")
	}
	if !permissionChangeRaises(forwarded, permissionChange{providerID: "claude", modeID: "bypassPermissions", delegated: true}) {
		t.Error("a forwarded edit-limited start at full did not raise")
	}
}

func TestOwnerOnlyRaiseAsksEvenAFullAccessParent(t *testing.T) {
	h := newStatelessMCPTestHarness(t)
	setChatModeAsHuman(t, h, "mcp-tab", "mcp-chat", "bypass", "human-parent-full-owner-only")
	ceilingSeedChat(t, h, "child-tab", "child-chat", "ask")
	rules := defaultAgentPermissionSettings()
	rules.SubagentRaise = false
	h.handler.agentControl.settings = ownerOnlyRules(rules)

	raise := func(op string) <-chan ceilingCall {
		return ceilingToolAsync(h, "mcp-owner", "mcp-chat", "mcp-tab", "workass_configure_chat",
			map[string]any{"operation_id": op, "tab_id": "child-tab", "chat_id": "child-chat", "mode_id": "bypass"})
	}
	denied := raise("owner-only-raise-denied")
	card := waitRaiseCard(t, h, denied)
	if text := fieldString(mapFromAnyMain(card["question"]), "question"); !strings.Contains(text, "Only you raise") || !strings.Contains(text, "bypass") {
		t.Fatalf("the card does not say why it asks: %q", text)
	}
	answerRaiseCard(t, h, card, permissionRaiseDeny)
	if call := finishCall(t, denied); !strings.Contains(call.errorText(), permissionDeniedCode) {
		t.Fatalf("a denied raise = %#v", call.response)
	}
	if mode := ceilingChatMode(t, h, "child-chat"); mode != "ask" {
		t.Fatalf("a denied raise left the child on %q", mode)
	}
	allowed := raise("owner-only-raise-allowed")
	answerRaiseCard(t, h, waitRaiseCard(t, h, allowed), permissionRaiseAllow)
	if call := finishCall(t, allowed); call.errorText() != "" || ceilingChatMode(t, h, "child-chat") != "bypass" {
		t.Fatalf("an approved raise = %#v on %q", call.response, ceilingChatMode(t, h, "child-chat"))
	}

	// Starting a child where it would start anyway, and lowering one, never ask.
	for name, arguments := range map[string]map[string]any{
		"workass_spawn_subagent": {"operation_id": "owner-only-spawn-inherit", "task": "reply with the deterministic mock result", "label": "same"},
		"workass_configure_chat": {"operation_id": "owner-only-lower", "tab_id": "child-tab", "chat_id": "child-chat", "mode_id": "ask"},
	} {
		if call := ceilingTool(t, h, name, arguments); call.errorText() != "" || len(raiseCards(h)) != 0 {
			t.Fatalf("%s with the switch off = %#v, cards %d", name, call.response, len(raiseCards(h)))
		}
	}

	// Allowing a subagent's own request is the owner's too.
	call := ceilingTool(t, h, "workass_decide_subagent_permission", map[string]any{
		"operation_id": "owner-only-decide", "subagent_id": "wa-subagent-1", "decision": "allow",
	})
	if !strings.Contains(call.errorText(), permissionCeilingCode) || !strings.Contains(call.errorText(), "only they raise") {
		t.Fatalf("an allow with the switch off = %#v", call.response)
	}
}

// The browser rules, against a fake shell that records what reached it.
type browserRuleShell struct {
	mu         sync.Mutex
	dispatched []map[string]any
}

func (s *browserRuleShell) calls(method string) []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []map[string]any{}
	for _, call := range s.dispatched {
		if call["method"] == method {
			out = append(out, call)
		}
	}
	return out
}

func installBrowserRuleShell(t *testing.T, h statelessMCPTestHarness) *browserRuleShell {
	t.Helper()
	controlFile := filepath.Join(t.TempDir(), "browser-control.json")
	if err := os.WriteFile(controlFile, []byte(browserTestDescriptor), 0o600); err != nil {
		t.Fatal(err)
	}
	shell := &browserRuleShell{}
	h.handler.browserControlFile = controlFile
	h.handler.browserClient = &http.Client{Transport: browserRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		var payload map[string]any
		_ = json.NewDecoder(request.Body).Decode(&payload)
		shell.mu.Lock()
		shell.dispatched = append(shell.dispatched, payload)
		shell.mu.Unlock()
		reply := map[string]any{"id": payload["id"]}
		switch payload["method"] {
		case "browser.controlStatus":
			reply["result"] = map[string]any{"ready": true, "controller": true, "instanceId": browserTestInstanceID}
		case "browser.list":
			reply["result"] = map[string]any{"tabs": []any{map[string]any{"id": 7, "url": "https://upload.example.org/form", "active": true}}}
		case "browser.snapshot":
			reply["result"] = map[string]any{"snapshot_id": "snap-1"}
		default:
			reply["operationId"], reply["requestDigest"], reply["receipt"] = payload["operationId"], payload["requestDigest"], true
			reply["result"] = map[string]any{"ok": true}
		}
		body, _ := json.Marshal(reply)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}, nil
	})}
	return shell
}

func browserCards(h statelessMCPTestHarness) []map[string]any {
	cards := []map[string]any{}
	for _, raw := range h.manager.PendingPermissions() {
		card := mapFromAnyMain(raw)
		if fieldString(mapFromAnyMain(card["question"]), "questionId") == browserPermissionID {
			cards = append(cards, card)
		}
	}
	return cards
}

func waitBrowserCard(t *testing.T, h statelessMCPTestHarness, done <-chan ceilingCall) map[string]any {
	t.Helper()
	deadline := time.After(8 * time.Second)
	for {
		if cards := browserCards(h); len(cards) == 1 {
			return cards[0]
		}
		select {
		case call := <-done:
			t.Fatalf("the call ended with no card for the owner: %#v", call.response)
		case <-deadline:
			t.Fatal("no browser card appeared")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func browserRules(h statelessMCPTestHarness, navigate, download, upload string) {
	rules := defaultAgentPermissionSettings()
	rules.BrowserNavigate, rules.BrowserDownload, rules.BrowserUpload = navigate, download, upload
	h.handler.agentControl.settings = ownerOnlyRules(rules)
}

func TestBlockedAgentBrowserReachesNoPage(t *testing.T) {
	h := newStatelessMCPTestHarness(t)
	shell := installBrowserRuleShell(t, h)
	browserRules(h, browserPolicyBlock, browserPolicyAllow, browserPolicyAllow)
	for name, arguments := range map[string]map[string]any{
		"workass_browser_snapshot": {},
		"workass_browser_navigate": {"operation_id": "blocked-navigate", "url": "https://example.com"},
	} {
		if call := ceilingTool(t, h, name, arguments); !strings.Contains(call.errorText(), browserBlockedCode) {
			t.Fatalf("%s while blocked = %#v", name, call.response)
		}
	}
	if len(shell.calls("browser.snapshot"))+len(shell.calls("browser.navigate")) != 0 {
		t.Fatal("a blocked call reached the shell")
	}
	if call := ceilingTool(t, h, "workass_browser_close", map[string]any{"operation_id": "blocked-close"}); call.errorText() != "" {
		t.Fatalf("closing while blocked = %#v", call.response)
	}
}

func TestAskingBrowserAsksOncePerSiteAndCarriesTheDownloadRule(t *testing.T) {
	h := newStatelessMCPTestHarness(t)
	shell := installBrowserRuleShell(t, h)
	browserRules(h, browserPolicyAsk, browserPolicyBlock, browserPolicyAllow)
	navigate := func(op, address string) <-chan ceilingCall {
		return ceilingToolAsync(h, "mcp-owner", "mcp-chat", "mcp-tab", "workass_browser_navigate", map[string]any{"operation_id": op, "url": address})
	}

	first := navigate("ask-site-first", "https://docs.example.com/a")
	card := waitBrowserCard(t, h, first)
	if text := fieldString(mapFromAnyMain(card["question"]), "question"); !strings.Contains(text, "docs.example.com") {
		t.Fatalf("the card does not name the site: %q", text)
	}
	if len(shell.calls("browser.navigate")) != 0 {
		t.Fatal("the page opened before the owner answered")
	}
	answerRaiseCard(t, h, card, browserPermissionYes)
	if call := finishCall(t, first); call.errorText() != "" {
		t.Fatalf("an allowed navigation = %#v", call.response)
	}
	sent := shell.calls("browser.navigate")
	if len(sent) != 1 || fieldString(mapFromAnyMain(sent[0]["params"]), browserDownloadPolicy) != browserPolicyBlock {
		t.Fatalf("the shell did not get the download rule: %#v", sent)
	}

	// The same site again in this chat goes through.
	if call := finishCall(t, navigate("ask-site-again", "https://docs.example.com/b")); call.errorText() != "" || len(browserCards(h)) != 0 {
		t.Fatalf("a second page of an allowed site = %#v", call.response)
	}

	other := navigate("ask-site-other", "https://elsewhere.example.net/")
	answerRaiseCard(t, h, waitBrowserCard(t, h, other), browserPermissionNo)
	if call := finishCall(t, other); !strings.Contains(call.errorText(), browserDeniedCode) {
		t.Fatalf("a refused site = %#v", call.response)
	}
	if got := len(shell.calls("browser.navigate")); got != 2 {
		t.Fatalf("a refused site reached the shell: %d navigations", got)
	}
	// A retry reads the answer it already has.
	if call := ceilingTool(t, h, "workass_browser_navigate", map[string]any{"operation_id": "ask-site-other", "url": "https://elsewhere.example.net/"}); !strings.Contains(call.errorText(), browserDeniedCode) || len(browserCards(h)) != 0 {
		t.Fatalf("a retried refusal = %#v", call.response)
	}
}

func TestUploadRules(t *testing.T) {
	h := newStatelessMCPTestHarness(t)
	shell := installBrowserRuleShell(t, h)
	upload := map[string]any{"operation_id": "upload-blocked", "tab_id": 7, "selector": "#file", "paths": []any{"/tmp/report.pdf"}}

	browserRules(h, browserPolicyAllow, browserPolicyAllow, browserPolicyBlock)
	if call := ceilingTool(t, h, "workass_browser_upload", upload); !strings.Contains(call.errorText(), browserBlockedCode) || len(shell.calls("browser.upload")) != 0 {
		t.Fatalf("a blocked upload = %#v", call.response)
	}

	browserRules(h, browserPolicyAllow, browserPolicyAllow, browserPolicyAsk)
	upload["operation_id"] = "upload-asked"
	done := ceilingToolAsync(h, "mcp-owner", "mcp-chat", "mcp-tab", "workass_browser_upload", upload)
	card := waitBrowserCard(t, h, done)
	if text := fieldString(mapFromAnyMain(card["question"]), "question"); !strings.Contains(text, "report.pdf") || !strings.Contains(text, "upload.example.org") || strings.Contains(text, "/tmp") {
		t.Fatalf("the upload card = %q", text)
	}
	answerRaiseCard(t, h, card, browserPermissionYes)
	if call := finishCall(t, done); call.errorText() != "" || len(shell.calls("browser.upload")) != 1 {
		t.Fatalf("an allowed upload = %#v", call.response)
	}
}
