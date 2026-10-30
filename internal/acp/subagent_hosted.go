package acp

import (
	"context"
	"strings"
)

// A subagent hosted here for a chat somewhere else.
//
// docs/specs/fleet-runtime.md §6 separates two facts that this product had
// always kept as one: a subagent's OWNER (the chat owed its outcome) and its
// HOME (the machine that runs it). Because they were one fact, the only way to
// name a machine was to name a chat on that machine — and the child then
// belonged to that chat. A coordinator asking for a subagent of ITS OWN chat,
// hosted on another box, got a child of the other box's chat instead, which the
// operator read exactly as it looked: "not a subagent, not even a child chat"
// (first live P1–P4 test, 2026-10-30).
//
// What this file adds is the host side of the separation. A hosted run has no
// owning chat here: it takes this machine's own defaults, it registers no row
// here, and the chat that asked holds the row, the monitor and the notice. The
// owner identity below exists only so the run has a stable internal key; it is
// never a chat, never listed, and never addressable as one.

// hostedSubagentOwnerPrefix marks the synthetic owner of a hosted run. It is
// deliberately not a chat id shape: nothing may ever resolve it to a chat,
// because there is no chat here to resolve it to.
const hostedSubagentOwnerPrefix = "fleet-guest:"

// HostedSubagentOwner is the internal owner key for a run hosted here on behalf
// of a chat on another machine. Both halves are returned so the run keys the
// same way every other run does.
func HostedSubagentOwner(originMachine, originChatID string) (string, string) {
	machine, chatID := strings.TrimSpace(originMachine), strings.TrimSpace(originChatID)
	if machine == "" || chatID == "" {
		return "", ""
	}
	owner := hostedSubagentOwnerPrefix + machine + "~" + chatID
	return owner, owner
}

// IsHostedSubagentOwner reports an owner that names no chat on this machine.
func IsHostedSubagentOwner(id string) bool {
	return strings.HasPrefix(strings.TrimSpace(id), hostedSubagentOwnerPrefix)
}

// hostedSubagentDefaults is what a hosted run inherits when there is no chat
// here to inherit from: this machine's own first ready provider and its root
// directory. Both are overridable by the spawn, and an explicit cwd is the
// normal case across machines — a path on the caller's machine would either
// miss here or, worse, hit a different directory.
func (m *Manager) hostedSubagentDefaults(ctx context.Context, ownerChatID, ownerTabID string) *Job {
	defaults := &Job{ChatID: ownerChatID, TabID: ownerTabID, CWD: strings.TrimSpace(m.opts.RootDir)}
	if provider := m.firstReadyProviderID(ctx); provider != "" {
		defaults.ProviderID = provider
		defaults.startOpts = JobStartOptions{ProviderID: provider, CWD: defaults.CWD}
	}
	return defaults
}

// firstReadyProviderID is the catalog order a brand-new chat on this machine
// would take. It is the same resolution chat.create performs for a chat created
// from another machine, so "what runs here" has one answer and not two.
func (m *Manager) firstReadyProviderID(ctx context.Context) string {
	groups, _ := m.Catalog(ctx)["groups"].([]CatalogGroup)
	for _, group := range groups {
		if group.Status == providerStatusReady {
			return group.ProviderID
		}
	}
	return ""
}

// narrowHostedSubagentIntent applies §6's narrowing to one hosted spawn.
//
// Two rules, and both exist because the parent is not here. `inherit` resolves
// to the forwarded limit, since across machines the asking chat's own ceiling
// is the only thing there is to inherit. And a requested intent wider than that
// ceiling is lowered to it: a forwarded command can never obtain a permission
// its caller did not hold (§6, §11).
func narrowHostedSubagentIntent(opts SubagentSpawnOptions) SubagentSpawnOptions {
	limit := hostedIntentRank(opts.LimitIntent)
	if limit == 0 {
		return opts
	}
	intent := strings.ToLower(strings.TrimSpace(opts.PermissionIntent))
	if intent == "" || intent == "inherit" {
		// An explicit mode_id is the caller naming a mode on THIS machine; the
		// ceiling still applies to it, but naming its replacement is not this
		// function's job — resolveSubagentSelection validates it against the
		// provider and the ceiling is applied there by the same substitution.
		if strings.TrimSpace(opts.ModeID) == "" {
			opts.PermissionIntent = canonicalHostedIntent(limit)
		}
		return opts
	}
	if hostedIntentRank(intent) > limit {
		opts.PermissionIntent = canonicalHostedIntent(limit)
	}
	return opts
}

func hostedIntentRank(intent string) int {
	switch strings.ToLower(strings.TrimSpace(intent)) {
	case "read":
		return 1
	case "edit":
		return 2
	case "full":
		return 3
	default:
		return 0
	}
}

func canonicalHostedIntent(rank int) string {
	switch rank {
	case 1:
		return "read"
	case 2:
		return "edit"
	default:
		return "full"
	}
}

// HostedSubagentRow is what the chat that ASKED needs in order to show the work
// it asked for: the child's own id at its home, its label, its selection, and
// the machine it runs on.
type HostedSubagentRow struct {
	SubagentID      string
	Label           string
	ModelLabel      string
	ProviderID      string
	HostMachine     string
	HostMachineName string
	StartedAt       string
	OriginLaneID    string
	// OriginOperationID binds the row to the parent turn that asked, exactly as
	// a local subagent's row is bound, so the "parent turn was stopped"
	// suppression and the lane routing are evaluable here too.
	OriginOperationID string
	OriginTurnID      string
	Forked            bool
}

// RegisterHostedSubagent records, in the chat that asked, one subagent running
// on another fleet machine.
//
// This is the row that was missing. The home registered its own row for a chat
// that had not asked for the work, and the asking chat had nothing at all until
// the completion notice arrived — so for the whole life of the child, the
// conversation that started it showed no sign of it. The row is the ordinary
// tracked-subagent row every surface already renders, with the host stamped on
// it; its id is the child's id at its home, so the monitor's exit settles it
// without a second identity to reconcile.
func (m *Manager) RegisterHostedSubagent(tabID, chatID string, row HostedSubagentRow) {
	tabID, chatID = strings.TrimSpace(tabID), strings.TrimSpace(chatID)
	id := strings.TrimSpace(row.SubagentID)
	if m == nil || tabID == "" || chatID == "" || id == "" {
		return
	}
	startedAt := strings.TrimSpace(row.StartedAt)
	if startedAt == "" {
		startedAt = isoNow()
	}
	label := compactText(redactSensitiveText(row.Label), 240)
	if label == "" {
		label = "Subagent"
	}
	host := strings.TrimSpace(row.HostMachineName)
	if host == "" {
		host = strings.TrimSpace(row.HostMachine)
	}
	// The host belongs in the row's own words, not in new chrome: `summary` is
	// the line every subagent row already carries ("Starting subagent"), so a
	// reader learns where the work runs from the text that is already there.
	summary := "Running on " + host
	if host == "" {
		summary = "Starting subagent"
	}
	item := SpawnedWorkItem{
		ID: id, TaskID: id, TabID: tabID, ChatID: chatID,
		ProviderID: strings.TrimSpace(row.ProviderID), Kind: trackedSubagentSpawnedWorkKind,
		ModelLabel: compactText(redactSensitiveText(row.ModelLabel), 120),
		Label:      label, Status: "running",
		StartedAt: startedAt, UpdatedAt: startedAt,
		Summary:           summary,
		OriginTurnID:      strings.TrimSpace(row.OriginTurnID),
		OriginLaneID:      strings.TrimSpace(row.OriginLaneID),
		OriginOperationID: strings.TrimSpace(row.OriginOperationID),
		Forked:            row.Forked,
		HostMachine:       strings.TrimSpace(row.HostMachine),
		HostMachineName:   strings.TrimSpace(row.HostMachineName),
	}
	key := spawnedWorkKey(tabID, chatID, id)
	m.spawnedWorkMu.Lock()
	if existing := m.spawnedWork[key]; existing != nil {
		// One operation id for "launch there" and "record here" (§6): a replay
		// must never produce a second row for one child.
		m.spawnedWorkMu.Unlock()
		return
	}
	m.spawnedWork[key] = &spawnedWorkRecord{Item: item}
	m.spawnedWorkMu.Unlock()
	m.commitSpawnedWorkChange(tabID, chatID)
}

// SettleHostedSubagent settles the asking chat's row from the receipt its
// monitor delivered. It is the same settle the local path performs, driven by
// the home's own record rather than by a run in this process.
func (m *Manager) SettleHostedSubagent(tabID, chatID string, receipt SubagentReceipt) {
	if m == nil {
		return
	}
	id := strings.TrimSpace(receipt.SubagentID)
	// The host stays in the settled row's own words for the same reason it was
	// there while it ran: a reader of a finished row still needs to know which
	// machine ran it, and the line that says so is the one already on the row.
	activity := "Finished"
	m.spawnedWorkMu.Lock()
	if rec := m.spawnedWork[spawnedWorkKey(strings.TrimSpace(tabID), strings.TrimSpace(chatID), id)]; rec != nil {
		if host := firstNonEmpty(rec.Item.HostMachineName, rec.Item.HostMachine); host != "" {
			activity = "Finished on " + host
		}
	}
	m.spawnedWorkMu.Unlock()
	m.settleSubagentSpawnedWork(tabID, chatID, SubagentRun{
		ID:             id,
		Status:         strings.TrimSpace(receipt.Status),
		Label:          receipt.Label,
		ModelLabel:     receipt.ModelLabel,
		Phase:          "finished",
		LatestActivity: activity,
		Result:         receipt.Result,
		Error:          receipt.Error,
	})
}
