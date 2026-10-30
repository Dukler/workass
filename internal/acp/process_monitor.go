package acp

import (
	"strings"
	"time"
)

// A process child's life, as a monitor stream.
//
// docs/specs/fleet-runtime.md §7: "Monitors are the only notification
// mechanism", and §13's M2 order makes process children the second subsystem to
// move, after subagents. This file is the manager's half of that, and it is
// deliberately the same shape as subagent_monitor.go: one observer, the home's
// own suppression verdict already evaluated, and no opinion about delivery.
//
// The home appends these to the entity's log; whether any of them becomes a
// notice in a chat is the watcher's business, and for a self-homed watcher that
// is the in-process delivery this daemon already does. That is §2's claim made
// literal — the local API is the fleet API with home == self — and it is why a
// job supervised on one machine can report into a chat on another without a
// second code path existing anywhere.
//
// One rule governs what is emitted at all: only a SUPERVISED row produces
// these events. Bare external work, registered through the adapter with no
// supervisor of its own, behaves exactly as it did before P4 — same row, same
// card, same receipt, same silence — which is the projection-parity gate the
// migration is held to.

// The kinds a process child's stream speaks. They are fleetlog's vocabulary;
// the words live beside the log and are plain strings here because
// internal/acp must not depend on the package it feeds.
const (
	ProcessLifecycleExit      = "exit"
	ProcessLifecycleAttention = "attention"
	// ProcessLifecycleStalled is a declared stall: non-terminal, one-shot, and
	// actionable precisely because the job itself said what silence would mean.
	ProcessLifecycleStalled = "stalled"
	// ProcessLifecycleState is the job's current state, for the row in the chat
	// that owns it. It is not an event in the log and never a notice.
	ProcessLifecycleState = "state"
)

// ProcessLifecycleEvent is one event on one process child's stream.
type ProcessLifecycleEvent struct {
	Kind string
	// Outcome is set for Kind exit: settled | failed | lost | timed_out |
	// cancelled. lost and timed_out are this subsystem's own, and neither can
	// be produced by a subagent.
	Outcome string
	ChildID string
	Label   string
	// Receipt is the bounded, redacted terminal record — the same
	// SpawnedWorkReceipt the local projection already reads, so a remote
	// projection of it is byte-identical to a local one.
	Receipt SpawnedWorkReceipt

	// Owner is the global fleet id the run declared, when it declared one.
	Owner        string
	ParentChatID string
	ParentTabID  string
	// The origin stamp binds the outcome to the turn that asked, so a notice
	// lands on the right lane and the "parent turn was stopped" suppression is
	// evaluable at the chat.
	OriginLaneID      string
	OriginOperationID string
	OriginTurnID      string

	// Resource, ExitCode and Isolation are the facts a reader of the event
	// needs without re-reading the row.
	Resource  string
	ExitCode  *int
	Isolation string

	AttentionKind string
	AttentionText string
	// State is set for Kind state.
	State *HostedWorkState

	// NoticeSuppressed carries this machine's own suppression verdict with its
	// reason, so a watcher elsewhere applies the SAME conditions rather than a
	// weaker copy of them. The event is still logged.
	NoticeSuppressed bool
	SuppressReason   string
	At               time.Time
}

// RemoteOwner reports whether this child's outcome is owed to a chat on
// another machine.
func (e ProcessLifecycleEvent) RemoteOwner(selfMachineID string) bool {
	home := strings.TrimSpace(ownerHomeOf(e.Owner))
	return home != "" && home != strings.TrimSpace(selfMachineID)
}

// ownerHomeOf reads the home out of a global id without importing the id
// package: internal/acp is below it, and one string split is cheaper than an
// import cycle to avoid.
func ownerHomeOf(owner string) string {
	owner = strings.TrimSpace(owner)
	if !strings.HasPrefix(owner, "wa:") {
		return ""
	}
	rest := strings.TrimPrefix(owner, "wa:")
	if cut := strings.Index(rest, "/"); cut > 0 {
		return rest[:cut]
	}
	return ""
}

// SetProcessLifecycleObserver installs the one observer. A setter rather than
// an option because the daemon opens its logs after it builds the manager,
// exactly as SetSubagentLifecycleObserver already works.
func (m *Manager) SetProcessLifecycleObserver(observe func(ProcessLifecycleEvent)) {
	if m == nil {
		return
	}
	m.processLifecycleMu.Lock()
	m.processLifecycle = observe
	m.processLifecycleMu.Unlock()
}

// processLifecycleLocked fills one event from the record it belongs to. The
// caller holds spawnedWorkMu; nothing here reaches outside the registry.
func (m *Manager) processLifecycleLocked(rec *spawnedWorkRecord, event ProcessLifecycleEvent) ProcessLifecycleEvent {
	if rec == nil {
		return event
	}
	event.ChildID = rec.Item.ID
	event.Label = rec.Item.Label
	event.Owner = rec.Item.Owner
	event.ParentChatID, event.ParentTabID = rec.Item.ChatID, rec.Item.TabID
	event.OriginLaneID, event.OriginOperationID = rec.Item.OriginLaneID, rec.Item.OriginOperationID
	event.OriginTurnID = rec.Item.OriginTurnID
	event.Resource, event.ExitCode, event.Isolation = rec.Item.Resource, rec.Item.ExitCode, rec.Item.Isolation
	event.At = time.Now().UTC()
	if event.Kind == ProcessLifecycleExit {
		event.Receipt = m.spawnedWorkReceiptFromItem(rec.Item)
	}
	return event
}

// emitProcessChildLifecycle hands a batch to the observer, in order, after the
// registry lock has been released. Delivery reaches the chat actor, and
// holding the registry lock across that is how two subsystems deadlock.
func (m *Manager) emitProcessChildLifecycle(events []ProcessLifecycleEvent) {
	if m == nil || len(events) == 0 {
		return
	}
	m.processLifecycleMu.RLock()
	observe := m.processLifecycle
	m.processLifecycleMu.RUnlock()
	if observe == nil {
		return
	}
	for _, event := range events {
		if strings.TrimSpace(event.ChildID) == "" {
			continue
		}
		observe(event)
	}
}
