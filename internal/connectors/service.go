package connectors

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"workass/internal/acp"
)

const (
	DefaultPollInterval = 30 * time.Second
	DefaultPollTimeout  = 10 * time.Second
	DefaultMaxBackoff   = 5 * time.Minute
	// Retention keeps resolved alerts listed for this long, then prunes them.
	Retention = 7 * 24 * time.Hour

	silenceMaxMinutes = 7 * 24 * 60
)

var ErrUnknownConnector = errors.New("unknown connector")

type Options struct {
	// StateDir is the daemon's state directory; files live in StateDir/connectors.
	StateDir     string
	Broadcast    func(channel string, payload any)
	Logf         func(format string, args ...any)
	Now          func() time.Time
	HTTPClient   *http.Client
	PollInterval time.Duration
	PollTimeout  time.Duration
	MaxBackoff   time.Duration
}

type Service struct {
	dir  string
	opts Options

	mu         sync.Mutex
	connectors map[string]*Connector
	secrets    map[string]secrets
	alerts     map[string]*Alert
	gens       map[string]uint64

	pollMu  sync.Mutex
	baseCtx context.Context
	closed  bool
	pollers map[string]*poller
	bg      sync.WaitGroup
	stopBg  context.CancelFunc
}

type poller struct {
	cancel context.CancelFunc
	done   chan struct{}
}

type storedConfig struct {
	Version    int         `json:"version"`
	Connectors []Connector `json:"connectors"`
}

type storedAlerts struct {
	Version int     `json:"version"`
	Alerts  []Alert `json:"alerts"`
}

// Open loads state/connectors. A missing directory is an empty store.
func Open(opts Options) (*Service, error) {
	if strings.TrimSpace(opts.StateDir) == "" {
		return nil, errors.New("connectors need a state directory")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{}
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = DefaultPollInterval
	}
	if opts.PollTimeout <= 0 {
		opts.PollTimeout = DefaultPollTimeout
	}
	if opts.MaxBackoff <= 0 {
		opts.MaxBackoff = DefaultMaxBackoff
	}
	s := &Service{
		dir:        filepath.Join(opts.StateDir, "connectors"),
		opts:       opts,
		connectors: map[string]*Connector{},
		secrets:    map[string]secrets{},
		alerts:     map[string]*Alert{},
		gens:       map[string]uint64{},
		pollers:    map[string]*poller{},
	}
	var cfg storedConfig
	if err := readJSON(filepath.Join(s.dir, "connectors.json"), &cfg); err != nil {
		return nil, fmt.Errorf("read connectors: %w", err)
	}
	for i := range cfg.Connectors {
		c := cfg.Connectors[i]
		if c.ID == "" {
			continue
		}
		normalizeStored(&c)
		s.connectors[c.ID] = &c
		var sec secrets
		if err := readJSON(filepath.Join(s.dir, "secrets", c.ID+".json"), &sec); err != nil {
			return nil, fmt.Errorf("read connector secrets: %w", err)
		}
		s.secrets[c.ID] = sec
	}
	var al storedAlerts
	if err := readJSON(filepath.Join(s.dir, "alerts.json"), &al); err != nil {
		return nil, fmt.Errorf("read alerts: %w", err)
	}
	for i := range al.Alerts {
		a := al.Alerts[i]
		if a.ID == "" || s.connectors[a.ConnectorID] == nil {
			continue
		}
		if a.Labels == nil {
			a.Labels = map[string]string{}
		}
		if a.Annotations == nil {
			a.Annotations = map[string]string{}
		}
		if a.History == nil {
			a.History = []HistoryEntry{}
		}
		s.alerts[a.ID] = &a
	}
	s.mu.Lock()
	s.pruneLocked(s.opts.Now())
	s.mu.Unlock()
	return s, nil
}

func normalizeStored(c *Connector) {
	c.Kind = KindGrafana
	if c.Mode != ModeWebhook {
		c.Mode = ModePoll
	}
	c.Notify = normalizeNotify(c.Notify)
	if c.Rules == nil {
		c.Rules = []Rule{}
	}
	if c.RuleTargets == nil {
		c.RuleTargets = map[string]Target{}
	}
	if len(c.MachineLabels) == 0 {
		c.MachineLabels = append([]string{}, defaultMachineLabels...)
	}
	if c.Status.State == "" {
		c.Status.State = StateIdle
	}
	c.WebhookPath = WebhookPrefix + c.ID + "/webhook"
	c.HasToken, c.TokenHint, c.HasWebhookSecret = false, "", false
}

// Start runs a poller per poll-mode connector and the hourly prune until
// ctx ends or Close.
func (s *Service) Start(ctx context.Context) {
	s.pollMu.Lock()
	if s.closed || s.baseCtx != nil {
		s.pollMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	s.baseCtx, s.stopBg = ctx, cancel
	s.pollMu.Unlock()
	s.mu.Lock()
	ids := make([]string, 0, len(s.connectors))
	for id := range s.connectors {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	for _, id := range ids {
		s.reconcile(id)
	}
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.prune()
			}
		}
	}()
}

// Close stops every poller and waits for them.
func (s *Service) Close() {
	s.pollMu.Lock()
	s.closed = true
	pollers := s.pollers
	s.pollers = map[string]*poller{}
	if s.stopBg != nil {
		s.stopBg()
	}
	s.pollMu.Unlock()
	for _, p := range pollers {
		p.cancel()
		<-p.done
	}
	s.bg.Wait()
}

// reconcile stops a connector's poller and starts a fresh one if it is a
// poll-mode connector and the service runs. Never call it holding s.mu.
func (s *Service) reconcile(id string) {
	s.pollMu.Lock()
	defer s.pollMu.Unlock()
	if p := s.pollers[id]; p != nil {
		p.cancel()
		<-p.done
		delete(s.pollers, id)
	}
	if s.closed || s.baseCtx == nil {
		return
	}
	s.mu.Lock()
	c := s.connectors[id]
	start := c != nil && c.Mode == ModePoll
	gen := s.gens[id]
	s.mu.Unlock()
	if !start {
		return
	}
	ctx, cancel := context.WithCancel(s.baseCtx)
	p := &poller{cancel: cancel, done: make(chan struct{})}
	s.pollers[id] = p
	go func() {
		defer close(p.done)
		s.pollLoop(ctx, id, gen)
	}()
}

func (s *Service) pollLoop(ctx context.Context, id string, gen uint64) {
	failures := 0
	for {
		if err := s.pollOnce(ctx, id, gen); err != nil {
			if ctx.Err() != nil {
				return
			}
			failures++
		} else {
			failures = 0
		}
		timer := time.NewTimer(backoffDelay(s.opts.PollInterval, s.opts.MaxBackoff, failures))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// pollOnce fetches the rules once and ingests them. gen fences a poll that
// started before the connector changed or was removed.
func (s *Service) pollOnce(ctx context.Context, id string, gen uint64) error {
	s.mu.Lock()
	c := s.connectors[id]
	if c == nil || s.gens[id] != gen {
		s.mu.Unlock()
		return nil
	}
	baseURL, token := c.URL, s.secrets[id].Token
	s.mu.Unlock()
	body, err := fetchGrafanaRules(ctx, s.opts.HTTPClient, baseURL, token, s.opts.PollTimeout)
	var result pollResult
	if err == nil {
		result, err = parseGrafanaRules(body, s.opts.Now())
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		s.setStatusError(id, gen, scrub(err.Error(), token))
		return err
	}
	s.ingestPoll(id, gen, result)
	return nil
}

func scrub(text, secret string) string {
	if secret != "" {
		text = strings.ReplaceAll(text, secret, "[redacted]")
	}
	return acp.RedactSensitiveText(text)
}

func (s *Service) logf(format string, args ...any) {
	if s.opts.Logf != nil {
		s.opts.Logf(format, args...)
	}
}

func (s *Service) broadcast(channel string, payload any) {
	if s.opts.Broadcast != nil {
		s.opts.Broadcast(channel, payload)
	}
}

func (s *Service) setStatusError(id string, gen uint64, msg string) {
	s.mu.Lock()
	c := s.connectors[id]
	if c == nil || s.gens[id] != gen {
		s.mu.Unlock()
		return
	}
	changed := c.Status.State != StateError || c.Status.Error != msg
	c.Status.State, c.Status.Error = StateError, msg
	var list []Connector
	if changed {
		s.saveConfigLocked()
		list = s.listLocked()
	}
	s.mu.Unlock()
	if changed {
		s.logf("[workass] connector %s: %s", id, msg)
		s.broadcast(EventConnectorsChanged, map[string]any{"connectors": list})
	}
}

// statusOKLocked marks a good contact; it reports whether the state changed.
func statusOKLocked(c *Connector, now time.Time) bool {
	changed := c.Status.State != StateOK
	c.Status = Status{State: StateOK, LastOkAt: now.UTC().Format(time.RFC3339)}
	return changed
}

// ---- ingest ----

type ingestOutcome struct {
	changed  map[string]bool
	announce []string
}

func (s *Service) ingestPoll(id string, gen uint64, result pollResult) {
	now := s.opts.Now()
	s.mu.Lock()
	c := s.connectors[id]
	if c == nil || s.gens[id] != gen {
		s.mu.Unlock()
		return
	}
	connChanged := statusOKLocked(c, now)
	if mergePolledRules(c, result.Rules) {
		connChanged = true
	}
	out := ingestOutcome{changed: map[string]bool{}}
	present := map[string]bool{}
	for _, obs := range result.Firing {
		alertID := id + ":" + obs.Fingerprint
		present[alertID] = true
		s.upsertFiringLocked(c, obs, false, now, &out)
	}
	for alertID, a := range s.alerts {
		if a.ConnectorID != id || a.State != AlertFiring || present[alertID] {
			continue
		}
		resolveLocked(a, now)
		out.changed[alertID] = true
	}
	s.finishIngestLocked(c, connChanged, out)
}

func (s *Service) ingestWebhook(id string, observations []observation) (int, error) {
	now := s.opts.Now()
	s.mu.Lock()
	c := s.connectors[id]
	if c == nil || c.Mode != ModeWebhook {
		s.mu.Unlock()
		return 0, ErrUnknownConnector
	}
	connChanged := statusOKLocked(c, now)
	out := ingestOutcome{changed: map[string]bool{}}
	for _, obs := range observations {
		if learnRule(c, obs, now) {
			connChanged = true
		}
		if obs.Firing {
			s.upsertFiringLocked(c, obs, true, now, &out)
			continue
		}
		alertID := id + ":" + obs.Fingerprint
		a := s.alerts[alertID]
		if a == nil {
			// A resolve for an episode this daemon never saw: kept as
			// resolved history, never announced.
			a = s.newAlertLocked(c, obs, now)
			a.State = AlertResolved
			a.StartsAt = timeOr(obs.StartsAt, now)
			a.History = appendHistory(a.History, a.StartsAt, AlertFiring)
			end := timeOr(obs.EndsAt, now)
			a.EndsAt = end
			a.History = appendHistory(a.History, end, AlertResolved)
			s.alerts[alertID] = a
			out.changed[alertID] = true
			continue
		}
		changed := applyObservation(a, c, obs, now)
		if a.State == AlertFiring {
			end := obs.EndsAt
			if end.IsZero() || end.After(now) {
				end = now
			}
			resolveLocked(a, end)
			changed = true
		}
		if changed {
			out.changed[alertID] = true
		}
	}
	s.finishIngestLocked(c, connChanged, out)
	return len(observations), nil
}

// finishIngestLocked persists, releases s.mu and broadcasts outside it.
func (s *Service) finishIngestLocked(c *Connector, connChanged bool, out ingestOutcome) {
	s.pruneLocked(s.opts.Now())
	if connChanged {
		s.saveConfigLocked()
	}
	if len(out.changed) > 0 {
		s.saveAlertsLocked()
	}
	var connList []Connector
	if connChanged {
		connList = s.listLocked()
	}
	enabled := ruleEnabledMap(c)
	changed := []Alert{}
	ids := make([]string, 0, len(out.changed))
	for id := range out.changed {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		a := s.alerts[id]
		if a == nil || !ruleOn(enabled, a.RuleKey) {
			continue
		}
		changed = append(changed, cloneAlert(a))
	}
	announce := []string{}
	for _, id := range out.announce {
		if a := s.alerts[id]; a != nil && ruleOn(enabled, a.RuleKey) {
			announce = append(announce, id)
		}
	}
	s.mu.Unlock()
	if connList != nil {
		s.broadcast(EventConnectorsChanged, map[string]any{"connectors": connList})
	}
	if len(changed) > 0 {
		s.broadcast(EventAlertsChanged, map[string]any{"alerts": changed, "announce": announce})
	}
}

func (s *Service) newAlertLocked(c *Connector, obs observation, now time.Time) *Alert {
	a := &Alert{
		ID: c.ID + ":" + obs.Fingerprint, ConnectorID: c.ID,
		Labels: map[string]string{}, Annotations: map[string]string{}, History: []HistoryEntry{},
	}
	applyObservation(a, c, obs, now)
	return a
}

// applyObservation copies what a transport saw (redacted) and reports
// whether anything a person would see changed.
func applyObservation(a *Alert, c *Connector, obs observation, now time.Time) bool {
	labels := redactMap(obs.Labels)
	annotations := redactMap(obs.Annotations)
	values := finiteValues(obs.Values)
	severity := normalizeSeverity(labels["severity"])
	machine := machineFor(labels, c.MachineLabels)
	generator := acp.RedactSensitiveText(obs.GeneratorURL)
	title := obs.RuleTitle
	if title == "" {
		title = labels["alertname"]
	}
	changed := !reflect.DeepEqual(a.Labels, labels) || !reflect.DeepEqual(a.Annotations, annotations) ||
		!reflect.DeepEqual(a.Values, values) || a.Severity != severity || a.Machine != machine ||
		a.RuleKey != obs.RuleKey || a.RuleTitle != title || (generator != "" && a.GeneratorURL != generator)
	a.Labels, a.Annotations, a.Values = labels, annotations, values
	a.Severity, a.Machine, a.RuleKey, a.RuleTitle = severity, machine, obs.RuleKey, title
	if generator != "" {
		a.GeneratorURL = generator
	}
	a.LastSeenAt = now.UTC().Format(time.RFC3339)
	return changed
}

// upsertFiringLocked is the episode rule. A missing or resolved alert, or a
// firing one whose start moved later (the resolve was missed between two
// looks), starts a new episode: announced, link and dismissal cleared. A
// webhook re-send of the same firing only counts a repeat, never announced.
func (s *Service) upsertFiringLocked(c *Connector, obs observation, resend bool, now time.Time, out *ingestOutcome) {
	alertID := c.ID + ":" + obs.Fingerprint
	a := s.alerts[alertID]
	if a == nil {
		a = s.newAlertLocked(c, obs, now)
		s.alerts[alertID] = a
		startEpisode(a, obs, now)
		out.changed[alertID] = true
		out.announce = append(out.announce, alertID)
		return
	}
	changed := applyObservation(a, c, obs, now)
	if a.State != AlertFiring {
		startEpisode(a, obs, now)
		out.changed[alertID] = true
		out.announce = append(out.announce, alertID)
		return
	}
	if prev := parseTime(a.StartsAt); !obs.StartsAt.IsZero() && !prev.IsZero() && obs.StartsAt.After(prev.Add(time.Second)) {
		a.History = appendHistory(a.History, obs.StartsAt.UTC().Format(time.RFC3339), AlertResolved)
		startEpisode(a, obs, now)
		out.changed[alertID] = true
		out.announce = append(out.announce, alertID)
		return
	}
	if resend {
		a.Repeats++
		a.LastRepeatAt = now.UTC().Format(time.RFC3339)
		changed = true
	}
	if changed {
		out.changed[alertID] = true
	}
}

func startEpisode(a *Alert, obs observation, now time.Time) {
	a.State = AlertFiring
	a.StartsAt = timeOr(obs.StartsAt, now)
	a.EndsAt = ""
	a.Repeats = 0
	a.LastRepeatAt = ""
	a.Link = nil
	a.DismissedAt = ""
	a.History = appendHistory(a.History, a.StartsAt, AlertFiring)
}

func resolveLocked(a *Alert, at time.Time) {
	if a.State != AlertFiring {
		return
	}
	a.State = AlertResolved
	a.EndsAt = at.UTC().Format(time.RFC3339)
	a.History = appendHistory(a.History, a.EndsAt, AlertResolved)
}

func appendHistory(h []HistoryEntry, at, state string) []HistoryEntry {
	h = append(h, HistoryEntry{At: at, State: state})
	if len(h) > historyMax {
		h = append([]HistoryEntry{}, h[len(h)-historyMax:]...)
	}
	return h
}

func timeOr(t, fallback time.Time) string {
	if t.IsZero() {
		t = fallback
	}
	return t.UTC().Format(time.RFC3339)
}

func redactMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		if strings.HasPrefix(k, "__") {
			continue
		}
		out[k] = acp.RedactSensitiveText(v)
	}
	return out
}

// mergePolledRules takes a poll's rules: every rule Grafana has now, keeping
// the owner's enabled choice. A vanished rule is dropped unless the owner
// turned it off (that choice is kept for when it comes back).
func mergePolledRules(c *Connector, polled []Rule) bool {
	old := map[string]Rule{}
	for _, r := range c.Rules {
		old[r.Key] = r
	}
	next := make([]Rule, 0, len(polled))
	seen := map[string]bool{}
	for _, r := range polled {
		if seen[r.Key] {
			continue
		}
		seen[r.Key] = true
		if prev, ok := old[r.Key]; ok {
			r.Enabled = prev.Enabled
		}
		next = append(next, r)
	}
	for _, r := range c.Rules {
		if !seen[r.Key] && !r.Enabled {
			next = append(next, r)
		}
	}
	changed := len(next) != len(c.Rules)
	if !changed {
		for i := range next {
			a, b := next[i], c.Rules[i]
			if a.Key != b.Key || a.Title != b.Title || a.Folder != b.Folder || a.Severity != b.Severity || a.Enabled != b.Enabled {
				changed = true
				break
			}
		}
	}
	c.Rules = next
	return changed
}

// learnRule adds a rule a webhook named for the first time.
func learnRule(c *Connector, obs observation, now time.Time) bool {
	seen := now.UTC().Format(time.RFC3339)
	for i := range c.Rules {
		if c.Rules[i].Key == obs.RuleKey {
			c.Rules[i].SeenAt = seen
			return false
		}
	}
	c.Rules = append(c.Rules, Rule{
		Key: obs.RuleKey, Title: obs.RuleTitle, Folder: obs.Folder,
		Severity: normalizeSeverity(obs.Labels["severity"]), Enabled: true, SeenAt: seen,
	})
	return true
}

func ruleEnabledMap(c *Connector) map[string]bool {
	out := map[string]bool{}
	if c == nil {
		return out
	}
	for _, r := range c.Rules {
		out[r.Key] = r.Enabled
	}
	return out
}

func ruleOn(enabled map[string]bool, key string) bool {
	on, known := enabled[key]
	return !known || on
}

// ---- prune ----

func (s *Service) prune() {
	s.mu.Lock()
	removed := s.pruneLocked(s.opts.Now())
	if len(removed) > 0 {
		s.saveAlertsLocked()
	}
	s.mu.Unlock()
	if len(removed) > 0 {
		s.broadcast(EventAlertsChanged, map[string]any{"alerts": []Alert{}, "announce": []string{}, "removed": removed})
	}
}

// pruneLocked drops resolved alerts that ended more than Retention ago. A
// firing alert is never pruned: dropping it would re-announce it next look.
func (s *Service) pruneLocked(now time.Time) []string {
	var removed []string
	cutoff := now.Add(-Retention)
	for id, a := range s.alerts {
		if s.connectors[a.ConnectorID] == nil {
			delete(s.alerts, id)
			removed = append(removed, id)
			continue
		}
		if a.State != AlertResolved {
			continue
		}
		end := parseTime(a.EndsAt)
		if end.IsZero() {
			end = parseTime(a.LastSeenAt)
		}
		if !end.IsZero() && end.Before(cutoff) {
			delete(s.alerts, id)
			removed = append(removed, id)
		}
	}
	sort.Strings(removed)
	return removed
}

// ---- reads ----

func (s *Service) List() []Connector {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listLocked()
}

func (s *Service) listLocked() []Connector {
	ids := make([]string, 0, len(s.connectors))
	for id := range s.connectors {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]Connector, 0, len(ids))
	for _, id := range ids {
		out = append(out, s.viewLocked(id))
	}
	return out
}

func (s *Service) viewLocked(id string) Connector {
	c := cloneConnector(*s.connectors[id])
	sec := s.secrets[id]
	c.HasToken = sec.Token != ""
	if c.HasToken {
		runes := []rune(sec.Token)
		if len(runes) > 4 {
			runes = runes[len(runes)-4:]
		}
		c.TokenHint = string(runes)
	}
	c.HasWebhookSecret = sec.WebhookSecretHash != ""
	c.WebhookPath = WebhookPrefix + id + "/webhook"
	return c
}

// Alerts lists every alert of an enabled rule, newest start first.
func (s *Service) Alerts() []Alert {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Alert{}
	enabled := map[string]map[string]bool{}
	for id, c := range s.connectors {
		enabled[id] = ruleEnabledMap(c)
	}
	for _, a := range s.alerts {
		if !ruleOn(enabled[a.ConnectorID], a.RuleKey) {
			continue
		}
		out = append(out, cloneAlert(a))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].StartsAt != out[j].StartsAt {
			return out[i].StartsAt > out[j].StartsAt
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func (s *Service) webhookSecretHash(id string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.connectors[id]
	if c == nil || c.Mode != ModeWebhook {
		return "", false
	}
	return s.secrets[id].WebhookSecretHash, true
}

// ---- mutations ----

type RuleToggle struct {
	Key     string `json:"key"`
	Enabled bool   `json:"enabled"`
}

// SetRequest is connectors:set. Token nil = keep, "" = clear.
type SetRequest struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	URL           string            `json:"url"`
	Mode          string            `json:"mode"`
	Token         *string           `json:"token"`
	Notify        string            `json:"notify"`
	Target        *Target           `json:"target"`
	RuleTargets   map[string]Target `json:"ruleTargets"`
	GuidePattern  *string           `json:"guidePattern"`
	Rules         []RuleToggle      `json:"rules"`
	MachineLabels []string          `json:"machineLabels"`
}

// Set creates or updates a connector. webhookSecret is non-empty only when
// a secret was generated by this call (shown once).
func (s *Service) Set(req SetRequest) (Connector, string, error) {
	baseURL, err := normalizeBaseURL(req.URL)
	if err != nil {
		return Connector{}, "", err
	}
	mode := strings.TrimSpace(req.Mode)
	if mode != ModePoll && mode != ModeWebhook {
		return Connector{}, "", errors.New(`mode must be "poll" or "webhook"`)
	}
	if req.Token != nil && strings.ContainsAny(*req.Token, "\r\n") {
		return Connector{}, "", errors.New("token must be one line")
	}
	s.mu.Lock()
	id := strings.TrimSpace(req.ID)
	var c *Connector
	isNew := id == ""
	if !isNew {
		c = s.connectors[id]
		if c == nil {
			s.mu.Unlock()
			return Connector{}, "", ErrUnknownConnector
		}
	} else {
		for {
			id = "gf-" + randomHex(4)
			if s.connectors[id] == nil {
				break
			}
		}
		c = &Connector{ID: id, Status: Status{State: StateIdle}}
		normalizeStored(c)
	}
	before := cloneConnector(*c)
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = hostOf(baseURL)
	}
	c.Name, c.URL, c.Mode = name, baseURL, mode
	if req.Notify != "" || c.Notify == "" {
		c.Notify = normalizeNotify(req.Notify)
	}
	if req.Target != nil {
		c.Target = Target{MachineID: strings.TrimSpace(req.Target.MachineID), CWD: strings.TrimSpace(req.Target.CWD)}
	}
	if req.RuleTargets != nil {
		targets := map[string]Target{}
		for k, v := range req.RuleTargets {
			if k = strings.TrimSpace(k); k != "" {
				targets[k] = Target{MachineID: strings.TrimSpace(v.MachineID), CWD: strings.TrimSpace(v.CWD)}
			}
		}
		c.RuleTargets = targets
	}
	if req.GuidePattern != nil {
		c.GuidePattern = strings.TrimSpace(*req.GuidePattern)
	}
	if req.MachineLabels != nil {
		labels := []string{}
		for _, l := range req.MachineLabels {
			if l = strings.TrimSpace(l); l != "" {
				labels = append(labels, l)
			}
		}
		if len(labels) == 0 {
			labels = append(labels, defaultMachineLabels...)
		}
		c.MachineLabels = labels
	}
	for _, toggle := range req.Rules {
		key := strings.TrimSpace(toggle.Key)
		if key == "" {
			continue
		}
		found := false
		for i := range c.Rules {
			if c.Rules[i].Key == key {
				c.Rules[i].Enabled = toggle.Enabled
				found = true
			}
		}
		if !found {
			c.Rules = append(c.Rules, Rule{Key: key, Title: strings.TrimPrefix(key, "title:"), Severity: SeverityWarning, Enabled: toggle.Enabled})
		}
	}
	sec := s.secrets[id]
	transportChanged := before.URL != c.URL || before.Mode != c.Mode
	if req.Token != nil && strings.TrimSpace(*req.Token) != sec.Token {
		sec.Token = strings.TrimSpace(*req.Token)
		transportChanged = true
	}
	secret := ""
	if c.Mode == ModeWebhook && sec.WebhookSecretHash == "" {
		secret = newWebhookSecret()
		sec.WebhookSecretHash = hashSecret(secret)
	}
	if transportChanged {
		c.Status = Status{State: StateIdle}
	}
	s.connectors[id] = c
	s.secrets[id] = sec
	if err := s.saveSecretsLocked(id); err != nil {
		if isNew {
			delete(s.connectors, id)
			delete(s.secrets, id)
		}
		s.mu.Unlock()
		return Connector{}, "", err
	}
	if isNew || transportChanged {
		// Fences a poll already in flight against the old transport.
		s.gens[id]++
	}
	if err := s.saveConfigLocked(); err != nil {
		s.mu.Unlock()
		return Connector{}, "", err
	}
	view := s.viewLocked(id)
	list := s.listLocked()
	alertsChanged := s.rulesToggledAlertsLocked(c, before)
	s.mu.Unlock()
	if isNew || transportChanged {
		s.reconcile(id)
	}
	s.broadcast(EventConnectorsChanged, map[string]any{"connectors": list})
	if len(alertsChanged) > 0 {
		s.broadcast(EventAlertsChanged, map[string]any{"alerts": alertsChanged, "announce": []string{}})
	}
	return view, secret, nil
}

// rulesToggledAlertsLocked: alerts of a rule switched back on reappear in
// the list. (A switched-off rule's alerts vanish; the client re-lists on
// connectors:changed.)
func (s *Service) rulesToggledAlertsLocked(c *Connector, before Connector) []Alert {
	was := ruleEnabledMap(&before)
	now := ruleEnabledMap(c)
	var out []Alert
	for _, a := range s.alerts {
		if a.ConnectorID == c.ID && !ruleOn(was, a.RuleKey) && ruleOn(now, a.RuleKey) {
			out = append(out, cloneAlert(a))
		}
	}
	return out
}

func hostOf(base string) string {
	h := strings.TrimPrefix(strings.TrimPrefix(base, "https://"), "http://")
	if i := strings.IndexAny(h, "/"); i >= 0 {
		h = h[:i]
	}
	return h
}

// Remove drops a connector, its secrets and its alerts.
func (s *Service) Remove(id string) error {
	id = strings.TrimSpace(id)
	s.mu.Lock()
	if s.connectors[id] == nil {
		s.mu.Unlock()
		return ErrUnknownConnector
	}
	delete(s.connectors, id)
	delete(s.secrets, id)
	s.gens[id]++
	removed := []string{}
	for alertID, a := range s.alerts {
		if a.ConnectorID == id {
			delete(s.alerts, alertID)
			removed = append(removed, alertID)
		}
	}
	sort.Strings(removed)
	err := s.saveConfigLocked()
	if rmErr := os.Remove(filepath.Join(s.dir, "secrets", id+".json")); rmErr != nil && !os.IsNotExist(rmErr) && err == nil {
		err = rmErr
	}
	if len(removed) > 0 {
		s.saveAlertsLocked()
	}
	list := s.listLocked()
	s.mu.Unlock()
	s.reconcile(id)
	s.broadcast(EventConnectorsChanged, map[string]any{"connectors": list})
	if len(removed) > 0 {
		s.broadcast(EventAlertsChanged, map[string]any{"alerts": []Alert{}, "announce": []string{}, "removed": removed})
	}
	return err
}

// RotateSecret mints a new webhook secret and returns it once.
func (s *Service) RotateSecret(id string) (string, error) {
	id = strings.TrimSpace(id)
	s.mu.Lock()
	if s.connectors[id] == nil {
		s.mu.Unlock()
		return "", ErrUnknownConnector
	}
	secret := newWebhookSecret()
	sec := s.secrets[id]
	sec.WebhookSecretHash = hashSecret(secret)
	s.secrets[id] = sec
	if err := s.saveSecretsLocked(id); err != nil {
		s.mu.Unlock()
		return "", err
	}
	list := s.listLocked()
	s.mu.Unlock()
	s.broadcast(EventConnectorsChanged, map[string]any{"connectors": list})
	return secret, nil
}

type TestRequest struct {
	ID    string  `json:"id"`
	URL   string  `json:"url"`
	Token *string `json:"token"`
}

type TestResult struct {
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
	Rules  int    `json:"rules"`
	Firing int    `json:"firing"`
}

// Test fetches the rules once with {id} (stored url/token) or {url, token};
// explicit fields override a stored connector's. It changes nothing.
func (s *Service) Test(ctx context.Context, req TestRequest) TestResult {
	baseURL, token := strings.TrimSpace(req.URL), ""
	if id := strings.TrimSpace(req.ID); id != "" {
		s.mu.Lock()
		c := s.connectors[id]
		if c == nil {
			s.mu.Unlock()
			return TestResult{Error: ErrUnknownConnector.Error()}
		}
		if baseURL == "" {
			baseURL = c.URL
		}
		token = s.secrets[id].Token
		s.mu.Unlock()
	}
	if req.Token != nil {
		token = strings.TrimSpace(*req.Token)
	}
	baseURL, err := normalizeBaseURL(baseURL)
	if err != nil {
		return TestResult{Error: err.Error()}
	}
	body, err := fetchGrafanaRules(ctx, s.opts.HTTPClient, baseURL, token, s.opts.PollTimeout)
	if err != nil {
		return TestResult{Error: scrub(err.Error(), token)}
	}
	result, err := parseGrafanaRules(body, s.opts.Now())
	if err != nil {
		return TestResult{Error: scrub(err.Error(), token)}
	}
	return TestResult{OK: true, Rules: len(result.Rules), Firing: len(result.Firing)}
}

type ActRequest struct {
	IDs     []string `json:"ids"`
	Action  string   `json:"action"`
	Minutes float64  `json:"minutes"`
	Link    *struct {
		MachineID string `json:"machineId"`
		TabID     string `json:"tabId"`
		ChatID    string `json:"chatId"`
	} `json:"link"`
}

// Act applies one owner decision to alerts and returns them.
func (s *Service) Act(req ActRequest) ([]Alert, error) {
	action := strings.TrimSpace(req.Action)
	switch action {
	case "silence", "unsilence", "dismiss", "undismiss", "link":
	default:
		return nil, fmt.Errorf("unknown action %q", req.Action)
	}
	if action == "link" && (req.Link == nil || strings.TrimSpace(req.Link.ChatID) == "") {
		return nil, errors.New("link needs {machineId, tabId, chatId}")
	}
	if len(req.IDs) == 0 {
		return nil, errors.New("ids are required")
	}
	now := s.opts.Now().UTC()
	stamp := now.Format(time.RFC3339)
	s.mu.Lock()
	changed := []Alert{}
	for _, id := range req.IDs {
		a := s.alerts[strings.TrimSpace(id)]
		if a == nil {
			continue
		}
		switch action {
		case "silence":
			minutes := req.Minutes
			if minutes <= 0 {
				minutes = 10
			}
			if minutes > silenceMaxMinutes {
				minutes = silenceMaxMinutes
			}
			a.SilencedUntil = now.Add(time.Duration(minutes * float64(time.Minute))).Format(time.RFC3339)
		case "unsilence":
			a.SilencedUntil = ""
		case "dismiss":
			a.DismissedAt = stamp
		case "undismiss":
			a.DismissedAt = ""
		case "link":
			a.Link = &AlertLink{
				MachineID: strings.TrimSpace(req.Link.MachineID), TabID: strings.TrimSpace(req.Link.TabID),
				ChatID: strings.TrimSpace(req.Link.ChatID), At: stamp,
			}
		}
		changed = append(changed, cloneAlert(a))
	}
	var err error
	if len(changed) > 0 {
		err = s.saveAlertsLocked()
	}
	s.mu.Unlock()
	if len(changed) > 0 {
		s.broadcast(EventAlertsChanged, map[string]any{"alerts": changed, "announce": []string{}})
	}
	return changed, err
}

// ---- persistence ----

func (s *Service) saveConfigLocked() error {
	cfg := storedConfig{Version: 1, Connectors: []Connector{}}
	ids := make([]string, 0, len(s.connectors))
	for id := range s.connectors {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		c := cloneConnector(*s.connectors[id])
		c.HasToken, c.TokenHint, c.HasWebhookSecret = false, "", false
		cfg.Connectors = append(cfg.Connectors, c)
	}
	return s.write(filepath.Join(s.dir, "connectors.json"), cfg)
}

func (s *Service) saveAlertsLocked() error {
	out := storedAlerts{Version: 1, Alerts: []Alert{}}
	for _, a := range s.alerts {
		out.Alerts = append(out.Alerts, cloneAlert(a))
	}
	sort.Slice(out.Alerts, func(i, j int) bool { return out.Alerts[i].ID < out.Alerts[j].ID })
	return s.write(filepath.Join(s.dir, "alerts.json"), out)
}

func (s *Service) saveSecretsLocked(id string) error {
	return s.write(filepath.Join(s.dir, "secrets", id+".json"), s.secrets[id])
}

func (s *Service) write(path string, value any) error {
	err := writeJSONAtomic(path, value)
	if err != nil {
		// The path names no secret; the error is the filesystem's.
		s.logf("[workass] connectors: write %s: %v", filepath.Base(path), err)
	}
	return err
}

func writeJSONAtomic(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func readJSON(path string, into any) error {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil
	}
	return json.Unmarshal(data, into)
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("connectors: no randomness: " + err.Error())
	}
	return hex.EncodeToString(b)
}

func newWebhookSecret() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("connectors: no randomness: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
