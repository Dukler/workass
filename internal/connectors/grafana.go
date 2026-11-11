package connectors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// GrafanaRulesPath is Grafana's Prometheus-compatible rules API for
// Grafana-managed rules. A Viewer service-account token can read it.
const GrafanaRulesPath = "/api/prometheus/grafana/api/v1/rules"

const grafanaRulesBodyMax = 32 << 20

// stringMap decodes a JSON object of labels tolerantly: a non-string value is
// kept as its JSON text instead of failing the whole poll.
type stringMap map[string]string

func (m *stringMap) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		// null, or not an object: no labels.
		*m = stringMap{}
		return nil
	}
	out := make(stringMap, len(raw))
	for k, v := range raw {
		var s string
		if err := json.Unmarshal(v, &s); err == nil {
			out[k] = s
			continue
		}
		if string(v) == "null" {
			continue
		}
		out[k] = string(v)
	}
	*m = out
	return nil
}

type grafanaRulesResponse struct {
	Status    string `json:"status"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
	Data      struct {
		Groups []grafanaRuleGroup `json:"groups"`
	} `json:"data"`
}

type grafanaRuleGroup struct {
	Name      string        `json:"name"`
	File      string        `json:"file"` // the folder's title (path)
	FolderUID string        `json:"folderUid"`
	Rules     []grafanaRule `json:"rules"`
}

type grafanaRule struct {
	UID         string         `json:"uid"`
	Name        string         `json:"name"`
	FolderUID   string         `json:"folderUid"`
	Labels      stringMap      `json:"labels"`
	Annotations stringMap      `json:"annotations"`
	Health      string         `json:"health"`
	Type        string         `json:"type"`
	State       string         `json:"state"`
	IsPaused    bool           `json:"isPaused"`
	Alerts      []grafanaAlert `json:"alerts"`
}

type grafanaAlert struct {
	Labels      stringMap       `json:"labels"`
	Annotations stringMap       `json:"annotations"`
	State       string          `json:"state"`
	ActiveAt    string          `json:"activeAt"`
	Value       json.RawMessage `json:"value"`
}

// observation is one alert instance as a transport saw it, before redaction.
type observation struct {
	Fingerprint  string
	RuleKey      string
	RuleTitle    string
	Folder       string
	Labels       map[string]string
	Annotations  map[string]string
	Values       map[string]float64
	Firing       bool
	StartsAt     time.Time
	EndsAt       time.Time
	GeneratorURL string
}

type pollResult struct {
	Rules  []Rule
	Firing []observation
}

// grafanaInstanceFiring: Alerting (with or without a reason, "Alerting
// (NoData)"), NoData and Error notify in Grafana, so they fire here too.
// Pending and Normal do not.
func grafanaInstanceFiring(state string) bool {
	s := strings.ToLower(strings.TrimSpace(state))
	switch {
	case strings.HasPrefix(s, "alerting"), s == "firing", s == "nodata", s == "error":
		return true
	}
	return false
}

func parseGrafanaRules(body []byte, now time.Time) (pollResult, error) {
	var resp grafanaRulesResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return pollResult{}, fmt.Errorf("grafana rules answer is not JSON: %w", err)
	}
	if resp.Status != "" && resp.Status != "success" {
		msg := strings.TrimSpace(resp.Error)
		if msg == "" {
			msg = resp.Status
		}
		return pollResult{}, fmt.Errorf("grafana rules: %s", msg)
	}
	seen := now.UTC().Format(time.RFC3339)
	var out pollResult
	for _, group := range resp.Data.Groups {
		for _, rule := range group.Rules {
			if t := strings.ToLower(strings.TrimSpace(rule.Type)); t != "" && t != "alerting" {
				continue
			}
			title := strings.TrimSpace(rule.Name)
			key := strings.TrimSpace(rule.UID)
			if key == "" {
				key = titleKey(title)
			}
			out.Rules = append(out.Rules, Rule{
				Key: key, Title: title, Folder: strings.TrimSpace(group.File),
				Severity: normalizeSeverity(rule.Labels["severity"]), Enabled: true, SeenAt: seen,
			})
			for _, inst := range rule.Alerts {
				if !grafanaInstanceFiring(inst.State) {
					continue
				}
				labels := map[string]string(inst.Labels)
				if labels == nil {
					labels = map[string]string{}
				}
				if _, ok := labels["alertname"]; !ok && title != "" {
					labels = cloneStrings(labels)
					labels["alertname"] = title
				}
				annotations := map[string]string(inst.Annotations)
				if len(annotations) == 0 {
					annotations = map[string]string(rule.Annotations)
				}
				out.Firing = append(out.Firing, observation{
					Fingerprint: labelFingerprint(labels),
					RuleKey:     key, RuleTitle: title, Folder: strings.TrimSpace(group.File),
					Labels: labels, Annotations: annotations,
					Values: parseGrafanaValue(inst.Value),
					Firing: true, StartsAt: parseTime(inst.ActiveAt),
				})
			}
		}
	}
	return out, nil
}

// labelFingerprint is Prometheus' LabelSet fingerprint (FNV-64a over the
// sorted name/value pairs, 0xff separated), the same shape Alertmanager puts
// in a webhook's fingerprint. Private "__" labels are left out.
func labelFingerprint(labels map[string]string) string {
	names := make([]string, 0, len(labels))
	for k := range labels {
		if strings.HasPrefix(k, "__") {
			continue
		}
		names = append(names, k)
	}
	sort.Strings(names)
	h := fnv.New64a()
	for _, k := range names {
		_, _ = h.Write([]byte(k))
		_, _ = h.Write([]byte{0xff})
		_, _ = h.Write([]byte(labels[k]))
		_, _ = h.Write([]byte{0xff})
	}
	return fmt.Sprintf("%016x", h.Sum64())
}

var (
	grafanaVarValueRE = regexp.MustCompile(`var='([^']+)'[^\]]*?value=([-+0-9.eE]+|NaN|[-+]?Inf)`)
	grafanaPairRE     = regexp.MustCompile(`([A-Za-z0-9_]+):\s*([-+0-9.eE]+)`)
)

// parseGrafanaValue reads Grafana's formatted instance value: one number
// ("1e+00"), "A: 1e+00, C: 1e+00", or "[ var='A' labels={..} value=1 ], ...".
func parseGrafanaValue(raw json.RawMessage) map[string]float64 {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var number float64
	if err := json.Unmarshal(raw, &number); err == nil {
		return finiteValues(map[string]float64{"value": number})
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return nil
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if v, err := strconv.ParseFloat(text, 64); err == nil {
		return finiteValues(map[string]float64{"value": v})
	}
	out := map[string]float64{}
	for _, m := range grafanaVarValueRE.FindAllStringSubmatch(text, -1) {
		if v, err := strconv.ParseFloat(m[2], 64); err == nil {
			out[m[1]] = v
		}
	}
	if len(out) == 0 {
		for _, m := range grafanaPairRE.FindAllStringSubmatch(text, -1) {
			if v, err := strconv.ParseFloat(m[2], 64); err == nil {
				out[m[1]] = v
			}
		}
	}
	return finiteValues(out)
}

func parseTime(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil || t.Year() <= 1 {
		return time.Time{}
	}
	return t.UTC()
}

// normalizeBaseURL accepts an http(s) base URL; it drops a trailing slash.
func normalizeBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("url is required")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", errors.New("url must be an http:// or https:// address")
	}
	if u.User != nil {
		return "", errors.New("url must not carry credentials; use the token")
	}
	u.RawQuery, u.Fragment = "", ""
	return strings.TrimRight(u.String(), "/"), nil
}

// fetchGrafanaRules is one poll. Its error text names no secret: the caller
// still redacts it before it reaches a status, a log or the wire.
func fetchGrafanaRules(ctx context.Context, client *http.Client, baseURL, token string, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+GrafanaRulesPath, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("grafana did not answer within %s", timeout)
		}
		return nil, fmt.Errorf("grafana unreachable: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, grafanaRulesBodyMax+1))
	if err != nil {
		return nil, fmt.Errorf("grafana answer cut: %v", err)
	}
	if len(body) > grafanaRulesBodyMax {
		return nil, errors.New("grafana rules answer is larger than 32 MiB")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("grafana answered %s%s", resp.Status, grafanaErrorDetail(body))
	}
	return body, nil
}

func grafanaErrorDetail(body []byte) string {
	var payload struct {
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	if json.Unmarshal(body, &payload) == nil {
		msg := strings.TrimSpace(payload.Message)
		if msg == "" {
			msg = strings.TrimSpace(payload.Error)
		}
		if msg != "" {
			if runes := []rune(msg); len(runes) > 200 {
				msg = string(runes[:200]) + "…"
			}
			return ": " + msg
		}
	}
	return ""
}

// backoffDelay is the wait after `failures` consecutive failed polls:
// interval, then doubling up to max.
func backoffDelay(interval, max time.Duration, failures int) time.Duration {
	if failures <= 0 {
		return interval
	}
	d := interval
	for i := 0; i < failures; i++ {
		d *= 2
		if d >= max {
			return max
		}
	}
	return d
}
