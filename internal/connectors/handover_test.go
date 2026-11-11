package connectors

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// The live answer of the fiumba Grafana 13.1.0 (read-only GET through its
// tailnet proxy, 2026-11-10): two rules, both inactive, instances only in
// "Normal (NoData)". Nothing fires; both rules are learned with their folder.
func TestGrafana13LiveRulesFixture(t *testing.T) {
	body, err := os.ReadFile("testdata/grafana13-rules-live.json")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 11, 0, 10, 0, 0, time.UTC)
	result, err := parseGrafanaRules(body, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Firing) != 0 {
		t.Fatalf("Normal (NoData) instances fired: %#v", result.Firing)
	}
	want := map[string]string{
		"depthcut-camera-moved-stale": "DepthCut camera moved: calibration stale|critical",
		"depthcut-camera-move-bridge": "DepthCut camera moved: running on re-aim bridge|warning",
	}
	if len(result.Rules) != len(want) {
		t.Fatalf("rules = %#v", result.Rules)
	}
	for _, r := range result.Rules {
		if want[r.Key] != r.Title+"|"+r.Severity || r.Folder != "Fiumba Alerts" || !r.Enabled || r.SeenAt != "2026-11-11T00:10:00Z" {
			t.Fatalf("rule = %#v", r)
		}
	}

	// Polled with no token (the proxy authenticates): no Authorization header.
	sawAuth := "unset"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	clk := &clock{t: now}
	s := openTest(t, t.TempDir(), clk, &recorder{})
	c, _, err := s.Set(SetRequest{Name: "fiumba", URL: srv.URL, Mode: ModePoll})
	if err != nil {
		t.Fatal(err)
	}
	if err := pollNow(t, s, c.ID); err != nil {
		t.Fatal(err)
	}
	if sawAuth != "" {
		t.Fatalf("Authorization sent without a token: %q", sawAuth)
	}
	got := s.List()[0]
	if got.Status.State != StateOK || len(got.Rules) != 2 || len(s.Alerts()) != 0 {
		t.Fatalf("after a tokenless poll: %#v alerts %#v", got, s.Alerts())
	}
}

func TestHandoverAdoptMovesTheListenerWithoutAnnouncing(t *testing.T) {
	g := newFakeGrafana(t)
	g.set(grafanaRulesFixture("Alerting", "2026-11-10T09:55:00Z", "Pending"))
	clk := &clock{t: time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)}
	oldRec, newRec := &recorder{}, &recorder{}
	old := openTest(t, t.TempDir(), clk, oldRec)
	c, _, err := old.Set(SetRequest{Name: "fiumba", URL: "http://10.0.0.9:13000", Mode: ModePoll, Token: strp(testToken),
		Target: &Target{MachineID: "m-1", CWD: "/srv"}, Rules: []RuleToggle{{Key: "title:NoUIDRule", Enabled: false}}})
	if err != nil {
		t.Fatal(err)
	}
	// Point it at the fake for one poll (the new machine reaches it by another URL).
	if _, _, err := old.Set(SetRequest{ID: c.ID, Name: "fiumba", URL: g.srv.URL, Mode: ModePoll}); err != nil {
		t.Fatal(err)
	}
	if err := pollNow(t, old, c.ID); err != nil {
		t.Fatal(err)
	}
	firing := old.Alerts()[0]
	if _, err := old.Act(ActRequest{IDs: []string{firing.ID}, Action: "link", Link: &struct {
		MachineID string `json:"machineId"`
		TabID     string `json:"tabId"`
		ChatID    string `json:"chatId"`
	}{"m-1", "tab-1", "chat-1"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := old.Act(ActRequest{IDs: []string{firing.ID}, Action: "silence", Minutes: 30}); err != nil {
		t.Fatal(err)
	}

	if _, err := old.Handover("gf-00000000"); err != ErrUnknownConnector {
		t.Fatalf("handover of an unknown id: %v", err)
	}
	h, err := old.Handover(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if h.Token != testToken || len(h.Alerts) != 1 || h.Connector.ID != c.ID {
		t.Fatalf("handover = %#v", h)
	}
	if len(old.List()) != 1 {
		t.Fatal("handover removed the connector")
	}
	// Over the wire: the JSON round trip the client makes.
	raw, _ := json.Marshal(h)
	var req AdoptRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatal(err)
	}
	req.URL = g.srv.URL + "/"

	adopter := openTest(t, t.TempDir(), clk, newRec)
	adopter.Start(context.Background())
	adopted, err := adopter.Adopt(req)
	if err != nil {
		t.Fatal(err)
	}
	if adopted.ID != c.ID || adopted.URL != g.srv.URL || !adopted.HasToken || adopted.TokenHint != "3c4d" ||
		adopted.Target.CWD != "/srv" || adopted.Status.State != StateIdle {
		t.Fatalf("adopted = %#v", adopted)
	}
	listed, _ := json.Marshal(adopter.List())
	if strings.Contains(string(listed), testToken) {
		t.Fatal("the adopted token is on connectors:list")
	}
	a := adopter.Alerts()
	if len(a) != 1 || a[0].Link == nil || a[0].Link.ChatID != "chat-1" || a[0].SilencedUntil == "" ||
		a[0].StartsAt != "2026-11-10T09:55:00Z" || len(a[0].History) != 1 {
		t.Fatalf("imported alerts = %#v", a)
	}
	// The adopter's poller runs and does not re-announce the firing alert.
	deadline := time.Now().Add(2 * time.Second)
	for adopter.List()[0].Status.State != StateOK && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if adopter.List()[0].Status.State != StateOK {
		t.Fatal("the adopted poller never polled")
	}
	if got := newRec.announces(); len(got) != 0 {
		t.Fatalf("adopt or its poll announced: %v", got)
	}
	disabled := false
	for _, r := range adopter.List()[0].Rules {
		if r.Key == "title:NoUIDRule" && !r.Enabled {
			disabled = true
		}
	}
	if !disabled {
		t.Fatal("a switched-off rule came back on after the move")
	}
	if strings.Contains(newRec.everything(), testToken) || strings.Contains(oldRec.everything(), testToken) {
		t.Fatal("a token reached an event or a log")
	}

	// A retried identical adopt is fine; a different one under the same id is refused.
	if _, err := adopter.Adopt(req); err != nil {
		t.Fatalf("identical adopt refused: %v", err)
	}
	other := req
	other.Connector.Name = "someone else"
	if _, err := adopter.Adopt(other); err == nil {
		t.Fatal("a different connector under an existing id was adopted")
	}
	// The client removes it from the old listener last.
	if err := old.Remove(c.ID); err != nil || len(old.List()) != 0 {
		t.Fatal(err)
	}
}

func TestHandoverAdoptKeepsTheWebhookBearer(t *testing.T) {
	clk := &clock{t: time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)}
	old := openTest(t, t.TempDir(), clk, &recorder{})
	c, secret, err := old.Set(SetRequest{Name: "hook", URL: "http://127.0.0.1:13000", Mode: ModeWebhook})
	if err != nil {
		t.Fatal(err)
	}
	h, err := old.Handover(c.ID)
	if err != nil || h.WebhookSecretHash == "" || strings.Contains(h.WebhookSecretHash, secret) {
		t.Fatalf("handover = %#v %v", h, err)
	}
	rec := &recorder{}
	adopter := openTest(t, t.TempDir(), clk, rec)
	if _, err := adopter.Adopt(AdoptRequest{Connector: h.Connector, WebhookSecretHash: h.WebhookSecretHash, Alerts: h.Alerts}); err != nil {
		t.Fatal(err)
	}
	if got := postWebhook(t, adopter.WebhookHandler(), c.WebhookPath, "Bearer "+secret, webhookBody("firing", "2026-11-10T09:55:00Z", "")); got != 200 {
		t.Fatalf("the old bearer on the new listener: %d", got)
	}
	if _, err := adopter.Adopt(AdoptRequest{Connector: Connector{ID: "not-an-id", URL: "http://x"}}); err == nil {
		t.Fatal("adopt without a connector id accepted")
	}
	if _, err := adopter.Adopt(AdoptRequest{Connector: Connector{ID: "gf-12345678", URL: "http://x", Mode: ModeWebhook}, WebhookSecretHash: "plain-secret"}); err == nil {
		t.Fatal("a non-hash webhook secret accepted")
	}
}
