package connectors

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testToken = "glsa_Zx9pQ2rT7vW1yB4nM6kL8jH3gF5dS0aE_1a2b3c4d"

// grafanaRulesFixture is Grafana 11-13's /api/prometheus/grafana/api/v1/rules
// answer: groups with folder title (file) and folderUid, alerting rules with
// uid, labels, health, state, totals and their instances; one recording rule.
func grafanaRulesFixture(cpuState, cpuActiveAt string, diskState string) string {
	cpuAlerts := ""
	if cpuState != "" {
		cpuAlerts = fmt.Sprintf(`{
            "labels": {"alertname": "HighCPU", "grafana_folder": "Infra", "instance": "duklerfin:9100", "job": "node", "severity": "critical"},
            "annotations": {"summary": "CPU above 90%% on duklerfin", "description": "token=hunter2hunter2hunter2 leaked in a template"},
            "state": %q,
            "activeAt": %q,
            "value": "9.5e+01"
          }`, cpuState, cpuActiveAt)
	}
	return fmt.Sprintf(`{
  "status": "success",
  "data": {
    "groups": [
      {
        "name": "node",
        "file": "Infra",
        "folderUid": "f1a2b3c4",
        "rules": [
          {
            "state": "firing",
            "name": "HighCPU",
            "query": "[{\"refId\":\"A\",\"queryType\":\"\",\"model\":{\"expr\":\"100 - avg(rate(node_cpu_seconds_total{mode=\\\"idle\\\"}[5m]))*100\"}}]",
            "queriedDatasourceUIDs": ["prom1"],
            "duration": 300,
            "labels": {"severity": "critical"},
            "health": "ok",
            "type": "alerting",
            "lastEvaluation": "2026-11-10T10:00:00Z",
            "evaluationTime": 0.012,
            "uid": "cpu-rule-uid",
            "folderUid": "f1a2b3c4",
            "isPaused": false,
            "alerts": [%s],
            "totals": {"alerting": 1},
            "totalsFiltered": {"alerting": 1}
          },
          {
            "state": "pending",
            "name": "DiskFilling",
            "query": "[]",
            "labels": {"severity": "warning"},
            "health": "ok",
            "type": "alerting",
            "uid": "disk-rule-uid",
            "folderUid": "f1a2b3c4",
            "alerts": [{
              "labels": {"alertname": "DiskFilling", "grafana_folder": "Infra", "host": "duklerfin", "mountpoint": "/"},
              "annotations": {"summary": "disk"},
              "state": %q,
              "activeAt": "2026-11-10T09:59:00Z",
              "value": "[ var='A' labels={host=duklerfin} value=91.5 ], [ var='C' labels={host=duklerfin} value=1 ]"
            }]
          },
          {
            "name": "node:cpu:rate5m",
            "query": "rate(node_cpu_seconds_total[5m])",
            "health": "ok",
            "type": "recording",
            "uid": "rec-uid"
          }
        ],
        "totals": {"alerting": 1, "pending": 1},
        "interval": 60,
        "lastEvaluation": "2026-11-10T10:00:00Z",
        "evaluationTime": 0.02
      },
      {
        "name": "legacy",
        "file": "Old",
        "rules": [
          {"name": "NoUIDRule", "type": "alerting", "state": "inactive", "health": "ok", "labels": null, "alerts": null}
        ]
      }
    ],
    "totals": {"alerting": 1, "pending": 1, "inactive": 1}
  }
}`, cpuAlerts, diskState)
}

type fakeGrafana struct {
	mu     sync.Mutex
	body   string
	status int
	hits   atomic.Int64
	srv    *httptest.Server
}

func newFakeGrafana(t *testing.T) *fakeGrafana {
	g := &fakeGrafana{status: 200}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.hits.Add(1)
		if r.URL.Path != GrafanaRulesPath {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			w.WriteHeader(http.StatusUnauthorized)
			// Grafana's real answer; this one also echoes the presented header.
			fmt.Fprintf(w, `{"message":"invalid API key (got %s)","traceID":""}`, r.Header.Get("Authorization"))
			return
		}
		g.mu.Lock()
		defer g.mu.Unlock()
		w.WriteHeader(g.status)
		_, _ = w.Write([]byte(g.body))
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *fakeGrafana) set(body string) {
	g.mu.Lock()
	g.body = body
	g.mu.Unlock()
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type recorder struct {
	mu     sync.Mutex
	events []recorded
	logs   []string
}

type recorded struct {
	channel string
	payload map[string]any
}

func (r *recorder) broadcast(channel string, payload any) {
	raw, _ := json.Marshal(payload)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	r.mu.Lock()
	r.events = append(r.events, recorded{channel, m})
	r.mu.Unlock()
}

func (r *recorder) logf(format string, args ...any) {
	r.mu.Lock()
	r.logs = append(r.logs, fmt.Sprintf(format, args...))
	r.mu.Unlock()
}

func (r *recorder) announces() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, e := range r.events {
		if e.channel != EventAlertsChanged {
			continue
		}
		for _, id := range e.payload["announce"].([]any) {
			out = append(out, id.(string))
		}
	}
	return out
}

func (r *recorder) count(channel string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, e := range r.events {
		if e.channel == channel {
			n++
		}
	}
	return n
}

func (r *recorder) everything() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	raw, _ := json.Marshal(r.events)
	return string(raw) + strings.Join(r.logs, "\n")
}

func openTest(t *testing.T, dir string, clk *clock, rec *recorder) *Service {
	t.Helper()
	s, err := Open(Options{StateDir: dir, Broadcast: rec.broadcast, Logf: rec.logf, Now: clk.now,
		PollInterval: 20 * time.Millisecond, PollTimeout: 2 * time.Second, MaxBackoff: 80 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func strp(s string) *string { return &s }

func connectorByID(s *Service, id string) Connector {
	for _, c := range s.List() {
		if c.ID == id {
			return c
		}
	}
	return Connector{}
}

func pollNow(t *testing.T, s *Service, id string) error {
	t.Helper()
	s.mu.Lock()
	gen := s.gens[id]
	s.mu.Unlock()
	return s.pollOnce(context.Background(), id, gen)
}

func alertByRule(s *Service, ruleKey string) *Alert {
	for _, a := range s.Alerts() {
		if a.RuleKey == ruleKey {
			a := a
			return &a
		}
	}
	return nil
}

func TestPollEpisodesAnnounceOnlyNewFirings(t *testing.T) {
	g := newFakeGrafana(t)
	clk := &clock{t: time.Date(2026, 11, 10, 10, 0, 30, 0, time.UTC)}
	rec := &recorder{}
	s := openTest(t, t.TempDir(), clk, rec)
	c, secret, err := s.Set(SetRequest{Name: "fiumba", URL: g.srv.URL + "/", Mode: ModePoll, Token: strp(testToken)})
	if err != nil {
		t.Fatal(err)
	}
	if secret != "" || !strings.HasPrefix(c.ID, "gf-") || len(c.ID) != 11 || c.URL != g.srv.URL || !c.HasToken || c.TokenHint != "3c4d" {
		t.Fatalf("connector = %#v secret=%q", c, secret)
	}

	// 1: HighCPU firing, DiskFilling only Pending.
	g.set(grafanaRulesFixture("Alerting", "2026-11-10T09:55:00Z", "Pending"))
	if err := pollNow(t, s, c.ID); err != nil {
		t.Fatal(err)
	}
	alerts := s.Alerts()
	if len(alerts) != 1 {
		t.Fatalf("pending must not fire: %#v", alerts)
	}
	a := alerts[0]
	if a.RuleKey != "cpu-rule-uid" || a.RuleTitle != "HighCPU" || a.Severity != SeverityCritical || a.Machine != "duklerfin:9100" ||
		a.State != AlertFiring || a.StartsAt != "2026-11-10T09:55:00Z" || a.Values["value"] != 95 || len(a.History) != 1 {
		t.Fatalf("alert = %#v", a)
	}
	if strings.Contains(a.Annotations["description"], "hunter2hunter2hunter2") {
		t.Fatalf("annotation not redacted: %q", a.Annotations["description"])
	}
	conn := s.List()[0]
	if conn.Status.State != StateOK || conn.Status.LastOkAt == "" {
		t.Fatalf("status = %#v", conn.Status)
	}
	keys := []string{}
	for _, r := range conn.Rules {
		keys = append(keys, r.Key+"/"+r.Folder+"/"+r.Severity)
	}
	if got := strings.Join(keys, ","); got != "cpu-rule-uid/Infra/critical,disk-rule-uid/Infra/warning,title:NoUIDRule/Old/warning" {
		t.Fatalf("rules = %s", got)
	}

	// 2: the same firing again: no announce, no repeat, no event.
	events := rec.count(EventAlertsChanged)
	clk.add(30 * time.Second)
	if err := pollNow(t, s, c.ID); err != nil {
		t.Fatal(err)
	}
	if rec.count(EventAlertsChanged) != events || s.Alerts()[0].Repeats != 0 {
		t.Fatalf("a still-firing poll moved the bell: %d events", rec.count(EventAlertsChanged)-events)
	}

	// Owner dismisses and links it.
	if _, err := s.Act(ActRequest{IDs: []string{a.ID}, Action: "dismiss"}); err != nil {
		t.Fatal(err)
	}

	// 3: gone (Normal): resolved at poll time.
	clk.add(30 * time.Second)
	g.set(grafanaRulesFixture("Normal", "", "Normal"))
	if err := pollNow(t, s, c.ID); err != nil {
		t.Fatal(err)
	}
	a = s.Alerts()[0]
	if a.State != AlertResolved || a.EndsAt != clk.now().Format(time.RFC3339) || a.DismissedAt == "" {
		t.Fatalf("resolved = %#v", a)
	}

	// 4: fires again: a new episode, announced, undismissed.
	clk.add(30 * time.Second)
	g.set(grafanaRulesFixture("Alerting", "2026-11-10T10:02:00Z", "Pending"))
	if err := pollNow(t, s, c.ID); err != nil {
		t.Fatal(err)
	}
	a = s.Alerts()[0]
	if a.State != AlertFiring || a.StartsAt != "2026-11-10T10:02:00Z" || a.DismissedAt != "" || a.EndsAt != "" || len(a.History) != 3 {
		t.Fatalf("re-fire = %#v", a)
	}
	if got := rec.announces(); len(got) != 2 || got[0] != a.ID || got[1] != a.ID {
		t.Fatalf("announces = %v, want two of %s", got, a.ID)
	}

	// 5: a resolve missed between two looks (activeAt moved later) is still a new episode.
	clk.add(30 * time.Second)
	g.set(grafanaRulesFixture("Alerting (NoData)", "2026-11-10T10:03:00Z", "Pending"))
	if err := pollNow(t, s, c.ID); err != nil {
		t.Fatal(err)
	}
	if got := rec.announces(); len(got) != 3 {
		t.Fatalf("announces = %v", got)
	}
}

func TestDisabledRuleIsHiddenAndNeverAnnounced(t *testing.T) {
	g := newFakeGrafana(t)
	clk := &clock{t: time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)}
	rec := &recorder{}
	s := openTest(t, t.TempDir(), clk, rec)
	c, _, err := s.Set(SetRequest{Name: "fiumba", URL: g.srv.URL, Mode: ModePoll, Token: strp(testToken)})
	if err != nil {
		t.Fatal(err)
	}
	g.set(grafanaRulesFixture("Normal", "", "Alerting"))
	if err := pollNow(t, s, c.ID); err != nil {
		t.Fatal(err)
	}
	if a := alertByRule(s, "disk-rule-uid"); a == nil || a.Machine != "duklerfin" || a.Values["A"] != 91.5 || a.Values["C"] != 1 {
		t.Fatalf("disk alert = %#v", a)
	}
	if _, _, err := s.Set(SetRequest{ID: c.ID, Name: "fiumba", URL: g.srv.URL, Mode: ModePoll,
		Rules: []RuleToggle{{Key: "disk-rule-uid", Enabled: false}, {Key: "cpu-rule-uid", Enabled: false}}}); err != nil {
		t.Fatal(err)
	}
	if len(s.Alerts()) != 0 {
		t.Fatalf("a disabled rule's alert is listed: %#v", s.Alerts())
	}
	before := len(rec.announces())
	g.set(grafanaRulesFixture("Alerting", "2026-11-10T09:59:00Z", "Alerting"))
	if err := pollNow(t, s, c.ID); err != nil {
		t.Fatal(err)
	}
	if len(rec.announces()) != before || len(s.Alerts()) != 0 {
		t.Fatalf("disabled rules announced: %v", rec.announces())
	}
	conn := s.List()[0]
	disabled := 0
	for _, r := range conn.Rules {
		if !r.Enabled {
			disabled++
		}
	}
	if disabled != 2 || !conn.HasToken {
		t.Fatalf("rules after poll = %#v (token kept %v)", conn.Rules, conn.HasToken)
	}
}

func TestBadTokenStatusErrorIsRedacted(t *testing.T) {
	g := newFakeGrafana(t)
	g.set(grafanaRulesFixture("Alerting", "2026-11-10T09:55:00Z", "Pending"))
	clk := &clock{t: time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)}
	rec := &recorder{}
	s := openTest(t, t.TempDir(), clk, rec)
	bad := "glsa_WRONGwrongWRONGwrong_99887766"
	c, _, err := s.Set(SetRequest{Name: "fiumba", URL: g.srv.URL, Mode: ModePoll, Token: strp(bad)})
	if err != nil {
		t.Fatal(err)
	}
	if err := pollNow(t, s, c.ID); err == nil {
		t.Fatal("a bad token polled fine")
	}
	st := s.List()[0].Status
	if st.State != StateError || !strings.Contains(st.Error, "401") || strings.Contains(st.Error, bad) {
		t.Fatalf("status = %#v", st)
	}
	res := s.Test(context.Background(), TestRequest{ID: c.ID})
	if res.OK || strings.Contains(res.Error, bad) || !strings.Contains(res.Error, "401") {
		t.Fatalf("test = %#v", res)
	}
	if res := s.Test(context.Background(), TestRequest{URL: g.srv.URL, Token: strp(testToken)}); !res.OK || res.Rules != 3 || res.Firing != 1 {
		t.Fatalf("good test = %#v", res)
	}
	if strings.Contains(rec.everything(), bad) || strings.Contains(rec.everything(), testToken) {
		t.Fatal("a token reached an event or a log")
	}
	if len(s.Alerts()) != 0 {
		t.Fatal("a failed poll must not resolve or create alerts")
	}
}

func TestBackoffDelayAndPollerLoop(t *testing.T) {
	for failures, want := range map[int]time.Duration{0: 30 * time.Second, 1: time.Minute, 2: 2 * time.Minute, 3: 4 * time.Minute, 4: 5 * time.Minute, 30: 5 * time.Minute} {
		if got := backoffDelay(30*time.Second, 5*time.Minute, failures); got != want {
			t.Fatalf("backoff(%d) = %s, want %s", failures, got, want)
		}
	}
	g := newFakeGrafana(t)
	clk := &clock{t: time.Now()}
	rec := &recorder{}
	s := openTest(t, t.TempDir(), clk, rec)
	// Wrong token: every poll fails. Interval 20 ms, backoff 40, 80, 80...
	if _, _, err := s.Set(SetRequest{Name: "x", URL: g.srv.URL, Mode: ModePoll, Token: strp("nope")}); err != nil {
		t.Fatal(err)
	}
	s.Start(context.Background())
	time.Sleep(500 * time.Millisecond)
	s.Close()
	hits := g.hits.Load()
	// Without backoff ~25 polls; with it at most ~8.
	if hits < 2 || hits > 12 {
		t.Fatalf("polls in 500 ms with backoff = %d", hits)
	}
	after := g.hits.Load()
	time.Sleep(100 * time.Millisecond)
	if g.hits.Load() != after {
		t.Fatal("a poller survived Close")
	}
}

func TestPollerStartsAndStopsWithConnectors(t *testing.T) {
	g := newFakeGrafana(t)
	g.set(grafanaRulesFixture("Alerting", "2026-11-10T09:55:00Z", "Pending"))
	clk := &clock{t: time.Now()}
	rec := &recorder{}
	s := openTest(t, t.TempDir(), clk, rec)
	s.Start(context.Background())
	c, _, err := s.Set(SetRequest{Name: "x", URL: g.srv.URL, Mode: ModePoll, Token: strp(testToken)})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(s.Alerts()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(s.Alerts()) != 1 {
		t.Fatal("the poller never ingested")
	}
	if err := s.Remove(c.ID); err != nil {
		t.Fatal(err)
	}
	after := g.hits.Load()
	time.Sleep(100 * time.Millisecond)
	if g.hits.Load() != after || len(s.Alerts()) != 0 {
		t.Fatalf("removed connector still polled (%d) or kept alerts (%d)", g.hits.Load()-after, len(s.Alerts()))
	}
}

const webhookFiring = `{
  "receiver": "workass",
  "status": "firing",
  "orgId": 1,
  "alerts": [{
    "status": "%s",
    "labels": {"alertname": "HighCPU", "grafana_folder": "Infra", "instance": "duklerfin:9100", "severity": "critical"},
    "annotations": {"summary": "CPU above 90%%", "runbook_url": "https://user:pa55w0rdpa55w0rd@wiki.example/cpu"},
    "startsAt": "%s",
    "endsAt": "%s",
    "generatorURL": "http://127.0.0.1:13000/alerting/grafana/cpu-rule-uid/view?orgId=1",
    "fingerprint": "57c6d9296de2ad39",
    "silenceURL": "http://127.0.0.1:13000/alerting/silence/new",
    "dashboardURL": "",
    "panelURL": "",
    "values": {"A": 95.2, "C": 1},
    "valueString": "[ var='A' labels={instance=duklerfin:9100} value=95.2 ]"
  }],
  "groupLabels": {"alertname": "HighCPU"},
  "commonLabels": {"alertname": "HighCPU"},
  "commonAnnotations": {},
  "externalURL": "http://127.0.0.1:13000/",
  "version": "1",
  "groupKey": "{}:{alertname=\"HighCPU\"}",
  "truncatedAlerts": 0,
  "title": "[FIRING:1] HighCPU",
  "state": "alerting",
  "message": "..."
}`

func webhookBody(status, startsAt, endsAt string) string {
	return fmt.Sprintf(webhookFiring, status, startsAt, endsAt)
}

func postWebhook(t *testing.T, h http.Handler, path, auth, body string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w.Code
}

func TestWebhookAuthIngestAndEpisodes(t *testing.T) {
	clk := &clock{t: time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)}
	rec := &recorder{}
	dir := t.TempDir()
	s := openTest(t, dir, clk, rec)
	h := s.WebhookHandler()
	c, secret, err := s.Set(SetRequest{Name: "fiumba", URL: "http://127.0.0.1:13000", Mode: ModeWebhook})
	if err != nil {
		t.Fatal(err)
	}
	if len(secret) < 40 || !c.HasWebhookSecret || c.WebhookPath != "/workass/connectors/"+c.ID+"/webhook" {
		t.Fatalf("connector = %#v secret %q", c, secret)
	}
	poll, _, err := s.Set(SetRequest{Name: "p", URL: "http://127.0.0.1:13000", Mode: ModePoll})
	if err != nil {
		t.Fatal(err)
	}
	firing := webhookBody("firing", "2026-11-10T09:55:00Z", "0001-01-01T00:00:00Z")
	for _, tc := range []struct {
		name, path, auth string
		want             int
	}{
		{"missing secret", c.WebhookPath, "", 401},
		{"bad secret", c.WebhookPath, "Bearer " + strings.Repeat("A", len(secret)), 401},
		{"basic scheme", c.WebhookPath, "Basic " + secret, 401},
		{"unknown id", "/workass/connectors/gf-00000000/webhook", "Bearer " + secret, 404},
		{"poll mode", poll.WebhookPath, "Bearer " + secret, 404},
		{"bad tail", "/workass/connectors/" + c.ID + "/other", "Bearer " + secret, 404},
	} {
		if got := postWebhook(t, h, tc.path, tc.auth, firing); got != tc.want {
			t.Fatalf("%s: %d, want %d", tc.name, got, tc.want)
		}
	}
	if len(s.Alerts()) != 0 {
		t.Fatal("a refused webhook ingested")
	}
	big := `{"alerts":[],"pad":"` + strings.Repeat("x", WebhookBodyMax) + `"}`
	if got := postWebhook(t, h, c.WebhookPath, "Bearer "+secret, big); got != http.StatusRequestEntityTooLarge {
		t.Fatalf("body cap: %d", got)
	}
	if got := postWebhook(t, h, c.WebhookPath, "Bearer "+secret, "not json"); got != 400 {
		t.Fatalf("bad json: %d", got)
	}

	if got := postWebhook(t, h, c.WebhookPath, "Bearer "+secret, firing); got != 200 {
		t.Fatalf("good secret: %d", got)
	}
	a := s.Alerts()[0]
	if a.ID != c.ID+":57c6d9296de2ad39" || a.RuleKey != "cpu-rule-uid" || a.Severity != SeverityCritical || a.Machine != "duklerfin:9100" ||
		a.Values["A"] != 95.2 || a.StartsAt != "2026-11-10T09:55:00Z" || a.GeneratorURL == "" {
		t.Fatalf("alert = %#v", a)
	}
	if strings.Contains(a.Annotations["runbook_url"], "pa55w0rdpa55w0rd") {
		t.Fatalf("annotation not redacted: %q", a.Annotations["runbook_url"])
	}
	if conn := connectorByID(s, c.ID); conn.Status.State != StateOK || len(conn.Rules) != 1 || conn.Rules[0].Folder != "Infra" {
		t.Fatalf("learned = %#v", conn)
	}
	// Grafana re-sends the same firing: a repeat, never announced.
	clk.add(4 * time.Hour)
	postWebhook(t, h, c.WebhookPath, "Bearer "+secret, firing)
	a = s.Alerts()[0]
	if a.Repeats != 1 || a.LastRepeatAt != clk.now().Format(time.RFC3339) || len(rec.announces()) != 1 {
		t.Fatalf("repeat = %#v announces %v", a, rec.announces())
	}
	if _, err := s.Act(ActRequest{IDs: []string{a.ID}, Action: "link", Link: &struct {
		MachineID string `json:"machineId"`
		TabID     string `json:"tabId"`
		ChatID    string `json:"chatId"`
	}{"", "tab-1", "chat-1"}}); err != nil {
		t.Fatal(err)
	}
	clk.add(time.Minute)
	postWebhook(t, h, c.WebhookPath, "Bearer "+secret, webhookBody("resolved", "2026-11-10T09:55:00Z", "2026-11-10T14:00:30Z"))
	a = s.Alerts()[0]
	if a.State != AlertResolved || a.EndsAt != "2026-11-10T14:00:30Z" || a.Link == nil {
		t.Fatalf("resolved = %#v", a)
	}
	clk.add(time.Minute)
	postWebhook(t, h, c.WebhookPath, "Bearer "+secret, webhookBody("firing", "2026-11-10T14:02:00Z", "0001-01-01T00:00:00Z"))
	a = s.Alerts()[0]
	if a.State != AlertFiring || a.Repeats != 0 || a.Link != nil || len(rec.announces()) != 2 || len(a.History) != 3 {
		t.Fatalf("re-fire = %#v announces %v", a, rec.announces())
	}

	// Rotation: the old secret stops working.
	newSecret, err := s.RotateSecret(c.ID)
	if err != nil || newSecret == secret {
		t.Fatal(err)
	}
	if got := postWebhook(t, h, c.WebhookPath, "Bearer "+secret, firing); got != 401 {
		t.Fatalf("old secret after rotate: %d", got)
	}
	if got := postWebhook(t, h, c.WebhookPath, "Bearer "+newSecret, firing); got != 200 {
		t.Fatalf("new secret: %d", got)
	}
	if strings.Contains(rec.everything(), secret) || strings.Contains(rec.everything(), newSecret) {
		t.Fatal("a webhook secret reached an event or a log")
	}
}

func TestSecretsStayOffTheWireAndPersistAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	clk := &clock{t: time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)}
	rec := &recorder{}
	s := openTest(t, dir, clk, rec)
	g := newFakeGrafana(t)
	g.set(grafanaRulesFixture("Alerting", "2026-11-10T09:55:00Z", "Pending"))
	c, _, err := s.Set(SetRequest{Name: "fiumba", URL: g.srv.URL, Mode: ModePoll, Token: strp(testToken),
		Notify: "all", Target: &Target{MachineID: "m-1", CWD: "/srv/depthcut"}, GuidePattern: strp("ops/runbooks/<regla>.md"),
		RuleTargets: map[string]Target{"cpu-rule-uid": {MachineID: "", CWD: "/srv/other"}}})
	if err != nil {
		t.Fatal(err)
	}
	w, secret, err := s.Set(SetRequest{Name: "hook", URL: "http://127.0.0.1:13000", Mode: ModeWebhook})
	if err != nil {
		t.Fatal(err)
	}
	if err := pollNow(t, s, c.ID); err != nil {
		t.Fatal(err)
	}
	alertID := s.Alerts()[0].ID
	if _, err := s.Act(ActRequest{IDs: []string{alertID}, Action: "silence", Minutes: 10}); err != nil {
		t.Fatal(err)
	}
	listed, _ := json.Marshal(s.List())
	for _, blob := range []string{string(listed), rec.everything()} {
		if strings.Contains(blob, testToken) || strings.Contains(blob, secret) {
			t.Fatalf("secret on the wire or in a log: %s", blob)
		}
	}
	for _, name := range []string{"connectors.json", "alerts.json"} {
		raw, err := os.ReadFile(filepath.Join(dir, "connectors", name))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), testToken) || strings.Contains(string(raw), secret) {
			t.Fatalf("%s holds a secret", name)
		}
	}
	for _, id := range []string{c.ID, w.ID} {
		info, err := os.Stat(filepath.Join(dir, "connectors", "secrets", id+".json"))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("secrets file %s: %v %v", id, err, info)
		}
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "connectors", "secrets", w.ID+".json"))
	if strings.Contains(string(raw), secret) {
		t.Fatal("the webhook secret is stored in clear")
	}
	s.Close()

	// Restart.
	s2 := openTest(t, dir, clk, rec)
	list := s2.List()
	if len(list) != 2 {
		t.Fatalf("connectors after restart = %#v", list)
	}
	got := list[0]
	if got.ID != c.ID {
		got = list[1]
	}
	if !got.HasToken || got.TokenHint != "3c4d" || got.Notify != NotifyAll || got.Target.CWD != "/srv/depthcut" ||
		got.RuleTargets["cpu-rule-uid"].CWD != "/srv/other" || got.GuidePattern != "ops/runbooks/<regla>.md" || len(got.Rules) != 3 {
		t.Fatalf("connector after restart = %#v", got)
	}
	alerts := s2.Alerts()
	if len(alerts) != 1 || alerts[0].ID != alertID || alerts[0].SilencedUntil != "2026-11-10T10:10:00Z" {
		t.Fatalf("alerts after restart = %#v", alerts)
	}
	// Still firing after the restart: no new announce.
	before := len(rec.announces())
	if err := pollNow(t, s2, c.ID); err != nil {
		t.Fatal(err)
	}
	if len(rec.announces()) != before {
		t.Fatal("a restart re-announced a firing alert")
	}
	if got := postWebhook(t, s2.WebhookHandler(), w.WebhookPath, "Bearer "+secret, webhookBody("firing", "2026-11-10T09:55:00Z", "")); got != 200 {
		t.Fatalf("webhook secret after restart: %d", got)
	}
	// Clearing the token.
	if _, _, err := s2.Set(SetRequest{ID: c.ID, Name: "fiumba", URL: g.srv.URL, Mode: ModePoll, Token: strp("")}); err != nil {
		t.Fatal(err)
	}
	for _, conn := range s2.List() {
		if conn.ID == c.ID && (conn.HasToken || conn.TokenHint != "") {
			t.Fatalf("token not cleared: %#v", conn)
		}
	}
}

func TestAlertsActAndPrune(t *testing.T) {
	clk := &clock{t: time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)}
	rec := &recorder{}
	s := openTest(t, t.TempDir(), clk, rec)
	c, secret, err := s.Set(SetRequest{Name: "hook", URL: "http://127.0.0.1:13000", Mode: ModeWebhook})
	if err != nil {
		t.Fatal(err)
	}
	h := s.WebhookHandler()
	postWebhook(t, h, c.WebhookPath, "Bearer "+secret, webhookBody("firing", "2026-11-10T09:55:00Z", ""))
	id := s.Alerts()[0].ID
	act := func(req ActRequest) Alert {
		t.Helper()
		req.IDs = []string{id}
		out, err := s.Act(req)
		if err != nil || len(out) != 1 {
			t.Fatalf("act %s: %v %#v", req.Action, err, out)
		}
		return out[0]
	}
	if a := act(ActRequest{Action: "silence", Minutes: 10}); a.SilencedUntil != "2026-11-10T10:10:00Z" {
		t.Fatalf("silence = %#v", a)
	}
	if a := act(ActRequest{Action: "unsilence"}); a.SilencedUntil != "" {
		t.Fatalf("unsilence = %#v", a)
	}
	if a := act(ActRequest{Action: "dismiss"}); a.DismissedAt != "2026-11-10T10:00:00Z" {
		t.Fatalf("dismiss = %#v", a)
	}
	if a := act(ActRequest{Action: "undismiss"}); a.DismissedAt != "" {
		t.Fatalf("undismiss = %#v", a)
	}
	if _, err := s.Act(ActRequest{IDs: []string{id}, Action: "explode"}); err == nil {
		t.Fatal("unknown action accepted")
	}
	if _, err := s.Act(ActRequest{IDs: []string{id}, Action: "link"}); err == nil {
		t.Fatal("link without a chat accepted")
	}

	postWebhook(t, h, c.WebhookPath, "Bearer "+secret, webhookBody("resolved", "2026-11-10T09:55:00Z", "2026-11-10T10:00:00Z"))
	clk.add(6 * 24 * time.Hour)
	s.prune()
	if len(s.Alerts()) != 1 {
		t.Fatal("pruned before 7 days")
	}
	clk.add(2 * 24 * time.Hour)
	s.prune()
	if len(s.Alerts()) != 0 {
		t.Fatal("a resolved alert outlived 7 days")
	}
}

func TestGrafanaValueFormats(t *testing.T) {
	for raw, want := range map[string]map[string]float64{
		`"1e+00"`:                {"value": 1},
		`"A: 9.5e+01, C: 1e+00"`: {"A": 95, "C": 1},
		`"[ var='B' labels={} value=0.5 ], [ var='C' labels={} value=1 ]"`: {"B": 0.5, "C": 1},
		`3`:     {"value": 3},
		`""`:    nil,
		`"NaN"`: nil,
	} {
		got := parseGrafanaValue(json.RawMessage(raw))
		if len(got) != len(want) {
			t.Fatalf("%s = %#v, want %#v", raw, got, want)
		}
		for k, v := range want {
			if got[k] != v {
				t.Fatalf("%s = %#v, want %#v", raw, got, want)
			}
		}
	}
}
