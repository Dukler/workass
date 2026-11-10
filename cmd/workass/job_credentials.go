package main

import (
	"errors"
	"strings"

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
