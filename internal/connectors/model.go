// Package connectors is Conectores + alerts (docs/specs/connectors-alerts.md).
//
// A connector lives on one daemon, the "machine that listens". Kind grafana
// is the only kind: the daemon either polls Grafana's Prometheus-compatible
// rules API (mode poll, "Consultar") or receives Grafana's webhook contact
// point (mode webhook, "Recibir"). Both feed one alert table with episode
// logic: a new firing episode is announced once; Grafana repeats never are.
//
// Owner law 1: nothing here opens, writes into or wakes a chat. An alert only
// reaches the bell (alerts:changed); an agent gets one only after the owner
// hands it over from the client, which then records the link with alerts:act.
package connectors

import (
	"math"
	"strings"
)

const (
	KindGrafana = "grafana"
	ModePoll    = "poll"
	ModeWebhook = "webhook"

	StateOK    = "ok"
	StateError = "error"
	StateIdle  = "idle"

	AlertFiring   = "firing"
	AlertResolved = "resolved"

	NotifyNever    = "never"
	NotifyCritical = "critical"
	NotifyAll      = "all"

	SeverityCritical = "critical"
	SeverityWarning  = "warning"
	SeverityInfo     = "info"

	// WebhookPrefix is the daemon HTTP route prefix; the full path is
	// WebhookPrefix + <id> + "/webhook".
	WebhookPrefix = "/workass/connectors/"

	// EventAlertsChanged and EventConnectorsChanged are the two events.
	EventAlertsChanged     = "alerts:changed"
	EventConnectorsChanged = "connectors:changed"

	historyMax = 20
)

var defaultMachineLabels = []string{"machine", "instance", "host"}

// Target is where a hand-off chat opens; MachineID "" = the client's local machine.
type Target struct {
	MachineID string `json:"machineId"`
	CWD       string `json:"cwd"`
}

type Status struct {
	State    string `json:"state"`
	LastOkAt string `json:"lastOkAt,omitempty"`
	Error    string `json:"error,omitempty"`
}

type Rule struct {
	Key      string `json:"key"`
	Title    string `json:"title"`
	Folder   string `json:"folder"`
	Severity string `json:"severity"`
	Enabled  bool   `json:"enabled"`
	SeenAt   string `json:"seenAt"`
}

// Connector is both the wire shape and (with the secret-derived fields
// recomputed on every read) the stored record. No secret is ever in it.
type Connector struct {
	ID               string            `json:"id"`
	Kind             string            `json:"kind"`
	Name             string            `json:"name"`
	URL              string            `json:"url"`
	Mode             string            `json:"mode"`
	HasToken         bool              `json:"hasToken"`
	TokenHint        string            `json:"tokenHint"`
	WebhookPath      string            `json:"webhookPath"`
	HasWebhookSecret bool              `json:"hasWebhookSecret"`
	Status           Status            `json:"status"`
	Rules            []Rule            `json:"rules"`
	Notify           string            `json:"notify"`
	Target           Target            `json:"target"`
	RuleTargets      map[string]Target `json:"ruleTargets"`
	GuidePattern     string            `json:"guidePattern"`
	MachineLabels    []string          `json:"machineLabels"`
}

type AlertLink struct {
	MachineID string `json:"machineId"`
	TabID     string `json:"tabId"`
	ChatID    string `json:"chatId"`
	At        string `json:"at"`
}

type HistoryEntry struct {
	At    string `json:"at"`
	State string `json:"state"`
}

type Alert struct {
	ID            string             `json:"id"`
	ConnectorID   string             `json:"connectorId"`
	RuleKey       string             `json:"ruleKey"`
	RuleTitle     string             `json:"ruleTitle"`
	Severity      string             `json:"severity"`
	Machine       string             `json:"machine"`
	Labels        map[string]string  `json:"labels"`
	Annotations   map[string]string  `json:"annotations"`
	Values        map[string]float64 `json:"values,omitempty"`
	State         string             `json:"state"`
	StartsAt      string             `json:"startsAt"`
	EndsAt        string             `json:"endsAt,omitempty"`
	LastSeenAt    string             `json:"lastSeenAt"`
	Repeats       int                `json:"repeats"`
	LastRepeatAt  string             `json:"lastRepeatAt,omitempty"`
	GeneratorURL  string             `json:"generatorURL,omitempty"`
	SilencedUntil string             `json:"silencedUntil,omitempty"`
	DismissedAt   string             `json:"dismissedAt,omitempty"`
	Link          *AlertLink         `json:"link,omitempty"`
	History       []HistoryEntry     `json:"history"`
}

// secrets is one connector's state/connectors/secrets/<id>.json (0600).
// The webhook secret is kept only as its SHA-256: the daemon shows it once.
type secrets struct {
	Token             string `json:"token,omitempty"`
	WebhookSecretHash string `json:"webhookSecretHash,omitempty"`
}

func normalizeSeverity(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "critical", "crit", "page", "p0", "p1", "high", "error", "fatal", "emergency", "alert", "disaster":
		return SeverityCritical
	case "info", "informational", "information", "low", "notice", "none", "debug", "p4", "p5":
		return SeverityInfo
	default:
		return SeverityWarning
	}
}

func normalizeNotify(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case NotifyNever:
		return NotifyNever
	case NotifyAll:
		return NotifyAll
	default:
		return NotifyCritical
	}
}

func machineFor(labels map[string]string, order []string) string {
	if len(order) == 0 {
		order = defaultMachineLabels
	}
	for _, key := range order {
		if v := strings.TrimSpace(labels[key]); v != "" {
			return v
		}
	}
	return ""
}

func titleKey(title string) string { return "title:" + title }

func finiteValues(in map[string]float64) map[string]float64 {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]float64, len(in))
	for k, v := range in {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func cloneConnector(c Connector) Connector {
	c.Rules = append([]Rule{}, c.Rules...)
	targets := make(map[string]Target, len(c.RuleTargets))
	for k, v := range c.RuleTargets {
		targets[k] = v
	}
	c.RuleTargets = targets
	c.MachineLabels = append([]string{}, c.MachineLabels...)
	return c
}

func cloneAlert(a *Alert) Alert {
	out := *a
	out.Labels = cloneStrings(a.Labels)
	out.Annotations = cloneStrings(a.Annotations)
	if a.Values != nil {
		out.Values = make(map[string]float64, len(a.Values))
		for k, v := range a.Values {
			out.Values[k] = v
		}
	}
	if a.Link != nil {
		link := *a.Link
		out.Link = &link
	}
	out.History = append([]HistoryEntry{}, a.History...)
	return out
}

func cloneStrings(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
