package acp

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"workass/internal/agenttext"

	"workass/internal/toolcli"
)

type sessionToolContext struct {
	config toolcli.Config
	path   string
}

// toolContextBrief does no provider registration. It creates one private file
// for a live owner and reuses it across turns. Owner identity comes from the
// admitted session, so spare adoption and resumed chats cannot reuse an old
// chat's authority. The path is not a credential; the file is never quoted.
func (m *Manager) toolContextBrief(sessionID, chatID, tabID string) (string, error) {
	if m.opts.WorkassToolsOrigin == "" {
		return "", nil
	}
	m.mu.Lock()
	owner := m.agentOwnerBySession[sessionID]
	bound, exists := m.agentOwners[owner]
	m.mu.Unlock()
	if !exists || owner == "" || bound.ChatID != chatID || bound.TabID != tabID {
		return "", errors.New("Workass tools have no owner for this session")
	}
	config := toolcli.Config{Endpoint: strings.TrimRight(m.opts.WorkassToolsOrigin, "/") + "/workass/tools", CAFile: m.opts.WorkassToolsCAFile, Credential: owner, ChatID: chatID, TabID: tabID}
	// All launched harnesses inherit a process-local context path. A resumed
	// native thread may retain older prompt text, so never embed that disposable
	// path in new prompts. Binding is refreshed before every generic ACP turn,
	// just as it is for native instruction delivery.
	if bridge := m.bridgeForSession(sessionID, SessionOptions{SessionID: sessionID}); bridge != nil {
		bridge.mu.Lock()
		hasContext := bridge.nativeInstructionsDir != ""
		bridge.mu.Unlock()
		if hasContext {
			if err := bridge.bindNativeToolContext(owner, chatID, tabID); err != nil {
				return "", err
			}
			command := `"$WORKASS_TOOLS_COMMAND" tools`
			if runtime.GOOS == "windows" {
				command = `& $env:WORKASS_TOOLS_COMMAND tools`
			}
			return fmt.Sprintf(agenttext.Get("cli.bootstrap"), command, command), nil
		}
	}
	m.toolContextMu.Lock()
	defer m.toolContextMu.Unlock()
	if m.toolContexts == nil {
		m.toolContexts = make(map[string]sessionToolContext)
	}
	entry, ok := m.toolContexts[owner]
	_, fileErr := os.Stat(entry.path)
	if !ok || entry.config != config || fileErr != nil {
		if !filepath.IsAbs(m.opts.WorkassToolsCommand) || !filepath.IsAbs(m.opts.WorkassToolsCAFile) {
			return "", errors.New("Workass tools require absolute command and certificate paths")
		}
		dir := filepath.Join(m.opts.StateDir, "tool-contexts")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", errors.New("cannot prepare Workass tool context directory")
		}
		file, err := os.CreateTemp(dir, "session-*.json")
		if err != nil {
			return "", errors.New("cannot create Workass tool context")
		}
		err = json.NewEncoder(file).Encode(config)
		closeErr := file.Close()
		if err != nil || closeErr != nil {
			_ = os.Remove(file.Name())
			return "", errors.New("cannot save Workass tool context")
		}
		if ok {
			_ = os.Remove(entry.path)
		}
		entry = sessionToolContext{config: config, path: file.Name()}
		m.toolContexts[owner] = entry
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	prefix := ""
	if runtime.GOOS == "windows" {
		prefix = "& "
		quote = func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	}
	command := prefix + quote(m.opts.WorkassToolsCommand) + " tools --context " + quote(entry.path)
	return fmt.Sprintf(agenttext.Get("cli.bootstrap"), command, command), nil
}

func (m *Manager) removeToolContexts() {
	m.toolContextMu.Lock()
	defer m.toolContextMu.Unlock()
	for _, entry := range m.toolContexts {
		_ = os.Remove(entry.path)
	}
	m.toolContexts = nil
}

func (m *Manager) removeToolContext(owner string) {
	m.toolContextMu.Lock()
	defer m.toolContextMu.Unlock()
	if entry, ok := m.toolContexts[owner]; ok {
		_ = os.Remove(entry.path)
		delete(m.toolContexts, owner)
	}
}
