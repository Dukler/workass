package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"workass/internal/acp"
)

// Ajustes > Herramientas: which Workass tools agents on this machine receive.
// A person switches a group off; its tools leave `tools list`, their schemas and
// calls are refused at /workass/tools, and the guide drops its sentences
// (internal/acp/tool_groups.go). The setting lives in the app-settings blob.

// agentToolGroupsOffKey is the app-settings key: the ids of the groups off.
const agentToolGroupsOffKey = "agentToolGroupsOff"

// agentToolGroups assigns every Workass tool that a switch may remove. Browser
// tools are the workass_browser_ family. Everything else is in
// essentialAgentTools; TestEveryWorkassToolHasAGroupOrIsEssential keeps a new
// tool from landing in neither.
var agentToolGroups = map[string]string{
	"workass_render_html": acp.ToolGroupPages,

	"workass_spawn_subagent":             acp.ToolGroupSubagents,
	"workass_list_subagents":             acp.ToolGroupSubagents,
	"workass_wait_subagent":              acp.ToolGroupSubagents,
	"workass_wait_subagents":             acp.ToolGroupSubagents,
	"workass_message_subagent":           acp.ToolGroupSubagents,
	"workass_retry_subagent":             acp.ToolGroupSubagents,
	"workass_list_subagent_receipts":     acp.ToolGroupSubagents,
	"workass_cancel_subagent":            acp.ToolGroupSubagents,
	"workass_decide_subagent_permission": acp.ToolGroupSubagents,
	"workass_create_fork_point":          acp.ToolGroupSubagents,
	"workass_list_forks":                 acp.ToolGroupSubagents,
	"workass_fork_run":                   acp.ToolGroupSubagents,
	"workass_wait_fork_run":              acp.ToolGroupSubagents,
	"workass_cancel_fork_child":          acp.ToolGroupSubagents,
	"workass_retry_fork_child":           acp.ToolGroupSubagents,

	"workass_start_process_child":        acp.ToolGroupJobs,
	"workass_list_process_children":      acp.ToolGroupJobs,
	"workass_read_process_child":         acp.ToolGroupJobs,
	"workass_cancel_process_child":       acp.ToolGroupJobs,
	"workass_register_external_work":     acp.ToolGroupJobs,
	"workass_settle_external_work":       acp.ToolGroupJobs,
	"workass_list_spawned_work":          acp.ToolGroupJobs,
	"workass_list_spawned_work_receipts": acp.ToolGroupJobs,

	"workass_schedule_message":         acp.ToolGroupScheduled,
	"workass_list_scheduled_messages":  acp.ToolGroupScheduled,
	"workass_cancel_scheduled_message": acp.ToolGroupScheduled,

	"workass_list_chats":           acp.ToolGroupChats,
	"workass_read_chat":            acp.ToolGroupChats,
	"workass_create_chat":          acp.ToolGroupChats,
	"workass_create_subsession":    acp.ToolGroupChats,
	"workass_rename_chat":          acp.ToolGroupChats,
	"workass_settle_chat":          acp.ToolGroupChats,
	"workass_configure_chat":       acp.ToolGroupChats,
	"workass_focus_chat":           acp.ToolGroupChats,
	"workass_delete_chat":          acp.ToolGroupChats,
	"workass_send_chat_message":    acp.ToolGroupChats,
	"workass_cancel_chat_turn":     acp.ToolGroupChats,
	"workass_get_chat_diagnostics": acp.ToolGroupChats,

	"workass_discover_machines":   acp.ToolGroupMachines,
	"workass_list_update_targets": acp.ToolGroupMachines,
	"workass_get_update_status":   acp.ToolGroupMachines,
	"workass_apply_update":        acp.ToolGroupMachines,
}

// essentialAgentTools are what Workass itself depends on, so no switch removes
// them: a question card to the person, delivering a file to the person, the
// real model catalog other tools are given ids from, and the report every
// `workass run` job makes (switching that off would orphan running jobs).
var essentialAgentTools = map[string]bool{
	"workass_ask_user_question":      true,
	"workass_host_artifact":          true,
	"workass_agent_catalog":          true,
	"workass_register_process_child": true,
}

// agentToolGroup is the group a tool belongs to; "" is essential.
func agentToolGroup(name string) string {
	if strings.HasPrefix(name, "workass_browser_") {
		return acp.ToolGroupBrowser
	}
	return agentToolGroups[name]
}

// withoutToolGroups is the catalog minus every group switched off.
func withoutToolGroups(catalog []map[string]any, off acp.ToolGroups) []map[string]any {
	if len(off) == 0 {
		return catalog
	}
	kept := make([]map[string]any, 0, len(catalog))
	for _, tool := range catalog {
		if !off.Off(agentToolGroup(toString(tool["name"]))) {
			kept = append(kept, tool)
		}
	}
	return kept
}

// toolSwitchedOffError is what an agent hears for a tool whose group is off.
func toolSwitchedOffError(name string) string {
	return name + " is switched off on this machine (Workass settings, Herramientas); tools list shows the tools that are on"
}

// normalizeToolGroupsOff keeps known group ids, once each, in Settings order.
func normalizeToolGroupsOff(value any) []any {
	items, _ := value.([]any)
	off := map[string]bool{}
	for _, item := range items {
		if id, ok := item.(string); ok && acp.KnownToolGroup(strings.TrimSpace(id)) {
			off[strings.TrimSpace(id)] = true
		}
	}
	out := []any{}
	for _, id := range acp.ToolGroupOrder {
		if off[id] {
			out = append(out, id)
		}
	}
	return out
}

func toolGroupsOffFromSettings(settings map[string]any) acp.ToolGroups {
	off := acp.ToolGroups{}
	for _, item := range normalizeToolGroupsOff(settings[agentToolGroupsOffKey]) {
		off[item.(string)] = true
	}
	return off
}

// agentToolsCatalog is what a chat's agent can list, before any switch.
func agentToolsCatalog() []map[string]any {
	return append(agentMCPTools(), browserMCPTools()...)
}

// toolListText is the exact output of `tools list`.
func toolListText(off acp.ToolGroups) string {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(toolListing(withoutToolGroups(agentToolsCatalog(), off)))
	return buffer.String()
}

// approxTokens is the usual rough count: four characters a token.
func approxTokens(characters int) int {
	return (characters + 2) / 4
}

// agentToolsView is Ajustes > Herramientas: each group with whether it is on
// and what it costs, and what Workass adds in total. Every number is counted
// from the text agents are actually given: the instructions and guide
// (WorkassGuide) and the `tools list` output, so it stays true when the text
// changes. A group's cost is what switching it off removes.
func agentToolsView(manager *acp.Manager, off acp.ToolGroups) map[string]any {
	type size struct{ guide, list int }
	measure := func(off acp.ToolGroups) size {
		instructions, guide := manager.WorkassGuide(off)
		return size{utf8.RuneCountInString(instructions + guide), utf8.RuneCountInString(toolListText(off))}
	}
	now := measure(off)
	everything := measure(nil)
	allOff := acp.ToolGroups{}
	for _, id := range acp.ToolGroupOrder {
		allOff[id] = true
	}
	essential := measure(allOff)
	counts := map[string]int{}
	essentialCount := 0
	for _, tool := range agentToolsCatalog() {
		name := toString(tool["name"])
		if internalWorkassTools[name] {
			continue
		}
		if group := agentToolGroup(name); group != "" {
			counts[group]++
		} else {
			essentialCount++
		}
	}
	groups := make([]any, 0, len(acp.ToolGroupOrder))
	for _, id := range acp.ToolGroupOrder {
		without := measure(acp.ToolGroups{id: true})
		removed := (everything.guide + everything.list) - (without.guide + without.list)
		groups = append(groups, map[string]any{
			"id": id, "on": !off.Off(id), "tools": counts[id], "tokens": approxTokens(removed),
		})
	}
	return map[string]any{
		"groups":      groups,
		"essential":   map[string]any{"tools": essentialCount, "tokens": approxTokens(essential.guide + essential.list)},
		"guideTokens": approxTokens(now.guide),
		"listTokens":  approxTokens(now.list),
		"totalTokens": approxTokens(now.guide + now.list),
	}
}

// setAgentToolGroup switches one group on or off in the settings blob, under
// the store's lock, keeping every other key.
func setAgentToolGroup(store *appSettingsStore, group string, on bool) (map[string]any, error) {
	group = strings.TrimSpace(group)
	if !acp.KnownToolGroup(group) {
		return nil, errors.New("unknown tool group " + group)
	}
	return store.update(func(settings map[string]any) {
		off := toolGroupsOffFromSettings(settings)
		if on {
			delete(off, group)
		} else {
			off[group] = true
		}
		list := make([]any, 0, len(off))
		for id := range off {
			list = append(list, id)
		}
		settings[agentToolGroupsOffKey] = normalizeToolGroupsOff(list)
	})
}
