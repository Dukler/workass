package main

import (
	"errors"
	"fmt"
	"strings"
)

// Remote parity for the Workass Agent API.
//
// Every agent tool reaches exactly one machine. Which one is decided here, in
// one table, instead of being rediscovered per method: the previous routing
// knew only the chat read/send/cancel family, so a tool as ordinary as
// workass_configure_chat answered "remote-chat MCP method is not supported" on
// a chat the very same agent could read and drive.
//
// A method belongs to one of four classes:
//
//   - chat-scoped: it addresses one exact (tab_id, chat_id) pair. A
//     machine-tagged pair names the owning machine; an untagged pair is local.
//   - machine-scoped: it has no chat to address, so it carries machine_id.
//   - fork-scoped: it addresses a fork point / run id minted by the machine
//     that owns the fork.
//   - local-only: it acts on the calling chat, on this machine's filesystem, or
//     on the person at this controller. A tagged id here is a caller mistake
//     and gets that method's own reason, never a generic "not supported".
type remoteAgentMethodKind uint8

const (
	remoteAgentLocalOnly remoteAgentMethodKind = iota
	remoteAgentChatScoped
	remoteAgentMachineScoped
	remoteAgentForkScoped
)

// remoteAgentMethodKinds is the single authority for Agent API remote reach.
// update.* is deliberately absent: it owns an older machine_id contract with
// its own local/remote split and must keep answering exactly as it does today.
var remoteAgentMethodKinds = map[string]remoteAgentMethodKind{
	"chat.read":               remoteAgentChatScoped,
	"chat.diagnostics":        remoteAgentChatScoped,
	"chat.configure":          remoteAgentChatScoped,
	"chat.rename":             remoteAgentChatScoped,
	"chat.settle":             remoteAgentChatScoped,
	"chat.focus":              remoteAgentChatScoped,
	"chat.delete":             remoteAgentChatScoped,
	"chat.send":               remoteAgentChatScoped,
	"chat.cancel":             remoteAgentChatScoped,
	"chat.subsession":         remoteAgentChatScoped,
	"spawned_work.list":       remoteAgentChatScoped,
	"spawned_work.receipts":   remoteAgentChatScoped,
	"agent.spawn":             remoteAgentChatScoped,
	"agent.list":              remoteAgentChatScoped,
	"agent.receipts":          remoteAgentChatScoped,
	"agent.wait":              remoteAgentChatScoped,
	"agent.wait_many":         remoteAgentChatScoped,
	"agent.message":           remoteAgentChatScoped,
	"agent.retry":             remoteAgentChatScoped,
	"agent.cancel":            remoteAgentChatScoped,
	"agent.decide_permission": remoteAgentChatScoped,
	"fork.create":             remoteAgentChatScoped,
	"fork.list":               remoteAgentChatScoped,
	"fork.run":                remoteAgentForkScoped,
	"fork.wait":               remoteAgentForkScoped,
	"fork.cancel":             remoteAgentForkScoped,
	"fork.retry":              remoteAgentForkScoped,
	"chat.create":             remoteAgentMachineScoped,
	"agent.catalog":           remoteAgentMachineScoped,
	"agent.question":          remoteAgentLocalOnly,
	"html.render":             remoteAgentLocalOnly,
	"artifact.host":           remoteAgentLocalOnly,
	"schedule.create":         remoteAgentLocalOnly,
	"schedule.list":           remoteAgentLocalOnly,
	"schedule.cancel":         remoteAgentLocalOnly,
	"external.register":       remoteAgentLocalOnly,
	"external.settle":         remoteAgentLocalOnly,
	"obligation.get":          remoteAgentLocalOnly,
}

// remoteAgentLocalOnlyReasons says, per method, why another machine cannot be
// addressed and what to do instead. A tool description is the whole surface an
// agent sees, so an explicit reason here is the difference between a retry and
// a dead end.
var remoteAgentLocalOnlyReasons = map[string]string{
	"agent.question":    "workass_ask_user_question always asks the person at this controller; it has no remote form",
	"html.render":       "workass_render_html renders into the calling chat on this machine; to show a page in a chat on another machine send its markdown with workass_send_chat_message",
	"artifact.host":     "workass_host_artifact serves a file from this machine's workspace; run it on the machine that holds the file and send the returned link to the remote chat",
	"schedule.create":   "scheduled messages are delivered to the calling chat by this machine's daemon; schedule them here and send the result with workass_send_chat_message",
	"schedule.list":     "scheduled messages are owned by the calling chat on this machine",
	"schedule.cancel":   "scheduled messages are owned by the calling chat on this machine",
	"external.register": "external work tracks a process on this machine and must stay in the exact calling chat; register it on the machine that runs the process",
	"external.settle":   "external work tracks a process on this machine and must stay in the exact calling chat",
	"obligation.get":    "the obligation belongs to the calling chat's running turn on this machine",
}

// remoteAgentAddressParams are the params whose machine tag selects the
// destination machine, per class. Only addressing ids are consulted: scanning
// a whole payload would let message text or a model id decide the route.
func remoteAgentAddressParams(kind remoteAgentMethodKind) []string {
	switch kind {
	case remoteAgentChatScoped:
		return []string{"tab_id", "chat_id"}
	case remoteAgentForkScoped:
		return []string{"fork_point_id", "run_id", "child_id"}
	case remoteAgentMachineScoped:
		// A machine-scoped method has no chat to address, so ANY tagged id in it
		// is a caller mistake worth naming rather than ignoring.
		return []string{"tab_id", "chat_id", "fork_point_id", "run_id", "child_id", "work_id", "id", "subagent_id"}
	default:
		return []string{"tab_id", "chat_id", "fork_point_id", "run_id", "child_id", "work_id", "id", "subagent_id"}
	}
}

// remoteAgentMethodTarget decides which machine one Agent API call addresses.
// The second result is false for this machine, which keeps the whole existing
// local path byte-identical.
func remoteAgentMethodTarget(method string, params map[string]any) (string, bool, error) {
	kind, known := remoteAgentMethodKinds[method]
	if !known {
		return "", false, nil
	}
	requested := strings.TrimSpace(fieldString(params, "machine_id"))
	tagged := ""
	// A pair where only one half is tagged cannot be resolved: one id names
	// another machine and the other names this one. The pair is the address.
	if kind == remoteAgentChatScoped {
		tabMachine, _, tabTagged, tabErr := splitMachineTaggedID(fieldString(params, "tab_id"))
		chatMachine, _, chatTagged, chatErr := splitMachineTaggedID(fieldString(params, "chat_id"))
		if tabErr != nil {
			return "", false, fmt.Errorf("tab_id: %w", tabErr)
		}
		if chatErr != nil {
			return "", false, fmt.Errorf("chat_id: %w", chatErr)
		}
		if (tabTagged != chatTagged) || (tabTagged && tabMachine != chatMachine) {
			return "", false, errors.New("tab_id and chat_id must belong to the same machine")
		}
	}
	for _, field := range remoteAgentAddressParams(kind) {
		machineID, _, isTagged, err := splitMachineTaggedID(fieldString(params, field))
		if err != nil {
			return "", false, fmt.Errorf("%s: %w", field, err)
		}
		if !isTagged {
			continue
		}
		if tagged != "" && tagged != machineID {
			return "", false, errors.New("every id in one " + method + " call must belong to the same machine")
		}
		tagged = machineID
	}
	if kind == remoteAgentLocalOnly {
		if tagged != "" || requested != "" {
			reason := remoteAgentLocalOnlyReasons[method]
			if reason == "" {
				reason = "this tool has no remote form"
			}
			return "", false, errors.New(reason)
		}
		return "", false, nil
	}
	if kind == remoteAgentMachineScoped {
		if tagged != "" {
			return "", false, errors.New(method + " takes machine_id, not a machine-tagged id")
		}
		if requested == "" {
			return "", false, nil
		}
		return requested, true, nil
	}
	// A secondary id (a subagent, a tracked lane) can be machine-tagged while
	// the owning pair is not: the agent read the id from a remote listing and
	// then called the follow-up tool without re-naming the chat. Say so instead
	// of failing locally with "subagent not found".
	for _, field := range []string{"id", "subagent_id", "work_id"} {
		machineID, _, isTagged, err := splitMachineTaggedID(fieldString(params, field))
		if err != nil {
			return "", false, fmt.Errorf("%s: %w", field, err)
		}
		if !isTagged {
			continue
		}
		if tagged == "" {
			return "", false, errors.New(method + " also needs the owning chat: pass the machine-tagged tab_id and chat_id from workass_list_chats next to the id")
		}
		if machineID != tagged {
			return "", false, errors.New("every id in one " + method + " call must belong to the same machine")
		}
	}
	if requested != "" && tagged != "" && requested != tagged {
		return "", false, errors.New("machine_id does not match the machine that owns the requested ids")
	}
	if tagged == "" {
		if requested != "" {
			// A bare machine_id cannot say WHICH chat on that machine owns the
			// work. Workass never infers a target, so name the pair instead.
			return "", false, errors.New(method + " addresses an exact chat: pass the machine-tagged tab_id and chat_id from workass_list_chats instead of machine_id")
		}
		return "", false, nil
	}
	return tagged, true, nil
}

// remoteAgentSubagentPair is the local guard for the subagent and background
// tools, which used to read the calling chat implicitly. An explicit pair is
// now accepted so the same tool can name a chat on another machine; locally it
// must still be the calling chat, so no agent gains reach into a sibling chat.
func remoteAgentSubagentPair(params map[string]any, callerTabID, callerChatID string) (string, string, error) {
	tabID := strings.TrimSpace(fieldString(params, "tab_id"))
	chatID := strings.TrimSpace(fieldString(params, "chat_id"))
	if tabID == "" && chatID == "" {
		return callerTabID, callerChatID, nil
	}
	if tabID == "" || chatID == "" {
		return "", "", errors.New("pass tab_id and chat_id together, or neither for the calling chat")
	}
	if tabID != callerTabID || chatID != callerChatID {
		return "", "", errors.New("this tool acts on the calling chat; an explicit tab_id + chat_id may only name a chat on another fleet machine")
	}
	return callerTabID, callerChatID, nil
}

// remoteAgentRejectAttachments keeps a remote send honest. Attachment paths are
// read on the machine that runs the chat, so a path from this machine would
// either miss or, worse, hit a different file there.
func remoteAgentRejectAttachments(params map[string]any) error {
	if len(anySlice(params["attachments"])) == 0 {
		return nil
	}
	return errors.New("attachments are read from the machine that runs the chat; send them from that machine, or host them with workass_host_artifact and send the link")
}
