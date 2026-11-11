package connectors

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
)

// WebhookBodyMax caps one Grafana webhook body.
const WebhookBodyMax = 1 << 20

type grafanaWebhookPayload struct {
	Status string                `json:"status"`
	Alerts []grafanaWebhookAlert `json:"alerts"`
}

type grafanaWebhookAlert struct {
	Status       string                     `json:"status"`
	Labels       stringMap                  `json:"labels"`
	Annotations  stringMap                  `json:"annotations"`
	StartsAt     string                     `json:"startsAt"`
	EndsAt       string                     `json:"endsAt"`
	GeneratorURL string                     `json:"generatorURL"`
	Fingerprint  string                     `json:"fingerprint"`
	Values       map[string]json.RawMessage `json:"values"`
}

var grafanaRuleUIDRE = regexp.MustCompile(`/alerting/grafana/([^/?#]+)/view`)

func webhookObservations(payload grafanaWebhookPayload) []observation {
	out := make([]observation, 0, len(payload.Alerts))
	for _, a := range payload.Alerts {
		labels := map[string]string(a.Labels)
		if labels == nil {
			labels = map[string]string{}
		}
		title := strings.TrimSpace(labels["alertname"])
		key := ""
		if m := grafanaRuleUIDRE.FindStringSubmatch(a.GeneratorURL); m != nil {
			key = m[1]
		} else if uid := strings.TrimSpace(labels["__alert_rule_uid__"]); uid != "" {
			key = uid
		} else {
			key = titleKey(title)
		}
		status := strings.ToLower(strings.TrimSpace(a.Status))
		if status == "" {
			status = strings.ToLower(strings.TrimSpace(payload.Status))
		}
		fingerprint := strings.TrimSpace(a.Fingerprint)
		if fingerprint == "" || !safeFingerprint(fingerprint) {
			fingerprint = labelFingerprint(labels)
		}
		values := map[string]float64{}
		for k, raw := range a.Values {
			var v float64
			if json.Unmarshal(raw, &v) == nil {
				values[k] = v
			}
		}
		out = append(out, observation{
			Fingerprint: fingerprint, RuleKey: key, RuleTitle: title,
			Folder: strings.TrimSpace(labels["grafana_folder"]),
			Labels: labels, Annotations: map[string]string(a.Annotations),
			Values: finiteValues(values), Firing: status != AlertResolved,
			StartsAt: parseTime(a.StartsAt), EndsAt: parseTime(a.EndsAt),
			GeneratorURL: strings.TrimSpace(a.GeneratorURL),
		})
	}
	return out
}

var fingerprintRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func safeFingerprint(s string) bool { return fingerprintRE.MatchString(s) }

func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func secretMatches(given, storedHash string) bool {
	if given == "" || storedHash == "" {
		return false
	}
	want, err := hex.DecodeString(storedHash)
	if err != nil {
		return false
	}
	got := sha256.Sum256([]byte(given))
	return subtle.ConstantTimeCompare(got[:], want) == 1
}

func bearerToken(r *http.Request) string {
	h := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(h) < 7 || !strings.EqualFold(h[:7], "bearer ") {
		return ""
	}
	return strings.TrimSpace(h[7:])
}

// WebhookHandler serves POST /workass/connectors/<id>/webhook. It touches
// the alert table only, never a chat (owner law 1).
func (s *Service) WebhookHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, WebhookPrefix)
		id, tail, ok := strings.Cut(rest, "/")
		if !ok || tail != "webhook" || id == "" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		hash, found := s.webhookSecretHash(id)
		if !found {
			http.NotFound(w, r)
			return
		}
		if !secretMatches(bearerToken(r), hash) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="workass"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, WebhookBodyMax))
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				http.Error(w, "body larger than 1 MiB", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "unreadable body", http.StatusBadRequest)
			return
		}
		var payload grafanaWebhookPayload
		if err := json.Unmarshal(body, &payload); err != nil {
			http.Error(w, "body is not a Grafana webhook JSON", http.StatusBadRequest)
			return
		}
		n, err := s.ingestWebhook(id, webhookObservations(payload))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "alerts": n})
	})
}
