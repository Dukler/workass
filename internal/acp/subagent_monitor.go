package acp

import "strings"

// A subagent's life, as a monitor stream.
//
// docs/specs/fleet-runtime.md §7: "Monitors are the only notification
// mechanism", and §13's M2 order makes subagents the first subsystem to move —
// they already own the notice path, so they are the one place where the new
// vocabulary can be proven against behaviour that already exists.
//
// This file is the manager's half of that: it reports a subagent's terminal and
// attention events to ONE observer, in §7's words, with the home's own
// suppression conditions already evaluated. It decides nothing about delivery.
// The home appends them to the entity's log; whether any of them becomes a
// notice in a chat is the watcher's business, and for a self-homed watcher that
// is still the in-process call this manager has always made — which is exactly
// §2's claim that the local API is the fleet API with home == self.

// SubagentLifecycleEvent is one event on one subagent's monitor stream.
type SubagentLifecycleEvent struct {
	// Kind is fleetlog's vocabulary: "exit" or "attention". It is a plain
	// string here because internal/acp must not depend on the log package it
	// feeds; the constants live beside the log.
	Kind string
	// Outcome is set for Kind "exit": settled | failed | cancelled. (lost and
	// timed_out belong to process children, P4.)
	Outcome    string
	SubagentID string
	Label      string
	// Receipt is the bounded, redacted terminal record — the same one the local
	// delivery path uses, so a remote projection of it is byte-identical.
	Receipt SubagentReceipt
	// Attention is set for Kind "attention".
	Attention *SubagentAttention

	ParentChatID string
	ParentTabID  string
	// The origin stamp, when this subagent was spawned from a chat on another
	// machine. OriginMachine is the home that is owed the outcome.
	OriginMachine     string
	OriginChatID      string
	OriginTabID       string
	OriginLaneID      string
	OriginOperationID string

	// NoticeSuppressed carries the home's own suppression verdict, with the
	// reason, so a watcher elsewhere applies the SAME three conditions this
	// machine applies rather than a weaker copy of them: a live waiter will
	// consume the result, the parent cancelled the child, or the engine died
	// and orphaned it. The event is still logged — suppression is about the
	// notice, never about the history.
	NoticeSuppressed bool
	SuppressReason   string
}

// RemoteOrigin reports whether this subagent's outcome is owed to a chat on
// another machine.
func (e SubagentLifecycleEvent) RemoteOrigin(selfMachineID string) bool {
	origin := strings.TrimSpace(e.OriginMachine)
	return origin != "" && origin != strings.TrimSpace(selfMachineID)
}

// SetSubagentLifecycleObserver installs the one observer. It is a setter rather
// than an option because the daemon opens its logs after it builds the manager,
// exactly as SetChatEnvObserver already works.
func (m *Manager) SetSubagentLifecycleObserver(observe func(SubagentLifecycleEvent)) {
	if m == nil {
		return
	}
	m.subagentLifecycleMu.Lock()
	m.subagentLifecycle = observe
	m.subagentLifecycleMu.Unlock()
}

func (m *Manager) emitSubagentLifecycle(event SubagentLifecycleEvent) {
	if m == nil {
		return
	}
	m.subagentLifecycleMu.RLock()
	observe := m.subagentLifecycle
	m.subagentLifecycleMu.RUnlock()
	if observe == nil {
		return
	}
	observe(event)
}
