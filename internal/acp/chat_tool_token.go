package acp

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"workass/internal/durablefs"
	"workass/internal/toolcli"
)

// A chat tool token is the credential a chat's tool context carries. The
// per-session owner capability dies with its engine, and Workass stops idle
// engines on its own (idle sleep, age recycling) — something Claude Code never
// does to a live session. Every helper a chat had launched then lost its tools
// at once and could not even tell why (lagspace "cleanup", 2026-10-16: a waker
// failed four hours later). The chat token lives as long as the chat: engines
// come and go underneath it, and deleting the chat revokes it.
//
// It authenticates exactly like the session owner (ownerBinding). Tools that
// need a running turn still require one, so a detached helper can send,
// schedule and read, but not ask questions or act inside a turn.
type chatToolTokenRecord struct {
	Token    string `json:"token"`
	ChatID   string `json:"chatId"`
	TabID    string `json:"tabId"`
	IssuedAt string `json:"issuedAt"`
}

func chatToolTokenKey(chatID, tabID string) string {
	return strings.TrimSpace(tabID) + "\x00" + strings.TrimSpace(chatID)
}

func (m *Manager) chatToolTokensPath() string {
	if strings.TrimSpace(m.opts.StateDir) == "" {
		return ""
	}
	return filepath.Join(m.opts.StateDir, "tool-contexts", "chat-tokens.json")
}

func (m *Manager) toolBootstrapRoot() string {
	if strings.TrimSpace(m.opts.StateDir) == "" {
		return ""
	}
	return filepath.Join(m.opts.StateDir, "tool-bootstrap")
}

// loadChatToolTokens restores the chat tokens so helpers keep working across a
// daemon restart. Unreadable or malformed state yields none: fail closed.
func (m *Manager) loadChatToolTokens() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.chatToolTokens = map[string]agentOwnerBinding{}
	m.chatToolTokenByChat = map[string]string{}
	path := m.chatToolTokensPath()
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var records []chatToolTokenRecord
	if json.Unmarshal(data, &records) != nil {
		return
	}
	for _, record := range records {
		token, chatID, tabID := strings.TrimSpace(record.Token), strings.TrimSpace(record.ChatID), strings.TrimSpace(record.TabID)
		if token == "" || chatID == "" || tabID == "" {
			continue
		}
		m.chatToolTokens[token] = agentOwnerBinding{ChatID: chatID, TabID: tabID}
		m.chatToolTokenByChat[chatToolTokenKey(chatID, tabID)] = token
	}
}

func (m *Manager) persistChatToolTokensLocked() {
	path := m.chatToolTokensPath()
	if path == "" {
		return
	}
	dir := filepath.Dir(path)
	records := make([]chatToolTokenRecord, 0, len(m.chatToolTokens))
	for token, binding := range m.chatToolTokens {
		records = append(records, chatToolTokenRecord{Token: token, ChatID: binding.ChatID, TabID: binding.TabID})
	}
	data, err := json.Marshal(records)
	if err != nil {
		return
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return
	}
	// Same durable replace pattern as provider_lane_store: no helper may see a
	// half-written credential file after a lost power cut.
	temp, err := os.CreateTemp(dir, ".chat-tokens-*.tmp")
	if err != nil {
		return
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return
	}
	if err := temp.Close(); err != nil {
		return
	}
	if os.Rename(tempName, path) != nil {
		return
	}
	_ = durablefs.SyncDirectory(dir)
}

// chatToolTokenLocked returns the chat's token, minting it on first use.
func (m *Manager) chatToolTokenLocked(chatID, tabID string) string {
	chatID, tabID = strings.TrimSpace(chatID), strings.TrimSpace(tabID)
	if chatID == "" || tabID == "" {
		return ""
	}
	if m.chatToolTokens == nil {
		m.chatToolTokens = map[string]agentOwnerBinding{}
		m.chatToolTokenByChat = map[string]string{}
	}
	key := chatToolTokenKey(chatID, tabID)
	if token := m.chatToolTokenByChat[key]; token != "" {
		return token
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return ""
	}
	token := "wct_" + hex.EncodeToString(raw)
	m.chatToolTokens[token] = agentOwnerBinding{ChatID: chatID, TabID: tabID}
	m.chatToolTokenByChat[key] = token
	m.persistChatToolTokensLocked()
	return token
}

// chatToolTokenForChat reads a chat's token without minting one. ForgetChat
// needs the value before it revokes the token, to find the bootstrap
// directories it should remove afterwards.
func (m *Manager) chatToolTokenForChat(chatID, tabID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.chatToolTokenByChat[chatToolTokenKey(chatID, tabID)]
}

// revokeChatToolToken ends a deleted chat's token; every context file that
// carried it stops working at once.
func (m *Manager) revokeChatToolToken(chatID, tabID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := chatToolTokenKey(chatID, tabID)
	token := m.chatToolTokenByChat[key]
	if token == "" {
		return
	}
	delete(m.chatToolTokenByChat, key)
	delete(m.chatToolTokens, token)
	m.persistChatToolTokensLocked()
}

// pruneToolBootstrap drops every tool-context directory that no longer names a
// live chat token. Engine owner keys never survive a daemon restart, so a
// context file still carrying one (a stopped engine, a crash, "{}" prepared
// but never bound) is dead weight no helper can authenticate with. A fresh
// manager runs this once on its restored token set, before any loop starts.
func (m *Manager) pruneToolBootstrap() {
	root := m.toolBootstrapRoot()
	if root == "" {
		return
	}
	m.mu.Lock()
	tokens := make(map[string]struct{}, len(m.chatToolTokens))
	for token := range m.chatToolTokens {
		tokens[token] = struct{}{}
	}
	m.mu.Unlock()
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		if chatToolBootstrapDirIsLive(filepath.Join(dir, "context.json"), tokens) {
			continue
		}
		_ = os.RemoveAll(dir)
	}
}

// chatToolBootstrapDirIsLive reports whether a prepared context directory
// still carries a credential the manager would accept. Missing context,
// unreadable bytes and unknown credentials all fail closed into removal.
func chatToolBootstrapDirIsLive(path string, tokens map[string]struct{}) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var config toolcli.Config
	if json.Unmarshal(data, &config) != nil {
		return false
	}
	_, known := tokens[config.Credential]
	return known
}

// forgetChatToolBootstrapDirs removes tool-context directories whose context
// named the deleted chat's token. Bridge state is snapshotted before any file
// is read or removed, so the manager mutex never spans this file I/O, and a
// directory still attached to a live bridge is spared: its engine may not have
// closed yet and its helpers still hold that path.
func (m *Manager) forgetChatToolBootstrapDirs(token string) {
	if token == "" {
		return
	}
	root := m.toolBootstrapRoot()
	if root == "" {
		return
	}
	live := m.liveBridgeToolBootstrapDirs()
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		if _, ok := live[filepath.Clean(dir)]; ok {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, "context.json"))
		if err != nil {
			continue
		}
		var config toolcli.Config
		if json.Unmarshal(data, &config) != nil || config.Credential != token {
			continue
		}
		_ = os.RemoveAll(dir)
	}
}

func (m *Manager) liveBridgeToolBootstrapDirs() map[string]struct{} {
	m.mu.Lock()
	bridges := make([]*Bridge, 0, len(m.bridges))
	for _, bridge := range m.bridges {
		bridges = append(bridges, bridge)
	}
	m.mu.Unlock()
	live := map[string]struct{}{}
	for _, bridge := range bridges {
		if bridge == nil || bridge.Closed() {
			continue
		}
		bridge.mu.Lock()
		dir := bridge.nativeInstructionsDir
		bridge.mu.Unlock()
		if dir != "" {
			live[filepath.Clean(dir)] = struct{}{}
		}
	}
	return live
}
