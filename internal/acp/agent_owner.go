package acp

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// AgentOwnerCWD resolves the exact working directory owned by an injected
// workass-agent capability. It is intentionally narrower than chat-control
// targeting: artifact hosting may publish only from the caller's own workspace.
func (m *Manager) AgentOwnerCWD(ownerKey, chatID, tabID string) (string, error) {
	if m == nil || !m.ValidateAgentOwner(ownerKey, chatID, tabID) {
		return "", errors.New("no Workass agent session owns this artifact hosting request")
	}
	ownerKey = strings.TrimSpace(ownerKey)
	chatID = strings.TrimSpace(chatID)
	tabID = strings.TrimSpace(tabID)

	m.mu.Lock()
	cwd := ""
	if job := m.runningJobForIdentityLocked(chatID, tabID); job != nil {
		cwd = strings.TrimSpace(job.CWD)
	}
	type ownedSession struct {
		id     string
		bridge *Bridge
	}
	var sessions []ownedSession
	if cwd == "" {
		for sessionID, candidateOwner := range m.agentOwnerBySession {
			if candidateOwner == ownerKey && m.sessionBridge[sessionID] != nil {
				sessions = append(sessions, ownedSession{id: sessionID, bridge: m.sessionBridge[sessionID]})
			}
		}
	}
	rootDir := strings.TrimSpace(m.opts.RootDir)
	m.mu.Unlock()

	for _, session := range sessions {
		live, ok := session.bridge.liveSession(session.id)
		if ok && live.ChatID == chatID && live.TabID == tabID {
			cwd = strings.TrimSpace(live.Info.CWD)
			if cwd != "" {
				break
			}
		}
	}
	if cwd == "" {
		cwd = rootDir
	}
	if !filepath.IsAbs(cwd) && rootDir != "" {
		cwd = filepath.Join(rootDir, cwd)
	}
	cwd, err := filepath.Abs(filepath.Clean(cwd))
	if err != nil {
		return "", errors.New("the Workass agent working directory is invalid")
	}
	info, err := os.Stat(cwd)
	if err != nil || !info.IsDir() {
		return "", errors.New("the Workass agent has no readable working directory")
	}
	return cwd, nil
}
