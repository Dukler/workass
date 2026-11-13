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

// toolGroupSpawn is not a switch. It marks the sentences about spawning and
// orchestrating subagents, which leave with the subagents group and are never
// in a delegated child's guide: a child cannot spawn subagents of its own
// (agentToolsForChild in cmd/workass), though it keeps the fork tools.
const toolGroupSpawn = "subagents.spawn"

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
	return m.WorkassGuideFor(off, false)
}

// WorkassGuideFor is WorkassGuide for a chat's own engine or, forSubagent, for
// a delegated child's. A child's catalog has no browser, page, question or
// subagent tools, so its guide names none of them: a guide that offers a tool
// `tools call` then answers "unknown" sends the child down a dead end.
func (m *Manager) WorkassGuideFor(off ToolGroups, forSubagent bool) (instructions, guide string) {
	off = guideGroupsOff(off, forSubagent)
	if m == nil {
		return workassNativeInstructions, nativeToolGuide(off)
	}
	return workassNativeInstructions, m.environmentBriefWith(forSubagent, off) + nativeToolGuide(off)
}

// guideGroupsOff is off widened to the sentences the reader cannot act on: the
// spawn sentences whenever subagents are off, and for a delegated child every
// group its catalog lacks. It is idempotent, so text built from an already
// widened set is unchanged.
func guideGroupsOff(off ToolGroups, forSubagent bool) ToolGroups {
	widened := ToolGroups{}
	for group, isOff := range off {
		widened[group] = isOff
	}
	if widened[ToolGroupSubagents] {
		widened[toolGroupSpawn] = true
	}
	if forSubagent {
		widened[ToolGroupBrowser] = true
		widened[ToolGroupPages] = true
		widened[toolGroupSpawn] = true
	}
	return widened
}

// environmentControls is the brief's paragraph on control tools, one clause
// per group.
func environmentControls(off ToolGroups) string {
	off = guideGroupsOff(off, false)
	return joinToolText(off,
		toolText{"", agenttext.Get("environment.controls")},
		toolText{ToolGroupChats, agenttext.Get("environment.controls.chats")},
		toolText{ToolGroupMachines, agenttext.Get("environment.controls.machines")},
		toolText{toolGroupSpawn, agenttext.Get("environment.controls.subagents")},
		toolText{ToolGroupPages, agenttext.Get("environment.controls.pages")},
		toolText{"", "."},
		toolText{ToolGroupChats, agenttext.Get("environment.controls.chats.pairs")},
		toolText{"", "\n"},
	)
}

// nativeToolGuide is the reference `tools guide` prints after the brief.
func nativeToolGuide(off ToolGroups) string {
	off = guideGroupsOff(off, false)
	return joinToolText(off,
		toolText{"", agenttext.Get("native.guide")},
		toolText{ToolGroupMachines, agenttext.Get("native.guide.machines")},
		toolText{ToolGroupSubagents, agenttext.Get("native.guide.subagents")},
		toolText{"", agenttext.Get("native.guide.background")},
		toolText{toolGroupSpawn, agenttext.Get("native.guide.background.subagents")},
		toolText{ToolGroupSubagents, agenttext.Get("native.guide.background.forks")},
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
