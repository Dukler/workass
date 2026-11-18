package acp

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The subagent registry lives in memory, so a daemon restart (every update)
// and the completed-history prune both drop what workass_retry_subagent needs:
// the task prompt and its selection. The chat actor still lists such a run, so
// its owner could see it and not retry it (bug-20261118-afbfeed6). This store
// keeps exactly the spawn intent a retry replays, one file per chat, written
// when the run starts: a run the restart kills mid-flight never reaches any
// terminal write.
const (
	subagentRetrySpecDir          = "subagent-specs"
	maxSubagentRetrySpecsPerChat  = 256
	maxSubagentRetrySpecFileBytes = 8 * 1024 * 1024
	maxSubagentRetrySpecPrompt    = 1024 * 1024
)

type subagentRetrySpec struct {
	ID           string `json:"id"`
	ParentChatID string `json:"parentChatId"`
	ParentTabID  string `json:"parentTabId"`
	Prompt       string `json:"prompt"`
	Label        string `json:"label,omitempty"`
	ProviderID   string `json:"providerId,omitempty"`
	ModelID      string `json:"modelId,omitempty"`
	Effort       string `json:"effort,omitempty"`
	ModeID       string `json:"modeId,omitempty"`
	CWD          string `json:"cwd,omitempty"`
	Forked       bool   `json:"forked,omitempty"`
	CreatedAt    string `json:"createdAt"`
}

func (m *Manager) subagentRetrySpecPath(chatID, tabID string) string {
	stateDir := strings.TrimSpace(m.opts.StateDir)
	key := firstNonEmpty(strings.TrimSpace(tabID), strings.TrimSpace(chatID))
	if stateDir == "" || key == "" {
		return ""
	}
	return filepath.Join(stateDir, subagentRetrySpecDir, safeArchiveName(key)+".json")
}

func readSubagentRetrySpecs(path string) []subagentRetrySpec {
	info, err := os.Stat(path)
	if err != nil || info.Size() > maxSubagentRetrySpecFileBytes {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var specs []subagentRetrySpec
	if json.Unmarshal(data, &specs) != nil {
		return nil
	}
	return specs
}

// persistSubagentRetrySpec records a run's spawn intent. It is best effort: a
// failed write costs only the retry after a restart, never the spawn itself.
func (m *Manager) persistSubagentRetrySpec(spec subagentRetrySpec) error {
	spec.ParentChatID, spec.ParentTabID = strings.TrimSpace(spec.ParentChatID), strings.TrimSpace(spec.ParentTabID)
	path := m.subagentRetrySpecPath(spec.ParentChatID, spec.ParentTabID)
	if strings.TrimSpace(m.opts.StateDir) == "" {
		return nil // nothing is durable without a state directory
	}
	if path == "" || spec.ID == "" || spec.ParentChatID == "" || spec.ParentTabID == "" {
		return errors.New("subagent retry spec has no owning chat")
	}
	if len(spec.Prompt) > maxSubagentRetrySpecPrompt {
		return errors.New("subagent prompt is too large to keep for a retry")
	}
	if spec.CreatedAt == "" {
		spec.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	m.subagentSpecMu.Lock()
	defer m.subagentSpecMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	specs := []subagentRetrySpec{}
	for _, prior := range readSubagentRetrySpecs(path) {
		if prior.ID != spec.ID {
			specs = append(specs, prior)
		}
	}
	specs = append(specs, spec)
	sort.SliceStable(specs, func(i, j int) bool { return specs[i].CreatedAt < specs[j].CreatedAt })
	if len(specs) > maxSubagentRetrySpecsPerChat {
		specs = specs[len(specs)-maxSubagentRetrySpecsPerChat:]
	}
	payload, err := json.Marshal(specs)
	for err == nil && len(payload) > maxSubagentRetrySpecFileBytes && len(specs) > 1 {
		specs = specs[1:]
		payload, err = json.Marshal(specs)
	}
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, payload, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// loadSubagentRetrySpec answers only the exact chat that spawned the run.
func (m *Manager) loadSubagentRetrySpec(chatID, tabID, id string) (subagentRetrySpec, bool) {
	chatID, tabID, id = strings.TrimSpace(chatID), strings.TrimSpace(tabID), strings.TrimSpace(id)
	path := m.subagentRetrySpecPath(chatID, tabID)
	if path == "" || id == "" {
		return subagentRetrySpec{}, false
	}
	m.subagentSpecMu.Lock()
	specs := readSubagentRetrySpecs(path)
	m.subagentSpecMu.Unlock()
	for _, spec := range specs {
		if spec.ID == id && spec.ParentChatID == chatID && spec.ParentTabID == tabID && strings.TrimSpace(spec.Prompt) != "" {
			return spec, true
		}
	}
	return subagentRetrySpec{}, false
}

// StoredSubagentPermissionMode is SubagentPermissionMode for a run this process
// never held: what a retry from its stored spec would run at.
func (m *Manager) StoredSubagentPermissionMode(chatID, tabID, id string) (string, string, bool) {
	if m == nil {
		return "", "", false
	}
	spec, ok := m.loadSubagentRetrySpec(chatID, tabID, id)
	if !ok {
		return "", "", false
	}
	return spec.ProviderID, strings.TrimSpace(spec.ModeID), true
}

// forgetSubagentRetrySpecs drops a deleted chat's stored prompts. The file is
// keyed by tab, so another chat's specs under the same key stay.
func (m *Manager) forgetSubagentRetrySpecs(chatID, tabID string) {
	chatID = strings.TrimSpace(chatID)
	path := m.subagentRetrySpecPath(chatID, tabID)
	if path == "" {
		return
	}
	m.subagentSpecMu.Lock()
	defer m.subagentSpecMu.Unlock()
	kept := []subagentRetrySpec{}
	for _, spec := range readSubagentRetrySpecs(path) {
		if spec.ParentChatID != chatID {
			kept = append(kept, spec)
		}
	}
	if len(kept) == 0 {
		_ = os.Remove(path)
		return
	}
	if payload, err := json.Marshal(kept); err == nil {
		tmp := path + ".tmp"
		if os.WriteFile(tmp, payload, 0o600) == nil && os.Rename(tmp, path) != nil {
			_ = os.Remove(tmp)
		}
	}
}
