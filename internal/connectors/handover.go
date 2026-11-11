package connectors

import (
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
)

// Changing the machine that listens, in one click (owner 2026-11-10):
// connectors:handover on the old listener hands the controller everything
// the connector is, secrets included; connectors:adopt on the new listener
// recreates it under the same id; the client then removes it from the old
// one. Handover is the only path that ever returns a secret, and only as a
// reply to the paired controller (a mutating channel). It removes nothing.

var connectorIDRE = regexp.MustCompile(`^gf-[0-9a-f]{8}$`)

var webhookHashRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Handover is the connectors:handover reply. Token and WebhookSecretHash are
// secrets: never log this value.
type Handover struct {
	Connector         Connector `json:"connector"`
	Token             string    `json:"token,omitempty"`
	WebhookSecretHash string    `json:"webhookSecretHash,omitempty"`
	Alerts            []Alert   `json:"alerts"`
}

func (s *Service) Handover(id string) (Handover, error) {
	id = strings.TrimSpace(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.connectors[id] == nil {
		return Handover{}, ErrUnknownConnector
	}
	sec := s.secrets[id]
	out := Handover{Connector: s.viewLocked(id), Token: sec.Token, WebhookSecretHash: sec.WebhookSecretHash, Alerts: []Alert{}}
	// Every alert, those of a switched-off rule too: they come back if it
	// is switched on again over there.
	for _, a := range s.alerts {
		if a.ConnectorID == id {
			out.Alerts = append(out.Alerts, cloneAlert(a))
		}
	}
	sort.Slice(out.Alerts, func(i, j int) bool { return out.Alerts[i].ID < out.Alerts[j].ID })
	return out, nil
}

// AdoptRequest is connectors:adopt: a Handover plus an optional url, the
// address this machine reaches Grafana by.
type AdoptRequest struct {
	Connector         Connector `json:"connector"`
	Token             *string   `json:"token"`
	WebhookSecretHash string    `json:"webhookSecretHash"`
	Alerts            []Alert   `json:"alerts"`
	URL               string    `json:"url"`
}

// Adopt creates a handed-over connector under its own id, imports its
// alerts as they were (link, dismissal, silence, history, episode start)
// without announcing anything, and starts its poller. An id already here is
// refused unless the connector is identical (a retried adopt).
func (s *Service) Adopt(req AdoptRequest) (Connector, error) {
	in := req.Connector
	id := strings.TrimSpace(in.ID)
	if !connectorIDRE.MatchString(id) {
		return Connector{}, errors.New("adopt needs the handed-over connector with its id")
	}
	if in.Kind != "" && in.Kind != KindGrafana {
		return Connector{}, fmt.Errorf("unknown connector kind %q", in.Kind)
	}
	rawURL := strings.TrimSpace(req.URL)
	if rawURL == "" {
		rawURL = in.URL
	}
	baseURL, err := normalizeBaseURL(rawURL)
	if err != nil {
		return Connector{}, err
	}
	token := ""
	if req.Token != nil {
		token = strings.TrimSpace(*req.Token)
	}
	if strings.ContainsAny(token, "\r\n") {
		return Connector{}, errors.New("token must be one line")
	}
	hash := strings.ToLower(strings.TrimSpace(req.WebhookSecretHash))
	if hash != "" && !webhookHashRE.MatchString(hash) {
		return Connector{}, errors.New("webhookSecretHash must be a SHA-256 hex digest")
	}

	c := cloneConnector(in)
	c.ID, c.URL = id, baseURL
	c.Status = Status{State: StateIdle}
	normalizeStored(&c)
	c.Name = strings.TrimSpace(c.Name)
	if c.Name == "" {
		c.Name = hostOf(baseURL)
	}
	sec := secrets{Token: token, WebhookSecretHash: hash}

	s.mu.Lock()
	if existing := s.connectors[id]; existing != nil {
		same := sameConfig(*existing, c) && s.secrets[id] == sec
		view := s.viewLocked(id)
		s.mu.Unlock()
		if !same {
			return Connector{}, fmt.Errorf("connector %s already exists on this machine", id)
		}
		return view, nil
	}
	imported := []Alert{}
	for i := range req.Alerts {
		a := cloneAlert(&req.Alerts[i])
		if a.ConnectorID != id || !strings.HasPrefix(a.ID, id+":") || len(a.ID) == len(id)+1 {
			continue
		}
		if a.State != AlertFiring && a.State != AlertResolved {
			continue
		}
		a.Labels = redactMap(a.Labels)
		a.Annotations = redactMap(a.Annotations)
		a.Values = finiteValues(a.Values)
		if len(a.History) > historyMax {
			a.History = a.History[len(a.History)-historyMax:]
		}
		imported = append(imported, a)
	}
	s.connectors[id] = &c
	s.secrets[id] = sec
	if err := s.saveSecretsLocked(id); err != nil {
		delete(s.connectors, id)
		delete(s.secrets, id)
		s.mu.Unlock()
		return Connector{}, err
	}
	for i := range imported {
		a := imported[i]
		s.alerts[a.ID] = &a
	}
	s.pruneLocked(s.opts.Now())
	s.gens[id]++
	saveErr := s.saveConfigLocked()
	if err := s.saveAlertsLocked(); saveErr == nil {
		saveErr = err
	}
	view := s.viewLocked(id)
	list := s.listLocked()
	enabled := ruleEnabledMap(&c)
	listable := []Alert{}
	for _, a := range imported {
		if kept := s.alerts[a.ID]; kept != nil && ruleOn(enabled, a.RuleKey) {
			listable = append(listable, cloneAlert(kept))
		}
	}
	s.mu.Unlock()
	if saveErr != nil {
		return Connector{}, saveErr
	}
	s.reconcile(id)
	s.broadcast(EventConnectorsChanged, map[string]any{"connectors": list})
	if len(listable) > 0 {
		s.broadcast(EventAlertsChanged, map[string]any{"alerts": listable, "announce": []string{}})
	}
	return view, nil
}

// sameConfig compares what the owner configured, not what was observed.
func sameConfig(a, b Connector) bool {
	return a.Name == b.Name && a.URL == b.URL && a.Mode == b.Mode && a.Notify == b.Notify &&
		a.Target == b.Target && a.GuidePattern == b.GuidePattern &&
		reflect.DeepEqual(a.RuleTargets, b.RuleTargets) && reflect.DeepEqual(a.MachineLabels, b.MachineLabels)
}
