package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"workass/internal/acp"
)

type daemonOptions struct {
	StateDir            string
	ConfigPath          string
	Engine              engineConfig
	EngineFlagOverrides map[string]bool
	ChatControl         *chatControlCoordinator
}

type daemonState struct {
	config   *appConfigStore
	setting  *appSettingsStore
	stateDir string
}

type engineConfig struct {
	HibernateTTL            time.Duration
	MaxRSSKB                int
	MaxAge                  time.Duration
	SpareSessions           int
	RSSSampleInterval       time.Duration
	CompactionEnabled       bool
	CompactionThresholdPct  int
	CompactionKeepLastTurns int
}

func defaultEngineConfig() engineConfig {
	return engineConfig{
		HibernateTTL:            20 * time.Minute,
		MaxRSSKB:                4 * 1024 * 1024,
		MaxAge:                  12 * time.Hour,
		SpareSessions:           0,
		RSSSampleInterval:       30 * time.Second,
		CompactionEnabled:       true,
		CompactionThresholdPct:  80,
		CompactionKeepLastTurns: 4,
	}
}

func (c engineConfig) publicMap() map[string]any {
	return map[string]any{
		"hibernateTtlMs":          c.HibernateTTL.Milliseconds(),
		"maxRssKb":                c.MaxRSSKB,
		"maxAgeMs":                c.MaxAge.Milliseconds(),
		"spareSessions":           c.SpareSessions,
		"rssSampleIntervalMs":     c.RSSSampleInterval.Milliseconds(),
		"compactionEnabled":       c.CompactionEnabled,
		"compactionThresholdPct":  c.CompactionThresholdPct,
		"compactionKeepLastTurns": c.CompactionKeepLastTurns,
	}
}

func newDaemonState(cwd string, opts daemonOptions) *daemonState {
	engine := opts.Engine
	if engine == (engineConfig{}) {
		engine = defaultEngineConfig()
	}
	configPath := strings.TrimSpace(opts.ConfigPath)
	if configPath == "" && strings.TrimSpace(opts.StateDir) != "" {
		configPath = filepath.Join(filepath.Dir(opts.StateDir), "app-config.json")
	}
	settingsPath := ""
	if strings.TrimSpace(opts.StateDir) != "" {
		settingsPath = filepath.Join(opts.StateDir, "app-settings.json")
	}
	store := newAppConfigStore(configPath, engine, opts.EngineFlagOverrides)
	if configPath != "" {
		_ = store.ensureFile()
	}
	_ = cwd
	return &daemonState{
		config:   store,
		setting:  newAppSettingsStore(settingsPath),
		stateDir: strings.TrimSpace(opts.StateDir),
	}
}

type appConfigStore struct {
	mu            sync.Mutex
	path          string
	baseEngine    engineConfig
	flagOverrides map[string]bool
}

func newAppConfigStore(path string, engine engineConfig, flagOverrides map[string]bool) *appConfigStore {
	if engine == (engineConfig{}) {
		engine = defaultEngineConfig()
	}
	cp := map[string]bool{}
	for k, v := range flagOverrides {
		if v {
			cp[k] = true
		}
	}
	return &appConfigStore{path: path, baseEngine: engine, flagOverrides: cp}
}

func (s *appConfigStore) ensureFile() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.path == "" {
		return nil
	}
	if _, err := os.Stat(s.path); err == nil {
		return nil
	}
	cfg := s.getLocked()
	cfg["engine"] = s.effectiveEngineLocked(cfg).publicMap()
	return s.writeWholeLocked(cfg)
}

func (s *appConfigStore) getRedacted() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	full := s.getLocked()
	full["engine"] = s.effectiveEngineLocked(full).publicMap()
	if redacted, ok := redactValue(full).(map[string]any); ok {
		return redacted
	}
	return full
}

func (s *appConfigStore) effectiveEngine() engineConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.effectiveEngineLocked(s.getLocked())
}

func (s *appConfigStore) set(patch map[string]any) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := deepMergeMap(s.getLocked(), patch)
	if err := s.writeWholeLocked(next); err != nil {
		return nil, err
	}
	next["engine"] = s.effectiveEngineLocked(next).publicMap()
	if redacted, ok := redactValue(next).(map[string]any); ok {
		return redacted, nil
	}
	return next, nil
}

func (s *appConfigStore) getLocked() map[string]any {
	return deepMergeMap(defaultAppConfig(), s.readRawLocked())
}

func (s *appConfigStore) readRawLocked() map[string]any {
	if s.path == "" {
		return map[string]any{}
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return map[string]any{}
	}
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
	var out map[string]any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil || out == nil {
		return map[string]any{}
	}
	return out
}

func (s *appConfigStore) writeWholeLocked(cfg map[string]any) error {
	if s.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *appConfigStore) effectiveEngineLocked(cfg map[string]any) engineConfig {
	engine := engineFromConfig(cfg, s.baseEngine)
	if s.flagOverrides["hibernate-ttl"] {
		engine.HibernateTTL = s.baseEngine.HibernateTTL
	}
	if s.flagOverrides["engine-max-rss-kb"] {
		engine.MaxRSSKB = s.baseEngine.MaxRSSKB
	}
	if s.flagOverrides["engine-max-age"] {
		engine.MaxAge = s.baseEngine.MaxAge
	}
	if s.flagOverrides["spare-sessions"] {
		engine.SpareSessions = s.baseEngine.SpareSessions
	}
	if s.flagOverrides["rss-sample-interval"] {
		engine.RSSSampleInterval = s.baseEngine.RSSSampleInterval
	}
	if s.flagOverrides["compaction-enabled"] {
		engine.CompactionEnabled = s.baseEngine.CompactionEnabled
	}
	if s.flagOverrides["compaction-threshold-pct"] {
		engine.CompactionThresholdPct = s.baseEngine.CompactionThresholdPct
	}
	if s.flagOverrides["compaction-keep-last-turns"] {
		engine.CompactionKeepLastTurns = s.baseEngine.CompactionKeepLastTurns
	}
	return engine
}

func engineFromConfig(cfg map[string]any, base engineConfig) engineConfig {
	if base == (engineConfig{}) {
		base = defaultEngineConfig()
	}
	engine := base
	raw := mapFromAnyMain(cfg["engine"])
	if raw == nil {
		return engine
	}
	if v, ok := intFieldOK(raw, "hibernateTtlMs"); ok && v > 0 {
		engine.HibernateTTL = time.Duration(v) * time.Millisecond
	}
	if v, ok := intFieldOK(raw, "maxRssKb"); ok && v > 0 {
		engine.MaxRSSKB = v
	}
	if v, ok := intFieldOK(raw, "maxAgeMs"); ok && v > 0 {
		engine.MaxAge = time.Duration(v) * time.Millisecond
	}
	if v, ok := intFieldOK(raw, "spareSessions"); ok {
		if v < 0 {
			v = 0
		}
		if v > 4 {
			v = 4
		}
		engine.SpareSessions = v
	}
	if v, ok := intFieldOK(raw, "rssSampleIntervalMs"); ok && v > 0 {
		engine.RSSSampleInterval = time.Duration(v) * time.Millisecond
	}
	if v, ok := boolFieldOK(raw, "compactionEnabled"); ok {
		engine.CompactionEnabled = v
	}
	if v, ok := intFieldOK(raw, "compactionThresholdPct"); ok && v > 0 {
		if v > 100 {
			v = 100
		}
		engine.CompactionThresholdPct = v
	}
	if v, ok := intFieldOK(raw, "compactionKeepLastTurns"); ok && v > 0 {
		engine.CompactionKeepLastTurns = v
	}
	return engine
}

func defaultAppConfig() map[string]any {
	return map[string]any{
		"version": 1,
		"chat": map[string]any{
			"defaultModel":      "",
			"defaultMode":       "",
			"spareSessions":     2,
			"autoCompact":       true,
			"autoCompactPct":    0.85,
			"autoResumeOnCrash": true,
		},
		"acp": map[string]any{
			"provider":       "mock",
			"command":        "node",
			"args":           []any{filepath.Join("desktop", "acp", "mock-server.mjs")},
			"env":            map[string]any{},
			"probeTimeoutMs": 5000,
		},
		"ui": map[string]any{
			"startMode":   "agent",
			"sidebarOpen": true,
			"density":     "compact",
			"theme":       "command-deck",
		},
		"groups": []any{},
		"lan": map[string]any{
			"enabled": false,
			"bind":    "localhost",
			"port":    8787,
			"access":  map[string]any{"allow": []any{}, "deny": []any{}},
		},
		"agentApi":     map[string]any{"enabled": true},
		"terminal":     map[string]any{"shell": "pwsh"},
		"integrations": map[string]any{"teams": map[string]any{"graphToken": ""}},
	}
}

func deepMergeMap(base, patch map[string]any) map[string]any {
	out := cloneMap(base)
	for k, v := range patch {
		if pm, ok := v.(map[string]any); ok {
			if bm, ok := out[k].(map[string]any); ok {
				out[k] = deepMergeMap(bm, pm)
				continue
			}
		}
		out[k] = cloneAny(v)
	}
	return out
}

func cloneMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = cloneAny(v)
	}
	return out
}

func cloneAny(v any) any {
	switch x := v.(type) {
	case map[string]any:
		return cloneMap(x)
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = cloneAny(item)
		}
		return out
	default:
		return x
	}
}

type appSettingsStore struct {
	mu   sync.Mutex
	path string
}

func newAppSettingsStore(path string) *appSettingsStore {
	return &appSettingsStore{path: path}
}

func (s *appSettingsStore) get() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return normalizeSettings(s.readRawLocked())
}

func (s *appSettingsStore) set(input map[string]any) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := normalizeSettings(input)
	if s.path == "" {
		return next, nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return nil, err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return nil, err
	}
	return next, nil
}

func (s *appSettingsStore) readRawLocked() map[string]any {
	if s.path == "" {
		return map[string]any{}
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return map[string]any{}
	}
	var out map[string]any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil || out == nil {
		return map[string]any{}
	}
	return out
}

func normalizeSettings(input map[string]any) map[string]any {
	base := defaultSettings()
	next := map[string]any{
		"version":            1,
		"models":             cloneAny(base["models"]),
		"permissionModes":    cloneAny(base["permissionModes"]),
		"chatMode":           "",
		"autoProcessEnabled": base["autoProcessEnabled"],
		"notifications":      base["notifications"],
		"prApprover":         "",
		"modelScores":        acp.ModelScores{},
		"modelFavorites":     []any{},
	}
	if input == nil {
		return next
	}
	models := mapFromAnyMain(input["models"])
	nextModels := next["models"].(map[string]any)
	for _, kind := range taskKinds() {
		if v, ok := models[kind].(string); ok {
			nextModels[kind] = strings.TrimSpace(v)
		}
	}
	permissionModes := mapFromAnyMain(input["permissionModes"])
	nextPerms := next["permissionModes"].(map[string]any)
	for _, kind := range taskKinds() {
		if kind == "chat" {
			continue
		}
		nextPerms[kind] = normalizePermissionMode(permissionModes[kind])
	}
	if v, ok := input["chatMode"].(string); ok {
		next["chatMode"] = strings.TrimSpace(v)
	}
	if v, ok := input["autoProcessEnabled"].(bool); ok {
		next["autoProcessEnabled"] = v
	}
	if v, ok := input["prApprover"].(string); ok {
		next["prApprover"] = strings.TrimSpace(v)
	}
	if v := normalizeNotifications(input["notifications"]); v != "" {
		next["notifications"] = v
	}
	next["modelScores"] = acp.NormalizeModelScores(input["modelScores"])
	next["modelFavorites"] = normalizeModelFavorites(input["modelFavorites"])
	return next
}

func normalizeModelFavorites(value any) []any {
	items, _ := value.([]any)
	out := make([]any, 0, min(len(items), 100))
	seen := make(map[string]bool, len(items))
	for _, raw := range items {
		favorite := mapFromAnyMain(raw)
		providerID := strings.TrimSpace(toString(favorite["providerId"]))
		modelID := strings.TrimSpace(toString(favorite["modelId"]))
		if providerID == "" || modelID == "" {
			continue
		}
		key := providerID + "\x00" + modelID
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, map[string]any{"providerId": providerID, "modelId": modelID})
		if len(out) == 100 {
			break
		}
	}
	return out
}

func normalizePermissionMode(v any) string {
	s := strings.TrimSpace(toString(v))
	if s == "auto" || s == "dangerous" {
		return s
	}
	return ""
}

func normalizeNotifications(v any) string {
	s := strings.TrimSpace(toString(v))
	if s == "all" || s == "none" {
		return s
	}
	return ""
}

func mapFromAnyMain(v any) map[string]any {
	m, _ := v.(map[string]any)
	if m == nil {
		return map[string]any{}
	}
	return m
}

func intFieldOK(m map[string]any, key string) (int, bool) {
	if m == nil || m[key] == nil {
		return 0, false
	}
	switch v := m[key].(type) {
	case json.Number:
		n, err := v.Int64()
		return int(n), err == nil
	case float64:
		return int(v), true
	case int:
		return v, true
	default:
		n, err := strconv.Atoi(strings.TrimSpace(toString(v)))
		return n, err == nil
	}
}

func boolFieldOK(m map[string]any, key string) (bool, bool) {
	if m == nil || m[key] == nil {
		return false, false
	}
	switch v := m[key].(type) {
	case bool:
		return v, true
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "true", "1", "yes", "on":
			return true, true
		case "false", "0", "no", "off":
			return false, true
		}
	}
	return false, false
}

func toString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return strings.TrimSpace(strings.Trim(toJSONMain(v), `"`))
}

func toJSONMain(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(data)
}
