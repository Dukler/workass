package acp

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"workass/internal/durablefs"
	"workass/internal/toolcli"
)

// A job-start credential: what a chat hands its own timers.
//
// A timer (launchd, systemd, cron) has no chat turn, so it holds no Workass
// credential, and `workass run` refuses a run with no chat to tell. The only
// workaround was a saved copy of a chat's own context, which authenticates as
// that chat for EVERY tool and dies with the session that issued it, after which
// every timer-started job refused to start (former issue #4, 2026-11-10).
//
// This credential is the narrow, durable answer:
//   - it authorizes exactly one method, process.register, so a timer can start
//     and report supervised jobs and nothing else: no message, no chat read, no
//     agent, no job on another machine;
//   - every job it reports is owned by the chat that issued it, and only by
//     that chat: the owner is fixed when it is issued, not chosen per job;
//   - it names no chat, so no chat-authorized path can accept it;
//   - it lives until it is revoked or its chat is deleted, across engine
//     recycles, daemon restarts and updates;
//   - `workass run` never deletes its file when it reads it (only a one-shot
//     wrt_ run credential is spent that way).

const jobStartTokenPrefix = toolcli.JobStartCredentialPrefix

// maxJobStartTokensPerChat bounds what one chat may hold at once.
const maxJobStartTokensPerChat = 16

// JobStartBinding is what one job-start credential authorizes.
type JobStartBinding struct {
	// ID is the public name of the credential; the token itself is never
	// shown again after it is issued.
	ID string `json:"id"`
	// Owner is the global id of the chat its jobs belong to; empty on a
	// machine with no fleet id, where the chat itself is the owner.
	Owner       string `json:"owner,omitempty"`
	TabID       string `json:"tabId"`
	ChatID      string `json:"chatId"`
	Label       string `json:"label"`
	OperationID string `json:"operationId"`
	File        string `json:"file"`
	IssuedAt    string `json:"issuedAt"`
	LastUsedAt  string `json:"lastUsedAt,omitempty"`
}

type jobStartTokenRecord struct {
	Token string `json:"token"`
	JobStartBinding
}

func (m *Manager) jobStartTokensPath() string {
	if strings.TrimSpace(m.opts.StateDir) == "" {
		return ""
	}
	return filepath.Join(m.opts.StateDir, "tool-contexts", "job-start-tokens.json")
}

// JobStartCredentialDir is where the credential files live, one directory per
// credential so each is its own 0700 boundary.
func (m *Manager) JobStartCredentialDir() string {
	if strings.TrimSpace(m.opts.StateDir) == "" {
		return ""
	}
	return filepath.Join(m.opts.StateDir, "job-credentials")
}

// loadJobStartTokens restores them at boot. Unreadable state yields none: fail
// closed, and the chat issues a new one.
func (m *Manager) loadJobStartTokens() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.jobStartTokens = map[string]JobStartBinding{}
	path := m.jobStartTokensPath()
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var records []jobStartTokenRecord
	if json.Unmarshal(data, &records) != nil {
		return
	}
	for _, record := range records {
		token := strings.TrimSpace(record.Token)
		if !strings.HasPrefix(token, jobStartTokenPrefix) || record.ID == "" || record.TabID == "" || record.ChatID == "" {
			continue
		}
		m.jobStartTokens[token] = record.JobStartBinding
	}
	// A timer holds its file for months, across restarts that may move this
	// daemon's endpoint or certificate. Each file still on disk is brought up to
	// date, so the timer's next job reaches the daemon that is running now.
	template, err := m.ToolContextTemplate()
	if err != nil {
		return
	}
	for token, binding := range m.jobStartTokens {
		current, readErr := toolcli.ReadConfig(binding.File)
		if readErr != nil {
			continue
		}
		if current.Endpoint != template.Endpoint || current.CAFile != template.CAFile || current.StateDir != m.opts.StateDir || current.Owner != binding.Owner {
			_ = writeJobStartFile(binding.File, template, token, binding.Owner, m.opts.StateDir)
		}
	}
}

func (m *Manager) persistJobStartTokensLocked() error {
	path := m.jobStartTokensPath()
	if path == "" {
		return errors.New("this daemon has no state directory to keep a credential in")
	}
	records := make([]jobStartTokenRecord, 0, len(m.jobStartTokens))
	for token, binding := range m.jobStartTokens {
		records = append(records, jobStartTokenRecord{Token: token, JobStartBinding: binding})
	}
	sort.Slice(records, func(i, j int) bool { return records[i].ID < records[j].ID })
	data, err := json.Marshal(records)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, ".job-start-tokens-*.tmp")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempName, path); err != nil {
		return err
	}
	_ = durablefs.SyncDirectory(dir)
	return nil
}

// IssueJobStartToken issues a credential for the chat tabID/chatID, whose global
// id is owner, and writes its file. The same operation id answers with the
// credential it already issued, so a retried call never strands a second one.
func (m *Manager) IssueJobStartToken(tabID, chatID, owner, label, operationID string) (JobStartBinding, bool, error) {
	tabID, chatID, owner = strings.TrimSpace(tabID), strings.TrimSpace(chatID), strings.TrimSpace(owner)
	label, operationID = strings.TrimSpace(label), strings.TrimSpace(operationID)
	if m == nil || tabID == "" || chatID == "" {
		return JobStartBinding{}, false, errors.New("a job-start credential is issued by a chat, for that chat")
	}
	if label == "" {
		return JobStartBinding{}, false, errors.New("a job-start credential needs a label naming the timer that will hold it")
	}
	if operationID == "" {
		return JobStartBinding{}, false, errors.New("operation_id is required; one id is one credential")
	}
	template, err := m.ToolContextTemplate()
	if err != nil {
		return JobStartBinding{}, false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.jobStartTokens == nil {
		m.jobStartTokens = map[string]JobStartBinding{}
	}
	held := 0
	for _, binding := range m.jobStartTokens {
		if binding.TabID != tabID || binding.ChatID != chatID {
			continue
		}
		if binding.OperationID == operationID {
			if _, statErr := os.Stat(binding.File); statErr == nil {
				return binding, true, nil
			}
			// Its file is gone (deleted by hand), so the answer to the retry is
			// the same credential written again.
			for token, existing := range m.jobStartTokens {
				if existing.ID == binding.ID {
					if err := writeJobStartFile(binding.File, template, token, binding.Owner, m.opts.StateDir); err != nil {
						return JobStartBinding{}, false, err
					}
					return binding, true, nil
				}
			}
		}
		held++
	}
	if held >= maxJobStartTokensPerChat {
		return JobStartBinding{}, false, errors.New("this chat already holds 16 job-start credentials; revoke one with workass_revoke_job_credential first")
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return JobStartBinding{}, false, err
	}
	token := jobStartTokenPrefix + hex.EncodeToString(raw)
	idRaw := make([]byte, 6)
	if _, err := rand.Read(idRaw); err != nil {
		return JobStartBinding{}, false, err
	}
	id := "jsc-" + hex.EncodeToString(idRaw)
	file := filepath.Join(m.JobStartCredentialDir(), id, "context.json")
	binding := JobStartBinding{
		ID: id, Owner: owner, TabID: tabID, ChatID: chatID, Label: SanitizeDisplayText(label, 120),
		OperationID: operationID, File: file, IssuedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := writeJobStartFile(file, template, token, owner, m.opts.StateDir); err != nil {
		return JobStartBinding{}, false, err
	}
	m.jobStartTokens[token] = binding
	if err := m.persistJobStartTokensLocked(); err != nil {
		delete(m.jobStartTokens, token)
		_ = os.RemoveAll(filepath.Dir(file))
		return JobStartBinding{}, false, err
	}
	return binding, false, nil
}

func writeJobStartFile(path string, template toolcli.Config, token, owner, stateDir string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	template.Credential = token
	template.ChatID, template.TabID = "", ""
	template.StateDir = stateDir
	template.Owner = owner
	return toolcli.WriteConfig(path, template)
}

// JobStartTokenFor reports what a credential authorizes, if it is a job-start
// credential at all.
func (m *Manager) JobStartTokenFor(token string) (JobStartBinding, bool) {
	token = strings.TrimSpace(token)
	if m == nil || !strings.HasPrefix(token, jobStartTokenPrefix) {
		return JobStartBinding{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	binding, ok := m.jobStartTokens[token]
	return binding, ok
}

// TouchJobStartToken records a use, coalesced to the hour.
func (m *Manager) TouchJobStartToken(token string) {
	token = strings.TrimSpace(token)
	if m == nil || token == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	binding, ok := m.jobStartTokens[token]
	if !ok {
		return
	}
	now := time.Now().UTC()
	if last, err := time.Parse(time.RFC3339, binding.LastUsedAt); err == nil && now.Sub(last) < time.Hour {
		return
	}
	binding.LastUsedAt = now.Format(time.RFC3339)
	m.jobStartTokens[token] = binding
	_ = m.persistJobStartTokensLocked()
}

// ListJobStartTokens is every credential the chat holds, never the tokens.
func (m *Manager) ListJobStartTokens(tabID, chatID string) []JobStartBinding {
	tabID, chatID = strings.TrimSpace(tabID), strings.TrimSpace(chatID)
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]JobStartBinding, 0)
	for _, binding := range m.jobStartTokens {
		if binding.TabID == tabID && binding.ChatID == chatID {
			out = append(out, binding)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].IssuedAt < out[j].IssuedAt || (out[i].IssuedAt == out[j].IssuedAt && out[i].ID < out[j].ID)
	})
	return out
}

// RevokeJobStartToken ends one of the chat's credentials and deletes its file.
// Revoking one that is already gone is answered as done.
func (m *Manager) RevokeJobStartToken(tabID, chatID, id string) (bool, error) {
	tabID, chatID, id = strings.TrimSpace(tabID), strings.TrimSpace(chatID), strings.TrimSpace(id)
	if id == "" {
		return false, errors.New("credential_id is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for token, binding := range m.jobStartTokens {
		if binding.ID != id {
			continue
		}
		if binding.TabID != tabID || binding.ChatID != chatID {
			return false, errors.New("that job-start credential belongs to another chat")
		}
		delete(m.jobStartTokens, token)
		if err := m.persistJobStartTokensLocked(); err != nil {
			m.jobStartTokens[token] = binding
			return false, err
		}
		m.removeJobStartFile(binding)
		return true, nil
	}
	return false, nil
}

// RevokeJobStartTokensForChat ends every credential of a deleted chat: its jobs
// would have nowhere to report.
func (m *Manager) RevokeJobStartTokensForChat(tabID, chatID string) {
	tabID, chatID = strings.TrimSpace(tabID), strings.TrimSpace(chatID)
	if m == nil || tabID == "" || chatID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	removed := make([]JobStartBinding, 0)
	for token, binding := range m.jobStartTokens {
		if binding.TabID == tabID && binding.ChatID == chatID {
			delete(m.jobStartTokens, token)
			removed = append(removed, binding)
		}
	}
	if len(removed) == 0 {
		return
	}
	_ = m.persistJobStartTokensLocked()
	for _, binding := range removed {
		m.removeJobStartFile(binding)
	}
}

// removeJobStartFile deletes a credential's own directory, and only one that
// sits where this daemon writes them.
func (m *Manager) removeJobStartFile(binding JobStartBinding) {
	root := m.JobStartCredentialDir()
	dir := filepath.Dir(binding.File)
	if root == "" || filepath.Dir(dir) != root || filepath.Base(dir) != binding.ID {
		return
	}
	_ = os.RemoveAll(dir)
}

// JobStartChatOwner resolves a job-start credential to the chat it reports for:
// that chat's own durable tool credential, and its tab and chat ids.
func (m *Manager) JobStartChatOwner(token string) (ownerKey, tabID, chatID string, ok bool) {
	token = strings.TrimSpace(token)
	if m == nil || !strings.HasPrefix(token, jobStartTokenPrefix) {
		return "", "", "", false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	binding, found := m.jobStartTokens[token]
	if !found {
		return "", "", "", false
	}
	key := m.chatToolTokenLocked(binding.ChatID, binding.TabID)
	if key == "" {
		return "", "", "", false
	}
	return key, binding.TabID, binding.ChatID, true
}
