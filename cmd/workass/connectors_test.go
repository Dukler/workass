package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workass/internal/connectors"
	"workass/internal/wire"
)

func TestConnectorChannelsRoundTripWithoutSecrets(t *testing.T) {
	svc, err := connectors.Open(connectors.Options{StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)
	hub := wire.NewHub()
	registerDaemonHandlers(hub, t.TempDir(), nil, daemonOptions{Connectors: svc})
	const token = "glsa_channelTestTokenValue_0123456789abcdef"

	raw, err := hub.Invoke("connectors:set", []any{map[string]any{
		"name": "fiumba", "url": "http://127.0.0.1:13000", "mode": "webhook", "token": token,
		"notify": "critical", "target": map[string]any{"machineId": "", "cwd": "/srv"}, "ruleTargets": map[string]any{}, "guidePattern": "",
	}})
	if err != nil {
		t.Fatal(err)
	}
	reply := raw.(map[string]any)
	secret, _ := reply["webhookSecret"].(string)
	connector := reply["connector"].(connectors.Connector)
	if secret == "" || !connector.HasToken || connector.TokenHint != "cdef" {
		t.Fatalf("set reply = %#v", reply)
	}
	listed, err := hub.Invoke("connectors:list", nil)
	if err != nil {
		t.Fatal(err)
	}
	wireJSON, _ := json.Marshal(listed)
	if strings.Contains(string(wireJSON), token) || strings.Contains(string(wireJSON), secret) {
		t.Fatalf("connectors:list carries a secret: %s", wireJSON)
	}

	handler := connectorWebhookHandler(svc)
	body := `{"status":"firing","alerts":[{"status":"firing","labels":{"alertname":"HighCPU","severity":"critical","instance":"duklerfin"},"annotations":{},"startsAt":"2026-11-10T09:55:00Z","endsAt":"0001-01-01T00:00:00Z","generatorURL":"http://g/alerting/grafana/uid1/view","fingerprint":"abc123","values":{"A":1}}]}`
	req := httptest.NewRequest(http.MethodPost, connector.WebhookPath, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+secret)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("webhook = %d %s", w.Code, w.Body.String())
	}
	raw, err = hub.Invoke("alerts:list", nil)
	if err != nil {
		t.Fatal(err)
	}
	alerts := raw.(map[string]any)["alerts"].([]connectors.Alert)
	if len(alerts) != 1 || alerts[0].ID != connector.ID+":abc123" {
		t.Fatalf("alerts = %#v", alerts)
	}
	raw, err = hub.Invoke("alerts:act", []any{map[string]any{"ids": []any{alerts[0].ID}, "action": "link", "link": map[string]any{"machineId": "", "tabId": "t1", "chatId": "c1"}}})
	if err != nil {
		t.Fatal(err)
	}
	if acted := raw.(map[string]any)["alerts"].([]connectors.Alert); len(acted) != 1 || acted[0].Link == nil || acted[0].Link.ChatID != "c1" {
		t.Fatalf("act = %#v", raw)
	}
	raw, err = hub.Invoke("connectors:rotate-secret", []any{map[string]any{"id": connector.ID}})
	if err != nil || raw.(map[string]any)["webhookSecret"] == secret {
		t.Fatalf("rotate = %#v %v", raw, err)
	}
	if _, err := hub.Invoke("connectors:remove", []any{map[string]any{"id": connector.ID}}); err != nil {
		t.Fatal(err)
	}
	if raw, _ := hub.Invoke("alerts:list", nil); len(raw.(map[string]any)["alerts"].([]connectors.Alert)) != 0 {
		t.Fatal("alerts outlived their connector")
	}

	// Without a service the reads are empty and the rest refuses.
	bare := wire.NewHub()
	registerConnectorHandlers(bare, nil)
	if raw, err := bare.Invoke("connectors:list", nil); err != nil || len(raw.(map[string]any)["connectors"].([]connectors.Connector)) != 0 {
		t.Fatalf("bare list = %#v %v", raw, err)
	}
	if _, err := bare.Invoke("connectors:set", []any{map[string]any{}}); err == nil {
		t.Fatal("set without a service succeeded")
	}
}
