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
