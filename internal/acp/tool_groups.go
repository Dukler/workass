package acp

import (
	"strings"

	"workass/internal/agenttext"
)

// Tool groups are the switches in Ajustes > Herramientas. Each one removes a
// family of Workass tools from what agents on this machine receive: its rows
// leave `tools list`, its schemas and calls are refused, and its sentences
// leave the guide. Tools in no group are the ones Workass itself depends on
// (questions to the person, file delivery, the model catalog, and the report
// `workass run` makes), and no switch removes them.
const (
	ToolGroupBrowser   = "browser"
	ToolGroupPages     = "pages"
	ToolGroupSubagents = "subagents"
	ToolGroupJobs      = "jobs"
	ToolGroupScheduled = "scheduled"
	ToolGroupChats     = "chats"
	ToolGroupMachines  = "machines"
)

// ToolGroupOrder is every group, in the order Settings lists them.
var ToolGroupOrder = []string{
	ToolGroupBrowser, ToolGroupPages, ToolGroupSubagents, ToolGroupJobs,
	ToolGroupScheduled, ToolGroupChats, ToolGroupMachines,
}

// ToolGroups is the set of groups switched off. The zero value has every
// group on.
type ToolGroups map[string]bool

// Off reports whether group is switched off. The empty group, which essential
// tools belong to, is never off.
func (g ToolGroups) Off(group string) bool {
	return group != "" && g[group]
}

// KnownToolGroup reports whether id names a group.
func KnownToolGroup(id string) bool {
	for _, known := range ToolGroupOrder {
		if id == known {
			return true
		}
	}
	return false
}

// SetToolGroupsSource tells the manager where to read the groups switched off.
// It is read on every guide it composes, so a change applies to the next turn.
func (m *Manager) SetToolGroupsSource(source func() ToolGroups) {
	if m == nil {
		return
	}
	m.toolGroupsSource.Store(&source)
}

// ToolGroupsOff is the set of groups switched off right now.
func (m *Manager) ToolGroupsOff() ToolGroups {
	if m == nil {
		return nil
	}
	source := m.toolGroupsSource.Load()
	if source == nil || *source == nil {
		return nil
	}
	return (*source)()
}

// WorkassGuide is what a native harness receives about Workass with these
// groups off: the instructions appended to its system prompt, and the guide
// `tools guide` prints. Settings counts its cost from exactly this text.
func (m *Manager) WorkassGuide(off ToolGroups) (instructions, guide string) {
	if m == nil {
		return workassNativeInstructions, nativeToolGuide(off)
	}
	return workassNativeInstructions, m.environmentBriefWith(false, off) + nativeToolGuide(off)
}

// environmentControls is the brief's paragraph on control tools, one clause
// per group.
func environmentControls(off ToolGroups) string {
	return joinToolText(off,
		toolText{"", agenttext.Get("environment.controls")},
		toolText{ToolGroupChats, agenttext.Get("environment.controls.chats")},
		toolText{ToolGroupMachines, agenttext.Get("environment.controls.machines")},
		toolText{ToolGroupSubagents, agenttext.Get("environment.controls.subagents")},
		toolText{ToolGroupPages, agenttext.Get("environment.controls.pages")},
		toolText{"", "."},
		toolText{ToolGroupChats, agenttext.Get("environment.controls.chats.pairs")},
		toolText{"", "\n"},
	)
}

// nativeToolGuide is the reference `tools guide` prints after the brief.
func nativeToolGuide(off ToolGroups) string {
	return joinToolText(off,
		toolText{"", agenttext.Get("native.guide")},
		toolText{ToolGroupMachines, agenttext.Get("native.guide.machines")},
		toolText{ToolGroupSubagents, agenttext.Get("native.guide.subagents")},
		toolText{"", agenttext.Get("native.guide.background")},
		toolText{ToolGroupSubagents, agenttext.Get("native.guide.background.subagents")},
		toolText{ToolGroupChats, agenttext.Get("native.guide.background.chats")},
		toolText{ToolGroupJobs, agenttext.Get("native.guide.background.jobs")},
		toolText{"", ".\n"},
	)
}

// joinToolText keeps the parts whose group is on, in order.
func joinToolText(off ToolGroups, parts ...toolText) string {
	var out strings.Builder
	for _, part := range parts {
		if !off.Off(part.group) {
			out.WriteString(part.text)
		}
	}
	return out.String()
}

type toolText struct {
	group string
	text  string
}
