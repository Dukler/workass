package acp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"workass/internal/agenttext"
)

// A tracked subagent's own background work.
//
// A subagent owns a whole engine, and that engine's background shells, Monitors
// and native agents die when its session closes: Claude Code stops them on
// shutdown, and Codex kills its unified-exec processes when the thread goes. So
// the turn ending is not the end of the work. An agent that starts a sweep and
// says "the monitor will report" is relying on being woken, which is what its
// harness does for it in an open session (verified with real Claude Code and
// real Codex, 2026-11-01, docs/evidence/subagent-background-20261101).
//
// Closing on the first end_turn used to kill that work a second later and report
// "completed" (live bug, 2026-11-01: a parent told a sweep finished that had
// died with an empty output file). Two rules replace it:
//
//  1. A live engine waits. While work it owes is running the run stays "running",
//     in phase waiting_background, and it stays cancellable and messageable. The
//     harness wakes the child (Claude Code); one that cannot gets one prompt from
//     here when the work ends (Codex).
//  2. No close is silent. Whatever is still running when the session is about to
//     close is named on the receipt, and work it owed turns the receipt "failed":
//     a parent never reads "completed" over work that was lost.

const subagentPhaseWaitingBackground = "waiting_background"

const (
	defaultSubagentBackgroundCeiling    = 24 * time.Hour
	defaultSubagentBackgroundWakeGrace  = 10 * time.Second
	defaultSubagentBackgroundSettle     = 2 * time.Second
	defaultSubagentBackgroundPollPeriod = 250 * time.Millisecond
	maxSubagentBackgroundStoppedListed  = 16
)

// SubagentBackgroundWork names one piece of background work a subagent's closing
// session stopped. Service marks work that owed nothing, like a dev server.
type SubagentBackgroundWork struct {
	Label   string `json:"label"`
	Kind    string `json:"kind,omitempty"`
	Service bool   `json:"service,omitempty"`
}

type subagentBackgroundOutcome struct {
	// wakePrompt is a prompt the child needs because nothing woke it.
	wakePrompt string
	err        error
}

// subagentOwedBackgroundWork is the work the child's session would kill and that
// it still owes: launched into the background, alive, and not a service. A
// foreground call's task is excluded because it lives inside its own tool call,
// and external work and child subagents because they outlive this session or
// have their own owner.
func (m *Manager) subagentOwedBackgroundWork(tabID, chatID, sessionID string) []SpawnedWorkItem {
	owed := []SpawnedWorkItem{}
	m.spawnedWorkMu.Lock()
	defer m.spawnedWorkMu.Unlock()
	for _, rec := range m.spawnedWork {
		if rec == nil || rec.Item.Status != "running" || rec.Item.TabID != tabID || rec.Item.ChatID != chatID || rec.SessionID != sessionID {
			continue
		}
		if !subagentSessionOwnsWork(rec.Item) || isServiceSpawnedWork(rec.Item) {
			continue
		}
		_, launched := m.spawnedBackgroundCalls[backgroundToolCallKey(tabID, chatID, rec.Item.ToolCallID)]
		if rec.Backgrounded || launched {
			owed = append(owed, rec.Item)
		}
	}
	return owed
}

func subagentSessionOwnsWork(item SpawnedWorkItem) bool {
	switch spawnedWorkLivenessClassFor(item) {
	case spawnedWorkLivenessExternal, spawnedWorkLivenessSubagent, spawnedWorkLivenessHosted:
		return false
	}
	return true
}

// subagentHasQueuedFollowup reports a coordinator message waiting to be sent.
func (m *Manager) subagentHasQueuedFollowup(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	run := m.subagents[id]
	return run != nil && len(run.followups) > 0
}

// awaitSubagentBackgroundWork holds a child whose turn ended until it owes nothing
// more. It returns at once for the usual child that started nothing.
func (m *Manager) awaitSubagentBackgroundWork(ctx context.Context, run *SubagentRun, bridge *Bridge, sessionID, tabID, chatID string) subagentBackgroundOutcome {
	id := run.ID
	var parkedSince, quietSince time.Time
	wokenSinceQuiet := false
	announced := ""
	// A turn can start and end between two polls, so "it was woken" is read from
	// the turn-end report changing, not only from catching the turn running.
	lastEvidence := m.harnessTurns.get(sessionID)
	var lastOwed []SpawnedWorkItem
	poll := time.NewTicker(defaultSubagentBackgroundPollPeriod)
	defer poll.Stop()
	for {
		if ctx.Err() != nil {
			return subagentBackgroundOutcome{}
		}
		// A coordinator message is sent now, by the same loop that sends follow-ups.
		if m.subagentHasQueuedFollowup(id) {
			return subagentBackgroundOutcome{}
		}
		owed := m.subagentOwedBackgroundWork(tabID, chatID, sessionID)
		evidence := m.harnessTurns.get(sessionID)
		wakes := 0
		if evidence != nil {
			wakes = evidence.oneShotCrons()
		}
		waiting := len(owed) > 0 || wakes > 0
		if evidence != lastEvidence {
			lastEvidence, wokenSinceQuiet = evidence, true
		}
		if bridge.Closed() || bridge.Hibernated() {
			if !waiting && parkedSince.IsZero() {
				return subagentBackgroundOutcome{}
			}
			m.rememberSubagentBackgroundWaitedOn(run, lastOwed)
			return subagentBackgroundOutcome{err: errors.New("the provider engine exited while this subagent was waiting for its background work")}
		}
		if !parkedSince.IsZero() && evidence != nil && evidence.At.After(parkedSince) {
			if status, _, text := harnessTerminalOutcome(evidence.TerminalReason, evidence.StopReason, false); status == "failed" {
				return subagentBackgroundOutcome{err: errors.New(text)}
			}
		}
		switch {
		case m.harnessTurns.active(sessionID):
			// The harness woke the child, and it is working: its own events
			// report that. Not bounded here, like any other turn.
			parkedSince = firstNonZeroTime(parkedSince, time.Now())
			quietSince, wokenSinceQuiet, announced = time.Time{}, true, ""
		case waiting:
			now := time.Now()
			parkedSince = firstNonZeroTime(parkedSince, now)
			quietSince, wokenSinceQuiet, lastOwed = time.Time{}, false, owed
			if now.Sub(parkedSince) >= m.opts.SubagentBackgroundCeiling {
				return subagentBackgroundOutcome{err: fmt.Errorf("its background work was still running after %s, the most a subagent waits", m.opts.SubagentBackgroundCeiling)}
			}
			if text := subagentWaitingActivity(owed, wakes); text != announced {
				announced = text
				m.updateSubagentActivity(id, subagentPhaseWaitingBackground, text)
			}
		case parkedSince.IsZero():
			return subagentBackgroundOutcome{}
		default:
			now := time.Now()
			quietSince = firstNonZeroTime(quietSince, now)
			quiet := now.Sub(quietSince)
			if wokenSinceQuiet && quiet >= m.opts.SubagentBackgroundSettle {
				return subagentBackgroundOutcome{}
			}
			if !wokenSinceQuiet && quiet >= m.opts.SubagentBackgroundWakeGrace {
				return subagentBackgroundOutcome{wakePrompt: m.subagentBackgroundWakePrompt(lastOwed, tabID, chatID)}
			}
		}
		select {
		case <-ctx.Done():
		case <-poll.C:
		}
	}
}

func firstNonZeroTime(value, fallback time.Time) time.Time {
	if value.IsZero() {
		return fallback
	}
	return value
}

func subagentWaitingActivity(owed []SpawnedWorkItem, wakes int) string {
	if len(owed) == 0 {
		return fmt.Sprintf("Waiting for %d scheduled wake(s)", wakes)
	}
	labels := make([]string, 0, len(owed))
	for _, item := range owed {
		labels = append(labels, item.Label)
	}
	sortStrings(labels)
	return "Waiting for background work: " + strings.Join(labels, "; ")
}

// rememberSubagentBackgroundWaitedOn keeps the names of the work an engine took down
// with it: by the time the run settles, the registry may already have orphaned
// those rows, and the close would then report nothing lost.
func (m *Manager) rememberSubagentBackgroundWaitedOn(run *SubagentRun, owed []SpawnedWorkItem) {
	work := make([]SubagentBackgroundWork, 0, len(owed))
	for _, item := range owed {
		work = append(work, SubagentBackgroundWork{Label: compactText(redactSensitiveText(item.Label), 240), Kind: item.Kind})
	}
	m.mu.Lock()
	run.backgroundWaitedOn = boundedSubagentBackgroundWork(work)
	m.mu.Unlock()
}

// subagentBackgroundWakePrompt tells a child nothing else woke that its work ended,
// with the provider's own record of how. Empty when what ended was only
// reclassified as a service: it did not finish, and nothing owes it a reply.
func (m *Manager) subagentBackgroundWakePrompt(owed []SpawnedWorkItem, tabID, chatID string) string {
	var b strings.Builder
	m.spawnedWorkMu.Lock()
	for _, item := range owed {
		key := spawnedWorkKey(tabID, chatID, item.TaskID)
		current := item
		if rec := m.spawnedWork[key]; rec != nil {
			current = rec.Item
		} else if settled, ok := m.spawnedWorkSettled[key]; ok {
			current = settled
		}
		if current.Status == "running" {
			continue
		}
		fmt.Fprintf(&b, "- %s: %s", current.Label, current.Status)
		if summary := strings.TrimSpace(current.Summary); summary != "" {
			fmt.Fprintf(&b, "\n%s", summary)
		}
		b.WriteString("\n")
	}
	m.spawnedWorkMu.Unlock()
	if b.Len() == 0 {
		return ""
	}
	return agenttext.Get("subagent.background_finished") + b.String()
}

// subagentBackgroundAtClose lists what the session is about to stop. It also
// reads the harness's own turn-end report, because a task the registry never saw
// would otherwise be lost in silence; an absent report simply adds nothing.
func (m *Manager) subagentBackgroundAtClose(run *SubagentRun) (owed, services []SubagentBackgroundWork) {
	m.mu.Lock()
	sessionID := run.SessionID
	id := run.ID
	m.mu.Unlock()
	if sessionID == "" {
		return nil, nil
	}
	chatID := subagentChatIDPrefix + id
	tabID := chatID
	known := map[string]bool{}
	m.spawnedWorkMu.Lock()
	for _, rec := range m.spawnedWork {
		if rec == nil || rec.Item.TabID != tabID || rec.Item.ChatID != chatID {
			continue
		}
		known[rec.Item.TaskID] = true
		if rec.Item.Status != "running" || rec.SessionID != sessionID || !subagentSessionOwnsWork(rec.Item) {
			continue
		}
		work := SubagentBackgroundWork{Label: compactText(redactSensitiveText(rec.Item.Label), 240), Kind: rec.Item.Kind}
		if isServiceSpawnedWork(rec.Item) {
			work.Service = true
			services = append(services, work)
			continue
		}
		_, launched := m.spawnedBackgroundCalls[backgroundToolCallKey(tabID, chatID, rec.Item.ToolCallID)]
		if rec.Backgrounded || launched {
			owed = append(owed, work)
		}
	}
	if evidence := m.harnessTurns.get(sessionID); evidence != nil {
		for _, task := range evidence.Tasks {
			taskID := normalizeSpawnedWorkTaskID(task.ID)
			if taskID == "" || known[taskID] {
				continue
			}
			if _, settled := m.spawnedWorkSettled[spawnedWorkKey(tabID, chatID, taskID)]; settled {
				continue
			}
			owed = append(owed, SubagentBackgroundWork{Label: firstNonEmpty(task.Type, "background task") + " " + taskID, Kind: task.Type})
		}
	}
	m.spawnedWorkMu.Unlock()
	return boundedSubagentBackgroundWork(owed), boundedSubagentBackgroundWork(services)
}

func boundedSubagentBackgroundWork(list []SubagentBackgroundWork) []SubagentBackgroundWork {
	if len(list) > maxSubagentBackgroundStoppedListed {
		return list[:maxSubagentBackgroundStoppedListed]
	}
	return list
}

func subagentBackgroundLostText(cause string, owed []SubagentBackgroundWork) string {
	labels := make([]string, 0, len(owed))
	for _, work := range owed {
		labels = append(labels, fmt.Sprintf("%q", work.Label))
	}
	lost := "Background work was still running when this subagent's session closed, and it was stopped: " + strings.Join(labels, ", ") + ". Treat anything it was producing as incomplete."
	if cause = strings.TrimRight(strings.TrimSpace(cause), "."); cause != "" {
		return cause + ". " + lost
	}
	return lost
}
