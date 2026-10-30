package acp

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"workass/internal/durablefs"
)

// A credential for exactly one supervised job, and nothing else.
//
// docs/specs/fleet-runtime.md §9 keeps daemon-side spawning a non-goal because
// a child must never inherit the daemon's environment or its secrets. That rule
// left a real hole, which the first live P1–P4 test found: an agent on machine P
// could own a job on machine H (`workass run --owner` already accepts a global
// chat id) but had no way to START one there, because registration is performed
// by the supervisor on H and there was nobody on H to run it.
//
// `process.start` closes it without weakening the rule: H runs its OWN
// `workass run` with an environment built from nothing, and the only credential
// that job ever holds is this one — a token bound to one chat on H, one
// operation id, and the single method `process.register`. It is consumed by the
// first successful registration and it cannot be used for anything else, so a
// job that reads its own token file gains nothing a job could not already do.
//
// The chat-tool token (chat_tool_token.go) is deliberately NOT reused here: it
// authenticates as its chat for every tool, lives as long as the chat, and
// handing one to arbitrary argv would be a far wider grant than the job needs.

// runTokenPrefix marks a one-shot supervised-job credential.
const runTokenPrefix = "wrt_"

// runTokenTTL bounds a credential that is IDLE. A job whose supervisor never
// started must not leave a usable token behind forever — but a job that is
// still reporting must not lose its credential at a birthday either, which is
// what an age-based ceiling would do to anything that runs for more than a day.
// So the clock is "since last used", and a report is a use.
const runTokenTTL = 24 * time.Hour

type runTokenRecord struct {
	Token       string `json:"token"`
	ChatID      string `json:"chatId"`
	TabID       string `json:"tabId"`
	OperationID string `json:"operationId"`
	IssuedAt    string `json:"issuedAt"`
	LastUsedAt  string `json:"lastUsedAt,omitempty"`
}

// RunTokenBinding is what one run credential authorizes.
type RunTokenBinding struct {
	ChatID      string
	TabID       string
	OperationID string
	IssuedAt    time.Time
	LastUsedAt  time.Time
}

// idleSince is the last moment this credential was either issued or used.
func (b RunTokenBinding) idleSince() time.Time {
	if b.LastUsedAt.After(b.IssuedAt) {
		return b.LastUsedAt
	}
	return b.IssuedAt
}

func (m *Manager) runTokensPath() string {
	if strings.TrimSpace(m.opts.StateDir) == "" {
		return ""
	}
	return filepath.Join(m.opts.StateDir, "tool-contexts", "run-tokens.json")
}

// loadRunTokens restores the one-shot credentials so a job whose supervisor
// starts after a daemon restart can still register. Unreadable or malformed
// state yields none: fail closed.
func (m *Manager) loadRunTokens() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.runTokens = map[string]RunTokenBinding{}
	path := m.runTokensPath()
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var records []runTokenRecord
	if json.Unmarshal(data, &records) != nil {
		return
	}
	now := time.Now().UTC()
	for _, record := range records {
		token := strings.TrimSpace(record.Token)
		if token == "" || strings.TrimSpace(record.ChatID) == "" || strings.TrimSpace(record.OperationID) == "" {
			continue
		}
		binding := RunTokenBinding{
			ChatID: strings.TrimSpace(record.ChatID), TabID: strings.TrimSpace(record.TabID),
			OperationID: strings.TrimSpace(record.OperationID),
		}
		if issued, err := time.Parse(time.RFC3339, record.IssuedAt); err == nil {
			binding.IssuedAt = issued.UTC()
		}
		if used, err := time.Parse(time.RFC3339, record.LastUsedAt); err == nil {
			binding.LastUsedAt = used.UTC()
		}
		if since := binding.idleSince(); !since.IsZero() && now.Sub(since) > runTokenTTL {
			continue
		}
		m.runTokens[token] = binding
	}
}

func (m *Manager) persistRunTokensLocked() {
	path := m.runTokensPath()
	if path == "" {
		return
	}
	dir := filepath.Dir(path)
	// Each record keeps its OWN timestamps. Stamping every record with `now` on
	// every persist made the idle ceiling mean "24h since this machine last
	// minted any token", which is not a bound on anything.
	records := make([]runTokenRecord, 0, len(m.runTokens))
	for token, binding := range m.runTokens {
		record := runTokenRecord{
			Token: token, ChatID: binding.ChatID, TabID: binding.TabID,
			OperationID: binding.OperationID,
			IssuedAt:    binding.IssuedAt.UTC().Format(time.RFC3339),
		}
		if !binding.LastUsedAt.IsZero() {
			record.LastUsedAt = binding.LastUsedAt.UTC().Format(time.RFC3339)
		}
		records = append(records, record)
	}
	data, err := json.Marshal(records)
	if err != nil || os.MkdirAll(dir, 0o700) != nil {
		return
	}
	// The same durable replace the chat tokens use: no job may ever see a
	// half-written credential file.
	temp, err := os.CreateTemp(dir, ".run-tokens-*.tmp")
	if err != nil {
		return
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if temp.Chmod(0o600) != nil {
		temp.Close()
		return
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return
	}
	if temp.Sync() != nil || temp.Close() != nil || os.Rename(tempName, path) != nil {
		return
	}
	_ = durablefs.SyncDirectory(dir)
}

// MintRunToken issues the credential for one supervised job. Minting with an
// operation id that already has one returns that one, so a replayed start does
// not strand a second credential.
func (m *Manager) MintRunToken(chatID, tabID, operationID string) string {
	chatID, tabID = strings.TrimSpace(chatID), strings.TrimSpace(tabID)
	operationID = strings.TrimSpace(operationID)
	if m == nil || chatID == "" || tabID == "" || operationID == "" {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.runTokens == nil {
		m.runTokens = map[string]RunTokenBinding{}
	}
	for token, binding := range m.runTokens {
		if binding.OperationID == operationID && binding.ChatID == chatID && binding.TabID == tabID {
			return token
		}
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return ""
	}
	token := runTokenPrefix + hex.EncodeToString(raw)
	m.runTokens[token] = RunTokenBinding{
		ChatID: chatID, TabID: tabID, OperationID: operationID, IssuedAt: time.Now().UTC(),
	}
	m.persistRunTokensLocked()
	return token
}

// RunTokenFor reports what a credential authorizes, if it is one at all. A
// caller that is not holding a run token gets false, which is how the ordinary
// owner-key path stays exactly what it was.
func (m *Manager) RunTokenFor(token string) (RunTokenBinding, bool) {
	token = strings.TrimSpace(token)
	if m == nil || !strings.HasPrefix(token, runTokenPrefix) {
		return RunTokenBinding{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	binding, ok := m.runTokens[token]
	return binding, ok
}

// ConsumeRunToken ends a credential's life. It is called after the single
// registration it authorized has been accepted, so a token that leaks from a
// job's own file afterwards authorizes nothing.
func (m *Manager) ConsumeRunToken(token string) {
	token = strings.TrimSpace(token)
	if m == nil || token == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.runTokens[token]; !ok {
		return
	}
	delete(m.runTokens, token)
	m.persistRunTokensLocked()
}

// TouchRunToken records that a supervised job's credential was just used.
//
// It is what makes the idle ceiling an idle ceiling: a job reporting every
// minute keeps its credential for as long as it runs, and a credential nothing
// ever presents expires on the stated clock.
func (m *Manager) TouchRunToken(token string) {
	token = strings.TrimSpace(token)
	if m == nil || token == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	binding, ok := m.runTokens[token]
	if !ok {
		return
	}
	now := time.Now().UTC()
	if now.Sub(binding.idleSince()) < time.Hour {
		// Coalesced: the ceiling is a day, so recording a use to the minute
		// would be a disk write per report per job for no gain.
		return
	}
	binding.LastUsedAt = now
	m.runTokens[token] = binding
	m.persistRunTokensLocked()
}
