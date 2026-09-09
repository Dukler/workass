package acp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"workass/internal/toolcli"
)

func TestToolContextIsPrivateStableAndBoundToCurrentSession(t *testing.T) {
	root := t.TempDir()
	m := NewManager(Options{StateDir: root, WorkassToolsOrigin: "https://tools.localhost:8788", WorkassToolsCAFile: filepath.Join(root, "ca.pem"), WorkassToolsCommand: filepath.Join(root, "workass")})
	t.Cleanup(func() { m.Reset() })
	m.mu.Lock()
	owner := m.newAgentOwnerKeyLocked()
	m.agentOwnerBySession["session"] = owner
	m.bindAgentOwnerLocked(owner, "chat", "tab")
	m.mu.Unlock()
	first, err := m.toolContextBrief("session", "chat", "tab")
	if err != nil {
		t.Fatal(err)
	}
	entry := m.toolContexts[owner]
	info, err := os.Stat(entry.path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatal("context is accessible to other users")
	}
	if strings.Contains(first, owner) {
		t.Fatal("credential leaked into prompt")
	}
	config, err := toolcli.ReadConfig(entry.path)
	if err != nil {
		t.Fatal(err)
	}
	if config.ChatID != "chat" || config.TabID != "tab" || config.Credential != owner {
		t.Fatal("context lost exact owner")
	}
	second, err := m.toolContextBrief("session", "chat", "tab")
	if err != nil || first != second {
		t.Fatal("later turn replaced the session context")
	}
	after, _ := os.Stat(entry.path)
	if !info.ModTime().Equal(after.ModTime()) || !os.SameFile(info, after) {
		t.Fatal("later turn rewrote context")
	}
	if _, err := m.toolContextBrief("session", "other-chat", "tab"); err == nil {
		t.Fatal("context accepted another chat")
	}
	// A spare's owner can be rebound to its adopted chat. The old file is
	// removed and the old exact pair no longer authorizes requests.
	m.mu.Lock()
	m.bindAgentOwnerLocked(owner, "adopted", "new-tab")
	m.mu.Unlock()
	third, err := m.toolContextBrief("session", "adopted", "new-tab")
	if err != nil || third == first {
		t.Fatal("adopted chat retained old context")
	}
	if _, err := os.Stat(entry.path); !os.IsNotExist(err) {
		t.Fatal("old context was retained")
	}
	if m.ValidateAgentOwner(owner, "chat", "tab") {
		t.Fatal("old caller identity remained authorized")
	}
	newPath := m.toolContexts[owner].path
	m.Reset()
	if _, err := os.Stat(newPath); !os.IsNotExist(err) {
		t.Fatal("reset retained context")
	}
	if m.ValidateAgentOwner(owner, "adopted", "new-tab") {
		t.Fatal("reset retained tool authority")
	}
}

func TestToolContextDoesNotAdvertiseForUnconfiguredSessions(t *testing.T) {
	m := NewManager(Options{})
	t.Cleanup(func() { m.Reset() })
	brief, err := m.toolContextBrief("probe", "", "")
	if err != nil || brief != "" {
		t.Fatal("unconfigured probe received Workass tool instructions")
	}
}
