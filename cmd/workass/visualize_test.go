package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"workass/internal/artifacthost"
	"workass/internal/wire"
)

func TestVisualizeHostCapturesAllowedExecutorHTML(t *testing.T) {
	stateDir := t.TempDir()
	visualRoot := filepath.Join(filepath.Dir(stateDir), "visualizations", "turn-1")
	if err := os.MkdirAll(visualRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(visualRoot, "signals.html")
	if err := os.WriteFile(source, []byte(`<div id="chart">hello</div><script>document.title = "chart"</script>`), 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := artifacthost.New(stateDir, "http://127.0.0.1:8788")
	if err != nil {
		t.Fatal(err)
	}
	store := newSessionStore(filepath.Join(stateDir, sessionStateFilename))
	created, err := store.AgentCreateChat("Visualization", t.TempDir(), "mock", "mock", "ask", true)
	if err != nil {
		t.Fatal(err)
	}
	tabID, chatID := fieldString(created, "tabId"), fieldString(created, "chatId")
	hub := wire.NewHub()
	registerVisualizeHandler(hub, registry, store, stateDir)

	result, err := hub.Invoke("visualize:host", []any{map[string]any{
		"tabId": tabID, "chatId": chatID, "path": source, "mode": "wide", "title": "Signals",
	}})
	if err != nil {
		t.Fatal(err)
	}
	receipt, ok := result.(map[string]any)
	if !ok || fieldString(receipt, "urlPath") == "" || fieldString(receipt, "mode") != "wide" {
		t.Fatalf("visualization receipt = %#v", result)
	}

	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, fieldString(receipt, "urlPath")+fieldString(receipt, "entry"), nil)
	response := httptest.NewRecorder()
	registry.ServeHTTP(response, request)
	body := response.Body.String()
	if response.Code != http.StatusOK || !strings.Contains(body, "Content-Security-Policy") || !strings.Contains(body, "id=\"chart\"") {
		t.Fatalf("captured visualization status=%d body=%q", response.Code, body)
	}

	outside := filepath.Join(t.TempDir(), "outside.html")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := hub.Invoke("visualize:host", []any{map[string]any{
		"tabId": tabID, "chatId": chatID, "path": outside,
	}}); err == nil || !strings.Contains(err.Error(), "inside Workass visualizations") {
		t.Fatalf("outside visualization was accepted: %v", err)
	}
}
