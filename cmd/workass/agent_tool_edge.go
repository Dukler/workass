package main

import (
	"errors"
	"fmt"
	"strings"

	"workass/internal/agenttext"
)

// What an agent's tool call adds on top of the agent-control methods, and only
// there. Everything in this file runs after those methods answered or before they
// were asked, on the tool path alone: `chat.list` and the rest keep answering the
// renderer and every fleet peer exactly as they always did.

// listChatsMaxLimit is the most rows one list call asks for.
const listChatsMaxLimit = 1000

// agentToolDefaultsToCallingChat names the tools that read or adjust one chat and
// may leave the pair out to mean the caller's own. An exact pair still means that
// exact chat, and half a pair is refused by the schema. Acts on a chat that a
// wrong guess would make costly stay explicit: delete, cancel, send, settle, focus.
var agentToolDefaultsToCallingChat = map[string]bool{
	"workass_read_chat":                  true,
	"workass_get_chat_diagnostics":       true,
	"workass_list_forks":                 true,
	"workass_create_fork_point":          true,
	"workass_list_spawned_work":          true,
	"workass_list_spawned_work_receipts": true,
	"workass_rename_chat":                true,
	"workass_configure_chat":             true,
}

type chatListOptions struct {
	includeSettled bool
	query          string
	limit          int
}

// takeChatListOptions moves the tool's own options out of the params, so they
// never reach a route.
func takeChatListOptions(params map[string]any) chatListOptions {
	includeSettled, _ := boolField(params, "include_settled")
	options := chatListOptions{
		includeSettled: includeSettled,
		query:          strings.ToLower(strings.TrimSpace(fieldString(params, "query"))),
		limit:          intField(params, "limit"),
	}
	delete(params, "include_settled")
	delete(params, "query")
	delete(params, "limit")
	return options
}

// projectToolChatList is what an agent reads of the fleet's chat list: the chats
// that are not settled, with empty fields dropped, the caller's own marked, and a
// statement of what was left out and how to get it. With 255 of 261 chats settled
// the whole list cost about 20,000 tokens on every call, and the tool's description
// tells agents to call it first.
func projectToolChatList(result any, list chatListOptions, caller agentMCPOptions) map[string]any {
	answer := copyAnyMap(mapFromAnyMain(result))
	rows := anySlice(answer["chats"])
	kept := make([]any, 0, len(rows))
	settledOmitted := 0
	for _, raw := range rows {
		row := mapFromAnyMain(raw)
		if list.query != "" && !chatRowMatches(row, list.query) {
			continue
		}
		if chatRowSettled(row) && !list.includeSettled {
			settledOmitted++
			continue
		}
		kept = append(kept, compactChatRow(row, caller))
	}
	if list.limit > 0 && len(kept) > list.limit {
		answer["omittedByLimit"] = len(kept) - list.limit
		kept = kept[:list.limit]
	}
	answer["chats"] = kept
	answer["self"] = map[string]any{"tab_id": caller.TabID, "chat_id": caller.ChatID}
	if settledOmitted > 0 {
		answer["settledOmitted"] = settledOmitted
		answer["note"] = fmt.Sprintf(agenttext.Get("tools.list_chats.settled_note"), settledOmitted)
	}
	return answer
}

// chatRowSettled follows the sidebar: only an explicit settle files a chat, and
// live work never reads as filed. A row from a route that does not carry the field
// reads as not settled, so it is shown rather than hidden.
func chatRowSettled(row map[string]any) bool {
	running, _ := row["running"].(bool)
	return fieldString(row, "settled") == "settled" && !running
}

func chatRowMatches(row map[string]any, query string) bool {
	return strings.Contains(strings.ToLower(fieldString(row, "title")), query) ||
		strings.Contains(strings.ToLower(fieldString(row, "cwd")), query)
}

// compactChatRow drops what a row says by its absence: empty text, false, zero.
// Ids and machineId are what later calls need, and they are never empty on a row
// worth listing.
func compactChatRow(row map[string]any, caller agentMCPOptions) map[string]any {
	out := make(map[string]any, len(row))
	for key, value := range row {
		switch typed := value.(type) {
		case nil:
			continue
		case string:
			if typed == "" {
				continue
			}
		case bool:
			if !typed {
				continue
			}
		default:
			if number, ok := schemaNumber(value); ok && number == 0 {
				continue
			}
		}
		out[key] = value
	}
	// When it was settled is the sidebar's archive clock, and reactivated is the
	// same to a reader as never filed.
	delete(out, "settledAt")
	if fieldString(out, "settled") != "settled" {
		delete(out, "settled")
	}
	if fieldString(out, "machineName") == fieldString(out, "machineId") {
		delete(out, "machineName")
	}
	if fieldString(row, "tabId") == caller.TabID && fieldString(row, "chatId") == caller.ChatID {
		out["self"] = true
	}
	return out
}

// checkUpdateApplyArguments refuses an apply that could never pass, in words that
// say what would. The authorization is exact text about this one update: stating
// it does not grant anything, which is why the owner's rule is named beside it.
func checkUpdateApplyArguments(params map[string]any) error {
	machineID := strings.TrimSpace(fieldString(params, "machine_id"))
	current := strings.TrimSpace(fieldString(params, "expected_current_version"))
	target := strings.TrimSpace(fieldString(params, "expected_target_version"))
	if !updateVersionPattern.MatchString(current) || !updateVersionPattern.MatchString(target) {
		return errors.New("expected_current_version and expected_target_version must be exact X.Y.Z versions; read currentVersion and targetVersion from workass_get_update_status for this machine")
	}
	expected := fmt.Sprintf("update %s from %s to %s", machineID, current, target)
	if fieldString(params, "authorization") != expected {
		return fmt.Errorf("authorization must be exactly %q. That text only names the update; applying one needs the owner's permission, under Hard rules in AGENTS.md", expected)
	}
	return nil
}

// plainAgentToolError says what is really wrong when a call could not be carried,
// and what to do. The router's own words name plumbing a caller cannot act on.
func plainAgentToolError(toolName string, params map[string]any, selfMachineID string, err error) string {
	text := err.Error()
	machineID := strings.TrimSpace(fieldString(params, "machine_id"))
	if machineID == strings.TrimSpace(selfMachineID) {
		machineID = ""
	}
	switch {
	case strings.Contains(text, "no local renderer"):
		if strings.Contains(toolName, "update") && machineID == "" || strings.Contains(toolName, "update_targets") {
			return "no Workass window is open on this machine, and the updater runs in the app: open the Workass app here and try again"
		}
		if machineID != "" {
			return "this machine has no direct link to " + machineID + " and no Workass window open to reach it through: open the Workass app here, or check that machine with workass_list_update_targets, and try again"
		}
		return "this machine has no direct link to that machine and no Workass window open to reach it through: open the Workass app here and try again"
	case strings.Contains(text, "MCP router timed out"):
		return "the Workass window on this machine did not answer in time: if the app is busy or closing, try again in a moment, with the same operation_id if this was a mutation"
	case strings.Contains(text, "MCP router is unavailable"):
		return "this Workass daemon has no way to reach other machines: it was started with no fleet and no app window"
	}
	return text
}
