package acp

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// Each group switched off takes its own sentences out of the brief, the guide
// and the per-turn rule, and nothing else.
func TestSwitchedOffToolGroupLeavesItsSentences(t *testing.T) {
	t.Parallel()
	manager := NewManager(Options{WorkassToolsOrigin: "https://localhost:8788"})
	t.Cleanup(func() { manager.Reset() })
	everything := manager.environmentBriefWith(false, nil) + nativeToolGuide(nil)
	for group, gone := range map[string][]string{
		ToolGroupBrowser:   {"Browser tools:", "workass_browser_list"},
		ToolGroupPages:     {"workass_render_html"},
		ToolGroupSubagents: {"orchestrate tracked subagents", "timeout_ms=-1", "workass_spawn_subagent", "workass_fork_run"},
		ToolGroupJobs:      {"`workass run`", "workass_start_process_child", "workass_register_external_work"},
		ToolGroupChats:     {"list/read/create/rename", "workass_list_chats", "workass_create_subsession", "workass_create_chat"},
		ToolGroupMachines:  {"update state and failure logs", "workass_discover_machines"},
	} {
		off := ToolGroups{group: true}
		text := manager.environmentBriefWith(false, off) + nativeToolGuide(off)
		for _, phrase := range gone {
			if !strings.Contains(everything, phrase) {
				t.Fatalf("%q is not in the full guide to begin with", phrase)
			}
			if strings.Contains(text, phrase) {
				t.Errorf("%s off: still says %q", group, phrase)
			}
		}
		for _, kept := range []string{"Workass CLI reference", "workass_agent_catalog", "workass_host_artifact", "Chat transcripts live at"} {
			if !strings.Contains(text, kept) {
				t.Errorf("%s off: lost %q", group, kept)
			}
		}
	}
}

func TestBrowserRuleFollowsItsSwitch(t *testing.T) {
	t.Parallel()
	manager := NewManager(Options{WorkassToolsOrigin: "https://localhost:8788"})
	t.Cleanup(func() { manager.Reset() })
	if !strings.Contains(manager.buildUserRequestBlock("hola", true), "Browser tools:") {
		t.Fatal("the browser rule is missing with the browser on")
	}
	manager.SetToolGroupsSource(func() ToolGroups { return ToolGroups{ToolGroupBrowser: true} })
	if strings.Contains(manager.buildUserRequestBlock("hola", true), "Browser tools:") || strings.Contains(manager.buildEnvironmentBrief(false), "Browser tools:") {
		t.Fatal("the browser rule is still sent with the browser off")
	}
	if ToolGroups(nil).Off("") || (ToolGroups{"": true}).Off("") {
		t.Fatal("essential tools must never read as off")
	}
}

// A native engine keeps one guide.md for its life; the next turn after a
// switch rewrites it, and a turn with nothing changed leaves the file alone.
func TestNativeGuideFollowsSwitchesOnTheNextTurn(t *testing.T) {
	root := t.TempDir()
	opts := Options{StateDir: root, Provider: ProviderConfig{ID: "opencode"}, WorkassToolsOrigin: "https://tools.localhost:8788", WorkassToolsCommand: filepath.Join(root, "workass"), WorkassToolsCAFile: filepath.Join(root, "ca.pem")}
	m := NewManager(opts)
	t.Cleanup(func() { m.Reset() })
	var off atomic.Pointer[ToolGroups]
	off.Store(&ToolGroups{})
	m.SetToolGroupsSource(func() ToolGroups { return *off.Load() })
	b := newBridge("native-guide-test", opts, m)
	prepared, err := b.prepareNativeInstructions(ProviderConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(b.nativeInstructionsDir) })
	guidePath := prepared.Env["WORKASS_TOOLS_GUIDE"]
	read := func() string {
		data, err := os.ReadFile(guidePath)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	if !strings.Contains(read(), "workass_spawn_subagent") {
		t.Fatal("the first guide lacks subagents")
	}
	m.mu.Lock()
	owner := m.newAgentOwnerKeyLocked()
	m.bindAgentOwnerLocked(owner, "chat", "tab")
	m.mu.Unlock()
	off.Store(&ToolGroups{ToolGroupSubagents: true})
	if err := b.bindNativeToolContext(owner, "chat", "tab"); err != nil {
		t.Fatal(err)
	}
	if guide := read(); strings.Contains(guide, "workass_spawn_subagent") || !strings.Contains(guide, "Workass CLI reference") {
		t.Fatalf("the guide did not follow the switch:\n%s", guide)
	}
	if info, err := os.Stat(guidePath); err != nil || info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("guide is not private: %v %v", info, err)
	}
	before, _ := os.Stat(guidePath)
	if err := b.bindNativeToolContext(owner, "chat", "tab"); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(guidePath)
	if !os.SameFile(before, after) {
		t.Fatal("an unchanged guide was rewritten")
	}
	entries, _ := os.ReadDir(filepath.Dir(guidePath))
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "guide-") {
			t.Fatalf("a temporary guide was left behind: %s", entry.Name())
		}
	}
}

// bug-20261113-0bf46145: a delegated child's engine was given the chat's guide,
// which offered workass_spawn_subagent, a page and the browser, none of which
// its catalog has. Its guide.md now names the fork tools it keeps and nothing
// it lacks; a chat's engine keeps the full guide.
func TestADelegatedChildsGuideNamesOnlyItsTools(t *testing.T) {
	root := t.TempDir()
	opts := Options{StateDir: root, Provider: ProviderConfig{ID: "opencode"}, WorkassToolsOrigin: "https://tools.localhost:8788", WorkassToolsCommand: filepath.Join(root, "workass"), WorkassToolsCAFile: filepath.Join(root, "ca.pem")}
	m := NewManager(opts)
	t.Cleanup(func() { m.Reset() })
	guideOf := func(key string) string {
		t.Helper()
		b := newBridge(key, opts, m)
		prepared, err := b.prepareNativeInstructions(ProviderConfig{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(b.nativeInstructionsDir) })
		data, err := os.ReadFile(prepared.Env["WORKASS_TOOLS_GUIDE"])
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	child := guideOf("opencode:" + subagentChatIDPrefix + "wa-subagent-1")
	for _, gone := range []string{"workass_spawn_subagent", "orchestrate tracked subagents", "workass_render_html", "Browser tools:", "workass_browser_"} {
		if strings.Contains(child, gone) {
			t.Errorf("a child's guide offers %q, which its catalog lacks:\n%s", gone, child)
		}
	}
	for _, kept := range []string{"workass_fork_run", "timeout_ms=-1", "workass_create_subsession", "Workass CLI reference"} {
		if !strings.Contains(child, kept) {
			t.Errorf("a child's guide lost %q:\n%s", kept, child)
		}
	}
	chat := guideOf("opencode:chat-1")
	if _, full := m.WorkassGuide(m.ToolGroupsOff()); chat != full {
		t.Fatalf("a chat's guide is not the full guide:\n%s", chat)
	}
	for _, kept := range []string{"workass_spawn_subagent", "workass_fork_run", "orchestrate tracked subagents"} {
		if !strings.Contains(chat, kept) {
			t.Errorf("a chat's guide lost %q", kept)
		}
	}
	// With subagents switched off the fork sentence leaves too, child or not.
	m.SetToolGroupsSource(func() ToolGroups { return ToolGroups{ToolGroupSubagents: true} })
	if _, guide := m.WorkassGuideFor(m.ToolGroupsOff(), true); strings.Contains(guide, "workass_fork_run") {
		t.Fatalf("subagents off: a child's guide still offers forks:\n%s", guide)
	}
}
