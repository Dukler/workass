package main

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"workass/internal/acp"
	"workass/internal/fleetid"
)

// Job-start credentials: what a chat hands its own timers, so a launchd/systemd
// timer with no chat turn can start a supervised job owned by that chat
// (internal/acp/job_start_tokens.go, former issue #4).

func (h *agentControlHandler) jobCredentialCall(method, ownerKey, tabID, chatID string, params map[string]any) (any, error) {
	if h == nil || h.manager == nil {
		return nil, errors.New("job-start credentials require the chat manager")
	}
	if err := h.authorizeActorOwner(ownerKey, tabID, chatID, "Workass chat control caller is not an owned ACP session"); err != nil {
		return nil, err
	}
	if strings.HasPrefix(chatID, subagentTabPrefix) {
		return nil, errors.New("a subagent cannot issue job-start credentials: its jobs would outlive it with nobody to tell; ask the chat that owns you")
	}
	switch method {
	case "job_credential.issue":
		owner := ""
		if self := strings.TrimSpace(h.selfMachineID); self != "" {
			id, err := fleetid.New(self, fleetid.Chat, chatID)
			if err != nil {
				return nil, err
			}
			owner = id.String()
		}
		binding, replayed, err := h.manager.IssueJobStartToken(tabID, chatID, owner, fieldString(params, "label"), fieldString(params, "operation_id"))
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"ok": true, "replayed": replayed,
			"credential_id": binding.ID, "token_file": binding.File, "owner": binding.Owner,
			"label": binding.Label, "issued_at": binding.IssuedAt,
			"usage": "workass run --token-file " + shellQuoteWord(binding.File) + " --label LABEL --operation-id ID-UNIQUE-PER-RUN -- COMMAND...",
		}, nil
	case "job_credential.list":
		items := make([]map[string]any, 0)
		for _, binding := range h.manager.ListJobStartTokens(tabID, chatID) {
			item := map[string]any{
				"credential_id": binding.ID, "label": binding.Label, "token_file": binding.File,
				"owner": binding.Owner, "issued_at": binding.IssuedAt,
			}
			if binding.LastUsedAt != "" {
				item["last_used_at"] = binding.LastUsedAt
			}
			items = append(items, item)
		}
		return map[string]any{"credentials": items}, nil
	case "job_credential.revoke":
		revoked, err := h.manager.RevokeJobStartToken(tabID, chatID, fieldString(params, "credential_id"))
		if err != nil {
			return nil, err
		}
		return map[string]any{"ok": true, "revoked": revoked, "credential_id": fieldString(params, "credential_id")}, nil
	}
	return nil, errors.New("unknown job credential method")
}

// shellQuoteWord quotes a path for a POSIX shell when it needs it.
func shellQuoteWord(value string) string {
	if value != "" && !strings.ContainsAny(value, " \t\n'\"\\$`!*?[]{}()<>|&;#~") {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// jobStartCredentialTools are the tools a timer's job-start credential reaches:
// its own job's report, and the supervised-job family it needs to run its
// chat's jobs on ANOTHER machine — start, the "is it already running?" list, a
// read, and a stop (bug-20261110-12916e73). Before, a remote start was possible
// only with a saved copy of the chat's whole context, which reaches every tool
// and dies with the session.
var jobStartCredentialTools = map[string]bool{
	"workass_register_process_child": true,
	"workass_start_process_child":    true,
	"workass_list_process_children":  true,
	"workass_read_process_child":     true,
	"workass_cancel_process_child":   true,
}

// jobStartCredentialMethods are the same tools past the register, by method.
var jobStartCredentialMethods = map[string]bool{
	"process.start": true, "process.list": true, "process.read": true, "process.cancel": true,
}

// jobStartCredentialCall runs one of those calls as the chat that issued the
// credential, already swapped in by call(), and holds the answer to that chat's
// own jobs: another chat's job is neither listed, read nor stopped.
func (h *agentControlHandler) jobStartCredentialCall(r *http.Request, binding acp.JobStartBinding, method string, params map[string]any) (any, error) {
	if !jobStartCredentialMethods[method] {
		return nil, errors.New("this job-start credential does not authorize " + method)
	}
	machine := strings.TrimSpace(fieldString(params, "machine_id"))
	asChat := func(method string, params map[string]any) (any, error) {
		// The owner key is the chat's own now, so this is that chat's call.
		return h.call(r, agentControlRequest{Method: method, Params: params})
	}
	switch method {
	case "process.start":
		if machine == "" || machine == strings.TrimSpace(h.selfMachineID) {
			return nil, errors.New("a job-start credential starts a job on THIS machine with `workass run --token-file TOKEN_FILE --label ... --operation-id ... -- COMMAND`; workass_start_process_child is for another fleet machine, named by machine_id")
		}
		return asChat(method, params)
	case "process.list":
		answer, err := asChat(method, params)
		if err != nil {
			return nil, err
		}
		return jobStartOwnList(binding, answer), nil
	case "process.read":
		answer, err := asChat(method, params)
		if err != nil {
			return nil, err
		}
		owned := mapFromAnyMain(cloneJSON(answer))
		if err := jobStartOwnsJob(binding, mapFromAnyMain(owned["child"]), fieldString(params, "work_id")); err != nil {
			return nil, err
		}
		return owned, nil
	case "process.cancel":
		// Read first: a stop is written the moment it is asked for, so whose job
		// it is has to be settled before.
		read := copyAnyMap(params)
		delete(read, "operation_id")
		delete(read, "reason")
		read["tail_chars"] = 0
		answer, err := asChat("process.read", read)
		if err != nil {
			return nil, err
		}
		if err := jobStartOwnsJob(binding, mapFromAnyMain(mapFromAnyMain(cloneJSON(answer))["child"]), fieldString(params, "work_id")); err != nil {
			return nil, err
		}
		return asChat(method, params)
	}
	return nil, errors.New("this job-start credential does not authorize " + method)
}

// jobStartOwnsRow is a job row belonging to the credential's chat: by its
// global owner, or, for a job registered here, by its exact chat.
func jobStartOwnsRow(binding acp.JobStartBinding, row map[string]any) bool {
	if owner := strings.TrimSpace(binding.Owner); owner != "" && strings.TrimSpace(fieldString(row, "owner")) == owner {
		return true
	}
	return fieldString(row, "chat_id") == binding.ChatID && fieldString(row, "tab_id") == binding.TabID
}

func jobStartOwnsJob(binding acp.JobStartBinding, row map[string]any, workID string) error {
	if len(row) == 0 || !jobStartOwnsRow(binding, row) {
		return fmt.Errorf("job %s belongs to another chat; a job-start credential reads and stops only the jobs of the chat that issued it", strings.TrimSpace(workID))
	}
	return nil
}

// jobStartOwnList keeps the chat's own rows and queue entries. A chat at full
// access sees a machine's whole queue; a timer's credential sees its own work.
func jobStartOwnList(binding acp.JobStartBinding, answer any) map[string]any {
	out := mapFromAnyMain(cloneJSON(answer))
	children := make([]any, 0)
	for _, raw := range anySlice(out["children"]) {
		if jobStartOwnsRow(binding, mapFromAnyMain(raw)) {
			children = append(children, raw)
		}
	}
	out["children"] = children
	queues := make([]any, 0)
	for _, raw := range anySlice(out["resource_queues"]) {
		if owner := strings.TrimSpace(binding.Owner); owner != "" && fieldString(mapFromAnyMain(raw), "owner") == owner {
			queues = append(queues, raw)
		}
	}
	out["resource_queues"] = queues
	return out
}
