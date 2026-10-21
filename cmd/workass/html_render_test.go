package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workass/internal/acp"
	"workass/internal/artifacthost"
)

func TestHTMLRenderRejectsViewportHeightUnitsOnlyInCSS(t *testing.T) {
	rejected := map[string]string{
		`<style>body{min-height:100vh}</style>`:                        "100vh",
		`<style>.hero{height: 50.5dvh;}</style>`:                       "50.5dvh",
		`<div style="height:.5svh">x</div>`:                            ".5svh",
		`<div style='max-height:80LVH'>x</div>`:                        "80LVH",
		`<div style=height:30vh>x</div>`:                               "30vh",
		`<STYLE media="screen">main{height:calc(100vh - 2px)}</STYLE>`: "100vh",
		`<style>a{height:1vh}`:                                         "1vh",
	}
	for page, want := range rejected {
		if got := viewportHeightUnit(page); got != want {
			t.Errorf("viewportHeightUnit(%q) = %q, want %q", page, got, want)
		}
	}
	for _, page := range []string{
		`<p>Use 100vh with care.</p>`,
		`<!-- <style>body{height:100vh}</style> -->`,
		`<img src="data:image/png;base64,AAAA1vhBBBB">`,
		`<style>.x{background:url(data:image/png;base64,QUJD+1vh/9)}</style>`,
		`<style>.x{width:100vw;height:320px}</style>`,
		`<style>.x{height:100vhx}.y{margin:2vh-}</style>`,
		`<style>.x{height:calc(100% - 2px)}</style>`,
	} {
		if got := viewportHeightUnit(page); got != "" {
			t.Errorf("viewportHeightUnit(%q) = %q, want none", page, got)
		}
	}
}

func TestHTMLRenderSourceTakesExactlyOneBoundedUTF8Page(t *testing.T) {
	dir := t.TempDir()
	page := filepath.Join(dir, "page.html")
	if err := os.WriteFile(page, []byte("<h1>hola</h1>"), 0o600); err != nil {
		t.Fatal(err)
	}
	if source, err := htmlRenderSource(map[string]any{"path": page}); err != nil || source != "<h1>hola</h1>" {
		t.Fatalf("path source = %q, %v", source, err)
	}
	if source, err := htmlRenderSource(map[string]any{"html": "<p>x</p>"}); err != nil || source != "<p>x</p>" {
		t.Fatalf("inline source = %q, %v", source, err)
	}
	big := filepath.Join(dir, "big.html")
	if err := os.WriteFile(big, []byte(strings.Repeat("x", maxHTMLRenderSourceBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	notes := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(notes, []byte("<p>x</p>"), 0o600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "binary.html")
	if err := os.WriteFile(binary, []byte{0xff, 0xfe, 0x00, 0x41}, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, params := range map[string]map[string]any{
		"both":          {"html": "<p>x</p>", "path": page},
		"neither":       {},
		"relative path": {"path": "page.html"},
		"url":           {"path": "https://example.com/page.html"},
		"not html":      {"path": notes},
		"missing":       {"path": filepath.Join(dir, "missing.html")},
		"directory":     {"path": dir + string(filepath.Separator) + "."},
		"too large":     {"path": big},
		"inline large":  {"html": strings.Repeat("é", maxHTMLRenderSourceBytes/2+1)},
		"not utf-8":     {"path": binary},
		"blank":         {"html": "   \n"},
	} {
		if _, err := htmlRenderSource(params); err == nil {
			t.Errorf("%s: htmlRenderSource accepted %#v", name, params)
		}
	}
}

func TestHTMLRenderTitleAndReferenceStayBounded(t *testing.T) {
	if _, err := htmlRenderTitle("  "); err == nil {
		t.Fatal("empty title accepted")
	}
	if _, err := htmlRenderTitle("one\ntwo"); err == nil {
		t.Fatal("multi-line title accepted")
	}
	if _, err := htmlRenderTitle(strings.Repeat("x", maxHTMLRenderTitleBytes+1)); err == nil {
		t.Fatal("oversized title accepted")
	}
	worst := htmlRenderReferenceTitle(strings.Repeat("<", 200))
	encoded, _ := json.Marshal(worst)
	if len(encoded) > maxHTMLRenderReferenceTitle+2 {
		t.Fatalf("escaped reference title is %d bytes", len(encoded))
	}
	if got := htmlRenderReferenceTitle("Latencia del daemon · 24 h"); got != "Latencia del daemon · 24 h" {
		t.Fatalf("short title changed: %q", got)
	}
}

// The renderer strips exactly this prefix and builds its frame with the same
// policy, so the two copies can never drift apart.
func TestHTMLRenderPolicyMatchesTheRendererFrame(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "desktop", "renderer2", "src", "markdown", "html-render.ts"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(raw)
	if !strings.Contains(source, `export const HTML_RENDER_CSP = "`+htmlRenderCSP+`";`) {
		t.Fatal("daemon and renderer HTML page policies differ")
	}
	if !strings.Contains(source, `export const HTML_RENDER_STORE_MARKER = '`+htmlRenderStoreMarker+`';`) {
		t.Fatal("daemon and renderer store markers differ")
	}
	wrapped := string(wrapHTMLRenderPage("<p>x</p>"))
	if wrapped != `<!doctype html><meta http-equiv="Content-Security-Policy" content="`+htmlRenderCSP+`">`+htmlRenderStoreMarker+`<p>x</p>` {
		t.Fatalf("stored page prefix = %q", wrapped)
	}
}

func TestHTMLRenderToolIsAMutationOnlyTheChatItselfCanCall(t *testing.T) {
	var tool map[string]any
	for _, candidate := range agentMCPTools() {
		if candidate["name"] == "workass_render_html" {
			tool = candidate
		}
	}
	if tool == nil {
		t.Fatal("workass_render_html is not advertised")
	}
	description := toString(tool["description"])
	for _, want := range []string{"NO network", "512,000 bytes", "20,000px", "vh, svh, lvh or dvh", "--background", "data: URLs", "confirms"} {
		if !strings.Contains(description, want) {
			t.Errorf("description lacks %q", want)
		}
	}
	schema := mapFromAnyMain(tool["inputSchema"])
	required := schema["required"].([]string)
	if len(required) != 2 || required[0] != "title" || required[1] != "operation_id" {
		t.Fatalf("required = %#v", required)
	}
	if !workassToolMutates(agentToolKind, "workass_render_html") {
		t.Fatal("workass_render_html does not require a caller-stable operation_id")
	}
	for _, child := range agentToolsForChild() {
		if child["name"] == "workass_render_html" {
			t.Fatal("a delegated agent was offered a page it cannot show")
		}
	}
	if err := validateToolArguments(tool, map[string]any{"title": "x", "operation_id": "op", "html": "<p>x</p>", "style": 1}); err == nil {
		t.Fatal("unknown argument accepted")
	}
}

func TestAgentControlRendersHTMLPagesThroughTheArtifactRegistry(t *testing.T) {
	repo := repoRoot(t)
	workspace := t.TempDir()
	manager := acp.NewManager(acp.Options{
		RootDir: workspace, StateDir: filepath.Join(t.TempDir(), "state"),
		Provider: acp.ProviderConfig{
			ID: "mock", Command: "node", Args: []string{filepath.Join(repo, "desktop", "acp", "mock-server.mjs")},
			CWD: repo, Enabled: true, Label: "Workass Mock ACP",
		},
		DefaultProviderID: "mock", RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	stateDir := manager.StateDir()
	runtime := newTestProviderChatRuntime(t, manager, sharedSessionStore(stateDir), stateDir)
	const tabID, chatID, ownerKey = "html-tab", "html-chat", "html-owner"
	if _, err := runtime.CreateRendererChat(map[string]any{
		"tabId": tabID, "chatId": chatID, "operationId": "html-chat-create",
		"title": "HTML pages", "cwd": workspace, "providerId": "mock",
	}); err != nil {
		t.Fatalf("create actor: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if _, err := manager.NewSession(ctx, acp.SessionOptions{
		TabID: tabID, ChatID: chatID, ProviderID: "mock", CWD: workspace, AgentOwnerKey: ownerKey,
	}); err != nil {
		t.Fatalf("new owner session: %v", err)
	}
	registry, err := artifacthost.New(filepath.Join(t.TempDir(), "state"), "http://127.0.0.1:8788", "mac-test")
	if err != nil {
		t.Fatal(err)
	}
	handler := &agentControlHandler{manager: manager, chats: newChatControlCoordinator(manager, nil, runtime), artifacts: registry}
	call := func(params map[string]any) (any, error) {
		params["owner_key"], params["parent_chat_id"], params["parent_tab_id"] = ownerKey, chatID, tabID
		return handler.call(httptest.NewRequest(http.MethodPost, toolsPath, nil), agentControlRequest{Method: "html.render", Params: params})
	}
	const page = "<!doctype html><title>Cobertura</title><h1>Cobertura por paquete</h1><p>Total 76 %</p>"

	result, err := call(map[string]any{"operation_id": "page-once", "title": "Cobertura por paquete", "html": page})
	if err != nil {
		t.Fatalf("html.render: %v", err)
	}
	encoded, _ := json.Marshal(result)
	if !strings.HasPrefix(string(encoded), `{"htmlRender":{"id":"`) || len(encoded) > 600 {
		t.Fatalf("result must lead with the reference and fit the kept output head: %s", encoded)
	}
	reference := mapFromAnyMain(mapFromAnyMain(result)["htmlRender"])
	id, urlPath := toString(reference["id"]), toString(reference["urlPath"])
	if toString(reference["title"]) != "Cobertura por paquete" || urlPath != artifacthost.PathPrefix+"/@mac-test/"+id+"/" {
		t.Fatalf("reference = %#v", reference)
	}

	recorder := httptest.NewRecorder()
	registry.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, urlPath, nil))
	body, _ := io.ReadAll(recorder.Result().Body)
	if recorder.Code != http.StatusOK || string(body) != string(wrapHTMLRenderPage(page)) {
		t.Fatalf("served page = %d %q", recorder.Code, body)
	}
	if csp := recorder.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "sandbox") {
		t.Fatalf("served page CSP header = %q", csp)
	}

	again, err := call(map[string]any{"operation_id": "page-once", "title": "Cobertura por paquete", "html": page})
	if err != nil || toString(mapFromAnyMain(mapFromAnyMain(again)["htmlRender"])["id"]) != id {
		t.Fatalf("retry = %#v, %v; want the same page", again, err)
	}
	if _, err := call(map[string]any{"operation_id": "page-once", "title": "Cobertura por paquete", "html": "<p>otra</p>"}); err == nil || !strings.Contains(err.Error(), "different request") {
		t.Fatalf("reused operation with another page = %v", err)
	}

	file := filepath.Join(t.TempDir(), "dashboard.html")
	if err := os.WriteFile(file, []byte("<h1>Latencia</h1>"), 0o600); err != nil {
		t.Fatal(err)
	}
	fromFile, err := call(map[string]any{"operation_id": "page-file", "title": "Latencia", "path": file})
	if err != nil || toString(mapFromAnyMain(mapFromAnyMain(fromFile)["htmlRender"])["id"]) == id {
		t.Fatalf("page from path = %#v, %v", fromFile, err)
	}

	if _, err := call(map[string]any{"operation_id": "page-vh", "title": "Hero", "html": "<style>main{height:100vh}</style><main></main>"}); err == nil || !strings.Contains(err.Error(), "viewport height unit (100vh)") {
		t.Fatalf("vh page = %v", err)
	}
	if state, ok := runtime.Snapshot(chatID); !ok {
		t.Fatal("actor disappeared")
	} else if _, journaled := externalBrowserMutationEntry(state, "page-vh"); journaled {
		t.Fatal("a rejected page was journaled")
	}
	if _, err := call(map[string]any{"operation_id": "page-no-title", "html": page}); err == nil {
		t.Fatal("page without a title accepted")
	}
	if _, err := handler.call(httptest.NewRequest(http.MethodPost, toolsPath, nil), agentControlRequest{Method: "html.render", Params: map[string]any{
		"owner_key": "someone-else", "parent_chat_id": chatID, "parent_tab_id": tabID, "operation_id": "page-stranger", "title": "x", "html": page,
	}}); err == nil {
		t.Fatal("a caller that does not own the chat rendered a page")
	}
}
